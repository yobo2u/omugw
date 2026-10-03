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
	"sync/atomic"
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

	// HandshakeDeadline 只约束 101 写入；交付连接前清除，避免截断长会话。
	HandshakeDeadline time.Time

	// WriteTimeout 限制业务帧与心跳写，0 保持原有合法调用的无期限行为。
	WriteTimeout time.Duration
	// Budget 共享 payload 容量；非 nil 时只能通过 ReadOwnedMessage 取得所有权。
	Budget *BufferBudget
}

// Accept 把一个 HTTP 升级请求接管成 WebSocket 连接。
//
// 接管之后 net/http 不再管这条连接——它既不会写响应，也不会在 handler
// 返回时关闭它。因此这里必须自己写完 101 响应，调用方必须自己 Close。
func Accept(w http.ResponseWriter, r *http.Request, opts AcceptOptions) (*Conn, error) {
	if opts.MaxPayload < 0 || opts.Idle < 0 || opts.WriteTimeout < 0 {
		http.Error(w, "invalid websocket options", http.StatusInternalServerError)
		return nil, fmt.Errorf("%w: 负限额或负时长非法", ErrHandshake)
	}
	if err := ValidateUpgrade(r); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errUpgradeVersion) {
			// 拒绝版本时仍告知支持值，不能回显未经验证的客户端字段。
			w.Header().Set("Sec-WebSocket-Version", "13")
			status = http.StatusUpgradeRequired
		}
		http.Error(w, "invalid websocket upgrade", status)
		return nil, err
	}
	key := r.Header.Get("Sec-WebSocket-Key")

	hj, ok := w.(http.ResponseWriter).(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return nil, fmt.Errorf("%w: ResponseWriter 不支持 Hijack", ErrHandshake)
	}

	netConn, brw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("%w: 接管连接失败", ErrHandshake)
	}
	if err := netConn.SetWriteDeadline(opts.HandshakeDeadline); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("%w: 设置 101 写期限失败", ErrHandshake)
	}

	// 手写 101：接管之后 net/http 的 WriteHeader 不再有效。
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"
	if _, err := brw.WriteString(resp); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("%w: 写 101 响应失败", ErrHandshake)
	}
	if err := brw.Flush(); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("%w: 刷出 101 响应失败", ErrHandshake)
	}
	if err := netConn.SetDeadline(time.Time{}); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("%w: 清除握手期限失败", ErrHandshake)
	}

	// 用 brw.Reader 而不是裸 netConn：握手期间 bufio 可能已经预读了
	// 客户端紧跟着发来的帧字节。丢掉它等于丢掉客户端的第一条消息。
	c := newConnBuffered(netConn, brw.Reader, RoleServer, opts.MaxPayload, opts.Idle)
	c.writeTimeout, c.budget = opts.WriteTimeout, opts.Budget
	return c, nil
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

	// ConnectTimeout 只覆盖 TCP/TLS，0 沿用 ctx；不能侵占后续握手和业务阶段。
	ConnectTimeout time.Duration

	// MaxHandshakeBytes 限制状态行与 HTTP 头，0 默认 64 KiB，不限制 WS 帧。
	MaxHandshakeBytes int64

	// MaxErrorBodyBytes 限制失败响应体，0 默认 64 KiB；返回的内存 Body 归调用方。
	MaxErrorBodyBytes int64

	// WriteTimeout 只在写锁内设置和清除，不能泄漏到后续会话阶段。
	WriteTimeout time.Duration
	// Budget 防止多个会话同时持有的大消息越过全局容量。
	Budget *BufferBudget
}

// Dial 向 ws:// 或 wss:// 端点发起握手。
//
// 返回的 *http.Response 在失败时也非 nil（只要拿到了响应），因为状态码
// 往往就是唯一的线索：DashScope 对错误的 API Key 返回 401 而不是 101，
// 吞掉它会让运维只看到「连不上」。
func Dial(ctx context.Context, rawURL string, opts DialOptions) (*Conn, *http.Response, error) {
	if opts.ConnectTimeout < 0 || opts.Idle < 0 || opts.MaxPayload < 0 || opts.MaxHandshakeBytes < 0 || opts.MaxErrorBodyBytes < 0 || opts.WriteTimeout < 0 {
		return nil, nil, fmt.Errorf("%w: 负限额或负时长非法", ErrHandshake)
	}
	if opts.MaxHandshakeBytes == 0 {
		opts.MaxHandshakeBytes = 64 << 10
	}
	if opts.MaxErrorBodyBytes == 0 {
		opts.MaxErrorBodyBytes = 64 << 10
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: URL 非法", ErrHandshake)
	}

	var secure bool
	switch u.Scheme {
	case "ws":
	case "wss":
		secure = true
	default:
		return nil, nil, fmt.Errorf("%w: scheme 不是 ws 或 wss", ErrHandshake)
	}

	connectCtx := ctx
	cancelConnect := func() {}
	if opts.ConnectTimeout > 0 {
		connectCtx, cancelConnect = context.WithTimeout(ctx, opts.ConnectTimeout)
	}
	netConn, err := dialTCP(connectCtx, u, secure, opts.TLS)
	// 子预算在 TCP/TLS 完成后立即释放；后面的 CAS watchdog 仍只属于原握手 ctx。
	cancelConnect()
	if err != nil {
		return nil, nil, err
	}

	// 拨号成功后取消仍须唤醒握手读写，但不能把关闭权带进业务会话。
	// 同一个 CAS 仲裁取消与交接：取消先认领就必须报错，交接先认领后
	// 即使 watchdog 迟到且两个通道都 ready，也不再允许它关闭 socket。
	var handshakeClaimed atomic.Bool
	handshakeDone := make(chan struct{})
	watchdogDone := make(chan struct{})
	defer func() {
		close(handshakeDone)
		<-watchdogDone
	}()
	go func() {
		defer close(watchdogDone)
		select {
		case <-ctx.Done():
			if handshakeClaimed.CompareAndSwap(false, true) {
				_ = netConn.Close()
			}
		case <-handshakeDone:
		}
	}()

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		_ = netConn.Close()
		return nil, nil, fmt.Errorf("%w: 生成 Sec-WebSocket-Key 失败", ErrHandshake)
	}
	clientKey := base64.StdEncoding.EncodeToString(nonce[:])

	if err := writeHandshakeRequest(netConn, u, clientKey, opts.Header); err != nil {
		_ = netConn.Close()
		return nil, nil, err
	}

	// 限额只在 HTTP 解析时生效；保留同一个 bufio 再解除底层限额，既不丢
	// 预读首帧，也不把握手预算误用成后续整个 WebSocket 字节流的配额。
	source := &struct{ io.Reader }{io.LimitReader(netConn, opts.MaxHandshakeBytes)}
	br := bufio.NewReader(source)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = netConn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrHandshake, ctxErr)
		}
		return nil, nil, fmt.Errorf("%w: 握手响应不完整、非法或头超限", ErrHandshake)
	}
	source.Reader = netConn

	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, resp, retainHandshakeErrorBody(ctx, netConn, resp, opts.MaxErrorBodyBytes)
	}

	// 校验摘要：一个返回 101 却算错摘要的中间设备，会让我们把帧写进一个
	// 根本不解析它的端点——表现是「连上了但一条消息都收不到」。
	if err := validateUpgradeResponse(resp, clientKey); err != nil {
		_ = netConn.Close()
		return nil, resp, err
	}

	// 取消已取得关闭权时，101 与摘要校验通过也不能交付一条即将失效的连接。
	// defer join 确保真正关完 socket 或放弃关闭权之后，调用方才会收到结果。
	if !handshakeClaimed.CompareAndSwap(false, true) {
		return nil, resp, fmt.Errorf("%w: %v", ErrHandshake, ctx.Err())
	}
	c := newConnBuffered(netConn, br, RoleClient, opts.MaxPayload, opts.Idle)
	c.writeTimeout, c.budget = opts.WriteTimeout, opts.Budget
	return c, resp, nil
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
		return nil, fmt.Errorf("%w: TCP 连接失败", ErrHandshake)
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
		return nil, fmt.Errorf("%w: TLS 握手失败", ErrHandshake)
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
		return fmt.Errorf("%w: 写握手请求失败", ErrHandshake)
	}
	return nil
}

func isReservedHandshakeHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Upgrade", "Connection", "Sec-Websocket-Key", "Sec-Websocket-Version", "Host",
		"Sec-Websocket-Extensions", "Sec-Websocket-Protocol":
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
