package ws

import (
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
