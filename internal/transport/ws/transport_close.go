package ws

import (
	"crypto/tls"
	"net"
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

// 在 WS close 写入前启动同一个绝对期限守卫，不能到 TLS 收尾时重新给一份预算。
// 调用者同步执行写入和 Close，结束后必须 join；每次唯一关闭只拥有这一个工作者。
func watchTransportClose(conn net.Conn, deadline time.Time) (join func()) {
	timer := time.NewTimer(time.Until(deadline))
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer timer.Stop()
		select {
		case <-timer.C:
			_ = abortTransport(conn)
		case <-stop:
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}
