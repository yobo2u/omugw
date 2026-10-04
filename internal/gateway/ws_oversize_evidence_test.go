package gateway

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

func assertWSOversizeClose(t *testing.T, peer *wsTestPeer) {
	t.Helper()
	if err := peer.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		f, err := ws.ReadFrame(peer.reader, 125)
		if err != nil {
			t.Fatalf("预期实际 close 1009: %v", err)
		}
		// 只在拒绝后的取证阶段被动越过心跳：自动 pong 可能在已关闭的写侧
		// 报错，遮住后面的真实 close；不续读期限，也不把 EOF/写错算成功。
		if f.Opcode == ws.OpPing || f.Opcode == ws.OpPong {
			continue
		}
		if f.Opcode != ws.OpClose || len(f.Payload) < 2 || binary.BigEndian.Uint16(f.Payload) != 1009 || string(f.Payload[2:]) != "message too large" {
			t.Fatalf("预期 close 1009 / message too large，实际 opcode=%v payload=%q", f.Opcode, f.Payload)
		}
		return
	}
}

// 超限取证需要独立的原始收发流，避免失败发送或自动 pong 抢走关闭帧证据。
func openAITestRawPeer(t *testing.T, url string) *wsTestPeer {
	t.Helper()
	raw, err := net.DialTimeout("tcp", strings.TrimPrefix(url, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if err := raw.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprint(raw, "GET /v1/realtime?model=real-model HTTP/1.1\r\nHost: local\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nAuthorization: Bearer sk-test-1234567890\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(raw)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatal("原始客户端升级失败")
	}
	return &wsTestPeer{Conn: raw, reader: reader, masked: true}
}

type wsOversizeClosedConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *wsOversizeClosedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}

// 超限头先到、关闭完成后才发负载，固定 CI 中合法的早拒绝窗口，不靠调度碰运气。
type wsOversizeSender struct {
	*net.TCPConn
	closed <-chan struct{}
}

func (c wsOversizeSender) Write(p []byte) (int, error) {
	n, err := c.TCPConn.Write(p)
	if err == nil && len(p) == 14 && p[0] == 0x82 && p[1] == 0xff {
		select {
		case <-c.closed:
			// 固定失败写侧，排除不同内核暂收剩余负载的差异；读侧仍保留
			// 网关真实发出的 ping/close，不能注入一个合成关闭错误顶账。
			err = c.CloseWrite()
		case <-time.After(3 * time.Second):
			return n, errors.New("超限头未触发关闭")
		}
	}
	return n, err
}

func TestWSOversizeCloseEvidenceWithFailedSender(t *testing.T) {
	closed := make(chan struct{})
	x := startWSTestRelay(t, 4<<20, 1<<20, 0, func(c net.Conn) net.Conn {
		return &wsOversizeClosedConn{Conn: c, closed: closed}
	})
	if err := x.down.Ping(nil); err != nil {
		t.Fatal(err)
	}
	if err := x.client.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	err := ws.WriteFrame(wsOversizeSender{x.client.Conn.(*net.TCPConn), closed}, ws.Frame{
		FIN: true, Opcode: ws.OpBinary, Payload: bytes.Repeat([]byte{0xa5}, (1<<20)+1),
	}, true)
	if err == nil {
		t.Fatal("关闭屏障后整条超限帧意外写完，未覆盖发送失败窗口")
	}
	t.Logf("屏障后的负载写失败: %v", err)
	// 必须独立读到网关的准确关闭帧；发送错误与自动 pong 错误均不构成关闭证据。
	assertWSOversizeClose(t, x.client)
	if err := x.joined(t); !errors.Is(err, ws.ErrMessageTooLarge) {
		t.Fatalf("超限关闭终态=%v", err)
	}
}
