// Package dashscopewire 实现 DashScope Native 协议的线格式。
//
// 只覆盖 native 形态。DashScope 的 compatible 模式复用 OpenAI 信封，
// 由 internal/protocol/openaiwire 处理——这正是「compatible 与 native 必须
// 分成两个 Provider」的原因之一：连错误结构都不是同一套。
package dashscopewire

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
)

// Envelope 是 DashScope Native 的 HTTP 错误信封。
//
// 与 OpenAI/Anthropic 的嵌套信封不同，它是平铺的，且带 RequestID——
// 这个 ID 是向阿里云提工单时的唯一凭据，必须保留到审计日志里。
type Envelope struct {
	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// WSFailure 是 /api-ws/v1/inference（A 类 run-task 指令流）的 task-failed 事件。
//
// A 类协议的错误不走 HTTP 状态码，而是作为一个 WebSocket 事件送达，
// 因此需要独立的解码路径。
type WSFailure struct {
	Header struct {
		TaskID       string `json:"task_id,omitempty"`
		Event        string `json:"event,omitempty"`
		ErrorCode    string `json:"error_code,omitempty"`
		ErrorMessage string `json:"error_message,omitempty"`
	} `json:"header"`
}

// EncodeError 把统一错误编成 DashScope Native 线格式。
func EncodeError(e *canonical.Error) (status int, body []byte, headers map[string]string) {
	code := e.UpstreamCode
	if code == "" {
		code = wireCode(e.Class)
	}
	body, _ = json.Marshal(Envelope{Code: code, Message: e.Message, RequestID: e.UpstreamRequestID})
	return e.HTTPStatus(), body, e.Headers()
}

func wireCode(c canonical.ErrorClass) string {
	switch c {
	case canonical.ClassAuth:
		return "InvalidApiKey"
	case canonical.ClassRateLimit:
		return "Throttling.RateQuota"
	case canonical.ClassQuota:
		return "Arrearage"
	case canonical.ClassContextLength:
		return "InvalidParameter"
	case canonical.ClassContentFilter:
		return "DataInspectionFailed"
	case canonical.ClassBadRequest, canonical.ClassUnsupported:
		return "InvalidParameter"
	case canonical.ClassNotImplemented:
		return "Unsupported"
	case canonical.ClassUpstreamUnavailable:
		return "ServiceUnavailable"
	default:
		return "InternalError"
	}
}

// DecodeError 把 DashScope Native 的 HTTP 错误响应还原成统一错误。
func DecodeError(status int, body []byte, h http.Header, now time.Time) *canonical.Error {
	var env Envelope
	_ = json.Unmarshal(body, &env)

	e := build(status, env.Code, env.Message)
	if env.RequestID != "" {
		e.UpstreamRequestID = env.RequestID
	}
	if h != nil {
		e.RetryAfter = canonical.ParseRetryAfter(h.Get("Retry-After"), now)
		e.RateLimit = canonical.ParseRateLimitHeaders(h)
	}
	return e
}

// DecodeWSFailure 把 A 类协议的 task-failed 事件还原成统一错误。
//
// 这类失败没有 HTTP 状态码可依据，只能靠 error_code 判断——所以传 0 进去，
// classify 会走纯 code 分支。
func DecodeWSFailure(raw []byte) *canonical.Error {
	var f WSFailure
	if err := json.Unmarshal(raw, &f); err != nil {
		return canonical.Wrapf(err, canonical.ClassInternal,
			"无法解析 DashScope task-failed 事件")
	}
	e := build(0, f.Header.ErrorCode, f.Header.ErrorMessage)
	if f.Header.TaskID != "" {
		e.Param = "task_id=" + f.Header.TaskID
	}
	return e
}

func build(status int, code, msg string) *canonical.Error {
	class := classify(status, code, msg)
	if msg == "" {
		if status > 0 {
			msg = http.StatusText(status)
		} else {
			msg = string(class)
		}
	}
	return &canonical.Error{
		Class:          class,
		Message:        msg,
		Retryable:      class.DefaultRetryable(),
		UpstreamStatus: status,
		UpstreamCode:   code,
	}
}

// classify 从 DashScope 的错误码推断分类。
//
// 优先看 code：DashScope 把「余额不足」和「参数非法」都返回 400，只有 code
// 能区分，而前者换个凭据可能成功、后者换谁都失败。
func classify(status int, code, msg string) canonical.ErrorClass {
	switch {
	case code == "":
		// 无 code 可依，退回状态码判断。
	case strings.HasPrefix(code, "Throttling"):
		return canonical.ClassRateLimit
	case code == "Arrearage" || strings.Contains(code, "Quota") || code == "AllocationQuotaExceeded":
		return canonical.ClassQuota
	case code == "InvalidApiKey" || code == "Unauthorized" || strings.Contains(code, "AccessDenied"):
		return canonical.ClassAuth
	case code == "DataInspectionFailed":
		return canonical.ClassContentFilter
	case code == "ServiceUnavailable" || code == "InternalError" || strings.HasPrefix(code, "InternalError."):
		return canonical.ClassUpstreamUnavailable
	case code == "ModelNotFound" || code == "InvalidParameter" || strings.HasPrefix(code, "InvalidParameter."):
		// InvalidParameter 是个大筐，超长上下文也走这个 code。
		// 只能靠 message 里的关键词进一步区分——不优雅，但把「超长」误判成
		// 「参数错误」会让客户端收不到正确的截断提示。
		if mentionsLength(msg) {
			return canonical.ClassContextLength
		}
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
	case status == 0:
		// WebSocket 失败且 code 无法识别。
		return canonical.ClassUpstreamUnavailable
	default:
		return canonical.ClassInternal
	}
}

func mentionsLength(msg string) bool {
	l := strings.ToLower(msg)
	for _, kw := range []string{"length", "too long", "exceed", "max_tokens", "context"} {
		if strings.Contains(l, kw) {
			return true
		}
	}
	return false
}
