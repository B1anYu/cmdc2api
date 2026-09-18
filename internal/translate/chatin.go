package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/B1anYu/cmdc2api/internal/types"
)

// ErrChatUnsupportedN 客户端要求多候选采样（n>1）。上游没有多候选生成能力，
// 只能显式拒绝：静默按 n=1 返回会让按 index 归并候选的客户端拿到残缺结果。
var ErrChatUnsupportedN = errors.New("only n=1 is supported")

// ChatToRequest 把 OpenAI Chat Completions 请求归一化为规范格式（Anthropic 形状）。
// 归一化产物直接进共享管线，会话亲和、缓存断点合成、伪装与 usage 换算全部复用既有实现。
//
// 返回的 warns 描述所有被降级/丢弃的内容；err 仅用于无法归一化的硬失败（当前只有 n>1）。
//
// 注意：安全丢弃字段的存在性探测需要原始报文，handler 应优先使用 ChatToRequestBody，
// 否则 frequency_penalty/logit_bias 这类无类型字段不会留痕（与 Responses 侧同约定）。
func ChatToRequest(req *types.ChatRequest) (*types.Request, []string, error) {
	return ChatToRequestBody(nil, req)
}

// ChatToRequestBody 同 ChatToRequest，但额外把原始报文中「有上游能力缺口」的字段留痕并入 warns。
// 这些字段在 ChatRequest 里没有类型（见 types/chatcompletions.go），只有原始报文能证明
// 客户端确实发过它们；raw 为 nil（兼容入口）时不报这些字段。
func ChatToRequestBody(raw []byte, req *types.ChatRequest) (*types.Request, []string, error) {
	out, warns, err := chatToRequest(req)
	if err != nil {
		return nil, nil, err
	}
	return out, append(warns, chatIgnoredWarnings(raw)...), nil
}

// chatToRequest 归一化主流程（不含原始报文探测）。
func chatToRequest(req *types.ChatRequest) (*types.Request, []string, error) {
	var warns []string

	if req.N != nil && *req.N > 1 {
		return nil, nil, ErrChatUnsupportedN
	}

	out := &types.Request{
		Model:       req.Model,
		Stream:      req.Stream,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}
	// max_completion_tokens 是 max_tokens 的新名；两者同时出现时以前者为准（OpenAI 语义）。
	// ≤0 不在这里兜底：由 BuildCcRequest 统一填充默认上限。
	switch {
	case req.MaxCompletionTokens != nil:
		out.MaxTokens = *req.MaxCompletionTokens
	case req.MaxTokens != nil:
		out.MaxTokens = *req.MaxTokens
	}

	var systemParts []string
	var msgs []types.InboundMessage
	for i := range req.Messages {
		m := &req.Messages[i]
		switch m.Role {
		case "system", "developer":
			// developer 与 system 都并入 system：Anthropic 只有 system 一处能承载全局指令
			if txt := chatMessageText(m.Content); txt != "" {
				systemParts = append(systemParts, txt)
			}
		case "user":
			if blocks := chatUserBlocks(i, m, &warns); len(blocks) > 0 {
				msgs = append(msgs, types.InboundMessage{Role: "user", Content: InboundBlocksContent(blocks)})
			}
		case "assistant":
			if blocks := chatAssistantBlocks(i, m, &warns); len(blocks) > 0 {
				msgs = append(msgs, types.InboundMessage{Role: "assistant", Content: InboundBlocksContent(blocks)})
			}
		case "tool", "function":
			if msg, ok := chatToolResultMessage(i, m, &warns); ok {
				msgs = append(msgs, msg)
			}
		default:
			warns = append(warns, fmt.Sprintf("message %d: unsupported role %q dropped", i, m.Role))
		}
	}
	if len(systemParts) > 0 {
		out.System = chatJSON(strings.Join(systemParts, "\n\n"))
	}

	// 配对修复与 Responses 入站共用同一实现（三条不变式见 pairing.go）：
	// Chat 客户端把并行调用拆成多条 assistant 消息、把结果拆成多条 tool 消息是常态。
	repaired, pw := RepairToolPairing(msgs)
	warns = append(warns, pw...)
	out.Messages = repaired

	out.Tools = chatTools(req.Tools, &warns)
	hasTools := len(out.Tools) > 0
	if tc := chatToolChoice(req.ToolChoice, hasTools, &warns); tc != nil {
		out.ToolChoice = tc
	}
	// parallel_tool_calls=false 有上游载体（tool_choice.disable_parallel_tool_use），
	// 因此必须在 tool_choice 落地之后再合并，而不是当安全丢弃字段处理。
	parallelApplied := chatApplyParallelToolCalls(req.ParallelToolCalls, hasTools, &out.ToolChoice)
	if effort := chatReasoningEffort(req.ReasoningEffort, &warns); effort != "" {
		// 走 BuildCcRequest 的 adaptive 分支
		out.Thinking = chatJSON(struct {
			Type   string `json:"type"`
			Effort string `json:"effort"`
		}{Type: "adaptive", Effort: effort})
	}

	warns = append(warns, chatDroppedFieldWarnings(req, parallelApplied)...)
	return out, warns, nil
}

// chatToolChoiceParallel 带并行开关的 tool_choice 线上形态。
// 刻意不复用 types.CcToolChoice：并行开关是同名对象的兄弟字段，而 CcToolChoice 只描述
// type/name；这里序列化出的原文交给 BuildCcRequest 的 convertToolChoice 解析，
// 它同时读 disable_parallel_tool_use 落到 CcParams.ParallelToolCalls。
type chatToolChoiceParallel struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
}

// chatApplyParallelToolCalls 把 parallel_tool_calls=false 并入 toolChoice（就地改写）。
// 上游没有独立的并行开关，禁用并行只能经 tool_choice.disable_parallel_tool_use 表达，
// 故仅当显式为 false 且最终仍有工具时改写：
//   - 未声明 tool_choice → 补 {type:"auto", disable_parallel_tool_use:true}（可调用但不并行）；
//   - 已声明（对象或字符串形态）→ 就地并入标志，type/name 原样保留。
//
// 显式为 true 或未声明一律不动（上游默认即允许并行）。
// 返回是否成功表达：false 表示该字段仍须按安全丢弃留痕。
func chatApplyParallelToolCalls(parallel *bool, hasTools bool, toolChoice *json.RawMessage) bool {
	if parallel == nil || *parallel || !hasTools {
		return false
	}
	if len(*toolChoice) == 0 || string(*toolChoice) == "null" {
		*toolChoice = chatJSON(chatToolChoiceParallel{Type: "auto", DisableParallelToolUse: true})
		return true
	}
	// 字符串形态：当前链路里 chatToolChoice 已把 "auto"/"required" 等升级为对象，
	// 此处仍完整处理，使本函数对任意 tool_choice 原文都成立。
	var s string
	if json.Unmarshal(*toolChoice, &s) == nil {
		typ := "auto"
		switch s {
		case "none":
			typ = "none"
		case "required", "any":
			typ = "any"
		}
		*toolChoice = chatJSON(chatToolChoiceParallel{Type: typ, DisableParallelToolUse: true})
		return true
	}
	var merged chatToolChoiceParallel
	if err := json.Unmarshal(*toolChoice, &merged); err != nil || merged.Type == "" {
		// 形态不可识别：不猜（改写会让 tool_choice 整个失真），留下丢弃留痕
		return false
	}
	merged.DisableParallelToolUse = true
	*toolChoice = chatJSON(merged)
	return true
}

// chatDroppedFieldWarnings 安全丢弃清单：这些字段上游没有对应能力，
// 必须留痕（info: 前缀为良性，其余按警告计），否则客户端会以为限制已生效。
// parallelApplied 表示 parallel_tool_calls 已成功并入 tool_choice（见上），
// 此时字段并未丢失，不再留痕。
func chatDroppedFieldWarnings(req *types.ChatRequest, parallelApplied bool) []string {
	var warns []string
	// stop 是行为性丢失：客户端以为会在指定序列处截断，实际不会，必须按警告留痕。
	// 上游信封没有 stop 能力（Anthropic 的 stop_sequences 本身就在安全丢弃清单里），
	// 因此这里不做任何映射，只报「未生效」。
	if len(req.Stop) > 0 && string(req.Stop) != "null" {
		warns = append(warns, "stop ignored (upstream has no stop-sequence capability)")
	}
	if len(req.ResponseFormat) > 0 && string(req.ResponseFormat) != "null" {
		warns = append(warns, "response_format ignored (upstream has no structured-output mode)")
	}
	if req.ParallelToolCalls != nil && !parallelApplied {
		warns = append(warns, "parallel_tool_calls ignored (upstream has no per-request parallel-call toggle)")
	}
	if req.StreamOptions != nil {
		warns = append(warns, "info: stream_options ignored (usage chunk is always emitted, include_usage is always satisfied)")
	}
	if req.Seed != nil {
		warns = append(warns, "info: seed ignored (upstream sampling is not seedable)")
	}
	if req.User != "" {
		warns = append(warns, "info: user ignored (upstream has no end-user attribution field)")
	}
	if req.Logprobs != nil && *req.Logprobs {
		warns = append(warns, "info: logprobs ignored (upstream returns no log probabilities)")
	}
	if req.TopLogprobs != nil {
		warns = append(warns, "info: top_logprobs ignored (upstream returns no log probabilities)")
	}
	return warns
}

// chatIgnoredWarnReasons 无上游对应能力且**没有类型声明**（见 types/chatcompletions.go 的
// chatIgnoredFields）的顶层字段的留痕文案。分级与 Responses 侧一致：
// 良性丢失（上游本来就没有该维度的选择权）用 info: 前缀；行为性丢失按警告计。
var chatIgnoredWarnReasons = map[string]string{
	"frequency_penalty": "info: frequency_penalty ignored (upstream sampling has no repetition penalty)",
	"presence_penalty":  "info: presence_penalty ignored (upstream sampling has no repetition penalty)",
	"logit_bias":        "info: logit_bias ignored (upstream has no token-level bias control)",
	"prediction":        "info: prediction ignored (upstream has no predicted-outputs mode)",
	"extra_body":        "extra_body ignored (upstream has no corresponding parameters)",
}

// chatIgnoredWarnings 对原始报文做存在性探测，为每个「客户端发过、上游无能力」的字段
// 产出留痕（顺序由 types.ChatIgnoredFields 的声明顺序固定，同一请求的日志可复现）。
func chatIgnoredWarnings(raw []byte) []string {
	fields := types.ChatIgnoredFields(raw)
	if len(fields) == 0 {
		return nil
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f == "store" {
			// store 的协议默认值即 true（OpenAI 两个 API 皆然），显式 store:false 与代理行为
			// 完全一致；只有显式 true 才是真实能力缺失，按行为性丢失记 WARN。
			// 条件与级别和 Responses 侧 respin.go 的处理保持一致（双入站口径统一）。
			if chatStoreFlagTrue(raw) {
				out = append(out, "store ignored (no server-side response state is kept; resend the full history)")
			}
			continue
		}
		if reason, ok := chatIgnoredWarnReasons[f]; ok {
			out = append(out, reason)
			continue
		}
		// 新增而此处未登记：仍然留痕，避免静默丢弃（留痕本身比文案精确更重要）。
		out = append(out, fmt.Sprintf("info: %s ignored (no upstream equivalent)", f))
	}
	return out
}

// chatStoreFlagTrue 探测原始报文中 store 的值（仅显式 true 返回真；缺失/null/false 均否）。
func chatStoreFlagTrue(raw []byte) bool {
	var probe struct {
		Store *bool `json:"store"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return probe.Store != nil && *probe.Store
}

// ---------- content 解析 ----------

// parseChatContent 解析 message.content。两种形态互斥：字符串形态返回 text（parts 为 nil），
// 数组形态返回 parts（text 无意义）。null/缺失都视为空。
func parseChatContent(raw json.RawMessage) (string, []types.ChatContentPart, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil, nil
	}
	var parts []types.ChatContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, err
	}
	return "", parts, nil
}

// chatMessageText 抽取 system/developer 消息的纯文本：字符串直取，part 数组只取文本块，
// 非文本 part 静默忽略（系统提示里的图片本就没有上游载体）。
func chatMessageText(raw json.RawMessage) string {
	text, parts, err := parseChatContent(raw)
	if err != nil {
		return ""
	}
	if parts == nil {
		return text
	}
	var texts []string
	for _, p := range parts {
		if isChatTextPart(p.Type) && p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// isChatTextPart 文本 part 的类型白名单：Chat 用 "text"，部分网关沿用 Responses 的
// input_text/output_text 写法，一并接受（内容语义相同）。
func isChatTextPart(typ string) bool {
	return typ == "text" || typ == "input_text" || typ == "output_text"
}

// chatUserBlocks user 消息 content → text/image 块。
// 图片按 URL 形态分流：data: URI 解成 base64 source，http(s) URL 原样交给上游抓取
// （代理端不做下载：零依赖且没有可控的超时/体积上限，会把请求路径变成不可控的阻塞点）。
func chatUserBlocks(idx int, m *types.ChatMessage, warns *[]string) []types.Block {
	text, parts, err := parseChatContent(m.Content)
	if err != nil {
		*warns = append(*warns, fmt.Sprintf("message %d (user): content is neither string nor part array; dropped", idx))
		return nil
	}
	if parts == nil {
		if text == "" {
			return nil
		}
		return []types.Block{{Type: "text", Text: text}}
	}
	blocks := make([]types.Block, 0, len(parts))
	for _, p := range parts {
		switch {
		case isChatTextPart(p.Type):
			if p.Text == "" {
				continue
			}
			blocks = append(blocks, types.Block{Type: "text", Text: p.Text})
		case p.Type == "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				*warns = append(*warns, fmt.Sprintf("message %d (user): image_url without url dropped", idx))
				continue
			}
			src, ok := chatImageSource(p.ImageURL.URL)
			if !ok {
				*warns = append(*warns, fmt.Sprintf("message %d (user): image with unsupported url scheme dropped", idx))
				continue
			}
			blocks = append(blocks, types.Block{Type: "image", Source: src})
		default:
			*warns = append(*warns, fmt.Sprintf("message %d (user): unsupported content part %q dropped", idx, p.Type))
		}
	}
	return blocks
}

// chatImageSource 图片 URL → Anthropic image source。
func chatImageSource(url string) (*types.ImageSource, bool) {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		meta, data, ok := strings.Cut(rest, ",")
		if !ok || data == "" {
			return nil, false
		}
		media, encoding, ok := strings.Cut(meta, ";")
		if !ok || !strings.EqualFold(encoding, "base64") {
			return nil, false
		}
		if media == "" {
			media = "image/png"
		}
		return &types.ImageSource{Type: "base64", MediaType: media, Data: data}, true
	}
	if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
		return &types.ImageSource{Type: "url", URL: url}, true
	}
	return nil, false
}

// chatAssistantBlocks assistant 消息 content → 块序列。
// 次序固定为 [thinking, text, tool-call]（对齐 CC CLI 抓包，见 request.go convertAssistant）：
// 上游对思考块的相对位置敏感，历史里的块次序不能随客户端书写习惯漂移。
func chatAssistantBlocks(idx int, m *types.ChatMessage, warns *[]string) []types.Block {
	var thinking, texts, toolUses []types.Block

	if rc := chatReasoningText(m); rc != "" {
		thinking = append(thinking, types.Block{Type: "thinking", Thinking: rc})
	}

	text, parts, err := parseChatContent(m.Content)
	switch {
	case err != nil:
		*warns = append(*warns, fmt.Sprintf("message %d (assistant): content is neither string nor part array; dropped", idx))
	case parts == nil:
		if text != "" {
			texts = append(texts, types.Block{Type: "text", Text: text})
		}
	default:
		for _, p := range parts {
			if isChatTextPart(p.Type) {
				if p.Text != "" {
					texts = append(texts, types.Block{Type: "text", Text: p.Text})
				}
				continue
			}
			*warns = append(*warns, fmt.Sprintf("message %d (assistant): unsupported content part %q dropped", idx, p.Type))
		}
	}

	if m.FunctionCall != nil {
		// legacy 单工具调用没有 id，无法与 tool 消息配对，转换出去只会变成悬空调用。
		// 这里预告下游后果：客户端同时发了 role:"function" 的结果消息时，它会因为
		// 配不上任何 tool_use 而被丢弃（见 pairing.go 的 orphan 留痕），
		// 两条 warn 连起来才能解释「工具结果为什么没了」。
		*warns = append(*warns, fmt.Sprintf(
			"message %d (assistant): legacy function_call dropped (no id to pair with a tool result; "+
				`its role:"function" output will fail to pair and be dropped)`, idx))
	}

	for j := range m.ToolCalls {
		tc := &m.ToolCalls[j]
		if tc.Function.Name == "" {
			*warns = append(*warns, fmt.Sprintf("message %d (assistant): tool_call without function name dropped", idx))
			continue
		}
		if tc.ID == "" {
			// 没有 id 就无法与 tool 消息配对，配对修复阶段也只会把它当悬空调用丢掉
			*warns = append(*warns, fmt.Sprintf("message %d (assistant): tool_call %q without id dropped", idx, tc.Function.Name))
			continue
		}
		toolUses = append(toolUses, types.Block{
			Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: chatToolArgs(tc.Function.Arguments),
		})
	}

	blocks := make([]types.Block, 0, len(thinking)+len(texts)+len(toolUses))
	blocks = append(blocks, thinking...)
	blocks = append(blocks, texts...)
	blocks = append(blocks, toolUses...)
	return blocks
}

// chatReasoningText 抽取 assistant 消息的推理文本。
// reasoning_content 是主流字段；reasoning / reason 是别名，且可能是字符串或对象
// （对象形态取常见的 content / text 键）。
func chatReasoningText(m *types.ChatMessage) string {
	if m.ReasoningContent != "" {
		return m.ReasoningContent
	}
	for _, raw := range []json.RawMessage{m.Reasoning, m.Reason} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if s != "" {
				return s
			}
			continue
		}
		var obj struct {
			Content string `json:"content"`
			Text    string `json:"text"`
		}
		if err := json.Unmarshal(raw, &obj); err == nil {
			if obj.Content != "" {
				return obj.Content
			}
			if obj.Text != "" {
				return obj.Text
			}
		}
	}
	return ""
}

// chatToolArgs 解析工具调用参数。空串或非法 JSON 兜底为 {}：
// Anthropic 的 tool_use.input 必须是对象，半截 JSON 混进历史会让后续每一轮都 400。
func chatToolArgs(args string) json.RawMessage {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(trimmed)
}

// chatToolResultMessage tool（含 legacy function）消息 → user 消息 + tool_result 块。
// legacy function 角色没有 tool_call_id，用函数名充当（与 §7.2 的约定一致）。
func chatToolResultMessage(idx int, m *types.ChatMessage, warns *[]string) (types.InboundMessage, bool) {
	callID := m.ToolCallID
	if callID == "" && m.Role == "function" {
		callID = m.Name
	}
	if callID == "" {
		*warns = append(*warns, fmt.Sprintf("message %d (tool): missing tool_call_id, dropped", idx))
		return types.InboundMessage{}, false
	}
	content := chatToolResultContent(idx, m.Content, warns)
	return types.InboundMessage{
		Role: "user",
		Content: InboundBlocksContent([]types.Block{{
			Type: "tool_result", ToolUseID: callID, Content: content,
		}}),
	}, true
}

// chatToolResultContent tool 消息 content → tool_result.content。
// 字符串原样保留（空串也保留：命令无输出是常态，不能编造占位文本）；
// part 数组按 text/image 白名单重建，图片交给 convertUser 的既有搬迁逻辑处理。
func chatToolResultContent(idx int, raw json.RawMessage, warns *[]string) json.RawMessage {
	text, parts, err := parseChatContent(raw)
	if err != nil {
		*warns = append(*warns, fmt.Sprintf("message %d (tool): content is neither string nor part array; emptied", idx))
		return chatJSON("")
	}
	if parts == nil {
		return chatJSON(text)
	}
	blocks := make([]types.Block, 0, len(parts))
	for _, p := range parts {
		switch {
		case isChatTextPart(p.Type):
			if p.Text == "" {
				continue
			}
			blocks = append(blocks, types.Block{Type: "text", Text: p.Text})
		case p.Type == "image_url":
			if p.ImageURL == nil {
				*warns = append(*warns, fmt.Sprintf("message %d (tool): image_url without url dropped", idx))
				continue
			}
			src, ok := chatImageSource(p.ImageURL.URL)
			if !ok {
				*warns = append(*warns, fmt.Sprintf("message %d (tool): image with unsupported url scheme dropped", idx))
				continue
			}
			blocks = append(blocks, types.Block{Type: "image", Source: src})
		default:
			*warns = append(*warns, fmt.Sprintf("message %d (tool): unsupported content part %q dropped", idx, p.Type))
		}
	}
	if len(blocks) == 0 {
		return chatJSON("")
	}
	return InboundBlocksContent(blocks)
}

// ---------- 工具 / tool_choice / thinking ----------

// chatTools 映射 tools 数组。只认 function 类型（含 type 缺省）：
// 其余类型（computer_use、web_search、mcp 等）没有上游载体，留痕丢弃。
func chatTools(tools []types.ChatTool, warns *[]string) []types.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]types.Tool, 0, len(tools))
	for i := range tools {
		t := &tools[i]
		if t.Type != "" && t.Type != "function" {
			*warns = append(*warns, fmt.Sprintf("tool %d (%s): unsupported tool type %q dropped", i, chatToolName(t), t.Type))
			continue
		}
		if t.Function == nil || t.Function.Name == "" {
			*warns = append(*warns, fmt.Sprintf("tool %d: function definition without name dropped", i))
			continue
		}
		if t.Function.Strict != nil && *t.Function.Strict {
			*warns = append(*warns, fmt.Sprintf("tool %s: strict schema flag dropped (upstream has no strict-schema mode)", t.Function.Name))
		}
		out = append(out, types.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters, // 形状校验交给 BuildCcRequest 的 normalizeSchema
		})
	}
	return out
}

func chatToolName(t *types.ChatTool) string {
	if t.Function != nil && t.Function.Name != "" {
		return t.Function.Name
	}
	return "<unnamed>"
}

// chatToolChoice 映射 OpenAI tool_choice → Anthropic 风味 {type,name}。
// required → any 是协议事实映射；工具全被丢弃时整个字段不设置——
// 带 name 的强制调用指向不存在的工具会被上游直接拒收。
func chatToolChoice(raw json.RawMessage, hasTools bool, warns *[]string) json.RawMessage {
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
			*warns = append(*warns, fmt.Sprintf("unknown tool_choice %q; falling back to auto", s))
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
	case "function":
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
		// Responses 的形态，上游没有等价约束（无法表达「只允许这几个」）
		*warns = append(*warns, "tool_choice allowed_tools downgraded to auto (upstream cannot express an allow-list)")
		return chatJSON(types.CcToolChoice{Type: "auto"})
	default:
		*warns = append(*warns, fmt.Sprintf("unknown tool_choice type %q; falling back to auto", tc.Type))
		return chatJSON(types.CcToolChoice{Type: "auto"})
	}
}

// chatReasoningEffort reasoning_effort → cmdc reasoning_effort 档位（走 adaptive 分支）。
// 上游只认 low/medium/high，因此 minimal→low、xhigh/max→high 做档位收敛；
// none/空表示不启用思考，不设置字段。
func chatReasoningEffort(raw json.RawMessage, warns *[]string) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	effort := ""
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		effort = s
	} else {
		// 对象形态：{effort, summary}（Responses→Chat 桥接层会传下来）
		var obj struct {
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			*warns = append(*warns, "unparsable reasoning_effort ignored")
			return ""
		}
		effort = obj.Effort
	}
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "", "none":
		return ""
	case "low", "medium", "high":
		return strings.ToLower(strings.TrimSpace(effort))
	case "minimal":
		return "low"
	case "xhigh", "max":
		return "high"
	default:
		*warns = append(*warns, fmt.Sprintf("unknown reasoning_effort %q ignored", effort))
		return ""
	}
}

// chatJSON 序列化归一化产物。喂入的都是自造值，失败只可能来自将来的误用，
// 因此返回 nil 而不 panic：单个字段留空好过整条请求炸掉。
func chatJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
