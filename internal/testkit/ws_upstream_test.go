package testkit

import (
	"bufio"
	"context"
	"encoding/json"
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

// 错 method/path/query、绕过协商 token 校验或把错配伪装为普通 404 都必须失败。
func TestWSUpstreamHandshake(t *testing.T) {
	t.Run("exact_upgrade_and_safe_protocols", func(t *testing.T) {
		f := wsUpstreamFixture()
		f.Response.WS.Upstream.Headers["Sec-WebSocket-Protocol"] = "realtime, openai-beta.realtime-v1"
		u, srv := newWSUpstreamTest(t, f, DefaultWSLimits())
		// 此处验证回放 matcher 的安全 token 与脱敏契约，不能借生产 Dial
		// 发出它不支持的协商提议；原始请求仍完整经过 WSReplayUpstream。
		r := wsUpstreamHTTPRequest(t, srv)
		r.URL.RawQuery = "tag=b&tag=a&model=synthetic-model&tag=a&token=synthetic-private"
		r.Header.Set("Sec-WebSocket-Protocol", "openai-beta.realtime-v1, openai-insecure-api-key.synthetic-private, realtime")
		peer, err := net.DialTimeout("tcp", r.URL.Host, 2*time.Second)
		if err != nil {
			t.Fatal("本地原始 TCP 拨号失败")
		}
		t.Cleanup(func() { _ = peer.Close() })
		if err := peer.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal("本地原始 TCP 期限设置失败")
		}
		if err := r.Write(peer); err != nil {
			t.Fatal("本地原始握手发送失败")
		}
		resp, err := http.ReadResponse(bufio.NewReader(peer), r)
		if err != nil {
			t.Fatal("本地原始握手响应读取失败")
		}
		if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Protocol") != "" {
			t.Fatal("升级状态错误或回放端伪造了尚未实现的子协议选择")
		}
		conn := claimWSUpstreamTest(t, u)
		if err := ws.WriteFrame(peer, ws.Frame{FIN: true, Opcode: ws.OpText, Payload: []byte("synthetic-message")}, true); err != nil {
			t.Fatal("本地消息发送失败")
		}
		result := readWSUpstreamTest(t, conn)
		if result.err != nil || result.op != ws.OpText || string(result.payload) != "synthetic-message" || u.Err() != nil {
			t.Fatal("精确握手未交付可用连接")
		}
	})

	for _, tc := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"method", func(r *http.Request) { r.Method = "POST" }},
		{"route", func(r *http.Request) { r.URL.Path = "/synthetic-private" }},
		{"escaped_route", func(r *http.Request) { r.URL.RawPath = "/v1%2Frealtime" }},
		{"query", func(r *http.Request) { r.URL.RawQuery = "model=synthetic-private&tag=a&tag=a&tag=b" }},
		{"query_multiplicity", func(r *http.Request) { r.URL.RawQuery = "model=synthetic-model&tag=a&tag=b" }},
		{"missing_safe_header", func(r *http.Request) { r.Header.Del("X-Safe") }},
		{"missing_auth", func(r *http.Request) { r.Header.Del("Authorization") }},
		{"unknown_protocol", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Protocol", "vendor.synthetic-private") }},
		{"unrecorded_protocol", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Protocol", "realtime") }},
		{"nonce", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "synthetic-private") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, srv := newWSUpstreamTest(t, wsUpstreamFixture(), DefaultWSLimits())
			r := wsUpstreamHTTPRequest(t, srv)
			tc.mutate(r)
			resp := sendWSUpstreamHTTP(t, r)
			if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Sec-WebSocket-Accept") != "" {
				t.Fatal("错配没有在劫持前作为回放断言失败拒绝")
			}
			first := u.Err()
			assertWSUpstreamStaticError(t, first)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if conn, err := u.Connection(ctx); conn != nil || err != first {
				t.Fatal("等待连接未返回已记录的错配")
			}
			if u.Close() != first || u.Close() != first || u.Err() != first {
				t.Fatal("关闭或重复关闭丢失首次错配")
			}
		})
	}

	t.Run("missing_recorded_protocol", func(t *testing.T) {
		f := wsUpstreamFixture()
		f.Response.WS.Upstream.Headers["Sec-WebSocket-Protocol"] = "realtime"
		u, srv := newWSUpstreamTest(t, f, DefaultWSLimits())
		resp := sendWSUpstreamHTTP(t, wsUpstreamHTTPRequest(t, srv))
		if resp.StatusCode != 400 || u.Err() == nil {
			t.Fatal("缺少预期安全子协议仍被接受")
		}
	})
}

// 固定 HTTP 负例不能自动升级；错误对象与下游 response.status 不能混用。
func TestWSUpstreamHandshakeFailure(t *testing.T) {
	for _, status := range []int{401, 429} {
		name := "auth"
		if status == 429 {
			name = "quota"
		}
		t.Run(name, func(t *testing.T) {
			f := wsUpstreamFixture()
			f.Response.Status = 502
			f.Response.WS.UpstreamExpectedStatus = status
			f.Response.WS.UpstreamError = json.RawMessage(" {\"error\":{\"code\":\"synthetic-error\"}} ")
			f.Response.WS.Nodes = nil
			f.Response.WS.Coverage = nil
			f.Response.WS.Outcome = WSOutcome{Kind: "handshake_failed"}
			u, srv := newWSUpstreamTest(t, f, DefaultWSLimits())
			resp := sendWSUpstreamHTTP(t, wsUpstreamHTTPRequest(t, srv))
			body, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != status || string(body) != " {\"error\":{\"code\":\"synthetic-error\"}} " || resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("Sec-WebSocket-Accept") != "" {
				t.Fatal("预期握手失败的状态或字面错误信封被改变")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			conn, err := u.Connection(ctx)
			if conn != nil || err == nil || errors.Is(err, context.DeadlineExceeded) || u.Err() != nil || u.Close() != nil {
				t.Fatal("合法 HTTP 负例被当成可领取连接或回放断言错误")
			}
			assertWSUpstreamStaticError(t, err)
		})
	}
}

// 构造期漏验会让无效预算、会话或敏感握手直到服务器启动后才暴露。
func TestWSUpstreamConstructor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Fixture, *WSLimits)
	}{
		{"missing_session", func(f *Fixture, _ *WSLimits) { f.Response.WS = nil }},
		{"missing_note", func(f *Fixture, _ *WSLimits) { f.Note = "" }},
		{"invalid_trace", func(f *Fixture, _ *WSLimits) { f.Response.WS.Nodes[1].After = []string{"synthetic-private"} }},
		{"invalid_budget", func(_ *Fixture, l *WSLimits) { l.Replay = 0 }},
		{"small_budget", func(_ *Fixture, l *WSLimits) { l.MessageBytes = 1 }},
		{"unsafe_headers", func(f *Fixture, _ *WSLimits) {
			f.Response.WS.Upstream.Headers["Authorization"] = "Bearer synthetic-private"
		}},
		{"unsafe_protocol", func(f *Fixture, _ *WSLimits) {
			f.Response.WS.Upstream.Headers["Sec-WebSocket-Protocol"] = "vendor.synthetic-private"
		}},
		{"malformed_query", func(f *Fixture, _ *WSLimits) { f.Response.WS.Upstream.Query = "synthetic-private=%zz" }},
		{"client_body", func(f *Fixture, _ *WSLimits) { f.Request.Body = json.RawMessage(`{"synthetic-private":true}`) }},
		{"bad_error_object", func(f *Fixture, _ *WSLimits) {
			f.Name = "synthetic-private"
			f.Response.Status = 401
			f.Response.WS.UpstreamExpectedStatus = 401
			f.Response.WS.UpstreamError = json.RawMessage(`null`)
			f.Response.WS.Nodes = nil
			f.Response.WS.Outcome = WSOutcome{Kind: "handshake_failed"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, limits := wsUpstreamFixture(), DefaultWSLimits()
			tc.mutate(&f, &limits)
			u, err := NewWSReplayUpstream(f, limits)
			if u != nil || err == nil {
				t.Fatal("非法输入得到可用上游回放端")
			}
			assertWSUpstreamStaticError(t, err)
		})
	}
}

// 取消不得消费连接槽；并发领取只能成功一次；领取后迟到的错误仍由 Err/Close 保留。
func TestWSUpstreamConnectionCancellation(t *testing.T) {
	t.Run("cancel_then_claim_once", func(t *testing.T) {
		u, srv := newWSUpstreamTest(t, wsUpstreamFixture(), DefaultWSLimits())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if conn, err := u.Connection(ctx); conn != nil || !errors.Is(err, context.Canceled) || u.Err() != nil {
			t.Fatal("已取消等待未及时退出或污染了回放错误槽")
		}
		dialWSUpstreamTest(t, srv, wsUpstreamTestHeaders())
		claimWSUpstreamTest(t, u)
		ctx, cancel = context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if conn, err := u.Connection(ctx); conn != nil || err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("同一连接被重复领取或重复领取悬停")
		}
		if u.Close() != nil || u.Close() != nil || u.Err() != nil {
			t.Fatal("正常重复关闭产生了回放断言失败")
		}
	})

	t.Run("cancel_blocked_wait", func(t *testing.T) {
		u, _ := newWSUpstreamTest(t, wsUpstreamFixture(), DefaultWSLimits())
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { _, err := u.Connection(ctx); result <- err }()
		cancel()
		if err := awaitWSUpstreamError(t, result); !errors.Is(err, context.Canceled) {
			t.Fatal("取消未唤醒阻塞领取")
		}
	})

	t.Run("concurrent_claims", func(t *testing.T) {
		u, srv := newWSUpstreamTest(t, wsUpstreamFixture(), DefaultWSLimits())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		results := make(chan wsUpstreamClaimResult, 2)
		for range 2 {
			go func() { conn, err := u.Connection(ctx); results <- wsUpstreamClaimResult{conn, err} }()
		}
		dialWSUpstreamTest(t, srv, wsUpstreamTestHeaders())
		successes := 0
		for range 2 {
			select {
			case result := <-results:
				if result.conn != nil && result.err == nil {
					successes++
				} else if result.conn != nil || result.err == nil || errors.Is(result.err, context.DeadlineExceeded) {
					t.Fatal("并发领取产生无效结果")
				}
			case <-ctx.Done():
				t.Fatal("并发领取未全部退出")
			}
		}
		if successes != 1 {
			t.Fatal("连接槽被多个领取者消费")
		}
	})

	t.Run("late_duplicate_and_mismatch", func(t *testing.T) {
		u, srv := newWSUpstreamTest(t, wsUpstreamFixture(), DefaultWSLimits())
		dialWSUpstreamTest(t, srv, wsUpstreamTestHeaders())
		claimWSUpstreamTest(t, u)
		resp := sendWSUpstreamHTTP(t, wsUpstreamHTTPRequest(t, srv))
		if resp.StatusCode != http.StatusConflict {
			t.Fatal("领取后的第二次连接未拒绝")
		}
		first := u.Err()
		assertWSUpstreamStaticError(t, first)
		for range 16 {
			r := wsUpstreamHTTPRequest(t, srv)
			r.URL.Path = "/synthetic-private"
			sendWSUpstreamHTTP(t, r)
			if u.Err() != first {
				t.Fatal("迟到错误覆盖了有界首次错误槽")
			}
		}
		if u.Close() != first || u.Close() != first {
			t.Fatal("已领取连接的迟到错误没有汇总到关闭结果")
		}
	})
}

// 漏关未领取槽、Close 后还能接管或在锁内关 socket 会导致泄漏与竞态悬停。
func TestWSUpstreamClose(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		name := "unclaimed"
		if claimed {
			name = "claimed"
		}
		t.Run(name, func(t *testing.T) {
			u, srv := newWSUpstreamTest(t, wsUpstreamFixture(), DefaultWSLimits())
			peer, _ := dialWSUpstreamTest(t, srv, wsUpstreamTestHeaders())
			if claimed {
				claimWSUpstreamTest(t, u)
			} else {
				// 先等连接真正入槽，防止 unclaimed 分支偶然只测尚未完成的 Accept。
				waitWSUpstreamSignal(t, u.ready)
				if u.Err() != nil {
					t.Fatal("未领取连接就绪前已有错配")
				}
			}
			if u.Close() != nil || u.Close() != nil {
				t.Fatal("正常关闭失败")
			}
			if result := readWSUpstreamTest(t, peer); result.err == nil {
				t.Fatal("关闭没有释放回放端拥有的 socket")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if conn, err := u.Connection(ctx); conn != nil || err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("关闭后仍交付连接或等待未唤醒")
			}
		})
	}
	t.Run("close_wakes_wait_and_prevents_upgrade", func(t *testing.T) {
		u, srv := newWSUpstreamTest(t, wsUpstreamFixture(), DefaultWSLimits())
		result := make(chan error, 1)
		go func() { _, err := u.Connection(context.Background()); result <- err }()
		if u.Close() != nil {
			t.Fatal("空槽关闭失败")
		}
		assertWSUpstreamStaticError(t, awaitWSUpstreamError(t, result))
		resp := sendWSUpstreamHTTP(t, wsUpstreamHTTPRequest(t, srv))
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Sec-WebSocket-Accept") != "" {
			t.Fatal("Close 后的新握手被接管")
		}
		assertWSUpstreamStaticError(t, u.Err())
	})
}

// 阻塞点只包裹真实 TCP 的 Hijack/Write，不以 net.Pipe 或假的 Conn 掩盖所有权漏洞。
func TestWSUpstreamAcceptRaces(t *testing.T) {
	for _, stage := range []string{"before_hijack", "after_101"} {
		t.Run(stage, func(t *testing.T) {
			u, err := NewWSReplayUpstream(wsUpstreamFixture(), DefaultWSLimits())
			if err != nil {
				t.Fatal("合法上游构造失败")
			}
			gate := newWSUpstreamGate(stage)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(gate.finished)
				u.ServeHTTP(&wsUpstreamGatedWriter{ResponseWriter: w, gate: gate}, r)
			}))
			t.Cleanup(func() { gate.releaseOnce(); _ = u.Close(); srv.Close() })
			result := make(chan wsUpstreamClaimResult, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				peer, _, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/realtime?model=synthetic-model&tag=a&tag=a&tag=b", ws.DialOptions{Header: wsUpstreamTestHeaders()})
				result <- wsUpstreamClaimResult{peer, err}
			}()
			waitWSUpstreamSignal(t, gate.entered)
			closed := make(chan error, 1)
			go func() { closed <- u.Close() }()
			if awaitWSUpstreamError(t, closed) != nil {
				t.Fatal("Accept 竞争中的关闭失败")
			}
			gate.releaseOnce()
			waitWSUpstreamSignal(t, gate.finished)
			select {
			case got := <-result:
				if stage == "before_hijack" {
					if got.conn != nil || got.err == nil {
						t.Fatal("已关闭回放端仍在晚到 Hijack 中发出 101")
					}
				} else {
					if got.conn == nil || got.err != nil {
						t.Fatal("竞争测试未建立预定的真实 TCP 升级连接")
					}
					t.Cleanup(func() { _ = got.conn.Close(ws.CloseNormal, "") })
					if readWSUpstreamTest(t, got.conn).err == nil {
						t.Fatal("Accept 晚到的已升级 socket 泄漏")
					}
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Accept 竞争未退出")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if conn, err := u.Connection(ctx); conn != nil || err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("晚到连接进入了已关闭连接槽")
			}
		})
	}
}

// 第一条握手尚未完成时也要预占唯一名额，不能让两个 Accept 同时成为槽内连接。
func TestWSUpstreamDuplicateDuringAccept(t *testing.T) {
	u, err := NewWSReplayUpstream(wsUpstreamFixture(), DefaultWSLimits())
	if err != nil {
		t.Fatal("合法上游构造失败")
	}
	gate := newWSUpstreamGate("before_hijack")
	var first sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gated := false
		first.Do(func() { gated = true })
		if gated {
			defer close(gate.finished)
			u.ServeHTTP(&wsUpstreamGatedWriter{ResponseWriter: w, gate: gate}, r)
		} else {
			u.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(func() { gate.releaseOnce(); _ = u.Close(); srv.Close() })
	result := make(chan wsUpstreamClaimResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		peer, _, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/realtime?model=synthetic-model&tag=a&tag=a&tag=b", ws.DialOptions{Header: wsUpstreamTestHeaders()})
		result <- wsUpstreamClaimResult{peer, err}
	}()
	waitWSUpstreamSignal(t, gate.entered)
	resp := sendWSUpstreamHTTP(t, wsUpstreamHTTPRequest(t, srv))
	if resp.StatusCode != 409 {
		t.Fatal("进行中的 Accept 未排除重复握手")
	}
	firstErr := u.Err()
	assertWSUpstreamStaticError(t, firstErr)
	gate.releaseOnce()
	waitWSUpstreamSignal(t, gate.finished)
	select {
	case got := <-result:
		if got.conn != nil {
			t.Cleanup(func() { _ = got.conn.Close(ws.CloseNormal, "") })
			if readWSUpstreamTest(t, got.conn).err == nil {
				t.Fatal("已错配会话留下未领取的升级连接")
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("重复握手竞争未退出")
	}
	if conn, err := u.Connection(context.Background()); conn != nil || err != firstErr || u.Close() != firstErr {
		t.Fatal("Accept 晚到错误覆盖了重复连接断言")
	}
}

// 不暴露 Hijacker 的真实本地 handler 必须给出静态 Accept 错误而不是悬停。
func TestWSUpstreamAcceptFailure(t *testing.T) {
	u, err := NewWSReplayUpstream(wsUpstreamFixture(), DefaultWSLimits())
	if err != nil {
		t.Fatal("合法上游构造失败")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.ServeHTTP(struct{ http.ResponseWriter }{w}, r)
	}))
	t.Cleanup(func() { _ = u.Close(); srv.Close() })
	resp := sendWSUpstreamHTTP(t, wsUpstreamHTTPRequest(t, srv))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatal("Accept 失败未拒绝请求")
	}
	first := u.Err()
	assertWSUpstreamStaticError(t, first)
	if conn, err := u.Connection(context.Background()); conn != nil || err != first || u.Close() != first {
		t.Fatal("Accept 错误未保留或未唤醒领取者")
	}
}

// 错把默认生产预算传给 Accept 会使注入的小消息预算失效。
func TestWSUpstreamMessageLimit(t *testing.T) {
	limits := DefaultWSLimits()
	limits.MessageBytes = 128
	u, srv := newWSUpstreamTest(t, wsUpstreamFixture(), limits)
	peer, _ := dialWSUpstreamTest(t, srv, wsUpstreamTestHeaders())
	conn := claimWSUpstreamTest(t, u)
	if err := peer.WriteMessage(ws.OpBinary, make([]byte, 129)); err != nil {
		t.Fatal("合成超限消息发送失败")
	}
	if readWSUpstreamTest(t, conn).err == nil {
		t.Fatal("Accept 未采用显式完整消息预算")
	}
}

// 不得把 fixture 总回放预算套到 Conn 的 idle；总期限由后续驱动独立控制。
func TestWSUpstreamNoIdleDeadline(t *testing.T) {
	limits := DefaultWSLimits()
	limits.Replay = time.Millisecond
	u, srv := newWSUpstreamTest(t, wsUpstreamFixture(), limits)
	peer, _ := dialWSUpstreamTest(t, srv, wsUpstreamTestHeaders())
	conn := claimWSUpstreamTest(t, u)
	result := make(chan wsUpstreamReadResult, 1)
	go func() { op, payload, err := conn.ReadMessage(); result <- wsUpstreamReadResult{op, payload, err} }()
	select {
	case <-result:
		t.Fatal("总回放预算被错误地用作连接空闲期限")
	case <-time.After(30 * time.Millisecond):
	}
	if err := peer.WriteMessage(ws.OpText, []byte("synthetic-late-message")); err != nil {
		t.Fatal("延迟的本地消息发送失败")
	}
	select {
	case got := <-result:
		if got.err != nil || string(got.payload) != "synthetic-late-message" {
			t.Fatal("无空闲期限的连接未保留延迟消息")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("延迟的本地消息读取悬停")
	}
}

// 构造后的输入改写不能改变握手预期或错误对象，防止调用方共享可变 fixture。
func TestWSUpstreamSnapshots(t *testing.T) {
	t.Run("handshake", func(t *testing.T) {
		f := wsUpstreamFixture()
		u, srv := newWSUpstreamTest(t, f, DefaultWSLimits())
		f.Response.WS.Upstream.Path = "/synthetic-private"
		f.Response.WS.Upstream.Query = "synthetic-private=value"
		f.Response.WS.Upstream.Headers["X-Safe"] = "synthetic-private"
		f.Response.WS.UpstreamExpectedStatus = 401
		dialWSUpstreamTest(t, srv, wsUpstreamTestHeaders())
		claimWSUpstreamTest(t, u)
		if u.Err() != nil {
			t.Fatal("构造后的 fixture 改写污染了握手快照")
		}
	})
	t.Run("error_body", func(t *testing.T) {
		f := wsUpstreamFixture()
		f.Response.Status = 401
		f.Response.WS.UpstreamExpectedStatus = 401
		f.Response.WS.UpstreamError = json.RawMessage(`{"error":false}`)
		f.Response.WS.Nodes = nil
		f.Response.WS.Coverage = nil
		f.Response.WS.Outcome = WSOutcome{Kind: "handshake_failed"}
		_, srv := newWSUpstreamTest(t, f, DefaultWSLimits())
		copy(f.Response.WS.UpstreamError, `{"error":null} `)
		resp := sendWSUpstreamHTTP(t, wsUpstreamHTTPRequest(t, srv))
		body, err := io.ReadAll(resp.Body)
		if err != nil || string(body) != `{"error":false}` {
			t.Fatal("构造后的错误对象改写污染了回放快照")
		}
	})
}

func wsUpstreamFixture() Fixture {
	f := syntheticEnvelope()
	f.Response.WS.Upstream.Query = "model=synthetic-model&tag=a&tag=a&tag=b"
	f.Response.WS.Upstream.Headers = map[string]string{"Authorization": "<redacted>", "X-Safe": "version-1"}
	return f
}

func newWSUpstreamTest(t *testing.T, f Fixture, limits WSLimits) (*WSReplayUpstream, *httptest.Server) {
	t.Helper()
	u, err := NewWSReplayUpstream(f, limits)
	if err != nil {
		t.Fatal("合法上游构造失败")
	}
	srv := httptest.NewServer(u)
	t.Cleanup(func() { _ = u.Close(); srv.Close() })
	return u, srv
}

func wsUpstreamTestHeaders() http.Header {
	return http.Header{"Authorization": {"Bearer synthetic-outbound"}, "X-Safe": {"version-1"}}
}

func wsUpstreamHTTPRequest(t *testing.T, srv *httptest.Server) *http.Request {
	t.Helper()
	r, err := http.NewRequest("GET", srv.URL+"/v1/realtime?tag=b&model=synthetic-model&tag=a&tag=a", nil)
	if err != nil {
		t.Fatal("本地 HTTP 请求构造失败")
	}
	r.Header = wsUpstreamTestHeaders()
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "AAECAwQFBgcICQoLDA0ODw==")
	return r
}

func sendWSUpstreamHTTP(t *testing.T, r *http.Request) *http.Response {
	t.Helper()
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal("本地 HTTP 请求失败")
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func dialWSUpstreamTest(t *testing.T, srv *httptest.Server, headers http.Header) (*ws.Conn, *http.Response) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, resp, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/realtime?tag=b&tag=a&model=synthetic-model&tag=a&token=synthetic-private", ws.DialOptions{Header: headers})
	if err != nil || conn == nil || resp == nil {
		t.Fatal("本地 WS 拨号未成功升级")
	}
	t.Cleanup(func() { _ = conn.Close(ws.CloseNormal, "") })
	return conn, resp
}

func claimWSUpstreamTest(t *testing.T, u *WSReplayUpstream) *ws.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := u.Connection(ctx)
	if err != nil || conn == nil {
		t.Fatal("本地 WS 连接领取失败")
	}
	return conn
}

type wsUpstreamClaimResult struct {
	conn *ws.Conn
	err  error
}

type wsUpstreamReadResult struct {
	op      ws.Opcode
	payload []byte
	err     error
}

func readWSUpstreamTest(t *testing.T, conn *ws.Conn) wsUpstreamReadResult {
	t.Helper()
	result := make(chan wsUpstreamReadResult, 1)
	go func() { op, payload, err := conn.ReadMessage(); result <- wsUpstreamReadResult{op, payload, err} }()
	select {
	case got := <-result:
		return got
	case <-time.After(2 * time.Second):
		_ = conn.Close(ws.CloseNormal, "")
		t.Fatal("本地 WS 读取未在测试期限内退出")
		return wsUpstreamReadResult{}
	}
}

func awaitWSUpstreamError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("连接生命周期操作悬停")
		return nil
	}
}

func waitWSUpstreamSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("真实 TCP 竞争没有到达预定边界")
	}
}

func assertWSUpstreamStaticError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("缺少预期回放错误")
	}
	for _, marker := range []string{"synthetic-", "Bearer", "%zz", "vendor.", "AAECAwQF", "/v1/", "model="} {
		if strings.Contains(err.Error(), marker) {
			t.Fatal("回放错误泄漏了握手输入内容")
		}
	}
}

type wsUpstreamGate struct {
	stage    string
	entered  chan struct{}
	release  chan struct{}
	finished chan struct{}
	once     sync.Once
	written  sync.Once
}

func newWSUpstreamGate(stage string) *wsUpstreamGate {
	return &wsUpstreamGate{stage: stage, entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
}

func (g *wsUpstreamGate) releaseOnce() { g.once.Do(func() { close(g.release) }) }

type wsUpstreamGatedWriter struct {
	http.ResponseWriter
	gate *wsUpstreamGate
}

func (w *wsUpstreamGatedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.gate.stage == "before_hijack" {
		close(w.gate.entered)
		<-w.gate.release
	}
	conn, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil && w.gate.stage == "after_101" {
		conn = &wsUpstreamGatedConn{Conn: conn, gate: w.gate}
		rw.Writer = bufio.NewWriter(conn)
	}
	return conn, rw, err
}

type wsUpstreamGatedConn struct {
	net.Conn
	gate *wsUpstreamGate
}

func (c *wsUpstreamGatedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.gate.written.Do(func() {
		close(c.gate.entered)
		<-c.gate.release
	})
	return n, err
}
