package ws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
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
	// IncompleteMessage 保留关闭前仍在重组的分片证据，不把正常 close 当完整业务终结。
	// 它不改变关闭码、错误文本或自动回应，也不表示此关闭违反 RFC。
	IncompleteMessage bool
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("ws: 对端关闭连接 (code=%d reason=%q)", e.Code, e.Reason)
}

// Conn 是一条已完成握手的 WebSocket 连接。
//
// 它只负责帧层的生命周期：按角色掩码、自动回 pong、空闲超时、关闭握手。
// 业务语义（事件改写、采样率协商）一概不碰——那属于协议层。
type Conn struct {
	conn         net.Conn
	role         Role
	idle         time.Duration
	reader       *Reader
	budget       *BufferBudget
	writeTimeout time.Duration
	readMu       sync.Mutex

	// writeMu 串行化帧写。
	//
	// 帧是「头 + 负载」两次写，不串行化时一个帧的负载会插进另一个帧的头
	// 后面，对端解出来是彻底的乱码——而这种错误只在并发时偶发，最难复现。
	// 绝不在持有它的时候读，否则自动回 pong 会和对端的下一帧互相死等。
	writeMu sync.Mutex
	closed  atomic.Bool
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
		reader: NewReader(&deadlineReader{r: r, conn: conn, idle: idle}, limit),
	}
}

// ReadMessage 读一条业务消息，控制帧在这里被消化掉。
//
// ping 必须由传输层自己回 pong，不能冒泡给上层：上层在等一条业务消息时
// 根本不会去读，而对端收不到 pong 就会判定链路已死并断开——那时数据其实
// 还在正常传输。表现是「长会话莫名其妙断连」。
func (c *Conn) ReadMessage() (Opcode, []byte, error) {
	if c.budget != nil {
		return 0, nil, errors.New("ws: 配置 Budget 后必须使用 ReadOwnedMessage 并 Release")
	}
	m, err := c.ReadOwnedMessage(context.Background())
	if err != nil {
		return 0, nil, err
	}
	op, payload := m.Opcode, m.Payload
	m.Release()
	return op, payload, nil
}

// WriteMessage 发一条业务消息。
func (c *Conn) WriteMessage(op Opcode, payload []byte) error {
	if op == OpText && !utf8.Valid(payload) {
		return fmt.Errorf("%w: 文本不是合法 UTF-8", ErrProtocol)
	}
	if op == OpClose {
		if _, _, err := DecodeClosePayload(payload); err != nil {
			return err
		}
	}
	return c.writeFrame(op, payload)
}

// Ping 与业务写、自动 pong 共用写锁和期限，不能旁路成一个无限写。
func (c *Conn) Ping(payload []byte) error { return c.writeFrame(OpPing, payload) }

func (c *Conn) writeFrame(op Opcode, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.closed.Load() {
		return ErrClosed
	}
	if c.writeTimeout > 0 {
		if err := c.conn.SetWriteDeadline(time.Now().Add(c.writeTimeout)); err != nil {
			return err
		}
		defer c.conn.SetWriteDeadline(time.Time{})
	}
	return writeFrameBudget(c.conn, Frame{FIN: true, Opcode: op, Payload: payload}, c.role.masks(), c.budget)
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
	_, err := c.CloseWithResult(code, reason)
	return err
}

// CloseWithResult 防止将幂等关闭的 nil 或被动自动回应误当成本次指定帧发送证据。
// sent 只在本次 WriteFrame 完整成功后为 true，不保证对端收到；已关闭、放弃写锁
// 或帧写失败均为 false。底层释放错误独立返回，不抹掉已经实际写完帧的结果。
func (c *Conn) CloseWithResult(code uint16, reason string) (sent bool, err error) {
	if !c.closed.CompareAndSwap(false, true) {
		return false, nil
	}
	defer func() {
		if cerr := c.conn.Close(); err == nil {
			err = cerr
		}
	}()
	if code != CloseNoStatus && !validCloseCode(code) || !utf8.ValidString(reason) || code == CloseNoStatus && reason != "" {
		return false, fmt.Errorf("%w: 主动关闭状态码或原因非法", ErrProtocol)
	}

	// 已有业务帧阻塞时不能等写锁：直接关底层连接才能把那个写唤醒。
	// 没有并发写时尽力发送关闭帧，并给这次礼貌收尾一个有限期限。
	if c.writeMu.TryLock() {
		deadline := time.Second
		if c.idle > 0 && c.idle < deadline {
			deadline = c.idle
		}
		if c.writeTimeout > 0 && c.writeTimeout < deadline {
			deadline = c.writeTimeout
		}
		defer c.writeMu.Unlock()
		if err = c.conn.SetWriteDeadline(time.Now().Add(deadline)); err != nil {
			return false, err
		}
		defer c.conn.SetWriteDeadline(time.Time{})
		// 原因先按协议上限截断，避免超长 reason 让编码器保留一个超大容量。
		size := closePayloadSize(code, reason)
		if err = c.budget.acquire(int64(size)); err != nil {
			return false, err
		}
		payload := EncodeClosePayload(code, reason)
		defer func() { payload = nil; c.budget.release(int64(size)) }()
		err = writeFrameBudget(c.conn, Frame{
			FIN:     true,
			Opcode:  OpClose,
			Payload: payload,
		}, c.role.masks(), c.budget)
		sent = err == nil
	}
	return sent, err
}

// respondToPeerClose 处理收到的对端 close 帧。
//
// 按 RFC 6455 §5.5.1，收到 close 且自己没发过时必须回一个 close——不回的话
// 对端只能等自己的超时才断开。回完即释放本端连接：调用方那句惯常的
// defer Close() 此时会因为「已关闭」直接返回，fd 就此永远挂着。
func (c *Conn) respondToPeerClose(code uint16) {
	if code != CloseNoStatus {
		code = CloseNormal
	}
	_, _ = c.CloseWithResult(code, "")
}

func closePayloadSize(code uint16, reason string) int {
	if code == CloseNoStatus {
		return 0
	}
	n := min(len(reason), maxControlPayload-2)
	for n < len(reason) && n > 0 && !utf8.RuneStart(reason[n]) {
		n--
	}
	return 2 + n
}

// deadlineReader 在每次需要更多字节前刷新读期限。Reader 可能在一条消息里读取
// 很多分片，只在 ReadMessage 外层设一次会把活跃消息的总时长误当成空闲时间。
type deadlineReader struct {
	r    io.Reader
	conn net.Conn
	idle time.Duration
}

func (r *deadlineReader) Read(p []byte) (int, error) {
	if r.idle > 0 {
		if err := r.conn.SetReadDeadline(time.Now().Add(r.idle)); err != nil {
			return 0, err
		}
	}
	return r.r.Read(p)
}
