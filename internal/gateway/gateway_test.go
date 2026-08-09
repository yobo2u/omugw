package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/provider/passthrough"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

const testKey = "omugw-test-key-0123456789"

// upstream 是一个可编排的假上游。
type upstream struct {
	srv   *httptest.Server
	calls atomic.Int32
}

func newUpstream(t *testing.T, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		h(w, r)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// harness 组装一套可用的网关，targets 按顺序即 failover 顺序。
type harness struct {
	h       *Handler
	matrix  *degrade.Matrix
	metrics *obs.Metrics
	// path 是 do() 打入的入站路由，与所测入站协议一致。
	path string
}

// newHarness 是 Responses 入站的 harness。
func newHarness(t *testing.T, implemented bool, ups ...*upstream) *harness {
	t.Helper()
	return newHarnessFor(t, "/v1/responses", "/v1/responses", NewResponsesHandler, implemented, ups...)
}

// newChatHarness 是 Chat Completions 入站的 harness。
//
// provider 的默认路径故意用生产里 openai.compat 的默认值 "/v1/responses"，
// 而不是 Chat 自己的路径——这样只有当 handler 真的把 provider.Request.Path
// 注入成 "/v1/chat/completions" 时，请求才会打到正确的上游端点。若把两者设成
// 一样，即使 Path 注入被删掉测试也照样通过，等于没测。
func newChatHarness(t *testing.T, implemented bool, ups ...*upstream) *harness {
	t.Helper()
	return newHarnessFor(t, "/v1/chat/completions", "/v1/responses", NewChatHandler, implemented, ups...)
}

func newHarnessFor(t *testing.T, requestPath, providerDefaultPath string, mk func(Deps) *Handler, implemented bool, ups ...*upstream) *harness {
	t.Helper()

	m, err := degrade.Phase1()
	if err != nil {
		t.Fatal(err)
	}

	// 已实现的路径用 openai.compat（M1 已转正）；要测 PLANNED 行为就把目标
	// 指到一条仍未实现的路径上。不去「取消转正」——那需要给生产代码开一个
	// 只为测试存在的后门，而后门迟早会被当成正常用法。
	kind := degrade.ProviderOpenAICompat
	if !implemented {
		kind = degrade.ProviderDashScopeCompatible
	}

	timeouts := config.Timeouts{
		Connect:   200 * time.Millisecond,
		FirstByte: 1 * time.Second,
		Total:     10 * time.Second,
		Idle:      500 * time.Millisecond,
	}
	client := httpx.New(timeouts, nil)

	var targets []router.Target
	pools := map[string]*credential.Pool{}
	provs := map[string]provider.Provider{}

	for i, u := range ups {
		name := endpointName(i)
		targets = append(targets, router.Target{
			Kind:           kind,
			Endpoint:       name,
			BaseURL:        u.srv.URL,
			UpstreamModel:  "upstream-model",
			CredentialPool: name,
		})
		pool, err := credential.NewPool(name,
			[]credential.Credential{{ID: "k1", Secret: "sk-" + name}},
			credential.DefaultPolicy(), nil)
		if err != nil {
			t.Fatal(err)
		}
		pools[name] = pool
		provs[name] = passthrough.New(kind, providerDefaultPath, client, nil)
	}

	rt, err := router.New([]router.Rule{{Match: "*", Targets: targets}})
	if err != nil {
		t.Fatal(err)
	}

	metrics := obs.NewMetrics(prometheus.NewRegistry())
	return &harness{
		matrix:  m,
		metrics: metrics,
		path:    requestPath,
		h: mk(Deps{
			Matrix:    m,
			Router:    rt,
			Auth:      NewAuthenticator([]config.AuthKey{{ID: "tester", Key: testKey}}),
			Limits:    config.Default().Limits,
			Metrics:   metrics,
			Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			Pools:     pools,
			Providers: provs,
		}),
	}
}

func endpointName(i int) string { return string(rune('a' + i)) }

func (hs *harness) do(t *testing.T, body string, withAuth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, hs.path, strings.NewReader(body))
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+testKey)
	}
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	return rec
}

func jsonUpstream(t *testing.T, body string) *upstream {
	return newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
}

// TestPlannedRouteReturns501 固化 ADR-0001 的运行时表现。
//
// 501 而不是 422：前者告诉客户端「等」，后者告诉客户端「改请求」。
func TestPlannedRouteReturns501(t *testing.T) {
	hs := newHarness(t, false, jsonUpstream(t, `{}`))

	rec := hs.do(t, `{"model":"m","input":"hi"}`, true)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d, 期望 501", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "尚未实现") {
		t.Errorf("错误体应说明路径尚未实现: %s", rec.Body.String())
	}
}

func TestAuthIsEnforced(t *testing.T) {
	hs := newHarness(t, true, jsonUpstream(t, `{"id":"resp_1"}`))

	t.Run("缺少凭据", func(t *testing.T) {
		if rec := hs.do(t, `{"model":"m","input":"hi"}`, false); rec.Code != 401 {
			t.Errorf("状态码 = %d, 期望 401", rec.Code)
		}
	})

	t.Run("错误凭据", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses",
			strings.NewReader(`{"model":"m","input":"hi"}`))
		req.Header.Set("Authorization", "Bearer wrong-key-but-long-enough")
		rec := httptest.NewRecorder()
		hs.h.ServeHTTP(rec, req)

		if rec.Code != 401 {
			t.Errorf("状态码 = %d, 期望 401", rec.Code)
		}
		// 错误消息不得透露任何关于正确密钥的信息。
		if strings.Contains(rec.Body.String(), testKey) {
			t.Error("错误体泄露了正确的密钥")
		}
	})

	t.Run("Api-Key 头也接受", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses",
			strings.NewReader(`{"model":"m","input":"hi"}`))
		req.Header.Set("Api-Key", testKey)
		rec := httptest.NewRecorder()
		hs.h.ServeHTTP(rec, req)

		if rec.Code == 401 {
			t.Error("Azure 系客户端惯用的 Api-Key 头应当被接受")
		}
	})
}

func TestHappyPathNonStreaming(t *testing.T) {
	up := jsonUpstream(t, `{"id":"resp_1","usage":{"input_tokens":10,"output_tokens":5,
	  "output_tokens_details":{"reasoning_tokens":3}}}`)
	hs := newHarness(t, true, up)

	rec := hs.do(t, `{"model":"logical","input":"hi"}`, true)
	if rec.Code != 200 {
		t.Fatalf("状态码 = %d, 期望 200，响应体: %s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != "resp_1" {
		t.Errorf("响应体未原样转发: %v", got)
	}
}

func TestHappyPathStreaming(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, chunk := range []string{
			"event: response.created\ndata: {\"id\":\"resp_1\"}\n\n",
			"event: response.output_text.delta\ndata: {\"delta\":\"你好\"}\n\n",
			"event: response.completed\ndata: {\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":2}}}\n\n",
		} {
			_, _ = io.WriteString(w, chunk)
			f.Flush()
		}
	})
	hs := newHarness(t, true, up)

	rec := hs.do(t, `{"model":"logical","input":"hi","stream":true}`, true)
	if rec.Code != 200 {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}

	evs, err := testkit.ParseSSE(strings.NewReader(rec.Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("收到 %d 条事件，期望 3 条: %+v", len(evs), evs)
	}
	// data 负载逐字保留——只有帧的空白格式会被规范化。
	if evs[1].Data != `{"delta":"你好"}` {
		t.Errorf("data 负载被改动: %q", evs[1].Data)
	}
}

// TestFailoverBeforeFirstByte 覆盖 failover 允许的那一侧。
func TestFailoverBeforeFirstByte(t *testing.T) {
	bad := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"error":{"type":"api_error","message":"down"}}`)
	})
	good := jsonUpstream(t, `{"id":"resp_from_backup"}`)

	hs := newHarness(t, true, bad, good)

	rec := hs.do(t, `{"model":"logical","input":"hi"}`, true)
	if rec.Code != 200 {
		t.Fatalf("状态码 = %d, 期望 200（应已 failover 到备用上游）", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "resp_from_backup") {
		t.Errorf("未 failover 到备用上游: %s", rec.Body.String())
	}
	if good.calls.Load() != 1 {
		t.Errorf("备用上游被调用 %d 次，期望 1 次", good.calls.Load())
	}
}

// TestNoFailoverAfterFirstByte 是整个包最重要的一条测试（原则 2.4）。
//
// 上游在流到一半时断掉。此时客户端已经收到内容，重试会让它看到重复的字——
// 所以必须就此打住，发一个终止事件收尾，而**绝不能**去调备用上游。
func TestNoFailoverAfterFirstByte(t *testing.T) {
	dying := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		_, _ = io.WriteString(w,
			"event: response.output_text.delta\ndata: {\"delta\":\"开头\"}\n\n")
		f.Flush()
		// 之后一动不动，触发空闲超时。
		time.Sleep(2 * time.Second)
	})
	backup := jsonUpstream(t, `{"id":"MUST_NOT_APPEAR"}`)

	hs := newHarness(t, true, dying, backup)

	rec := hs.do(t, `{"model":"logical","input":"hi","stream":true}`, true)

	body := rec.Body.String()
	if !strings.Contains(body, "开头") {
		t.Error("已发出的内容应当保留")
	}
	// 关键断言：备用上游一次都不能被调用。
	if n := backup.calls.Load(); n != 0 {
		t.Errorf("首字节之后仍然 failover 了 %d 次——客户端会收到重复内容", n)
	}
	if strings.Contains(body, "MUST_NOT_APPEAR") {
		t.Error("备用上游的内容被拼到了同一条流里")
	}
	// 必须有终止事件收尾，否则客户端会一直等下去。
	if !strings.Contains(body, "event: error") {
		t.Errorf("流中断后应发出终止错误事件:\n%s", body)
	}
}

// TestCredentialFailoverWithinOneUpstream 覆盖同一上游内的换凭据重试。
func TestCredentialFailoverWithinOneUpstream(t *testing.T) {
	var n atomic.Int32
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"slow"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_second_key"}`)
	})

	hs := newHarness(t, true, up)
	// 换上两份凭据的池子。
	pool, err := credential.NewPool("a", []credential.Credential{
		{ID: "k1", Secret: "sk-1"},
		{ID: "k2", Secret: "sk-2"},
	}, credential.DefaultPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	hs.h.deps.Pools["a"] = pool

	rec := hs.do(t, `{"model":"logical","input":"hi"}`, true)
	if rec.Code != 200 {
		t.Fatalf("状态码 = %d, 期望 200（应已换第二份凭据）: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "resp_second_key") {
		t.Errorf("未换凭据重试: %s", rec.Body.String())
	}
}

// TestRateLimitHeadersReachClient 固化「header 是协议契约的一部分」。
func TestRateLimitHeadersReachClient(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Ratelimit-Remaining-Tokens", "1234")
		w.Header().Set("Set-Cookie", "upstream_session=leak")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r"}`)
	})
	hs := newHarness(t, true, up)

	rec := hs.do(t, `{"model":"logical","input":"hi"}`, true)

	if got := rec.Header().Get("X-Ratelimit-Remaining-Tokens"); got != "1234" {
		t.Errorf("限流额度头未透传: %q", got)
	}
	// 上游的 Set-Cookie 既无用又会泄露上游部署细节。
	if rec.Header().Get("Set-Cookie") != "" {
		t.Error("上游的 Set-Cookie 不该透给客户端")
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	hs := newHarness(t, true, jsonUpstream(t, `{}`))
	hs.h.deps.Limits.MaxRequestBytes = 128

	rec := hs.do(t, `{"model":"m","input":"`+strings.Repeat("x", 500)+`"}`, true)
	if rec.Code != 400 {
		t.Errorf("状态码 = %d, 期望 400", rec.Code)
	}
}

// TestOversizedInlinePayloadIsRejected 固化原则 2.6 的上限。
func TestOversizedInlinePayloadIsRejected(t *testing.T) {
	hs := newHarness(t, true, jsonUpstream(t, `{}`))
	hs.h.deps.Limits.MaxInlineBytes = 8

	// 40 个 'A' 是合法 base64，解码后 30 字节，超过 8 字节上限。
	//
	// 早先这里写的是 strings.Repeat("aGVsbG8=", 4)——中间夹着 padding，
	// 根本不是合法 base64，于是 decodeDataURI 失败、回退成 URL 形态、
	// 一个字节都不计。测试数据自己不合法，测的就不是它想测的东西。
	big := strings.Repeat("A", 40)
	body := `{"model":"m","input":[{"role":"user","content":[
	  {"type":"input_image","image_url":"data:image/png;base64,` + big + `"}]}]}`

	rec := hs.do(t, body, true)
	if rec.Code != 400 {
		t.Fatalf("状态码 = %d, 期望 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "内联") {
		t.Errorf("错误应说明是内联负载超限: %s", rec.Body.String())
	}
}

func TestUnknownModelIsRejected(t *testing.T) {
	hs := newHarness(t, true, jsonUpstream(t, `{}`))
	// 换成没有兜底规则的路由。
	rt, err := router.New([]router.Rule{{Match: "only-this", Targets: []router.Target{{
		Kind: degrade.ProviderOpenAICompat, Endpoint: "a", BaseURL: "http://x",
		UpstreamModel: "u", CredentialPool: "a",
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	hs.h.deps.Router = rt

	rec := hs.do(t, `{"model":"nope","input":"hi"}`, true)
	if rec.Code != 400 {
		t.Errorf("状态码 = %d, 期望 400", rec.Code)
	}
}
