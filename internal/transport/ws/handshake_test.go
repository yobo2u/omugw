package ws

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// echoServer 起一个把收到的消息原样回送的 WebSocket 服务端。
//
// 用真实 httptest.Server 而不是造假连接：Accept 要验证的正是它与
// net/http 的接管协作——Hijack 之后 net/http 不再管这条连接，
// 这个交接点用假对象测不出来。
func echoServer(t *testing.T, onConn func(*Conn)) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Accept(w, r, AcceptOptions{
			MaxPayload: maxTestPayload,
			Idle:       2 * time.Second,
		})
		if err != nil {
			return
		}
		defer conn.Close(CloseNormal, "")
		onConn(conn)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// TestDialAndAcceptInteroperate 是这一层的主干：自己的客户端要能连上
// 自己的服务端，且消息双向通得过。
//
// 两侧都是自己写的，所以这条测不出「与别人互通」——那由 frame 层的
// 精确线格式断言与真实上游探针分别覆盖。这里证的是握手与帧层接得上。
func TestDialAndAcceptInteroperate(t *testing.T) {
	srv := echoServer(t, func(c *Conn) {
		op, payload, err := c.ReadMessage()
		if err != nil {
			return
		}
		_ = c.WriteMessage(op, payload)
	})

	client, resp, err := Dial(context.Background(), wsURL(t, srv), DialOptions{
		MaxPayload: maxTestPayload,
		Idle:       2 * time.Second,
	})
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer client.Close(CloseNormal, "")

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("状态码 = %d，期望 101", resp.StatusCode)
	}

	if err := client.WriteMessage(OpText, []byte("ping-payload")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	op, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if op != OpText || string(payload) != "ping-payload" {
		t.Fatalf("回声不符: opcode=%v payload=%q", op, payload)
	}
}

// TestDialSendsRequiredHandshakeHeaders 钉住客户端握手的必填头。
//
// 缺任何一个，合规的服务端都会拒绝升级。而拒绝时给出的往往是一个笼统的
// 400，从现象倒推回「少了哪个头」要花很久。
func TestDialSendsRequiredHandshakeHeaders(t *testing.T) {
	got := make(chan http.Header, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		conn, err := Accept(w, r, AcceptOptions{MaxPayload: maxTestPayload})
		if err != nil {
			return
		}
		_ = conn.Close(CloseNormal, "")
	}))
	defer srv.Close()

	client, _, err := Dial(context.Background(), wsURL(t, srv), DialOptions{
		MaxPayload: maxTestPayload,
	})
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer client.Close(CloseNormal, "")

	h := <-got
	if !strings.EqualFold(h.Get("Upgrade"), "websocket") {
		t.Errorf("Upgrade = %q，期望 websocket", h.Get("Upgrade"))
	}
	if !strings.Contains(strings.ToLower(h.Get("Connection")), "upgrade") {
		t.Errorf("Connection = %q，应含 Upgrade", h.Get("Connection"))
	}
	if h.Get("Sec-WebSocket-Version") != "13" {
		t.Errorf("版本 = %q，期望 13", h.Get("Sec-WebSocket-Version"))
	}
	if h.Get("Sec-WebSocket-Key") == "" {
		t.Error("缺少 Sec-WebSocket-Key")
	}
}

// TestDialForwardsCustomHeaders 覆盖鉴权头透传。
//
// 网关对上游是客户端，必须能带上自己的 Authorization——realtime 的鉴权
// 就发生在握手这一次 HTTP 请求里，之后没有第二次机会。
func TestDialForwardsCustomHeaders(t *testing.T) {
	got := make(chan string, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Authorization")
		conn, err := Accept(w, r, AcceptOptions{MaxPayload: maxTestPayload})
		if err != nil {
			return
		}
		_ = conn.Close(CloseNormal, "")
	}))
	defer srv.Close()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer sk-upstream")

	client, _, err := Dial(context.Background(), wsURL(t, srv), DialOptions{
		MaxPayload: maxTestPayload,
		Header:     hdr,
	})
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer client.Close(CloseNormal, "")

	if v := <-got; v != "Bearer sk-upstream" {
		t.Fatalf("Authorization = %q，期望原样透传", v)
	}
}

// TestDialRejectsBadAcceptKey 防的是「连上了一个不懂 WebSocket 的东西」。
//
// Sec-WebSocket-Accept 是服务端对我们那把随机 Key 的 SHA1 应答。不校验它，
// 一个返回 101 却算错摘要的中间设备就能让我们把帧写进一个根本不解析它的
// 端点——表现是「连上了但一条消息都收不到」。
func TestDialRejectsBadAcceptKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()

		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: this-is-not-the-right-digest\r\n\r\n")
		_ = brw.Flush()
	}))
	defer srv.Close()

	_, _, err := Dial(context.Background(), wsURL(t, srv), DialOptions{MaxPayload: maxTestPayload})
	if !errors.Is(err, ErrHandshake) {
		t.Fatalf("错误的 Accept 摘要应当以 ErrHandshake 拒绝，实际: %v", err)
	}
}

// TestDialRejectsNon101 覆盖上游拒绝升级的情形。
//
// 真实场景里这就是鉴权失败：DashScope 对错误的 API Key 返回 401 而不是 101。
// 必须把状态码带出来，否则运维只看到「连不上」，看不出是密钥问题。
func TestDialRejectsNon101(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, resp, err := Dial(context.Background(), wsURL(t, srv), DialOptions{MaxPayload: maxTestPayload})
	if !errors.Is(err, ErrHandshake) {
		t.Fatalf("非 101 应当以 ErrHandshake 拒绝，实际: %v", err)
	}
	if resp == nil {
		t.Fatal("应当把响应交回调用方，否则无从判断是鉴权失败还是别的原因")
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", resp.StatusCode)
	}
}

// TestAcceptRejectsNonUpgradeRequest：普通 GET 不该被当成握手。
func TestAcceptRejectsNonUpgradeRequest(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/realtime", nil)

	if _, err := Accept(rec, req, AcceptOptions{MaxPayload: maxTestPayload}); !errors.Is(err, ErrHandshake) {
		t.Fatalf("非升级请求应当被拒，实际: %v", err)
	}
	if rec.Code == http.StatusSwitchingProtocols {
		t.Error("不得对非升级请求回 101")
	}
}

// TestAcceptRejectsWrongVersion：只支持 RFC 6455 的版本 13。
//
// 按 RFC 6455 §4.2.2，拒绝时要带 Sec-WebSocket-Version 告诉对方我们支持哪版，
// 否则客户端只能盲试。
func TestAcceptRejectsWrongVersion(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/realtime", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "8")

	if _, err := Accept(rec, req, AcceptOptions{MaxPayload: maxTestPayload}); !errors.Is(err, ErrHandshake) {
		t.Fatalf("版本不符应当被拒，实际: %v", err)
	}
	if got := rec.Header().Get("Sec-WebSocket-Version"); got != "13" {
		t.Errorf("拒绝时应告知支持的版本，实际 %q", got)
	}
}

// TestAcceptRejectsMissingKey：缺 Sec-WebSocket-Key 无法算应答摘要。
func TestAcceptRejectsMissingKey(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/realtime", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")

	if _, err := Accept(rec, req, AcceptOptions{MaxPayload: maxTestPayload}); !errors.Is(err, ErrHandshake) {
		t.Fatalf("缺少 Key 应当被拒，实际: %v", err)
	}
}

// TestAcceptKeyMatchesRFCExample 用 RFC 6455 §1.3 的官方样例钉死摘要算法。
//
// 自己的客户端和服务端可以一起算错却互相认可。用规范给的定值比对，
// 才能证明我们与第三方实现互通。
func TestAcceptKeyMatchesRFCExample(t *testing.T) {
	const (
		key  = "dGhlIHNhbXBsZSBub25jZQ=="
		want = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	)
	if got := acceptKey(key); got != want {
		t.Fatalf("acceptKey(%q) = %q，期望 %q", key, got, want)
	}
}

// TestDialRespectsContextCancellation：拨号要能被上层取消。
//
// 没有这条，一个连不上的上游会把 goroutine 挂住直到 TCP 自己超时，
// 而那可能是好几分钟。
func TestDialRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := Dial(ctx, "ws://192.0.2.1:9/realtime", DialOptions{MaxPayload: maxTestPayload})
	if err == nil {
		t.Fatal("已取消的 context 应当让拨号失败")
	}
	if !errors.Is(err, context.Canceled) {
		t.Logf("拨号错误为 %v（可接受，只要不挂住）", err)
	}
}
