package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/B1anYu/cmdc2api/internal/translate"
	"github.com/B1anYu/cmdc2api/internal/types"
)

// handleChatCompletions POST /v1/chat/completions：OpenAI Chat Completions 入站端点。
//
// 与 handleMessages 同构（读体 → 鉴权 → 解析 → 归一化 → 共享管线），差异只在三处：
//   - 错误出口换 OpenAI 形状（openaiErrors），流式/非流式都是；
//   - 入站先经 translate.ChatToRequestBody（带原始报文，无类型字段才留得下痕）归一化为
//     规范格式，之后所有上游能力（会话亲和、缓存断点、伪装、usage 换算）与 messages 路径完全共用；
//   - 出站换 Chat 编/聚合器。
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readBody(w, r, openaiErrors{})
	if !ok {
		return
	}
	apiKey := getAPIKey(r.Header)
	if apiKey == "" {
		openaiErrors{}.Write(w, http.StatusUnauthorized, "authentication_error",
			"Missing API key. Send in Authorization: Bearer <key> or x-api-key header", 0)
		return
	}

	var creq types.ChatRequest
	if err := json.Unmarshal(body, &creq); err != nil {
		openaiErrors{}.Write(w, http.StatusBadRequest, "invalid_request_error", "Invalid request body: "+err.Error(), 0)
		return
	}
	if len(creq.Messages) == 0 {
		openaiErrors{}.Write(w, http.StatusBadRequest, "invalid_request_error", "messages: at least one message is required", 0)
		return
	}

	// 必须走「带原始报文」的版本：frequency_penalty/logit_bias/store 这类无上游能力的字段
	// 在 ChatRequest 里没有类型，只有原始报文能证明客户端确实发过它们（留痕硬约束）。
	areq, warns, err := translate.ChatToRequestBody(body, &creq)
	if err != nil {
		msg := err.Error()
		// n>1 是可预期的客户端能力问题，不是上游故障：单独给出可操作的提示，
		// 其余错误按原样透出（当前仅此三类硬失败）。
		if errors.Is(err, translate.ErrChatUnsupportedN) {
			msg = "Invalid 'n': " + msg
		}
		openaiErrors{}.Write(w, http.StatusBadRequest, "invalid_request_error", msg, 0)
		return
	}
	logChatConversion(warns, areq.Model)

	// 消息全被丢弃（空 content、未知 role 等）时归一化产物没有任何可发送内容。
	// 本地拒绝而不是把空信封丢给上游：上游会回一个与客户端输入无关的 400，
	// 排查成本远高于在这里说清原因。
	if len(areq.Messages) == 0 {
		slog.Warn("chat request normalized to zero messages", "model", areq.Model)
		openaiErrors{}.Write(w, http.StatusBadRequest, "invalid_request_error",
			"messages: no convertible content after normalization", 0)
		return
	}

	// 编码器与聚合器都持有请求级状态（累计文本、工具序号、usage），必须每请求新建；
	// 两者共用同一个 chatcmpl- id，保证流式/非流式响应 ID 语义一致。
	model := areq.Model
	if model == "" {
		model = translate.DefaultModel
	}
	id := translate.NewChatCompletionID()

	s.runPipeline(w, r, &PipelineRequest{
		Req:        areq,
		APIKey:     apiKey,
		Header:     r.Header,
		Stream:     areq.Stream,
		Encoder:    translate.NewChatEncoder(model, id),
		Aggregator: translate.NewChatAggregator(model, id),
		Errors:     openaiErrors{},
	})
}

// logChatConversion 归一化告警日志。分流口径与管线内 BuildCcRequest 的 warns 完全一致：
// "info: " 前缀是观测性信息（客户端字段本就被满足），其余按警告计。
// 落成独立函数是为了不把日志格式散落两处（responses 侧同此约定）。
func logChatConversion(warns []string, model string) {
	for _, wn := range warns {
		if strings.HasPrefix(wn, "info: ") {
			slog.Info("convert", "detail", wn, "model", model)
			continue
		}
		slog.Warn("convert", "detail", wn, "model", model)
	}
}
