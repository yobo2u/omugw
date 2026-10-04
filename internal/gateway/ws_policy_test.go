package gateway

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 完整 fake 只模拟准入/交付门闩，不借未来 Inference 实现生成期望。
type wsFakePolicy struct {
	before   func(context.Context, wsDirection, ws.Opcode, []byte) (wsForwardDecision, error)
	after    func(wsForwardTicket, error)
	expire   func(wsPolicyDeadline, time.Time) *wsPolicyEnd
	mu       sync.Mutex
	deadline wsPolicyDeadline
	changed  chan struct{}
	stopped  chan struct{}
	once     sync.Once
	finished atomic.Int32
}

func newWSFakePolicy() *wsFakePolicy {
	return &wsFakePolicy{changed: make(chan struct{}, 1), stopped: make(chan struct{})}
}
func (p *wsFakePolicy) BeforeForward(ctx context.Context, d wsDirection, op ws.Opcode, b []byte) (wsForwardDecision, error) {
	if p.before != nil {
		return p.before(ctx, d, op, b)
	}
	return wsForwardDecision{}, nil
}
func (p *wsFakePolicy) AfterForward(ticket wsForwardTicket, err error) {
	if p.after != nil {
		p.after(ticket, err)
	}
}
func (p *wsFakePolicy) Deadline() wsPolicyDeadline {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.deadline
}
func (p *wsFakePolicy) Changed() <-chan struct{} { return p.changed }
func (p *wsFakePolicy) Expire(d wsPolicyDeadline, now time.Time) *wsPolicyEnd {
	if p.expire != nil {
		return p.expire(d, now)
	}
	return nil
}
func (p *wsFakePolicy) Stop()   { p.once.Do(func() { close(p.stopped) }) }
func (p *wsFakePolicy) Finish() { p.finished.Add(1) }
func (p *wsFakePolicy) setDeadline(d wsPolicyDeadline) {
	p.mu.Lock()
	p.deadline = d
	p.mu.Unlock()
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

type wsPolicyHarness struct {
	down, up       *ws.Conn
	client, server *wsTestPeer
	b              *ws.BufferBudget
	p              *wsFakePolicy
	cancel         context.CancelFunc
	done           chan error
	opts           wsRelayOptions
	s              *wsSession
}

func startWSPolicyTest(t *testing.T, p *wsFakePolicy, capacity int64, wrap func(net.Conn) net.Conn, registry *wsRegistry) *wsPolicyHarness {
	t.Helper()
	x := &wsPolicyHarness{p: p, b: wsTestBudget(t, capacity), done: make(chan error, 1)}
	x.down, x.client = wsTestLink(t, false, x.b, 4<<20, 0, wrap)
	x.up, x.server = wsTestLink(t, true, x.b, 4<<20, 0, nil)
	x.opts = wsRelayOptions{Policy: p, Budget: x.b, MaxMessageBytes: 4 << 20, Timeouts: config.Timeouts{Connect: 80 * time.Millisecond, Idle: 5 * time.Second}}
	ctx, cancel := context.WithCancel(context.Background())
	x.cancel = cancel
	if registry != nil {
		var err error
		x.s, err = registry.Register(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !x.s.Attach(x.down) || !x.s.Attach(x.up) {
			t.Fatal("挂接失败")
		}
		ctx = x.s.Context()
	}
	t.Cleanup(cancel)
	exited := make(chan struct{})
	go func() { defer close(exited); x.done <- relayWS(ctx, x.down, x.up, nil, x.opts) }()
	t.Cleanup(func() {
		cancel()
		_ = x.client.Close()
		_ = x.server.Close()
		awaitWSTest(t, exited)
		if x.s != nil {
			x.s.Done()
		}
	})
	return x
}
func (x *wsPolicyHarness) join(t *testing.T) error {
	t.Helper()
	err := receiveWSTest(t, x.done)
	if x.b.Used() != 0 || x.p.finished.Load() != 1 {
		t.Fatalf("未归还/join: bytes=%d finish=%d", x.b.Used(), x.p.finished.Load())
	}
	if x.s != nil {
		x.s.Done()
	}
	return err
}

func TestWSRelayPolicyDeliveryBarriers(t *testing.T) {
	for _, step := range []wsForwardStep{wsStepStarted, wsStepTerminal} {
		t.Run(map[wsForwardStep]string{wsStepStarted: "started", wsStepTerminal: "terminal"}[step], func(t *testing.T) {
			p := newWSFakePolicy()
			afterEntered, afterAllow, clientWaiting, clientAdmit := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			var beforeCount, afterCount atomic.Int32
			var x *wsPolicyHarness
			p.before = func(_ context.Context, d wsDirection, _ ws.Opcode, _ []byte) (wsForwardDecision, error) {
				if beforeCount.Add(1) > 2 {
					return wsForwardDecision{}, errWSRelayPolicy
				}
				if d == wsClientToUpstream {
					close(clientWaiting)
					select {
					case <-clientAdmit:
					case <-p.stopped:
					}
				}
				return wsForwardDecision{Ticket: wsForwardTicket{Generation: 1, Step: step}}, nil
			}
			p.after = func(_ wsForwardTicket, err error) {
				if err != nil {
					t.Error(err)
				}
				if afterCount.Add(1) == 1 {
					close(afterEntered)
					select {
					case <-afterAllow:
					case <-p.stopped:
						return
					}
					if x.b.Used() < int64(len("terminal")+len("next")) {
						t.Error("After 前提前 Release")
					}
					close(clientAdmit)
				}
			}
			x = startWSPolicyTest(t, p, 4096, nil, nil)
			x.server.send(t, ws.OpText, []byte("terminal"))
			_ = x.client.read(t)
			awaitWSTest(t, afterEntered)
			x.client.send(t, ws.OpText, []byte("next"))
			awaitWSTest(t, clientWaiting)
			x.client.send(t, ws.OpText, []byte("queued"))
			if x.b.Used() != int64(len("terminal")+len("next")) || beforeCount.Load() != 2 {
				t.Fatal("门闩等待读取了额外消息")
			}
			// 让第二条客户端输入在原 worker 上失败，避免 fake 重复关闭门闩。
			close(afterAllow)
			if f := x.server.read(t); string(f.Payload) != "next" {
				t.Fatal("交付顺序改变")
			}
			x.cancel()
			_ = x.join(t)
			if afterCount.Load() != 2 {
				t.Fatal("成功 ticket 没有恰好 After 一次")
			}
		})
	}
}

func TestWSRelayPolicyRunAndLateFinishTickets(t *testing.T) {
	p := newWSFakePolicy()
	var generation atomic.Uint64
	finishAfter, terminalAfter, finishDone := make(chan struct{}), make(chan struct{}), make(chan wsForwardTicket, 1)
	var calls atomic.Int32
	p.before = func(_ context.Context, d wsDirection, _ ws.Opcode, b []byte) (wsForwardDecision, error) {
		calls.Add(1)
		step := wsStepNone
		switch string(b) {
		case "run":
			if d != wsClientToUpstream {
				t.Error("方向反转")
			}
			generation.Add(1)
			step = wsStepRun
		case "started":
			if generation.Load() != 1 {
				t.Error("run 尚未登记却能瞬回 started")
			}
			step = wsStepStarted
		case "finish":
			step = wsStepFinish
		case "terminal":
			step = wsStepTerminal
		}
		return wsForwardDecision{Ticket: wsForwardTicket{Generation: generation.Load(), Step: step}}, nil
	}
	p.after = func(ticket wsForwardTicket, err error) {
		if err != nil {
			t.Error(err)
		}
		switch ticket.Step {
		case wsStepFinish:
			close(finishAfter)
			select {
			case <-terminalAfter:
			case <-p.stopped:
				return
			}
			finishDone <- ticket
		case wsStepTerminal:
			// 对向 Before/After 可先推进 generation，迟到回调必须仍带原 ticket。
			generation.Store(2)
			close(terminalAfter)
		}
	}
	x := startWSPolicyTest(t, p, 4096, nil, nil)
	x.client.send(t, ws.OpText, []byte("run"))
	_ = x.server.read(t)
	if generation.Load() != 1 {
		t.Fatal("run Write 早于 Before 登记")
	}
	x.server.send(t, ws.OpText, []byte("started"))
	_ = x.client.read(t)
	x.client.send(t, ws.OpText, []byte("finish"))
	_ = x.server.read(t)
	awaitWSTest(t, finishAfter)
	x.server.send(t, ws.OpText, []byte("terminal"))
	_ = x.client.read(t)
	if ticket := receiveWSTest(t, finishDone); ticket.Generation != 1 || generation.Load() != 2 {
		t.Fatal("迟到 finish ticket 串代")
	}
	x.cancel()
	_ = x.join(t)
	if calls.Load() != 4 {
		t.Fatal("Before 重复")
	}
}

func TestWSRelayPolicyTimerAndStop(t *testing.T) {
	t.Run("stale-expire-in-flight", func(t *testing.T) {
		p := newWSFakePolicy()
		entered, allow := make(chan struct{}), make(chan struct{})
		p.expire = func(d wsPolicyDeadline, now time.Time) *wsPolicyEnd {
			if d.Revision == 1 {
				close(entered)
				select {
				case <-allow:
				case <-p.stopped:
					return nil
				}
			}
			if d != p.Deadline() {
				return nil
			}
			return &wsPolicyEnd{At: now, Failure: errWSFakeTask, Code: 1011, Reason: "upstream task failed"}
		}
		p.setDeadline(wsPolicyDeadline{At: time.Now().Add(-time.Second), Revision: 1})
		x := startWSPolicyTest(t, p, 4096, nil, nil)
		awaitWSTest(t, entered)
		p.setDeadline(wsPolicyDeadline{Revision: 2})
		close(allow)
		x.server.send(t, ws.OpText, []byte("new-phase"))
		if f := x.client.read(t); string(f.Payload) != "new-phase" {
			t.Fatal("旧 Expire 终止了新阶段")
		}
		x.cancel()
		if err := x.join(t); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("ping-and-revision", func(t *testing.T) {
		p := newWSFakePolicy()
		expired := make(chan wsPolicyDeadline, 1)
		p.expire = func(d wsPolicyDeadline, now time.Time) *wsPolicyEnd {
			if d != p.Deadline() || now.Before(d.At) {
				return nil
			}
			expired <- d
			return &wsPolicyEnd{At: now, Failure: errWSRelayPolicy, Code: 1008, Reason: "invalid event correlation"}
		}
		p.setDeadline(wsPolicyDeadline{At: time.Now().Add(-time.Second), Revision: 1})
		p.setDeadline(wsPolicyDeadline{At: time.Now().Add(100 * time.Millisecond), Revision: 2})
		x := startWSPolicyTest(t, p, 4096, nil, nil)
		x.server.send(t, ws.OpPing, []byte("alive"))
		if f := x.server.read(t); f.Opcode != ws.OpPong {
			t.Fatal("无 pong")
		}
		if d := receiveWSTest(t, expired); d.Revision != 2 {
			t.Fatal("旧 revision 击中新阶段")
		}
		assertWSTestClose(t, x.client, 1008, "invalid event correlation")
		if err := x.join(t); !errors.Is(err, errWSRelayPolicy) {
			t.Fatal(err)
		}
	})
	t.Run("shutdown-unblocks-before", func(t *testing.T) {
		p := newWSFakePolicy()
		waiting := make(chan struct{})
		after := make(chan error, 1)
		p.before = func(context.Context, wsDirection, ws.Opcode, []byte) (wsForwardDecision, error) {
			close(waiting)
			<-p.stopped
			return wsForwardDecision{Ticket: wsForwardTicket{Step: wsStepRun}}, nil
		}
		p.after = func(_ wsForwardTicket, err error) { after <- err }
		r := newWSRegistry(1, 80*time.Millisecond)
		x := startWSPolicyTest(t, p, 4096, nil, r)
		x.client.send(t, ws.OpText, []byte("blocked"))
		awaitWSTest(t, waiting)
		shutdown := make(chan error, 1)
		go func() { shutdown <- r.Shutdown(context.Background()) }()
		assertWSTestClose(t, x.client, 1001, "")
		assertWSTestClose(t, x.server, 1001, "")
		if err := receiveWSTest(t, after); !errors.Is(err, context.Canceled) {
			t.Fatalf("取消 ticket 未 After: %v", err)
		}
		if err := x.join(t); !errors.Is(err, errWSShutdown) {
			t.Fatal(err)
		}
		if err := receiveWSTest(t, shutdown); err != nil {
			t.Fatal(err)
		}
	})
}
