package openaiwire

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
)

var refTime = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

func TestDecodeErrorClassification(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantClass canonical.ErrorClass
		wantRetry bool
	}{
		{
			// 最重要的一格：超长上下文的 HTTP 状态是 400，与普通参数错误无法区分，
			// 只有 code 能认出来。误判成 bad_request 不影响可重试性，但会让客户端
			// 收不到「该截断输入了」的正确提示。
			name:      "context_length 靠 code 而非状态码识别",
			status:    400,
			body:      `{"error":{"message":"maximum context length is 128000 tokens","type":"invalid_request_error","code":"context_length_exceeded"}}`,
			wantClass: canonical.ClassContextLength,
			wantRetry: false,
		},
		{
			// 配额耗尽对**当前凭据**是确定性失败，但换一个凭据完全可能成功，
			// 所以对网关而言是可重试的。这与「上游临时故障」是两回事。
			name:      "insufficient_quota 可换凭据重试",
			status:    429,
			body:      `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`,
			wantClass: canonical.ClassQuota,
			wantRetry: true,
		},
		{
			name:      "rate_limit",
			status:    429,
			body:      `{"error":{"message":"Rate limit reached","type":"rate_limit_error"}}`,
			wantClass: canonical.ClassRateLimit,
			wantRetry: true,
		},
		{
			name:      "auth",
			status:    401,
			body:      `{"error":{"message":"Incorrect API key","type":"authentication_error"}}`,
			wantClass: canonical.ClassAuth,
			wantRetry: true,
		},
		{
			name:      "content_filter 换谁都失败",
			status:    400,
			body:      `{"error":{"message":"blocked","type":"invalid_request_error","code":"content_filter"}}`,
			wantClass: canonical.ClassContentFilter,
			wantRetry: false,
		},
		{
			// 上游 5xx 经常返回 HTML 或空体。解析不了 body 时状态码是唯一可靠信号，
			// 此时必须仍然给出可用的分类，而不是退化成 internal。
			name:      "非 JSON 的 5xx 仍按上游不可用处理",
			status:    502,
			body:      `<html>Bad Gateway</html>`,
			wantClass: canonical.ClassUpstreamUnavailable,
			wantRetry: true,
		},
		{
			name:      "空体 500",
			status:    500,
			body:      ``,
			wantClass: canonical.ClassUpstreamUnavailable,
			wantRetry: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := DecodeError(tc.status, []byte(tc.body), nil, refTime)
			if e.Class != tc.wantClass {
				t.Errorf("分类 = %q, 期望 %q", e.Class, tc.wantClass)
			}
			if e.Retryable != tc.wantRetry {
				t.Errorf("可重试 = %v, 期望 %v", e.Retryable, tc.wantRetry)
			}
			if e.Message == "" {
				t.Error("消息不得为空——即使 body 无法解析也要有可读信息")
			}
			if e.UpstreamStatus != tc.status {
				t.Errorf("上游状态码 = %d, 期望 %d", e.UpstreamStatus, tc.status)
			}
		})
	}
}

// TestDecodeErrorPreservesRetryHeaders 固化「header 是协议契约的一部分」。
// 丢掉 Retry-After，客户端 SDK 的指数退避就退化成固定间隔重试。
func TestDecodeErrorPreservesRetryHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "20")
	h.Set("X-RateLimit-Limit-Requests", "500")
	h.Set("X-RateLimit-Remaining-Requests", "0")
	h.Set("X-RateLimit-Limit-Tokens", "30000")
	h.Set("X-RateLimit-Remaining-Tokens", "12")
	h.Set("X-RateLimit-Reset-Tokens", "6m0s")

	e := DecodeError(429, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`), h, refTime)

	if e.RetryAfter != 20*time.Second {
		t.Errorf("RetryAfter = %v, 期望 20s", e.RetryAfter)
	}
	if e.RateLimit == nil {
		t.Fatal("限流额度信息丢失")
	}
	if e.RateLimit.LimitRequests != 500 || e.RateLimit.RemainingTokens != 12 {
		t.Errorf("额度还原有误: %+v", e.RateLimit)
	}
	if e.RateLimit.ResetTokens != 6*time.Minute {
		t.Errorf("ResetTokens = %v, 期望 6m", e.RateLimit.ResetTokens)
	}

	// 重新编码后 header 必须还原出来。
	_, _, out := EncodeError(e)
	if out["Retry-After"] != "20" {
		t.Errorf("Retry-After 未还原: %q", out["Retry-After"])
	}
	if out["X-RateLimit-Remaining-Tokens"] != "12" {
		t.Errorf("X-RateLimit-Remaining-Tokens 未还原: %q", out["X-RateLimit-Remaining-Tokens"])
	}
}

// TestRetryAfterHTTPDate 覆盖 RFC 7231 允许的第二种写法。
func TestRetryAfterHTTPDate(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", refTime.Add(45*time.Second).Format(http.TimeFormat))

	e := DecodeError(429, nil, h, refTime)
	if e.RetryAfter < 44*time.Second || e.RetryAfter > 45*time.Second {
		t.Errorf("RetryAfter = %v, 期望约 45s", e.RetryAfter)
	}
}

// TestRetryAfterInThePastIsIgnored 防止把过期的时间戳算成负数退避。
func TestRetryAfterInThePastIsIgnored(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", refTime.Add(-time.Hour).Format(http.TimeFormat))

	if e := DecodeError(429, nil, h, refTime); e.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v, 过期时间戳应视为 0", e.RetryAfter)
	}
}

func TestEncodeErrorShape(t *testing.T) {
	e := canonical.Newf(canonical.ClassUnsupported,
		"转换路径不支持 reasoning_signature")

	status, body, _ := EncodeError(e)
	if status != http.StatusUnprocessableEntity {
		t.Errorf("状态码 = %d, 期望 422", status)
	}

	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("编码结果不是合法 JSON: %v", err)
	}
	// 不为「路径不支持」编造新的 error.type：严格解析的 SDK 会崩。
	if env.Error.Type != typeInvalidRequest {
		t.Errorf("error.type = %q, 期望 %q", env.Error.Type, typeInvalidRequest)
	}
	if env.Error.Code != "unsupported_capability" {
		t.Errorf("error.code = %q, 细节应由 code 承载", env.Error.Code)
	}
}

// TestUpstreamCodeSurvivesRoundTrip 保证上游原始 code 不被网关自己的分类覆盖，
// 否则排障时无法对上游追查。
func TestUpstreamCodeSurvivesRoundTrip(t *testing.T) {
	e := DecodeError(400, []byte(`{"error":{"message":"x","type":"invalid_request_error","code":"context_length_exceeded"}}`), nil, refTime)

	_, body, _ := EncodeError(e)
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "context_length_exceeded" {
		t.Errorf("上游 code 丢失: %q", env.Error.Code)
	}
}
