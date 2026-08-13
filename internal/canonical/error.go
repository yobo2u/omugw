package canonical

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// ErrorClass 是统一错误分类。
//
// 分类的用途不是「好看的错误码」，而是驱动两个决策：
//   - 路由层：这个错误换一个凭据/Provider 重试有没有意义（见 Retryable）
//   - 编码层：映射成各入站协议的哪种错误结构与哪些 header
//
// 客户端 SDK 的重试逻辑依赖 error type 与 retry-after / x-ratelimit-* header。
// 丢掉这些 header，SDK 的退避就会退化成固定间隔重试，把上游打得更惨。
type ErrorClass string

const (
	ClassAuth                ErrorClass = "auth"
	ClassRateLimit           ErrorClass = "rate_limit"
	ClassQuota               ErrorClass = "quota"
	ClassContextLength       ErrorClass = "context_length"
	ClassContentFilter       ErrorClass = "content_filter"
	ClassUpstreamUnavailable ErrorClass = "upstream_unavailable"
	ClassBadRequest          ErrorClass = "bad_request"
	ClassUnsupported         ErrorClass = "unsupported"
	ClassInternal            ErrorClass = "internal"

	// ClassNotImplemented 表示这条路径已在降级矩阵中设计好，但实现尚未落地。
	//
	// 与 ClassUnsupported 分开：后者是「这条路永远承载不了该能力」，前者是
	// 「这条路会有，只是还没建好」。对客户端而言前者要改请求，后者只要等——
	// 混成一个错误码，用户会去改一个本来就对的请求。
	ClassNotImplemented ErrorClass = "not_implemented"
)

// Error 是网关的统一错误类型。
type Error struct {
	Class   ErrorClass
	Message string

	// Retryable 表示「换一个凭据或 Provider 重试可能成功」。
	//
	// 注意这与「上游是否临时故障」不同：auth 和 quota 对**同一个凭据**是确定性
	// 失败，但对凭据池里的另一个凭据完全可能成功，所以是 true。
	// context_length 和 content_filter 换谁都一样失败，是 false。
	//
	// 另有一条与 Retryable 无关的硬约束（原则 2.4）：流式响应一旦发出首字节，
	// 无论 Retryable 为何都不得重试——重试会让客户端收到重复内容。
	Retryable bool

	// 上游原始信息，用于审计与问题定位。
	UpstreamStatus int
	UpstreamCode   string
	// UpstreamRequestID 是上游调用的唯一追踪 ID（如 DashScope request_id），
	// 用于问题定位与审计，不得与 Param（出错的请求参数名）混淆。
	UpstreamRequestID string

	RetryAfter time.Duration
	RateLimit  *RateLimitInfo

	// Param 指向出错的请求字段，OpenAI 系协议会把它回给客户端。
	Param string

	cause error
}

// RateLimitInfo 承载上游的限流额度信息，用于还原 x-ratelimit-* 响应头。
type RateLimitInfo struct {
	LimitRequests     int64
	RemainingRequests int64
	ResetRequests     time.Duration

	LimitTokens     int64
	RemainingTokens int64
	ResetTokens     time.Duration
}

func (e *Error) Error() string {
	if e.UpstreamCode != "" {
		return fmt.Sprintf("%s: %s (upstream_code=%s)", e.Class, e.Message, e.UpstreamCode)
	}
	return fmt.Sprintf("%s: %s", e.Class, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// HTTPStatus 返回这个错误对外暴露的 HTTP 状态码。
func (e *Error) HTTPStatus() int { return e.Class.HTTPStatus() }

// HTTPStatus 是错误分类到 HTTP 状态码的映射。
func (c ErrorClass) HTTPStatus() int {
	switch c {
	case ClassAuth:
		return http.StatusUnauthorized
	case ClassRateLimit, ClassQuota:
		return http.StatusTooManyRequests
	case ClassContextLength, ClassContentFilter, ClassBadRequest:
		return http.StatusBadRequest
	case ClassUnsupported:
		// 422 而非 400：请求本身语法合法，只是这条转换路径不支持它。
		// 区分开来，客户端才知道换个模型/Provider 可能就行。
		return http.StatusUnprocessableEntity
	case ClassUpstreamUnavailable:
		return http.StatusBadGateway
	case ClassNotImplemented:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

// DefaultRetryable 给出各分类的默认可重试性。构造错误时若无更精确的判断，
// 应当采用这个默认值而不是随手填 false。
func (c ErrorClass) DefaultRetryable() bool {
	switch c {
	case ClassAuth, ClassRateLimit, ClassQuota, ClassUpstreamUnavailable:
		return true
	default:
		return false
	}
}

// Headers 生成应当附加到响应上的 header。
//
// 这些 header 是客户端 SDK 退避算法的输入，属于协议契约的一部分，
// 不是可选的调试信息。
func (e *Error) Headers() map[string]string {
	h := map[string]string{}
	if e.RetryAfter > 0 {
		// Retry-After 以秒为单位，向上取整——向下取整会让客户端提前重试。
		secs := int64((e.RetryAfter + time.Second - 1) / time.Second)
		h["Retry-After"] = strconv.FormatInt(secs, 10)
	}
	if rl := e.RateLimit; rl != nil {
		if rl.LimitRequests > 0 {
			h["X-RateLimit-Limit-Requests"] = strconv.FormatInt(rl.LimitRequests, 10)
			h["X-RateLimit-Remaining-Requests"] = strconv.FormatInt(rl.RemainingRequests, 10)
		}
		if rl.LimitTokens > 0 {
			h["X-RateLimit-Limit-Tokens"] = strconv.FormatInt(rl.LimitTokens, 10)
			h["X-RateLimit-Remaining-Tokens"] = strconv.FormatInt(rl.RemainingTokens, 10)
		}
		if rl.ResetRequests > 0 {
			h["X-RateLimit-Reset-Requests"] = formatReset(rl.ResetRequests)
		}
		if rl.ResetTokens > 0 {
			h["X-RateLimit-Reset-Tokens"] = formatReset(rl.ResetTokens)
		}
	}
	return h
}

// formatReset 采用 OpenAI 的紧凑时长写法（6m0s / 1.5s）。
func formatReset(d time.Duration) string { return d.String() }

// ParseRetryAfter 解析 Retry-After 头。
//
// RFC 7231 允许秒数与 HTTP-date 两种写法，各家上游都用得上；解析失败返回 0，
// 由调用方决定退避策略——绝不因为一个头解析不了就丢掉整个错误。
func ParseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// parseDurationLoose 解析限流重置时长。
//
// OpenAI 用 Go 风格的紧凑写法（"6m0s"、"1.5s"），部分兼容 Provider 直接给裸秒数。
// 两种都接受，解析不了就返回 0。
func parseDurationLoose(v string) time.Duration {
	if v == "" {
		return 0
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return 0
}

// ParseRateLimitHeaders 从上游响应头还原限流额度信息。
//
// 与 Error.Headers 互为逆操作。之所以要还原再重新编码，而不是把上游 header
// 原样转发：网关可能对接多个上游，直接转发会让客户端看到互相矛盾的额度数字。
func ParseRateLimitHeaders(h http.Header) *RateLimitInfo {
	if h == nil {
		return nil
	}
	geti := func(k string) int64 {
		n, err := strconv.ParseInt(h.Get(k), 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	rl := &RateLimitInfo{
		LimitRequests:     geti("X-Ratelimit-Limit-Requests"),
		RemainingRequests: geti("X-Ratelimit-Remaining-Requests"),
		LimitTokens:       geti("X-Ratelimit-Limit-Tokens"),
		RemainingTokens:   geti("X-Ratelimit-Remaining-Tokens"),
		ResetRequests:     parseDurationLoose(h.Get("X-Ratelimit-Reset-Requests")),
		ResetTokens:       parseDurationLoose(h.Get("X-Ratelimit-Reset-Tokens")),
	}
	if rl.LimitRequests == 0 && rl.LimitTokens == 0 &&
		rl.ResetRequests == 0 && rl.ResetTokens == 0 {
		return nil
	}
	return rl
}

// Newf 构造一个采用默认可重试性的错误。
func Newf(class ErrorClass, format string, args ...any) *Error {
	return &Error{
		Class:     class,
		Message:   fmt.Sprintf(format, args...),
		Retryable: class.DefaultRetryable(),
	}
}

// Wrapf 在保留 cause 的前提下构造错误。
func Wrapf(cause error, class ErrorClass, format string, args ...any) *Error {
	return &Error{
		Class:     class,
		Message:   fmt.Sprintf(format, args...),
		Retryable: class.DefaultRetryable(),
		cause:     cause,
	}
}

// AsError 把任意 error 归一成 *Error。无法识别的错误按 internal 处理，
// 且**不可重试**——来源不明的错误重试只会放大故障。
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{
		Class:     ClassInternal,
		Message:   err.Error(),
		Retryable: false,
		cause:     err,
	}
}
