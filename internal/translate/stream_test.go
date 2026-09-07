package translate

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/B1anYu/cmdc2api/internal/types"
)

func feedAll(t *testing.T, tr *StreamTranslator, lines ...string) []types.StreamEvent {
	t.Helper()
	var out []types.StreamEvent
	out = append(out, tr.Start()...)
	for _, l := range lines {
		out = append(out, tr.Feed([]byte(l))...)
	}
	out = append(out, tr.Finish()...)
	return out
}

// eventNames 提取事件名序列，便于断言顺序。
func eventNames(evs []types.StreamEvent) []string {
	names := make([]string, len(evs))
	for i, e := range evs {
		names[i] = e.Name
	}
	return names
}

func dataJSON(t *testing.T, e types.StreamEvent, dst any) {
	t.Helper()
	b, err := json.Marshal(e.Data)
	if err != nil {
		t.Fatalf("marshal event data: %v", err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatalf("unmarshal event data %s: %v", b, err)
	}
}

func TestStream_FullLifecycleBlockInvariants(t *testing.T) {
	tr := NewStreamTranslator("test-model", "msg_test")
	evs := feedAll(t, tr,
		`{"type":"start"}`,
		`{"type":"reasoning-start"}`,
		`{"type":"reasoning-delta","text":"thinking hard"}`,
		`{"type":"reasoning-end"}`,
		`{"type":"text-start"}`,
		`{"type":"text-delta","text":"Hello"}`,
		`{"type":"tool-call","toolCallId":"toolu_1","toolName":"get_weather","input":{"city":"SF"}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":100,"outputTokens":42,"cachedInputTokens":10,"inputTokenDetails":{"cacheWriteTokens":5}}}`,
	)

	want := []string{
		"message_start",
		"content_block_start",  // thinking idx 0
		"content_block_delta",  // thinking_delta
		"content_block_delta",  // signature_delta（关 thinking 块前必发）
		"content_block_stop",   // idx 0
		"content_block_start",  // text idx 1
		"content_block_delta",  // text_delta
		"content_block_stop",   // idx 1（tool-call 前先关 text 块）
		"content_block_start",  // tool_use idx 2
		"content_block_delta",  // input_json_delta
		"content_block_stop",   // idx 2
		"message_delta",
		"message_stop",
	}
	got := eventNames(evs)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence:\ngot  %v\nwant %v", got, want)
	}

	// 逐事件核对载荷
	var blk types.ContentBlockStartEvent
	dataJSON(t, evs[1], &blk)
	if blk.Index != 0 {
		t.Errorf("thinking block index = %d", blk.Index)
	}
	b, _ := json.Marshal(blk.ContentBlock)
	if !strings.Contains(string(b), `"thinking"`) {
		t.Errorf("block 0 not thinking: %s", b)
	}

	var sig types.ContentBlockDeltaEvent
	dataJSON(t, evs[3], &sig)
	if !strings.Contains(string(mustJSON(t, sig.Delta)), "signature_delta") {
		t.Errorf("event 3 should be signature_delta: %v", sig.Delta)
	}

	var delta types.ContentBlockDeltaEvent
	dataJSON(t, evs[9], &delta)
	var ij struct {
		Type        string `json:"type"`
		PartialJSON string `json:"partial_json"`
	}
	if err := json.Unmarshal([]byte(mustJSON(t, delta.Delta)), &ij); err != nil {
		t.Fatalf("unmarshal input_json_delta: %v", err)
	}
	if ij.Type != "input_json_delta" || ij.PartialJSON != `{"city":"SF"}` {
		t.Errorf("input_json_delta wrong: %+v", ij)
	}

	var md types.MessageDeltaEvent
	dataJSON(t, evs[11], &md)
	if md.Delta.StopReason != "tool_use" {
		t.Errorf("stop_reason = %s, want tool_use", md.Delta.StopReason)
	}
	if md.Usage.OutputTokens != 42 || md.Usage.InputTokens != 100 ||
		md.Usage.CacheReadInputTokens != 10 || md.Usage.CacheCreationInputTokens == nil || *md.Usage.CacheCreationInputTokens != 5 {
		t.Errorf("message_delta usage wrong: %+v", md.Usage)
	}
}

func TestStream_AggregationMatchesStreamShape(t *testing.T) {
	tr := NewStreamTranslator("test-model", "msg_agg")
	evs := feedAll(t, tr,
		`{"type":"reasoning-delta","text":"hmm"}`,
		`{"type":"text-delta","text":"answer"}`,
		`{"type":"tool-call","toolCallId":"toolu_2","toolName":"f","input":{"k":1}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":7,"outputTokens":9}}`,
	)
	agg := NewAggregator()
	for _, e := range evs {
		agg.Feed(e)
	}
	msg := agg.Message()
	if msg.ID != "msg_agg" || msg.Role != "assistant" || msg.Model != "test-model" {
		t.Errorf("message meta wrong: %+v", msg)
	}
	if len(msg.Content) != 3 {
		t.Fatalf("content = %d blocks, want 3: %+v", len(msg.Content), msg.Content)
	}
	if msg.Content[0].Type != "thinking" || msg.Content[0].Thinking != "hmm" || msg.Content[0].Signature == "" {
		t.Errorf("thinking block wrong: %+v", msg.Content[0])
	}
	if msg.Content[1].Type != "text" || msg.Content[1].Text != "answer" {
		t.Errorf("text block wrong: %+v", msg.Content[1])
	}
	tu := msg.Content[2]
	if tu.Type != "tool_use" || tu.ID != "toolu_2" || tu.Name != "f" || string(tu.Input) != `{"k":1}` {
		t.Errorf("tool_use block wrong: %+v", tu)
	}
	if msg.StopReason != "tool_use" {
		t.Errorf("stop_reason = %s", msg.StopReason)
	}
	if msg.Usage.InputTokens != 7 || msg.Usage.OutputTokens != 9 {
		t.Errorf("usage wrong: %+v", msg.Usage)
	}
}

func TestStream_ErrorEventMappedAndFinishSuppressed(t *testing.T) {
	tr := NewStreamTranslator("m", "msg_e")
	var evs []types.StreamEvent
	evs = append(evs, tr.Start()...)
	evs = append(evs, tr.Feed([]byte(`{"type":"error","error":{"message":"<429> slow down"}}`))...)
	fin := tr.Finish()
	if len(fin) != 0 {
		t.Errorf("Finish after error should emit nothing, got %v", eventNames(fin))
	}
	if !tr.HasUpstreamError() || tr.ErrMessage() != "<429> slow down" {
		t.Fatalf("error state wrong: %+v", tr)
	}
	// 流内 error 事件带 Anthropic 类型映射
	found := false
	for _, e := range evs {
		if e.Name == "error" {
			var ee types.ErrorEvent
			dataJSON(t, e, &ee)
			if ee.Error.Type != "rate_limit_error" {
				t.Errorf("error event type = %s", ee.Error.Type)
			}
			found = true
		}
	}
	if !found {
		t.Error("error event not emitted")
	}
}

func TestStream_ZeroOutputBecomesError(t *testing.T) {
	tr := NewStreamTranslator("m", "msg_z")
	evs := feedAll(t, tr,
		`{"type":"text-delta","text":"hi"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":50,"outputTokens":0,"cachedInputTokens":30}}`,
	)
	last := evs[len(evs)-1]
	if last.Name != "error" {
		t.Fatalf("zero output should end with error event, got %v", eventNames(evs))
	}
	var ee types.ErrorEvent
	dataJSON(t, last, &ee)
	if ee.Error.Type != "rate_limit_error" || !strings.Contains(ee.Error.Message, "zero output") {
		t.Errorf("zero-output error wrong: %+v", ee.Error)
	}
	// 零输出时 input/cached 一并清零（反虚假计费）
	var md types.MessageDeltaEvent
	_ = md
	if tr.DeltaUsage().InputTokens != 0 || tr.DeltaUsage().CacheReadInputTokens != 0 {
		t.Errorf("usage not zeroed on zero output: %+v", tr.DeltaUsage())
	}
}

func TestStream_MissingIDGeneratesToolu(t *testing.T) {
	tr := NewStreamTranslator("m", "msg_t")
	evs := feedAll(t, tr,
		`{"type":"tool-call","toolName":"f","input":{}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":1,"outputTokens":20}}`,
	)
	for _, e := range evs {
		if e.Name == "content_block_start" {
			var blk types.ContentBlockStartEvent
			dataJSON(t, e, &blk)
			b, _ := json.Marshal(blk.ContentBlock)
			if !strings.Contains(string(b), `"id":"toolu_`) {
				t.Errorf("generated tool id missing: %s", b)
			}
		}
	}
}

func TestMapStopReason(t *testing.T) {
	cases := map[string]string{
		"stop": "end_turn", "length": "max_tokens", "tool-calls": "tool_use",
		"content-filter": "refusal", "unknown": "end_turn", "": "end_turn",
	}
	for in, want := range cases {
		if got := MapStopReason(in); got != want {
			t.Errorf("MapStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFakeThinkingSignature(t *testing.T) {
	s1 := FakeThinkingSignature("some thinking")
	s2 := FakeThinkingSignature("some thinking")
	if s1 != s2 {
		t.Error("signature must be deterministic")
	}
	if s1 == FakeThinkingSignature("other thinking") {
		t.Error("different text must yield different signature")
	}
	raw, err := base64.StdEncoding.DecodeString(s1)
	if err != nil {
		t.Fatalf("signature not base64: %v", err)
	}
	if raw[0] != 0x12 {
		t.Errorf("payload first byte = %#x, want 0x12", raw[0])
	}
	if s1[0] != 'E' {
		t.Errorf("base64 must start with 'E' (Claude Code shallow check), got %q", s1[0])
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
