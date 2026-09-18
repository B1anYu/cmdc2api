package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/B1anYu/cmdc2api/internal/types"
)

func chatReq(t *testing.T, raw string) *types.ChatRequest {
	t.Helper()
	var req types.ChatRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("parse chat request: %v", err)
	}
	return &req
}

func mustChatToRequest(t *testing.T, req *types.ChatRequest) (*types.Request, []string) {
	t.Helper()
	out, warns, err := ChatToRequest(req)
	if err != nil {
		t.Fatalf("ChatToRequest: %v", err)
	}
	return out, warns
}

// mustChatToRequestBody 走 handler 实际使用的那条入口（带原始报文）。
func mustChatToRequestBody(t *testing.T, raw string) (*types.Request, []string) {
	t.Helper()
	var req types.ChatRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("parse chat request: %v", err)
	}
	out, warns, err := ChatToRequestBody([]byte(raw), &req)
	if err != nil {
		t.Fatalf("ChatToRequestBody: %v", err)
	}
	return out, warns
}

// system 与 developer 都并入 system，按出现顺序以空行连接。
func TestChatToRequest_SystemDeveloperMerged(t *testing.T) {
	out, _ := mustChatToRequest(t, chatReq(t, `{
		"model": "m",
		"messages": [
			{"role": "system", "content": "be brief"},
			{"role": "developer", "content": [{"type": "text", "text": "no markdown"}]},
			{"role": "system", "content": "reply in Chinese"},
			{"role": "user", "content": "hi"}
		]
	}`))
	if got, want := string(out.System), `"be brief\n\nno markdown\n\nreply in Chinese"`; got != want {
		t.Errorf("system = %s, want %s", got, want)
	}
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v, want one user message", out.Messages)
	}
}

// user content 的各种形态。
func TestChatToRequest_UserContentShapes(t *testing.T) {
	cases := []struct {
		name       string
		content    string
		wantBlocks []string // 期望的块类型序列
		wantWarn   string
	}{
		{"string", `"hello"`, []string{"text"}, ""},
		{"empty-string", `""`, nil, ""},
		{"null", `null`, nil, ""},
		{"text-parts", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, []string{"text", "text"}, ""},
		{"data-uri-image", `[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,QUJD"}}]`, []string{"text", "image"}, ""},
		{"https-image", `[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]`, []string{"image"}, ""},
		{"unsupported-part", `[{"type":"input_audio","input_audio":{"data":"x"}}]`, nil, `unsupported content part "input_audio" dropped`},
		{"bad-image-url", `[{"type":"image_url","image_url":{"url":"ftp://x/y.png"}}]`, nil, "unsupported url scheme dropped"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, warns := mustChatToRequest(t, chatReq(t,
				`{"model":"m","messages":[{"role":"user","content":`+tc.content+`}]}`))
			if len(out.Messages) == 0 {
				if len(tc.wantBlocks) > 0 {
					t.Fatalf("expected %v blocks, got no message", tc.wantBlocks)
				}
			} else {
				blocks := blocksOf(t, out.Messages[0])
				got := make([]string, len(blocks))
				for i, b := range blocks {
					got[i] = b.Type
				}
				if !equalStrings(got, tc.wantBlocks) {
					t.Fatalf("block types = %v, want %v", got, tc.wantBlocks)
				}
			}
			if tc.wantWarn != "" {
				assertWarnsContain(t, warns, tc.wantWarn)
			}
		})
	}
}

// data URI 解成 base64 source 并保留 media_type；http(s) 走 url source。
func TestChatToRequest_ImageSources(t *testing.T) {
	out, _ := mustChatToRequest(t, chatReq(t, `{
		"model": "m",
		"messages": [{"role": "user", "content": [
			{"type": "image_url", "image_url": {"url": "data:image/webp;base64,ZZZ"}},
			{"type": "image_url", "image_url": {"url": "https://x/y.png"}},
			{"type": "image_url", "image_url": "http://plain/string.png"}
		]}]
	}`))
	blocks := blocksOf(t, out.Messages[0])
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3", len(blocks))
	}
	if s := blocks[0].Source; s == nil || s.Type != "base64" || s.MediaType != "image/webp" || s.Data != "ZZZ" {
		t.Errorf("data URI source = %+v", s)
	}
	if s := blocks[1].Source; s == nil || s.Type != "url" || s.URL != "https://x/y.png" {
		t.Errorf("https source = %+v", s)
	}
	if s := blocks[2].Source; s == nil || s.Type != "url" || s.URL != "http://plain/string.png" {
		t.Errorf("bare-string source = %+v", s)
	}
}

// assistant：推理文本进 thinking 块，块次序固定 [thinking, text, tool-call]。
func TestChatToRequest_AssistantBlocks(t *testing.T) {
	out, warns := mustChatToRequest(t, chatReq(t, `{
		"model": "m",
		"messages": [
			{"role": "user", "content": "go"},
			{"role": "assistant", "content": "calling",
			 "reasoning_content": "let me think",
			 "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "ls", "arguments": "{\"path\":\".\"}"}},
				{"id": "call_2", "type": "function", "function": {"name": "noargs"}}
			 ]},
			{"role": "tool", "tool_call_id": "call_1", "content": "a.go"},
			{"role": "tool", "tool_call_id": "call_2", "content": "b"}
		]
	}`))
	assertPairing(t, out.Messages)

	asst := blocksOf(t, out.Messages[1])
	if got, want := []string{asst[0].Type, asst[1].Type, asst[2].Type, asst[3].Type}, []string{"thinking", "text", "tool_use", "tool_use"}; !equalStrings(got, want) {
		t.Fatalf("assistant block order = %v, want %v", got, want)
	}
	if asst[0].Thinking != "let me think" {
		t.Errorf("thinking = %q", asst[0].Thinking)
	}
	if asst[2].ID != "call_1" || asst[2].Name != "ls" || string(asst[2].Input) != `{"path":"."}` {
		t.Errorf("tool_use = %+v", asst[2])
	}
	// 空 arguments 兜底为 {}
	if string(asst[3].Input) != `{}` {
		t.Errorf("empty arguments should become {}, got %s", asst[3].Input)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warns: %v", warns)
	}
}

// 推理文本的三个来源与对象形态别名。
func TestChatToRequest_ReasoningAliases(t *testing.T) {
	cases := []struct{ name, msg, want string }{
		{"reasoning_content", `{"role":"assistant","content":"a","reasoning_content":"rc"}`, "rc"},
		{"reasoning-string", `{"role":"assistant","content":"a","reasoning":"rs"}`, "rs"},
		{"reason-object", `{"role":"assistant","content":"a","reason":{"content":"ro"}}`, "ro"},
		{"reason-text-key", `{"role":"assistant","content":"a","reason":{"text":"rt"}}`, "rt"},
		{"absent", `{"role":"assistant","content":"a"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := mustChatToRequest(t, chatReq(t, `{"model":"m","messages":[`+tc.msg+`]}`))
			blocks := blocksOf(t, out.Messages[0])
			if tc.want == "" {
				for _, b := range blocks {
					if b.Type == "thinking" {
						t.Fatalf("unexpected thinking block: %+v", b)
					}
				}
				return
			}
			if len(blocks) == 0 || blocks[0].Type != "thinking" || blocks[0].Thinking != tc.want {
				t.Fatalf("thinking = %+v, want %q", blocks, tc.want)
			}
		})
	}
}

// 工具调用缺 id / 缺函数名 被丢弃并留痕；legacy function_call 同样留痕丢弃。
// 保留下来的 call_2 有对应结果，因此不会被配对修复当成悬空调用清掉。
func TestChatToRequest_DroppedToolCalls(t *testing.T) {
	out, warns := mustChatToRequest(t, chatReq(t, `{
		"model": "m",
		"messages": [{"role": "assistant", "content": "x",
			"function_call": {"name": "legacy", "arguments": "{}"},
			"tool_calls": [
				{"type": "function", "function": {"name": "noid", "arguments": "{}"}},
				{"id": "call_1", "type": "function", "function": {"arguments": "{}"}},
				{"id": "call_2", "type": "function", "function": {"name": "ok", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "call_2", "content": "done"}]
	}`))
	blocks := blocksOf(t, out.Messages[0])
	if len(blocks) != 2 || blocks[1].Type != "tool_use" || blocks[1].ID != "call_2" {
		t.Fatalf("blocks = %+v, want [text, tool_use call_2]", blocks)
	}
	assertWarnsContain(t, warns, "legacy function_call dropped")
	assertWarnsContain(t, warns, "without id dropped")
	assertWarnsContain(t, warns, "without function name dropped")
}

// tool 消息：字符串/数组两种 content，legacy function 角色用 name 当 call_id，
// 缺 call_id 的整条丢弃。
func TestChatToRequest_ToolMessages(t *testing.T) {
	t.Run("shapes", func(t *testing.T) {
		out, warns := mustChatToRequest(t, chatReq(t, `{
			"model": "m",
			"messages": [
				{"role": "assistant", "content": "x", "tool_calls": [
					{"id": "call_1", "type": "function", "function": {"name": "a", "arguments": "{}"}},
					{"id": "call_2", "type": "function", "function": {"name": "b", "arguments": "{}"}},
					{"id": "call_3", "type": "function", "function": {"name": "c", "arguments": "{}"}}
				]},
				{"role": "tool", "tool_call_id": "call_1", "content": "plain"},
				{"role": "tool", "tool_call_id": "call_2", "content": [{"type": "text", "text": "part1"}, {"type": "text", "text": "part2"}]},
				{"role": "tool", "tool_call_id": "call_3", "content": null}
			]
		}`))
		assertPairing(t, out.Messages)
		// 三条结果被合并进紧随调用的一条 user 消息（配对修复的正常产物）
		results := blocksOf(t, out.Messages[len(out.Messages)-1])
		if len(results) != 3 {
			t.Fatalf("results = %d, want 3", len(results))
		}
		if string(results[0].Content) != `"plain"` {
			t.Errorf("string content = %s", results[0].Content)
		}
		var parts []types.Block
		if err := json.Unmarshal(results[1].Content, &parts); err != nil || len(parts) != 2 {
			t.Errorf("part content = %s (%v)", results[1].Content, err)
		}
		if string(results[2].Content) != `""` {
			t.Errorf("null content should become an empty string, got %s", results[2].Content)
		}
		if len(warns) != 0 {
			t.Errorf("unexpected warns: %v", warns)
		}
	})

	t.Run("legacy-function-role", func(t *testing.T) {
		out, _ := mustChatToRequest(t, chatReq(t, `{
			"model": "m",
			"messages": [
				{"role": "assistant", "content": "x", "tool_calls": [
					{"id": "call_1", "type": "function", "function": {"name": "call_1", "arguments": "{}"}}]},
				{"role": "function", "name": "call_1", "content": "legacy out"}
			]
		}`))
		assertPairing(t, out.Messages)
	})

	t.Run("missing-id-dropped", func(t *testing.T) {
		out, warns := mustChatToRequest(t, chatReq(t, `{
			"model": "m",
			"messages": [{"role": "user", "content": "q"}, {"role": "tool", "content": "out"}]
		}`))
		if len(out.Messages) != 1 {
			t.Fatalf("messages = %+v, want only the user message", out.Messages)
		}
		assertWarnsContain(t, warns, "missing tool_call_id")
	})
}

// 配对修复在归一化末尾生效：插队在调用与结果之间的 user 消息会把相邻性打断。
func TestChatToRequest_PairingRepairsInterleavedHistory(t *testing.T) {
	out, _ := mustChatToRequest(t, chatReq(t, `{
		"model": "m",
		"messages": [
			{"role": "user", "content": "list files"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "ls", "arguments": "{}"}}]},
			{"role": "user", "content": "actually also check tests"},
			{"role": "tool", "tool_call_id": "call_1", "content": "a.go"}
		]
	}`))
	assertPairing(t, out.Messages)
	if len(out.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (user, assistant, user)", len(out.Messages))
	}
	if !containsText(blocksOf(t, out.Messages[2]), "actually also check tests") {
		t.Error("interleaved user text lost")
	}
}

// tool_choice 全形态映射；工具全被丢弃时整个字段不设置。
func TestChatToRequest_ToolChoice(t *testing.T) {
	tools := `"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]`
	cases := []struct {
		name     string
		tc       string
		want     string
		wantWarn string
	}{
		{"auto", `"auto"`, `{"type":"auto"}`, ""},
		{"none", `"none"`, `{"type":"none"}`, ""},
		{"required", `"required"`, `{"type":"any"}`, ""},
		{"named-nested", `{"type":"function","function":{"name":"f"}}`, `{"type":"tool","name":"f"}`, ""},
		{"named-flat", `{"type":"function","name":"f"}`, `{"type":"tool","name":"f"}`, ""},
		{"object-auto", `{"type":"auto"}`, `{"type":"auto"}`, ""},
		{"allowed-tools", `{"type":"allowed_tools","tools":["f"]}`, `{"type":"auto"}`, "allowed_tools downgraded to auto"},
		{"unknown-string", `"whatever"`, `{"type":"auto"}`, "unknown tool_choice"},
		{"function-without-name", `{"type":"function"}`, `{"type":"auto"}`, "without name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, warns := mustChatToRequest(t, chatReq(t,
				`{"model":"m",`+tools+`,"tool_choice":`+tc.tc+`,"messages":[{"role":"user","content":"q"}]}`))
			if string(out.ToolChoice) != tc.want {
				t.Errorf("tool_choice = %s, want %s", out.ToolChoice, tc.want)
			}
			if tc.wantWarn != "" {
				assertWarnsContain(t, warns, tc.wantWarn)
			}
		})
	}

	t.Run("no-tools-means-no-tool-choice", func(t *testing.T) {
		out, warns := mustChatToRequest(t, chatReq(t,
			`{"model":"m","tool_choice":"required","messages":[{"role":"user","content":"q"}]}`))
		if len(out.ToolChoice) != 0 {
			t.Errorf("tool_choice = %s, want unset", out.ToolChoice)
		}
		assertWarnsContain(t, warns, "no tool survived conversion")
	})
}

// 工具声明：type 缺省按 function；非 function 类型、无名字的丢弃；strict 留痕。
func TestChatToRequest_Tools(t *testing.T) {
	out, warns := mustChatToRequest(t, chatReq(t, `{
		"model": "m",
		"messages": [{"role": "user", "content": "q"}],
		"tools": [
			{"function": {"name": "no_type", "parameters": {"type": "object"}}},
			{"type": "function", "function": {"name": "strict_one", "strict": true, "parameters": {"type": "object"}}},
			{"type": "web_search"},
			{"type": "function", "function": {"parameters": {"type": "object"}}}
		]
	}`))
	if len(out.Tools) != 2 || out.Tools[0].Name != "no_type" || out.Tools[1].Name != "strict_one" {
		t.Fatalf("tools = %+v", out.Tools)
	}
	assertWarnsContain(t, warns, `unsupported tool type "web_search" dropped`)
	assertWarnsContain(t, warns, "strict schema flag dropped")
	assertWarnsContain(t, warns, "without name dropped")
}

// max_completion_tokens 优先于 max_tokens；都没有时留给 BuildCcRequest 兜底。
func TestChatToRequest_MaxTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"max_tokens", `"max_tokens":100`, 100},
		{"max_completion_tokens", `"max_completion_tokens":200`, 200},
		{"both", `"max_tokens":100,"max_completion_tokens":200`, 200},
		{"neither", ``, 0},
		{"zero", `"max_tokens":0`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":"q"}]`
			if tc.body != "" {
				body += "," + tc.body
			}
			body += "}"
			out, _ := mustChatToRequest(t, chatReq(t, body))
			if out.MaxTokens != tc.want {
				t.Errorf("max_tokens = %d, want %d", out.MaxTokens, tc.want)
			}
		})
	}
}

// stop 的各种形态：字符串、数组、空白项过滤、过滤后为空则不下发。
// stop 无上游载体：不再映射进 out.StopSequences（BuildCcRequest 本就不消费该字段），
// 而是并入安全丢弃留痕——客户端以为会在指定序列处截断，实际不会，必须让它知道。
func TestChatToRequest_StopDropped(t *testing.T) {
	cases := []struct {
		name     string
		field    string // 追加到请求体的 stop 字段；空表示客户端没发该字段
		wantWarn bool
	}{
		{"absent", ``, false},
		{"string", `"stop":"END",`, true},
		{"array", `"stop":["a","b"],`, true},
		{"all-blank", `"stop":["  "],`, true},
		{"empty-array", `"stop":[],`, true},
		{"null", `"stop":null,`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, warns := mustChatToRequest(t, chatReq(t,
				`{"model":"m",`+tc.field+`"messages":[{"role":"user","content":"q"}]}`))
			if len(out.StopSequences) != 0 {
				t.Errorf("stop_sequences = %v, want 空（stop 不再映射）", out.StopSequences)
			}
			if tc.wantWarn {
				assertWarnsContain(t, warns, "stop ignored (upstream has no stop-sequence capability)")
				return
			}
			for _, w := range warns {
				if strings.Contains(w, "stop ignored") {
					t.Errorf("未发送 stop 却收到留痕: %v", warns)
				}
			}
		})
	}
}

// reasoning_effort → adaptive thinking（档位收敛）。
func TestChatToRequest_ReasoningEffort(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"low", `"low"`, "low"},
		{"medium", `"medium"`, "medium"},
		{"high", `"high"`, "high"},
		{"minimal", `"minimal"`, "low"},
		{"xhigh", `"xhigh"`, "high"},
		{"max", `"max"`, "high"},
		{"object", `{"effort":"medium"}`, "medium"},
		{"none", `"none"`, ""},
		{"empty", `""`, ""},
		{"absent", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":"q"}]`
			if tc.raw != "" {
				body += `,"reasoning_effort":` + tc.raw
			}
			body += "}"
			out, _ := mustChatToRequest(t, chatReq(t, body))
			if tc.want == "" {
				if len(out.Thinking) != 0 {
					t.Fatalf("thinking = %s, want unset", out.Thinking)
				}
				return
			}
			var th struct {
				Type   string `json:"type"`
				Effort string `json:"effort"`
			}
			if err := json.Unmarshal(out.Thinking, &th); err != nil {
				t.Fatalf("thinking = %s: %v", out.Thinking, err)
			}
			if th.Type != "adaptive" || th.Effort != tc.want {
				t.Errorf("thinking = %+v, want adaptive/%s", th, tc.want)
			}
		})
	}
}

// n>1 显式拒绝（上游无多候选生成）。
func TestChatToRequest_N(t *testing.T) {
	for _, raw := range []string{`1`, `0`, `null`} {
		if _, _, err := ChatToRequest(chatReq(t,
			`{"model":"m","n":`+raw+`,"messages":[{"role":"user","content":"q"}]}`)); err != nil {
			t.Errorf("n=%s should be accepted, got %v", raw, err)
		}
	}
	_, _, err := ChatToRequest(chatReq(t, `{"model":"m","n":3,"messages":[{"role":"user","content":"q"}]}`))
	if err == nil || err.Error() != "only n=1 is supported" {
		t.Fatalf("n=3 error = %v, want ErrChatUnsupportedN", err)
	}
}

// parallel_tool_calls=false 是有上游载体的行为性约束（并入 tool_choice.disable_parallel_tool_use），
// 不再是安全丢弃字段；显式为 true 与未声明一律不动（上游默认即允许并行）。
func TestChatToRequest_ParallelToolCalls(t *testing.T) {
	tools := `"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]`
	cases := []struct {
		name     string
		fields   string // 追加到请求体的顶层字段（含结尾逗号则自带）
		want     string // 期望的 tool_choice 原文；"" 表示整个字段不设置
		wantWarn bool   // 是否必须保留丢弃留痕
	}{
		{"false-with-tools", `"parallel_tool_calls":false,` + tools,
			`{"type":"auto","disable_parallel_tool_use":true}`, false},
		{"false-without-tools", `"parallel_tool_calls":false`, "", true},
		{"false-with-object-tool-choice",
			`"parallel_tool_calls":false,"tool_choice":{"type":"function","function":{"name":"f"}},` + tools,
			`{"type":"tool","name":"f","disable_parallel_tool_use":true}`, false},
		{"false-with-string-tool-choice", `"parallel_tool_calls":false,"tool_choice":"required",` + tools,
			`{"type":"any","disable_parallel_tool_use":true}`, false},
		{"true-with-tools", `"parallel_tool_calls":true,` + tools, "", true},
		{"absent-with-tools", tools, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, warns := mustChatToRequest(t, chatReq(t,
				`{"model":"m",`+tc.fields+`,"messages":[{"role":"user","content":"q"}]}`))
			if string(out.ToolChoice) != tc.want {
				t.Errorf("tool_choice = %s, want %s", out.ToolChoice, tc.want)
			}
			hasWarn := false
			for _, w := range warns {
				if strings.Contains(w, "parallel_tool_calls ignored") {
					hasWarn = true
				}
			}
			if hasWarn != tc.wantWarn {
				t.Errorf("丢弃留痕 = %v, want %v\nwarns: %v", hasWarn, tc.wantWarn, warns)
			}
		})
	}

	// 端到端：标志必须经既有信封构造真的下发（上游并行开关只有这一条表达路径）
	t.Run("reaches-upstream-envelope", func(t *testing.T) {
		out, _ := mustChatToRequest(t, chatReq(t,
			`{"model":"m",`+tools+`,"parallel_tool_calls":false,"messages":[{"role":"user","content":"q"}]}`))
		cc, _ := BuildCcRequest(out, testOpts())
		if cc.Params.ParallelToolCalls == nil || *cc.Params.ParallelToolCalls {
			t.Errorf("params.parallel_tool_calls = %v, want false", cc.Params.ParallelToolCalls)
		}
		if cc.Params.ToolChoice == nil || cc.Params.ToolChoice.Type != "auto" {
			t.Errorf("params.tool_choice = %+v, want {type:auto}", cc.Params.ToolChoice)
		}
	})
}

// 安全丢弃清单必须逐项留痕（info: 前缀为良性）。
func TestChatToRequest_DroppedFieldsWarned(t *testing.T) {
	cases := []struct {
		name     string
		field    string
		wantWarn string
		infoOnly bool
	}{
		{"stop", `"stop":["END"]`, "stop ignored", false},
		{"response_format", `"response_format":{"type":"json_object"}`, "response_format ignored", false},
		{"parallel_tool_calls", `"parallel_tool_calls":false`, "parallel_tool_calls ignored", false},
		{"stream_options", `"stream_options":{"include_usage":true}`, "info: stream_options ignored", true},
		{"seed", `"seed":7`, "info: seed ignored", true},
		{"user", `"user":"u1"`, "info: user ignored", true},
		{"logprobs", `"logprobs":true`, "info: logprobs ignored", true},
		{"top_logprobs", `"top_logprobs":5`, "info: top_logprobs ignored", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, warns := mustChatToRequest(t, chatReq(t,
				`{"model":"m","messages":[{"role":"user","content":"q"}],`+tc.field+`}`))
			assertWarnsContain(t, warns, tc.wantWarn)
			for _, w := range warns {
				if strings.Contains(w, strings.TrimPrefix(tc.wantWarn, "info: ")) && !tc.infoOnly {
					continue
				}
			}
		})
	}
}

// 无类型字段靠原始报文探测留痕：这些字段在 ChatRequest 里连类型都没有，
// 只有 raw 能证明客户端确实发过（与 Responses 侧的 ResponsesIgnoredFields 对等）。
func TestChatToRequestBody_IgnoredFieldsWarned(t *testing.T) {
	_, warns := mustChatToRequestBody(t, `{
		"model": "m",
		"frequency_penalty": 0.5,
		"presence_penalty": 0.1,
		"logit_bias": {"50256": -100},
		"store": true,
		"prediction": {"type": "content", "content": "x"},
		"extra_body": {"foo": 1},
		"messages": [{"role": "user", "content": "q"}]
	}`)
	for _, want := range []string{
		"frequency_penalty ignored", "presence_penalty ignored", "logit_bias ignored",
		"store ignored", "prediction ignored", "extra_body ignored",
	} {
		assertWarnsContain(t, warns, want)
	}
}

// 显式 store:false 与代理行为完全一致（OpenAI 两 API 的默认值即 true），
// 不得产生噪音留痕；条件与 Responses 侧的 store:false 用例口径对齐。
func TestChatToRequestBody_StoreFalseNotWarned(t *testing.T) {
	_, warns := mustChatToRequestBody(t, `{"model":"m","store":false,"messages":[{"role":"user","content":"q"}]}`)
	if len(warns) != 0 {
		t.Errorf("warns = %v, want 空（store:false 与无服务端状态的行为一致）", warns)
	}
}

// 已有显式留痕的字段不得因探测而重复报一次（会被客户端误读成两个问题）。
func TestChatToRequestBody_NoDuplicateWarns(t *testing.T) {
	_, warns := mustChatToRequestBody(t, `{
		"model": "m",
		"logprobs": true,
		"top_logprobs": 3,
		"seed": 7,
		"user": "u1",
		"stream_options": {"include_usage": true},
		"messages": [{"role": "user", "content": "q"}]
	}`)
	counts := map[string]int{}
	for _, w := range warns {
		counts[w]++
	}
	for w, n := range counts {
		if n > 1 {
			t.Errorf("留痕重复 %d 次: %s", n, w)
		}
	}
	if len(warns) != 5 {
		t.Errorf("warns = %v, want 5 条（logprobs/top_logprobs/seed/user/stream_options）", warns)
	}
}

// 兼容入口（raw 为 nil）不得探测无类型字段：没有报文就没有证据。
func TestChatToRequest_NilRawSkipsIgnoredFields(t *testing.T) {
	_, warns := mustChatToRequest(t, chatReq(t,
		`{"model":"m","store":true,"frequency_penalty":0.5,"messages":[{"role":"user","content":"q"}]}`))
	if len(warns) != 0 {
		t.Errorf("warns = %v, want 空（nil raw 不探测）", warns)
	}
}

// 原始报文非法（调用方已先报解析错误）时同样不探测。
func TestChatToRequestBody_InvalidRawSkipsIgnoredFields(t *testing.T) {
	var req types.ChatRequest
	_, warns, err := ChatToRequestBody([]byte(`{`), &req)
	if err != nil {
		t.Fatalf("ChatToRequestBody: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("warns = %v, want 空（非法报文不探测）", warns)
	}
}

// 归一化产物必须能直接进既有信封构造（端到端组合性验证）。
func TestChatToRequest_ComposesWithBuildCcRequest(t *testing.T) {
	req, _ := mustChatToRequest(t, chatReq(t, `{
		"model": "gpt-4o",
		"temperature": 0.2,
		"top_p": 0.9,
		"max_tokens": 512,
		"tools": [{"type": "function", "function": {"name": "get", "parameters": {"type": "object", "properties": {}}}}],
		"tool_choice": "required",
		"reasoning_effort": "high",
		"messages": [
			{"role": "system", "content": "sys"},
			{"role": "user", "content": "hi"}
		]
	}`))
	cc, warns := BuildCcRequest(req, testOpts())

	if cc.Params.Model != "gpt-4o" || cc.Params.MaxTokens != 512 {
		t.Errorf("model/max_tokens = %s/%d", cc.Params.Model, cc.Params.MaxTokens)
	}
	if cc.Params.System != "sys" {
		t.Errorf("system = %q", cc.Params.System)
	}
	if cc.Params.ReasoningEffort != "high" {
		t.Errorf("reasoning_effort = %q", cc.Params.ReasoningEffort)
	}
	if cc.Params.ToolChoice == nil || cc.Params.ToolChoice.Type != "any" {
		t.Errorf("tool_choice = %+v", cc.Params.ToolChoice)
	}
	if len(cc.Params.Tools) != 1 || cc.Params.Tools[0].Name != "get" {
		t.Errorf("tools = %+v", cc.Params.Tools)
	}
	// stop 属于安全丢弃字段（上游无 stop 能力），归一化阶段既不映射也不消费，
	// 因此信封构造不会再收到 stop_sequences，warns 为空。
	if len(warns) != 0 {
		t.Errorf("unexpected warns: %v", warns)
	}
}

// 未知角色丢弃并留痕。
func TestChatToRequest_UnknownRole(t *testing.T) {
	out, warns := mustChatToRequest(t, chatReq(t,
		`{"model":"m","messages":[{"role":"tool_result","content":"x"},{"role":"user","content":"q"}]}`))
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %+v", out.Messages)
	}
	assertWarnsContain(t, warns, `unsupported role "tool_result" dropped`)
}

// content 既不是字符串也不是数组（客户端 bug）：整条消息丢弃并留痕，
// 不能因为一条坏消息把整轮打成解析失败。
func TestChatToRequest_UnparsableContent(t *testing.T) {
	cases := []struct{ name, msg string }{
		{"user", `{"role":"user","content":123}`},
		{"assistant", `{"role":"assistant","content":123}`},
		{"tool", `{"role":"tool","tool_call_id":"call_1","content":123}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, warns := mustChatToRequest(t, chatReq(t, `{"model":"m","messages":[`+tc.msg+`]}`))
			if len(out.Messages) != 0 {
				t.Fatalf("messages = %+v, want none", out.Messages)
			}
			assertWarnsContain(t, warns, "neither string nor part array")
		})
	}
}

// 图片 URL 的边界形态：缺 url、坏 data URI、非 base64 编码都留痕丢弃。
func TestChatToRequest_ImageEdgeCases(t *testing.T) {
	out, warns := mustChatToRequest(t, chatReq(t, `{
		"model": "m",
		"messages": [{"role": "user", "content": [
			{"type": "image_url", "image_url": {"detail": "high"}},
			{"type": "image_url", "image_url": {"url": "data:image/png;base64"}},
			{"type": "image_url", "image_url": {"url": "data:image/png;utf8,abc"}},
			{"type": "text", "text": "kept"}
		]}]
	}`))
	blocks := blocksOf(t, out.Messages[0])
	if len(blocks) != 1 || blocks[0].Text != "kept" {
		t.Fatalf("blocks = %+v, want only the text block", blocks)
	}
	assertWarnsContain(t, warns, "image_url without url dropped")
	assertWarnsContain(t, warns, "unsupported url scheme dropped")
}

// tool_result 的 part 形态：文本与图片保留（图片交给 convertUser 搬迁），
// 未知 part 留痕丢弃。
func TestChatToRequest_ToolResultParts(t *testing.T) {
	out, warns := mustChatToRequest(t, chatReq(t, `{
		"model": "m",
		"messages": [
			{"role": "assistant", "content": "x", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "f", "arguments": "{}"}}]},
			{"role": "tool", "tool_call_id": "call_1", "content": [
				{"type": "text", "text": "out"},
				{"type": "image_url", "image_url": {"url": "https://x/y.png"}},
				{"type": "file", "file": {"file_id": "f1"}}
			]}
		]
	}`))
	assertPairing(t, out.Messages)
	var blocks []types.Block
	if err := json.Unmarshal(blocksOf(t, out.Messages[1])[0].Content, &blocks); err != nil {
		t.Fatalf("tool_result content: %v", err)
	}
	if len(blocks) != 2 || blocks[0].Type != "text" || blocks[1].Type != "image" {
		t.Fatalf("tool_result blocks = %+v", blocks)
	}
	assertWarnsContain(t, warns, `unsupported content part "file" dropped`)
}

// tool_choice 完全无法解析时降级为 auto（不能让一个坏字段打挂整轮）。
func TestChatToRequest_ToolChoiceUnparsable(t *testing.T) {
	out, warns := mustChatToRequest(t, chatReq(t,
		`{"model":"m","tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":123,"messages":[{"role":"user","content":"q"}]}`))
	if string(out.ToolChoice) != `{"type":"auto"}` {
		t.Errorf("tool_choice = %s, want auto", out.ToolChoice)
	}
	assertWarnsContain(t, warns, "unparsable tool_choice")
}

// 未知 reasoning_effort 档位：留痕且不设置 thinking（不猜测上游语义）。
func TestChatToRequest_ReasoningEffortUnknown(t *testing.T) {
	out, warns := mustChatToRequest(t, chatReq(t,
		`{"model":"m","reasoning_effort":"turbo","messages":[{"role":"user","content":"q"}]}`))
	if len(out.Thinking) != 0 {
		t.Errorf("thinking = %s, want unset", out.Thinking)
	}
	assertWarnsContain(t, warns, `unknown reasoning_effort "turbo"`)
}
