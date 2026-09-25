package translate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/B1anYu/cmdc2api/internal/types"
)

// ===========================================================================
// Responses 入站归一化（POST /v1/responses，Codex CLI）
//
// 与 chatin.go 是姊妹实现：把客户端协议折叠成规范格式（Anthropic 形状的 types.Request）
// 后交给既有共享管线，会话亲和、缓存断点、伪装与 usage 换算全部免费继承。
// 两条入站路径的口径必须一致，因此凡是能共用的解析与映射（图片源、参数兜底、
// 档位收敛、配对修复）一律复用，只在协议形状确实不同的地方分叉。
//
// Responses 特有的三处复杂度：
//  1. 工具族降级：custom / tool_search / namespace 三种声明在上游都没有对应能力，
//     统一降级为普通 function 工具，还原信息记进 ResponsesToolMapping 供出站还原；
//     降级会引入重名风险，冲突必须显式 400（§4.3），绝不静默吞掉一个工具声明；
//  2. 工具发现提升：tool_search_output 的发现结果要提升为正式声明并追加到末尾，
//     与既有声明同名同 schema 去重、同名不同 schema 报 400；
//  3. 历史回放：Codex 以 store:false 每轮重发全量 input，开发者通知、并行兄弟调用、
//     未应答的调用都会插在 call 与 output 之间，必须靠 RepairToolPairing 重建三条不变式。
//
// 安全丢弃字段（include/text/... ）在 types.ResponsesRequest 里刻意没有类型，
// 只能在原始报文上做存在性探测留痕，故入口有「带报文」与「不带报文」两个版本。
// ===========================================================================

// ErrResponsesPreviousResponseID 客户端要求续接服务端保存的上一轮响应。
// 本代理不保存任何服务端状态（store 恒为 false），静默忽略会让客户端拿到一段
// 没有前置上下文的对话，因此显式拒绝并提示重发全量历史。
var ErrResponsesPreviousResponseID = errors.New(
	"previous_response_id is not supported; resend the full input history instead (store:false)")

// ErrResponsesEmptyInput input 里没有留下任何可发送的消息。
// Anthropic 对空 messages 直接 400，与其把畸形信封送上去不如在归一化阶段拒绝。
var ErrResponsesEmptyInput = errors.New(
	"input produced no convertible messages; send at least one non-empty user message")

// ResponsesToRequest 把 Responses 请求归一化为规范格式（Anthropic 形状）。
// 返回的 mapping 记录工具族降级信息（供出站还原 item 形态），warns 描述所有被降级/丢弃的内容；
// err 承载 400 类拒绝：previous_response_id、工具名冲突、input 无法产出消息。
//
// 注意：安全丢弃字段的存在性探测需要原始报文，handler 应优先使用 ResponsesToRequestBody，
// 否则 include/text 这类无类型字段不会留痕（违反 §0.4 的留痕硬约束）。
func ResponsesToRequest(req *types.ResponsesRequest) (*types.Request, *ResponsesToolMapping, []string, error) {
	return ResponsesToRequestBody(nil, req)
}

// ResponsesToRequestBody 同 ResponsesToRequest，但额外把原始报文中「有上游能力缺口」的
// 字段留痕并入 warns。这些字段在 ResponsesRequest 里没有类型（见 types/responses.go），
// 只有原始报文能证明客户端确实发过它们。
func ResponsesToRequestBody(raw []byte, req *types.ResponsesRequest) (*types.Request, *ResponsesToolMapping, []string, error) {
	out, mapping, warns, err := responsesToRequest(req)
	if err != nil {
		return nil, nil, warns, err
	}
	return out, mapping, append(warns, ResponsesIgnoredWarnings(raw)...), nil
}

// responsesToRequest 归一化主流程。
func responsesToRequest(req *types.ResponsesRequest) (*types.Request, *ResponsesToolMapping, []string, error) {
	if strings.TrimSpace(req.PreviousResponseID) != "" {
		return nil, nil, nil, ErrResponsesPreviousResponseID
	}

	items, plain, err := responsesInput(req.Input)
	if err != nil {
		return nil, nil, nil, err
	}

	out := &types.Request{
		Model:       req.Model,
		Stream:      req.Stream,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxOutputTokens, // ≤0 交给 BuildCcRequest 统一兜底 64000
		// OpenAI Responses 协议没有 cache_control 字段，信封断点只能由代理在末尾合成
		// （否则本端点永远拿不到断点，只剩会话亲和一条缓存路径）。
		SynthesizeTailCacheMarker: true,
	}
	// prompt_cache_key 被消费为会话亲和候选（不再是「ignored」字段）。此处只搬运值，
	// 最终是否采用由会话解析决定（显式 session 请求头优先），故不在此处留痕——
	// 它既不是丢弃项，也没有信息损失。依据 A 级参照 proxy.mjs:204-210。
	if req.PromptCacheKey != nil {
		out.PromptCacheKey = *req.PromptCacheKey
	}

	s := &respinState{mapping: &ResponsesToolMapping{}}
	// instructions 是全局指令，占 system 首段；input 里的 system/developer item 按序并入其后。
	if inst := strings.TrimSpace(req.Instructions); inst != "" {
		s.systemParts = append(s.systemParts, inst)
	}
	// input 纯字符串形态是合法协议（单条 user 消息）：CLIProxyAPI 静默丢弃它导致整轮空转，
	// 必须支持。空串不产消息，随后由「无消息」错误统一拦下。
	if plain != "" {
		s.msgs = append(s.msgs, types.InboundMessage{
			Role:    "user",
			Content: InboundBlocksContent([]types.Block{{Type: "text", Text: plain}}),
		})
	}
	// 工具声明必须先收集完：tool_search_output 的发现提升要据此判断本轮是否声明了 tool_search，
	// 而声明可能出现在 input 之前（顶层 tools）也可能出现在之后（additional_tools item）。
	s.collectToolDeclarations(req.Tools, items)
	for i := range items {
		s.convertItem(i, &items[i])
	}

	// 工具声明在三处汇合：顶层 tools 优先，additional_tools item 次之，发现提升追加到最后。
	tools, err := responsesTools(req.Tools, s.additional, s.discovered, s.mapping, &s.warns)
	if err != nil {
		return nil, nil, aggregateRequestWarns(s.warns), err
	}
	out.Tools = tools

	// 配对修复与 Chat 侧共用同一实现（三条不变式见 pairing.go）。
	repaired, pw := RepairToolPairing(s.msgs)
	s.warns = append(s.warns, pw...)
	out.Messages = repaired
	if len(out.Messages) == 0 {
		return nil, nil, aggregateRequestWarns(s.warns), ErrResponsesEmptyInput
	}

	if len(s.systemParts) > 0 {
		out.System = chatJSON(strings.Join(s.systemParts, "\n\n"))
	}
	if tc := responsesToolChoice(req.ToolChoice, len(out.Tools) > 0, &s.warns); tc != nil {
		out.ToolChoice = tc
	}
	// parallel_tool_calls=false 有上游载体（tool_choice.disable_parallel_tool_use），因此
	// 必须在 tool_choice 落地**之后**再合并——与 Chat 侧同序、同实现（chatApplyParallelToolCalls），
	// 两条入站的映射口径因此天然一致。显式为 true 或未声明一律不动（上游默认即允许并行）。
	parallelApplied := chatApplyParallelToolCalls(req.ParallelToolCalls, len(out.Tools) > 0, &out.ToolChoice)
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls && !parallelApplied {
		// 显式 false 但本轮最终没有工具：没有 tool_choice 可挂标志，串行约束无法表达。
		// 文案与触发口径照搬 Chat 侧（chatin.go 的 chatDroppedFieldWarnings）——客户端
		// 以为并行已关闭，实际上游只会照常并行，属于行为性丢失，按 WARN 计（无 info: 前缀）。
		s.warns = append(s.warns,
			"parallel_tool_calls ignored (upstream has no per-request parallel-call toggle)")
	}
	// reasoning.effort → adaptive thinking。值域与 Chat 侧 reasoning_effort 完全一致
	// （low/medium/high/max/xhigh 透传、minimal→low，依据见 chatin.go 的 chatReasoningEffort），
	// 直接复用同一实现，避免两条入站路径各写一份映射表。
	if req.Reasoning != nil {
		if effort := chatReasoningEffort(chatJSON(req.Reasoning.Effort), &s.warns); effort != "" {
			out.Thinking = chatJSON(struct {
				Type   string `json:"type"`
				Effort string `json:"effort"`
			}{Type: "adaptive", Effort: effort})
		}
		if req.Reasoning.Summary != nil {
			s.warns = append(s.warns,
				"info: reasoning.summary ignored (thinking summaries are streamed whenever the upstream emits reasoning)")
		}
	}
	if req.Store != nil && *req.Store {
		// store:false 与本代理的行为完全一致（无服务端状态），无需留痕；
		// store:true 才是真的能力缺失，必须让客户端知道历史不会被保存。
		// 级别与条件和 Chat 侧 chatin.go 的处理保持一致（双入站口径统一，均为 WARN）。
		s.warns = append(s.warns,
			"store ignored (this proxy keeps no server-side state; previous_response_id is rejected)")
	}
	return out, s.mapping, aggregateRequestWarns(s.warns), nil
}

// ---------- 安全丢弃字段留痕 ----------

// responsesIgnoredWarnReasons 无上游对应能力的顶层字段的留痕文案。
// 分级与 Chat 侧一致：良性丢失（上游本来就没有该维度的选择权）用 info: 前缀；
// 行为性丢失（客户端以为限制已生效，实际不会）按警告计。
//
// parallel_tool_calls 不在其中：它有上游载体（并入 tool_choice.disable_parallel_tool_use），
// 是**被消费**的字段，只在无法表达时才留痕（见 responsesToRequest 里的映射）。
var responsesIgnoredWarnReasons = map[string]string{
	"include":           "info: include ignored (upstream returns no log probabilities; reasoning signatures are always sent back as encrypted_content, so it needs no include entry)",
	"truncation":        "info: truncation ignored (upstream applies its own context-window handling)",
	"background":        "background ignored (upstream has no async execution mode)",
	"service_tier":      "info: service_tier ignored (upstream has no service-tier selection)",
	"safety_identifier": "info: safety_identifier ignored (upstream has no end-user attribution field)",
	"user":              "info: user ignored (upstream has no end-user attribution field)",
	"metadata":          "info: metadata ignored (upstream has no metadata field)",
	"text":              "text.format/verbosity ignored (upstream has no structured-output mode)",
	"top_logprobs":      "info: top_logprobs ignored (upstream returns no log probabilities)",
	"stream_options":    "info: stream_options ignored (usage is always emitted with the final events)",
}

// ResponsesIgnoredWarnings 对原始报文做存在性探测，为每个「客户端发过、上游无能力」的字段
// 产出留痕（顺序由 types.ResponsesIgnoredFields 的声明顺序固定，同一请求的日志可复现）。
func ResponsesIgnoredWarnings(raw []byte) []string {
	fields := types.ResponsesIgnoredFields(raw)
	if len(fields) == 0 {
		return nil
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if reason, ok := responsesIgnoredWarnReasons[f]; ok {
			out = append(out, reason)
			continue
		}
		// 新增而此处未登记：仍然留痕，避免静默丢弃（留痕本身比文案精确更重要）。
		out = append(out, fmt.Sprintf("info: %s ignored (no upstream equivalent)", f))
	}
	return out
}

// ---------- input 解析与逐项转换 ----------

// respinState 归一化过程中的累积状态：消息、system 段、工具声明与留痕。
// 逐项转换需要在多处累积，用一个状态对象比层层传参清晰。
type respinState struct {
	systemParts []string
	msgs        []types.InboundMessage
	mapping     *ResponsesToolMapping
	additional  []types.ResponsesTool // additional_tools item 声明（优先级低于顶层 tools）
	discovered  []types.ResponsesTool // tool_search_output 提升的工具（最后追加）
	warns       []string
}

func (s *respinState) warnf(format string, args ...any) {
	s.warns = append(s.warns, fmt.Sprintf(format, args...))
}

// responsesInput 解析 input 字段（string | []item）。
// 缺失/null 视为空；两种形态互斥：字符串形态返回 plain（items 为 nil）。
func responsesInput(raw json.RawMessage) ([]types.ResponsesInputItem, string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, "", nil
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return nil, plain, nil
	}
	var items []types.ResponsesInputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		// 带上底层错误：逐项字段类型不对时只报「input 形状不对」，会把客户端引到它明明
		// 写对了的 input 结构上，而真正出问题的是某个 item 的某个字段。
		return nil, "", fmt.Errorf("input must be a string or an array of input items: %w", err)
	}
	return items, "", nil
}

// collectToolDeclarations 第一遍扫描：收集 additional_tools item 的声明，
// 并判定本轮是否声明了 tool_search（后者决定 tool_search_output 能否提升为正式工具）。
func (s *respinState) collectToolDeclarations(top []types.ResponsesTool, items []types.ResponsesInputItem) {
	s.mapping.ToolSearch = declaresToolSearch(top)
	for i := range items {
		if items[i].Type != "additional_tools" {
			continue
		}
		tools, err := responsesToolList(items[i].Tools)
		if err != nil {
			s.warnf("input item %d: additional_tools payload is not a tool array; dropped", i)
			continue
		}
		s.additional = append(s.additional, tools...)
		if declaresToolSearch(tools) {
			s.mapping.ToolSearch = true
		}
	}
}

// declaresToolSearch 判断一组声明里是否含 tool_search 代理工具。
func declaresToolSearch(tools []types.ResponsesTool) bool {
	for i := range tools {
		if tools[i].Type == types.ResponsesToolToolSearch {
			return true
		}
	}
	return false
}

// convertItem 按 type 分派单个 input item。未知类型一律丢弃 + 留痕：
// 白名单式降级太绕，明确丢弃比猜语义安全。
func (s *respinState) convertItem(idx int, item *types.ResponsesInputItem) {
	switch item.Type {
	case "", "message":
		// 无 type 的 item 靠 role 判定（Codex 的 user/assistant 消息不带 type）。
		s.convertMessageItem(idx, item)
	case "function_call", "custom_tool_call":
		s.convertToolCallItem(idx, item)
	case "function_call_output", "custom_tool_call_output":
		s.convertToolOutputItem(idx, item)
	case "reasoning":
		s.convertReasoningItem(idx, item)
	case "additional_tools":
		// 已在 collectToolDeclarations 里收集（不产消息）
	case "tool_search_output":
		s.convertToolSearchOutput(idx, item)
	default:
		s.warnf("input item %d: unsupported type %q dropped", idx, clipWarnValue(item.Type))
	}
}

// convertMessageItem 处理 message item（无 type 靠 role，或 type=="message"）。
func (s *respinState) convertMessageItem(idx int, item *types.ResponsesInputItem) {
	switch item.Role {
	case "system", "developer":
		// 与 Chat 侧同口径：两者都并入 system（Anthropic 只有 system 能承载全局指令）。
		if txt := s.systemText(idx, item.Content); txt != "" {
			s.systemParts = append(s.systemParts, txt)
		}
	case "user":
		if blocks := s.userBlocks(idx, item.Content); len(blocks) > 0 {
			s.msgs = append(s.msgs, types.InboundMessage{Role: "user", Content: InboundBlocksContent(blocks)})
		}
	case "assistant":
		if blocks := s.assistantBlocks(idx, item.Content); len(blocks) > 0 {
			s.msgs = append(s.msgs, types.InboundMessage{Role: "assistant", Content: InboundBlocksContent(blocks)})
		}
	default:
		// 未知 role 兜底为 user（保留内容），而不是整条丢弃：CC 的信封只有 user/assistant
		// 两种角色，丢掉这条就把客户端历史里的一段内容从模型视野里抹掉了。
		// 口径与 A 级参照 proxy.mjs 的 `{role:'user', content:[{type:'text', text:...}]}` 一致。
		s.warnf("input item %d: unknown role %q downgraded to a user message (upstream accepts user/assistant roles only; its content is kept)", idx, clipWarnValue(item.Role))
		if blocks := s.userBlocks(idx, item.Content); len(blocks) > 0 {
			s.msgs = append(s.msgs, types.InboundMessage{Role: "user", Content: InboundBlocksContent(blocks)})
		}
	}
}

// convertToolCallItem function_call / custom_tool_call → assistant 消息 + tool_use 块。
// custom 的参数包成 {"input": <自由文本>}，与 §4.3 降级 schema（单 input:string 属性）对偶，
// 出站才能按同一约定解包还原成 custom_tool_call 的裸字符串。
func (s *respinState) convertToolCallItem(idx int, item *types.ResponsesInputItem) {
	if item.Name == "" {
		s.warnf("input item %d (%s): tool call without name dropped", idx, clipWarnValue(item.Type))
		return
	}
	if item.CallID == "" {
		// 没有 call_id 的调用无法与结果配对，配对修复阶段也只会把它当悬空调用清掉，
		// 这里提前丢弃并说清原因。
		s.warnf("input item %d (%s): tool call %q without call_id dropped (cannot pair with its output)",
			idx, clipWarnValue(item.Type), clipWarnValue(item.Name))
		return
	}
	input := chatToolArgsReport(item.Name, item.Arguments, &s.warns) // 空串兜底 {}；非法 JSON 与非 object 留痕
	if item.Type == "custom_tool_call" {
		input = chatJSON(map[string]string{"input": item.Input})
	}
	s.msgs = append(s.msgs, types.InboundMessage{
		Role: "assistant",
		Content: InboundBlocksContent([]types.Block{{
			Type: "tool_use", ID: item.CallID, Name: item.Name, Input: input,
		}}),
	})
}

// convertToolOutputItem function_call_output / custom_tool_call_output → user 消息 + tool_result 块。
func (s *respinState) convertToolOutputItem(idx int, item *types.ResponsesInputItem) {
	if item.CallID == "" {
		s.warnf("input item %d (%s): missing call_id, dropped (cannot be paired with a tool call)",
			idx, clipWarnValue(item.Type))
		return
	}
	s.msgs = append(s.msgs, types.InboundMessage{
		Role: "user",
		Content: InboundBlocksContent([]types.Block{{
			Type: "tool_result", ToolUseID: item.CallID, Content: s.toolResultContent(idx, item.Output),
		}}),
	})
}

// toolResultContent 归一化工具输出：
//   - 字符串直用（空串也直用：命令无输出是常态，编造占位文本会污染模型看到的事实）；
//   - part 数组按 text/image 白名单重建为块数组，图片交给 convertUser 既有的搬迁逻辑；
//   - 其它形态（对象/数字）保留原始 JSON 文本，信息量大于直接清空。
//
// is_error 无上游对应能力，安全丢弃（见项目规约 §3.5）。
func (s *respinState) toolResultContent(idx int, raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return chatJSON("")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return chatJSON(text)
	}
	parts, err := respinParts(raw)
	if err != nil {
		s.warnf("input item %d: tool output is neither string nor part array; retained as raw JSON", idx)
		return chatJSON(string(raw))
	}
	blocks := make([]types.Block, 0, len(parts))
	for _, p := range parts {
		switch {
		case isChatTextPart(p.Type):
			if p.Text == "" {
				continue
			}
			blocks = append(blocks, types.Block{Type: "text", Text: p.Text})
		case p.Type == "input_image":
			src, ok := respinImageSource(p.ImageURL)
			if !ok {
				s.warnf("input item %d: tool output image with unsupported or missing url dropped", idx)
				continue
			}
			blocks = append(blocks, types.Block{Type: "image", Source: src})
		default:
			s.warnf("input item %d: unsupported tool output part %q dropped", idx, clipWarnValue(p.Type))
		}
	}
	if len(blocks) == 0 {
		return chatJSON("")
	}
	return InboundBlocksContent(blocks)
}

// convertReasoningItem reasoning item → assistant 消息 + thinking 块（不是丢弃：
// 历史里的思考过程在上游思考模式下强制校验，缺失会导致 502）。
// 全空（无明文可回放）则跳过；encrypted_content 丢弃且不产消息——它本来就是给服务端
// 状态存储用的密文，发上游前签名一律被过滤，留着毫无意义。
func (s *respinState) convertReasoningItem(idx int, item *types.ResponsesInputItem) {
	text := respinReasoningText(item)
	if text == "" {
		return
	}
	s.msgs = append(s.msgs, types.InboundMessage{
		Role:    "assistant",
		Content: InboundBlocksContent([]types.Block{{Type: "thinking", Thinking: text}}),
	})
}

// convertToolSearchOutput tool_search_output 的发现结果提升为正式工具声明。
// 未声明 tool_search 或 status 不是 completed 的输出不提升（客户端没有要求过搜索能力）。
func (s *respinState) convertToolSearchOutput(idx int, item *types.ResponsesInputItem) {
	if !s.mapping.ToolSearch {
		s.warnf("input item %d: tool_search_output ignored (no tool_search declaration in this request)", idx)
		return
	}
	// 只有 completed 才提升：缺失 status 同样不提升（空串不等于 completed，
	// 认下它会与上一行的注释自相矛盾）。
	if item.Status != types.ResponsesStatusCompleted {
		s.warnf("input item %d: tool_search_output with status %q not promoted (only completed results are promoted; a missing status is not treated as completed, so the tools it discovered are not declared upstream)",
			idx, clipWarnValue(item.Status))
		return
	}
	tools, err := responsesToolList(item.Tools)
	if err != nil {
		s.warnf("input item %d: tool_search_output payload is not a tool array; dropped", idx)
		return
	}
	if len(tools) == 0 {
		s.warnf("input item %d: tool_search_output carries no tool; dropped", idx)
		return
	}
	s.discovered = append(s.discovered, tools...)
}

// ---------- content 解析 ----------

// respinPart content part 的宽松解析形态。
// 与 types.ResponsesContentPart 的差别只有一处：image_url 收成 RawMessage，
// 因为部分桥接层发的是 {"url": "..."} 对象而不是 URL 字符串。
// 入站解析一律宽松：对不上形状的 part 走留痕丢弃，而不是把整条消息打成解析失败。
// image 的 detail 与 Chat 侧同口径忽略（上游只按 URL 抓图，没有保真度档位）。
type respinPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
}

// respinParts 解析 content（string | []part | null）。字符串形态折成单个 text part，
// 与数组形态共用同一条白名单逻辑。
func respinParts(raw json.RawMessage) ([]respinPart, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one == "" {
			return nil, nil
		}
		return []respinPart{{Type: "text", Text: one}}, nil
	}
	var parts []respinPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, err
	}
	return parts, nil
}

// systemText 抽取 system/developer item 的纯文本。
// part 白名单与 Chat 侧共用（isChatTextPart）：Responses 的 input_text/output_text
// 与 Chat 的 text 承载同一语义，多段按出现顺序以换行连接。
//
// 两条丢弃路径各留痕（与 userBlocks/assistantBlocks 同口径）：
//   - 非文本 part（如 input_image）：Anthropic 的 system 段只承载文本，整段指令无处安放；
//   - content 既非字符串也非 part 数组：解析失败。
//
// 返回空串有两种含义——「本来就没内容」与「内容被丢弃/解析失败」——前者不留痕，
// 后者必定已经 warn，调用点据此可区分「空」与「坏」。
func (s *respinState) systemText(idx int, raw json.RawMessage) string {
	parts, err := respinParts(raw)
	if err != nil {
		s.warnf("input item %d (system/developer): content is neither string nor part array; dropped (these instructions are not sent upstream)", idx)
		return ""
	}
	var texts []string
	for _, p := range parts {
		if isChatTextPart(p.Type) {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
			continue
		}
		s.warnf("input item %d (system/developer): unsupported content part %q dropped (the system prompt carries text only, so this instruction is not sent upstream)", idx, p.Type)
	}
	return strings.Join(texts, "\n")
}

// userBlocks user 消息 content → text/image 块。
// 图片源解析复用 Chat 侧实现（chatImageSource）：data: URI 解成 base64 source，
// http(s) URL 原样交给上游抓取，代理端不做下载。
func (s *respinState) userBlocks(idx int, raw json.RawMessage) []types.Block {
	parts, err := respinParts(raw)
	if err != nil {
		s.warnf("input item %d (user): content is neither string nor part array; dropped", idx)
		return nil
	}
	blocks := make([]types.Block, 0, len(parts))
	for _, p := range parts {
		switch {
		case isChatTextPart(p.Type):
			if p.Text == "" {
				continue
			}
			blocks = append(blocks, types.Block{Type: "text", Text: p.Text})
		case p.Type == "input_image":
			src, ok := respinImageSource(p.ImageURL)
			if !ok {
				s.warnf("input item %d (user): image with unsupported or missing url dropped", idx)
				continue
			}
			blocks = append(blocks, types.Block{Type: "image", Source: src})
		default:
			s.warnf("input item %d (user): unsupported content part %q dropped", idx, p.Type)
		}
	}
	return blocks
}

// assistantBlocks assistant 消息 content → text 块。
// assistant 历史里的图片没有上游载体（cmdc 只在 user 段接受 image part），留痕丢弃。
func (s *respinState) assistantBlocks(idx int, raw json.RawMessage) []types.Block {
	parts, err := respinParts(raw)
	if err != nil {
		s.warnf("input item %d (assistant): content is neither string nor part array; dropped", idx)
		return nil
	}
	blocks := make([]types.Block, 0, len(parts))
	for _, p := range parts {
		if isChatTextPart(p.Type) {
			if p.Text != "" {
				blocks = append(blocks, types.Block{Type: "text", Text: p.Text})
			}
			continue
		}
		s.warnf("input item %d (assistant): unsupported content part %q dropped", idx, p.Type)
	}
	return blocks
}

// respinImageSource input_image.image_url → Anthropic image source。
// 接受字符串形态与 {"url": ...} 对象形态。
func respinImageSource(raw json.RawMessage) (*types.ImageSource, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var url string
	if err := json.Unmarshal(raw, &url); err != nil {
		var obj struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, false
		}
		url = obj.URL
	}
	if url == "" {
		return nil, false
	}
	return chatImageSource(url)
}

// respinReasoningText 抽取 reasoning item 的思考明文：
// summary[].text 优先，为空才回退 content[] 里的 reasoning_text。
// 两组只取先非空者（同一次思考常常两边都写，都取会让历史里出现两遍）。
func respinReasoningText(item *types.ResponsesInputItem) string {
	var summary []string
	for _, p := range item.Summary {
		if p.Text != "" {
			summary = append(summary, p.Text)
		}
	}
	if len(summary) > 0 {
		return strings.Join(summary, "\n")
	}
	parts, err := respinParts(item.Content)
	if err != nil {
		return ""
	}
	var reasoning []string
	for _, p := range parts {
		if p.Type == "reasoning_text" && p.Text != "" {
			reasoning = append(reasoning, p.Text)
		}
	}
	return strings.Join(reasoning, "\n")
}

// ---------- 工具族 ----------

// respinToolNameMax 摊平后的工具名长度上限（Codex 侧对工具名的限制）。
const respinToolNameMax = 64

// respinToolSearchHelp tool_search 代理工具的说明文案。
const respinToolSearchHelp = "Search the available tools by keyword."

// respinToolSearchSchema tool_search 代理工具的固定 schema。
var respinToolSearchSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"query":{"type":"string","description":"Search for tools by keyword."},` +
	`"limit":{"type":"integer","description":"Maximum number of tools to return."}},` +
	`"required":["query"]}`)

// respinToolSet 工具声明累积器：负责三类重名冲突的显式拒绝（§4.3 不允许静默降级）
// 与发现提升的同名同 schema 去重。名字即身份——摊平名、代理名与原始名同处一个命名空间。
type respinToolSet struct {
	tools    []types.Tool
	schemas  map[string]string // 名字 → 规范化 schema 文本（同 schema 去重的依据）
	origins  map[string]string // 名字 → 声明来源（冲突文案要说清是谁跟谁撞了）
	replaced []string          // 说明书被整体替换的工具名（按请求聚合成一条留痕，见 responsesTools）
	search   bool              // 是否已声明 tool_search
}

// add 登记一个工具声明。dedupe 为真时，与既有声明同名同 schema 视为重复声明静默跳过
// （Codex 会在后续轮次的 additional_tools / 发现结果里重发同一批工具）；
// 其余重名一律报错——名字被覆盖意味着客户端声明的某个工具永远调不到。
func (t *respinToolSet) add(name, description, origin string, schema json.RawMessage, dedupe bool) error {
	schema, wasReplaced := normalizeSchemaReport(schema)
	key := string(schema)
	if prev, ok := t.origins[name]; ok {
		if dedupe && t.schemas[name] == key {
			return nil
		}
		if name == responsesToolSearchName {
			return fmt.Errorf(
				"tool name %q is reserved by the tool_search declaration (conflicting declaration from %s); rename the tool or drop the tool_search declaration",
				name, origin)
		}
		return fmt.Errorf(
			"tool name %q is declared more than once (%s and %s); rename one of them, or wrap the duplicates in a namespace so they flatten to distinct names",
			name, prev, origin)
	}
	t.tools = append(t.tools, types.Tool{Name: name, Description: description, InputSchema: schema})
	t.schemas[name] = key
	t.origins[name] = origin
	if wasReplaced {
		// 只在真正登记时记录：被去重跳过的那次是同一份声明，报同一名字只会刷屏。
		t.replaced = append(t.replaced, name)
	}
	return nil
}

// declare 登记一批 Responses 工具声明，把 custom / tool_search / namespace 降级为 function。
func (t *respinToolSet) declare(list []types.ResponsesTool, origin string, dedupe bool,
	mapping *ResponsesToolMapping, warns *[]string) error {
	for i := range list {
		tool := &list[i]
		switch tool.Type {
		case "", types.ResponsesToolFunction:
			if tool.Name == "" {
				*warns = append(*warns, fmt.Sprintf("tool %d (%s): function without name dropped", i, clipWarnValue(origin)))
				continue
			}
			if tool.Strict != nil && *tool.Strict {
				// 与 Chat 侧同口径留痕：上游没有严格 schema 模式
				*warns = append(*warns, fmt.Sprintf(
					"tool %s: strict schema flag dropped (upstream has no strict-schema mode)", tool.Name))
			}
			if err := t.add(tool.Name, tool.Description, origin, tool.Parameters, dedupe); err != nil {
				return err
			}

		case types.ResponsesToolCustom:
			if tool.Name == "" {
				*warns = append(*warns, fmt.Sprintf("tool %d (%s): custom tool without name dropped", i, clipWarnValue(origin)))
				continue
			}
			if err := t.add(tool.Name, tool.Description, origin,
				responsesCustomSchema(tool.Description, tool.Format), dedupe); err != nil {
				return err
			}
			mapping.markCustom(tool.Name)

		case types.ResponsesToolToolSearch:
			if t.search {
				// 重复声明同一个代理工具：幂等忽略（它不是客户端工具，重名没有歧义）
				continue
			}
			if err := t.add(responsesToolSearchName, respinToolSearchHelp, origin,
				respinToolSearchSchema, dedupe); err != nil {
				return err
			}
			t.search = true
			mapping.ToolSearch = true

		case types.ResponsesToolNamespace:
			if err := t.declareNamespace(tool, origin, dedupe, mapping, warns); err != nil {
				return err
			}

		default:
			*warns = append(*warns, fmt.Sprintf(
				"tool %s: unsupported tool type %q dropped (upstream has no server-side tool for it)",
				responsesToolLabel(tool), tool.Type))
		}
	}
	return nil
}

// declareNamespace 把 namespace 的子工具摊平成 <ns>__<child> 的普通 function 工具。
func (t *respinToolSet) declareNamespace(ns *types.ResponsesTool, origin string, dedupe bool,
	mapping *ResponsesToolMapping, warns *[]string) error {
	if ns.Name == "" {
		*warns = append(*warns, fmt.Sprintf("namespace tool without name dropped (%s)", clipWarnValue(origin)))
		return nil
	}
	nsOrigin := fmt.Sprintf("namespace %q", clipWarnValue(ns.Name))
	for j := range ns.Tools {
		child := &ns.Tools[j]
		if child.Name == "" {
			*warns = append(*warns, fmt.Sprintf("%s: child tool without name dropped", nsOrigin))
			continue
		}
		flat := flattenNamespaceToolName(ns.Name, child.Name)
		schema := child.Parameters
		custom := false
		switch child.Type {
		case "", types.ResponsesToolFunction:
		case types.ResponsesToolCustom:
			schema = responsesCustomSchema(child.Description, child.Format)
			custom = true
		default:
			*warns = append(*warns, fmt.Sprintf(
				"%s: child %q of unsupported type %q dropped", nsOrigin, child.Name, child.Type))
			continue
		}
		if err := t.add(flat, child.Description, nsOrigin, schema, dedupe); err != nil {
			return err
		}
		// 还原信息只在摊平成功后才登记：出站看到这个名字时要还原成 namespace 子工具的形态。
		mapping.markNamespace(flat, NamespacedName{Namespace: ns.Name, Name: child.Name})
		if custom {
			mapping.markCustom(flat)
		}
	}
	return nil
}

// promote 登记 tool_search_output 的发现结果：追加到工具清单末尾，同名同 schema 去重，
// 同名不同 schema 直接报错——静默保留旧的声明会让模型永远看不到客户端刚发现的新工具。
func (t *respinToolSet) promote(list []types.ResponsesTool, mapping *ResponsesToolMapping, warns *[]string) error {
	const origin = "a tool_search_output discovery result"
	for i := range list {
		tool := &list[i]
		if tool.Name == "" {
			*warns = append(*warns, "discovered tool without name dropped")
			continue
		}
		schema := tool.Parameters
		switch tool.Type {
		case "", types.ResponsesToolFunction:
		case types.ResponsesToolCustom:
			schema = responsesCustomSchema(tool.Description, tool.Format)
		default:
			*warns = append(*warns, fmt.Sprintf(
				"discovered tool %q of unsupported type %q dropped", tool.Name, tool.Type))
			continue
		}
		if prev, ok := t.origins[tool.Name]; ok {
			if t.schemas[tool.Name] == string(normalizeSchema(schema)) {
				// 同一份声明的重发：幂等跳过（不算冲突）
				*warns = append(*warns, fmt.Sprintf(
					"info: discovered tool %q already declared (%s); skipped", tool.Name, prev))
				continue
			}
			return fmt.Errorf(
				"discovered tool conflicts with an existing declaration: %q (declared by %s, discovered schema differs); rename it or drop the stale declaration",
				tool.Name, prev)
		}
		if err := t.add(tool.Name, tool.Description, origin, schema, false); err != nil {
			return err
		}
		if tool.Type == types.ResponsesToolCustom {
			mapping.markCustom(tool.Name)
		}
	}
	return nil
}

// responsesTools 按优先级登记全部工具声明：顶层 tools → additional_tools item → 发现提升。
func responsesTools(top, additional, discovered []types.ResponsesTool,
	mapping *ResponsesToolMapping, warns *[]string) ([]types.Tool, error) {
	set := &respinToolSet{schemas: map[string]string{}, origins: map[string]string{}}
	if err := set.declare(top, "the top-level tools array", false, mapping, warns); err != nil {
		return nil, err
	}
	if err := set.declare(additional, "an additional_tools input item", true, mapping, warns); err != nil {
		return nil, err
	}
	if err := set.promote(discovered, mapping, warns); err != nil {
		return nil, err
	}
	if len(set.replaced) > 0 {
		// 与 Anthropic/Chat 侧 convertTools 同口径：同请求聚合成一条，附工具名。
		*warns = append(*warns, fmt.Sprintf(
			"tool parameters replaced with {\"type\":\"object\",\"properties\":{}} for %d tool(s) [%s]: the cmdc envelope requires input_schema to be an object and these Responses tool declarations were not (no parameters at all, top-level anyOf/oneOf, \"type\" missing or not \"object\", or invalid JSON). The model now sees them as parameterless tools, so their calls may arrive with empty or missing arguments — if one of them is affected and you need its real parameters, check the client-side tool declaration.",
			len(set.replaced), strings.Join(set.replaced, ", ")))
	}
	return set.tools, nil
}

// responsesCustomSchema custom 工具降级后的固定 schema：单个 input:string 参数。
// grammar（format.syntax/definition）拼进参数描述——上游没有语法约束能力，
// 但把文法写进描述能让模型自己遵守（这是 Codex 侧 apply_patch 之类工具的关键提示）。
func responsesCustomSchema(description string, format *types.ResponsesToolFormat) json.RawMessage {
	desc := strings.TrimRight(description, "\n")
	if format != nil && (format.Syntax != "" || format.Definition != "") {
		grammar := "Format:\n```" + format.Syntax + "\n" + format.Definition + "\n```"
		if desc == "" {
			desc = grammar
		} else {
			desc += "\n\n" + grammar
		}
	}
	return chatJSON(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"input": map[string]any{"type": "string", "description": desc},
		},
		"required": []string{"input"},
	})
}

// flattenNamespaceToolName 摊平 namespace 子工具名：<ns>__<child>。
// 超过 64 字符（Codex 的工具名上限）时按 rune 截断并追加 "__" + sha256 前 4 字节 hex：
// 后缀由完整的摊平名决定，因此出站还原与后续轮次重算出的名字始终一致。
func flattenNamespaceToolName(namespace, child string) string {
	flat := namespace + "__" + child
	if utf8.RuneCountInString(flat) <= respinToolNameMax {
		return flat
	}
	sum := sha256.Sum256([]byte(flat))
	suffix := hex.EncodeToString(sum[:4])
	return truncateRunes(flat, respinToolNameMax-len("__")-len(suffix)) + "__" + suffix
}

// truncateRunes 按 rune 截断。不能切字节：中文命名空间切成半个字符会产出非法工具名。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// responsesToolList 宽松解析工具数组（additional_tools / tool_search_output 的 tools 字段）。
func responsesToolList(raw json.RawMessage) ([]types.ResponsesTool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var tools []types.ResponsesTool
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, err
	}
	return tools, nil
}

// responsesToolLabel 工具在留痕文案里的显示名。
func responsesToolLabel(t *types.ResponsesTool) string {
	if t.Name != "" {
		return t.Name
	}
	return "<unnamed>"
}

// ---------- tool_choice ----------

// responsesToolChoice 映射 Responses tool_choice → Anthropic 风味 {type,name}。
// required → any 是协议事实映射；allowed_tools 无上游等价约束（表达不了「只允许这几个」），
// 降级 auto 并留痕；工具全被丢弃时整个字段不设置——指向不存在工具的强制调用会被上游拒收。
func responsesToolChoice(raw json.RawMessage, hasTools bool, warns *[]string) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if !hasTools {
		*warns = append(*warns, "tool_choice ignored: no tool survived conversion")
		return nil
	}

	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return chatJSON(types.CcToolChoice{Type: "auto"})
		case "none":
			return chatJSON(types.CcToolChoice{Type: "none"})
		case "required":
			return chatJSON(types.CcToolChoice{Type: "any"})
		default:
			*warns = append(*warns, fmt.Sprintf("unknown tool_choice %q; falling back to auto", clipWarnValue(s)))
			return chatJSON(types.CcToolChoice{Type: "auto"})
		}
	}

	var tc struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		*warns = append(*warns, "unparsable tool_choice; falling back to auto")
		return chatJSON(types.CcToolChoice{Type: "auto"})
	}
	name := tc.Function.Name
	if name == "" {
		name = tc.Name
	}
	switch tc.Type {
	case "function", "custom":
		// custom 工具已被降级为同名 function 工具，指名强制调用时按普通工具处理。
		if name == "" {
			*warns = append(*warns, "tool_choice function without name; falling back to auto")
			return chatJSON(types.CcToolChoice{Type: "auto"})
		}
		return chatJSON(types.CcToolChoice{Type: "tool", Name: name})
	case "auto":
		return chatJSON(types.CcToolChoice{Type: "auto"})
	case "none":
		return chatJSON(types.CcToolChoice{Type: "none"})
	case "required", "any":
		return chatJSON(types.CcToolChoice{Type: "any"})
	case "allowed_tools":
		*warns = append(*warns,
			"tool_choice allowed_tools downgraded to auto (upstream cannot express an allow-list)")
		return chatJSON(types.CcToolChoice{Type: "auto"})
	default:
		*warns = append(*warns, fmt.Sprintf("unknown tool_choice type %q; falling back to auto", clipWarnValue(tc.Type)))
		return chatJSON(types.CcToolChoice{Type: "auto"})
	}
}

// ---------- mapping 登记 ----------

// markCustom 登记一个降级为 function 的 custom 工具（懒建 map：无降级工具时保持 nil）。
func (m *ResponsesToolMapping) markCustom(name string) {
	if m.Custom == nil {
		m.Custom = map[string]bool{}
	}
	m.Custom[name] = true
}

// markNamespace 登记摊平名到原始 namespace/子工具名的还原信息。
func (m *ResponsesToolMapping) markNamespace(flat string, orig NamespacedName) {
	if m.Namespace == nil {
		m.Namespace = map[string]NamespacedName{}
	}
	m.Namespace[flat] = orig
}
