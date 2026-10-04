package ws

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 必须在 handler 返回之前取样，防 HTTP 正常结束时的取消混淆接管后的读取副作用。
func TestAcceptReadFailureDoesNotCancelHTTPRequest(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		buffered bool
	}{
		{"eof", false}, {"idle", false}, {"external-cancel", false},
		{"eof", true}, {"idle", true}, {"external-cancel", true},
	} {
		t.Run(fmt.Sprintf("%s/buffered=%v", tc.mode, tc.buffered), func(t *testing.T) {
			mode := tc.mode
			var prefix bytes.Buffer
			if tc.buffered {
				if err := WriteFrame(&prefix, Frame{FIN: true, Opcode: OpText, Payload: []byte("prebuffered")}, true); err != nil {
					t.Fatal(err)
				}
			}
			type result struct{ readErr, requestErr error }
			got := make(chan result, 1)
			exited := make(chan struct{})
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			idle := time.Second
			if mode == "idle" {
				idle = 80 * time.Millisecond
			}
			s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(exited)
				c, err := Accept(&acceptPrefetchWriter{w, prefix.Len()}, r, AcceptOptions{MaxPayload: 1024, Idle: idle, WriteTimeout: time.Second})
				if err != nil {
					got <- result{err, r.Context().Err()}
					return
				}
				defer c.Close(1000, "")
				if tc.buffered {
					op, payload, err := c.ReadMessage()
					if err != nil || op != OpText || string(payload) != "prebuffered" {
						got <- result{fmt.Errorf("首条预读消息丢失: %v", err), r.Context().Err()}
						return
					}
					if err := c.WriteMessage(OpText, payload); err != nil {
						got <- result{err, r.Context().Err()}
						return
					}
				}
				// 显式父取消仍要唤醒读取；EOF/idle 不应反过来伪造父取消。
				m, err := c.ReadOwnedMessage(r.Context())
				if m != nil {
					m.Release()
				}
				got <- result{err, r.Context().Err()}
			}))
			s.Config.BaseContext = func(net.Listener) context.Context { return parent }
			s.Start()
			defer s.Close()
			raw, reader := acceptTestHTTPClient(t, s.URL, prefix.Bytes())
			defer raw.Close()
			acceptTest101(t, reader)
			if tc.buffered {
				frame, err := ReadFrame(reader, 1024)
				if err != nil || frame.Opcode != OpText || string(frame.Payload) != "prebuffered" {
					t.Fatalf("未收到预读确认: %v", err)
				}
			}
			want := error(io.EOF)
			switch mode {
			case "eof":
				if err := raw.(*net.TCPConn).CloseWrite(); err != nil {
					t.Fatal(err)
				}
			case "idle":
				want = ErrIdleTimeout
			case "external-cancel":
				want = context.Canceled
				cancel()
			}
			r := awaitDialHandoff(t, got, "接管读取未有限返回")
			awaitDialHandoff(t, exited, "接管 handler 未归还关闭工作")
			if !errors.Is(r.readErr, want) {
				t.Errorf("读取原因=%v，期望=%v", r.readErr, want)
			}
			if mode != "external-cancel" && r.requestErr != nil {
				t.Errorf("接管后的 %s 伪造 HTTP 请求取消: %v", mode, r.requestErr)
			}
			if mode == "external-cancel" && !errors.Is(r.requestErr, context.Canceled) {
				t.Error("真实外部取消被屏蔽")
			}
		})
	}
}

// 分界落在 opcode、扩展长度、掩码、payload、分片/控制帧之间时，都不能漏字节或重读。
func TestAcceptPreservesBufferedFrameBoundaries(t *testing.T) {
	payload := bytes.Repeat([]byte{0, 255, 128, 1, 2}, 26)
	var wire bytes.Buffer
	for _, frame := range []Frame{
		{FIN: false, Opcode: OpBinary, Payload: payload},
		{FIN: true, Opcode: OpPing, Payload: []byte("ping")},
		{FIN: true, Opcode: OpContinuation, Payload: []byte("tail")},
		{FIN: true, Opcode: OpText, Payload: []byte("next")},
	} {
		if err := WriteFrame(&wire, frame, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, prefix := range []int{0, 1, 2, 3, 5, 7, 8, 9, 137, 138, 139, wire.Len()} {
		t.Run(fmt.Sprint(prefix), func(t *testing.T) {
			done := make(chan error, 1)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				done <- func() error {
					c, err := Accept(&acceptPrefetchWriter{w, prefix}, r, AcceptOptions{MaxPayload: 1024, Idle: time.Second, WriteTimeout: time.Second})
					if err != nil {
						return err
					}
					defer c.Close(1000, "")
					for i, want := range [][]byte{append(bytes.Clone(payload), []byte("tail")...), []byte("next"), []byte("after-buffer")} {
						op, raw, err := c.ReadMessage()
						wantOp := OpText
						if i == 0 {
							wantOp = OpBinary
						}
						if err != nil || op != wantOp || !bytes.Equal(raw, want) {
							return fmt.Errorf("预读分界 %d 的第 %d 条类型/字节丢失: %v", prefix, i, err)
						}
						if err := c.WriteMessage(op, raw); err != nil {
							return err
						}
					}
					return nil
				}()
			}))
			defer s.Close()
			raw, reader := acceptTestHTTPClient(t, s.URL, wire.Bytes()[:prefix])
			defer raw.Close()
			acceptTest101(t, reader)
			if _, err := raw.Write(wire.Bytes()[prefix:]); err != nil {
				t.Fatal(err)
			}
			client := newConnBuffered(raw, reader, RoleClient, 1024, time.Second)
			for _, want := range [][]byte{append(bytes.Clone(payload), []byte("tail")...), []byte("next")} {
				_, got, err := client.ReadMessage()
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("未收到原样回包: %v", err)
				}
			}
			if err := client.WriteMessage(OpText, []byte("after-buffer")); err != nil {
				t.Fatal(err)
			}
			_, got, err := client.ReadMessage()
			if err != nil || string(got) != "after-buffer" {
				t.Fatalf("缓存耗尽后仍未交接 socket: %v", err)
			}
			if err := awaitDialHandoff(t, done, "预读测试 handler 未退出"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type acceptPrefetchWriter struct {
	http.ResponseWriter
	prefix int
}

func (w *acceptPrefetchWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return c, brw, err
	}
	// 客户端在 101 前只写 prefix；强制其进入真实 HTTP bufio，避免 TCP 分包让测试空跑。
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := brw.Peek(w.prefix); err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	return c, brw, nil
}

func acceptTestHTTPClient(t *testing.T, url string, prefix []byte) (net.Conn, *bufio.Reader) {
	t.Helper()
	raw, err := net.DialTimeout("tcp", strings.TrimPrefix(url, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
	request := []byte("GET /realtime HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	if _, err := raw.Write(append(request, prefix...)); err != nil {
		t.Fatal(err)
	}
	return raw, bufio.NewReader(raw)
}

func acceptTest101(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	resp, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("本地 HTTP 接管失败: %v", err)
	}
}
