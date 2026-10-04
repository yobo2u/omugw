package ws

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

// 真实 TCP/TLS 握手和 WS 帧均原样传输；仅 TLS 1.2 alert 注入遵守期限的背压。
// 应用写延迟只用于消耗同一关闭预算，测试的先后关系由帧接收和门闩确定。
type tlsCloseBackpressure struct {
	net.Conn
	mu                                sync.Mutex
	deadline, firstDeadline, closedAt time.Time
	changed, closed                   chan struct{}
	alertEntered, alertExited         chan struct{}
	closeOnce, alertOnce, appOnce     sync.Once
	closeErr                          error
	armed, rejectDeadline             bool
	appDelay                          time.Duration
}

func (c *tlsCloseBackpressure) SetWriteDeadline(d time.Time) error {
	c.mu.Lock()
	if c.rejectDeadline {
		c.rejectDeadline = false
		c.mu.Unlock()
		return errors.New("合成 101 期限设置失败")
	}
	c.deadline = d
	if c.armed && c.firstDeadline.IsZero() && !d.IsZero() {
		c.firstDeadline = d
	}
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
	return c.Conn.SetWriteDeadline(d)
}

func (c *tlsCloseBackpressure) Write(p []byte) (int, error) {
	if !c.armed || len(p) == 0 {
		return c.Conn.Write(p)
	}
	if p[0] == 23 && c.appDelay > 0 {
		c.appOnce.Do(func() {
			timer := time.NewTimer(c.appDelay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-c.closed:
			}
		})
	}
	if p[0] != 21 {
		return c.Conn.Write(p)
	}
	c.alertOnce.Do(func() { close(c.alertEntered) })
	defer close(c.alertExited)
	for {
		c.mu.Lock()
		d := c.deadline
		c.mu.Unlock()
		var expired <-chan time.Time
		var timer *time.Timer
		if !d.IsZero() {
			timer = time.NewTimer(time.Until(d))
			expired = timer.C
		}
		select {
		case <-c.closed:
			if timer != nil {
				timer.Stop()
			}
			return 0, net.ErrClosed
		case <-expired:
			return 0, os.ErrDeadlineExceeded
		case <-c.changed:
			if timer != nil {
				timer.Stop()
			}
		}
	}
}

func (c *tlsCloseBackpressure) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.Conn.Close()
		c.closedAt = time.Now()
		close(c.closed)
	})
	return c.closeErr
}

func tlsClosePair(t *testing.T) (*tls.Conn, *tls.Conn, *tlsCloseBackpressure) {
	t.Helper()
	certSource := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cfg := certSource.TLS.Clone()
	certSource.Close()
	cfg.MaxVersion = tls.VersionTLS12
	a, b := tcpPair(t)
	gate := &tlsCloseBackpressure{Conn: a, changed: make(chan struct{}, 1), closed: make(chan struct{}),
		alertEntered: make(chan struct{}), alertExited: make(chan struct{})}
	client := tls.Client(gate, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12})
	server := tls.Server(b, cfg)
	_ = a.SetDeadline(time.Now().Add(3 * time.Second))
	_ = b.SetDeadline(time.Now().Add(3 * time.Second))
	ready := make(chan error, 1)
	exited := make(chan struct{})
	t.Cleanup(func() {
		_ = gate.Close()
		_ = b.Close()
		awaitDialHandoff(t, exited, "TLS 握手工作者未退出")
	})
	go func() { defer close(exited); ready <- server.Handshake() }()
	if err := client.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := awaitDialHandoff(t, ready, "TLS 握手未完成"); err != nil {
		t.Fatal(err)
	}
	_ = a.SetDeadline(time.Time{})
	_ = b.SetDeadline(time.Time{})
	gate.armed = true
	return client, server, gate
}

func TestProductionTLSGracefulCloseBounded(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		write, idle, delay, allowed time.Duration
	}{
		{"default-one-second", 0, 0, 0, time.Second},
		{"shared-with-frame-write", 400 * time.Millisecond, 0, 250 * time.Millisecond, 400 * time.Millisecond},
		{"shorter-idle", time.Second, 150 * time.Millisecond, 0, 150 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server, gate := tlsClosePair(t)
			gate.appDelay = tc.delay
			c := NewConn(client, RoleClient, 1024, tc.idle)
			c.writeTimeout = tc.write
			c.budget = productionBudget(t, 64)
			type result struct {
				sent bool
				err  error
			}
			done := make(chan result, 1)
			exited := make(chan struct{})
			t.Cleanup(func() {
				_ = gate.Close()
				_ = server.NetConn().Close()
				awaitDialHandoff(t, exited, "关闭工作者未回收")
			})
			start := time.Now()
			go func() { defer close(exited); sent, err := c.CloseWithResult(1000, "bye"); done <- result{sent, err} }()
			_ = server.SetReadDeadline(start.Add(3 * time.Second))
			f, err := ReadFrame(server, 125)
			if err != nil || f.Opcode != OpClose || string(f.Payload) != "\x03\xe8bye" {
				t.Fatalf("TLS 背压前未实际收到完整 WS close: %v", err)
			}
			awaitDialHandoff(t, gate.alertEntered, "WS close 之后未进入 TLS alert 背压")
			gate.mu.Lock()
			first, tlsDeadline := gate.firstDeadline, gate.deadline
			gate.mu.Unlock()
			t.Logf("WS close 已收到；TLS 自设期限剩余 %v，完整关闭预算 %v", time.Until(tlsDeadline), tc.allowed)
			bound := time.NewTimer(time.Until(start.Add(tc.allowed + 100*time.Millisecond)))
			defer bound.Stop()
			var got result
			select {
			case got = <-done:
			case <-bound.C:
				t.Fatal("WS close 已发送，但 TLS 收尾超过同一关闭预算")
			}
			if !got.sent || got.err == nil {
				t.Fatalf("TLS 释放失败抹掉了帧发送证据或错误: sent=%t err=%v", got.sent, got.err)
			}
			awaitDialHandoff(t, gate.closed, "底层 TCP 未关闭")
			awaitDialHandoff(t, gate.alertExited, "TLS alert 写未退出")
			if gate.closedAt.After(first.Add(100 * time.Millisecond)) {
				t.Error("TLS 收尾另起了关闭预算")
			}
			if c.budget.Used() != 0 {
				t.Error("关闭后遗留 payload 容量")
			}
			if sent, err := c.CloseWithResult(1000, ""); sent || err != nil {
				t.Error("幂等关闭伪造 sent")
			}
		})
	}
}

func TestProductionTLSOwnedReadCancellation(t *testing.T) {
	for _, passive := range []bool{false, true} {
		name := "blocked-read"
		if passive {
			name = "passive-close-alert-in-flight"
		}
		t.Run(name, func(t *testing.T) {
			client, server, gate := tlsClosePair(t)
			probe := &productionReadSignal{Reader: client, entered: make(chan struct{}), payload: make(chan struct{})}
			c := newConnBuffered(client, probe, RoleClient, 1024, 0)
			c.budget = productionBudget(t, 64)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			exited := make(chan struct{})
			t.Cleanup(func() {
				cancel()
				_ = gate.Close()
				_ = server.NetConn().Close()
				awaitDialHandoff(t, exited, "取消读工作者未回收")
			})
			go func() {
				defer close(exited)
				m, err := c.ReadOwnedMessage(ctx)
				m.Release()
				releaseCloseError(err)
				done <- err
			}()
			awaitDialHandoff(t, probe.entered, "TLS 读未开始")
			if passive {
				if err := WriteFrame(server, Frame{FIN: true, Opcode: OpClose, Payload: []byte{3, 232, 'x'}}, false); err != nil {
					t.Fatal(err)
				}
				_ = server.SetReadDeadline(time.Now().Add(3 * time.Second))
				if f, err := ReadFrame(server, 125); err != nil || f.Opcode != OpClose {
					t.Fatalf("未实际收到被动 WS 回应: %v", err)
				}
				awaitDialHandoff(t, gate.alertEntered, "被动回应未进入 TLS 收尾")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("取消未取得唯一关闭权: %v", err)
				}
			case <-time.After(250 * time.Millisecond):
				t.Fatal("取消没有立即中止 TCP 并 join；被 TLS close_notify 拖住")
			}
			awaitDialHandoff(t, gate.closed, "取消返回时底层关闭未完成")
			if passive {
				awaitDialHandoff(t, gate.alertExited, "取消后 TLS 收尾未回收")
			} else {
				select {
				case <-gate.alertEntered:
					t.Error("取消错误地开始了 TLS 礼貌收尾")
				default:
				}
			}
			if c.budget.Used() != 0 {
				t.Error("取消丢弃 CloseError 时泄漏原因容量")
			}
			if sent, _ := c.CloseWithResult(1000, ""); sent {
				t.Error("取消后补造了关闭发送证据")
			}
		})
	}
}

type tlsFailureHijacker struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (w *tlsFailureHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func TestProductionTLSHandshakeFailureAbort(t *testing.T) {
	for _, accept := range []bool{false, true} {
		name := "error-response-body"
		if accept {
			name = "accept-101-deadline-failure"
		}
		t.Run(name, func(t *testing.T) {
			client, server, gate := tlsClosePair(t)
			var resp *http.Response
			if !accept {
				if _, err := io.WriteString(server, "HTTP/1.1 401 Unauthorized\r\nContent-Length: 2\r\n\r\nno"); err != nil {
					t.Fatal(err)
				}
				var err error
				resp, err = http.ReadResponse(bufio.NewReader(client), nil)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				gate.rejectDeadline = true
			}
			done := make(chan error, 1)
			exited := make(chan struct{})
			t.Cleanup(func() {
				_ = gate.Close()
				_ = server.NetConn().Close()
				awaitDialHandoff(t, exited, "握手失败释放工作者未回收")
			})
			go func() {
				defer close(exited)
				if accept {
					_, err := Accept(&tlsFailureHijacker{httptest.NewRecorder(), client}, productionUpgradeRequest(), AcceptOptions{})
					done <- err
				} else {
					done <- retainHandshakeErrorBody(context.Background(), client, resp, 64)
				}
			}()
			select {
			case err := <-done:
				if !errors.Is(err, ErrHandshake) {
					t.Fatalf("失败分类丢失: %v", err)
				}
			case <-time.After(250 * time.Millisecond):
				t.Fatal("握手失败释放错误地等待 TLS 礼貌收尾")
			}
			awaitDialHandoff(t, gate.closed, "握手失败未实际释放 TCP")
			select {
			case <-gate.alertEntered:
				t.Error("握手失败发送了 TLS close_notify")
			default:
			}
			if resp != nil {
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || string(body) != "no" {
					t.Error("中止 socket 丢失已保留的失败 body")
				}
			}
		})
	}
}
