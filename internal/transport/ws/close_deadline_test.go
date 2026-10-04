package ws

import (
	"context"
	"errors"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func closeDeadlineBy[T any](t *testing.T, done <-chan T, deadline time.Time) T {
	t.Helper()
	timer := time.NewTimer(time.Until(deadline.Add(closeDeadlineSchedulingTolerance)))
	defer timer.Stop()
	select {
	case result := <-done:
		if time.Now().After(deadline.Add(closeDeadlineSchedulingTolerance)) {
			t.Fatal("迟到的结果不能因同时就绪而绕过时间上界")
		}
		return result
	case <-timer.C:
		t.Fatal("工作者越过绝对期限与固定调度容差")
		var zero T
		return zero
	}
}

// 独立兜底晚于验收界；否则测试清理本身会替代待验证的关闭守卫。
func closeDeadlineCleanup(t *testing.T, deadline time.Time, cleanup func()) {
	t.Helper()
	timer := time.AfterFunc(time.Until(deadline.Add(closeDeadlineSchedulingTolerance+time.Second)), cleanup)
	t.Cleanup(func() { timer.Stop(); cleanup() })
}

func closeDeadlineJoin(t *testing.T, workers *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	awaitDialHandoff(t, done, "独立清理后工作者仍未 join")
}

// 失败分支也归还尚未被断言接走的 CloseError，并等待真实读工作者退出。
func closeDeadlineRead(t *testing.T, c *Conn) <-chan error {
	t.Helper()
	done, exited := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(exited)
		m, err := c.ReadOwnedMessage(context.Background())
		m.Release()
		done <- err
	}()
	t.Cleanup(func() {
		_ = abortTransport(c.conn)
		awaitDialHandoff(t, exited, "独立清理后读工作者仍未退出")
		select {
		case err := <-done:
			releaseCloseError(err)
		default:
		}
	})
	return done
}

func closeGuardWorkers() int {
	stack := make([]byte, 1<<20)
	n := runtime.Stack(stack, true)
	return strings.Count(string(stack[:n]), "github.com/yobo2u/omugw/internal/transport/ws.(*Conn).watchTransportClose.func1(")
}

func TestArmCloseDeadlineOnlyTightens(t *testing.T) {
	t.Run("zero", func(t *testing.T) {
		a, _ := tcpPair(t)
		c := NewConn(a, RoleClient, 1024, 0)
		finish, err := c.ArmCloseDeadline(time.Time{})
		if !errors.Is(err, ErrProtocol) || finish != nil || c.closeDeadline.Load() != nil || c.closed.Load() || c.closing.Load() {
			t.Fatal("零期限未安全失败或接管了连接", err)
		}
	})
	t.Run("normal-writes-and-begin-close", func(t *testing.T) {
		a, raw := tcpPair(t)
		c := NewConn(a, RoleClient, 1024, 0)
		c.budget = productionBudget(t, 128)
		workers := closeGuardWorkers()
		initial := time.Now().Add(2 * time.Second)
		first, err := c.ArmCloseDeadline(initial)
		if err != nil {
			t.Fatal(err)
		}
		defer first()
		finishes := []func(){first}
		for range 24 {
			finish, err := c.ArmCloseDeadline(initial.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			finishes = append(finishes, finish)
		}
		if !c.closeDeadline.Load().Equal(initial) || c.closing.Load() || c.closed.Load() {
			t.Fatal("Arm 续期或提前封写")
		}
		_ = raw.SetDeadline(initial.Add(time.Second))
		if err := c.WriteMessage(OpText, []byte("after-arm")); err != nil {
			t.Fatal(err)
		}
		if f, err := ReadFrame(raw, 125); err != nil || f.Opcode != OpText || string(f.Payload) != "after-arm" {
			t.Fatal("Arm 发了额外帧或封住普通写", f, err)
		}
		if err := c.Ping([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		if f, err := ReadFrame(raw, 125); err != nil || f.Opcode != OpPing {
			t.Fatal(f, err)
		}
		deadline := time.Now().Add(120 * time.Millisecond)
		closeDeadlineCleanup(t, deadline, func() { _ = a.Close() })
		finish, err := c.ArmCloseDeadline(deadline)
		if err != nil {
			t.Fatal(err)
		}
		finishes = append(finishes, finish)
		read := closeDeadlineRead(t, c)
		sent, finish, err := c.BeginClose(1000, "local", initial)
		finishes = append(finishes, finish)
		if !sent || err != nil {
			t.Fatal(sent, err)
		}
		if f, err := ReadFrame(raw, 125); err != nil || string(f.Payload) != "\x03\xe8local" {
			t.Fatal(f, err)
		}
		if !c.closeDeadline.Load().Equal(deadline) || closeGuardWorkers() != workers+1 {
			t.Fatal("重复 Arm/BeginClose 未共用最早期限和唯一守卫")
		}
		if err := closeDeadlineBy(t, read, deadline); err == nil {
			t.Fatal("无取消的 reader 未到期退出")
		}
		var joins sync.WaitGroup
		for _, finish := range finishes {
			joins.Go(func() { finish(); finish() })
		}
		joined := make(chan struct{})
		go func() { joins.Wait(); close(joined) }()
		closeDeadlineBy(t, joined, deadline)
		if closeGuardWorkers() != workers || c.budget.Used() != 0 {
			t.Fatal("自然到期后的多 finish 未 join 或预算泄漏")
		}
		finish, err = c.ArmCloseDeadline(initial)
		if err != nil {
			t.Fatal(err)
		}
		finish()
		if closeGuardWorkers() != workers || !c.closeDeadline.Load().Equal(deadline) {
			t.Fatal("finish 后重装创建了新守卫或续期")
		}
	})
	t.Run("concurrent-tightening", func(t *testing.T) {
		a, _ := tcpPair(t)
		c := NewConn(a, RoleClient, 1024, 0)
		workers := closeGuardWorkers()
		deadline := time.Now().Add(120 * time.Millisecond)
		closeDeadlineCleanup(t, deadline, func() { _ = a.Close() })
		start := make(chan struct{})
		finishes := make(chan func(), 32)
		var arms sync.WaitGroup
		for i := range 32 {
			arms.Go(func() {
				<-start
				finish, err := c.ArmCloseDeadline(deadline.Add(time.Duration(i) * time.Second))
				if err != nil {
					t.Error(err)
				} else {
					t.Cleanup(finish)
				}
				finishes <- finish
			})
		}
		close(start)
		arms.Wait()
		close(finishes)
		if !c.closeDeadline.Load().Equal(deadline) || closeGuardWorkers() != workers+1 {
			t.Fatal("竞争装入丢了最早期限或创建了多个守卫")
		}
		if err := closeDeadlineBy(t, closeDeadlineRead(t, c), deadline); err == nil {
			t.Fatal("收紧未生效")
		}
		for finish := range finishes {
			finish()
		}
		if closeGuardWorkers() != workers {
			t.Fatal("守卫未 join")
		}
	})
	t.Run("finish-joins-abort", func(t *testing.T) {
		a, _ := tcpPair(t)
		gate := &closeAbortGate{Conn: a, entered: make(chan struct{}), release: make(chan struct{})}
		c := NewConn(gate, RoleClient, 1024, 0)
		deadline := time.Now().Add(time.Second)
		closeDeadlineCleanup(t, deadline, func() { gate.unblock(); _ = a.Close() })
		first, err := c.ArmCloseDeadline(deadline)
		if err != nil {
			t.Fatal(err)
		}
		second, err := c.ArmCloseDeadline(deadline.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{}, 4)
		var workers sync.WaitGroup
		t.Cleanup(func() { gate.unblock(); closeDeadlineJoin(t, &workers) })
		for _, finish := range []func(){first, second, first, second} {
			workers.Go(func() { finish(); done <- struct{}{} })
		}
		closeDeadlineBy(t, gate.entered, deadline)
		select {
		case <-done:
			t.Fatal("finish 在物理中止工作者返回前冒充 join")
		default:
		}
		gate.unblock()
		for range 4 {
			closeDeadlineBy(t, done, deadline)
		}
	})
}

// 闩在物理 Close 返回前，区分“已发送停止信号”与“确实 join 完成”。
type closeAbortGate struct {
	net.Conn
	entered, release       chan struct{}
	enterOnce, releaseOnce sync.Once
}

func (g *closeAbortGate) Close() error {
	err := g.Conn.Close()
	g.enterOnce.Do(func() { close(g.entered) })
	<-g.release
	return err
}
func (g *closeAbortGate) unblock() { g.releaseOnce.Do(func() { close(g.release) }) }

func TestArmCloseDeadlineCoversBlockedIO(t *testing.T) {
	for _, mode := range []string{"write-lock", "ping-lock", "begin-lock", "auto-pong", "peer-close"} {
		t.Run(mode, func(t *testing.T) {
			a, raw := tcpPair(t)
			gate := &handshakeWriteGate{Conn: a, entered: make(chan struct{}), closed: make(chan struct{})}
			c := NewConn(gate, RoleClient, 1024, 0)
			c.budget = productionBudget(t, 128)
			c.writeTimeout = 5 * time.Second
			setupDeadline := time.Now().Add(time.Second)
			closeDeadlineCleanup(t, setupDeadline, func() { _ = gate.Close() })
			var writers sync.WaitGroup
			t.Cleanup(func() { _ = gate.Close(); closeDeadlineJoin(t, &writers) })
			var first, read <-chan error
			locked := strings.HasSuffix(mode, "-lock")
			if locked {
				done := make(chan error, 1)
				writers.Go(func() { done <- c.WriteMessage(OpText, []byte("holding-lock")) })
				first = done
			} else {
				read = closeDeadlineRead(t, c)
				op, payload := OpPing, []byte("heartbeat")
				if mode == "peer-close" {
					op, payload = OpClose, []byte("\x03\xe9peer-original")
				}
				_ = raw.SetDeadline(setupDeadline.Add(time.Second))
				if err := WriteFrame(raw, Frame{FIN: true, Opcode: op, Payload: payload}, false); err != nil {
					t.Fatal(err)
				}
			}
			closeDeadlineBy(t, gate.entered, setupDeadline)
			deadline := time.Now().Add(120 * time.Millisecond)
			finish, err := c.ArmCloseDeadline(deadline)
			if err != nil {
				t.Fatal(err)
			}
			defer finish()
			if locked {
				done := make(chan error, 1)
				writers.Go(func() {
					switch mode {
					case "write-lock":
						done <- c.WriteMessage(OpText, []byte("waiting-lock"))
					case "ping-lock":
						done <- c.Ping([]byte("waiting-lock"))
					case "begin-lock":
						sent, join, err := c.BeginClose(1000, "", deadline.Add(time.Second))
						join()
						if sent {
							t.Error("未写完的 close 冒充发送")
						}
						done <- err
					}
				})
				if err := closeDeadlineBy(t, done, deadline); err == nil {
					t.Fatal("等锁后仍写成功")
				}
				if err := closeDeadlineBy(t, first, deadline); err == nil {
					t.Fatal("锁持有者未中止")
				}
			} else {
				err := closeDeadlineBy(t, read, deadline)
				var ce *CloseError
				if mode == "peer-close" {
					if !errors.As(err, &ce) || ce.Code != 1001 || ce.Reason != "peer-original" {
						t.Fatal("自动回应阻塞丢了实际 close 事实", err)
					}
					if c.budget.Used() != int64(len("peer-original")) {
						t.Fatal("关闭原因所有权未交接")
					}
					copyOfClose := *ce
					ce.Release()
					copyOfClose.Release()
				} else if err == nil {
					t.Fatal("自动 pong 未受同一期限约束")
				}
			}
			finish()
			if !c.closeDeadline.Load().Equal(deadline) || c.budget.Used() != 0 {
				t.Fatal("等锁/自动回应续期或预算泄漏")
			}
		})
	}
	for _, duringTLS := range []bool{false, true} {
		name := "tls-arm-before-close"
		if duringTLS {
			name = "tls-tighten-during-close"
		}
		t.Run(name, func(t *testing.T) {
			client, server, gate := tlsClosePair(t)
			c := NewConn(client, RoleClient, 1024, 0)
			c.budget = productionBudget(t, 128)
			setupDeadline := time.Now().Add(time.Second)
			closeDeadlineCleanup(t, setupDeadline, func() { _ = gate.Close(); _ = server.NetConn().Close() })
			deadline := time.Now().Add(120 * time.Millisecond)
			var finish func()
			if !duringTLS {
				var err error
				finish, err = c.ArmCloseDeadline(deadline)
				if err != nil {
					t.Fatal(err)
				}
				defer finish()
			}
			read := closeDeadlineRead(t, c)
			_ = server.SetDeadline(setupDeadline.Add(time.Second))
			if err := WriteFrame(server, Frame{FIN: true, Opcode: OpClose, Payload: []byte("\x03\xe9tls-peer")}, false); err != nil {
				t.Fatal(err)
			}
			if f, err := ReadFrame(server, 125); err != nil || f.Opcode != OpClose {
				t.Fatal("TLS 未收到实际回应", f, err)
			}
			closeDeadlineBy(t, gate.alertEntered, setupDeadline)
			if duringTLS {
				deadline = time.Now().Add(120 * time.Millisecond)
				var err error
				finish, err = c.ArmCloseDeadline(deadline)
				if err != nil {
					t.Fatal(err)
				}
				defer finish()
			}
			later, err := c.ArmCloseDeadline(deadline.Add(2 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			defer later()
			err = closeDeadlineBy(t, read, deadline)
			var ce *CloseError
			if !errors.As(err, &ce) || ce.Code != 1001 || ce.Reason != "tls-peer" {
				t.Fatal("TLS 释放抹掉 close 事实", err)
			}
			ce.Release()
			finish()
			later()
			closeDeadlineBy(t, gate.alertExited, deadline)
			if gate.closedAt.After(deadline.Add(closeDeadlineSchedulingTolerance)) || !c.closeDeadline.Load().Equal(deadline) || c.budget.Used() != 0 {
				t.Fatal("TLS 强制释放超出共享期限或泄漏预算")
			}
		})
	}
}

// 记录真实 TCP 的写期限调用，防止仅靠最终强制关闭掩盖 write/ping/pong 另起预算。
type closeDeadlineProbe struct {
	net.Conn
	deadlines chan time.Time
}

func (p *closeDeadlineProbe) SetWriteDeadline(d time.Time) error {
	if !d.IsZero() {
		p.deadlines <- d
	}
	return p.Conn.SetWriteDeadline(d)
}

func TestArmCloseDeadlineLimitsWrites(t *testing.T) {
	for _, timeout := range []time.Duration{0, 5 * time.Second, 40 * time.Millisecond} {
		t.Run(timeout.String(), func(t *testing.T) {
			a, raw := tcpPair(t)
			probe := &closeDeadlineProbe{Conn: a, deadlines: make(chan time.Time, 8)}
			c := NewConn(probe, RoleClient, 1024, 0)
			c.writeTimeout = timeout
			deadline := time.Now().Add(120 * time.Millisecond)
			closeDeadlineCleanup(t, deadline, func() { _ = a.Close(); _ = raw.Close() })
			finish, err := c.ArmCloseDeadline(deadline)
			if err != nil {
				t.Fatal(err)
			}
			defer finish()
			_ = raw.SetDeadline(deadline.Add(closeDeadlineSchedulingTolerance + time.Second))
			for _, op := range []Opcode{OpText, OpPing, OpPong} {
				var read <-chan error
				start := time.Now()
				switch op {
				case OpText:
					err = c.WriteMessage(op, []byte("payload"))
				case OpPing:
					err = c.Ping([]byte("payload"))
				case OpPong:
					read = closeDeadlineRead(t, c)
					err = WriteFrame(raw, Frame{FIN: true, Opcode: OpPing, Payload: []byte("payload")}, false)
				}
				if err != nil {
					t.Fatal(err)
				}
				if f, err := ReadFrame(raw, 125); err != nil || f.Opcode != op || string(f.Payload) != "payload" {
					t.Fatal("普通写或自动控制帧被 Arm 改变", f, err)
				}
				applied := closeDeadlineBy(t, probe.deadlines, deadline)
				if applied.After(deadline) || (timeout == 0 || timeout == 5*time.Second) && !applied.Equal(deadline) {
					t.Fatal("实际写期限越过已 Arm 的绝对值", applied, deadline)
				}
				if timeout == 40*time.Millisecond && (applied.Before(start.Add(timeout)) || applied.After(time.Now().Add(timeout))) {
					t.Fatal("Arm 延长了更短的普通写期限", applied)
				}
				if read != nil {
					if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpText, Payload: []byte("done")}, false); err != nil {
						t.Fatal(err)
					}
					if err := closeDeadlineBy(t, read, deadline); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
