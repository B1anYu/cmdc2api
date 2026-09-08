// Package errs 把 cmdc 上游错误映射为 Anthropic 风味错误（状态码 + 类型 +
// Retry-After）。流式翻译器（SSE error 事件）与 HTTP 层（JSON 错误）共用。
package errs

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// 上游状态码 → (对客户端状态码, Anthropic 错误类型, Retry-After 秒)。
// 402 payment required 映射为 429 rate_limit_error 并附带 Retry-After，驱动客户端 SDK 自动退避重试。
func MapStatus(ccStatus int) (status int, typ string, retryAfter int) {
	switch ccStatus {
	case 400:
		return 400, "invalid_request_error", 0
	case 401:
		return 401, "authentication_error", 0
	case 402:
		return 429, "rate_limit_error", 30
	case 403:
		return 401, "authentication_error", 0
	case 404:
		return 404, "not_found_error", 0
	case 422:
		return 400, "invalid_request_error", 0
	case 429:
		return 429, "rate_limit_error", 30
	case 500, 502:
		return 502, "api_error", 0
	case 503:
		return 503, "overloaded_error", 0
	default:
		return 502, "api_error", 0
	}
}

// StatusFromMessage 从流内 error 事件的消息里提取上游状态码。
// cmdc 把状态拼在消息前缀里，形如 "<500> internal error"。
var statusRe = regexp.MustCompile(`^<(\d{3})>`)

func StatusFromMessage(msg string) int {
	if m := statusRe.FindStringSubmatch(msg); m != nil {
		var code int
		if _, err := fmt.Sscanf(m[1], "%d", &code); err == nil {
			return code
		}
	}
	return 502
}

// ExtractMessage 从上游错误响应体提取人类可读消息。
func ExtractMessage(body []byte, ccStatus int) string {
	fallback := fmt.Sprintf("CC API error (%d)", ccStatus)
	if len(body) == 0 {
		return fallback
	}
	var parsed struct {
		Error   *struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		if parsed.Error != nil && parsed.Error.Message != "" {
			return parsed.Error.Message
		}
		if parsed.Message != "" {
			return parsed.Message
		}
	}
	if len(body) > 200 {
		body = body[:200]
	}
	if s := strings.TrimSpace(string(body)); s != "" {
		return s
	}
	return fallback
}

// Map 映射一次上游 HTTP 错误响应。
func Map(ccStatus int, body []byte) (status int, typ, message string, retryAfter int) {
	status, typ, retryAfter = MapStatus(ccStatus)
	return status, typ, ExtractMessage(body, ccStatus), retryAfter
}

// MapEvent 映射流内 error 事件。
func MapEvent(message string) (status int, typ, msg string, retryAfter int) {
	if message == "" {
		message = "Unknown CC error"
	}
	status = StatusFromMessage(message)
	status, typ, retryAfter = MapStatus(status)
	return status, typ, message, retryAfter
}
