package anthropicwire

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
			name:      "overloaded 是典型的可重试上游故障",
			status:    529,
			body:      `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantClass: canonical.ClassUpstreamUnavailable,
			wantRetry: true,
		},
		{
			// Anthropic 用 request_too_large 表达超长，与 OpenAI 的
			// context_length_exceeded 是同一件事，必须归到同一个分类，
			// 否则跨协议时客户端会收到不一致的错误语义。
			name:      "request_too_large 归入超长上下文",
			status:    413,
			body:      `{"type":"error","error":{"type":"request_too_large","message":"too large"}}`,
			wantClass: canonical.ClassContextLength,
			wantRetry: false,
		},
		{
			name:      "authentication",
			status:    401,
			body:      `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			wantClass: canonical.ClassAuth,
			wantRetry: true,
		},
		{
			name:      "rate_limit",
			status:    429,
			body:      `{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`,
			wantClass: canonical.ClassRateLimit,
			wantRetry: true,
		},
		{
			name:      "invalid_request",
			status:    400,
			body:      `{"type":"error","error":{"type":"invalid_request_error","message":"bad tool schema"}}`,
			wantClass: canonical.ClassBadRequest,
			wantRetry: false,
		},
		{
			name:      "非 JSON 的 5xx",
			status:    503,
			body:      `upstream down`,
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
				t.Error("消息不得为空")
			}
		})
	}
}

// TestEncodeKeepsOuterTypeDiscriminator 固化 Anthropic 信封的实质差异：
// 外层有一个 "type":"error" 判别字段，官方 SDK 会检查它。
func TestEncodeKeepsOuterTypeDiscriminator(t *testing.T) {
	_, body, _ := EncodeError(canonical.Newf(canonical.ClassRateLimit, "slow down"))

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["type"]; !ok {
		t.Fatal(`缺少外层 "type" 字段，Anthropic SDK 会拒绝解析`)
	}

	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Type != "error" {
		t.Errorf(`外层 type = %q, 期望 "error"`, env.Type)
	}
	if env.Error.Type != typeRateLimit {
		t.Errorf("error.type = %q, 期望 %q", env.Error.Type, typeRateLimit)
	}
}

// TestQuotaMapsToRateLimit 记录一处有意的信息损失：
// Anthropic 没有独立的配额错误类型，只能并入 rate_limit_error。
// 网关内部仍保留 ClassQuota 用于路由决策，只是对外无法区分。
func TestQuotaMapsToRateLimit(t *testing.T) {
	status, body, _ := EncodeError(canonical.Newf(canonical.ClassQuota, "quota exhausted"))
	if status != http.StatusTooManyRequests {
		t.Errorf("状态码 = %d, 期望 429", status)
	}

	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Type != typeRateLimit {
		t.Errorf("error.type = %q, 期望 %q", env.Error.Type, typeRateLimit)
	}
}
