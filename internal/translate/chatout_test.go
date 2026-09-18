package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/B1anYu/cmdc2api/internal/types"
)

// 编译期契约：与 server 共享管线的 EventEncoder / EventAggregator 形状一致。
// 形状一旦漂移在编译期就会失败，而不是等到 handler 接线时才发现。
var (
	_ interface {
		Feed(types.StreamEvent) [][]byte
		Finish() [][]byte
	} = (*ChatEncoder)(nil)
	_ interface {
		Feed(types.StreamEvent)
		Result() any
	} = (*ChatAggregator)(nil)
)

// ---------- 助手：通过真实上游链路产出分片 ----------

// framesFrom 伪造 cmdc NDJSON 走「StreamTranslator → ChatEncoder」全链路，
// 返回解析后的帧载荷与是否收到 [DONE]。用真实翻译器而非手搓事件，
// 才能钉住编码器面对的确实是生产事件序列（含 signature_delta 等中间事件）。
func framesFrom(t *testing.T, enc *ChatEncoder, lines ...string) ([]map[string]any, bool) {
	t.Helper()
	var out []map[string]any
	done := false
	collect := func(evs []types.StreamEvent) {
		for _, ev := range evs {
			for _, f := range enc.Feed(ev) {
				obj, isDone := parseFrame(t, f)
				if isDone {
					done = true
					continue
				}
				out = append(out, obj)
			}
		}
	}
	tr := NewStreamTranslator("test-model", "msg_1")
	collect(tr.Start())
	for _, l := range lines {
		collect(tr.Feed([]byte(l)))
	}
	collect(tr.Finish())
	for _, f := range enc.Finish() {
		obj, isDone := parseFrame(t, f)
		if isDone {
			done = true
			continue
		}
		out = append(out, obj)
	}
	return out, done
}

// parseFrame 校验帧形状（必须是 `data: ...\n\n`，不能带 event: 行）并解出载荷。
func parseFrame(t *testing.T, frame []byte) (map[string]any, bool) {
	t.Helper()
	s := string(frame)
	if !strings.HasPrefix(s, "data: ") || !strings.HasSuffix(s, "\n\n") {
		t.Fatalf("malformed frame %q", s)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(s, "data: "), "\n\n")
	if body == "[DONE]" {
		return nil, true
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("frame payload %q: %v", body, err)
	}
	if _, isError := obj["error"]; isError {
		return obj, false // 流内错误帧没有分片信封
	}
	if obj["object"] != "chat.completion.chunk" {
		t.Errorf("object = %v, want chat.completion.chunk", obj["object"])
	}
	for _, k := range []string{"id", "created", "model", "choices"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("frame missing %q: %v", k, obj)
		}
	}
	return obj, false
}

// deltaOfOK 取分片 delta；没有 choices 的帧（usage chunk）返回 false。
func deltaOfOK(frame map[string]any) (map[string]any, bool) {
	list, ok := frame["choices"].([]any)
	if !ok || len(list) == 0 {
		return nil, false
	}
	choice, ok := list[0].(map[string]any)
	if !ok {
		return nil, false
	}
	d, ok := choice["delta"].(map[string]any)
	return d, ok
}

func choice0(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	list, ok := frame["choices"].([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("frame has no choices: %v", frame)
	}
	c, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("choice is not an object: %v", list[0])
	}
	return c
}

func deltaOf(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	d, ok := choice0(t, frame)["delta"].(map[string]any)
	if !ok {
		t.Fatalf("choice has no delta: %v", frame)
	}
	return d
}

// finishReasonOK 返回 finish_reason 的值与键是否存在（nil 也是合法值，键必须存在）；
// 没有 choices 的帧（usage chunk）返回 ok=false。
func finishReasonOK(frame map[string]any) (any, bool) {
	list, ok := frame["choices"].([]any)
	if !ok || len(list) == 0 {
		return nil, false
	}
	choice, ok := list[0].(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := choice["finish_reason"]
	return v, ok
}

func finishReasonOf(t *testing.T, frame map[string]any) (any, bool) {
	t.Helper()
	v, ok := finishReasonOK(frame)
	if !ok {
		t.Fatalf("frame has no choices: %v", frame)
	}
	return v, ok
}

// lastFinishReason 扫描帧序列取最后一个非 null 的 finish_reason。
func lastFinishReason(t *testing.T, frames []map[string]any) any {
	t.Helper()
	for i := len(frames) - 1; i >= 0; i-- {
		if v, ok := finishReasonOK(frames[i]); ok && v != nil {
			return v
		}
	}
	return nil
}

// ---------- 流式编码 ----------

// 纯文本流：首帧 role 声明 → 正文增量 → finish → usage → [DONE]。
func TestChatEncoder_TextStreamFrames(t *testing.T) {
	enc := NewChatEncoder("test-model", "chatcmpl-abc")
	frames, done := framesFrom(t, enc,
		`{"type":"text-start"}`,
		`{"type":"text-delta","text":"Hello"}`,
		`{"type":"text-delta","text":" world"}`,
		`{"type":"text-end"}`,
		`{"type":"finish-step","finishReason":"stop","usage":{"inputTokens":3,"outputTokens":2}}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":5,"cachedInputTokens":4,"inputTokenDetails":{"noCacheTokens":6,"cacheReadTokens":4}}}`,
	)

	if len(frames) != 5 || !done {
		t.Fatalf("frames = %d (done=%v), want 5 + [DONE]: %v", len(frames), done, frames)
	}
	// 首帧：role 声明 + 显式空 content
	first := deltaOf(t, frames[0])
	if first["role"] != "assistant" || first["content"] != "" {
		t.Errorf("first delta = %v, want role=assistant content=\"\"", first)
	}
	if _, ok := first["tool_calls"]; ok {
		t.Error("first delta must not carry tool_calls")
	}
	if got := deltaOf(t, frames[1])["content"]; got != "Hello" {
		t.Errorf("delta 1 content = %v", got)
	}
	if got := deltaOf(t, frames[2])["content"]; got != " world" {
		t.Errorf("delta 2 content = %v", got)
	}
	// 中间分片的 finish_reason 必须显式为 null（键存在）
	for i := 0; i < 3; i++ {
		if v, ok := finishReasonOf(t, frames[i]); !ok || v != nil {
			t.Errorf("frame %d finish_reason = %v (present=%v), want explicit null", i, v, ok)
		}
	}
	// finish chunk
	if v, _ := finishReasonOf(t, frames[3]); v != "stop" {
		t.Errorf("finish_reason = %v, want stop", v)
	}
	if d := deltaOf(t, frames[3]); len(d) != 0 {
		t.Errorf("finish chunk delta should be empty, got %v", d)
	}
	// usage chunk：choices 为空数组 + OpenAI 口径（非缓存 6 + 缓存读取 4）
	usageFrame := frames[4]
	if list, ok := usageFrame["choices"].([]any); !ok || len(list) != 0 {
		t.Errorf("usage chunk choices = %v, want []", usageFrame["choices"])
	}
	if frames[3]["usage"] != nil {
		t.Error("finish chunk must not carry usage")
	}
	usage, _ := usageFrame["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(5) || usage["total_tokens"] != float64(15) {
		t.Errorf("usage = %v, want prompt 10 / completion 5 / total 15", usage)
	}
	details, _ := usage["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(4) {
		t.Errorf("cached_tokens = %v, want 4", details["cached_tokens"])
	}
}

// 思考流：thinking_delta → reasoning_content；signature_delta 不产帧。
func TestChatEncoder_ThinkingFrames(t *testing.T) {
	enc := NewChatEncoder("test-model", "chatcmpl-abc")
	frames, _ := framesFrom(t, enc,
		`{"type":"reasoning-start"}`,
		`{"type":"reasoning-delta","text":"let me think"}`,
		`{"type":"reasoning-end"}`,
		`{"type":"text-delta","text":"answer"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":5,"outputTokens":3}}`,
	)
	if len(frames) != 5 { // role + reasoning + text + finish + usage
		t.Fatalf("frames = %d, want 5: %v", len(frames), frames)
	}
	if deltaOf(t, frames[0])["role"] != "assistant" {
		t.Errorf("first frame must declare role: %v", frames[0])
	}
	if got := deltaOf(t, frames[1])["reasoning_content"]; got != "let me think" {
		t.Errorf("reasoning_content = %v", got)
	}
	if got := deltaOf(t, frames[2])["content"]; got != "answer" {
		t.Errorf("content = %v", got)
	}
	for _, f := range frames {
		if d, ok := deltaOfOK(f); ok {
			if _, has := d["signature"]; has {
				t.Error("signature must not leak into chat frames")
			}
		}
	}
}

// 工具调用：start 帧带 index/id/name + 空 arguments，参数按分片增量送出，
// 分片拼接必须还原完整 JSON 且不切断多字节字符。
func TestChatEncoder_ToolCallFragments(t *testing.T) {
	enc := NewChatEncoder("test-model", "chatcmpl-abc")
	frames, _ := framesFrom(t, enc,
		`{"type":"tool-call","toolCallId":"call_1","toolName":"read","input":{"path":"目录/子目录/a.go","n":1}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":5,"outputTokens":9}}`,
	)

	if deltaOf(t, frames[0])["role"] != "assistant" {
		t.Fatalf("first frame must declare role: %v", frames[0])
	}
	// 第二个内容帧：工具调用头
	calls, _ := deltaOf(t, frames[1])["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v", frames[1])
	}
	head, _ := calls[0].(map[string]any)
	if head["index"] != float64(0) || head["id"] != "call_1" || head["type"] != "function" {
		t.Errorf("tool call head = %v", head)
	}
	fn, _ := head["function"].(map[string]any)
	if fn["name"] != "read" || fn["arguments"] != "" {
		t.Errorf("start arguments must be an explicit empty string: %v", fn)
	}

	// 参数分片：拼回完整 JSON，且每片不超过 10 个 rune
	var sb strings.Builder
	frags := 0
	for _, f := range frames[2:] {
		d, ok := deltaOfOK(f)
		if !ok {
			continue
		}
		callList, ok := d["tool_calls"].([]any)
		if !ok {
			continue
		}
		part, _ := callList[0].(map[string]any)
		if part["index"] != float64(0) {
			t.Errorf("fragment index = %v, want 0", part["index"])
		}
		if _, ok := part["id"]; ok {
			t.Error("fragment must not repeat the tool id")
		}
		pfn, _ := part["function"].(map[string]any)
		seg, _ := pfn["arguments"].(string)
		if n := len([]rune(seg)); n > chatArgChunkSize {
			t.Errorf("fragment %q has %d runes, want <= %d", seg, n, chatArgChunkSize)
		}
		frags++
		sb.WriteString(seg)
	}
	if frags < 2 {
		t.Errorf("fragments = %d, want the full input split into pieces", frags)
	}
	if got, want := sb.String(), `{"path":"目录/子目录/a.go","n":1}`; got != want {
		t.Errorf("reassembled arguments = %q, want %q", got, want)
	}
	if v := lastFinishReason(t, frames); v != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", v)
	}
}

// 并行工具调用：index 只计工具（与 Anthropic 块序号无关），第二个工具的参数
// 必须落在 index 1 上。
func TestChatEncoder_ParallelToolsIndex(t *testing.T) {
	enc := NewChatEncoder("test-model", "chatcmpl-abc")
	frames, _ := framesFrom(t, enc,
		`{"type":"text-delta","text":"calling"}`,
		`{"type":"tool-call","toolCallId":"call_a","toolName":"a","input":{"x":1}}`,
		`{"type":"tool-call","toolCallId":"call_b","toolName":"b","input":{"y":2}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":5,"outputTokens":9}}`,
	)

	seen := map[float64]string{}
	for _, f := range frames {
		d, ok := deltaOfOK(f)
		if !ok {
			continue
		}
		callList, ok := d["tool_calls"].([]any)
		if !ok {
			continue
		}
		part, _ := callList[0].(map[string]any)
		idx, _ := part["index"].(float64)
		if id, ok := part["id"].(string); ok && id != "" {
			seen[idx] = id
		}
	}
	if seen[0] != "call_a" || seen[1] != "call_b" {
		t.Errorf("tool index assignment = %v, want 0→call_a 1→call_b", seen)
	}
}

// 流内错误：已开流时发一帧 OpenAI 形状错误后终止（不再补 [DONE]）。
func TestChatEncoder_ErrorMidStream(t *testing.T) {
	enc := NewChatEncoder("test-model", "chatcmpl-abc")
	frames, done := framesFrom(t, enc,
		`{"type":"text-delta","text":"partial"}`,
		`{"type":"error","error":{"message":"<429> rate limited"}}`,
	)
	if done {
		t.Error("[DONE] must not follow an in-stream error")
	}
	last := frames[len(frames)-1]
	errObj, ok := last["error"].(map[string]any)
	if !ok {
		t.Fatalf("last frame = %v, want an error frame", last)
	}
	if _, ok := last["choices"]; ok {
		t.Error("error frame must not carry choices")
	}
	if errObj["type"] != "rate_limit_error" || errObj["code"] != "rate_limit_exceeded" {
		t.Errorf("error shape = %v", errObj)
	}
	if errObj["message"] != "<429> rate limited" {
		t.Errorf("message = %v", errObj["message"])
	}
	if _, ok := errObj["param"]; !ok {
		t.Error("param key must be present (null)")
	}
	if enc.Finish() != nil {
		t.Error("Finish after an error must not emit frames")
	}
}

// 首帧前错误：编码器不发任何帧，由 handler 改出 JSON 错误响应（保留可重试状态码）。
func TestChatEncoder_ErrorBeforeContent(t *testing.T) {
	enc := NewChatEncoder("test-model", "chatcmpl-abc")
	frames, done := framesFrom(t, enc, `{"type":"error","error":{"message":"boom"}}`)
	if len(frames) != 0 || done {
		t.Fatalf("frames = %v (done=%v), want none", frames, done)
	}
	if enc.Started() {
		t.Error("encoder must report Started=false so the handler can answer with JSON")
	}
}

// Finish 幂等；没有 message_stop 的截断流也要收尾。
func TestChatEncoder_FinishContract(t *testing.T) {
	t.Run("idempotent", func(t *testing.T) {
		enc := NewChatEncoder("test-model", "chatcmpl-abc")
		frames, done := framesFrom(t, enc,
			`{"type":"text-delta","text":"hi"}`,
			`{"type":"finish","finishReason":"length","totalUsage":{"inputTokens":5,"outputTokens":3}}`,
		)
		if !done || len(frames) != 4 { // role + text + finish + usage
			t.Fatalf("frames = %d (done=%v)", len(frames), done)
		}
		if v, _ := finishReasonOf(t, frames[len(frames)-2]); v != "length" {
			t.Errorf("finish_reason = %v, want length", v)
		}
		if got := enc.Finish(); got != nil {
			t.Errorf("second Finish = %v, want nil", got)
		}
	})

	t.Run("finish-without-content", func(t *testing.T) {
		// 仅 usage、零内容的极端流：仍要形成合法分片序列（role 声明 + finish + usage + [DONE]）
		enc := NewChatEncoder("test-model", "chatcmpl-abc")
		frames, done := framesFrom(t, enc,
			`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":5,"outputTokens":1}}`,
		)
		if !done || len(frames) != 3 {
			t.Fatalf("frames = %d (done=%v), want 3", len(frames), done)
		}
		if d := deltaOf(t, frames[0]); d["role"] != "assistant" || d["content"] != "" {
			t.Errorf("first frame = %v, want a role declaration", d)
		}
	})
}

// Started 在首个内容增量后翻真（管线据此决定错误能否落在流内）。
func TestChatEncoder_StartedFlag(t *testing.T) {
	enc := NewChatEncoder("test-model", "chatcmpl-abc")
	tr := NewStreamTranslator("test-model", "msg_1")
	for _, ev := range tr.Start() {
		if got := enc.Feed(ev); got != nil {
			t.Fatalf("message_start must not emit frames, got %q", got)
		}
	}
	if enc.Started() {
		t.Error("Started must stay false until the first content delta")
	}
	for _, ev := range tr.Feed([]byte(`{"type":"text-delta","text":"x"}`)) {
		enc.Feed(ev)
	}
	if !enc.Started() {
		t.Error("Started must be true after the first content delta")
	}
}

// ---------- 非流式聚合 ----------

func feedAggregator(agg *ChatAggregator, lines ...string) {
	tr := NewStreamTranslator("test-model", "msg_1")
	for _, ev := range tr.Start() {
		agg.Feed(ev)
	}
	for _, l := range lines {
		for _, ev := range tr.Feed([]byte(l)) {
			agg.Feed(ev)
		}
	}
	for _, ev := range tr.Finish() {
		agg.Feed(ev)
	}
}

func TestChatAggregator_MessageShape(t *testing.T) {
	agg := NewChatAggregator("test-model", "chatcmpl-abc")
	feedAggregator(agg,
		`{"type":"reasoning-delta","text":"think"}`,
		`{"type":"text-delta","text":"Hello"}`,
		`{"type":"text-delta","text":" world"}`,
		`{"type":"tool-call","toolCallId":"call_1","toolName":"read","input":{"path":"a.go"}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":10,"outputTokens":5,"cachedInputTokens":4,"inputTokenDetails":{"noCacheTokens":6,"cacheReadTokens":4,"cacheWriteTokens":2}}}`,
	)
	got := agg.Completion()

	if got.Object != "chat.completion" || got.ID != "chatcmpl-abc" || got.Created == 0 {
		t.Errorf("envelope = %+v", got)
	}
	if len(got.Choices) != 1 || got.Choices[0].Index != 0 {
		t.Fatalf("choices = %+v", got.Choices)
	}
	if fr := got.Choices[0].FinishReason; fr == nil || *fr != "tool_calls" {
		t.Errorf("finish_reason = %v", fr)
	}
	msg := got.Choices[0].Message
	if msg == nil || msg.Role != "assistant" {
		t.Fatalf("message = %+v", msg)
	}
	if msg.Content == nil || *msg.Content != "Hello world" {
		t.Errorf("content = %v", msg.Content)
	}
	if msg.ReasoningContent != "think" {
		t.Errorf("reasoning_content = %q", msg.ReasoningContent)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v", msg.ToolCalls)
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" || tc.Function.Name != "read" || tc.Function.Arguments != `{"path":"a.go"}` {
		t.Errorf("tool_call = %+v", tc)
	}
	if got.Usage == nil || got.Usage.PromptTokens != 12 || got.Usage.CompletionTokens != 5 || got.Usage.TotalTokens != 17 {
		t.Errorf("usage = %+v, want prompt 12 (6+4+2) / completion 5", got.Usage)
	}
	if got.Usage.PromptTokensDetails.CachedTokens != 4 {
		t.Errorf("cached_tokens = %d", got.Usage.PromptTokensDetails.CachedTokens)
	}
}

// 只有工具调用时 content 为 nil（序列化成 null），推理为空时省略。
func TestChatAggregator_ToolCallOnly(t *testing.T) {
	agg := NewChatAggregator("test-model", "chatcmpl-abc")
	feedAggregator(agg,
		`{"type":"tool-call","toolCallId":"call_1","toolName":"noargs"}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":5,"outputTokens":2}}`,
	)
	msg := agg.Completion().Choices[0].Message
	if msg.Content != nil {
		t.Errorf("content = %v, want nil", *msg.Content)
	}
	if msg.ReasoningContent != "" {
		t.Errorf("reasoning_content = %q, want empty", msg.ReasoningContent)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Arguments != `{}` {
		t.Errorf("empty arguments must become {}: %+v", msg.ToolCalls)
	}
}

// 聚合器与流式编码器必须给出同一份语义（同一批事件的两种出站形态）。
func TestChatOut_StreamMatchesAggregate(t *testing.T) {
	lines := []string{
		`{"type":"reasoning-delta","text":"why"}`,
		`{"type":"text-delta","text":"an"}`,
		`{"type":"text-delta","text":"swer"}`,
		`{"type":"tool-call","toolCallId":"call_1","toolName":"read","input":{"path":"a.go","n":2}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":10,"outputTokens":5,"cachedInputTokens":4,"inputTokenDetails":{"noCacheTokens":6,"cacheReadTokens":4}}}`,
	}

	agg := NewChatAggregator("test-model", "chatcmpl-abc")
	feedAggregator(agg, lines...)
	want := agg.Completion()

	enc := NewChatEncoder("test-model", "chatcmpl-abc")
	frames, done := framesFrom(t, enc, lines...)
	if !done {
		t.Fatal("[DONE] missing")
	}

	var text, reasoning, args strings.Builder
	finish := any(nil)
	var usage map[string]any
	for _, f := range frames {
		if d, ok := deltaOfOK(f); ok {
			if s, ok := d["content"].(string); ok {
				text.WriteString(s)
			}
			if s, ok := d["reasoning_content"].(string); ok {
				reasoning.WriteString(s)
			}
			if callList, ok := d["tool_calls"].([]any); ok {
				part, _ := callList[0].(map[string]any)
				fn, _ := part["function"].(map[string]any)
				if s, ok := fn["arguments"].(string); ok {
					args.WriteString(s)
				}
			}
		}
		if v, ok := finishReasonOK(f); ok && v != nil {
			finish = v
		}
		if u, ok := f["usage"].(map[string]any); ok {
			usage = u
		}
	}

	wantMsg := want.Choices[0].Message
	if text.String() != *wantMsg.Content {
		t.Errorf("stream text = %q, aggregate = %q", text.String(), *wantMsg.Content)
	}
	if reasoning.String() != wantMsg.ReasoningContent {
		t.Errorf("stream reasoning = %q, aggregate = %q", reasoning.String(), wantMsg.ReasoningContent)
	}
	if args.String() != wantMsg.ToolCalls[0].Function.Arguments {
		t.Errorf("stream arguments = %q, aggregate = %q", args.String(), wantMsg.ToolCalls[0].Function.Arguments)
	}
	if finish != *want.Choices[0].FinishReason {
		t.Errorf("stream finish = %v, aggregate = %v", finish, *want.Choices[0].FinishReason)
	}
	if usage["prompt_tokens"] != float64(want.Usage.PromptTokens) || usage["completion_tokens"] != float64(want.Usage.CompletionTokens) {
		t.Errorf("stream usage = %v, aggregate = %+v", usage, want.Usage)
	}
}

// ---------- 往返对称（§8.5） ----------

// 入站 assistant 轮次（推理 + 文本 + 工具调用）经归一化后，其语义必须能从
// 出站聚合结果中原样取回，并且还能再次作为入站消息被接受（闭环）。
func TestChatRoundTrip_RequestToResponseAndBack(t *testing.T) {
	req := chatReq(t, `{
		"model": "gpt-4o",
		"messages": [
			{"role": "system", "content": "be terse"},
			{"role": "user", "content": "read a.go"},
			{"role": "assistant", "content": "reading", "reasoning_content": "need to read",
			 "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "read", "arguments": "{\"path\":\"a.go\"}"}}]},
			{"role": "tool", "tool_call_id": "call_1", "content": "package main"}
		]
	}`)
	norm, _ := mustChatToRequest(t, req)
	assertPairing(t, norm.Messages)

	asst := blocksOf(t, norm.Messages[1])
	if len(asst) != 3 || asst[0].Type != "thinking" || asst[1].Type != "text" || asst[2].Type != "tool_use" {
		t.Fatalf("normalized assistant blocks = %+v", asst)
	}
	if string(norm.Messages[2].Content) == "" || !hasToolResult(blocksOf(t, norm.Messages[2]), "call_1") {
		t.Fatalf("normalized tool result = %s", norm.Messages[2].Content)
	}

	// 模型复现同一轮：推理 → 文本 → 同一工具调用
	agg := NewChatAggregator("gpt-4o", "chatcmpl-abc")
	feedAggregator(agg,
		`{"type":"reasoning-delta","text":"need to read"}`,
		`{"type":"text-delta","text":"reading"}`,
		`{"type":"tool-call","toolCallId":"call_1","toolName":"read","input":{"path":"a.go"}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":12,"outputTokens":4,"cachedInputTokens":0,"inputTokenDetails":{"noCacheTokens":12}}}`,
	)
	out := agg.Completion().Choices[0].Message

	if out.Content == nil || *out.Content != asst[1].Text {
		t.Errorf("content = %v, want %q", out.Content, asst[1].Text)
	}
	if out.ReasoningContent != asst[0].Thinking {
		t.Errorf("reasoning = %q, want %q", out.ReasoningContent, asst[0].Thinking)
	}
	if len(out.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v", out.ToolCalls)
	}
	tc := out.ToolCalls[0]
	if tc.ID != asst[2].ID || tc.Function.Name != asst[2].Name {
		t.Errorf("tool call identity lost: %+v vs %+v", tc, asst[2])
	}
	if tc.Function.Arguments != string(asst[2].Input) {
		t.Errorf("arguments = %q, want %q", tc.Function.Arguments, asst[2].Input)
	}

	// 闭环：出站 message 必须能再次作为入站 assistant 消息被接受，语义不变。
	// 客户端会把工具调用与它的结果一起回传（store:false 全量重发），少了结果这一半，
	// 配对修复会（正确地）把调用当悬空调用清掉。
	echo := chatReq(t, `{"model":"gpt-4o","messages":[`+rawString(t, map[string]any{
		"role":              "assistant",
		"content":           *out.Content,
		"reasoning_content": out.ReasoningContent,
		"tool_calls": []any{map[string]any{
			"id": tc.ID, "type": tc.Type,
			"function": map[string]any{"name": tc.Function.Name, "arguments": tc.Function.Arguments},
		}},
	})+`,`+rawString(t, map[string]any{
		"role": "tool", "tool_call_id": tc.ID, "content": "package main",
	})+`]}`)
	echoNorm, _ := mustChatToRequest(t, echo)
	back := blocksOf(t, echoNorm.Messages[0])
	if len(back) != 3 || back[0].Type != "thinking" || back[0].Thinking != asst[0].Thinking {
		t.Fatalf("echoed blocks = %+v", back)
	}
	if back[2].ID != asst[2].ID || back[2].Name != asst[2].Name || string(back[2].Input) != string(asst[2].Input) {
		t.Errorf("echoed tool_use lost meaning: %+v", back[2])
	}
}

func rawString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// ---------- 小工具 ----------

// 分片按 rune 切分：多字节字符不能被切断（否则客户端拼出损坏的 UTF-8）。
func TestSplitRunes(t *testing.T) {
	cases := []struct {
		in   string
		size int
		want []string
	}{
		{"", 10, nil},
		{"short", 10, []string{"short"}},
		{"0123456789abc", 10, []string{"0123456789", "abc"}},
		{"中文中文中文中文中文中", 10, []string{"中文中文中文中文中文", "中"}},
	}
	for _, tc := range cases {
		got := chatSplitRunes(tc.in, tc.size)
		if !equalStrings(got, tc.want) {
			t.Errorf("chatSplitRunes(%q, %d) = %v, want %v", tc.in, tc.size, got, tc.want)
		}
	}
}

// finish_reason 全表映射。
func TestChatFinishReason(t *testing.T) {
	cases := map[string]string{
		"end_turn": "stop", "stop_sequence": "stop", "": "stop",
		"max_tokens": "length", "tool_use": "tool_calls", "refusal": "content_filter",
		"unknown": "stop",
	}
	for in, want := range cases {
		if got := chatFinishReason(in); got != want {
			t.Errorf("chatFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

// 流内错误帧的 OpenAI 形状映射表（与 server 侧错误出口同源）。
func TestOpenAIErrorShape(t *testing.T) {
	cases := []struct{ in, typ, code string }{
		{"authentication_error", "invalid_request_error", "invalid_api_key"},
		{"invalid_request_error", "invalid_request_error", "invalid_request_error"},
		{"rate_limit_error", "rate_limit_error", "rate_limit_exceeded"},
		{"not_found_error", "not_found_error", "not_found_error"},
		{"api_error", "server_error", "server_error"},
		{"overloaded_error", "server_error", "server_error"},
	}
	for _, tc := range cases {
		body := OpenAIErrorBody(tc.in, "msg")
		if body["type"] != tc.typ || body["code"] != tc.code || body["message"] != "msg" {
			t.Errorf("OpenAIErrorBody(%q) = %v, want %s/%s", tc.in, body, tc.typ, tc.code)
		}
		if v, ok := body["param"]; !ok || v != nil {
			t.Errorf("param = %v (present=%v), want explicit null", v, ok)
		}
	}
}

// 响应 ID 与 Result 接口出口。
func TestChatCompletionIDAndResult(t *testing.T) {
	id := NewChatCompletionID()
	if !strings.HasPrefix(id, "chatcmpl-") || len(id) != len("chatcmpl-")+24 {
		t.Errorf("id = %q, want chatcmpl-<24 hex chars>", id)
	}
	if NewChatCompletionID() == id {
		t.Error("ids must not repeat")
	}

	agg := NewChatAggregator("m", "chatcmpl-abc")
	feedAggregator(agg, `{"type":"text-delta","text":"hi"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":1}}`)
	viaResult, ok := agg.Result().(*types.ChatCompletion)
	if !ok {
		t.Fatalf("Result() = %T, want *types.ChatCompletion", agg.Result())
	}
	if direct := agg.Completion(); viaResult.ID != direct.ID || viaResult.Object != direct.Object ||
		*viaResult.Choices[0].Message.Content != *direct.Choices[0].Message.Content {
		t.Errorf("Result() and Completion() disagree: %+v vs %+v", viaResult, direct)
	}
}
