package dashscopewire

import (
	"encoding/json"
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
			name:      "Throttling 前缀族归入限流",
			status:    429,
			body:      `{"code":"Throttling.RateQuota","message":"Requests rate limit exceeded","request_id":"r-1"}`,
			wantClass: canonical.ClassRateLimit,
			wantRetry: true,
		},
		{
			name:      "Throttling.AllocationQuota 同样是限流而非配额耗尽",
			status:    429,
			body:      `{"code":"Throttling.AllocationQuota","message":"allocated quota exceeded"}`,
			wantClass: canonical.ClassRateLimit,
			wantRetry: true,
		},
		{
			// 欠费与参数非法都可能返回 400，只有 code 能区分，
			// 而两者的可重试性完全相反。
			name:      "Arrearage 是配额问题，换凭据可能成功",
			status:    400,
			body:      `{"code":"Arrearage","message":"Access denied, please make sure your account is in good standing"}`,
			wantClass: canonical.ClassQuota,
			wantRetry: true,
		},
		{
			name:      "InvalidApiKey",
			status:    401,
			body:      `{"code":"InvalidApiKey","message":"Invalid API-key provided"}`,
			wantClass: canonical.ClassAuth,
			wantRetry: true,
		},
		{
			name:      "DataInspectionFailed 是内容安全拦截，换谁都失败",
			status:    400,
			body:      `{"code":"DataInspectionFailed","message":"Input data may contain inappropriate content"}`,
			wantClass: canonical.ClassContentFilter,
			wantRetry: false,
		},
		{
			// InvalidParameter 是个大筐，超长上下文也走这个 code，
			// 只能靠 message 关键词进一步区分。
			name:      "InvalidParameter 带长度关键词时归入超长上下文",
			status:    400,
			body:      `{"code":"InvalidParameter","message":"Range of input length should be [1, 30000]"}`,
			wantClass: canonical.ClassContextLength,
			wantRetry: false,
		},
		{
			name:      "InvalidParameter 不带长度关键词时是普通参数错误",
			status:    400,
			body:      `{"code":"InvalidParameter","message":"tool_choice is not supported for this model"}`,
			wantClass: canonical.ClassBadRequest,
			wantRetry: false,
		},
		{
			name:      "ServiceUnavailable",
			status:    503,
			body:      `{"code":"ServiceUnavailable","message":"try later"}`,
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
		})
	}
}

// TestRequestIDIsPreserved 固化「request_id 必须保留」。
// 没有它就无法向阿里云追查一次失败调用。
func TestRequestIDIsPreserved(t *testing.T) {
	e := DecodeError(400, []byte(`{"code":"InvalidParameter","message":"x","request_id":"abc-123"}`), nil, refTime)
	if e.UpstreamRequestID != "abc-123" {
		t.Errorf("request_id 丢失: UpstreamRequestID = %q", e.UpstreamRequestID)
	}
	if e.Param != "" {
		t.Errorf("request_id 不得占用 Param: Param = %q", e.Param)
	}

	_, body, _ := EncodeError(e)
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("编码错误: %v", err)
	}
	if env.RequestID != "abc-123" {
		t.Errorf("request_id 重编码丢失: RequestID = %q", env.RequestID)
	}
}

// TestDecodeWSFailure 覆盖 A 类协议（/api-ws/v1/inference，run-task 指令流）。
// 这类失败不走 HTTP 状态码，而是作为 WebSocket 事件送达，需要独立解码路径。
func TestDecodeWSFailure(t *testing.T) {
	raw := []byte(`{"header":{"task_id":"t-9","event":"task-failed",` +
		`"error_code":"Throttling.RateQuota","error_message":"too many concurrent tasks"}}`)

	e := DecodeWSFailure(raw)
	if e.Class != canonical.ClassRateLimit {
		t.Errorf("分类 = %q, 期望 rate_limit", e.Class)
	}
	if !e.Retryable {
		t.Error("限流应可重试")
	}
	if e.Param != "task_id=t-9" {
		t.Errorf("task_id 丢失: Param = %q", e.Param)
	}
	if e.UpstreamStatus != 0 {
		t.Errorf("WebSocket 失败没有 HTTP 状态码，应为 0，实际 %d", e.UpstreamStatus)
	}
}

// TestDecodeWSFailureUnknownCodeFailsSafe 验证无法识别的 WS 错误码不会被
// 归类成 internal——那样会导致本可重试的上游故障被判成不可重试。
func TestDecodeWSFailureUnknownCodeFailsSafe(t *testing.T) {
	e := DecodeWSFailure([]byte(`{"header":{"event":"task-failed","error_code":"SomethingNew"}}`))
	if e.Class != canonical.ClassUpstreamUnavailable {
		t.Errorf("未知 WS 错误码应按上游不可用处理，实际 %q", e.Class)
	}
	if !e.Retryable {
		t.Error("未知的上游故障应允许换凭据重试")
	}
}

func TestEncodeErrorShape(t *testing.T) {
	_, body, _ := EncodeError(canonical.Newf(canonical.ClassContentFilter, "blocked"))

	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("编码结果不是合法 JSON: %v", err)
	}
	if env.Code != "DataInspectionFailed" {
		t.Errorf("code = %q, 期望 DataInspectionFailed", env.Code)
	}
	// DashScope 的信封是平铺的，不是 OpenAI 那种 {"error":{...}} 嵌套结构。
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, nested := raw["error"]; nested {
		t.Error("DashScope Native 信封应为平铺结构，不应有 error 嵌套层")
	}
}
