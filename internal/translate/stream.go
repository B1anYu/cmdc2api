package translate

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/B1anYu/cmdc2api/internal/errs"
	"github.com/B1anYu/cmdc2api/internal/masq"
	"github.com/B1anYu/cmdc2api/internal/types"
)

// StreamTranslator 把 cmdc NDJSON 事件流增量翻译为 Anthropic SSE 事件。
// 调用方逐行喂入 Feed，结束时调一次 Finish；无内部 goroutine。
//
// 核心状态机不变式：
//   - 「开启新块前必关旧块」：text 与 thinking 交错时若未先关闭旧块，累积文本会被客户端 SDK 覆盖丢失；
//   - thinking 块收尾顺序：必须先发送 signature_delta 再发送 content_block_stop，确保客户端能将签名带入下一轮；
//   - stop_reason 判定：严格以 finishReason 为准，不依赖最后一个内容块的类型。
type StreamTranslator struct {
	model     string
	messageID string

	nextIndex    int
	openType     string // "" | "text" | "thinking"
	openIndex    int
	thinkingText strings.Builder

	usage         types.CcUsage
	hasUsage      bool
	outEstimate   int // 上游漏发 usage 时的输出 token 估算保底（按 delta 与工具调用估算）
	stopReason    string
	stepReasonSet bool // finish-step 已给出 stop_reason 时置为 true，防止被随后的 finish 事件覆盖

	contentStarted bool
	hasError       bool
	errMessage     string
	lastEvent      string
	finished       bool
}

func NewStreamTranslator(model, messageID string) *StreamTranslator {
	return &StreamTranslator{model: model, messageID: messageID}
}

// Start 返回恒定的首个事件（message_start）。
func (t *StreamTranslator) Start() []types.StreamEvent {
	return []types.StreamEvent{types.NewMessageStart(t.messageID, t.model)}
}

// Feed 解析一行 NDJSON，返回 0..N 个 Anthropic SSE 事件。
// 无法解析的行静默忽略（上游可能夹带非 JSON 行）。
func (t *StreamTranslator) Feed(line []byte) []types.StreamEvent {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] == ':' || string(trimmed) == "[DONE]" {
		return nil
	}
	var ev types.CcEvent
	if err := json.Unmarshal(trimmed, &ev); err != nil || ev.Type == "" {
		return nil
	}
	t.lastEvent = ev.Type

	var out []types.StreamEvent
	switch ev.Type {
	case "text-delta":
		text := ev.Text
		if text == "" {
			text = ev.Delta
		}
		if text == "" {
			return nil
		}
		out = append(out, t.startBlock("text")...)
		out = append(out, types.NewContentBlockDelta(t.openIndex, types.TextDelta{Type: "text_delta", Text: text}))
		t.outEstimate++
		t.contentStarted = true

	case "reasoning-delta":
		if ev.Text == "" {
			return nil
		}
		out = append(out, t.startBlock("thinking")...)
		t.thinkingText.WriteString(ev.Text)
		out = append(out, types.NewContentBlockDelta(t.openIndex, types.ThinkingDelta{Type: "thinking_delta", Thinking: ev.Text}))
		t.outEstimate++
		t.contentStarted = true

	case "tool-call":
		out = append(out, t.closeBlock()...)
		id := ev.ToolCallID
		if id == "" {
			id = "toolu_" + masq.NewHexID(6)
		}
		idx := t.nextIndex
		t.nextIndex++
		out = append(out, types.NewContentBlockStart(idx, types.ToolUseBlockStart{
			Type: "tool_use", ID: id, Name: ev.ToolName, Input: map[string]any{},
		}))
		// 上游一次性给出完整 input：通过单条 input_json_delta 承载全文
		out = append(out, types.NewContentBlockDelta(idx, types.InputJSONDelta{
			Type: "input_json_delta", PartialJSON: partialJSON(ev.Input),
		}))
		out = append(out, types.NewContentBlockStop(idx))
		t.outEstimate += 20
		t.contentStarted = true

	case "finish-step":
		if ev.FinishReason != "" {
			t.stopReason = MapStopReason(ev.FinishReason)
			t.stepReasonSet = true
		}
		if ev.Usage != nil {
			t.usage = *ev.Usage
			t.hasUsage = true
		}

	case "finish":
		if ev.TotalUsage != nil {
			t.usage = *ev.TotalUsage
			t.hasUsage = true
		} else if ev.Usage != nil {
			t.usage = *ev.Usage
			t.hasUsage = true
		}
		if !t.stepReasonSet {
			if ev.FinishReason != "" {
				t.stopReason = MapStopReason(ev.FinishReason)
			} else if t.stopReason == "" {
				t.stopReason = "end_turn"
			}
		}

	case "error":
		t.hasError = true
		msg := ev.Error.Message
		if msg == "" {
			msg = ev.Message
		}
		if msg == "" {
			msg = "Unknown CC error"
		}
		t.errMessage = msg
		_, typ, _, _ := errs.MapEvent(msg)
		out = append(out, types.NewErrorEvent(typ, msg))

	default:
		// start / start-step / text-start / reasoning-start / text-end /
		// reasoning-end / provider-metadata / tool-input-* / tool-error：
		// 信号与增量协议事件，无用户可见内容
	}
	return out
}

// Finish 收尾（幂等）：关块、message_delta（含最终 usage）、message_stop；
// 上游错误或零输出时改为错误事件。正常流结束后必须调用。
func (t *StreamTranslator) Finish() []types.StreamEvent {
	if t.finished {
		return nil
	}
	t.finished = true
	if t.hasError {
		return nil
	}
	out := t.closeBlock()
	if t.stopReason == "" {
		t.stopReason = "end_turn"
	}
	if t.OutputTokens() == 0 {
		// 零输出转换为错误事件，避免下游客户端产生异常计费
		return append(out, types.NewErrorEvent("rate_limit_error", "Empty response from upstream (zero output tokens)"))
	}
	out = append(out, types.NewMessageDelta(t.stopReason, t.DeltaUsage()))
	out = append(out, types.NewMessageStop())
	return out
}

// DeltaUsage 最终 usage 快照（含反虚假计费归零）。
func (t *StreamTranslator) DeltaUsage() types.DeltaUsage {
	u := t.usage
	u.Normalize()
	cacheWrite := 0
	if u.InputTokenDetails != nil {
		cacheWrite = u.InputTokenDetails.CacheWriteTokens
	}
	var ccPtr *int
	if cacheWrite > 0 {
		ccPtr = &cacheWrite
	}
	// cmdc 上游返回的 inputTokens 包含内部多步处理累加的缓存读取（数值约为后台控制台统计的两倍）。
	// 换算公式 input_tokens = max(0, inputTokens − cachedInputTokens) 精准对齐 cmdc 控制台去重后的实际 Prompt 输入，
	// 并在多步累加与总量包含缓存两种模型下均保持一致。
	input := u.InputTokens - u.CachedInputTokens
	if input < 0 {
		input = 0
	}
	cached := u.CachedInputTokens
	if !t.hasUsage {
		input, cached = 0, 0
	}
	output := t.OutputTokens()
	return types.DeltaUsage{
		OutputTokens:             output,
		InputTokens:              input,
		CacheReadInputTokens:     cached,
		CacheCreationInputTokens: ccPtr,
	}
}

// OutputTokens 真实 usage 优先；上游漏发 usage 时用增量估算兜底（含 reasoning 思考块与 text/tool-call）。
func (t *StreamTranslator) OutputTokens() int {
	if t.hasUsage {
		return t.usage.OutputTokens
	}
	return t.outEstimate
}

func (t *StreamTranslator) HasUpstreamError() bool   { return t.hasError }
func (t *StreamTranslator) ErrMessage() string       { return t.errMessage }
func (t *StreamTranslator) ContentStarted() bool     { return t.contentStarted }
func (t *StreamTranslator) LastEvent() string        { return t.lastEvent }
func (t *StreamTranslator) StopReason() string       { return t.stopReason }
func (t *StreamTranslator) MessageID() string        { return t.messageID }

// ---------- 块生命周期 ----------

func (t *StreamTranslator) closeBlock() []types.StreamEvent {
	if t.openType == "" {
		return nil
	}
	var out []types.StreamEvent
	if t.openType == "thinking" {
		out = append(out, types.NewContentBlockDelta(t.openIndex, types.SignatureDelta{
			Type:      "signature_delta",
			Signature: FakeThinkingSignature(t.thinkingText.String()),
		}))
	}
	out = append(out, types.NewContentBlockStop(t.openIndex))
	t.openType = ""
	return out
}

func (t *StreamTranslator) startBlock(typ string) []types.StreamEvent {
	if t.openType == typ {
		return nil // 连续同类型 delta 合并进当前块
	}
	out := t.closeBlock()
	t.openIndex = t.nextIndex
	t.nextIndex++
	t.openType = typ
	var block any
	if typ == "text" {
		block = types.TextBlockStart{Type: "text", Text: ""}
	} else {
		block = types.ThinkingBlockStart{Type: "thinking", Thinking: ""}
	}
	return append(out, types.NewContentBlockStart(t.openIndex, block))
}

// ---------- 映射 ----------

// MapStopReason cmdc finishReason → Anthropic stop_reason。
func MapStopReason(cc string) string {
	switch cc {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool-calls":
		return "tool_use"
	case "content-filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

// partialJSON 上游 tool-call 的 input 可能是对象或 JSON 字符串，统一为
// partial_json 文本；空/null 补 "{}"。
func partialJSON(raw json.RawMessage) string {
	s := string(raw)
	if s == "" || s == "null" {
		return "{}"
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	return s
}

// FakeThinkingSignature 确定性伪造 thinking 签名：sha256(thinking 文本) 前
// 32 字节加 0x12 长度前缀再 base64。满足 Claude Code 浅校验要求（base64 以 'E' 开头且载荷首字节为 0x12），
// 且同一文本签名保持确定性稳定。
func FakeThinkingSignature(text string) string {
	if text == "" {
		text = "dsh-proxy-thinking" // 空文本兜底种子
	}
	seed := sha256.Sum256([]byte(text))
	raw := append([]byte{0x12, byte(len(seed))}, seed[:]...)
	return base64.StdEncoding.EncodeToString(raw)
}
