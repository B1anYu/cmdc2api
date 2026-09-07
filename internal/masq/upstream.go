package masq

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/B1anYu/cmdc2api/internal/types"
)

// Upstream 封装对 cmdc 的全部出站请求。请求头集合、预请求节奏、slug/traceparent
// 生成规则均对齐 proxy.mjs 的可观察行为。
type Upstream struct {
	base      string
	zdr       bool
	state     *StateStore
	versions  *CCVersion
	client    *http.Client // generate：无总超时，靠 body 读超时控制
	fast      *http.Client // 预请求/模型列表：10s 总超时
	modelsTTL time.Duration

	initMu   sync.Mutex
	inflight map[string]bool // 每 key 预请求去重

	modelsMu    sync.Mutex
	modelsCache []Model
	modelsAt    time.Time
}

// Model 上游模型条目。
type Model struct {
	ID   string
	Name string
}

func NewUpstream(base string, zdr bool, modelsTTL time.Duration, state *StateStore, ver *CCVersion) *Upstream {
	// 真实 CLI 是 Node 程序，undici 默认走 HTTP/1.1；Go 默认协商 h2，
	// 强制 http/1.1 对齐传输层指纹。User-Agent 置空同理（undici 不发 UA）。
	transport := &http.Transport{
		ForceAttemptHTTP2:     false,
		TLSClientConfig:      &tls.Config{NextProtos: []string{"http/1.1"}},
		TLSHandshakeTimeout:  15 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		MaxIdleConnsPerHost:  4,
	}
	return &Upstream{
		base:      strings.TrimRight(base, "/"),
		zdr:       zdr,
		state:     state,
		versions:  ver,
		client:    &http.Client{Transport: transport},
		fast:      &http.Client{Timeout: 10 * time.Second, Transport: transport.Clone()},
		modelsTTL: modelsTTL,
		inflight:  make(map[string]bool),
	}
}

func (u *Upstream) baseHeaders(apiKey string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "*/*")
	h.Set("User-Agent", "")
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("x-cli-environment", "production")
	h.Set("x-command-code-version", u.versions.Get())
	return h
}

// EnsureInitialized 首次见到某 key（或距上次超过 8h+2h 抖动）时并行发出
// fingerprint/record 与 lifecycle-events 两个预请求。失败不阻断主请求，
// 下次请求重试。与原版一致：无论成败都推进节流时间。
func (u *Upstream) EnsureInitialized(apiKey string) {
	st := u.state.KeyState(apiKey)
	if time.Now().Before(st.NextInitAt) {
		return
	}
	u.initMu.Lock()
	if u.inflight[apiKey] {
		u.initMu.Unlock()
		return
	}
	u.inflight[apiKey] = true
	u.initMu.Unlock()
	defer func() {
		u.initMu.Lock()
		delete(u.inflight, apiKey)
		u.initMu.Unlock()
	}()

	fpBody, err := json.Marshal(st.Fingerprint)
	if err != nil {
		return
	}
	lcBody, _ := json.Marshal(map[string]any{
		"eventType": "cli_session_exists",
		"metadata": map[string]any{
			// 原版的 lifecycle sessionId 是每次随机的临时 ID，与 x-session-id 无关
			"sessionId":  "sess_" + NewHexID(8),
			"cliVersion": u.versions.Get(),
			"mode":       "interactive",
			"os":         "win32-x64",
		},
	})

	var wg sync.WaitGroup
	for _, job := range []struct {
		path string
		body []byte
		what string
	}{
		{"/alpha/fingerprint/record", fpBody, "fingerprint"},
		{"/alpha/lifecycle-events", lcBody, "lifecycle"},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, u.base+job.path, bytes.NewReader(job.body))
			if err != nil {
				return
			}
			req.Header = u.baseHeaders(apiKey)
			resp, err := u.fast.Do(req)
			if err != nil {
				slog.Warn("init request error", "what", job.what, "error", err)
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 16<<10))
			if resp.StatusCode >= 400 {
				slog.Warn("init request failed", "what", job.what, "status", resp.StatusCode)
			} else {
				slog.Info("init request ok", "what", job.what)
			}
		}()
	}
	wg.Wait()

	jitter := time.Duration(rand.Int64N(int64(2 * time.Hour)))
	u.state.ScheduleNextInit(apiKey, time.Now().Add(8*time.Hour+jitter))
}

// Generate 转发主推理请求到 /alpha/generate。
func (u *Upstream) Generate(ctx context.Context, apiKey, session string, inbound http.Header, body *types.CcRequest) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal cc request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.base+"/alpha/generate", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	h := u.baseHeaders(apiKey)
	h.Set("x-session-id", session)
	h.Set("x-co-flag", "false")
	h.Set("x-taste-learning", "false")
	h.Set("x-project-slug", FakeProjectSlug(session))
	h.Set("traceparent", NewTraceparent())
	if u.zdr || inbound.Get("x-cmd-zdr") == "1" {
		h.Set("x-cmd-zdr", "1")
	}
	req.Header = h
	return u.client.Do(req)
}

// Models 拉取上游模型列表，带 TTL 缓存；失败回退硬编码列表（原版行为）。
func (u *Upstream) Models(apiKey string) []Model {
	u.modelsMu.Lock()
	if u.modelsCache != nil && time.Since(u.modelsAt) < u.modelsTTL {
		c := u.modelsCache
		u.modelsMu.Unlock()
		return c
	}
	u.modelsMu.Unlock()

	if apiKey == "" {
		return FallbackModels
	}
	req, err := http.NewRequest(http.MethodGet, u.base+"/provider/v1/models", nil)
	if err != nil {
		return FallbackModels
	}
	req.Header = u.baseHeaders(apiKey)
	resp, err := u.fast.Do(req)
	if err != nil {
		slog.Warn("models fetch error, using fallback", "error", err)
		return FallbackModels
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("models fetch non-200, using fallback", "status", resp.StatusCode)
		return FallbackModels
	}
	var data struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil || len(data.Data) == 0 {
		return FallbackModels
	}
	models := make([]Model, 0, len(data.Data))
	for _, m := range data.Data {
		models = append(models, Model{ID: m.ID, Name: m.ID})
	}
	u.modelsMu.Lock()
	u.modelsCache = models
	u.modelsAt = time.Now()
	u.modelsMu.Unlock()
	slog.Info("fetched models from provider", "count", len(models))
	return models
}

// ---------- slug / traceparent / workingDir ----------

var slugNames = []string{
	"app", "api", "backend", "bot", "cli", "core", "data", "frontend",
	"lib", "plugin", "proxy", "server", "service", "tool", "web", "worker",
}

// slugParts 按 sessionId 派生 (项目名, 后缀)。sessionId 前 4 字符按 16 进制
// 解析（JS parseInt 语义：取前导合法十六进制位）；解析不出（如自定义
// prompt-cache-key 风格的串）退化为确定性字符哈希。
func slugParts(sessionID string) (string, string) {
	head := sessionID
	if len(head) > 4 {
		head = head[:4]
	}
	var idx int64
	digits := 0
	for _, c := range head {
		var d int64
		switch {
		case c >= '0' && c <= '9':
			d = int64(c - '0')
		case c >= 'a' && c <= 'f':
			d = int64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int64(c-'A') + 10
		default:
			goto parsed
		}
		idx = idx*16 + d
		digits++
	}
parsed:
	if digits == 0 {
		var h uint32
		for i := 0; i < len(sessionID); i++ {
			h = h*31 + uint32(sessionID[i])
		}
		idx = int64(h)
	}
	suffix := head
	if suffix == "" {
		suffix = "0000"
	}
	return slugNames[idx%int64(len(slugNames))], suffix
}

func fakeProjectPath(sessionID string) string {
	name, suffix := slugParts(sessionID)
	return `C:\Users\dev\projects\` + name + `-` + suffix
}

// FakeProjectSlug 生成 x-project-slug（对齐真实 CLI 的 slug 格式）。
func FakeProjectSlug(sessionID string) string {
	p := strings.ToLower(fakeProjectPath(sessionID))
	if len(p) >= 2 && p[1] == ':' {
		p = p[2:]
	}
	var b strings.Builder
	prevDash := false
	for _, c := range p {
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if ok {
			b.WriteRune(c)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// WorkingDirForSession 生成与 slug 自洽的伪装工作目录（win32 风格路径，
// 与指纹的 platform/arch 一致；原版发 Linux 的 process.cwd() 自相矛盾）。
func WorkingDirForSession(sessionID string) string {
	return fakeProjectPath(sessionID)
}

// NewTraceparent 生成 OTel W3C traceparent：00-<32hex>-<16hex>-01。
func NewTraceparent() string {
	return "00-" + NewHexID(16) + "-" + NewHexID(8) + "-01"
}
