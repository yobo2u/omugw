package gateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// Accept 尚未交付 Conn 的窗口也必须响应 pending 取消。绝对写期限限制总预算，
// 这个 watchdog 只打断已 Hijack 的握手 IO，交付前撤销并 join，不能侵入业务会话。
func acceptWS(ctx context.Context, w http.ResponseWriter, r *http.Request, opts ws.AcceptOptions) (*ws.Conn, error) {
	watched := &wsAcceptWriter{ResponseWriter: w, ctx: ctx}
	c, err := ws.Accept(watched, r, opts)
	if watched.conn != nil {
		if cancelErr := watched.conn.finish(); cancelErr != nil {
			err = cancelErr
		}
	}
	return c, err
}

type wsAcceptWriter struct {
	http.ResponseWriter
	ctx  context.Context
	conn *wsHandshakeConn
}

func (w *wsAcceptWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return nil, nil, err
	}
	n := &wsHandshakeConn{Conn: c, ctx: w.ctx, pending: true, joined: make(chan struct{})}
	n.stop = context.AfterFunc(w.ctx, func() {
		defer close(n.joined)
		n.mu.Lock()
		defer n.mu.Unlock()
		if n.pending {
			_ = n.Conn.SetDeadline(time.Now())
		}
	})
	w.conn = n
	// 保留 brw 的读写缓存；101 写仍使用同一个底层连接的期限。
	return n, brw, nil
}

type wsHandshakeConn struct {
	net.Conn
	mu      sync.Mutex
	ctx     context.Context
	pending bool
	stop    func() bool
	joined  chan struct{}
}

// 包装不能遮蔽 transport 对 TLS 底层的强制关闭权。WS close 已由 Conn 发出，
// 此处必须直达 TCP，避免 tls.Close 的 close_notify 重新申请五秒写期限。
func (c *wsHandshakeConn) Close() error {
	underlying := c.Conn
	for {
		secure, ok := underlying.(*tls.Conn)
		if !ok {
			return underlying.Close()
		}
		underlying = secure.NetConn()
	}
}

func (c *wsHandshakeConn) SetWriteDeadline(d time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending && c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	return c.Conn.SetWriteDeadline(d)
}

func (c *wsHandshakeConn) SetDeadline(d time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending && c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	return c.Conn.SetDeadline(d)
}

func (c *wsHandshakeConn) finish() error {
	c.mu.Lock()
	err := c.ctx.Err()
	c.pending, c.ctx = false, nil
	c.mu.Unlock()
	if !c.stop() {
		<-c.joined
	}
	c.stop = nil
	return err
}
