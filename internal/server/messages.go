package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/B1anYu/cmdc2api/internal/masq"
	"github.com/B1anYu/cmdc2api/internal/translate"
	"github.com/B1anYu/cmdc2api/internal/types"
)

// scannerMaxLine 单行 NDJSON 上限：tool-call input 可能回显大段生成内容，
// 上限取 64MB（入站体上限 100MB 内）。
const scannerMaxLine = 64 << 20

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	apiKey := getAPIKey(r.Header)
	if apiKey == "" {
		anthropicError(w, http.StatusUnauthorized, "authentication_error",
			"Missing API key. Send in Authorization: Bearer <key> or x-api-key header", 0)
		return
	}
	var areq types.Request
	if err := json.Unmarshal(body, &areq); err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "Invalid request body: "+err.Error(), 0)
		return
	}
	if len(areq.Messages) == 0 {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "messages: at least one message is required", 0)
		return
	}

	// 会话亲和：显式 session 头 > 前缀派生（默认）/按 key 轮换
	prefixKey := translate.PrefixCacheKey(&areq)
	session := s.sessions.ResolveSession(r.Header, apiKey, prefixKey)

	ccReq, warns := translate.BuildCcRequest(&areq, translate.BuildOpts{
		Now:                time.Now(),
		NodeVersion:        s.cfg.FakeNodeVersion,
		WorkingDir:         masq.WorkingDirForSession(session),
		AssistantReasoning: s.cfg.AssistantReasoning,
	})
	for _, wn := range warns {
		slog.Warn("convert", "detail", wn, "model", areq.Model)
	}

	model := areq.Model
	if model == "" {
		model = translate.DefaultModel
	}
	messageID := "msg_" + masq.NewHexID(6)

	// 客户端断连会经由 ctx 取消打断上游读；超时/零输出由本地 cancel 触发
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	s.upstream.EnsureInitialized(apiKey)
	resp, err := s.upstream.Generate(ctx, apiKey, session, r.Header, ccReq)
	if err != nil {
		if r.Context().Err() != nil {
			slog.Info("client disconnected before upstream response", "model", model)
			return
		}
		slog.Error("upstream request failed", "error", err)
		anthropicError(w, http.StatusBadGateway, "api_error", "Upstream error: "+err.Error(), 0)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		slog.Error("cc api error", "status", resp.StatusCode)
		mapHTTPError(w, resp.StatusCode, errBody)
		return
	}

	idle := s.cfg.IdleStream
	if !areq.Stream {
		idle = s.cfg.IdleBuffer
	}
	upstreamBody := newIdleReader(resp.Body, idle)
	defer func() { _ = upstreamBody.Close() }()

	tr := translate.NewStreamTranslator(model, messageID)
	if areq.Stream {
		s.serveStream(w, r, upstreamBody, tr, cancel, model)
	} else {
		s.serveBuffer(w, r, upstreamBody, tr, cancel, model)
	}
}

// serveStream 流式出站：cmdc NDJSON → Anthropic SSE。
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, body io.Reader, tr *translate.StreamTranslator, cancel context.CancelFunc, model string) {
	sw := &sseWriter{w: w}
	sw.add(tr.Start()...)

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), scannerMaxLine)
	for sc.Scan() {
		if evs := tr.Feed(sc.Bytes()); len(evs) > 0 {
			sw.add(evs...)
		}
		if tr.ContentStarted() {
			sw.start()
		}
	}

	if err := sc.Err(); err != nil {
		switch {
		case r.Context().Err() != nil:
			// 客户端已断连，ctx 取消打断了上游读，无需也无法再写响应
			slog.Info("client disconnected mid-stream", "model", model, "lastEvent", tr.LastEvent())
		case errors.Is(err, ErrIdleTimeout):
			cancel()
			hint := s.timeoutHint()
			slog.Warn("stream idle timeout", "model", model, "lastEvent", tr.LastEvent())
			if !sw.started {
				anthropicError(w, http.StatusTooManyRequests, "rate_limit_error", hint, 5)
			} else {
				sw.add(types.NewErrorEvent("rate_limit_error", hint))
			}
		default:
			cancel()
			slog.Error("stream read error", "error", err)
			if !sw.started {
				anthropicError(w, http.StatusBadGateway, "api_error", "Upstream error: "+err.Error(), 0)
			} else {
				sw.add(types.NewErrorEvent("api_error", "Upstream error: "+err.Error()))
			}
		}
		return
	}

	sw.add(tr.Finish()...)

	if tr.HasUpstreamError() {
		status, typ, msg, retry := mapStreamEventError(tr.ErrMessage())
		slog.Warn("upstream stream error", "status", status, "message", msg)
		if !sw.started {
			anthropicError(w, status, typ, msg, retry)
			return
		}
		return // started：错误事件已在流内发出
	}
	if tr.OutputTokens() == 0 {
		// 零输出按错误处理，避免下游异常计费；已开流时错误事件在 Finish 里
		cancel()
		if !sw.started {
			anthropicError(w, http.StatusTooManyRequests, "rate_limit_error",
				"Empty response from upstream (zero output tokens)", 10)
		}
		return
	}
	s.resetTimeouts()
	sw.start() // 兜底：仅 usage 无内容等极端情形下也要完成 SSE 生命周期
}

// serveBuffer 非流式出站：聚合同一事件序列为完整 Message。
func (s *Server) serveBuffer(w http.ResponseWriter, r *http.Request, body io.Reader, tr *translate.StreamTranslator, cancel context.CancelFunc, model string) {
	agg := translate.NewAggregator()
	for _, e := range tr.Start() {
		agg.Feed(e)
	}

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), scannerMaxLine)
	for sc.Scan() {
		for _, e := range tr.Feed(sc.Bytes()) {
			agg.Feed(e)
		}
	}

	if err := sc.Err(); err != nil {
		switch {
		case r.Context().Err() != nil:
			slog.Info("client disconnected while buffering", "model", model, "lastEvent", tr.LastEvent())
		case errors.Is(err, ErrIdleTimeout):
			cancel()
			hint := s.timeoutHint()
			slog.Warn("buffer idle timeout", "model", model, "lastEvent", tr.LastEvent())
			anthropicError(w, http.StatusTooManyRequests, "rate_limit_error", hint, 5)
		default:
			cancel()
			slog.Error("buffer read error", "error", err)
			anthropicError(w, http.StatusBadGateway, "api_error", "Upstream error: "+err.Error(), 0)
		}
		return
	}

	for _, e := range tr.Finish() {
		agg.Feed(e)
	}

	if tr.HasUpstreamError() {
		status, typ, msg, retry := mapStreamEventError(tr.ErrMessage())
		slog.Warn("upstream stream error (non-stream)", "status", status, "message", msg)
		anthropicError(w, status, typ, msg, retry)
		return
	}
	if tr.OutputTokens() == 0 {
		cancel()
		anthropicError(w, http.StatusTooManyRequests, "rate_limit_error",
			"Empty response from upstream (zero output tokens)", 10)
		return
	}
	s.resetTimeouts()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = writeJSON(w, agg.Message())
}
