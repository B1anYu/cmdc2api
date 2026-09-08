package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/B1anYu/cmdc2api/internal/types"
)

// ---------- 请求体读取（413 排空模式） ----------

var errBodyTooLarge = errors.New("body too large")

// drainLimit 413 请求体超限拒绝后的排空上限：继续读取并丢弃剩余部分以复用 keep-alive 连接，
// 确保客户端能正常接收 413 响应而非连接被直接重置；若超额上传超过该上限则强制关闭。
const drainLimit = 32 << 20

type limitedReader struct {
	r         io.Reader
	remaining int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	if l.remaining < 0 {
		return n, errBodyTooLarge
	}
	return n, err
}

// readBody 读取并校验请求体。超限/坏 JSON 时已写好错误响应，返回 false。
func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	lr := &limitedReader{r: r.Body, remaining: s.cfg.MaxBodyBytes}
	data, err := io.ReadAll(lr)
	if errors.Is(err, errBodyTooLarge) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, drainLimit))
		mb := s.cfg.MaxBodyBytes / (1 << 20)
		anthropicError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
			fmt.Sprintf("Request body exceeds %dMB limit", mb), 0)
		return nil, false
	}
	if err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON body", 0)
		return nil, false
	}
	if !json.Valid(data) {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON body", 0)
		return nil, false
	}
	return data, true
}

// ---------- 鉴权 ----------

var userKeyRe = regexp.MustCompile(`user_[a-zA-Z0-9_-]+`)

// getAPIKey 从 Authorization: Bearer 或 x-api-key 提取 user_ 前缀的 cmdc key，
// 原样透传上游。
func getAPIKey(h http.Header) string {
	if auth := h.Get("Authorization"); len(auth) >= 7 && auth[:7] == "Bearer " {
		if m := userKeyRe.FindString(auth[7:]); m != "" {
			return m
		}
	}
	if xk := h.Get("x-api-key"); xk != "" {
		if m := userKeyRe.FindString(xk); m != "" {
			return m
		}
	}
	return ""
}

// ---------- 上游空闲超时 ----------

// idleReader 包装上游响应体：超时未收到新数据时主动断开连接，唤醒阻塞的 Read 并返回 ErrIdleTimeout（流式默认 30s / 非流式默认 90s）。
var ErrIdleTimeout = errors.New("upstream idle timeout")

type idleReader struct {
	rc       io.ReadCloser
	idle     time.Duration
	mu       sync.Mutex
	timer    *time.Timer
	timedOut bool
}

func newIdleReader(rc io.ReadCloser, idle time.Duration) *idleReader {
	ir := &idleReader{rc: rc, idle: idle}
	ir.timer = time.AfterFunc(idle, func() {
		ir.mu.Lock()
		defer ir.mu.Unlock()
		ir.timedOut = true
		_ = ir.rc.Close()
	})
	return ir
}

func (ir *idleReader) Read(p []byte) (int, error) {
	ir.mu.Lock()
	if ir.timedOut {
		ir.mu.Unlock()
		return 0, ErrIdleTimeout
	}
	ir.timer.Reset(ir.idle)
	ir.mu.Unlock()
	n, err := ir.rc.Read(p)
	if err != nil {
		ir.mu.Lock()
		if ir.timedOut {
			err = ErrIdleTimeout
		}
		ir.mu.Unlock()
	}
	return n, err
}

func (ir *idleReader) Close() error {
	ir.mu.Lock()
	ir.timer.Stop()
	ir.mu.Unlock()
	return ir.rc.Close()
}

// ---------- SSE 写出（延迟 200 头） ----------

// sseWriter 延迟发送 200 状态码与 SSE 响应头：在未确认产出实质内容（text/thinking/tool_use）前先缓冲事件。
// 若首帧前发生超时或错误，可直接回退为标准 HTTP JSON 错误响应，便于客户端 SDK 进行退避重试。
type sseWriter struct {
	w       http.ResponseWriter
	started bool
	pending []types.StreamEvent
}

func (s *sseWriter) add(evs ...types.StreamEvent) {
	if s.started {
		s.write(evs)
		return
	}
	s.pending = append(s.pending, evs...)
}

// start 写 200 头并刷出缓冲。若从未有内容（错误/零输出），调用方
// 改走 JSON 错误路径，缓冲直接丢弃。
func (s *sseWriter) start() {
	if s.started {
		return
	}
	s.started = true
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.write(s.pending)
	s.pending = nil
}

func (s *sseWriter) write(evs []types.StreamEvent) {
	for _, e := range evs {
		if _, err := s.w.Write(e.Encode()); err != nil {
			return
		}
	}
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}

// ---------- 小工具 ----------

func itoa(n int) string          { return strconv.Itoa(n) }
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	return enc.Encode(v)
}
