package translate

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/B1anYu/cmdc2api/internal/types"
)

func parseReq(t *testing.T, raw string) *types.Request {
	t.Helper()
	var req types.Request
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("parse request: %v", err)
	}
	return &req
}

func testOpts() BuildOpts {
	return BuildOpts{
		Now:                time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC),
		NodeVersion:        "v22.21.0",
		WorkingDir:         `C:\Users\dev\projects\app-a3f2`,
		AssistantReasoning: true,
	}
}

func TestBuildCcRequest_SystemArrayToStringAndCacheMarker(t *testing.T) {
	req := parseReq(t, `{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1000,
		"system": [
			{"type": "text", "text": "You are helpful.", "cache_control": {"type": "ephemeral"}},
			{"type": "text", "text": "Be concise."}
		],
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]
	}`)
	cc, _ := BuildCcRequest(req, testOpts())

	if got, want := cc.Params.System, "You are helpful.\nBe concise."; got != want {
		t.Errorf("system = %q, want %q", got, want)
	}
	// system 上的 cache_control 无法落在字符串上 → 应在首个 user 消息 text part 合成标记
	if len(cc.Params.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(cc.Params.Messages))
	}
	m := cc.Params.Messages[0]
	if m.Role != "user" || len(m.Content) != 1 || m.Content[0].Type != "text" {
		t.Fatalf("unexpected first message: %+v", m)
	}
	if cc := m.Content[0].CacheControl; cc == nil || cc.Type != "ephemeral" {
		t.Errorf("synthesized cache_control missing: %+v", m.Content[0])
	}
	// 信封形状
	if cc.Params.Stream != true {
		t.Error("params.stream must be true")
	}
	var raw map[string]any
	b, _ := json.Marshal(cc)
	_ = json.Unmarshal(b, &raw)
	if _, ok := raw["memory"]; !ok {
		t.Error("memory field must be present (null)")
	}
	params := raw["params"].(map[string]any)
	if v, ok := params["system"]; !ok {
		t.Error("params.system must be present")
	} else if _, isStr := v.(string); !isStr {
		t.Errorf("params.system must be string, got %T", v)
	}
	if cfg := raw["config"].(map[string]any); cfg["environment"] != "win32-x64, Node.js v22.21.0" {
		t.Errorf("environment = %v", cfg["environment"])
	}
}

// F3：客户端 part 级 cache_control 的 ttl 必须原样到达出站信封（绝不覆写、绝不合成）。
func TestBuildCcRequest_CacheControlTTLPreservedVerbatim(t *testing.T) {
	req := parseReq(t, `{
		"max_tokens": 10,
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "a"},
			{"type": "text", "text": "b", "cache_control": {"type": "ephemeral", "ttl": "1h"}}
		]}]
	}`)
	cc, _ := BuildCcRequest(req, testOpts())
	part := cc.Params.Messages[0].Content[1]
	if part.CacheControl == nil || part.CacheControl.Type != "ephemeral" || part.CacheControl.TTL != "1h" {
		t.Fatalf("ttl must survive verbatim into the envelope: %+v", part.CacheControl)
	}
	b, _ := json.Marshal(part.CacheControl)
	if string(b) != `{"type":"ephemeral","ttl":"1h"}` {
		t.Errorf("cache_control wire shape = %s, want {\"type\":\"ephemeral\",\"ttl\":\"1h\"}", b)
	}
	// 端到端：整个信封的线格式里 ttl 必须原样出现（F3 审计的出站构造点覆盖）
	whole, err := json.Marshal(cc)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if !strings.Contains(string(whole), `"cache_control":{"type":"ephemeral","ttl":"1h"}`) {
		t.Errorf("ttl missing from serialized envelope: %s", whole)
	}
}

// F3：没有 ttl 时不注入 ttl（不合成）；type 缺失时补 "ephemeral"。
func TestBuildCcRequest_CacheControlNoTTLSynthesized(t *testing.T) {
	req := parseReq(t, `{
		"max_tokens": 10,
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "a", "cache_control": {"type": "ephemeral"}},
			{"type": "text", "text": "b", "cache_control": {}}
		]}]
	}`)
	cc, _ := BuildCcRequest(req, testOpts())
	for i, want := range []string{`{"type":"ephemeral"}`, `{"type":"ephemeral"}`} {
		part := cc.Params.Messages[0].Content[i]
		if part.CacheControl == nil {
			t.Fatalf("part %d lost its cache_control: %+v", i, part)
		}
		b, _ := json.Marshal(part.CacheControl)
		if string(b) != want {
			t.Errorf("part %d cache_control = %s, want %s (ttl must NOT be added)", i, b, want)
		}
	}
}

func TestBuildCcRequest_ImagesAndToolRoundTrip(t *testing.T) {
	req := parseReq(t, `{
		"max_tokens": 100,
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "look"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/jpeg", "data": "QUJD"}}
			]},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_1", "name": "screenshot", "input": {"x": 1}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": [
					{"type": "text", "text": "result text"},
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "WFla"}}
				]},
				{"type": "text", "text": "and this"}
			]}
		]
	}`)
	cc, warns := BuildCcRequest(req, testOpts())
	if len(warns) != 0 {
		t.Logf("warnings: %v", warns)
	}
	msgs := cc.Params.Messages
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4 (user/assistant/tool/relocated): %+v", len(msgs), msgs)
	}
	// user: text + image
	if p := msgs[0].Content[1]; p.Type != "image" || p.Image != "data:image/jpeg;base64,QUJD" {
		t.Errorf("user image part wrong: %+v", p)
	}
	// assistant: tool-call，id 恒等透传，input 保持对象
	tc := msgs[1].Content[0]
	if tc.Type != "tool-call" || tc.ToolCallID != "toolu_1" || tc.ToolName != "screenshot" {
		t.Errorf("tool-call part wrong: %+v", tc)
	}
	// 字节级保真：input 原样透传（含原始空白）
	if string(tc.Input) != `{"x": 1}` {
		t.Errorf("tool-call input = %s", tc.Input)
	}
	// tool: tool-result 只收文本
	tr := msgs[2].Content[0]
	if tr.Type != "tool-result" || tr.ToolCallID != "toolu_1" || tr.ToolName != "screenshot" {
		t.Errorf("tool-result part wrong: %+v", tr)
	}
	if tr.Output == nil || tr.Output.Value != "result text" {
		t.Errorf("tool-result output wrong: %+v", tr.Output)
	}
	// 图片搬迁：并入 tool-result 之后的 user 段（段首标记 + 图片 + 原文本）
	reloc := msgs[3]
	if reloc.Role != "user" || len(reloc.Content) != 3 {
		t.Fatalf("relocated user segment wrong: %+v", reloc)
	}
	if reloc.Content[0].Type != "text" || reloc.Content[0].Text != "[Tool output media for call toolu_1]" {
		t.Errorf("relocation marker wrong: %+v", reloc.Content[0])
	}
	if reloc.Content[1].Type != "image" || reloc.Content[1].Image != "data:image/png;base64,WFla" {
		t.Errorf("relocated image wrong: %+v", reloc.Content[1])
	}
	if reloc.Content[2].Type != "text" || reloc.Content[2].Text != "and this" {
		t.Errorf("relocated segment must keep the trailing text: %+v", reloc.Content[2])
	}
}

func TestBuildCcRequest_OrphanToolResultDropped(t *testing.T) {
	req := parseReq(t, `{
		"max_tokens": 10,
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_missing", "content": "stale"}]}
		]
	}`)
	cc, warns := BuildCcRequest(req, testOpts())
	for _, m := range cc.Params.Messages {
		for _, p := range m.Content {
			if p.Type == "tool-result" {
				t.Errorf("orphan tool-result should be dropped: %+v", p)
			}
		}
	}
	found := false
	for _, w := range warns {
		if contains(w, "orphan tool_result") {
			found = true
		}
	}
	if !found {
		t.Errorf("orphan drop should be warned, got %v", warns)
	}
}

func TestBuildCcRequest_ToolUseMissingInputBecomesEmptyObject(t *testing.T) {
	req := parseReq(t, `{
		"max_tokens": 10,
		"messages": [
			{"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "f"}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}]}
		]
	}`)
	cc, _ := BuildCcRequest(req, testOpts())
	if got := string(cc.Params.Messages[0].Content[0].Input); got != "{}" {
		t.Errorf("missing tool input = %s, want {}", got)
	}
}

func TestBuildCcRequest_ThinkingEffortThresholds(t *testing.T) {
	cases := []struct {
		thinking string
		want     string
	}{
		{`{"type": "enabled", "budget_tokens": 12000}`, "high"},
		{`{"type": "enabled", "budget_tokens": 10000}`, "high"},
		{`{"type": "enabled", "budget_tokens": 6000}`, "medium"},
		{`{"type": "enabled", "budget_tokens": 5000}`, "medium"},
		{`{"type": "enabled", "budget_tokens": 2500}`, "low"},
		{`{"type": "disabled"}`, ""},
		{`{"type": "adaptive"}`, "medium"},
		{`{"type": "adaptive", "effort": "high"}`, "high"},
	}
	for _, c := range cases {
		req := parseReq(t, `{"max_tokens": 10, "thinking": `+c.thinking+`,
			"messages": [{"role": "user", "content": "hi"}]}`)
		cc, _ := BuildCcRequest(req, testOpts())
		if cc.Params.ReasoningEffort != c.want {
			t.Errorf("thinking %s → effort %q, want %q", c.thinking, cc.Params.ReasoningEffort, c.want)
		}
	}
}

func TestBuildCcRequest_ToolChoiceAndParallel(t *testing.T) {
	cases := []struct {
		raw      string
		wantType string
		wantName string
		parallel *bool // nil = 不设
	}{
		{`{"type": "any"}`, "any", "", nil},
		{`{"type": "tool", "name": "X"}`, "tool", "X", nil},
		{`{"type": "none"}`, "none", "", nil},
		{`{"type": "weird"}`, "auto", "", nil},
		{`{"type": "auto", "disable_parallel_tool_use": true}`, "auto", "", boolPtr(false)},
	}
	for _, c := range cases {
		req := parseReq(t, `{"max_tokens": 10, "tool_choice": `+c.raw+`,
			"messages": [{"role": "user", "content": "hi"}]}`)
		cc, _ := BuildCcRequest(req, testOpts())
		tc := cc.Params.ToolChoice
		if tc == nil || tc.Type != c.wantType || tc.Name != c.wantName {
			t.Errorf("tool_choice %s → %+v, want type=%s name=%s", c.raw, tc, c.wantType, c.wantName)
		}
		if (cc.Params.ParallelToolCalls == nil) != (c.parallel == nil) ||
			(c.parallel != nil && *cc.Params.ParallelToolCalls != *c.parallel) {
			t.Errorf("tool_choice %s → parallel=%v, want %v", c.raw, cc.Params.ParallelToolCalls, c.parallel)
		}
	}
}

// F2：未知 tool_choice type 静默降级为 auto 时必须留痕（该分支仅 Anthropic 原生入站可达）。
func TestBuildCcRequest_ToolChoiceUnknownTypeWarned(t *testing.T) {
	req := parseReq(t, `{"max_tokens": 10, "tool_choice": {"type": "weird"},
		"messages": [{"role": "user", "content": "hi"}]}`)
	cc, warns := BuildCcRequest(req, testOpts())
	if tc := cc.Params.ToolChoice; tc == nil || tc.Type != "auto" {
		t.Fatalf("unknown tool_choice must still fall back to auto: %+v", cc.Params.ToolChoice)
	}
	w := warnContaining(t, warns, `tool_choice type "weird"`)
	if !strings.Contains(w, `{"type":"auto"}`) || !strings.Contains(w, "any tool") {
		t.Errorf("warn must say what we did and what it means for the model, got %q", w)
	}
	if !strings.Contains(w, "Anthropic Messages inbound path") {
		t.Errorf("warn must scope the branch to the Anthropic-native inbound path, got %q", w)
	}
}

// F2：{type:"tool"} 缺 name 会产出无名 {"type":"tool"}，必须留痕。
func TestBuildCcRequest_ToolChoiceToolWithoutNameWarned(t *testing.T) {
	req := parseReq(t, `{"max_tokens": 10, "tool_choice": {"type": "tool"},
		"messages": [{"role": "user", "content": "hi"}]}`)
	cc, warns := BuildCcRequest(req, testOpts())
	tc := cc.Params.ToolChoice
	if tc == nil || tc.Type != "tool" || tc.Name != "" {
		t.Fatalf("behavior unchanged: want nameless {type:tool}, got %+v", tc)
	}
	b, _ := json.Marshal(tc)
	if string(b) != `{"type":"tool"}` {
		t.Errorf("wire shape = %s, want {\"type\":\"tool\"} (omitempty drops the empty name)", b)
	}
	w := warnContaining(t, warns, `tool_choice {"type":"tool"} carries no "name"`)
	if !strings.Contains(w, "cannot resolve to any tool") {
		t.Errorf("warn must state the consequence, got %q", w)
	}
	// 带 name 的同一形态不得留痕（避免 over-warn）
	named := parseReq(t, `{"max_tokens": 10, "tool_choice": {"type": "tool", "name": "f"},
		"messages": [{"role": "user", "content": "hi"}]}`)
	_, namedWarns := BuildCcRequest(named, testOpts())
	for _, w := range namedWarns {
		if strings.Contains(w, "tool_choice") {
			t.Errorf("valid tool_choice must not warn, got %q", w)
		}
	}
}

func TestBuildCcRequest_MaxTokensClamp(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{{0, 64000}, {5000, 5000}, {300000, 200000}}
	for _, c := range cases {
		req := parseReq(t, `{"max_tokens": `+strconv.Itoa(c.in)+`, "messages": [{"role":"user","content":"hi"}]}`)
		cc, _ := BuildCcRequest(req, testOpts())
		if cc.Params.MaxTokens != c.want {
			t.Errorf("max_tokens %d → %d, want %d", c.in, cc.Params.MaxTokens, c.want)
		}
	}
}

// F4：200000 上限保留（不提高、不改为透传），但触发钳制时必须留痕，附请求值与钳制后的值。
func TestBuildCcRequest_MaxTokensClampWarned(t *testing.T) {
	req := parseReq(t, `{"max_tokens": 300000, "messages": [{"role":"user","content":"hi"}]}`)
	cc, warns := BuildCcRequest(req, testOpts())
	if cc.Params.MaxTokens != 200000 {
		t.Fatalf("ceiling must be kept: max_tokens = %d, want 200000", cc.Params.MaxTokens)
	}
	w := warnContaining(t, warns, "max_tokens 300000")
	if !strings.Contains(w, "clamped to 200000") {
		t.Errorf("warn must carry the clamped value, got %q", w)
	}
	if !strings.Contains(w, "earlier than the client asked for") {
		t.Errorf("warn must state the consequence, got %q", w)
	}
	// 未超过上限时不产生该 warn
	ok := parseReq(t, `{"max_tokens": 5000, "messages": [{"role":"user","content":"hi"}]}`)
	_, okWarns := BuildCcRequest(ok, testOpts())
	for _, w := range okWarns {
		if strings.Contains(w, "clamped to 200000") {
			t.Errorf("no clamp happened, but warn fired: %q", w)
		}
	}
}

func TestBuildCcRequest_AssistantThinkingDefaultKeptAndOrdered(t *testing.T) {
	raw := `{"max_tokens": 10, "messages": [
		{"role": "assistant", "content": [
			{"type": "text", "text": "answer"},
			{"type": "tool_use", "id": "call_1", "name": "testTool", "input": {}},
			{"type": "thinking", "thinking": "hmm", "signature": "sig"}
		]},
		{"role": "user", "content": "next"}
	]}`
	// 默认 AssistantReasoning: true
	cc, _ := BuildCcRequest(parseReq(t, raw), testOpts())
	content := cc.Params.Messages[0].Content
	if len(content) != 3 {
		t.Fatalf("expected 3 parts, got %d", len(content))
	}
	// 严格遵循 [reasoning, text, tool-call] 次序
	if content[0].Type != "reasoning" || content[0].Text != "hmm" {
		t.Errorf("part 0: want reasoning 'hmm', got %+v", content[0])
	}
	if content[1].Type != "text" || content[1].Text != "answer" {
		t.Errorf("part 1: want text 'answer', got %+v", content[1])
	}
	if content[2].Type != "tool-call" || content[2].ToolCallID != "call_1" {
		t.Errorf("part 2: want tool-call 'call_1', got %+v", content[2])
	}

	// 显式关闭 AssistantReasoning: false 时丢弃思考块
	opts := testOpts()
	opts.AssistantReasoning = false
	cc2, _ := BuildCcRequest(parseReq(t, raw), opts)
	for _, p := range cc2.Params.Messages[0].Content {
		if p.Type == "reasoning" {
			t.Error("thinking should be dropped when AssistantReasoning is false")
		}
	}
}

func TestBuildCcRequest_AssistantReasoningContentMessageLevel(t *testing.T) {
	raw := `{"max_tokens": 10, "messages": [
		{"role": "assistant", "content": "hello world", "reasoning_content": "deep thinking"},
		{"role": "user", "content": "next"}
	]}`
	cc, _ := BuildCcRequest(parseReq(t, raw), testOpts())
	content := cc.Params.Messages[0].Content
	if len(content) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(content))
	}
	if content[0].Type != "reasoning" || content[0].Text != "deep thinking" {
		t.Errorf("part 0: want reasoning 'deep thinking', got %+v", content[0])
	}
	if content[1].Type != "text" || content[1].Text != "hello world" {
		t.Errorf("part 1: want text 'hello world', got %+v", content[1])
	}
}

func TestBuildCcRequest_ToolSchemaNormalized(t *testing.T) {
	req := parseReq(t, `{"max_tokens": 10,
		"tools": [
			{"name": "a", "input_schema": {"type": "object"}},
			{"name": "b"},
			{"name": "c", "input_schema": {"type": "string"}}
		],
		"messages": [{"role":"user","content":"hi"}]}`)
	cc, warns := BuildCcRequest(req, testOpts())
	for i, tool := range cc.Params.Tools {
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("tool %d schema not valid JSON: %v", i, err)
		}
		if schema["type"] != "object" {
			t.Errorf("tool %d schema type = %v, want object", i, schema["type"])
		}
		if _, ok := schema["properties"].(map[string]any); !ok {
			t.Errorf("tool %d schema properties missing: %s", i, tool.InputSchema)
		}
		if tool.Type != "function" {
			t.Errorf("tool %d type = %s", i, tool.Type)
		}
	}
	// tools b（无 schema）与 c（type:string）被替换 → 必须留痕
	w := warnContaining(t, warns, "input_schema replaced with")
	if !strings.Contains(w, "2 tool(s)") || !strings.Contains(w, "b, c") {
		t.Errorf("replace warn must name the affected tools, got %q", w)
	}
}

// F1：顶层 anyOf 的工具 schema 仍被整体替换（保守行为已亲批、不改为透传），但必须留痕。
func TestBuildCcRequest_NonObjectSchemaReplacedWithWarn(t *testing.T) {
	req := parseReq(t, `{"max_tokens": 10,
		"tools": [
			{"name": "union_tool", "input_schema": {"anyOf": [{"type":"object"},{"type":"string"}]}},
			{"name": "ok_tool", "input_schema": {"type": "object", "properties": {"x": {"type": "string"}}}}
		],
		"messages": [{"role":"user","content":"hi"}]}`)
	cc, warns := BuildCcRequest(req, testOpts())
	if got := string(cc.Params.Tools[0].InputSchema); got != `{"type":"object","properties":{}}` {
		t.Errorf("non-object schema must still be replaced (behavior unchanged): %s", got)
	}
	if got := string(cc.Params.Tools[1].InputSchema); !strings.Contains(got, `"x"`) {
		t.Errorf("object schema must pass through untouched: %s", got)
	}
	w := warnContaining(t, warns, "input_schema replaced with")
	if !strings.Contains(w, "1 tool(s)") || !strings.Contains(w, "union_tool") {
		t.Errorf("warn must carry the affected tool name and count, got %q", w)
	}
	if strings.Contains(w, "ok_tool") {
		t.Errorf("warn must not name untouched tools, got %q", w)
	}
	if !strings.Contains(w, "anyOf") || !strings.Contains(w, "parameterless") {
		t.Errorf("warn must state what was lost and the consequence, got %q", w)
	}
}

// F1 + A′2：同一请求内多个工具的说明书被清空时聚合为一条 warn（避免长对话刷屏）。
func TestBuildCcRequest_NonObjectSchemaWarnAggregatedPerRequest(t *testing.T) {
	req := parseReq(t, `{"max_tokens": 10,
		"tools": [
			{"name": "t_a", "input_schema": {"type": "string"}},
			{"name": "t_b"},
			{"name": "t_c", "input_schema": {"oneOf": [{"type":"object"}]}}
		],
		"messages": [{"role":"user","content":"hi"}]}`)
	_, warns := BuildCcRequest(req, testOpts())
	n := 0
	agg := ""
	for _, w := range warns {
		if strings.Contains(w, "input_schema replaced with") {
			n++
			agg = w
		}
	}
	if n != 1 {
		t.Fatalf("warn must be aggregated into a single line per request, got %d: %v", n, warns)
	}
	if !strings.Contains(agg, "3 tool(s)") || !strings.Contains(agg, "t_a, t_b, t_c") {
		t.Errorf("aggregated warn must list every affected tool, got %q", agg)
	}
}

func TestBuildCcRequest_CacheMarkerFollowsConversationTail(t *testing.T) {
	req := parseReq(t, `{
		"max_tokens": 100,
		"system": [{"type": "text", "text": "sys", "cache_control": {"type": "ephemeral"}}],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "turn1"}]},
			{"role": "assistant", "content": [{"type": "text", "text": "ok"}]},
			{"role": "user", "content": [{"type": "text", "text": "turn2"}]},
			{"role": "assistant", "content": [{"type": "text", "text": "ok2"}]},
			{"role": "user", "content": [{"type": "text", "text": "turn3"}]}
		]
	}`)
	cc, _ := BuildCcRequest(req, testOpts())
	msgs := cc.Params.Messages
	if len(msgs) != 5 {
		t.Fatalf("messages = %d", len(msgs))
	}
	if cc.Params.Messages[0].Content[0].CacheControl != nil {
		t.Error("marker must NOT sit on the first user turn (breakpoint would never advance past it)")
	}
	if m := msgs[4].Content[0]; m.CacheControl == nil || m.CacheControl.Type != "ephemeral" {
		t.Errorf("marker must sit on the last user turn's text part: %+v", m)
	}
}

func TestBuildCcRequest_CacheMarkerFallsBackPastToolResultTail(t *testing.T) {
	req := parseReq(t, `{
		"max_tokens": 100,
		"tools": [{"name": "f", "input_schema": {"type": "object", "properties": {}}, "cache_control": {"type": "ephemeral"}}],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "go"}]},
			{"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "f", "input": {}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "res"}]}
		]
	}`)
	cc, _ := BuildCcRequest(req, testOpts())
	msgs := cc.Params.Messages
	// 末条消息只有 tool_result（无 text part）→ 标记回退到上一条 user 文本；
	// 该轮 tool_result 当轮不缓存，下一轮成为前缀后命中
	if m := msgs[0].Content[0]; m.CacheControl == nil {
		t.Errorf("marker should fall back to the previous user text part: %+v", msgs[0].Content[0])
	}
	if tr := msgs[2].Content[0]; tr.CacheControl != nil {
		t.Errorf("tool_result part should not carry the marker (unverified shape): %+v", tr)
	}
}

// 回归测试：agent 式会话的信封尾部是 role:"tool" 消息，合成断点必须从信封
// 整体末尾回扫（允许落在 assistant 的 text part 上），而不是只扫 user 消息
// —— 只扫 user 时断点钉在第一条人类消息，位置随工具回合永不前进。
func TestBuildCcRequest_CacheMarkerAdvancesThroughToolRounds(t *testing.T) {
	req := parseReq(t, `{
		"max_tokens": 100,
		"system": [{"type": "text", "text": "sys", "cache_control": {"type": "ephemeral"}}],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "task"}]},
			{"role": "assistant", "content": [
				{"type": "text", "text": "checking"},
				{"type": "tool_use", "id": "t1", "name": "f", "input": {}}
			]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "res1"}]},
			{"role": "assistant", "content": [
				{"type": "text", "text": "found it"},
				{"type": "tool_use", "id": "t2", "name": "f", "input": {}}
			]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t2", "content": "res2"}]}
		]
	}`)
	cc, _ := BuildCcRequest(req, testOpts())
	msgs := cc.Params.Messages
	if len(msgs) != 5 {
		t.Fatalf("messages = %d", len(msgs))
	}
	if m := msgs[0].Content[0]; m.CacheControl != nil {
		t.Error("marker must NOT sit on the first human message (breakpoint would never advance past it)")
	}
	// 信封尾部是 tool 消息（无 text part）→ 断点落在全文最后的 text part，
	// 即倒数第二条 assistant 消息的 "found it" —— 位置随工具回合前进
	if m := msgs[3].Content[0]; m.CacheControl == nil || m.CacheControl.Type != "ephemeral" {
		t.Errorf("marker must land on the last text part of the envelope (assistant text), got: %+v", m)
	}
}

func TestBuildCcRequest_ReplaceMarkersStripsAndSynthesizesAtTail(t *testing.T) {
	req := parseReq(t, `{
		"max_tokens": 100,
		"system": [{"type": "text", "text": "sys"}],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "old turn", "cache_control": {"type": "ephemeral"}}]},
			{"role": "assistant", "content": [{"type": "text", "text": "ok"}]},
			{"role": "user", "content": [{"type": "text", "text": "new turn"}]}
		]
	}`)
	opts := testOpts()
	opts.CacheMarkers = "replace"
	cc, warns := BuildCcRequest(req, opts)
	msgs := cc.Params.Messages
	// 客户端标记被剥掉
	if m := msgs[0].Content[0]; m.CacheControl != nil {
		t.Errorf("replace mode must strip inbound part-level markers: %+v", m)
	}
	// 强制在信封尾部合成
	if m := msgs[2].Content[0]; m.CacheControl == nil || m.CacheControl.Type != "ephemeral" {
		t.Errorf("replace mode must synthesize at the envelope tail: %+v", m)
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w, "CC_CACHE_MARKERS=replace stripped 1") {
			found = true
		}
	}
	if !found {
		t.Errorf("replace-mode strip should be logged, got %v", warns)
	}
}

func TestPrefixCacheKey_StableWithinConversation(t *testing.T) {
	turn1 := parseReq(t, `{
		"system": "You are helpful.",
		"tools": [{"name": "t", "input_schema": {"type": "object", "properties": {}}}],
		"messages": [{"role": "user", "content": "start the analysis"}]
	}`)
	turn2 := parseReq(t, `{
		"system": "You are helpful.",
		"tools": [{"name": "t", "input_schema": {"type": "object", "properties": {}}}],
		"messages": [
			{"role": "user", "content": "start the analysis"},
			{"role": "assistant", "content": "ok"},
			{"role": "user", "content": "continue"}
		]
	}`)
	other := parseReq(t, `{
		"system": "You are helpful.",
		"tools": [{"name": "t", "input_schema": {"type": "object", "properties": {}}}],
		"messages": [{"role": "user", "content": "a different opener"}]
	}`)
	k1, k2, k3 := PrefixCacheKey(turn1), PrefixCacheKey(turn2), PrefixCacheKey(other)
	if k1 != k2 {
		t.Errorf("same conversation must keep same session key: %s != %s", k1, k2)
	}
	if k1 == k3 {
		t.Errorf("different conversation should differ: %s == %s", k1, k3)
	}
	// UUID 形状（8-4-4-4-12）
	if len(k1) != 36 || k1[8] != '-' || k1[13] != '-' || k1[18] != '-' || k1[23] != '-' {
		t.Errorf("key not UUID-shaped: %s", k1)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// warnContaining 返回第一条含 sub 的留痕；缺失即测试失败（新留痕用例一律正向钉住出现）。
func warnContaining(t *testing.T, warns []string, sub string) string {
	t.Helper()
	for _, w := range warns {
		if strings.Contains(w, sub) {
			return w
		}
	}
	t.Fatalf("no warning containing %q; got %v", sub, warns)
	return ""
}

func boolPtr(b bool) *bool { return &b }
