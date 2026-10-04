package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
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

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/protocol/dashscoperealtime"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

const wsTestInitial = `{"type":"session.created","session":{"id":"s"},"unknown":"保留"}`

func receiveWSTest[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(4 * time.Second):
		t.Fatal("worker 未在期限内退出/交接")
		var zero T
		return zero
	}
}

func awaitWSTest(t *testing.T, ch <-chan struct{}) { t.Helper(); receiveWSTest(t, ch) }

func wsTestBudget(t *testing.T, n int64) *ws.BufferBudget {
	t.Helper()
	b, err := ws.NewBufferBudget(n)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type wsTestPeer struct {
	net.Conn
	reader *bufio.Reader
	masked bool
}

func (p *wsTestPeer) send(t *testing.T, op ws.Opcode, payload []byte) {
	t.Helper()
	if err := ws.WriteFrame(p.Conn, ws.Frame{FIN: true, Opcode: op, Payload: payload}, p.masked); err != nil {
		t.Fatal(err)
	}
}

func (p *wsTestPeer) read(t *testing.T) ws.Frame {
	t.Helper()
	_ = p.SetReadDeadline(time.Now().Add(4 * time.Second))
	f, err := ws.ReadFrame(p.reader, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

type wsTestHijacker struct {
	http.ResponseWriter
	wrap func(net.Conn) net.Conn
	raw  net.Conn
}

func (w *wsTestHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil {
		w.raw = c
		if w.wrap != nil {
			c = w.wrap(c)
			brw.Writer = bufio.NewWriter(c)
		}
	}
	return c, brw, err
}

// 每段都实际升级本地 TCP；原始对端可写分片/坏帧，并独立承担失败用例的兜底清理。
func wsTestLink(t *testing.T, client bool, budget *ws.BufferBudget, limit int64, idle time.Duration, wrap func(net.Conn) net.Conn) (*ws.Conn, *wsTestPeer) {
	t.Helper()
	type accepted struct {
		c   *ws.Conn
		raw net.Conn
		err error
	}
	acceptedCh := make(chan accepted, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj := &wsTestHijacker{ResponseWriter: w, wrap: wrap}
		opts := ws.AcceptOptions{MaxPayload: limit, Idle: idle, WriteTimeout: time.Second, Budget: budget}
		if client {
			opts.Budget = nil
			opts.Idle = 0
		}
		c, err := ws.Accept(hj, r, opts)
		acceptedCh <- accepted{c, hj.raw, err}
	}))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	if client {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c, _, err := ws.Dial(ctx, url, ws.DialOptions{Header: http.Header{"Origin": {"https://client.example"}}, MaxPayload: limit, Idle: idle, WriteTimeout: time.Second, Budget: budget})
		if err != nil {
			t.Fatal(err)
		}
		a := receiveWSTest(t, acceptedCh)
		if a.err != nil {
			t.Fatal(a.err)
		}
		t.Cleanup(func() { _ = a.raw.Close(); _ = c.Close(1001, "") })
		return c, &wsTestPeer{Conn: a.raw, reader: bufio.NewReader(a.raw)}
	}
	raw, err := net.DialTimeout("tcp", strings.TrimPrefix(srv.URL, "http://"), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	_ = raw.SetDeadline(time.Now().Add(4 * time.Second))
	_, err = fmt.Fprintf(raw, "GET / HTTP/1.1\r\nHost: local\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nOrigin: https://client.example\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("升级: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	_ = raw.SetDeadline(time.Time{})
	a := receiveWSTest(t, acceptedCh)
	if a.err != nil {
		t.Fatal(a.err)
	}
	t.Cleanup(func() { _ = a.raw.Close(); _ = a.c.Close(1001, "") })
	return a.c, &wsTestPeer{Conn: raw, reader: br, masked: true}
}

func assertWSTestClose(t *testing.T, p *wsTestPeer, code uint16, reason string) {
	t.Helper()
	f := p.read(t)
	if f.Opcode != ws.OpClose {
		t.Fatalf("不是实际 close 帧: %v", f.Opcode)
	}
	if code == 1005 {
		if len(f.Payload) != 0 {
			t.Fatal("空 close 被编码了保留码")
		}
		return
	}
	if len(f.Payload) < 2 || binary.BigEndian.Uint16(f.Payload) != code || string(f.Payload[2:]) != reason {
		t.Fatalf("close = %q, want %d %q", f.Payload, code, reason)
	}
}

type wsTestRelay struct {
	down, up       *ws.Conn
	client, server *wsTestPeer
	b              *ws.BufferBudget
	usage          *wsUsage
	cancel         context.CancelFunc
	done           chan error
}

func startWSTestRelay(t *testing.T, budget, limit int64, idle time.Duration, wrap func(net.Conn) net.Conn) *wsTestRelay {
	t.Helper()
	x := &wsTestRelay{b: wsTestBudget(t, budget), usage: newWSUsage(nil, "dashscope.realtime", "dashscope.realtime"), done: make(chan error, 1)}
	x.down, x.client = wsTestLink(t, false, x.b, limit, idle, wrap)
	x.up, x.server = wsTestLink(t, true, x.b, limit, idle, nil)
	x.server.send(t, ws.OpText, []byte(wsTestInitial))
	initial, err := x.up.ReadOwnedMessage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	x.cancel = cancel
	t.Cleanup(cancel)
	go func() { x.done <- relayWS(ctx, x.down, x.up, initial, x.usage, idle) }()
	f := x.client.read(t)
	if f.Opcode != ws.OpText || string(f.Payload) != wsTestInitial {
		t.Fatal("首条 session.created 未原样最先发送")
	}
	return x
}

func (x *wsTestRelay) joined(t *testing.T) error {
	t.Helper()
	err := receiveWSTest(t, x.done)
	if x.b.Used() != 0 {
		t.Fatalf("返回时仍持预算: %d", x.b.Used())
	}
	if !x.usage.finished {
		t.Fatal("worker join 后未 Finish")
	}
	return err
}

func TestWSRelayPreservesMessagesAndUsage(t *testing.T) {
	x := startWSTestRelay(t, 16<<20, 4<<20, 0, nil)
	for _, peers := range [][2]*wsTestPeer{{x.client, x.server}, {x.server, x.client}} {
		for _, v := range []struct {
			op      ws.Opcode
			payload []byte
		}{
			{ws.OpText, []byte(" {\"type\":\"future.event\",\"unknown\": [1, 2]}\n")},
			{ws.OpBinary, []byte{0, 255, 0, 128}},
			{ws.OpBinary, bytes.Repeat([]byte{0x83, 0, 0x17}, 700000)},
			{ws.OpText, []byte(`{"type":"error","error":{"code":"invalid_value","message":"secret-body"}}`)},
		} {
			write := make(chan error, 1)
			go func() {
				write <- ws.WriteFrame(peers[0].Conn, ws.Frame{FIN: true, Opcode: v.op, Payload: v.payload}, peers[0].masked)
			}()
			f := peers[1].read(t)
			if f.Opcode != v.op || !bytes.Equal(f.Payload, v.payload) {
				t.Fatal("opcode/原字节/单向顺序改变")
			}
			if err := receiveWSTest(t, write); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, peers := range [][2]*wsTestPeer{{x.client, x.server}, {x.server, x.client}} {
		for _, f := range []ws.Frame{{Opcode: ws.OpText, Payload: []byte(`{"type":"future.event","x":"` + "\xe4")}, {FIN: true, Opcode: ws.OpContinuation, Payload: []byte("\xb8\xad\"}")}} {
			if err := ws.WriteFrame(peers[0].Conn, f, peers[0].masked); err != nil {
				t.Fatal(err)
			}
		}
		if f := peers[1].read(t); f.Opcode != ws.OpText || string(f.Payload) != `{"type":"future.event","x":"中"}` {
			t.Fatal("分片重组未保全")
		}
	}
	x.client.send(t, ws.OpText, []byte(`{"type":"response.done","response":{"id":"forged","usage":{"input_tokens":999,"output_tokens":0}}}`))
	_ = x.server.read(t)
	x.server.send(t, ws.OpText, []byte(`{"type":"session.finished"}`))
	_ = x.client.read(t)
	x.server.send(t, ws.OpClose, ws.EncodeClosePayload(1000, "正常终止"))
	assertWSTestClose(t, x.client, 1000, "正常终止")
	if err := x.joined(t); err != nil {
		t.Fatal(err)
	}
	if len(x.usage.records) != 1 || !x.usage.records[wsUsageKey{"session", "s"}].terminal {
		t.Fatal("初始会话未 Observe，或误观测下游伪造 usage")
	}
}

func TestWSRelayCloseMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   uint16
		reason string
		eof    bool
	}{
		{"normal", 1000, "private close reason", false}, {"empty", 1005, "", false}, {"rate", 1011, "To many requests. private", false}, {"eof", 1011, "upstream connection failed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := startWSTestRelay(t, 1<<20, 1<<18, 0, nil)
			if tc.eof {
				_ = x.server.Close()
			} else {
				x.server.send(t, ws.OpClose, ws.EncodeClosePayload(tc.code, tc.reason))
			}
			assertWSTestClose(t, x.client, tc.code, tc.reason)
			err := x.joined(t)
			if err != nil && strings.Contains(err.Error(), "private") {
				t.Fatal("close reason 泄漏到错误")
			}
			if tc.name == "rate" && canonical.AsError(err).Class != canonical.ClassRateLimit {
				t.Fatalf("丢失安全分类: %v", err)
			}
			if tc.eof && (err == nil || canonical.AsError(err).Retryable) {
				t.Fatal("未知断流应为安全的非重试错误")
			}
		})
	}
}

func TestWSRelayProtocolAndLocalLimits(t *testing.T) {
	for _, tc := range []struct {
		name          string
		op            ws.Opcode
		payload       []byte
		budget, limit int64
		code          uint16
		reason        string
	}{
		{"utf8", ws.OpText, []byte{255}, 4096, 1024, 1007, "invalid UTF-8"},
		{"rfc", ws.OpContinuation, []byte("x"), 4096, 1024, 1002, "invalid websocket frame"},
		{"message", ws.OpBinary, bytes.Repeat([]byte("x"), 1025), 4096, 1024, 1009, "message too large"},
		{"budget", ws.OpBinary, bytes.Repeat([]byte("x"), 1025), 1024, 4096, 1013, "buffer capacity exhausted"},
		{"policy", ws.OpText, []byte(`{"type":"response.done","response":{}}`), 4096, 1024, 1008, "invalid event correlation"},
	} {
		for _, upstream := range []bool{false, true} {
			if tc.name == "policy" && !upstream {
				continue
			}
			t.Run(fmt.Sprint(tc.name, "/upstream=", upstream), func(t *testing.T) {
				x := startWSTestRelay(t, tc.budget, tc.limit, 0, nil)
				source, other := x.client, x.server
				if upstream {
					source, other = other, source
				}
				source.send(t, tc.op, tc.payload)
				assertWSTestClose(t, other, tc.code, tc.reason)
				if err := x.joined(t); err == nil {
					t.Fatal("错误结束被当成成功")
				}
			})
		}
	}
}

func TestWSRelayCancellationSendsCloseBeforeReadCancel(t *testing.T) {
	x := startWSTestRelay(t, 1<<20, 1<<18, 0, nil)
	x.cancel()
	assertWSTestClose(t, x.client, 1001, "")
	assertWSTestClose(t, x.server, 1001, "")
	if err := x.joined(t); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type wsTestGate struct {
	net.Conn
	armed             atomic.Bool
	entered, released chan struct{}
	enter, release    sync.Once
	fail              bool
}

func newWSTestGate(c net.Conn, fail bool) *wsTestGate {
	return &wsTestGate{Conn: c, entered: make(chan struct{}), released: make(chan struct{}), fail: fail}
}
func (g *wsTestGate) unblock() { g.release.Do(func() { close(g.released) }) }
func (g *wsTestGate) Write(p []byte) (int, error) {
	if g.armed.Load() {
		g.enter.Do(func() { close(g.entered) })
		if g.fail {
			return 0, errors.New("private write error")
		}
		<-g.released
	}
	return g.Conn.Write(p)
}
func (g *wsTestGate) Close() error { g.unblock(); return g.Conn.Close() }

func TestWSRelayObserveBeforeFailedWrite(t *testing.T) {
	var gate *wsTestGate
	x := startWSTestRelay(t, 1<<20, 1<<18, 0, func(c net.Conn) net.Conn { gate = newWSTestGate(c, true); return gate })
	gate.armed.Store(true)
	x.server.send(t, ws.OpText, []byte(`{"type":"response.done","response":{"id":"r","status":"cancelled","usage":{"input_tokens":7,"output_tokens":3}}}`))
	awaitWSTest(t, gate.entered)
	err := x.joined(t)
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("本地写失败分类不安全: %v", err)
	}
	record := x.usage.records[wsUsageKey{"response", "r"}]
	if record.usage.Fidelity != canonical.FidelityAuthoritative || record.usage.InputTokens != 7 || record.usage.OutputTokens != 3 {
		t.Fatal("下游断开抹掉已收到的权威用量")
	}
}

func TestWSRelayBlockedWriteCancelAndConcurrentClose(t *testing.T) {
	for _, peerClose := range []bool{false, true} {
		t.Run(fmt.Sprint("close=", peerClose), func(t *testing.T) {
			var gate *wsTestGate
			x := startWSTestRelay(t, 1<<20, 1<<18, 0, func(c net.Conn) net.Conn { gate = newWSTestGate(c, false); return gate })
			gate.armed.Store(true)
			x.server.send(t, ws.OpText, []byte(`{"type":"response.created","response":{"id":"first"}}`))
			awaitWSTest(t, gate.entered)
			x.server.send(t, ws.OpText, []byte(`{"type":"response.created","response":{"id":"queued"}}`))
			if x.b.Used() == 0 {
				t.Fatal("阻塞写没有保有消息所有权")
			}
			start := time.Now()
			if peerClose {
				x.client.send(t, ws.OpClose, ws.EncodeClosePayload(1000, "owned close reason"))
			} else {
				x.cancel()
			}
			_ = x.joined(t)
			if time.Since(start) > 1500*time.Millisecond {
				t.Fatal("close 未打断阻塞写")
			}
			if _, ok := x.usage.records[wsUsageKey{"response", "queued"}]; ok {
				t.Fatal("慢下游时仍提前读取并累计消息")
			}
		})
	}
}

func TestWSRelayHeartbeatKeepsSilentSessionAlive(t *testing.T) {
	idle := 300 * time.Millisecond
	x := startWSTestRelay(t, 1<<20, 1<<18, idle, nil)
	for i := 0; i < 5; i++ {
		for _, p := range []*wsTestPeer{x.client, x.server} {
			f := p.read(t)
			if f.Opcode != ws.OpPing {
				t.Fatal("业务沉默时没有主动心跳")
			}
			p.send(t, ws.OpPong, f.Payload)
		}
	}
	x.client.send(t, ws.OpPing, []byte("probe"))
	if f := x.client.read(t); f.Opcode != ws.OpPong || string(f.Payload) != "probe" {
		t.Fatal("读者没有自动响应 ping")
	}
	x.cancel()
	_ = x.joined(t)
}

func TestWSRelayUsageCapacityPolicy(t *testing.T) {
	b := wsTestBudget(t, 1<<20)
	down, client := wsTestLink(t, false, b, 1<<18, 0, nil)
	up, server := wsTestLink(t, true, b, 1<<18, 0, nil)
	u := newWSUsage(nil, "dashscope.realtime", "dashscope.realtime")
	for i := 0; i < 4096; i++ {
		if err := u.Observe(dashscoperealtime.Event{Source: "response", ID: fmt.Sprint(i), Started: true}); err != nil {
			t.Fatal(err)
		}
	}
	server.send(t, ws.OpText, []byte(wsTestInitial))
	initial, err := up.ReadOwnedMessage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- relayWS(context.Background(), down, up, initial, u, 0) }()
	assertWSTestClose(t, client, 1008, "usage record capacity exhausted")
	if err := receiveWSTest(t, done); !errors.Is(err, errWSUsageLimit) {
		t.Fatal(err)
	}
	if b.Used() != 0 || !u.finished {
		t.Fatal("初始消息策略失败未回收/join")
	}
}

func TestWSRelayRegistryShutdownJoinsExistingClose(t *testing.T) {
	r := newWSRegistry(1)
	s, _ := r.Register(context.Background())
	b := wsTestBudget(t, 1<<20)
	var gate *wsTestGate
	down, client := wsTestLink(t, false, b, 1<<18, 0, func(c net.Conn) net.Conn {
		gate = newWSTestGate(c, false)
		return gate
	})
	up, server := wsTestLink(t, true, b, 1<<18, 0, nil)
	server.send(t, ws.OpText, []byte(wsTestInitial))
	initial, err := up.ReadOwnedMessage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !s.Attach(up) || !s.Attach(down) {
		t.Fatal("Attach 失败")
	}
	gate.armed.Store(true)
	shutdown := make(chan error, 1)
	go func() { shutdown <- r.Shutdown(context.Background()) }()
	// 强制 registry 先持有 close 的写所有权，relay 的幂等 close 不能冒充 join。
	awaitWSTest(t, gate.entered)
	u := newWSUsage(nil, "dashscope.realtime", "dashscope.realtime")
	done := make(chan error, 1)
	go func() { done <- relayWS(s.Context(), down, up, initial, u, 0) }()
	select {
	case err := <-done:
		used := b.Used()
		gate.unblock()
		s.Done()
		t.Fatalf("registry 仍持有 close payload，relay 已返回: %v, budget=%d", err, used)
	case <-time.After(40 * time.Millisecond):
	}
	gate.unblock()
	assertWSTestClose(t, client, 1001, "")
	assertWSTestClose(t, server, 1001, "")
	if err := receiveWSTest(t, done); !errors.Is(err, errWSShutdown) {
		t.Fatal(err)
	}
	if b.Used() != 0 || !u.finished {
		t.Fatal("关停返回时所有权仍在途")
	}
	s.Done()
	if err := receiveWSTest(t, shutdown); err != nil {
		t.Fatal(err)
	}
}

func TestWSRelaySimultaneousCloseReleasesBothReasons(t *testing.T) {
	for i := 0; i < 12; i++ {
		x := startWSTestRelay(t, 1<<20, 1<<18, 0, nil)
		start := make(chan struct{})
		writes := make(chan error, 2)
		for _, p := range []*wsTestPeer{x.client, x.server} {
			go func() {
				<-start
				writes <- ws.WriteFrame(p.Conn, ws.Frame{FIN: true, Opcode: ws.OpClose, Payload: ws.EncodeClosePayload(1000, "private simultaneous reason")}, p.masked)
			}()
		}
		close(start)
		_ = receiveWSTest(t, writes)
		_ = receiveWSTest(t, writes)
		if err := x.joined(t); err != nil && strings.Contains(err.Error(), "private") {
			t.Fatal("竞争 close 暴露原始 reason")
		}
	}
}

func TestWSRelayFirstCloseSurvivesLaterShutdown(t *testing.T) {
	r := newWSRegistry(1)
	s, _ := r.Register(context.Background())
	b := wsTestBudget(t, 1<<20)
	var gate *wsTestGate
	down, client := wsTestLink(t, false, b, 1<<18, 0, func(c net.Conn) net.Conn {
		gate = newWSTestGate(c, false)
		return gate
	})
	up, server := wsTestLink(t, true, b, 1<<18, 0, nil)
	if !s.Attach(up) || !s.Attach(down) {
		t.Fatal("Attach 失败")
	}
	server.send(t, ws.OpText, []byte(wsTestInitial))
	initial, err := up.ReadOwnedMessage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u := newWSUsage(nil, "dashscope.realtime", "dashscope.realtime")
	done := make(chan error, 1)
	go func() { done <- relayWS(s.Context(), down, up, initial, u, 0) }()
	_ = client.read(t)
	gate.armed.Store(true)
	server.send(t, ws.OpClose, ws.EncodeClosePayload(1011, "To many requests. private reason"))
	awaitWSTest(t, gate.entered)
	shutdown := make(chan error, 1)
	go func() { shutdown <- r.Shutdown(context.Background()) }()
	awaitWSTest(t, s.shutdown.started)
	gate.unblock()
	assertWSTestClose(t, client, 1011, "To many requests. private reason")
	err = receiveWSTest(t, done)
	if err == nil || canonical.AsError(err).Class != canonical.ClassRateLimit || strings.Contains(err.Error(), "private") {
		t.Fatalf("后发 shutdown 改写了首个原因: %v", err)
	}
	if b.Used() != 0 {
		t.Fatal("胜出的 CloseError 所有权未归还")
	}
	s.Done()
	if err := receiveWSTest(t, shutdown); err != nil {
		t.Fatal(err)
	}
}

type wsTestWriteSignal struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *wsTestWriteSignal) Write(p []byte) (int, error) {
	if len(p) >= 1<<20 {
		c.once.Do(func() { close(c.started) })
	}
	return c.Conn.Write(p)
}

func TestWSRelayRealSlowTCPReader(t *testing.T) {
	var writer *wsTestWriteSignal
	x := startWSTestRelay(t, 16<<20, 4<<20, 0, func(c net.Conn) net.Conn {
		if err := c.(*net.TCPConn).SetWriteBuffer(1024); err != nil {
			t.Error(err)
		}
		writer = &wsTestWriteSignal{Conn: c, started: make(chan struct{})}
		return writer
	})
	if err := x.client.Conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0, 255}, 1<<20)
	write := make(chan error, 1)
	go func() {
		write <- ws.WriteFrame(x.server.Conn, ws.Frame{FIN: true, Opcode: ws.OpBinary, Payload: payload}, false)
	}()
	awaitWSTest(t, writer.started)
	if err := receiveWSTest(t, write); err != nil {
		t.Fatal(err)
	}
	if x.b.Used() < int64(len(payload)) {
		t.Fatal("慢读期间消息所有权提前释放")
	}
	x.cancel()
	if err := x.joined(t); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type wsTestClassificationGate struct {
	entered, released chan struct{}
	enter, release    sync.Once
}

func (e *wsTestClassificationGate) Error() string { return "test downstream write failure" }
func (e *wsTestClassificationGate) unblock()      { e.release.Do(func() { close(e.released) }) }
func (e *wsTestClassificationGate) As(target any) bool {
	if _, ok := target.(**ws.CloseError); ok {
		e.enter.Do(func() { close(e.entered) })
		<-e.released
	}
	return false
}

type wsTestFailOnce struct {
	net.Conn
	armed atomic.Bool
	err   error
}

func (c *wsTestFailOnce) Write(p []byte) (int, error) {
	if c.armed.Swap(false) {
		return 0, c.err
	}
	return c.Conn.Write(p)
}

func TestWSRelayShutdownCannotOverrideSelectedWireReason(t *testing.T) {
	wsTestSelectedClose(t, false)
}

func wsTestSelectedClose(t *testing.T, duplicateAttach bool) {
	t.Helper()
	r := newWSRegistry(1)
	s, err := r.Register(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b := wsTestBudget(t, 1<<20)
	gate := &wsTestClassificationGate{entered: make(chan struct{}), released: make(chan struct{})}
	var writer *wsTestFailOnce
	down, client := wsTestLink(t, false, b, 1<<18, 0, func(c net.Conn) net.Conn {
		writer = &wsTestFailOnce{Conn: c, err: gate}
		return writer
	})
	up, server := wsTestLink(t, true, b, 1<<18, 0, nil)
	t.Cleanup(gate.unblock)
	if !s.Attach(up) || !s.Attach(down) {
		t.Fatal("Attach 失败")
	}
	server.send(t, ws.OpText, []byte(wsTestInitial))
	initial, err := up.ReadOwnedMessage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u := newWSUsage(nil, "dashscope.realtime", "dashscope.realtime")
	done := make(chan error, 1)
	go func() { done <- relayWS(s.Context(), down, up, initial, u, 0) }()
	_ = client.read(t)
	writer.armed.Store(true)
	server.send(t, ws.OpText, []byte(`{"type":"future.event"}`))
	// errors.As 固定“已经选中原因、尚未取得物理 close 权”的窗口，不靠帧写屏障。
	awaitWSTest(t, gate.entered)
	shutdown := make(chan error, 1)
	if duplicateAttach {
		attaching := make(chan struct{})
		go func() {
			close(attaching)
			if s.Attach(up) {
				shutdown <- errors.New("终止后仍接受重复 Attach")
				return
			}
			shutdown <- nil
		}()
		awaitWSTest(t, attaching)
	} else {
		go func() { shutdown <- r.Shutdown(context.Background()) }()
		awaitWSTest(t, s.shutdown.started)
	}
	// 正确实现必须等待获胜者完成分类；旧实现在屏障仍闭合时已经发出 1001。
	_ = server.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	premature, readErr := ws.ReadFrame(server.reader, 1024)
	gate.unblock()
	if readErr == nil {
		t.Errorf("分类屏障尚未释放就被 registry 关闭: opcode=%v payload=%q", premature.Opcode, premature.Payload)
	} else {
		var ne net.Error
		if !errors.As(readErr, &ne) || !ne.Timeout() {
			t.Errorf("不是等待获胜关闭者: %v", readErr)
		}
		assertWSTestClose(t, server, 1011, "downstream connection failed")
	}
	f := client.read(t)
	if f.Opcode != ws.OpClose || !bytes.Equal(f.Payload, ws.EncodeClosePayload(1011, "downstream connection failed")) {
		t.Errorf("获胜关闭在线路上被改写: opcode=%v payload=%q", f.Opcode, f.Payload)
	}
	if err := receiveWSTest(t, done); !errors.Is(err, errWSRelayDownstream) {
		t.Fatal(err)
	}
	if b.Used() != 0 || !u.finished {
		t.Fatal("共享关闭未回收预算或未 join")
	}
	s.Done()
	if err := receiveWSTest(t, shutdown); err != nil {
		t.Fatal(err)
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
