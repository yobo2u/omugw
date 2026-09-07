package ws

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// tcpPair 造一对带内核缓冲的本地连接。
//
// 不用 net.Pipe：它完全同步无缓冲，一端写就阻塞到另一端读。而本层要测的
// 恰恰是「读到 ping 时自动回 pong」——那是在读循环里发起一次写，
// 对端此时正准备写下一帧，双方各自阻塞在写上，测试直接死锁。
// 那个死锁是 net.Pipe 的性质，不是被测代码的问题。
func tcpPair(t *testing.T) (client, server net.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()

	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	server = <-accepted
	if server == nil {
		t.Fatal("接受连接失败")
	}

	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

// pipeConns 造一对已握手的 Conn：一端客户端角色（帧掩码），一端服务端角色。
func pipeConns(t *testing.T, idle time.Duration) (client, server *Conn) {
	t.Helper()

	c, s := tcpPair(t)
	return NewConn(c, RoleClient, maxTestPayload, idle),
		NewConn(s, RoleServer, maxTestPayload, idle)
}

// pipeRaw 造「一端是 Conn，另一端是裸连接」的组合，用于断言线上字节。
//
// 需要裸端是因为有些行为只在线格式上可见：自动回的 pong、主动发的 close 帧。
// 从 Conn 那一侧读不到它们——ReadMessage 刻意把控制帧消化掉了。
func pipeRaw(t *testing.T, idle time.Duration) (client *Conn, raw net.Conn) {
	t.Helper()

	c, s := tcpPair(t)
	return NewConn(c, RoleClient, maxTestPayload, idle), s
}

// TestConnMessageRoundTrip 覆盖双向收发。
func TestConnMessageRoundTrip(t *testing.T) {
	client, server := pipeConns(t, 2*time.Second)

	go func() {
		_ = client.WriteMessage(OpText, []byte(`{"type":"session.update"}`))
	}()

	op, payload, err := server.ReadMessage()
	if err != nil {
		t.Fatalf("服务端读取失败: %v", err)
	}
	if op != OpText || string(payload) != `{"type":"session.update"}` {
		t.Fatalf("收到 opcode=%v payload=%q", op, payload)
	}

	go func() {
		_ = server.WriteMessage(OpBinary, []byte{0x01, 0x02, 0x03})
	}()

	op, payload, err = client.ReadMessage()
	if err != nil {
		t.Fatalf("客户端读取失败: %v", err)
	}
	if op != OpBinary || !bytes.Equal(payload, []byte{0x01, 0x02, 0x03}) {
		t.Fatalf("收到 opcode=%v payload=% x", op, payload)
	}
}

// TestConnMasksByRole 直接查线上字节，确认掩码位随角色而定。
//
// 客户端发出的帧必须掩码（RFC 6455 §5.1）。方向搞反时真实上游会以 1002
// 断开——而本地自测看不出来，因为自己的解码器对掩码与否一视同仁。
func TestConnMasksByRole(t *testing.T) {
	client, raw := pipeRaw(t, 2*time.Second)

	go func() {
		_ = client.WriteMessage(OpText, []byte("hello"))
	}()

	var hdr [2]byte
	if _, err := io.ReadFull(raw, hdr[:]); err != nil {
		t.Fatalf("读取帧头失败: %v", err)
	}
	if hdr[1]&0x80 == 0 {
		t.Error("客户端角色发出的帧必须置 MASK 位")
	}
}

// TestConnAnswersPingAutomatically 是这一层最重要的一条。
//
// ping 必须由传输层自己回 pong，不能交给上层：上层在等一条业务消息时
// 根本不会去读，而对端收不到 pong 就会判定链路已死并断开——
// 那时数据其实还在正常传输。表现是「长会话莫名其妙断连」。
func TestConnAnswersPingAutomatically(t *testing.T) {
	client, raw := pipeRaw(t, 2*time.Second)

	go func() {
		_ = WriteFrame(raw, Frame{FIN: true, Opcode: OpPing, Payload: []byte("hb")}, false)
		_ = WriteFrame(raw, Frame{FIN: true, Opcode: OpText, Payload: []byte("data")}, false)
	}()

	op, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("客户端读取失败: %v", err)
	}
	if op != OpText || string(payload) != "data" {
		t.Fatalf("ping 不应冒泡给调用方，实际 opcode=%v payload=%q", op, payload)
	}

	f, err := ReadFrame(raw, maxTestPayload)
	if err != nil {
		t.Fatalf("未收到 pong: %v", err)
	}
	if f.Opcode != OpPong || string(f.Payload) != "hb" {
		t.Fatalf("pong 应原样带回 ping 负载（§5.5.3），实际 opcode=%v payload=%q",
			f.Opcode, f.Payload)
	}
}

// TestConnIgnoresPong：对端主动发的 pong 不该冒泡给调用方。
func TestConnIgnoresPong(t *testing.T) {
	client, raw := pipeRaw(t, 2*time.Second)

	go func() {
		_ = WriteFrame(raw, Frame{FIN: true, Opcode: OpPong}, false)
		_ = WriteFrame(raw, Frame{FIN: true, Opcode: OpText, Payload: []byte("data")}, false)
	}()

	op, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if op != OpText || string(payload) != "data" {
		t.Fatalf("pong 不应冒泡，实际 opcode=%v payload=%q", op, payload)
	}
}

// TestConnReadReturnsCloseError 固化：收到 close 帧要变成带状态码的错误。
//
// 实测中 DashScope 用 1011 + "To many requests..." 表达过载。把它当成普通
// EOF 会让网关以为上游正常收尾从而不重试——而这恰恰最该换一个凭据重试。
func TestConnReadReturnsCloseError(t *testing.T) {
	client, raw := pipeRaw(t, 2*time.Second)

	go func() {
		_ = WriteFrame(raw, Frame{
			FIN: true, Opcode: OpClose, Payload: EncodeClosePayload(1011, "throttled"),
		}, false)
	}()

	_, _, err := client.ReadMessage()

	var ce *CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("应当返回 *CloseError，实际: %v", err)
	}
	if ce.Code != 1011 || ce.Reason != "throttled" {
		t.Fatalf("CloseError = (%d, %q)，期望 (1011, %q)", ce.Code, ce.Reason, "throttled")
	}
}

// TestConnCloseSendsCloseFrame：主动关闭要先发 close 帧，而不是直接断 TCP。
//
// 直接断连会让对端看到 abnormal closure（1006），无从区分「对方正常收尾」
// 与「网络断了」。发一个带码的 close 是把原因说清楚的唯一机会。
func TestConnCloseSendsCloseFrame(t *testing.T) {
	client, raw := pipeRaw(t, 2*time.Second)

	go func() {
		_ = client.Close(CloseNormal, "bye")
	}()

	f, err := ReadFrame(raw, maxTestPayload)
	if err != nil {
		t.Fatalf("未收到 close 帧: %v", err)
	}
	if f.Opcode != OpClose {
		t.Fatalf("opcode = %v，期望 close", f.Opcode)
	}
	code, reason, err := DecodeClosePayload(f.Payload)
	if err != nil {
		t.Fatalf("解析 close 负载失败: %v", err)
	}
	if code != CloseNormal || reason != "bye" {
		t.Fatalf("close 负载 = (%d, %q)，期望 (%d, %q)", code, reason, CloseNormal, "bye")
	}
}

// TestConnCloseIsIdempotent：重复关闭不得 panic，也不得重复发帧。
//
// 关闭路径常被 defer 与错误分支同时触发。第二次发 close 帧会写进一个
// 可能已经关掉的连接，在生产里表现为一条毫无意义的 write on closed conn。
func TestConnCloseIsIdempotent(t *testing.T) {
	client, raw := pipeRaw(t, 2*time.Second)

	if err := client.Close(CloseNormal, ""); err != nil {
		t.Fatalf("首次关闭失败: %v", err)
	}
	if err := client.Close(CloseNormal, ""); err != nil {
		t.Fatalf("重复关闭应当是空操作，实际: %v", err)
	}

	if _, err := ReadFrame(raw, maxTestPayload); err != nil {
		t.Fatalf("未收到首个 close 帧: %v", err)
	}
	_ = raw.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if f, err := ReadFrame(raw, maxTestPayload); err == nil {
		t.Fatalf("不应发出第二个帧，实际收到 opcode=%v", f.Opcode)
	}
}

// TestConnWriteAfterCloseIsRejected：关闭后再写要报错而不是静默成功。
func TestConnWriteAfterCloseIsRejected(t *testing.T) {
	client, raw := pipeRaw(t, 2*time.Second)

	go func() {
		for {
			if _, err := ReadFrame(raw, maxTestPayload); err != nil {
				return
			}
		}
	}()

	if err := client.Close(CloseNormal, ""); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if err := client.WriteMessage(OpText, []byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后写入应当报 ErrClosed，实际: %v", err)
	}
}

// TestConnIdleTimeoutFires 固化空闲超时的判据。
//
// 沿用 httpx 的语义：两帧之间超过 idle 就判定上游挂死。realtime 场景下
// 这比总超时有意义得多——一个 30 分钟的语音会话完全正常，而 60 秒收不到
// 任何帧（连 ping 都没有）就是异常。
func TestConnIdleTimeoutFires(t *testing.T) {
	client, _ := pipeRaw(t, 60*time.Millisecond)

	start := time.Now()
	_, _, err := client.ReadMessage()
	elapsed := time.Since(start)

	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("应当报 ErrIdleTimeout，实际: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("空闲超时用了 %v，远超设定的 60ms", elapsed)
	}
}

// TestConnIdleTimeoutResetsOnTraffic：有帧到达就重置计时。
//
// 不重置的话，一个持续 5 分钟的正常会话会在 idle 到期时被掐断——
// 而它一直在正常收发。
func TestConnIdleTimeoutResetsOnTraffic(t *testing.T) {
	client, raw := pipeRaw(t, 300*time.Millisecond)

	go func() {
		for i := 0; i < 3; i++ {
			time.Sleep(90 * time.Millisecond)
			_ = WriteFrame(raw, Frame{FIN: true, Opcode: OpText, Payload: []byte("tick")}, false)
		}
	}()

	for i := 0; i < 3; i++ {
		if _, _, err := client.ReadMessage(); err != nil {
			t.Fatalf("第 %d 条消息读取失败（超时未被重置）: %v", i+1, err)
		}
	}
}

// TestConnConcurrentWritesDoNotInterleave 防的是帧被写花。
//
// WebSocket 帧是「头 + 负载」两次写。两个 goroutine 同时写而不加锁时，
// 一个帧的负载会插进另一个帧的头后面，对端解出来是彻底的乱码——
// 而这种错误只在并发时偶发，最难复现。
func TestConnConcurrentWritesDoNotInterleave(t *testing.T) {
	client, server := pipeConns(t, 5*time.Second)

	const writers = 8
	payload := []byte(strings.Repeat("x", 200))

	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			_ = client.WriteMessage(OpBinary, payload)
		}()
	}

	for i := 0; i < writers; i++ {
		op, got, err := server.ReadMessage()
		if err != nil {
			t.Fatalf("第 %d 条读取失败: %v", i+1, err)
		}
		if op != OpBinary || !bytes.Equal(got, payload) {
			t.Fatalf("第 %d 条帧被写花: opcode=%v 长度=%d", i+1, op, len(got))
		}
	}
	wg.Wait()
}

// TestConnCloseReleasesUnderlyingConn 防的是文件描述符泄漏。
//
// 只发 close 帧而不关底层连接时，fd 会一直挂着。realtime 网关同时持有大量
// 长连接，泄漏几百个就撞上系统上限，表现是「跑了几小时后突然连不上任何
// 上游」——而那时根本看不出是谁没关。
func TestConnCloseReleasesUnderlyingConn(t *testing.T) {
	c, s := tcpPair(t)
	defer s.Close()

	conn := NewConn(c, RoleClient, maxTestPayload, 2*time.Second)
	if err := conn.Close(CloseNormal, ""); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	if _, err := c.Write([]byte{0}); err == nil {
		t.Fatal("Close 之后底层 net.Conn 应当已关闭")
	}
}

// TestConnReleasesAfterPeerClose 覆盖读路径上的同一个泄漏。
//
// 收到对端 close 后只标记状态，用户那句 defer Close() 会因为「已关闭」而
// 直接返回 nil——fd 就此永远挂着。收到 close 的一方必须自己把连接释放掉。
func TestConnReleasesAfterPeerClose(t *testing.T) {
	c, s := tcpPair(t)
	defer s.Close()

	conn := NewConn(c, RoleClient, maxTestPayload, 2*time.Second)

	go func() {
		_ = WriteFrame(s, Frame{
			FIN: true, Opcode: OpClose, Payload: EncodeClosePayload(CloseNormal, ""),
		}, false)
	}()

	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("收到 close 帧应当报错")
	}

	// 用户惯常的 defer Close() 此时是空操作，所以释放必须已经发生。
	if err := conn.Close(CloseNormal, ""); err != nil {
		t.Fatalf("重复关闭应当是空操作，实际: %v", err)
	}
	if _, err := c.Write([]byte{0}); err == nil {
		t.Fatal("收到对端 close 后底层 net.Conn 应当已释放")
	}
}

// TestConnEchoesPeerCloseFrame 固化 RFC 6455 §5.5.1：收到 close 且自己没发过
// 时必须回一个 close，否则对端只能等到自己的超时才断开。
func TestConnEchoesPeerCloseFrame(t *testing.T) {
	client, raw := pipeRaw(t, 2*time.Second)

	go func() {
		_ = WriteFrame(raw, Frame{
			FIN: true, Opcode: OpClose, Payload: EncodeClosePayload(CloseGoingAway, "bye"),
		}, false)
	}()

	if _, _, err := client.ReadMessage(); err == nil {
		t.Fatal("收到 close 帧应当报错")
	}

	// 加读期限：没有回应时要快速失败，而不是把整个测试挂到超时。
	_ = raw.SetReadDeadline(time.Now().Add(time.Second))
	f, err := ReadFrame(raw, maxTestPayload)
	if err != nil {
		t.Fatalf("未收到 close 回应: %v", err)
	}
	if f.Opcode != OpClose {
		t.Fatalf("回应 opcode = %v，期望 close", f.Opcode)
	}
}

// TestConnPeerHangupIsNotACloseError：对端不发 close 直接断开，
// 要与「收到 close 帧」区分——前者是链路异常，后者是对端说清了原因。
func TestConnPeerHangupIsNotACloseError(t *testing.T) {
	client, raw := pipeRaw(t, 2*time.Second)

	if err := raw.Close(); err != nil {
		t.Fatalf("关闭裸端失败: %v", err)
	}

	_, _, err := client.ReadMessage()
	if err == nil {
		t.Fatal("对端断开后读取应当报错")
	}
	var ce *CloseError
	if errors.As(err, &ce) {
		t.Fatalf("未收到 close 帧不应报 CloseError，实际: %v", err)
	}
}
