package ws

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 畸形请求必须在接管前失败；合法 Origin 和扩展提议不能被当成坏握手。
func TestProductionUpgradeValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*http.Request)
		valid bool
	}{
		{"valid", func(*http.Request) {}, true},
		{"origin_and_extension", func(r *http.Request) {
			r.Header.Set("Origin", "https://synthetic.invalid")
			r.Header.Set("Sec-WebSocket-Extensions", "permessage-deflate; client_max_window_bits")
		}, true},
		{"token_lists", func(r *http.Request) {
			r.Header.Set("Connection", "keep-alive, UpGrAdE")
			r.Header.Set("Upgrade", "other, WebSocket")
		}, true},
		{"method", func(r *http.Request) { r.Method = http.MethodPost }, false},
		{"http_1_0", func(r *http.Request) { r.ProtoMinor = 0 }, false},
		{"http_2", func(r *http.Request) { r.ProtoMajor = 2; r.ProtoMinor = 0 }, false},
		{"missing_upgrade", func(r *http.Request) { r.Header.Del("Upgrade") }, false},
		{"missing_connection", func(r *http.Request) { r.Header.Del("Connection") }, false},
		{"version", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Version", "8") }, false},
		{"duplicate_version", func(r *http.Request) { r.Header.Add("Sec-WebSocket-Version", "13") }, false},
		{"duplicate_key", func(r *http.Request) { r.Header.Add("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==") }, false},
		{"missing_key", func(r *http.Request) { r.Header.Del("Sec-WebSocket-Key") }, false},
		{"invalid_key", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "synthetic-private") }, false},
		{"short_nonce", func(r *http.Request) {
			r.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString([]byte("short")))
		}, false},
		{"newline_nonce", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBs\nZSBub25jZQ==") }, false},
		{"body", func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader("synthetic-private"))
			r.ContentLength = 17
		}, false},
		{"unknown_body", func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader("synthetic-private"))
			r.ContentLength = -1
		}, false},
		{"unmarked_body", func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader("synthetic-private")) }, false},
		{"transfer_encoding", func(r *http.Request) { r.TransferEncoding = []string{"chunked"} }, false},
		{"transfer_encoding_header", func(r *http.Request) { r.Header.Set("Transfer-Encoding", "chunked") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := productionUpgradeRequest()
			tc.edit(r)
			validationErr := ValidateUpgrade(r)
			if (validationErr == nil) != tc.valid || (!tc.valid && !errors.Is(validationErr, ErrHandshake)) {
				t.Error("纯预检与接管前裁决不一致")
			}
			w := &productionHijackProbe{ResponseRecorder: httptest.NewRecorder()}
			_, err := Accept(w, r, AcceptOptions{})
			if w.hijacked != tc.valid {
				t.Error("请求合法性没有在接管前裁决")
			}
			if !errors.Is(err, ErrHandshake) || strings.Contains(err.Error(), "synthetic-private") {
				t.Error("预检错误分类或脱敏不符")
			}
		})
	}
}

func productionUpgradeRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/realtime", nil)
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	return r
}

type productionHijackProbe struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (w *productionHijackProbe) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return nil, nil, errors.New("合成接管屏障")
}

// 仅摘要正确不代表协商成功；错误路径须实际释放 TCP，不能只返回一个错误。
func TestProductionDialRejectsInvalid101(t *testing.T) {
	for _, tc := range []struct{ name, remove, add string }{
		{"missing_upgrade", "Upgrade: websocket\r\n", ""},
		{"missing_connection", "Connection: Upgrade\r\n", ""},
		{"duplicate_accept", "", "Sec-WebSocket-Accept: synthetic-private\r\n"},
		{"extension", "", "Sec-WebSocket-Extensions: permessage-deflate\r\n"},
		{"subprotocol", "", "Sec-WebSocket-Protocol: synthetic-private\r\n"},
		{"http_1_0", "HTTP/1.1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, done := productionPeer(t, func(c net.Conn, br *bufio.Reader, r *http.Request) error {
				head := production101(r)
				if tc.name == "http_1_0" {
					head = strings.Replace(head, "HTTP/1.1", "HTTP/1.0", 1)
				} else {
					head = strings.Replace(head, tc.remove, "", 1)
				}
				head = strings.TrimSuffix(head, "\r\n") + tc.add + "\r\n"
				if _, err := io.WriteString(c, head); err != nil {
					return err
				}
				return productionExpectEOF(br)
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			c, resp, err := Dial(ctx, url, DialOptions{})
			if c != nil {
				_ = c.conn.Close()
			}
			if c != nil || resp == nil || !errors.Is(err, ErrHandshake) {
				t.Error("不完整的 101 协商被接受或丢失状态")
			} else if strings.Contains(err.Error(), "synthetic-private") {
				t.Error("握手错误泄露响应头")
			}
			if err := awaitDialHandoff(t, done, "错误路径未释放 TCP"); err != nil {
				t.Fatal("错误路径没有实际 EOF")
			}
		})
	}
}

func TestProductionDialErrorBody(t *testing.T) {
	for _, size := range []int{17, 64 << 10, (64 << 10) + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			body := strings.Repeat("x", size)
			url, done := productionPeer(t, func(c net.Conn, br *bufio.Reader, _ *http.Request) error {
				resp := &http.Response{StatusCode: 401, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{"Retry-After": []string{"7"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(size)}
				if err := resp.Write(c); err != nil {
					return err
				}
				return productionExpectEOF(br)
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			c, resp, err := Dial(ctx, url, DialOptions{})
			if c != nil || !errors.Is(err, ErrHandshake) || resp == nil || resp.StatusCode != 401 || resp.Header.Get("Retry-After") != "7" {
				t.Fatal("失败响应未保留分类与元数据")
			}
			got, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil || string(got) != body[:min(size, 64<<10)] {
				t.Error("返回后的失败 body 不可读或未按默认上限截断")
			}
			if size > 64<<10 && !strings.Contains(err.Error(), "截断") {
				t.Error("body 截断未显式报告")
			}
			if err := awaitDialHandoff(t, done, "失败响应未关闭 TCP"); err != nil {
				t.Fatal("失败响应没有实际 EOF")
			}
		})
	}
}

// 默认 HTTP 头预算不能无限分配，也不能误用为升级后的消息上限。
func TestProductionDialHeaderDefaultLimit(t *testing.T) {
	url, _ := productionPeer(t, func(c net.Conn, _ *bufio.Reader, r *http.Request) error {
		head := strings.TrimSuffix(production101(r), "\r\n") + "X-Large: " + strings.Repeat("x", 64<<10) + "\r\n\r\n"
		_, err := io.WriteString(c, head)
		return err
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, _, err := Dial(ctx, url, DialOptions{})
	if c != nil {
		_ = c.conn.Close()
	}
	if c != nil || !errors.Is(err, ErrHandshake) {
		t.Fatal("默认握手头预算未限制超大响应")
	}
}

func TestProductionDialSafeDiagnostics(t *testing.T) {
	t.Run("url", func(t *testing.T) {
		_, _, err := Dial(context.Background(), "ws://synthetic-private/%zz", DialOptions{})
		if !errors.Is(err, ErrHandshake) || strings.Contains(err.Error(), "synthetic-private") {
			t.Fatal("URL 解析错误泄露原始 URL")
		}
	})
	t.Run("response", func(t *testing.T) {
		url, _ := productionPeer(t, func(c net.Conn, _ *bufio.Reader, _ *http.Request) error {
			_, err := io.WriteString(c, "HTTP/1.1 synthetic-private\r\n\r\n")
			return err
		})
		_, _, err := Dial(context.Background(), url, DialOptions{})
		if !errors.Is(err, ErrHandshake) || strings.Contains(err.Error(), "synthetic-private") {
			t.Fatal("HTTP 解析错误泄露响应原文")
		}
	})
}

func production101(r *http.Request) string {
	return "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + acceptKey(r.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"
}

// 本机监听器独立拥有清理权，取消实现失效时也能关闭并 join，避免假红变成挂死。
func productionPeer(t *testing.T, serve func(net.Conn, *bufio.Reader, *http.Request) error) (string, <-chan error) {
	t.Helper()
	return productionTCPPeer(t, func(c net.Conn) error {
		br := bufio.NewReader(c)
		r, err := http.ReadRequest(br)
		if err != nil {
			return err
		}
		return serve(c, br, r)
	})
}

func productionTCPPeer(t *testing.T, serve func(net.Conn) error) (string, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("本机监听失败")
	}
	sockets := newDialHandoffSockets()
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		sockets.track(c, http.StateNew)
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(dialHandoffWait))
		done <- serve(c)
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		sockets.close()
		awaitDialHandoff(t, exited, "测试上游没有归还")
	})
	return "ws://" + ln.Addr().String(), done
}

func productionExpectEOF(r io.Reader) error {
	var b [1]byte
	if n, err := r.Read(b[:]); n != 0 || !errors.Is(err, io.EOF) {
		return errors.New("连接没有实际释放")
	}
	return nil
}

// 连续两帧一并发送，避免只保全第一段 bufio 预读、丢掉其后的字节。
func TestProductionDialPreservesBufferedFrames(t *testing.T) {
	url, done := productionPeer(t, func(c net.Conn, _ *bufio.Reader, r *http.Request) error {
		var b bytes.Buffer
		b.WriteString(production101(r))
		b.Write([]byte{0x82, 3, 0, 1, 255, 0x81, 2, 'o', 'k'})
		_, err := c.Write(b.Bytes())
		return err
	})
	c, _, err := Dial(context.Background(), url, DialOptions{MaxPayload: maxTestPayload})
	if err != nil {
		t.Fatal("合法 101 握手失败")
	}
	defer c.conn.Close()
	op, body, err := c.ReadMessage()
	if err != nil || op != OpBinary || !bytes.Equal(body, []byte{0, 1, 255}) {
		t.Fatal("预读二进制首帧丢失")
	}
	op, body, err = c.ReadMessage()
	if err != nil || op != OpText || string(body) != "ok" {
		t.Fatal("预读后继帧丢失")
	}
	if err := awaitDialHandoff(t, done, "上游未结束"); err != nil {
		t.Fatal("上游发送失败")
	}
}

// 精确边界包含状态行、每行 CRLF 与最后空行；升级后的大帧另走 MaxPayload。
func TestProductionDialHeaderBudget(t *testing.T) {
	for _, limit := range []int64{128, 129, 130} {
		t.Run(strconv.FormatInt(limit, 10), func(t *testing.T) {
			payload := bytes.Repeat([]byte{0, 255}, 64<<10)
			url, done := productionPeer(t, func(c net.Conn, _ *bufio.Reader, r *http.Request) error {
				var b bytes.Buffer
				b.WriteString(production101(r))
				if err := WriteFrame(&b, Frame{FIN: true, Opcode: OpBinary, Payload: payload}, false); err != nil {
					return err
				}
				_, err := c.Write(b.Bytes())
				return err
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			c, _, err := Dial(ctx, url, DialOptions{MaxHandshakeBytes: limit, MaxPayload: 256 << 10})
			if c != nil {
				defer c.conn.Close()
			}
			if limit < 129 {
				if c != nil || !errors.Is(err, ErrHandshake) {
					t.Fatal("超出头预算一字节仍然升级成功")
				}
				return
			}
			if err != nil || c == nil {
				t.Fatal("头恰好在预算内却被拒绝")
			}
			op, got, err := c.ReadMessage()
			if err != nil || op != OpBinary || !bytes.Equal(got, payload) {
				t.Fatal("HTTP 头限额截断了升级后的 WS 大帧")
			}
			if err := awaitDialHandoff(t, done, "上游未退出"); err != nil {
				t.Fatal("上游未完整发送")
			}
		})
	}
}

func TestProductionDialErrorBodyBudget(t *testing.T) {
	for _, tc := range []struct {
		name, wire, want string
		truncated        bool
	}{
		{"exact", "HTTP/1.1 401 Unauthorized\r\nContent-Length: 4\r\n\r\ndata", "data", false},
		{"large", "HTTP/1.1 401 Unauthorized\r\nContent-Length: 5\r\n\r\ndata!", "data", true},
		{"chunked", "HTTP/1.1 401 Unauthorized\r\nTransfer-Encoding: chunked\r\n\r\n5\r\ndata!\r\n0\r\n\r\n", "data", true},
		{"short", "HTTP/1.1 401 Unauthorized\r\nContent-Length: 4\r\n\r\nda", "da", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, _ := productionPeer(t, func(c net.Conn, _ *bufio.Reader, _ *http.Request) error {
				_, err := io.WriteString(c, tc.wire)
				return err
			})
			_, resp, err := Dial(context.Background(), url, DialOptions{MaxErrorBodyBytes: 4})
			if resp == nil || !errors.Is(err, ErrHandshake) {
				t.Fatal("失败响应丢失")
			}
			got, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil || string(got) != tc.want {
				t.Error("错误 body 限额或有界可读前缀不符")
			}
			if strings.Contains(err.Error(), "截断") != tc.truncated {
				t.Error("限额临界或截断诊断不符")
			}
			if tc.name == "short" && !strings.Contains(err.Error(), "不完整") {
				t.Error("不完整 body 被当成完整响应")
			}
		})
	}
}

// abort 时未读完 TCP 数据可使对端收到 reset；超时或仍读到字节都不能证明释放。
func productionAbortClosed(n int, err error) bool {
	return n == 0 && (errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET))
}

func TestProductionAbortCloseEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		n      int
		err    error
		closed bool
	}{
		{"eof", 0, io.EOF, true},
		{"reset", 0, syscall.ECONNRESET, true},
		{"wrapped-reset", 0, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, true},
		{"timeout", 0, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ETIMEDOUT}, false},
		{"unexpected-eof", 0, io.ErrUnexpectedEOF, false},
		{"broken-pipe", 0, syscall.EPIPE, false},
		{"local-closed", 0, net.ErrClosed, false},
		{"reset-text-only", 0, errors.New("connection reset by peer"), false},
		{"no-error", 0, nil, false},
		{"data-with-eof", 1, io.EOF, false},
		{"data-with-reset", 1, syscall.ECONNRESET, false},
		{"data-without-error", 1, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if productionAbortClosed(tc.n, tc.err) != tc.closed {
				t.Fatal("abort 关闭证据判定不符")
			}
		})
	}
}

// 没有 EOF 的失败 body 仍属于原握手 ctx；只保留已收到的前缀并释放实际 socket。
func TestProductionDialSlowErrorBodyCancellation(t *testing.T) {
	ready := make(chan struct{})
	exited := make(chan struct{})
	t.Cleanup(func() { awaitDialHandoff(t, exited, "取消后 Dial 工作者未归还") })
	prefix := strings.Repeat("p", 4<<20)
	url, done := productionPeer(t, func(c net.Conn, br *bufio.Reader, _ *http.Request) error {
		// 超过内核发送缓冲的前缀写完才就绪，确保客户端已解析头并进入 body 读取。
		if err := c.(*net.TCPConn).SetWriteBuffer(1024); err != nil {
			return err
		}
		if _, err := io.WriteString(c, "HTTP/1.1 401 Unauthorized\r\nRetry-After: 7\r\n\r\n"+prefix); err != nil {
			return err
		}
		close(ready)
		var b [1]byte
		n, err := br.Read(b[:])
		if !productionAbortClosed(n, err) {
			return errors.New("取消未实际释放连接")
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan dialHandoffResult, 1)
	go func() {
		defer close(exited)
		c, resp, err := Dial(ctx, url, DialOptions{MaxErrorBodyBytes: 4 << 20})
		result <- dialHandoffResult{c, resp, err}
	}()
	awaitDialHandoff(t, ready, "失败响应未就绪")
	cancel()
	got := awaitDialHandoff(t, result, "ctx 取消没有中止慢 body")
	if got.conn != nil || got.resp == nil || !errors.Is(got.err, ErrHandshake) {
		t.Fatal("慢 body 的握手错误分类不符")
	}
	if !errors.Is(got.err, context.Canceled) {
		t.Error("未报告 body 读取被握手 ctx 取消")
	}
	body, err := io.ReadAll(got.resp.Body)
	_ = got.resp.Body.Close()
	// 取消可能发生在读取前；不能把内核已收到等同于应用已读取。
	if err != nil || len(body) == 0 || !strings.HasPrefix(prefix, string(body)) {
		t.Error("取消后没有可读的有界前缀")
	}
	if err := awaitDialHandoff(t, done, "取消未释放上游"); err != nil {
		t.Error("取消未实际关闭 socket")
	}
}

func TestProductionDialIncompleteResponse(t *testing.T) {
	for _, wire := range []string{"HTTP/1.1 101", "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"} {
		url, _ := productionPeer(t, func(c net.Conn, _ *bufio.Reader, _ *http.Request) error {
			_, err := io.WriteString(c, wire)
			return err
		})
		c, _, err := Dial(context.Background(), url, DialOptions{MaxHandshakeBytes: 256})
		if c != nil || !errors.Is(err, ErrHandshake) {
			t.Fatal("不完整 HTTP 响应被接纳")
		}
	}
}

func TestProductionDialNoNegotiationOffers(t *testing.T) {
	url, done := productionPeer(t, func(c net.Conn, _ *bufio.Reader, r *http.Request) error {
		if r.Header.Get("Sec-WebSocket-Protocol") != "" || r.Header.Get("Sec-WebSocket-Extensions") != "" {
			return errors.New("发出了不受支持的协商提议")
		}
		_, err := io.WriteString(c, production101(r))
		return err
	})
	h := http.Header{}
	h.Set("Sec-WebSocket-Protocol", "synthetic-private")
	h.Set("Sec-WebSocket-Extensions", "permessage-deflate")
	c, _, err := Dial(context.Background(), url, DialOptions{Header: h})
	if c != nil {
		_ = c.conn.Close()
	}
	if err != nil {
		t.Error("忽略不支持的提议后应可握手")
	}
	if err := awaitDialHandoff(t, done, "上游未结束"); err != nil {
		t.Error("Dial 向上游提供了不支持的扩展或子协议")
	}
}

func TestProductionDialConnectTimeout(t *testing.T) {
	t.Run("tls_stall", func(t *testing.T) {
		ready := make(chan struct{})
		url, done := productionTCPPeer(t, func(c net.Conn) error {
			var b [1]byte
			if _, err := c.Read(b[:]); err != nil {
				return err
			}
			close(ready)
			_, err := io.Copy(io.Discard, c)
			return err
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		c, _, err := Dial(ctx, "wss"+strings.TrimPrefix(url, "ws"), DialOptions{ConnectTimeout: 100 * time.Millisecond})
		awaitDialHandoff(t, ready, "TLS ClientHello 未到达")
		if c != nil || !errors.Is(err, ErrHandshake) || ctx.Err() != nil {
			t.Error("TCP/TLS 子预算没有独立终止阻塞的 TLS 握手")
		}
		if err := awaitDialHandoff(t, done, "TLS 超时未释放 socket"); err != nil {
			t.Error("TLS 超时未正常释放实际连接")
		}
	})
	t.Run("http_and_business_survive", func(t *testing.T) {
		url, done := productionPeer(t, func(c net.Conn, br *bufio.Reader, r *http.Request) error {
			// 请求已经到达后再跨过 connect 期限；这是期限边界测试，不猜工作者是否就绪。
			<-time.After(100 * time.Millisecond)
			if _, err := io.WriteString(c, production101(r)); err != nil {
				return err
			}
			frame, err := ReadFrame(br, maxTestPayload)
			if err != nil || string(frame.Payload) != "after-connect" {
				return errors.New("connect 期限污染业务阶段")
			}
			return nil
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		c, _, err := Dial(ctx, url, DialOptions{ConnectTimeout: 25 * time.Millisecond})
		if err != nil || c == nil {
			t.Fatal("connect 期限错误地终止 HTTP 握手")
		}
		defer c.conn.Close()
		cancel()
		if err := c.WriteMessage(OpText, []byte("after-connect")); err != nil {
			t.Error("阶段期限或取消污染交接后的业务连接")
		}
		if err := awaitDialHandoff(t, done, "上游未结束"); err != nil {
			t.Error("业务帧未真正到达")
		}
	})
}

func TestProductionDialNegativeOptions(t *testing.T) {
	for _, opts := range []DialOptions{{ConnectTimeout: -1}, {Idle: -1}, {MaxPayload: -1}, {MaxHandshakeBytes: -1}, {MaxErrorBodyBytes: -1}} {
		url, _ := productionPeer(t, func(c net.Conn, _ *bufio.Reader, r *http.Request) error {
			_, err := io.WriteString(c, production101(r))
			return err
		})
		c, _, err := Dial(context.Background(), url, opts)
		if c != nil {
			_ = c.conn.Close()
		}
		if c != nil || !errors.Is(err, ErrHandshake) {
			t.Error("负选项没有显式拒绝")
		}
	}
}

type productionTCPHijacker struct {
	*httptest.ResponseRecorder
	c   net.Conn
	brw *bufio.ReadWriter
}

func (w *productionTCPHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.c, w.brw, nil }

// 实际 TCP 写必须受明确期限限制；返回成功后原有读期限和握手写期限都不能残留。
func TestProductionAcceptHandshakeDeadline(t *testing.T) {
	t.Run("expired_closes", func(t *testing.T) {
		a, peer := tcpPair(t)
		w := &productionTCPHijacker{httptest.NewRecorder(), a, bufio.NewReadWriter(bufio.NewReader(a), bufio.NewWriter(a))}
		c, err := Accept(w, productionUpgradeRequest(), AcceptOptions{HandshakeDeadline: time.Now().Add(-time.Second)})
		if c != nil {
			_ = c.conn.Close()
		}
		if c != nil || !errors.Is(err, ErrHandshake) {
			t.Error("过期握手预算仍写出 101")
		}
		_ = peer.SetReadDeadline(time.Now().Add(time.Second))
		if err := productionExpectEOF(peer); err != nil {
			t.Error("101 写失败没有关闭 socket 或泄漏了响应字节")
		}
	})
	t.Run("success_clears_and_preserves", func(t *testing.T) {
		a, peer := tcpPair(t)
		brw := bufio.NewReadWriter(bufio.NewReader(a), bufio.NewWriter(a))
		// RFC 掩码首帧在接管前预读，防止期限修复顺带改丢 brw.Reader。
		if _, err := peer.Write([]byte{0x81, 0x82, 1, 2, 3, 4, 'o' ^ 1, 'k' ^ 2}); err != nil {
			t.Fatal("注入首帧失败")
		}
		if _, err := brw.Peek(8); err != nil {
			t.Fatal("预读首帧失败")
		}
		_ = a.SetDeadline(time.Now().Add(-time.Second))
		deadline := time.Now().Add(100 * time.Millisecond)
		w := &productionTCPHijacker{httptest.NewRecorder(), a, brw}
		r := productionUpgradeRequest()
		r.Header.Set("Origin", "https://synthetic.invalid")
		r.Header.Set("Sec-WebSocket-Extensions", "permessage-deflate")
		c, err := Accept(w, r, AcceptOptions{HandshakeDeadline: deadline, MaxPayload: maxTestPayload})
		if err != nil || c == nil {
			t.Fatal("Accept 没有显式覆盖旧写期限")
		}
		defer c.conn.Close()
		reader := bufio.NewReader(peer)
		resp, err := http.ReadResponse(reader, r)
		if err != nil || resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Extensions") != "" {
			t.Fatal("合法提议被拒或意外选择了扩展")
		}
		op, body, err := c.ReadMessage()
		if err != nil || op != OpText || string(body) != "ok" {
			t.Fatal("Accept 丢失预读首帧")
		}
		if err := WriteFrame(peer, Frame{FIN: true, Opcode: OpText, Payload: []byte("fresh")}, true); err != nil {
			t.Fatal("注入后继帧失败")
		}
		_, body, err = c.ReadMessage()
		if err != nil || string(body) != "fresh" {
			t.Fatal("Accept 没有清除接管遗留读期限")
		}
		<-time.After(time.Until(deadline) + time.Millisecond)
		if err := c.WriteMessage(OpText, []byte("later")); err != nil {
			t.Fatal("握手写期限污染业务消息")
		}
		frame, err := ReadFrame(reader, maxTestPayload)
		if err != nil || string(frame.Payload) != "later" {
			t.Fatal("期限后的业务帧未实际到达")
		}
	})
	t.Run("negative_options", func(t *testing.T) {
		for _, opts := range []AcceptOptions{{Idle: -1}, {MaxPayload: -1}} {
			w := &productionHijackProbe{ResponseRecorder: httptest.NewRecorder()}
			_, err := Accept(w, productionUpgradeRequest(), opts)
			if w.hijacked || !errors.Is(err, ErrHandshake) {
				t.Error("负选项没有在接管前拒绝")
			}
		}
	})
}
