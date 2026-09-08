// Package server 实现 HTTP 入口：路由、中间件（CORS/日志/panic 恢复）、
// /v1/messages 主 handler、/v1/models 与 /health。
package server

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/B1anYu/cmdc2api/internal/config"
	"github.com/B1anYu/cmdc2api/internal/errs"
	"github.com/B1anYu/cmdc2api/internal/masq"
)

// timeoutReduceContextThreshold 连续超时达到该阈值后，返回包含缩减上下文提示的错误消息（通常为会话上下文过长引发）。
const timeoutReduceContextThreshold = 3

type Server struct {
	cfg      config.Config
	upstream *masq.Upstream
	sessions *masq.SessionStore

	tmu    sync.Mutex
	consec int // 连续超时计数（全局，任意成功请求重置）
}

func New(cfg config.Config, state *masq.StateStore, ver *masq.CCVersion) *Server {
	return &Server{
		cfg:      cfg,
		upstream: masq.NewUpstream(cfg.APIBase, cfg.ZDR, cfg.ModelRefresh, state, ver),
		sessions: masq.NewSessionStore(cfg.SessionStrategy),
	}
}

func (s *Server) CleanupSessions() { s.sessions.Cleanup() }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", s.handleMessages)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /{$}", handleHealth)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		anthropicError(w, http.StatusNotFound, "not_found_error", "Not found", 0)
	})
	return s.middleware(mux)
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// middleware：跨域支持（CORS 全开）、panic 恢复与结构化请求日志。
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered", "path", r.URL.Path, "panic", rec)
				if !sw.wrote {
					anthropicError(sw, http.StatusInternalServerError, "api_error", "internal server error", 0)
				}
			}
			slog.Info("request",
				"method", r.Method, "path", r.URL.Path,
				"status", sw.status, "duration", time.Since(start).Round(time.Millisecond))
		}()
		next.ServeHTTP(sw, r)
	})
}

// statusWriter 捕获状态码并透传 Flush（SSE 必需）。
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// timeoutHint 记录一次超时并返回给客户端的提示消息。
func (s *Server) timeoutHint() string {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	s.consec++
	if s.consec >= timeoutReduceContextThreshold {
		return "Response timeout - try reducing context length (summarize earlier messages)"
	}
	return "Response timeout - request timed out"
}

func (s *Server) resetTimeouts() {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	s.consec = 0
}

// anthropicError 统一错误出口：Anthropic 形状 + Retry-After 头。
func anthropicError(w http.ResponseWriter, status int, errType, message string, retryAfter int) {
	body := map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": message},
	}
	if retryAfter > 0 {
		body["retry_after"] = retryAfter
	}
	w.Header().Set("Content-Type", "application/json")
	if retryAfter > 0 {
		w.Header().Set("Retry-After", itoa(retryAfter))
	}
	w.WriteHeader(status)
	_ = writeJSON(w, body)
}

// mapHTTPError 上游 HTTP 错误响应 → Anthropic 错误。
func mapHTTPError(w http.ResponseWriter, ccStatus int, body []byte) {
	status, typ, msg, retry := errs.Map(ccStatus, body)
	anthropicError(w, status, typ, msg, retry)
}
