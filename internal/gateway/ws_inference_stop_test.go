package gateway

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/config"
	dsi "github.com/yobo2u/omugw/internal/protocol/dashscopeinference"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 只扩大真实 Before/Expire/After 已封口、原 owner 尚未 report 的窗口；不伪造决策。
// failed/expire 两例来自 final-review 的外部 overlay，保留真实 policy 与本地 TCP。
type inferenceStopBarrier struct {
	*wsInferencePolicy
	entered chan *wsPolicyEnd
	release chan struct{}
	errors  chan error
	waiting chan struct{}
	receipt chan error
	once    sync.Once
}

func (p *inferenceStopBarrier) unblock() { p.once.Do(func() { close(p.release) }) }

func (p *inferenceStopBarrier) barrier(end *wsPolicyEnd) {
	p.entered <- end
	<-p.release
}

func (p *inferenceStopBarrier) BeforeForward(ctx context.Context, dir wsDirection, op ws.Opcode, raw []byte) (wsForwardDecision, error) {
	if dir == wsClientToUpstream && op == ws.OpBinary {
		// Done 仅在真实 policy 进入门闩 select 时求值，不能把 goroutine 已启动当成已等待。
		ctx = &inferenceWaitContext{Context: ctx, waiting: p.waiting}
	}
	d, err := p.wsInferencePolicy.BeforeForward(ctx, dir, op, raw)
	if d.End != nil {
		p.barrier(d.End)
	}
	if err != nil {
		p.errors <- err
	}
	return d, err
}

func (p *inferenceStopBarrier) Expire(d wsPolicyDeadline, now time.Time) *wsPolicyEnd {
	end := p.wsInferencePolicy.Expire(d, now)
	if end != nil {
		p.barrier(end)
	}
	return end
}

func (p *inferenceStopBarrier) AfterForward(ticket wsForwardTicket, err error) {
	p.wsInferencePolicy.AfterForward(ticket, err)
	if ticket.Step == wsStepStarted && err != nil {
		p.receipt <- err
		p.barrier(nil)
	}
}

// 写错只注入一次，让 started 的真实 Write/After/report 走完，close 仍可验证实际 wire。
type inferenceFailedWrite struct {
	net.Conn
	armed   atomic.Bool
	entered chan struct{}
	allow   chan struct{}
	err     error
}

func (c *inferenceFailedWrite) Write(raw []byte) (int, error) {
	if c.armed.CompareAndSwap(true, false) {
		close(c.entered)
		<-c.allow
		return 0, c.err
	}
	return c.Conn.Write(raw)
}

type inferenceStopHarness struct {
	p              *inferenceStopBarrier
	client, server *wsTestPeer
	b              *ws.BufferBudget
	r              *wsRegistry
	s              *wsSession
	cancel         context.CancelFunc
	done           chan error
	timeouts       config.Timeouts
}

func startInferenceStopTest(t *testing.T, expire bool, wrap func(net.Conn) net.Conn) *inferenceStopHarness {
	t.Helper()
	x := &inferenceStopHarness{b: wsTestBudget(t, 1<<20), done: make(chan error, 1), timeouts: config.Timeouts{Connect: 200 * time.Millisecond, FirstByte: time.Second, Idle: 3 * time.Second}}
	if expire {
		x.timeouts.FirstByte = 80 * time.Millisecond
	}
	binding, err := newWSInferenceBinding(inferenceTarget())
	if err != nil {
		t.Fatal(err)
	}
	x.p = &inferenceStopBarrier{wsInferencePolicy: newWSInferencePolicy(binding, inferenceRouter(t), inferenceMatrix(t, true), nil, x.timeouts, nil), entered: make(chan *wsPolicyEnd, 1), release: make(chan struct{}), errors: make(chan error, 2), waiting: make(chan struct{}), receipt: make(chan error, 1)}
	down, client := wsTestLink(t, false, x.b, 1<<18, 0, wrap)
	up, server := wsTestLink(t, true, x.b, 1<<18, 0, nil)
	x.client, x.server = client, server
	ctx, cancel := context.WithCancel(context.Background())
	x.cancel = cancel
	x.r = newWSRegistry(1, wsCloseBudget(x.timeouts))
	x.s, err = x.r.Register(ctx)
	if err != nil || !x.s.Attach(down) || !x.s.Attach(up) {
		t.Fatal("注册/挂接失败", err)
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		x.done <- relayWS(x.s.Context(), down, up, nil, wsRelayOptions{Policy: x.p, ClassifyClose: dsi.ClassifyClose, Timeouts: x.timeouts, Budget: x.b, MaxMessageBytes: 1 << 18})
	}()
	t.Cleanup(func() {
		x.p.unblock()
		cancel()
		_ = client.Close()
		_ = server.Close()
		awaitWSTest(t, exited)
		x.s.Done()
		if err := x.r.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
		inferenceEmpty(t, x.r, x.b)
	})
	return x
}

func (x *inferenceStopHarness) run(t *testing.T, started bool) {
	t.Helper()
	run := inferenceRun("a", inferenceTTS, "tts", "duplex")
	x.client.send(t, ws.OpText, run)
	inferenceWire(t, x.server, ws.OpText, run)
	if started {
		s := inferenceServer("a", "task-started", `{}`)
		x.server.send(t, ws.OpText, s)
		inferenceWire(t, x.client, ws.OpText, s)
	}
}

func (x *inferenceStopHarness) join(t *testing.T, at time.Time, budget time.Duration) error {
	t.Helper()
	var result error
	select {
	case result = <-x.done:
	case <-time.After(max(0, time.Until(at.Add(budget+wsSchedulingSlack())))):
		t.Fatal("原预算内未 join")
	}
	if time.Since(at) > budget+wsSchedulingSlack() {
		t.Error("原绝对预算被延长")
	}
	if !x.p.finished || x.b.Used() != 0 {
		t.Fatalf("未 join/Finish/归还: finished=%v bytes=%d", x.p.finished, x.b.Used())
	}
	x.s.Done()
	inferenceEmpty(t, x.r, x.b)
	return result
}

func TestInferenceStopBeforeSelection(t *testing.T) {
	for _, mode := range []string{"task-failed", "task-failed-tail", "expire", "reject", "started-write-failure"} {
		t.Run(mode, func(t *testing.T) {
			var write *inferenceFailedWrite
			var wrap func(net.Conn) net.Conn
			var allow sync.Once
			if mode == "started-write-failure" {
				wrap = func(c net.Conn) net.Conn {
					write = &inferenceFailedWrite{Conn: c, entered: make(chan struct{}), allow: make(chan struct{}), err: errors.New("private started write failure")}
					return write
				}
			}
			x := startInferenceStopTest(t, mode == "expire", wrap)
			if write != nil {
				t.Cleanup(func() { allow.Do(func() { close(write.allow) }) })
			}
			x.run(t, mode == "task-failed" || mode == "task-failed-tail" || mode == "reject")
			failed := inferenceServer("a", "task-failed", `{}`)
			if mode == "task-failed" || mode == "task-failed-tail" {
				x.server.send(t, ws.OpText, failed)
			} else if mode == "reject" {
				x.server.send(t, ws.OpText, inferenceServer("a", "future", `{"model":null}`))
			} else if write != nil {
				write.armed.Store(true)
				x.server.send(t, ws.OpText, inferenceServer("a", "task-started", `{}`))
				awaitWSTest(t, write.entered)
				x.client.send(t, ws.OpBinary, []byte{1})
				awaitWSTest(t, x.p.waiting)
				allow.Do(func() { close(write.allow) })
			}
			end := receiveWSTest(t, x.p.entered)
			if write == nil {
				x.client.send(t, ws.OpText, inferenceClient("a", "future-action", `{}`))
			}
			internal := receiveWSTest(t, x.p.errors)
			term := x.s.shutdown.termination
			// 旧实现已给出错误的取消信号时，等其真实认领再放原 owner，稳定保留 RED 的 1001 实证。
			if errors.Is(internal, context.Canceled) {
				awaitWSTest(t, term.selected)
				t.Error("内部封口冒充 context.Canceled")
			}
			if internal != errWSPolicyStopped {
				t.Errorf("不是专用封口哨兵: %v", internal)
			}
			if x.s.Context().Err() != nil {
				t.Fatal("反例意外依赖了外部取消")
			}
			at := time.Now()
			if end != nil {
				at = end.At
			}
			tail := inferenceServer("a", "future-tail", `{"model":"cosyvoice-v2","extension":[null,7]}`)
			if mode == "task-failed-tail" {
				// 原文 owner 尚在屏障，上游尾音/文本/真实 close 排队，只有原 reader 可按序消费。
				x.server.send(t, ws.OpBinary, []byte{0, 255, 128})
				x.server.send(t, ws.OpText, tail)
				x.server.send(t, ws.OpClose, ws.EncodeClosePayload(4003, "peer after tail"))
			}
			x.p.unblock()
			first := x.client.read(t)
			wantCode, reason := uint16(1011), "upstream task failed"
			want := failed
			switch mode {
			case "task-failed-tail":
				wantCode, reason = 4003, "peer after tail"
			case "expire":
				reason = "task_start_timeout"
				want = []byte(`{"header":{"event":"task-failed","task_id":"a","error_code":"Gateway.TaskStartTimeout","error_message":"task did not start within gateway budget"},"payload":{}}`)
			case "reject":
				wantCode, reason = 1008, "invalid task envelope or binding"
				want = []byte(`{"header":{"event":"task-failed","task_id":"a","error_code":"Gateway.InvalidTask","error_message":"invalid task envelope or binding"},"payload":{}}`)
			case "started-write-failure":
				want, reason = nil, "downstream connection failed"
				if err := receiveWSTest(t, x.p.receipt); err != write.err {
					t.Errorf("After 未收到原写错: %v", err)
				}
			}
			if want != nil {
				if first.Opcode != ws.OpText || !bytes.Equal(first.Payload, want) {
					t.Errorf("失败文本未先交付: op=%d payload=%q", first.Opcode, first.Payload)
				} else {
					if mode == "task-failed-tail" {
						inferenceWire(t, x.client, ws.OpBinary, []byte{0, 255, 128})
						inferenceWire(t, x.client, ws.OpText, tail)
					}
					first = x.client.read(t)
				}
			}
			if first.Opcode != ws.OpClose || !bytes.Equal(first.Payload, ws.EncodeClosePayload(wantCode, reason)) {
				t.Errorf("close=%q want=%d/%s", first.Payload, wantCode, reason)
			}
			// 保留物理对端至 relay 自行收尾，不能提前断 TCP 掩盖预算/worker 泄漏。
			result := x.join(t, at, 2*wsCloseBudget(x.timeouts))
			t.Logf("internal=%v external_ctx=nil result=%v outcome=%s first_close=%q budget=%d", internal, result, wsOutcome(result), first.Payload, x.b.Used())
			if end != nil && result != end.Failure || write != nil && result != errWSRelayDownstream {
				t.Errorf("原 owner 失败被覆盖: %v", result)
			}
			if wsOutcome(result) == "cancelled" || result == nil {
				t.Errorf("失败结果被误记: %v", result)
			}
			term.mu.Lock()
			deadline := term.deadline
			term.mu.Unlock()
			if deadline.After(at.Add(2 * wsCloseBudget(x.timeouts))) {
				t.Error("认领时重启预算")
			}
		})
	}
}

func TestInferenceStopStillAllowsExternalTermination(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		name := "context-cancel"
		if shutdown {
			name = "registry-shutdown"
		}
		t.Run(name, func(t *testing.T) {
			x := startInferenceStopTest(t, false, nil)
			x.run(t, true)
			x.server.send(t, ws.OpText, inferenceServer("a", "task-failed", `{}`))
			_ = receiveWSTest(t, x.p.entered)
			x.client.send(t, ws.OpText, inferenceClient("a", "future-action", `{}`))
			if err := receiveWSTest(t, x.p.errors); err != errWSPolicyStopped {
				t.Fatal("未停在内部封口窗口", err)
			}
			start := time.Now()
			var shutdownDone chan error
			want := error(context.Canceled)
			if shutdown {
				want = errWSShutdown
				shutdownDone = make(chan error, 1)
				go func() { shutdownDone <- x.r.Shutdown(context.Background()) }()
			} else {
				x.cancel()
			}
			term := x.s.shutdown.termination
			awaitWSTest(t, term.selected)
			term.mu.Lock()
			deadline := term.deadline
			term.mu.Unlock()
			x.p.unblock()
			inferenceWireClose(t, x.client, 1001, "")
			inferenceWireClose(t, x.server, 1001, "")
			if err := x.join(t, start, wsCloseBudget(x.timeouts)); !errors.Is(err, want) {
				t.Fatalf("真实取消/停机被吞或覆盖: got=%v want=%v", err, want)
			}
			assertWSDeadline(t, term, deadline)
			if shutdownDone != nil {
				if err := receiveWSTest(t, shutdownDone); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestInferencePolicyStopSignalAndContext(t *testing.T) {
	for _, finish := range []bool{false, true} {
		p, _, _ := inferencePolicy(t)
		if finish {
			p.Finish()
		} else {
			p.Stop()
		}
		for _, dir := range []wsDirection{wsClientToUpstream, wsUpstreamToClient} {
			inferenceExpectStopped(t, p, dir, ws.OpBinary, []byte{1})
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			d, err := p.BeforeForward(ctx, dir, ws.OpBinary, []byte{1})
			if !errors.Is(err, ctx.Err()) || err == errWSPolicyStopped || d != (wsForwardDecision{}) {
				t.Fatalf("真实 ctx.Err 被内部封口掩盖: %+v %v", d, err)
			}
		}
	}
	p, _, _ := inferencePolicy(t)
	inferenceSend(t, p, wsClientToUpstream, inferenceRun("a", inferenceASR, "asr", "duplex"))
	started := inferenceBefore(t, p, wsUpstreamToClient, ws.OpText, inferenceServer("a", "task-started", `{}`))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := &inferenceWaitContext{Context: ctx, waiting: make(chan struct{})}
	done := make(chan inferenceResult, 1)
	go func() {
		d, err := p.BeforeForward(waiting, wsClientToUpstream, ws.OpBinary, []byte{1})
		done <- inferenceResult{d, err}
	}()
	awaitWSTest(t, waiting.waiting)
	cancel()
	r := receiveWSTest(t, done)
	if !errors.Is(r.err, ctx.Err()) || r.err == errWSPolicyStopped || r.decision != (wsForwardDecision{}) {
		t.Fatalf("门闩真实取消不符: %+v", r)
	}
	p.AfterForward(started.Ticket, nil)
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := p.BeforeForward(ctx, wsClientToUpstream, ws.OpBinary, []byte{1})
	if !errors.Is(err, ctx.Err()) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("真实截止被吞", err)
	}
}
