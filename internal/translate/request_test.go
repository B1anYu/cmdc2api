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
		Now:         time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC),
		NodeVersion: "v22.21.0",
		WorkingDir:  `C:\Users\dev\projects\app-a3f2`,
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

func TestBuildCcRequest_PartLevelCacheControlKeptTTlStripped(t *testing.T) {
	req := parseReq(t, `{
		"max_tokens": 10,
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "a"},
			{"type": "text", "text": "b", "cache_control": {"type": "ephemeral", "ttl": "1h"}}
		]}]
	}`)
	cc, _ := BuildCcRequest(req, testOpts())
	part := cc.Params.Messages[0].Content[1]
	if part.CacheControl == nil || part.CacheControl.Type != "ephemeral" {
		t.Errorf("cache_control lost: %+v", part)
	}
	b, _ := json.Marshal(part.CacheControl)
	if string(b) != `{"type":"ephemeral"}` {
		t.Errorf("cache_control should be normalized to {type:ephemeral}, got %s", b)
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

func TestBuildCcRequest_AssistantThinkingDefaultDroppedOptInKept(t *testing.T) {
	raw := `{"max_tokens": 10, "messages": [
		{"role": "assistant", "content": [
			{"type": "thinking", "thinking": "hmm", "signature": "sig"},
			{"type": "text", "text": "answer"}
		]},
		{"role": "user", "content": "next"}
	]}`
	cc, _ := BuildCcRequest(parseReq(t, raw), testOpts())
	for _, p := range cc.Params.Messages[0].Content {
		if p.Type == "reasoning" {
			t.Error("thinking should be dropped by default")
		}
	}
	opts := testOpts()
	opts.AssistantReasoning = true
	cc2, _ := BuildCcRequest(parseReq(t, raw), opts)
	found := false
	for _, p := range cc2.Params.Messages[0].Content {
		if p.Type == "reasoning" && p.Text == "hmm" {
			found = true
		}
	}
	if !found {
		t.Error("AssistantReasoning=1 should keep thinking as {type:reasoning}")
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
	cc, _ := BuildCcRequest(req, testOpts())
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

func boolPtr(b bool) *bool { return &b }
