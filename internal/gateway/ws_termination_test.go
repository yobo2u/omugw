package gateway

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/config"
	dsi "github.com/yobo2u/omugw/internal/protocol/dashscopeinference"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

var errWSFakeTask = errors.New("test upstream task failed")

func wsTestFailedPolicy(after func(wsForwardTicket, error)) *wsFakePolicy {
	p := newWSFakePolicy()
	p.before = func(_ context.Context, d wsDirection, _ ws.Opcode, b []byte) (wsForwardDecision, error) {
		if d == wsUpstreamToClient && string(b) == "failed-original" {
			return wsForwardDecision{Ticket: wsForwardTicket{Generation: 1, Step: wsStepFailed}, Action: wsForwardThenEnd, End: &wsPolicyEnd{At: time.Now(), Failure: errWSFakeTask, Code: 1011, Reason: "upstream task failed", PeerGrace: true}}, nil
		}
		return wsForwardDecision{}, nil
	}
	p.after = after
	return p
}

func TestWSRelayFailureDeliveryAndPeerClose(t *testing.T) {
	for _, kind := range []string{"silent", "eof", "normal", "custom", "empty"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			p := wsTestFailedPolicy(func(ticket wsForwardTicket, err error) {
				if ticket.Step == wsStepFailed {
					calls.Add(1)
					if err != nil {
						t.Error(err)
					}
				}
			})
			x := startWSPolicyTest(t, p, 4096, nil, nil)
			x.server.send(t, ws.OpText, []byte("failed-original"))
			code, reason := uint16(1011), "upstream task failed"
			switch kind {
			case "normal":
				code, reason = 1000, "private normal"
			case "custom":
				code, reason = 4001, "private custom"
			case "empty":
				code, reason = 1005, ""
			}
			if kind == "normal" || kind == "custom" || kind == "empty" {
				x.server.send(t, ws.OpClose, ws.EncodeClosePayload(code, reason))
			}
			if kind == "eof" {
				_ = x.server.Close()
			}
			if f := x.client.read(t); f.Opcode != ws.OpText || string(f.Payload) != "failed-original" {
				t.Fatal("failed 未先于 close 原样交付")
			}
			assertWSTestClose(t, x.client, code, reason)
			if err := x.join(t); !errors.Is(err, errWSFakeTask) {
				t.Fatalf("peer/shutdown 覆盖业务失败: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatal("原文 ticket 不是一次 After")
			}
		})
	}
}

func wsSchedulingSlack() time.Duration {
	return wsCloseSchedulingTolerance
}

func assertWSDeadline(t *testing.T, term *wsTermination, want time.Time) {
	t.Helper()
	term.mu.Lock()
	actual := term.deadline
	term.mu.Unlock()
	if actual != want {
		t.Fatalf("绝对期限被重置: got=%s want=%s", actual, want)
	}
}

func TestWSRelayFailureLatePeerAndSingleOwner(t *testing.T) {
	b := wsTestBudget(t, 4096)
	down, client := wsTestLink(t, false, b, 1024, 0, nil)
	up, server := wsTestLink(t, true, b, 1024, 0, nil)
	p := newWSFakePolicy()
	closing, allow := make(chan struct{}), make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(allow) }) })
	var calls atomic.Int32
	var wireCode uint16
	var wireReason string
	term := newWSTermination(func(code uint16, reason string, d time.Time) {
		calls.Add(1)
		wireCode, wireReason = code, reason
		close(closing)
		<-allow
		closeWSConnections([]*ws.Conn{down, up}, code, reason, d)
	}, 80*time.Millisecond)
	opts := wsRelayOptions{Policy: p, Timeouts: config.Timeouts{Connect: 80 * time.Millisecond, Idle: time.Second}, Budget: b, MaxMessageBytes: 1024}
	if err := term.attachRelay(down, up, opts); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	won, delivered := term.policyEnd(wsPolicyEnd{At: at, Failure: errWSFakeTask, Code: 1011, Reason: "upstream task failed", PeerGrace: true}, true)
	if !won {
		t.Fatal("失败未认领")
	}
	assertWSDeadline(t, term, at.Add(160*time.Millisecond))
	delivered(nil)
	delivered(errors.New("迟到回执不能覆写"))
	done := make(chan error, 8)
	for range 8 {
		go func() { done <- term.close() }()
	}
	awaitWSTest(t, closing)
	// 已过 G 且 wire 槽封口，此时才由真实 TCP 收到的 CloseError 必须立即归还。
	server.send(t, ws.OpClose, ws.EncodeClosePayload(4002, "late private reason"))
	_, err := up.ReadOwnedMessage(context.Background())
	var peer *ws.CloseError
	if !errors.As(err, &peer) {
		t.Fatal(err)
	}
	term.report(wsRelayResult{err: err, upstream: true})
	if b.Used() != 0 {
		t.Fatal("G 外 CloseError 被滞留")
	}
	term.report(wsRelayResult{err: errWSShutdown})
	release.Do(func() { close(allow) })
	assertWSTestClose(t, client, 1011, "upstream task failed")
	for range 8 {
		if err := receiveWSTest(t, done); !errors.Is(err, errWSFakeTask) {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 || wireCode != 1011 || wireReason != "upstream task failed" || b.Used() != 0 {
		t.Fatal("多 owner 或虚构 peer 事实")
	}
	term.mu.Lock()
	deadline := term.deadline
	term.mu.Unlock()
	if deadline.After(at.Add(160*time.Millisecond)) || !deadline.Before(at.Add(120*time.Millisecond)) {
		t.Fatal("原文成功后没有收紧 B")
	}
}

func TestWSRelayFailureAfterStopReceipt(t *testing.T) {
	p := wsTestFailedPolicy(nil)
	after := make(chan struct{})
	p.after = func(ticket wsForwardTicket, err error) {
		if ticket.Step == wsStepFailed {
			<-p.stopped
			if err != nil {
				t.Error(err)
			}
			close(after)
		}
	}
	x := startWSPolicyTest(t, p, 4096, nil, nil)
	x.server.send(t, ws.OpText, []byte("failed-original"))
	if f := x.client.read(t); string(f.Payload) != "failed-original" {
		t.Fatal("原文未交付")
	}
	awaitWSTest(t, after)
	assertWSTestClose(t, x.client, 1011, "upstream task failed")
	if err := x.join(t); !errors.Is(err, errWSFakeTask) {
		t.Fatal(err)
	}
}

func TestWSRelayFailureTailAndCancelledClient(t *testing.T) {
	p := wsTestFailedPolicy(nil)
	releaseOriginal := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(releaseOriginal) }) })
	clientRejected := make(chan error, 1)
	before := p.before
	p.before = func(ctx context.Context, d wsDirection, op ws.Opcode, b []byte) (wsForwardDecision, error) {
		if d == wsClientToUpstream {
			return wsForwardDecision{Ticket: wsForwardTicket{Step: wsStepRun}}, nil
		}
		return before(ctx, d, op, b)
	}
	p.after = func(ticket wsForwardTicket, err error) {
		if ticket.Step == wsStepFailed {
			<-p.stopped
			<-releaseOriginal
		}
		if ticket.Step == wsStepRun {
			clientRejected <- err
		}
	}
	x := startWSPolicyTest(t, p, 4096, nil, nil)
	x.server.send(t, ws.OpText, []byte("failed-original"))
	_ = x.client.read(t)
	x.server.send(t, ws.OpBinary, []byte{0, 255, 0, 128})
	x.server.send(t, ws.OpClose, ws.EncodeClosePayload(4003, "peer after tail"))
	x.client.send(t, ws.OpText, []byte("must-not-admit"))
	if err := receiveWSTest(t, clientRejected); !errors.Is(err, context.Canceled) {
		t.Fatal("终止后仍写客户端请求", err)
	}
	release.Do(func() { close(releaseOriginal) })
	if f := x.client.read(t); f.Opcode != ws.OpBinary || !bytes.Equal(f.Payload, []byte{0, 255, 0, 128}) {
		t.Fatal("取消客户端 ticket 干扰 G 内尾消息")
	}
	assertWSTestClose(t, x.client, 4003, "peer after tail")
	if err := x.join(t); !errors.Is(err, errWSFakeTask) {
		t.Fatal(err)
	}
}

func TestWSRelayFailureTransportWriteLock(t *testing.T) {
	var gate *wsTestGate
	at := make(chan time.Time, 1)
	p := wsTestFailedPolicy(nil)
	before := p.before
	p.before = func(ctx context.Context, d wsDirection, op ws.Opcode, b []byte) (wsForwardDecision, error) {
		decision, err := before(ctx, d, op, b)
		if decision.End != nil {
			at <- decision.End.At
		}
		return decision, err
	}
	r := newWSRegistry(1, 80*time.Millisecond)
	x := startWSPolicyTest(t, p, 4096, func(c net.Conn) net.Conn { gate = newWSTestGate(c, false); return gate }, r)
	gate.armed.Store(true)
	x.client.send(t, ws.OpPing, []byte("holds-transport-write-lock"))
	awaitWSTest(t, gate.entered)
	x.server.send(t, ws.OpText, []byte("failed-original"))
	start := receiveWSTest(t, at)
	awaitWSTest(t, x.s.shutdown.termination.selected)
	assertWSDeadline(t, x.s.shutdown.termination, start.Add(160*time.Millisecond))
	if err := x.join(t); !errors.Is(err, errWSFakeTask) {
		t.Fatal(err)
	}
	assertWSDeadline(t, x.s.shutdown.termination, start.Add(160*time.Millisecond))
	if time.Since(start) > 160*time.Millisecond+wsSchedulingSlack() {
		t.Fatal("等写锁越过 Dall")
	}
	_ = r.Shutdown(context.Background())
}

func TestWSRelayFailureSlowTCPAbsoluteBudget(t *testing.T) {
	var writer *wsTestWriteSignal
	p := newWSFakePolicy()
	at := make(chan time.Time, 1)
	after := make(chan error, 1)
	p.before = func(context.Context, wsDirection, ws.Opcode, []byte) (wsForwardDecision, error) {
		now := time.Now()
		at <- now
		return wsForwardDecision{Action: wsForwardThenEnd, Ticket: wsForwardTicket{Step: wsStepFailed}, End: &wsPolicyEnd{At: now, Failure: errWSFakeTask, Code: 1011, Reason: "upstream task failed", PeerGrace: true}}, nil
	}
	p.after = func(_ wsForwardTicket, err error) { after <- err }
	x := startWSPolicyTest(t, p, 8<<20, func(c net.Conn) net.Conn {
		if err := c.(*net.TCPConn).SetWriteBuffer(1024); err != nil {
			t.Error(err)
		}
		writer = &wsTestWriteSignal{Conn: c, started: make(chan struct{})}
		return writer
	}, nil)
	if err := x.client.Conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 2<<20)
	wrote := make(chan error, 1)
	go func() {
		wrote <- ws.WriteFrame(x.server.Conn, ws.Frame{FIN: true, Opcode: ws.OpText, Payload: payload}, false)
	}()
	start := receiveWSTest(t, at)
	awaitWSTest(t, writer.started)
	if err := receiveWSTest(t, wrote); err != nil {
		t.Fatal(err)
	}
	if err := x.join(t); !errors.Is(err, errWSFakeTask) {
		t.Fatal(err)
	}
	if time.Since(start) > 160*time.Millisecond+wsSchedulingSlack() {
		t.Fatal("真实慢 TCP 重开预算")
	}
	if err := receiveWSTest(t, after); err == nil {
		t.Fatal("慢写失败未 After")
	}
}

func TestWSRelayLocalFailureWaitsForDeliveryLock(t *testing.T) {
	var gate *wsTestGate
	p := newWSFakePolicy()
	at := make(chan time.Time, 1)
	var callbacks atomic.Int32
	p.before = func(_ context.Context, d wsDirection, _ ws.Opcode, _ []byte) (wsForwardDecision, error) {
		if d == wsUpstreamToClient {
			return wsForwardDecision{}, nil
		}
		now := time.Now()
		at <- now
		return wsForwardDecision{Action: wsRejectThenEnd, End: &wsPolicyEnd{At: now, Failure: errWSRelayPolicy, Code: 1008, Reason: "invalid event correlation", Local: &wsLocalTaskFailure{TaskID: "task", Kind: dsi.LocalPolicy}}}, nil
	}
	p.after = func(_ wsForwardTicket, err error) {
		callbacks.Add(1)
		if err == nil {
			t.Error("已阻塞/拒绝的消息竟成功")
		}
	}
	r := newWSRegistry(1, 80*time.Millisecond)
	x := startWSPolicyTest(t, p, 4096, func(c net.Conn) net.Conn { gate = newWSTestGate(c, false); return gate }, r)
	gate.armed.Store(true)
	x.server.send(t, ws.OpText, []byte("busy"))
	awaitWSTest(t, gate.entered)
	x.client.send(t, ws.OpText, []byte("reject"))
	start := receiveWSTest(t, at)
	awaitWSTest(t, x.s.shutdown.termination.selected)
	assertWSDeadline(t, x.s.shutdown.termination, start.Add(160*time.Millisecond))
	shutdown := make(chan error, 1)
	go func() { shutdown <- r.Shutdown(context.Background()) }()
	if err := x.join(t); !errors.Is(err, errWSRelayPolicy) {
		t.Fatal(err)
	}
	assertWSDeadline(t, x.s.shutdown.termination, start.Add(160*time.Millisecond))
	if time.Since(start) > 160*time.Millisecond+wsSchedulingSlack() || callbacks.Load() != 2 {
		t.Fatal("本地交付等锁越界或遗失 After")
	}
	if err := receiveWSTest(t, shutdown); err != nil {
		t.Fatal(err)
	}
}

func TestWSRegistryPendingAbsoluteBudget(t *testing.T) {
	r := newWSRegistry(1, 80*time.Millisecond)
	s, err := r.Register(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b := wsTestBudget(t, 4096)
	var gates []*wsTestGate
	for range 2 {
		c, _ := wsTestLink(t, false, b, 1024, 0, func(raw net.Conn) net.Conn { g := newWSTestGate(raw, false); gates = append(gates, g); return g })
		if !s.Attach(c) {
			t.Fatal("挂接失败")
		}
	}
	for _, g := range gates {
		g.armed.Store(true)
	}
	shutdown := make(chan error, 1)
	start := time.Now()
	go func() { shutdown <- r.Shutdown(context.Background()) }()
	for _, g := range gates {
		awaitWSTest(t, g.entered)
	}
	s.shutdown.termination.mu.Lock()
	deadline := s.shutdown.termination.deadline
	s.shutdown.termination.mu.Unlock()
	awaitWSTest(t, s.Context().Done())
	if time.Now().After(deadline.Add(wsSchedulingSlack())) || deadline.After(start.Add(80*time.Millisecond+wsSchedulingSlack())) {
		t.Fatal("pending 两段没有共用 B")
	}
	assertWSDeadline(t, s.shutdown.termination, deadline)
	s.Done()
	if err := receiveWSTest(t, shutdown); err != nil {
		t.Fatal(err)
	}
	if b.Used() != 0 {
		t.Fatal("pending close 未归还预算")
	}
}

func TestWSRelayFailureAbsoluteBudget(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "blocked-failed", true: "failed-and-shutdown"}[shutdown], func(t *testing.T) {
			after := make(chan error, 1)
			p := wsTestFailedPolicy(func(ticket wsForwardTicket, err error) {
				if ticket.Step == wsStepFailed {
					after <- err
				}
			})
			var gate *wsTestGate
			r := newWSRegistry(1, 80*time.Millisecond)
			x := startWSPolicyTest(t, p, 4096, func(c net.Conn) net.Conn { gate = newWSTestGate(c, false); return gate }, r)
			gate.armed.Store(true)
			start := time.Now()
			x.server.send(t, ws.OpText, []byte("failed-original"))
			awaitWSTest(t, gate.entered)
			ended := make(chan error, 1)
			if shutdown {
				go func() { ended <- r.Shutdown(context.Background()) }()
			}
			if err := x.join(t); !errors.Is(err, errWSFakeTask) {
				t.Fatal(err)
			}
			if elapsed := time.Since(start); elapsed > 160*time.Millisecond+wsSchedulingSlack() {
				t.Fatalf("Dall 重启: %v", elapsed)
			}
			if err := receiveWSTest(t, after); err == nil {
				t.Fatal("写失败未交给 After")
			}
			if shutdown {
				if err := receiveWSTest(t, ended); err != nil {
					t.Fatal(err)
				}
			} else {
				_ = r.Shutdown(context.Background())
			}
		})
	}
}

func TestWSRelayLocalFailureBudget(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		capacity int64
		payload  bool
	}{
		{"id512", strings.Repeat("x", 512), 8192, true},
		{"invalid-id", strings.Repeat("x", 513), 8192, false},
		{"capacity", "task", 128, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newWSFakePolicy()
			p.before = func(context.Context, wsDirection, ws.Opcode, []byte) (wsForwardDecision, error) {
				return wsForwardDecision{Action: wsRejectThenEnd, End: &wsPolicyEnd{At: time.Now(), Failure: errWSRelayPolicy, Code: 1008, Reason: "invalid event correlation", Local: &wsLocalTaskFailure{TaskID: tc.id, Kind: dsi.LocalPolicy}}}, nil
			}
			x := startWSPolicyTest(t, p, tc.capacity, nil, nil)
			x.client.send(t, ws.OpText, []byte("reject"))
			if tc.payload {
				f := x.client.read(t)
				if f.Opcode != ws.OpText || !strings.Contains(string(f.Payload), tc.id) || !strings.Contains(string(f.Payload), "Gateway.InvalidTask") {
					t.Fatal("本地失败不安全或无界")
				}
			}
			assertWSTestClose(t, x.client, 1008, "invalid event correlation")
			if err := x.join(t); !errors.Is(err, errWSRelayPolicy) {
				t.Fatal(err)
			}
		})
	}
}
