package passthrough

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

var refTime = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

// captured 记录上游实际收到了什么。
type captured struct {
	body   []byte
	header http.Header
	path   string
}

func serve(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.body, _ = io.ReadAll(r.Body)
		got.header = r.Header.Clone()
		got.path = r.URL.Path
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func call(t *testing.T, srv *httptest.Server, raw string, upstreamModel string, stream bool) (*httpx.Response, error) {
	t.Helper()

	p := New(degrade.ProviderOpenAICompat, "/v1/responses",
		httpx.New(config.Default().Timeouts, func() time.Time { return refTime }),
		func() time.Time { return refTime })

	return p.Call(context.Background(), provider.Request{
		Target: router.Target{
			Kind:           degrade.ProviderOpenAICompat,
			Endpoint:       "test",
			BaseURL:        srv.URL,
			UpstreamModel:  upstreamModel,
			CredentialPool: "test",
		},
		Credential: credential.Credential{ID: "k1", Secret: "sk-gateway-own-key"},
		Raw:        []byte(raw),
		Stream:     stream,
	})
}

// TestUnknownFieldsSurvive 是这个包存在的理由。
//
// 直通保住的不只是 TTFT，还有我们**没有建模**的字段：一个上游刚发布、网关还
// 不认识的新参数，走这条路能正常工作。用结构体往返一次，那些字段就悄悄消失了
// ——而客户端不会收到任何提示。
func TestUnknownFieldsSurvive(t *testing.T) {
	srv, got := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"resp_1"}`)
	})

	raw := `{"model":"logical","input":"hi","brand_new_param":{"nested":[1,2,3]},"another":true}`
	resp, err := call(t, srv, raw, "gpt-5-upstream", false)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got.body, &fields); err != nil {
		t.Fatal(err)
	}

	if _, ok := fields["brand_new_param"]; !ok {
		t.Error("网关不认识的字段被吞掉了——直通的意义就没了")
	}
	if string(fields["brand_new_param"]) != `{"nested":[1,2,3]}` {
		t.Errorf("未知字段的原始字节被改动了: %s", fields["brand_new_param"])
	}
	if string(fields["another"]) != "true" {
		t.Errorf("未知字段丢失: %s", got.body)
	}
}

func TestModelIsRewritten(t *testing.T) {
	srv, got := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})

	if _, err := call(t, srv, `{"model":"fast","input":"hi"}`, "qwen-turbo", false); err != nil {
		t.Fatal(err)
	}

	var fields map[string]any
	if err := json.Unmarshal(got.body, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["model"] != "qwen-turbo" {
		t.Errorf("模型名 = %v, 期望 qwen-turbo", fields["model"])
	}
	if fields["input"] != "hi" {
		t.Errorf("其余字段应保持不变，实际 %v", fields)
	}
}

// TestIdenticalModelIsByteExact 覆盖真正的零改动转发。
// 逻辑名与上游名相同时连重新序列化都省掉。
func TestIdenticalModelIsByteExact(t *testing.T) {
	srv, got := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})

	raw := `{"model":"gpt-5","input":"hi","z_last":1,"a_first":2}`
	if _, err := call(t, srv, raw, "gpt-5", false); err != nil {
		t.Fatal(err)
	}

	if string(got.body) != raw {
		t.Errorf("同名时应逐字节转发，实际:\n got %s\nwant %s", got.body, raw)
	}
}

// TestClientAuthorizationIsNotForwarded 是一条安全约束。
//
// 网关用自己的凭据。把客户端发来的 Authorization 转给上游，等于把网关的
// API Key 泄露给一个没有理由知道它的第三方。
func TestClientAuthorizationIsNotForwarded(t *testing.T) {
	srv, got := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})

	if _, err := call(t, srv, `{"model":"m","input":"hi"}`, "m", false); err != nil {
		t.Fatal(err)
	}

	auth := got.header.Get("Authorization")
	if auth != "Bearer sk-gateway-own-key" {
		t.Errorf("上游应收到网关自己的凭据，实际 %q", auth)
	}
	if strings.Contains(auth, "client") {
		t.Error("客户端凭据被转发给了上游")
	}
}

func TestAcceptHeaderMatchesStreaming(t *testing.T) {
	for _, tc := range []struct {
		stream bool
		want   string
	}{
		{true, "text/event-stream"},
		{false, "application/json"},
	} {
		srv, got := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{}`)
		})
		if _, err := call(t, srv, `{"model":"m","input":"hi"}`, "m", tc.stream); err != nil {
			t.Fatal(err)
		}
		if a := got.header.Get("Accept"); a != tc.want {
			t.Errorf("stream=%v 时 Accept = %q, 期望 %q", tc.stream, a, tc.want)
		}
	}
}

func TestPathIsAppendedToBaseURL(t *testing.T) {
	srv, got := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})

	if _, err := call(t, srv, `{"model":"m","input":"hi"}`, "m", false); err != nil {
		t.Fatal(err)
	}
	if got.path != "/v1/responses" {
		t.Errorf("上游路径 = %q, 期望 /v1/responses", got.path)
	}
}

// TestUpstreamErrorBecomesCanonical 验证错误经 openaiwire 解码。
func TestUpstreamErrorBecomesCanonical(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
		_, _ = io.WriteString(w,
			`{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	})

	_, err := call(t, srv, `{"model":"m","input":"hi"}`, "m", false)
	if err == nil {
		t.Fatal("上游 429 应当返回错误")
	}

	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际 %T", err)
	}
	if cerr.Class != canonical.ClassRateLimit {
		t.Errorf("分类 = %q, 期望 rate_limit", cerr.Class)
	}
	// Retry-After 必须活着传到凭据池——它决定冷却多久。
	if cerr.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, 期望 30s", cerr.RetryAfter)
	}
	if !cerr.Retryable {
		t.Error("限流应可重试（换一份凭据可能成功）")
	}
}

// TestOversizedErrorBodyIsCapped 固化「读错误体也要设上限」。
// 一个故障上游可能在 500 里塞进一整个 HTML 页面，甚至更糟。
func TestOversizedErrorBodyIsCapped(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		// 1 MiB 的垃圾。
		_, _ = io.WriteString(w, strings.Repeat("x", 1<<20))
	})

	_, err := call(t, srv, `{"model":"m","input":"hi"}`, "m", false)
	if err == nil {
		t.Fatal("上游 500 应当返回错误")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际 %T", err)
	}
	// 无法解析的体不该让分类退化——状态码本身就是可靠信号。
	if cerr.Class != canonical.ClassUpstreamUnavailable {
		t.Errorf("分类 = %q, 期望 upstream_unavailable", cerr.Class)
	}
	if len(cerr.Message) > 128<<10 {
		t.Errorf("错误消息长达 %d 字节，读取上限没有生效", len(cerr.Message))
	}
}

// TestStreamingResponsePassesThrough 验证流式响应原样转发。
func TestStreamingResponsePassesThrough(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		for _, chunk := range []string{
			"event: response.created\ndata: {\"id\":\"resp_1\"}\n\n",
			"event: response.output_text.delta\ndata: {\"delta\":\"你\"}\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, chunk)
			f.Flush()
		}
	})

	resp, err := call(t, srv, `{"model":"m","input":"hi","stream":true}`, "m", true)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"response.created", "你", "[DONE]"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("流式响应缺少 %q:\n%s", want, body)
		}
	}
}

func TestMissingModelIsRejected(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {})

	_, err := call(t, srv, `{"input":"hi"}`, "m", false)
	if err == nil {
		t.Fatal("缺少 model 的请求体应当被拒绝")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) || cerr.Class != canonical.ClassBadRequest {
		t.Errorf("应为 bad_request，实际 %v", err)
	}
}

func TestNonObjectBodyIsRejected(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {})

	if _, err := call(t, srv, `["not","an","object"]`, "m", false); err == nil {
		t.Fatal("非 JSON 对象的请求体应当被拒绝")
	}
}

// TestDashScopeTenantHeadersForwarded 固化同源直通要保住租户边界头：
// X-DashScope-WorkSpace 必须原样带给上游，否则请求会落到错误的子租户；
// 而客户端的 Authorization 必须被网关自己的凭据覆盖，绝不透传。
func TestDashScopeTenantHeadersForwarded(t *testing.T) {
	srv, got := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"output":{}}`)
	})

	p := New(degrade.ProviderDashScopeNative, "/api/v1/services/aigc/text-generation/generation",
		httpx.New(config.Default().Timeouts, nil), nil)

	clientHeader := http.Header{}
	clientHeader.Set("X-DashScope-WorkSpace", "ws-tenant-1")
	clientHeader.Set("X-DashScope-DataInspection", "enable")
	clientHeader.Set("Authorization", "Bearer client-secret-must-not-leak")
	clientHeader.Set("X-Random-Client-Header", "should-not-forward")

	resp, err := p.Call(context.Background(), provider.Request{
		Target: router.Target{
			Kind:           degrade.ProviderDashScopeNative,
			Endpoint:       "test",
			BaseURL:        srv.URL,
			UpstreamModel:  "qwen-turbo",
			CredentialPool: "test",
		},
		Credential: credential.Credential{ID: "k1", Secret: "sk-gateway-own"},
		Raw:        []byte(`{"model":"m","input":{"messages":[{"role":"user","content":"x"}]}}`),
		Header:     clientHeader,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got.header.Get("X-DashScope-WorkSpace") != "ws-tenant-1" {
		t.Errorf("租户头未透传: %q", got.header.Get("X-DashScope-WorkSpace"))
	}
	if got.header.Get("X-DashScope-DataInspection") != "enable" {
		t.Errorf("DataInspection 头未透传: %q", got.header.Get("X-DashScope-DataInspection"))
	}
	if auth := got.header.Get("Authorization"); auth != "Bearer sk-gateway-own" {
		t.Errorf("Authorization 应为网关凭据，实际 %q（客户端密钥泄露风险）", auth)
	}
	if got.header.Get("X-Random-Client-Header") != "" {
		t.Error("白名单之外的客户端头不应透传给上游")
	}
}

// TestDashScopeStreamingSetsSSEHeader 固化流式信号要替客户端带给上游。
func TestDashScopeStreamingSetsSSEHeader(t *testing.T) {
	srv, got := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"output":{}}`)
	})

	p := New(degrade.ProviderDashScopeNative, "/api/v1/services/aigc/text-generation/generation",
		httpx.New(config.Default().Timeouts, nil), nil)

	resp, err := p.Call(context.Background(), provider.Request{
		Target: router.Target{
			Kind:           degrade.ProviderDashScopeNative,
			Endpoint:       "test",
			BaseURL:        srv.URL,
			UpstreamModel:  "qwen-turbo",
			CredentialPool: "test",
		},
		Credential: credential.Credential{ID: "k1", Secret: "sk-x"},
		Raw:        []byte(`{"model":"m","input":{"messages":[{"role":"user","content":"x"}]}}`),
		Stream:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got.header.Get("X-DashScope-SSE") != "enable" {
		t.Errorf("流式请求应带 X-DashScope-SSE: enable，实际 %q", got.header.Get("X-DashScope-SSE"))
	}
}

// TestRequestPathOverridesDefault 固化「上游路径随请求走」：同一个 openai.compat
// 适配器既要打 Responses 端点也要打 Chat 端点，路径不能写死在装配时。
func TestRequestPathOverridesDefault(t *testing.T) {
	srv, got := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1"}`)
	})

	p := New(degrade.ProviderOpenAICompat, "/v1/responses",
		httpx.New(config.Default().Timeouts, nil), nil)

	resp, err := p.Call(context.Background(), provider.Request{
		Target: router.Target{
			Kind:           degrade.ProviderOpenAICompat,
			Endpoint:       "test",
			BaseURL:        srv.URL,
			UpstreamModel:  "gpt-4o",
			CredentialPool: "test",
		},
		Credential: credential.Credential{ID: "k1", Secret: "sk-x"},
		Raw:        []byte(`{"model":"logical","messages":[{"role":"user","content":"hi"}]}`),
		Path:       "/v1/chat/completions",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got.path != "/v1/chat/completions" {
		t.Errorf("上游路径 = %q, 期望 /v1/chat/completions", got.path)
	}
}

// TestDefaultPathUsedWhenRequestPathEmpty 保证未携带路径时退回装配默认，
// 既有单上游装配不受影响。
func TestDefaultPathUsedWhenRequestPathEmpty(t *testing.T) {
	srv, got := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"resp_1"}`)
	})

	if _, err := call(t, srv, `{"model":"logical","input":"hi"}`, "m", false); err != nil {
		t.Fatal(err)
	}
	if got.path != "/v1/responses" {
		t.Errorf("上游路径 = %q, 期望默认的 /v1/responses", got.path)
	}
}
