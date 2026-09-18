package server

import (
	"net/http"
	"time"

	"github.com/B1anYu/cmdc2api/internal/errs"
)

// handleModels 模型列表（Anthropic 与 OpenAI 的兼容超集）。带上 key 时透传上游
// /provider/v1/models（带 TTL 缓存），否则回退硬编码列表。
//
// 条目形状是两种协议的并集：object/created/owned_by 供 OpenAI 客户端（/v1/chat/completions
// 的使用者会读 list 形状），type/display_name/created_at 供既有 Anthropic 用法。
// 字段只增不删，避免破坏已经在读 Anthropic 形状的下游。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := s.upstream.Models(getAPIKey(r.Header))
	now := time.Now().UTC()

	type modelEntry struct {
		Type        string `json:"type"`
		Object      string `json:"object"`
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		Created     int64  `json:"created"`
		CreatedAt   string `json:"created_at"`
		OwnedBy     string `json:"owned_by"`
	}
	data := make([]modelEntry, 0, len(models))
	for _, m := range models {
		data = append(data, modelEntry{
			Type: "model", Object: "model", ID: m.ID, DisplayName: m.Name,
			// created 用 unix 秒（OpenAI 口径），created_at 保留 RFC3339（Anthropic 口径）
			Created: now.Unix(), CreatedAt: now.Format(time.RFC3339), OwnedBy: "cmdc",
		})
	}
	var first, last *string
	if len(data) > 0 {
		first, last = &data[0].ID, &data[len(data)-1].ID
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = writeJSON(w, map[string]any{
		"object":   "list",
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
