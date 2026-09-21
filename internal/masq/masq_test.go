package masq

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFakeProjectSlug_HexHead(t *testing.T) {
	// "a3f2" = 0xa3f2 = 41970，41970 % 16 = 2 → names[2] = "backend"
	got := FakeProjectSlug("a3f2bbbb-cccc-dddd-eeee-ffffffffffff")
	if want := "users-dev-projects-backend-a3f2"; got != want {
		t.Errorf("slug = %q, want %q", got, want)
	}
}

func TestFakeProjectSlug_NonHexFallbackDeterministic(t *testing.T) {
	a := FakeProjectSlug("my-stable-cache-key-001")
	b := FakeProjectSlug("my-stable-cache-key-001")
	if a != b {
		t.Error("slug must be deterministic for non-hex session ids")
	}
	if !strings.HasPrefix(a, "users-dev-projects-") {
		t.Errorf("slug prefix wrong: %q", a)
	}
	if strings.Contains(a, "undefined") {
		t.Errorf("slug must not contain 'undefined': %q", a)
	}
}

func TestWorkingDirForSession_ConsistentWithSlug(t *testing.T) {
	session := "a3f2bbbb-cccc-dddd-eeee-ffffffffffff"
	wd := WorkingDirForSession(session)
	if !strings.HasPrefix(wd, `C:\Users\dev\projects\`) {
		t.Errorf("workingDir = %q", wd)
	}
	// slug 由同一 (name, suffix) 派生，路径分隔符归一化为 '-'
	if want := `C:\Users\dev\projects\backend-a3f2`; wd != want {
		t.Errorf("workingDir = %q, want %q", wd, want)
	}
	if slug := FakeProjectSlug(session); slug != "users-dev-projects-backend-a3f2" {
		t.Errorf("slug = %q", slug)
	}
}

func TestNewTraceparent_Format(t *testing.T) {
	re := regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)
	for i := 0; i < 10; i++ {
		if tp := NewTraceparent(); !re.MatchString(tp) {
			t.Fatalf("traceparent = %q", tp)
		}
	}
}

func TestSessionStore_HeaderPriority(t *testing.T) {
	s := NewSessionStore("prefix")
	h := http.Header{}
	h.Set("X-Session-Id", "my-custom-session-1")
	if got := s.ResolveSession(h, "user_k", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"); got != "my-custom-session-1" {
		t.Errorf("explicit session header must win, got %q", got)
	}
	h2 := http.Header{}
	h2.Set("X-Claude-Code-Session-Id", "cc-session-12345")
	if got := s.ResolveSession(h2, "user_k", ""); got != "cc-session-12345" {
		t.Errorf("claude-code session header must win, got %q", got)
	}
	// 长度小于 8 字符的 session header 视为无效，降级处理
	h3 := http.Header{}
	h3.Set("X-Session-Id", "short")
	prefix := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if got := s.ResolveSession(h3, "user_k", prefix); got != prefix {
		t.Errorf("short header should fall through to prefix key, got %q", got)
	}
}

func TestSessionStore_PrefixStrategyStable(t *testing.T) {
	s := NewSessionStore("prefix")
	h := http.Header{}
	pk := "11111111-2222-3333-4444-555555555555"
	a := s.ResolveSession(h, "user_k", pk)
	b := s.ResolveSession(h, "user_k", pk)
	if a != b || a != pk {
		t.Errorf("prefix strategy must be stateless-stable: %q vs %q", a, b)
	}
}

func TestSessionStore_KeyStrategyShape(t *testing.T) {
	s := NewSessionStore("key")
	h := http.Header{}
	a := s.ResolveSession(h, "user_k", "")
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	if !re.MatchString(a) {
		t.Errorf("key strategy session not UUID: %q", a)
	}
	if b := s.ResolveSession(h, "user_k", ""); b != a {
		t.Errorf("key strategy should reuse live session: %q vs %q", a, b)
	}
}

func TestGenerateFingerprint_Shape(t *testing.T) {
	fp := GenerateFingerprint()
	c := fp.Components
	if c.Platform != "win32" || c.Arch != "x64" || c.OSRelease != "10.0.22631" {
		t.Errorf("platform shape wrong: %+v", c)
	}
	if len(c.MACHashes) < 2 || len(c.MACHashes) > 5 {
		t.Errorf("mac count = %d, want 2..5", len(c.MACHashes))
	}
	if len(fp.Thumbmark) != 64 {
		t.Errorf("thumbmark not sha256 hex: %q", fp.Thumbmark)
	}
	if c.CPUCount <= 0 || c.MemGiB <= 0 || c.Timezone == "" {
		t.Errorf("components incomplete: %+v", c)
	}
}

func TestStateStore_PersistAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s1 := LoadState(path)
	st := s1.KeyState("user_abc123")
	if st.Fingerprint == nil {
		t.Fatal("fingerprint not generated")
	}
	tm1 := st.Fingerprint.Thumbmark

	s2 := LoadState(path)
	st2 := s2.KeyState("user_abc123")
	if st2.Fingerprint == nil || st2.Fingerprint.Thumbmark != tm1 {
		t.Errorf("fingerprint must survive reload: %v vs %v", st2.Fingerprint.Thumbmark, tm1)
	}
	// 明文 key 绝不落盘
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file should exist: %v", err)
	}
	if strings.Contains(string(raw), "user_abc123") {
		t.Error("plaintext api key must never be persisted")
	}
}

// ---------- Upstream：/provider/v1/models 缓存按 key 隔离（F28） ----------

// fakeModelsUpstream 按 Authorization 里的 key 返回不同列表，并记录每个 key 的调用次数。
type fakeModelsUpstream struct {
	mu    sync.Mutex
	calls map[string]int
	srv   *httptest.Server
}

func newFakeModelsUpstream(t *testing.T) *fakeModelsUpstream {
	t.Helper()
	f := &fakeModelsUpstream{calls: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/provider/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		f.calls[key]++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data":[{"id":"model-for-%s"}]}`, key)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeModelsUpstream) callsFor(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func newTestUpstream(t *testing.T, base string, ttl time.Duration) *Upstream {
	t.Helper()
	return NewUpstream(base, false, ttl, LoadState(filepath.Join(t.TempDir(), "state.json")), NewCCVersion())
}

// TestUpstream_ModelsCacheIsPerKey models 缓存曾是 Upstream 单例上的一个共享字段：
// 被某个 key 填充后，无 key 请求与其它 key 在 TTL 内都会命中它（串号）。
// 缓存改为按 key 分开存后，每个 key 只拿自己的列表，无 key 恒为兜底列表。
func TestUpstream_ModelsCacheIsPerKey(t *testing.T) {
	f := newFakeModelsUpstream(t)
	u := newTestUpstream(t, f.srv.URL, time.Minute)

	a := u.Models("user_key_a")
	if len(a) != 1 || a[0].ID != "model-for-user_key_a" {
		t.Fatalf("key A list = %v", a)
	}
	if b := u.Models("user_key_b"); len(b) != 1 || b[0].ID != "model-for-user_key_b" {
		t.Fatalf("key B must not reuse key A's cache, got %v", b)
	}
	if got := u.Models(""); !reflect.DeepEqual(got, FallbackModels) {
		t.Fatalf("no-key must return the fallback list, got %v", got)
	}
	// 同一 key 在 TTL 内仍命中缓存（行为不变）
	if got := u.Models("user_key_a"); len(got) != 1 || got[0].ID != "model-for-user_key_a" {
		t.Fatalf("key A second call = %v", got)
	}
	for _, key := range []string{"user_key_a", "user_key_b"} {
		if n := f.callsFor(key); n != 1 {
			t.Errorf("%s upstream calls = %d, want 1 (TTL 内应命中缓存)", key, n)
		}
	}
	if n := f.callsFor(""); n != 0 {
		t.Errorf("no-key request must not reach upstream, calls = %d", n)
	}
}

// TestUpstream_ModelsCacheExpiresPerKey TTL 过期后同一 key 重新拉取（TTL 语义不变）。
func TestUpstream_ModelsCacheExpiresPerKey(t *testing.T) {
	f := newFakeModelsUpstream(t)
	u := newTestUpstream(t, f.srv.URL, 20*time.Millisecond)

	u.Models("user_key_a")
	time.Sleep(60 * time.Millisecond)
	if got := u.Models("user_key_a"); len(got) != 1 || got[0].ID != "model-for-user_key_a" {
		t.Fatalf("after TTL = %v", got)
	}
	if n := f.callsFor("user_key_a"); n != 2 {
		t.Errorf("upstream calls = %d, want 2 (TTL 过期后应重拉)", n)
	}
}

// TestUpstream_ModelsCacheConcurrent 并发读写下按 key 隔离且无数据竞争（-race 下有效）。
func TestUpstream_ModelsCacheConcurrent(t *testing.T) {
	f := newFakeModelsUpstream(t)
	u := newTestUpstream(t, f.srv.URL, time.Minute)

	keys := []string{"user_key_a", "user_key_b", "", "user_key_c"}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			got := u.Models(key)
			if key == "" {
				if !reflect.DeepEqual(got, FallbackModels) {
					t.Errorf("no-key must return the fallback list, got %v", got)
				}
				return
			}
			if len(got) != 1 || got[0].ID != "model-for-"+key {
				t.Errorf("key %q list = %v", key, got)
			}
		}(keys[i%len(keys)])
	}
	wg.Wait()
}
