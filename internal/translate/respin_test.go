package translate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/B1anYu/cmdc2api/internal/types"
)

// ---------- 统一入口 ----------

// respin 解析 Responses 报文并归一化。统一走带报文版本（handler 的推荐入口），
// 因此安全丢弃字段的留痕也在覆盖范围内。
func respin(t *testing.T, raw string) (*types.Request, *ResponsesToolMapping, []string) {
	t.Helper()
	var req types.ResponsesRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("parse responses request: %v", err)
	}
	out, mapping, warns, err := ResponsesToRequestBody([]byte(raw), &req)
	if err != nil {
		t.Fatalf("ResponsesToRequest: %v", err)
	}
	return out, mapping, warns
}

// respinRaw 同 respin，但返回错误供拒绝场景断言。
func respinRaw(raw string) (*types.Request, *ResponsesToolMapping, []string, error) {
	var req types.ResponsesRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		return nil, nil, nil, err
	}
	return ResponsesToRequestBody([]byte(raw), &req)
}

func mustRespinErr(t *testing.T, raw string) error {
	t.Helper()
	_, _, _, err := respinRaw(raw)
	if err == nil {
		t.Fatalf("want error for %s", raw)
	}
	return err
}

func toolSchema(t *testing.T, tool types.Tool) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(tool.InputSchema, &m); err != nil {
		t.Fatalf("tool %s input_schema: %v (%s)", tool.Name, err, tool.InputSchema)
	}
	return m
}

func toolNames(tools []types.Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}

func blockTypes(blocks []types.Block) []string {
	out := make([]string, len(blocks))
	for i, b := range blocks {
		out[i] = b.Type
	}
	return out
}

func countWarns(warns []string, substr string) int {
	n := 0
	for _, w := range warns {
		if strings.Contains(w, substr) {
			n++
		}
	}
	return n
}

// flatSuffix 复算摊平名的哈希后缀（独立用标准库重算，确认后缀确实来自完整摊平名）。
func flatSuffix(flat string) string {
	sum := sha256.Sum256([]byte(flat))
	return hex.EncodeToString(sum[:4])
}

// ---------- 顶层字段 ----------

// input 的两种形态都必须支持；空 input 无法产出消息。
func TestResponsesToRequest_InputShapes(t *testing.T) {
	t.Run("plain-string", func(t *testing.T) {
		out, _, _ := respin(t, `{"model":"m","input":"just a string"}`)
		if len(out.Messages) != 1 || out.Messages[0].Role != "user" {
			t.Fatalf("messages = %+v, want a single user message", out.Messages)
		}
		if !containsText(blocksOf(t, out.Messages[0]), "just a string") {
			t.Error("string input text lost")
		}
	})

	t.Run("array", func(t *testing.T) {
		out, _, _ := respin(t, `{"model":"m","input":[{"role":"user","content":"hi"}]}`)
		if len(out.Messages) != 1 {
			t.Fatalf("messages = %+v", out.Messages)
		}
	})

	t.Run("empty-input", func(t *testing.T) {
		for _, raw := range []string{`{"model":"m"}`, `{"model":"m","input":null}`, `{"model":"m","input":""}`, `{"model":"m","input":[]}`} {
			if err := mustRespinErr(t, raw); err != ErrResponsesEmptyInput {
				t.Errorf("%s: err = %v, want ErrResponsesEmptyInput", raw, err)
			}
		}
	})

	t.Run("invalid-shape", func(t *testing.T) {
		err := mustRespinErr(t, `{"model":"m","input":123}`)
		if !strings.Contains(err.Error(), "input must be a string or an array") {
			t.Errorf("err = %v", err)
		}
	})
}

// previous_response_id 非空直接拒绝（无服务端状态）。
func TestResponsesToRequest_PreviousResponseIDRejected(t *testing.T) {
	err := mustRespinErr(t, `{"model":"m","input":"hi","previous_response_id":"resp_abc"}`)
	if err != ErrResponsesPreviousResponseID {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "resend the full input history") {
		t.Errorf("error text must carry the actionable hint: %v", err)
	}
	// 空白值不算声明：与「未声明」同义，不该拒绝
	out, _, _ := respin(t, `{"model":"m","input":"hi","previous_response_id":"  "}`)
	if len(out.Messages) != 1 {
		t.Errorf("blank previous_response_id must not be treated as a continuation: %+v", out.Messages)
	}
}

// instructions 去空白后占 system 首段，input 里的 system/developer item 按序并入其后。
func TestResponsesToRequest_SystemSegments(t *testing.T) {
	out, _, _ := respin(t, `{
		"model": "m",
		"instructions": "  be terse  ",
		"input": [
			{"role": "system", "content": "sys one"},
			{"role": "developer", "content": [{"type": "input_text", "text": "dev two"}, {"type": "text", "text": "dev three"}]},
			{"role": "user", "content": "hi"}
		]
	}`)
	if got, want := string(out.System), `"be terse\n\nsys one\n\ndev two\ndev three"`; got != want {
		t.Fatalf("system = %s, want %s", got, want)
	}
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" {
		t.Errorf("messages = %+v, want only the user message", out.Messages)
	}
}

// F12a：system/developer item 内的非文本 part 与「content 非 string 非数组」都必须留痕，
// 且「空」（本来就没内容）与「坏」（内容被丢弃/解析失败）在留痕上可区分。
func TestResponsesToRequest_SystemItemDrops(t *testing.T) {
	out, _, warns := respin(t, `{
		"model": "m",
		"input": [
			{"role": "developer", "content": [
				{"type": "input_text", "text": "keep"},
				{"type": "input_image", "image_url": "https://x/y.png"}
			]},
			{"role": "system", "content": 123},
			{"role": "developer"},
			{"role": "developer", "content": ""},
			{"role": "user", "content": "hi"}
		]
	}`)
	if got, want := string(out.System), `"keep"`; got != want {
		t.Fatalf("system = %s, want %s (两条坏 item 都不该贡献内容)", got, want)
	}
	assertWarnsContain(t, warns,
		`input item 0 (system/developer): unsupported content part "input_image" dropped`)
	assertWarnsContain(t, warns,
		`input item 1 (system/developer): content is neither string nor part array; dropped`)
	// 空 content 不留痕：与「坏」区分开，否则真正的解析失败会被噪声淹没。
	if got := countWarns(warns, "(system/developer)"); got != 2 {
		t.Errorf("(system/developer) warn count = %d, want 2 (只有两条丢弃路径): %v", got, warns)
	}
}

// max_output_tokens / temperature / top_p / stream 直传，≤0 留给信封构造兜底。
func TestResponsesToRequest_ScalarFields(t *testing.T) {
	out, _, _ := respin(t, `{"model":"m","input":"hi","max_output_tokens":4096,"temperature":0.3,"top_p":0.8,"stream":true}`)
	if out.Model != "m" || out.MaxTokens != 4096 || !out.Stream {
		t.Errorf("scalars = %+v", out)
	}
	if out.Temperature == nil || *out.Temperature != 0.3 || out.TopP == nil || *out.TopP != 0.8 {
		t.Errorf("sampling = %+v / %+v", out.Temperature, out.TopP)
	}
	out, _, _ = respin(t, `{"model":"m","input":"hi"}`)
	if out.MaxTokens != 0 {
		t.Errorf("max_tokens = %d, want 0 (由 BuildCcRequest 兜底)", out.MaxTokens)
	}
}

// ---------- message item ----------

// user / assistant 消息的 content 形态与块白名单。
func TestResponsesToRequest_MessageItems(t *testing.T) {
	t.Run("user-parts", func(t *testing.T) {
		out, _, warns := respin(t, `{
			"model": "m",
			"input": [{"role": "user", "content": [
				{"type": "input_text", "text": "look"},
				{"type": "input_image", "image_url": "data:image/png;base64,QUJD"},
				{"type": "input_image", "image_url": {"url": "https://x/y.png"}},
				{"type": "input_audio", "data": "zzz"},
				{"type": "input_text", "text": ""}
			]}]
		}`)
		blocks := blocksOf(t, out.Messages[0])
		if got, want := blockTypes(blocks), []string{"text", "image", "image"}; !equalStrings(got, want) {
			t.Fatalf("blocks = %v, want %v", got, want)
		}
		if s := blocks[1].Source; s == nil || s.Type != "base64" || s.MediaType != "image/png" || s.Data != "QUJD" {
			t.Errorf("data URI source = %+v", s)
		}
		if s := blocks[2].Source; s == nil || s.Type != "url" || s.URL != "https://x/y.png" {
			t.Errorf("object-form image source = %+v", s)
		}
		assertWarnsContain(t, warns, `unsupported content part "input_audio" dropped`)
	})

	t.Run("assistant-parts", func(t *testing.T) {
		out, _, warns := respin(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": "q"},
				{"role": "assistant", "content": [{"type": "output_text", "text": "a"}, {"type": "input_image", "image_url": "https://x/y.png"}]}
			]
		}`)
		blocks := blocksOf(t, out.Messages[1])
		if got, want := blockTypes(blocks), []string{"text"}; !equalStrings(got, want) {
			t.Fatalf("assistant blocks = %v, want %v", got, want)
		}
		assertWarnsContain(t, warns, `input item 1 (assistant): unsupported content part "input_image" dropped`)
	})

	t.Run("empty-content-and-unknown-role-downgraded", func(t *testing.T) {
		out, _, warns := respin(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": []},
				{"type": "message", "role": "tool", "content": "x"},
				{"role": "user", "content": "kept"}
			]
		}`)
		// 空 content 不产消息；未知 role（F25）兜底为 user 且内容保留，与后一条 user 合并同类。
		if got, want := rolesOf(out.Messages), []string{"user"}; !equalStrings(got, want) {
			t.Fatalf("roles = %v, want %v", got, want)
		}
		blocks := blocksOf(t, out.Messages[0])
		if !containsText(blocks, "x") || !containsText(blocks, "kept") {
			t.Fatalf("unknown-role content lost: blocks = %+v", blocks)
		}
		assertWarnsContain(t, warns, `unknown role "tool" downgraded to a user message`)
	})

	t.Run("unknown-role-downgrade-keeps-parts", func(t *testing.T) {
		// 兜底走 userBlocks：文本与合法图片都随内容一起保留，非法 part 仍按 user 口径留痕。
		out, _, warns := respin(t, `{
			"model": "m",
			"input": [{"type": "message", "role": "critic", "content": [
				{"type": "input_text", "text": "review this"},
				{"type": "input_image", "image_url": "data:image/png;base64,QUJD"}
			]}]
		}`)
		blocks := blocksOf(t, out.Messages[0])
		if got, want := blockTypes(blocks), []string{"text", "image"}; !equalStrings(got, want) {
			t.Fatalf("blocks = %v, want %v", got, want)
		}
		assertWarnsContain(t, warns, `unknown role "critic" downgraded to a user message`)
	})

	t.Run("unknown-role-without-content-keeps-no-message", func(t *testing.T) {
		out, _, warns := respin(t, `{
			"model": "m",
			"input": [{"type": "message", "role": "critic"}, {"role": "user", "content": "hi"}]
		}`)
		if got, want := rolesOf(out.Messages), []string{"user"}; !equalStrings(got, want) {
			t.Fatalf("roles = %v, want %v", got, want)
		}
		assertWarnsContain(t, warns, `unknown role "critic" downgraded to a user message`)
	})

	t.Run("unparsable-content", func(t *testing.T) {
		out, _, warns := respin(t, `{"model":"m","input":[{"role":"user","content":123},{"role":"user","content":"ok"}]}`)
		if len(out.Messages) != 1 {
			t.Fatalf("messages = %+v", out.Messages)
		}
		assertWarnsContain(t, warns, "content is neither string nor part array")
	})

	t.Run("image-edge-cases", func(t *testing.T) {
		out, _, warns := respin(t, `{
			"model": "m",
			"input": [{"role": "user", "content": [
				{"type": "input_image", "image_url": "ftp://x/y.png"},
				{"type": "input_image"},
				{"type": "input_text", "text": "kept"}
			]}]
		}`)
		blocks := blocksOf(t, out.Messages[0])
		if got, want := blockTypes(blocks), []string{"text"}; !equalStrings(got, want) {
			t.Fatalf("blocks = %v, want %v", got, want)
		}
		// 两条入站路径现在都做请求内聚合（aggregateRequestWarns）：两处同因丢弃折叠为
		// 一条，保留首条原文并标明「另有 N 处同因」，故这里断言折叠后的形态而非条数。
		if n := countWarns(warns, "unsupported or missing url dropped"); n != 1 {
			t.Errorf("image drop warns = %d, want 1 aggregated line (%v)", n, warns)
		}
		assertWarnsContain(t, warns, "and 1 more of the same kind")
	})
}

// ---------- 工具调用 item ----------

func TestResponsesToRequest_ToolCalls(t *testing.T) {
	t.Run("function-call", func(t *testing.T) {
		out, _, warns := respin(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": "go"},
				{"type": "function_call", "call_id": "call_1", "name": "shell", "arguments": "{\"cmd\":\"ls\"}"},
				{"type": "function_call_output", "call_id": "call_1", "output": "a.go"}
			]
		}`)
		assertPairing(t, out.Messages)
		block := blocksOf(t, out.Messages[1])[0]
		if block.Type != "tool_use" || block.ID != "call_1" || block.Name != "shell" || string(block.Input) != `{"cmd":"ls"}` {
			t.Fatalf("tool_use = %+v", block)
		}
		if len(warns) != 0 {
			t.Errorf("unexpected warns: %v", warns)
		}
	})

	t.Run("bad-arguments-become-empty-object", func(t *testing.T) {
		out, _, _ := respin(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": "go"},
				{"type": "function_call", "call_id": "call_1", "name": "shell", "arguments": "{\"cmd\":"},
				{"type": "function_call_output", "call_id": "call_1", "output": "x"}
			]
		}`)
		if got := string(blocksOf(t, out.Messages[1])[0].Input); got != `{}` {
			t.Errorf("input = %s, want {}", got)
		}
	})

	t.Run("custom-call-wraps-free-text", func(t *testing.T) {
		out, _, _ := respin(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": "go"},
				{"type": "custom_tool_call", "call_id": "call_9", "name": "apply_patch", "input": "*** Begin Patch\n*** End Patch"},
				{"type": "custom_tool_call_output", "call_id": "call_9", "output": "done"}
			]
		}`)
		assertPairing(t, out.Messages)
		block := blocksOf(t, out.Messages[1])[0]
		if block.Type != "tool_use" || block.ID != "call_9" || block.Name != "apply_patch" {
			t.Fatalf("tool_use = %+v", block)
		}
		var payload map[string]string
		if err := json.Unmarshal(block.Input, &payload); err != nil || payload["input"] != "*** Begin Patch\n*** End Patch" {
			t.Errorf("custom input = %s (%v)", block.Input, err)
		}
	})

	t.Run("empty-custom-input-still-object", func(t *testing.T) {
		out, _, _ := respin(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": "go"},
				{"type": "custom_tool_call", "call_id": "call_9", "name": "apply_patch"},
				{"type": "custom_tool_call_output", "call_id": "call_9", "output": "done"}
			]
		}`)
		if got := string(blocksOf(t, out.Messages[1])[0].Input); got != `{"input":""}` {
			t.Errorf("input = %s, want {\"input\":\"\"}", got)
		}
	})

	t.Run("dropped-incomplete-calls", func(t *testing.T) {
		out, _, warns := respin(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": "go"},
				{"type": "function_call", "call_id": "call_1", "arguments": "{}"},
				{"type": "function_call", "name": "no_id", "arguments": "{}"}
			]
		}`)
		if len(out.Messages) != 1 {
			t.Fatalf("messages = %+v, want only the user message", out.Messages)
		}
		assertWarnsContain(t, warns, "without name dropped")
		assertWarnsContain(t, warns, `tool call "no_id" without call_id dropped`)
	})
}

// 工具输出归一化：字符串/空/part 数组/对象/缺 call_id。
func TestResponsesToRequest_ToolOutputShapes(t *testing.T) {
	prefix := `{"model":"m","input":[{"role":"user","content":"go"},` +
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}`
	// outputItem 拼一个 function_call_output item（raw 为空表示该字段缺失）。
	outputItem := func(raw string) string {
		if raw == "" {
			return `,{"type":"function_call_output","call_id":"c1"}`
		}
		return `,{"type":"function_call_output","call_id":"c1","output":` + raw + `}`
	}
	resultContent := func(t *testing.T, raw string) json.RawMessage {
		t.Helper()
		out, _, _ := respin(t, prefix+outputItem(raw)+`]}`)
		assertPairing(t, out.Messages)
		return blocksOf(t, out.Messages[2])[0].Content
	}

	cases := []struct {
		name   string
		output string
		want   string
	}{
		{"string", `"sunny"`, `"sunny"`},
		{"empty-string", `""`, `""`},
		{"null", `null`, `""`},
		{"absent", ``, `""`},
		{"text-parts", `[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]`, `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`},
		{"unknown-parts-only", `[{"type":"input_audio","data":"x"}]`, `""`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(resultContent(t, tc.output)); got != tc.want {
				t.Errorf("tool_result content = %s, want %s", got, tc.want)
			}
		})
	}

	t.Run("image-parts-handed-to-convertUser", func(t *testing.T) {
		got := string(resultContent(t, `[{"type":"input_text","text":"a"},{"type":"input_image","image_url":"https://x/y.png"}]`))
		var blocks []types.Block
		if err := json.Unmarshal([]byte(got), &blocks); err != nil {
			t.Fatalf("blocks = %s (%v)", got, err)
		}
		if len(blocks) != 2 || blocks[1].Type != "image" {
			t.Fatalf("blocks = %+v, want [text, image]", blocks)
		}
	})

	t.Run("object-retained-as-json", func(t *testing.T) {
		got := string(resultContent(t, `{"status":"ok","count":2}`))
		if got != `"{\"status\":\"ok\",\"count\":2}"` {
			t.Errorf("tool_result content = %s", got)
		}
	})

	t.Run("missing-call-id-dropped", func(t *testing.T) {
		out, _, warns := respin(t, `{
			"model": "m",
			"input": [{"role": "user", "content": "go"}, {"type": "function_call_output", "output": "orphan"}]
		}`)
		if len(out.Messages) != 1 {
			t.Fatalf("messages = %+v", out.Messages)
		}
		assertWarnsContain(t, warns, "missing call_id, dropped")
	})

	t.Run("custom-output-type-alias", func(t *testing.T) {
		out, _, _ := respin(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": "go"},
				{"type": "custom_tool_call", "call_id": "c9", "name": "apply_patch"},
				{"type": "custom_tool_call_output", "call_id": "c9", "output": "patched"}
			]
		}`)
		assertPairing(t, out.Messages)
		if got := string(blocksOf(t, out.Messages[2])[0].Content); got != `"patched"` {
			t.Errorf("tool_result content = %s", got)
		}
	})
}

// ---------- reasoning item ----------

func TestResponsesToRequest_ReasoningItem(t *testing.T) {
	reasoning := func(item string) []types.Block {
		out, _, _ := respin(t, `{
			"model": "m",
			"input": [{"role": "user", "content": "go"}, `+item+`]
		}`)
		for _, m := range out.Messages {
			for _, b := range blocksOf(t, m) {
				if b.Type == "thinking" {
					return blocksOf(t, m)
				}
			}
		}
		return nil
	}

	t.Run("summary-joined", func(t *testing.T) {
		blocks := reasoning(`{"type":"reasoning","summary":[{"type":"summary_text","text":"first"},{"type":"summary_text","text":"second"}]}`)
		if len(blocks) != 1 || blocks[0].Thinking != "first\nsecond" {
			t.Fatalf("blocks = %+v", blocks)
		}
	})

	t.Run("content-fallback", func(t *testing.T) {
		blocks := reasoning(`{"type":"reasoning","content":[{"type":"reasoning_text","text":"from content"}]}`)
		if len(blocks) != 1 || blocks[0].Thinking != "from content" {
			t.Fatalf("blocks = %+v", blocks)
		}
	})

	t.Run("summary-wins-over-content", func(t *testing.T) {
		// 同一次思考两组都写了：只取 summary，不能把同一段思考回放两遍。
		blocks := reasoning(`{"type":"reasoning","summary":[{"type":"summary_text","text":"summary"}],"content":[{"type":"reasoning_text","text":"content"}]}`)
		if len(blocks) != 1 || blocks[0].Thinking != "summary" {
			t.Fatalf("blocks = %+v", blocks)
		}
	})

	t.Run("empty-skipped", func(t *testing.T) {
		if blocks := reasoning(`{"type":"reasoning","summary":[{"type":"summary_text","text":""}]}`); blocks != nil {
			t.Fatalf("blocks = %+v, want no thinking block", blocks)
		}
	})

	t.Run("encrypted-content-dropped", func(t *testing.T) {
		// 密文无法解密也无上游载体：不产消息、不留痕（Codex 每轮都会发，留痕只会淹没日志）。
		out, _, warns := respin(t, `{
			"model": "m",
			"input": [{"role": "user", "content": "go"},
				{"type": "reasoning", "encrypted_content": "gAAAAA", "summary": []}]
		}`)
		if len(out.Messages) != 1 {
			t.Fatalf("messages = %+v", out.Messages)
		}
		if len(warns) != 0 {
			t.Errorf("unexpected warns: %v", warns)
		}
	})
}

// 未知 item 类型明确丢弃 + 留痕。
func TestResponsesToRequest_UnknownItemsDropped(t *testing.T) {
	out, _, warns := respin(t, `{
		"model": "m",
		"input": [
			{"role": "user", "content": "go"},
			{"type": "web_search_call", "id": "ws_1"},
			{"type": "item_reference", "id": "x"},
			{"type": "computer_call", "call_id": "c"}
		]
	}`)
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %+v", out.Messages)
	}
	for _, typ := range []string{"web_search_call", "item_reference", "computer_call"} {
		assertWarnsContain(t, warns, `unsupported type "`+typ+`" dropped`)
	}
}

// ---------- 工具族 ----------

func TestResponsesToRequest_Tools(t *testing.T) {
	t.Run("function", func(t *testing.T) {
		out, mapping, warns := respin(t, `{
			"model": "m",
			"input": "hi",
			"tools": [{"type": "function", "name": "shell", "description": "run", "parameters": {"type": "object", "properties": {"cmd": {"type": "string"}}}}]
		}`)
		if got := toolNames(out.Tools); !equalStrings(got, []string{"shell"}) {
			t.Fatalf("tools = %v", got)
		}
		schema := toolSchema(t, out.Tools[0])
		if schema["type"] != "object" {
			t.Errorf("schema = %+v", schema)
		}
		if mapping.Custom != nil || mapping.Namespace != nil || mapping.ToolSearch {
			t.Errorf("mapping = %+v, want empty", mapping)
		}
		if len(warns) != 0 {
			t.Errorf("unexpected warns: %v", warns)
		}
	})

	t.Run("function-without-parameters", func(t *testing.T) {
		out, _, _ := respin(t, `{"model":"m","input":"hi","tools":[{"type":"function","name":"bare"}]}`)
		schema := toolSchema(t, out.Tools[0])
		if schema["type"] != "object" {
			t.Errorf("schema = %+v, want the empty-object default", schema)
		}
	})

	t.Run("strict-flag-warned", func(t *testing.T) {
		_, _, warns := respin(t, `{"model":"m","input":"hi","tools":[{"type":"function","name":"s","strict":true,"parameters":{"type":"object"}}]}`)
		assertWarnsContain(t, warns, "strict schema flag dropped")
	})

	// G1：非 object schema 被整体替换必须留痕（此前 Responses 入站走的是静默变体
	// normalizeSchema，只有 Anthropic/Chat 侧的 convertTools 留痕）。
	t.Run("non-object-schema-replacement-warned", func(t *testing.T) {
		out, _, warns := respin(t, `{
			"model": "m",
			"input": "hi",
			"tools": [
				{"type": "function", "name": "union", "parameters": {"anyOf": [{"type": "object", "properties": {"a": {"type": "string"}}}]}},
				{"type": "function", "name": "good", "parameters": {"type": "object", "properties": {"b": {"type": "string"}}}}
			]
		}`)
		if got, want := toolNames(out.Tools), []string{"union", "good"}; !equalStrings(got, want) {
			t.Fatalf("tools = %v, want %v", got, want)
		}
		schema := toolSchema(t, out.Tools[0])
		props, _ := schema["properties"].(map[string]any)
		if schema["type"] != "object" || len(props) != 0 {
			t.Fatalf("union schema = %+v, want the empty-object default", schema)
		}
		// 同请求内聚合成一条，且只点名真正被替换的工具。
		if got := countWarns(warns, "tool parameters replaced"); got != 1 {
			t.Fatalf("replacement warn count = %d, want 1: %v", got, warns)
		}
		assertWarnsContain(t, warns, "[union]")
		for _, w := range warns {
			if strings.Contains(w, "tool parameters replaced") && strings.Contains(w, "good") {
				t.Errorf("未被替换的工具不该出现在留痕里: %q", w)
			}
		}
	})

	// 重复声明（additional_tools 重发同一份坏 schema）不得重复留痕：聚合按请求去重。
	t.Run("schema-replacement-aggregated-once", func(t *testing.T) {
		bad := `{"type":"function","name":"union","parameters":{"anyOf":[{"type":"object"}]}}`
		_, _, warns := respin(t, `{
			"model": "m",
			"input": [{"role": "user", "content": "go"}, {"type": "additional_tools", "tools": [`+bad+`]}],
			"tools": [`+bad+`]
		}`)
		if got := countWarns(warns, "tool parameters replaced"); got != 1 {
			t.Fatalf("replacement warn count = %d, want 1 (同请求聚合，重发不重复报): %v", got, warns)
		}
		assertWarnsContain(t, warns, "for 1 tool(s) [union]")
	})

	t.Run("custom-downgrade", func(t *testing.T) {
		out, mapping, _ := respin(t, `{
			"model": "m",
			"input": "hi",
			"tools": [{"type": "custom", "name": "apply_patch", "description": "patch files",
				"format": {"type": "grammar", "syntax": "lark", "definition": "start: /.+/"}}]
		}`)
		if !mapping.Custom["apply_patch"] {
			t.Fatalf("mapping.Custom = %+v", mapping.Custom)
		}
		schema := toolSchema(t, out.Tools[0])
		props, _ := schema["properties"].(map[string]any)
		input, _ := props["input"].(map[string]any)
		if input == nil || input["type"] != "string" {
			t.Fatalf("custom schema = %+v", schema)
		}
		desc, _ := input["description"].(string)
		if !strings.Contains(desc, "patch files") || !strings.Contains(desc, "```lark") || !strings.Contains(desc, "start: /.+/") {
			t.Errorf("grammar not folded into description: %q", desc)
		}
		required, _ := schema["required"].([]any)
		if len(required) != 1 || required[0] != "input" {
			t.Errorf("required = %+v", required)
		}
	})

	t.Run("custom-string-shorthand", func(t *testing.T) {
		out, mapping, _ := respin(t, `{"model":"m","input":"hi","tools":["exec"]}`)
		if !mapping.Custom["exec"] || len(out.Tools) != 1 || out.Tools[0].Name != "exec" {
			t.Fatalf("tools = %+v mapping = %+v", toolNames(out.Tools), mapping)
		}
	})

	t.Run("tool-search-proxy", func(t *testing.T) {
		out, mapping, _ := respin(t, `{"model":"m","input":"hi","tools":[{"type":"tool_search"}]}`)
		if !mapping.ToolSearch || len(out.Tools) != 1 || out.Tools[0].Name != responsesToolSearchName {
			t.Fatalf("tools = %+v mapping = %+v", toolNames(out.Tools), mapping)
		}
		schema := toolSchema(t, out.Tools[0])
		props, _ := schema["properties"].(map[string]any)
		if _, ok := props["query"]; !ok {
			t.Errorf("proxy schema = %+v", schema)
		}
		if _, ok := props["limit"]; !ok {
			t.Errorf("proxy schema = %+v", schema)
		}
		required, _ := schema["required"].([]any)
		if len(required) != 1 || required[0] != "query" {
			t.Errorf("required = %+v", required)
		}
	})

	t.Run("namespace-flattened", func(t *testing.T) {
		out, mapping, _ := respin(t, `{
			"model": "m",
			"input": "hi",
			"tools": [{"type": "namespace", "name": "fs", "tools": [
				{"type": "function", "name": "read", "description": "read a file", "parameters": {"type": "object"}},
				{"name": "write", "parameters": {"type": "object"}},
				{"type": "custom", "name": "patch", "format": {"syntax": "lark", "definition": "d"}},
				{"type": "web_search", "name": "nope"}
			]}]
		}`)
		if got, want := toolNames(out.Tools), []string{"fs__read", "fs__write", "fs__patch"}; !equalStrings(got, want) {
			t.Fatalf("tools = %v, want %v", got, want)
		}
		if mapping.Namespace["fs__read"] != (NamespacedName{Namespace: "fs", Name: "read"}) {
			t.Errorf("namespace mapping = %+v", mapping.Namespace)
		}
		if !mapping.Custom["fs__patch"] {
			t.Errorf("namespace custom child must be marked custom: %+v", mapping.Custom)
		}
	})

	t.Run("unsupported-types-dropped", func(t *testing.T) {
		out, _, warns := respin(t, `{
			"model": "m",
			"input": "hi",
			"tools": [
				{"type": "web_search"},
				{"type": "image_generation"},
				{"type": "function", "name": "kept", "parameters": {"type": "object"}}
			]
		}`)
		if got := toolNames(out.Tools); !equalStrings(got, []string{"kept"}) {
			t.Fatalf("tools = %v", got)
		}
		assertWarnsContain(t, warns, `unsupported tool type "web_search" dropped`)
		assertWarnsContain(t, warns, `unsupported tool type "image_generation" dropped`)
	})

	t.Run("anonymous-declarations-dropped", func(t *testing.T) {
		out, _, warns := respin(t, `{"model":"m","input":"hi","tools":[{"type":"function"},{"type":"namespace"}]}`)
		if len(out.Tools) != 0 {
			t.Fatalf("tools = %v", toolNames(out.Tools))
		}
		assertWarnsContain(t, warns, "function without name dropped")
		assertWarnsContain(t, warns, "namespace tool without name dropped")
	})
}

// 超长摊平名按 rune 截断 + 确定性哈希后缀。
func TestResponsesToRequest_NamespaceFlattenTruncation(t *testing.T) {
	ns := strings.Repeat("名", 60) // 多字节字符：按字节截断会切出非法 UTF-8
	child := "子工具名称"
	flat := flattenNamespaceToolName(ns, child)
	if utf8.RuneCountInString(flat) != respinToolNameMax {
		t.Fatalf("rune count = %d, want %d (%q)", utf8.RuneCountInString(flat), respinToolNameMax, flat)
	}
	if !utf8.ValidString(flat) {
		t.Fatalf("flattened name is not valid UTF-8: %q", flat)
	}
	if !strings.HasSuffix(flat, "__"+flatSuffix(ns+"__"+child)) {
		t.Fatalf("suffix = %q, want the deterministic sha256 prefix", flat)
	}
	if again := flattenNamespaceToolName(ns, child); again != flat {
		t.Errorf("flatten must be deterministic: %q vs %q", again, flat)
	}

	// 恰好 64 rune 的名字不截断、不加后缀。
	exact := strings.Repeat("a", 31) + "__" + strings.Repeat("b", 31)
	if got := flattenNamespaceToolName(strings.Repeat("a", 31), strings.Repeat("b", 31)); got != exact {
		t.Errorf("64-rune name = %q, want %q", got, exact)
	}
	// 65 rune 截断。
	long := strings.Repeat("a", 32) + "__" + strings.Repeat("b", 31)
	if got := flattenNamespaceToolName(strings.Repeat("a", 32), strings.Repeat("b", 31)); utf8.RuneCountInString(got) != respinToolNameMax || got == long {
		t.Errorf("65-rune name = %q", got)
	}

	// 端到端：截断名仍是 mapping 的键（出站据此还原 namespace 子工具）。
	out, mapping, _ := respin(t, `{
		"model": "m",
		"input": "hi",
		"tools": [{"type": "namespace", "name": "`+ns+`", "tools": [{"type": "function", "name": "`+child+`"}]}]
	}`)
	if len(out.Tools) != 1 || out.Tools[0].Name != flat {
		t.Fatalf("tools = %v, want [%s]", toolNames(out.Tools), flat)
	}
	if mapping.Namespace[flat] != (NamespacedName{Namespace: ns, Name: child}) {
		t.Errorf("mapping.Namespace = %+v", mapping.Namespace)
	}
}

// 三类重名冲突显式 400（不静默降级），文案带可操作建议。
func TestResponsesToRequest_ToolConflicts(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			"top-level-duplicate",
			`{"model":"m","input":"hi","tools":[
				{"type":"function","name":"dup","parameters":{"type":"object"}},
				{"type":"function","name":"dup","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}]}`,
			"is declared more than once",
		},
		{
			"custom-vs-function",
			`{"model":"m","input":"hi","tools":[
				{"type":"function","name":"dup","parameters":{"type":"object"}},
				{"type":"custom","name":"dup"}]}`,
			"is declared more than once",
		},
		{
			"collides-with-tool-search",
			`{"model":"m","input":"hi","tools":[
				{"type":"function","name":"tool_search","parameters":{"type":"object"}},
				{"type":"tool_search"}]}`,
			`reserved by the tool_search declaration`,
		},
		{
			"namespace-vs-top-level",
			`{"model":"m","input":"hi","tools":[
				{"type":"function","name":"fs__read","parameters":{"type":"object"}},
				{"type":"namespace","name":"fs","tools":[{"type":"function","name":"read"}]}]}`,
			"is declared more than once",
		},
		{
			"namespace-children-collide",
			`{"model":"m","input":"hi","tools":[
				{"type":"namespace","name":"fs","tools":[{"type":"function","name":"read"}]},
				{"type":"namespace","name":"fs","tools":[{"type":"function","name":"read"}]}]}`,
			"is declared more than once",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mustRespinErr(t, tc.raw)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "rename") && !strings.Contains(err.Error(), "wrap") {
				t.Errorf("error text must carry actionable advice: %v", err)
			}
		})
	}
}

// additional_tools item 并入工具清单，同 schema 重发幂等。
func TestResponsesToRequest_AdditionalTools(t *testing.T) {
	t.Run("merged", func(t *testing.T) {
		out, _, _ := respin(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": "go"},
				{"type": "additional_tools", "tools": [{"type": "function", "name": "extra", "parameters": {"type": "object"}}]}
			],
			"tools": [{"type": "function", "name": "top", "parameters": {"type": "object"}}]
		}`)
		if got, want := toolNames(out.Tools), []string{"top", "extra"}; !equalStrings(got, want) {
			t.Fatalf("tools = %v, want %v (additional 追加在顶层之后)", got, want)
		}
		if len(out.Messages) != 1 {
			t.Errorf("additional_tools 不应产出消息: %+v", out.Messages)
		}
	})

	t.Run("identical-redeclaration-deduped", func(t *testing.T) {
		out, _, warns := respin(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": "go"},
				{"type": "additional_tools", "tools": [{"type": "function", "name": "top", "parameters": {"type": "object", "properties": {"a": {"type": "string"}}}}]}
			],
			"tools": [{"type": "function", "name": "top", "parameters": {"type": "object", "properties": {"a": {"type": "string"}}}}]
		}`)
		if got := toolNames(out.Tools); !equalStrings(got, []string{"top"}) {
			t.Fatalf("tools = %v, want the duplicate to be dropped", got)
		}
		if len(warns) != 0 {
			t.Errorf("unexpected warns: %v", warns)
		}
	})

	t.Run("conflicting-redeclaration-rejected", func(t *testing.T) {
		err := mustRespinErr(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": "go"},
				{"type": "additional_tools", "tools": [{"type": "function", "name": "top", "parameters": {"type": "object", "properties": {"b": {"type": "string"}}}}]}
			],
			"tools": [{"type": "function", "name": "top", "parameters": {"type": "object", "properties": {"a": {"type": "string"}}}}]
		}`)
		if !strings.Contains(err.Error(), "declared more than once") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("unparsable-payload-dropped", func(t *testing.T) {
		out, _, warns := respin(t, `{"model":"m","input":[{"role":"user","content":"go"},{"type":"additional_tools","tools":123}]}`)
		if len(out.Tools) != 0 {
			t.Fatalf("tools = %v", toolNames(out.Tools))
		}
		assertWarnsContain(t, warns, "additional_tools payload is not a tool array")
	})
}

// tool_search_output 的发现提升：追加到末尾、同名同 schema 去重、同名不同 schema 400。
func TestResponsesToRequest_ToolSearchDiscovery(t *testing.T) {
	toolSearch := `{"type":"tool_search"}`
	t.Run("promoted", func(t *testing.T) {
		out, _, _ := respin(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": "go"},
				{"type": "tool_search_output", "status": "completed", "tools": [{"type": "function", "name": "discovered", "description": "d", "parameters": {"type": "object", "properties": {"q": {"type": "string"}}}}]}
			],
			"tools": [`+toolSearch+`]
		}`)
		if got, want := toolNames(out.Tools), []string{responsesToolSearchName, "discovered"}; !equalStrings(got, want) {
			t.Fatalf("tools = %v, want %v", got, want)
		}
	})

	t.Run("identical-discovery-deduped", func(t *testing.T) {
		decl := `{"type":"function","name":"known","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}`
		out, _, warns := respin(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": "go"},
				{"type": "tool_search_output", "status": "completed", "tools": [`+decl+`]}
			],
			"tools": [`+toolSearch+`,`+decl+`]
		}`)
		if got, want := toolNames(out.Tools), []string{responsesToolSearchName, "known"}; !equalStrings(got, want) {
			t.Fatalf("tools = %v, want %v", got, want)
		}
		assertWarnsContain(t, warns, "already declared")
	})

	t.Run("conflicting-discovery-rejected", func(t *testing.T) {
		err := mustRespinErr(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": "go"},
				{"type": "tool_search_output", "status": "completed", "tools": [{"type": "function", "name": "known", "parameters": {"type": "object", "properties": {"other": {"type": "string"}}}}]}
			],
			"tools": [`+toolSearch+`,{"type":"function","name":"known","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]
		}`)
		if !strings.Contains(err.Error(), "discovered tool conflicts with an existing declaration") {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(err.Error(), "known") {
			t.Errorf("error must name the conflicting tool: %v", err)
		}
	})

	t.Run("without-tool-search-declaration-ignored", func(t *testing.T) {
		out, _, warns := respin(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": "go"},
				{"type": "tool_search_output", "status": "completed", "tools": [{"type": "function", "name": "discovered"}]}
			]
		}`)
		if len(out.Tools) != 0 {
			t.Fatalf("tools = %v, want none", toolNames(out.Tools))
		}
		assertWarnsContain(t, warns, "no tool_search declaration in this request")
	})

	t.Run("incomplete-status-ignored", func(t *testing.T) {
		out, _, warns := respin(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": "go"},
				{"type": "tool_search_output", "status": "in_progress", "tools": [{"type": "function", "name": "discovered"}]}
			],
			"tools": [`+toolSearch+`]
		}`)
		if got, want := toolNames(out.Tools), []string{responsesToolSearchName}; !equalStrings(got, want) {
			t.Fatalf("tools = %v, want %v", got, want)
		}
		assertWarnsContain(t, warns, "only completed results are promoted")
	})

	// F13：status 缺失不是 completed，不得提升（此前空串落空判定、直接进提升分支）。
	t.Run("missing-status-not-promoted", func(t *testing.T) {
		out, _, warns := respin(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": "go"},
				{"type": "tool_search_output", "tools": [{"type": "function", "name": "discovered", "parameters": {"type": "object", "properties": {"q": {"type": "string"}}}}]}
			],
			"tools": [`+toolSearch+`]
		}`)
		if got, want := toolNames(out.Tools), []string{responsesToolSearchName}; !equalStrings(got, want) {
			t.Fatalf("tools = %v, want %v (缺 status 的输出不提升)", got, want)
		}
		assertWarnsContain(t, warns, `tool_search_output with status "" not promoted`)
		assertWarnsContain(t, warns, "a missing status is not treated as completed")
	})

	t.Run("empty-and-unparsable", func(t *testing.T) {
		out, _, warns := respin(t, `{
			"model": "m",
			"input": [
				{"role": "user", "content": "go"},
				{"type": "tool_search_output", "status": "completed"},
				{"type": "tool_search_output", "status": "completed", "tools": 5}
			],
			"tools": [`+toolSearch+`]
		}`)
		if len(out.Tools) != 1 {
			t.Fatalf("tools = %v", toolNames(out.Tools))
		}
		assertWarnsContain(t, warns, "carries no tool; dropped")
		assertWarnsContain(t, warns, "not a tool array; dropped")
	})
}

// ---------- tool_choice ----------

func TestResponsesToRequest_ToolChoice(t *testing.T) {
	tools := `"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]`
	cases := []struct {
		name     string
		tc       string
		want     string
		wantWarn string
	}{
		{"auto", `"auto"`, `{"type":"auto"}`, ""},
		{"none", `"none"`, `{"type":"none"}`, ""},
		{"required", `"required"`, `{"type":"any"}`, ""},
		{"named-flat", `{"type":"function","name":"f"}`, `{"type":"tool","name":"f"}`, ""},
		{"named-legacy-nested", `{"type":"function","function":{"name":"f"}}`, `{"type":"tool","name":"f"}`, ""},
		{"named-custom", `{"type":"custom","name":"apply_patch"}`, `{"type":"tool","name":"apply_patch"}`, ""},
		{"object-auto", `{"type":"auto"}`, `{"type":"auto"}`, ""},
		{"allowed-tools", `{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"f"}]}`, `{"type":"auto"}`, "allowed_tools downgraded to auto"},
		{"unknown-string", `"whatever"`, `{"type":"auto"}`, "unknown tool_choice"},
		{"unknown-object", `{"type":"mcp"}`, `{"type":"auto"}`, `unknown tool_choice type "mcp"`},
		{"function-without-name", `{"type":"function"}`, `{"type":"auto"}`, "without name"},
		{"unparsable", `123`, `{"type":"auto"}`, "unparsable tool_choice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _, warns := respin(t, `{"model":"m",`+tools+`,"tool_choice":`+tc.tc+`,"input":"go"}`)
			if string(out.ToolChoice) != tc.want {
				t.Errorf("tool_choice = %s, want %s", out.ToolChoice, tc.want)
			}
			if tc.wantWarn != "" {
				assertWarnsContain(t, warns, tc.wantWarn)
			}
		})
	}

	t.Run("absent", func(t *testing.T) {
		out, _, _ := respin(t, `{"model":"m",`+tools+`,"input":"go"}`)
		if len(out.ToolChoice) != 0 {
			t.Errorf("tool_choice = %s, want unset", out.ToolChoice)
		}
	})

	t.Run("no-tools-means-no-tool-choice", func(t *testing.T) {
		out, _, warns := respin(t, `{"model":"m","tool_choice":"required","input":"go"}`)
		if len(out.ToolChoice) != 0 {
			t.Errorf("tool_choice = %s, want unset", out.ToolChoice)
		}
		assertWarnsContain(t, warns, "no tool survived conversion")
	})

	t.Run("all-tools-dropped-means-no-tool-choice", func(t *testing.T) {
		out, _, warns := respin(t, `{
			"model": "m",
			"input": "go",
			"tools": [{"type": "web_search"}],
			"tool_choice": {"type":"function","name":"f"}
		}`)
		if len(out.ToolChoice) != 0 || len(out.Tools) != 0 {
			t.Errorf("tool_choice = %s tools = %v", out.ToolChoice, toolNames(out.Tools))
		}
		assertWarnsContain(t, warns, "no tool survived conversion")
	})
}

// ---------- thinking ----------

func TestResponsesToRequest_ReasoningEffort(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"low", `{"effort":"low"}`, "low"},
		{"medium", `{"effort":"medium"}`, "medium"},
		{"high", `{"effort":"high"}`, "high"},
		{"minimal", `{"effort":"minimal"}`, "low"},
		// xhigh / max 原样透传（用户亲批：上游有模型支持；xhigh 依据为对上游的实测认知，
		// 无 A 级文档回执；minimal→low 是未验证的下映射）。
		{"xhigh", `{"effort":"xhigh"}`, "xhigh"},
		{"max", `{"effort":"max"}`, "max"},
		{"none", `{"effort":"none"}`, ""},
		{"empty", `{"effort":""}`, ""},
		{"no-effort", `{"summary":"auto"}`, ""},
		{"unknown", `{"effort":"turbo"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _, warns := respin(t, `{"model":"m","input":"go","reasoning":`+tc.raw+`}`)
			if tc.want == "" {
				if len(out.Thinking) != 0 {
					t.Fatalf("thinking = %s, want unset", out.Thinking)
				}
			} else {
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
			}
			if tc.name == "unknown" {
				assertWarnsContain(t, warns, `unknown reasoning_effort "turbo"`)
			}
			// summary 无上游对应能力：声明了就留痕
			if tc.name == "no-effort" {
				assertWarnsContain(t, warns, "reasoning.summary ignored")
			}
		})
	}
}

// ---------- 安全丢弃留痕 ----------

func TestResponsesToRequest_IgnoredFields(t *testing.T) {
	t.Run("warned", func(t *testing.T) {
		raw := `{"model":"m","input":"go","include":["reasoning.encrypted_content"],"text":{"format":{"type":"json_schema"}},` +
			`"parallel_tool_calls":false,"metadata":{"k":"v"},"store":true,"truncation":"auto"}`
		_, _, warns := respin(t, raw)
		// F18：文案必须与事实一致——签名经 respout 以 encrypted_content 恒下发，
		// 只有 logprobs 是真的拿不到。
		assertWarnsContain(t, warns,
			"info: include ignored (upstream returns no log probabilities; reasoning signatures are always sent back as encrypted_content")
		assertWarnsContain(t, warns, "info: parallel_tool_calls ignored")
		assertWarnsContain(t, warns, "info: metadata ignored")
		assertWarnsContain(t, warns, "info: truncation ignored")
		assertWarnsContain(t, warns, "store ignored")
		// text.format 是行为性丢失：客户端以为结构化输出生效，实际不会 → 不算良性
		assertWarnsContain(t, warns, "text.format/verbosity ignored")
		for _, w := range warns {
			if strings.Contains(w, "text.format/verbosity") && strings.HasPrefix(w, "info:") {
				t.Errorf("text.format 必须按警告计: %q", w)
			}
		}
	})

	t.Run("silent-when-absent", func(t *testing.T) {
		_, _, warns := respin(t, `{"model":"m","input":"go","store":false}`)
		if len(warns) != 0 {
			t.Errorf("unexpected warns: %v", warns)
		}
	})

	t.Run("probe-is-order-stable-and-tolerant", func(t *testing.T) {
		got := ResponsesIgnoredWarnings([]byte(`{"stream_options":{"include_usage":true},"user":"u","text":{}}`))
		want := []string{
			responsesIgnoredWarnReasons["user"],
			responsesIgnoredWarnReasons["text"],
			responsesIgnoredWarnReasons["stream_options"],
		}
		if !equalStrings(got, want) {
			t.Fatalf("warns = %v, want %v", got, want)
		}
		if ResponsesIgnoredWarnings([]byte(`{`)) != nil || ResponsesIgnoredWarnings(nil) != nil {
			t.Error("非法/空报文不应产出留痕")
		}
	})

	t.Run("plain-entry-still-works", func(t *testing.T) {
		// 不带报文的入口（签名固定）：归一化功能完整，只是没有安全丢弃留痕。
		var req types.ResponsesRequest
		if err := json.Unmarshal([]byte(`{"model":"m","input":"go","include":["x"]}`), &req); err != nil {
			t.Fatal(err)
		}
		out, _, warns, err := ResponsesToRequest(&req)
		if err != nil || out == nil {
			t.Fatalf("ResponsesToRequest: %v", err)
		}
		if len(warns) != 0 {
			t.Errorf("unexpected warns: %v", warns)
		}
	})

	t.Run("errors-propagate", func(t *testing.T) {
		if _, _, _, err := respinRaw(`{"model":"m","input":"go","previous_response_id":"r"}`); err != ErrResponsesPreviousResponseID {
			t.Errorf("err = %v", err)
		}
	})
}

// ---------- 端到端（Codex store:false 全量回放） ----------

// 一组真实故障形状：并行兄弟调用、结果之间插入的 user 通知、以及一个没回来的调用。
// 归一化产物必须满足配对三条不变式，且能无缝进入既有信封构造。
func TestResponsesToRequest_CodexHistoryRepair(t *testing.T) {
	out, mapping, warns := respin(t, `{
		"model": "gpt-5-codex",
		"instructions": "be terse",
		"tools": [
			{"type": "function", "name": "shell", "parameters": {"type": "object", "properties": {"cmd": {"type": "string"}}}},
			{"type": "custom", "name": "apply_patch", "format": {"syntax": "lark", "definition": "start: /.+/"}},
			{"type": "namespace", "name": "fs", "tools": [{"type": "function", "name": "read", "parameters": {"type": "object"}}]},
			{"type": "tool_search"}
		],
		"input": [
			{"role": "user", "content": [{"type": "input_text", "text": "run the tests"}]},
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "list files first"}], "encrypted_content": "gAAAAA"},
			{"type": "function_call", "call_id": "call_1", "name": "shell", "arguments": "{\"cmd\":\"ls\"}"},
			{"type": "function_call", "call_id": "call_2", "name": "shell", "arguments": "{\"cmd\":\"cat go.mod\"}"},
			{"type": "function_call", "call_id": "call_3", "name": "fs__read", "arguments": "{\"path\":\"a.go\"}"},
			{"role": "user", "content": "Approved command prefix saved"},
			{"role": "developer", "content": "<permissions>approved</permissions>"},
			{"type": "function_call_output", "call_id": "call_1", "output": "a.go\nmain.go"},
			{"type": "function_call_output", "call_id": "call_2", "output": [{"type": "input_text", "text": "module x"}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "I listed the files."}]}
		]
	}`)

	assertPairing(t, out.Messages) // call_3 没有结果 → 必须被清掉，其余保持合法

	roles := rolesOf(out.Messages)
	if !equalStrings(roles, []string{"user", "assistant", "user", "assistant"}) {
		t.Fatalf("roles = %v", roles)
	}
	asst := blocksOf(t, out.Messages[1])
	if got, want := blockTypes(asst), []string{"thinking", "tool_use", "tool_use"}; !equalStrings(got, want) {
		t.Fatalf("assistant blocks = %v, want %v", got, want)
	}
	if asst[0].Thinking != "list files first" {
		t.Errorf("thinking = %q", asst[0].Thinking)
	}
	res := blocksOf(t, out.Messages[2])
	var results []types.Block
	for _, b := range res {
		if b.Type == "tool_result" {
			results = append(results, b)
		}
	}
	if len(results) != 2 || results[0].ToolUseID != "call_1" || results[1].ToolUseID != "call_2" {
		t.Fatalf("results = %+v, want call_1 then call_2", results)
	}
	if !containsText(res, "Approved command prefix saved") {
		t.Error("interleaved notice lost")
	}
	assertWarnsContain(t, warns, "unanswered tool_use dropped")

	// developer item 并入 system，不占消息位。
	// 反序列化后比较：JSON 会把 <> 转义成 <（parseSystem 会还原，线上无损）。
	var sys string
	if err := json.Unmarshal(out.System, &sys); err != nil {
		t.Fatalf("system = %s (%v)", out.System, err)
	}
	if want := "be terse\n\n<permissions>approved</permissions>"; sys != want {
		t.Errorf("system = %q, want %q", sys, want)
	}

	// 工具族全部到位，降级信息可还原
	if got, want := toolNames(out.Tools), []string{"shell", "apply_patch", "fs__read", responsesToolSearchName}; !equalStrings(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	if !mapping.Custom["apply_patch"] || !mapping.ToolSearch {
		t.Errorf("mapping = %+v", mapping)
	}
	if mapping.Namespace["fs__read"] != (NamespacedName{Namespace: "fs", Name: "read"}) {
		t.Errorf("namespace mapping = %+v", mapping.Namespace)
	}
}

// 归一化产物直接进既有信封构造（组合性验证）。
func TestResponsesToRequest_ComposesWithBuildCcRequest(t *testing.T) {
	out, _, _ := respin(t, `{
		"model": "gpt-5-codex",
		"instructions": "sys",
		"max_output_tokens": 2048,
		"temperature": 0.2,
		"reasoning": {"effort": "high"},
		"tools": [{"type": "function", "name": "shell", "parameters": {"type": "object", "properties": {}}}],
		"input": [
			{"role": "user", "content": "go"},
			{"type": "function_call", "call_id": "call_1", "name": "shell", "arguments": "{\"cmd\":\"ls\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "a.go"}
		]
	}`)
	cc, warns := BuildCcRequest(out, testOpts())

	if cc.Params.Model != "gpt-5-codex" || cc.Params.MaxTokens != 2048 {
		t.Errorf("model/max_tokens = %s/%d", cc.Params.Model, cc.Params.MaxTokens)
	}
	if cc.Params.System != "sys" || cc.Params.ReasoningEffort != "high" {
		t.Errorf("system/effort = %q/%q", cc.Params.System, cc.Params.ReasoningEffort)
	}
	if len(cc.Params.Tools) != 1 || cc.Params.Tools[0].Name != "shell" {
		t.Errorf("tools = %+v", cc.Params.Tools)
	}
	// tool_result 必须被 convertUser 认出来（tool_use 与结果配对成功 → 有 tool 消息）
	var sawTool, sawUser bool
	for _, m := range cc.Params.Messages {
		switch m.Role {
		case "tool":
			sawTool = true
			if m.Content[0].ToolName != "shell" || m.Content[0].Output == nil || m.Content[0].Output.Value != "a.go" {
				t.Errorf("tool message = %+v", m.Content[0])
			}
		case "user":
			sawUser = true
		}
	}
	if !sawTool || !sawUser {
		t.Errorf("messages = %+v, want a tool message and a user message", cc.Params.Messages)
	}
	// F5：Responses 入站协议没有 cache_control 字段，信封断点由代理在末尾合成，
	// 故这里断言合成确实发生 + 只有这一条告警（旧断言「warns 为空」钉的是修复前的行为）。
	assertWarnsContain(t, warns, "no part-level cache_control present in the envelope")
	if len(warns) != 1 {
		t.Errorf("warns = %v, want exactly the synthesized-breakpoint notice", warns)
	}
	if m := envelopeTailMarker(cc); m == nil || m.Type != "ephemeral" {
		t.Errorf("tail cache_control marker = %+v, want a synthesized {type:ephemeral}", m)
	}
}
