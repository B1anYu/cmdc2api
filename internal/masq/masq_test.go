package masq

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
		t.Errorf("slug must not contain 'undefined' (NaN bug from原版已修复): %q", a)
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
	// 过短的值不算（对齐原版 ≥8 字符规则）
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
