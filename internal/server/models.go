package server

import (
	"net/http"
	"time"

	"github.com/B1anYu/cmdc2api/internal/errs"
)

// handleModels 模型列表（Anthropic 风味）。带上 key 时透传上游
// /provider/v1/models（带 TTL 缓存），否则回退硬编码列表。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := s.upstream.Models(getAPIKey(r.Header))
	now := time.Now().UTC().Format(time.RFC3339)

	type modelEntry struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		CreatedAt   string `json:"created_at"`
	}
	data := make([]modelEntry, 0, len(models))
	for _, m := range models {
		data = append(data, modelEntry{Type: "model", ID: m.ID, DisplayName: m.Name, CreatedAt: now})
	}
	var first, last *string
	if len(data) > 0 {
		first, last = &data[0].ID, &data[len(data)-1].ID
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = writeJSON(w, map[string]any{
		"data":     data,
		"has_more": false,
		"first_id": first,
		"last_id":  last,
	})
}

// mapStreamEventError 流内 error 事件的映射（与 HTTP 错误共用 errs 映射表）。
func mapStreamEventError(message string) (status int, typ, msg string, retryAfter int) {
	return errs.MapEvent(message)
}
