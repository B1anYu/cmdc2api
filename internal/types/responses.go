package types

import "encoding/json"

// ===========================================================================
// Responses 协议（POST /v1/responses，Codex CLI）—— 入站解析类型 + 出站 wire 构造。
//
// 出站侧刻意不用 struct tag + omitempty 产线格式：output_index / content_index /
// summary_index 的 0 是有意义的值，function_call 的 arguments 允许是空串但键必须存在，
// message 的 content 必须是数组而不是 null —— 这些零值恰好会被 omitempty 吞掉，
// 而 Codex 是严格客户端（字段缺失直接拒收 / 解析崩溃）。因此每个事件、每个 output item
// 与 Response 对象都由显式构造函数逐键拼装 map，字段存在性只由本文件决定。
// ===========================================================================

// ---------- 入站请求（宽松解析） ----------

// ResponsesRequest Codex CLI 的 Responses 请求。
// 只声明归一化真正消费的字段：无上游对应能力的字段连类型都不定义，
// 由 ResponsesIgnoredFields 在原始报文上探测存在性后统一留痕（见 §4.1 的 warn 要求）。
type ResponsesRequest struct {
	Model              string              `json:"model"`
	Input              json.RawMessage     `json:"input"` // string | []ResponsesInputItem
	Instructions       string              `json:"instructions,omitempty"`
	MaxOutputTokens    int                 `json:"max_output_tokens,omitempty"`
	Stream             bool                `json:"stream,omitempty"`
	Temperature        *float64            `json:"temperature,omitempty"`
	TopP               *float64            `json:"top_p,omitempty"` // 有意静默丢弃：不转发、不回显、不留痕，理由见 AGENTS.md §三.6
	Tools              []ResponsesTool     `json:"tools,omitempty"`
	ToolChoice         json.RawMessage     `json:"tool_choice,omitempty"` // string | object
	Reasoning          *ResponsesReasoning `json:"reasoning,omitempty"`
	PreviousResponseID string              `json:"previous_response_id,omitempty"`
	Store              *bool               `json:"store,omitempty"`
	// PromptCacheKey 被**消费**（作为会话亲和的候选键，见 types.Request.PromptCacheKey），
	// 故不再列在 responsesIgnoredFields 里——它既不是丢弃项，也就不该报「ignored」。
	PromptCacheKey *string `json:"prompt_cache_key,omitempty"`
	// ParallelToolCalls 同样被**消费**：false 映射为上游 tool_choice 侧的同名开关
	// （disable_parallel_tool_use，与 Chat 侧同一实现），故不再列在 responsesIgnoredFields 里。
	// 未声明与 true 一律不动；回显由 server/responses.go 按真实值给出（未声明 = 协议默认 true）。
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
}

// responsesIgnoredFields 声明「客户端会发、但上游无对应能力」的字段。
// 顺序固定，保证同一请求的 warns 输出可复现。
var responsesIgnoredFields = []string{
	"include", "truncation", "background", "service_tier",
	"safety_identifier", "user", "metadata", "text",
	"top_logprobs", "stream_options",
}

// ResponsesIgnoredFields 返回原始请求体中实际出现的安全丢弃字段名（按声明顺序）。
// 这些字段语义上必须忽略，但静默忽略会掩盖客户端的能力预期，故保留存在性探测供归一化层 warn。
// 报文不是合法 JSON 时返回 nil：调用方随后会在解析请求结构体时报出更准确的错误。
func ResponsesIgnoredFields(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil
	}
	var present []string
	for _, name := range responsesIgnoredFields {
		if _, ok := probe[name]; ok {
			present = append(present, name)
		}
	}
	return present
}

// ResponsesReasoning reasoning 参数：仅 effort 有上游对应（映射为 thinking effort）；
// summary 形态无上游对应能力，保持 any 以便原样忽略。
type ResponsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary any    `json:"summary,omitempty"`
}

// ResponsesInputItem input 数组元素的并集，type 决定字段语义。
// type 取值：function_call / custom_tool_call / function_call_output /
// custom_tool_call_output / reasoning / tool_search_output / additional_tools /
// 以及无 type 的 message（有 role 即视为 message）。
type ResponsesInputItem struct {
	Type             string                 `json:"type,omitempty"`
	ID               string                 `json:"id,omitempty"`
	Role             string                 `json:"role,omitempty"` // user / assistant / system / developer
	Content          json.RawMessage        `json:"content,omitempty"`
	CallID           string                 `json:"call_id,omitempty"`
	Name             string                 `json:"name,omitempty"`
	Arguments        string                 `json:"arguments,omitempty"` // function_call：JSON 字符串
	Input            string                 `json:"input,omitempty"`     // custom_tool_call：自由文本
	Output           json.RawMessage        `json:"output,omitempty"`    // function_call_output：string | []part | 对象
	Summary          []ResponsesSummaryPart `json:"summary,omitempty"`   // reasoning：思考摘要
	EncryptedContent string                 `json:"encrypted_content,omitempty"`
	Namespace        string                 `json:"namespace,omitempty"`
	Tools            json.RawMessage        `json:"tools,omitempty"` // additional_tools / tool_search_output
	Status           string                 `json:"status,omitempty"`
}

// ResponsesContentPart content part 并集：input_text/text/output_text{type,text}
// 与 input_image{type,image_url,detail}。image_url 可为 data: URI 或 http(s) URL。
type ResponsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// ResponsesSummaryPart reasoning item 的 summary 元素。入站解析与出站 wire 共用同一形状：
// Type 在线上一律是 summary_text，空值按 summary_text 输出。
type ResponsesSummaryPart struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

// 顶层工具声明的 type 取值（Responses 侧是扁平结构，没有嵌套的 function 对象）。
const (
	ResponsesToolFunction   = "function"
	ResponsesToolCustom     = "custom"
	ResponsesToolToolSearch = "tool_search"
	ResponsesToolNamespace  = "namespace"
)

// ResponsesTool 工具声明并集，按 Type 分支解释：
//   - function：{name, description, parameters, strict}（parameters 直接在顶层）；
//   - custom：{name, description, format:{syntax,definition}} —— 自由文本工具；
//   - tool_search：{...} —— 代码代理工具搜索声明；
//   - namespace：{name, tools:[子工具]} —— 一组需要摊平的子工具。
//
// 其余类型（web_search / image_generation / file_search / mcp / local_shell / apply_patch 等）
// 无 cmdc 对应能力，由归一化层丢弃并 warn。
type ResponsesTool struct {
	Type        string               `json:"type,omitempty"`
	Name        string               `json:"name,omitempty"`
	Description string               `json:"description,omitempty"`
	Parameters  json.RawMessage      `json:"parameters,omitempty"`
	Strict      *bool                `json:"strict,omitempty"`
	Format      *ResponsesToolFormat `json:"format,omitempty"` // custom：输入文法
	Tools       []ResponsesTool      `json:"tools,omitempty"`  // namespace：子工具
}

// ResponsesToolFormat custom 工具的输入文法（如 lark / regex）。
type ResponsesToolFormat struct {
	Type       string `json:"type,omitempty"`
	Syntax     string `json:"syntax,omitempty"`
	Definition string `json:"definition,omitempty"`
}

// UnmarshalJSON 容忍工具声明的字符串简写：部分 codex 版本用 "name" 直接声明 custom 工具，
// 按对象强解会让整个请求 400（宽松解析是入站类型的既定契约）。
func (t *ResponsesTool) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var name string
		if err := json.Unmarshal(data, &name); err != nil {
			return err
		}
		*t = ResponsesTool{Type: ResponsesToolCustom, Name: name}
		return nil
	}
	// 别名类型不继承本方法，否则会无限递归。
	type plainTool ResponsesTool
	var p plainTool
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*t = ResponsesTool(p)
	return nil
}

// ---------- 出站 item / 状态常量 ----------

// output item 类型（我们只会生成这五种）。
const (
	ResponsesItemMessage        = "message"
	ResponsesItemReasoning      = "reasoning"
	ResponsesItemFunctionCall   = "function_call"
	ResponsesItemCustomToolCall = "custom_tool_call"
	ResponsesItemToolSearchCall = "tool_search_call"
)

// Response 与 item 的 status 取值。
const (
	ResponsesStatusInProgress = "in_progress"
	ResponsesStatusCompleted  = "completed"
	ResponsesStatusIncomplete = "incomplete"
	ResponsesStatusFailed     = "failed"
)

// ResponsesOutputTextPart message item 的 content 元素；
// 线上恒带 Type/Text/annotations/logprobs 四个键（后两者恒为空数组）。
type ResponsesOutputTextPart struct {
	Type string
	Text string
}

// ResponsesOutputItem 出站 output item 的字段并集，由 Type 决定哪些字段有意义。
// 各处字段按下述规则收敛成线上形状（见 wire）：
//   - message：id / role / status / content（恒数组）；
//   - reasoning：无 id、无 status、无 content（null 会让 C# SDK 崩），summary 恒数组；
//   - function_call：id / call_id / name / arguments（可为 ""）/ status；
//   - custom_tool_call：id / call_id / name / input（不补 "{}"）/ status；
//   - tool_search_call：id / call_id / execution:"client" / arguments（对象）/ status。
type ResponsesOutputItem struct {
	Type   string
	ID     string
	Status string
	Role   string // message，空值按 assistant 输出

	Content []ResponsesOutputTextPart // message
	Summary []ResponsesSummaryPart    // reasoning

	EncryptedContent string // reasoning：思考签名，空则不输出该键

	CallID    string
	Name      string
	Namespace string // function_call：namespace 子工具的回程还原标记

	Arguments           string // function_call：JSON 字符串，空串也必须输出键
	Input               string // custom_tool_call：解包后的裸自由文本
	ToolSearchArguments any    // tool_search_call：线上是 JSON 对象而非字符串
}

// MarshalJSON 让 Response.output 数组里的 item 也走同一套严格形状，
// 避免聚合结果与流式 item 出现两套字段规则。
func (i ResponsesOutputItem) MarshalJSON() ([]byte, error) {
	return json.Marshal(i.wire())
}

func (i ResponsesOutputItem) wire() map[string]any {
	switch i.Type {
	case ResponsesItemReasoning:
		// 无 id：OpenAI 不为 reasoning item 签发 id，伪造 id 反而破坏客户端回放。
		// status / content 连键都不出现：值为 null 时 C# SDK 直接崩。
		m := map[string]any{"type": i.Type, "summary": responsesSummaryWire(i.Summary)}
		if i.EncryptedContent != "" {
			m["encrypted_content"] = i.EncryptedContent
		}
		return m

	case ResponsesItemMessage:
		return map[string]any{
			"type":    ResponsesItemMessage,
			"id":      i.ID,
			"role":    responsesFallback(i.Role, "assistant"),
			"status":  responsesItemStatus(i.Status),
			"content": responsesMessageContentWire(i.Content),
		}

	case ResponsesItemFunctionCall:
		m := map[string]any{
			"type":      ResponsesItemFunctionCall,
			"id":        i.ID,
			"call_id":   i.CallID,
			"name":      i.Name,
			"arguments": i.Arguments, // 空串合法，但键必须在
			"status":    responsesItemStatus(i.Status),
		}
		// codex 按 namespace+name 路由子工具，缺该字段会被判为 unsupported call。
		if i.Namespace != "" {
			m["namespace"] = i.Namespace
		}
		return m

	case ResponsesItemCustomToolCall:
		return map[string]any{
			"type":    ResponsesItemCustomToolCall,
			"id":      i.ID,
			"call_id": i.CallID,
			"name":    i.Name,
			"input":   i.Input, // 自由文本，刻意不补 "{}"
			"status":  responsesItemStatus(i.Status),
		}

	case ResponsesItemToolSearchCall:
		return map[string]any{
			"type":      ResponsesItemToolSearchCall,
			"id":        i.ID,
			"call_id":   i.CallID,
			"execution": "client", // 非 client 时 codex 直接忽略该调用
			"arguments": responsesToolSearchArguments(i.ToolSearchArguments),
			"status":    responsesItemStatus(i.Status),
		}

	default:
		// 不该发生的类型：给出不产 null 字段的最小形状，而不是静默产出畸形 item。
		return map[string]any{
			"type":   i.Type,
			"id":     i.ID,
			"status": responsesItemStatus(i.Status),
		}
	}
}

// ---------- 出站 item 构造器（把字段存在性规则固化在构造函数里） ----------

// NewResponsesMessageItem 构造 message item；parts 为空时线上仍是 []。
func NewResponsesMessageItem(id, status string, parts ...ResponsesOutputTextPart) ResponsesOutputItem {
	return ResponsesOutputItem{Type: ResponsesItemMessage, ID: id, Status: status, Content: parts}
}

// NewResponsesReasoningItem 构造 reasoning item（无 id；encryptedContent 为空则不出该键）。
func NewResponsesReasoningItem(encryptedContent string, summary ...ResponsesSummaryPart) ResponsesOutputItem {
	return ResponsesOutputItem{
		Type: ResponsesItemReasoning, EncryptedContent: encryptedContent, Summary: summary,
	}
}

// NewResponsesFunctionCallItem 构造 function_call item。
func NewResponsesFunctionCallItem(id, callID, name, namespace, arguments, status string) ResponsesOutputItem {
	return ResponsesOutputItem{
		Type: ResponsesItemFunctionCall, ID: id, CallID: callID, Name: name,
		Namespace: namespace, Arguments: arguments, Status: status,
	}
}

// NewResponsesCustomToolCallItem 构造 custom_tool_call item。
func NewResponsesCustomToolCallItem(id, callID, name, input, status string) ResponsesOutputItem {
	return ResponsesOutputItem{
		Type: ResponsesItemCustomToolCall, ID: id, CallID: callID, Name: name,
		Input: input, Status: status,
	}
}

// NewResponsesToolSearchCallItem 构造 tool_search_call item；arguments 线上恒为对象。
func NewResponsesToolSearchCallItem(id, callID string, arguments any, status string) ResponsesOutputItem {
	return ResponsesOutputItem{
		Type: ResponsesItemToolSearchCall, ID: id, CallID: callID,
		ToolSearchArguments: arguments, Status: status,
	}
}

// ---------- 出站 usage ----------

// ResponsesInputTokensDetails 恒存在，cached_tokens 为 0 也要输出。
type ResponsesInputTokensDetails struct {
	CachedTokens int
}

// ResponsesUsage Responses 口径的 usage：总量含缓存（内部 noCache 口径的加法回填见 §5.4）。
// 所有字段恒存在（含 0），故不用 struct tag。
type ResponsesUsage struct {
	InputTokens        int
	InputTokensDetails ResponsesInputTokensDetails
	OutputTokens       int
	TotalTokens        int
}

// NewResponsesUsage 组装 usage 并顺带算好 total，避免两处口径漂移。
func NewResponsesUsage(inputTokens, cachedTokens, outputTokens int) ResponsesUsage {
	return ResponsesUsage{
		InputTokens:        inputTokens,
		OutputTokens:       outputTokens,
		TotalTokens:        inputTokens + outputTokens,
		InputTokensDetails: ResponsesInputTokensDetails{CachedTokens: cachedTokens},
	}
}

// ResponsesUsageFromDelta 把 cmdc 侧（Anthropic 形状）的 DeltaUsage 换算成 Responses 口径：
// 内部 input_tokens 是非缓存量，Responses 要的是含缓存总量，故把缓存读取量加回，
// 缓存写入量存在且为正时同样计入（为 0 或 nil 表示未产生写入，不能重复计）。
func ResponsesUsageFromDelta(u DeltaUsage) ResponsesUsage {
	input := u.InputTokens + u.CacheReadInputTokens
	if u.CacheCreationInputTokens != nil && *u.CacheCreationInputTokens > 0 {
		input += *u.CacheCreationInputTokens
	}
	return NewResponsesUsage(input, u.CacheReadInputTokens, u.OutputTokens)
}

func (u ResponsesUsage) MarshalJSON() ([]byte, error) {
	return json.Marshal(u.wire())
}

func (u ResponsesUsage) wire() map[string]any {
	return map[string]any{
		"input_tokens":         u.InputTokens,
		"input_tokens_details": map[string]any{"cached_tokens": u.InputTokensDetails.CachedTokens},
		"output_tokens":        u.OutputTokens,
		"total_tokens":         u.TotalTokens,
	}
}

// ---------- 出站 Response 对象 ----------

// ResponsesIncompleteDetails status=incomplete 时的原因，恒为 {reason: ...}。
type ResponsesIncompleteDetails struct {
	Reason string // max_output_tokens | content_filter
}

// ResponsesError 终态 failed 的错误体，挂在 response.error 下（OpenAI 形状）。
type ResponsesError struct {
	Code    string
	Message string
}

// ResponsesResponse Responses 响应对象：既是非流式的 200 响应体，
// 也是 response.created/in_progress/completed/incomplete/failed 事件携带的 response。
//
// 顶层字段存在性是硬约束（id/object/created_at/status/output/usage 恒在），
// 另有请求回显字段供 Codex 的浅校验：model、instructions（有才输出）、
// tools、tool_choice、temperature、max_output_tokens、parallel_tool_calls，
// 以及恒定值 previous_response_id:null 与 store:false（本代理不保存任何服务端状态）。
//
// top_p 刻意不在回显字段里（键也不出现）：它从未被转发给上游（BuildCcRequest 只转发
// temperature），回显它等于谎报参数已生效——真缺陷是谎报而非丢弃（见 translate/respout.go
// 的 ResponsesEcho 注释与 AGENTS.md §三.6 的丢弃清单）。
type ResponsesResponse struct {
	ID                string
	Model             string
	CreatedAt         int64
	Status            string
	Output            []ResponsesOutputItem
	Usage             ResponsesUsage
	IncompleteDetails *ResponsesIncompleteDetails
	Error             *ResponsesError

	// 回显字段。Tools 是客户端原始声明的解析结果（重新序列化仍是 Responses 扁平形状），
	// ToolChoice 是原始原文；绝不能回显归一化后的 Anthropic 形状（name/input_schema），
	// 否则严格客户端解析回显对象时失败。
	Instructions      string
	Tools             []ResponsesTool
	ToolChoice        json.RawMessage // 原始 tool_choice：string | object
	Temperature       *float64
	MaxOutputTokens   *int
	ParallelToolCalls bool
}

// MarshalJSON 让非流式响应体与终态事件里的 response 共用同一套严格字段规则。
func (r ResponsesResponse) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.Wire())
}

// Wire 展开成线上 map（字段存在性在此一处决定）。
func (r ResponsesResponse) Wire() map[string]any {
	m := map[string]any{
		"id":         r.ID,
		"object":     "response",
		"created_at": r.CreatedAt,
		"status":     r.Status,
		"output":     responsesOutputWire(r.Output),
		"usage":      r.Usage.wire(),

		"model":                r.Model,
		"parallel_tool_calls":  r.ParallelToolCalls,
		"tool_choice":          r.toolChoiceWire(),
		"tools":                responsesToolsWire(r.Tools),
		"temperature":          r.Temperature, // 指针：nil 序列化为 null（保留键）
		"max_output_tokens":    r.MaxOutputTokens,
		"previous_response_id": nil,   // 无服务端状态，恒 null
		"store":                false, // 无服务端状态，恒 false
	}
	if r.Instructions != "" {
		m["instructions"] = r.Instructions
	}
	if r.IncompleteDetails != nil {
		m["incomplete_details"] = map[string]any{"reason": r.IncompleteDetails.Reason}
	}
	if r.Error != nil {
		m["error"] = map[string]any{"code": r.Error.Code, "message": r.Error.Message}
	}
	return m
}

// toolChoiceWire 缺省回 "auto"：tool_choice 是客户端会读的回显字段，
// 客户端未声明时给出 OpenAI 的默认值比输出 null 更安全。
func (r ResponsesResponse) toolChoiceWire() any {
	if len(r.ToolChoice) > 0 && json.Valid(r.ToolChoice) {
		return r.ToolChoice
	}
	return "auto"
}

// ---------- 事件名常量（我们只生成这些；Responses 流没有 data: [DONE]） ----------

const (
	ResponsesEventCreated                   = "response.created"
	ResponsesEventInProgress                = "response.in_progress"
	ResponsesEventOutputItemAdded           = "response.output_item.added"
	ResponsesEventOutputItemDone            = "response.output_item.done"
	ResponsesEventContentPartAdded          = "response.content_part.added"
	ResponsesEventContentPartDone           = "response.content_part.done"
	ResponsesEventOutputTextDelta           = "response.output_text.delta"
	ResponsesEventOutputTextDone            = "response.output_text.done"
	ResponsesEventReasoningSummaryPartAdded = "response.reasoning_summary_part.added"
	ResponsesEventReasoningSummaryPartDone  = "response.reasoning_summary_part.done"
	ResponsesEventReasoningSummaryTextDelta = "response.reasoning_summary_text.delta"
	ResponsesEventReasoningSummaryTextDone  = "response.reasoning_summary_text.done"
	ResponsesEventFunctionCallArgsDelta     = "response.function_call_arguments.delta"
	ResponsesEventFunctionCallArgsDone      = "response.function_call_arguments.done"
	ResponsesEventCustomToolCallInputDelta  = "response.custom_tool_call_input.delta"
	ResponsesEventCustomToolCallInputDone   = "response.custom_tool_call_input.done"
	ResponsesEventCompleted                 = "response.completed"
	ResponsesEventIncomplete                = "response.incomplete"
	ResponsesEventFailed                    = "response.failed"
)

// ---------- 事件构造器（sequence_number 由调用方传入并负责递增） ----------

// responsesEvent 拼事件载荷：type 与 sequence_number 恒在，其余键来自 payload。
func responsesEvent(name string, seq int, payload map[string]any) StreamEvent {
	m := make(map[string]any, len(payload)+2)
	m["type"] = name
	m["sequence_number"] = seq
	for k, v := range payload {
		m[k] = v
	}
	return StreamEvent{Name: name, Data: m}
}

// NewResponsesCreated 流已建立：response 为 in_progress、output 为空数组。
func NewResponsesCreated(seq int, resp ResponsesResponse) StreamEvent {
	return responsesEvent(ResponsesEventCreated, seq, map[string]any{"response": resp.Wire()})
}

// NewResponsesInProgress 紧随 created 的心跳事件，携带同一份响应对象。
func NewResponsesInProgress(seq int, resp ResponsesResponse) StreamEvent {
	return responsesEvent(ResponsesEventInProgress, seq, map[string]any{"response": resp.Wire()})
}

// NewResponsesOutputItemAdded 宣告一个新 item；status 缺省按 in_progress 输出。
func NewResponsesOutputItemAdded(seq, outputIndex int, item ResponsesOutputItem) StreamEvent {
	return responsesEvent(ResponsesEventOutputItemAdded, seq, map[string]any{
		"output_index": outputIndex,
		"item":         item.wire(),
	})
}

// NewResponsesOutputItemDone 关闭一个 item。done 语义下 item 必然已结束，
// status 缺省按 completed 补全，避免客户端等待一个永远不结束的 item。
func NewResponsesOutputItemDone(seq, outputIndex int, item ResponsesOutputItem) StreamEvent {
	if item.Status == "" {
		item.Status = ResponsesStatusCompleted
	}
	return responsesEvent(ResponsesEventOutputItemDone, seq, map[string]any{
		"output_index": outputIndex,
		"item":         item.wire(),
	})
}

// NewResponsesContentPartAdded 宣告 message item 内一个 output_text part 开始。
// 必须早于该 part 的首个 delta：累积式 SDK 只在收到本事件后才 append part。
func NewResponsesContentPartAdded(seq, outputIndex, contentIndex int, itemID string) StreamEvent {
	payload := responsesIndexPayload(outputIndex, contentIndex, itemID)
	payload["part"] = responsesOutputTextPartWire("")
	return responsesEvent(ResponsesEventContentPartAdded, seq, payload)
}

// NewResponsesContentPartDone 关闭该 part，part 与 done 事件同样携带全文。
func NewResponsesContentPartDone(seq, outputIndex, contentIndex int, itemID, text string) StreamEvent {
	payload := responsesIndexPayload(outputIndex, contentIndex, itemID)
	payload["part"] = responsesOutputTextPartWire(text)
	return responsesEvent(ResponsesEventContentPartDone, seq, payload)
}

// NewResponsesOutputTextDelta 文本增量。
func NewResponsesOutputTextDelta(seq, outputIndex, contentIndex int, itemID, delta string) StreamEvent {
	payload := responsesIndexPayload(outputIndex, contentIndex, itemID)
	payload["delta"] = delta
	return responsesEvent(ResponsesEventOutputTextDelta, seq, payload)
}

// NewResponsesOutputTextDone 文本收尾，text 为全文而非增量。
func NewResponsesOutputTextDone(seq, outputIndex, contentIndex int, itemID, text string) StreamEvent {
	payload := responsesIndexPayload(outputIndex, contentIndex, itemID)
	payload["text"] = text
	return responsesEvent(ResponsesEventOutputTextDone, seq, payload)
}

// NewResponsesReasoningSummaryPartAdded 宣告 reasoning item 的摘要 part 开始。
func NewResponsesReasoningSummaryPartAdded(seq, outputIndex, summaryIndex int, itemID string) StreamEvent {
	payload := responsesReasoningPayload(outputIndex, summaryIndex, itemID)
	payload["part"] = responsesSummaryTextPartWire("")
	return responsesEvent(ResponsesEventReasoningSummaryPartAdded, seq, payload)
}

// NewResponsesReasoningSummaryPartDone 关闭摘要 part，part 携带全文。
func NewResponsesReasoningSummaryPartDone(seq, outputIndex, summaryIndex int, itemID, text string) StreamEvent {
	payload := responsesReasoningPayload(outputIndex, summaryIndex, itemID)
	payload["part"] = responsesSummaryTextPartWire(text)
	return responsesEvent(ResponsesEventReasoningSummaryPartDone, seq, payload)
}

// NewResponsesReasoningSummaryTextDelta 思考摘要增量。
func NewResponsesReasoningSummaryTextDelta(seq, outputIndex, summaryIndex int, itemID, delta string) StreamEvent {
	payload := responsesReasoningPayload(outputIndex, summaryIndex, itemID)
	payload["delta"] = delta
	return responsesEvent(ResponsesEventReasoningSummaryTextDelta, seq, payload)
}

// NewResponsesReasoningSummaryTextDone 思考摘要收尾，text 为全文。
func NewResponsesReasoningSummaryTextDone(seq, outputIndex, summaryIndex int, itemID, text string) StreamEvent {
	payload := responsesReasoningPayload(outputIndex, summaryIndex, itemID)
	payload["text"] = text
	return responsesEvent(ResponsesEventReasoningSummaryTextDone, seq, payload)
}

// NewResponsesFunctionCallArgumentsDelta 函数调用参数增量（分片后逐片发）。
func NewResponsesFunctionCallArgumentsDelta(seq, outputIndex int, itemID, callID, name, delta string) StreamEvent {
	payload := responsesToolCallPayload(outputIndex, itemID, callID, name)
	payload["delta"] = delta
	return responsesEvent(ResponsesEventFunctionCallArgsDelta, seq, payload)
}

// NewResponsesFunctionCallArgumentsDone 参数收尾，arguments 为完整 JSON 字符串（可为 ""）。
func NewResponsesFunctionCallArgumentsDone(seq, outputIndex int, itemID, callID, name, arguments string) StreamEvent {
	payload := responsesToolCallPayload(outputIndex, itemID, callID, name)
	payload["arguments"] = arguments
	return responsesEvent(ResponsesEventFunctionCallArgsDone, seq, payload)
}

// NewResponsesCustomToolCallInputDelta custom 工具自由文本增量。
func NewResponsesCustomToolCallInputDelta(seq, outputIndex int, itemID, callID, name, delta string) StreamEvent {
	payload := responsesToolCallPayload(outputIndex, itemID, callID, name)
	payload["delta"] = delta
	return responsesEvent(ResponsesEventCustomToolCallInputDelta, seq, payload)
}

// NewResponsesCustomToolCallInputDone 自由文本收尾，input 为解包后的裸字符串（可为 ""）。
func NewResponsesCustomToolCallInputDone(seq, outputIndex int, itemID, callID, name, input string) StreamEvent {
	payload := responsesToolCallPayload(outputIndex, itemID, callID, name)
	payload["input"] = input
	return responsesEvent(ResponsesEventCustomToolCallInputDone, seq, payload)
}

// NewResponsesCompleted 正常终态。终态事件的 response 必须携带完整 output 与 usage：
// Codex 的 get_final_response() 直接解析终态事件，空 output 会拿到空结果。
func NewResponsesCompleted(seq int, resp ResponsesResponse) StreamEvent {
	if resp.Status == "" {
		resp.Status = ResponsesStatusCompleted
	}
	return responsesEvent(ResponsesEventCompleted, seq, map[string]any{"response": resp.Wire()})
}

// NewResponsesIncomplete 被截断 / 内容过滤的终态，reason 恒随事件输出。
func NewResponsesIncomplete(seq int, resp ResponsesResponse, reason string) StreamEvent {
	resp.Status = ResponsesStatusIncomplete
	resp.IncompleteDetails = &ResponsesIncompleteDetails{Reason: reason}
	return responsesEvent(ResponsesEventIncomplete, seq, map[string]any{"response": resp.Wire()})
}

// NewResponsesFailed 流内失败的终态，错误体挂在 response.error 下（OpenAI 形状），
// output 仍需带已聚合的 item，便于客户端还原失败前的产出。
func NewResponsesFailed(seq int, resp ResponsesResponse, code, message string) StreamEvent {
	resp.Status = ResponsesStatusFailed
	resp.Error = &ResponsesError{Code: code, Message: message}
	return responsesEvent(ResponsesEventFailed, seq, map[string]any{"response": resp.Wire()})
}

// ---------- wire 小工具 ----------

// responsesIndexPayload 文本类事件的公共索引：三个索引键恒在，0 也必须出现。
func responsesIndexPayload(outputIndex, contentIndex int, itemID string) map[string]any {
	m := map[string]any{"output_index": outputIndex, "content_index": contentIndex}
	responsesPutItemID(m, itemID)
	return m
}

// responsesReasoningPayload 摘要类事件用 summary_index 取代 content_index。
func responsesReasoningPayload(outputIndex, summaryIndex int, itemID string) map[string]any {
	m := map[string]any{"output_index": outputIndex, "summary_index": summaryIndex}
	responsesPutItemID(m, itemID)
	return m
}

// responsesToolCallPayload 工具类事件的公共索引。reasoning item 没有 id，
// item_id 因此按存在才输出，而不是硬塞空串。
func responsesToolCallPayload(outputIndex int, itemID, callID, name string) map[string]any {
	m := map[string]any{"output_index": outputIndex}
	responsesPutItemID(m, itemID)
	if callID != "" {
		m["call_id"] = callID
	}
	if name != "" {
		m["name"] = name
	}
	return m
}

func responsesPutItemID(m map[string]any, itemID string) {
	if itemID != "" {
		m["item_id"] = itemID
	}
}

// responsesOutputTextPartWire 恒带 annotations/logprobs 空数组（严格客户端拒收缺字段的 part）。
func responsesOutputTextPartWire(text string) map[string]any {
	return map[string]any{
		"type":        "output_text",
		"text":        text,
		"annotations": []any{},
		"logprobs":    []any{},
	}
}

func responsesSummaryTextPartWire(text string) map[string]any {
	return map[string]any{"type": "summary_text", "text": text}
}

// responsesMessageContentWire message 的 content 恒为数组，空集输出 [] 而非 null。
func responsesMessageContentWire(parts []ResponsesOutputTextPart) []map[string]any {
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		typ := p.Type
		if typ == "" {
			typ = "output_text"
		}
		out = append(out, map[string]any{
			"type":        typ,
			"text":        p.Text,
			"annotations": []any{},
			"logprobs":    []any{},
		})
	}
	return out
}

// responsesSummaryWire reasoning 的 summary 恒为数组。
func responsesSummaryWire(summary []ResponsesSummaryPart) []map[string]any {
	out := make([]map[string]any, 0, len(summary))
	for _, s := range summary {
		typ := s.Type
		if typ == "" {
			typ = "summary_text"
		}
		out = append(out, map[string]any{"type": typ, "text": s.Text})
	}
	return out
}

// responsesOutputWire Response.output 恒为数组（顺序即 output_index 顺序）。
func responsesOutputWire(items []ResponsesOutputItem) []ResponsesOutputItem {
	if len(items) == 0 {
		return []ResponsesOutputItem{}
	}
	return items
}

// responsesToolsWire 工具回显恒为数组。
func responsesToolsWire(tools []ResponsesTool) []ResponsesTool {
	if len(tools) == 0 {
		return []ResponsesTool{}
	}
	return tools
}

// responsesToolSearchArguments 保证 tool_search_call 的 arguments 在线上恒为对象。
//
// 可达输入只有两种：item 宣告阶段的字面 nil，与收尾阶段由 respout 解析好的 map[string]any。
// 参数原文的 JSON 解析一律在入站/出站侧完成（见 translate/respout.go 的 toolSearchArguments），
// 因此这里不再保留 string / RawMessage / []byte 那类分支——它们没有调用点，
// 只会被测试固定成「看似可达」的死代码。
func responsesToolSearchArguments(v any) any {
	if m, ok := v.(map[string]any); ok && len(m) > 0 {
		return m
	}
	// 其余输入一律收敛为空对象，包括：
	//   - typed-nil map：参数原文恰为 "null" 时 json.Unmarshal 既不报错也不填 map，
	//     留下的是 typed-nil map——若原样下发，线上会变成 arguments:null，
	//     而 codex 物化该调用时要求 arguments 是对象（非对象会让它拿到坏参数）；
	//   - 字面 nil、空 map，以及任何未来新增的非预期类型：arguments 恒为对象是硬约束，
	//     宁缺勿坏形状。
	return map[string]any{}
}

// responsesItemStatus item 的 status 键恒存在：调用方漏设时按 added 阶段语义回退
// in_progress，避免线上出现空字符串状态。
func responsesItemStatus(status string) string {
	return responsesFallback(status, ResponsesStatusInProgress)
}

func responsesFallback(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
