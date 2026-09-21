package types

import "encoding/json"

// ---------- 入站 Chat Completions 请求（宽松解析） ----------

// ChatRequest OpenAI Chat Completions 入站请求。
// 解析取宽松策略：未知字段一律忽略；已知但上游无对应能力的字段由归一化层
// （translate.ChatToRequestBody）留痕丢弃，而不是在这里报错——客户端 SDK 的默认
// 请求体里带一堆可选字段，任何一处严格都会把整轮打成 400。
type ChatRequest struct {
	Model               string             `json:"model"`
	Messages            []ChatMessage      `json:"messages"`
	Temperature         *float64           `json:"temperature,omitempty"`
	TopP                *float64           `json:"top_p,omitempty"`
	MaxTokens           *int               `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int               `json:"max_completion_tokens,omitempty"`
	Stream              bool               `json:"stream,omitempty"`
	StreamOptions       *ChatStreamOptions `json:"stream_options,omitempty"`
	// Stop string | []string。上游没有 stop 能力，值不参与归一化（不映射进 StopSequences），
	// 只用于按「客户端是否发过」留痕。
	Stop              json.RawMessage `json:"stop,omitempty"`
	Tools             []ChatTool      `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"` // string | object
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort   json.RawMessage `json:"reasoning_effort,omitempty"` // string | {effort,summary}
	// PromptCacheKey 被**消费**（作为会话亲和的候选键，见 types.Request.PromptCacheKey），
	// 故不列入 chatIgnoredFields：它既不是丢弃项，也就不该报「ignored」。
	PromptCacheKey *string `json:"prompt_cache_key,omitempty"`
	User           string  `json:"user,omitempty"`
	Seed           *int    `json:"seed,omitempty"`
	// N 采样数：仅支持 1（上游无多候选生成），>1 由归一化层拒绝。
	N              *int            `json:"n,omitempty"`
	Logprobs       *bool           `json:"logprobs,omitempty"`
	TopLogprobs    *int            `json:"top_logprobs,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
}

// chatIgnoredFields 声明「客户端会发、但上游无对应能力且归一化层连类型都不定义」的
// Chat 顶层字段（与 Responses 侧的 responsesIgnoredFields 对等）。
// 顺序固定，保证同一请求的 warns 输出可复现。
// 已有类型字段（logprobs/top_logprobs/seed/user/stream_options/response_format/
// parallel_tool_calls/stop）由 translate 侧直接在结构体上留痕，不在此重复声明。
var chatIgnoredFields = []string{
	"frequency_penalty", "presence_penalty", "logit_bias", "store", "prediction", "extra_body",
}

// ChatIgnoredFields 返回原始请求体中实际出现的安全丢弃字段名（按声明顺序）。
// 这些字段语义上必须忽略，但静默忽略会掩盖客户端的能力预期，故保留存在性探测供归一化层 warn。
// raw 为 nil（兼容入口）或报文不是合法 JSON 时返回 nil：后者调用方随后会在解析结构体时报出更准确的错误。
func ChatIgnoredFields(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil
	}
	var present []string
	for _, name := range chatIgnoredFields {
		if _, ok := probe[name]; ok {
			present = append(present, name)
		}
	}
	return present
}

// ChatStreamOptions stream_options：include_usage 恒被满足（我们总是补发 usage chunk），
// 因此整个对象只用于留痕，不改变出站行为。
type ChatStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ChatMessage 入站消息并集。content 用 RawMessage 保真（string | []ChatContentPart | null），
// 因为「content 为字符串」与「content 为 part 数组」是两套代码路径，且 tool 消息的 content 可为 null。
type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ChatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	// 推理文本有三个来源：reasoning_content 是主流写法，reasoning / reason 是别名。
	// 别名取 RawMessage：部分客户端把它塞成对象（如 {content:...}），
	// 而字段类型不匹配会让整个请求解析失败。
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Reasoning        json.RawMessage `json:"reasoning,omitempty"`
	Reason           json.RawMessage `json:"reason"`
	// FunctionCall 是 OpenAI 的 legacy 单工具调用字段，无 id 可配对，归一化时留痕丢弃。
	FunctionCall *ChatFunctionCall `json:"function_call,omitempty"`
}

// ChatContentPart content 数组元素。只声明 text / image_url / input_audio 三种已见形状，
// 其余类型（file、input_file、video_url 等）由 Type 判定为未知并留痕丢弃。
type ChatContentPart struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ImageURL   *ChatImageURL   `json:"image_url,omitempty"`
	InputAudio json.RawMessage `json:"input_audio,omitempty"`
}

// ChatImageURL 图片引用。UnmarshalJSON 兼容 {"url":...} 对象与裸字符串两种写法。
type ChatImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

func (u *ChatImageURL) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &u.URL)
	}
	// alias 断开递归：否则会再次调用本方法而无限递归
	type alias ChatImageURL
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*u = ChatImageURL(a)
	return nil
}

// ChatTool 工具声明。type 缺省时按 function 处理（部分客户端省略）。
type ChatTool struct {
	Type     string           `json:"type,omitempty"`
	Function *ChatFunctionDef `json:"function,omitempty"`
}

type ChatFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	// Strict 是 OpenAI 的结构化输出开关，上游无对应能力，仅在置 true 时留痕。
	Strict *bool `json:"strict,omitempty"`
}

// ChatToolCall 入站 assistant 消息里的工具调用。index 只出现在流式回放中，解析后不使用。
type ChatToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function ChatFunctionCall `json:"function"`
}

// ChatFunctionCall 出/入站共用的函数调用体：入站 arguments 是 JSON 字符串，
// 出站 arguments 是增量分片（同样是字符串，客户端按 index 拼接）。
//
// Arguments 刻意不带 omitempty：工具调用首帧必须显式给出 arguments:""，
// 客户端按该键的存在性判断「参数从这里开始累积」，缺键会丢掉首帧语义。
type ChatFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// ---------- 出站 Chat Completions 响应与流式分片 ----------

// ChatCompletion 非流式响应。Choices 用非指针切片，且生产者必须显式赋值（哪怕是空数组）：
// null 会让部分 SDK 直接报错。
type ChatCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"` // chat.completion
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *ChatUsage   `json:"usage,omitempty"`
}

// ChatChoice 非流式候选。FinishReason 用指针且**不带 omitempty**：
// null 与 "stop" 是两种语义（流未结束 / 正常结束），字段必须恒存在。
type ChatChoice struct {
	Index        int                  `json:"index"`
	Message      *ChatResponseMessage `json:"message,omitempty"`
	FinishReason *string              `json:"finish_reason"`
}

// ChatResponseMessage 非流式 message 体。Content 同样不带 omitempty：
// 只有工具调用时 OpenAI 也发 content:null，客户端据此区分「无文本」与「字段缺失」。
type ChatResponseMessage struct {
	Role             string                `json:"role"`
	Content          *string               `json:"content"`
	ReasoningContent string                `json:"reasoning_content,omitempty"`
	ToolCalls        []ChatMessageToolCall `json:"tool_calls,omitempty"`
}

// ChatMessageToolCall 非流式工具调用：不含 index（OpenAI 的 message.tool_calls 无此字段），
// 与流式增量形态（ChatDeltaToolCall）刻意分开，避免把 index 漏进非流式响应。
type ChatMessageToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ChatFunctionCall `json:"function"`
}

// ChatChunk 流式响应分片。usage 只在收尾的独立 chunk 上出现，
// 该 chunk 的 Choices 必须是空数组而非 null——OpenAI 的既有形态，客户端按此判定流结束。
type ChatChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"` // chat.completion.chunk
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []ChatChunkChoice `json:"choices"`
	Usage   *ChatUsage        `json:"usage,omitempty"`
}

type ChatChunkChoice struct {
	Index        int       `json:"index"`
	Delta        ChatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

// ChatDelta 流式增量体。Content 用「指针 + omitempty」表达三态语义：
//   - nil：本帧不带 content 键（工具调用帧）；
//   - 指向空串：显式空内容（首帧声明 role 时用）；
//   - 指向非空：正文增量。
//
// 这是 omitempty 陷阱的正面用法——普通 string + omitempty 无法区分「空内容」与「无此字段」。
type ChatDelta struct {
	Role             string              `json:"role,omitempty"`
	Content          *string             `json:"content,omitempty"`
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	ToolCalls        []ChatDeltaToolCall `json:"tool_calls,omitempty"`
}

// ChatDeltaToolCall 增量工具调用：首帧带 index/id/type/name，后续帧只带 index + arguments 分片。
// Index 不带 omitempty（0 也必须出现，否则客户端无法归并多工具调用）。
type ChatDeltaToolCall struct {
	Index    int              `json:"index"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function ChatFunctionCall `json:"function"`
}

// ChatUsage OpenAI usage 口径：prompt_tokens **含**缓存命中，
// 与 Anthropic 的 input_tokens 只计非缓存部分相反（换算见 chatout.go）。
type ChatUsage struct {
	PromptTokens        int                     `json:"prompt_tokens"`
	CompletionTokens    int                     `json:"completion_tokens"`
	TotalTokens         int                     `json:"total_tokens"`
	PromptTokensDetails ChatPromptTokensDetails `json:"prompt_tokens_details"`
}

// ChatPromptTokensDetails 缓存命中量单列，同时已计入 prompt_tokens（不重复相加）。
type ChatPromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}
