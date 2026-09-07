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
	mu      sync.Mutex
	script  []string
	status  int // 非 0 时 /alpha/generate 返回该状态码
	errBody string
	inits   int
	genReqs []capturedGen
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
		w.Header().Set("Content-Type", "application/json")
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
	defer resp.Body.Close()
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
	io.Copy(io.Discard, post1.Body)
	post1.Body.Close()
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
	io.Copy(io.Discard, post2.Body)
	post2.Body.Close()

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
	io.Copy(io.Discard, post3.Body)
	post3.Body.Close()
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
	defer resp.Body.Close()
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
	if usage["input_tokens"] != float64(100) || usage["output_tokens"] != float64(42) ||
		usage["cache_read_input_tokens"] != float64(10) {
		t.Errorf("usage wrong: %v", usage)
	}
}

func TestEndToEnd_AuthAndValidation(t *testing.T) {
	f := newFakeCC(t)
	proxy := newProxy(t, f, nil)

	// 401：缺 key
	resp := postMessages(t, proxy, anthropicReq, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", resp.StatusCode)
	}
	// 401：非 user_ key
	resp = postMessages(t, proxy, anthropicReq, map[string]string{"x-api-key": "sk-bad"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad key: status = %d, want 401", resp.StatusCode)
	}
	// 400：坏 JSON
	resp = postMessages(t, proxy, `{not json`, map[string]string{"x-api-key": "user_test123"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad json: status = %d, want 400", resp.StatusCode)
	}
	// 400：空 messages
	resp = postMessages(t, proxy, `{"model":"m","messages":[]}`, map[string]string{"x-api-key": "user_test123"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty messages: status = %d, want 400", resp.StatusCode)
	}
	// 413：超限（MaxBodyBytes 压到 1KB）
	small := newProxy(t, f, func(c *config.Config) { c.MaxBodyBytes = 1024 })
	resp = postMessages(t, small, anthropicReq+strings.Repeat(" ", 4096), map[string]string{"x-api-key": "user_test123"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize: status = %d, want 413", resp.StatusCode)
	}
	// 404：未知路径
	resp2, _ := http.Get(proxy.URL + "/v1/unknown")
	resp2.Body.Close()
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
	defer resp.Body.Close()
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
	defer resp.Body.Close()
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
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

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
	resp.Body.Close()
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
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(b) != "OK" {
		t.Errorf("health = %d %q", resp.StatusCode, b)
	}
}
