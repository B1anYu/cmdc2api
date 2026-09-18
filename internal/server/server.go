// Package server 实现 HTTP 入口：路由、中间件（CORS/日志/panic 恢复）、
// /v1/messages 主 handler、/v1/models 与 /health。
package server

import (
	"log/slog"
	"net/http"
	"strings"
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
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("POST /v1/responses", s.handleResponses)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /{$}", handleHealth)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writePathError(w, r, http.StatusNotFound, "not_found_error", "Not found")
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
					writePathError(sw, r, http.StatusInternalServerError, "api_error", "internal server error")
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

// writePathError 按请求路径选择错误出口形状，供中间件与兜底 404 这类拿不到 handler 上下文
// （或端点根本不存在）的出口使用：OpenAI 端点出 OpenAI 形状，其余（含 /v1/models）保持
// Anthropic 形状。「端点用什么协议，错误就用什么形状」在管线之外也必须成立，
// 否则客户端 SDK 会在这些路径上解析失败。
func writePathError(w http.ResponseWriter, r *http.Request, status int, errType, message string) {
	if isOpenAIErrorPath(r.URL.Path) {
		openaiErrors{}.Write(w, status, errType, message, 0)
		return
	}
	anthropicError(w, status, errType, message, 0)
}

// isOpenAIErrorPath 判断路径是否属于 OpenAI 形状端点。
// 前缀匹配而非等值匹配：带上尾部子路径（如 /v1/chat/completions/xxx）仍按该协议出口。
func isOpenAIErrorPath(path string) bool {
	return strings.HasPrefix(path, "/v1/responses") || strings.HasPrefix(path, "/v1/chat/completions")
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

// mapHTTPError 上游 HTTP 错误响应 → 出站协议错误（形状由 ew 决定）。
func mapHTTPError(w http.ResponseWriter, ew ErrorWriter, ccStatus int, body []byte) {
	status, typ, msg, retry := errs.Map(ccStatus, body)
	ew.Write(w, status, typ, msg, retry)
}
