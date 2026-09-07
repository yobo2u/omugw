package ws

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Role 决定本端在 RFC 6455 里的方向，进而决定发出的帧要不要掩码。
//
// 网关同时扮演两个角色：对下游客户端是服务端（不得掩码），对上游是客户端
// （必须掩码）。搞反时真实上游会以 1002 断开，而本地自测发现不了——
// 自己的解码器对掩码与否一视同仁。
type Role int

const (
	RoleClient Role = iota
	RoleServer
)

func (r Role) masks() bool { return r == RoleClient }

var (
	// ErrClosed 表示连接已关闭，不能再写。
	ErrClosed = errors.New("ws: 连接已关闭")

	// ErrIdleTimeout 表示两帧之间的间隔超过上限。
	//
	// 与「总超时」分开：一个 30 分钟的语音会话完全正常，而 60 秒收不到任何帧
	// （连 ping 都没有）才是上游挂死的真正判据。这与 httpx 的四层超时同源。
	ErrIdleTimeout = errors.New("ws: 空闲超时")
)

// CloseError 是对端发来的关闭帧。
//
// 独立成类型是为了让上层能拿到状态码：实测中 DashScope 用 1011 +
// "To many requests..." 表达过载。把它当成普通 EOF 会让网关以为上游正常
// 收尾从而不重试——而这恰恰是最该换一个凭据重试的场景。
type CloseError struct {
	Code   uint16
	Reason string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("ws: 对端关闭连接 (code=%d reason=%q)", e.Code, e.Reason)
}

// Conn 是一条已完成握手的 WebSocket 连接。
//
// 它只负责帧层的生命周期：按角色掩码、自动回 pong、空闲超时、关闭握手。
// 业务语义（事件改写、采样率协商）一概不碰——那属于协议层。
type Conn struct {
	conn   net.Conn
	role   Role
	idle   time.Duration
	reader *Reader

	// mu 同时串行化写与保护 closed。
	//
	// 帧是「头 + 负载」两次写，不串行化时一个帧的负载会插进另一个帧的头
	// 后面，对端解出来是彻底的乱码——而这种错误只在并发时偶发，最难复现。
	// 绝不在持有它的时候读，否则自动回 pong 会和对端的下一帧互相死等。
	mu     sync.Mutex
	closed bool
}

// NewConn 包装一条已握手的连接。limit 是单条消息重组后的负载上限，
// idle 是两帧之间的最大间隔（传 0 表示不设空闲超时）。
func NewConn(conn net.Conn, role Role, limit int64, idle time.Duration) *Conn {
	return newConnBuffered(conn, conn, role, limit, idle)
}

// newConnBuffered 从一个独立的读取源构造连接。
//
// 握手期间 bufio 可能已经把客户端紧跟着发来的帧字节预读进缓冲区。此时若
// 改从裸 net.Conn 读，那批字节就永远拿不回来了——表现是「客户端的第一条
// 消息凭空丢失」，而且只在客户端连发时才复现。
func newConnBuffered(conn net.Conn, r io.Reader, role Role, limit int64, idle time.Duration) *Conn {
	return &Conn{
		conn:   conn,
		role:   role,
		idle:   idle,
		reader: NewReader(r, limit),
	}
}

// ReadMessage 读一条业务消息，控制帧在这里被消化掉。
//
// ping 必须由传输层自己回 pong，不能冒泡给上层：上层在等一条业务消息时
// 根本不会去读，而对端收不到 pong 就会判定链路已死并断开——那时数据其实
// 还在正常传输。表现是「长会话莫名其妙断连」。
func (c *Conn) ReadMessage() (Opcode, []byte, error) {
	for {
		if c.idle > 0 {
			// 每条消息前重置，所以有流量就等于续命；不重置的话，一个持续
			// 五分钟的正常会话会在 idle 到期时被掐断。
			if err := c.conn.SetReadDeadline(time.Now().Add(c.idle)); err != nil {
				return 0, nil, err
			}
		}

		op, payload, err := c.reader.ReadMessage()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return 0, nil, ErrIdleTimeout
			}
			return 0, nil, err
		}

		switch op {
		case OpPing:
			// 负载必须原样带回（RFC 6455 §5.5.3）——对端用它来配对请求。
			if err := c.writeFrame(OpPong, payload); err != nil {
				return 0, nil, err
			}
		case OpPong:
			// 对端主动发的心跳应答，与业务无关，丢弃。
		case OpClose:
			code, reason, derr := DecodeClosePayload(payload)
			if derr != nil {
				return 0, nil, derr
			}
			c.respondToPeerClose()
			return 0, nil, &CloseError{Code: code, Reason: reason}
		default:
			return op, payload, nil
		}
	}
}

// WriteMessage 发一条业务消息。
func (c *Conn) WriteMessage(op Opcode, payload []byte) error {
	return c.writeFrame(op, payload)
}

func (c *Conn) writeFrame(op Opcode, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return ErrClosed
	}
	return WriteFrame(c.conn, Frame{FIN: true, Opcode: op, Payload: payload}, c.role.masks())
}

// Close 发出关闭帧并释放底层连接。重复调用是空操作。
//
// 先发 close 帧而不是直接断 TCP：直接断连会让对端看到 abnormal closure
// （1006），无从区分「对方正常收尾」与「网络断了」。发一个带码的 close
// 是把原因说清楚的唯一机会。
//
// 发完必须关底层连接：只发帧不关，fd 会一直挂着。realtime 网关同时持有大量
// 长连接，泄漏几百个就撞上系统上限，表现是「跑了几小时后突然连不上任何
// 上游」——而那时根本看不出是谁没关。
//
// 幂等是必须的：关闭路径常被 defer 与错误分支同时触发，第二次发帧会写进
// 一个已经关掉的连接，在生产里表现为一条毫无意义的 write on closed conn。
func (c *Conn) Close(code uint16, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}
	c.closed = true

	// 帧写失败也要继续关连接，否则一条写不进去的连接就永远泄漏在那里。
	err := WriteFrame(c.conn, Frame{
		FIN:     true,
		Opcode:  OpClose,
		Payload: EncodeClosePayload(code, reason),
	}, c.role.masks())

	if cerr := c.conn.Close(); err == nil {
		err = cerr
	}
	return err
}

// respondToPeerClose 处理收到的对端 close 帧。
//
// 按 RFC 6455 §5.5.1，收到 close 且自己没发过时必须回一个 close——不回的话
// 对端只能等自己的超时才断开。回完即释放本端连接：调用方那句惯常的
// defer Close() 此时会因为「已关闭」直接返回，fd 就此永远挂着。
func (c *Conn) respondToPeerClose() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return
	}
	c.closed = true

	_ = WriteFrame(c.conn, Frame{
		FIN:     true,
		Opcode:  OpClose,
		Payload: EncodeClosePayload(CloseNormal, ""),
	}, c.role.masks())
	_ = c.conn.Close()
}
