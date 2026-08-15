package dashscopecompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

var refTime = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

// captured 记录上游实际收到了什么。
type captured struct {
	body   []byte
	header http.Header
	method string
	path   string
}

func serve(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.body, _ = io.ReadAll(r.Body)
		got.header = r.Header.Clone()
		got.method = r.Method
		got.path = r.URL.Path
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// okServer 是最常见的桩：收下请求，回一个空的 2xx JSON。
func okServer(t *testing.T) (*httptest.Server, *captured) {
	t.Helper()
	return serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
}

func call(t *testing.T, srv *httptest.Server, raw, upstreamModel string, stream bool) (*httpx.Response, error) {
	t.Helper()

	p := New(httpx.New(config.Default().Timeouts, func() time.Time { return refTime }),
		func() time.Time { return refTime })

	return p.Call(context.Background(), provider.Request{
		Target: router.Target{
			Kind:           degrade.ProviderDashScopeCompatible,
			Endpoint:       "test",
			BaseURL:        srv.URL,
			UpstreamModel:  upstreamModel,
			CredentialPool: "test",
		},
		Credential: credential.Credential{ID: "k1", Secret: "sk-gateway-own-key"},
		Raw:        []byte(raw),
		Stream:     stream,
		Path:       ChatCompletionsPath,
	})
}

// upstreamFields 把上游实际收到的请求体解成字段表，供语义比对。
//
// 按语义比对而不是按字节：重新序列化会改变键序，钉死字节等于钉死
// encoding/json 的实现细节，而那不是我们要保证的契约。
func upstreamFields(t *testing.T, got *captured) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got.body, &fields); err != nil {
		t.Fatalf("上游收到的不是 JSON 对象: %s", got.body)
	}
	return fields
}

func TestKindIsDashScopeCompatible(t *testing.T) {
	p := New(httpx.New(config.Default().Timeouts, nil), nil)
	if p.Kind() != degrade.ProviderDashScopeCompatible {
		t.Errorf("Kind() = %q，期望 %q", p.Kind(), degrade.ProviderDashScopeCompatible)
	}
}

// TestRequestShape 钉死上游请求的协议事实：method、path、网关凭据、模型改写、
// Accept 随流式与否。
//
// 凭据这一项是安全项：转发客户端的 Authorization 等于把客户端的密钥泄给上游，
// 而上游没有理由知道它；反过来上游也无从用它计费。
func TestRequestShape(t *testing.T) {
	for _, tc := range []struct {
		stream     bool
		wantAccept string
	}{
		{false, "application/json"},
		{true, "text/event-stream"},
	} {
		srv, got := okServer(t)

		resp, err := call(t, srv,
			`{"model":"logical","messages":[{"role":"user","content":"hi"}]}`,
			"qwen-plus", tc.stream)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		if got.method != http.MethodPost {
			t.Errorf("method = %q，期望 POST", got.method)
		}
		if got.path != ChatCompletionsPath {
			t.Errorf("path = %q，期望 %q", got.path, ChatCompletionsPath)
		}
		if auth := got.header.Get("Authorization"); auth != "Bearer sk-gateway-own-key" {
			t.Errorf("Authorization = %q，期望网关自己的凭据", auth)
		}
		if ct := got.header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		if a := got.header.Get("Accept"); a != tc.wantAccept {
			t.Errorf("stream=%v 时 Accept = %q，期望 %q", tc.stream, a, tc.wantAccept)
		}

		fields := upstreamFields(t, got)
		if string(fields["model"]) != `"qwen-plus"` {
			t.Errorf("model = %s，期望 qwen-plus", fields["model"])
		}
	}
}

// TestDefaultPathWhenRequestPathEmpty：请求没带路径时退回本适配器唯一的端点，
// 而不是打到上游根地址。
func TestDefaultPathWhenRequestPathEmpty(t *testing.T) {
	srv, got := okServer(t)

	p := New(httpx.New(config.Default().Timeouts, func() time.Time { return refTime }),
		func() time.Time { return refTime })
	resp, err := p.Call(context.Background(), provider.Request{
		Target: router.Target{
			Kind:           degrade.ProviderDashScopeCompatible,
			Endpoint:       "test",
			BaseURL:        srv.URL + "/",
			UpstreamModel:  "m",
			CredentialPool: "test",
		},
		Credential: credential.Credential{ID: "k1", Secret: "sk-gateway-own-key"},
		Raw:        []byte(`{"model":"m","messages":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got.path != ChatCompletionsPath {
		t.Errorf("path = %q，期望缺省退回 %q", got.path, ChatCompletionsPath)
	}
}

// TestUpstreamErrorDecoded：非 2xx 按 OpenAI 信封解码，Retry-After 保留。
// DashScope Compatible 的错误信封与 OpenAI 同形。
func TestUpstreamErrorDecoded(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)
	})

	_, err := call(t, srv,
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "m", false)
	if err == nil {
		t.Fatal("非 2xx 应当返回错误")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassRateLimit {
		t.Errorf("分类 = %q，期望 rate_limit", cerr.Class)
	}
	if !cerr.Retryable {
		t.Error("429 应可重试（换凭据可能成功）")
	}
	if cerr.RetryAfter != 7*time.Second {
		t.Errorf("Retry-After = %v，期望 7s", cerr.RetryAfter)
	}
	if cerr.UpstreamStatus != http.StatusTooManyRequests {
		t.Errorf("UpstreamStatus = %d，期望 429", cerr.UpstreamStatus)
	}
}

// TestMissingModelRejected：请求体缺 model 是入站解码就该拦下的，
// 适配器这里只做兜底，不能 panic。
func TestMissingModelRejected(t *testing.T) {
	srv, _ := okServer(t)

	_, err := call(t, srv, `{"messages":[{"role":"user","content":"hi"}]}`, "m", false)
	assertClass(t, err, canonical.ClassBadRequest)
}

// TestInvalidJSONRejected：请求体不是 JSON 对象时必须在适配器边界拦下，
// 而不是把一段垃圾原样打给上游再让上游给出一条与网关无关的错。
func TestInvalidJSONRejected(t *testing.T) {
	srv, _ := okServer(t)

	for _, raw := range []string{`not json at all`, `[1,2,3]`, `"a string"`} {
		_, err := call(t, srv, raw, "m", false)
		assertClass(t, err, canonical.ClassBadRequest)
	}
}

// TestMissingUpstreamModelIsInternal：路由目标缺上游模型名是网关自己的装配
// 错误，不是客户端的错，分类必须是 internal——否则会误导客户端去改请求。
func TestMissingUpstreamModelIsInternal(t *testing.T) {
	srv, _ := okServer(t)

	_, err := call(t, srv, `{"model":"m","messages":[]}`, "", false)
	assertClass(t, err, canonical.ClassInternal)
}

func assertClass(t *testing.T, err error, want canonical.ErrorClass) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望 %q 错误，实际为 nil", want)
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T: %v", err, err)
	}
	if cerr.Class != want {
		t.Errorf("分类 = %q，期望 %q", cerr.Class, want)
	}
}
