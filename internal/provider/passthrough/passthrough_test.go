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

// TestOversizedErrorBodyIsCapped 固化「垃圾错误体不让分类退化」。
//
// 一个故障上游可能在 500 里塞进一整个 HTML 页面，甚至更糟。适配器必须能
// 读完就走，且分类仍按状态码走 upstream_unavailable——状态码本身就是可靠
// 信号。不在这里断言 Message 长度：不可解析的体回退成 21 字节的
// "Internal Server Error"，长度断言无论有没有读取上限都会通过，一直在空转。
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
	if cerr.Class != canonical.ClassUpstreamUnavailable {
		t.Errorf("分类 = %q, 期望 upstream_unavailable", cerr.Class)
	}
	if cerr.UpstreamStatus != 500 {
		t.Errorf("UpstreamStatus = %d, 期望 500", cerr.UpstreamStatus)
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

// fillingReader 模拟无限填充缓冲区的 ReadCloser，并累计读取字节数。
// 读满 1 MiB 后返回 io.ErrUnexpectedEOF，确保无上限实现会触发失败而非死循环挂住。
type fillingReader struct {
	read int
}

func (r *fillingReader) Read(p []byte) (int, error) {
	if r.read >= 1<<20 {
		return 0, io.ErrUnexpectedEOF
	}
	for i := range p {
		p[i] = 'x'
	}
	n := len(p)
	r.read += n
	return n, nil
}

func (r *fillingReader) Close() error {
	return nil
}

// TestDecodeErrorCapsReadVolumeAt64KiB 验证 decodeError 在处理错误响应体时，
// 读入字节量被严格限制在 64 KiB 上限（65536 字节）。
//
// 契约关注点在于**底层的读取字节数**（body.read），而非最终解码出的错误消息长度——
// 上游故障时可能返回巨大响应体（如几兆的 HTML 错误页），网关必须在 transport 读
// 阶段掐断读取以防资源耗尽。测试中 p.client 为 nil 是因为 decodeError 仅依赖 p.now()
// 构造错误时间戳，不需要网络客户端。
func TestDecodeErrorCapsReadVolumeAt64KiB(t *testing.T) {
	body := &fillingReader{}
	p := New(degrade.ProviderOpenAICompat, "/v1/responses", nil, func() time.Time { return refTime })

	resp := &httpx.Response{
		Response: &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     make(http.Header),
			Body:       body,
		},
	}

	err := p.decodeError(resp)
	if err == nil {
		t.Fatal("期望 decodeError 返回非 nil 错误")
	}

	const wantBytes = 64 << 10
	if body.read != wantBytes {
		t.Errorf("读取字节数 = %d，期望精确等于 %d (64 KiB)", body.read, wantBytes)
	}
}
