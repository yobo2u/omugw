package dashscopecompat

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/degrade"
)

// responsesInput 构造一次打 Responses 门的调用。
func responsesInput(raw string) callInput {
	return callInput{
		raw:             raw,
		upstreamModel:   "qwen-up",
		inboundEndpoint: degrade.EndpointOpenAIResponses,
	}
}

func asCanonical(t *testing.T, err error) *canonical.Error {
	t.Helper()
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("期望 *canonical.Error，实际 %T: %v", err, err)
	}
	return cerr
}

// TestResponsesDoorDecodesFlatEnvelope 防的是上游的原话被吞掉：这扇门返回
// 扁平信封 {request_id, code, message}，按 OpenAI 嵌套信封解码会取不到 code
// 与 message，客户端只能看到一句 Bad Request。
//
// 用例取自 2026-10-11 实测的续轮未命中响应。它必须是 bad_request 且不可重试：
// 这不是凭据故障，换凭据只会更糟。
func TestResponsesDoorDecodesFlatEnvelope(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"request_id":"req-1","code":"InvalidParameter",`+
			`"message":"Not found previous_response_id: resp_x."}`)
	})

	_, err := call(t, srv, responsesInput(`{"model":"m","input":"hi","previous_response_id":"resp_x"}`))
	e := asCanonical(t, err)

	if e.Class != canonical.ClassBadRequest {
		t.Errorf("分类 = %s，期望 bad_request", e.Class)
	}
	if e.Retryable {
		t.Error("续轮未命中不得标为可重试")
	}
	if e.Message != "Not found previous_response_id: resp_x." {
		t.Errorf("message = %q，上游原话丢了", e.Message)
	}
	if e.UpstreamCode != "InvalidParameter" {
		t.Errorf("upstream code = %q，期望 InvalidParameter", e.UpstreamCode)
	}
	if e.UpstreamRequestID != "req-1" {
		t.Errorf("request_id = %q，期望 req-1", e.UpstreamRequestID)
	}
	if e.UpstreamStatus != http.StatusBadRequest {
		t.Errorf("upstream status = %d，期望 400", e.UpstreamStatus)
	}
}

// TestResponsesDoorFallsBackToNestedEnvelope 防的是把两种形状写成二选一：
// 官方文档里嵌套信封同样出现过，取不到扁平字段时必须回落。
func TestResponsesDoorFallsBackToNestedEnvelope(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
	})

	_, err := call(t, srv, responsesInput(`{"model":"m","input":"hi"}`))
	e := asCanonical(t, err)

	if e.Class != canonical.ClassRateLimit {
		t.Errorf("分类 = %s，期望 rate_limit", e.Class)
	}
	if e.Message != "slow down" {
		t.Errorf("message = %q，期望 slow down", e.Message)
	}
}

// TestResponsesDoorNonJSONErrorUsesStatus 防的是 5xx 的 HTML 页面让解码崩掉：
// 此时状态码是唯一可靠的信息。
func TestResponsesDoorNonJSONErrorUsesStatus(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `<html>bad gateway</html>`)
	})

	_, err := call(t, srv, responsesInput(`{"model":"m","input":"hi"}`))
	e := asCanonical(t, err)

	if e.Class != canonical.ClassUpstreamUnavailable {
		t.Errorf("分类 = %s，期望 upstream_unavailable", e.Class)
	}
}

// TestResponsesDoorFlatAuthErrorIsRetryable 防的是把所有扁平错误都当参数错误：
// 鉴权失败换一把凭据可能成功，必须保持可重试。
func TestResponsesDoorFlatAuthErrorIsRetryable(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"request_id":"req-2","code":"InvalidApiKey","message":"Invalid API-key provided."}`)
	})

	_, err := call(t, srv, responsesInput(`{"model":"m","input":"hi"}`))
	e := asCanonical(t, err)

	if e.Class != canonical.ClassAuth || !e.Retryable {
		t.Errorf("分类 = %s retryable = %v，期望 auth 且可重试", e.Class, e.Retryable)
	}
}
