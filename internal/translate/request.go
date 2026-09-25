// Package translate 实现协议转换：Anthropic Messages → cmdc 信封单跳直转，
// 以及 cmdc NDJSON 流 → Anthropic SSE/JSON 响应转换。采用纯函数与无锁状态机设计。
package translate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/B1anYu/cmdc2api/internal/types"
)

// DefaultModel 请求未指定 model 时的兜底默认模型。
const DefaultModel = "deepseek/deepseek-v4-flash"

// BuildOpts 信封环境参数（由 handler 注入，随会话/时间变化）。
type BuildOpts struct {
	Now                time.Time
	NodeVersion        string // 伪装的 Node.js 版本（如 "v22.21.0"）
	WorkingDir         string // win32 风格伪装路径，由会话派生
	AssistantReasoning bool   // 将 assistant thinking 块回传上游（对齐 CC CLI 并避免多轮思考 502）
	// CacheMarkers 缓存断点策略：
	// "" / "respect"（默认：透传客户端 part 级标记，缺失时在末尾合成）；
	// "replace"（剥除客户端标记，强制在末尾合成，用于诊断）。
	CacheMarkers string
}

// BuildCcRequest 把 Anthropic 请求直转为 cmdc 信封。
// 返回的 warnings 描述所有被降级/丢弃的内容，供日志观测。
func BuildCcRequest(req *types.Request, opts BuildOpts) (*types.CcRequest, []string) {
	var warns []string

	sysText, sysCC := parseSystem(req.System)
	tools, toolsCC := convertTools(req.Tools, &warns)

	// 预解析全部消息（单次解析，避免大 body 反复 unmarshal）
	parsed := make([]parsedMessage, 0, len(req.Messages))
	toolNames := make(map[string]string)
	for i := range req.Messages {
		m := &req.Messages[i]
		blocks, err := parseBlocks(m.Content)
		if err != nil {
			warns = append(warns, fmt.Sprintf("message %d (role %s): content not string/block-array, dropped: %v", i, clipWarnValue(m.Role), err))
			continue
		}
		parsed = append(parsed, parsedMessage{
			role:             m.Role,
			blocks:           blocks,
			reasoningContent: m.ReasoningContent,
		})
		if m.Role == "assistant" {
			for _, b := range blocks {
				if b.Type == "tool_use" && b.ID != "" {
					toolNames[b.ID] = b.Name
				}
			}
		}
	}

	// CC_CACHE_MARKERS=replace（诊断模式）：剥除全部入站 part 级标记并强制在末尾合成断点
	replaceMarkers := opts.CacheMarkers == "replace"
	if replaceMarkers {
		stripped := 0
		for i := range parsed {
			for j := range parsed[i].blocks {
				if parsed[i].blocks[j].CacheControl != nil {
					parsed[i].blocks[j].CacheControl = nil
					stripped++
				}
			}
		}
		warns = append(warns, fmt.Sprintf(
			"info: CC_CACHE_MARKERS=replace stripped %d inbound part-level marker(s); breakpoint will be synthesized at the envelope tail", stripped))
	}

	var ccMsgs []types.CcMessage
	for i := range parsed {
		pm := &parsed[i]
		if pm.role == "assistant" {
			ccMsgs = append(ccMsgs, convertAssistant(pm, opts, &warns)...)
		} else {
			// user 及未知角色兜底按 user 处理
			ccMsgs = append(ccMsgs, convertUser(pm, toolNames, &warns)...)
		}
	}
	if len(ccMsgs) == 0 {
		warns = append(warns, "request produced no convertible messages")
	}

	// 记录信封中实际存在的 part 级 cache_control 标记位置，供排查缓存命中状态
	var markerIdx []int
	for i := range ccMsgs {
		for _, p := range ccMsgs[i].Content {
			if p.CacheControl != nil {
				markerIdx = append(markerIdx, i)
				break
			}
		}
	}
	if len(markerIdx) > 0 {
		warns = append(warns, fmt.Sprintf(
			"info: %d part-level cache_control marker(s) present in envelope at message indexes %v",
			len(markerIdx), markerIdx))
	}

	// 断点合成的三个来源：
	//   ① 客户端把标记打在 system/tools 上（信封无对应字段，只能折算到 part 级）；
	//   ② CC_CACHE_MARKERS=replace 强制合成（仅 A/B 诊断）；
	//   ③ 入站协议本就没有 cache_control 字段（OpenAI Chat/Responses）——见
	//      types.Request.SynthesizeTailCacheMarker 的说明。
	// synthesizeCacheMarker 内部若发现信封里已有任一 part 级标记则直接返回，
	// 故 ①③ 都不会覆写客户端标记。
	synthesizeCacheMarker(ccMsgs, sysCC || toolsCC || replaceMarkers || req.SynthesizeTailCacheMarker, replaceMarkers, &warns)

	model := req.Model
	if model == "" {
		model = DefaultModel
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 64000
	}
	if maxTokens > 200000 {
		// 上限保留（不提高、不改为纯透传）：钳制静默发生会让客户端以为请求的
		// max_tokens 生效，故必须留痕，附请求值与钳制后的值。
		warns = append(warns, fmt.Sprintf(
			"max_tokens %d exceeds the 200000 ceiling of the cmdc envelope and was clamped to 200000; the model will stop earlier than the client asked for. (The ceiling mirrors the reference client: reference/commandcode-proxy/proxy.mjs:500 Math.min(max_tokens || 64000, 200000); it has no upstream error receipt, so treat a hit here as a signal worth reporting.)",
			req.MaxTokens))
		maxTokens = 200000
	}

	cc := &types.CcRequest{
		Config: types.CcConfig{
			WorkingDir:    opts.WorkingDir,
			Date:          opts.Now.Format("2006-01-02"),
			Environment:   "win32-x64, Node.js " + opts.NodeVersion,
			Structure:     []any{},
			RecentCommits: []string{},
		},
		Memory:         nil,
		Taste:          nil,
		Skills:         "",
		PermissionMode: "standard",
		Params: types.CcParams{
			Model:     model,
			Messages:  ccMsgs,
			MaxTokens: maxTokens,
			Stream:    true, // cmdc 恒流式，非流式由代理端聚合
		},
	}
	if sysText != "" {
		cc.Params.System = sysText
	}
	if req.Temperature != nil {
		cc.Params.Temperature = req.Temperature
	}
	if effort := thinkingEffort(req.Thinking); effort != "" {
		cc.Params.ReasoningEffort = effort
	}
	if len(tools) > 0 {
		cc.Params.Tools = tools
	}
	if tc, parallel := convertToolChoice(req.ToolChoice, &warns); tc != nil {
		cc.Params.ToolChoice = tc
		cc.Params.ParallelToolCalls = parallel
	}
	return cc, warns
}

// PrefixCacheKey 决定本次请求的**派生会话键**（会话亲和把它当候选；显式 session 请求头仍优先）。
//
// 来源二选一：
//   - 客户端显式声明了 prompt_cache_key（OpenAI Chat/Responses 字段，长度 ≥8 才采用）——
//     这是客户端对「这条对话属于哪个缓存前缀」的显式声明，比我们从内容派生的哈希更权威，
//     口径对齐 A 级参照 reference/commandcode-proxy/proxy.mjs:204-210；
//   - 否则从可缓存前缀（system + tools + 首条用户消息文本）派生：同一对话的前缀在多轮
//     交互中保持不变，确保命中上游按会话粒度的 Prompt Cache；当工具定义、系统提示词或
//     首条消息发生压缩/变更时自动切换会话。
//
// 本函数输出还会进入 WorkingDirForSession 与上游请求头，故无论哪个来源都**统一再哈希一次**
// 并格式化为 UUID 形状，避免客户端可控字符串直接外泄到这些位置。
func PrefixCacheKey(req *types.Request) string {
	if k := req.PromptCacheKey; len(k) >= 8 {
		sum := sha256.Sum256([]byte("prompt_cache_key:" + k))
		return uuidShape(hex.EncodeToString(sum[:]))
	}
	h := sha256.New()
	sys, _ := parseSystem(req.System)
	_, _ = fmt.Fprintf(h, "system:%s\n", sys)
	for _, t := range req.Tools {
		_, _ = fmt.Fprintf(h, "tool:%s:%s\n", t.Name, t.InputSchema)
	}
	first := ""
	for i := range req.Messages {
		if req.Messages[i].Role == "user" {
			blocks, _ := parseBlocks(req.Messages[i].Content)
			var texts []string
			for _, b := range blocks {
				if b.Type == "text" {
					texts = append(texts, b.Text)
				}
			}
			first = strings.Join(texts, "\n")
			break
		}
	}
	if len(first) > 4096 {
		first = first[:4096]
	}
	_, _ = fmt.Fprintf(h, "first:%s", first)
	return uuidShape(hex.EncodeToString(h.Sum(nil)))
}

// uuidShape 把十六进制摘要的前 32 个字符格式化为 UUID 形状（8-4-4-4-12）。
func uuidShape(sum string) string {
	return sum[0:8] + "-" + sum[8:12] + "-" + sum[12:16] + "-" + sum[16:20] + "-" + sum[20:32]
}

// ---------- 消息解析 ----------

type parsedMessage struct {
	role             string
	blocks           []types.Block
	reasoningContent string
}

// parseBlocks 解析 content 字段（string | []block；null 视为空）。
func parseBlocks(raw json.RawMessage) ([]types.Block, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []types.Block{{Type: "text", Text: s}}, nil
	}
	var blocks []types.Block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("content is neither string nor block array")
	}
	return blocks, nil
}

// parseSystem 解析 system 参数（支持 string 或 block 数组），返回拼接后的纯文本及是否包含 cache_control。
// 由于 cmdc 强制要求 system 为字符串，结构化块上的缓存标记需合并并在末尾文本块合成。
func parseSystem(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, false
	}
	var blocks []types.Block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", false
	}
	var texts []string
	hadCC := false
	for _, b := range blocks {
		if b.CacheControl != nil {
			hadCC = true
		}
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n"), hadCC
}

// ---------- user / assistant 转换 ----------

// convertUser 把 Anthropic user 消息转换为 cmdc 消息序列：
// text/image 块归入 user 消息，tool_result 块转换为独立的 tool 消息。
// 由于 cmdc 的 tool-result 仅支持文本，tool_result 内嵌的图片会自动提取并搬迁至后续 user 消息段中。
func convertUser(pm *parsedMessage, toolNames map[string]string, warns *[]string) []types.CcMessage {
	var out []types.CcMessage
	var userParts []types.CcPart
	var pendingReloc []types.CcPart

	// flushUser 输出当前 user 段，搬迁图片作为段首 part
	flushUser := func() {
		if len(userParts) > 0 {
			parts := append(pendingReloc, userParts...)
			pendingReloc = nil
			out = append(out, types.CcMessage{Role: "user", Content: parts})
			userParts = nil
		}
	}

	for _, b := range pm.blocks {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			userParts = append(userParts, types.CcPart{Type: "text", Text: b.Text, CacheControl: ccCC(b.CacheControl)})
		case "image":
			uri, ok := imageToCc(b.Source)
			if !ok {
				*warns = append(*warns, "image block with unsupported source dropped")
				continue
			}
			userParts = append(userParts, types.CcPart{Type: "image", Image: uri, CacheControl: ccCC(b.CacheControl)})
		case "tool_result":
			flushUser()
			name, known := toolNames[b.ToolUseID]
			if !known {
				// 历史记录被裁剪或压缩可能导致孤儿 tool_result（无对应 tool_use），丢弃并记录警告以避免上游校验失败
				*warns = append(*warns, "orphan tool_result dropped (no matching tool_use in history): "+clipWarnValue(b.ToolUseID))
				continue
			}
			value, images := toolResultContent(b.Content, warns)
			out = append(out, types.CcMessage{Role: "tool", Content: []types.CcPart{{
				Type:         "tool-result",
				ToolCallID:   b.ToolUseID,
				ToolName:     name,
				Output:       &types.CcToolOutput{Type: "text", Value: value},
				CacheControl: ccCC(b.CacheControl),
			}}})
			for _, img := range images {
				pendingReloc = append(pendingReloc,
					types.CcPart{Type: "text", Text: "[Tool output media for call " + b.ToolUseID + "]"},
					types.CcPart{Type: "image", Image: img},
				)
			}
		case "thinking", "redacted_thinking":
			*warns = append(*warns, "unexpected thinking block in user message dropped")
		case "document":
			*warns = append(*warns, "document block dropped (upstream has no document part)")
		default:
			*warns = append(*warns, "unknown user block type dropped: "+clipWarnValue(b.Type))
		}
	}
	flushUser()
	// 尾部没有后续 user 段可并入时，搬迁图片独立成 user 消息
	if len(pendingReloc) > 0 {
		out = append(out, types.CcMessage{Role: "user", Content: pendingReloc})
	}
	return out
}

func convertAssistant(pm *parsedMessage, opts BuildOpts, warns *[]string) []types.CcMessage {
	var reasoningParts []types.CcPart
	var textParts []types.CcPart
	var toolParts []types.CcPart
	thinkingDropped := false

	// 若消息级携带 reasoning_content，先放入 reasoning
	if pm.reasoningContent != "" {
		if opts.AssistantReasoning {
			reasoningParts = append(reasoningParts, types.CcPart{Type: "reasoning", Text: pm.reasoningContent})
		} else {
			thinkingDropped = true
		}
	}

	for _, b := range pm.blocks {
		switch b.Type {
		case "thinking", "reasoning":
			txt := b.Thinking
			if txt == "" {
				txt = b.Reasoning
			}
			if txt == "" {
				txt = b.ReasoningContent
			}
			if txt == "" {
				txt = b.Text
			}
			if txt == "" {
				continue
			}
			if opts.AssistantReasoning {
				if pm.reasoningContent != txt {
					reasoningParts = append(reasoningParts, types.CcPart{Type: "reasoning", Text: txt})
				}
			} else {
				thinkingDropped = true
			}
		case "redacted_thinking":
			// 无明文可还原，跳过
		case "text":
			if b.Text == "" {
				continue
			}
			textParts = append(textParts, types.CcPart{Type: "text", Text: b.Text, CacheControl: ccCC(b.CacheControl)})
		case "tool_use":
			input := b.Input
			if len(input) == 0 || string(input) == "null" {
				input = json.RawMessage("{}")
			}
			toolParts = append(toolParts, types.CcPart{
				Type:         "tool-call",
				ToolCallID:   b.ID, // 恒等透传，续轮配对依赖原始 ID
				ToolName:     b.Name,
				Input:        input,
				CacheControl: ccCC(b.CacheControl),
			})
		case "image":
			*warns = append(*warns, "image block in assistant message dropped")
		default:
			*warns = append(*warns, "unknown assistant block type dropped: "+clipWarnValue(b.Type))
		}
	}
	if thinkingDropped {
		*warns = append(*warns, "assistant thinking block(s) dropped (CC_ASSISTANT_REASONING=0)")
	}

	// 按照 CC CLI 抓包规范严格排序：[reasoning, text, tool-call]
	parts := make([]types.CcPart, 0, len(reasoningParts)+len(textParts)+len(toolParts))
	parts = append(parts, reasoningParts...)
	parts = append(parts, textParts...)
	parts = append(parts, toolParts...)

	if len(parts) == 0 {
		return nil
	}
	return []types.CcMessage{{Role: "assistant", Content: parts}}
}

// imageToCc Anthropic 图片源 → cmdc image 字段（data URI 或 URL 原样）。
func imageToCc(src *types.ImageSource) (string, bool) {
	if src == nil {
		return "", false
	}
	switch src.Type {
	case "base64":
		media := src.MediaType
		if media == "" {
			media = "image/png"
		}
		return "data:" + media + ";base64," + src.Data, true
	case "url":
		return src.URL, true
	default:
		return "", false
	}
}

// toolResultContent 解析 tool_result.content：文本块拼接为 value，
// 图片块提取出来等待搬迁。空内容给空串（cmdc 接受）。
func toolResultContent(raw json.RawMessage, warns *[]string) (string, []string) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []types.Block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		*warns = append(*warns, "tool_result content neither string nor block array; raw JSON retained")
		return string(raw), nil
	}
	var texts []string
	var images []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "image":
			if uri, ok := imageToCc(b.Source); ok {
				images = append(images, uri)
			} else {
				*warns = append(*warns, "image in tool_result with unsupported source dropped")
			}
		}
	}
	return strings.Join(texts, "\n\n"), images
}

// ccCC 原样保留客户端 part 级 cache_control（含 ttl），仅在 type 缺失时补 "ephemeral"。
// 绝不覆写、绝不合成 ttl：cmdc 之下还有它自己的上游（deepseek/qwen/anthropic/gpt 等），
// 覆写 ttl（例如统一成 1h）会真的拉低下游 provider 的默认缓存时长。详见 types.CacheControl 注释。
func ccCC(cc *types.CacheControl) *types.CacheControl {
	if cc == nil {
		return nil
	}
	out := *cc
	if out.Type == "" {
		out.Type = "ephemeral"
	}
	return &out
}

// ---------- tools / tool_choice / thinking ----------

func convertTools(tools []types.Tool, warns *[]string) ([]types.CcTool, bool) {
	if len(tools) == 0 {
		return nil, false
	}
	out := make([]types.CcTool, 0, len(tools))
	var replaced []string // 说明书被整体清空的工具名，按请求聚合成一条留痕
	hadCC := false
	for _, t := range tools {
		hadCC = hadCC || t.CacheControl != nil
		schema, wasReplaced := normalizeSchemaReport(t.InputSchema)
		if wasReplaced {
			replaced = append(replaced, t.Name)
		}
		out = append(out, types.CcTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	if len(replaced) > 0 {
		*warns = append(*warns, fmt.Sprintf(
			"tool input_schema replaced with {\"type\":\"object\",\"properties\":{}} for %d tool(s) [%s]: the cmdc envelope requires input_schema to be an object and these were not (top-level anyOf/oneOf, \"type\" missing or not \"object\", or invalid JSON). The model now sees them as parameterless tools, so their calls may arrive with empty or missing arguments — if one of them is affected and you need its real parameters, check the client-side tool declaration.",
			len(replaced), clipWarnValue(strings.Join(replaced, ", "))))
	}
	return out, hadCC
}

// normalizeSchema 规范化工具 input_schema（仅形状、不留痕）：
// 确保为合法 object 且包含 properties 字段，防止上游 400 校验错误。
// respin.go 的工具声明与去重键比较使用本入口；需要留痕的入站漏斗请用 normalizeSchemaReport。
func normalizeSchema(raw json.RawMessage) json.RawMessage {
	out, _ := normalizeSchemaReport(raw)
	return out
}

// normalizeSchemaReport 与 normalizeSchema 同实现，另返回「是否发生了替换」，
// 供 convertTools 按请求聚合成一条留痕（见其调用点文案）。
// 替换为有意保留的保守行为：透传顶层 anyOf/oneOf 无任何一级证据，而 MCP 工具的
// 联合 schema 一旦被上游拒绝会使整轮 400 —— 失败方向更糟。
func normalizeSchemaReport(raw json.RawMessage) (json.RawMessage, bool) {
	def := json.RawMessage(`{"type":"object","properties":{}}`)
	if len(raw) == 0 {
		return def, true
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return def, true
	}
	if t, ok := m["type"]; !ok || strings.TrimSpace(string(t)) != `"object"` {
		return def, true
	}
	if _, ok := m["properties"]; !ok {
		m["properties"] = json.RawMessage(`{}`)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return def, true
	}
	return b, false
}

func convertToolChoice(raw json.RawMessage, warns *[]string) (*types.CcToolChoice, *bool) {
	if len(raw) == 0 {
		return nil, nil
	}
	var tc struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse *bool  `json:"disable_parallel_tool_use"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil || tc.Type == "" {
		return nil, nil
	}
	var choice *types.CcToolChoice
	switch tc.Type {
	case "auto", "any", "tool", "none":
		if tc.Type == "tool" && tc.Name == "" {
			*warns = append(*warns, `tool_choice {"type":"tool"} carries no "name": it was forwarded to the cmdc upstream as a nameless {"type":"tool"}, which cannot resolve to any tool — expect either a validation error from upstream or the model calling an arbitrary tool. The Anthropic Messages API requires the tool name when type is "tool". (This shape can only reach the envelope from the Anthropic Messages inbound path: /v1/chat/completions and /v1/responses resolve tool_choice in their own inbound layer and fall back to auto with a warning when the name is missing.)`)
		}
		choice = &types.CcToolChoice{Type: tc.Type, Name: tc.Name}
	default:
		*warns = append(*warns, fmt.Sprintf(
			"tool_choice type %q is not one of auto/any/tool/none: it was downgraded to {\"type\":\"auto\"}, so the model may now call any tool (or none) instead of being constrained as the client asked. (Only the Anthropic Messages inbound path reaches this branch; /v1/chat/completions and /v1/responses normalize tool_choice in their own inbound layer and already log the unknown value there.)",
			clipWarnValue(tc.Type)))
		choice = &types.CcToolChoice{Type: "auto"}
	}
	var parallel *bool
	if tc.DisableParallelToolUse != nil {
		v := !*tc.DisableParallelToolUse
		parallel = &v
	}
	return choice, parallel
}

// thinkingEffort 将 Anthropic thinking 预算映射为 cmdc 的 reasoning_effort 档位（≥10000 为 high，≥5000 为 medium，>0 为 low）。
func thinkingEffort(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var th struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
		Effort       string `json:"effort"`
	}
	if err := json.Unmarshal(raw, &th); err != nil {
		return ""
	}
	switch th.Type {
	case "", "disabled", "none":
		return ""
	case "adaptive":
		if th.Effort != "" {
			return th.Effort
		}
		return "medium"
	}
	switch {
	case th.BudgetTokens >= 10000:
		return "high"
	case th.BudgetTokens >= 5000:
		return "medium"
	case th.BudgetTokens > 0:
		return "low"
	}
	return ""
}

// ---------- 缓存标记 ----------

// synthesizeCacheMarker 当入站请求未包含内容级断点（或需要折算 system/tools 标记）时，在信封末尾合成缓存断点：
// 从整条对话末尾向前回扫找到最后一个 text part 并附加 {type: "ephemeral"}。
// 必须从信封整体末尾向前回扫（而非仅回溯 user 消息），以确保 Agent 多轮工具调用（role: "tool"）场景下断点能随对话历史前进。
func synthesizeCacheMarker(msgs []types.CcMessage, need, force bool, warns *[]string) {
	if !need {
		return
	}
	for i := range msgs {
		for j := range msgs[i].Content {
			if msgs[i].Content[j].CacheControl != nil {
				return // 已有 part 级标记
			}
		}
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		for j := len(msgs[i].Content) - 1; j >= 0; j-- {
			if msgs[i].Content[j].Type == "text" {
				msgs[i].Content[j].CacheControl = &types.CacheControl{Type: "ephemeral"}
				if !force {
					*warns = append(*warns,
						"no part-level cache_control present in the envelope; synthesized a breakpoint on the tail's last text part. The inbound request carried no usable part-level marker, which happens when either (a) it declared cache_control only on system/tools and the cmdc envelope has no field to carry those, or (b) its protocol has no cache_control field at all (OpenAI Chat/Responses).")
				}
				return
			}
		}
	}
	*warns = append(*warns, "no text part in the envelope can carry a cache breakpoint, so none was synthesized. The inbound request carried no usable part-level marker — either it declared cache_control only on system/tools, which the cmdc envelope has no field to carry, or its protocol has no cache_control field at all (OpenAI Chat/Responses) — and the envelope's tail has no text part to attach one to. This request goes upstream with no part-level breakpoint; only session affinity can still reach the cache.")
}
