package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/B1anYu/cmdc2api/internal/types"
)

// ---------- 可执行规格：三条不变式 ----------

// assertPairing 把 §4.4 的三条不变式写成可执行断言。任何一条被违反，
// 上游都会回 400（"tool_result ... must have a corresponding tool_use block
// in the previous message" / 未应答的 tool_use 直接拒收 / 角色必须交替）。
func assertPairing(t *testing.T, msgs []types.InboundMessage) {
	t.Helper()
	for i := range msgs {
		blocks := blocksOf(t, msgs[i])
		if i > 0 && msgs[i-1].Role == msgs[i].Role {
			t.Errorf("invariant 3 violated: consecutive %s messages at index %d", msgs[i].Role, i)
		}
		for _, b := range blocks {
			switch b.Type {
			case "tool_result":
				if i == 0 {
					t.Errorf("invariant 1 violated: tool_result %s has no preceding message", b.ToolUseID)
					continue
				}
				if !hasToolUse(blocksOf(t, msgs[i-1]), b.ToolUseID) {
					t.Errorf("invariant 1 violated: tool_result %s has no matching tool_use in the immediately preceding %s message",
						b.ToolUseID, msgs[i-1].Role)
				}
			case "tool_use":
				if i+1 >= len(msgs) {
					t.Errorf("invariant 2 violated: tool_use %s has no following message", b.ID)
					continue
				}
				if !hasToolResult(blocksOf(t, msgs[i+1]), b.ID) {
					t.Errorf("invariant 2 violated: tool_use %s is not answered in the immediately following %s message",
						b.ID, msgs[i+1].Role)
				}
			}
		}
	}
}

func hasToolUse(blocks []types.Block, id string) bool {
	for _, b := range blocks {
		if b.Type == "tool_use" && b.ID == id {
			return true
		}
	}
	return false
}

func hasToolResult(blocks []types.Block, id string) bool {
	for _, b := range blocks {
		if b.Type == "tool_result" && b.ToolUseID == id {
			return true
		}
	}
	return false
}

// toolResultOf 取指定 id 的 tool_result 块（不存在返回 nil）。
func toolResultOf(blocks []types.Block, id string) *types.Block {
	for i := range blocks {
		if blocks[i].Type == "tool_result" && blocks[i].ToolUseID == id {
			return &blocks[i]
		}
	}
	return nil
}

func blocksOf(t *testing.T, m types.InboundMessage) []types.Block {
	t.Helper()
	blocks, err := parseBlocks(m.Content)
	if err != nil {
		t.Fatalf("parse blocks of %s message: %v", m.Role, err)
	}
	return blocks
}

func rolesOf(msgs []types.InboundMessage) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}

// ---------- 构造助手 ----------

func rawJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func pmsg(role string, blocks ...types.Block) types.InboundMessage {
	return types.InboundMessage{Role: role, Content: InboundBlocksContent(blocks)}
}

func ptext(s string) types.Block { return types.Block{Type: "text", Text: s} }

func puse(id string) types.Block {
	return types.Block{Type: "tool_use", ID: id, Name: "exec", Input: json.RawMessage(`{}`)}
}

func presult(id, out string) types.Block {
	return types.Block{Type: "tool_result", ToolUseID: id, Content: rawJSON(out)}
}

// repair 执行配对修复并断言不变式（全部用例的统一入口）。
func repair(t *testing.T, msgs ...types.InboundMessage) ([]types.InboundMessage, []string) {
	t.Helper()
	out, warns := RepairToolPairing(msgs)
	assertPairing(t, out)
	return out, warns
}

func assertWarnsContain(t *testing.T, warns []string, substr string) {
	t.Helper()
	for _, w := range warns {
		if strings.Contains(w, substr) {
			return
		}
	}
	t.Errorf("warns %v missing %q", warns, substr)
}

// ---------- 场景 ----------

// 开发者通知插队在 call 与 output 之间：这是生产环境 400 的原始形状
// （"tool_result ... must have a corresponding tool_use block in the previous message"）。
// 结果必须被搬到调用之后，通知文本原位保留。
func TestRepairToolPairing_DeveloperNoticeBetween(t *testing.T) {
	out, warns := repair(t,
		pmsg("user", ptext("do it")),
		pmsg("assistant", puse("call_A")),
		pmsg("user", ptext("Approved command prefix saved")),
		pmsg("user", presult("call_A", "ok")),
	)

	if got, want := rolesOf(out), []string{"user", "assistant", "user"}; !equalStrings(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	if !hasToolResult(blocksOf(t, out[2]), "call_A") {
		t.Error("tool_result not re-emitted immediately after its call")
	}
	if !containsText(blocksOf(t, out[2]), "Approved command prefix saved") {
		t.Error("interleaved notice text lost")
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warns: %v", warns)
	}
}

// 并行的两个调用都拿到结果：必须聚成同一条 assistant 消息 + 紧随其后的
// 一条 user 消息（结果按调用声明顺序）。
func TestRepairToolPairing_ParallelBothAnswered(t *testing.T) {
	out, _ := repair(t,
		pmsg("user", ptext("features?")),
		pmsg("assistant", puse("call_c0")),
		pmsg("assistant", puse("call_c1")),
		pmsg("user", presult("call_c0", "log")),
		pmsg("user", presult("call_c1", "tags")),
	)

	if got, want := rolesOf(out), []string{"user", "assistant", "user"}; !equalStrings(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	if !hasToolUse(blocksOf(t, out[1]), "call_c0") || !hasToolUse(blocksOf(t, out[1]), "call_c1") {
		t.Error("parallel tool_use blocks must share one assistant message")
	}
	res := blocksOf(t, out[2])
	if len(res) != 2 || res[0].ToolUseID != "call_c0" || res[1].ToolUseID != "call_c1" {
		t.Errorf("results not re-emitted in call order: %+v", res)
	}
}

// 并行调用里有一个结果没回来：未应答的那个必须被丢弃，剩下的仍然合法。
func TestRepairToolPairing_ParallelOneUnanswered(t *testing.T) {
	out, warns := repair(t,
		pmsg("user", ptext("q")),
		pmsg("assistant", puse("call_A"), puse("call_B")),
		pmsg("user", presult("call_A", "oa")),
	)

	if got, want := rolesOf(out), []string{"user", "assistant", "user"}; !equalStrings(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	if hasToolUse(blocksOf(t, out[1]), "call_B") {
		t.Error("unanswered tool_use must be dropped")
	}
	if !hasToolUse(blocksOf(t, out[1]), "call_A") {
		t.Error("answered tool_use must survive")
	}
	assertWarnsContain(t, warns, "unanswered tool_use dropped")
}

// 孤儿 tool_result（全历史都没有对应 tool_use）：丢弃并留痕。
func TestRepairToolPairing_OrphanToolResult(t *testing.T) {
	out, warns := repair(t,
		pmsg("user", ptext("q")),
		pmsg("user", presult("call_ghost", "stale")),
		pmsg("assistant", ptext("hi")),
	)

	for _, m := range out {
		if hasToolResult(blocksOf(t, m), "call_ghost") {
			t.Error("orphan tool_result must be dropped")
		}
	}
	assertWarnsContain(t, warns, "orphan tool_result dropped")
}

// 同一 tool_use_id 出现多份 tool_result（历史被压缩后的常见形态）：后者胜，
// 被覆盖的那份必须留痕且同 id 只报一次（长历史反复压缩会累积大量重复，不能刷屏）。
func TestRepairToolPairing_DuplicateToolResult(t *testing.T) {
	out, warns := repair(t,
		pmsg("user", ptext("q")),
		pmsg("assistant", puse("call_D")),
		pmsg("user", presult("call_D", "stale")),
		pmsg("user", presult("call_D", "fresh")),
		pmsg("user", presult("call_D", "newest")),
	)

	n := 0
	for _, w := range warns {
		if strings.Contains(w, "duplicate tool_result dropped") {
			n++
			if !strings.Contains(w, "call_D") {
				t.Errorf("留痕缺 id: %s", w)
			}
		}
	}
	if n != 1 {
		t.Errorf("duplicate 留痕 %d 条, want 1: %v", n, warns)
	}
	for _, m := range out {
		if b := toolResultOf(blocksOf(t, m), "call_D"); b != nil && string(b.Content) != `"newest"` {
			t.Errorf("tool_result content = %s, want 后者胜（\"newest\"）", b.Content)
		}
	}
}

// 悬空调用：没有结果回来的 tool_use 整块丢弃；消息里还有其它内容时降级为文本消息，
// 一条都没剩时整条丢弃。两种情况都不能留下「assistant 后面没有 user」的断尾。
func TestRepairToolPairing_DanglingCall(t *testing.T) {
	t.Run("keeps-other-content", func(t *testing.T) {
		out, warns := repair(t,
			pmsg("user", ptext("q")),
			pmsg("assistant", ptext("sorry"), puse("call_X")),
		)
		if got, want := rolesOf(out), []string{"user", "assistant"}; !equalStrings(got, want) {
			t.Fatalf("roles = %v, want %v", got, want)
		}
		blocks := blocksOf(t, out[1])
		if len(blocks) != 1 || blocks[0].Type != "text" {
			t.Errorf("dangling call's text content must survive alone: %+v", blocks)
		}
		assertWarnsContain(t, warns, "unanswered tool_use dropped")
	})

	t.Run("drops-empty-assistant", func(t *testing.T) {
		out, _ := repair(t,
			pmsg("user", ptext("q")),
			pmsg("assistant", puse("call_X")),
		)
		if got, want := rolesOf(out), []string{"user"}; !equalStrings(got, want) {
			t.Fatalf("roles = %v, want %v", got, want)
		}
	})
}

// 顺序调用被插队切开：两轮调用都要完整保留，且各自的结果紧跟各自的调用。
func TestRepairToolPairing_SequentialCallsSeparated(t *testing.T) {
	out, warns := repair(t,
		pmsg("user", ptext("start")),
		pmsg("assistant", puse("call_A")),
		pmsg("user", ptext("some notice")),
		pmsg("user", presult("call_A", "ok")),
		pmsg("assistant", ptext("next"), puse("call_B")),
		pmsg("user", presult("call_B", "ok2")),
	)

	if got, want := rolesOf(out), []string{"user", "assistant", "user", "assistant", "user"}; !equalStrings(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	for i, m := range out {
		if hasToolUse(blocksOf(t, m), "call_A") && !hasToolResult(blocksOf(t, out[i+1]), "call_A") {
			t.Error("call_A lost its adjacency")
		}
		if hasToolUse(blocksOf(t, m), "call_B") && !hasToolResult(blocksOf(t, out[i+1]), "call_B") {
			t.Error("call_B lost its adjacency")
		}
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warns: %v", warns)
	}
}

// 已经合法的序列（含消息级 reasoning_content）必须原样通过，不被无谓改写。
func TestRepairToolPairing_ValidSequenceUntouched(t *testing.T) {
	in := []types.InboundMessage{
		pmsg("user", ptext("q")),
		{Role: "assistant", Content: InboundBlocksContent([]types.Block{puse("call_A")}), ReasoningContent: "think"},
		pmsg("user", presult("call_A", "ok")),
	}
	out, warns := RepairToolPairing(in)
	assertPairing(t, out)

	if len(warns) != 0 {
		t.Errorf("unexpected warns: %v", warns)
	}
	if len(out) != 3 || out[1].ReasoningContent != "think" {
		t.Errorf("valid sequence was rewritten: %+v", out)
	}
}

// 无法解析的 content 消息整条丢弃并留痕；空消息静默丢弃（客户端常见空壳）。
func TestRepairToolPairing_EmptyAndUnparsableMessages(t *testing.T) {
	out, warns := repair(t,
		types.InboundMessage{Role: "user", Content: json.RawMessage(`{}`)},
		types.InboundMessage{Role: "user", Content: nil},
		pmsg("user", ptext("q")),
		pmsg("assistant", ptext("a")),
	)
	if got, want := rolesOf(out), []string{"user", "assistant"}; !equalStrings(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	assertWarnsContain(t, warns, "content is neither string nor block array")
}

// 字符串形态 content 也要能被解析（入站兼容写法）。
func TestRepairToolPairing_StringContentParsed(t *testing.T) {
	out, _ := repair(t,
		types.InboundMessage{Role: "user", Content: rawJSON("plain text")},
		types.InboundMessage{Role: "assistant", Content: rawJSON("answer")},
	)
	if len(out) != 2 {
		t.Fatalf("messages = %d, want 2", len(out))
	}
	if !containsText(blocksOf(t, out[0]), "plain text") {
		t.Error("string content must be parsed into a text block")
	}
}

// 同一 tool_use id 出现两次结果：后者胜（历史被压缩后会出现重复结果）。
func TestRepairToolPairing_DuplicateResultLastWins(t *testing.T) {
	out, _ := repair(t,
		pmsg("user", ptext("q")),
		pmsg("assistant", puse("call_A")),
		pmsg("user", presult("call_A", "first")),
		pmsg("user", presult("call_A", "second")),
	)
	res := blocksOf(t, out[2])
	if len(res) != 1 {
		t.Fatalf("results = %d, want 1", len(res))
	}
	if string(res[0].Content) != `"second"` {
		t.Errorf("content = %s, want the last result", res[0].Content)
	}
}

// 完全没有 tool_use_id 的 tool_result：无法归位，丢弃并留痕。
func TestRepairToolPairing_MissingToolUseID(t *testing.T) {
	out, warns := repair(t,
		pmsg("user", ptext("q")),
		pmsg("user", types.Block{Type: "tool_result", Content: rawJSON("void")}),
		pmsg("assistant", ptext("a")),
	)
	if got, want := rolesOf(out), []string{"user", "assistant"}; !equalStrings(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	assertWarnsContain(t, warns, "tool_result without tool_use_id dropped")
}

// 合并连续同角色消息时，消息级 reasoning_content 也要接续（不能丢后半段思考）。
func TestRepairToolPairing_MergeKeepsReasoning(t *testing.T) {
	out, _ := RepairToolPairing([]types.InboundMessage{
		{Role: "assistant", Content: InboundBlocksContent([]types.Block{ptext("a")}), ReasoningContent: "first"},
		{Role: "assistant", Content: InboundBlocksContent([]types.Block{ptext("b")}), ReasoningContent: "second"},
	})
	assertPairing(t, out)
	if len(out) != 1 {
		t.Fatalf("messages = %d, want 1 merged assistant message", len(out))
	}
	if out[0].ReasoningContent != "first\n\nsecond" {
		t.Errorf("reasoning = %q, want both halves", out[0].ReasoningContent)
	}
	if len(blocksOf(t, out[0])) != 2 {
		t.Errorf("blocks = %+v, want both kept in order", blocksOf(t, out[0]))
	}
}

// ---------- 小工具 ----------

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

func containsText(blocks []types.Block, text string) bool {
	for _, b := range blocks {
		if b.Type == "text" && b.Text == text {
			return true
		}
	}
	return false
}
