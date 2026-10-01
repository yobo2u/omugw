package ws

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const dialHandoffWait = 3 * time.Second

type dialHandoffResult struct {
	conn *Conn
	resp *http.Response
	err  error
}

// 成功后的握手 context 不再拥有 socket；首条预读消息、双向业务帧与实际关闭都须保全。
func TestDialHandoffSurvivesContextCancellation(t *testing.T) {
	peer := newDialHandoffPeer(t)
	peer.upgrade()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := startDialHandoff(t, ctx, peer.url, cancel, peer.fallback)
	got := awaitDialHandoff(t, result, "握手未归还 Dial 工作者")
	cancel()
	conn, resp, err := got.conn, got.resp, got.err
	if err != nil || conn == nil {
		t.Fatalf("握手失败: %v", err)
	}
	t.Cleanup(func() { _ = conn.conn.Close() })
	assertDialHandoffExchange(t, conn, resp)
	if err := awaitDialHandoff(t, peer.done, "上游工作者未归还"); err != nil {
		t.Fatalf("上游实际消息或关闭证据不符: %v", err)
	}
}

// 不让取消只在 TCP 拨号前奏效：请求已到达且上游不回 101 时也必须唤醒读与 handler。
func TestDialBlockedHandshakeCancellation(t *testing.T) {
	ready := make(chan struct{})
	handlerDone := make(chan struct{})
	handlerStop := make(chan struct{})
	var stopOnce sync.Once
	sockets := newDialHandoffSockets()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		close(ready)
		select {
		case <-r.Context().Done():
		case <-handlerStop:
		}
	}))
	srv.Config.ConnState = sockets.track
	srv.Start()
	fallback := func() {
		sockets.close()
		stopOnce.Do(func() { close(handlerStop) })
	}
	t.Cleanup(func() {
		fallback()
		// 先解除 socket 与 handler，再 join；不能让 Server.Close 等被测取消机制。
		srv.Close()
		select {
		case <-ready:
			awaitDialHandoff(t, handlerDone, "清理未归还阻塞握手 handler")
			t.Log("测试 owner fallback 已释放实际 socket 并 join 阻塞握手 handler")
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := startDialHandoff(t, ctx, wsURL(t, srv), cancel, fallback)
	awaitDialHandoff(t, ready, "握手请求未到达")
	cancel()
	got := awaitDialHandoff(t, result, "取消未唤醒阻塞握手")
	if got.conn != nil {
		_ = got.conn.conn.Close()
		t.Fatal("取消阻塞握手却返回了可用连接")
	}
	if !errors.Is(got.err, ErrHandshake) || got.resp != nil {
		t.Fatalf("阻塞握手取消结果不符: %v", got.err)
	}
	awaitDialHandoff(t, handlerDone, "取消未释放实际 TCP 与上游 handler")
}

// 门闩只暂停测试 context 的 watchdog 访问，模拟该 goroutine 的合法迟到调度。
// 返回成功但它仍被门闩挂住，等价于调用方拿到 socket 时旧 context 尚持有关闭权。
func TestDialHandoffJoinsWatchdog(t *testing.T) {
	peer := newDialHandoffPeer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gate := &dialWatchdogGateContext{
		Context: ctx,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	t.Cleanup(gate.open)
	result := startDialHandoff(t, gate, peer.url, func() {
		cancel()
		gate.open()
	}, peer.fallback)
	awaitDialHandoff(t, gate.entered, "watchdog 未进入测试门闩")
	awaitDialHandoff(t, peer.ready, "握手请求未到达")
	peer.upgrade()
	awaitDialHandoff(t, peer.sent101, "上游未写完 101")

	var got dialHandoffResult
	select {
	case got = <-result:
		t.Error("Dial 返回时握手 watchdog 尚未归还 socket 关闭权")
		cancel()
		gate.open()
	case <-time.After(100 * time.Millisecond):
		// 这里只给“不应提前返回”一个有界观察窗，不靠等待让旧竞态消失。
		// 放行前先取消，让交接与取消都 ready；只补 join 而不仲裁仍可能交付死连接。
		cancel()
		gate.open()
		got = awaitDialHandoff(t, result, "放行 watchdog 后 Dial 仍未返回")
	}
	if got.err != nil {
		if got.conn != nil || !errors.Is(got.err, ErrHandshake) {
			t.Fatalf("取消认领关闭权后的结果不符: %v", got.err)
		}
		err := awaitDialHandoff(t, peer.done, "取消后上游工作者未归还")
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatal("取消后实际 socket 未释放，只靠测试 deadline 退出")
		}
		return
	}
	if got.conn == nil {
		t.Fatal("成功交接没有连接")
	}
	t.Cleanup(func() { _ = got.conn.conn.Close() })
	assertDialHandoffExchange(t, got.conn, got.resp)
	if err := awaitDialHandoff(t, peer.done, "上游工作者未归还"); err != nil {
		t.Fatalf("交接后实际 socket 不可用: %v", err)
	}
}

// 取消与 101 同时放行：只接受“错误且无连接”或“成功且实际双向消息与关闭均完整”。
func TestDialHandoffCancellationRace(t *testing.T) {
	for i := 0; i < 16; i++ {
		t.Run("attempt_"+string(rune('a'+i)), func(t *testing.T) {
			peer := newDialHandoffPeer(t)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			result := startDialHandoff(t, ctx, peer.url, cancel, peer.fallback)
			awaitDialHandoff(t, peer.ready, "握手请求未到达")
			start := make(chan struct{})
			cancelDone := make(chan struct{})
			go func() {
				<-start
				cancel()
				close(cancelDone)
			}()
			close(start)
			peer.upgrade()
			got := awaitDialHandoff(t, result, "取消竞争未归还 Dial 工作者")
			awaitDialHandoff(t, cancelDone, "取消工作者未归还")
			if got.err != nil {
				if got.conn != nil {
					_ = got.conn.conn.Close()
					t.Fatal("取消失败结果仍携带可用连接")
				}
				if !errors.Is(got.err, ErrHandshake) {
					t.Fatalf("取消竞争丢失握手错误分类: %v", got.err)
				}
				if err := awaitDialHandoff(t, peer.done, "取消后上游工作者未归还"); err != nil {
					var ne net.Error
					if errors.As(err, &ne) && ne.Timeout() {
						t.Fatal("取消后实际 socket 未释放，只靠测试 deadline 退出")
					}
				}
				return
			}
			if got.conn == nil {
				t.Fatal("成功结果没有连接")
			}
			t.Cleanup(func() { _ = got.conn.conn.Close() })
			assertDialHandoffExchange(t, got.conn, got.resp)
			if err := awaitDialHandoff(t, peer.done, "成功后上游工作者未归还"); err != nil {
				t.Fatalf("成功结果却交付了不可用 socket: %v", err)
			}
		})
	}
}

type dialWatchdogGateContext struct {
	context.Context
	entered, release chan struct{}
	enterOnce        sync.Once
	releaseOnce      sync.Once
}

func (c *dialWatchdogGateContext) Done() <-chan struct{} {
	// net.Dialer 也访问 Done；只延迟 Dial 自己的 goroutine，不能把拨号阻塞冒充交接失败。
	// 调用栈仅用于安排交错；成败仍由真实 Dial、TCP 消息和关闭证据裁决。
	pc, _, _, ok := runtime.Caller(1)
	if ok {
		if fn := runtime.FuncForPC(pc); fn != nil && strings.Contains(fn.Name(), "/ws.Dial.func") {
			c.enterOnce.Do(func() { close(c.entered) })
			<-c.release
		}
	}
	return c.Context.Done()
}

func (c *dialWatchdogGateContext) open() {
	c.releaseOnce.Do(func() { close(c.release) })
}

// ConnState 从 StateNew 就登记真实 TCP；Hijack 后仍保留兜底关闭权。
// close 与迟到登记用同一把锁封口，实际 Close 在锁外，避免测试清理卡住服务器状态回调。
type dialHandoffSockets struct {
	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

func newDialHandoffSockets() *dialHandoffSockets {
	return &dialHandoffSockets{conns: make(map[net.Conn]struct{})}
}

func (s *dialHandoffSockets) track(conn net.Conn, state http.ConnState) {
	s.mu.Lock()
	if state == http.StateClosed {
		delete(s.conns, conn)
		s.mu.Unlock()
		return
	}
	if s.closed {
		s.mu.Unlock()
		_ = conn.Close()
		return
	}
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
}

func (s *dialHandoffSockets) close() {
	s.mu.Lock()
	s.closed = true
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for conn := range conns {
		_ = conn.Close()
	}
}

type dialHandoffPeer struct {
	url     string
	ready   chan struct{}
	sent101 chan struct{}
	done    chan error
	started chan struct{}
	exited  chan struct{}
	sockets *dialHandoffSockets
	allow   chan struct{}
	once    sync.Once
}

func newDialHandoffPeer(t *testing.T) *dialHandoffPeer {
	t.Helper()
	p := &dialHandoffPeer{
		ready: make(chan struct{}), sent101: make(chan struct{}),
		done: make(chan error, 1), started: make(chan struct{}), exited: make(chan struct{}),
		sockets: newDialHandoffSockets(), allow: make(chan struct{}),
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(p.exited)
		close(p.started)
		p.done <- func() error {
			raw, brw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return err
			}
			defer raw.Close()
			if err := raw.SetDeadline(time.Now().Add(dialHandoffWait)); err != nil {
				return err
			}
			close(p.ready)
			select {
			case <-p.allow:
			case <-time.After(dialHandoffWait):
				return errors.New("测试未放行 101")
			}
			// 把 101 与首帧一同刷出，防止修复同步时改从裸 socket 读、丢掉预读字节。
			_, err = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
				"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
				"X-Handoff-Test: synthetic\r\nSec-WebSocket-Accept: " +
				acceptKey(r.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n")
			if err != nil {
				return err
			}
			if err := WriteFrame(brw, Frame{FIN: true, Opcode: OpBinary, Payload: []byte{0, 1, 255}}, false); err != nil {
				return err
			}
			if err := brw.Flush(); err != nil {
				return err
			}
			close(p.sent101)
			frame, err := ReadFrame(brw.Reader, maxTestPayload)
			if err != nil {
				return err
			}
			if frame.Opcode != OpText || !frame.FIN || string(frame.Payload) != "handoff-message" {
				return errors.New("实际上行完整消息不符")
			}
			if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpText, Payload: []byte("handoff-reply")}, false); err != nil {
				return err
			}
			frame, err = ReadFrame(brw.Reader, maxTestPayload)
			if err != nil {
				return err
			}
			code, reason, err := DecodeClosePayload(frame.Payload)
			if err != nil || frame.Opcode != OpClose || code != CloseNormal || reason != "handoff-done" {
				return errors.New("实际关闭帧不符")
			}
			var extra [1]byte
			if _, err := brw.Read(extra[:]); !errors.Is(err, io.EOF) {
				return errors.New("实际 socket 未在关闭帧后释放")
			}
			return nil
		}()
	}))
	srv.Config.ConnState = p.sockets.track
	srv.Start()
	p.url = wsURL(t, srv)
	t.Cleanup(func() {
		p.fallback()
		srv.Close()
		select {
		case <-p.started:
			awaitDialHandoff(t, p.exited, "清理未归还实际上游工作者")
			t.Log("测试 owner fallback 后已 join 上游 handler")
		default:
		}
	})
	return p
}

func (p *dialHandoffPeer) upgrade() { p.once.Do(func() { close(p.allow) }) }

// 门闩、未接管的 active 连接与 Hijack 后的 socket 都须在 join 之前独立释放。
func (p *dialHandoffPeer) fallback() {
	p.sockets.close()
	p.upgrade()
}

func assertDialHandoffExchange(t *testing.T, conn *Conn, resp *http.Response) {
	t.Helper()
	if resp == nil || resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("X-Handoff-Test") != "synthetic" {
		t.Fatal("交接丢失握手响应状态或头")
	}
	if err := conn.conn.SetDeadline(time.Now().Add(dialHandoffWait)); err != nil {
		t.Fatalf("成功返回后实际 socket 已失效: %v", err)
	}
	op, payload, err := conn.ReadMessage()
	if err != nil || op != OpBinary || !bytes.Equal(payload, []byte{0, 1, 255}) {
		t.Fatalf("交接丢失预读完整首帧: %v", err)
	}
	if err := conn.WriteMessage(OpText, []byte("handoff-message")); err != nil {
		t.Fatalf("成功返回后的上行消息失败: %v", err)
	}
	op, payload, err = conn.ReadMessage()
	if err != nil || op != OpText || string(payload) != "handoff-reply" {
		t.Fatalf("成功返回后的下行消息失败: %v", err)
	}
	if err := conn.Close(CloseNormal, "handoff-done"); err != nil {
		t.Fatalf("成功返回后的实际 Close 失败: %v", err)
	}
}

// 被测取消可能失效，cleanup 必须先放行门闩并独立关 socket，再 join Dial。
// fallback 只在正常证据断言结束或失败后运行，不能替产品取消制造假绿。
func startDialHandoff(t *testing.T, ctx context.Context, url string, stop, fallback func()) <-chan dialHandoffResult {
	t.Helper()
	result := make(chan dialHandoffResult, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		conn, resp, err := Dial(ctx, url, DialOptions{MaxPayload: maxTestPayload})
		result <- dialHandoffResult{conn, resp, err}
	}()
	t.Cleanup(func() {
		stop()
		fallback()
		awaitDialHandoff(t, exited, "清理未归还 Dial 工作者")
		t.Log("测试 owner fallback 后已 join Dial 工作者")
		select {
		case got := <-result:
			if got.conn != nil {
				_ = got.conn.conn.Close()
			}
		default:
		}
	})
	return result
}

func awaitDialHandoff[T any](t *testing.T, ch <-chan T, message string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(dialHandoffWait):
		t.Fatal(message)
		var zero T
		return zero
	}
}
