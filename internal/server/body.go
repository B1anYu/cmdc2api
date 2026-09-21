package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
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

// readBody 读取并校验请求体。超限/坏 JSON 时已按出站协议写好错误响应，返回 false。
// ew 决定错误体形状：入站协议决定错误形状，读体阶段同样不能硬编码 Anthropic 形状，
// 否则 OpenAI 端点会在开流前吐出下游 SDK 解析不了的错误体。
func (s *Server) readBody(w http.ResponseWriter, r *http.Request, ew ErrorWriter) ([]byte, bool) {
	lr := &limitedReader{r: r.Body, remaining: s.cfg.MaxBodyBytes}
	data, err := io.ReadAll(lr)
	if errors.Is(err, errBodyTooLarge) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, drainLimit))
		mb := s.cfg.MaxBodyBytes / (1 << 20)
		ew.Write(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
			fmt.Sprintf("Request body exceeds %dMB limit", mb), 0)
		return nil, false
	}
	if err != nil {
		ew.Write(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON body", 0)
		return nil, false
	}
	if !json.Valid(data) {
		ew.Write(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON body", 0)
		return nil, false
	}
	return data, true
}

// ---------- 鉴权 ----------

// missingAPIKeyMessage 三端点共用的 401 文案。只有「确实没发 key」才会走到这里，
// 因此文案点明期望的 cmdc key 形状（通常为 user_ 前缀）——按前缀过滤 key 会把
// 「发了但格式不对」误报成「没发」，那种情况一律透传上游、由上游报错原路返回。
const missingAPIKeyMessage = "Missing API key. Send a cmdc key (user_ prefix) in Authorization: Bearer <key> or x-api-key header"

// getAPIKey 从 Authorization: Bearer 或 x-api-key 提取客户端 key，原样透传上游。
//
// 不在代码里限制前缀：代理本身不校验 key 的合法性（见 AGENTS.md §三.1），
// 上游未来也可能改用别的 key 形态（如 sk- 开头），此处按前缀过滤只会把
// 「格式不对」误报成「没发 key」。
func getAPIKey(h http.Header) string {
	if auth := h.Get("Authorization"); len(auth) >= 7 && auth[:7] == "Bearer " {
		if tok := strings.TrimSpace(auth[7:]); tok != "" {
			return tok
		}
	}
	return strings.TrimSpace(h.Get("x-api-key"))
}

// writeMissingAPIKey 三端点共用的 401 出口：状态码、错误类型与文案只在上面一处定义，
// 避免三份文案各自漂移（此前 messages/chat/responses 各抄了一份）。
func writeMissingAPIKey(w http.ResponseWriter, ew ErrorWriter) {
	ew.Write(w, http.StatusUnauthorized, "authentication_error", missingAPIKeyMessage, 0)
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
//
// 缓冲的是已编码的字节帧而非类型化事件：Anthropic / Responses / Chat 三种出站协议的帧形状不同，
// 在写出这一层只认字节，协议差异由 EventEncoder 吸收。
type sseWriter struct {
	w       http.ResponseWriter
	started bool
	pending [][]byte
}

func (s *sseWriter) add(frames ...[]byte) {
	if len(frames) == 0 {
		return
	}
	if s.started {
		s.write(frames)
		return
	}
	s.pending = append(s.pending, frames...)
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

func (s *sseWriter) write(frames [][]byte) {
	for _, f := range frames {
		if _, err := s.w.Write(f); err != nil {
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
