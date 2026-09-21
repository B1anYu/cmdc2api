package translate

import (
	"encoding/json"
	"fmt"

	"github.com/B1anYu/cmdc2api/internal/types"
)

// ---------- tool_use / tool_result 配对修复（Chat / Responses 共用） ----------
//
// Anthropic 对消息序列有三条硬约束，违反任何一条上游都会回 400：
//  1. 每个 tool_result 的 tool_use_id 必须在**紧邻前一条** assistant 消息里有对应 tool_use；
//  2. 每个 tool_use 必须在**紧邻下一条** user 消息里被 tool_result 应答（悬空调用会被拒收）；
//  3. user / assistant 必须严格交替。
//
// 逐条转换天然违反它们：OpenAI 风格的客户端（尤其 store:false 每轮重发全量历史的那种）
// 会在一对 call/output 之间插入别的 item——审批通知、开发者提示、兄弟并行调用的结果，
// 或者某个并行调用的结果压根没回来。修复算法分三步：
//
//	merge → pair → merge
//
// 先合并连续同角色消息（把并行调用及其结果聚到一起），再重建配对（结果按调用声明顺序
// 重发射到 call 之后），最后再合并一次收拢配对阶段切开的轮次。
//
// 本文件是共享实现：Chat 与 Responses 入站归一化产物形状一致（都是 Anthropic 形状消息），
// 因此只维护这一份算法，避免两条入站路径的正确性分叉。

// RepairToolPairing 修复消息序列的 tool_use/tool_result 配对并恢复角色交替。
// 被丢弃的内容记入返回的 warns，供调用方按 info:/warn 分级打日志。
func RepairToolPairing(msgs []types.InboundMessage) ([]types.InboundMessage, []string) {
	var warns []string

	work, w := parseInboundMessages(msgs)
	warns = append(warns, w...)

	work = mergeSameRole(work)
	work, w = pairToolCalls(work)
	warns = append(warns, w...)
	work = mergeSameRole(work)

	return buildInboundMessages(work), warns
}

// InboundBlocksContent 把块数组序列化为 InboundMessage.Content。
// 归一化产物统一用块数组形态（string 形态只作为入站解析时的兼容写法被接受）。
func InboundBlocksContent(blocks []types.Block) json.RawMessage {
	b, err := json.Marshal(blocks)
	if err != nil {
		return json.RawMessage(`[]`)
	}
	return b
}

// pairingMessage 配对修复的工作形态：角色 + 已解析的块序列。
// 在解析后的块上做增删，比反复 marshal/unmarshal RawMessage 清晰且不易漏字段。
type pairingMessage struct {
	role      string
	blocks    []types.Block
	reasoning string
}

// parseInboundMessages 解析各消息的 content（string | []block），无法解析的消息整条丢弃。
func parseInboundMessages(msgs []types.InboundMessage) ([]pairingMessage, []string) {
	var warns []string
	out := make([]pairingMessage, 0, len(msgs))
	for i := range msgs {
		blocks, err := parseBlocks(msgs[i].Content)
		if err != nil {
			warns = append(warns, fmt.Sprintf(
				"message %d (role %s) dropped: content is neither string nor block array", i, msgs[i].Role))
			continue
		}
		if len(blocks) == 0 {
			continue
		}
		out = append(out, pairingMessage{role: msgs[i].Role, blocks: blocks, reasoning: msgs[i].ReasoningContent})
	}
	return out, warns
}

// mergeSameRole 合并连续同角色消息（块按原顺序拼接）。
// 只合并角色完全相同的相邻消息：未知角色原样保留，不与 user/assistant 混并。
//
// 这是刻意的中立：未知 role 的兜底（降级为 user）是**入站层**的职责，两条入站都已在
// 归一化时做完（chatin.go 的 role switch default 分支、respin.go 的同名分支），
// 因此本文件不该再持有一份可能与之分叉的角色策略。
func mergeSameRole(msgs []pairingMessage) []pairingMessage {
	out := make([]pairingMessage, 0, len(msgs))
	for _, m := range msgs {
		if n := len(out); n > 0 && out[n-1].role == m.role {
			prev := &out[n-1]
			blocks := make([]types.Block, 0, len(prev.blocks)+len(m.blocks))
			blocks = append(blocks, prev.blocks...)
			blocks = append(blocks, m.blocks...)
			prev.blocks = blocks
			if m.reasoning != "" {
				if prev.reasoning == "" {
					prev.reasoning = m.reasoning
				} else {
					prev.reasoning += "\n\n" + m.reasoning
				}
			}
			continue
		}
		out = append(out, m)
	}
	return out
}

// pairToolCalls 重建配对：
//   - assistant 消息只保留被应答的 tool_use（未应答的悬空调用丢弃并留痕；
//     一条都没剩时整条丢弃，除非还有非工具内容），并按声明顺序把对应结果
//     重发射为紧随其后的 user 消息；
//   - user 消息原位剥离 tool_result（它们已在 call 之后重发射），
//     剥空的消息整条丢弃；没有对应 tool_use 的孤儿结果丢弃并留痕。
func pairToolCalls(msgs []pairingMessage) ([]pairingMessage, []string) {
	var warns []string

	// 结果索引：重复 id 以后者胜（历史被压缩后可能出现同 id 的多份结果）。
	// 被覆盖的那份内容就此消失，必须留痕（同 id 只报一次，避免长历史刷屏）。
	results := make(map[string]types.Block)
	dupWarned := make(map[string]bool)
	// 声明集合：用于区分「孤儿结果」与「结果比调用先到」——只有全历史都没有该 tool_use
	// 才算孤儿，否则它只是位置不对，配对阶段会把它搬到调用之后。
	declared := make(map[string]bool)
	for _, m := range msgs {
		for _, b := range m.blocks {
			switch {
			case m.role == "user" && b.Type == "tool_result" && b.ToolUseID != "":
				if _, dup := results[b.ToolUseID]; dup && !dupWarned[b.ToolUseID] {
					dupWarned[b.ToolUseID] = true
					warns = append(warns, "duplicate tool_result dropped (later result wins): "+b.ToolUseID)
				}
				results[b.ToolUseID] = b
			case m.role == "assistant" && b.Type == "tool_use" && b.ID != "":
				declared[b.ID] = true
			}
		}
	}

	warns = append(warns, orphanWarnings(msgs, declared)...)

	out := make([]pairingMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m.role {
		case "assistant":
			out = append(out, pairAssistant(m, results, &warns)...)
		case "user":
			out = append(out, stripToolResults(m)...)
		default:
			// 未知角色原样保留（本文件对角色策略保持中立，见 mergeSameRole 的注释）。
			// 从两条入站（chatin.go / respin.go）来的消息在这里都已降级为 user，
			// 故本分支当前不可达；它保留为直调入口（RepairToolPairing 是导出函数）的安全出口——
			// 注意不能在此就地改写成 user：结果索引按 role=="user" 收集 tool_result，
			// 在派发阶段才改角色会让该消息里的 tool_result 既不被索引也不被剥离，
			// 破坏不变式 1（结果必须紧邻其调用）。
			out = append(out, m)
		}
	}
	return out, warns
}

// orphanWarnings 为无对应 tool_use 的 tool_result 生成留痕（同 id 只报一次）。
func orphanWarnings(msgs []pairingMessage, declared map[string]bool) []string {
	var warns []string
	seen := make(map[string]bool)
	for _, m := range msgs {
		if m.role != "user" {
			continue
		}
		for _, b := range m.blocks {
			if b.Type != "tool_result" {
				continue
			}
			if b.ToolUseID == "" {
				if !seen[""] {
					seen[""] = true
					warns = append(warns, "tool_result without tool_use_id dropped")
				}
				continue
			}
			if declared[b.ToolUseID] || seen[b.ToolUseID] {
				continue
			}
			seen[b.ToolUseID] = true
			warns = append(warns, "orphan tool_result dropped (no matching tool_use in history): "+b.ToolUseID)
		}
	}
	return warns
}

// pairAssistant 处理单条 assistant 消息：非工具内容原位保留，被应答的 tool_use
// 排在后面（对齐 CC CLI 抓包的 [thinking, text, tool-call] 次序），
// 对应结果作为紧随其后的 user 消息重发射。
func pairAssistant(m pairingMessage, results map[string]types.Block, warns *[]string) []pairingMessage {
	var toolUses, others []types.Block
	for _, b := range m.blocks {
		if b.Type == "tool_use" {
			toolUses = append(toolUses, b)
			continue
		}
		others = append(others, b)
	}
	if len(toolUses) == 0 {
		return []pairingMessage{m}
	}

	kept := make([]types.Block, 0, len(toolUses))
	for _, b := range toolUses {
		if _, ok := results[b.ID]; ok {
			kept = append(kept, b)
			continue
		}
		*warns = append(*warns, "unanswered tool_use dropped (no matching tool_result in history): "+b.ID)
	}

	if len(kept) == 0 {
		// 全部未应答：把这条消息降级为纯文本消息，没有任何内容可留则整条丢弃
		if len(others) == 0 {
			return nil
		}
		return []pairingMessage{{role: "assistant", blocks: others, reasoning: m.reasoning}}
	}

	blocks := make([]types.Block, 0, len(others)+len(kept))
	blocks = append(blocks, others...)
	blocks = append(blocks, kept...)

	res := pairingMessage{role: "user", blocks: make([]types.Block, 0, len(kept))}
	for _, b := range kept {
		res.blocks = append(res.blocks, results[b.ID])
	}
	return []pairingMessage{{role: "assistant", blocks: blocks, reasoning: m.reasoning}, res}
}

// stripToolResults 剥离 user 消息里的 tool_result（已在调用侧重发射），
// 其余内容原位保留；剥空则整条丢弃。
func stripToolResults(m pairingMessage) []pairingMessage {
	kept := make([]types.Block, 0, len(m.blocks))
	hadResult := false
	for _, b := range m.blocks {
		if b.Type == "tool_result" {
			hadResult = true
			continue
		}
		kept = append(kept, b)
	}
	if !hadResult {
		return []pairingMessage{m}
	}
	if len(kept) == 0 {
		return nil
	}
	return []pairingMessage{{role: m.role, blocks: kept, reasoning: m.reasoning}}
}

// buildInboundMessages 把工作形态序列化回入站消息；空消息在这里做最后一道兜底丢弃。
func buildInboundMessages(msgs []pairingMessage) []types.InboundMessage {
	out := make([]types.InboundMessage, 0, len(msgs))
	for _, m := range msgs {
		if len(m.blocks) == 0 {
			continue
		}
		out = append(out, types.InboundMessage{
			Role:             m.role,
			Content:          InboundBlocksContent(m.blocks),
			ReasoningContent: m.reasoning,
		})
	}
	return out
}
