package translate

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/B1anYu/cmdc2api/internal/masq"
	"github.com/B1anYu/cmdc2api/internal/types"
)

// OpenAI Chat Completions 出站：把规范格式的 StreamEvent 序列编码为流式分片，
// 或折叠为非流式响应。两种形态消费同一批事件，语义不分裂。

// chatArgChunkSize 工具参数分片粒度（按 rune 计）：上游一次性给出完整 JSON 参数，
// 拆片只为还原 OpenAI 的增量节奏，客户端按 index 顺序拼接，语义不受分片影响。
const chatArgChunkSize = 10

// NewChatCompletionID 生成 chatcmpl- 前缀的响应 ID（handler 侧调用后传给编/聚合器）。
func NewChatCompletionID() string { return "chatcmpl-" + masq.NewHexID(12) }

// ---------- 流式编码器 ----------

// ChatEncoder 把 StreamEvent 编码为 `data: {...}\n\n` 帧（Chat 的 SSE 帧没有 event: 行）。
// 契约与 StreamTranslator 同构：Feed 增量产出 0..N 帧，Finish 幂等收尾；无内部 goroutine。
//
// 三条与 Anthropic 出站不同的约定：
//   - 首帧（role 声明）延迟到首个内容增量，避免空流首帧——管线据此判断「内容是否已开始」，
//     首帧前出错才能回退成 JSON 错误响应；
//   - 工具调用序号是**只计工具的独立计数器**（OpenAI 语义），与 Anthropic 的块序号无关；
//   - 收尾必须是 finish chunk → usage chunk → `data: [DONE]` 三段（Chat 有 [DONE]，Responses 没有）。
type ChatEncoder struct {
	model   string
	id      string
	created int64

	started  bool // 已发出首帧：决定错误是落在流内还是回退为 JSON 响应
	hasError bool
	finished bool

	toolIndex int // 已开启的工具调用数（下一个工具的 index）
	curTool   int // 当前打开的工具调用 index：参数的 input_json_delta 归属它

	stopReason string
	usage      types.DeltaUsage
}

// NewChatEncoder model 为回显模型名，id 为 handler 生成的 chatcmpl-... 标识。
func NewChatEncoder(model, id string) *ChatEncoder {
	return &ChatEncoder{model: model, id: id, created: time.Now().Unix()}
}

// Started 报告流内是否已发出首帧。首帧前失败时调用方应改出 JSON 错误响应。
func (e *ChatEncoder) Started() bool { return e.started }

// Feed 编码一条上游事件，返回 0..N 个 SSE 帧。
func (e *ChatEncoder) Feed(ev types.StreamEvent) [][]byte {
	if e.finished || e.hasError {
		return nil
	}
	switch d := ev.Data.(type) {
	case types.ContentBlockStartEvent:
		b, ok := d.ContentBlock.(types.ToolUseBlockStart)
		if !ok {
			return nil // text/thinking 块开启不发帧：首帧与正文都等第一个增量
		}
		e.curTool = e.toolIndex
		e.toolIndex++
		return e.withRole(e.chunk(types.ChatDelta{ToolCalls: []types.ChatDeltaToolCall{{
			Index: e.curTool, ID: b.ID, Type: "function",
			Function: types.ChatFunctionCall{Name: b.Name, Arguments: ""},
		}}}))

	case types.ContentBlockDeltaEvent:
		switch dd := d.Delta.(type) {
		case types.TextDelta:
			if dd.Text == "" {
				return nil
			}
			return e.withRole(e.chunk(types.ChatDelta{Content: chatStrPtr(dd.Text)}))
		case types.ThinkingDelta:
			if dd.Thinking == "" {
				return nil
			}
			return e.withRole(e.chunk(types.ChatDelta{ReasoningContent: dd.Thinking}))
		case types.InputJSONDelta:
			frags := chatSplitRunes(dd.PartialJSON, chatArgChunkSize)
			frames := make([][]byte, 0, len(frags))
			for _, frag := range frags {
				frames = append(frames, e.chunk(types.ChatDelta{ToolCalls: []types.ChatDeltaToolCall{{
					Index: e.curTool, Function: types.ChatFunctionCall{Arguments: frag},
				}}}))
			}
			return frames
		case types.SignatureDelta:
			// 签名只服务于 Anthropic 客户端的跨轮回放，Chat 侧没有对应概念
			return nil
		}

	case types.MessageDeltaEvent:
		// 未收到 message_delta 时 usage 保持零值：Chat 没有输出 token 估算兜底，
		// 宁可报 0 也不能编造计费数字
		e.stopReason = d.Delta.StopReason
		e.usage = d.Usage

	case types.MessageStopEvent:
		return e.finishFrames()

	case types.ErrorEvent:
		return e.errorFrames(d)
	}
	return nil
}

// Finish 收尾（幂等）：finish chunk → usage chunk → [DONE]。
// 事件流带 error 时不再补任何帧——错误帧之后必须终止，客户端不认半截正常收尾。
func (e *ChatEncoder) Finish() [][]byte {
	if e.finished || e.hasError {
		return nil
	}
	return e.finishFrames()
}

func (e *ChatEncoder) finishFrames() [][]byte {
	if e.finished {
		return nil
	}
	e.finished = true

	var frames [][]byte
	if !e.started {
		// 极端情形：整条流只有 usage 没有内容。补发 role 声明，保证分片序列合法。
		e.started = true
		frames = append(frames, e.chunk(types.ChatDelta{Role: "assistant", Content: chatStrPtr("")}))
	}

	reason := chatFinishReason(e.stopReason)
	frames = append(frames, chatDataFrame(types.ChatChunk{
		ID: e.id, Object: "chat.completion.chunk", Created: e.created, Model: e.model,
		Choices: []types.ChatChunkChoice{{Index: 0, FinishReason: &reason}},
	}))
	usage := e.chatUsage()
	frames = append(frames, chatDataFrame(types.ChatChunk{
		ID: e.id, Object: "chat.completion.chunk", Created: e.created, Model: e.model,
		Choices: []types.ChatChunkChoice{}, // usage chunk 的 choices 是空数组而非 null
		Usage:   &usage,
	}))
	return append(frames, chatDoneFrame())
}

// errorFrames 流内错误：已开流时发一帧 OpenAI 形状错误后终止（HTTP 状态码已发出，改不了）。
// 未开流时什么都不发，由调用方改出 JSON 错误响应（保留可重试的状态码与 Retry-After 头）。
func (e *ChatEncoder) errorFrames(d types.ErrorEvent) [][]byte {
	e.hasError = true
	if !e.started {
		return nil
	}
	return [][]byte{chatDataFrame(map[string]any{"error": OpenAIErrorBody(d.Error.Type, d.Error.Message)})}
}

// withRole 保证 role 声明帧先于首个内容帧发出（只发一次）。
func (e *ChatEncoder) withRole(frames ...[]byte) [][]byte {
	if e.started {
		return frames
	}
	e.started = true
	role := e.chunk(types.ChatDelta{Role: "assistant", Content: chatStrPtr("")})
	return append([][]byte{role}, frames...)
}

// chunk 组装一帧非终态分片（finish_reason 恒为 null）。
func (e *ChatEncoder) chunk(delta types.ChatDelta) []byte {
	return chatDataFrame(types.ChatChunk{
		ID: e.id, Object: "chat.completion.chunk", Created: e.created, Model: e.model,
		Choices: []types.ChatChunkChoice{{Index: 0, Delta: delta}},
	})
}

// chatUsage 流式收尾的 usage 帧：换算口径与聚合器共用 chatUsageFrom（单一实现），
// 两处各写一份公式正是流式/非流式语义漂移的温床。
func (e *ChatEncoder) chatUsage() types.ChatUsage { return chatUsageFrom(e.usage) }

// ---------- 非流式聚合器 ----------

// ChatAggregator 把同一批 StreamEvent 折叠为非流式 ChatCompletion。
// 与流式编码器消费同一事件序列，两种出站形态语义一致。
type ChatAggregator struct {
	model   string
	id      string
	created int64

	text      strings.Builder
	reasoning strings.Builder
	// tools 存**指针**而非值：chatAggTool 内含 strings.Builder，按值放进切片时
	// append 扩容会整体复制 Builder，被复制的 Builder 再被 String() 读取即触发
	// copyCheck 恐慌（Go 1.26 前的运行时保护）——上游一旦改为分片发参数、同一块
	// 出现多条 InputJSONDelta 就会踩到。指针切片扩容只复制指针，Builder 身份恒定。
	tools     []*chatAggTool
	toolByBlk map[int]int // Anthropic 块序号 → 工具序号（多工具并行时的归属判定）

	stopReason string
	usage      types.DeltaUsage
}

type chatAggTool struct {
	id   string
	name string
	args strings.Builder
}

func NewChatAggregator(model, id string) *ChatAggregator {
	return &ChatAggregator{
		model: model, id: id, created: time.Now().Unix(),
		toolByBlk: make(map[int]int),
	}
}

func (a *ChatAggregator) Feed(ev types.StreamEvent) {
	switch d := ev.Data.(type) {
	case types.ContentBlockStartEvent:
		b, ok := d.ContentBlock.(types.ToolUseBlockStart)
		if !ok {
			return
		}
		a.toolByBlk[d.Index] = len(a.tools)
		a.tools = append(a.tools, &chatAggTool{id: b.ID, name: b.Name})

	case types.ContentBlockDeltaEvent:
		switch dd := d.Delta.(type) {
		case types.TextDelta:
			a.text.WriteString(dd.Text)
		case types.ThinkingDelta:
			a.reasoning.WriteString(dd.Thinking)
		case types.InputJSONDelta:
			if idx, ok := a.toolByBlk[d.Index]; ok {
				a.tools[idx].args.WriteString(dd.PartialJSON)
			}
		case types.SignatureDelta:
			// 非流式响应没有签名字段（Chat 客户端不做思考回放）
		}

	case types.MessageDeltaEvent:
		a.stopReason = d.Delta.StopReason
		a.usage = d.Usage

	case types.ErrorEvent:
		// 错误路径由 handler 决定出站形态（本聚合器不吞事件，也不改产物）
	}
}

// Result 满足共享管线的 EventAggregator 接口。
func (a *ChatAggregator) Result() any { return a.Completion() }

// Completion 产出最终响应。
func (a *ChatAggregator) Completion() *types.ChatCompletion {
	reason := chatFinishReason(a.stopReason)

	msg := types.ChatResponseMessage{Role: "assistant"}
	// content 恒存在：有文本给文本，纯工具调用时给 null（OpenAI 的既有形态）
	if text := a.text.String(); text != "" {
		msg.Content = chatStrPtr(text)
	}
	if reasoning := a.reasoning.String(); reasoning != "" {
		msg.ReasoningContent = reasoning
	}
	for _, t := range a.tools {
		args := t.args.String()
		if args == "" || !json.Valid([]byte(args)) {
			// 上游截断或空参数：给空对象，避免毒 JSON 进入客户端会话
			args = "{}"
		}
		msg.ToolCalls = append(msg.ToolCalls, types.ChatMessageToolCall{
			ID: t.id, Type: "function",
			Function: types.ChatFunctionCall{Name: t.name, Arguments: args},
		})
	}

	u := chatUsageFrom(a.usage)
	return &types.ChatCompletion{
		ID: a.id, Object: "chat.completion", Created: a.created, Model: a.model,
		Choices: []types.ChatChoice{{Index: 0, Message: &msg, FinishReason: &reason}},
		Usage:   &u,
	}
}

// ---------- 映射与小工具 ----------

// chatFinishReason Anthropic stop_reason → OpenAI finish_reason。
// 方向与 MapStopReason 相反，映射表按 OpenAI 规范补齐（refusal 归 content_filter）。
func chatFinishReason(stopReason string) string {
	switch stopReason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default: // end_turn / stop_sequence / "" 及未知值
		return "stop"
	}
}

// OpenAIErrorBody OpenAI 形状错误体。type 决定客户端重试分流，code 供程序化判定，
// param 恒为 null（SDK 按字段存在性解析）。
// **全仓唯一**的 OpenAI 错误体实现：流内错误帧（ChatEncoder.errorFrames）与
// server 侧开流前错误出口（openaiErrors.Write）都调它，两条路径形状不可能漂移。
func OpenAIErrorBody(errType, message string) map[string]any {
	typ, code := openAIErrorShape(errType)
	return map[string]any{"message": message, "type": typ, "param": nil, "code": code}
}

// openAIErrorShape Anthropic 错误类型 → OpenAI (type, code)。
// type 决定客户端的重试分流，code 供程序化判定（如 invalid_api_key 触发换 key），
// 两者都要给，不能只给其一。改表即改所有 OpenAI 形状出口，勿再抄一份到别处。
func openAIErrorShape(errType string) (typ, code string) {
	switch errType {
	case "authentication_error":
		return "invalid_request_error", "invalid_api_key"
	case "invalid_request_error":
		return "invalid_request_error", "invalid_request_error"
	case "rate_limit_error":
		return "rate_limit_error", "rate_limit_exceeded"
	case "not_found_error":
		return "not_found_error", "not_found_error"
	default: // api_error / overloaded_error 等统归 server_error
		return "server_error", "server_error"
	}
}

// chatUsageFrom 内部 noCache 口径 → OpenAI prompt_tokens 口径的加法回填，流式（经
// ChatEncoder.chatUsage）与非流式（ChatAggregator.Completion）共用的**唯一**实现：
// Anthropic 的 input_tokens 只计非缓存部分，OpenAI 的 prompt_tokens 含缓存命中，
// 因此总输入 = 非缓存 + 缓存读取（+ 缓存写入）。cached_tokens 只报缓存读取。
func chatUsageFrom(u types.DeltaUsage) types.ChatUsage {
	cached := u.CacheReadInputTokens
	prompt := u.InputTokens + cached
	if u.CacheCreationInputTokens != nil && *u.CacheCreationInputTokens > 0 {
		prompt += *u.CacheCreationInputTokens
	}
	return types.ChatUsage{
		PromptTokens:        prompt,
		CompletionTokens:    u.OutputTokens,
		TotalTokens:         prompt + u.OutputTokens,
		PromptTokensDetails: types.ChatPromptTokensDetails{CachedTokens: cached},
	}
}

// chatSplitRunes 按 rune 切分字符串。必须按 rune 而非字节：参数里常含多字节字符，
// 从中间切断会产出非法 UTF-8，客户端拼接后得到损坏的 JSON。
func chatSplitRunes(s string, size int) []string {
	if s == "" {
		return nil
	}
	runes := []rune(s)
	if size <= 0 || len(runes) <= size {
		return []string{s}
	}
	out := make([]string, 0, (len(runes)+size-1)/size)
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

// chatDataFrame 组装 `data: <json>\n\n` 帧（Chat 的 SSE 帧不带 event: 行）。
func chatDataFrame(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`{}`)
	}
	buf := make([]byte, 0, len(b)+10)
	buf = append(buf, "data: "...)
	buf = append(buf, b...)
	buf = append(buf, '\n', '\n')
	return buf
}

// chatDoneFrame 流结束标记：Chat 有 [DONE]，Responses 没有。
func chatDoneFrame() []byte { return []byte("data: [DONE]\n\n") }

func chatStrPtr(s string) *string { return &s }
