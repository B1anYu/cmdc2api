package types

import (
	"encoding/json"
	"reflect"
	"testing"
)

// keysOf 把值序列化后取顶层键，用于断言「字段是否真的出现在线上」——
// omitempty 陷阱只有在 marshal 后的 JSON 上才看得见。
func keysOf(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return m
}

func strPtr(s string) *string { return &s }

// finish_reason 三态：nil 必须序列化为 null（流未结束），指针必须给字符串。
// 用 omitempty 会让中间分片少一个键，严格客户端会按「字段缺失」处理。
func TestChatChunkWire_FinishReasonAlwaysPresent(t *testing.T) {
	cases := []struct {
		name string
		in   *string
		want string
	}{
		{"nil", nil, "null"},
		{"empty", strPtr(""), `""`},
		{"value", strPtr("tool_calls"), `"tool_calls"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunk := ChatChunk{
				ID: "chatcmpl-x", Object: "chat.completion.chunk", Created: 1, Model: "m",
				Choices: []ChatChunkChoice{{Index: 0, FinishReason: tc.in}},
			}
			choices := keysOf(t, chunk)["choices"]
			var list []map[string]json.RawMessage
			if err := json.Unmarshal(choices, &list); err != nil {
				t.Fatalf("choices: %v", err)
			}
			raw, ok := list[0]["finish_reason"]
			if !ok {
				t.Fatal("finish_reason key missing")
			}
			if string(raw) != tc.want {
				t.Errorf("finish_reason = %s, want %s", raw, tc.want)
			}
			if _, ok := list[0]["index"]; !ok {
				t.Error("index key missing")
			}
		})
	}
}

// delta.content 三态：nil 表示本帧不带该键（工具调用帧），空串指针表示显式空内容（首帧）。
func TestChatDeltaWire_ContentTriState(t *testing.T) {
	cases := []struct {
		name       string
		in         *string
		wantAbsent bool
		wantValue  string
	}{
		{"nil-absent", nil, true, ""},
		{"empty-present", strPtr(""), false, `""`},
		{"text-present", strPtr("hi"), false, `"hi"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys := keysOf(t, ChatDelta{Role: "assistant", Content: tc.in})
			raw, ok := keys["content"]
			if tc.wantAbsent {
				if ok {
					t.Fatalf("content should be absent, got %s", raw)
				}
				return
			}
			if !ok {
				t.Fatal("content key missing")
			}
			if string(raw) != tc.wantValue {
				t.Errorf("content = %s, want %s", raw, tc.wantValue)
			}
			if _, ok := keys["role"]; !ok {
				t.Error("role key missing on first delta")
			}
		})
	}
}

// 工具调用帧：index 0 必须出现；name/arguments 为空串时 arguments 仍要存在
// （客户端按 index 归并、按 arguments 拼接，缺键会被当成空）。
func TestChatDeltaToolCallWire_IndexZeroAndEmptyArguments(t *testing.T) {
	keys := keysOf(t, ChatDelta{ToolCalls: []ChatDeltaToolCall{{
		Index: 0, ID: "call_1", Type: "function",
		Function: ChatFunctionCall{Name: "get_weather", Arguments: ""},
	}}})
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(keys["tool_calls"], &calls); err != nil {
		t.Fatalf("tool_calls: %v", err)
	}
	raw, ok := calls[0]["index"]
	if !ok || string(raw) != "0" {
		t.Errorf("index = %s (present=%v), want 0", raw, ok)
	}
	var fn map[string]json.RawMessage
	if err := json.Unmarshal(calls[0]["function"], &fn); err != nil {
		t.Fatalf("function: %v", err)
	}
	if _, ok := fn["arguments"]; !ok {
		t.Error("arguments key missing (clients accumulate it by key presence)")
	}
}

// usage chunk 的 choices 必须是空数组而非 null，且 usage 四个字段全在。
func TestChatChunkWire_UsageChunkShape(t *testing.T) {
	keys := keysOf(t, ChatChunk{
		ID: "chatcmpl-x", Object: "chat.completion.chunk", Model: "m",
		Choices: []ChatChunkChoice{},
		Usage:   &ChatUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
	})
	if got := string(keys["choices"]); got != "[]" {
		t.Errorf("choices = %s, want []", got)
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(keys["usage"], &usage); err != nil {
		t.Fatalf("usage: %v", err)
	}
	for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens", "prompt_tokens_details"} {
		if _, ok := usage[k]; !ok {
			t.Errorf("usage.%s missing", k)
		}
	}
	var details map[string]json.RawMessage
	if err := json.Unmarshal(usage["prompt_tokens_details"], &details); err != nil {
		t.Fatalf("prompt_tokens_details: %v", err)
	}
	if _, ok := details["cached_tokens"]; !ok {
		t.Error("prompt_tokens_details.cached_tokens missing")
	}
}

// 非流式 message：只有工具调用时 content 必须是显式 null（不能缺键）。
func TestChatResponseMessageWire_ContentAlwaysPresent(t *testing.T) {
	keys := keysOf(t, ChatResponseMessage{
		Role:      "assistant",
		ToolCalls: []ChatMessageToolCall{{ID: "call_1", Type: "function"}},
	})
	raw, ok := keys["content"]
	if !ok {
		t.Fatal("content key missing")
	}
	if string(raw) != "null" {
		t.Errorf("content = %s, want null", raw)
	}
	if _, ok := keys["reasoning_content"]; ok {
		t.Error("reasoning_content should be omitted when empty")
	}
}

// 非流式 choices：数组形态且带 index / finish_reason / message。
func TestChatCompletionWire_ChoicesArray(t *testing.T) {
	reason := "stop"
	keys := keysOf(t, ChatCompletion{
		ID: "chatcmpl-x", Object: "chat.completion", Created: 1, Model: "m",
		Choices: []ChatChoice{{Index: 0, Message: &ChatResponseMessage{Role: "assistant", Content: strPtr("hi")}, FinishReason: &reason}},
	})
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(keys["choices"], &choices); err != nil {
		t.Fatalf("choices: %v", err)
	}
	if len(choices) != 1 {
		t.Fatalf("choices len = %d, want 1", len(choices))
	}
	if string(choices[0]["finish_reason"]) != `"stop"` {
		t.Errorf("finish_reason = %s", choices[0]["finish_reason"])
	}
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(choices[0]["message"], &msg); err != nil {
		t.Fatalf("message: %v", err)
	}
	if string(msg["content"]) != `"hi"` {
		t.Errorf("message.content = %s", msg["content"])
	}
}

// 入站解析宽松性：字符串/对象两种 stop、裸字符串 image_url、对象形态 reasoning 别名、
// 缺 type 的工具声明都要能解析成功。
func TestChatRequest_LooseParsing(t *testing.T) {
	raw := `{
		"model": "m",
		"n": 1,
		"stop": "END",
		"tool_choice": {"type": "function", "function": {"name": "f"}},
		"reasoning_effort": {"effort": "low", "summary": "auto"},
		"stream_options": {"include_usage": true},
		"tools": [{"function": {"name": "f", "parameters": {"type": "object"}}}],
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "look"},
				{"type": "image_url", "image_url": "https://example.com/a.png"}
			]},
			{"role": "assistant", "content": null, "reasoning": {"content": "hmm"},
			 "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "f", "arguments": "{}"}}]}
		]
	}`
	var req ChatRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(req.Messages))
	}
	var parts []ChatContentPart
	if err := json.Unmarshal(req.Messages[0].Content, &parts); err != nil {
		t.Fatalf("user content: %v", err)
	}
	if parts[1].ImageURL == nil || parts[1].ImageURL.URL != "https://example.com/a.png" {
		t.Errorf("bare-string image_url not parsed: %+v", parts[1].ImageURL)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function == nil || req.Tools[0].Function.Name != "f" {
		t.Errorf("tool without type not parsed: %+v", req.Tools)
	}
	var effort struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(req.ReasoningEffort, &effort); err != nil || effort.Effort != "low" {
		t.Errorf("reasoning_effort object not preserved: %s", req.ReasoningEffort)
	}
}

// 无类型的安全丢弃字段靠原始报文探测存在性（与 ResponsesIgnoredFields 对等）。
func TestChatIgnoredFields(t *testing.T) {
	cases := []struct {
		label string
		raw   string
		want  []string
	}{
		{"无", `{"model":"m","messages":[]}`, nil},
		{"单个", `{"store":true}`, []string{"store"}},
		{
			"多个按声明顺序",
			`{"extra_body":{},"prediction":{"type":"content"},"logit_bias":{"1":-1},"presence_penalty":0.1,"frequency_penalty":0.2,"store":false}`,
			[]string{"frequency_penalty", "presence_penalty", "logit_bias", "store", "prediction", "extra_body"},
		},
		// 已有类型字段由 translate 侧留痕，这里不得重复报
		{"已声明字段不重复", `{"logprobs":true,"top_logprobs":3,"seed":1,"user":"u","stream_options":{},"stop":["a"]}`, nil},
		{"空报文", ``, nil},
		{"非法报文", `{`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			got := ChatIgnoredFields([]byte(tc.raw))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ChatIgnoredFields = %v, want %v", got, tc.want)
			}
		})
	}
}

// image_url 的两种写法都要能解析。
func TestChatImageURL_BothShapes(t *testing.T) {
	cases := []struct{ raw, url, detail string }{
		{`{"url":"https://a/b.png","detail":"high"}`, "https://a/b.png", "high"},
		{`"https://a/b.png"`, "https://a/b.png", ""},
	}
	for _, tc := range cases {
		var part ChatContentPart
		raw := `{"type":"image_url","image_url":` + tc.raw + `}`
		if err := json.Unmarshal([]byte(raw), &part); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		if part.ImageURL == nil || part.ImageURL.URL != tc.url || part.ImageURL.Detail != tc.detail {
			t.Errorf("image_url = %+v, want %s/%s", part.ImageURL, tc.url, tc.detail)
		}
	}
}
