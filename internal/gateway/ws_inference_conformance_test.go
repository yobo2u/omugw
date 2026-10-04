package gateway

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 六项显式授权只服务隔离机制测试；新增表达能力不能自动被这张测试矩阵批准。
func inferenceTestCaps() []canonical.Capability {
	return []canonical.Capability{canonical.CapTextGeneration, canonical.CapStreaming, canonical.CapAudioInput,
		canonical.CapAudioOutput, canonical.CapSpeechSynthesis, canonical.CapSpeechRecognition}
}

func inferenceBuildMatrix(t *testing.T, three bool, caps []canonical.Capability) *degrade.Matrix {
	t.Helper()
	base, err := degrade.Phase1()
	if err != nil {
		t.Fatal(err)
	}
	if three {
		base = openAITestMatrix(t, openAITestCaps(), true)
	}
	m := degrade.NewMatrix()
	for _, r := range base.Routes() {
		if r.InProtocol() == degrade.ProtoDashScopeInference && r.OutProvider() == degrade.ProviderDashScopeWSInference {
			err = m.Add(degrade.NewRoute(degrade.ProtoDashScopeInference, degrade.ProviderDashScopeWSInference).
				Pass(inferenceTestCaps()...).Redeem(degrade.EndpointDashScopeInference, caps...).MarkHomogeneous().Build())
		} else {
			err = m.Add(r, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func inferenceBuildConfig(url string, three bool) config.Config {
	cfg := buildTestConfig(url)
	cfg.Providers[0].Kind = string(degrade.ProviderDashScopeWSInference)
	cfg.Models = nil
	for _, model := range []string{inferenceASR, inferenceTTS, "sambert-zhichu-v1"} {
		cfg.Models = append(cfg.Models, config.ModelSpec{Match: model, Targets: []config.TargetSpec{{Endpoint: "ep1", UpstreamModel: model}}})
	}
	cfg.Timeouts = config.Timeouts{Connect: 80 * time.Millisecond, FirstByte: time.Second, Idle: 2 * time.Second, Total: 3 * time.Second}
	cfg.WebSocket = config.WebSocket{MaxMessageBytes: 1 << 20, MaxBufferedBytes: 8 << 20, MaxSessions: 4}
	if three {
		for _, p := range []config.ProviderSpec{
			{Endpoint: "oa", Kind: string(degrade.ProviderOpenAIRealtime), BaseURL: url, CredentialPool: "pool1"},
			{Endpoint: "ds", Kind: string(degrade.ProviderDashScopeWSRealtime), BaseURL: url, CredentialPool: "pool1"},
		} {
			cfg.Providers = append(cfg.Providers, p)
			cfg.Models = append(cfg.Models, config.ModelSpec{Match: p.Endpoint, Targets: []config.TargetSpec{{Endpoint: p.Endpoint, UpstreamModel: p.Endpoint}}})
		}
	}
	return cfg
}

func TestInferenceConformanceReplay(t *testing.T) {
	for _, name := range []string{"asr-duplex", "tts-duplex", "sambert-out", "two-tasks", "failed-close"} {
		t.Run(name, func(t *testing.T) {
			limits := testkit.DefaultWSLimits()
			f, err := testkit.ReadWSFixture(filepath.Join("..", "..", "testdata", "testkit", "ws", "inference-audio", name+".json"), limits)
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range f.Response.WS.Nodes {
				if n.Kind == "message" && (n.Match != "bytes" || len(n.Fields) != 0) {
					t.Fatal("回放必须逐 opcode/bytes 匹配")
				}
			}
			u, err := testkit.NewWSReplayUpstream(f, limits)
			if err != nil {
				t.Fatal(err)
			}
			us := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer sec1" || r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" {
					t.Error("干净握手失效")
				}
				u.ServeHTTP(w, r)
			}))
			reg := prometheus.NewRegistry()
			b, err := buildWithWS(inferenceBuildConfig(us.URL, false), inferenceBuildMatrix(t, false, inferenceTestCaps()), obs.NewMetrics(reg), nil, degrade.EndpointDashScopeInference)
			if err != nil {
				_ = u.Close()
				us.Close()
				t.Fatal(err)
			}
			done := make(chan struct{})
			gs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); b.Mux.ServeHTTP(w, r) }))
			t.Cleanup(func() {
				_ = u.Close()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second+wsSchedulingSlack())
				defer cancel()
				if err := b.ShutdownWebSockets(ctx); err != nil {
					t.Error(err)
				}
				select {
				case <-done:
				case <-ctx.Done():
					t.Error("回放 handler 未 join")
				}
				gs.Close()
				us.Close()
				inferenceEmpty(t, b.wsRegistry, b.wsBudget)
			})
			ctx, cancel := context.WithTimeout(context.Background(), limits.Replay)
			defer cancel()
			c, resp, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(gs.URL, "http")+f.Request.Path, ws.DialOptions{Header: http.Header{"Authorization": {"Bearer sk-test-1234567890"}, "Cookie": {"private=synthetic"}}, MaxPayload: limits.MessageBytes, WriteTimeout: time.Second})
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if err != nil {
				t.Fatal("无首事件握手失败:", err)
			}
			t.Cleanup(func() { _ = c.Close(1001, "") })
			up, err := u.Connection(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// 没有 Realtime prelude 或 tail 视图；第一个应用消息就是 fixture 内的 run-task。
			result, err := testkit.ReplayWS(ctx, f, testkit.WSReplayEndpoints{Client: c, Upstream: up}, 1, limits)
			if err != nil {
				t.Fatal(err)
			}
			awaitWSTest(t, done)
			if err := u.Close(); err != nil || u.Err() != nil {
				t.Fatal("回放握手/关闭未通过", err, u.Err())
			}
			if len(result.Nodes) != len(f.Response.WS.Nodes) || result.Outcome.Kind != f.Response.WS.Outcome.Kind {
				t.Fatal("完整因果轨迹未交付")
			}
			outcome := "ok"
			if name == "failed-close" {
				outcome = "internal"
			}
			if wsUsageMetricSum(t, reg, "omugw_requests_total", map[string]string{"inbound": "dashscope.inference", "outcome": outcome}) != 1 {
				t.Fatal("正式 handler 未按业务结果结算")
			}
			inferenceEmpty(t, b.wsRegistry, b.wsBudget)
		})
	}
}

func TestInferenceThreeDoorBuild(t *testing.T) {
	eps := []degrade.Endpoint{degrade.EndpointOpenAIRealtime, degrade.EndpointDashScopeRealtime, degrade.EndpointDashScopeInference}
	for _, mode := range []string{"three", "partial", "missing", "duplicate", "two-providers", "two-providers-health-only"} {
		t.Run(mode, func(t *testing.T) {
			cfg := inferenceBuildConfig("http://127.0.0.1:0", true)
			caps := inferenceTestCaps()
			doors := append([]degrade.Endpoint(nil), eps...)
			switch mode {
			case "partial":
				caps = caps[:5]
			case "missing":
				doors = doors[:2]
			case "duplicate":
				doors = append(doors, degrade.EndpointDashScopeInference)
			case "two-providers", "two-providers-health-only":
				p := cfg.Providers[0]
				p.Endpoint = "second"
				cfg.Providers = append(cfg.Providers, p)
			}
			if mode == "two-providers-health-only" {
				cfg.Models = nil
				doors = nil
			}
			b, err := buildWithWS(cfg, inferenceBuildMatrix(t, true, caps), obs.NewMetrics(prometheus.NewRegistry()), nil, doors...)
			if mode != "three" {
				if err == nil {
					t.Fatal("错误门/唯一 Provider 被绕过")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer b.ShutdownWebSockets(context.Background())
			for _, ep := range eps {
				w := httptest.NewRecorder()
				b.Mux.ServeHTTP(w, httptest.NewRequest("GET", string(ep), nil))
				if w.Code != 401 {
					t.Fatalf("门 %s 未注册: %d", ep, w.Code)
				}
			}
		})
	}
	t.Run("错profile不能顶账", func(t *testing.T) {
		d := inferenceHandlerDeps(t, "http://127.0.0.1:0")
		for _, h := range []*WSHandler{NewOpenAIRealtimeHandler(d), NewDashScopeRealtimeHandler(d)} {
			if checkWSDoor(d.Matrix, h) == nil {
				t.Fatal("其他 profile 借用了 Inference 六项批准")
			}
		}
	})
}

func TestInferenceProductionDoorsRemainClosed(t *testing.T) {
	m, err := degrade.Phase1()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(inferenceBuildConfig("http://127.0.0.1:0", true), m, obs.NewMetrics(prometheus.NewRegistry()), nil)
	if err != nil {
		t.Fatal("应装配 Provider 但不注册生产入口:", err)
	}
	defer b.ShutdownWebSockets(context.Background())
	for _, ep := range []degrade.Endpoint{degrade.EndpointOpenAIRealtime, degrade.EndpointDashScopeRealtime, degrade.EndpointDashScopeInference} {
		w := httptest.NewRecorder()
		b.Mux.ServeHTTP(w, httptest.NewRequest("GET", string(ep), nil))
		if w.Code != 404 {
			t.Fatalf("生产门 %s 意外开放: %d", ep, w.Code)
		}
	}
	_, err = m.Check(degrade.Inbound{Protocol: degrade.ProtoDashScopeInference, Endpoint: degrade.EndpointDashScopeInference}, degrade.ProviderDashScopeWSInference, inferenceTestCaps())
	if canonical.AsError(err).Class != canonical.ClassNotImplemented {
		t.Fatal("默认矩阵不再返回501", err)
	}
}

func TestInferenceThreeDoorSharedLimits(t *testing.T) {
	type peer struct {
		path string
		p    *wsTestPeer
	}
	up := make(chan peer, 4)
	var calls atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		hj := &wsTestHijacker{ResponseWriter: w}
		c, err := ws.Accept(hj, r, ws.AcceptOptions{MaxPayload: 1024, WriteTimeout: time.Second})
		if err != nil {
			return
		}
		if r.URL.Path == "/v1/realtime" {
			_ = c.WriteMessage(ws.OpText, []byte(openAIReady))
		}
		if r.URL.Path == "/api-ws/v1/realtime" {
			_ = c.WriteMessage(ws.OpText, []byte(wsTestInitial))
		}
		up <- peer{r.URL.Path, &wsTestPeer{Conn: hj.raw, reader: bufio.NewReader(hj.raw)}}
	}))
	cfg := inferenceBuildConfig(u.URL, true)
	// 保留128字节给关闭帧和掩码；容量耗尽时不能虚构一定有余量发送close。
	cfg.WebSocket = config.WebSocket{MaxMessageBytes: 1024, MaxBufferedBytes: 3200, MaxSessions: 3}
	b, err := buildWithWS(cfg, inferenceBuildMatrix(t, true, inferenceTestCaps()), obs.NewMetrics(prometheus.NewRegistry()), nil, degrade.EndpointOpenAIRealtime, degrade.EndpointDashScopeRealtime, degrade.EndpointDashScopeInference)
	if err != nil {
		u.Close()
		t.Fatal(err)
	}
	type wrapped struct {
		path string
		gate *wsTestGate
	}
	gates := make(chan wrapped, 4)
	done := make(chan string, 4)
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { done <- r.URL.Path }()
		b.Mux.ServeHTTP(&wsTestHijacker{ResponseWriter: w, wrap: func(c net.Conn) net.Conn {
			gate := newWSTestGate(c, false)
			gates <- wrapped{r.URL.Path, gate}
			return gate
		}}, r)
	}))
	clients, servers, blocked := map[string]*wsTestPeer{}, map[string]*wsTestPeer{}, map[string]*wsTestGate{}
	t.Cleanup(func() {
		for _, gate := range blocked {
			gate.unblock()
		}
		for _, c := range clients {
			_ = c.Close()
		}
		for _, s := range servers {
			_ = s.Close()
		}
		for len(up) > 0 {
			_ = (<-up).p.Close()
		}
		for len(gates) > 0 {
			_ = (<-gates).gate.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second+wsSchedulingSlack())
		defer cancel()
		if err := b.ShutdownWebSockets(ctx); err != nil {
			t.Error(err)
		}
		g.Close()
		u.Close()
		inferenceEmpty(t, b.wsRegistry, b.wsBudget)
	})
	for _, path := range []string{"/v1/realtime", "/api-ws/v1/realtime", "/api-ws/v1/inference"} {
		query := ""
		if path == "/v1/realtime" {
			query = "?model=oa"
		}
		if path == "/api-ws/v1/realtime" {
			query = "?model=ds"
		}
		clients[path] = inferenceRawDial(t, g.URL, path+query, "sk-test-1234567890")
		u := receiveWSTest(t, up)
		servers[u.path] = u.p
		w := receiveWSTest(t, gates)
		blocked[w.path] = w.gate
		if query != "" {
			want := wsTestInitial
			if path == "/v1/realtime" {
				want = openAIReady
			}
			inferenceWire(t, clients[path], ws.OpText, []byte(want))
		}
	}
	const in = "/api-ws/v1/inference"
	run := inferenceRun("a", inferenceTTS, "tts", "duplex")
	clients[in].send(t, ws.OpText, run)
	inferenceWire(t, servers[in], ws.OpText, run)
	started := inferenceServer("a", "task-started", `{}`)
	servers[in].send(t, ws.OpText, started)
	inferenceWire(t, clients[in], ws.OpText, started)
	// 第四个请求与三扇门共同竞争 registry；若各门各一个 registry，此处会拨第四次上游。
	r := inferenceRequest()
	r.Header.Set("Authorization", "Bearer sk-test-1234567890")
	w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
	b.Mux.ServeHTTP(w, r)
	if w.Code != 429 || calls.Load() != 3 || w.calls != 0 {
		t.Fatal("三门未共用 MaxSessions")
	}
	for path, server := range servers {
		blocked[path].armed.Store(true)
		body := `{"type":"future.event","pad":""}`
		if path == in {
			body = string(inferenceServer("a", "future-event", `{"pad":""}`))
		}
		body += strings.Repeat(" ", 1024-len(body))
		server.send(t, ws.OpText, []byte(body))
		awaitWSTest(t, blocked[path].entered)
	}
	if b.wsBudget.Used() != 3072 {
		t.Fatalf("三门没有把实际在途消息合入同一预算: %d", b.wsBudget.Used())
	}
	clients[in].send(t, ws.OpText, append(inferenceClient("a", "continue-task", `{}`), []byte(strings.Repeat(" ", 256))...))
	// 不放开三条下行写，第四份负载只能被共享限额拒绝；上游看见真实1013而非输入。
	inferenceWireClose(t, servers[in], 1013, "buffer capacity exhausted")
	if path := receiveWSTest(t, done); path != in {
		t.Fatalf("错误会话退出: %s", path)
	}
	if b.wsBudget.Used() != 2048 {
		t.Fatalf("Inference退出误归还另两门的所有权: %d", b.wsBudget.Used())
	}
	for path, gate := range blocked {
		if path != in {
			gate.unblock()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond+wsSchedulingSlack())
	defer cancel()
	if err := b.ShutdownWebSockets(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		receiveWSTest(t, done)
	}
	inferenceEmpty(t, b.wsRegistry, b.wsBudget)
}
