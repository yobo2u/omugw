package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	dsi "github.com/yobo2u/omugw/internal/protocol/dashscopeinference"
	"github.com/yobo2u/omugw/internal/provider"
	dsiws "github.com/yobo2u/omugw/internal/provider/dashscopeinference"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

func inferenceHandlerDeps(t *testing.T, url string) WSDeps {
	t.Helper()
	d := wsHandlerDeps(t, url)
	d.Matrix = inferenceBuildMatrix(t, false, inferenceTestCaps())
	target := router.Target{Kind: degrade.ProviderDashScopeWSInference, Endpoint: "ep", BaseURL: url, CredentialPool: "pool"}
	d.InferenceTarget = &target
	d.Router = inferenceHandlerRouter(t, target)
	d.Timeouts = config.Timeouts{Connect: 80 * time.Millisecond, FirstByte: time.Second, Idle: 2 * time.Second, Total: 3 * time.Second}
	inferenceRefreshTransport(&d)
	return d
}

func inferenceHandlerRouter(t *testing.T, target router.Target) *router.Router {
	t.Helper()
	var rules []router.Rule
	for _, model := range []string{inferenceASR, inferenceTTS, "sambert-zhichu-v1", "multimodal-dialog", "tingwu-meeting-realtime"} {
		target.UpstreamModel = model
		rules = append(rules, router.Rule{Match: model, Targets: []router.Target{target}})
	}
	rt, err := router.New(rules)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func inferenceRefreshTransport(d *WSDeps) {
	d.Providers["ep"] = dsiws.New(d.Timeouts, d.Limits, d.Budget)
	d.Registry = newWSRegistry(d.Limits.MaxSessions, wsCloseBudget(d.Timeouts))
}

func inferenceRequest() *http.Request {
	r := wsHandlerRequest()
	r.URL.Path, r.URL.RawQuery = "/api-ws/v1/inference", ""
	return r
}

// 外部云端只由字面 TCP 对端替代；Provider、鉴权、路由、矩阵、policy、relay、Lease 均为正式实现。
type inferenceNetwork struct {
	d                WSDeps
	reg              *prometheus.Registry
	client, upstream *wsTestPeer
	done             chan struct{}
	session          *wsSession
	calls            atomic.Int32
	header           http.Header
}

func inferenceRawDial(t *testing.T, url, path, key string) *wsTestPeer {
	t.Helper()
	raw, err := net.DialTimeout("tcp", strings.TrimPrefix(url, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	_ = raw.SetDeadline(time.Now().Add(time.Second + wsSchedulingSlack()))
	_, err = fmt.Fprintf(raw, "GET %s HTTP/1.1\r\nHost: local\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nAuthorization: Bearer %s\r\nX-DashScope-Workspace: tenant-one\r\nCookie: private=synthetic\r\nOrigin: https://client.invalid\r\n\r\n", path, key)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal("握手不应等待 run 才可能触发的 task-started:", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 101 {
		t.Fatalf("升级状态=%d", resp.StatusCode)
	}
	_ = raw.SetDeadline(time.Time{})
	return &wsTestPeer{Conn: raw, reader: br, masked: true}
}

func startInferenceNetwork(t *testing.T, change func(*WSDeps), wrap func(net.Conn) net.Conn) *inferenceNetwork {
	t.Helper()
	x := &inferenceNetwork{reg: prometheus.NewRegistry(), done: make(chan struct{})}
	type accepted struct {
		peer   *wsTestPeer
		header http.Header
	}
	up := make(chan accepted, 4)
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.calls.Add(1)
		if r.URL.Path != "/api-ws/v1/inference" || r.URL.RawQuery != "" || r.URL.ForceQuery {
			t.Error("固定无模型上游 URL 被改写")
		}
		hj := &wsTestHijacker{ResponseWriter: w}
		_, err := ws.Accept(hj, r, ws.AcceptOptions{MaxPayload: 4 << 20, WriteTimeout: time.Second})
		if err != nil {
			return
		}
		up <- accepted{&wsTestPeer{Conn: hj.raw, reader: bufio.NewReader(hj.raw)}, r.Header.Clone()}
	}))
	x.d = inferenceHandlerDeps(t, u.URL)
	if change != nil {
		change(&x.d)
	}
	inferenceRefreshTransport(&x.d)
	x.d.Metrics = obs.NewMetrics(x.reg)
	h := NewDashScopeInferenceHandler(x.d)
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(x.done)
		h.ServeHTTP(&wsTestHijacker{ResponseWriter: w, wrap: wrap}, r)
	}))
	// 失败断言也先断物理对端、有限 Shutdown/join，防 httptest.Close 掩盖泄漏而永远挂起。
	t.Cleanup(func() {
		if x.client != nil {
			_ = x.client.Close()
		}
		if x.upstream != nil {
			_ = x.upstream.Close()
		}
		for len(up) > 0 {
			_ = (<-up).peer.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), x.d.Timeouts.FirstByte+2*wsCloseBudget(x.d.Timeouts)+wsSchedulingSlack())
		defer cancel()
		if err := x.d.Registry.Shutdown(ctx); err != nil {
			t.Error("清理未 join:", err)
		}
		select {
		case <-x.done:
		case <-ctx.Done():
			t.Error("handler 未退出")
		}
		g.Close()
		u.Close()
		inferenceEmpty(t, x.d.Registry, x.d.Budget)
	})
	x.client = inferenceRawDial(t, g.URL, "/api-ws/v1/inference", "synthetic-key")
	a := receiveWSTest(t, up)
	x.upstream, x.header = a.peer, a.header
	x.d.Registry.mu.Lock()
	for s := range x.d.Registry.sessions {
		x.session = s
	}
	x.d.Registry.mu.Unlock()
	if x.session == nil {
		t.Fatal("活跃连接未登记")
	}
	return x
}

func inferenceEmpty(t *testing.T, r *wsRegistry, b *ws.BufferBudget) {
	t.Helper()
	r.mu.Lock()
	n := len(r.sessions)
	r.mu.Unlock()
	if n != 0 || b.Used() != 0 {
		t.Errorf("未排空: sessions=%d bytes=%d", n, b.Used())
	}
}

func inferenceRead(t *testing.T, p *wsTestPeer) ws.Frame {
	t.Helper()
	for {
		f := p.read(t)
		if f.Opcode == ws.OpPing {
			p.send(t, ws.OpPong, f.Payload)
			continue
		}
		if f.Opcode == ws.OpPong {
			continue
		}
		return f
	}
}

func inferenceWire(t *testing.T, p *wsTestPeer, op ws.Opcode, want []byte) {
	t.Helper()
	f := inferenceRead(t, p)
	if f.Opcode != op || !bytes.Equal(f.Payload, want) {
		t.Fatalf("未原样交付: op=%d payload=%q want=%q", f.Opcode, f.Payload, want)
	}
}

func inferenceWireClose(t *testing.T, p *wsTestPeer, code uint16, reason string) {
	t.Helper()
	f := inferenceRead(t, p)
	if f.Opcode != ws.OpClose || !bytes.Equal(f.Payload, ws.EncodeClosePayload(code, reason)) {
		t.Fatalf("关闭不符: op=%d payload=%q want=%d/%s", f.Opcode, f.Payload, code, reason)
	}
	// 对端可能已在同一绝对期限强制释放；回声只负责尽力结束握手，不伪造发送证据。
	_ = ws.WriteFrame(p.Conn, ws.Frame{FIN: true, Opcode: ws.OpClose, Payload: f.Payload}, p.masked)
}

func (x *inferenceNetwork) roundtrip(t *testing.T, upstream bool, op ws.Opcode, raw []byte) {
	t.Helper()
	a, b := x.client, x.upstream
	if upstream {
		a, b = b, a
	}
	a.send(t, op, raw)
	inferenceWire(t, b, op, raw)
}

func (x *inferenceNetwork) start(t *testing.T, id, model, task, streaming string) {
	t.Helper()
	x.roundtrip(t, false, ws.OpText, inferenceRun(id, model, task, streaming))
	x.roundtrip(t, true, ws.OpText, inferenceServer(id, "task-started", `{}`))
}

func (x *inferenceNetwork) join(t *testing.T, outcome string) {
	t.Helper()
	select {
	case <-x.done:
	case <-time.After(2*wsCloseBudget(x.d.Timeouts) + wsSchedulingSlack()):
		t.Fatal("终止超预算未 join")
	}
	inferenceEmpty(t, x.d.Registry, x.d.Budget)
	if x.calls.Load() != 1 {
		t.Fatal("升级后重拨")
	}
	stats := x.d.Pools["pool"].Stats()
	if stats[0].Picks != 1 || stats[1].Picks != 0 || stats[0].ConsecutiveFails != 0 || !stats[0].Available {
		t.Fatal("Lease 借用/未知失败冷却不符", stats)
	}
	if wsUsageMetricSum(t, x.reg, "omugw_requests_total", map[string]string{"inbound": "dashscope.inference", "outcome": outcome}) != 1 || wsUsageMetricSum(t, x.reg, "omugw_requests_total", nil) != 1 {
		t.Fatal("请求结果被覆盖或重复结算")
	}
}

func TestInferenceHandlerHandshakeAndBinding(t *testing.T) {
	t.Run("就绪模式不能错绑或伪造ready", func(t *testing.T) {
		d := inferenceHandlerDeps(t, "http://127.0.0.1:0")
		for _, mode := range []wsReadyMode{wsReadyEvent, wsReadyMode(99)} {
			h := NewDashScopeInferenceHandler(d)
			h.profile.readyMode = mode
			w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
			h.ServeHTTP(w, inferenceRequest())
			if w.Code != 500 || w.calls != 0 {
				t.Fatal("错模式未在升级前拒绝")
			}
		}
		h := NewOpenAIRealtimeHandler(d)
		h.profile.readyMode = wsReadyHandshake
		w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
		h.ServeHTTP(w, openAITestRequest())
		if w.Code != 500 || w.calls != 0 {
			t.Fatal("Realtime借用了握手即就绪")
		}
		if d.Pools["pool"].Stats()[0].Picks != 0 {
			t.Fatal("错profile仍借用凭据")
		}
	})
	t.Run("预检及整门零Dial", func(t *testing.T) {
		var calls atomic.Int32
		u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(500) }))
		defer u.Close()
		for _, tc := range []struct {
			name   string
			change func(*http.Request, *WSDeps)
			status int
		}{
			{"model", func(r *http.Request, _ *WSDeps) { r.URL.RawQuery = "model=" + inferenceASR }, 400},
			{"query", func(r *http.Request, _ *WSDeps) { r.URL.RawQuery = "unknown=1" }, 400},
			{"空问号", func(r *http.Request, _ *WSDeps) { r.URL.ForceQuery = true }, 400},
			{"无鉴权", func(r *http.Request, _ *WSDeps) { r.Header.Del("Authorization") }, 401},
			{"重复租户", func(r *http.Request, _ *WSDeps) { r.Header["X-Dashscope-Workspace"] = []string{"a", "b"} }, 400},
			{"子协议", func(r *http.Request, _ *WSDeps) { r.Header.Set("Sec-WebSocket-Protocol", "private-token") }, 400},
			{"未投放", func(_ *http.Request, d *WSDeps) { d.Matrix, _ = degrade.Phase1() }, 501},
			{"部分批准", func(_ *http.Request, d *WSDeps) { d.Matrix = inferenceBuildMatrix(t, false, inferenceTestCaps()[:5]) }, 501},
			{"无固定目标", func(_ *http.Request, d *WSDeps) { d.InferenceTarget = nil }, 500},
			{"错固定身份", func(_ *http.Request, d *WSDeps) { d.InferenceTarget.Kind = degrade.ProviderOpenAIRealtime }, 500},
			{"握手带模型", func(_ *http.Request, d *WSDeps) { d.InferenceTarget.UpstreamModel = inferenceASR }, 500},
		} {
			t.Run(tc.name, func(t *testing.T) {
				d, r := inferenceHandlerDeps(t, u.URL), inferenceRequest()
				tc.change(r, &d)
				w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
				NewDashScopeInferenceHandler(d).ServeHTTP(w, r)
				if w.Code != tc.status || w.calls != 0 || calls.Load() != 0 {
					t.Fatalf("status=%d hijack=%d dial=%d", w.Code, w.calls, calls.Load())
				}
				for _, st := range d.Pools["pool"].Stats() {
					if st.Picks != 0 {
						t.Fatal("预检借用凭据")
					}
				}
				if !strings.Contains(w.Body.String(), `"code"`) {
					t.Fatal("没有 DashScope 错误信封")
				}
				inferenceEmpty(t, d.Registry, d.Budget)
			})
		}
	})
	t.Run("无ready即可101且鉴权替换", func(t *testing.T) {
		x := startInferenceNetwork(t, nil, nil)
		if x.header.Get("Authorization") != "Bearer synthetic-a" || x.header.Get("X-DashScope-Workspace") != "tenant-one" || x.header.Get("Origin") != "" || x.header.Get("Cookie") != "" {
			t.Fatal("鉴权/租户头串线")
		}
		x.start(t, "a", inferenceASR, "asr", "duplex")
		x.roundtrip(t, false, ws.OpBinary, []byte{0, 255, 128})
		x.roundtrip(t, true, ws.OpText, inferenceServer("a", "task-finished", `{"usage":{"duration":2}}`))
		x.client.send(t, ws.OpClose, ws.EncodeClosePayload(1000, "done"))
		inferenceWireClose(t, x.upstream, 1000, "done")
		x.join(t, "ok")
	})
	for _, kind := range []string{"missing", "alias", "kind", "endpoint", "url", "pool", "native", "multimodal-dialog", "tingwu-meeting-realtime"} {
		t.Run("模型拒绝零应用发送/"+kind, func(t *testing.T) {
			model := inferenceASR
			if strings.Contains(kind, "dialog") || strings.Contains(kind, "tingwu") {
				model = kind
			}
			x := startInferenceNetwork(t, func(d *WSDeps) {
				target := *d.InferenceTarget
				target.UpstreamModel = model
				switch kind {
				case "missing":
					target.UpstreamModel = "other"
				case "alias":
					target.UpstreamModel = "real-other"
				case "kind":
					target.Kind = degrade.ProviderOpenAIRealtime
				case "endpoint":
					target.Endpoint = "other"
				case "url":
					target.BaseURL += "/other"
				case "pool":
					target.CredentialPool = "tenant-two"
				case "native":
					target.NativeEndpoint = "text-generation"
				}
				match := model
				if kind == "missing" {
					match = "other"
				}
				var err error
				d.Router, err = router.New([]router.Rule{{Match: match, Targets: []router.Target{target}}})
				if err != nil {
					t.Fatal(err)
				}
			}, nil)
			x.client.send(t, ws.OpText, inferenceRun("rejected", model, "asr", "duplex"))
			code := "Gateway.InvalidTask"
			if kind == "multimodal-dialog" || kind == "tingwu-meeting-realtime" {
				code = "Gateway.UnsupportedTask"
			}
			inferenceLocalFailure(t, x.client, "rejected", code)
			inferenceWireClose(t, x.client, 1008, "invalid task envelope or binding")
			// 若任何 run 已穿透，本读取会先取得 text 而不是 close，严格失败。
			inferenceWireClose(t, x.upstream, 1008, "invalid task envelope or binding")
			x.join(t, "internal")
		})
	}
}

func inferenceLocalFailure(t *testing.T, p *wsTestPeer, id, code string) {
	t.Helper()
	f := inferenceRead(t, p)
	var body struct {
		Header struct {
			Event string `json:"event"`
			ID    string `json:"task_id"`
			Code  string `json:"error_code"`
		} `json:"header"`
	}
	if f.Opcode != ws.OpText || json.Unmarshal(f.Payload, &body) != nil || body.Header.Event != "task-failed" || body.Header.ID != id || body.Header.Code != code {
		t.Fatalf("没有准确本地 task-failed: %q", f.Payload)
	}
}

func TestInferenceHandlerLifecycle(t *testing.T) {
	t.Run("成功后次轮failed加1000", func(t *testing.T) {
		x := startInferenceNetwork(t, nil, nil)
		x.start(t, "a", inferenceTTS, "tts", "duplex")
		x.roundtrip(t, true, ws.OpText, inferenceServer("a", "task-finished", `{"usage":{"characters":13}}`))
		x.start(t, "b", inferenceTTS, "tts", "duplex")
		failed := []byte(` {"header":{"event":"task-failed","task_id":"b","error_code":"UnknownQuota","error_message":"private auth quota"},"payload":{"usage":{"characters":6}},"future":false} `)
		x.upstream.send(t, ws.OpText, failed)
		x.upstream.send(t, ws.OpClose, ws.EncodeClosePayload(1000, "peer normal"))
		inferenceWire(t, x.client, ws.OpText, failed)
		inferenceWireClose(t, x.client, 1000, "peer normal")
		x.join(t, "internal")
		if wsUsageMetricSum(t, x.reg, "omugw_ws_characters_total", nil) != 19 || wsUsageMetricSum(t, x.reg, "omugw_ws_usage_records_total", map[string]string{"fidelity": "authoritative"}) != 2 || wsUsageMetricSum(t, x.reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "task_failed"}) != 1 {
			t.Fatal("多轮结算/失败诊断不符")
		}
	})
	t.Run("finished后断线保权威", func(t *testing.T) {
		x := startInferenceNetwork(t, nil, nil)
		x.start(t, "a", inferenceASR, "asr", "duplex")
		x.roundtrip(t, true, ws.OpText, inferenceServer("a", "task-finished", `{"usage":{"duration":6}}`))
		_ = x.upstream.Close()
		inferenceWireClose(t, x.client, 1011, "upstream connection failed")
		x.join(t, "internal")
		if wsUsageMetricSum(t, x.reg, "omugw_ws_audio_input_seconds_total", nil) != 6 || wsUsageMetricSum(t, x.reg, "omugw_ws_usage_records_total", map[string]string{"fidelity": "authoritative"}) != 1 {
			t.Fatal("已结 usage 被断流清掉")
		}
	})
	for _, phase := range []string{"start", "drain", "out"} {
		t.Run("绝对阶段超时/"+phase, func(t *testing.T) {
			x := startInferenceNetwork(t, func(d *WSDeps) {
				d.Timeouts = config.Timeouts{Connect: 80 * time.Millisecond, FirstByte: 160 * time.Millisecond, Idle: 160 * time.Millisecond, Total: 300 * time.Millisecond}
			}, nil)
			start := time.Now()
			code, reason := "Gateway.TaskStartTimeout", "task_start_timeout"
			if phase == "out" {
				x.start(t, "a", "sambert-zhichu-v1", "tts", "out")
			} else if phase == "drain" {
				x.start(t, "a", inferenceASR, "asr", "duplex")
				start = time.Now()
				x.roundtrip(t, false, ws.OpText, inferenceClient("a", "finish-task", `{}`))
			} else {
				x.roundtrip(t, false, ws.OpText, inferenceRun("a", inferenceASR, "asr", "duplex"))
			}
			if phase != "start" {
				code, reason = "Gateway.TaskDrainTimeout", "task_drain_timeout"
			}
			// 双端持续读取并响应心跳，证明终止来自业务期限而非 transport idle。
			upDone := make(chan struct{})
			go func() {
				defer close(upDone)
				for {
					f, err := ws.ReadFrame(x.upstream.reader, 1<<20)
					if err != nil {
						return
					}
					if f.Opcode == ws.OpPing {
						_ = ws.WriteFrame(x.upstream.Conn, ws.Frame{FIN: true, Opcode: ws.OpPong, Payload: f.Payload}, false)
					}
					if f.Opcode == ws.OpClose {
						_ = ws.WriteFrame(x.upstream.Conn, f, false)
						return
					}
				}
			}()
			t.Cleanup(func() { _ = x.upstream.Close(); awaitWSTest(t, upDone) })
			inferenceLocalFailure(t, x.client, "a", code)
			inferenceWireClose(t, x.client, 1011, reason)
			x.join(t, "upstream_unavailable")
			awaitWSTest(t, upDone)
			if time.Since(start) > 320*time.Millisecond+wsSchedulingSlack() {
				t.Fatal("阶段或关闭期限续时")
			}
			if wsUsageMetricSum(t, x.reg, "omugw_ws_diagnostics_total", map[string]string{"reason": reason}) != 1 {
				t.Fatal("超时诊断不是一次")
			}
		})
	}
	t.Run("active跨HTTPtotal并关停", func(t *testing.T) {
		x := startInferenceNetwork(t, func(d *WSDeps) {
			d.Timeouts = config.Timeouts{Connect: 80 * time.Millisecond, FirstByte: 160 * time.Millisecond, Idle: 160 * time.Millisecond, Total: 200 * time.Millisecond}
		}, nil)
		x.start(t, "a", inferenceASR, "asr", "duplex")
		// 定时器仅验证 total 边界，不用它安排竞争；双端 reader 始终回 pong。
		finished := make(chan struct{}, 2)
		for _, p := range []*wsTestPeer{x.client, x.upstream} {
			go func() {
				defer func() { finished <- struct{}{} }()
				for {
					f, err := ws.ReadFrame(p.reader, 1<<20)
					if err != nil {
						return
					}
					if f.Opcode == ws.OpPing {
						_ = ws.WriteFrame(p.Conn, ws.Frame{FIN: true, Opcode: ws.OpPong, Payload: f.Payload}, p.masked)
					}
					if f.Opcode == ws.OpClose {
						_ = ws.WriteFrame(p.Conn, f, p.masked)
						return
					}
				}
			}()
		}
		t.Cleanup(func() {
			_ = x.client.Close()
			_ = x.upstream.Close()
			for range 2 {
				awaitWSTest(t, finished)
			}
		})
		timer := time.NewTimer(2 * x.d.Timeouts.Total)
		defer timer.Stop()
		select {
		case <-x.done:
			t.Fatal("HTTP total 截断 active")
		case <-timer.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond+wsSchedulingSlack())
		defer cancel()
		if err := x.d.Registry.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
		x.join(t, "cancelled")
	})
}

// Done 只在真实 policy 的等待 select 被取用时发信号，不能用 sleep 猜 worker 已到门闩。
type inferenceWaitContext struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (c *inferenceWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

type inferencePolicyProbe struct {
	*wsInferencePolicy
	waitFor  []byte
	waiting  chan struct{}
	after    chan wsForwardTicket
	finished atomic.Int32
}

func (p *inferencePolicyProbe) BeforeForward(ctx context.Context, dir wsDirection, op ws.Opcode, raw []byte) (wsForwardDecision, error) {
	if dir == wsClientToUpstream && bytes.Equal(raw, p.waitFor) {
		ctx = &inferenceWaitContext{Context: ctx, waiting: p.waiting}
	}
	return p.wsInferencePolicy.BeforeForward(ctx, dir, op, raw)
}
func (p *inferencePolicyProbe) AfterForward(ticket wsForwardTicket, err error) {
	p.wsInferencePolicy.AfterForward(ticket, err)
	p.after <- ticket
}
func (p *inferencePolicyProbe) Finish() { p.wsInferencePolicy.Finish(); p.finished.Add(1) }

func TestInferenceHandlerDeliveryBarriers(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		for _, stop := range []bool{false, true} {
			t.Run(fmt.Sprintf("terminal=%v/shutdown=%v", terminal, stop), func(t *testing.T) {
				binding, err := newWSInferenceBinding(inferenceTarget())
				if err != nil {
					t.Fatal(err)
				}
				reg := newWSRegistry(1, 80*time.Millisecond)
				s, err := reg.Register(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				b := wsTestBudget(t, 1<<20)
				var gate *wsTestGate
				down, client := wsTestLink(t, false, b, 1<<18, 0, func(c net.Conn) net.Conn { gate = newWSTestGate(c, false); return gate })
				up, server := wsTestLink(t, true, b, 1<<18, 0, nil)
				if !s.Attach(down) || !s.Attach(up) {
					t.Fatal("未挂接")
				}
				timeouts := config.Timeouts{Connect: 80 * time.Millisecond, FirstByte: time.Second, Idle: 2 * time.Second}
				p := &inferencePolicyProbe{wsInferencePolicy: newWSInferencePolicy(binding, inferenceRouter(t), inferenceMatrix(t, true), nil, timeouts, nil), waiting: make(chan struct{}), after: make(chan wsForwardTicket, 16)}
				p.waitFor = []byte{0, 255, 128}
				event := "task-started"
				if terminal {
					p.waitFor = inferenceRun("b", inferenceASR, "asr", "duplex")
					event = "task-finished"
				}
				done := make(chan error, 1)
				exited := make(chan struct{})
				go func() {
					defer close(exited)
					result := relayWS(s.Context(), down, up, nil, wsRelayOptions{Policy: p, ClassifyClose: dsi.ClassifyClose, Timeouts: timeouts, Budget: b, MaxMessageBytes: 1 << 18})
					s.Done()
					done <- result
				}()
				t.Cleanup(func() {
					gate.unblock()
					_ = client.Close()
					_ = server.Close()
					ctx, cancel := context.WithTimeout(context.Background(), 160*time.Millisecond+wsSchedulingSlack())
					defer cancel()
					if err := reg.Shutdown(ctx); err != nil {
						t.Error(err)
					}
					select {
					case <-exited:
					case <-ctx.Done():
						t.Error("门闩 worker 未 join")
					}
					inferenceEmpty(t, reg, b)
				})
				run := inferenceRun("a", inferenceASR, "asr", "duplex")
				client.send(t, ws.OpText, run)
				inferenceWire(t, server, ws.OpText, run)
				_ = receiveWSTest(t, p.after)
				if terminal {
					started := inferenceServer("a", "task-started", `{}`)
					server.send(t, ws.OpText, started)
					inferenceWire(t, client, ws.OpText, started)
					_ = receiveWSTest(t, p.after)
				}
				gate.armed.Store(true)
				raw := inferenceServer("a", event, `{}`)
				server.send(t, ws.OpText, raw)
				awaitWSTest(t, gate.entered)
				op := ws.OpBinary
				if terminal {
					op = ws.OpText
				}
				client.send(t, op, p.waitFor)
				awaitWSTest(t, p.waiting)
				// 门闩里仅持有该方向一条已计额消息；After 未发生不能发输入或下一 run。
				if b.Used() != int64(len(raw)+len(p.waitFor)) {
					t.Fatalf("等待时消息所有权不符: %d", b.Used())
				}
				select {
				case <-p.after:
					t.Fatal("写完成前提前 After")
				default:
				}
				if stop {
					start := time.Now()
					ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond+wsSchedulingSlack())
					defer cancel()
					if err := reg.Shutdown(ctx); err != nil {
						t.Fatal(err)
					}
					if err := receiveWSTest(t, done); !errors.Is(err, errWSShutdown) {
						t.Fatal("Stop 未保持关停结果", err)
					}
					if time.Since(start) > 80*time.Millisecond+wsSchedulingSlack() {
						t.Fatal("等交付锁重开关闭预算")
					}
					// 上游除 run 外只能收到1001，不能因迟到成功回执把下一输入放行。
					inferenceWireClose(t, server, 1001, "")
				} else {
					gate.unblock()
					inferenceWire(t, client, ws.OpText, raw)
					inferenceWire(t, server, op, p.waitFor)
					client.send(t, ws.OpClose, ws.EncodeClosePayload(1000, ""))
					inferenceWireClose(t, server, 1000, "")
					if err := receiveWSTest(t, done); err != nil {
						t.Fatal(err)
					}
				}
				if p.finished.Load() != 1 {
					t.Fatal("Finish 不是 join 后一次")
				}
				inferenceEmpty(t, reg, b)
			})
		}
	}
}

func TestInferenceHandlerFailedStopTailAndHistory(t *testing.T) {
	for _, history := range []bool{false, true} {
		t.Run(fmt.Sprintf("history=%v", history), func(t *testing.T) {
			var gate *wsTestGate
			x := startInferenceNetwork(t, nil, func(c net.Conn) net.Conn { gate = newWSTestGate(c, false); return gate })
			t.Cleanup(gate.unblock)
			x.start(t, "a", inferenceTTS, "tts", "duplex")
			x.roundtrip(t, true, ws.OpText, inferenceServer("a", "task-finished", `{"usage":{"characters":13}}`))
			x.start(t, "b", inferenceTTS, "tts", "duplex")
			gate.armed.Store(true)
			failed := []byte(`{"header":{"event":"task-failed","task_id":"b","error_code":"Unknown"},"payload":{"usage":{"characters":6}}}`)
			x.upstream.send(t, ws.OpText, failed)
			awaitWSTest(t, gate.entered)
			term := x.session.shutdown.termination
			awaitWSTest(t, term.selected)
			term.mu.Lock()
			policy := term.options.Policy.(*wsInferencePolicy)
			deadline := term.deadline
			term.mu.Unlock()
			policy.Stop()
			if history {
				x.upstream.send(t, ws.OpText, inferenceServer("a", "task-finished", `{"usage":{"characters":13}}`))
			} else {
				x.upstream.send(t, ws.OpBinary, []byte{0, 255, 128})
				x.upstream.send(t, ws.OpText, inferenceServer("b", "future-event", `{"future":[null,false,0,{}]}`))
				x.upstream.send(t, ws.OpClose, ws.EncodeClosePayload(4003, "tail close"))
			}
			gate.unblock()
			inferenceWire(t, x.client, ws.OpText, failed)
			if history {
				// 旧 ID 不属于失败任务尾消息；若错误保全，本处会先读到历史 text 而非close。
				inferenceWireClose(t, x.client, 1011, "upstream task failed")
			} else {
				inferenceWire(t, x.client, ws.OpBinary, []byte{0, 255, 128})
				inferenceWire(t, x.client, ws.OpText, inferenceServer("b", "future-event", `{"future":[null,false,0,{}]}`))
				inferenceWireClose(t, x.client, 4003, "tail close")
			}
			x.join(t, "internal")
			term.mu.Lock()
			final := term.deadline
			term.mu.Unlock()
			if final.After(deadline) {
				t.Fatal("尾消息重开关闭预算")
			}
			if wsUsageMetricSum(t, x.reg, "omugw_ws_characters_total", nil) != 19 || wsUsageMetricSum(t, x.reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "task_failed"}) != 1 {
				t.Fatal("Stop后重结失败")
			}
		})
	}
}

func TestInferenceHandlerFailedShutdownAndWriteError(t *testing.T) {
	for _, mode := range []string{"shutdown", "write-error", "slow-delivery"} {
		t.Run(mode, func(t *testing.T) {
			var gate *wsTestGate
			x := startInferenceNetwork(t, nil, func(c net.Conn) net.Conn { gate = newWSTestGate(c, false); return gate })
			t.Cleanup(gate.unblock)
			x.start(t, "a", inferenceTTS, "tts", "duplex")
			gate.armed.Store(true)
			failed := []byte(`{"header":{"event":"task-failed","task_id":"a","error_code":"Unknown"},"payload":{"usage":{"characters":6}}}`)
			start := time.Now()
			x.upstream.send(t, ws.OpText, failed)
			awaitWSTest(t, gate.entered)
			term := x.session.shutdown.termination
			awaitWSTest(t, term.selected)
			term.mu.Lock()
			deadline, at := term.deadline, term.end.At
			term.mu.Unlock()
			if deadline != at.Add(160*time.Millisecond) {
				t.Fatal("failed 未在写前认领并覆盖慢交付")
			}
			if mode == "write-error" {
				// 先核对认领时的 Dall，再用真实 socket 关闭造成写错，避免即时 fake 错误与收紧期限抢跑。
				_ = gate.Conn.Close()
				gate.unblock()
			}
			if mode == "shutdown" {
				stopped := make(chan error, 1)
				stopDone := make(chan struct{})
				go func() {
					defer close(stopDone)
					ctx, cancel := context.WithTimeout(context.Background(), 160*time.Millisecond+wsSchedulingSlack())
					defer cancel()
					stopped <- x.d.Registry.Shutdown(ctx)
				}()
				t.Cleanup(func() { gate.unblock(); awaitWSTest(t, stopDone) })
				awaitWSTest(t, x.session.shutdown.started)
				x.upstream.send(t, ws.OpClose, ws.EncodeClosePayload(1000, "peer normal"))
				gate.unblock()
				inferenceWire(t, x.client, ws.OpText, failed)
				inferenceWireClose(t, x.client, 1000, "peer normal")
				if err := receiveWSTest(t, stopped); err != nil {
					t.Fatal(err)
				}
			}
			x.join(t, "internal")
			if time.Since(start) > 160*time.Millisecond+wsSchedulingSlack() {
				t.Fatal("失败关闭超出同一 Dall")
			}
			term.mu.Lock()
			end := term.deadline
			term.mu.Unlock()
			if end.After(deadline) {
				t.Fatal("交付失败/关停延长期限")
			}
			if wsUsageMetricSum(t, x.reg, "omugw_ws_characters_total", nil) != 6 || wsUsageMetricSum(t, x.reg, "omugw_ws_usage_records_total", nil) != 1 || wsUsageMetricSum(t, x.reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "task_failed"}) != 1 {
				t.Fatal("原文写失败抹掉业务结果或重复结账")
			}
		})
	}
}

// 只记录真实 Provider 的调用参数；不替代 Dial、HTTP 分类、连接或首次字节预算。
type inferenceDialProbe struct {
	provider.StreamProvider
	mu        sync.Mutex
	deadlines []time.Time
	ids       []string
}

func (p *inferenceDialProbe) Dial(ctx context.Context, r provider.Request) (*ws.Conn, *http.Response, error) {
	d, _ := ctx.Deadline()
	p.mu.Lock()
	p.deadlines = append(p.deadlines, d)
	p.ids = append(p.ids, r.Credential.ID)
	p.mu.Unlock()
	return p.StreamProvider.Dial(ctx, r)
}

func TestInferenceHandlerHandshakeBudgetAndPending(t *testing.T) {
	for _, mode := range []string{"deadline101", "shutdown101", "pending"} {
		t.Run(mode, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var calls atomic.Int32
			up := make(chan net.Conn, 1)
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if n == 1 && mode != "pending" {
					w.WriteHeader(401)
					_, _ = w.Write([]byte(`{"code":"InvalidApiKey","message":"synthetic"}`))
					return
				}
				if mode == "pending" {
					close(entered)
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				hj := &wsTestHijacker{ResponseWriter: w}
				_, err := ws.Accept(hj, r, ws.AcceptOptions{})
				if err == nil {
					up <- hj.raw
				}
			}))
			d := inferenceHandlerDeps(t, u.URL)
			d.Timeouts.FirstByte = 160 * time.Millisecond
			inferenceRefreshTransport(&d)
			probe := &inferenceDialProbe{StreamProvider: d.Providers["ep"]}
			d.Providers["ep"] = probe
			h := NewDashScopeInferenceHandler(d)
			blocked := make(chan *wsStalled101, 1)
			done := make(chan struct{})
			g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				h.ServeHTTP(&wsTestHijacker{ResponseWriter: w, wrap: func(c net.Conn) net.Conn {
					b := &wsStalled101{Conn: c, entered: make(chan struct{}), unblock: make(chan struct{})}
					blocked <- b
					return b
				}}, r)
			}))
			clientDone := make(chan struct{})
			start := time.Now()
			go func() {
				defer close(clientDone)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				c, resp, _ := ws.Dial(ctx, "ws"+strings.TrimPrefix(g.URL, "http")+"/api-ws/v1/inference", ws.DialOptions{Header: http.Header{"Authorization": {"Bearer synthetic-key"}}})
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				if c != nil {
					_ = c.Close(1001, "")
				}
			}()
			t.Cleanup(func() {
				once.Do(func() { close(release) })
				for len(up) > 0 {
					_ = (<-up).Close()
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = d.Registry.Shutdown(ctx)
				awaitWSTest(t, done)
				awaitWSTest(t, clientDone)
				g.Close()
				u.Close()
				inferenceEmpty(t, d.Registry, d.Budget)
			})
			if mode == "pending" {
				awaitWSTest(t, entered)
			} else {
				b := receiveWSTest(t, blocked)
				t.Cleanup(func() { _ = b.Close() })
				awaitWSTest(t, b.entered)
			}
			if mode != "deadline101" {
				ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond+wsSchedulingSlack())
				defer cancel()
				if err := d.Registry.Shutdown(ctx); err != nil {
					t.Fatal(err)
				}
			}
			awaitWSTest(t, done)
			awaitWSTest(t, clientDone)
			if time.Since(start) > 240*time.Millisecond+wsSchedulingSlack() {
				t.Fatal("候选/下游101不共用绝对 firstByte")
			}
			want := 1
			if mode != "pending" {
				want = 2
			}
			if int(calls.Load()) != want {
				t.Fatal("升级承诺后仍重试", calls.Load())
			}
			probe.mu.Lock()
			ds, ids := append([]time.Time(nil), probe.deadlines...), append([]string(nil), probe.ids...)
			probe.mu.Unlock()
			if len(ds) != want || len(ids) != want || ids[0] != "a" {
				t.Fatal("错误凭据循环")
			}
			if want == 2 && (ds[0] != ds[1] || ids[1] != "b") {
				t.Fatal("凭据循环重置 firstByte")
			}
			stats := d.Pools["pool"].Stats()
			if want == 2 && stats[0].ConsecutiveFails != 1 || stats[want-1].ConsecutiveFails != 0 || stats[want-1].Picks != 1 {
				t.Fatal("pending或101错误冷却凭据", stats)
			}
		})
	}
}

func TestInferenceHandlerFailedLeaseDoesNotSucceed(t *testing.T) {
	// 先失败再推进可用时间：同代旧失败若被错误 Succeed 会清零，单看未知失败不冷却抓不到它。
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	pool, err := credential.NewPool("pool", []credential.Credential{{ID: "a", Secret: "synthetic-a"}, {ID: "b", Secret: "synthetic-b", Priority: 1}}, credential.DefaultPolicy(), func() time.Time { return time.Unix(0, clock.Load()) })
	if err != nil {
		t.Fatal(err)
	}
	l, err := pool.Acquire(map[string]bool{"b": true})
	if err != nil {
		t.Fatal(err)
	}
	l.Fail(canonical.Newf(canonical.ClassRateLimit, "synthetic"))
	clock.Add(int64(time.Hour))
	x := startInferenceNetwork(t, func(d *WSDeps) { d.Pools["pool"] = pool }, nil)
	x.client.send(t, ws.OpText, inferenceRun("a", inferenceTTS, "tts", "duplex"))
	inferenceWire(t, x.upstream, ws.OpText, inferenceRun("a", inferenceTTS, "tts", "duplex"))
	x.upstream.send(t, ws.OpText, []byte(`{"header":{"event":"task-failed","task_id":"a","error_code":"Unknown"},"payload":{}}`))
	inferenceWire(t, x.client, ws.OpText, []byte(`{"header":{"event":"task-failed","task_id":"a","error_code":"Unknown"},"payload":{}}`))
	inferenceWireClose(t, x.client, 1011, "upstream task failed")
	awaitWSTest(t, x.done)
	stats := pool.Stats()
	if stats[0].ConsecutiveFails != 1 || stats[0].Picks != 2 || stats[1].Picks != 0 || !stats[0].Available {
		t.Fatal("failed 被错误 Succeed 或重新冷却", stats)
	}
	if wsUsageMetricSum(t, x.reg, "omugw_requests_total", map[string]string{"outcome": "internal"}) != 1 {
		t.Fatal("业务结果未结算一次")
	}
}
