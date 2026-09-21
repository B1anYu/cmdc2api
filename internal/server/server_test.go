package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/B1anYu/cmdc2api/internal/config"
	"github.com/B1anYu/cmdc2api/internal/masq"
)

// newFakeCC 模拟 cmdc 上游：记录收到的请求，按脚本回 NDJSON。
type fakeCC struct {
	mu         sync.Mutex
	script     []string
	status     int // 非 0 时 /alpha/generate 返回该状态码
	errBody    string
	inits      int
	modelsHits int // /provider/v1/models 被真实请求的次数（缓存隔离断言用）
	genReqs    []capturedGen
}

type capturedGen struct {
	headers http.Header
	body    map[string]any
}

func newFakeCC(t *testing.T) *fakeCC {
	return &fakeCC{script: defaultScript()}
}

func defaultScript() []string {
	return []string{
		`{"type":"start"}`,
		`{"type":"reasoning-delta","text":"pondering"}`,
		`{"type":"text-delta","text":"Hello!"}`,
		`{"type":"tool-call","toolCallId":"toolu_1","toolName":"get_weather","input":{"city":"SF"}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":100,"outputTokens":42,"cachedInputTokens":10}}`,
	}
}

// newProxy 起一个连着假上游的代理。
func newProxy(t *testing.T, f *fakeCC, mutate func(*config.Config)) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 假上游本体
		f.serve(w, r)
	}))
	t.Cleanup(up.Close)

	cfg := config.Config{
		Port:         0,
		Host:         "127.0.0.1",
		APIBase:      up.URL,
		StateFile:    filepath.Join(t.TempDir(), "state.json"),
		ZDR:          false,
		MaxBodyBytes: 10 << 20,
		ModelRefresh: time.Minute,
		IdleStream:   5 * time.Second,
		IdleBuffer:   5 * time.Second,
		SessionStrategy:   "prefix",
		AssistantReasoning: false,
		FakeNodeVersion:   "v22.21.0",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	state := masq.LoadState(cfg.StateFile)
	ver := masq.NewCCVersion() // 不 Start()，避免测试访问 npm
	srv := New(cfg, state, ver)
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	return proxy
}

// serve 复用 mux 逻辑：直接把假上游做成独立 handler 集合。
func (f *fakeCC) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/alpha/fingerprint/record" && r.Method == "POST":
		f.mu.Lock()
		f.inits++
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case r.URL.Path == "/alpha/lifecycle-events" && r.Method == "POST":
		w.WriteHeader(http.StatusOK)
	case r.URL.Path == "/provider/v1/models" && r.Method == "GET":
		f.mu.Lock()
		f.modelsHits++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// 按 key 返回不同列表：/v1/models 的缓存必须按 key 隔离（F28），
		// 否则后一个 key（或无 key 请求）会拿到前一个 key 拉回来的列表。
		if strings.HasSuffix(r.Header.Get("Authorization"), "user_keyb") {
			_, _ = w.Write([]byte(`{"data":[{"id":"provider/model-b"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"provider/model-x"}]}`))
	case r.URL.Path == "/alpha/generate" && r.Method == "POST":
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		f.mu.Lock()
		f.genReqs = append(f.genReqs, capturedGen{headers: r.Header.Clone(), body: parsed})
		status, errBody, script := f.status, f.errBody, f.script
		f.mu.Unlock()
		if status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(errBody))
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		if flusher, ok := w.(http.Flusher); ok {
			for _, line := range script {
				_, _ = w.Write([]byte(line + "\n"))
				flusher.Flush()
			}
		} else {
			for _, line := range script {
				_, _ = w.Write([]byte(line + "\n"))
			}
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

const anthropicReq = `{
	"model": "claude-sonnet-4-6",
	"max_tokens": 1024,
	"stream": true,
	"system": [{"type": "text", "text": "You are a weather bot.", "cache_control": {"type": "ephemeral"}}],
	"tools": [{"name": "get_weather", "description": "get weather", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}],
	"messages": [{"role": "user", "content": "weather in SF?"}]
}`

func postMessages(t *testing.T, proxy *httptest.Server, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestEndToEnd_StreamHappyPath(t *testing.T) {
	f := newFakeCC(t)
	proxy := newProxy(t, f, nil)

	resp := postMessages(t, proxy, anthropicReq, map[string]string{"x-api-key": "user_test123"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type = %s", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	sse := string(body)

	// SSE 生命周期完整性
	if !strings.HasPrefix(sse, "event: message_start") {
		t.Errorf("stream must start with message_start:\n%.200s", sse)
	}
	if !strings.Contains(sse, "thinking_delta") || !strings.Contains(sse, "signature_delta") {
		t.Errorf("thinking + signature missing:\n%s", sse)
	}
	if !strings.Contains(sse, `"partial_json":"{\"city\":\"SF\"}"`) {
		t.Errorf("tool input missing:\n%s", sse)
	}
	if !strings.Contains(sse, `"stop_reason":"tool_use"`) {
		t.Errorf("stop_reason missing:\n%s", sse)
	}
	if !strings.HasSuffix(strings.TrimSpace(sse), "event: message_stop\ndata: {\"type\":\"message_stop\"}") {
		t.Errorf("stream must end with message_stop:\n%.200s", sse)
	}

	// 上游侧断言：伪装头 + 直转信封
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.genReqs) != 1 {
		t.Fatalf("generate calls = %d", len(f.genReqs))
	}
	gen := f.genReqs[0]
	h := gen.headers
	checkUpstreamHeaders(t, h)
	// prefix 会话策略：无显式 session 头时必须带 x-session-id
	if h.Get("x-session-id") == "" {
		t.Error("x-session-id missing")
	}

	params := gen.body["params"].(map[string]any)
	if sys, ok := params["system"].(string); !ok || !strings.Contains(sys, "weather bot") {
		t.Errorf("params.system wrong: %v", params["system"])
	}
	if params["stream"] != true {
		t.Error("params.stream must be true")
	}
	if _, ok := params["tools"].([]any); !ok {
		t.Error("params.tools missing")
	}
	// system 带 cache_control 而 part 无 → 首个 user text part 合成 ephemeral 标记
	msgs := params["messages"].([]any)
	first := msgs[0].(map[string]any)
	parts := first["content"].([]any)
	part := parts[0].(map[string]any)
	if cc, ok := part["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
		t.Errorf("synthesized cache marker missing: %v", part)
	}
}

func TestEndToEnd_SessionAffinity(t *testing.T) {
	f := newFakeCC(t)
	proxy := newProxy(t, f, nil)

	// 同一对话两轮：首条用户消息不变 → 同一会话
	post1 := postMessages(t, proxy, anthropicReq, map[string]string{"x-api-key": "user_test123"})
	_, _ = io.Copy(io.Discard, post1.Body)
	_ = post1.Body.Close()
	post2 := postMessages(t, proxy, `{
		"model": "claude-sonnet-4-6", "max_tokens": 1024, "stream": true,
		"system": [{"type": "text", "text": "You are a weather bot."}],
		"tools": [{"name": "get_weather", "description": "get weather", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}],
		"messages": [
			{"role": "user", "content": "weather in SF?"},
			{"role": "assistant", "content": "Let me check."},
			{"role": "user", "content": "thanks"}
		]
	}`, map[string]string{"x-api-key": "user_test123"})
	_, _ = io.Copy(io.Discard, post2.Body)
	_ = post2.Body.Close()

	f.mu.Lock()
	if len(f.genReqs) != 2 {
		f.mu.Unlock()
		t.Fatalf("generate calls = %d", len(f.genReqs))
	}
	s1 := f.genReqs[0].headers.Get("x-session-id")
	s2 := f.genReqs[1].headers.Get("x-session-id")
	f.mu.Unlock()
	if s1 == "" || s1 != s2 {
		t.Errorf("same conversation must reuse session: %q vs %q", s1, s2)
	}

	// 显式 session 头优先
	post3 := postMessages(t, proxy, anthropicReq, map[string]string{
		"x-api-key": "user_test123", "X-Session-Id": "explicit-session-42",
	})
	_, _ = io.Copy(io.Discard, post3.Body)
	_ = post3.Body.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.genReqs) != 3 {
		t.Fatalf("generate calls = %d, want 3", len(f.genReqs))
	}
	if s3 := f.genReqs[2].headers.Get("x-session-id"); s3 != "explicit-session-42" {
		t.Errorf("explicit session header must win, got %q", s3)
	}
}

func checkUpstreamHeaders(t *testing.T, h http.Header) {
	t.Helper()
	if ua := h.Get("User-Agent"); strings.HasPrefix(ua, "Go-http-client") {
		t.Errorf("default Go UA leaked: %q", ua)
	}
	if h.Get("Authorization") != "Bearer user_test123" {
		t.Errorf("authorization = %q", h.Get("Authorization"))
	}
	if h.Get("x-cli-environment") != "production" {
		t.Error("x-cli-environment missing")
	}
	if h.Get("x-command-code-version") == "" {
		t.Error("x-command-code-version missing")
	}
	if h.Get("x-co-flag") != "false" || h.Get("x-taste-learning") != "false" {
		t.Error("x-co-flag/x-taste-learning missing")
	}
	if !strings.HasPrefix(h.Get("x-project-slug"), "users-dev-projects-") {
		t.Errorf("x-project-slug = %q", h.Get("x-project-slug"))
	}
	if !traceparentRe.MatchString(h.Get("traceparent")) {
		t.Errorf("traceparent = %q", h.Get("traceparent"))
	}
}

var traceparentRe = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)

func TestEndToEnd_NonStreamAggregation(t *testing.T) {
	f := newFakeCC(t)
	proxy := newProxy(t, f, nil)

	req := strings.Replace(anthropicReq, `"stream": true`, `"stream": false`, 1)
	resp := postMessages(t, proxy, req, map[string]string{"x-api-key": "user_test123"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var msg map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Errorf("message meta wrong: %v", msg)
	}
	content := msg["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content = %d blocks, want 3 (thinking/text/tool_use): %v", len(content), content)
	}
	thinking := content[0].(map[string]any)
	if thinking["type"] != "thinking" || thinking["signature"] == nil {
		t.Errorf("thinking block wrong: %v", thinking)
	}
	tu := content[2].(map[string]any)
	if tu["type"] != "tool_use" || tu["name"] != "get_weather" {
		t.Errorf("tool_use block wrong: %v", tu)
	}
	if inp, ok := tu["input"].(map[string]any); !ok || inp["city"] != "SF" {
		t.Errorf("tool input wrong: %v", tu["input"])
	}
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", msg["stop_reason"])
	}
	usage := msg["usage"].(map[string]any)
	// input_tokens = inputTokens − cached = 100 − 10 = 90
	if usage["input_tokens"] != float64(90) || usage["output_tokens"] != float64(42) ||
		usage["cache_read_input_tokens"] != float64(10) {
		t.Errorf("usage wrong: %v", usage)
	}
}

func TestEndToEnd_AuthAndValidation(t *testing.T) {
	f := newFakeCC(t)
	proxy := newProxy(t, f, nil)

	// 401：缺 key（文案点明期望的 key 形状，见 missingAPIKeyMessage）
	resp := postMessages(t, proxy, anthropicReq, nil)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", resp.StatusCode)
	}
	if !strings.Contains(body, "user_") {
		t.Errorf("401 message must hint the user_ key shape: %s", body)
	}
	// 非 user_ 形态的 key 不再被本地拒绝：代理不校验 key 合法性，原样透传上游，
	// 由上游判定并把错误原路返回（F26）。断言见 TestEndToEnd_NonUserKeyPassedThrough。
	resp = postMessages(t, proxy, anthropicReq, map[string]string{"x-api-key": "sk-bad"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("non-user key: status = %d, want 200 (透传上游)", resp.StatusCode)
	}
	// 400：坏 JSON（Anthropic 端点 → Anthropic 形状；OpenAI 端点侧见
	// TestChatCompletions_ReadBodyErrorsUseOpenAIShape / TestResponses_ReadBodyErrorsUseOpenAIShape）
	resp = postMessages(t, proxy, `{not json`, map[string]string{"x-api-key": "user_test123"})
	body = readAll(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad json: status = %d, want 400", resp.StatusCode)
	}
	assertAnthropicError(t, decodeJSON(t, body), "invalid_request_error", "Invalid JSON body")
	// 400：空 messages
	resp = postMessages(t, proxy, `{"model":"m","messages":[]}`, map[string]string{"x-api-key": "user_test123"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty messages: status = %d, want 400", resp.StatusCode)
	}
	// 413：超限（MaxBodyBytes 压到 1KB）
	small := newProxy(t, f, func(c *config.Config) { c.MaxBodyBytes = 1024 })
	resp = postMessages(t, small, anthropicReq+strings.Repeat(" ", 4096), map[string]string{"x-api-key": "user_test123"})
	body = readAll(t, resp)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize: status = %d, want 413", resp.StatusCode)
	}
	assertAnthropicError(t, decodeJSON(t, body), "invalid_request_error", "exceeds")
	// 404：未知路径
	resp2, _ := http.Get(proxy.URL + "/v1/unknown")
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path: status = %d, want 404", resp2.StatusCode)
	}
}

func TestEndToEnd_UpstreamErrorMapping(t *testing.T) {
	f := newFakeCC(t)
	f.mu.Lock()
	f.status = 429
	f.errBody = `{"error":{"message":"rate limited, slow down"}}`
	f.mu.Unlock()
	proxy := newProxy(t, f, nil)

	resp := postMessages(t, proxy, anthropicReq, map[string]string{"x-api-key": "user_test123"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "30" {
		t.Errorf("Retry-After = %q, want 30", ra)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	errObj := body["error"].(map[string]any)
	if errObj["type"] != "rate_limit_error" || !strings.Contains(errObj["message"].(string), "slow down") {
		t.Errorf("error body wrong: %v", errObj)
	}
}

func TestEndToEnd_ZeroOutputReturns429BeforeHeaders(t *testing.T) {
	f := newFakeCC(t)
	f.mu.Lock()
	f.script = []string{
		`{"type":"start"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":50,"outputTokens":0}}`,
	}
	f.mu.Unlock()
	proxy := newProxy(t, f, nil)

	resp := postMessages(t, proxy, anthropicReq, map[string]string{"x-api-key": "user_test123"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("zero output: status = %d, want 429 (delayed 200)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %s", ct)
	}
}

func TestEndToEnd_LifecyclePreRequests(t *testing.T) {
	f := newFakeCC(t)
	proxy := newProxy(t, f, nil)

	resp := postMessages(t, proxy, anthropicReq, map[string]string{"x-api-key": "user_test456"})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inits < 1 {
		t.Errorf("fingerprint/record should fire on first request per key, got %d", f.inits)
	}
}

func TestEndToEnd_Models(t *testing.T) {
	f := newFakeCC(t)
	proxy := newProxy(t, f, nil)

	// 无 key → 硬编码回退
	resp, _ := http.Get(proxy.URL + "/v1/models")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	_ = resp.Body.Close()
	data := body["data"].([]any)
	first := data[0].(map[string]any)
	if first["type"] != "model" || first["id"] != "claude-sonnet-4-6" {
		t.Errorf("fallback models wrong: %v", first)
	}
}

func TestEndToEnd_Health(t *testing.T) {
	f := newFakeCC(t)
	proxy := newProxy(t, f, nil)
	resp, err := http.Get(proxy.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(b) != "OK" {
		t.Errorf("health = %d %q", resp.StatusCode, b)
	}
}

// 兜底 404（未注册路径）与 panic 恢复这类「handler 之外的出口」也必须按端点协议出形状：
// OpenAI 端点出 OpenAI 形状，其余保持 Anthropic 形状，否则客户端 SDK 解析失败。
// 形状判定依据：Anthropic 体有顶层 "type":"error"，OpenAI 体只有 "error" 对象。
func TestEndToEnd_NotFoundShapeByPath(t *testing.T) {
	cases := []struct {
		path   string
		openAI bool
	}{
		{"/v1/messages/nope", false},
		{"/v1/models/nope", false},
		{"/nope", false},
		{"/v1/chat/completions/nope", true},
		{"/v1/responses/nope", true},
	}
	f := newFakeCC(t)
	proxy := newProxy(t, f, nil)
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := http.Post(proxy.URL+tc.path, "application/json", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", resp.StatusCode)
			}
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			_, hasTopLevelType := body["type"]
			if hasTopLevelType == tc.openAI {
				t.Fatalf("body = %v, want openAI=%v", body, tc.openAI)
			}
			errObj, ok := body["error"].(map[string]any)
			if !ok || errObj["type"] != "not_found_error" {
				t.Errorf("error = %v, want {type: not_found_error}", body["error"])
			}
			if !tc.openAI && errObj["message"] != "Not found" {
				t.Errorf("message = %v", errObj["message"])
			}
		})
	}
}

// ---------- 鉴权：不校验 key 形态（F26） ----------

// assertAnthropicError 断言 Anthropic 形状错误体：顶层 "type":"error" + error.{type,message}。
// OpenAI 形状无顶层 type（判定依据见 TestEndToEnd_NotFoundShapeByPath）。
func assertAnthropicError(t *testing.T, body map[string]any, wantType, wantMsgSub string) {
	t.Helper()
	if got := mStr(t, body, "type"); got != "error" {
		t.Errorf("top-level type = %q, want error (Anthropic shape)", got)
	}
	errObj := mMap(t, body, "error")
	if got := mStr(t, errObj, "type"); got != wantType {
		t.Errorf("error.type = %q, want %q", got, wantType)
	}
	if msg := mStr(t, errObj, "message"); !strings.Contains(msg, wantMsgSub) {
		t.Errorf("error.message = %q, want substring %q", msg, wantMsgSub)
	}
	if _, ok := errObj["code"]; ok {
		t.Errorf("Anthropic shape must not carry error.code: %v", errObj)
	}
}

// postJSONPath 打任意端点路径（三端点仅路径不同，错误出口形状才是有差异的部分）。
func postJSONPath(t *testing.T, proxy *httptest.Server, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxy.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestGetAPIKey_NoPrefixRestriction 代理不校验 key 的合法性（AGENTS.md §三.1）：
// getAPIKey 只做「取 Bearer 之后的 token / 取 x-api-key 原值」的提取，不限制前缀形状。
func TestGetAPIKey_NoPrefixRestriction(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"x-api-key user_", map[string]string{"x-api-key": "user_abc123"}, "user_abc123"},
		{"x-api-key sk-", map[string]string{"x-api-key": "sk-abc123"}, "sk-abc123"},
		{"x-api-key 原值去掉首尾空白", map[string]string{"x-api-key": "  sk-abc123  "}, "sk-abc123"},
		{"bearer user_", map[string]string{"Authorization": "Bearer user_abc123"}, "user_abc123"},
		{"bearer sk-", map[string]string{"Authorization": "Bearer sk-abc123"}, "sk-abc123"},
		{"bearer token 去掉首尾空白", map[string]string{"Authorization": "Bearer   sk-abc123 "}, "sk-abc123"},
		{"bearer 空 token 时回落到 x-api-key", map[string]string{"Authorization": "Bearer ", "x-api-key": "user_zzz"}, "user_zzz"},
		{"非 Bearer 方案不当作 key", map[string]string{"Authorization": "Basic dXNlcg=="}, ""},
		{"无鉴权头", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			if got := getAPIKey(h); got != tc.want {
				t.Errorf("getAPIKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMissingAPIKeyMessageMentionsUserPrefix 401 只在确实没发 key 时触发，
// 文案需点明期望的 cmdc key 形状（user_ 前缀），且三端点共用同一句（避免三份漂移）。
func TestMissingAPIKeyMessageMentionsUserPrefix(t *testing.T) {
	f := newFakeCC(t)
	proxy := newProxy(t, f, nil)

	cases := []struct{ name, path, body string }{
		{"messages", "/v1/messages", anthropicReq},
		{"chat", "/v1/chat/completions", chatRequestBody(false)},
		{"responses", "/v1/responses", responsesBody(false, "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postJSONPath(t, proxy, tc.path, tc.body, nil)
			body := readAll(t, resp)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401\nbody: %s", resp.StatusCode, body)
			}
			if !strings.Contains(body, "user_") {
				t.Errorf("401 message must hint the user_ key shape: %s", body)
			}
			if !strings.Contains(body, "Missing API key") {
				t.Errorf("401 message must keep the 'Missing API key' phrasing: %s", body)
			}
		})
	}

	// 未鉴权的请求不得触上游
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.genReqs) != 0 {
		t.Errorf("unauthenticated requests must not reach upstream, got %d", len(f.genReqs))
	}
}

// TestEndToEnd_NonUserKeyPassedThrough 非 user_ 形态的 key（如 sk-…）不做本地格式校验：
// 必须原样透传给上游（上游会报 invalid api key，原路返回给客户端），
// 而不是在本地被判成「没发 key」——那会把「格式不对」误报成「没发」。
func TestEndToEnd_NonUserKeyPassedThrough(t *testing.T) {
	cases := []struct {
		name       string
		headers    map[string]string
		wantBearer string
	}{
		{"x-api-key", map[string]string{"x-api-key": "sk-test-key"}, "Bearer sk-test-key"},
		{"authorization bearer", map[string]string{"Authorization": "Bearer sk-test-key"}, "Bearer sk-test-key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCC(t)
			proxy := newProxy(t, f, nil)

			resp := postMessages(t, proxy, anthropicReq, tc.headers)
			body := readAll(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (key 应透传上游)\nbody: %s", resp.StatusCode, body)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.genReqs) != 1 {
				t.Fatalf("generate calls = %d, want 1", len(f.genReqs))
			}
			if got := f.genReqs[0].headers.Get("Authorization"); got != tc.wantBearer {
				t.Errorf("upstream Authorization = %q, want %q", got, tc.wantBearer)
			}
		})
	}
}

// ---------- /v1/models 缓存按 key 隔离（F28） ----------

// TestEndToEnd_ModelsCacheIsPerKey models 缓存曾挂在 Upstream 单例上（先填后串号）：
// 先用 key A 拉取，再用无 key 与 key B 请求，都必须拿到各自该拿的列表，
// 而不是 A 缓存里的那一份。
func TestEndToEnd_ModelsCacheIsPerKey(t *testing.T) {
	f := newFakeCC(t)
	proxy := newProxy(t, f, nil)

	fetch := func(t *testing.T, key string) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, proxy.URL+"/v1/models", nil)
		if err != nil {
			t.Fatal(err)
		}
		if key != "" {
			req.Header.Set("x-api-key", key)
		}
		resp, err := proxy.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body: %s", resp.StatusCode, body)
		}
		data := mList(t, decodeJSON(t, body), "data")
		if len(data) == 0 {
			t.Fatalf("data is empty: %s", body)
		}
		return mStr(t, data[0].(map[string]any), "id")
	}

	// key A 填充缓存
	if got := fetch(t, "user_keya"); got != "provider/model-x" {
		t.Fatalf("key A list = %q, want provider/model-x", got)
	}
	// 无 key：恒为兜底列表，不得命中 A 的缓存
	if got := fetch(t, ""); got != "claude-sonnet-4-6" {
		t.Errorf("无 key 请求拿到 %q，应为兜底列表（未隔离的缓存会串成 key A 的列表）", got)
	}
	// key B：拿到自己的列表，不得命中 A 的缓存
	if got := fetch(t, "user_keyb"); got != "provider/model-b" {
		t.Errorf("key B 拿到 %q，应为 provider/model-b（未隔离的缓存会串成 key A 的列表）", got)
	}
	// 同一 key 仍走 TTL 缓存（行为不变）
	if got := fetch(t, "user_keya"); got != "provider/model-x" {
		t.Errorf("key A 复取 = %q", got)
	}
	var modelsCalls int
	f.mu.Lock()
	modelsCalls = f.modelsHits
	f.mu.Unlock()
	if modelsCalls != 2 {
		t.Errorf("upstream models calls = %d, want 2 (A/B 各一次；同 key 复取走缓存)", modelsCalls)
	}
}
