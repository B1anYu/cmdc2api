package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/B1anYu/cmdc2api/internal/config"
	"github.com/B1anYu/cmdc2api/internal/masq"
)

// ---------- 请求构造与上游 harness ----------

// chatAuth 测试用客户端鉴权头（cmdc key 形状；getAPIKey 只做提取，不限前缀）。
var chatAuth = map[string]string{"x-api-key": "user_test123"}

// chatRequestBody 组一个最小可用的 Chat 请求体。model 用回退列表里的名字，
// 保证 /v1/models 与 chat 两条路径可用同一个模型名。
func chatRequestBody(stream bool) string {
	return fmt.Sprintf(`{
		"model": "claude-sonnet-4-6",
		"stream": %t,
		"messages": [{"role": "user", "content": "weather in SF?"}]
	}`, stream)
}

// postChat 打 /v1/chat/completions。
func postChat(t *testing.T, proxy *httptest.Server, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions", strings.NewReader(body))
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

// newChatProxy 与 server_test.go 的 newProxy 同构（同配置、同假上游装配），
// 但允许注入任意上游 handler：中途断流场景需要在 /alpha/generate 上劫持连接，
// 而 fakeCC 只能写完整响应，做不到半截关闭。
func newChatProxy(t *testing.T, up http.Handler, mutate func(*config.Config)) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(up)
	t.Cleanup(upstream.Close)

	cfg := config.Config{
		Port:               0,
		Host:               "127.0.0.1",
		APIBase:            upstream.URL,
		StateFile:          filepath.Join(t.TempDir(), "state.json"),
		ZDR:                false,
		MaxBodyBytes:       10 << 20,
		ModelRefresh:       time.Minute,
		IdleStream:         5 * time.Second,
		IdleBuffer:         5 * time.Second,
		SessionStrategy:    "prefix",
		AssistantReasoning: false,
		FakeNodeVersion:    "v22.21.0",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	proxy := httptest.NewServer(New(cfg, masq.LoadState(cfg.StateFile), masq.NewCCVersion()).Handler())
	t.Cleanup(proxy.Close)
	return proxy
}

// chatProxy 起一个连着假上游（fakeCC）的代理，脚本由 script 指定。
func chatProxy(t *testing.T, script []string) (*fakeCC, *httptest.Server) {
	t.Helper()
	f := newFakeCC(t)
	f.mu.Lock()
	f.script = script
	f.mu.Unlock()
	return f, newProxy(t, f, nil)
}

// readAll 读完并关闭响应体。
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// ---------- SSE / JSON 断言工具 ----------

// chatDataLines 抽出 SSE 流的 data: 载荷（Chat 的帧没有 event: 行）。
func chatDataLines(t *testing.T, sse string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(sse, "\n") {
		if payload, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "data: "); ok {
			out = append(out, payload)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no SSE data frame in body:\n%s", sse)
	}
	return out
}

// assertOrdered 断言 subs 在 s 中按序出现。用有序子串而非整帧比对：
// id/created 每次请求都不同，整帧相等只能靠正则，脆弱且不比子串多验证什么。
func assertOrdered(t *testing.T, label, s string, subs []string) {
	t.Helper()
	idx := 0
	for _, sub := range subs {
		n := strings.Index(s[idx:], sub)
		if n < 0 {
			t.Fatalf("%s: %q not found in order\nbody:\n%s", label, sub, s)
		}
		idx += n + len(sub)
	}
}

// assertAbsent 断言 subs 全部不出现。
func assertAbsent(t *testing.T, label, s string, subs []string) {
	t.Helper()
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			t.Errorf("%s: %q must not appear\nbody:\n%s", label, sub, s)
		}
	}
}

func mGet(t *testing.T, m map[string]any, key string) any {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("key %q missing in %v", key, m)
	}
	return v
}

func mMap(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := mGet(t, m, key).(map[string]any)
	if !ok {
		t.Fatalf("key %q is not an object: %v", key, m[key])
	}
	return v
}

func mList(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	v, ok := mGet(t, m, key).([]any)
	if !ok {
		t.Fatalf("key %q is not an array: %v", key, m[key])
	}
	return v
}

func mStr(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := mGet(t, m, key).(string)
	if !ok {
		t.Fatalf("key %q is not a string: %v", key, m[key])
	}
	return v
}

func mNum(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := mGet(t, m, key).(float64)
	if !ok {
		t.Fatalf("key %q is not a number: %v", key, m[key])
	}
	return v
}

// decodeJSON 解析响应体为对象。
func decodeJSON(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode body: %v\nbody:\n%s", err, body)
	}
	return m
}

// firstChoice 取 choices[0]。
func firstChoice(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	choices := mList(t, body, "choices")
	if len(choices) == 0 {
		t.Fatalf("choices is empty: %v", body)
	}
	c, ok := choices[0].(map[string]any)
	if !ok {
		t.Fatalf("choices[0] is not an object: %v", choices[0])
	}
	return c
}

// assertChatUsage 断言 OpenAI 口径 usage：prompt 含缓存命中、total 为两者之和、缓存量单列。
func assertChatUsage(t *testing.T, body map[string]any, prompt, completion, cached float64) {
	t.Helper()
	usage := mMap(t, body, "usage")
	if got := mNum(t, usage, "prompt_tokens"); got != prompt {
		t.Errorf("prompt_tokens = %v, want %v", got, prompt)
	}
	if got := mNum(t, usage, "completion_tokens"); got != completion {
		t.Errorf("completion_tokens = %v, want %v", got, completion)
	}
	if got := mNum(t, usage, "total_tokens"); got != prompt+completion {
		t.Errorf("total_tokens = %v, want %v", got, prompt+completion)
	}
	details := mMap(t, usage, "prompt_tokens_details")
	if got := mNum(t, details, "cached_tokens"); got != cached {
		t.Errorf("cached_tokens = %v, want %v", got, cached)
	}
}

// assertOpenAIError 断言 OpenAI 形状错误体：type/code 走映射表，param 恒存在且为 null
// （SDK 按字段存在性解析，缺键比 null 更容易触发解码告警）。
func assertOpenAIError(t *testing.T, body map[string]any, wantType, wantCode, wantMsgSub string) {
	t.Helper()
	errObj := mMap(t, body, "error")
	if got := mStr(t, errObj, "type"); got != wantType {
		t.Errorf("error.type = %q, want %q", got, wantType)
	}
	if got := mStr(t, errObj, "code"); got != wantCode {
		t.Errorf("error.code = %q, want %q", got, wantCode)
	}
	param, ok := errObj["param"]
	if !ok || param != nil {
		t.Errorf("error.param must be present and null, got %v (present=%v)", param, ok)
	}
	if msg := mStr(t, errObj, "message"); wantMsgSub != "" && !strings.Contains(msg, wantMsgSub) {
		t.Errorf("error.message = %q, want substring %q", msg, wantMsgSub)
	}
}

// ---------- 场景表：流式与非流式共用同一批上游事件 ----------

// chatScenario 一组假上游 NDJSON 脚本 + 出站断言。
// 同一脚本同时打流式与非流式：两种出站形态消费的必须是同一批事件（语义不分裂）。
type chatScenario struct {
	name  string
	lines []string
	// 流式（status 恒 200：内容已开流后错误只能落在流内）
	streamOrder  []string
	streamAbsent []string
	streamSuffix string // 去空白后的流尾（断言收尾帧，等效断言 [DONE] 是否出现）
	// 非流式
	nonStreamStatus int // 0 表示 200
	checkNonStream  func(t *testing.T, body map[string]any)
}

func chatScenarios() []chatScenario {
	return []chatScenario{
		{
			name: "text",
			lines: []string{
				`{"type":"start"}`,
				`{"type":"text-delta","text":"Hello"}`,
				`{"type":"text-delta","text":" world"}`,
				`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":100,"outputTokens":7,"cachedInputTokens":10}}`,
			},
			streamOrder: []string{
				`data: {"id":"chatcmpl-`,
				`"object":"chat.completion.chunk"`,
				// 首帧只声明 role：内容帧必须等第一个真实增量
				`"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]`,
				`"model":"claude-sonnet-4-6"`,
				`"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]`,
				`"choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]`,
				`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`,
				// usage 独立 chunk：choices 空数组 + 加法回填后的 prompt_tokens(90+10)
				`"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107,"prompt_tokens_details":{"cached_tokens":10}}`,
			},
			streamSuffix: "data: [DONE]",
			checkNonStream: func(t *testing.T, body map[string]any) {
				if got := mStr(t, body, "object"); got != "chat.completion" {
					t.Errorf("object = %q", got)
				}
				if !strings.HasPrefix(mStr(t, body, "id"), "chatcmpl-") {
					t.Errorf("id = %q", mStr(t, body, "id"))
				}
				choice := firstChoice(t, body)
				msg := mMap(t, choice, "message")
				if got := mStr(t, msg, "role"); got != "assistant" {
					t.Errorf("role = %q", got)
				}
				if got := mStr(t, msg, "content"); got != "Hello world" {
					t.Errorf("content = %q", got)
				}
				if _, ok := msg["reasoning_content"]; ok {
					t.Error("reasoning_content must be omitted when there is no thinking")
				}
				if got := mStr(t, choice, "finish_reason"); got != "stop" {
					t.Errorf("finish_reason = %q", got)
				}
				assertChatUsage(t, body, 100, 7, 10)
			},
		},
		{
			name: "thinking_and_text",
			lines: []string{
				`{"type":"start"}`,
				`{"type":"reasoning-delta","text":"weighing"}`,
				`{"type":"text-delta","text":"done"}`,
				`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":5}}`,
			},
			streamOrder: []string{
				`"delta":{"role":"assistant","content":""}`,
				`"choices":[{"index":0,"delta":{"reasoning_content":"weighing"},"finish_reason":null}]`,
				`"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":null}]`,
				`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`,
				`"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":0}}`,
			},
			streamAbsent: []string{
				// 思考块的伪造签名只服务于 Anthropic 客户端的跨轮回放，Chat 侧无对应概念
				"signature",
			},
			streamSuffix: "data: [DONE]",
			checkNonStream: func(t *testing.T, body map[string]any) {
				choice := firstChoice(t, body)
				msg := mMap(t, choice, "message")
				if got := mStr(t, msg, "reasoning_content"); got != "weighing" {
					t.Errorf("reasoning_content = %q", got)
				}
				if got := mStr(t, msg, "content"); got != "done" {
					t.Errorf("content = %q", got)
				}
				assertChatUsage(t, body, 10, 5, 0)
			},
		},
		{
			name: "tool_call",
			lines: []string{
				`{"type":"start"}`,
				`{"type":"text-delta","text":"checking"}`,
				`{"type":"tool-call","toolCallId":"toolu_1","toolName":"get_weather","input":{"city":"SF"}}`,
				`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":20,"outputTokens":9}}`,
			},
			streamOrder: []string{
				`"delta":{"role":"assistant","content":""}`,
				`"delta":{"content":"checking"}`,
				// 工具首帧：index/id/type/name 齐全，arguments 显式空串
				`"delta":{"tool_calls":[{"index":0,"id":"toolu_1","type":"function","function":{"name":"get_weather","arguments":""}}]}`,
				// 参数按 10 rune 分片：`{"city":"SF"}` → 10 + 1
				`"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"S"}}]`,
				`"tool_calls":[{"index":0,"function":{"arguments":"F\"}"}}]`,
				`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`,
				`"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":9,"total_tokens":29,"prompt_tokens_details":{"cached_tokens":0}}`,
			},
			streamSuffix: "data: [DONE]",
			checkNonStream: func(t *testing.T, body map[string]any) {
				choice := firstChoice(t, body)
				if got := mStr(t, choice, "finish_reason"); got != "tool_calls" {
					t.Errorf("finish_reason = %q", got)
				}
				msg := mMap(t, choice, "message")
				// 文本与工具调用可以同轮共存：content 保留正文字段
				if got := mStr(t, msg, "content"); got != "checking" {
					t.Errorf("content = %q", got)
				}
				calls := mList(t, msg, "tool_calls")
				if len(calls) != 1 {
					t.Fatalf("tool_calls = %d, want 1", len(calls))
				}
				call := calls[0].(map[string]any)
				if got := mStr(t, call, "id"); got != "toolu_1" {
					t.Errorf("tool_calls[0].id = %q", got)
				}
				if got := mStr(t, call, "type"); got != "function" {
					t.Errorf("tool_calls[0].type = %q", got)
				}
				fn := mMap(t, call, "function")
				if got := mStr(t, fn, "name"); got != "get_weather" {
					t.Errorf("tool_calls[0].function.name = %q", got)
				}
				// 非流式必须把分片拼回完整合法 JSON
				if got := mStr(t, fn, "arguments"); got != `{"city":"SF"}` {
					t.Errorf("arguments = %q, want full JSON", got)
				}
				assertChatUsage(t, body, 20, 9, 0)
			},
		},
		{
			name: "tool_call_only",
			lines: []string{
				`{"type":"start"}`,
				`{"type":"tool-call","toolCallId":"toolu_2","toolName":"get_weather","input":{"city":"SF"}}`,
				`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":30,"outputTokens":4}}`,
			},
			streamOrder: []string{
				// 首帧依旧是 role 声明：工具调用也算内容开始
				`"delta":{"role":"assistant","content":""}`,
				`"delta":{"tool_calls":[{"index":0,"id":"toolu_2","type":"function","function":{"name":"get_weather","arguments":""}}]}`,
				`"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"S"}}]`,
				`"tool_calls":[{"index":0,"function":{"arguments":"F\"}"}}]`,
				`"finish_reason":"tool_calls"`,
			},
			streamSuffix: "data: [DONE]",
			checkNonStream: func(t *testing.T, body map[string]any) {
				msg := mMap(t, firstChoice(t, body), "message")
				// 纯工具调用时 content 是 null（OpenAI 的既有形态），不是缺字段
				if v := mGet(t, msg, "content"); v != nil {
					t.Errorf("content = %v, want null", v)
				}
				if len(mList(t, msg, "tool_calls")) != 1 {
					t.Error("tool_calls missing")
				}
				assertChatUsage(t, body, 30, 4, 0)
			},
		},
		{
			name: "usage_missing",
			lines: []string{
				`{"type":"start"}`,
				`{"type":"text-delta","text":"no usage here"}`,
				`{"type":"finish","finishReason":"stop"}`,
			},
			streamOrder: []string{
				`"delta":{"content":"no usage here"}`,
				`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`,
				// 上游漏发 usage：宁可报估算值也不编造计费数字（估算按增量条数，见 StreamTranslator.OutputTokens）
				`"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":1,"total_tokens":1,"prompt_tokens_details":{"cached_tokens":0}}`,
			},
			streamSuffix: "data: [DONE]",
			checkNonStream: func(t *testing.T, body map[string]any) {
				choice := firstChoice(t, body)
				msg := mMap(t, choice, "message")
				if got := mStr(t, msg, "content"); got != "no usage here" {
					t.Errorf("content = %q", got)
				}
				if got := mStr(t, choice, "finish_reason"); got != "stop" {
					t.Errorf("finish_reason = %q", got)
				}
				assertChatUsage(t, body, 0, 1, 0)
			},
		},
		{
			name: "error_event_after_content",
			lines: []string{
				`{"type":"start"}`,
				`{"type":"text-delta","text":"partial"}`,
				`{"type":"error","error":{"message":"upstream exploded"}}`,
			},
			streamOrder: []string{
				`"delta":{"content":"partial"}`,
				// 流内错误帧：HTTP 头已发出，只能换形状（Anthropic api_error → server_error）
				`"error":{"code":"server_error"`,
				`"message":"upstream exploded"`,
				`"type":"server_error"`,
			},
			// 错误帧之后必须终止：不得再补 finish/usage chunk，也不得发 [DONE]
			streamSuffix:    `"type":"server_error"}}`,
			nonStreamStatus: http.StatusBadGateway,
			checkNonStream: func(t *testing.T, body map[string]any) {
				assertOpenAIError(t, body, "server_error", "server_error", "upstream exploded")
			},
		},
	}
}

func TestChatCompletions_StreamScenarios(t *testing.T) {
	for _, sc := range chatScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			_, proxy := chatProxy(t, sc.lines)
			resp := postChat(t, proxy, chatRequestBody(true), chatAuth)
			if resp.StatusCode != http.StatusOK {
				_ = resp.Body.Close()
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
				t.Errorf("content-type = %q, want text/event-stream", ct)
			}
			sse := readAll(t, resp)

			// Chat 的帧形状：只有 data: 行，没有 event: 行
			if !strings.HasPrefix(sse, "data: ") {
				t.Errorf("stream must start with a data: frame:\n%.200s", sse)
			}
			assertAbsent(t, "chat sse", sse, []string{"event: "})

			payloads := chatDataLines(t, sse)
			assertOrdered(t, "chat sse", sse, sc.streamOrder)
			assertAbsent(t, "chat sse", sse, sc.streamAbsent)
			if got := strings.TrimSpace(sse); !strings.HasSuffix(got, sc.streamSuffix) {
				t.Errorf("stream must end with %q, tail:\n%.200s", sc.streamSuffix, got)
			}

			// 每一帧都必须是合法 JSON（[DONE] 除外），且首帧是 role 声明
			for _, p := range payloads {
				if p == "[DONE]" {
					continue
				}
				decodeJSON(t, p)
			}
			first := decodeJSON(t, payloads[0])
			delta := mMap(t, firstChoice(t, first), "delta")
			if got := mStr(t, delta, "role"); got != "assistant" {
				t.Errorf("first chunk delta.role = %q, want assistant", got)
			}
			if got := mStr(t, first, "object"); got != "chat.completion.chunk" {
				t.Errorf("first chunk object = %q", got)
			}
		})
	}
}

func TestChatCompletions_NonStreamScenarios(t *testing.T) {
	for _, sc := range chatScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			_, proxy := chatProxy(t, sc.lines)
			resp := postChat(t, proxy, chatRequestBody(false), chatAuth)
			want := sc.nonStreamStatus
			if want == 0 {
				want = http.StatusOK
			}
			if resp.StatusCode != want {
				body := readAll(t, resp)
				t.Fatalf("status = %d, want %d\nbody:\n%s", resp.StatusCode, want, body)
			}
			body := readAll(t, resp)
			if strings.Contains(body, "data: ") {
				t.Errorf("non-stream response must not be SSE:\n%s", body)
			}
			sc.checkNonStream(t, decodeJSON(t, body))
		})
	}
}

// ---------- 首帧前错误与中途断流 ----------

// TestChatCompletions_ErrorBeforeContent 上游在产出任何内容前报错：
// 未开流（200 头未发）时错误必须回退成标准 JSON 错误响应，客户端才能按状态码退避重试。
func TestChatCompletions_ErrorBeforeContent(t *testing.T) {
	_, proxy := chatProxy(t, []string{
		`{"type":"start"}`,
		`{"type":"error","error":{"message":"upstream exploded"}}`,
	})
	resp := postChat(t, proxy, chatRequestBody(true), chatAuth)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	body := readAll(t, resp)
	if strings.Contains(body, "data: ") {
		t.Errorf("must not emit SSE frames:\n%s", body)
	}
	assertOpenAIError(t, decodeJSON(t, body), "server_error", "server_error", "upstream exploded")
}

// truncatedProxy 上游在写完 lines 后直接掐断 TCP 连接（chunked 编码不写终止块），
// 复现真实链路的中途断流：代理侧 scanner 拿到的是 unexpected EOF 而非正常 EOF
// ——正常 EOF 会被当成一次干净结束，这条路径就测不到了。
func truncatedProxy(t *testing.T, f *fakeCC, lines []string) *httptest.Server {
	t.Helper()
	return newChatProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/alpha/generate" {
			f.serve(w, r) // 指纹/生命周期/models 等前置请求照常
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream: hijack unsupported")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("upstream: hijack failed: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nTransfer-Encoding: chunked\r\n\r\n")
		for _, line := range lines {
			payload := line + "\n"
			_, _ = fmt.Fprintf(buf, "%x\r\n%s\r\n", len(payload), payload)
		}
		_ = buf.Flush()
		// 故意不写 "0\r\n\r\n" 终止块
	}), nil)
}

func TestChatCompletions_TruncatedUpstream(t *testing.T) {
	f := newFakeCC(t)
	lines := []string{`{"type":"start"}`, `{"type":"text-delta","text":"half"}`}

	t.Run("stream", func(t *testing.T) {
		proxy := truncatedProxy(t, f, lines)
		resp := postChat(t, proxy, chatRequestBody(true), chatAuth)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (流已开启)", resp.StatusCode)
		}
		sse := readAll(t, resp)
		// 已开流 → 断流只能以流内错误帧收场，且不得补发 [DONE]
		assertOrdered(t, "chat sse", sse, []string{
			`"delta":{"content":"half"}`,
			`"error":{"code":"server_error"`,
			`Upstream error:`,
		})
		assertAbsent(t, "chat sse", sse, []string{"[DONE]", `"finish_reason":"stop"`})
		if got := strings.TrimSpace(sse); !strings.HasSuffix(got, `"type":"server_error"}}`) {
			t.Errorf("stream must end with the error frame, tail:\n%.200s", got)
		}
	})

	t.Run("non_stream", func(t *testing.T) {
		proxy := truncatedProxy(t, f, lines)
		resp := postChat(t, proxy, chatRequestBody(false), chatAuth)
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
		assertOpenAIError(t, decodeJSON(t, readAll(t, resp)), "server_error", "server_error", "Upstream error:")
	})
}

// ---------- 入站校验与错误形状 ----------

func TestChatCompletions_RequestValidation(t *testing.T) {
	_, proxy := chatProxy(t, defaultScript())

	// 401：缺 key（未鉴权的请求不应触上游）
	resp := postChat(t, proxy, chatRequestBody(false), nil)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", resp.StatusCode)
	}
	assertOpenAIError(t, decodeJSON(t, body), "invalid_request_error", "invalid_api_key", "Missing API key")

	// 400：合法 JSON 但字段类型对不上（chat 侧自有的解析出口，OpenAI 形状）
	resp = postChat(t, proxy, `{"model":"m","messages":"oops"}`, chatAuth)
	body = readAll(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad shape: status = %d, want 400", resp.StatusCode)
	}
	assertOpenAIError(t, decodeJSON(t, body), "invalid_request_error", "invalid_request_error", "Invalid request body")

	// 400：空 messages
	resp = postChat(t, proxy, `{"model":"m","messages":[]}`, chatAuth)
	body = readAll(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty messages: status = %d, want 400", resp.StatusCode)
	}
	assertOpenAIError(t, decodeJSON(t, body), "invalid_request_error", "invalid_request_error", "at least one message")

	// 400：消息全被丢弃（空 content）→ 归一化产物无任何可发送内容
	resp = postChat(t, proxy, `{"model":"m","messages":[{"role":"user","content":""}]}`, chatAuth)
	body = readAll(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no convertible content: status = %d, want 400", resp.StatusCode)
	}
	assertOpenAIError(t, decodeJSON(t, body), "invalid_request_error", "invalid_request_error", "no convertible content")

	// 400：n>1（哨兵分支，消息必须点出 n）
	resp = postChat(t, proxy, `{"model":"m","n":3,"messages":[{"role":"user","content":"hi"}]}`, chatAuth)
	body = readAll(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("n>1: status = %d, want 400", resp.StatusCode)
	}
	assertOpenAIError(t, decodeJSON(t, body), "invalid_request_error", "invalid_request_error", "Invalid 'n'")

	// n=1 必须放行（旧客户端习惯性地带上 n=1）
	resp = postChat(t, proxy, `{"model":"m","n":1,"messages":[{"role":"user","content":"hi"}]}`, chatAuth)
	body = readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("n=1: status = %d, want 200\nbody:\n%s", resp.StatusCode, body)
	}
}

// TestChatCompletions_ReadBodyErrorsUseOpenAIShape 读体阶段的两条错误出口（坏 JSON / 超限）
// 必须是 OpenAI 形状：readBody 曾硬编码 Anthropic 形状，/v1/chat/completions 会在此路径上
// 偏离端点协议（T6b 发现）。
func TestChatCompletions_ReadBodyErrorsUseOpenAIShape(t *testing.T) {
	_, proxy := chatProxy(t, defaultScript())

	// 400：语法不完整的 JSON
	resp := postChat(t, proxy, `{"model":"m","messages":`, chatAuth)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json: status = %d, want 400", resp.StatusCode)
	}
	assertOpenAIError(t, decodeJSON(t, readAll(t, resp)),
		"invalid_request_error", "invalid_request_error", "Invalid JSON body")

	// 413：请求体超限（用小上限的 Server 实例构造）
	f := newFakeCC(t)
	small := newProxy(t, f, func(c *config.Config) { c.MaxBodyBytes = 32 })
	resp = postChat(t, small, chatRequestBody(false), chatAuth)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status = %d, want 413", resp.StatusCode)
	}
	assertOpenAIError(t, decodeJSON(t, readAll(t, resp)),
		"invalid_request_error", "invalid_request_error", "exceeds")
}

// TestChatCompletions_UpstreamHTTPErrors 上游 HTTP 错误 → OpenAI 形状 + Retry-After 头。
// 流式与非流式共用同一映射（错误发生在开流前，两种形态都是 JSON 响应）。
func TestChatCompletions_UpstreamHTTPErrors(t *testing.T) {
	cases := []struct {
		name       string
		ccStatus   int
		ccBody     string
		wantStatus int
		wantType   string
		wantCode   string
		wantRetry  string
	}{
		{"rate_limited", 429, `{"error":{"message":"slow down please"}}`, 429, "rate_limit_error", "rate_limit_exceeded", "30"},
		{"payment_required", 402, `{"error":{"message":"out of credits"}}`, 429, "rate_limit_error", "rate_limit_exceeded", "30"},
		{"unauthorized", 401, `{"error":{"message":"bad api key"}}`, 401, "invalid_request_error", "invalid_api_key", ""},
		{"not_found", 404, `{"error":{"message":"no such model"}}`, 404, "not_found_error", "not_found_error", ""},
		{"server_error", 500, `{"error":{"message":"kaboom"}}`, 502, "server_error", "server_error", ""},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				f := newFakeCC(t)
				f.mu.Lock()
				f.status, f.errBody = tc.ccStatus, tc.ccBody
				f.mu.Unlock()
				proxy := newProxy(t, f, nil)

				resp := postChat(t, proxy, chatRequestBody(stream), chatAuth)
				if resp.StatusCode != tc.wantStatus {
					t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
				}
				if got := resp.Header.Get("Retry-After"); got != tc.wantRetry {
					t.Errorf("Retry-After = %q, want %q", got, tc.wantRetry)
				}
				body := readAll(t, resp)
				if strings.Contains(body, "data: ") {
					t.Errorf("error must be a JSON response, not SSE:\n%s", body)
				}
				assertOpenAIError(t, decodeJSON(t, body), tc.wantType, tc.wantCode, "")
			})
		}
	}
}

// ---------- 归一化落到上游信封 ----------

// TestChatCompletions_UpstreamEnvelope 断言 Chat 请求经归一化后在上游信封里的形状：
// 这是「星形单跳」的关键证据——Chat 客户端拿到的所有上游能力（会话亲和、缓存断点、
// 伪装）都来自与 messages 路径共用的那条链路。
func TestChatCompletions_UpstreamEnvelope(t *testing.T) {
	const reqBody = `{
		"model": "m-chat",
		"stream": false,
		"max_tokens": 111,
		"max_completion_tokens": 4096,
		"stop": ["END", "   "],
		"reasoning_effort": "minimal",
		"tools": [{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
		"messages": [
			{"role": "system", "content": "be terse"},
			{"role": "developer", "content": [{"type": "text", "text": "tools are strict"}]},
			{"role": "user", "content": "weather in SF?"},
			{"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"SF\"}"}}]},
			{"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
		]
	}`
	f, proxy := chatProxy(t, defaultScript())
	readAll(t, postChat(t, proxy, reqBody, chatAuth))
	// 同一对话第二轮：首条用户消息不变 → 必须复用同一会话
	readAll(t, postChat(t, proxy, reqBody, chatAuth))

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.genReqs) != 2 {
		t.Fatalf("generate calls = %d, want 2", len(f.genReqs))
	}
	if s1, s2 := f.genReqs[0].headers.Get("x-session-id"), f.genReqs[1].headers.Get("x-session-id"); s1 == "" || s1 != s2 {
		t.Errorf("chat must inherit prefix session affinity: %q vs %q", s1, s2)
	}
	checkUpstreamHeaders(t, f.genReqs[0].headers)

	gen := f.genReqs[0]
	params, ok := gen.body["params"].(map[string]any)
	if !ok {
		t.Fatalf("params missing in envelope: %v", gen.body)
	}
	if got := params["model"]; got != "m-chat" {
		t.Errorf("params.model = %v", got)
	}
	// system 恒为字符串（传数组会被上游 400）；system 与 developer 按书写顺序合并
	sys, ok := params["system"].(string)
	if !ok {
		t.Fatalf("params.system must be a string, got %T", params["system"])
	}
	if sys != "be terse\n\ntools are strict" {
		t.Errorf("params.system = %q", sys)
	}
	// max_completion_tokens 是 max_tokens 的新名，同时出现时前者为准
	if got := params["max_tokens"]; got != float64(4096) {
		t.Errorf("params.max_tokens = %v, want 4096", got)
	}
	// reasoning_effort: minimal → low（走 adaptive 分支）
	if got := params["reasoning_effort"]; got != "low" {
		t.Errorf("params.reasoning_effort = %v, want low", got)
	}
	// stop 是安全丢弃字段：上游无停用词能力，丢弃必须留痕（日志）且不得下发
	if _, ok := params["stop_sequences"]; ok {
		t.Error("stop_sequences must not be sent upstream")
	}
	if params["stream"] != true {
		t.Error("params.stream must be true (cmdc 恒流式)")
	}

	// 消息配对：assistant(tool-call) 与 tool 消息的 tool-result 必须相邻。
	// 断言的是 cmdc 信封的实际线格式（tool-call/tool-result + toolCallId），不是 Anthropic 字段名。
	msgs, ok := params["messages"].([]any)
	if !ok || len(msgs) != 3 {
		t.Fatalf("params.messages = %#v, want 3 (user/assistant/tool)", params["messages"])
	}
	wantRoles := []string{"user", "assistant", "tool"}
	wantTypes := []string{"text", "tool-call", "tool-result"}
	for i, raw := range msgs {
		m := raw.(map[string]any)
		if got := m["role"]; got != wantRoles[i] {
			t.Errorf("messages[%d].role = %v, want %v", i, got, wantRoles[i])
		}
		parts, ok := m["content"].([]any)
		if !ok || len(parts) == 0 {
			t.Fatalf("messages[%d].content = %#v", i, m["content"])
		}
		part := parts[0].(map[string]any)
		if got := part["type"]; got != wantTypes[i] {
			t.Errorf("messages[%d].content[0].type = %v, want %v", i, got, wantTypes[i])
		}
		if wantTypes[i] == "tool-call" {
			if part["toolCallId"] != "call_1" || part["toolName"] != "get_weather" {
				t.Errorf("tool-call part wrong: %v", part)
			}
			if input, ok := part["input"].(map[string]any); !ok || input["city"] != "SF" {
				t.Errorf("tool-call input must be parsed JSON: %v", part["input"])
			}
		}
		if wantTypes[i] == "tool-result" {
			if part["toolCallId"] != "call_1" {
				t.Errorf("tool-result must answer call_1: %v", part)
			}
			output, ok := part["output"].(map[string]any)
			if !ok || output["value"] != "sunny" {
				t.Errorf("tool-result output wrong: %v", part["output"])
			}
		}
	}

	// 工具声明走 input_schema（Anthropic 形状，不是 OpenAI 的 parameters）
	tools, ok := params["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("params.tools = %#v", params["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_weather" || tool["input_schema"] == nil {
		t.Errorf("tool declaration wrong: %v", tool)
	}
	if _, ok := tool["parameters"]; ok {
		t.Error("OpenAI-style parameters must be normalized to input_schema")
	}
}

// ---------- /v1/models 的 OpenAI 兼容超集 ----------

func TestEndToEnd_ModelsOpenAICompatShape(t *testing.T) {
	check := func(t *testing.T, body map[string]any, wantID string) {
		t.Helper()
		if got := mStr(t, body, "object"); got != "list" {
			t.Errorf("object = %q, want list", got)
		}
		// Anthropic 形状字段保持存在（字段超集，只增不删）
		for _, key := range []string{"has_more", "first_id", "last_id"} {
			mGet(t, body, key)
		}
		data := mList(t, body, "data")
		if len(data) == 0 {
			t.Fatal("data is empty")
		}
		first := data[0].(map[string]any)
		if got := mStr(t, first, "id"); got != wantID {
			t.Errorf("data[0].id = %q, want %q", got, wantID)
		}
		if got := mStr(t, first, "object"); got != "model" {
			t.Errorf("data[0].object = %q, want model", got)
		}
		if got := mStr(t, first, "type"); got != "model" {
			t.Errorf("data[0].type = %q, want model", got)
		}
		if got := mStr(t, first, "owned_by"); got != "cmdc" {
			t.Errorf("data[0].owned_by = %q, want cmdc", got)
		}
		if got := mNum(t, first, "created"); got <= 0 {
			t.Errorf("data[0].created = %v, want unix seconds", got)
		}
		if _, err := time.Parse(time.RFC3339, mStr(t, first, "created_at")); err != nil {
			t.Errorf("data[0].created_at not RFC3339: %v", err)
		}
		if mStr(t, first, "display_name") == "" {
			t.Error("data[0].display_name must not be empty")
		}
	}

	// 无 key：硬编码回退列表（models 缓存按 key 隔离，无 key 恒走兜底，不会被别的 key 填充）
	_, fallbackProxy := chatProxy(t, defaultScript())
	resp, err := http.Get(fallbackProxy.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	check(t, decodeJSON(t, readAll(t, resp)), "claude-sonnet-4-6")

	// 带 key：透传上游列表
	_, proxy := chatProxy(t, defaultScript())
	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/v1/models", nil)
	req.Header.Set("x-api-key", "user_test123")
	resp, err = proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	check(t, decodeJSON(t, readAll(t, resp)), "provider/model-x")
}
