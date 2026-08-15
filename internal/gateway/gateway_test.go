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

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/protocol/dashscopenative"
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
//
// 已实现的路径用 openai.compat（已转正）；要测 PLANNED 行为就把目标指到一条
// 仍未实现的路径上。不去「取消转正」——那需要给生产代码开一个只为测试存在的
// 后门，而后门迟早会被当成正常用法。
func newHarness(t *testing.T, implemented bool, ups ...*upstream) *harness {
	t.Helper()
	kind := degrade.ProviderOpenAICompat
	if !implemented {
		kind = degrade.ProviderDashScopeCompatible
	}
	return newHarnessFor(t, "/v1/responses", "/v1/responses", kind, NewResponsesHandler,
		config.Default().Limits, ups...)
}

// newChatHarness 是 Chat Completions 入站的 harness。
//
// provider 的默认路径故意用生产里 openai.compat 的默认值 "/v1/responses"，
// 而不是 Chat 自己的路径——这样只有当 handler 真的把 provider.Request.Path
// 注入成 "/v1/chat/completions" 时，请求才会打到正确的上游端点。若把两者设成
// 一样，即使 Path 注入被删掉测试也照样通过，等于没测。
func newChatHarness(t *testing.T, implemented bool, ups ...*upstream) *harness {
	t.Helper()
	kind := degrade.ProviderOpenAICompat
	if !implemented {
		kind = degrade.ProviderDashScopeCompatible
	}
	return newHarnessFor(t, "/v1/chat/completions", "/v1/responses", kind, NewChatHandler,
		config.Default().Limits, ups...)
}

// newDashScopeNativeHarness 是 DashScope Native 入站的 harness。
//
// provider 默认路径故意用 /v1/responses（而非 DashScope 自己的端点），这样只有
// 当 handler 真的注入了请求路径，上游才会收到正确端点——否则测试会失败。
func newDashScopeNativeHarness(t *testing.T, implemented bool, ups ...*upstream) *harness {
	t.Helper()
	kind := degrade.ProviderDashScopeNative
	if !implemented {
		kind = degrade.ProviderDashScopeCompatible
	}
	return newHarnessFor(t, dashscopenative.TextGenerationPath, "/v1/responses",
		kind, NewDashScopeNativeHandler, config.Default().Limits, ups...)
}

// newDashScopeNativeHarnessWithLimits 与 newDashScopeNativeHarness 相同，
// 但注入自定义 Limits——内联闸门先于矩阵裁决生效的性质要用一个小到会被
// 击穿的上限来证明。不走 hs.h.deps.Limits 事后改写：那是绕过构造契约的后门，
// 而这里要证的恰恰是「按配置构造出来的网关」在闸门顺序上的行为。
func newDashScopeNativeHarnessWithLimits(t *testing.T, limits config.Limits, ups ...*upstream) *harness {
	t.Helper()
	return newHarnessFor(t, dashscopenative.TextGenerationPath, "/v1/responses",
		degrade.ProviderDashScopeNative, NewDashScopeNativeHandler, limits, ups...)
}

func newHarnessFor(t *testing.T, requestPath, providerDefaultPath string, kind degrade.Provider, mk func(Deps) *Handler, limits config.Limits, ups ...*upstream) *harness {
	t.Helper()

	m, err := degrade.Phase1()
	if err != nil {
		t.Fatal(err)
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
			Limits:    limits,
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

// doPath 按指定端点路径直接打 Handler。
//
// 走 Handler 而不是 Build 出来的 Mux 是刻意的：Mux 的 /api/v1/ 兜底会把任何
// 未注册端点一律答成 501，那条 501 出自路由表，与矩阵裁决无关。要证的是矩阵
// 按 (端点, 能力) 作答，就必须绕开兜底，让请求真的走完 serve 的闸门。
func doPath(t *testing.T, hs *harness, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
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

// TestDashScopeNativeErrorsUseFlatEnvelope 固化 DashScope Native 入站的错误必须用
// 它自己的扁平 {code,message} 信封，而不是 OpenAI 的嵌套 {"error":...}——
// 客户端按自己说的协议解析错误，给错信封它就读不懂。
func TestDashScopeNativeErrorsUseFlatEnvelope(t *testing.T) {
	hs := newDashScopeNativeHarness(t, true, jsonUpstream(t, `{}`))

	// 缺少 model → bad_request，经 fail() 用 dashscopewire 编码。
	rec := hs.do(t, `{"input":{"messages":[{"role":"user","content":"x"}]}}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400, body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code"`) || !strings.Contains(body, `"message"`) {
		t.Errorf("DashScope 错误应为扁平 {code,message} 信封: %s", body)
	}
	if strings.Contains(body, `"error"`) {
		t.Errorf("DashScope 错误不应是 OpenAI 嵌套 error 信封: %s", body)
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

func TestDashScopeNativeUpstreamErrorRoundtrip(t *testing.T) {
	ups := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":"InvalidParameter","message":"x","request_id":"ups-req-77"}`)
	})
	// 只配置一个上游（无备用），防止 400 触发 Provider 级 failover 从而掩盖了错误信封的往返测试。
	hs := newDashScopeNativeHarness(t, true, ups)

	rec := hs.do(t, `{"model":"logical","input":{"messages":[{"role":"user","content":"hi"}]}}`, true)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400, body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	var errObj map[string]any
	if err := json.Unmarshal([]byte(body), &errObj); err != nil {
		t.Fatalf("响应不是合法 JSON: %v, body: %s", err, body)
	}

	if errObj["code"] != "InvalidParameter" {
		t.Errorf("期望 code=InvalidParameter, 实际: %v", errObj["code"])
	}
	if errObj["message"] != "x" {
		t.Errorf("期望 message=x, 实际: %v", errObj["message"])
	}
	if errObj["request_id"] != "ups-req-77" {
		t.Errorf("期望 request_id=ups-req-77, 实际: %v", errObj["request_id"])
	}
	if _, ok := errObj["error"]; ok {
		t.Errorf("DashScope 错误不应是 OpenAI 嵌套 error 信封: %s", body)
	}
	if n := ups.calls.Load(); n != 1 {
		t.Errorf("上游应被调用 1 次，实际 %d 次", n)
	}
}

func TestDashScopeNativeStreamAbortFlatEnvelope(t *testing.T) {
	dying := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		_, _ = io.WriteString(w,
			"event: result\ndata: {\"output\":{\"text\":\"开头\"}}\n\n")
		f.Flush()
		// 之后一动不动，触发空闲超时。
		time.Sleep(2 * time.Second)
	})
	backup := jsonUpstream(t, `{"code":"MUST_NOT_APPEAR"}`)

	hs := newDashScopeNativeHarness(t, true, dying, backup)

	req := httptest.NewRequest(http.MethodPost, hs.path, strings.NewReader(`{"model":"logical","input":{"messages":[{"role":"user","content":"hi"}]},"parameters":{"result_format":"text"}}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-DashScope-SSE", "enable")
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "开头") {
		t.Error("已发出的内容应当保留")
	}
	if n := backup.calls.Load(); n != 0 {
		t.Errorf("首字节之后仍然 failover 了 %d 次", n)
	}
	if strings.Contains(body, "MUST_NOT_APPEAR") {
		t.Error("备用上游的内容被拼到了同一条流里")
	}

	// 必须有终止事件收尾，且 data 是扁平信封
	if !strings.Contains(body, "event: error") {
		t.Fatalf("流中断后应发出终止错误事件:\n%s", body)
	}

	events, err := testkit.ParseSSE(strings.NewReader(body))
	if err != nil {
		t.Fatalf("解析 SSE 失败: %v", err)
	}

	var errorData string
	for _, ev := range events {
		if ev.Event == "error" {
			errorData = ev.Data
			break
		}
	}
	if errorData == "" {
		t.Fatalf("未找到 event: error 对应的 data 行:\n%s", body)
	}

	var errObj map[string]any
	if err := json.Unmarshal([]byte(errorData), &errObj); err != nil {
		t.Fatalf("error data 不是合法 JSON: %v, data: %s", err, errorData)
	}

	if errObj["code"] != "ServiceUnavailable" {
		t.Errorf("error data 期望 code=ServiceUnavailable, 实际: %v", errObj["code"])
	}
	if msg, ok := errObj["message"].(string); !ok || msg == "" {
		t.Errorf("error data 期望非空 message, 实际: %v", errObj["message"])
	}
	if _, ok := errObj["error"]; ok {
		t.Errorf("error data 不应包含嵌套 error 字段: %s", errorData)
	}
}

// TestTextDoorRejectsVisionInput 固化「设计处置不等于投放承诺」。
//
// vision_input 在 dashscope.native 同源直通上的设计处置是 PASSTHROUGH，
// 但文本门没有它的 fixture 证据，就不该兑现。501 说「等实现」，
// 而请求必须在矩阵这一关就被挡住，一个字节都不出门。
func TestTextDoorRejectsVisionInput(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newDashScopeNativeHarness(t, true, up)

	rec := doPath(t, hs, dashscopenative.TextGenerationPath,
		`{"model":"m","input":{"messages":[{"role":"user","content":[{"text":"图里有什么？"},{"image":"https://example.com/x.png"}]}]}}`)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501（vision_input 未投放到文本门）: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——应当被矩阵拦下，没有出门", n)
	}
	if !strings.Contains(rec.Body.String(), string(canonical.CapVisionInput)) {
		t.Errorf("错误应点名能力: %s", rec.Body.String())
	}
}

// TestMultimodalDoorRejectsFileInput 固化多模态门不承接 file_input。
//
// 官方内容块词表是 text / image / audio / video，没有通用 file 块；
// 两扇门都不兑现它，运行时必须 501 而不是把请求丢给上游试运气。
func TestMultimodalDoorRejectsFileInput(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newDashScopeNativeHarness(t, true, up)

	rec := doPath(t, hs, dashscopenative.MultimodalGenerationPath,
		`{"model":"m","input":{"messages":[{"role":"user","content":[{"text":"处理这个文件"},{"file":"https://example.com/a.pdf"}]}]}}`)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501（file_input 两扇门都不兑现）: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——应当被矩阵拦下，没有出门", n)
	}
	if !strings.Contains(rec.Body.String(), string(canonical.CapFileInput)) {
		t.Errorf("错误应点名能力: %s", rec.Body.String())
	}
}

// TestMultimodalDoorRejectsReasoning 是「无证据不兑现」纪律的活体证明。
//
// enable_thinking 在文本门有 fixture 证据、已兑现；多模态门没有，
// 哪怕上游模型多半也支持，矩阵照样拦下——投放跟着证据走，不跟着猜测走。
func TestMultimodalDoorRejectsReasoning(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newDashScopeNativeHarness(t, true, up)

	rec := doPath(t, hs, dashscopenative.MultimodalGenerationPath,
		`{"model":"m","input":{"messages":[{"role":"user","content":[{"text":"想一想再回答"}]}]},"parameters":{"enable_thinking":true}}`)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501（reasoning 未投放到多模态门）: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——应当被矩阵拦下，没有出门", n)
	}
	if !strings.Contains(rec.Body.String(), string(canonical.CapReasoning)) {
		t.Errorf("错误应点名能力: %s", rec.Body.String())
	}
}

// TestInlineLimitBeatsMatrixOnMultimodalDoor 固化内联闸门先于矩阵裁决。
//
// 顺序反过来的话，一个塞满 base64 视频的请求要先跑完整套矩阵裁决才被拒，
// 内存早就吃进去了（原则 2.6）。400 说「改请求」，与 501 的「等实现」
// 是两件事，不能互相顶替。
func TestInlineLimitBeatsMatrixOnMultimodalDoor(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	limits := config.Default().Limits
	limits.MaxInlineBytes = 64
	hs := newDashScopeNativeHarnessWithLimits(t, limits, up)

	// 128 个 'A' 是合法 base64，解码后 96 字节，越过 64 字节上限。
	inline := strings.Repeat("A", 128)
	rec := doPath(t, hs, dashscopenative.MultimodalGenerationPath,
		`{"model":"m","input":{"messages":[{"role":"user","content":[{"text":"x"},{"image":"data:image/png;base64,`+inline+`"}]}]}}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（内联负载超限）: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——内联闸门应先于矩阵拦下请求", n)
	}
	if !strings.Contains(rec.Body.String(), "内联") {
		t.Errorf("错误应说明是内联负载超限: %s", rec.Body.String())
	}
}
