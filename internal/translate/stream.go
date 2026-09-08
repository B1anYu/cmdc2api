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
// 不变式（事故注释，sub2api 同款教训）：
//   - 「开新块前必关旧块」：text→thinking 交错时若不先关旧块，
//     累积文本会被客户端 SDK 直接覆盖丢失；
//   - thinking 块收尾必须先发 signature_delta 再发 content_block_stop，
//     否则客户端无法把签名带回下一轮；
//   - stop_reason 不依赖最后一个块的内容，只认上游 finishReason。
type StreamTranslator struct {
	model     string
	messageID string

	nextIndex    int
	openType     string // "" | "text" | "thinking"
	openIndex    int
	thinkingText strings.Builder

	usage         types.CcUsage
	hasUsage      bool
	outEstimate   int // 上游漏发 usage 时的输出下限估计（原版手法：delta/工具计数）
	stopReason    string
	stepReasonSet bool // finish-step 已给出 stop_reason（finish 不再覆盖，原版语义）

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
		// 上游一次性给完整 input：单条 input_json_delta 承载全文（原版手法）
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
		// 零输出按错误处理，避免下游异常计费（原版语义）
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
	// 实弹定案（2026-09-09，两轮实测迭代后的最终口径）：cmdc 的 inputTokens
	// 疑似按内部多步循环求和（tool 回合的 step1 全量处理、step2 全量命中
	// 缓存），导致 inputTokens ≈ 2×真实prompt、cachedInputTokens ≈ 真实
	// prompt——C/I 恒定 ~50% 与后台总输入恰为 API 一半均由此而来。
	// 换算 input_tokens = inputTokens − cachedInputTokens（钳 ≥0）：
	// 在「多步求和」与「总量含缓存」两种模型下都等于真实口径，且与
	// 上游后台的总输入对齐。曾试过 −2×cached（报未缓存量），实测把
	// input_tokens 钳成 0、命中率显示 100%，已证伪废弃。
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

// OutputTokens 真实 usage 优先；上游漏发 usage 时用增量估计兜底
// （原版只数 text/tool-call，这里把 reasoning 也计入：纯思考响应是有效输出）。
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
// 32 字节加 0x12 长度前缀再 base64。第三方代理不可能铸造合法签名；
// Claude Code 的浅校验只看 base64 以 'E' 开头且载荷首字节 0x12，本函数
// 恰好满足，且同一文本签名稳定（可复现，多轮回传不冲突）。
func FakeThinkingSignature(text string) string {
	if text == "" {
		text = "dsh-proxy-thinking" // 原版对空文本的兜底种子
	}
	seed := sha256.Sum256([]byte(text))
	raw := append([]byte{0x12, byte(len(seed))}, seed[:]...)
	return base64.StdEncoding.EncodeToString(raw)
}
