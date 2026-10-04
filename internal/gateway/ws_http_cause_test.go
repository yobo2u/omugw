package gateway

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 真实 HTTP/Hijack 下，无 close 的 EOF/idle 必须向仍存活上游发送 1011；
// 同链路的显式取消与 Built shutdown 保持 1001，不能统一改码或屏蔽整个请求 context。
func TestOpenAIRealtimeHTTPReadFailureCause(t *testing.T) {
	for _, mode := range []string{"eof", "idle", "external-cancel", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			type closeResult struct {
				code   uint16
				reason string
				err    error
			}
			upDone := make(chan closeResult, 1)
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1024, Idle: 3 * time.Second, WriteTimeout: time.Second})
				if err != nil {
					upDone <- closeResult{err: err}
					return
				}
				defer c.Close(1000, "")
				if err := c.WriteMessage(ws.OpText, []byte(openAIReady)); err != nil {
					upDone <- closeResult{err: err}
					return
				}
				_, _, err = c.ReadMessage()
				var closed *ws.CloseError
				if !errors.As(err, &closed) {
					upDone <- closeResult{err: err}
					return
				}
				upDone <- closeResult{code: closed.Code, reason: closed.Reason}
				closed.Release()
			}))
			t.Cleanup(u.Close)
			cfg := openAITestConfig(u.URL)
			cfg.Timeouts.Idle = 2 * time.Second
			b, _ := openAITestBuild(t, cfg, false)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			gate := &wsHTTPErrorGate{release: make(chan struct{})}
			defer gate.open()
			done := make(chan struct{}, 1)
			s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { done <- struct{}{} }()
				b.Mux.ServeHTTP(&wsHTTPCauseWriter{w, gate, mode == "idle"}, r)
			}))
			s.Config.BaseContext = func(net.Listener) context.Context { return parent }
			s.Start()
			t.Cleanup(func() {
				gate.open()
				ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
				defer stop()
				_ = b.ShutdownWebSockets(ctx)
				s.Close()
			})
			raw, err := net.DialTimeout("tcp", strings.TrimPrefix(s.URL, "http://"), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
			_, err = io.WriteString(raw, "GET /v1/realtime?model=real-model HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer sk-test-1234567890\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
			if err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(raw)
			resp, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
			if err != nil || resp.StatusCode != 101 {
				t.Fatalf("HTTP 握手失败: %v", err)
			}
			frame, err := ws.ReadFrame(reader, 1024)
			if err != nil || string(frame.Payload) != openAIReady {
				t.Fatalf("未收到原首事件: %v", err)
			}
			var stopped chan error
			switch mode {
			case "eof":
				if err := raw.(*net.TCPConn).CloseWrite(); err != nil {
					t.Fatal(err)
				}
			case "external-cancel":
				cancel()
			case "shutdown":
				stopped = make(chan error, 1)
				go func() {
					ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
					defer stop()
					stopped <- b.ShutdownWebSockets(ctx)
				}()
			}
			got := receiveWSTest(t, upDone)
			gate.open()
			awaitWSTest(t, done)
			if stopped != nil {
				if err := receiveWSTest(t, stopped); err != nil {
					t.Fatal(err)
				}
			}
			wantCode, wantReason := uint16(1011), "downstream connection failed"
			if mode == "external-cancel" || mode == "shutdown" {
				wantCode, wantReason = 1001, ""
			}
			if got.err != nil || got.code != wantCode || got.reason != wantReason {
				t.Errorf("%s 被误分类: code=%d reason=%q err=%v，期望 %d %q", mode, got.code, got.reason, got.err, wantCode, wantReason)
			}
			if b.wsBudget.Used() != 0 {
				t.Fatal("原因仲裁完成后预算未归零")
			}
		})
	}
}

// 在原 HTTP reader 已返回错误之后暂停 relay 的错误交付，稳定复现请求取消先胜出。
// 修复后不再从这个空缓存 reader 补读；真实 TCP 上的 EOF/timeout 仍必须正常分类。
type wsHTTPErrorGate struct {
	r       io.Reader
	release chan struct{}
	once    sync.Once
}

func (g *wsHTTPErrorGate) Read(p []byte) (int, error) {
	n, err := g.r.Read(p)
	if err != nil {
		<-g.release
	}
	return n, err
}

func (g *wsHTTPErrorGate) open() { g.once.Do(func() { close(g.release) }) }

type wsHTTPCauseWriter struct {
	http.ResponseWriter
	gate *wsHTTPErrorGate
	idle bool
}

func (w *wsHTTPCauseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return c, brw, err
	}
	if brw.Reader.Buffered() != 0 {
		_ = c.Close()
		return nil, nil, errors.New("原因测试不应预先发送客户端业务字节")
	}
	w.gate.r = brw.Reader
	brw.Reader = bufio.NewReader(w.gate)
	if w.idle {
		c = wsHTTPCauseIdleConn{c}
	}
	return c, brw, nil
}

type wsHTTPCauseIdleConn struct{ net.Conn }

func (c wsHTTPCauseIdleConn) SetReadDeadline(d time.Time) error {
	// 用真实 TCP 读期限触发 idle，但先于周期心跳，隔离待验证的原因竞争与写锁竞争。
	if !d.IsZero() {
		d = time.Now().Add(80 * time.Millisecond)
	}
	return c.Conn.SetReadDeadline(d)
}
