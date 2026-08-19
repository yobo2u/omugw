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

// callInput 是一次适配器调用的测试输入，是 provider.Request 在测试侧的投影。
//
// 收成一个具名类型而不是继续加形参：调用点写 `call(t, srv, "raw", "m", false)`
// 时，读的人无从判断末尾那个 false 是 stream 还是别的开关，加第六项时更是要
// 逐个调用点数位置。零值即缺省——path 留空验证缺省退回，baseURL 留空用测试服务器。
type callInput struct {
	raw           string
	upstreamModel string
	stream        bool

	path    string
	baseURL string

	// header 是客户端原始请求头。适配器不得把它们转给上游。
	header http.Header
}

// call 发起一次适配器调用，成功时把响应体的关闭登记进 t.Cleanup。
//
// 在这里登记而不是让每个调用点自己关：httpx 的 Body 上挂着空闲计时器与整体
// 超时的 cancel，漏关会把它们一起漏掉；而且 httptest.Server.Close 会等未完成的
// 连接，漏关能让收尾挂住。t.Cleanup 是 LIFO，serve 先登记 srv.Close，
// 这里后登记 Body.Close，于是先关体、后关服务器——顺序正是要的那个。
func call(t *testing.T, srv *httptest.Server, in callInput) (*httpx.Response, error) {
	t.Helper()

	p := New(httpx.New(config.Default().Timeouts, func() time.Time { return refTime }),
		func() time.Time { return refTime })

	baseURL := in.baseURL
	if baseURL == "" {
		baseURL = srv.URL
	}

	resp, err := p.Call(context.Background(), provider.Request{
		Target: router.Target{
			Kind:           degrade.ProviderDashScopeCompatible,
			Endpoint:       "test",
			BaseURL:        baseURL,
			UpstreamModel:  in.upstreamModel,
			CredentialPool: "test",
		},
		Credential: credential.Credential{ID: "k1", Secret: "sk-gateway-own-key"},
		Raw:        []byte(in.raw),
		Stream:     in.stream,
		Path:       in.path,
		Header:     in.header,
	})
	if err == nil && resp != nil {
		t.Cleanup(func() { resp.Body.Close() })
	}
	return resp, err
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
// Accept 随流式与否，以及客户端头一概不转发。
//
// 头这两项是安全项：客户端发来的 Authorization 转给上游等于泄露客户端密钥，
// 而上游没有理由知道它；转发任意自定义头则等于给客户端开了一条直通上游的
// 隧道，能绕过网关去操纵上游的租户、审查、异步等行为。所以这里刻意让客户端
// 带上一个假凭据与一个自定义头，再断言上游两样都没收到。
func TestRequestShape(t *testing.T) {
	for _, tc := range []struct {
		stream     bool
		wantAccept string
	}{
		{false, "application/json"},
		{true, "text/event-stream"},
	} {
		srv, got := okServer(t)

		clientHeader := http.Header{}
		clientHeader.Set("Authorization", "Bearer sk-client-must-not-leak")
		clientHeader.Set("X-Client-Custom", "must-not-be-forwarded")
		clientHeader.Set("X-DashScope-WorkSpace", "ws-must-not-be-forwarded")

		if _, err := call(t, srv, callInput{
			raw:           `{"model":"logical","messages":[{"role":"user","content":"hi"}]}`,
			upstreamModel: "qwen-plus",
			stream:        tc.stream,
			path:          ChatCompletionsPath,
			header:        clientHeader,
		}); err != nil {
			t.Fatal(err)
		}

		if got.method != http.MethodPost {
			t.Errorf("method = %q，期望 POST", got.method)
		}
		if got.path != ChatCompletionsPath {
			t.Errorf("path = %q，期望 %q", got.path, ChatCompletionsPath)
		}
		if auth := got.header.Get("Authorization"); auth != "Bearer sk-gateway-own-key" {
			t.Errorf("Authorization = %q，期望网关自己的凭据而非客户端的", auth)
		}
		for _, name := range []string{"X-Client-Custom", "X-DashScope-WorkSpace"} {
			if v := got.header.Get(name); v != "" {
				t.Errorf("客户端头 %s 被转发给上游了: %q", name, v)
			}
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

	if _, err := call(t, srv, callInput{
		raw:           `{"model":"m","messages":[]}`,
		upstreamModel: "m",
		baseURL:       srv.URL + "/",
	}); err != nil {
		t.Fatal(err)
	}

	if got.path != ChatCompletionsPath {
		t.Errorf("path = %q，期望缺省退回 %q", got.path, ChatCompletionsPath)
	}
}

// TestOfficialBaseURLDoesNotRepeatVersion：官方 base_url 已经包含
// /compatible-mode/v1，适配器不得再把入站路径开头的 /v1 重复拼进去。
func TestOfficialBaseURLDoesNotRepeatVersion(t *testing.T) {
	srv, got := okServer(t)

	if _, err := call(t, srv, callInput{
		raw:           `{"model":"m","messages":[]}`,
		upstreamModel: "m",
		baseURL:       srv.URL + "/compatible-mode/v1",
		path:          ChatCompletionsPath,
	}); err != nil {
		t.Fatal(err)
	}

	want := "/compatible-mode/v1/chat/completions"
	if got.path != want {
		t.Errorf("path = %q，期望官方 base_url 只保留一个版本段 %q", got.path, want)
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

	_, err := call(t, srv, callInput{
		raw:           `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		upstreamModel: "m",
		path:          ChatCompletionsPath,
	})
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

	_, err := call(t, srv, callInput{
		raw:           `{"messages":[{"role":"user","content":"hi"}]}`,
		upstreamModel: "m",
		path:          ChatCompletionsPath,
	})
	assertClass(t, err, canonical.ClassBadRequest)
}

// TestInvalidJSONRejected：请求体不是 JSON 对象时必须在适配器边界拦下，
// 而不是把一段垃圾原样打给上游再让上游给出一条与网关无关的错。
func TestInvalidJSONRejected(t *testing.T) {
	srv, _ := okServer(t)

	for _, raw := range []string{`not json at all`, `[1,2,3]`, `"a string"`} {
		_, err := call(t, srv, callInput{
			raw:           raw,
			upstreamModel: "m",
			path:          ChatCompletionsPath,
		})
		assertClass(t, err, canonical.ClassBadRequest)
	}
}

// TestMissingUpstreamModelIsInternal：路由目标缺上游模型名是网关自己的装配
// 错误，不是客户端的错，分类必须是 internal——否则会误导客户端去改请求。
func TestMissingUpstreamModelIsInternal(t *testing.T) {
	srv, _ := okServer(t)

	_, err := call(t, srv, callInput{
		raw:  `{"model":"m","messages":[]}`,
		path: ChatCompletionsPath,
	})
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
