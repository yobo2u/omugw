package gateway

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/provider"
	dsws "github.com/yobo2u/omugw/internal/provider/dashscoperealtime"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 显式测试矩阵不修改 Phase1；部分兑现必须仍可构建，用来验证 handler 的整门闸门。
func wsHandlerMatrix(t *testing.T, full bool) *degrade.Matrix {
	t.Helper()
	base, err := degrade.Phase1()
	if err != nil {
		t.Fatal(err)
	}
	m := degrade.NewMatrix()
	for _, r := range base.Routes() {
		if r.InProtocol() == degrade.ProtoDashScopeRealtime && r.OutProvider() == degrade.ProviderDashScopeWSRealtime {
			caps := degrade.ExpressibleSet(degrade.ProtoDashScopeRealtime)
			if !full {
				caps = caps[:1]
			}
			if err := m.Add(degrade.NewRoute(degrade.ProtoDashScopeRealtime, degrade.ProviderDashScopeWSRealtime).
				Pass(degrade.ExpressibleSet(degrade.ProtoDashScopeRealtime)...).
				Redeem(degrade.EndpointDashScopeRealtime, caps...).MarkHomogeneous().Build()); err != nil {
				t.Fatal(err)
			}
		} else if err := m.Add(r, nil); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func wsHandlerDeps(t *testing.T, baseURL string) WSDeps {
	t.Helper()
	pool, err := credential.NewPool("pool", []credential.Credential{{ID: "a", Secret: "synthetic-a"}, {ID: "b", Secret: "synthetic-b"}}, credential.DefaultPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := router.New([]router.Rule{{Match: "*", Targets: []router.Target{{Kind: degrade.ProviderDashScopeWSRealtime, Endpoint: "ep", BaseURL: baseURL, UpstreamModel: "real-model", CredentialPool: "pool"}}}})
	if err != nil {
		t.Fatal(err)
	}
	limits := config.WebSocket{MaxMessageBytes: 1 << 20, MaxSessions: 8, MaxBufferedBytes: 8 << 20}
	budget := wsTestBudget(t, limits.MaxBufferedBytes)
	timeouts := config.Timeouts{Connect: time.Second, FirstByte: 2 * time.Second, Idle: time.Second, Total: 3 * time.Second}
	return WSDeps{Matrix: wsHandlerMatrix(t, true), Router: rt,
		Auth:    NewAuthenticator([]config.AuthKey{{ID: "caller", Key: "synthetic-key"}}),
		Metrics: obs.NewMetrics(prometheus.NewRegistry()), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Pools:     map[string]*credential.Pool{"pool": pool},
		Providers: map[string]provider.StreamProvider{"ep": dsws.New(timeouts, limits, budget)},
		Timeouts:  timeouts, Limits: limits, Budget: budget, Registry: newWSRegistry(limits.MaxSessions)}
}

func wsHandlerRequest() *http.Request {
	r := httptest.NewRequest("GET", "/api-ws/v1/realtime?model=real-model", nil)
	r.Header = http.Header{"Authorization": {"Bearer synthetic-key"}, "Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-Websocket-Version": {"13"}, "Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="}}
	return r
}

type wsRejectHijacker struct {
	*httptest.ResponseRecorder
	calls int
}

func (w *wsRejectHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.calls++
	return nil, nil, errors.New("synthetic hijack failure")
}

func TestWSHandlerPreflight(t *testing.T) {
	var calls atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer u.Close()
	for _, tc := range []struct {
		name   string
		mutate func(*http.Request, *WSDeps)
		status int
	}{
		{"no-auth", func(r *http.Request, _ *WSDeps) { r.Header.Del("Authorization") }, 401},
		{"duplicate-auth", func(r *http.Request, _ *WSDeps) { r.Header.Add("Authorization", "Bearer synthetic-key") }, 400},
		{"duplicate-api-key", func(r *http.Request, _ *WSDeps) {
			r.Header.Del("Authorization")
			r.Header["Api-Key"] = []string{"synthetic-key", "synthetic-key"}
		}, 400},
		{"both-auth", func(r *http.Request, _ *WSDeps) { r.Header.Set("Api-Key", "synthetic-key") }, 400},
		{"subprotocol", func(r *http.Request, _ *WSDeps) { r.Header.Set("Sec-WebSocket-Protocol", "private-token") }, 400},
		{"tenant-duplicate", func(r *http.Request, _ *WSDeps) { r.Header["X-Dashscope-Workspace"] = []string{"one", "two"} }, 400},
		{"tenant-control", func(r *http.Request, _ *WSDeps) { r.Header.Set("X-DashScope-DataInspection", "x\n") }, 400},
		{"post", func(r *http.Request, _ *WSDeps) { r.Method = "POST" }, 400},
		{"http2", func(r *http.Request, _ *WSDeps) { r.ProtoMajor = 2 }, 400},
		{"upgrade", func(r *http.Request, _ *WSDeps) { r.Header.Del("Upgrade") }, 400},
		{"nonce", func(r *http.Request, _ *WSDeps) { r.Header.Set("Sec-WebSocket-Key", "invalid") }, 400},
		{"duplicate-version", func(r *http.Request, _ *WSDeps) { r.Header.Add("Sec-WebSocket-Version", "13") }, 400},
		{"body", func(r *http.Request, _ *WSDeps) { r.Body = io.NopCloser(strings.NewReader("x")); r.ContentLength = 1 }, 400},
		{"transfer", func(r *http.Request, _ *WSDeps) { r.TransferEncoding = []string{"chunked"} }, 400},
		{"unknown-query", func(r *http.Request, _ *WSDeps) { r.URL.RawQuery += "&api_key=private-token" }, 400},
		{"malformed-query", func(r *http.Request, _ *WSDeps) { r.URL.RawQuery = "model=%xx" }, 400},
		{"duplicate-model", func(r *http.Request, _ *WSDeps) { r.URL.RawQuery += "&model=real-model" }, 400},
		{"empty-model", func(r *http.Request, _ *WSDeps) { r.URL.RawQuery = "model=" }, 400},
		{"missing-model", func(r *http.Request, _ *WSDeps) { r.URL.RawQuery = "" }, 400},
		{"alias-wildcard", func(r *http.Request, _ *WSDeps) { r.URL.RawQuery = "model=alias" }, 400},
		{"partial-door", func(_ *http.Request, d *WSDeps) { d.Matrix = wsHandlerMatrix(t, false) }, 501},
		{"planned-door", func(_ *http.Request, d *WSDeps) { d.Matrix, _ = degrade.Phase1() }, 501},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := wsHandlerDeps(t, u.URL)
			r := wsHandlerRequest()
			tc.mutate(r, &d)
			w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
			NewDashScopeRealtimeHandler(d).ServeHTTP(w, r)
			if w.Code != tc.status || calls.Load() != 0 || w.calls != 0 {
				t.Fatalf("预检状态/零拨号边界错误: status=%d dials=%d hijacks=%d", w.Code, calls.Load(), w.calls)
			}
			if !strings.Contains(w.Body.String(), `"code"`) || strings.Contains(w.Body.String(), "private-token") {
				t.Fatal("本地错误未使用安全 DashScope 信封")
			}
		})
	}
	t.Run("no-hijacker", func(t *testing.T) {
		d := wsHandlerDeps(t, u.URL)
		w := httptest.NewRecorder()
		NewDashScopeRealtimeHandler(d).ServeHTTP(w, wsHandlerRequest())
		if w.Code != 500 || calls.Load() != 0 {
			t.Fatal("无 Hijacker 时触达上游")
		}
	})
}

func TestWSHandlerLeaseAndShutdownAdmission(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		d := wsHandlerDeps(t, "http://127.0.0.1:0")
		d.Registry = newWSRegistry(1)
		s, err := d.Registry.Register(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := 429
		if sealed {
			s.Done()
			if err := d.Registry.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			want = 503
		}
		w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
		NewDashScopeRealtimeHandler(d).ServeHTTP(w, wsHandlerRequest())
		if w.Code != want || d.Pools["pool"].Stats()[0].Picks != 0 {
			t.Fatalf("本地准入状态丢失或借用了凭据: %d", w.Code)
		}
		s.Done()
	}
}

// 真实 HTTP 401/429 分类决定换 key；未知 101 前断流不能凭正文猜 auth。
func TestWSHandlerFailoverHTTP(t *testing.T) {
	for _, status := range []int{401, 429, 302} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "7")
				w.Header().Set("X-Private", "private-token")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"message":"private-token auth quota"}`)
			}))
			defer u.Close()
			d := wsHandlerDeps(t, u.URL)
			w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
			NewDashScopeRealtimeHandler(d).ServeHTTP(w, wsHandlerRequest())
			wantCalls, wantStatus := int32(2), status
			if status == 302 {
				wantCalls, wantStatus = 1, 500
			}
			if calls.Load() != wantCalls || w.Code != wantStatus || w.calls != 0 {
				t.Fatalf("错误重试/状态不符: calls=%d status=%d", calls.Load(), w.Code)
			}
			if strings.Contains(w.Body.String(), "private-token") || w.Header().Get("X-Private") != "" {
				t.Fatal("上游原文或头泄漏")
			}
			for _, stat := range d.Pools["pool"].Stats() {
				if stat.Picks > 1 || status != 302 && stat.ConsecutiveFails != 1 || status == 302 && stat.ConsecutiveFails != 0 {
					t.Fatal("凭据重复使用或错误冷却")
				}
			}
			if status != 302 && w.Header().Get("Retry-After") != "7" {
				t.Fatal("过滤后错误头丢失")
			}
		})
	}
}

func wsHandlerServer(t *testing.T, d WSDeps) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	done := make(chan struct{}, 16)
	h := NewDashScopeRealtimeHandler(d)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer func() { done <- struct{}{} }(); h.ServeHTTP(w, r) }))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if err := d.Registry.Shutdown(ctx); err != nil {
			t.Error("handler 未排空")
		}
		srv.Close()
		if d.Budget.Used() != 0 {
			t.Error("handler 退出后预算未归零")
		}
	})
	return srv, done
}

func wsHandlerDial(t *testing.T, url string) *ws.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, resp, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(url, "http")+"/api-ws/v1/realtime?model=real-model", ws.DialOptions{Header: http.Header{"Authorization": {"Bearer synthetic-key"}, "Origin": {"https://client.example"}}, MaxPayload: 1 << 20, WriteTimeout: time.Second})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal("本地下游升级失败")
	}
	t.Cleanup(func() { _ = c.Close(ws.CloseGoingAway, "") })
	return c
}

func wsReadLiteral(t *testing.T, c *ws.Conn, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m, err := c.ReadOwnedMessage(ctx)
	if err != nil {
		releaseWSRelayError(err)
		t.Fatal("未读到预期应用事件")
	}
	defer m.Release()
	if m.Opcode != ws.OpText || string(m.Payload) != want {
		t.Fatal("事件字节或顺序被改写")
	}
}

func TestWSHandlerFailoverFirstEvent(t *testing.T) {
	for _, mode := range []string{"error", "close-normal", "close-rate", "binary", "bad-json", "eof"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				hj := &wsTestHijacker{ResponseWriter: w}
				c, err := ws.Accept(hj, r, ws.AcceptOptions{MaxPayload: 1 << 20, WriteTimeout: time.Second})
				if err != nil {
					return
				}
				switch mode {
				case "error":
					_ = c.WriteMessage(ws.OpText, []byte(`{"type":"error","error":{"code":"unknown_auth_quota","message":"private-token"}}`))
				case "close-normal":
					_ = c.Close(1000, "private-token")
				case "close-rate":
					_ = c.Close(1011, "To many requests")
				case "binary":
					_ = c.WriteMessage(ws.OpBinary, []byte{0, 1})
				case "bad-json":
					_ = c.WriteMessage(ws.OpText, []byte(`{"type":`))
				case "eof":
					_ = hj.raw.Close()
				}
				_ = c.Close(1000, "")
			}))
			defer u.Close()
			d := wsHandlerDeps(t, u.URL)
			w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
			NewDashScopeRealtimeHandler(d).ServeHTTP(w, wsHandlerRequest())
			want := int32(1)
			if mode == "close-rate" {
				want = 2
			}
			if calls.Load() != want || w.calls != 0 || w.Code == 101 {
				t.Fatalf("首 error/close/非法事件误认就绪: calls=%d status=%d", calls.Load(), w.Code)
			}
			if strings.Contains(w.Body.String(), "private-token") {
				t.Fatal("首事件正文泄漏")
			}
			for _, st := range d.Pools["pool"].Stats() {
				if mode != "close-rate" && st.ConsecutiveFails != 0 {
					t.Fatal("未经证实的故障冷却凭据")
				}
			}
			if d.Budget.Used() != 0 {
				t.Fatal("首事件失败后预算未释放")
			}
		})
	}
}

func TestWSHandlerFailoverCommitted(t *testing.T) {
	for _, mode := range []string{"hijack", "short-101"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			upDone := make(chan struct{}, 4)
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { upDone <- struct{}{} }()
				calls.Add(1)
				c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: time.Second})
				if err != nil {
					return
				}
				defer c.Close(1000, "")
				_ = c.WriteMessage(ws.OpText, []byte(wsTestInitial))
				_, _, _ = c.ReadMessage()
			}))
			defer u.Close()
			d := wsHandlerDeps(t, u.URL)
			if mode == "hijack" {
				w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
				NewDashScopeRealtimeHandler(d).ServeHTTP(w, wsHandlerRequest())
				if w.calls != 1 || w.Body.Len() != 0 {
					t.Fatal("Accept 失败后写 HTTP 或重复 Hijack")
				}
			} else {
				done := make(chan struct{}, 1)
				h := NewDashScopeRealtimeHandler(d)
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer func() { done <- struct{}{} }()
					h.ServeHTTP(&wsTestHijacker{ResponseWriter: w, wrap: func(c net.Conn) net.Conn { return wsShort101{c} }}, r)
				}))
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				c, resp, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/api-ws/v1/realtime?model=real-model", ws.DialOptions{Header: http.Header{"Authorization": {"Bearer synthetic-key"}}})
				if c != nil {
					_ = c.Close(1000, "")
				}
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				awaitWSTest(t, done)
				s.Close()
				if err == nil {
					t.Fatal("101 短写被当成功")
				}
			}
			awaitWSTest(t, upDone)
			if calls.Load() != 1 || d.Budget.Used() != 0 {
				t.Fatal("承诺后重拨或初始事件泄漏")
			}
			if d.Pools["pool"].Stats()[0].ConsecutiveFails != 0 {
				t.Fatal("客户端 Accept 故障冷却凭据")
			}
		})
	}
}

type wsShort101 struct{ net.Conn }

func (c wsShort101) Write(p []byte) (int, error) { return c.Conn.Write(p[:min(len(p), 8)]) }

func TestWSHandlerLeaseAndShutdownActive(t *testing.T) {
	up := make(chan *ws.Conn, 1)
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, WriteTimeout: time.Second})
		if err != nil {
			return
		}
		_ = c.WriteMessage(ws.OpText, []byte(wsTestInitial))
		up <- c
	}))
	defer u.Close()
	d := wsHandlerDeps(t, u.URL)
	s, done := wsHandlerServer(t, d)
	c := wsHandlerDial(t, s.URL)
	uc := receiveWSTest(t, up)
	defer uc.Close(1000, "")
	wsReadLiteral(t, c, wsTestInitial)
	// 同一凭据的较新失败不能被本会话后到的正常结束清掉。
	l, err := d.Pools["pool"].Acquire(map[string]bool{"b": true})
	if err != nil {
		t.Fatal(err)
	}
	l.Fail(canonical.Newf(canonical.ClassRateLimit, "synthetic"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := d.Registry.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	awaitWSTest(t, done)
	for _, peer := range []*ws.Conn{c, uc} {
		_, err := peer.ReadOwnedMessage(ctx)
		var closed *ws.CloseError
		if !errors.As(err, &closed) {
			t.Fatal("关停未实际发送 close")
		}
		code := closed.Code
		closed.Release()
		if code != 1001 {
			t.Fatal("关停关闭码不符")
		}
	}
	st := d.Pools["pool"].Stats()[0]
	if st.ConsecutiveFails != 1 || st.Available {
		t.Fatal("旧会话结算重置较新冷却")
	}
}

// transport 自己会消化 ping；握手期限若错误地只用 idle，此测试将无法结束。
func TestWSHandlerFailoverPingCannotExtendFirstByte(t *testing.T) {
	upDone := make(chan struct{}, 1)
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { upDone <- struct{}{} }()
		c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, WriteTimeout: 100 * time.Millisecond})
		if err != nil {
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
	d := wsHandlerDeps(t, u.URL)
	d.Timeouts.FirstByte = 80 * time.Millisecond
	w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
	start := time.Now()
	NewDashScopeRealtimeHandler(d).ServeHTTP(w, wsHandlerRequest())
	if time.Since(start) > 800*time.Millisecond || w.calls != 0 || w.Code != 502 {
		t.Fatal("ping 延长握手总预算或误升级")
	}
	awaitWSTest(t, upDone)
	if d.Pools["pool"].Stats()[0].ConsecutiveFails != 0 || d.Pools["pool"].Stats()[1].Picks != 0 {
		t.Fatal("本地预算到期冷却或重试了凭据")
	}
}

type wsDialFunc func(context.Context, provider.Request) (*ws.Conn, *http.Response, error)

func (f wsDialFunc) Kind() degrade.Provider { return degrade.ProviderDashScopeWSRealtime }
func (f wsDialFunc) Dial(ctx context.Context, r provider.Request) (*ws.Conn, *http.Response, error) {
	return f(ctx, r)
}

func TestWSHandlerFailoverCandidates(t *testing.T) {
	d := wsHandlerDeps(t, "http://127.0.0.1:0")
	target := router.Target{Kind: degrade.ProviderDashScopeWSRealtime, Endpoint: "ep", BaseURL: "http://127.0.0.1:0", UpstreamModel: "real-model", CredentialPool: "pool"}
	alias, other, next := target, target, target
	alias.Endpoint, alias.UpstreamModel = "alias", "wrong-model"
	other.Endpoint, other.Kind = "other", degrade.ProviderOpenAIRealtime
	next.Endpoint = "next"
	var err error
	d.Router, err = router.New([]router.Rule{{Match: "real-model", Targets: []router.Target{alias, other, target, next}}})
	if err != nil {
		t.Fatal(err)
	}
	var deadlines []time.Time
	var ids []string
	fn := wsDialFunc(func(ctx context.Context, req provider.Request) (*ws.Conn, *http.Response, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("拨号无绝对截止时间")
		}
		deadlines = append(deadlines, deadline)
		ids = append(ids, req.Credential.ID)
		if req.Inbound.Protocol != degrade.ProtoDashScopeRealtime || req.Target.UpstreamModel != "real-model" {
			t.Error("入站身份或真模型未传给 provider")
		}
		return nil, nil, errors.New("private-token unknown upstream failure")
	})
	d.Providers["ep"], d.Providers["next"] = fn, fn
	d.Providers["alias"], d.Providers["other"] = wsDialFunc(func(context.Context, provider.Request) (*ws.Conn, *http.Response, error) {
		t.Error("别名或跨协议候选被调用")
		return nil, nil, errors.New("unexpected")
	}), fn
	w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
	NewDashScopeRealtimeHandler(d).ServeHTTP(w, wsHandlerRequest())
	if len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) || len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatal("未知错误重试了同 target 的 key、候选预算重置或跳错候选")
	}
	if strings.Contains(w.Body.String(), "private-token") {
		t.Fatal("未知拨号错误泄漏原文")
	}
}

func TestWSHandlerLeaseAndShutdownPending(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		d := wsHandlerDeps(t, "http://127.0.0.1:0")
		entered := make(chan struct{})
		d.Providers["ep"] = wsDialFunc(func(ctx context.Context, _ provider.Request) (*ws.Conn, *http.Response, error) {
			close(entered)
			<-ctx.Done()
			// 即使出站层把 timeout 映射为 Retryable，客户端取消仍不能冷却凭据。
			return nil, nil, canonical.Newf(canonical.ClassUpstreamUnavailable, "synthetic")
		})
		ctx, cancel := context.WithCancel(context.Background())
		r := wsHandlerRequest().WithContext(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			NewDashScopeRealtimeHandler(d).ServeHTTP(&wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}, r)
		}()
		awaitWSTest(t, entered)
		if shutdown {
			stop, stopCancel := context.WithTimeout(context.Background(), time.Second)
			if err := d.Registry.Shutdown(stop); err != nil {
				t.Error("pending 未被取消并排空")
			}
			stopCancel()
		} else {
			cancel()
		}
		awaitWSTest(t, done)
		cancel()
		stats := d.Pools["pool"].Stats()
		if stats[0].ConsecutiveFails != 0 || stats[0].Picks != 1 || stats[1].Picks != 0 {
			t.Fatal("pending 取消被误算上游故障或重试")
		}
	}
}

type wsStalled101 struct {
	net.Conn
	entered chan struct{}
	unblock chan struct{}
	once    sync.Once
}

func (c *wsStalled101) Write([]byte) (int, error) {
	close(c.entered)
	<-c.unblock
	return 0, io.ErrClosedPipe
}
func (c *wsStalled101) release()     { c.once.Do(func() { close(c.unblock) }) }
func (c *wsStalled101) Close() error { c.release(); return c.Conn.Close() }
func (c *wsStalled101) SetWriteDeadline(d time.Time) error {
	if !d.IsZero() && !d.After(time.Now()) {
		c.release()
	}
	return c.Conn.SetWriteDeadline(d)
}
func (c *wsStalled101) SetDeadline(d time.Time) error {
	if !d.IsZero() && !d.After(time.Now()) {
		c.release()
	}
	return c.Conn.SetDeadline(d)
}

// Accept 内已 Hijack、尚未返回 Conn 的窗口同样属于 pending，关停不能漏掉它。
func TestWSHandlerLeaseAndShutdownDuring101(t *testing.T) {
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: time.Second})
		if err != nil {
			return
		}
		defer c.Close(1000, "")
		_ = c.WriteMessage(ws.OpText, []byte(wsTestInitial))
		_, _, _ = c.ReadMessage()
	}))
	defer u.Close()
	d := wsHandlerDeps(t, u.URL)
	h := NewDashScopeRealtimeHandler(d)
	stalled := make(chan *wsStalled101, 1)
	done := make(chan struct{}, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { done <- struct{}{} }()
		h.ServeHTTP(&wsTestHijacker{ResponseWriter: w, wrap: func(c net.Conn) net.Conn {
			blocked := &wsStalled101{Conn: c, entered: make(chan struct{}), unblock: make(chan struct{})}
			stalled <- blocked
			return blocked
		}}, r)
	}))
	defer s.Close()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c, resp, _ := ws.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/api-ws/v1/realtime?model=real-model", ws.DialOptions{Header: http.Header{"Authorization": {"Bearer synthetic-key"}}})
		if c != nil {
			_ = c.Close(1000, "")
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	blocked := receiveWSTest(t, stalled)
	defer blocked.Close()
	awaitWSTest(t, blocked.entered)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	err := d.Registry.Shutdown(ctx)
	blocked.release()
	awaitWSTest(t, done)
	awaitWSTest(t, clientDone)
	if err != nil {
		t.Fatal("关停漏掉 Accept 内已 Hijack 的 pending 连接")
	}
	if d.Budget.Used() != 0 {
		t.Fatal("101 取消后初始消息未释放")
	}
}
