package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/B1anYu/cmdc2api/internal/masq"
	"github.com/B1anYu/cmdc2api/internal/translate"
	"github.com/B1anYu/cmdc2api/internal/types"
)

// scannerMaxLine 单行 NDJSON 上限：tool-call input 可能回显大段生成内容，
// 上限取 64MB（入站体上限 100MB 内）。
const scannerMaxLine = 64 << 20

// ---------- 出站协议适配接口 ----------
//
// 上游侧只有一条链路（types.Request → BuildCcRequest → cmdc → StreamTranslator），
// 协议差异全部收敛到下面三个接口：入站协议先把请求归一化为规范格式，
// 出站协议只需换一组 Encoder/Aggregator/Errors 实现，管线本身不再分支。

// EventEncoder 把规范格式的流式事件增量编码为出站协议的 SSE 字节帧（0..N 帧）。
// Finish 幂等，产出收尾帧（终态事件、usage 等）。
//
// 帧必须自带完整的 "event: ...\ndata: ...\n\n" 形状：管线只做缓冲与写出，
// 不感知也不解析帧内容。返回 [][]byte 而非类型化事件，正是为了容纳
// 一条上游事件展开成多帧（Responses 的 item 生命周期）或零帧（Chat 的忽略事件）的协议。
type EventEncoder interface {
	Feed(ev types.StreamEvent) [][]byte
	Finish() [][]byte
}

// EventAggregator 把同一事件序列折叠为非流式响应对象。
// 与 EventEncoder 消费同一批事件，保证同一协议的流式/非流式语义不分裂。
// 有状态，每个请求都要新建。
type EventAggregator interface {
	Feed(ev types.StreamEvent)
	// Result 返回可直接 JSON 序列化的最终对象
	Result() any
}

// ErrorWriter 按出站协议写错误响应。retryAfter > 0 时附 Retry-After 头，
// 供客户端 SDK 退避重试。
type ErrorWriter interface {
	Write(w http.ResponseWriter, status int, errType, message string, retryAfter int)
}

// ---------- Anthropic 出站适配 ----------

// anthropicEncoder 每个 StreamEvent 编码为一帧（既有行为，逐字节不变）。
type anthropicEncoder struct{}

func (anthropicEncoder) Feed(ev types.StreamEvent) [][]byte { return [][]byte{ev.Encode()} }

// Finish Anthropic 流的收尾帧已由 StreamTranslator.Finish 给出，无需补帧。
func (anthropicEncoder) Finish() [][]byte { return nil }

// anthropicAggregator 复用既有 Aggregator，只做接口适配。
type anthropicAggregator struct{ inner *translate.Aggregator }

func newAnthropicAggregator() *anthropicAggregator {
	return &anthropicAggregator{inner: translate.NewAggregator()}
}

func (a *anthropicAggregator) Feed(ev types.StreamEvent) { a.inner.Feed(ev) }
func (a *anthropicAggregator) Result() any               { return a.inner.Message() }

// ---------- 错误出口 ----------

// anthropicErrors Anthropic 形状错误出口（现状）。
type anthropicErrors struct{}

func (anthropicErrors) Write(w http.ResponseWriter, status int, errType, message string, retryAfter int) {
	anthropicError(w, status, errType, message, retryAfter)
}

// openaiErrors OpenAI 形状错误出口，/v1/responses 与 /v1/chat/completions 共用。
// 状态码原样保留（只换形状不换码位），但错误类型走 openaiErrorShape 的映射表。
type openaiErrors struct{}

func (openaiErrors) Write(w http.ResponseWriter, status int, errType, message string, retryAfter int) {
	typ, code := openaiErrorShape(errType)
	w.Header().Set("Content-Type", "application/json")
	if retryAfter > 0 {
		w.Header().Set("Retry-After", itoa(retryAfter))
	}
	w.WriteHeader(status)
	// param 恒为 null：OpenAI SDK 按字段存在性解析，缺字段比 null 更容易触发解码告警。
	// retry_after 不进 JSON 体（OpenAI 形状无此字段），只走 Retry-After 头。
	_ = writeJSON(w, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    typ,
			"param":   nil,
			"code":    code,
		},
	})
}

// openaiErrorShape Anthropic 错误类型 → OpenAI (type, code)。
// type 决定客户端的重试分流，code 供程序化判定（如 invalid_api_key 触发换 key），
// 两者都要给，不能只给其一。
func openaiErrorShape(errType string) (typ, code string) {
	switch errType {
	case "authentication_error":
		return "invalid_request_error", "invalid_api_key"
	case "invalid_request_error":
		return "invalid_request_error", "invalid_request_error"
	case "rate_limit_error":
		return "rate_limit_error", "rate_limit_exceeded"
	case "not_found_error":
		return "not_found_error", "not_found_error"
	default:
		// api_error / overloaded_error 等：OpenAI 形状没有对应类型，统归 server_error
		return "server_error", "server_error"
	}
}

// ---------- 共享上游管线 ----------

// PipelineRequest 一次「归一化完成后」的上游转发请求。
// 入站协议差异已在调用前抹平：Req 恒为规范格式（Anthropic 形状），
// 因此会话亲和、缓存断点、伪装、usage 换算全部免费继承。
type PipelineRequest struct {
	Req    *types.Request // 规范格式请求
	APIKey string         // 客户端 cmdc key，原样透传上游
	Header http.Header    // 客户端原始请求头：会话亲和解析与伪装透传共用
	// Stream 出站形态（客户端是否要求流式）：同时决定空闲超时档位与出站分支
	Stream     bool
	Encoder    EventEncoder    // Stream=true 时使用；无状态可复用
	Aggregator EventAggregator // Stream=false 时使用；有状态，每请求新建
	Errors     ErrorWriter     // 错误出口形状
}

// runPipeline 归一化完成后的共享上游管线：
// 会话亲和 → 信封构造 → 上游请求 → NDJSON 扫描 → 出站编码/聚合。
func (s *Server) runPipeline(w http.ResponseWriter, r *http.Request, pr *PipelineRequest) {
	areq := pr.Req

	// 会话亲和解析：显式 session 请求头 > 前缀哈希派生（默认） > 按 Key 轮换
	prefixKey := translate.PrefixCacheKey(areq)
	session := s.sessions.ResolveSession(pr.Header, pr.APIKey, prefixKey)

	ccReq, warns := translate.BuildCcRequest(areq, translate.BuildOpts{
		Now:                time.Now(),
		NodeVersion:        s.cfg.FakeNodeVersion,
		WorkingDir:         masq.WorkingDirForSession(session),
		AssistantReasoning: s.cfg.AssistantReasoning,
		CacheMarkers:       s.cfg.CacheMarkers,
	})
	for _, wn := range warns {
		// "info: " 前缀的是观测性信息（如断点透传落点），不算异常
		if strings.HasPrefix(wn, "info: ") {
			slog.Info("convert", "detail", wn, "model", areq.Model)
			continue
		}
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

	s.upstream.EnsureInitialized(pr.APIKey)
	resp, err := s.upstream.Generate(ctx, pr.APIKey, session, pr.Header, ccReq)
	if err != nil {
		if r.Context().Err() != nil {
			slog.Info("client disconnected before upstream response", "model", model)
			return
		}
		slog.Error("upstream request failed", "error", err)
		pr.Errors.Write(w, http.StatusBadGateway, "api_error", "Upstream error: "+err.Error(), 0)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		slog.Error("cc api error", "status", resp.StatusCode)
		mapHTTPError(w, pr.Errors, resp.StatusCode, errBody)
		return
	}

	idle := s.cfg.IdleStream
	if !pr.Stream {
		idle = s.cfg.IdleBuffer
	}
	upstreamBody := newIdleReader(resp.Body, idle)
	defer func() { _ = upstreamBody.Close() }()

	tr := translate.NewStreamTranslator(model, messageID)
	if pr.Stream {
		s.serveStream(w, r, upstreamBody, tr, pr, cancel, model)
	} else {
		s.serveBuffer(w, r, upstreamBody, tr, pr, cancel, model)
	}
}

// encodeAll 把一批事件喂给编码器并展开为字节帧。
func encodeAll(enc EventEncoder, evs []types.StreamEvent) [][]byte {
	if len(evs) == 0 {
		return nil
	}
	var frames [][]byte
	for _, e := range evs {
		frames = append(frames, enc.Feed(e)...)
	}
	return frames
}

// serveStream 流式出站：cmdc NDJSON → 出站协议 SSE。
//
// 「何时发 200」沿用以内容为准的既有语义：message_start 级的事件同样缓冲，
// 只有 StreamTranslator 确认产出实质内容（text/thinking/tool_use）才落头，
// 使首帧前的超时/错误仍能回退为 JSON 错误响应。
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, body io.Reader, tr *translate.StreamTranslator, pr *PipelineRequest, cancel context.CancelFunc, model string) {
	enc, ew := pr.Encoder, pr.Errors
	sw := &sseWriter{w: w}
	sw.add(encodeAll(enc, tr.Start())...)

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), scannerMaxLine)
	for sc.Scan() {
		sw.add(encodeAll(enc, tr.Feed(sc.Bytes()))...)
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
				ew.Write(w, http.StatusTooManyRequests, "rate_limit_error", hint, 5)
			} else {
				sw.add(encodeAll(enc, []types.StreamEvent{types.NewErrorEvent("rate_limit_error", hint)})...)
			}
		default:
			cancel()
			slog.Error("stream read error", "error", err)
			if !sw.started {
				ew.Write(w, http.StatusBadGateway, "api_error", "Upstream error: "+err.Error(), 0)
			} else {
				sw.add(encodeAll(enc, []types.StreamEvent{types.NewErrorEvent("api_error", "Upstream error: "+err.Error())})...)
			}
		}
		return
	}

	sw.add(encodeAll(enc, tr.Finish())...)
	// 协议级收尾帧（Responses 的终态事件、Chat 的 [DONE]）：编码器自行判断是否已失败，
	// 因此这里无条件调用，幂等由编码器保证
	sw.add(enc.Finish()...)

	if tr.HasUpstreamError() {
		status, typ, msg, retry := mapStreamEventError(tr.ErrMessage())
		slog.Warn("upstream stream error", "status", status, "message", msg)
		if !sw.started {
			ew.Write(w, status, typ, msg, retry)
			return
		}
		return // started：错误事件已在流内发出
	}
	if tr.OutputTokens() == 0 {
		// 零输出转换为 429 错误处理，防止下游客户端异常计费；流已开启时在 Finish 中发出错误事件
		cancel()
		if !sw.started {
			ew.Write(w, http.StatusTooManyRequests, "rate_limit_error",
				"Empty response from upstream (zero output tokens)", 10)
		}
		return
	}
	s.resetTimeouts()
	sw.start() // 兜底：仅 usage 无内容等极端情形下也要完成 SSE 生命周期
}

// serveBuffer 非流式出站：聚合同一事件序列为完整响应对象。
func (s *Server) serveBuffer(w http.ResponseWriter, r *http.Request, body io.Reader, tr *translate.StreamTranslator, pr *PipelineRequest, cancel context.CancelFunc, model string) {
	agg, ew := pr.Aggregator, pr.Errors
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
			ew.Write(w, http.StatusTooManyRequests, "rate_limit_error", hint, 5)
		default:
			cancel()
			slog.Error("buffer read error", "error", err)
			ew.Write(w, http.StatusBadGateway, "api_error", "Upstream error: "+err.Error(), 0)
		}
		return
	}

	for _, e := range tr.Finish() {
		agg.Feed(e)
	}

	if tr.HasUpstreamError() {
		status, typ, msg, retry := mapStreamEventError(tr.ErrMessage())
		slog.Warn("upstream stream error (non-stream)", "status", status, "message", msg)
		ew.Write(w, status, typ, msg, retry)
		return
	}
	if tr.OutputTokens() == 0 {
		cancel()
		ew.Write(w, http.StatusTooManyRequests, "rate_limit_error",
			"Empty response from upstream (zero output tokens)", 10)
		return
	}
	s.resetTimeouts()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = writeJSON(w, agg.Result())
}
