package gateway

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 真实本地 TLS 1.2；仅给 close_notify 注入遵守写期限的背压，业务帧照常上网。
type wsTLSAlertGate struct {
	net.Conn
	deadline atomic.Int64
	closed   chan struct{}
	once     sync.Once
}

func (c *wsTLSAlertGate) SetWriteDeadline(d time.Time) error {
	c.deadline.Store(d.UnixNano())
	return c.Conn.SetWriteDeadline(d)
}
func (c *wsTLSAlertGate) Write(p []byte) (int, error) {
	if len(p) == 0 || p[0] != 21 {
		return c.Conn.Write(p)
	}
	timer := time.NewTimer(time.Until(time.Unix(0, c.deadline.Load())))
	defer timer.Stop()
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	case <-timer.C:
		return 0, os.ErrDeadlineExceeded
	}
}
func (c *wsTLSAlertGate) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

type wsTLSGateListener struct {
	net.Listener
	accepted chan *wsTLSAlertGate
}

func (l wsTLSGateListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	g := &wsTLSAlertGate{Conn: c, closed: make(chan struct{})}
	l.accepted <- g
	return g, nil
}

func TestWSHandlerTLSCloseRemainsBounded(t *testing.T) {
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20})
		if err != nil {
			return
		}
		defer c.Close(1000, "")
		_ = c.WriteMessage(ws.OpText, []byte(wsTestInitial))
		_ = c.Close(1000, "")
	}))
	defer u.Close()
	d := wsHandlerDeps(t, u.URL)
	h := NewDashScopeRealtimeHandler(d)
	done := make(chan struct{}, 1)
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer func() { done <- struct{}{} }(); h.ServeHTTP(w, r) }))
	accepted := make(chan *wsTLSAlertGate, 1)
	s.Listener = wsTLSGateListener{Listener: s.Listener, accepted: accepted}
	s.TLS = &tls.Config{MaxVersion: tls.VersionTLS12}
	s.StartTLS()
	defer s.Close()
	c, err := tls.Dial("tcp", strings.TrimPrefix(s.URL, "https://"), &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal("本地 TLS 握手失败")
	}
	defer c.NetConn().Close()
	gate := receiveWSTest(t, accepted)
	defer gate.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = fmt.Fprintf(c, "GET /api-ws/v1/realtime?model=real-model HTTP/1.1\r\nHost: local\r\nAuthorization: Bearer synthetic-key\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(c)
	resp, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil || resp.StatusCode != 101 {
		t.Fatal("TLS 下游未升级")
	}
	initial, err := ws.ReadFrame(reader, 1<<20)
	if err != nil || string(initial.Payload) != wsTestInitial {
		t.Fatal("TLS 初始帧不符")
	}
	closed, err := ws.ReadFrame(reader, 125)
	if err != nil || closed.Opcode != ws.OpClose || string(closed.Payload) != "\x03\xe8" {
		t.Fatal("TLS 关闭预算前未收到真实 WS close")
	}
	late := false
	select {
	case <-done:
	case <-time.After(1300 * time.Millisecond):
		late = true
		_ = gate.Close()
		awaitWSTest(t, done)
	}
	if late {
		t.Fatal("握手连接包装遮蔽 TLS 强制关闭，close_notify 另起五秒期限")
	}
	if d.Budget.Used() != 0 {
		t.Fatal("TLS 会话收尾预算未归零")
	}
}
