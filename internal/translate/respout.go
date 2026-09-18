package translate

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/B1anYu/cmdc2api/internal/masq"
	"github.com/B1anYu/cmdc2api/internal/types"
)

// Responses 出站：把规范格式（Anthropic 形状）的 StreamEvent 序列编码为 Responses SSE 事件，
// 或折叠为非流式响应对象。两种形态共用同一状态机，语义只有一处实现
// （三处分裂的实现是本项目明确避免的反面教材）。
//
// 本文件只做「事件到事件」的搬运：所有线上载荷（事件 map、item map、usage）都由
// types 包的 wire 构造器生成，字段存在性规则不允许在这里复制一份。

// responsesArgChunkSize 工具参数分片粒度（按 rune 计）。上游一次性给出完整 JSON 参数，
// 拆片只为还原 OpenAI 的增量节奏；客户端按序拼接，语义与分片无关。
const responsesArgChunkSize = 10

// responsesToolSearchName tool_search 代理工具降级后的 function 名（与入站归一化的降级名一致）。
const responsesToolSearchName = "tool_search"

// item id 前缀。function/custom/tool_search 的 id 由前缀 + 上游 call id 构成，
// 保证同一工具调用在流式与非流式、跨轮次之间都是同一个可回放标识。
const (
	responsesIDPrefix               = "resp_"
	responsesMessageIDPrefix        = "msg_"
	responsesFunctionCallIDPrefix   = "fc_"
	responsesCustomToolCallIDPrefix = "ctc_"
	responsesToolSearchCallIDPrefix = "tsc_"
)

// 停止原因与截断原因取值（协议事实：Anthropic stop_reason 与 Responses incomplete_details.reason）。
const (
	stopReasonMaxTokens = "max_tokens"
	stopReasonRefusal   = "refusal"

	responsesReasonMaxOutputTokens = "max_output_tokens"
	responsesReasonContentFilter   = "content_filter"
)

// ResponsesEcho 终态 Response 对象要回显的请求字段。
// Codex 对响应对象做浅校验（模型名、工具声明、采样参数要与请求一致），
// 缺失会让它判定响应与请求不符，因此由 handler 从入站请求原样取值后经 SetEcho 注入。
//
// Tools/ToolChoice 必须是客户端原始声明（Responses 扁平形状），
// 绝不能回显归一化后的 Anthropic 形状，否则严格客户端解析回显对象时失败。
type ResponsesEcho struct {
	Instructions      string
	Tools             []types.ResponsesTool
	ToolChoice        json.RawMessage
	Temperature       *float64
	TopP              *float64
	MaxOutputTokens   *int
	ParallelToolCalls bool
}

// ---------- 核心状态机 ----------

// responsesOut 是 Responses 出站的核心状态机：吃 Anthropic 形状的 StreamEvent，
// 吐 Responses 形状的 StreamEvent。编码器与聚合器都只是它的外壳。
//
// 三条流式形状硬约束（Codex 是严格客户端，违反任一条都会被拒收或解析崩溃）：
//   - 每个 output_item.added 必有同 id、同 output_index 的 output_item.done；
//   - content_part.added 早于该 part 的首个 output_text.delta，content_index 只在 part 关闭时前进；
//   - sequence_number 从 0 起对每个已发事件（含终态）单调递增。
type responsesOut struct {
	responseID string
	model      string
	createdAt  int64
	echo       ResponsesEcho
	mapping    ResponsesToolMapping // 值拷贝：nil 映射与空映射语义相同，各处读取无需判空

	seq         int // 下一个 sequence_number
	outputIndex int // 下一个 output_index，同时是当前打开 item 的槽位（同一时刻只有一个 item 打开）
	output      []types.ResponsesOutputItem
	open        *responsesOpenItem

	stopReason string
	usage      types.DeltaUsage
	ccTotal    int // 最近一次 message_delta 的缓存写入量（快照语义，见 captureMeta）
	ccSeen     bool

	started    bool // 已发出 response.created
	terminated bool // 已发出终态事件：同时是 Finish 幂等与「终态后不再接受输入」的依据
	failed     bool
	errCode    string
	errMsg     string

	messageSeq int // 已宣告的 message item 数，用于生成确定性的 item id
}

// responsesOpenItem 一个已宣告 added、尚未 done 的 item 的累积状态。
// 字段按 kind 分组使用：只有 message 用 part 系列，只有 reasoning 用思考系列，等等。
type responsesOpenItem struct {
	kind  string // types.ResponsesItem*
	index int    // output_index
	id    string // message 自生成；工具类为前缀 + call id；reasoning 无 id

	// message
	contentIndex int
	partOpen     bool
	partText     strings.Builder
	parts        []types.ResponsesOutputTextPart

	// reasoning：摘要全文与来自 signature_delta 的思考签名
	thinking  strings.Builder
	signature string

	// 工具调用：call id 原样回传，name/namespace 为还原后的形态，arguments 为累积的上游参数
	callID       string
	name         string
	namespace    string
	arguments    strings.Builder
	unwrapped    string // custom：已解包的自由文本全文
	inputEmitted int    // custom：已作为 delta 发出的字节数
}

func newResponsesOut(model, responseID string, mapping *ResponsesToolMapping, echo ResponsesEcho) *responsesOut {
	if model == "" {
		model = DefaultModel
	}
	return &responsesOut{
		responseID: responseID,
		model:      model,
		createdAt:  time.Now().Unix(),
		echo:       echo,
		mapping:    responsesMappingValue(mapping),
	}
}

// responsesMappingValue 把可空映射表收敛成值：nil 与空表语义相同（无降级工具需要还原）。
func responsesMappingValue(m *ResponsesToolMapping) ResponsesToolMapping {
	if m == nil {
		return ResponsesToolMapping{}
	}
	return *m
}

// nextSeq 分配下一个 sequence_number。所有对外事件（含终态）都必须经它取号。
func (o *responsesOut) nextSeq() int {
	seq := o.seq
	o.seq++
	return seq
}

// feed 消费一条上游事件，返回 0..N 条 Responses 事件。
func (o *responsesOut) feed(ev types.StreamEvent) []types.StreamEvent {
	if o.terminated {
		// 终态之后一律丢弃：管线在流内已报错时仍会无条件调用 Finish，
		// 幂等必须由状态机自己保证，不能指望调用方。
		return nil
	}
	switch d := ev.Data.(type) {
	case types.MessageStartEvent:
		return o.start()
	case types.ContentBlockStartEvent:
		return o.startBlock(d)
	case types.ContentBlockDeltaEvent:
		return o.blockDelta(d)
	case types.ContentBlockStopEvent:
		return o.closeBlock()
	case types.MessageDeltaEvent:
		o.captureMeta(d)
		return nil
	case types.MessageStopEvent:
		return o.finish()
	case types.ErrorEvent:
		return o.fail(d.Error.Type, d.Error.Message)
	default:
		// 未知事件（规范格式未来新增的类型）静默忽略：输出形状由白名单保证
		return nil
	}
}

// start 处理 message_start：宣告流已建立（in_progress + 空 output），并跟随一个 in_progress 心跳。
func (o *responsesOut) start() []types.StreamEvent {
	if o.started {
		return nil
	}
	o.started = true
	resp := o.response(types.ResponsesStatusInProgress)
	created := types.NewResponsesCreated(o.nextSeq(), resp)
	inProgress := types.NewResponsesInProgress(o.nextSeq(), resp)
	return []types.StreamEvent{created, inProgress}
}

// startBlock 处理 content_block_start。
func (o *responsesOut) startBlock(d types.ContentBlockStartEvent) []types.StreamEvent {
	switch b := d.ContentBlock.(type) {
	case types.TextBlockStart:
		return o.startTextPart()
	case types.ThinkingBlockStart:
		// 开新 item 前必关旧 item：thinking 到来时不关，message item 的文本会被覆盖丢失
		// （表现为「上游成功但客户端拿到空输出」）。
		out := o.closeOpen()
		open := &responsesOpenItem{kind: types.ResponsesItemReasoning, index: o.outputIndex}
		o.open = open
		out = append(out, types.NewResponsesOutputItemAdded(
			o.nextSeq(), open.index, types.NewResponsesReasoningItem("")))
		out = append(out, types.NewResponsesReasoningSummaryPartAdded(
			o.nextSeq(), open.index, 0, ""))
		return out
	case types.ToolUseBlockStart:
		return o.startToolCall(b)
	default:
		return nil
	}
}

// startTextPart 开启一个 output_text part。
// 当前打开的若已是 message item，说明同一 message 内还有下一个 text 块（交错场景），
// 复用该 item 只补 part 事件；否则先关旧 item 再宣告新的 message item，content_index 归 0。
func (o *responsesOut) startTextPart() []types.StreamEvent {
	var out []types.StreamEvent
	if o.open == nil || o.open.kind != types.ResponsesItemMessage {
		out = append(out, o.closeOpen()...)
		o.open = &responsesOpenItem{
			kind:  types.ResponsesItemMessage,
			index: o.outputIndex,
			id:    o.nextMessageItemID(),
		}
		out = append(out, types.NewResponsesOutputItemAdded(o.nextSeq(), o.open.index,
			types.NewResponsesMessageItem(o.open.id, types.ResponsesStatusInProgress)))
	} else {
		// 上一个 text 块没走 stop 就来了新块（理论上不该发生）：先收尾，保证 part 生命周期成对
		out = append(out, o.closeTextPart()...)
	}
	open := o.open
	open.partOpen = true
	out = append(out, types.NewResponsesContentPartAdded(
		o.nextSeq(), open.index, open.contentIndex, open.id))
	return out
}

// startToolCall 开启一个工具调用 item，按入站降级映射还原出 Codex 期望的 item 类型。
func (o *responsesOut) startToolCall(b types.ToolUseBlockStart) []types.StreamEvent {
	out := o.closeOpen()
	callID := responsesToolCallID(b.ID)
	open := &responsesOpenItem{kind: types.ResponsesItemFunctionCall, index: o.outputIndex, callID: callID}
	switch {
	case o.mapping.ToolSearch && b.Name == responsesToolSearchName:
		open.kind = types.ResponsesItemToolSearchCall
		open.id = responsesToolSearchCallIDPrefix + callID
	case o.mapping.Custom[b.Name]:
		open.kind = types.ResponsesItemCustomToolCall
		open.id = responsesCustomToolCallIDPrefix + callID
		open.name = b.Name
	default:
		open.id = responsesFunctionCallIDPrefix + callID
		open.name = b.Name
		if ns, ok := o.mapping.Namespace[b.Name]; ok {
			// namespace 子工具：还原成 {name: 子工具, namespace: 命名空间}。
			// codex 按 namespace + name 路由，缺 namespace 会被判为 unsupported call。
			open.name, open.namespace = ns.Name, ns.Namespace
		}
	}
	o.open = open

	var item types.ResponsesOutputItem
	switch open.kind {
	case types.ResponsesItemToolSearchCall:
		item = types.NewResponsesToolSearchCallItem(open.id, open.callID, nil, types.ResponsesStatusInProgress)
	case types.ResponsesItemCustomToolCall:
		item = types.NewResponsesCustomToolCallItem(open.id, open.callID, open.name, "", types.ResponsesStatusInProgress)
	default:
		item = types.NewResponsesFunctionCallItem(
			open.id, open.callID, open.name, open.namespace, "", types.ResponsesStatusInProgress)
	}
	return append(out, types.NewResponsesOutputItemAdded(o.nextSeq(), open.index, item))
}

// blockDelta 处理 content_block_delta。
func (o *responsesOut) blockDelta(d types.ContentBlockDeltaEvent) []types.StreamEvent {
	open := o.open
	if open == nil {
		return nil // 无打开 item 的增量没有归属：宁可丢弃，也不产出引用未宣告 item 的 delta
	}
	switch dd := d.Delta.(type) {
	case types.TextDelta:
		if open.kind != types.ResponsesItemMessage || dd.Text == "" {
			return nil
		}
		open.partText.WriteString(dd.Text)
		return []types.StreamEvent{types.NewResponsesOutputTextDelta(
			o.nextSeq(), open.index, open.contentIndex, open.id, dd.Text)}

	case types.ThinkingDelta:
		if open.kind != types.ResponsesItemReasoning || dd.Thinking == "" {
			return nil
		}
		open.thinking.WriteString(dd.Thinking)
		return []types.StreamEvent{types.NewResponsesReasoningSummaryTextDelta(
			o.nextSeq(), open.index, 0, "", dd.Thinking)}

	case types.SignatureDelta:
		// 签名不产事件：它在 reasoning item 关闭时以 encrypted_content 落到 item 上，
		// 客户端据此跨工具轮次回放思考。
		if open.kind == types.ResponsesItemReasoning {
			open.signature = dd.Signature
		}
		return nil

	case types.InputJSONDelta:
		return o.appendToolArguments(dd.PartialJSON)

	default:
		return nil
	}
}

// appendToolArguments 累积上游一次性给出的完整参数，并按 item 类型产出增量：
//   - function_call：参数原文按 ~10 字符分片发 delta（模拟 OpenAI 增量节奏）；
//   - custom_tool_call：降级 schema 的 {input:string} 先解包成裸文本再分片，
//     保证 delta 拼接结果恰好等于 done 给出的全文；
//   - tool_search_call：只累积不发——codex 从 output_item.done 物化该调用，不消费参数增量。
func (o *responsesOut) appendToolArguments(partial string) []types.StreamEvent {
	open := o.open
	if open == nil || partial == "" {
		return nil
	}
	switch open.kind {
	case types.ResponsesItemFunctionCall:
		open.arguments.WriteString(partial)
		return o.emitToolDeltas(partial)
	case types.ResponsesItemCustomToolCall:
		open.arguments.WriteString(partial)
		return o.appendCustomInputDelta()
	case types.ResponsesItemToolSearchCall:
		open.arguments.WriteString(partial)
		return nil
	default:
		return nil
	}
}

// appendCustomInputDelta 解包 custom 工具当前累积的参数，只发「新增部分」的增量。
// 参数尚未完整（非法 JSON）时解包结果不可判定，等收尾时按整串兜底。
func (o *responsesOut) appendCustomInputDelta() []types.StreamEvent {
	open := o.open
	unwrapped, ok := responsesCustomInput(open.arguments.String())
	if !ok {
		return nil
	}
	if !strings.HasPrefix(unwrapped, open.unwrapped) {
		// 解包结果不再以已发内容为前缀（异常分片）：已发内容与新结果无关，从头重发
		open.inputEmitted = 0
	}
	open.unwrapped = unwrapped
	rest := unwrapped[open.inputEmitted:]
	open.inputEmitted = len(unwrapped)
	return o.emitToolDeltas(rest)
}

// emitToolDeltas 把一段文本切成 ~10 字符的增量事件。
// 按 rune 切分：从多字节字符中间截断会产出非法 UTF-8，客户端拼接后得到损坏的 JSON。
func (o *responsesOut) emitToolDeltas(chunk string) []types.StreamEvent {
	open := o.open
	if open == nil || chunk == "" {
		return nil
	}
	pieces := chatSplitRunes(chunk, responsesArgChunkSize)
	out := make([]types.StreamEvent, 0, len(pieces))
	for _, piece := range pieces {
		switch open.kind {
		case types.ResponsesItemFunctionCall:
			out = append(out, types.NewResponsesFunctionCallArgumentsDelta(
				o.nextSeq(), open.index, open.id, open.callID, open.name, piece))
		case types.ResponsesItemCustomToolCall:
			out = append(out, types.NewResponsesCustomToolCallInputDelta(
				o.nextSeq(), open.index, open.id, open.callID, open.name, piece))
		}
	}
	return out
}

// closeBlock 处理 content_block_stop。
// message item 是例外：它刻意保持打开，只关闭当前 part —— 后续 text 块仍属于同一条
// assistant 消息，提前关 item 会让被切断的文本在客户端变成两条消息。
func (o *responsesOut) closeBlock() []types.StreamEvent {
	open := o.open
	if open == nil {
		return nil
	}
	switch open.kind {
	case types.ResponsesItemMessage:
		return o.closeTextPart()

	case types.ResponsesItemReasoning:
		text := open.thinking.String()
		var out []types.StreamEvent
		out = append(out, types.NewResponsesReasoningSummaryTextDone(o.nextSeq(), open.index, 0, "", text))
		out = append(out, types.NewResponsesReasoningSummaryPartDone(o.nextSeq(), open.index, 0, "", text))
		return append(out, o.closeOpen()...)

	case types.ResponsesItemFunctionCall:
		out := []types.StreamEvent{types.NewResponsesFunctionCallArgumentsDone(
			o.nextSeq(), open.index, open.id, open.callID, open.name, open.arguments.String())}
		return append(out, o.closeOpen()...)

	case types.ResponsesItemCustomToolCall:
		return o.closeCustom()

	default:
		return o.closeOpen()
	}
}

// closeTextPart 关闭当前 output_text part：done 事件带全文（delta 只带增量），
// 全文记入 item 的 content 数组。content_index 只在此处推进，part 之间的序号因此严格递增。
func (o *responsesOut) closeTextPart() []types.StreamEvent {
	open := o.open
	if open == nil || open.kind != types.ResponsesItemMessage || !open.partOpen {
		return nil
	}
	text := open.partText.String()
	out := []types.StreamEvent{
		types.NewResponsesOutputTextDone(o.nextSeq(), open.index, open.contentIndex, open.id, text),
		types.NewResponsesContentPartDone(o.nextSeq(), open.index, open.contentIndex, open.id, text),
	}
	open.parts = append(open.parts, types.ResponsesOutputTextPart{Type: "output_text", Text: text})
	open.partText.Reset()
	open.partOpen = false
	open.contentIndex++
	return out
}

// closeCustom 收尾 custom_tool_call：把参数解包成裸自由文本。
// 非法 JSON（上游截断）时按原样整串给出——宁可回放原文，也不静默丢内容。
func (o *responsesOut) closeCustom() []types.StreamEvent {
	open := o.open
	raw := open.arguments.String()
	input, ok := responsesCustomInput(raw)
	if !ok {
		input = raw
	}
	if !strings.HasPrefix(input, open.unwrapped) {
		// 此前解包不可判定（或分片异常）：余量按全文补发，保证 delta 拼接结果等于 done
		open.inputEmitted = 0
	}
	out := o.emitToolDeltas(input[open.inputEmitted:])
	open.unwrapped = input
	out = append(out, types.NewResponsesCustomToolCallInputDone(
		o.nextSeq(), open.index, open.id, open.callID, open.name, input))
	return append(out, o.closeOpen()...)
}

// closeOpen 关闭当前打开的 item：先补齐未收尾的 part，再发 output_item.done 并推进 output_index。
// 无打开 item 时是空操作（幂等）。
func (o *responsesOut) closeOpen() []types.StreamEvent {
	if o.open == nil {
		return nil
	}
	open := o.open
	var out []types.StreamEvent
	if open.kind == types.ResponsesItemMessage {
		out = append(out, o.closeTextPart()...)
	}
	item := open.doneItem()
	out = append(out, types.NewResponsesOutputItemDone(o.nextSeq(), open.index, item))
	o.output = append(o.output, item)
	o.outputIndex++
	o.open = nil
	return out
}

// doneItem 把打开的 item 收敛成终态形状（status=completed）。
// 截断/失败时残留的 item 也按 completed 收尾：客户端拿到的是「已结束但内容可能不全」的 item，
// 好过 item 永远停在 in_progress。
func (i *responsesOpenItem) doneItem() types.ResponsesOutputItem {
	switch i.kind {
	case types.ResponsesItemReasoning:
		return types.NewResponsesReasoningItem(i.signature,
			types.ResponsesSummaryPart{Type: "summary_text", Text: i.thinking.String()})
	case types.ResponsesItemFunctionCall:
		return types.NewResponsesFunctionCallItem(i.id, i.callID, i.name, i.namespace,
			i.arguments.String(), types.ResponsesStatusCompleted)
	case types.ResponsesItemCustomToolCall:
		return types.NewResponsesCustomToolCallItem(
			i.id, i.callID, i.name, i.unwrapped, types.ResponsesStatusCompleted)
	case types.ResponsesItemToolSearchCall:
		return types.NewResponsesToolSearchCallItem(
			i.id, i.callID, i.toolSearchArguments(), types.ResponsesStatusCompleted)
	default:
		return types.NewResponsesMessageItem(i.id, types.ResponsesStatusCompleted, i.parts...)
	}
}

// toolSearchArguments tool_search_call 的 arguments 线上是 JSON 对象。
// 解析失败或空参数时给 nil，由 types 层的 wire 收敛补成空对象
// （codex 从 done 物化该调用，不接受字符串形态）。
func (i *responsesOpenItem) toolSearchArguments() any {
	raw := strings.TrimSpace(i.arguments.String())
	if raw == "" {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return nil
	}
	return obj
}

// nextMessageItemID message item 的 id：msg_<响应 ID 去前缀>_<序号>。
// 确定性生成（不用随机 hex）：同一响应内的 item id 可复现，便于客户端回放与测试钉住。
func (o *responsesOut) nextMessageItemID() string {
	base := strings.TrimPrefix(o.responseID, responsesIDPrefix)
	id := responsesMessageIDPrefix + strconv.Itoa(o.messageSeq)
	if base != "" {
		id = responsesMessageIDPrefix + base + "_" + strconv.Itoa(o.messageSeq)
	}
	o.messageSeq++
	return id
}

// captureMeta 捕获 message_delta 的停止原因与 usage（不产事件）。
// usage 是**覆盖**而非累加：同一条流里 usage 是最终快照（上游多步循环已在内部累加完毕），
// 与 Chat 侧及 Aggregator 的 last-write-wins 口径一致；累加会把重复上报的 message_delta 放大成倍。
// 缓存写入量例外：它按「非 nil 才记入」，因此仍需另行累计并保留「见过」标志（nil 与 0 语义不同）。
func (o *responsesOut) captureMeta(d types.MessageDeltaEvent) {
	if d.Delta.StopReason != "" {
		o.stopReason = d.Delta.StopReason
	}
	u := d.Usage
	o.usage.InputTokens = u.InputTokens
	o.usage.OutputTokens = u.OutputTokens
	o.usage.CacheReadInputTokens = u.CacheReadInputTokens
	if u.CacheCreationInputTokens != nil {
		// nil 表示上游未产生缓存写入，0 表示写入量为 0：只有非 nil 才记入「见过」，
		// 否则会把「无写入」误报成「写入 0」。
		// 覆盖语义下这里同样只保留最后一次快照的写入量，而不是把各次相加。
		o.ccTotal = *u.CacheCreationInputTokens
		o.ccSeen = true
	}
}

// fail 处理流内 error 事件（也包含管线在 idle 超时/断流时喂进来的合成错误）：
// 关掉残留 item 后发 response.failed，output 带已聚合内容，
// 让客户端能还原失败前已经拿到的产出。
func (o *responsesOut) fail(code, message string) []types.StreamEvent {
	if o.terminated {
		return nil
	}
	o.failed = true
	if code == "" {
		code = "api_error"
	}
	o.errCode, o.errMsg = code, message
	return o.finish()
}

// finish 收尾（幂等）：关掉残留 item 并发出终态事件。已终态的流返回 nil——
// 管线无论流内是否已报错都会无条件调用 Finish，幂等不能指望调用方。
func (o *responsesOut) finish() []types.StreamEvent {
	if o.terminated {
		return nil
	}
	o.terminated = true
	out := o.closeOpen()
	resp := o.response(o.terminalStatus())
	var terminal types.StreamEvent
	switch o.terminalStatus() {
	case types.ResponsesStatusFailed:
		terminal = types.NewResponsesFailed(o.nextSeq(), resp, o.errCode, o.errMsg)
	case types.ResponsesStatusIncomplete:
		terminal = types.NewResponsesIncomplete(o.nextSeq(), resp, o.incompleteReason())
	default:
		terminal = types.NewResponsesCompleted(o.nextSeq(), resp)
	}
	return append(out, terminal)
}

// terminalStatus 终态状态：失败优先；anthropic 停止原因决定正常完成还是被截断。
func (o *responsesOut) terminalStatus() string {
	switch {
	case o.failed:
		return types.ResponsesStatusFailed
	case o.stopReason == stopReasonMaxTokens, o.stopReason == stopReasonRefusal:
		return types.ResponsesStatusIncomplete
	default:
		return types.ResponsesStatusCompleted
	}
}

// incompleteReason Anthropic stop_reason → Responses incomplete_details.reason。
func (o *responsesOut) incompleteReason() string {
	if o.stopReason == stopReasonRefusal {
		return responsesReasonContentFilter
	}
	return responsesReasonMaxOutputTokens
}

// response 组装 Response 对象：终态事件与非流式响应体共用同一份，
// 字段存在性（含空数组、null 键）交由 types 层决定。
func (o *responsesOut) response(status string) types.ResponsesResponse {
	return types.ResponsesResponse{
		ID:        o.responseID,
		Model:     o.model,
		CreatedAt: o.createdAt,
		Status:    status,
		Output:    o.output,
		Usage:     o.finalUsage(),

		Instructions:      o.echo.Instructions,
		Tools:             o.echo.Tools,
		ToolChoice:        o.echo.ToolChoice,
		Temperature:       o.echo.Temperature,
		TopP:              o.echo.TopP,
		MaxOutputTokens:   o.echo.MaxOutputTokens,
		ParallelToolCalls: o.echo.ParallelToolCalls,
	}
}

// finalUsage 把 cmdc 口径的 usage 换算成 Responses 口径（含缓存总量）。
// 加法回填公式复用 types 层，避免两处口径漂移。
func (o *responsesOut) finalUsage() types.ResponsesUsage {
	u := o.usage
	if o.ccSeen {
		cc := o.ccTotal
		u.CacheCreationInputTokens = &cc
	}
	return types.ResponsesUsageFromDelta(u)
}

// result 非流式结果：未收到终态事件时先自行收尾，保证 Result 恒返回完整对象。
func (o *responsesOut) result() *types.ResponsesResponse {
	if !o.terminated {
		o.finish()
	}
	status := o.terminalStatus()
	resp := o.response(status)
	// 截断原因必须与流式终态事件同形：流式由 types.NewResponsesIncomplete 落到事件上，
	// 非流式若不在此补齐，同一批事件会产出「流式有 reason、非流式没有」的分裂语义。
	// reason 的来源与流式同源（状态机记录的 stopReason），无需另记一份。
	if status == types.ResponsesStatusIncomplete {
		resp.IncompleteDetails = &types.ResponsesIncompleteDetails{Reason: o.incompleteReason()}
	}
	if o.failed {
		resp.Error = &types.ResponsesError{Code: o.errCode, Message: o.errMsg}
	}
	return &resp
}

// ---------- 流式编码器 ----------

// ResponsesEncoder 把规范格式事件流编码为 Responses SSE 帧。
// 契约与 StreamTranslator 同构：Feed 增量产出 0..N 帧，Finish 幂等收尾；无内部 goroutine。
// Responses 流没有 `data: [DONE]`，收尾帧就是 response.completed / .incomplete / .failed。
type ResponsesEncoder struct {
	out *responsesOut
}

// NewResponsesEncoder model 为回显模型名，responseID 为 handler 生成的 resp_... 标识，
// mapping 为入站工具族降级信息（nil 视为无降级工具）。
// 请求回显字段（tools / tool_choice / temperature 等）必须由 handler 调用 SetEcho 注入。
func NewResponsesEncoder(model, responseID string, mapping *ResponsesToolMapping) *ResponsesEncoder {
	return &ResponsesEncoder{out: newResponsesOut(model, responseID, mapping, ResponsesEcho{})}
}

// SetEcho 注入终态 Response 要回显的请求字段。须在首次 Feed 之前调用。
func (e *ResponsesEncoder) SetEcho(echo ResponsesEcho) { e.out.echo = echo }

// Feed 编码一条上游事件，返回 0..N 个 SSE 帧。
func (e *ResponsesEncoder) Feed(ev types.StreamEvent) [][]byte {
	return responsesFrames(e.out.feed(ev))
}

// Finish 收尾（幂等）：发出终态帧。
func (e *ResponsesEncoder) Finish() [][]byte {
	return responsesFrames(e.out.finish())
}

// ---------- 非流式聚合器 ----------

// ResponsesAggregator 把同一批事件折叠为非流式 *types.ResponsesResponse。
// 与 ResponsesEncoder 共用同一个状态机：流式与非流式语义只有一处实现。
type ResponsesAggregator struct {
	out *responsesOut
}

// NewResponsesAggregator 参数与 NewResponsesEncoder 一致；有状态，每请求新建。
func NewResponsesAggregator(model, responseID string, mapping *ResponsesToolMapping) *ResponsesAggregator {
	return &ResponsesAggregator{out: newResponsesOut(model, responseID, mapping, ResponsesEcho{})}
}

// SetEcho 注入响应体要回显的请求字段，语义同 ResponsesEncoder.SetEcho。
func (a *ResponsesAggregator) SetEcho(echo ResponsesEcho) { a.out.echo = echo }

func (a *ResponsesAggregator) Feed(ev types.StreamEvent) { a.out.feed(ev) }

// Result 返回可直接 JSON 序列化的最终响应对象（*types.ResponsesResponse）。
func (a *ResponsesAggregator) Result() any { return a.out.result() }

// ---------- 工具 ----------

// responsesFrames 把 Responses 事件逐个编码为 SSE 帧。
// 帧形状与 Anthropic 出站一致（event + data + 空行），由 types.StreamEvent.Encode 统一定义。
func responsesFrames(events []types.StreamEvent) [][]byte {
	if len(events) == 0 {
		return nil
	}
	frames := make([][]byte, 0, len(events))
	for _, ev := range events {
		frames = append(frames, ev.Encode())
	}
	return frames
}

// responsesCustomInput 解包 custom 工具的降级参数 {input: string}。
// 降级 schema 只有一个 input 字段，出站必须还原成裸自由文本：
// `{"input":"ls -la"}` → `ls -la`；空对象或无 input 键 → `""`；
// 非字符串 input（异常降级产物）保留原文 JSON，宁可多给也不丢内容。
// ok=false 表示参数还不是合法 JSON 对象——调用方据此区分「尚不完整」与「解包结果为空」。
func responsesCustomInput(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &fields); err != nil {
		return "", false
	}
	v, ok := fields["input"]
	if !ok || string(v) == "null" {
		return "", true
	}
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		return s, true
	}
	return string(v), true
}

// responsesToolCallID 取上游工具调用 id。空 id 会让 item 的 id 与 call_id 同时失去可回放性，
// 故兜底生成一个（上游正常路径不会走到：StreamTranslator 已为缺失 id 生成 toolu_ 标识）。
func responsesToolCallID(id string) string {
	if id == "" {
		return "toolu_" + masq.NewHexID(6)
	}
	return id
}
