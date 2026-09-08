// Package translate 实现协议转换：Anthropic Messages → cmdc 信封（请求直转，
// 不经 OpenAI chat 中间层），cmdc NDJSON → Anthropic SSE/JSON（响应方向）。
// 架构沿用 sub2api 的纯函数 + 状态机三段式，无 goroutine。
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

// DefaultModel 请求未带 model 时的默认值（原版 buildCcRequest 默认）。
const DefaultModel = "deepseek/deepseek-v4-flash"

// BuildOpts 信封环境参数（由 handler 注入，随会话/时间变化）。
type BuildOpts struct {
	Now                time.Time
	NodeVersion        string // 例 "v22.21.0"
	WorkingDir         string // win32 风格伪装路径，由会话派生
	AssistantReasoning bool   // 实验开关：assistant thinking 块回传上游
	// CacheMarkers 断点策略："" / "respect"（默认，客户端 part 级标记
	// 透传，缺失才末尾合成）；"replace"（剥掉客户端标记，强制末尾合成，
	// 用于 A/B 验证 cmdc 对客户端标记的消费语义）
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
			warns = append(warns, fmt.Sprintf("message %d (role %s): content not string/block-array, dropped: %v", i, m.Role, err))
			continue
		}
		parsed = append(parsed, parsedMessage{role: m.Role, blocks: blocks})
		if m.Role == "assistant" {
			for _, b := range blocks {
				if b.Type == "tool_use" && b.ID != "" {
					toolNames[b.ID] = b.Name
				}
			}
		}
	}

	// CC_CACHE_MARKERS=replace（A/B 用）：剥掉全部入站 part 级标记，
	// 断点一律由末尾合成 —— 用于验证 cmdc 对客户端原生标记的消费语义
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
			// user 及未知角色兜底为 user（对齐原版）
			ccMsgs = append(ccMsgs, convertUser(pm, toolNames, &warns)...)
		}
	}
	if len(ccMsgs) == 0 {
		warns = append(warns, "request produced no convertible messages")
	}

	// 断点可观测（info 级）：记录信封里实际存在的 part 级标记落点，
	// 与 synthesizeCacheMarker 的告警互斥互补——两条日志二选一出现，
	// 即可判定客户端断点是在透传还是中途丢失
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

	synthesizeCacheMarker(ccMsgs, sysCC || toolsCC || replaceMarkers, replaceMarkers, &warns)

	model := req.Model
	if model == "" {
		model = DefaultModel
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 64000
	}
	if maxTokens > 200000 {
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
	if tc, parallel := convertToolChoice(req.ToolChoice); tc != nil {
		cc.Params.ToolChoice = tc
		cc.Params.ParallelToolCalls = parallel
	}
	return cc, warns
}

// PrefixCacheKey 从可缓存前缀（system + tools + 首条用户消息文本）派生
// 稳定会话 ID。同一对话跨轮次前缀不变 → 同一会话 → 命中上游 prompt cache；
// 前缀变化（如工具集变更、上下文压缩）时换会话，此时旧缓存本来也无法复用。
func PrefixCacheKey(req *types.Request) string {
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
	sum := hex.EncodeToString(h.Sum(nil))
	// UUID 形状（8-4-4-4-12），与真实 CLI 会话 ID 一致
	return sum[0:8] + "-" + sum[8:12] + "-" + sum[12:16] + "-" + sum[16:20] + "-" + sum[20:32]
}

// ---------- 消息解析 ----------

type parsedMessage struct {
	role   string
	blocks []types.Block
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

// parseSystem 解析 system（string | []block），返回拼接文本与是否携带
// cache_control。cmdc 强制 system 为字符串，块结构与断点只能折算进
// 会话亲和 + part 级缓存标记（synthesizeCacheMarker）。
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

// convertUser 把一条 Anthropic user 消息切分为 cmdc 消息序列：
// text/image 块归 user 消息、tool_result 块归 tool 消息（1 part/条），
// 保持块出现顺序；tool_result 内嵌图片并入紧随其后的 user 段
// （cmdc 的 tool-result 只收文本，sub2api 同款媒体搬迁手法）。
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
				// 孤儿 tool_result（历史被裁剪/摘要后残留）：发送大概率被上游
				// 拒收，丢弃并留痕（sub2api normalizeAnthropicToolPairing 手法）
				*warns = append(*warns, "orphan tool_result dropped (no matching tool_use in history): "+b.ToolUseID)
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
			*warns = append(*warns, "unknown user block type dropped: "+b.Type)
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
	var parts []types.CcPart
	thinkingDropped := false
	for _, b := range pm.blocks {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			parts = append(parts, types.CcPart{Type: "text", Text: b.Text, CacheControl: ccCC(b.CacheControl)})
		case "thinking":
			if opts.AssistantReasoning {
				// 实验路径：上游 reasoning part 的确切形状未实弹验证
				parts = append(parts, types.CcPart{Type: "reasoning", Text: b.Thinking})
			} else {
				thinkingDropped = true
			}
		case "redacted_thinking":
			// 无明文可还原，跳过
		case "tool_use":
			input := b.Input
			if len(input) == 0 || string(input) == "null" {
				input = json.RawMessage("{}")
			}
			parts = append(parts, types.CcPart{
				Type:         "tool-call",
				ToolCallID:   b.ID, // 恒等透传，续轮配对依赖原始 ID
				ToolName:     b.Name,
				Input:        input,
				CacheControl: ccCC(b.CacheControl),
			})
		case "image":
			*warns = append(*warns, "image block in assistant message dropped")
		default:
			*warns = append(*warns, "unknown assistant block type dropped: "+b.Type)
		}
	}
	if thinkingDropped {
		*warns = append(*warns, "assistant thinking block(s) dropped (set CC_ASSISTANT_REASONING=1 to experiment with {type:reasoning} passthrough)")
	}
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

// ccCC 归一化 cache_control：只保留已验证的 {type:"ephemeral"} 形状（剥 TTL）。
func ccCC(cc *types.CacheControl) *types.CacheControl {
	if cc == nil {
		return nil
	}
	return &types.CacheControl{Type: "ephemeral"}
}

// ---------- tools / tool_choice / thinking ----------

func convertTools(tools []types.Tool, warns *[]string) ([]types.CcTool, bool) {
	if len(tools) == 0 {
		return nil, false
	}
	out := make([]types.CcTool, 0, len(tools))
	hadCC := false
	for _, t := range tools {
		hadCC = hadCC || t.CacheControl != nil
		out = append(out, types.CcTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			InputSchema: normalizeSchema(t.InputSchema),
		})
	}
	return out, hadCC
}

// normalizeSchema 兜底 schema：nil/非 object/缺 properties 一律补成
// {"type":"object","properties":{}}，避免上游 400。
func normalizeSchema(raw json.RawMessage) json.RawMessage {
	def := json.RawMessage(`{"type":"object","properties":{}}`)
	if len(raw) == 0 {
		return def
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return def
	}
	if t, ok := m["type"]; !ok || strings.TrimSpace(string(t)) != `"object"` {
		return def
	}
	if _, ok := m["properties"]; !ok {
		m["properties"] = json.RawMessage(`{}`)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return def
	}
	return b
}

func convertToolChoice(raw json.RawMessage) (*types.CcToolChoice, *bool) {
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
		choice = &types.CcToolChoice{Type: tc.Type, Name: tc.Name}
	default:
		choice = &types.CcToolChoice{Type: "auto"}
	}
	var parallel *bool
	if tc.DisableParallelToolUse != nil {
		v := !*tc.DisableParallelToolUse
		parallel = &v
	}
	return choice, parallel
}

// thinkingEffort Anthropic thinking → cmdc reasoning_effort。
// 阈值对齐原版（LiteLLM 标准）：≥10000 high，≥5000 medium，>0 low。
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

// synthesizeCacheMarker 在需要时把缓存断点合成到信封尾部：从最后一条消息
// 的最后一个 part 往前找第一个 text part（PR#10 验证过的落点形状），把
// {type:"ephemeral"} 打在那里。
// 注意从信封整体末尾回扫而非只扫 user 消息：agent 式会话（一条人类消息 +
// 连续工具回合）的信封尾部全是 role:"tool" 消息，只扫 user 会把断点钉在
// 第一条人类消息上、位置永不前进。text-less 的纯工具轮有一轮迟滞，属可
// 接受代价（cmdc 对非 text part 上标记的接受度未实弹验证，不放上去赌）。
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
						"no part-level cache_control found on inbound messages; synthesized breakpoint on the envelope tail's last text part (system/tools markers folded — the cmdc envelope has no fields to carry them)")
				}
				return
			}
		}
	}
	*warns = append(*warns, "cache_control present on system/tools but no text part to carry the marker")
}
