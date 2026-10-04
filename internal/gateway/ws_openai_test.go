package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/provider"
	oaws "github.com/yobo2u/omugw/internal/provider/openairealtime"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 明列本次测试授权，防表达集合扩张后测试不经审阅便自动开门；不是生产兑现。
func openAITestCaps() []canonical.Capability {
	return []canonical.Capability{
		canonical.CapTextGeneration, canonical.CapStreaming, canonical.CapToolCalling,
		canonical.CapParallelToolCalls, canonical.CapReasoning, canonical.CapVisionInput,
		canonical.CapImageDetail, canonical.CapAudioInput, canonical.CapAudioOutput,
		canonical.CapSpeechSynthesis, canonical.CapSpeechRecognition, canonical.CapStatefulConversation,
		canonical.CapRealtimeSession, canonical.CapRealtimeServerVAD, canonical.CapRealtimeInterruptTurns,
	}
}

func openAITestMatrix(t *testing.T, caps []canonical.Capability, dashscope bool) *degrade.Matrix {
	t.Helper()
	base, err := degrade.Phase1()
	if err != nil {
		t.Fatal(err)
	}
	if dashscope {
		base = wsHandlerMatrix(t, true)
	}
	m := degrade.NewMatrix()
	for _, r := range base.Routes() {
		if r.InProtocol() == degrade.ProtoOpenAIRealtime && r.OutProvider() == degrade.ProviderOpenAIRealtime {
			err = m.Add(degrade.NewRoute(degrade.ProtoOpenAIRealtime, degrade.ProviderOpenAIRealtime).
				Pass(openAITestCaps()...).Redeem(degrade.EndpointOpenAIRealtime, caps...).MarkHomogeneous().Build())
		} else {
			err = m.Add(r, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func openAITestConfig(url string) config.Config {
	cfg := buildTestConfig(url)
	cfg.Providers[0].Kind = string(degrade.ProviderOpenAIRealtime)
	cfg.Models[0].Targets[0].UpstreamModel = "real-model"
	cfg.WebSocket = config.WebSocket{MaxMessageBytes: 1 << 20, MaxBufferedBytes: 8 << 20, MaxSessions: 4}
	return cfg
}

func openAITestDeps(t *testing.T, url string) WSDeps {
	t.Helper()
	d := wsHandlerDeps(t, url)
	d.Matrix = openAITestMatrix(t, openAITestCaps(), false)
	var err error
	d.Router, err = router.New([]router.Rule{{Match: "*", Targets: []router.Target{{Kind: degrade.ProviderOpenAIRealtime, Endpoint: "ep", BaseURL: url, UpstreamModel: "real-model", CredentialPool: "pool"}}}})
	if err != nil {
		t.Fatal(err)
	}
	d.Providers["ep"] = oaws.New(d.Timeouts, d.Limits, d.Budget)
	return d
}

func openAITestRequest() *http.Request {
	r := wsHandlerRequest()
	r.URL.Path = "/v1/realtime"
	return r
}

func TestBuildOpenAIRealtimePlanned(t *testing.T) {
	cfg := openAITestConfig("http://127.0.0.1:0")
	m, err := degrade.Phase1()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(cfg, m, obs.NewMetrics(prometheus.NewRegistry()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal("GA Provider 应可配置，但生产门仍关闭:", err)
	}
	for _, path := range []string{"/v1/realtime", "/api-ws/v1/realtime"} {
		w := httptest.NewRecorder()
		b.Mux.ServeHTTP(w, httptest.NewRequest("GET", path+"?model=real-model", nil))
		if w.Code != 404 {
			t.Fatalf("生产门 %s 意外注册: %d", path, w.Code)
		}
	}
	if err := b.ShutdownWebSockets(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBuildOpenAIRealtimeDoors(t *testing.T) {
	const oa, ds = degrade.EndpointOpenAIRealtime, degrade.EndpointDashScopeRealtime
	for _, tc := range []struct {
		name      string
		matrix    func() *degrade.Matrix
		doors     []degrade.Endpoint
		wantError bool
	}{
		{"GA", func() *degrade.Matrix { return openAITestMatrix(t, openAITestCaps(), false) }, []degrade.Endpoint{oa}, false},
		{"S1", func() *degrade.Matrix { return wsHandlerMatrix(t, true) }, []degrade.Endpoint{ds}, false},
		{"both", func() *degrade.Matrix { return openAITestMatrix(t, openAITestCaps(), true) }, []degrade.Endpoint{ds, oa}, false},
		{"planned", func() *degrade.Matrix {
			m, err := degrade.Phase1()
			if err != nil {
				t.Fatal(err)
			}
			return m
		}, []degrade.Endpoint{oa}, true},
		{"partial", func() *degrade.Matrix { return openAITestMatrix(t, openAITestCaps()[:12], false) }, []degrade.Endpoint{oa}, true},
		{"missing-GA", func() *degrade.Matrix { return openAITestMatrix(t, openAITestCaps(), true) }, []degrade.Endpoint{ds}, true},
		{"missing-S1", func() *degrade.Matrix { return openAITestMatrix(t, openAITestCaps(), true) }, []degrade.Endpoint{oa}, true},
		{"unredeemed-S1", func() *degrade.Matrix { return openAITestMatrix(t, openAITestCaps(), false) }, []degrade.Endpoint{oa, ds}, true},
		{"duplicate", func() *degrade.Matrix { return openAITestMatrix(t, openAITestCaps(), false) }, []degrade.Endpoint{oa, oa}, true},
		{"unknown", func() *degrade.Matrix { return openAITestMatrix(t, openAITestCaps(), false) }, []degrade.Endpoint{"/unknown"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := buildWithWS(openAITestConfig("http://127.0.0.1:0"), tc.matrix(), obs.NewMetrics(prometheus.NewRegistry()), slog.New(slog.NewTextHandler(io.Discard, nil)), tc.doors...)
			if (err != nil) != tc.wantError {
				t.Fatalf("门清单 %v: %v", tc.doors, err)
			}
			if err != nil {
				return
			}
			defer b.ShutdownWebSockets(context.Background())
			for _, ep := range []degrade.Endpoint{oa, ds} {
				want := 404
				for _, registered := range tc.doors {
					if ep == registered {
						want = 401
					}
				}
				w := httptest.NewRecorder()
				b.Mux.ServeHTTP(w, httptest.NewRequest("GET", string(ep)+"?model=real-model", nil))
				if w.Code != want {
					t.Fatalf("门 %s: %d want %d", ep, w.Code, want)
				}
				if want == 401 && ep == oa && !strings.Contains(w.Body.String(), `"error":`) {
					t.Fatal("GA 门接入了错误协议 handler")
				}
			}
		})
	}
	t.Run("health-only-cannot-register", func(t *testing.T) {
		m, err := degrade.Phase1()
		if err != nil {
			t.Fatal(err)
		}
		for _, eps := range [][]degrade.Endpoint{{oa}, {oa, oa}, {"/unknown"}} {
			if _, err := buildWithWS(config.Config{}, m, obs.NewMetrics(prometheus.NewRegistry()), nil, eps...); err == nil {
				t.Fatalf("健康检查配置忽略了显式门: %v", eps)
			}
		}
	})
}

func TestOpenAIRealtimePreflightRealModel(t *testing.T) {
	var calls atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer u.Close()
	for _, tc := range []struct {
		name   string
		mutate func(*http.Request, *WSDeps)
		status int
	}{
		{"wildcard-alias", func(r *http.Request, _ *WSDeps) { r.URL.RawQuery = "model=alias" }, 400},
		{"exact-alias", func(r *http.Request, d *WSDeps) {
			r.URL.RawQuery = "model=alias"
			var err error
			d.Router, err = router.New([]router.Rule{{Match: "alias", Targets: []router.Target{{Kind: degrade.ProviderOpenAIRealtime, Endpoint: "ep", BaseURL: u.URL, UpstreamModel: "real-model", CredentialPool: "pool"}}}})
			if err != nil {
				t.Fatal(err)
			}
		}, 400},
		{"foreign-homogeneous", func(_ *http.Request, d *WSDeps) {
			d.Matrix = degrade.NewMatrix()
			if err := d.Matrix.Add(degrade.NewRoute(degrade.ProtoOpenAIRealtime, degrade.ProviderDashScopeWSRealtime).Pass(openAITestCaps()...).Redeem(degrade.EndpointOpenAIRealtime, openAITestCaps()...).MarkHomogeneous().Build()); err != nil {
				t.Fatal(err)
			}
			var err error
			d.Router, err = router.New([]router.Rule{{Match: "*", Targets: []router.Target{{Kind: degrade.ProviderDashScopeWSRealtime, Endpoint: "ep", BaseURL: u.URL, UpstreamModel: "real-model", CredentialPool: "pool"}}}})
			if err != nil {
				t.Fatal(err)
			}
		}, 400},
		{"partial-door", func(_ *http.Request, d *WSDeps) { d.Matrix = openAITestMatrix(t, openAITestCaps()[:12], false) }, 501},
		{"planned", func(_ *http.Request, d *WSDeps) { d.Matrix, _ = degrade.Phase1() }, 501},
		{"query-extra", func(r *http.Request, _ *WSDeps) { r.URL.RawQuery += "&call_id=private-token" }, 400},
		{"transcription", func(r *http.Request, _ *WSDeps) { r.URL.RawQuery += "&intent=transcription" }, 400},
		{"duplicate-model", func(r *http.Request, _ *WSDeps) { r.URL.RawQuery += "&model=real-model" }, 400},
		{"empty-model", func(r *http.Request, _ *WSDeps) { r.URL.RawQuery = "model=" }, 400},
		{"malformed-query", func(r *http.Request, _ *WSDeps) { r.URL.RawQuery = "model=%xx" }, 400},
		{"beta-empty", func(r *http.Request, _ *WSDeps) { r.Header["openai-beta"] = nil }, 400},
		{"subprotocol", func(r *http.Request, _ *WSDeps) { r.Header.Set("Sec-WebSocket-Protocol", "private-token") }, 400},
		{"safety-duplicate", func(r *http.Request, _ *WSDeps) {
			r.Header["OpenAI-Safety-Identifier"] = []string{"one", "private-token"}
		}, 400},
		{"auth-duplicate", func(r *http.Request, _ *WSDeps) { r.Header.Add("Authorization", "Bearer private-token") }, 400},
		{"no-auth", func(r *http.Request, _ *WSDeps) { r.Header.Del("Authorization") }, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, r := openAITestDeps(t, u.URL), openAITestRequest()
			tc.mutate(r, &d)
			w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
			NewOpenAIRealtimeHandler(d).ServeHTTP(w, r)
			if w.Code != tc.status || calls.Load() != 0 || w.calls != 0 {
				t.Fatalf("预检: status=%d dials=%d hijacks=%d", w.Code, calls.Load(), w.calls)
			}
			if !strings.Contains(w.Body.String(), `"error":`) || strings.Contains(w.Body.String(), "private-token") {
				t.Fatal("未用安全 OpenAI 信封")
			}
			for _, stat := range d.Pools["pool"].Stats() {
				if stat.Picks != 0 {
					t.Fatal("预检失败仍借用凭据")
				}
			}
		})
	}
	t.Run("no-hijacker", func(t *testing.T) {
		d := openAITestDeps(t, u.URL)
		w := httptest.NewRecorder()
		NewOpenAIRealtimeHandler(d).ServeHTTP(w, openAITestRequest())
		if w.Code != 500 || calls.Load() != 0 {
			t.Fatal("无 Hijacker 仍拨上游")
		}
	})
	for _, missing := range []canonical.Capability{canonical.CapVisionInput, canonical.CapImageDetail, canonical.CapReasoning} {
		t.Run("missing-"+string(missing), func(t *testing.T) {
			d := openAITestDeps(t, u.URL)
			var caps []canonical.Capability
			for _, c := range openAITestCaps() {
				if c != missing {
					caps = append(caps, c)
				}
			}
			d.Matrix = openAITestMatrix(t, caps, false)
			w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
			NewOpenAIRealtimeHandler(d).ServeHTTP(w, openAITestRequest())
			if w.Code != 501 || calls.Load() != 0 || w.calls != 0 {
				t.Fatal("整门检查沿用了旧 12 项集合")
			}
		})
	}
}

func openAITestBuild(t *testing.T, cfg config.Config, both bool) (*Built, *prometheus.Registry) {
	t.Helper()
	eps := []degrade.Endpoint{degrade.EndpointOpenAIRealtime}
	if both {
		eps = append(eps, degrade.EndpointDashScopeRealtime)
	}
	reg := prometheus.NewRegistry()
	b, err := buildWithWS(cfg, openAITestMatrix(t, openAITestCaps(), both), obs.NewMetrics(reg), slog.New(slog.NewTextHandler(io.Discard, nil)), eps...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := b.ShutdownWebSockets(ctx); err != nil {
			t.Error(err)
		}
		if b.wsBudget.Used() != 0 {
			t.Error("Built 全部 worker 退出后预算未归零")
		}
	})
	return b, reg
}

func openAITestServer(t *testing.T, b *Built, wrap func(http.ResponseWriter) http.ResponseWriter) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	done := make(chan struct{}, 16)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { done <- struct{}{} }()
		if wrap != nil {
			w = wrap(w)
		}
		b.Mux.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s, done
}

func openAITestDial(t *testing.T, url, path string) (*ws.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	c, resp, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(url, "http")+path+"?model=real-model", ws.DialOptions{
		Header: http.Header{"Authorization": {"Bearer sk-test-1234567890"}}, MaxPayload: 2 << 20, WriteTimeout: time.Second})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if c != nil {
		t.Cleanup(func() { _ = c.Close(1001, "") })
	}
	return c, resp, err
}

func TestOpenAIRealtimeFirstEvent(t *testing.T) {
	for _, tc := range []struct {
		name, raw     string
		op            ws.Opcode
		status, calls int
	}{
		{"legacy", wsTestInitial, ws.OpText, 500, 1},
		{"wrong-object", `{"type":"session.created","session":{"id":"s","type":"realtime","object":"other"}}`, ws.OpText, 500, 1},
		{"transcription-session", `{"type":"session.created","session":{"id":"s","type":"transcription","object":"realtime.session"}}`, ws.OpText, 500, 1},
		{"missing-id", `{"type":"session.created","session":{"type":"realtime","object":"realtime.session"}}`, ws.OpText, 500, 1},
		{"updated", `{"type":"session.updated","session":{"id":"s","type":"realtime","object":"realtime.session"}}`, ws.OpText, 500, 1},
		{"binary", openAIReady, ws.OpBinary, 500, 1},
		{"bad-json", `{"type":`, ws.OpText, 500, 1},
		{"error-retry", `{"type":"error","error":{"code":"rate_limit_exceeded","message":"private-token"}}`, ws.OpText, 429, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upDone := make(chan struct{}, 4)
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { upDone <- struct{}{} }()
				calls.Add(1)
				c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: time.Second, WriteTimeout: time.Second})
				if err != nil {
					t.Error(err)
					return
				}
				defer c.Close(1000, "")
				_ = c.WriteMessage(tc.op, []byte(tc.raw))
				_, _, _ = c.ReadMessage()
			}))
			defer u.Close()
			cfg := openAITestConfig(u.URL)
			cfg.Credentials["pool1"] = append(cfg.Credentials["pool1"], config.CredentialSpec{ID: "2", Secret: "sec2"})
			b, _ := openAITestBuild(t, cfg, false)
			w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
			r := openAITestRequest()
			r.Header.Set("Authorization", "Bearer sk-test-1234567890")
			b.Mux.ServeHTTP(w, r)
			for i := 0; i < tc.calls; i++ {
				awaitWSTest(t, upDone)
			}
			if w.calls != 0 || w.Code != tc.status || calls.Load() != int32(tc.calls) {
				t.Fatalf("首帧误升级/重试: status=%d hijacks=%d calls=%d", w.Code, w.calls, calls.Load())
			}
			if strings.Contains(w.Body.String(), "private-token") {
				t.Fatal("首错误原文泄漏")
			}
		})
	}
}

func TestOpenAIRealtimeCommitted(t *testing.T) {
	for _, mode := range []string{"hijack", "short-101", "post-101-error"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			upDone := make(chan struct{}, 4)
			const failure = ` {"type":"error","error":{"code":"rate_limit_exceeded","message":"原样错误"}} `
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { upDone <- struct{}{} }()
				calls.Add(1)
				c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: time.Second, WriteTimeout: time.Second})
				if err != nil {
					t.Error(err)
					return
				}
				defer c.Close(1000, "")
				_ = c.WriteMessage(ws.OpText, []byte(openAIReady))
				if mode == "post-101-error" {
					_ = c.WriteMessage(ws.OpText, []byte(failure))
				}
				_, _, _ = c.ReadMessage()
			}))
			defer u.Close()
			cfg := openAITestConfig(u.URL)
			cfg.Credentials["pool1"] = append(cfg.Credentials["pool1"], config.CredentialSpec{ID: "2", Secret: "sec2"})
			cfg.Providers = append(cfg.Providers, config.ProviderSpec{Endpoint: "ep2", Kind: "openai.realtime", BaseURL: u.URL, CredentialPool: "pool1"})
			cfg.Models[0].Targets = append(cfg.Models[0].Targets, config.TargetSpec{Endpoint: "ep2", UpstreamModel: "real-model"})
			b, _ := openAITestBuild(t, cfg, false)
			if mode == "hijack" {
				w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
				r := openAITestRequest()
				r.Header.Set("Authorization", "Bearer sk-test-1234567890")
				b.Mux.ServeHTTP(w, r)
				if w.calls != 1 || w.Body.Len() != 0 {
					t.Fatal("承诺后重复 Hijack 或补写 HTTP")
				}
			} else {
				var wrap func(http.ResponseWriter) http.ResponseWriter
				if mode == "short-101" {
					wrap = func(w http.ResponseWriter) http.ResponseWriter {
						return &wsTestHijacker{ResponseWriter: w, wrap: func(c net.Conn) net.Conn { return wsShort101{c} }}
					}
				}
				s, done := openAITestServer(t, b, wrap)
				c, _, err := openAITestDial(t, s.URL, "/v1/realtime")
				if mode == "short-101" {
					if err == nil {
						t.Fatal("101 短写被接受")
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					wsReadLiteral(t, c, openAIReady)
					wsReadLiteral(t, c, failure)
					_ = c.Close(1000, "")
				}
				awaitWSTest(t, done)
			}
			awaitWSTest(t, upDone)
			if calls.Load() != 1 || b.wsBudget.Used() != 0 {
				t.Fatal("committed 后重拨或首帧未释放")
			}
		})
	}
}

func TestOpenAIRealtimeHandshakeBudget(t *testing.T) {
	t.Run("finite-credentials", func(t *testing.T) {
		var calls atomic.Int32
		var seen atomic.Uint32
		u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			switch r.Header.Get("Authorization") {
			case "Bearer sec1":
				seen.Or(1)
			case "Bearer sec2":
				seen.Or(2)
			default:
				t.Error("拨号使用了凭据池之外的密钥")
			}
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"private-token"}}`)
		}))
		defer u.Close()
		cfg := openAITestConfig(u.URL)
		cfg.Credentials["pool1"] = append(cfg.Credentials["pool1"], config.CredentialSpec{ID: "2", Secret: "sec2"})
		b, _ := openAITestBuild(t, cfg, false)
		r := openAITestRequest()
		r.Header.Set("Authorization", "Bearer sk-test-1234567890")
		w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
		b.Mux.ServeHTTP(w, r)
		if calls.Load() != 2 || seen.Load() != 3 || w.Code != 429 || w.calls != 0 || w.Header().Get("Retry-After") != "7" || strings.Contains(w.Body.String(), "private-token") {
			t.Fatal("凭据次数或安全错误头错误")
		}
	})
	t.Run("shared-candidate-deadline", func(t *testing.T) {
		d := openAITestDeps(t, "http://127.0.0.1:0")
		d.Timeouts.FirstByte = time.Second
		target := router.Target{Kind: degrade.ProviderOpenAIRealtime, Endpoint: "ep", BaseURL: "http://127.0.0.1:0", UpstreamModel: "real-model", CredentialPool: "pool"}
		next := target
		next.Endpoint = "next"
		var err error
		d.Router, err = router.New([]router.Rule{{Match: "real-model", Targets: []router.Target{target, next}}})
		if err != nil {
			t.Fatal(err)
		}
		var deadlines []time.Time
		fn := openAIDialFunc(func(ctx context.Context, req provider.Request) (*ws.Conn, *http.Response, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Error("无共同期限")
			}
			deadlines = append(deadlines, deadline)
			if req.Inbound.Protocol != degrade.ProtoOpenAIRealtime || req.Inbound.Endpoint != degrade.EndpointOpenAIRealtime || req.Target.UpstreamModel != "real-model" {
				t.Error("GA 身份丢失")
			}
			return nil, nil, errors.New("未知故障")
		})
		d.Providers["ep"], d.Providers["next"] = fn, fn
		NewOpenAIRealtimeHandler(d).ServeHTTP(&wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}, openAITestRequest())
		if len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) {
			t.Fatal("换候选重置了 first_byte")
		}
	})
	t.Run("ping-cannot-extend-ready", func(t *testing.T) {
		upDone := make(chan struct{}, 4)
		var calls atomic.Int32
		u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { upDone <- struct{}{} }()
			calls.Add(1)
			c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, WriteTimeout: 100 * time.Millisecond})
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close(1000, "")
			for i := 0; i < 200; i++ {
				if c.Ping(nil) != nil {
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}))
		defer u.Close()
		cfg := openAITestConfig(u.URL)
		cfg.Timeouts.Connect, cfg.Timeouts.FirstByte = 50*time.Millisecond, 120*time.Millisecond
		cfg.Credentials["pool1"] = append(cfg.Credentials["pool1"], config.CredentialSpec{ID: "2", Secret: "sec2"})
		b, _ := openAITestBuild(t, cfg, false)
		w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
		r := openAITestRequest()
		r.Header.Set("Authorization", "Bearer sk-test-1234567890")
		start := time.Now()
		b.Mux.ServeHTTP(w, r)
		awaitWSTest(t, upDone)
		if time.Since(start) > time.Second || w.Code != 502 || w.calls != 0 || calls.Load() != 1 {
			t.Fatal("心跳延长 first_byte 或到期后重拨")
		}
	})
	t.Run("101-in-common-deadline", func(t *testing.T) {
		upDone := make(chan struct{}, 4)
		var calls atomic.Int32
		u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { upDone <- struct{}{} }()
			calls.Add(1)
			c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, WriteTimeout: time.Second})
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close(1000, "")
			_ = c.WriteMessage(ws.OpText, []byte(openAIReady))
			_, _, _ = c.ReadMessage()
		}))
		defer u.Close()
		cfg := openAITestConfig(u.URL)
		cfg.Timeouts.Connect, cfg.Timeouts.FirstByte = 50*time.Millisecond, 200*time.Millisecond
		b, _ := openAITestBuild(t, cfg, false)
		stalled := make(chan *wsStalled101, 1)
		s, done := openAITestServer(t, b, func(w http.ResponseWriter) http.ResponseWriter {
			return &wsTestHijacker{ResponseWriter: w, wrap: func(c net.Conn) net.Conn {
				blocked := &wsStalled101{Conn: c, entered: make(chan struct{}), unblock: make(chan struct{})}
				stalled <- blocked
				return blocked
			}}
		})
		start := time.Now()
		c, _, err := openAITestDial(t, s.URL, "/v1/realtime")
		blocked := receiveWSTest(t, stalled)
		defer blocked.Close()
		awaitWSTest(t, done)
		awaitWSTest(t, upDone)
		if c != nil || err == nil || time.Since(start) > time.Second || calls.Load() != 1 {
			t.Fatal("101 写未受共同期限约束或失败后重拨")
		}
	})
}

type openAIDialFunc func(context.Context, provider.Request) (*ws.Conn, *http.Response, error)

func (f openAIDialFunc) Kind() degrade.Provider { return degrade.ProviderOpenAIRealtime }
func (f openAIDialFunc) Dial(ctx context.Context, r provider.Request) (*ws.Conn, *http.Response, error) {
	return f(ctx, r)
}
