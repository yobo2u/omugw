package ws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func handshakeRead(c *Conn) <-chan error {
	done := make(chan error, 1)
	go func() {
		m, err := c.ReadOwnedMessage(context.Background())
		m.Release()
		done <- err
	}()
	return done
}

func handshakeReadBy(t *testing.T, c *Conn, done <-chan error, deadline time.Time) error {
	t.Helper()
	timer := time.NewTimer(time.Until(deadline.Add(100 * time.Millisecond)))
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		t.Error("读/TLS worker 未在同一绝对期限退出")
		_ = abortTransport(c.conn)
		return awaitDialHandoff(t, done, "强制关闭后读 worker 仍未退出")
	}
}

func TestCloseHandshakePhysicalSingleFrame(t *testing.T) {
	for _, role := range []Role{RoleClient, RoleServer} {
		t.Run(fmt.Sprint(role), func(t *testing.T) {
			a, raw := tcpPair(t)
			c := NewConn(a, role, 1024, 0)
			c.budget = productionBudget(t, 128)
			deadline := time.Now().Add(300 * time.Millisecond)
			_ = raw.SetDeadline(deadline.Add(time.Second))
			done := handshakeRead(c)
			sent, finish, err := c.BeginClose(1000, "local", deadline)
			defer finish()
			if !sent || err != nil {
				t.Fatalf("sent=%v err=%v", sent, err)
			}
			h, err := readFrameHeader(raw)
			if err != nil || h.opcode != OpClose || h.masked != role.masks() {
				t.Fatalf("物理帧/方向错误: %+v %v", h, err)
			}
			payload := make([]byte, int(h.size))
			if err := readFramePayload(raw, h, payload); err != nil || string(payload) != "\x03\xe8local" {
				t.Fatalf("原始 close 字节改变：%q %v", payload, err)
			}
			if err := c.WriteMessage(OpText, []byte("late")); !errors.Is(err, ErrClosed) {
				t.Error("发送 close 后仍发送数据帧")
			}
			if err := c.WriteMessage(OpClose, EncodeClosePayload(1000, "second")); !errors.Is(err, ErrClosed) {
				t.Error("发送 close 后仍发送第二帧")
			}
			if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpClose, Payload: EncodeClosePayload(1001, "peer-original")}, role == RoleServer); err != nil {
				t.Fatal(err)
			}
			err = handshakeReadBy(t, c, done, deadline)
			var ce *CloseError
			if !errors.As(err, &ce) || ce.Code != 1001 || ce.Reason != "peer-original" {
				t.Fatalf("实际回应丢失: %v", err)
			}
			ce.Release()
			if sent, err := c.CloseWithResult(1000, "cleanup"); sent || err != nil {
				t.Fatalf("cleanup 补造发送: %v %v", sent, err)
			}
			finish()
			if f, err := ReadFrame(raw, 125); err == nil {
				t.Fatalf("物理重复帧：%v %q", f.Opcode, f.Payload)
			}
			if c.budget.Used() != 0 {
				t.Fatal("关闭后预算未归还")
			}
		})
	}
}

func TestCloseHandshakeNoReplyDeadline(t *testing.T) {
	a, raw := tcpPair(t)
	c := NewConn(a, RoleClient, 1024, 0)
	deadline := time.Now().Add(120 * time.Millisecond)
	done := handshakeRead(c)
	sent, finish, err := c.BeginClose(1000, "", deadline)
	defer finish()
	if !sent || err != nil {
		t.Fatal(sent, err)
	}
	if f, err := ReadFrame(raw, 125); err != nil || f.Opcode != OpClose {
		t.Fatal("未收到实际 close", err)
	}
	err = handshakeReadBy(t, c, done, deadline)
	var ce *CloseError
	if err == nil || errors.As(err, &ce) {
		t.Fatal("无回应被伪造为 peer close", err)
	}
	finish()
	if _, err := ReadFrame(raw, 125); !errors.Is(err, io.EOF) {
		t.Fatal("清理没有释放 socket 或重复发送", err)
	}
}

// 闩住真实 TCP 写入口，Close 立即唤醒；不使用随机调度或 sleep 决定竞争顺序。
type handshakeWriteGate struct {
	net.Conn
	entered, closed      chan struct{}
	enterOnce, closeOnce sync.Once
}

func (g *handshakeWriteGate) Write([]byte) (int, error) {
	g.enterOnce.Do(func() { close(g.entered) })
	<-g.closed
	return 0, net.ErrClosed
}
func (g *handshakeWriteGate) Close() error {
	g.closeOnce.Do(func() { close(g.closed) })
	return g.Conn.Close()
}

func TestCloseHandshakeSlowWriteAndLockDeadline(t *testing.T) {
	for _, locked := range []bool{false, true} {
		t.Run(fmt.Sprint("locked=", locked), func(t *testing.T) {
			a, raw := tcpPair(t)
			gate := &handshakeWriteGate{Conn: a, entered: make(chan struct{}), closed: make(chan struct{})}
			t.Cleanup(func() { _ = gate.Close() })
			c := NewConn(gate, RoleClient, 1024, 0)
			c.writeTimeout = 5 * time.Second
			var dataDone chan error
			if locked {
				dataDone = make(chan error, 1)
				go func() { dataDone <- c.WriteMessage(OpText, []byte("blocked")) }()
				awaitDialHandoff(t, gate.entered, "数据写未持锁")
			}
			deadline := time.Now().Add(120 * time.Millisecond)
			type result struct {
				sent   bool
				finish func()
				err    error
			}
			done := make(chan result, 1)
			go func() { sent, finish, err := c.BeginClose(1000, "", deadline); done <- result{sent, finish, err} }()
			awaitDialHandoff(t, gate.entered, "close 写未进入")
			timer := time.NewTimer(time.Until(deadline.Add(100 * time.Millisecond)))
			defer timer.Stop()
			var got result
			select {
			case got = <-done:
			case <-timer.C:
				t.Error("等锁/写入续期，未受共同 close deadline 中止")
				_ = gate.Close()
				got = <-done
			}
			got.finish()
			if got.sent || got.err == nil {
				t.Fatal("未物理写完仍声称成功", got.sent, got.err)
			}
			if locked {
				if err := awaitDialHandoff(t, dataDone, "数据 worker 未退出"); err == nil {
					t.Fatal("写未被中止")
				}
			}
			if f, err := ReadFrame(raw, 125); err == nil {
				t.Fatal("争锁失败仍发送了帧", f)
			}
		})
	}
}

func TestCloseHandshakePeerFirstAndRace(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprint("race=", race), func(t *testing.T) {
			a, raw := tcpPair(t)
			var gate *handshakeWriteGate
			var conn net.Conn = a
			if race {
				gate = &handshakeWriteGate{Conn: a, entered: make(chan struct{}), closed: make(chan struct{})}
				conn = gate
				t.Cleanup(func() { _ = gate.Close() })
			}
			c := NewConn(conn, RoleClient, 1024, 0)
			read := handshakeRead(c)
			deadline := time.Now().Add(300 * time.Millisecond)
			_ = raw.SetDeadline(deadline.Add(time.Second))
			type result struct {
				sent   bool
				finish func()
				err    error
			}
			done := make(chan result, 1)
			begin := func() { sent, finish, err := c.BeginClose(1000, "local", deadline); done <- result{sent, finish, err} }
			if race {
				go begin()
				awaitDialHandoff(t, gate.entered, "主动 close 未持锁")
			}
			if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpClose, Payload: EncodeClosePayload(1000, "peer")}, false); err != nil {
				t.Fatal(err)
			}
			if !race {
				if f, err := ReadFrame(raw, 125); err != nil || f.Opcode != OpClose {
					t.Fatal("自动回应未上网", err)
				}
			}
			err := handshakeReadBy(t, c, read, deadline)
			var ce *CloseError
			if !errors.As(err, &ce) || ce.Reason != "peer" {
				t.Fatalf("竞争丢失真实 close: %v", err)
			}
			ce.Release()
			if !race {
				go begin()
			}
			got := awaitDialHandoff(t, done, "主动 close 未退出")
			got.finish()
			if got.sent {
				t.Fatal("被抢先关闭仍声称实际发送")
			}
			if f, err := ReadFrame(raw, 125); err == nil {
				t.Fatal("竞争/清理补发 close", f)
			}
		})
	}
}

func TestCloseHandshakeTLSAbsoluteDeadline(t *testing.T) {
	client, server, gate := tlsClosePair(t)
	gate.appDelay = 150 * time.Millisecond
	c := NewConn(client, RoleClient, 1024, 0)
	c.budget = productionBudget(t, 128)
	deadline := time.Now().Add(300 * time.Millisecond)
	done := handshakeRead(c)
	sent, finish, err := c.BeginClose(1000, "tls", deadline)
	defer finish()
	if !sent || err != nil {
		t.Fatal(sent, err)
	}
	_ = server.SetDeadline(deadline.Add(time.Second))
	if f, err := ReadFrame(server, 125); err != nil || string(f.Payload) != "\x03\xe8tls" {
		t.Fatal("TLS 下未收到实际 WS close", err)
	}
	if err := WriteFrame(server, Frame{FIN: true, Opcode: OpClose, Payload: EncodeClosePayload(1000, "peer")}, false); err != nil {
		t.Fatal(err)
	}
	awaitDialHandoff(t, gate.alertEntered, "自动回应未进入 TLS cleanup 背压")
	err = handshakeReadBy(t, c, done, deadline)
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Reason != "peer" {
		t.Fatal("TLS 清理抹掉实际回应", err)
	}
	ce.Release()
	finish()
	awaitDialHandoff(t, gate.alertExited, "TLS worker 未 join")
	if gate.closedAt.After(deadline.Add(100 * time.Millisecond)) {
		t.Fatal("TLS cleanup 重新获取一秒")
	}
	if f, err := ReadFrame(server, 125); err == nil {
		t.Fatal("TLS 上物理重复 WS close", f)
	}
	if c.budget.Used() != 0 {
		t.Fatal("TLS 收尾预算泄漏")
	}
}

func TestCloseHandshakeCannotRenewOrCleanupResend(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		t.Run(fmt.Sprint("cleanup=", cleanup), func(t *testing.T) {
			a, raw := tcpPair(t)
			c := NewConn(a, RoleClient, 1024, 0)
			deadline := time.Now().Add(120 * time.Millisecond)
			done := handshakeRead(c)
			sent, finish, err := c.BeginClose(1000, "first", deadline)
			defer finish()
			if !sent || err != nil {
				t.Fatal(sent, err)
			}
			if f, err := ReadFrame(raw, 125); err != nil || string(f.Payload) != "\x03\xe8first" {
				t.Fatal("首帧未完整上网", err)
			}
			if cleanup {
				if sent, err := c.CloseWithResult(1001, "cleanup"); sent || err != nil {
					t.Fatal("cleanup 重新发送", sent, err)
				}
			} else {
				sent, finishAgain, err := c.BeginClose(1001, "again", deadline.Add(time.Second))
				defer finishAgain()
				if sent || !errors.Is(err, ErrClosed) {
					t.Fatal("重复 BeginClose 伪造发送", sent, err)
				}
			}
			if err := handshakeReadBy(t, c, done, deadline); err == nil {
				t.Fatal("缺少对端 close 却成功")
			}
			finish()
			if f, err := ReadFrame(raw, 125); err == nil {
				t.Fatal("清理/重复请求写出第二帧", f)
			}
		})
	}
}

func TestCloseHandshakeTLSAlreadyClosingUsesEarlierDeadline(t *testing.T) {
	client, server, gate := tlsClosePair(t)
	c := NewConn(client, RoleClient, 1024, 0)
	done := handshakeRead(c)
	if err := WriteFrame(server, Frame{FIN: true, Opcode: OpClose, Payload: EncodeClosePayload(1000, "peer")}, false); err != nil {
		t.Fatal(err)
	}
	if f, err := ReadFrame(server, 125); err != nil || f.Opcode != OpClose {
		t.Fatal("被动回应未完整上网", err)
	}
	awaitDialHandoff(t, gate.alertEntered, "被动回应未进入 TLS 清理")
	deadline := time.Now().Add(120 * time.Millisecond)
	sent, finish, err := c.BeginClose(1000, "local", deadline)
	defer finish()
	if sent || !errors.Is(err, ErrClosed) {
		t.Fatal("已由对端关闭仍冒充显式发送", sent, err)
	}
	err = handshakeReadBy(t, c, done, deadline)
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Reason != "peer" {
		t.Fatal("更早期限丢失接收证据", err)
	}
	ce.Release()
	finish()
	awaitDialHandoff(t, gate.alertExited, "旧 TLS cleanup worker 未退出")
	if gate.closedAt.After(deadline.Add(100 * time.Millisecond)) {
		t.Fatal("已开始的 TLS 清理不受更早期限约束")
	}
	if f, err := ReadFrame(server, 125); err == nil {
		t.Fatal("追加 API 发送了第二帧", f)
	}
}

func TestCloseHandshakeValidationBudgetAndPing(t *testing.T) {
	for _, tt := range []struct {
		name   string
		code   uint16
		reason string
		budget int64
		want   error
	}{
		{"bad_code", 1006, "", 128, ErrProtocol},
		{"bad_utf8", 1000, string([]byte{255}), 128, ErrInvalidUTF8},
		{"no_status_reason", 1005, "bad", 128, ErrProtocol},
		{"budget", 1000, "", 1, ErrBufferLimit},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, raw := tcpPair(t)
			c := NewConn(a, RoleClient, 1024, 0)
			c.budget = productionBudget(t, tt.budget)
			sent, finish, err := c.BeginClose(tt.code, tt.reason, time.Now().Add(time.Second))
			finish()
			if sent || !errors.Is(err, tt.want) {
				t.Fatalf("sent=%v err=%v", sent, err)
			}
			_ = a.Close()
			if f, err := ReadFrame(raw, 125); err == nil {
				t.Fatal("非法/超额 close 写上网", f)
			}
			if c.budget.Used() != 0 {
				t.Fatal("失败泄漏预算")
			}
		})
	}
	a, raw := tcpPair(t)
	c := NewConn(a, RoleClient, 1024, 0)
	deadline := time.Now().Add(300 * time.Millisecond)
	done := handshakeRead(c)
	sent, finish, err := c.BeginClose(1005, "", deadline)
	defer finish()
	if !sent || err != nil {
		t.Fatal(sent, err)
	}
	if f, err := ReadFrame(raw, 125); err != nil || f.Opcode != OpClose || len(f.Payload) != 0 {
		t.Fatal("无状态 close 编码失真", err)
	}
	if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpPing, Payload: []byte("late-heartbeat")}, false); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpClose}, false); err != nil {
		t.Fatal(err)
	}
	err = handshakeReadBy(t, c, done, deadline)
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Code != 1005 {
		t.Fatal("收尾心跳阻断了实际 close", err)
	}
	ce.Release()
	finish()
	if f, err := ReadFrame(raw, 125); err == nil {
		t.Fatal("已关闭写侧仍发送 pong/close", f)
	}
}

func TestCloseHandshakeShortWriteCannotRestartFrame(t *testing.T) {
	for _, at := range []int{1, 2} {
		t.Run(fmt.Sprint(at), func(t *testing.T) {
			a, raw := tcpPair(t)
			short := &productionShortConn{Conn: a, shortAt: at}
			c := NewConn(short, RoleClient, 1024, 0)
			c.budget = productionBudget(t, 64)
			sent, finish, err := c.BeginClose(1000, "bye", time.Now().Add(time.Second))
			defer finish()
			if sent || !errors.Is(err, io.ErrShortWrite) {
				t.Fatal("半帧冒充 sent", sent, err)
			}
			calls := short.calls
			if sent, _ := c.CloseWithResult(1000, "retry"); sent {
				t.Fatal("半帧之后重新开始了一帧")
			}
			finish()
			if short.calls != calls {
				t.Fatal("cleanup 试图修补物理半帧")
			}
			if _, err := ReadFrame(raw, 125); err == nil {
				t.Fatal("测试未产生真实半帧")
			}
			if c.budget.Used() != 0 {
				t.Fatal("失败写未归还预算")
			}
		})
	}
}

func TestCloseHandshakeRejectsUnboundedOrExpiredDeadline(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint("expired=", expired), func(t *testing.T) {
			a, raw := tcpPair(t)
			c := NewConn(a, RoleClient, 1024, 0)
			var deadline time.Time
			want := ErrProtocol
			if expired {
				deadline = time.Now().Add(-time.Second)
				want = context.DeadlineExceeded
			}
			sent, finish, err := c.BeginClose(1000, "", deadline)
			finish()
			if sent || !errors.Is(err, want) {
				t.Fatal("无期限或过期仍发起关闭帧", sent, err)
			}
			_ = a.Close()
			if f, err := ReadFrame(raw, 125); err == nil {
				t.Fatal("无有效预算却发送了物理帧", f)
			}
		})
	}
}
