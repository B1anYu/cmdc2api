package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/B1anYu/cmdc2api/internal/masq"
	"github.com/B1anYu/cmdc2api/internal/translate"
	"github.com/B1anYu/cmdc2api/internal/types"
)

// handleResponses POST /v1/responses：Codex CLI 的 Responses 入站端点。
//
// 与 handleMessages / handleChatCompletions 同构（读体 → 鉴权 → 解析 → 归一化 → 共享管线），
// 差异只在三处：
//   - 错误出口用 OpenAI 形状（openaiErrors），流式与非流式一致；
//   - 入站先经 translate.ResponsesToRequestBody 归一化为规范格式，之后会话亲和、缓存断点、
//     伪装、usage 换算全部与 messages 路径共用同一条链路（星形单跳）；
//   - 出站换 Responses 编/聚合器，并把入站工具族降级信息（mapping）透传给出站做 item 形态还原。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readBody(w, r, openaiErrors{})
	if !ok {
		return
	}
	apiKey := getAPIKey(r.Header)
	if apiKey == "" {
		writeMissingAPIKey(w, openaiErrors{})
		return
	}

	var rreq types.ResponsesRequest
	if err := json.Unmarshal(body, &rreq); err != nil {
		openaiErrors{}.Write(w, http.StatusBadRequest, "invalid_request_error", "Invalid request body: "+err.Error(), 0)
		return
	}

	// 必须走「带原始报文」的版本：include/text 这类无上游能力的字段
	// 在 ResponsesRequest 里没有类型，只有原始报文能证明客户端确实发过它们（§0.4 留痕硬约束）。
	areq, mapping, warns, err := translate.ResponsesToRequestBody(body, &rreq)
	if err != nil {
		// 归一化的硬失败都是可预期的客户端问题（续接上一轮、空 input、工具重名），
		// 消息本身已带可操作建议，按原样透出；这里只按原因分流日志级别。
		switch {
		case errors.Is(err, translate.ErrResponsesPreviousResponseID):
			// 本代理不保存任何服务端状态，静默忽略会让客户端拿到一段没有前置上下文的对话
			slog.Info("previous_response_id rejected", "model", rreq.Model)
		case errors.Is(err, translate.ErrResponsesEmptyInput):
			// input 缺失、空串或内容全被丢弃：与其把畸形信封送上去，不如在本地说清原因
			slog.Warn("responses request normalized to zero messages", "model", rreq.Model)
		default:
			slog.Warn("responses conversion failed", "error", err)
		}
		openaiErrors{}.Write(w, http.StatusBadRequest, "invalid_request_error", err.Error(), 0)
		return
	}
	logResponsesConversion(warns, areq.Model)

	model := areq.Model
	if model == "" {
		model = translate.DefaultModel
	}
	// resp_ 前缀由出站侧约定（见 respout.go 的 responsesIDPrefix）。
	responseID := "resp_" + masq.NewHexID(12)

	// 终态事件与非流式响应体都要回显请求字段（Codex 对响应对象做浅校验），
	// 回显的必须是客户端原始声明，不能是归一化后的 Anthropic 形状。
	// top_p 刻意不在其列：它从未被转发给上游（BuildCcRequest 只转发 temperature），
	// 回显等于向客户端谎报参数已生效（见 translate/respout.go 的 ResponsesEcho 注释）。
	echo := translate.ResponsesEcho{
		Instructions:    strings.TrimSpace(rreq.Instructions),
		Tools:           rreq.Tools,
		ToolChoice:      rreq.ToolChoice,
		Temperature:     rreq.Temperature,
		MaxOutputTokens: responsesMaxOutputTokensEcho(rreq.MaxOutputTokens),
		// parallel_tool_calls 回显客户端的真实声明：未声明 → 协议默认 true
		// （上游默认即允许并行，与本代理「任由上游并行」的实际行为一致）；显式 false
		// 已被映射为上游的 tool_choice.disable_parallel_tool_use，故回显 false。
		ParallelToolCalls: rreq.ParallelToolCalls == nil || *rreq.ParallelToolCalls,
	}

	// 编码器与聚合器都持有请求级状态（item 生命周期、sequence_number、usage），必须每请求新建；
	// 两者共用同一个 resp_ id，保证流式/非流式响应 ID 语义一致。
	enc := translate.NewResponsesEncoder(model, responseID, mapping)
	enc.SetEcho(echo)
	agg := translate.NewResponsesAggregator(model, responseID, mapping)
	agg.SetEcho(echo)

	s.runPipeline(w, r, &PipelineRequest{
		Req:        areq,
		APIKey:     apiKey,
		Header:     r.Header,
		Stream:     areq.Stream,
		Encoder:    enc,
		Aggregator: agg,
		Errors:     openaiErrors{},
	})
}

// responsesMaxOutputTokensEcho 把 max_output_tokens 收敛成回显指针。
// 入站类型带 omitempty，故 ≤0 只表示「客户端没声明」而非「要求 0 个 token」：
// 回显 null（键仍存在）比伪造一个 0 更接近请求事实。
func responsesMaxOutputTokensEcho(v int) *int {
	if v <= 0 {
		return nil
	}
	return &v
}

// logResponsesConversion 归一化告警日志。分流口径与 logChatConversion 完全一致：
// "info: " 前缀是观测性信息（客户端字段本就被满足），其余按警告计。
func logResponsesConversion(warns []string, model string) {
	for _, wn := range warns {
		if strings.HasPrefix(wn, "info: ") {
			slog.Info("convert", "detail", wn, "model", model)
			continue
		}
		slog.Warn("convert", "detail", wn, "model", model)
	}
}
