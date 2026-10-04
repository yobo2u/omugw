package ws

import (
	"crypto/tls"
	"net"
	"sync"
	"time"
)

// 取消与失败释放不能调用 tls.Conn.Close：它会发送 close_notify，并自行设置
// 五秒写期限。保留 NetConn 的强制关闭权，才能中止读写或正在进行的 TLS 收尾。
func abortTransport(conn net.Conn) error {
	for {
		tlsConn, ok := conn.(*tls.Conn)
		if !ok {
			return conn.Close()
		}
		conn = tlsConn.NetConn()
	}
}

type transportCloseGuard struct {
	changed, stop, done chan struct{}
	stopOnce            sync.Once
}

// 多个所有者都等待同一个 done；只发停止信号不能冒充物理中止已完成。
func (g *transportCloseGuard) finish() {
	g.stopOnce.Do(func() { close(g.stop) })
	<-g.done
}

// 必须经 Arm 在 closeMu 内创建一次；期限更新只唤醒原工作者，不能为每次收紧再起守卫。
// 守卫直达 TCP，TLS close_notify 自设的五秒期限也不能让收尾续命。
func (c *Conn) watchTransportClose() *transportCloseGuard {
	g := &transportCloseGuard{changed: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	timer := time.NewTimer(time.Until(*c.closeDeadline.Load()))
	go func() {
		defer close(g.done)
		defer timer.Stop()
		defer func() {
			c.closed.Store(true)
			_ = abortTransport(c.conn)
		}()
		for {
			select {
			case <-timer.C:
				return
			case <-g.stop:
				return
			case <-g.changed:
				timer.Reset(time.Until(*c.closeDeadline.Load()))
			}
		}
	}()
	return g
}
