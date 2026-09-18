package server

import (
	"encoding/json"
	"net/http"

	"github.com/B1anYu/cmdc2api/internal/types"
)

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readBody(w, r, anthropicErrors{})
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

	// Anthropic 是一等公民：请求体即规范格式，无需归一化，直接进共享管线
	s.runPipeline(w, r, &PipelineRequest{
		Req:        &areq,
		APIKey:     apiKey,
		Header:     r.Header,
		Stream:     areq.Stream,
		Encoder:    anthropicEncoder{},
		Aggregator: newAnthropicAggregator(),
		Errors:     anthropicErrors{},
	})
}
