package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrHandshake 表示握手未能完成。
//
// 与帧层的 ErrProtocol 分开：握手失败多半是配置或鉴权问题（错的密钥、
// 打错端点、被中间设备拦截），而帧层协议错误说明连上之后对端实现有问题。
// 两者的排查方向完全不同，混成一个错误会让运维从最贵的方向查起。
var ErrHandshake = errors.New("ws: 握手失败")

// magicGUID 是 RFC 6455 §1.3 规定的固定串，参与 Sec-WebSocket-Accept 摘要。
const magicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// acceptKey 按 RFC 6455 §4.2.2 计算握手应答摘要。
func acceptKey(clientKey string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, clientKey+magicGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// AcceptOptions 是服务端接受握手的选项。
type AcceptOptions struct {
	// MaxPayload 是单条消息重组后的负载上限。
	MaxPayload int64

	// Idle 是两帧之间的最大间隔，0 表示不设空闲超时。
	Idle time.Duration
}

// Accept 把一个 HTTP 升级请求接管成 WebSocket 连接。
//
// 接管之后 net/http 不再管这条连接——它既不会写响应，也不会在 handler
// 返回时关闭它。因此这里必须自己写完 101 响应，调用方必须自己 Close。
func Accept(w http.ResponseWriter, r *http.Request, opts AcceptOptions) (*Conn, error) {
	if !headerContainsToken(r.Header, "Connection", "upgrade") ||
		!strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "expected websocket upgrade", http.StatusBadRequest)
		return nil, fmt.Errorf("%w: 不是 WebSocket 升级请求", ErrHandshake)
	}

	if v := r.Header.Get("Sec-WebSocket-Version"); v != "13" {
		// RFC 6455 §4.2.2 要求在拒绝时告知支持的版本，否则客户端只能盲试。
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusUpgradeRequired)
		return nil, fmt.Errorf("%w: 不支持的版本 %q", ErrHandshake, v)
	}

	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, fmt.Errorf("%w: 缺少 Sec-WebSocket-Key", ErrHandshake)
	}

	hj, ok := w.(http.ResponseWriter).(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return nil, fmt.Errorf("%w: ResponseWriter 不支持 Hijack", ErrHandshake)
	}

	netConn, brw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("%w: 接管连接失败: %v", ErrHandshake, err)
	}

	// 手写 101：接管之后 net/http 的 WriteHeader 不再有效。
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"
	if _, err := brw.WriteString(resp); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("%w: 写 101 响应失败: %v", ErrHandshake, err)
	}
	if err := brw.Flush(); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("%w: 刷出 101 响应失败: %v", ErrHandshake, err)
	}

	// 用 brw.Reader 而不是裸 netConn：握手期间 bufio 可能已经预读了
	// 客户端紧跟着发来的帧字节。丢掉它等于丢掉客户端的第一条消息。
	return newConnBuffered(netConn, brw.Reader, RoleServer, opts.MaxPayload, opts.Idle), nil
}

// DialOptions 是客户端拨号的选项。
type DialOptions struct {
	// Header 是要带给上游的额外请求头。
	//
	// realtime 的鉴权就发生在握手这一次 HTTP 请求里，之后没有第二次机会——
	// Authorization 必须在这里带上。
	Header http.Header

	// MaxPayload 是单条消息重组后的负载上限。
	MaxPayload int64

	// Idle 是两帧之间的最大间隔，0 表示不设空闲超时。
	Idle time.Duration

	// TLS 用于 wss。为 nil 时用默认配置。
	TLS *tls.Config
}

// Dial 向 ws:// 或 wss:// 端点发起握手。
//
// 返回的 *http.Response 在失败时也非 nil（只要拿到了响应），因为状态码
// 往往就是唯一的线索：DashScope 对错误的 API Key 返回 401 而不是 101，
// 吞掉它会让运维只看到「连不上」。
func Dial(ctx context.Context, rawURL string, opts DialOptions) (*Conn, *http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: URL 非法: %v", ErrHandshake, err)
	}

	var secure bool
	switch u.Scheme {
	case "ws":
	case "wss":
		secure = true
	default:
		return nil, nil, fmt.Errorf("%w: scheme %q 不是 ws 或 wss", ErrHandshake, u.Scheme)
	}

	netConn, err := dialTCP(ctx, u, secure, opts.TLS)
	if err != nil {
		return nil, nil, err
	}

	// 拨号成功后 ctx 仍可能被取消。挂一个 watchdog 关掉连接，让阻塞中的
	// 握手读写立刻返回——否则一个不应答的上游会把调用方挂到 TCP 自己超时。
	handshakeDone := make(chan struct{})
	defer close(handshakeDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = netConn.Close()
		case <-handshakeDone:
		}
	}()

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		_ = netConn.Close()
		return nil, nil, fmt.Errorf("%w: 生成 Sec-WebSocket-Key 失败: %v", ErrHandshake, err)
	}
	clientKey := base64.StdEncoding.EncodeToString(nonce[:])

	if err := writeHandshakeRequest(netConn, u, clientKey, opts.Header); err != nil {
		_ = netConn.Close()
		return nil, nil, err
	}

	br := bufio.NewReader(netConn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = netConn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrHandshake, ctxErr)
		}
		return nil, nil, fmt.Errorf("%w: 读握手响应失败: %v", ErrHandshake, err)
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = netConn.Close()
		return nil, resp, fmt.Errorf("%w: 上游未升级协议，状态码 %d", ErrHandshake, resp.StatusCode)
	}

	// 校验摘要：一个返回 101 却算错摘要的中间设备，会让我们把帧写进一个
	// 根本不解析它的端点——表现是「连上了但一条消息都收不到」。
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != acceptKey(clientKey) {
		_ = netConn.Close()
		return nil, resp, fmt.Errorf("%w: Sec-WebSocket-Accept 摘要不匹配", ErrHandshake)
	}

	return newConnBuffered(netConn, br, RoleClient, opts.MaxPayload, opts.Idle), resp, nil
}

func dialTCP(ctx context.Context, u *url.URL, secure bool, tlsCfg *tls.Config) (net.Conn, error) {
	host := u.Host
	if u.Port() == "" {
		if secure {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}

	d := &net.Dialer{}
	netConn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("%w: 连接 %s 失败: %v", ErrHandshake, host, err)
	}
	if !secure {
		return netConn, nil
	}

	cfg := tlsCfg
	if cfg == nil {
		cfg = &tls.Config{}
	}
	if cfg.ServerName == "" {
		cfg = cfg.Clone()
		cfg.ServerName = u.Hostname()
	}

	tlsConn := tls.Client(netConn, cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("%w: TLS 握手失败: %v", ErrHandshake, err)
	}
	return tlsConn, nil
}

func writeHandshakeRequest(w io.Writer, u *url.URL, clientKey string, extra http.Header) error {
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&b, "Host: %s\r\n", u.Host)
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&b, "Sec-WebSocket-Key: %s\r\n", clientKey)
	b.WriteString("Sec-WebSocket-Version: 13\r\n")

	for name, values := range extra {
		// 握手必填头由上面统一写出，不许被覆盖——重复的 Upgrade 或
		// 两把不同的 Key 会让服务端直接拒绝，而错误信息通常语焉不详。
		if isReservedHandshakeHeader(name) {
			continue
		}
		for _, v := range values {
			fmt.Fprintf(&b, "%s: %s\r\n", name, v)
		}
	}
	b.WriteString("\r\n")

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("%w: 写握手请求失败: %v", ErrHandshake, err)
	}
	return nil
}

func isReservedHandshakeHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Upgrade", "Connection", "Sec-Websocket-Key", "Sec-Websocket-Version", "Host":
		return true
	default:
		return false
	}
}

// headerContainsToken 按逗号分隔比对 token，大小写不敏感。
//
// Connection 头可以是 "keep-alive, Upgrade" 这样的列表，直接做等值比较
// 会漏判——而漏判的后果是把一个合法的升级请求当成普通 GET 拒掉。
func headerContainsToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}
