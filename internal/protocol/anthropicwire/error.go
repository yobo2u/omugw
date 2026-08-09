// Package anthropicwire 实现 Anthropic Messages 协议的线格式。
package anthropicwire

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
)

// Envelope 是 Anthropic 的错误信封。注意外层多一个 "type":"error" 判别字段——
// 这是 Anthropic 与 OpenAI 信封的实质差异，不能靠改字段名蒙混过去。
type Envelope struct {
	Type  string `json:"type"`
	Error Body   `json:"error"`
}

// Body 是错误主体。
type Body struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Anthropic 的 error.type 取值。
const (
	typeInvalidRequest  = "invalid_request_error"
	typeAuthentication  = "authentication_error"
	typePermission      = "permission_error"
	typeNotFound        = "not_found_error"
	typeRequestTooLarge = "request_too_large"
	typeRateLimit       = "rate_limit_error"
	typeAPI             = "api_error"
	typeOverloaded      = "overloaded_error"
)

// EncodeError 把统一错误编成 Anthropic 线格式。
func EncodeError(e *canonical.Error) (status int, body []byte, headers map[string]string) {
	payload := Envelope{
		Type:  "error",
		Error: Body{Type: wireType(e.Class), Message: e.Message},
	}
	body, _ = json.Marshal(payload)
	return e.HTTPStatus(), body, e.Headers()
}

func wireType(c canonical.ErrorClass) string {
	switch c {
	case canonical.ClassAuth:
		return typeAuthentication
	case canonical.ClassRateLimit, canonical.ClassQuota:
		return typeRateLimit
	case canonical.ClassContextLength:
		return typeRequestTooLarge
	case canonical.ClassContentFilter, canonical.ClassBadRequest, canonical.ClassUnsupported:
		return typeInvalidRequest
	case canonical.ClassUpstreamUnavailable:
		return typeOverloaded
	default:
		return typeAPI
	}
}

// DecodeError 把 Anthropic 上游的错误响应还原成统一错误。
func DecodeError(status int, body []byte, h http.Header, now time.Time) *canonical.Error {
	var env Envelope
	_ = json.Unmarshal(body, &env)

	class := classify(status, env.Error.Type)
	msg := env.Error.Message
	if msg == "" {
		msg = http.StatusText(status)
	}

	e := &canonical.Error{
		Class:          class,
		Message:        msg,
		Retryable:      class.DefaultRetryable(),
		UpstreamStatus: status,
		UpstreamCode:   env.Error.Type,
	}
	if h != nil {
		e.RetryAfter = canonical.ParseRetryAfter(h.Get("Retry-After"), now)
		e.RateLimit = canonical.ParseRateLimitHeaders(h)
	}
	return e
}

func classify(status int, typ string) canonical.ErrorClass {
	switch typ {
	case typeAuthentication, typePermission:
		return canonical.ClassAuth
	case typeRateLimit:
		return canonical.ClassRateLimit
	case typeRequestTooLarge:
		return canonical.ClassContextLength
	case typeOverloaded, typeAPI:
		return canonical.ClassUpstreamUnavailable
	case typeNotFound, typeInvalidRequest:
		return canonical.ClassBadRequest
	}

	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return canonical.ClassAuth
	case status == http.StatusTooManyRequests:
		return canonical.ClassRateLimit
	case status == http.StatusRequestEntityTooLarge:
		return canonical.ClassContextLength
	case status >= 500:
		return canonical.ClassUpstreamUnavailable
	case status >= 400:
		return canonical.ClassBadRequest
	default:
		return canonical.ClassInternal
	}
}
