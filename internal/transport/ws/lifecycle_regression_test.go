package ws

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type writeSignalConn struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *writeSignalConn) Write(p []byte) (int, error) {
	if len(p) > 125 {
		c.once.Do(func() { close(c.started) })
	}
	return c.Conn.Write(p)
}

// TestCloseReleasesBlockedWriter 防的是业务帧写满 socket 后持锁，令清理路径也无法关 fd。
func TestCloseReleasesBlockedWriter(t *testing.T) {
	client, peer := tcpPair(t)
	if err := client.(*net.TCPConn).SetWriteBuffer(1024); err != nil {
		t.Fatal(err)
	}
	if err := peer.(*net.TCPConn).SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	observed := &writeSignalConn{Conn: client, started: make(chan struct{})}
	conn := NewConn(observed, RoleServer, 16<<20, 50*time.Millisecond)
	writeDone := make(chan error, 1)
	go func() { writeDone <- conn.WriteMessage(OpBinary, []byte(strings.Repeat("x", 16<<20))) }()
	<-observed.started

	closeDone := make(chan error, 1)
	go func() { closeDone <- conn.Close(CloseNormal, "") }()
	select {
	case <-closeDone:
	case <-time.After(300 * time.Millisecond):
		_ = client.Close()
		<-writeDone
		t.Fatal("Close 无法释放被阻塞写占用的连接")
	}
	select {
	case <-writeDone:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Close 返回后，业务写仍未退出")
	}
}

// CloseWithResult 的本次发送结果不能把已关闭、被动回应或写锁占用的 nil 当作证据。
func TestConnCloseWithResult(t *testing.T) {
	for _, role := range []Role{RoleClient, RoleServer} {
		t.Run(map[Role]string{RoleClient: "client", RoleServer: "server"}[role], func(t *testing.T) {
			a, raw := tcpPair(t)
			conn := NewConn(a, role, maxTestPayload, 0)
			sent, err := conn.CloseWithResult(1011, "fixture-close")
			if err != nil || !sent {
				t.Fatal("本次成功写完 close 没有发送证据")
			}
			_ = raw.SetReadDeadline(time.Now().Add(time.Second))
			var header [2]byte
			if _, err := io.ReadFull(raw, header[:]); err != nil || (header[1]&0x80 != 0) != (role == RoleClient) {
				t.Fatal("实际关闭帧角色掩码不符")
			}
			frame, err := ReadFrame(io.MultiReader(bytes.NewReader(header[:]), raw), maxTestPayload)
			if err != nil || frame.Opcode != OpClose {
				t.Fatal("实际关闭帧不符")
			}
			code, reason, err := DecodeClosePayload(frame.Payload)
			if err != nil || code != 1011 || reason != "fixture-close" {
				t.Fatal("实际关闭负载不符")
			}
			if sent, err := conn.CloseWithResult(1000, ""); sent || err != nil {
				t.Fatal("已关闭连接的幂等调用补造了第二个发送")
			}
			if _, err := ReadFrame(raw, maxTestPayload); !errors.Is(err, io.EOF) {
				t.Fatal("实际连接没有在唯一关闭帧后释放")
			}
		})
	}
	t.Run("peer_auto_closed", func(t *testing.T) {
		conn, raw := pipeRaw(t, 0)
		if WriteFrame(raw, Frame{FIN: true, Opcode: OpClose, Payload: EncodeClosePayload(1001, "")}, false) != nil {
			t.Fatal("被动关闭注入失败")
		}
		_, _, err := conn.ReadMessage()
		var closed *CloseError
		if !errors.As(err, &closed) || closed.Code != 1001 {
			t.Fatal("没有实际观测到被动关闭")
		}
		if sent, err := conn.CloseWithResult(1011, ""); sent || err != nil {
			t.Fatal("自动回应不能证明本次指定关闭已发送")
		}
		_ = raw.SetReadDeadline(time.Now().Add(time.Second))
		frame, err := ReadFrame(raw, maxTestPayload)
		if err != nil || frame.Opcode != OpClose {
			t.Fatal("原有自动回应行为丢失")
		}
		code, _, err := DecodeClosePayload(frame.Payload)
		if err != nil || code != 1000 {
			t.Fatal("自动回应不再是既有的 1000")
		}
		if _, err := ReadFrame(raw, maxTestPayload); !errors.Is(err, io.EOF) {
			t.Fatal("自动回应后出现不存在的第二个关闭帧")
		}
	})
	t.Run("write_failed", func(t *testing.T) {
		a, raw := tcpPair(t)
		conn := NewConn(a, RoleClient, maxTestPayload, 0)
		_ = a.Close()
		if sent, err := conn.CloseWithResult(1000, ""); sent || err == nil {
			t.Fatal("失败的关闭帧写入仍给出发送证据或吞掉错误")
		}
		_ = raw.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := ReadFrame(raw, maxTestPayload); !errors.Is(err, io.EOF) {
			t.Fatal("失败帧写入后仍存在实际帧")
		}
	})
	t.Run("release_error_after_frame", func(t *testing.T) {
		a, raw := tcpPair(t)
		conn := NewConn(&closeResultReleaseError{Conn: a}, RoleClient, maxTestPayload, 0)
		sent, err := conn.CloseWithResult(1000, "")
		if !sent || !errors.Is(err, errCloseResultRelease) {
			t.Fatal("实际发帧结果与底层释放错误没有独立保留")
		}
		_ = raw.SetReadDeadline(time.Now().Add(time.Second))
		frame, err := ReadFrame(raw, maxTestPayload)
		if err != nil || frame.Opcode != OpClose {
			t.Fatal("释放失败前没有实际完整帧")
		}
	})
	t.Run("concurrent_claim", func(t *testing.T) {
		conn, raw := pipeRaw(t, 0)
		start := make(chan struct{})
		results := make(chan bool, 8)
		for i := 0; i < cap(results); i++ {
			go func() {
				<-start
				sent, err := conn.CloseWithResult(1000, "")
				results <- sent && err == nil
			}()
		}
		close(start)
		count := 0
		for i := 0; i < cap(results); i++ {
			if <-results {
				count++
			}
		}
		if count != 1 {
			t.Fatal("并发关闭没有唯一实际发送者")
		}
		_ = raw.SetReadDeadline(time.Now().Add(time.Second))
		frame, err := ReadFrame(raw, maxTestPayload)
		if err != nil || frame.Opcode != OpClose {
			t.Fatal("唯一发送者没有实际帧")
		}
		if _, err := ReadFrame(raw, maxTestPayload); !errors.Is(err, io.EOF) {
			t.Fatal("并发关闭写出了额外帧")
		}
	})
}

// 实际业务写占锁时，关闭必须放弃 close 帧并唤醒 writer，不能用 nil 冒充发帧。
func TestConnCloseWithResultBlockedWriter(t *testing.T) {
	a, raw := tcpPair(t)
	gate := &closeResultWriteGate{Conn: a, entered: make(chan struct{}), closed: make(chan struct{})}
	t.Cleanup(func() { _ = gate.Close() })
	conn := NewConn(gate, RoleServer, maxTestPayload, 0)
	done := make(chan error, 1)
	go func() { done <- conn.WriteMessage(OpText, []byte("blocked")) }()
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("实际 socket 写没有进入屏障")
	}
	sent, err := conn.CloseWithResult(1000, "")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("关闭未唤醒并归还 writer")
	}
	if sent || err != nil {
		t.Fatal("未取得写锁也未写关闭帧却报告已发送")
	}
	_ = raw.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := ReadFrame(raw, maxTestPayload); !errors.Is(err, io.EOF) {
		t.Fatal("放弃关闭帧后仍伪造实际字节")
	}
}

var errCloseResultRelease = errors.New("合成释放错误")

type closeResultReleaseError struct{ net.Conn }

func (c *closeResultReleaseError) Close() error {
	_ = c.Conn.Close()
	return errCloseResultRelease
}

type closeResultWriteGate struct {
	net.Conn
	entered, closed      chan struct{}
	enterOnce, closeOnce sync.Once
}

func (g *closeResultWriteGate) Write(p []byte) (int, error) {
	g.enterOnce.Do(func() { close(g.entered) })
	<-g.closed
	return 0, net.ErrClosed
}

func (g *closeResultWriteGate) Close() error {
	g.closeOnce.Do(func() { close(g.closed) })
	return g.Conn.Close()
}

// TestFragmentsRefreshIdleDeadline 防的是活跃分片消息被一次性的消息级 deadline 掐断。
func TestFragmentsRefreshIdleDeadline(t *testing.T) {
	client, peer := tcpPair(t)
	conn := NewConn(client, RoleClient, 1024, 200*time.Millisecond)
	stop := make(chan struct{})
	sent := make(chan int, 1)
	go func() {
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		count := 0
		defer func() { sent <- count }()
		for i := 0; i < 10; i++ {
			if i > 0 {
				select {
				case <-ticker.C:
				case <-stop:
					return
				}
			}
			op := OpContinuation
			if i == 0 {
				op = OpText
			}
			if err := WriteFrame(peer, Frame{FIN: i == 9, Opcode: op, Payload: []byte("x")}, false); err != nil {
				return
			}
			count++
		}
	}()

	_, payload, err := conn.ReadMessage()
	close(stop)
	frames := <-sent
	if err != nil {
		t.Fatalf("活跃分片流在 %d 帧后被中止: %v", frames, err)
	}
	if len(payload) != 10 {
		t.Fatalf("消息长度 = %d, 期望 10", len(payload))
	}
}
