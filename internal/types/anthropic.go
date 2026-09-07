// Package types 定义线上协议类型：入站 Anthropic Messages、cmdc 信封与 NDJSON
// 事件、代理回给客户端的 Anthropic SSE 事件与非流式消息。
// 多态字段（system/content/tool_choice 等）用 json.RawMessage 保真，
// 解析时先试 string 再试数组（sub2api 手法）。
package types

import "encoding/json"

// ---------- 入站 Anthropic 请求 ----------

type Request struct {
	Model         string           `json:"model"`
	Messages      []InboundMessage `json:"messages"`
	System        json.RawMessage  `json:"system,omitempty"`  // string | []block
	MaxTokens     int              `json:"max_tokens,omitempty"`
	Stream        bool             `json:"stream,omitempty"`
	Temperature   *float64         `json:"temperature,omitempty"`
	TopP          *float64         `json:"top_p,omitempty"`
	TopK          *int             `json:"top_k,omitempty"`
	StopSequences []string         `json:"stop_sequences,omitempty"`
	Tools         []Tool           `json:"tools,omitempty"`
	ToolChoice    json.RawMessage  `json:"tool_choice,omitempty"`
	Thinking      json.RawMessage  `json:"thinking,omitempty"`
	Metadata      *Metadata        `json:"metadata,omitempty"`
}

type InboundMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string | []block
}

type Tool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	CacheControl *CacheControl  `json:"cache_control,omitempty"`
}

type Metadata struct {
	UserID json.RawMessage `json:"user_id,omitempty"`
}

// CacheControl Anthropic 与 cmdc part 共用的缓存断点标记。
// 上游已验证的形状只有 {type:"ephemeral"}，TTL 一律剥掉。
type CacheControl struct {
	Type string `json:"type"`
}

// Block 入站 content 块的并集（text/image/thinking/tool_use/tool_result/...）。
type Block struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Signature    string          `json:"signature,omitempty"` // 仅入站历史携带，不回传上游
	Source       *ImageSource    `json:"source,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"` // tool_result: string | []block（递归）
	IsError      bool            `json:"is_error,omitempty"`
	CacheControl *CacheControl   `json:"cache_control,omitempty"`
}

type ImageSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// ---------- 出站 Anthropic SSE 事件 ----------

// StreamEvent 一条 SSE 事件：Name 是 event: 行，Data 是 data: 载荷。
type StreamEvent struct {
	Name string
	Data any
}

// Encode 输出 SSE 帧（event + data + 空行）。
func (e StreamEvent) Encode() []byte {
	data, err := json.Marshal(e.Data)
	if err != nil {
		data = []byte(`{}`)
	}
	buf := make([]byte, 0, len(e.Name)+len(data)+24)
	buf = append(buf, "event: "...)
	buf = append(buf, e.Name...)
	buf = append(buf, "\ndata: "...)
	buf = append(buf, data...)
	buf = append(buf, '\n', '\n')
	return buf
}

type MessageStartEvent struct {
	Type    string      `json:"type"`
	Message MessageBody `json:"message"`
}

type MessageBody struct {
	ID           string  `json:"id"`
	Type         string  `json:"type"`
	Role         string  `json:"role"`
	Content      []any   `json:"content"`
	Model        string  `json:"model"`
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
	Usage        Usage   `json:"usage"`
}

type ContentBlockStartEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock any    `json:"content_block"`
}

type TextBlockStart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ThinkingBlockStart struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

type ToolUseBlockStart struct {
	Type  string         `json:"type"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

type ContentBlockDeltaEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta any    `json:"delta"`
}

type TextDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ThinkingDelta struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

type SignatureDelta struct {
	Type      string `json:"type"`
	Signature string `json:"signature"`
}

type InputJSONDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

type ContentBlockStopEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type MessageDeltaEvent struct {
	Type  string         `json:"type"`
	Delta StopReasonBody `json:"delta"`
	Usage DeltaUsage     `json:"usage"`
}

type StopReasonBody struct {
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

// DeltaUsage message_delta 的 usage：input_tokens 是原版手法（message_start
// 只能发 0，finish 才知道真实值，靠这里纠正 SDK 累积的最终消息）。
type DeltaUsage struct {
	OutputTokens             int   `json:"output_tokens"`
	InputTokens              int   `json:"input_tokens"`
	CacheReadInputTokens     int   `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int  `json:"cache_creation_input_tokens"`
}

type MessageStopEvent struct {
	Type string `json:"type"`
}

type ErrorEvent struct {
	Type  string     `json:"type"`
	Error ErrorEventBody `json:"error"`
}

type ErrorEventBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ---------- 事件构造器 ----------

func NewMessageStart(id, model string) StreamEvent {
	return StreamEvent{Name: "message_start", Data: MessageStartEvent{
		Type: "message_start",
		Message: MessageBody{
			ID: id, Type: "message", Role: "assistant",
			Content: []any{}, Model: model,
			StopReason: nil, StopSequence: nil, Usage: Usage{},
		},
	}}
}

func NewContentBlockStart(index int, block any) StreamEvent {
	return StreamEvent{Name: "content_block_start", Data: ContentBlockStartEvent{
		Type: "content_block_start", Index: index, ContentBlock: block,
	}}
}

func NewContentBlockDelta(index int, delta any) StreamEvent {
	return StreamEvent{Name: "content_block_delta", Data: ContentBlockDeltaEvent{
		Type: "content_block_delta", Index: index, Delta: delta,
	}}
}

func NewContentBlockStop(index int) StreamEvent {
	return StreamEvent{Name: "content_block_stop", Data: ContentBlockStopEvent{
		Type: "content_block_stop", Index: index,
	}}
}

func NewMessageDelta(stopReason string, u DeltaUsage) StreamEvent {
	return StreamEvent{Name: "message_delta", Data: MessageDeltaEvent{
		Type:  "message_delta",
		Delta: StopReasonBody{StopReason: stopReason, StopSequence: nil},
		Usage: u,
	}}
}

func NewMessageStop() StreamEvent {
	return StreamEvent{Name: "message_stop", Data: MessageStopEvent{Type: "message_stop"}}
}

func NewErrorEvent(typ, message string) StreamEvent {
	return StreamEvent{Name: "error", Data: ErrorEvent{
		Type:  "error",
		Error: ErrorEventBody{Type: typ, Message: message},
	}}
}

// ---------- 出站 Anthropic 非流式消息 ----------

type Message struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Content      []ContentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   string         `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        Usage          `json:"usage"`
}

// ContentBlock 非流式消息的 content 块并集。
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}
