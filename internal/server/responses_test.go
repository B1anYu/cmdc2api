package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/B1anYu/cmdc2api/internal/config"
	"github.com/B1anYu/cmdc2api/internal/translate"
	"github.com/B1anYu/cmdc2api/internal/types"
)

// ===========================================================================
// /v1/responses 集成测试：httptest 模拟 cmdc NDJSON 上游，打完整的 handler，
// 断言出站 SSE / JSON 与上游信封。场景与 chat_test.go 逐项对齐，另加 Responses 特有项
// （事件名序列、sequence_number 单调、终态事件的 output/usage、无 [DONE]、回显字段、
// reasoning item 无 id、工具族 item 形态还原）。
// ===========================================================================

// ---------- 请求构造与上游 harness ----------

// responsesAuth 测试用客户端鉴权头（getAPIKey 只做提取，不限前缀的 cmdc key）。
var responsesAuth = map[string]string{"x-api-key": "user_test123"}

// responsesTestModel 用回退列表里的模型名，保证 /v1/models 与 responses 两条路径可用同一名字。
const responsesTestModel = "claude-sonnet-4-6"

// responsesBody 组一个最小可用的 Responses 请求体；extra 为追加的顶层字段（需自带结尾逗号）。
func responsesBody(stream bool, extra string) string {
	return fmt.Sprintf(`{"model":%q,"stream":%t,%s"input":"weather in SF?"}`,
		responsesTestModel, stream, extra)
}

// 工具声明片段（Responses 是扁平结构：parameters 直接挂在工具对象上）。
const (
	weatherToolJSON   = `{"type":"function","name":"get_weather","description":"get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}`
	customToolJSON    = `{"type":"custom","name":"apply_patch","description":"apply a patch","format":{"type":"grammar","syntax":"lark","definition":"start: /./"}}`
	namespaceToolJSON = `{"type":"namespace","name":"git","tools":[{"name":"status","description":"git status","parameters":{"type":"object","properties":{"short":{"type":"boolean"}}}}]}`
	toolSearchJSON    = `{"type":"tool_search"}`
)

// postResponses 打 /v1/responses。
func postResponses(t *testing.T, proxy *httptest.Server, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// responsesProxy 起一个连着假上游（fakeCC）的代理，脚本由 script 指定。
// mutate 用于按用例改配置（如打开思考回传），与 newProxy 同构。
func responsesProxy(t *testing.T, script []string, mutate func(*config.Config)) (*fakeCC, *httptest.Server) {
	t.Helper()
	f := newFakeCC(t)
	f.mu.Lock()
	f.script = script
	f.mu.Unlock()
	return f, newProxy(t, f, mutate)
}

// ---------- SSE 解析与断言工具 ----------

// respFrame 一帧 Responses SSE：事件名 + data 载荷。
// Responses 的帧恒为 "event: <name>\ndata: <json>\n\n" 两行，没有裸 data 帧，也没有 [DONE]。
type respFrame struct {
	name string
	data map[string]any
}

// respFrames 解析 Responses SSE 流，顺带钉住帧形状与 data.type 的一致性。
func respFrames(t *testing.T, sse string) []respFrame {
	t.Helper()
	var out []respFrame
	for _, chunk := range strings.Split(sse, "\n\n") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		lines := strings.Split(chunk, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("SSE 帧形状不对（应为 event/data 两行）: %q", chunk)
		}
		name := strings.TrimPrefix(lines[0], "event: ")
		var data map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &data); err != nil {
			t.Fatalf("帧 data 不是合法 JSON (%v): %q", err, lines[1])
		}
		if got, _ := data["type"].(string); got != name {
			t.Fatalf("帧 data.type = %q, 与事件名 %q 不一致", got, name)
		}
		out = append(out, respFrame{name: name, data: data})
	}
	if len(out) == 0 {
		t.Fatalf("流里没有任何 SSE 帧:\n%s", sse)
	}
	return out
}

// respEvents 抽出帧名序列。
func respEvents(frames []respFrame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.name)
	}
	return out
}

// respItemKey added/done 的配对键：item id（reasoning 无 id，退化为类型）。
func respItemKey(t *testing.T, item map[string]any) string {
	t.Helper()
	if id, ok := item["id"].(string); ok && id != "" {
		return id
	}
	return mStr(t, item, "type")
}

// assertResponsesStreamInvariants 校验 Responses 流式形状的硬约束（Codex 是严格客户端）：
// 首事件必须是 response.created → response.in_progress；sequence_number 从 0 起逐帧 +1；
// output_item.added/done 严格配对且 output_index 连续分配；流尾是终态事件。
func assertResponsesStreamInvariants(t *testing.T, frames []respFrame) {
	t.Helper()
	if frames[0].name != types.ResponsesEventCreated {
		t.Fatalf("首事件 = %s, want response.created", frames[0].name)
	}
	if len(frames) < 2 || frames[1].name != types.ResponsesEventInProgress {
		t.Fatalf("第二个事件必须是 response.in_progress: %v", respEvents(frames))
	}

	open := map[int]string{}
	nextIndex := 0
	for i, f := range frames {
		if got := mNum(t, f.data, "sequence_number"); int(got) != i {
			t.Fatalf("第 %d 帧 (%s) 的 sequence_number = %v, want %d（必须从 0 单调递增）", i, f.name, got, i)
		}
		switch f.name {
		case types.ResponsesEventOutputItemAdded:
			idx := int(mNum(t, f.data, "output_index"))
			if idx != nextIndex {
				t.Fatalf("output_index = %d, want %d（必须连续分配）", idx, nextIndex)
			}
			nextIndex++
			open[idx] = respItemKey(t, mMap(t, f.data, "item"))
		case types.ResponsesEventOutputItemDone:
			idx := int(mNum(t, f.data, "output_index"))
			key, ok := open[idx]
			if !ok {
				t.Fatalf("output_item.done 没有可配对的 added: %v", f.data)
			}
			if got := respItemKey(t, mMap(t, f.data, "item")); got != key {
				t.Fatalf("output_item.done %s 与 added %s 不配对", got, key)
			}
			delete(open, idx)
		}
	}
	if len(open) != 0 {
		t.Fatalf("有 item 只 added 未 done: %v", open)
	}

	switch frames[len(frames)-1].name {
	case types.ResponsesEventCompleted, types.ResponsesEventIncomplete, types.ResponsesEventFailed:
	default:
		t.Fatalf("流尾必须是终态事件，得到 %s", frames[len(frames)-1].name)
	}
}

// respTerminalResponse 取最后一个终态事件携带的 response 对象。
func respTerminalResponse(t *testing.T, frames []respFrame) map[string]any {
	t.Helper()
	return mMap(t, frames[len(frames)-1].data, "response")
}

// respOutputItems 断言 output 数组长度并展开为对象列表。
// 终态事件的 response 必须带完整 output：Codex 的 get_final_response() 直接解析它。
func respOutputItems(t *testing.T, resp map[string]any, want int) []map[string]any {
	t.Helper()
	raw := mList(t, resp, "output")
	if len(raw) != want {
		t.Fatalf("output = %d 项, want %d: %v", len(raw), want, raw)
	}
	out := make([]map[string]any, 0, len(raw))
	for i := range raw {
		item, ok := raw[i].(map[string]any)
		if !ok {
			t.Fatalf("output[%d] 不是对象: %v", i, raw[i])
		}
		out = append(out, item)
	}
	return out
}

// respContentText 取 message item 首个 output_text part 的全文。
func respContentText(t *testing.T, item map[string]any) string {
	t.Helper()
	parts := mList(t, item, "content")
	if len(parts) == 0 {
		t.Fatalf("message item 的 content 为空: %v", item)
	}
	part, ok := parts[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] 不是对象: %v", parts[0])
	}
	return mStr(t, part, "text")
}

// assertResponsesUsage 断言 Responses 口径 usage：总输入含缓存（加法回填）、详细信息单列缓存量、
// total 为两者之和，字段恒存在（含 0）。
func assertResponsesUsage(t *testing.T, resp map[string]any, input, cached, output float64) {
	t.Helper()
	usage := mMap(t, resp, "usage")
	if got := mNum(t, usage, "input_tokens"); got != input {
		t.Errorf("input_tokens = %v, want %v", got, input)
	}
	if got := mNum(t, usage, "output_tokens"); got != output {
		t.Errorf("output_tokens = %v, want %v", got, output)
	}
	if got := mNum(t, usage, "total_tokens"); got != input+output {
		t.Errorf("total_tokens = %v, want %v", got, input+output)
	}
	details := mMap(t, usage, "input_tokens_details")
	if got := mNum(t, details, "cached_tokens"); got != cached {
		t.Errorf("input_tokens_details.cached_tokens = %v, want %v", got, cached)
	}
}

// assertResponsesResponseMeta 断言响应对象的公共顶层字段（流式终态与非流式响应体共用形状）。
func assertResponsesResponseMeta(t *testing.T, body map[string]any, status string) {
	t.Helper()
	if got := mStr(t, body, "object"); got != "response" {
		t.Errorf("object = %q, want response", got)
	}
	if !strings.HasPrefix(mStr(t, body, "id"), "resp_") {
		t.Errorf("id = %q, want resp_ 前缀", mStr(t, body, "id"))
	}
	if got := mStr(t, body, "status"); got != status {
		t.Errorf("status = %q, want %q", got, status)
	}
	if got := mNum(t, body, "created_at"); got <= 0 {
		t.Errorf("created_at = %v, want unix 秒", got)
	}
	// 无服务端状态：这两个键恒存在且为 null/false（Codex 会读）
	if v := mGet(t, body, "previous_response_id"); v != nil {
		t.Errorf("previous_response_id = %v, want null", v)
	}
	if store, ok := mGet(t, body, "store").(bool); !ok || store {
		t.Errorf("store = %v, want false", mGet(t, body, "store"))
	}
	// tool_choice / tools 是回显字段，缺省也要给默认值而不是缺键
	mGet(t, body, "tool_choice")
	mList(t, body, "tools")
}

// ---------- 场景表：流式与非流式共用同一批上游事件 ----------

// responsesScenario 一组假上游 NDJSON 脚本 + 出站断言。
type responsesScenario struct {
	name string
	body func(stream bool) string
	// 上游 NDJSON 脚本
	lines []string
	// 流式：完整事件名序列（钉住映射与事件节奏）
	streamEvents []string
	streamAbsent []string
	checkStream  func(t *testing.T, frames []respFrame)
	// 非流式
	nonStreamStatus int // 0 表示 200
	checkNonStream  func(t *testing.T, body map[string]any)
}

func responsesScenarios() []responsesScenario {
	plain := func(stream bool) string { return responsesBody(stream, "") }
	withTools := func(tools string) func(bool) string {
		return func(stream bool) string { return responsesBody(stream, `"tools":[`+tools+`],`) }
	}

	return []responsesScenario{
		{
			name: "text",
			body: plain,
			lines: []string{
				`{"type":"start"}`,
				`{"type":"text-delta","text":"Hello"}`,
				`{"type":"text-delta","text":" world"}`,
				`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":100,"outputTokens":7,"cachedInputTokens":10}}`,
			},
			streamEvents: []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added",
				"response.content_part.added",
				"response.output_text.delta",
				"response.output_text.delta",
				"response.output_text.done",
				"response.content_part.done",
				"response.output_item.done",
				"response.completed",
			},
			streamAbsent: []string{"reasoning", "encrypted_content", "function_call", "custom_tool_call", "tool_search_call"},
			checkStream: func(t *testing.T, frames []respFrame) {
				resp := respTerminalResponse(t, frames)
				assertResponsesResponseMeta(t, resp, types.ResponsesStatusCompleted)
				if got := mStr(t, resp, "model"); got != responsesTestModel {
					t.Errorf("model = %q", got)
				}
				items := respOutputItems(t, resp, 1)
				if got := mStr(t, items[0], "type"); got != types.ResponsesItemMessage {
					t.Fatalf("output[0].type = %q", got)
				}
				if got := mStr(t, items[0], "role"); got != "assistant" {
					t.Errorf("output[0].role = %q, want assistant", got)
				}
				// delta 只带增量，done 带全文
				if frames[4].data["delta"] != "Hello" || frames[5].data["delta"] != " world" {
					t.Errorf("delta 不是增量: %v / %v", frames[4].data["delta"], frames[5].data["delta"])
				}
				if frames[6].data["text"] != "Hello world" {
					t.Errorf("output_text.done 不是全文: %v", frames[6].data["text"])
				}
				if frames[7].data["part"] == nil {
					t.Errorf("content_part.done 必须带 part: %v", frames[7].data)
				}
				// 加法回填：内部非缓存 90 + 缓存读取 10 = 含缓存总量 100
				assertResponsesUsage(t, resp, 100, 10, 7)
			},
			checkNonStream: func(t *testing.T, body map[string]any) {
				assertResponsesResponseMeta(t, body, types.ResponsesStatusCompleted)
				items := respOutputItems(t, body, 1)
				if got := respContentText(t, items[0]); got != "Hello world" {
					t.Errorf("content text = %q", got)
				}
				assertResponsesUsage(t, body, 100, 10, 7)
			},
		},
		{
			name: "thinking_and_text",
			body: plain,
			lines: []string{
				`{"type":"start"}`,
				`{"type":"reasoning-delta","text":"weighing"}`,
				`{"type":"text-delta","text":"done"}`,
				`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":5}}`,
			},
			streamEvents: []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added", // reasoning idx 0
				"response.reasoning_summary_part.added",
				"response.reasoning_summary_text.delta",
				"response.reasoning_summary_text.done",
				"response.reasoning_summary_part.done",
				"response.output_item.done",
				"response.output_item.added", // message idx 1
				"response.content_part.added",
				"response.output_text.delta",
				"response.output_text.done",
				"response.content_part.done",
				"response.output_item.done",
				"response.completed",
			},
			streamAbsent: []string{"function_call"},
			checkStream: func(t *testing.T, frames []respFrame) {
				resp := respTerminalResponse(t, frames)
				items := respOutputItems(t, resp, 2)

				reasoning := items[0]
				if got := mStr(t, reasoning, "type"); got != types.ResponsesItemReasoning {
					t.Fatalf("output[0].type = %q, want reasoning", got)
				}
				// reasoning item 绝不带 id：OpenAI 未签发，伪造 id 会破坏客户端回放
				if _, ok := reasoning["id"]; ok {
					t.Errorf("reasoning item 不应带 id: %v", reasoning)
				}
				// 思考签名以 encrypted_content 落到 item 上，供客户端跨工具轮次回放
				sig := mStr(t, reasoning, "encrypted_content")
				if sig == "" {
					t.Fatalf("encrypted_content 缺失: %v", reasoning)
				}
				if want := translate.FakeThinkingSignature("weighing"); sig != want {
					t.Errorf("encrypted_content = %q, want %q", sig, want)
				}
				summary := mList(t, reasoning, "summary")
				if len(summary) != 1 {
					t.Fatalf("summary = %v", summary)
				}
				if part := summary[0].(map[string]any); mStr(t, part, "text") != "weighing" ||
					mStr(t, part, "type") != "summary_text" {
					t.Errorf("summary part 不对: %v", part)
				}
				if got := respContentText(t, items[1]); got != "done" {
					t.Errorf("message text = %q", got)
				}
				// reasoning 无 id，其 delta 事件也不得带 item_id
				if _, ok := frames[4].data["item_id"]; ok {
					t.Errorf("reasoning delta 不应带 item_id: %v", frames[4].data)
				}
				if got := mNum(t, frames[4].data, "summary_index"); got != 0 {
					t.Errorf("summary_index = %v, want 0", got)
				}
				assertResponsesUsage(t, resp, 10, 0, 5)
			},
			checkNonStream: func(t *testing.T, body map[string]any) {
				items := respOutputItems(t, body, 2)
				if _, ok := items[0]["id"]; ok {
					t.Error("reasoning item 不应带 id")
				}
				if mStr(t, items[0], "encrypted_content") == "" {
					t.Error("非流式也必须带上思考签名")
				}
				if got := respContentText(t, items[1]); got != "done" {
					t.Errorf("content text = %q", got)
				}
				assertResponsesUsage(t, body, 10, 0, 5)
			},
		},
		{
			name: "tool_call",
			body: withTools(weatherToolJSON),
			lines: []string{
				`{"type":"start"}`,
				`{"type":"text-delta","text":"checking"}`,
				`{"type":"tool-call","toolCallId":"toolu_1","toolName":"get_weather","input":{"city":"SF"}}`,
				`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":20,"outputTokens":9}}`,
			},
			streamEvents: []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added", // message
				"response.content_part.added",
				"response.output_text.delta",
				"response.output_text.done",
				"response.content_part.done",
				"response.output_item.done",
				"response.output_item.added", // function_call
				"response.function_call_arguments.delta",
				"response.function_call_arguments.delta",
				"response.function_call_arguments.done",
				"response.output_item.done",
				"response.completed",
			},
			streamAbsent: []string{"custom_tool_call", "tool_search_call"},
			checkStream: func(t *testing.T, frames []respFrame) {
				resp := respTerminalResponse(t, frames)
				items := respOutputItems(t, resp, 2)
				if got := respContentText(t, items[0]); got != "checking" {
					t.Errorf("message text = %q", got)
				}
				call := items[1]
				if got := mStr(t, call, "type"); got != types.ResponsesItemFunctionCall {
					t.Fatalf("output[1].type = %q", got)
				}
				// item id 与 call_id 都是前缀 + 上游 toolCallId：跨流式/非流式可回放
				if got := mStr(t, call, "id"); got != "fc_toolu_1" {
					t.Errorf("id = %q, want fc_toolu_1", got)
				}
				if got := mStr(t, call, "call_id"); got != "toolu_1" {
					t.Errorf("call_id = %q", got)
				}
				if got := mStr(t, call, "name"); got != "get_weather" {
					t.Errorf("name = %q", got)
				}
				if got := mStr(t, call, "arguments"); got != `{"city":"SF"}` {
					t.Errorf("arguments = %q, want 完整 JSON", got)
				}
				if got := mStr(t, call, "status"); got != types.ResponsesStatusCompleted {
					t.Errorf("status = %q", got)
				}
				// 参数分片：逐片 delta 拼接结果必须等于 done 的全文
				shards := ""
				for _, f := range frames {
					if f.name == types.ResponsesEventFunctionCallArgsDelta {
						shards += mStr(t, f.data, "delta")
					}
				}
				if shards != `{"city":"SF"}` {
					t.Errorf("delta 分片拼接 = %q, want 完整 JSON", shards)
				}
				assertResponsesUsage(t, resp, 20, 0, 9)
			},
			checkNonStream: func(t *testing.T, body map[string]any) {
				items := respOutputItems(t, body, 2)
				call := items[1]
				if got := mStr(t, call, "arguments"); got != `{"city":"SF"}` {
					t.Errorf("arguments = %q", got)
				}
				assertResponsesUsage(t, body, 20, 0, 9)
			},
		},
		{
			name: "custom_tool_call",
			body: withTools(customToolJSON),
			lines: []string{
				`{"type":"start"}`,
				`{"type":"tool-call","toolCallId":"toolu_c1","toolName":"apply_patch","input":{"input":"*** Begin Patch"}}`,
				`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":12,"outputTokens":6}}`,
			},
			streamEvents: []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added", // custom_tool_call
				"response.custom_tool_call_input.delta",
				"response.custom_tool_call_input.delta",
				"response.custom_tool_call_input.done",
				"response.output_item.done",
				"response.completed",
			},
			// custom 工具走专用事件族，不得复用 function_call 的参数增量
			streamAbsent: []string{"function_call_arguments", `"type":"function_call"`},
			checkStream: func(t *testing.T, frames []respFrame) {
				resp := respTerminalResponse(t, frames)
				items := respOutputItems(t, resp, 1)
				call := items[0]
				if got := mStr(t, call, "type"); got != types.ResponsesItemCustomToolCall {
					t.Fatalf("output[0].type = %q", got)
				}
				if got := mStr(t, call, "id"); got != "ctc_toolu_c1" {
					t.Errorf("id = %q, want ctc_ 前缀", got)
				}
				if got := mStr(t, call, "call_id"); got != "toolu_c1" {
					t.Errorf("call_id = %q", got)
				}
				if got := mStr(t, call, "name"); got != "apply_patch" {
					t.Errorf("name = %q", got)
				}
				// 降级 schema 的 {"input":"..."} 必须解包成裸自由文本
				if got := mStr(t, call, "input"); got != "*** Begin Patch" {
					t.Errorf("input = %q, want 解包后的裸文本", got)
				}
				// added 阶段 input 键必须存在（空串，不省略、不补 "{}"）
				added := mMap(t, frames[2].data, "item")
				if v, ok := added["input"]; !ok || v != "" {
					t.Errorf("added item 的 input 必须是空串且键存在: %v", added)
				}
				shards := ""
				for _, f := range frames {
					if f.name == types.ResponsesEventCustomToolCallInputDelta {
						shards += mStr(t, f.data, "delta")
					}
				}
				if shards != "*** Begin Patch" {
					t.Errorf("input 分片拼接 = %q", shards)
				}
				assertResponsesUsage(t, resp, 12, 0, 6)
			},
			checkNonStream: func(t *testing.T, body map[string]any) {
				call := respOutputItems(t, body, 1)[0]
				if got := mStr(t, call, "input"); got != "*** Begin Patch" {
					t.Errorf("input = %q", got)
				}
				assertResponsesUsage(t, body, 12, 0, 6)
			},
		},
		{
			name: "tool_search_call",
			body: withTools(toolSearchJSON),
			lines: []string{
				`{"type":"start"}`,
				`{"type":"tool-call","toolCallId":"toolu_s1","toolName":"tool_search","input":{"query":"git","limit":3}}`,
				`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":30,"outputTokens":4}}`,
			},
			streamEvents: []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added",
				"response.output_item.done", // tool_search 不消费参数增量，done 一次给全
				"response.completed",
			},
			streamAbsent: []string{"function_call_arguments", "custom_tool_call_input"},
			checkStream: func(t *testing.T, frames []respFrame) {
				resp := respTerminalResponse(t, frames)
				call := respOutputItems(t, resp, 1)[0]
				if got := mStr(t, call, "type"); got != types.ResponsesItemToolSearchCall {
					t.Fatalf("output[0].type = %q", got)
				}
				if got := mStr(t, call, "id"); got != "tsc_toolu_s1" {
					t.Errorf("id = %q, want tsc_ 前缀", got)
				}
				// 非 client 时 codex 直接忽略该调用
				if got := mStr(t, call, "execution"); got != "client" {
					t.Errorf("execution = %q, want client", got)
				}
				// arguments 线上是对象而非字符串
				args := mMap(t, call, "arguments")
				if got := mStr(t, args, "query"); got != "git" {
					t.Errorf("arguments.query = %q", got)
				}
				if got := mNum(t, args, "limit"); got != 3 {
					t.Errorf("arguments.limit = %v", got)
				}
				// added 阶段 arguments 也必须是对象（空对象），不能是 null/字符串
				added := mMap(t, frames[2].data, "item")
				if _, ok := added["arguments"].(map[string]any); !ok {
					t.Errorf("added item 的 arguments 必须是对象: %v", added["arguments"])
				}
				assertResponsesUsage(t, resp, 30, 0, 4)
			},
			checkNonStream: func(t *testing.T, body map[string]any) {
				call := respOutputItems(t, body, 1)[0]
				if got := mStr(t, mMap(t, call, "arguments"), "query"); got != "git" {
					t.Errorf("arguments.query = %q", got)
				}
				assertResponsesUsage(t, body, 30, 0, 4)
			},
		},
		{
			name: "namespace_restore",
			body: withTools(namespaceToolJSON),
			lines: []string{
				`{"type":"start"}`,
				`{"type":"tool-call","toolCallId":"toolu_n1","toolName":"git__status","input":{"short":true}}`,
				`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":15,"outputTokens":3}}`,
			},
			streamEvents: []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added",
				"response.function_call_arguments.delta",
				"response.function_call_arguments.delta",
				"response.function_call_arguments.done",
				"response.output_item.done",
				"response.completed",
			},
			// 摊平名只是上游的传输形态，绝不能泄漏给客户端
			streamAbsent: []string{"git__status"},
			checkStream: func(t *testing.T, frames []respFrame) {
				resp := respTerminalResponse(t, frames)
				call := respOutputItems(t, resp, 1)[0]
				if got := mStr(t, call, "name"); got != "status" {
					t.Errorf("name = %q, want 子工具名 status", got)
				}
				// codex 按 namespace + name 路由，缺 namespace 会被判为 unsupported call
				if got := mStr(t, call, "namespace"); got != "git" {
					t.Errorf("namespace = %q, want git", got)
				}
				if got := mStr(t, call, "arguments"); got != `{"short":true}` {
					t.Errorf("arguments = %q", got)
				}
				assertResponsesUsage(t, resp, 15, 0, 3)
			},
			checkNonStream: func(t *testing.T, body map[string]any) {
				call := respOutputItems(t, body, 1)[0]
				if mStr(t, call, "name") != "status" || mStr(t, call, "namespace") != "git" {
					t.Errorf("namespace 还原失败: %v", call)
				}
				assertResponsesUsage(t, body, 15, 0, 3)
			},
		},
		{
			name: "parallel_tool_calls",
			body: withTools(weatherToolJSON),
			lines: []string{
				`{"type":"start"}`,
				`{"type":"tool-call","toolCallId":"toolu_p1","toolName":"get_weather","input":{"city":"SF"}}`,
				`{"type":"tool-call","toolCallId":"toolu_p2","toolName":"get_weather","input":{"city":"NYC"}}`,
				`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":40,"outputTokens":8}}`,
			},
			streamEvents: []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added",
				"response.function_call_arguments.delta",
				"response.function_call_arguments.delta",
				"response.function_call_arguments.done",
				"response.output_item.done",
				"response.output_item.added",
				"response.function_call_arguments.delta",
				"response.function_call_arguments.delta",
				"response.function_call_arguments.done",
				"response.output_item.done",
				"response.completed",
			},
			checkStream: func(t *testing.T, frames []respFrame) {
				resp := respTerminalResponse(t, frames)
				items := respOutputItems(t, resp, 2)
				wantID := []string{"fc_toolu_p1", "fc_toolu_p2"}
				wantArgs := []string{`{"city":"SF"}`, `{"city":"NYC"}`}
				for i := range items {
					if got := mStr(t, items[i], "id"); got != wantID[i] {
						t.Errorf("output[%d].id = %q, want %q", i, got, wantID[i])
					}
					// 并行调用必须各占一个 output_index，顺序与上游给出顺序一致
					if got := mStr(t, items[i], "arguments"); got != wantArgs[i] {
						t.Errorf("output[%d].arguments = %q, want %q", i, got, wantArgs[i])
					}
				}
				assertResponsesUsage(t, resp, 40, 0, 8)
			},
			checkNonStream: func(t *testing.T, body map[string]any) {
				items := respOutputItems(t, body, 2)
				if mStr(t, items[0], "call_id") != "toolu_p1" || mStr(t, items[1], "call_id") != "toolu_p2" {
					t.Errorf("并行调用顺序错乱: %v", items)
				}
				assertResponsesUsage(t, body, 40, 0, 8)
			},
		},
		{
			name: "error_event",
			body: plain,
			lines: []string{
				`{"type":"start"}`,
				`{"type":"text-delta","text":"partial"}`,
				`{"type":"error","error":{"message":"upstream exploded"}}`,
			},
			streamEvents: []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added",
				"response.content_part.added",
				"response.output_text.delta",
				"response.output_text.done",
				"response.content_part.done",
				"response.output_item.done",
				"response.failed",
			},
			streamAbsent: []string{"response.completed", "[DONE]"},
			checkStream: func(t *testing.T, frames []respFrame) {
				resp := respTerminalResponse(t, frames)
				if got := mStr(t, resp, "status"); got != types.ResponsesStatusFailed {
					t.Errorf("status = %q, want failed", got)
				}
				errObj := mMap(t, resp, "error")
				if got := mStr(t, errObj, "code"); got != "api_error" {
					t.Errorf("error.code = %q, want api_error", got)
				}
				if !strings.Contains(mStr(t, errObj, "message"), "upstream exploded") {
					t.Errorf("error.message = %q", mStr(t, errObj, "message"))
				}
				// 失败前的产出必须保留在 output 里，客户端才能还原已拿到的内容
				if got := respContentText(t, respOutputItems(t, resp, 1)[0]); got != "partial" {
					t.Errorf("失败前的 output 丢了: %q", got)
				}
			},
			// 非流式：流内错误在聚合阶段升级为上游错误 → OpenAI 形状 JSON + 502
			nonStreamStatus: http.StatusBadGateway,
			checkNonStream: func(t *testing.T, body map[string]any) {
				assertOpenAIError(t, body, "server_error", "server_error", "upstream exploded")
			},
		},
		{
			name: "max_tokens_incomplete",
			body: plain,
			lines: []string{
				`{"type":"start"}`,
				`{"type":"text-delta","text":"trunc"}`,
				`{"type":"finish","finishReason":"length","totalUsage":{"inputTokens":25,"outputTokens":2}}`,
			},
			streamEvents: []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added",
				"response.content_part.added",
				"response.output_text.delta",
				"response.output_text.done",
				"response.content_part.done",
				"response.output_item.done",
				"response.incomplete",
			},
			streamAbsent: []string{"response.completed"},
			checkStream: func(t *testing.T, frames []respFrame) {
				resp := respTerminalResponse(t, frames)
				if got := mStr(t, resp, "status"); got != types.ResponsesStatusIncomplete {
					t.Fatalf("status = %q, want incomplete", got)
				}
				details := mMap(t, resp, "incomplete_details")
				if got := mStr(t, details, "reason"); got != "max_output_tokens" {
					t.Errorf("incomplete_details.reason = %q", got)
				}
				if got := respContentText(t, respOutputItems(t, resp, 1)[0]); got != "trunc" {
					t.Errorf("被截断的内容必须保留: %q", got)
				}
				assertResponsesUsage(t, resp, 25, 0, 2)
			},
			checkNonStream: func(t *testing.T, body map[string]any) {
				if got := mStr(t, body, "status"); got != types.ResponsesStatusIncomplete {
					t.Errorf("status = %q, want incomplete", got)
				}
				// 非流式必须与流式终态事件同形：incomplete_details.reason 缺席即语义分裂
				// （聚合路径曾整体丢失该字段），故这里严格断言键存在且取值正确。
				details := mMap(t, body, "incomplete_details")
				if got := mStr(t, details, "reason"); got != "max_output_tokens" {
					t.Errorf("incomplete_details.reason = %q, want max_output_tokens", got)
				}
				if _, ok := body["error"]; ok {
					t.Errorf("incomplete 不应带 error: %v", body["error"])
				}
				// 被截断的内容必须保留
				if got := respContentText(t, respOutputItems(t, body, 1)[0]); got != "trunc" {
					t.Errorf("content text = %q", got)
				}
				assertResponsesUsage(t, body, 25, 0, 2)
			},
		},
		{
			name: "usage_missing",
			body: plain,
			lines: []string{
				`{"type":"start"}`,
				`{"type":"text-delta","text":"no usage here"}`,
				`{"type":"finish","finishReason":"stop"}`,
			},
			streamEvents: []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added",
				"response.content_part.added",
				"response.output_text.delta",
				"response.output_text.done",
				"response.content_part.done",
				"response.output_item.done",
				"response.completed",
			},
			checkStream: func(t *testing.T, frames []respFrame) {
				resp := respTerminalResponse(t, frames)
				if got := respContentText(t, respOutputItems(t, resp, 1)[0]); got != "no usage here" {
					t.Errorf("content text = %q", got)
				}
				// 上游漏发 usage：宁可报估算值也不编造计费数字（估算按增量条数）
				assertResponsesUsage(t, resp, 0, 0, 1)
			},
			checkNonStream: func(t *testing.T, body map[string]any) {
				assertResponsesUsage(t, body, 0, 0, 1)
			},
		},
	}
}

// runResponsesStreamScenario 打一次流式请求并校验全部 Responses 流式形状硬约束。
func runResponsesStreamScenario(t *testing.T, sc responsesScenario) {
	t.Helper()
	_, proxy := responsesProxy(t, sc.lines, nil)
	resp := postResponses(t, proxy, sc.body(true), responsesAuth)
	if resp.StatusCode != http.StatusOK {
		body := readAll(t, resp)
		t.Fatalf("status = %d, want 200\nbody:\n%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	sse := readAll(t, resp)

	// Responses 流没有 data: [DONE]，收尾帧就是终态事件
	if strings.Contains(sse, "[DONE]") {
		t.Errorf("Responses 流不得出现 [DONE]:\n%s", sse)
	}
	frames := respFrames(t, sse)
	if got := respEvents(frames); !equalStrings(got, sc.streamEvents) {
		t.Fatalf("事件名序列不符\n got: %v\nwant: %v", got, sc.streamEvents)
	}
	assertResponsesStreamInvariants(t, frames)
	assertAbsent(t, "responses sse", sse, sc.streamAbsent)
	if sc.checkStream != nil {
		sc.checkStream(t, frames)
	}
}

func TestResponses_StreamScenarios(t *testing.T) {
	for _, sc := range responsesScenarios() {
		t.Run(sc.name, func(t *testing.T) { runResponsesStreamScenario(t, sc) })
	}
}

func TestResponses_NonStreamScenarios(t *testing.T) {
	for _, sc := range responsesScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			_, proxy := responsesProxy(t, sc.lines, nil)
			resp := postResponses(t, proxy, sc.body(false), responsesAuth)
			want := sc.nonStreamStatus
			if want == 0 {
				want = http.StatusOK
			}
			if resp.StatusCode != want {
				body := readAll(t, resp)
				t.Fatalf("status = %d, want %d\nbody:\n%s", resp.StatusCode, want, body)
			}
			body := readAll(t, resp)
			if strings.Contains(body, "event: ") {
				t.Errorf("非流式响应不得是 SSE:\n%s", body)
			}
			sc.checkNonStream(t, decodeJSON(t, body))
		})
	}
}

// equalStrings 逐项比较两个字符串切片（避免引入 reflect 依赖的额外噪音）。
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------- 首帧前错误与中途断流 ----------

// TestResponses_ErrorBeforeContent 上游在产出任何内容前报错：
// 未开流（200 头未发）时错误必须回退成标准 JSON 错误响应，客户端才能按状态码退避重试。
func TestResponses_ErrorBeforeContent(t *testing.T) {
	_, proxy := responsesProxy(t, []string{
		`{"type":"start"}`,
		`{"type":"error","error":{"message":"upstream exploded"}}`,
	}, nil)
	resp := postResponses(t, proxy, responsesBody(true, ""), responsesAuth)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	body := readAll(t, resp)
	if strings.Contains(body, "event: ") {
		t.Errorf("未开流时不得发 SSE 帧:\n%s", body)
	}
	assertOpenAIError(t, decodeJSON(t, body), "server_error", "server_error", "upstream exploded")
}

// TestResponses_TruncatedUpstream 上游写完若干行后掐断 TCP（chunked 不写终止块）：
// 流已开启 → 只能以流内 response.failed 收场；非流式 → 502 JSON。
func TestResponses_TruncatedUpstream(t *testing.T) {
	f := newFakeCC(t)
	lines := []string{`{"type":"start"}`, `{"type":"text-delta","text":"half"}`}

	t.Run("stream", func(t *testing.T) {
		proxy := truncatedProxy(t, f, lines)
		resp := postResponses(t, proxy, responsesBody(true, ""), responsesAuth)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200（流已开启）", resp.StatusCode)
		}
		sse := readAll(t, resp)
		frames := respFrames(t, sse)
		last := frames[len(frames)-1]
		if last.name != types.ResponsesEventFailed {
			t.Fatalf("断流必须以 response.failed 收场，得到 %v", respEvents(frames))
		}
		respObj := mMap(t, last.data, "response")
		errObj := mMap(t, respObj, "error")
		if got := mStr(t, errObj, "code"); got != "api_error" {
			t.Errorf("error.code = %q, want api_error", got)
		}
		if !strings.Contains(mStr(t, errObj, "message"), "Upstream error:") {
			t.Errorf("error.message = %q", mStr(t, errObj, "message"))
		}
		assertAbsent(t, "responses sse", sse, []string{"[DONE]", "response.completed"})
		// 断流前已产出的 item 必须补齐生命周期并留在 output 里
		assertResponsesStreamInvariants(t, frames)
		if got := respContentText(t, respOutputItems(t, respObj, 1)[0]); got != "half" {
			t.Errorf("断流前的部分输出丢了: %q", got)
		}
	})

	t.Run("non_stream", func(t *testing.T) {
		proxy := truncatedProxy(t, f, lines)
		resp := postResponses(t, proxy, responsesBody(false, ""), responsesAuth)
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
		assertOpenAIError(t, decodeJSON(t, readAll(t, resp)), "server_error", "server_error", "Upstream error:")
	})
}

// ---------- 入站校验与错误形状 ----------

func TestResponses_RequestValidation(t *testing.T) {
	_, proxy := responsesProxy(t, defaultScript(), nil)

	post := func(t *testing.T, body string, headers map[string]string) (int, map[string]any) {
		t.Helper()
		resp := postResponses(t, proxy, body, headers)
		return resp.StatusCode, decodeJSON(t, readAll(t, resp))
	}

	// 401：缺 key（未鉴权的请求不应触上游）
	if status, body := post(t, responsesBody(false, ""), nil); status != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", status)
	} else {
		assertOpenAIError(t, body, "invalid_request_error", "invalid_api_key", "Missing API key")
	}

	// 400：合法 JSON 但字段类型对不上（入站解析出口，OpenAI 形状）
	status, body := post(t, `{"model":"m","input":"hi","tools":"oops"}`, responsesAuth)
	if status != http.StatusBadRequest {
		t.Errorf("bad shape: status = %d, want 400", status)
	} else {
		assertOpenAIError(t, body, "invalid_request_error", "invalid_request_error", "Invalid request body")
	}

	// 400：previous_response_id（哨兵分支）—— 本代理不保存服务端状态，必须显式拒绝
	status, body = post(t, `{"model":"m","input":"hi","previous_response_id":"resp_abc"}`, responsesAuth)
	if status != http.StatusBadRequest {
		t.Errorf("previous_response_id: status = %d, want 400", status)
	} else {
		assertOpenAIError(t, body, "invalid_request_error", "invalid_request_error", "previous_response_id is not supported")
	}

	// 400：input 形态非法（既不是字符串也不是数组）
	status, body = post(t, `{"model":"m","input":123}`, responsesAuth)
	if status != http.StatusBadRequest {
		t.Errorf("bad input shape: status = %d, want 400", status)
	} else {
		assertOpenAIError(t, body, "invalid_request_error", "invalid_request_error", "input must be a string or an array")
	}

	// 400：空 input 的各种形态（缺失 / 空串 / 空数组 / 无可转换内容）走同一个哨兵。
	// 注意 F25 之后「未知 role」不再等于「被丢弃」——它按 A 级参照降级为一条 user 消息
	// （内容保留），故此处用「未知 role 且无 content」来构造真正的零内容输入。
	for _, tc := range []struct{ name, body string }{
		{"missing", `{"model":"m"}`},
		{"empty_string", `{"model":"m","input":""}`},
		{"empty_array", `{"model":"m","input":[]}`},
		{"unknown_role_without_content", `{"model":"m","input":[{"role":"nobody"}]}`},
	} {
		status, body := post(t, tc.body, responsesAuth)
		if status != http.StatusBadRequest {
			t.Errorf("empty input (%s): status = %d, want 400", tc.name, status)
			continue
		}
		assertOpenAIError(t, body, "invalid_request_error", "invalid_request_error", "no convertible messages")
	}

	// 400：工具重名冲突必须显式拒绝，不静默降级（撞名的工具永远调不到）
	status, body = post(t, `{"model":"m","input":"hi","tools":[`+weatherToolJSON+`,`+weatherToolJSON+`]}`, responsesAuth)
	if status != http.StatusBadRequest {
		t.Errorf("tool conflict: status = %d, want 400", status)
	} else {
		assertOpenAIError(t, body, "invalid_request_error", "invalid_request_error", "declared more than once")
	}

	// 200：最小可用请求（store 是安全丢弃字段，只留痕不影响可用性）
	status, body = post(t, `{"model":"m","input":"hi","store":true}`, responsesAuth)
	if status != http.StatusOK {
		t.Fatalf("minimal: status = %d, want 200\nbody: %v", status, body)
	}
	assertResponsesResponseMeta(t, body, types.ResponsesStatusCompleted)
}

// TestResponses_ReadBodyErrorsUseOpenAIShape 请求体读取阶段的两条错误出口（坏 JSON / 超限）
// 必须与端点协议一致：readBody 曾硬编码 Anthropic 形状，OpenAI 端点会在开流前
// 吐出下游 SDK 解析不了的错误体（T6b 发现）。
func TestResponses_ReadBodyErrorsUseOpenAIShape(t *testing.T) {
	_, proxy := responsesProxy(t, defaultScript(), nil)

	// 400：语法不完整的 JSON（合法 UTF-8，仅不满足 JSON 语法）
	resp := postResponses(t, proxy, `{"model":"m","input":`, responsesAuth)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json: status = %d, want 400", resp.StatusCode)
	}
	assertOpenAIError(t, decodeJSON(t, readAll(t, resp)),
		"invalid_request_error", "invalid_request_error", "Invalid JSON body")

	// 413：请求体超限（用小上限的 Server 实例构造，免得真发 10MB）
	f := newFakeCC(t)
	small := newProxy(t, f, func(c *config.Config) { c.MaxBodyBytes = 32 })
	resp = postResponses(t, small, responsesBody(false, ""), responsesAuth)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status = %d, want 413", resp.StatusCode)
	}
	assertOpenAIError(t, decodeJSON(t, readAll(t, resp)),
		"invalid_request_error", "invalid_request_error", "exceeds")
}

// TestResponses_UpstreamHTTPErrors 上游 HTTP 错误 → OpenAI 形状 + Retry-After 头。
// 错误发生在开流前，流式与非流式都是 JSON 响应。
func TestResponses_UpstreamHTTPErrors(t *testing.T) {
	cases := []struct {
		name       string
		ccStatus   int
		ccBody     string
		wantStatus int
		wantType   string
		wantCode   string
		wantRetry  string
	}{
		{"rate_limited", 429, `{"error":{"message":"slow down please"}}`, 429, "rate_limit_error", "rate_limit_exceeded", "30"},
		{"payment_required", 402, `{"error":{"message":"out of credits"}}`, 429, "rate_limit_error", "rate_limit_exceeded", "30"},
		{"unauthorized", 401, `{"error":{"message":"bad api key"}}`, 401, "invalid_request_error", "invalid_api_key", ""},
		{"not_found", 404, `{"error":{"message":"no such model"}}`, 404, "not_found_error", "not_found_error", ""},
		{"server_error", 500, `{"error":{"message":"kaboom"}}`, 502, "server_error", "server_error", ""},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				f := newFakeCC(t)
				f.mu.Lock()
				f.status, f.errBody = tc.ccStatus, tc.ccBody
				f.mu.Unlock()
				proxy := newProxy(t, f, nil)

				resp := postResponses(t, proxy, responsesBody(stream, ""), responsesAuth)
				if resp.StatusCode != tc.wantStatus {
					t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
				}
				if got := resp.Header.Get("Retry-After"); got != tc.wantRetry {
					t.Errorf("Retry-After = %q, want %q", got, tc.wantRetry)
				}
				body := readAll(t, resp)
				if strings.Contains(body, "event: ") {
					t.Errorf("错误必须是 JSON 响应而非 SSE:\n%s", body)
				}
				assertOpenAIError(t, decodeJSON(t, body), tc.wantType, tc.wantCode, "")
			})
		}
	}
}

// ---------- 零输出（上游正常结束但零 token） ----------

// responsesZeroOutputScript 上游正常结束（发 finish）但 usage 报零输出；
// prefix 为内容事件（留空即「零可见输出」的纯粹形态）。
func responsesZeroOutputScript(prefix ...string) []string {
	lines := []string{`{"type":"start"}`}
	lines = append(lines, prefix...)
	return append(lines,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":50,"outputTokens":0}}`)
}

// TestResponses_ZeroOutput 上游正常结束但零输出时必须转成 429 / response.failed，
// 而不是把一次空响应当成功下发（下游会按成功计费）。Responses 侧此前只有编码器级
// 覆盖（translate/respout_test.go 手工喂事件），整条管线从未执行过：
// usage_missing 场景自带 text、估算 OutputTokens 为 1，不构成零输出。
//
// 零输出判定在管线里有两条出口，内容形态各测一次：
//   - 零可见输出（流未开）→ 200 头都没发出去，回退 JSON 429；
//   - 已有内容（流已开，状态码改不了）→ 以 response.failed 收场并保留已产出的内容。
//
// 断言钉住错误消息里的 "zero output tokens" 与 Retry-After: 10（空闲超时出口是 5，
// 断流出口是 502 + "Upstream error:"），证明走的确实是零输出判定。
func TestResponses_ZeroOutput(t *testing.T) {
	t.Run("no_content/stream", func(t *testing.T) {
		_, proxy := responsesProxy(t, responsesZeroOutputScript(), nil)
		resp := postResponses(t, proxy, responsesBody(true, ""), responsesAuth)
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429（零输出不得当成功下发）\nbody:\n%s", resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("content-type = %q, want application/json（未开流时必须回退 JSON）", ct)
		}
		if strings.Contains(body, "event: ") {
			t.Errorf("零输出不得发出任何 SSE 帧:\n%s", body)
		}
		if ra := resp.Header.Get("Retry-After"); ra != "10" {
			t.Errorf("Retry-After = %q, want 10（零输出出口；空闲超时出口是 5）", ra)
		}
		assertOpenAIError(t, decodeJSON(t, body), "rate_limit_error", "rate_limit_exceeded", "zero output tokens")
	})

	t.Run("no_content/non_stream", func(t *testing.T) {
		_, proxy := responsesProxy(t, responsesZeroOutputScript(), nil)
		resp := postResponses(t, proxy, responsesBody(false, ""), responsesAuth)
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429\nbody:\n%s", resp.StatusCode, body)
		}
		if strings.Contains(body, "event: ") {
			t.Errorf("非流式不得是 SSE:\n%s", body)
		}
		if ra := resp.Header.Get("Retry-After"); ra != "10" {
			t.Errorf("Retry-After = %q, want 10（零输出出口）", ra)
		}
		assertOpenAIError(t, decodeJSON(t, body), "rate_limit_error", "rate_limit_exceeded", "zero output tokens")
	})

	t.Run("content_then_zero_usage/stream", func(t *testing.T) {
		_, proxy := responsesProxy(t, responsesZeroOutputScript(`{"type":"text-delta","text":"half"}`), nil)
		resp := postResponses(t, proxy, responsesBody(true, ""), responsesAuth)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200（内容已开流，状态码改不了）\nbody:\n%s",
				resp.StatusCode, readAll(t, resp))
		}
		sse := readAll(t, resp)
		frames := respFrames(t, sse)
		last := frames[len(frames)-1]
		if last.name != types.ResponsesEventFailed {
			t.Fatalf("零输出流必须以 response.failed 收场，得到 %v", respEvents(frames))
		}
		assertAbsent(t, "responses sse", sse, []string{"[DONE]", "response.completed"})
		assertResponsesStreamInvariants(t, frames)

		respObj := mMap(t, last.data, "response")
		if got := mStr(t, respObj, "status"); got != types.ResponsesStatusFailed {
			t.Errorf("status = %q, want failed", got)
		}
		errObj := mMap(t, respObj, "error")
		if got := mStr(t, errObj, "code"); got != "rate_limit_error" {
			t.Errorf("error.code = %q, want rate_limit_error（不是断流的 api_error）", got)
		}
		if msg := mStr(t, errObj, "message"); !strings.Contains(msg, "zero output tokens") {
			t.Errorf("error.message = %q, want 命中零输出判定", msg)
		}
		// 终态仍要带完整 output：零输出判定发生在收尾，已产出的内容不得丢
		if got := respContentText(t, respOutputItems(t, respObj, 1)[0]); got != "half" {
			t.Errorf("零输出前的部分产出丢了: %q", got)
		}
	})

	t.Run("content_then_zero_usage/non_stream", func(t *testing.T) {
		_, proxy := responsesProxy(t, responsesZeroOutputScript(`{"type":"text-delta","text":"half"}`), nil)
		resp := postResponses(t, proxy, responsesBody(false, ""), responsesAuth)
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429（已有内容也照样是零输出）\nbody:\n%s", resp.StatusCode, body)
		}
		assertOpenAIError(t, decodeJSON(t, body), "rate_limit_error", "rate_limit_exceeded", "zero output tokens")
	})
}

// F9：parallel_tool_calls 的回显语义——必须回显客户端的**真实**声明。
// 该字段的协议默认值是 true，本代理的实际行为也确实是「任由上游并行」，
// 因此旧的硬编码 false 与事实相反：客户端发 true、或干脆没发，都会收回一个 false。
// 修好后：显式 false → 真串行且回显 false；未声明 → 并行且回显 true。
func TestResponses_ParallelToolCallsEcho(t *testing.T) {
	cases := []struct {
		name   string
		fields string // 追加到请求体的顶层字段（含结尾逗号则自带）
		want   bool
	}{
		{"undeclared-echoes-protocol-default", "", true},
		{"explicit-true", `"parallel_tool_calls":true,`, true},
		{"explicit-false", `"parallel_tool_calls":false,`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, proxy := responsesProxy(t, defaultScript(), nil)
			respBody := decodeJSON(t, readAll(t, postResponses(t, proxy, responsesBody(false, tc.fields), responsesAuth)))
			got, ok := mGet(t, respBody, "parallel_tool_calls").(bool)
			if !ok {
				t.Fatalf("parallel_tool_calls 键必须存在（严格客户端做浅校验）: %v", respBody)
			}
			if got != tc.want {
				t.Errorf("echo parallel_tool_calls = %v, want %v", got, tc.want)
			}
		})
	}

	// 流式终态事件的 response 与聚合器共用同一份 echo
	t.Run("stream-terminal-event", func(t *testing.T) {
		_, proxy := responsesProxy(t, defaultScript(), nil)
		frames := respFrames(t, readAll(t, postResponses(t, proxy, responsesBody(true, ""), responsesAuth)))
		respObj := respTerminalResponse(t, frames)
		if got, ok := mGet(t, respObj, "parallel_tool_calls").(bool); !ok || !got {
			t.Errorf("流式终态 parallel_tool_calls = %v, want true（未声明 → 协议默认值）",
				mGet(t, respObj, "parallel_tool_calls"))
		}
	})

	// 显式 false 但本轮没有工具：串行约束无处挂（tool_choice 不存在），必须按行为性丢失留痕。
	// 这是该字段修复后**唯一**仍会留痕的情形——字段本身已被消费，不再是「无上游能力的丢弃项」。
	t.Run("false-without-tools-warns", func(t *testing.T) {
		rec := &levelRecorder{}
		old := slog.Default()
		slog.SetDefault(slog.New(rec))
		t.Cleanup(func() { slog.SetDefault(old) })

		_, proxy := responsesProxy(t, defaultScript(), nil)
		if resp := postResponses(t, proxy, responsesBody(false, `"parallel_tool_calls":false,`), responsesAuth); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200\nbody:\n%s", resp.StatusCode, readAll(t, resp))
		} else {
			_ = resp.Body.Close()
		}
		found := ""
		for _, l := range strings.Split(rec.all(), "\n") {
			if strings.Contains(l, "parallel_tool_calls ignored") {
				found = l
				break
			}
		}
		if found == "" {
			t.Fatalf("显式 false 且无工具时必须留痕\nlogs:\n%s", rec.all())
		}
		if !strings.HasPrefix(found, "WARN") {
			t.Errorf("该留痕是行为性丢失，必须按 WARN 计（旧的 info: 前缀随字段被消费一并消失）: %q", found)
		}
	})
}

// ---------- 归一化落到上游信封 ----------

// TestResponses_UpstreamEnvelope 断言 Responses 请求经归一化后在上游信封里的形状，
// 并顺带钉住响应对象的回显字段（Codex 对响应对象做浅校验）。
func TestResponses_UpstreamEnvelope(t *testing.T) {
	const reqBody = `{
		"model": "m-responses",
		"stream": false,
		"max_output_tokens": 111,
		"temperature": 0.3,
		"top_p": 0.9,
		"instructions": "be terse",
		"reasoning": {"effort": "minimal", "summary": "auto"},
		"tool_choice": "required",
		"parallel_tool_calls": false,
		"truncation": "auto",
		"store": false,
		"tools": [
			{"type": "function", "name": "get_weather", "description": "get weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}},
			{"type": "custom", "name": "apply_patch", "description": "apply a patch", "format": {"type": "grammar", "syntax": "lark", "definition": "start: /./"}},
			{"type": "namespace", "name": "git", "tools": [{"name": "status", "description": "git status", "parameters": {"type": "object", "properties": {"short": {"type": "boolean"}}}}]},
			{"type": "tool_search"},
			{"type": "web_search"}
		],
		"input": [
			{"role": "developer", "content": [{"type": "input_text", "text": "tools are strict"}]},
			{"role": "user", "content": [{"type": "input_text", "text": "weather in SF?"}]},
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "prior thought"}]},
			{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\":\"SF\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "sunny"}
		]
	}`

	// 思考回传默认在测试配置里是关的：显式打开才能验证 reasoning item 落到 thinking 块
	f, proxy := responsesProxy(t, defaultScript(), func(c *config.Config) { c.AssistantReasoning = true })
	respBody := decodeJSON(t, readAll(t, postResponses(t, proxy, reqBody, responsesAuth)))
	// 同一对话第二轮：首条用户消息不变 → 必须复用同一会话
	readAll(t, postResponses(t, proxy, reqBody, responsesAuth))

	// ---- 出站回显：回显的必须是客户端原始声明（Responses 扁平形状） ----
	assertResponsesResponseMeta(t, respBody, types.ResponsesStatusCompleted)
	if got := mStr(t, respBody, "model"); got != "m-responses" {
		t.Errorf("echo model = %q", got)
	}
	if got := mStr(t, respBody, "instructions"); got != "be terse" {
		t.Errorf("echo instructions = %q", got)
	}
	if got := mNum(t, respBody, "temperature"); got != 0.3 {
		t.Errorf("echo temperature = %v", got)
	}
	// top_p 不在回显字段里（请求体里发了 0.9，响应里连键都不该出现）：
	// 它从未被转发给上游，回显等于谎报参数已生效（#14）
	if v, ok := respBody["top_p"]; ok {
		t.Errorf("响应不应回显 top_p（该参数从未到达上游）: %v", v)
	}
	if got := mNum(t, respBody, "max_output_tokens"); got != 111 {
		t.Errorf("echo max_output_tokens = %v", got)
	}
	if got := mStr(t, respBody, "tool_choice"); got != "required" {
		t.Errorf("echo tool_choice = %q, want 原始字符串", got)
	}
	if pc, ok := mGet(t, respBody, "parallel_tool_calls").(bool); !ok || pc {
		t.Errorf("echo parallel_tool_calls = %v, want false", mGet(t, respBody, "parallel_tool_calls"))
	}
	// 工具回显是客户端原始声明（含被丢弃的 web_search），不是归一化后的 Anthropic 形状
	echoTools := mList(t, respBody, "tools")
	if len(echoTools) != 5 {
		t.Fatalf("echo tools = %d 项, want 5（客户端原始声明）", len(echoTools))
	}
	echoTool := echoTools[0].(map[string]any)
	if _, ok := echoTool["parameters"]; !ok {
		t.Errorf("回显工具必须保持 Responses 扁平形状（parameters 在顶层）: %v", echoTool)
	}
	if _, ok := echoTool["input_schema"]; ok {
		t.Errorf("回显工具不得是归一化后的 Anthropic 形状: %v", echoTool)
	}

	// ---- 上游信封 ----
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.genReqs) != 2 {
		t.Fatalf("generate calls = %d, want 2", len(f.genReqs))
	}
	if s1, s2 := f.genReqs[0].headers.Get("x-session-id"), f.genReqs[1].headers.Get("x-session-id"); s1 == "" || s1 != s2 {
		t.Errorf("responses 必须继承前缀会话亲和: %q vs %q", s1, s2)
	}
	checkUpstreamHeaders(t, f.genReqs[0].headers)

	gen := f.genReqs[0]
	params, ok := gen.body["params"].(map[string]any)
	if !ok {
		t.Fatalf("envelope params missing: %v", gen.body)
	}
	if got := params["model"]; got != "m-responses" {
		t.Errorf("params.model = %v", got)
	}
	// system 恒为字符串（传数组会被上游 400）；instructions 在首段，developer item 依序并入
	if sys, ok := params["system"].(string); !ok || sys != "be terse\n\ntools are strict" {
		t.Errorf("params.system = %#v", params["system"])
	}
	if got := params["max_tokens"]; got != float64(111) {
		t.Errorf("params.max_tokens = %v, want 111", got)
	}
	// reasoning.effort: minimal → low（走 adaptive 分支，与 Chat 侧同一张映射表）
	if got := params["reasoning_effort"]; got != "low" {
		t.Errorf("params.reasoning_effort = %v, want low", got)
	}
	if params["stream"] != true {
		t.Error("params.stream 必须为 true（cmdc 恒流式）")
	}
	// parallel_tool_calls=false 不再是被丢弃字段（F9）：它经 tool_choice 侧的同名开关
	// 落到信封顶层的 params.parallel_tool_calls（convertToolChoice 读 disable_parallel_tool_use）。
	if got := params["parallel_tool_calls"]; got != false {
		t.Errorf("params.parallel_tool_calls = %v, want false（客户端显式声明了 false）", got)
	}
	// 安全丢弃字段：上游无对应能力，绝不能下发（留痕走日志）
	for _, key := range []string{"truncation", "store", "include", "text"} {
		if _, ok := params[key]; ok {
			t.Errorf("被丢弃的字段 %q 不得进入上游信封", key)
		}
	}

	// 工具族降级：function 原样、custom 补单 input 参数、namespace 摊平、tool_search 变代理工具
	tools, ok := params["tools"].([]any)
	if !ok || len(tools) != 4 {
		t.Fatalf("params.tools = %#v, want 4（web_search 已被丢弃）", params["tools"])
	}
	byName := map[string]map[string]any{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		byName[mStr(t, tool, "name")] = tool
	}
	if _, ok := byName["get_weather"]; !ok {
		t.Fatalf("get_weather 丢失: %v", byName)
	}
	if _, ok := byName["web_search"]; ok {
		t.Error("web_search 无上游能力，不得出现在工具清单里")
	}
	// custom → 单 input:string 参数的 function，grammar 拼进参数描述
	patch, ok := byName["apply_patch"]
	if !ok {
		t.Fatalf("custom 工具未降级为 function: %v", byName)
	}
	patchSchema := mMap(t, patch, "input_schema")
	props := mMap(t, patchSchema, "properties")
	inputProp := mMap(t, props, "input")
	if got := mStr(t, inputProp, "type"); got != "string" {
		t.Errorf("custom 降级 schema 的 input.type = %q", got)
	}
	if desc := mStr(t, inputProp, "description"); !strings.Contains(desc, "apply a patch") || !strings.Contains(desc, "start: /./") {
		t.Errorf("custom 降级 schema 必须把 grammar 拼进描述: %q", desc)
	}
	if req, ok := patchSchema["required"].([]any); !ok || len(req) != 1 || req[0] != "input" {
		t.Errorf("custom 降级 schema 的 required = %v", patchSchema["required"])
	}
	// namespace → <ns>__<child> 的普通 function
	if _, ok := byName["git__status"]; !ok {
		t.Errorf("namespace 未摊平或摊平名不符: %v", byName)
	}
	// tool_search → 固定的查询代理工具
	search, ok := byName["tool_search"]
	if !ok {
		t.Fatalf("tool_search 代理工具缺失: %v", byName)
	}
	if _, ok := mMap(t, mMap(t, search, "input_schema"), "properties")["query"]; !ok {
		t.Errorf("tool_search 代理工具缺 query 参数: %v", search)
	}
	// tool_choice "required" → any
	if tc := mMap(t, params, "tool_choice"); mStr(t, tc, "type") != "any" {
		t.Errorf("params.tool_choice = %v, want {type:any}", tc)
	}

	// 消息配对：developer 并入 system；reasoning item 回传为 thinking 块；
	// assistant(tool-call) 与 user(tool-result) 必须相邻。
	msgs, ok := params["messages"].([]any)
	if !ok || len(msgs) != 3 {
		t.Fatalf("params.messages = %#v, want 3 (user/assistant/tool)", params["messages"])
	}
	wantRoles := []string{"user", "assistant", "tool"}
	for i, raw := range msgs {
		m := raw.(map[string]any)
		if got := m["role"]; got != wantRoles[i] {
			t.Errorf("messages[%d].role = %v, want %v", i, got, wantRoles[i])
		}
	}
	assistant := msgs[1].(map[string]any)
	parts := assistant["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("assistant content = %#v, want [reasoning, tool-call]", assistant["content"])
	}
	// 思考回传：严格按 CC CLI 抓包的 [reasoning, text, tool-call] 顺序
	if part := parts[0].(map[string]any); part["type"] != "reasoning" || part["text"] != "prior thought" {
		t.Errorf("历史思考块必须回传为 reasoning part: %v", part)
	}
	if part := parts[1].(map[string]any); part["type"] != "tool-call" ||
		part["toolCallId"] != "call_1" || part["toolName"] != "get_weather" {
		t.Errorf("tool-call part 不对: %v", part)
	}
	toolResult := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if toolResult["type"] != "tool-result" || toolResult["toolCallId"] != "call_1" {
		t.Errorf("tool-result part 不对: %v", toolResult)
	}
	if out, ok := toolResult["output"].(map[string]any); !ok || out["value"] != "sunny" {
		t.Errorf("tool-result output 不对: %v", toolResult["output"])
	}
}

// ---------- 入站 → 出站往返 ----------

// TestResponses_InboundToOutboundRoundTrip 用归一化产出的 mapping 驱动出站编码器：
// custom / tool_search / namespace 三类工具在上游只能以普通 function 形态调用，
// 出站必须按 mapping 还原为 Codex 期望的 item 形态，否则回放历史时会因 item 与声明不符而中止。
func TestResponses_InboundToOutboundRoundTrip(t *testing.T) {
	const reqBody = `{
		"model": "m-roundtrip",
		"stream": false,
		"tools": [` + customToolJSON + `,` + toolSearchJSON + `,` + namespaceToolJSON + `],
		"input": "go"
	}`
	var rreq types.ResponsesRequest
	if err := json.Unmarshal([]byte(reqBody), &rreq); err != nil {
		t.Fatal(err)
	}
	areq, mapping, _, err := translate.ResponsesToRequestBody([]byte(reqBody), &rreq)
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	if len(areq.Messages) != 1 {
		t.Fatalf("归一化消息数 = %d, want 1", len(areq.Messages))
	}

	// 摊平名由归一化决定（出站不得自行猜），从 mapping 取回来驱动上游 tool-call
	flat := ""
	for name := range mapping.Namespace {
		flat = name
	}
	if flat == "" || !mapping.Custom["apply_patch"] || !mapping.ToolSearch {
		t.Fatalf("降级映射不完整: %+v", mapping)
	}

	lines := []string{
		`{"type":"start"}`,
		`{"type":"tool-call","toolCallId":"call_c","toolName":"apply_patch","input":{"input":"*** Begin Patch\n*** End Patch"}}`,
		`{"type":"tool-call","toolCallId":"call_s","toolName":"tool_search","input":{"query":"git","limit":3}}`,
		fmt.Sprintf(`{"type":"tool-call","toolCallId":"call_n","toolName":%q,"input":{"short":true}}`, flat),
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":30,"outputTokens":12}}`,
	}

	tr := translate.NewStreamTranslator(areq.Model, "msg_upstream")
	enc := translate.NewResponsesEncoder(areq.Model, "resp_roundtrip", mapping)
	var frames [][]byte
	feed := func(evs []types.StreamEvent) {
		for _, ev := range evs {
			frames = append(frames, enc.Feed(ev)...)
		}
	}
	feed(tr.Start())
	for _, l := range lines {
		feed(tr.Feed([]byte(l)))
	}
	feed(tr.Finish())
	frames = append(frames, enc.Finish()...)

	parsed := respFrames(t, string(bytes.Join(frames, nil)))
	assertResponsesStreamInvariants(t, parsed)
	items := respOutputItems(t, respTerminalResponse(t, parsed), 3)

	// custom：还原为 custom_tool_call，并把降级 schema 的 {"input":"..."} 解包成裸文本
	custom := items[0]
	if got := mStr(t, custom, "type"); got != types.ResponsesItemCustomToolCall {
		t.Errorf("output[0].type = %q, want custom_tool_call", got)
	}
	if got := mStr(t, custom, "id"); got != "ctc_call_c" {
		t.Errorf("output[0].id = %q, want ctc_call_c", got)
	}
	if got := mStr(t, custom, "input"); got != "*** Begin Patch\n*** End Patch" {
		t.Errorf("output[0].input = %q, want 解包后的裸文本", got)
	}

	// tool_search：还原为 tool_search_call，arguments 是对象且 execution 为 client
	search := items[1]
	if got := mStr(t, search, "type"); got != types.ResponsesItemToolSearchCall {
		t.Errorf("output[1].type = %q, want tool_search_call", got)
	}
	if got := mStr(t, search, "id"); got != "tsc_call_s" {
		t.Errorf("output[1].id = %q, want tsc_call_s", got)
	}
	if got := mStr(t, search, "execution"); got != "client" {
		t.Errorf("output[1].execution = %q, want client", got)
	}
	if got := mStr(t, mMap(t, search, "arguments"), "query"); got != "git" {
		t.Errorf("output[1].arguments.query = %q", got)
	}

	// namespace：摊平名还原为 {name: 子工具, namespace: 命名空间}
	ns := items[2]
	if got := mStr(t, ns, "type"); got != types.ResponsesItemFunctionCall {
		t.Errorf("output[2].type = %q, want function_call", got)
	}
	if got := mStr(t, ns, "name"); got != "status" {
		t.Errorf("output[2].name = %q, want status", got)
	}
	if got := mStr(t, ns, "namespace"); got != "git" {
		t.Errorf("output[2].namespace = %q, want git", got)
	}
	if got := mStr(t, ns, "arguments"); got != `{"short":true}` {
		t.Errorf("output[2].arguments = %q", got)
	}
}

// ---------- 安全丢弃字段的留痕 ----------

// levelRecorder 捕获 slog 记录（含级别）。默认 logger 是全局的，用例结束必须还原。
type levelRecorder struct {
	mu      sync.Mutex
	records []string
}

func (r *levelRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *levelRecorder) Handle(_ context.Context, rec slog.Record) error {
	line := rec.Level.String() + " " + rec.Message
	rec.Attrs(func(a slog.Attr) bool {
		line += " " + a.Key + "=" + a.Value.String()
		return true
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, line)
	return nil
}

func (r *levelRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *levelRecorder) WithGroup(string) slog.Handler      { return r }

func (r *levelRecorder) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.records, "\n")
}

// TestResponses_IgnoredFieldsAreLogged 无上游能力的入站字段必须留痕且分级正确（§0.4）：
// 行为性丢失按 WARN 计，良性丢失（上游本就没有该维度的选择权）用 "info: " 前缀降到 INFO。
//
// 这条用例同时钉住 handler 的集成点：留痕依赖**原始报文**做存在性探测，
// 若把 ResponsesToRequestBody 换成不带报文的 ResponsesToRequest（raw=nil），这些日志会整体消失。
func TestResponses_IgnoredFieldsAreLogged(t *testing.T) {
	rec := &levelRecorder{}
	old := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(old) })

	_, proxy := responsesProxy(t, defaultScript(), nil)
	body := responsesBody(false,
		`"truncation":"auto","include":["reasoning.encrypted_content"],`+
			`"text":{"format":{"type":"json_object"}},`)
	if resp := postResponses(t, proxy, body, responsesAuth); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200\nbody:\n%s", resp.StatusCode, readAll(t, resp))
	} else {
		_ = resp.Body.Close()
	}

	logs := rec.all()
	for _, tc := range []struct{ sub, level string }{
		{"text.format/verbosity ignored", "WARN"},
		{"include ignored", "INFO"},
		{"truncation ignored", "INFO"},
	} {
		line := ""
		for _, l := range strings.Split(logs, "\n") {
			if strings.Contains(l, tc.sub) {
				line = l
				break
			}
		}
		if line == "" {
			t.Errorf("安全丢弃字段未留痕: %s\nlogs:\n%s", tc.sub, logs)
			continue
		}
		if !strings.HasPrefix(line, tc.level) {
			t.Errorf("%s 的分级 = %q, want %s", tc.sub, line, tc.level)
		}
	}
}

// ---------- 终态事件的回显与流式形状 ----------

// TestResponses_TopPDroppedSilently top_p 是「所有丢弃必须留痕」规约的**显式例外**
// （用户 2026-09-21 拍板：静默丢弃，不引入 warn）：它从未被转发给上游
// （BuildCcRequest 三条路径共用，只转发 temperature），此前却在响应里被回显，
// 让客户端误以为参数生效了——谎报才是真缺陷。
//
// 本用例刻意与项目默认规约相反，同时钉住两件事：
//  1. 响应体（流式终态事件与非流式响应体）里连 top_p 键都不出现；
//  2. 这次丢弃不产生任何日志（WARN / INFO 都不允许），否则等于偷偷加了留痕。
func TestResponses_TopPDroppedSilently(t *testing.T) {
	rec := &levelRecorder{}
	old := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(old) })

	_, proxy := responsesProxy(t, defaultScript(), nil)

	// 非流式：JSON 响应体
	body := readAll(t, postResponses(t, proxy, responsesBody(false, `"top_p":0.9,`), responsesAuth))
	if v, ok := decodeJSON(t, body)["top_p"]; ok {
		t.Errorf("非流式响应不应回显 top_p（该参数从未到达上游）: %v", v)
	}

	// 流式：终态事件的 response 与非流式共用同一个 Wire()
	sse := readAll(t, postResponses(t, proxy, responsesBody(true, `"top_p":0.9,`), responsesAuth))
	final := respTerminalResponse(t, respFrames(t, sse))
	if v, ok := final["top_p"]; ok {
		t.Errorf("流式终态不应回显 top_p（该参数从未到达上游）: %v", v)
	}

	if logs := rec.all(); strings.Contains(strings.ToLower(logs), "top_p") {
		t.Errorf("top_p 的丢弃是有意静默的，不允许产生任何留痕:\n%s", logs)
	}
}

// TestResponses_StreamTerminalCarriesEcho 终态事件必须带完整 output、usage 与请求回显字段：
// Codex 的 get_final_response() 直接解析终态事件，且对响应对象做浅校验。
func TestResponses_StreamTerminalCarriesEcho(t *testing.T) {
	body := responsesBody(true, `"instructions":"be terse","tools":[`+weatherToolJSON+`],"tool_choice":"auto",`)
	_, proxy := responsesProxy(t, defaultScript(), nil)
	resp := postResponses(t, proxy, body, responsesAuth)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	frames := respFrames(t, readAll(t, resp))
	assertResponsesStreamInvariants(t, frames)

	final := respTerminalResponse(t, frames)
	if got := mStr(t, final, "status"); got != types.ResponsesStatusCompleted {
		t.Fatalf("终态 status = %q", got)
	}
	if got := mStr(t, final, "instructions"); got != "be terse" {
		t.Errorf("终态回显 instructions = %q", got)
	}
	if got := mStr(t, final, "tool_choice"); got != "auto" {
		t.Errorf("终态回显 tool_choice = %q", got)
	}
	if len(mList(t, final, "tools")) != 1 {
		t.Errorf("终态回显 tools = %v", final["tools"])
	}
	// defaultScript：thinking + text + tool-call 三个 item 全部落在终态 output 里
	items := respOutputItems(t, final, 3)
	kinds := []string{types.ResponsesItemReasoning, types.ResponsesItemMessage, types.ResponsesItemFunctionCall}
	for i, kind := range kinds {
		if got := mStr(t, items[i], "type"); got != kind {
			t.Errorf("output[%d].type = %q, want %s", i, got, kind)
		}
	}
	// usage 加法回填：inputTokens 100 − cached 10 = 90 非缓存，再 + 10 缓存 = 100
	assertResponsesUsage(t, final, 100, 10, 42)
}
