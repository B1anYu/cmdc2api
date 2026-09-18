package types

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// ---------- 测试辅助 ----------

// frameData 走真实线上路径（Encode 出的 SSE 帧）取 data 载荷，
// 保证断言的是客户端实际收到的字节，而不是某个中间结构。
func frameData(t *testing.T, ev StreamEvent) map[string]any {
	t.Helper()
	frame := string(ev.Encode())
	prefix := "event: " + ev.Name + "\ndata: "
	if !strings.HasPrefix(frame, prefix) {
		t.Fatalf("frame = %q, want prefix %q", frame, prefix)
	}
	if !strings.HasSuffix(frame, "\n\n") {
		t.Fatalf("frame = %q, want trailing blank line", frame)
	}
	if strings.Contains(frame, "[DONE]") {
		t.Fatalf("frame = %q, Responses 流不得出现 [DONE] 哨兵", frame)
	}
	body := frame[len(prefix) : len(frame)-2]
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode frame payload %s: %v", body, err)
	}
	return m
}

func requireKeys(t *testing.T, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing key %q in %v", k, m)
		}
	}
}

func requireNoKeys(t *testing.T, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if v, ok := m[k]; ok {
			t.Fatalf("unexpected key %q (= %v) in %v", k, v, m)
		}
	}
}

func childMap(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("missing key %q in %v", key, m)
	}
	child, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("key %q = %#v, want JSON object", key, v)
	}
	return child
}

func childList(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("missing key %q in %v", key, m)
	}
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("key %q = %#v, want JSON array", key, v)
	}
	return list
}

// jsonMap 把任意 Go 值过一遍 JSON，得到与客户端所见完全一致的形状
// （Wire 出来的是强类型切片，直接断言会踩到类型而非协议）。
func jsonMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return m
}

// itemWire 取 output item 的线上形状。
func itemWire(t *testing.T, item ResponsesOutputItem) map[string]any {
	t.Helper()
	return jsonMap(t, item)
}

func sampleResponse() ResponsesResponse {
	return ResponsesResponse{
		ID:        "resp_abc123",
		Model:     "claude-sonnet-4-6",
		CreatedAt: 1758000000,
		Status:    ResponsesStatusInProgress,
		Output: []ResponsesOutputItem{
			NewResponsesMessageItem("msg_1", ResponsesStatusCompleted, ResponsesOutputTextPart{Text: "hi"}),
		},
		Usage: NewResponsesUsage(10, 5, 3),
	}
}

// ---------- 事件名常量全集 ----------

func TestResponsesEventNames_ExactSet(t *testing.T) {
	got := []string{
		ResponsesEventCreated,
		ResponsesEventInProgress,
		ResponsesEventOutputItemAdded,
		ResponsesEventOutputItemDone,
		ResponsesEventContentPartAdded,
		ResponsesEventContentPartDone,
		ResponsesEventOutputTextDelta,
		ResponsesEventOutputTextDone,
		ResponsesEventReasoningSummaryPartAdded,
		ResponsesEventReasoningSummaryPartDone,
		ResponsesEventReasoningSummaryTextDelta,
		ResponsesEventReasoningSummaryTextDone,
		ResponsesEventFunctionCallArgsDelta,
		ResponsesEventFunctionCallArgsDone,
		ResponsesEventCustomToolCallInputDelta,
		ResponsesEventCustomToolCallInputDone,
		ResponsesEventCompleted,
		ResponsesEventIncomplete,
		ResponsesEventFailed,
	}
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.output_item.done",
		"response.content_part.added",
		"response.content_part.done",
		"response.output_text.delta",
		"response.output_text.done",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.done",
		"response.completed",
		"response.incomplete",
		"response.failed",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event name constants drifted:\n got %v\nwant %v", got, want)
	}
}

// ---------- 每个构造函数：type / sequence_number 恒在且来自参数 ----------

func TestResponsesEvents_TypeAndSequenceNumberAlwaysPresent(t *testing.T) {
	const seq = 23
	item := NewResponsesMessageItem("msg_1", ResponsesStatusCompleted)
	resp := sampleResponse()

	events := []StreamEvent{
		NewResponsesCreated(seq, resp),
		NewResponsesInProgress(seq, resp),
		NewResponsesOutputItemAdded(seq, 0, item),
		NewResponsesOutputItemDone(seq, 0, item),
		NewResponsesContentPartAdded(seq, 0, 0, "msg_1"),
		NewResponsesContentPartDone(seq, 0, 0, "msg_1", "hi"),
		NewResponsesOutputTextDelta(seq, 0, 0, "msg_1", "h"),
		NewResponsesOutputTextDone(seq, 0, 0, "msg_1", "hi"),
		NewResponsesReasoningSummaryPartAdded(seq, 0, 0, ""),
		NewResponsesReasoningSummaryPartDone(seq, 0, 0, "", "think"),
		NewResponsesReasoningSummaryTextDelta(seq, 0, 0, "", "t"),
		NewResponsesReasoningSummaryTextDone(seq, 0, 0, "", "think"),
		NewResponsesFunctionCallArgumentsDelta(seq, 1, "fc_1", "call_1", "get_weather", "{"),
		NewResponsesFunctionCallArgumentsDone(seq, 1, "fc_1", "call_1", "get_weather", "{}"),
		NewResponsesCustomToolCallInputDelta(seq, 0, "ctc_1", "call_1", "exec", "l"),
		NewResponsesCustomToolCallInputDone(seq, 0, "ctc_1", "call_1", "exec", "ls"),
		NewResponsesCompleted(seq, resp),
		NewResponsesIncomplete(seq, resp, "max_output_tokens"),
		NewResponsesFailed(seq, resp, "server_error", "boom"),
	}
	wantNames := []string{
		ResponsesEventCreated, ResponsesEventInProgress,
		ResponsesEventOutputItemAdded, ResponsesEventOutputItemDone,
		ResponsesEventContentPartAdded, ResponsesEventContentPartDone,
		ResponsesEventOutputTextDelta, ResponsesEventOutputTextDone,
		ResponsesEventReasoningSummaryPartAdded, ResponsesEventReasoningSummaryPartDone,
		ResponsesEventReasoningSummaryTextDelta, ResponsesEventReasoningSummaryTextDone,
		ResponsesEventFunctionCallArgsDelta, ResponsesEventFunctionCallArgsDone,
		ResponsesEventCustomToolCallInputDelta, ResponsesEventCustomToolCallInputDone,
		ResponsesEventCompleted, ResponsesEventIncomplete, ResponsesEventFailed,
	}
	if len(events) != len(wantNames) {
		t.Fatalf("构造器数量 %d != 事件名数量 %d", len(events), len(wantNames))
	}
	for i, ev := range events {
		if ev.Name != wantNames[i] {
			t.Fatalf("event[%d].Name = %q, want %q", i, ev.Name, wantNames[i])
		}
		m := frameData(t, ev)
		requireKeys(t, m, "type", "sequence_number")
		if m["type"] != wantNames[i] {
			t.Fatalf("%s: type = %v, want %q", ev.Name, m["type"], wantNames[i])
		}
		if m["sequence_number"] != float64(seq) {
			t.Fatalf("%s: sequence_number = %v, want %d", ev.Name, m["sequence_number"], seq)
		}
	}
}

// ---------- 索引 0 必须出现 ----------

func TestResponsesEvents_ZeroIndexesAlwaysPresent(t *testing.T) {
	cases := []struct {
		label string
		ev    StreamEvent
		index string
	}{
		{"content_part.added", NewResponsesContentPartAdded(1, 0, 0, "msg_1"), "content_index"},
		{"content_part.done", NewResponsesContentPartDone(1, 0, 0, "msg_1", "x"), "content_index"},
		{"output_text.delta", NewResponsesOutputTextDelta(1, 0, 0, "msg_1", "x"), "content_index"},
		{"output_text.done", NewResponsesOutputTextDone(1, 0, 0, "msg_1", "x"), "content_index"},
		{"reasoning_summary_part.added", NewResponsesReasoningSummaryPartAdded(1, 0, 0, ""), "summary_index"},
		{"reasoning_summary_part.done", NewResponsesReasoningSummaryPartDone(1, 0, 0, "", "x"), "summary_index"},
		{"reasoning_summary_text.delta", NewResponsesReasoningSummaryTextDelta(1, 0, 0, "", "x"), "summary_index"},
		{"reasoning_summary_text.done", NewResponsesReasoningSummaryTextDone(1, 0, 0, "", "x"), "summary_index"},
		{"output_item.added", NewResponsesOutputItemAdded(1, 0, NewResponsesMessageItem("msg_1", "")), "output_index"},
		{"output_item.done", NewResponsesOutputItemDone(1, 0, NewResponsesMessageItem("msg_1", "")), "output_index"},
		{"function_call_arguments.delta", NewResponsesFunctionCallArgumentsDelta(1, 0, "fc_1", "c", "f", "x"), "output_index"},
		{"custom_tool_call_input.delta", NewResponsesCustomToolCallInputDelta(1, 0, "ctc_1", "c", "f", "x"), "output_index"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			m := frameData(t, tc.ev)
			requireKeys(t, m, "output_index", tc.index)
			if m["output_index"] != float64(0) {
				t.Fatalf("output_index = %v, want 0", m["output_index"])
			}
			if m[tc.index] != float64(0) {
				t.Fatalf("%s = %v, want 0", tc.index, m[tc.index])
			}
		})
	}
}

func TestResponsesTextEventPayloads(t *testing.T) {
	added := frameData(t, NewResponsesContentPartAdded(1, 2, 0, "msg_9"))
	if added["item_id"] != "msg_9" {
		t.Fatalf("item_id = %v, want msg_9", added["item_id"])
	}
	part := childMap(t, added, "part")
	requireKeys(t, part, "type", "text", "annotations", "logprobs")
	if part["type"] != "output_text" || part["text"] != "" {
		t.Fatalf("added part = %v, want 空 output_text", part)
	}
	if !reflect.DeepEqual(part["annotations"], []any{}) || !reflect.DeepEqual(part["logprobs"], []any{}) {
		t.Fatalf("part 的 annotations/logprobs 必须是空数组，got %v", part)
	}

	done := frameData(t, NewResponsesContentPartDone(1, 2, 1, "msg_9", "全文"))
	donePart := childMap(t, done, "part")
	if donePart["text"] != "全文" {
		t.Fatalf("done part.text = %v, want 全文", donePart["text"])
	}

	delta := frameData(t, NewResponsesOutputTextDelta(1, 0, 0, "msg_9", "增"))
	if delta["delta"] != "增" {
		t.Fatalf("delta = %v", delta["delta"])
	}
	textDone := frameData(t, NewResponsesOutputTextDone(1, 0, 0, "msg_9", "全文"))
	if textDone["text"] != "全文" {
		t.Fatalf("text = %v", textDone["text"])
	}
	requireNoKeys(t, textDone, "delta")
}

func TestResponsesReasoningEvents(t *testing.T) {
	// reasoning item 没有 id：item_id 不该被塞成空串。
	delta := frameData(t, NewResponsesReasoningSummaryTextDelta(1, 0, 0, "", "思"))
	requireNoKeys(t, delta, "item_id")
	if delta["summary_index"] != float64(0) {
		t.Fatalf("summary_index = %v", delta["summary_index"])
	}

	partDone := frameData(t, NewResponsesReasoningSummaryPartDone(1, 0, 0, "rs_1", "思考全文"))
	requireKeys(t, partDone, "item_id", "summary_index", "part")
	part := childMap(t, partDone, "part")
	if part["type"] != "summary_text" || part["text"] != "思考全文" {
		t.Fatalf("summary part = %v", part)
	}
}

func TestResponsesToolCallEvents_CallIDAndNameOmittedWhenEmpty(t *testing.T) {
	ev := NewResponsesFunctionCallArgumentsDone(1, 3, "fc_1", "call_1", "get_weather", `{"city":"SF"}`)
	m := frameData(t, ev)
	requireKeys(t, m, "output_index", "item_id", "call_id", "name", "arguments")
	if m["arguments"] != `{"city":"SF"}` {
		t.Fatalf("arguments = %v", m["arguments"])
	}
	requireNoKeys(t, m, "delta")

	deltaEv := frameData(t, NewResponsesFunctionCallArgumentsDelta(1, 3, "fc_1", "call_1", "get_weather", `{"ci`))
	if deltaEv["delta"] != `{"ci` {
		t.Fatalf("delta = %v", deltaEv["delta"])
	}
	requireNoKeys(t, deltaEv, "arguments")

	// 空 call_id / name 时不出键（工具名晚于参数到达的场景）。
	sparse := frameData(t, NewResponsesFunctionCallArgumentsDelta(1, 3, "fc_1", "", "", "x"))
	requireNoKeys(t, sparse, "call_id", "name")
}

func TestResponsesCustomToolCallEvents_InputAlwaysPresent(t *testing.T) {
	done := frameData(t, NewResponsesCustomToolCallInputDone(1, 0, "ctc_1", "call_1", "exec", ""))
	requireKeys(t, done, "output_index", "item_id", "call_id", "name", "input")
	if done["input"] != "" {
		t.Fatalf("input = %v, want 空串", done["input"])
	}
	requireNoKeys(t, done, "delta", "arguments")
}

// ---------- output item 的字段存在性硬约束 ----------

func TestResponsesItemWire_FunctionCallArgumentsKeyAlwaysPresent(t *testing.T) {
	item := NewResponsesFunctionCallItem("fc_1", "call_1", "get_weather", "", "", ResponsesStatusInProgress)
	m := itemWire(t, item)
	requireKeys(t, m, "type", "id", "call_id", "name", "arguments", "status")
	if m["arguments"] != "" {
		t.Fatalf("arguments = %v, want 空串但键必须在", m["arguments"])
	}
	requireNoKeys(t, m, "namespace", "input", "execution")

	ns := itemWire(t, NewResponsesFunctionCallItem("fc_2", "call_2", "read_file", "filesystem", "{}", ResponsesStatusCompleted))
	if ns["namespace"] != "filesystem" {
		t.Fatalf("namespace = %v, want filesystem", ns["namespace"])
	}
}

func TestResponsesItemWire_MessageContentIsAlwaysArray(t *testing.T) {
	m := itemWire(t, NewResponsesMessageItem("msg_1", ""))
	requireKeys(t, m, "type", "id", "role", "status", "content")
	if m["role"] != "assistant" {
		t.Fatalf("role = %v, want assistant", m["role"])
	}
	if m["status"] != ResponsesStatusInProgress {
		t.Fatalf("status = %v, want in_progress", m["status"])
	}
	if !reflect.DeepEqual(m["content"], []any{}) {
		t.Fatalf("content = %#v, want 空数组（不能是 null）", m["content"])
	}

	withParts := itemWire(t, NewResponsesMessageItem("msg_2", ResponsesStatusCompleted,
		ResponsesOutputTextPart{Text: "第一段"}, ResponsesOutputTextPart{Text: "第二段"}))
	parts := childList(t, withParts, "content")
	if len(parts) != 2 {
		t.Fatalf("content 长度 = %d, want 2", len(parts))
	}
	first, _ := parts[0].(map[string]any)
	requireKeys(t, first, "type", "text", "annotations", "logprobs")
	if first["type"] != "output_text" || first["text"] != "第一段" {
		t.Fatalf("part[0] = %v", first)
	}
	if !reflect.DeepEqual(first["annotations"], []any{}) || !reflect.DeepEqual(first["logprobs"], []any{}) {
		t.Fatalf("part[0] 的 annotations/logprobs 必须是空数组，got %v", first)
	}
}

func TestResponsesItemWire_ReasoningHasNoIDAndNoNullFields(t *testing.T) {
	m := itemWire(t, NewResponsesReasoningItem(""))
	requireKeys(t, m, "type", "summary")
	// OpenAI 未给 reasoning item 签发 id；status/content 为 null 会让 C# SDK 崩。
	requireNoKeys(t, m, "id", "status", "content", "encrypted_content")
	if !reflect.DeepEqual(m["summary"], []any{}) {
		t.Fatalf("summary = %#v, want 空数组", m["summary"])
	}

	signed := itemWire(t, NewResponsesReasoningItem("sig-abc",
		ResponsesSummaryPart{Text: "想了一下"}))
	if signed["encrypted_content"] != "sig-abc" {
		t.Fatalf("encrypted_content = %v", signed["encrypted_content"])
	}
	requireNoKeys(t, signed, "id", "status", "content")
	summary := childList(t, signed, "summary")
	if len(summary) != 1 {
		t.Fatalf("summary 长度 = %d, want 1", len(summary))
	}
	part, _ := summary[0].(map[string]any)
	if part["type"] != "summary_text" || part["text"] != "想了一下" {
		t.Fatalf("summary[0] = %v", part)
	}
}

func TestResponsesItemWire_CustomToolCallKeepsEmptyInput(t *testing.T) {
	m := itemWire(t, NewResponsesCustomToolCallItem("ctc_1", "call_1", "exec", "", ResponsesStatusInProgress))
	requireKeys(t, m, "type", "id", "call_id", "name", "input", "status")
	if m["input"] != "" {
		t.Fatalf("input = %v, want 空串（不补 \"{}\"）", m["input"])
	}
	requireNoKeys(t, m, "arguments", "execution")
}

func TestResponsesItemWire_ToolSearchArgumentsIsObject(t *testing.T) {
	cases := []struct {
		label string
		in    any
		want  map[string]any
	}{
		{"nil", nil, map[string]any{}},
		{"对象", map[string]any{"query": "slack"}, map[string]any{"query": "slack"}},
		{"JSON 字符串", `{"limit":3}`, map[string]any{"limit": float64(3)}},
		{"RawMessage", json.RawMessage(`{"query":"x"}`), map[string]any{"query": "x"}},
		{"字节切片", []byte(`{"query":"y"}`), map[string]any{"query": "y"}},
		{"其它对象类型", map[string]string{"query": "z"}, map[string]any{"query": "z"}},
		{"非法字符串", "not json", map[string]any{}},
		{"空字符串", "", map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			m := itemWire(t, NewResponsesToolSearchCallItem("tsc_1", "call_1", tc.in, ResponsesStatusInProgress))
			requireKeys(t, m, "type", "id", "call_id", "execution", "arguments", "status")
			if m["type"] != "tool_search_call" || m["execution"] != "client" {
				t.Fatalf("item = %v", m)
			}
			if !reflect.DeepEqual(m["arguments"], tc.want) {
				t.Fatalf("arguments = %#v, want %#v（线上必须是对象）", m["arguments"], tc.want)
			}
		})
	}
}

func TestResponsesItemWire_UnknownTypeFallsBackWithoutNulls(t *testing.T) {
	// 未知类型不该发生，但兜底形状也不允许出现 null 字段。
	m := itemWire(t, ResponsesOutputItem{Type: "web_search_call", ID: "ws_1"})
	requireKeys(t, m, "type", "id", "status")
	requireNoKeys(t, m, "content", "summary", "arguments", "input", "call_id")
}

func TestResponsesItemWire_AddedAndDoneStatusDefaults(t *testing.T) {
	added := frameData(t, NewResponsesOutputItemAdded(1, 0, NewResponsesMessageItem("msg_1", "")))
	addedItem := childMap(t, added, "item")
	if addedItem["status"] != ResponsesStatusInProgress {
		t.Fatalf("added status = %v, want in_progress", addedItem["status"])
	}

	done := frameData(t, NewResponsesOutputItemDone(1, 0, NewResponsesMessageItem("msg_1", "")))
	doneItem := childMap(t, done, "item")
	if doneItem["status"] != ResponsesStatusCompleted {
		t.Fatalf("done status = %v, want completed", doneItem["status"])
	}

	explicit := frameData(t, NewResponsesOutputItemDone(1, 0, NewResponsesMessageItem("msg_1", ResponsesStatusIncomplete)))
	if childMap(t, explicit, "item")["status"] != ResponsesStatusIncomplete {
		t.Fatalf("done 显式 status 被覆盖")
	}
}

// ---------- usage ----------

func TestResponsesUsageWire_AllFieldsPresentIncludingZero(t *testing.T) {
	m := frameData(t, NewResponsesCompleted(1, ResponsesResponse{Usage: ResponsesUsage{}}))
	usage := childMap(t, m, "response")
	usage = childMap(t, usage, "usage")
	requireKeys(t, usage, "input_tokens", "output_tokens", "total_tokens", "input_tokens_details")
	details := childMap(t, usage, "input_tokens_details")
	requireKeys(t, details, "cached_tokens")
	if details["cached_tokens"] != float64(0) {
		t.Fatalf("cached_tokens = %v, want 0", details["cached_tokens"])
	}
}

func TestNewResponsesUsage_TotalIsSum(t *testing.T) {
	u := NewResponsesUsage(21, 7, 4)
	if u.TotalTokens != 25 {
		t.Fatalf("total_tokens = %d, want 25", u.TotalTokens)
	}
	var m map[string]any
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal usage: %v", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	if m["input_tokens"] != float64(21) || m["output_tokens"] != float64(4) {
		t.Fatalf("usage = %v", m)
	}
}

func TestResponsesUsageFromDelta(t *testing.T) {
	zero := 0
	three := 3
	cases := []struct {
		label          string
		in             DeltaUsage
		input, cached  int
		output, totals int
	}{
		{
			label: "无缓存写入", in: DeltaUsage{InputTokens: 10, CacheReadInputTokens: 5, OutputTokens: 3},
			input: 15, cached: 5, output: 3, totals: 18,
		},
		{
			label: "缓存写入为零不计入", in: DeltaUsage{InputTokens: 10, CacheReadInputTokens: 5, OutputTokens: 3, CacheCreationInputTokens: &zero},
			input: 15, cached: 5, output: 3, totals: 18,
		},
		{
			label: "缓存写入为正计入", in: DeltaUsage{InputTokens: 10, CacheReadInputTokens: 5, OutputTokens: 3, CacheCreationInputTokens: &three},
			input: 18, cached: 5, output: 3, totals: 21,
		},
		{
			label: "全零", in: DeltaUsage{},
			input: 0, cached: 0, output: 0, totals: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			u := ResponsesUsageFromDelta(tc.in)
			if u.InputTokens != tc.input || u.InputTokensDetails.CachedTokens != tc.cached ||
				u.OutputTokens != tc.output || u.TotalTokens != tc.totals {
				t.Fatalf("usage = %+v, want input=%d cached=%d output=%d total=%d",
					u, tc.input, tc.cached, tc.output, tc.totals)
			}
		})
	}
}

// ---------- Response 对象 ----------

func TestResponsesResponseWire_RequiredTopLevelFields(t *testing.T) {
	m := jsonMap(t, sampleResponse())
	requireKeys(t, m, "id", "object", "created_at", "status", "output", "usage")
	if m["object"] != "response" {
		t.Fatalf("object = %v, want response", m["object"])
	}
	if m["created_at"] != float64(1758000000) {
		t.Fatalf("created_at = %v", m["created_at"])
	}
	if got := childList(t, m, "output"); len(got) != 1 {
		t.Fatalf("output 长度 = %d, want 1", len(got))
	}

	empty := jsonMap(t, ResponsesResponse{ID: "resp_x", CreatedAt: 1, Status: ResponsesStatusInProgress})
	if !reflect.DeepEqual(empty["output"], []any{}) {
		t.Fatalf("output = %#v, want 空数组（不能是 null）", empty["output"])
	}
	if !reflect.DeepEqual(empty["tools"], []any{}) {
		t.Fatalf("tools = %#v, want 空数组", empty["tools"])
	}
}

func TestResponsesResponseWire_EchoFields(t *testing.T) {
	strict := true
	temp := 0.7
	topP := 0.9
	maxOut := 4096
	resp := ResponsesResponse{
		ID:                "resp_1",
		Model:             "claude-sonnet-4-6",
		CreatedAt:         1758000000,
		Status:            ResponsesStatusCompleted,
		Instructions:      "be terse",
		Tools:             []ResponsesTool{{Type: ResponsesToolFunction, Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`), Strict: &strict}},
		ToolChoice:        json.RawMessage(`{"type":"function","name":"get_weather"}`),
		Temperature:       &temp,
		TopP:              &topP,
		MaxOutputTokens:   &maxOut,
		ParallelToolCalls: true,
	}
	m := jsonMap(t, resp)
	requireKeys(t, m, "instructions", "tools", "tool_choice", "temperature", "top_p",
		"max_output_tokens", "parallel_tool_calls", "previous_response_id", "store")
	if m["instructions"] != "be terse" {
		t.Fatalf("instructions = %v", m["instructions"])
	}
	// 无服务端状态：两个字段恒为常量，不允许出现别的取值。
	if v, ok := m["previous_response_id"]; !ok || v != nil {
		t.Fatalf("previous_response_id = %v, want null", v)
	}
	if m["store"] != false {
		t.Fatalf("store = %v, want false", m["store"])
	}
	tools := childList(t, m, "tools")
	if len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "get_weather" || tool["strict"] != true {
		t.Fatalf("tool 回显形状 = %v（必须是 Responses 扁平结构）", tool)
	}
	if _, ok := tool["input_schema"]; ok {
		t.Fatalf("tool 回显不得出现 Anthropic 形状的 input_schema：%v", tool)
	}
	wantChoice := map[string]any{"type": "function", "name": "get_weather"}
	if !reflect.DeepEqual(m["tool_choice"], wantChoice) {
		t.Fatalf("tool_choice = %#v, want %#v", m["tool_choice"], wantChoice)
	}

	// instructions 为空则不出键；缺省回显值按 OpenAI 默认。
	bare := jsonMap(t, ResponsesResponse{ID: "resp_2", CreatedAt: 1, Status: ResponsesStatusInProgress})
	requireNoKeys(t, bare, "instructions", "incomplete_details", "error")
	if bare["tool_choice"] != "auto" {
		t.Fatalf("缺省 tool_choice = %v, want auto", bare["tool_choice"])
	}
	requireKeys(t, bare, "temperature", "top_p", "max_output_tokens")
	if bare["temperature"] != nil || bare["top_p"] != nil || bare["max_output_tokens"] != nil {
		t.Fatalf("未设置的可选回显字段必须是 null：%v", bare)
	}

	// 非法 tool_choice 原文不能进线上（RawMessage 会产出坏 JSON）。
	bad := jsonMap(t, ResponsesResponse{ToolChoice: json.RawMessage(`{`)})
	if bad["tool_choice"] != "auto" {
		t.Fatalf("非法 tool_choice = %v, want auto", bad["tool_choice"])
	}
}

func TestResponsesResponseWire_NestedItemStrictness(t *testing.T) {
	resp := ResponsesResponse{
		ID: "resp_1", CreatedAt: 1, Status: ResponsesStatusCompleted,
		Output: []ResponsesOutputItem{
			NewResponsesReasoningItem("sig-1", ResponsesSummaryPart{Text: "想了想"}),
			NewResponsesMessageItem("msg_1", ResponsesStatusCompleted, ResponsesOutputTextPart{Text: "答案"}),
			NewResponsesFunctionCallItem("fc_1", "call_1", "get_weather", "", "", ResponsesStatusCompleted),
		},
		Usage: NewResponsesUsage(1, 0, 2),
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	items := childList(t, m, "output")
	if len(items) != 3 {
		t.Fatalf("output 长度 = %d, want 3", len(items))
	}
	reasoning, _ := items[0].(map[string]any)
	requireNoKeys(t, reasoning, "id", "status", "content")
	requireKeys(t, reasoning, "summary", "encrypted_content")
	message, _ := items[1].(map[string]any)
	requireKeys(t, message, "id", "role", "status", "content")
	call, _ := items[2].(map[string]any)
	requireKeys(t, call, "id", "call_id", "name", "arguments", "status")
	if call["arguments"] != "" {
		t.Fatalf("arguments = %v", call["arguments"])
	}
}

func TestResponsesMarshalJSON_MatchesWire(t *testing.T) {
	resp := sampleResponse()
	direct, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	fromWire, err := json.Marshal(resp.Wire())
	if err != nil {
		t.Fatalf("marshal wire: %v", err)
	}
	if string(direct) != string(fromWire) {
		t.Fatalf("MarshalJSON 与 Wire 不一致:\n%s\n%s", direct, fromWire)
	}

	item := NewResponsesFunctionCallItem("fc_1", "call_1", "f", "", "", ResponsesStatusInProgress)
	itemDirect, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal item: %v", err)
	}
	itemWireJSON, err := json.Marshal(item.wire())
	if err != nil {
		t.Fatalf("marshal item wire: %v", err)
	}
	if string(itemDirect) != string(itemWireJSON) {
		t.Fatalf("item MarshalJSON 与 wire 不一致:\n%s\n%s", itemDirect, itemWireJSON)
	}
}

// ---------- 终态事件 ----------

func TestResponsesTerminalEvents(t *testing.T) {
	t.Run("completed 补全 status", func(t *testing.T) {
		m := frameData(t, NewResponsesCompleted(9, ResponsesResponse{ID: "resp_1", CreatedAt: 1}))
		resp := childMap(t, m, "response")
		if resp["status"] != ResponsesStatusCompleted {
			t.Fatalf("status = %v", resp["status"])
		}
		requireKeys(t, resp, "id", "object", "created_at", "output", "usage")
		requireNoKeys(t, resp, "incomplete_details", "error")
	})

	t.Run("incomplete 恒带 incomplete_details", func(t *testing.T) {
		m := frameData(t, NewResponsesIncomplete(9, ResponsesResponse{ID: "resp_1", CreatedAt: 1}, "max_output_tokens"))
		resp := childMap(t, m, "response")
		if resp["status"] != ResponsesStatusIncomplete {
			t.Fatalf("status = %v", resp["status"])
		}
		details := childMap(t, resp, "incomplete_details")
		if details["reason"] != "max_output_tokens" {
			t.Fatalf("reason = %v", details["reason"])
		}
		requireKeys(t, resp, "output", "usage")
	})

	t.Run("failed 带 error 与已聚合 output", func(t *testing.T) {
		resp := sampleResponse()
		resp.Output = append(resp.Output, NewResponsesFunctionCallItem("fc_1", "call_1", "f", "", "{}", ResponsesStatusCompleted))
		m := frameData(t, NewResponsesFailed(9, resp, "server_error", "boom"))
		body := childMap(t, m, "response")
		if body["status"] != ResponsesStatusFailed {
			t.Fatalf("status = %v", body["status"])
		}
		errBody := childMap(t, body, "error")
		if errBody["code"] != "server_error" || errBody["message"] != "boom" {
			t.Fatalf("error = %v", errBody)
		}
		if got := childList(t, body, "output"); len(got) != 2 {
			t.Fatalf("output 长度 = %d, want 2", len(got))
		}
	})
}

func TestResponsesCreatedAndInProgressCarryResponse(t *testing.T) {
	resp := sampleResponse()
	for _, ev := range []StreamEvent{NewResponsesCreated(0, resp), NewResponsesInProgress(1, resp)} {
		m := frameData(t, ev)
		body := childMap(t, m, "response")
		requireKeys(t, body, "id", "object", "created_at", "status", "output", "usage")
	}
}

// ---------- 入站解析 ----------

func TestResponsesRequest_ParseLooseUnion(t *testing.T) {
	raw := `{
		"model": "claude-sonnet-4-6",
		"instructions": "be terse",
		"max_output_tokens": 4096,
		"stream": true,
		"temperature": 0.5,
		"top_p": 0.9,
		"store": false,
		"previous_response_id": "resp_prev",
		"reasoning": {"effort": "xhigh", "summary": "auto"},
		"tool_choice": {"type": "function", "name": "get_weather"},
		"tools": [
			{"type": "function", "name": "get_weather", "description": "天气", "parameters": {"type": "object"}},
			"exec",
			{"type": "custom", "name": "apply_patch", "format": {"syntax": "lark", "definition": "start: /.+/"}},
			{"type": "namespace", "name": "fs", "tools": [{"type": "function", "name": "read"}]},
			{"type": "tool_search"}
		],
		"input": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [{"type": "output_text", "text": "hello"}]},
			{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\":\"SF\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "sunny"},
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "想了"}]},
			{"type": "additional_tools", "tools": [{"type": "function", "name": "extra"}]}
		]
	}`
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("parse request: %v", err)
	}
	if req.Model != "claude-sonnet-4-6" || req.Instructions != "be terse" || req.MaxOutputTokens != 4096 || !req.Stream {
		t.Fatalf("顶层字段解析异常：%+v", req)
	}
	if req.Temperature == nil || *req.Temperature != 0.5 || req.TopP == nil || *req.TopP != 0.9 {
		t.Fatalf("采样参数解析异常：%+v", req)
	}
	if req.Store == nil || *req.Store {
		t.Fatalf("store 解析异常：%+v", req.Store)
	}
	if req.PreviousResponseID != "resp_prev" {
		t.Fatalf("previous_response_id = %q", req.PreviousResponseID)
	}
	if req.Reasoning == nil || req.Reasoning.Effort != "xhigh" {
		t.Fatalf("reasoning 解析异常：%+v", req.Reasoning)
	}
	if string(req.ToolChoice) != `{"type": "function", "name": "get_weather"}` {
		t.Fatalf("tool_choice 必须保真：%s", req.ToolChoice)
	}

	if len(req.Tools) != 5 {
		t.Fatalf("tools 长度 = %d, want 5", len(req.Tools))
	}
	if req.Tools[1].Type != ResponsesToolCustom || req.Tools[1].Name != "exec" {
		t.Fatalf("字符串简写未按 custom 处理：%+v", req.Tools[1])
	}
	if req.Tools[2].Format == nil || req.Tools[2].Format.Syntax != "lark" {
		t.Fatalf("custom format 解析异常：%+v", req.Tools[2])
	}
	if len(req.Tools[3].Tools) != 1 || req.Tools[3].Tools[0].Name != "read" {
		t.Fatalf("namespace 子工具解析异常：%+v", req.Tools[3])
	}
	if req.Tools[4].Type != ResponsesToolToolSearch {
		t.Fatalf("tool_search 解析异常：%+v", req.Tools[4])
	}

	var items []ResponsesInputItem
	if err := json.Unmarshal(req.Input, &items); err != nil {
		t.Fatalf("parse input: %v", err)
	}
	if len(items) != 6 {
		t.Fatalf("input 长度 = %d, want 6", len(items))
	}
	if items[0].Role != "user" || string(items[0].Content) != `"hi"` {
		t.Fatalf("字符串 content 保真失败：%+v", items[0])
	}
	var parts []ResponsesContentPart
	if err := json.Unmarshal(items[1].Content, &parts); err != nil {
		t.Fatalf("parse parts: %v", err)
	}
	if len(parts) != 1 || parts[0].Type != "output_text" || parts[0].Text != "hello" {
		t.Fatalf("content parts 解析异常：%+v", parts)
	}
	if items[2].Type != "function_call" || items[2].CallID != "call_1" || items[2].Arguments != `{"city":"SF"}` {
		t.Fatalf("function_call 解析异常：%+v", items[2])
	}
	if items[3].Type != "function_call_output" || string(items[3].Output) != `"sunny"` {
		t.Fatalf("function_call_output 解析异常：%+v", items[3])
	}
	if len(items[4].Summary) != 1 || items[4].Summary[0].Text != "想了" {
		t.Fatalf("reasoning 解析异常：%+v", items[4])
	}
	if items[5].Type != "additional_tools" || len(items[5].Tools) == 0 {
		t.Fatalf("additional_tools 解析异常：%+v", items[5])
	}

	// 纯字符串 input 是合法形态（不许像别家实现那样静默丢弃）。
	var asString ResponsesRequest
	if err := json.Unmarshal([]byte(`{"input":"just a string"}`), &asString); err != nil {
		t.Fatalf("parse string input: %v", err)
	}
	if string(asString.Input) != `"just a string"` {
		t.Fatalf("input = %s", asString.Input)
	}
}

func TestResponsesToolUnmarshal_InvalidJSONStillFails(t *testing.T) {
	// 这两个错误分支只能直调：json.Unmarshal 会先做全量合法性预扫描，
	// 非法报文根本到不了 UnmarshalJSON，必须确认它自己也不会静默吞掉坏数据。
	var tool ResponsesTool
	if err := tool.UnmarshalJSON([]byte(`"abc`)); err == nil {
		t.Fatal("截断的字符串简写必须报错")
	}
	if err := tool.UnmarshalJSON([]byte(`{`)); err == nil {
		t.Fatal("非法对象必须报错")
	}
	if err := json.Unmarshal([]byte(`{`), &tool); err == nil {
		t.Fatal("非法 JSON 必须报错")
	}
}

func TestResponsesIgnoredFields(t *testing.T) {
	cases := []struct {
		label string
		raw   string
		want  []string
	}{
		{"无", `{"model":"m","input":"hi"}`, nil},
		{"单个", `{"metadata":{}}`, []string{"metadata"}},
		{
			"多个按声明顺序",
			`{"stream_options":{"include_usage":true},"text":{"format":"x"},"service_tier":"auto","prompt_cache_key":"k","user":"u"}`,
			[]string{"service_tier", "prompt_cache_key", "user", "text", "stream_options"},
		},
		{"空报文", ``, nil},
		{"非法报文", `{`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			got := ResponsesIgnoredFields([]byte(tc.raw))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ResponsesIgnoredFields = %v, want %v", got, tc.want)
			}
		})
	}
}
