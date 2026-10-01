package testkit

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"sync"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

var (
	errWSUpstreamFixture = errors.New("WS 上游回放 fixture 或握手不合法")
	errWSUpstreamLimits  = errors.New("WS 上游回放预算不合法")
	errWSUpstreamClosed  = errors.New("WS 上游回放已关闭")
	errWSUpstreamRepeat  = errors.New("WS 上游回放收到重复连接")
	errWSUpstreamClaimed = errors.New("WS 上游回放连接已领取")
	errWSUpstreamHTTP    = errors.New("WS 上游回放预期 HTTP 握手失败，没有升级连接")
	errWSUpstreamAccept  = errors.New("WS 上游回放接管连接失败")
	errWSUpstreamWrite   = errors.New("WS 上游回放写握手错误信封失败")
	errWSUpstreamClose   = errors.New("WS 上游回放释放连接失败")
)

// WSReplayUpstream 只负责本地握手，不消费消息轨迹，也不把 101 当业务成功。
// 唯一连接即使已领取仍由本对象负责兜底释放；调用方必须 Close，httptest 不会关闭劫持连接。
// ready 只广播一次状态变化，连接槽与首次错误槽各一个，拒绝洪泛不能积累连接或错误队列。
type WSReplayUpstream struct {
	expected     Request
	status       int
	errorBody    []byte
	messageBytes int64

	mu        sync.Mutex
	ready     chan struct{}
	signaled  bool
	attempted bool
	claimed   bool
	closed    bool
	httpDone  bool
	err       error
	conn      *ws.Conn
	accepting net.Conn
}

// NewWSReplayUpstream 先守完整会话与握手边界，再冻结回放所需的最小快照。
// 校验器可能携带 fixture 名称；这里只返回固定错误，防止错误输入绕入诊断输出。
func NewWSReplayUpstream(f Fixture, limits WSLimits) (*WSReplayUpstream, error) {
	if err := limits.validate(); err != nil {
		return nil, errWSUpstreamLimits
	}
	if err := ValidateWSSession(f, limits); err != nil {
		return nil, errWSUpstreamFixture
	}
	if _, err := SanitizeWSHandshake(f.Request); err != nil {
		return nil, errWSUpstreamFixture
	}
	expected, err := SanitizeWSHandshake(f.Response.WS.Upstream)
	if err != nil {
		return nil, errWSUpstreamFixture
	}
	return &WSReplayUpstream{
		expected: expected, status: f.Response.WS.UpstreamExpectedStatus,
		errorBody: bytes.Clone(f.Response.WS.UpstreamError), messageBytes: limits.MessageBytes,
		ready: make(chan struct{}),
	}, nil
}

// ServeHTTP 的错路由必须成为回放断言错误，不能返回普通 404 掩盖被测调用方发错端点。
// 子协议仅由 MatchWSHandshake 校验安全 token；现有 ws.Accept 不回协商结果，不伪造选择证明。
func (u *WSReplayUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := MatchWSHandshake(u.expected, r); err != nil {
		u.mu.Lock()
		u.recordLocked(err)
		u.mu.Unlock()
		http.Error(w, "WS replay handshake mismatch", http.StatusBadRequest)
		return
	}

	u.mu.Lock()
	var reject error
	status := http.StatusConflict
	switch {
	case u.closed:
		reject, status = errWSUpstreamClosed, http.StatusServiceUnavailable
	case u.attempted:
		reject = errWSUpstreamRepeat
	case u.err != nil:
		reject = u.err
	}
	if reject != nil {
		u.recordLocked(reject)
		u.mu.Unlock()
		http.Error(w, "WS replay connection rejected", status)
		return
	}
	// 在 Accept 前预占名额，防止两条并发握手都升级，随后抢同一个槽。
	u.attempted = true
	u.mu.Unlock()

	if u.status != http.StatusSwitchingProtocols {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.status)
		_, err := w.Write(u.errorBody)
		u.mu.Lock()
		u.httpDone = true
		if err != nil {
			u.recordLocked(errWSUpstreamWrite)
		}
		u.signalLocked()
		u.mu.Unlock()
		return
	}

	// Hijack 后到 Accept 返回前原始 socket 也必须登记，Close 才能唤醒尚未完成的 101 写。
	acceptWriter := w
	if _, ok := w.(http.Hijacker); ok {
		acceptWriter = &wsUpstreamHijacker{ResponseWriter: w, upstream: u}
	}
	conn, err := ws.Accept(acceptWriter, r, ws.AcceptOptions{MaxPayload: u.messageBytes, Idle: 0})
	u.mu.Lock()
	u.accepting = nil
	if err != nil && !u.closed {
		u.recordLocked(errWSUpstreamAccept)
	}
	keep := conn != nil && !u.closed && u.err == nil
	if keep {
		u.conn = conn
		u.signalLocked()
	}
	u.mu.Unlock()
	if conn != nil && !keep {
		// Close 或错配抢先结束了会话，晚到连接不能入槽，也不能因未领取而泄漏。
		_ = conn.Close(ws.CloseNormal, "")
	}
}

// Connection 只转交一次使用权，不转移 socket 的兜底所有权；取消等待不占领取名额。
// 领取成功不证明后续握手没有错配，最终驱动仍必须检查 Err 与 Close 的返回值。
func (u *WSReplayUpstream) Connection(ctx context.Context) (*ws.Conn, error) {
	for {
		u.mu.Lock()
		switch {
		case u.err != nil:
			err := u.err
			u.mu.Unlock()
			return nil, err
		case u.closed:
			u.mu.Unlock()
			return nil, errWSUpstreamClosed
		case u.claimed:
			u.mu.Unlock()
			return nil, errWSUpstreamClaimed
		case ctx.Err() != nil:
			u.mu.Unlock()
			return nil, ctx.Err()
		case u.httpDone:
			u.mu.Unlock()
			return nil, errWSUpstreamHTTP
		case u.conn != nil:
			u.claimed = true
			conn := u.conn
			u.mu.Unlock()
			return conn, nil
		}
		u.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-u.ready:
		}
	}
}

// Err 不因领取或 Close 清空，防止迟到错配被“连接已拿到”掩盖。
func (u *WSReplayUpstream) Err() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.err
}

// Close 在锁内终结槽所有权、锁外释放资源，不能让阻塞网络写占住状态锁。
// 接管中的原始 socket 与已接受的 Conn 二选一；即使从未领取，也必须全部释放。
func (u *WSReplayUpstream) Close() error {
	u.mu.Lock()
	if u.closed {
		err := u.err
		u.mu.Unlock()
		return err
	}
	u.closed = true
	conn, accepting := u.conn, u.accepting
	u.conn, u.accepting = nil, nil
	u.signalLocked()
	u.mu.Unlock()

	var err error
	if accepting != nil {
		err = accepting.Close()
	}
	if conn != nil {
		err = conn.Close(ws.CloseNormal, "")
	}
	if err != nil {
		u.mu.Lock()
		u.recordLocked(errWSUpstreamClose)
		u.mu.Unlock()
	}
	return u.Err()
}

func (u *WSReplayUpstream) recordLocked(err error) {
	if u.err == nil {
		u.err = err
	}
	u.signalLocked()
}

func (u *WSReplayUpstream) signalLocked() {
	if !u.signaled {
		close(u.ready)
		u.signaled = true
	}
}

type wsUpstreamHijacker struct {
	http.ResponseWriter
	upstream *WSReplayUpstream
}

func (w *wsUpstreamHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return nil, nil, errWSUpstreamAccept
	}
	u := w.upstream
	u.mu.Lock()
	keep := !u.closed && u.err == nil
	if keep {
		u.accepting = conn
	}
	u.mu.Unlock()
	if !keep {
		// 底层 Hijack 本身可能阻塞，必须在返回后重新检查 Close，不能再向已关闭会话写 101。
		_ = conn.Close()
		return nil, nil, errWSUpstreamClosed
	}
	return conn, rw, nil
}
