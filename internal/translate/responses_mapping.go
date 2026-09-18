package translate

// ResponsesToolMapping 记录 Responses 入站工具族（custom / tool_search / namespace）
// 被降级/摊平为普通 function 工具时的还原信息，由 respin（入站归一化）产出、
// respout（出站编码）消费：出站看到 function_call 的工具名命中映射时，
// 需还原为 Codex 期望的 custom_tool_call / tool_search_call / 带 namespace 的 item 形态，
// 否则 Codex 回放历史时会因 item 形态与声明不符而中止 turn。
type ResponsesToolMapping struct {
	// Custom 被降级为「单 input:string 参数 function 工具」的 custom 工具名集合。
	Custom map[string]bool
	// ToolSearch 是否声明了 tool_search 代理工具（出站需还原为 tool_search_call item）。
	ToolSearch bool
	// Namespace 摊平名（ns__child）→ 原始命名空间与子工具名。
	Namespace map[string]NamespacedName
}

// NamespacedName namespace 工具摊平前的原始名称。
type NamespacedName struct {
	Namespace string
	Name      string
}
