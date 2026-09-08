package types

import "encoding/json"

// ---------- cmdc 请求信封（/alpha/generate 请求体） ----------

// CcRequest 结构对齐真实 CLI 抓包（proxy.mjs buildCcRequest）。
// 注意：params.system 恒为字符串，数组会被上游直接拒（400 Validation error）。
type CcRequest struct {
	Config         CcConfig  `json:"config"`
	Memory         any       `json:"memory"`
	Taste          any       `json:"taste"`
	Skills         string    `json:"skills"`
	PermissionMode string    `json:"permissionMode"`
	Params         CcParams  `json:"params"`
}

type CcConfig struct {
	WorkingDir    string   `json:"workingDir"`
	Date          string   `json:"date"`
	Environment   string   `json:"environment"`
	Structure     []any    `json:"structure"`
	IsGitRepo     bool     `json:"isGitRepo"`
	CurrentBranch string   `json:"currentBranch"`
	MainBranch    string   `json:"mainBranch"`
	GitStatus     string   `json:"gitStatus"`
	RecentCommits []string `json:"recentCommits"`
}

type CcParams struct {
	Model             string        `json:"model"`
	Messages          []CcMessage   `json:"messages"`
	MaxTokens         int           `json:"max_tokens"`
	Stream            bool          `json:"stream"` // cmdc 恒为 true
	System            string        `json:"system,omitempty"`
	Temperature       *float64      `json:"temperature,omitempty"`
	ReasoningEffort   string        `json:"reasoning_effort,omitempty"`
	Tools             []CcTool      `json:"tools,omitempty"`
	ToolChoice        *CcToolChoice `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool         `json:"parallel_tool_calls,omitempty"`
}

type CcMessage struct {
	Role    string   `json:"role"`
	Content []CcPart `json:"content"`
}

// CcPart cmdc content part 并集：text / image / tool-call / tool-result
// /（实验性）reasoning。cache_control 为 PR#10 验证过的缓存标记。
type CcPart struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Image        string          `json:"image,omitempty"` // data URI 或 URL
	ToolCallID   string          `json:"toolCallId,omitempty"`
	ToolName     string          `json:"toolName,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	Output       *CcToolOutput   `json:"output,omitempty"`
	CacheControl *CacheControl   `json:"cache_control,omitempty"`
}

type CcToolOutput struct {
	Type  string `json:"type"` // "text"
	Value string `json:"value"`
}

type CcTool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// CcToolChoice Anthropic 风味 {type: auto|any|tool|none, name?}。
type CcToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// ---------- cmdc NDJSON 响应事件 ----------

// CcEvent 上游 NDJSON 一行。字段按事件类型选择性出现；
// text-delta 的载荷字段可能是 text 或 delta，两者都收。
type CcEvent struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Delta        string          `json:"delta,omitempty"`
	ToolCallID   string          `json:"toolCallId,omitempty"`
	ToolName     string          `json:"toolName,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	FinishReason string          `json:"finishReason,omitempty"`
	TotalUsage   *CcUsage        `json:"totalUsage,omitempty"`
	Usage        *CcUsage        `json:"usage,omitempty"`
	Error        *CcEventError   `json:"error,omitempty"`
	Message      string          `json:"message,omitempty"`
}

type CcEventError struct {
	Message string `json:"message"`
}

// CcUsage 上游 usage 形状（proxy.mjs normalizeUsage 观察）。
// 实弹定案：inputTokens 把缓存读取重复计入（真实总输入 = inputTokens −
// cachedInputTokens），出站前由 translate 换算为 Anthropic 口径。
type CcUsage struct {
	InputTokens       int `json:"inputTokens"`
	OutputTokens      int `json:"outputTokens"`
	CachedInputTokens int `json:"cachedInputTokens"`
	InputTokenDetails *struct {
		CacheWriteTokens int `json:"cacheWriteTokens"`
	} `json:"inputTokenDetails,omitempty"`
}

// Normalize 反虚假计费：outputTokens 为 0 时把 input/cached 一并清零
// （上游偶尔报非零 input + 零 output，按错误处理）。
func (u *CcUsage) Normalize() {
	if u != nil && u.OutputTokens == 0 {
		u.InputTokens = 0
		u.CachedInputTokens = 0
	}
}
