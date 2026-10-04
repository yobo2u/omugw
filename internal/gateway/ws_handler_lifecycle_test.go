package gateway

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
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
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/provider"
	dsws "github.com/yobo2u/omugw/internal/provider/dashscoperealtime"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

func TestWSHandlerFailoverClientPipeline(t *testing.T) {
	const command = `{"type":"session.update","unknown":[null,false,0]}`
	upDone := make(chan bool, 1)
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: time.Second})
		if err != nil {
			upDone <- false
			return
		}
		defer c.Close(1000, "")
		_ = c.WriteMessage(ws.OpText, []byte(wsTestInitial))
		op, body, err := c.ReadMessage()
		ok := err == nil && op == ws.OpText && string(body) == command && r.Header.Get("Origin") == "" && r.Header.Get("Sec-WebSocket-Extensions") == ""
		if ok {
			ok = c.WriteMessage(ws.OpText, []byte(command)) == nil
		}
		upDone <- ok
		_, _, _ = c.ReadMessage()
	}))
	defer u.Close()
	d := wsHandlerDeps(t, u.URL)
	s, done := wsHandlerServer(t, d)
	raw, err := net.DialTimeout("tcp", strings.TrimPrefix(s.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
	var request bytes.Buffer
	fmt.Fprintf(&request, "GET /api-ws/v1/realtime?model=real-model HTTP/1.1\r\nHost: local\r\nAuthorization: Bearer synthetic-key\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nOrigin: https://client.example\r\nSec-WebSocket-Extensions: permessage-deflate; client_max_window_bits\r\n\r\n")
	if err := ws.WriteFrame(&request, ws.Frame{FIN: true, Opcode: ws.OpText, Payload: []byte(command)}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Write(request.Bytes()); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(raw)
	resp, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal("管线握手失败")
	}
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Extensions") != "" || resp.Header.Get("Sec-WebSocket-Protocol") != "" {
		t.Fatal("Origin/扩展提议被误拒绝或协商了未支持内容")
	}
	for _, want := range []string{wsTestInitial, command} {
		frame, err := ws.ReadFrame(reader, 1<<20)
		if err != nil || frame.Opcode != ws.OpText || string(frame.Payload) != want {
			t.Fatal("就绪消息顺序/管线帧预读丢失或重复")
		}
	}
	if !receiveWSTest(t, upDone) {
		t.Fatal("上游未取得原样管线消息")
	}
	if err := ws.WriteFrame(raw, ws.Frame{FIN: true, Opcode: ws.OpClose, Payload: ws.EncodeClosePayload(1000, "")}, true); err != nil {
		t.Fatal(err)
	}
	awaitWSTest(t, done)
}

func TestWSHandlerLeaseAndShutdownTotalDoesNotLimitSession(t *testing.T) {
	const command = `{"type":"future.event"}`
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, WriteTimeout: time.Second})
		if err != nil {
			return
		}
		defer c.Close(1000, "")
		_ = c.WriteMessage(ws.OpText, []byte(wsTestInitial))
		op, payload, err := c.ReadMessage()
		if err == nil {
			_ = c.WriteMessage(op, payload)
		}
		_, _, _ = c.ReadMessage()
	}))
	defer u.Close()
	d := wsHandlerDeps(t, u.URL)
	d.Timeouts = config.Timeouts{Connect: 100 * time.Millisecond, FirstByte: 200 * time.Millisecond, Total: 250 * time.Millisecond, Idle: 200 * time.Millisecond}
	d.Providers["ep"] = dsws.New(d.Timeouts, d.Limits, d.Budget)
	s, done := wsHandlerServer(t, d)
	c := wsHandlerDial(t, s.URL)
	wsReadLiteral(t, c, wsTestInitial)
	read := make(chan bool, 1)
	go func() {
		op, payload, err := c.ReadMessage()
		read <- err == nil && op == ws.OpText && string(payload) == command
	}()
	time.Sleep(400 * time.Millisecond)
	if err := c.WriteMessage(ws.OpText, []byte(command)); err != nil {
		t.Fatal("HTTP total/握手取消误杀已升级会话")
	}
	if !receiveWSTest(t, read) {
		t.Fatal("idle 心跳或会话超时接线错误")
	}
	_ = c.Close(1000, "")
	awaitWSTest(t, done)
}

func TestWSHandlerLeaseAndShutdownSettlement(t *testing.T) {
	for _, mode := range []string{"success", "old-success", "rate-close", "unknown-close"} {
		t.Run(mode, func(t *testing.T) {
			end := make(chan struct{})
			var calls atomic.Int32
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, WriteTimeout: time.Second})
				if err != nil {
					return
				}
				defer c.Close(1000, "")
				_ = c.WriteMessage(ws.OpText, []byte(wsTestInitial))
				<-end
				switch mode {
				case "rate-close":
					_ = c.Close(1011, "To many requests")
				case "unknown-close":
					_ = c.Close(1011, "auth quota private-token")
				default:
					_ = c.Close(1000, "")
				}
			}))
			defer u.Close()
			defer func() {
				select {
				case <-end:
				default:
					close(end)
				}
			}()
			d := wsHandlerDeps(t, u.URL)
			// 固定时钟推进到冷却后，成功必须清旧计数；而并发较新失败不能被清掉。
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			pool, err := credential.NewPool("pool", []credential.Credential{{ID: "a", Secret: "synthetic-a"}}, credential.DefaultPolicy(), func() time.Time { return time.Unix(0, clock.Load()) })
			if err != nil {
				t.Fatal(err)
			}
			d.Pools["pool"] = pool
			if mode == "success" {
				l, _ := pool.Acquire(nil)
				l.Fail(canonical.Newf(canonical.ClassRateLimit, "synthetic"))
				clock.Add(int64(time.Hour))
			}
			s, done := wsHandlerServer(t, d)
			c := wsHandlerDial(t, s.URL)
			wsReadLiteral(t, c, wsTestInitial)
			if mode == "old-success" {
				l, _ := pool.Acquire(nil)
				l.Fail(canonical.Newf(canonical.ClassRateLimit, "synthetic"))
			}
			close(end)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, readErr := c.ReadOwnedMessage(ctx)
			var closed *ws.CloseError
			if !errors.As(readErr, &closed) {
				t.Fatal("真实结束帧未到达")
			}
			closed.Release()
			awaitWSTest(t, done)
			st := pool.Stats()[0]
			wantFails := 0
			if mode == "old-success" || mode == "rate-close" {
				wantFails = 1
			}
			if st.ConsecutiveFails != wantFails || calls.Load() != 1 {
				t.Fatal("Lease 结算次数、generation 或 101 后禁止重拨失守")
			}
		})
	}
}

func TestWSHandlerHandshakeStopsWhenShutdownSelected(t *testing.T) {
	r := newWSRegistry(1)
	s, err := r.Register(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var gate *wsTestGate
	c, _ := wsTestLink(t, false, wsTestBudget(t, 1<<20), 1<<18, 0, func(c net.Conn) net.Conn { gate = newWSTestGate(c, false); return gate })
	if !s.Attach(c) {
		t.Fatal("连接登记失败")
	}
	gate.armed.Store(true)
	stop := make(chan error, 1)
	go func() { stop <- r.Shutdown(context.Background()) }()
	awaitWSTest(t, gate.entered)
	got := wsHandshakeContextError(s.Context())
	gate.unblock()
	s.Done()
	if err := receiveWSTest(t, stop); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(got, errWSShutdown) {
		t.Fatal("关停已胜出、context 尚未取消时仍允许拨号/凭据冷却")
	}
}

// 预留已经冷却完的失败历史：误走 Succeed 会清零，误冷却会增加，两者均可从真实池观察。
func TestWSHandlerIncompleteCloseSettlement(t *testing.T) {
	for _, upstream := range []bool{false, true} {
		t.Run(fmt.Sprint("upstream=", upstream), func(t *testing.T) {
			peers := make(chan *wsTestPeer, 1)
			var calls atomic.Int32
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				hj := &wsTestHijacker{ResponseWriter: w}
				c, err := ws.Accept(hj, r, ws.AcceptOptions{MaxPayload: 1 << 20, WriteTimeout: time.Second})
				if err != nil {
					t.Error(err)
					return
				}
				t.Cleanup(func() { _ = hj.raw.Close() })
				if err := c.WriteMessage(ws.OpText, []byte(wsTestInitial)); err != nil {
					t.Error(err)
					return
				}
				peers <- &wsTestPeer{Conn: hj.raw, reader: bufio.NewReader(hj.raw)}
			}))
			defer u.Close()
			d := wsHandlerDeps(t, u.URL)
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			pool, err := credential.NewPool("pool", []credential.Credential{{ID: "a", Secret: "synthetic-a"}}, credential.DefaultPolicy(), func() time.Time { return time.Unix(0, clock.Load()) })
			if err != nil {
				t.Fatal(err)
			}
			lease, err := pool.Acquire(nil)
			if err != nil {
				t.Fatal(err)
			}
			lease.Fail(canonical.Newf(canonical.ClassRateLimit, "synthetic"))
			clock.Add(int64(time.Hour))
			before := pool.Stats()[0]
			d.Pools["pool"] = pool
			reg := prometheus.NewRegistry()
			d.Metrics = obs.NewMetrics(reg)
			s, done := wsHandlerServer(t, d)
			raw, err := net.DialTimeout("tcp", strings.TrimPrefix(s.URL, "http://"), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			_ = raw.SetDeadline(time.Now().Add(4 * time.Second))
			if err := wsHandlerRequest().Write(raw); err != nil {
				t.Fatal(err)
			}
			client := &wsTestPeer{Conn: raw, reader: bufio.NewReader(raw), masked: true}
			resp, err := http.ReadResponse(client.reader, nil)
			if err != nil || resp.StatusCode != 101 {
				t.Fatalf("升级: %v %v", resp, err)
			}
			_ = resp.Body.Close()
			if f := client.read(t); string(f.Payload) != wsTestInitial {
				t.Fatal("初始事件未交付")
			}
			server := receiveWSTest(t, peers)
			defer server.Close()
			source, other := client, server
			if upstream {
				source, other = other, source
			}
			if err := ws.WriteFrame(source.Conn, ws.Frame{Opcode: ws.OpText, Payload: []byte(`{"type":"`)}, source.masked); err != nil {
				t.Fatal(err)
			}
			const reason = "private auth quota To many requests"
			source.send(t, ws.OpClose, ws.EncodeClosePayload(1000, reason))
			assertWSTestClose(t, other, 1000, reason)
			awaitWSTest(t, done)
			st := pool.Stats()[0]
			if st.ConsecutiveFails != before.ConsecutiveFails || !st.Available || !st.CooldownUntil.Equal(before.CooldownUntil) || st.Picks != 2 || calls.Load() != 1 {
				t.Errorf("中断应归还 lease，不能 Succeed 清历史/冷却/重拨: before=%+v after=%+v calls=%d", before, st, calls.Load())
			}
			if wsUsageMetricSum(t, reg, "omugw_requests_total", map[string]string{"outcome": "ok"}) != 0 || wsUsageMetricSum(t, reg, "omugw_requests_total", map[string]string{"outcome": "internal"}) != 1 {
				t.Error("未完成消息仍计为成功请求")
			}
			if d.Budget.Used() != 0 {
				t.Fatal("handler 返回后仍持有预算")
			}
		})
	}
}

func TestWSHandlerFailoverMetrics(t *testing.T) {
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }))
	defer u.Close()
	d := wsHandlerDeps(t, u.URL)
	reg := prometheus.NewRegistry()
	d.Metrics = obs.NewMetrics(reg)
	NewDashScopeRealtimeHandler(d).ServeHTTP(&wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}, wsHandlerRequest())
	if wsUsageMetricSum(t, reg, "omugw_requests_total", map[string]string{"inbound": "dashscope.realtime", "outbound": "dashscope.ws.realtime", "outcome": "rate_limit"}) != 1 {
		t.Fatal("握手失败请求丢失固定出站身份")
	}
	if wsUsageMetricSum(t, reg, "omugw_upstream_errors_total", map[string]string{"outbound": "dashscope.ws.realtime", "class": "rate_limit"}) != 2 {
		t.Fatal("凭据尝试错误数不符")
	}
}

type wsForeignProvider struct{ provider.StreamProvider }

func (wsForeignProvider) Kind() degrade.Provider { return degrade.ProviderOpenAIRealtime }

// 给跨协议路径完整兑现和历史 homogeneous 标记，避免 PLANNED 或 Kind 错配替真闸门顶账。
func TestWSHandlerPreflightRejectsHomogeneousForeignTarget(t *testing.T) {
	d := wsHandlerDeps(t, "http://127.0.0.1:0")
	m := degrade.NewMatrix()
	for _, route := range d.Matrix.Routes() {
		if route.InProtocol() == degrade.ProtoDashScopeRealtime && route.OutProvider() == degrade.ProviderOpenAIRealtime {
			if err := m.Add(degrade.NewRoute(degrade.ProtoDashScopeRealtime, degrade.ProviderOpenAIRealtime).
				Pass(degrade.ExpressibleSet(degrade.ProtoDashScopeRealtime)...).MarkHomogeneous().
				Redeem(degrade.EndpointDashScopeRealtime, degrade.ExpressibleSet(degrade.ProtoDashScopeRealtime)...).Build()); err != nil {
				t.Fatal(err)
			}
		} else if err := m.Add(route, nil); err != nil {
			t.Fatal(err)
		}
	}
	d.Matrix = m
	var err error
	d.Router, err = router.New([]router.Rule{{Match: "real-model", Targets: []router.Target{{Kind: degrade.ProviderOpenAIRealtime, Endpoint: "ep", BaseURL: "http://127.0.0.1:0", UpstreamModel: "real-model", CredentialPool: "pool"}}}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	d.Providers["ep"] = wsForeignProvider{wsDialFunc(func(context.Context, provider.Request) (*ws.Conn, *http.Response, error) {
		calls++
		return nil, nil, errors.New("unexpected")
	})}
	w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
	NewDashScopeRealtimeHandler(d).ServeHTTP(w, wsHandlerRequest())
	if w.Code != 400 || calls != 0 || d.Pools["pool"].Stats()[0].Picks != 0 {
		t.Fatal("同源历史标记绕过同协议身份闸门")
	}
}
