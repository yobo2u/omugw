// Package openaiwire 实现 OpenAI 系协议的线格式。
//
// 之所以把 Chat Completions 与 Responses 放在同一个包里，是因为两者共用同一套
// 错误信封与 header 约定；请求/响应体的差异由各自的 codec 处理。
package openaiwire

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
)

// Envelope 是 OpenAI 的错误信封。
type Envelope struct {
	Error Body `json:"error"`
}

// Body 是错误主体。
type Body struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

// OpenAI 的 error.type 取值。客户端 SDK 依赖它决定是否重试，
// 映射错了会让 SDK 对着一个永远不会成功的请求反复退避重试。
const (
	typeInvalidRequest = "invalid_request_error"
	typeAuthentication = "authentication_error"
	typePermission     = "permission_error"
	typeRateLimit      = "rate_limit_error"
	typeQuota          = "insufficient_quota"
	typeServer         = "server_error"
)

// EncodeError 把统一错误编成 OpenAI 线格式。
//
// 返回的 header 不是可选的调试信息：Retry-After 与 x-ratelimit-* 是客户端
// SDK 退避算法的输入，丢掉它们会让 SDK 退化成固定间隔重试。
func EncodeError(e *canonical.Error) (status int, body []byte, headers map[string]string) {
	typ, code := wireType(e.Class)
	if e.UpstreamCode != "" {
		code = e.UpstreamCode
	}

	payload := Envelope{Error: Body{
		Message: e.Message,
		Type:    typ,
		Param:   e.Param,
		Code:    code,
	}}
	// 字段全是字符串，Marshal 不会失败。
	body, _ = json.Marshal(payload)
	return e.HTTPStatus(), body, e.Headers()
}

func wireType(c canonical.ErrorClass) (typ, code string) {
	switch c {
	case canonical.ClassAuth:
		return typeAuthentication, ""
	case canonical.ClassRateLimit:
		return typeRateLimit, "rate_limit_exceeded"
	case canonical.ClassQuota:
		return typeQuota, "insufficient_quota"
	case canonical.ClassContextLength:
		return typeInvalidRequest, "context_length_exceeded"
	case canonical.ClassContentFilter:
		return typeInvalidRequest, "content_filter"
	case canonical.ClassUnsupported:
		// 仍归入 invalid_request_error：OpenAI 没有「路径不支持」这个类型，
		// 编造一个新 type 会让严格解析的 SDK 直接崩掉。用 code 承载细节。
		return typeInvalidRequest, "unsupported_capability"
	case canonical.ClassBadRequest:
		return typeInvalidRequest, ""
	case canonical.ClassUpstreamUnavailable:
		return typeServer, "upstream_unavailable"
	default:
		return typeServer, ""
	}
}

// DecodeError 把 OpenAI 系上游的错误响应还原成统一错误。
//
// now 用于解析 HTTP-date 形式的 Retry-After，由调用方注入以便测试。
func DecodeError(status int, body []byte, h http.Header, now time.Time) *canonical.Error {
	var env Envelope
	// 解析失败不是问题——上游在 5xx 时经常返回 HTML 或空体，
	// 此时状态码本身就是唯一可靠的信息。
	_ = json.Unmarshal(body, &env)

	class := classify(status, env.Error.Type, env.Error.Code)
	msg := env.Error.Message
	if msg == "" {
		msg = http.StatusText(status)
	}

	e := &canonical.Error{
		Class:          class,
		Message:        msg,
		Retryable:      class.DefaultRetryable(),
		UpstreamStatus: status,
		UpstreamCode:   env.Error.Code,
		Param:          env.Error.Param,
	}
	if h != nil {
		e.RetryAfter = canonical.ParseRetryAfter(h.Get("Retry-After"), now)
		e.RateLimit = canonical.ParseRateLimitHeaders(h)
	}
	return e
}

// classify 从状态码与 type/code 推断错误分类。
//
// 先看 code 再看 type 再看 status：越具体的信号越可靠。典型例子是
// context_length_exceeded——它的 HTTP 状态是 400，与普通参数错误无法区分，
// 只有 code 能把它认出来，而这两者的可重试性完全相反。
func classify(status int, typ, code string) canonical.ErrorClass {
	switch code {
	case "context_length_exceeded", "string_above_max_length":
		return canonical.ClassContextLength
	case "content_filter", "content_policy_violation":
		return canonical.ClassContentFilter
	case "insufficient_quota", "billing_hard_limit_reached":
		return canonical.ClassQuota
	case "rate_limit_exceeded":
		return canonical.ClassRateLimit
	}

	switch typ {
	case typeAuthentication, typePermission:
		return canonical.ClassAuth
	case typeQuota:
		return canonical.ClassQuota
	case typeRateLimit:
		return canonical.ClassRateLimit
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
