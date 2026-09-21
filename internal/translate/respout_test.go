package translate

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/B1anYu/cmdc2api/internal/types"
)

// ---------- 测试脚手架 ----------

// respStreamEvents 用真实的 StreamTranslator 把 cmdc NDJSON 行翻译成规范格式事件序列。
// 出站状态机的事实输入就是这条链路：手工拼事件会漏掉 signature_delta、
// tool-call 的「start + 单条全量 delta + stop」三连等真实形态。
func respStreamEvents(t *testing.T, lines ...string) []types.StreamEvent {
	t.Helper()
	tr := NewStreamTranslator("test-model", "msg_upstream")
	var evs []types.StreamEvent
	evs = append(evs, tr.Start()...)
	for _, l := range lines {
		evs = append(evs, tr.Feed([]byte(l))...)
	}
	evs = append(evs, tr.Finish()...)
	return evs
}

// respRun 按管线的方式驱动编码器：逐事件 Feed，最后无条件调用一次 Finish
// （管线在流内已报错时同样会调用），返回解出的帧名与载荷。
func respRun(t *testing.T, enc *ResponsesEncoder, evs []types.StreamEvent) ([]string, []map[string]any) {
	t.Helper()
	var frames [][]byte
	for _, ev := range evs {
		frames = append(frames, enc.Feed(ev)...)
	}
	frames = append(frames, enc.Finish()...)
	return respDecode(t, frames)
}

// respDecode 校验 SSE 帧形状并解出 (事件名, data) 序列。
func respDecode(t *testing.T, frames [][]byte) ([]string, []map[string]any) {
	t.Helper()
	names := make([]string, 0, len(frames))
	payloads := make([]map[string]any, 0, len(frames))
	for _, f := range frames {
		s := string(f)
		if !strings.HasPrefix(s, "event: ") || !strings.HasSuffix(s, "\n\n") {
			t.Fatalf("SSE 帧形状不对: %q", s)
		}
		body := strings.TrimSuffix(strings.TrimPrefix(s, "event: "), "\n\n")
		name, data, ok := strings.Cut(body, "\ndata: ")
		if !ok {
			t.Fatalf("SSE 帧缺少 data 行: %q", s)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("帧 data 不是 JSON (%v): %q", err, data)
		}
		names = append(names, name)
		payloads = append(payloads, m)
	}
	return names, payloads
}

// respAssertGolden 断言事件名序列，并连带校验全部流式不变式。
func respAssertGolden(t *testing.T, names []string, payloads []map[string]any, want []string, wantItems int) {
	t.Helper()
	if len(names) != len(want) {
		t.Fatalf("事件数 = %d, want %d\n got: %v\nwant: %v", len(names), len(want), names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("第 %d 个事件 = %s, want %s\n got: %v\nwant: %v", i, names[i], want[i], names, want)
		}
	}
	respAssertSequence(t, payloads)
	respAssertItemLifecycle(t, payloads, wantItems)
	respAssertPartOrdering(t, payloads)
}

// respAssertSequence sequence_number 必须从 0 起、每个已发事件（含终态）递增 1。
func respAssertSequence(t *testing.T, payloads []map[string]any) {
	t.Helper()
	for i, p := range payloads {
		got, ok := p["sequence_number"].(float64)
		if !ok {
			t.Fatalf("事件 %d (%v) 缺少 sequence_number", i, p["type"])
		}
		if int(got) != i {
			t.Fatalf("事件 %d (%v) 的 sequence_number = %v, want %d", i, p["type"], got, i)
		}
	}
}

// respItemKey added/done 配对键：output_index + item id（reasoning 无 id，退化为类型）。
func respItemKey(t *testing.T, p map[string]any) string {
	t.Helper()
	item := respMap(t, p, "item")
	id, ok := item["id"].(string)
	if !ok || id == "" {
		id = respStr(t, item, "type")
	}
	return strconv.Itoa(int(respNum(t, p, "output_index"))) + "/" + id
}

// respAssertItemLifecycle added/done 严格配对、output_index 连续分配、无残留打开 item。
func respAssertItemLifecycle(t *testing.T, payloads []map[string]any, wantItems int) {
	t.Helper()
	var open []string
	closed := 0
	nextIndex := 0
	for _, p := range payloads {
		switch p["type"] {
		case types.ResponsesEventOutputItemAdded:
			idx := int(respNum(t, p, "output_index"))
			if idx != nextIndex {
				t.Fatalf("output_index = %d, want %d（必须连续分配）", idx, nextIndex)
			}
			nextIndex++
			open = append(open, respItemKey(t, p))
		case types.ResponsesEventOutputItemDone:
			if len(open) == 0 {
				t.Fatalf("output_item.done 没有可配对的 added: %v", p)
			}
			last := open[len(open)-1]
			if key := respItemKey(t, p); key != last {
				t.Fatalf("output_item.done %s 与最近的 added %s 不配对", key, last)
			}
			open = open[:len(open)-1]
			closed++
		}
	}
	if len(open) != 0 {
		t.Fatalf("%d 个 item 只 added 未 done: %v", len(open), open)
	}
	if closed != wantItems {
		t.Fatalf("关闭的 item 数 = %d, want %d", closed, wantItems)
	}
}

// respAssertPartOrdering content_part.added 必须早于该 part 的首个 delta，
// 且 part 的 added/done 成对（reasoning 摘要 part 同理）。
func respAssertPartOrdering(t *testing.T, payloads []map[string]any) {
	t.Helper()
	parts := map[string]bool{}
	partsDone := map[string]bool{}
	summaries := map[string]bool{}
	summariesDone := map[string]bool{}
	for _, p := range payloads {
		switch p["type"] {
		case types.ResponsesEventContentPartAdded:
			parts[respIndexKey(p, "content_index")] = true
		case types.ResponsesEventContentPartDone:
			key := respIndexKey(p, "content_index")
			if !parts[key] {
				t.Fatalf("content_part.done 没有可配对的 added: %v", p)
			}
			partsDone[key] = true
		case types.ResponsesEventOutputTextDelta:
			if !parts[respIndexKey(p, "content_index")] {
				t.Fatalf("output_text.delta 先于 content_part.added: %v", p)
			}
		case types.ResponsesEventReasoningSummaryPartAdded:
			summaries[respIndexKey(p, "summary_index")] = true
		case types.ResponsesEventReasoningSummaryPartDone:
			key := respIndexKey(p, "summary_index")
			if !summaries[key] {
				t.Fatalf("reasoning_summary_part.done 没有可配对的 added: %v", p)
			}
			summariesDone[key] = true
		case types.ResponsesEventReasoningSummaryTextDelta:
			if !summaries[respIndexKey(p, "summary_index")] {
				t.Fatalf("reasoning_summary_text.delta 先于 part.added: %v", p)
			}
		}
	}
	for key := range parts {
		if !partsDone[key] {
			t.Fatalf("content_part %s 只 added 未 done", key)
		}
	}
	for key := range summaries {
		if !summariesDone[key] {
			t.Fatalf("reasoning_summary_part %s 只 added 未 done", key)
		}
	}
}

// respIndexKey 把 output_index 与某个子索引拼成 part 唯一键。
func respIndexKey(p map[string]any, key string) string {
	return strconv.Itoa(int(p["output_index"].(float64))) + "/" + strconv.Itoa(int(p[key].(float64)))
}

// ---------- 读取工具 ----------

func respMap(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("字段 %q 不是对象: %v", key, m[key])
	}
	return v
}

func respList(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	v, ok := m[key].([]any)
	if !ok {
		t.Fatalf("字段 %q 不是数组: %#v", key, m[key])
	}
	return v
}

func respStr(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("字段 %q 不是字符串: %#v", key, m[key])
	}
	return v
}

func respNum(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("字段 %q 不是数字: %#v", key, m[key])
	}
	return v
}

// respTerminal 返回终态事件携带的 response 对象。
func respTerminal(t *testing.T, payloads []map[string]any) map[string]any {
	t.Helper()
	last := payloads[len(payloads)-1]
	if !strings.HasPrefix(respStr(t, last, "type"), "response.") {
		t.Fatalf("最后一个事件不是终态: %v", last["type"])
	}
	return respMap(t, last, "response")
}

// respGoldens 出站映射信息：custom 降级、tool_search 代理、namespace 摊平各一类。
func respGoldens() *ResponsesToolMapping {
	return &ResponsesToolMapping{
		Custom:     map[string]bool{"apply_patch": true},
		ToolSearch: true,
		Namespace:  map[string]NamespacedName{"git__status": {Namespace: "git", Name: "status"}},
	}
}

// ---------- golden 场景 ----------

// TestResponsesOut_GoldenSequences 规格 §8.4 的 8 个场景全覆盖：
// 纯文本 / thinking+text / 交错 / 工具调用（普通+custom+tool_search+namespace）/
// 并行工具 / error 中断 / max_tokens incomplete / 零输出。
func TestResponsesOut_GoldenSequences(t *testing.T) {
	cases := []struct {
		name      string
		lines     []string
		want      []string
		items     int
		checkTail func(t *testing.T, payloads []map[string]any)
	}{
		{
			name: "纯文本",
			lines: []string{
				`{"type":"start"}`,
				`{"type":"text-start"}`,
				`{"type":"text-delta","text":"Hello"}`,
				`{"type":"text-delta","text":" world"}`,
				`{"type":"text-end"}`,
				`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":5,"cachedInputTokens":4}}`,
			},
			want: []string{
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
			items: 1,
			checkTail: func(t *testing.T, payloads []map[string]any) {
				resp := respTerminal(t, payloads)
				if resp["status"] != types.ResponsesStatusCompleted {
					t.Errorf("status = %v", resp["status"])
				}
				out := respList(t, resp, "output")[0].(map[string]any)
				part := respList(t, out, "content")[0].(map[string]any)
				if part["text"] != "Hello world" {
					t.Errorf("done 文本 = %v, want Hello world", part["text"])
				}
				// delta 只带增量、done 带全文：由 sequence 上的两条 delta 与 done 的全文共同钉住
				if payloads[4]["delta"] != "Hello" || payloads[5]["delta"] != " world" {
					t.Errorf("delta 不是增量: %v / %v", payloads[4]["delta"], payloads[5]["delta"])
				}
				if payloads[6]["text"] != "Hello world" {
					t.Errorf("output_text.done 不是全文: %v", payloads[6]["text"])
				}
			},
		},
		{
			name: "thinking+text",
			lines: []string{
				`{"type":"reasoning-start"}`,
				`{"type":"reasoning-delta","text":"think "}`,
				`{"type":"reasoning-delta","text":"hard"}`,
				`{"type":"reasoning-end"}`,
				`{"type":"text-delta","text":"hi"}`,
				`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":20,"outputTokens":8}}`,
			},
			want: []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added", // reasoning idx 0
				"response.reasoning_summary_part.added",
				"response.reasoning_summary_text.delta",
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
			items: 2,
			checkTail: func(t *testing.T, payloads []map[string]any) {
				resp := respTerminal(t, payloads)
				output := respList(t, resp, "output")
				if len(output) != 2 {
					t.Fatalf("output = %d 项, want 2", len(output))
				}
				reasoning := output[0].(map[string]any)
				if reasoning["type"] != types.ResponsesItemReasoning {
					t.Fatalf("output[0].type = %v", reasoning["type"])
				}
				// reasoning item 绝不带 id（OpenAI 未签发，伪造 id 会破坏客户端回放）
				if _, ok := reasoning["id"]; ok {
					t.Errorf("reasoning item 不应带 id: %v", reasoning)
				}
				if reasoning["encrypted_content"] == "" || reasoning["encrypted_content"] == nil {
					t.Errorf("思考签名未落到 encrypted_content: %v", reasoning)
				}
				summary := respList(t, reasoning, "summary")[0].(map[string]any)
				if summary["text"] != "think hard" || summary["type"] != "summary_text" {
					t.Errorf("summary 不对: %v", summary)
				}
				message := output[1].(map[string]any)
				if message["type"] != types.ResponsesItemMessage || message["role"] != "assistant" {
					t.Errorf("output[1] 不是 assistant message: %v", message)
				}
				// reasoning 的 delta 事件不带 item_id（reasoning 无 id）
				if _, ok := payloads[4]["item_id"]; ok {
					t.Errorf("reasoning delta 不应带 item_id: %v", payloads[4])
				}
				if payloads[4]["summary_index"] != float64(0) {
					t.Errorf("summary_index = %v", payloads[4]["summary_index"])
				}
			},
		},
		{
			name: "交错 text→thinking→text",
			lines: []string{
				`{"type":"text-delta","text":"a"}`,
				`{"type":"reasoning-delta","text":"b"}`,
				`{"type":"text-delta","text":"c"}`,
				`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":9,"outputTokens":3}}`,
			},
			want: []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added", // message idx 0
				"response.content_part.added",
				"response.output_text.delta",
				"response.output_text.done",
				"response.content_part.done",
				"response.output_item.done",  // thinking 到来先关旧 message item
				"response.output_item.added", // reasoning idx 1
				"response.reasoning_summary_part.added",
				"response.reasoning_summary_text.delta",
				"response.reasoning_summary_text.done",
				"response.reasoning_summary_part.done",
				"response.output_item.done",
				"response.output_item.added", // 新的 message idx 2
				"response.content_part.added",
				"response.output_text.delta",
				"response.output_text.done",
				"response.content_part.done",
				"response.output_item.done",
				"response.completed",
			},
			items: 3,
			checkTail: func(t *testing.T, payloads []map[string]any) {
				resp := respTerminal(t, payloads)
				output := respList(t, resp, "output")
				kinds := []string{"message", "reasoning", "message"}
				if len(output) != len(kinds) {
					t.Fatalf("output = %d 项, want %d", len(output), len(kinds))
				}
				for i, kind := range kinds {
					if got := output[i].(map[string]any)["type"]; got != kind {
						t.Errorf("output[%d].type = %v, want %s", i, got, kind)
					}
				}
				// 两段文本分属两个 message item：thinking 到来时旧 item 必须先关，
				// 否则第二段文本会把第一段覆盖掉（「成功但无输出」故障）
				first, second := output[0].(map[string]any), output[2].(map[string]any)
				if first["id"] == second["id"] {
					t.Errorf("交错场景的两个 message item 不应共用 id: %v", first["id"])
				}
				// content_index 新 item 归 0：两次 content_part.added 的 content_index 都是 0
				for _, i := range []int{3, 15} {
					if payloads[i]["content_index"] != float64(0) {
						t.Errorf("payload[%d].content_index = %v, want 0", i, payloads[i]["content_index"])
					}
				}
			},
		},
		{
			name: "工具调用族",
			lines: []string{
				`{"type":"tool-call","toolCallId":"toolu_1","toolName":"get_weather","input":{"city":"San Francisco"}}`,
				`{"type":"tool-call","toolCallId":"toolu_2","toolName":"apply_patch","input":{"input":"*** Begin Patch"}}`,
				`{"type":"tool-call","toolCallId":"toolu_3","toolName":"tool_search","input":{"query":"git","limit":3}}`,
				`{"type":"tool-call","toolCallId":"toolu_4","toolName":"git__status","input":{"short":true}}`,
				`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":30,"outputTokens":40}}`,
			},
			want: []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added",             // function_call idx 0
				"response.function_call_arguments.delta", // 3 片
				"response.function_call_arguments.delta", //
				"response.function_call_arguments.delta", //
				"response.function_call_arguments.done",  //
				"response.output_item.done",              //
				"response.output_item.added",             // custom_tool_call idx 1
				"response.custom_tool_call_input.delta",  // 2 片
				"response.custom_tool_call_input.delta",  //
				"response.custom_tool_call_input.done",   //
				"response.output_item.done",              //
				"response.output_item.added",             // tool_search_call idx 2
				"response.output_item.done",              // 无参数增量
				"response.output_item.added",             // namespace 还原 idx 3
				"response.function_call_arguments.delta", //
				"response.function_call_arguments.delta", //
				"response.function_call_arguments.done",  //
				"response.output_item.done",              //
				"response.completed",
			},
			items: 4,
			checkTail: func(t *testing.T, payloads []map[string]any) {
				resp := respTerminal(t, payloads)
				output := respList(t, resp, "output")
				if len(output) != 4 {
					t.Fatalf("output = %d 项, want 4", len(output))
				}

				// 普通 function：id/call_id 前缀规则 + 参数全文 + 分片拼接等于 done
				fn := output[0].(map[string]any)
				if fn["type"] != types.ResponsesItemFunctionCall || fn["id"] != "fc_toolu_1" ||
					fn["call_id"] != "toolu_1" || fn["name"] != "get_weather" {
					t.Fatalf("function_call item 不对: %v", fn)
				}
				if fn["arguments"] != `{"city":"San Francisco"}` {
					t.Errorf("arguments = %v", fn["arguments"])
				}
				if joined := respJoinDeltas(payloads, types.ResponsesEventFunctionCallArgsDelta, 0); joined != fn["arguments"] {
					t.Errorf("delta 拼接 = %q, done = %q", joined, fn["arguments"])
				}

				// custom：降级 schema 解包成裸自由文本
				custom := output[1].(map[string]any)
				if custom["type"] != types.ResponsesItemCustomToolCall || custom["id"] != "ctc_toolu_2" ||
					custom["call_id"] != "toolu_2" || custom["name"] != "apply_patch" {
					t.Fatalf("custom_tool_call item 不对: %v", custom)
				}
				if custom["input"] != "*** Begin Patch" {
					t.Errorf("custom input = %v, want 解包后的裸文本", custom["input"])
				}
				if _, ok := custom["arguments"]; ok {
					t.Errorf("custom_tool_call 不应带 arguments 键: %v", custom)
				}
				if joined := respJoinDeltas(payloads, types.ResponsesEventCustomToolCallInputDelta, 1); joined != "*** Begin Patch" {
					t.Errorf("custom delta 拼接 = %q", joined)
				}

				// tool_search：arguments 是对象，且 delta 阶段不发任何参数事件
				ts := output[2].(map[string]any)
				if ts["type"] != types.ResponsesItemToolSearchCall || ts["id"] != "tsc_toolu_3" ||
					ts["call_id"] != "toolu_3" || ts["execution"] != "client" {
					t.Fatalf("tool_search_call item 不对: %v", ts)
				}
				if got := mustJSON(t, ts["arguments"]); got != `{"limit":3,"query":"git"}` {
					t.Errorf("tool_search arguments = %s, want 对象", got)
				}
				for _, p := range payloads {
					if p["type"] == types.ResponsesEventFunctionCallArgsDelta && p["output_index"] == float64(2) {
						t.Errorf("tool_search 不应发参数增量: %v", p)
					}
				}

				// namespace：摊平名还原为 {name: 子工具, namespace: 命名空间}
				ns := output[3].(map[string]any)
				if ns["name"] != "status" || ns["namespace"] != "git" || ns["id"] != "fc_toolu_4" {
					t.Fatalf("namespace 还原不对: %v", ns)
				}
				for _, p := range payloads {
					if p["type"] == types.ResponsesEventFunctionCallArgsDelta &&
						p["output_index"] == float64(3) && p["name"] != "status" {
						t.Errorf("namespace 增量事件的名字未还原: %v", p)
					}
				}
			},
		},
		{
			name: "并行工具",
			lines: []string{
				`{"type":"text-delta","text":"let me check"}`,
				`{"type":"tool-call","toolCallId":"toolu_a","toolName":"read_file","input":{"path":"/a"}}`,
				`{"type":"tool-call","toolCallId":"toolu_b","toolName":"read_file","input":{"path":"/b"}}`,
				`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":11,"outputTokens":22}}`,
			},
			want: []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added", // message idx 0
				"response.content_part.added",
				"response.output_text.delta",
				"response.output_text.done",
				"response.content_part.done",
				"response.output_item.done",  // text 块被 tool-call 关闭 → message item 先关
				"response.output_item.added", // 工具一
				"response.function_call_arguments.delta",
				"response.function_call_arguments.delta",
				"response.function_call_arguments.done",
				"response.output_item.done",
				"response.output_item.added", // 工具二
				"response.function_call_arguments.delta",
				"response.function_call_arguments.delta",
				"response.function_call_arguments.done",
				"response.output_item.done",
				"response.completed",
			},
			items: 3,
			checkTail: func(t *testing.T, payloads []map[string]any) {
				resp := respTerminal(t, payloads)
				output := respList(t, resp, "output")
				if len(output) != 3 {
					t.Fatalf("output = %d 项, want 3", len(output))
				}
				callIDs := []string{}
				for i := 1; i <= 2; i++ {
					item := output[i].(map[string]any)
					if item["type"] != types.ResponsesItemFunctionCall {
						t.Fatalf("output[%d] 不是 function_call: %v", i, item)
					}
					callIDs = append(callIDs, item["call_id"].(string))
				}
				if callIDs[0] == callIDs[1] {
					t.Errorf("并行的两个调用必须有不同的 call_id: %v", callIDs)
				}
				// 两个工具的 output_index 连续
				if payloads[8]["output_index"] != float64(1) || payloads[13]["output_index"] != float64(2) {
					t.Errorf("并行工具的 output_index 不连续: %v / %v",
						payloads[8]["output_index"], payloads[13]["output_index"])
				}
			},
		},
		{
			name: "error 中断",
			lines: []string{
				`{"type":"text-delta","text":"partial"}`,
				`{"type":"error","error":{"message":"<429> slow down"}}`,
			},
			want: []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added",
				"response.content_part.added",
				"response.output_text.delta",
				// 错误到来时 text 块还开着：必须先把 part 收尾再关 item
				"response.output_text.done",
				"response.content_part.done",
				"response.output_item.done",
				"response.failed",
			},
			items: 1,
			checkTail: func(t *testing.T, payloads []map[string]any) {
				resp := respTerminal(t, payloads)
				if resp["status"] != types.ResponsesStatusFailed {
					t.Fatalf("status = %v, want failed", resp["status"])
				}
				errObj := respMap(t, resp, "error")
				if errObj["code"] != "rate_limit_error" || errObj["message"] != "<429> slow down" {
					t.Errorf("error = %v", errObj)
				}
				// 失败终态仍要带已聚合的 output，便于客户端还原失败前的产出
				if got := len(respList(t, resp, "output")); got != 1 {
					t.Fatalf("failed 的 output = %d 项, want 1", got)
				}
			},
		},
		{
			name: "max_tokens incomplete",
			lines: []string{
				`{"type":"text-delta","text":"truncated"}`,
				`{"type":"finish","finishReason":"length","totalUsage":{"inputTokens":15,"outputTokens":64}}`,
			},
			want: []string{
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
			items: 1,
			checkTail: func(t *testing.T, payloads []map[string]any) {
				resp := respTerminal(t, payloads)
				if resp["status"] != types.ResponsesStatusIncomplete {
					t.Fatalf("status = %v, want incomplete", resp["status"])
				}
				details := respMap(t, resp, "incomplete_details")
				if details["reason"] != "max_output_tokens" {
					t.Errorf("incomplete reason = %v", details["reason"])
				}
				if _, ok := resp["error"]; ok {
					t.Errorf("incomplete 不应带 error: %v", resp["error"])
				}
			},
		},
		{
			name: "零输出",
			lines: []string{
				`{"type":"start"}`,
				`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":12,"outputTokens":3}}`,
			},
			want: []string{
				"response.created",
				"response.in_progress",
				"response.completed",
			},
			items: 0,
			checkTail: func(t *testing.T, payloads []map[string]any) {
				resp := respTerminal(t, payloads)
				if resp["status"] != types.ResponsesStatusCompleted {
					t.Fatalf("status = %v", resp["status"])
				}
				// output 必须是数组而非 null
				if got := len(respList(t, resp, "output")); got != 0 {
					t.Fatalf("output = %d 项, want 0", got)
				}
				usage := respMap(t, resp, "usage")
				if usage["input_tokens"] != float64(12) || usage["output_tokens"] != float64(3) ||
					usage["total_tokens"] != float64(15) {
					t.Errorf("usage 不对: %v", usage)
				}
				if cached := respMap(t, usage, "input_tokens_details")["cached_tokens"]; cached != float64(0) {
					t.Errorf("cached_tokens = %v, want 0（键必须存在）", cached)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc := NewResponsesEncoder("test-model", "resp_abc123def456", respGoldens())
			names, payloads := respRun(t, enc, respStreamEvents(t, tc.lines...))
			respAssertGolden(t, names, payloads, tc.want, tc.items)
			if tc.checkTail != nil {
				tc.checkTail(t, payloads)
			}
		})
	}
}

// respJoinDeltas 拼接某个 item 的参数增量事件的 delta，
// 用于断言「增量拼接 == done 全文」。
func respJoinDeltas(payloads []map[string]any, eventType string, outputIndex int) string {
	var b strings.Builder
	for _, p := range payloads {
		if p["type"] == eventType && p["output_index"] == float64(outputIndex) {
			s, _ := p["delta"].(string)
			b.WriteString(s)
		}
	}
	return b.String()
}

// ---------- 状态机不变式 ----------

// TestResponsesOut_MessageItemKeepsContentIndexAcrossParts
// 同一 message item 内的第二个 text 块复用该 item，content_index 只在 part 关闭时前进。
// 该形态无法由当前上游产生（StreamTranslator 只在换块类型时关块），
// 因此手工构造事件，把不变式本身钉住。
func TestResponsesOut_MessageItemKeepsContentIndexAcrossParts(t *testing.T) {
	evs := []types.StreamEvent{
		types.NewMessageStart("msg_x", "test-model"),
		types.NewContentBlockStart(0, types.TextBlockStart{Type: "text"}),
		types.NewContentBlockDelta(0, types.TextDelta{Type: "text_delta", Text: "one"}),
		types.NewContentBlockStop(0),
		types.NewContentBlockStart(1, types.TextBlockStart{Type: "text"}),
		types.NewContentBlockDelta(1, types.TextDelta{Type: "text_delta", Text: "two"}),
		types.NewContentBlockStop(1),
		types.NewMessageStop(),
	}
	enc := NewResponsesEncoder("test-model", "resp_abc", nil)
	names, payloads := respRun(t, enc, evs)

	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added", // 只宣告一次
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done", // 终态前才关
		"response.completed",
	}
	respAssertGolden(t, names, payloads, want, 1)

	if payloads[3]["content_index"] != float64(0) || payloads[7]["content_index"] != float64(1) {
		t.Fatalf("content_index 未在 part 关闭时推进: %v / %v",
			payloads[3]["content_index"], payloads[7]["content_index"])
	}
	resp := respTerminal(t, payloads)
	item := respList(t, resp, "output")[0].(map[string]any)
	content := respList(t, item, "content")
	if len(content) != 2 {
		t.Fatalf("item.content = %d 个 part, want 2", len(content))
	}
	if content[0].(map[string]any)["text"] != "one" || content[1].(map[string]any)["text"] != "two" {
		t.Errorf("item.content 文本不对: %v", content)
	}
	// added 时 content 恒为空数组
	added := respMap(t, payloads[2], "item")
	if got := len(respList(t, added, "content")); got != 0 {
		t.Errorf("added 的 content = %d 项, want 0", got)
	}
}

// TestResponsesOut_ToolArgsSharding 参数分片：每片不超过 10 个 rune（多字节字符不被切断），
// 且拼接结果恰好等于 done 给出的全文。
func TestResponsesOut_ToolArgsSharding(t *testing.T) {
	lines := []string{
		`{"type":"tool-call","toolCallId":"toolu_1","toolName":"f","input":{"q":"中文参数中文参数中文参数中文参数"}}`,
		`{"type":"tool-call","toolCallId":"toolu_2","toolName":"apply_patch","input":{"input":"中文补丁中文补丁中文补丁中文补丁"}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":1,"outputTokens":9}}`,
	}
	enc := NewResponsesEncoder("test-model", "resp_x", respGoldens())
	_, payloads := respRun(t, enc, respStreamEvents(t, lines...))

	function := respJoinDeltas(payloads, types.ResponsesEventFunctionCallArgsDelta, 0)
	if !json.Valid([]byte(function)) {
		t.Fatalf("分片拼接后不是合法 JSON: %q", function)
	}
	if !strings.Contains(function, "中文参数中文参数") {
		t.Fatalf("多字节字符被破坏: %q", function)
	}
	custom := respJoinDeltas(payloads, types.ResponsesEventCustomToolCallInputDelta, 1)
	if custom != "中文补丁中文补丁中文补丁中文补丁" {
		t.Fatalf("custom 增量拼接 = %q", custom)
	}
	for _, p := range payloads {
		switch p["type"] {
		case types.ResponsesEventFunctionCallArgsDelta, types.ResponsesEventCustomToolCallInputDelta:
			if n := len([]rune(p["delta"].(string))); n > 10 {
				t.Errorf("分片 %q 长度 %d > 10", p["delta"], n)
			}
		}
	}
}

// TestResponsesOut_ToolSearchArgumentsNeverNull 参数原文恰为 null 时，
// done item 的 arguments 必须是对象而非 null（#4）：上游给出的是 JSON 字符串 "null"
// （partialJSON 会把裸 null 折成 "{}"，只有字符串形态能走到这条路径），
// 出站解析后得到 typed-nil map——codex 物化该调用时不接受非对象。
func TestResponsesOut_ToolSearchArgumentsNeverNull(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"字符串 null", `"null"`},
		{"空对象", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc := NewResponsesEncoder("test-model", "resp_x", respGoldens())
			_, payloads := respRun(t, enc, respStreamEvents(t,
				`{"type":"tool-call","toolCallId":"toolu_1","toolName":"tool_search","input":`+tc.input+`}`,
				`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":1,"outputTokens":9}}`))
			out := respList(t, respTerminal(t, payloads), "output")
			if len(out) != 1 {
				t.Fatalf("output = %d 项, want 1", len(out))
			}
			item := out[0].(map[string]any)
			if item["type"] != types.ResponsesItemToolSearchCall {
				t.Fatalf("item 类型 = %v", item["type"])
			}
			if item["arguments"] == nil {
				t.Fatalf("arguments = null，线上必须是对象: %v", item)
			}
			if got := mustJSON(t, item["arguments"]); got != "{}" {
				t.Errorf("arguments = %s, want {}", got)
			}
		})
	}
}

// TestResponsesOut_CustomInputUnwrap custom 工具的降级参数解包规则。
func TestResponsesOut_CustomInputUnwrap(t *testing.T) {
	cases := []struct {
		name string
		args string // 降级 function 的 arguments
		want string
	}{
		{"正常解包", `{"input":"ls -la"}`, "ls -la"},
		{"空对象", `{}`, ""},
		{"缺 input 键", `{"other":1}`, ""},
		{"input 为 null", `{"input":null}`, ""},
		{"input 非字符串", `{"input":42}`, "42"},
		{"非法 JSON 原样整串", `{"input":"cut`, `{"input":"cut`},
		{"空参数", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evs := []types.StreamEvent{
				types.NewMessageStart("msg_x", "test-model"),
				types.NewContentBlockStart(0, types.ToolUseBlockStart{Type: "tool_use", ID: "toolu_1", Name: "apply_patch"}),
				types.NewContentBlockDelta(0, types.InputJSONDelta{Type: "input_json_delta", PartialJSON: tc.args}),
				types.NewContentBlockStop(0),
				types.NewMessageStop(),
			}
			enc := NewResponsesEncoder("test-model", "resp_x", respGoldens())
			names, payloads := respRun(t, enc, evs)
			if names[len(names)-1] != "response.completed" {
				t.Fatalf("终态 = %s", names[len(names)-1])
			}
			resp := respTerminal(t, payloads)
			item := respList(t, resp, "output")[0].(map[string]any)
			if item["type"] != types.ResponsesItemCustomToolCall {
				t.Fatalf("item 类型 = %v", item["type"])
			}
			if item["input"] != tc.want {
				t.Errorf("input = %q, want %q", item["input"], tc.want)
			}
			// done 事件与 item 必须给同一份文本
			for _, p := range payloads {
				if p["type"] == types.ResponsesEventCustomToolCallInputDone && p["input"] != tc.want {
					t.Errorf("input.done = %q, want %q", p["input"], tc.want)
				}
			}
			// 增量拼接必须是 done 全文的前缀（客户端按序累积，不能多也不能错位）
			if joined := respJoinDeltas(payloads, types.ResponsesEventCustomToolCallInputDelta, 0); !strings.HasPrefix(tc.want, joined) {
				t.Errorf("增量拼接 %q 不是 done %q 的前缀", joined, tc.want)
			}
		})
	}
}

// TestResponsesOut_FinishIdempotent 幂等：管线在流内错误后仍会无条件调用 Finish，
// 终态之后的任何输入都必须被丢弃。
func TestResponsesOut_FinishIdempotent(t *testing.T) {
	enc := NewResponsesEncoder("test-model", "resp_x", nil)
	tr := NewStreamTranslator("test-model", "msg_upstream")
	for _, ev := range tr.Start() {
		enc.Feed(ev)
	}
	// 只喂内容事件，不喂 message_stop：终态必须由编码器自己的 Finish 补齐
	for _, ev := range tr.Feed([]byte(`{"type":"text-delta","text":"hi"}`)) {
		enc.Feed(ev)
	}
	if frames := enc.Finish(); len(frames) == 0 {
		t.Fatal("首次 Finish 必须产出终态帧")
	}
	if frames := enc.Finish(); len(frames) != 0 {
		t.Fatalf("重复 Finish 必须为空, got %d 帧", len(frames))
	}
	// 终态之后再喂事件（含 error 事件）不得再产帧
	for _, ev := range []types.StreamEvent{
		types.NewMessageStop(),
		types.NewErrorEvent("api_error", "late"),
		types.NewContentBlockStart(9, types.TextBlockStart{Type: "text"}),
	} {
		if frames := enc.Feed(ev); len(frames) != 0 {
			t.Fatalf("终态后的事件产生了帧: %q", frames)
		}
	}
	if frames := enc.Finish(); len(frames) != 0 {
		t.Fatalf("终态后 Finish 必须为空, got %d 帧", len(frames))
	}
}

// TestResponsesOut_ErrorBeforeContent 首帧前失败（管线 idle 超时/断流合成的错误事件）：
// 仍然要给出 response.created → response.failed 的完整生命周期，而不是只有裸错误。
func TestResponsesOut_ErrorBeforeContent(t *testing.T) {
	evs := []types.StreamEvent{
		types.NewMessageStart("msg_upstream", "test-model"),
		types.NewErrorEvent("rate_limit_error", "upstream idle timeout"),
	}
	enc := NewResponsesEncoder("test-model", "resp_x", nil)
	names, payloads := respRun(t, enc, evs)
	want := []string{"response.created", "response.in_progress", "response.failed"}
	respAssertGolden(t, names, payloads, want, 0)

	resp := respTerminal(t, payloads)
	errObj := respMap(t, resp, "error")
	if errObj["code"] != "rate_limit_error" || errObj["message"] != "upstream idle timeout" {
		t.Errorf("error = %v", errObj)
	}
	if got := len(respList(t, resp, "output")); got != 0 {
		t.Errorf("output = %d 项, want 0", got)
	}
}

// TestResponsesOut_ZeroOutputErrorPath 上游零输出时 StreamTranslator 会合成 error 事件，
// 出站必须转成 response.failed 而不是空 completed。
func TestResponsesOut_ZeroOutputErrorPath(t *testing.T) {
	enc := NewResponsesEncoder("test-model", "resp_x", nil)
	names, payloads := respRun(t, enc, respStreamEvents(t,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":50,"outputTokens":0}}`))
	if names[len(names)-1] != "response.failed" {
		t.Fatalf("零输出应以 response.failed 收尾: %v", names)
	}
	if got := respTerminal(t, payloads)["status"]; got != types.ResponsesStatusFailed {
		t.Errorf("status = %v", got)
	}
}

// TestResponsesOut_ItemPartsClosedOnEveryTerminationPath 「块类型 × 终结路径」矩阵：
// item 被强制收尾时（流内报错、上游半截断开），已宣告的 part 必须先 done 再关 item。
//
// 严格客户端（Codex）收到 reasoning_summary_part.added 后会一直等 part.done / summary_text.done，
// 只发 output_item.done 会让该 part 永久悬空（#3）。正常 content_block_stop 路径由既有 golden 覆盖，
// 这里补两条「不经 content_block_stop 的强制收尾」路径——单场景 golden 挡不住这类缺口（D9 教训）。
//
// 可达性：流内报错路径真实可达（idle 超时/断流由管线合成 error 事件喂给编码器，
// 而 StreamTranslator 在 hasError 时不再关块）；半截流路径钉的是编码器自身的 Finish 契约
// （管线无条件调用 enc.Finish()，不允许残留悬空 part）。
func TestResponsesOut_ItemPartsClosedOnEveryTerminationPath(t *testing.T) {
	cases := []struct {
		name       string
		lines      []string
		truncated  bool // true：不喂 StreamTranslator.Finish 的事件，模拟上游半截断开
		want       []string
		wantStatus string
		wantType   string
		wantText   string
	}{
		{
			name: "思考块 × 流内报错",
			lines: []string{
				`{"type":"reasoning-start"}`,
				`{"type":"reasoning-delta","text":"thinking hard"}`,
				`{"type":"error","error":{"message":"<429> slow down"}}`,
			},
			want: []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added",
				"response.reasoning_summary_part.added",
				"response.reasoning_summary_text.delta",
				// 报错时思考块还开着：摘要 part 必须先收尾，item 才能关
				"response.reasoning_summary_text.done",
				"response.reasoning_summary_part.done",
				"response.output_item.done",
				"response.failed",
			},
			wantStatus: types.ResponsesStatusFailed,
			wantType:   types.ResponsesItemReasoning,
			wantText:   "thinking hard",
		},
		{
			name: "思考块 × 上游半截断开",
			lines: []string{
				`{"type":"reasoning-start"}`,
				`{"type":"reasoning-delta","text":"cut off"}`,
			},
			truncated: true,
			want: []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added",
				"response.reasoning_summary_part.added",
				"response.reasoning_summary_text.delta",
				"response.reasoning_summary_text.done",
				"response.reasoning_summary_part.done",
				"response.output_item.done",
				"response.completed",
			},
			wantStatus: types.ResponsesStatusCompleted,
			wantType:   types.ResponsesItemReasoning,
			wantText:   "cut off",
		},
		{
			name:      "文本块 × 上游半截断开",
			lines:     []string{`{"type":"text-delta","text":"half a sentence"}`},
			truncated: true,
			want: []string{
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
			wantStatus: types.ResponsesStatusCompleted,
			wantType:   types.ResponsesItemMessage,
			wantText:   "half a sentence",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evs := respStreamEvents(t, tc.lines...)
			if tc.truncated {
				evs = respTruncatedStreamEvents(t, tc.lines...)
			}
			enc := NewResponsesEncoder("test-model", "resp_x", nil)
			names, payloads := respRun(t, enc, evs)
			// respAssertGolden 连带跑 item 配对与 part 成对不变式：
			// 悬空的 reasoning_summary_part 会被 respAssertPartOrdering 直接抓住。
			respAssertGolden(t, names, payloads, tc.want, 1)

			resp := respTerminal(t, payloads)
			if got := resp["status"]; got != tc.wantStatus {
				t.Errorf("status = %v, want %s", got, tc.wantStatus)
			}
			// 失败/断流前已聚合的产出必须留在 output 里，客户端才能还原
			items := respList(t, resp, "output")
			if len(items) != 1 {
				t.Fatalf("output = %d 项, want 1", len(items))
			}
			item := items[0].(map[string]any)
			if item["type"] != tc.wantType {
				t.Fatalf("output[0].type = %v, want %s", item["type"], tc.wantType)
			}
			var text string
			if tc.wantType == types.ResponsesItemReasoning {
				text = respStr(t, respList(t, item, "summary")[0].(map[string]any), "text")
			} else {
				text = respStr(t, respList(t, item, "content")[0].(map[string]any), "text")
			}
			if text != tc.wantText {
				t.Errorf("output 文本 = %q, want %q", text, tc.wantText)
			}
		})
	}
}

// respTruncatedStreamEvents 与 respStreamEvents 同源，但**不**调用 StreamTranslator.Finish：
// 模拟上游没给终止事件就断开，此时「关掉残留块」只能由出站编码器的 Finish 兜底。
func respTruncatedStreamEvents(t *testing.T, lines ...string) []types.StreamEvent {
	t.Helper()
	tr := NewStreamTranslator("test-model", "msg_upstream")
	evs := tr.Start()
	for _, l := range lines {
		evs = append(evs, tr.Feed([]byte(l))...)
	}
	return evs
}

// TestResponsesOut_RefusalIncomplete refusal 停止原因映射为 content_filter。
func TestResponsesOut_RefusalIncomplete(t *testing.T) {
	enc := NewResponsesEncoder("test-model", "resp_x", nil)
	_, payloads := respRun(t, enc, respStreamEvents(t,
		`{"type":"text-delta","text":"x"}`,
		`{"type":"finish","finishReason":"content-filter","totalUsage":{"inputTokens":3,"outputTokens":4}}`))
	resp := respTerminal(t, payloads)
	if resp["status"] != types.ResponsesStatusIncomplete {
		t.Fatalf("status = %v", resp["status"])
	}
	if got := respMap(t, resp, "incomplete_details")["reason"]; got != "content_filter" {
		t.Errorf("incomplete reason = %v", got)
	}
}

// TestResponsesOut_UsageAddition usage 加法回填：内部 noCache 口径 → Responses 含缓存总量，
// total 由 types 层算好，缓存写入量只在非 nil 且为正时计入。
func TestResponsesOut_UsageAddition(t *testing.T) {
	cases := []struct {
		name       string
		usage      types.DeltaUsage
		wantInput  float64
		wantCached float64
		wantTotal  float64
		wantOutput float64
	}{
		{
			name:      "含缓存读取与写入",
			usage:     types.DeltaUsage{InputTokens: 100, CacheReadInputTokens: 40, CacheCreationInputTokens: intPtr(5), OutputTokens: 20},
			wantInput: 145, wantCached: 40, wantTotal: 165, wantOutput: 20,
		},
		{
			name:      "缓存写入为 0 不重复计",
			usage:     types.DeltaUsage{InputTokens: 100, CacheReadInputTokens: 40, CacheCreationInputTokens: intPtr(0), OutputTokens: 7},
			wantInput: 140, wantCached: 40, wantTotal: 147, wantOutput: 7,
		},
		{
			name:      "无缓存写入",
			usage:     types.DeltaUsage{InputTokens: 12, OutputTokens: 3},
			wantInput: 12, wantCached: 0, wantTotal: 15, wantOutput: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evs := []types.StreamEvent{
				types.NewMessageStart("msg_upstream", "test-model"),
				types.NewContentBlockStart(0, types.TextBlockStart{Type: "text"}),
				types.NewContentBlockDelta(0, types.TextDelta{Type: "text_delta", Text: "hi"}),
				types.NewContentBlockStop(0),
				types.NewMessageDelta("end_turn", tc.usage),
				types.NewMessageStop(),
			}
			enc := NewResponsesEncoder("test-model", "resp_x", nil)
			_, payloads := respRun(t, enc, evs)
			usage := respMap(t, respTerminal(t, payloads), "usage")
			if usage["input_tokens"] != tc.wantInput {
				t.Errorf("input_tokens = %v, want %v", usage["input_tokens"], tc.wantInput)
			}
			if usage["output_tokens"] != tc.wantOutput {
				t.Errorf("output_tokens = %v, want %v", usage["output_tokens"], tc.wantOutput)
			}
			if usage["total_tokens"] != tc.wantTotal {
				t.Errorf("total_tokens = %v, want %v", usage["total_tokens"], tc.wantTotal)
			}
			details := respMap(t, usage, "input_tokens_details")
			if details["cached_tokens"] != tc.wantCached {
				t.Errorf("cached_tokens = %v, want %v", details["cached_tokens"], tc.wantCached)
			}
		})
	}
}

// TestResponsesOut_UsageLastWriteWins 多条 message_delta 的 usage 是**覆盖**语义：
// usage 在上游是多步循环累加后的最终快照，再累加会把同一份数字放大成倍
// （口径与 Chat 侧及 Aggregator 的 last-write-wins 一致）。
func TestResponsesOut_UsageLastWriteWins(t *testing.T) {
	evs := []types.StreamEvent{
		types.NewMessageStart("msg_upstream", "test-model"),
		types.NewContentBlockStart(0, types.TextBlockStart{Type: "text"}),
		types.NewContentBlockDelta(0, types.TextDelta{Type: "text_delta", Text: "hi"}),
		types.NewContentBlockStop(0),
		types.NewMessageDelta("end_turn", types.DeltaUsage{
			InputTokens: 100, CacheReadInputTokens: 40, CacheCreationInputTokens: intPtr(5), OutputTokens: 20,
		}),
		types.NewMessageDelta("end_turn", types.DeltaUsage{
			InputTokens: 120, CacheReadInputTokens: 50, CacheCreationInputTokens: intPtr(7), OutputTokens: 30,
		}),
		types.NewMessageStop(),
	}
	enc := NewResponsesEncoder("test-model", "resp_x", nil)
	_, payloads := respRun(t, enc, evs)
	usage := respMap(t, respTerminal(t, payloads), "usage")

	// 末次快照：input = 120 + 50 + 7（含缓存总量），output = 30
	if usage["input_tokens"] != float64(177) {
		t.Errorf("input_tokens = %v, want 177（覆盖而非累加）", usage["input_tokens"])
	}
	if usage["output_tokens"] != float64(30) {
		t.Errorf("output_tokens = %v, want 30（覆盖而非累加）", usage["output_tokens"])
	}
	if usage["total_tokens"] != float64(207) {
		t.Errorf("total_tokens = %v, want 207", usage["total_tokens"])
	}
	if got := respMap(t, usage, "input_tokens_details")["cached_tokens"]; got != float64(50) {
		t.Errorf("cached_tokens = %v, want 50", got)
	}
}

// TestResponsesOut_EchoFields 终态 response 必须回显请求字段（Codex 做浅校验），
// 并且恒带 previous_response_id:null 与 store:false（本代理不保存服务端状态）。
//
// top_p 反向钉住：它不在回显字段里（键也不出现）——它从未被转发给上游，
// 回显它等于谎报参数已生效（#14）。
func TestResponsesOut_EchoFields(t *testing.T) {
	enc := NewResponsesEncoder("test-model", "resp_echo", nil)
	temp, maxOut := 0.2, 4096
	enc.SetEcho(ResponsesEcho{
		Instructions: "be nice",
		Tools: []types.ResponsesTool{{
			Type: types.ResponsesToolFunction, Name: "f", Description: "d",
			Parameters: json.RawMessage(`{"type":"object"}`),
		}},
		ToolChoice:        json.RawMessage(`{"type":"function","name":"f"}`),
		Temperature:       &temp,
		MaxOutputTokens:   &maxOut,
		ParallelToolCalls: true,
	})
	_, payloads := respRun(t, enc, respStreamEvents(t,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":1}}`))
	resp := respTerminal(t, payloads)
	for _, key := range []string{"id", "object", "created_at", "status", "output", "usage", "model",
		"instructions", "tools", "tool_choice", "temperature", "max_output_tokens",
		"parallel_tool_calls", "previous_response_id", "store"} {
		if _, ok := resp[key]; !ok {
			t.Errorf("response 缺少字段 %q", key)
		}
	}
	if _, ok := resp["top_p"]; ok {
		t.Errorf("response 不应回显 top_p（该参数从未到达上游）: %v", resp["top_p"])
	}
	if resp["object"] != "response" || resp["store"] != false || resp["previous_response_id"] != nil {
		t.Errorf("顶层常量字段不对: object=%v store=%v prev=%v",
			resp["object"], resp["store"], resp["previous_response_id"])
	}
	if resp["instructions"] != "be nice" || resp["temperature"] != 0.2 || resp["max_output_tokens"] != float64(4096) {
		t.Errorf("回显字段不对: %v", resp)
	}
	tools := respList(t, resp, "tools")
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "f" {
		t.Errorf("tools 回显不对: %v", tools)
	}
	if _, ok := tools[0].(map[string]any)["input_schema"]; ok {
		t.Errorf("tools 回显必须是 Responses 扁平形状，不能是 Anthropic 形状: %v", tools[0])
	}
}

// TestResponsesOut_StreamMatchesAggregate 同一批事件下，流式终态事件里的 response
// 与非流式 Result() 必须逐字段一致（created_at 除外，两次构造的时间戳不同）。
func TestResponsesOut_StreamMatchesAggregate(t *testing.T) {
	evs := respStreamEvents(t,
		`{"type":"reasoning-delta","text":"think"}`,
		`{"type":"text-delta","text":"answer"}`,
		`{"type":"tool-call","toolCallId":"toolu_1","toolName":"apply_patch","input":{"input":"patch"}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":7,"outputTokens":9,"cachedInputTokens":2}}`,
	)
	respAssertStreamMatchesAggregate(t, evs, respGoldens(), ResponsesEcho{Instructions: "sys", ParallelToolCalls: true})
}

// TestResponsesOut_StreamMatchesAggregateIncomplete 截断/内容过滤场景下的同一对齐：
// incomplete 终态与非流式 Result() 必须逐字段一致，尤其是 incomplete_details
// （聚合路径曾整体丢失该字段，导致「流式有 reason、非流式没有」的语义分裂）。
func TestResponsesOut_StreamMatchesAggregateIncomplete(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"max_tokens", []string{
			`{"type":"text-delta","text":"truncated"}`,
			`{"type":"finish","finishReason":"length","totalUsage":{"inputTokens":15,"outputTokens":64}}`,
		}, responsesReasonMaxOutputTokens},
		{"content_filter", []string{
			`{"type":"text-delta","text":"refused"}`,
			`{"type":"finish","finishReason":"content-filter","totalUsage":{"inputTokens":9,"outputTokens":2}}`,
		}, responsesReasonContentFilter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evs := respStreamEvents(t, tc.lines...)
			respAssertStreamMatchesAggregate(t, evs, nil, ResponsesEcho{})

			// 对齐之外再单独钉一次取值：非流式必须自带 reason，缺席即本次修复的缺陷
			agg := NewResponsesAggregator("test-model", "resp_incomplete", nil)
			for _, ev := range evs {
				agg.Feed(ev)
			}
			result := agg.Result().(*types.ResponsesResponse)
			if result.Status != types.ResponsesStatusIncomplete {
				t.Fatalf("status = %q, want incomplete", result.Status)
			}
			if result.IncompleteDetails == nil {
				t.Fatalf("非流式 Result 丢失 incomplete_details（流式/非流式语义分裂）")
			}
			if result.IncompleteDetails.Reason != tc.want {
				t.Errorf("incomplete reason = %q, want %q", result.IncompleteDetails.Reason, tc.want)
			}
		})
	}
}

// respAssertStreamMatchesAggregate 用同一批事件分别驱动编码器与聚合器，
// 断言流式终态事件里的 response 与非流式 Result() 逐字段一致
// （created_at 除外，两次构造的时间戳不同）。语义只有一处实现，形状就必须处处相同。
func respAssertStreamMatchesAggregate(t *testing.T, evs []types.StreamEvent, mapping *ResponsesToolMapping, echo ResponsesEcho) {
	t.Helper()
	enc := NewResponsesEncoder("test-model", "resp_same", mapping)
	enc.SetEcho(echo)
	_, payloads := respRun(t, enc, evs)
	streamed := respTerminal(t, payloads)
	delete(streamed, "created_at")

	agg := NewResponsesAggregator("test-model", "resp_same", mapping)
	agg.SetEcho(echo)
	for _, ev := range evs {
		agg.Feed(ev)
	}
	result, ok := agg.Result().(*types.ResponsesResponse)
	if !ok {
		t.Fatalf("Result 类型 = %T", agg.Result())
	}
	aggregated := respNormalize(t, result.Wire())
	delete(aggregated, "created_at")

	if !reflect.DeepEqual(streamed, aggregated) {
		t.Fatalf("流式与非流式语义分裂:\n stream: %s\nbuffer: %s",
			mustJSON(t, streamed), mustJSON(t, aggregated))
	}
}

// TestResponsesOut_AggregateWithoutTerminalEvent 上游没给终止事件（如非流式路径下的
// 断流）时，Result 仍需自行收尾，产出完整对象而非半成品。
func TestResponsesOut_AggregateWithoutTerminalEvent(t *testing.T) {
	agg := NewResponsesAggregator("test-model", "resp_partial", nil)
	tr := NewStreamTranslator("test-model", "msg_upstream")
	agg.Feed(tr.Start()[0])
	for _, ev := range tr.Feed([]byte(`{"type":"text-delta","text":"cut off"}`)) {
		agg.Feed(ev)
	}
	result := agg.Result().(*types.ResponsesResponse)
	if result.Status != types.ResponsesStatusCompleted {
		t.Errorf("status = %v", result.Status)
	}
	if len(result.Output) != 1 {
		t.Fatalf("output = %d 项, want 1（残留 item 必须被收尾）", len(result.Output))
	}
	if len(result.Output[0].Content) != 1 || result.Output[0].Content[0].Text != "cut off" {
		t.Errorf("残留 item 内容不对: %+v", result.Output[0])
	}
}

// respNormalize 把任意值经 JSON 往返归一化为 map，便于与其他 JSON 反序列化产物比较。
func respNormalize(t *testing.T, v any) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(mustJSON(t, v)), &m); err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	return m
}

func intPtr(v int) *int { return &v }
