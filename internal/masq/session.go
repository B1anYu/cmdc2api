package masq

import (
	crand "crypto/rand"
	"encoding/hex"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"sync"
	"time"
)

// 会话策略（对齐 proxy.mjs sessionStore/ensureSession/getSessionId + PR#10）：
// 1. 客户端显式传入的 session 头优先（x-session-id / x-claude-code-session-id /
//    session_id，≥8 字符），这是缓存亲和的显式通道；
// 2. prefix 策略（默认）：从请求可缓存前缀派生稳定会话 ID —— 同一对话跨轮次
//    复用同一会话，命中上游按会话粒度的 prompt cache（无状态，无轮换）；
// 3. key 策略（原版行为）：按上游 key 隔离，12h + 1h 抖动轮换。
const (
	sessionDuration = 12 * time.Hour
	sessionJitter   = time.Hour
)

type sessionEntry struct {
	ID        string
	ExpiresAt time.Time
}

type SessionStore struct {
	strategy string
	mu       sync.Mutex
	byKey    map[string]*sessionEntry
}

func NewSessionStore(strategy string) *SessionStore {
	return &SessionStore{strategy: strategy, byKey: make(map[string]*sessionEntry)}
}

// ResolveSession 决定本次请求使用的 x-session-id。
func (s *SessionStore) ResolveSession(h http.Header, apiKey, prefixKey string) string {
	for _, name := range []string{"x-session-id", "x-claude-code-session-id", "session_id"} {
		if v := h.Get(name); len(v) >= 8 {
			return v
		}
	}
	if s.strategy == "prefix" && prefixKey != "" {
		return prefixKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if e, ok := s.byKey[apiKey]; ok && now.Before(e.ExpiresAt) {
		return e.ID
	}
	id := newUUID()
	s.byKey[apiKey] = &sessionEntry{
		ID:        id,
		ExpiresAt: now.Add(sessionDuration + time.Duration(mrand.Int64N(int64(sessionJitter)))),
	}
	slog.Info("session created", "sessionIdPrefix", id[:8])
	return id
}

// Cleanup 周期性清理过期会话（仅 key 策略使用 map）。
func (s *SessionStore) Cleanup() {
	if s.strategy != "key" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, e := range s.byKey {
		if now.After(e.ExpiresAt) {
			delete(s.byKey, k)
		}
	}
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// NewHexID 返回 n 字节的随机十六进制串（lifecycle sessionId、traceparent、
// msg id 生成用）。
func NewHexID(n int) string {
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
