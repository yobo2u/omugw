package gateway

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

var (
	errWSRelayPolicy     = errors.New("websocket invalid event correlation")
	errWSRelayDownstream = errors.New("websocket downstream connection failed")
	errWSRelayIncomplete = errors.New("websocket closed with incomplete message")
)

type wsRelayResult struct {
	err             error
	upstream        bool
	upstreamFailure *canonical.Error
}

// 协议分类在所有权交接前完成；仲裁器只保留安全结果，不在 Release 后回看 reason。
func wsUpstreamResult(err error, classifyClose func(uint16, string) *canonical.Error) wsRelayResult {
	r := wsRelayResult{err: err, upstream: true}
	var closed *ws.CloseError
	if errors.As(err, &closed) {
		r.upstreamFailure = classifyClose(closed.Code, closed.Reason)
	}
	return r
}

// 原因选择与实际关闭必须属于同一个仲裁器。仅共享返回错误而各自 Close，
// 会让 registry 的 1001 在故障已胜出、尚未完成分类时抢先写到线上。
type wsTermination struct {
	firstReason sync.Once
	selected    chan struct{}
	winner      wsRelayResult
	closeOnce   sync.Once
	closeConns  func(uint16, string)
	result      error
}

func newWSTermination(closeConns func(uint16, string)) *wsTermination {
	return &wsTermination{selected: make(chan struct{}), closeConns: closeConns}
}

func (t *wsTermination) report(r wsRelayResult) {
	won := false
	t.firstReason.Do(func() {
		won = true
		t.winner = r
		close(t.selected)
	})
	if !won {
		releaseWSRelayError(r.err)
	}
}

func (t *wsTermination) close() error {
	t.closeOnce.Do(func() {
		<-t.selected
		code, reason, result := classifyWSRelay(t.winner)
		t.closeConns(code, reason)
		// 原因所有权只由实际关闭者归还；其余调用者通过 Once 等待关闭和释放，
		// 不借 transport 幂等返回冒充 join，也不在释放后再借用 reason。
		releaseWSRelayError(t.winner.err)
		t.winner = wsRelayResult{}
		t.result = result
	})
	return t.result
}

// relayWS 接管 initial；唯一上游 reader 先观测后转发，调用者只在返回后结算 Lease。
func relayWS(ctx context.Context, downstream, upstream *ws.Conn, initial *ws.Message, observer wsEventObserver, classifyClose func(uint16, string) *canonical.Error, idle time.Duration) error {
	// 请求/registry 的取消先交给协调者发 close，不让 ReadOwnedMessage 的取消
	// 回调越过礼貌关闭。读 ctx 的取消权只在下方 close 完成之后使用。
	readCtx, cancelRead := context.WithCancel(context.WithoutCancel(ctx))
	stop := make(chan struct{})
	termination := newWSTermination(func(code uint16, reason string) {
		closeWSConnections([]*ws.Conn{downstream, upstream}, code, reason)
	})
	notice, _ := ctx.Value(wsShutdownKey{}).(*wsShutdownNotice)
	var shutdown <-chan struct{}
	if notice != nil {
		shutdown = notice.started
		termination = notice.termination
	}
	report := func(err error, isUpstream bool) {
		r := wsRelayResult{err: err}
		if isUpstream {
			r = wsUpstreamResult(err, classifyClose)
		}
		termination.report(r)
	}
	var workers sync.WaitGroup
	forward := func(src, dst *ws.Conn, fromUpstream bool, first *ws.Message) {
		defer workers.Done()
		m := first
		for {
			var err error
			if m == nil {
				m, err = src.ReadOwnedMessage(readCtx)
			}
			if err != nil {
				report(err, fromUpstream)
				return
			}
			if fromUpstream && m.Opcode == ws.OpText && observer != nil {
				err = observer.Observe(m.Payload)
				// 上游 error/failed/cancelled 只是观测；合法失败事件仍原字节流过。
				if err != nil {
					m.Release()
					report(err, true)
					return
				}
			}
			err = dst.WriteMessage(m.Opcode, m.Payload)
			m.Release()
			m = nil
			if err != nil {
				report(err, !fromUpstream)
				return
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}
	heartbeat := func(c *ws.Conn, isUpstream bool) {
		defer workers.Done()
		if idle <= 0 {
			<-stop
			return
		}
		ticker := time.NewTicker(max(idle/2, time.Nanosecond))
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := c.Ping(nil); err != nil {
					report(err, isUpstream)
					return
				}
			}
		}
	}
	workers.Add(4)
	go forward(upstream, downstream, true, initial)
	go forward(downstream, upstream, false, nil)
	go heartbeat(upstream, true)
	go heartbeat(downstream, false)
	select {
	case <-termination.selected:
	case <-ctx.Done():
		err := ctx.Err()
		if errors.Is(context.Cause(ctx), errWSShutdown) {
			err = errWSShutdown
		}
		report(err, false)
	case <-shutdown:
		report(errWSShutdown, false)
	}
	close(stop)
	result := termination.close()
	// 等待 registry 完成 context cause/退出交接，不能遗留它的关停工作者。
	select {
	case <-shutdown:
		<-notice.finished
	default:
	}
	cancelRead()
	workers.Wait()
	if observer != nil {
		observer.Finish()
	}
	return result
}

func releaseWSRelayError(err error) {
	var closed *ws.CloseError
	if errors.As(err, &closed) {
		closed.Release()
	}
}

// 只返回固定本地哨兵或重新构造的安全分类，绝不包裹网络原错/对端 reason。
func classifyWSRelay(r wsRelayResult) (uint16, string, error) {
	var closed *ws.CloseError
	if errors.As(r.err, &closed) {
		var result error
		if r.upstreamFailure != nil {
			result = r.upstreamFailure
		}
		// 分片中合法关闭不违反 RFC，仍转发原 code/reason；但丢弃的半条消息
		// 不能让 metrics/Lease 记成功。已核验的上游失败分类保留，不从 reason 猜测。
		if result == nil && closed.IncompleteMessage {
			result = errWSRelayIncomplete
		}
		return closed.Code, closed.Reason, result
	}
	switch {
	case errors.Is(r.err, errWSShutdown):
		return ws.CloseGoingAway, "", errWSShutdown
	case errors.Is(r.err, context.Canceled):
		return ws.CloseGoingAway, "", context.Canceled
	case errors.Is(r.err, context.DeadlineExceeded):
		return ws.CloseGoingAway, "", context.DeadlineExceeded
	case errors.Is(r.err, ws.ErrInvalidUTF8):
		return 1007, "invalid UTF-8", ws.ErrInvalidUTF8
	case errors.Is(r.err, ws.ErrProtocol):
		return ws.CloseProtocolError, "invalid websocket frame", ws.ErrProtocol
	case errors.Is(r.err, ws.ErrMessageTooLarge):
		return ws.CloseTooLarge, "message too large", ws.ErrMessageTooLarge
	case errors.Is(r.err, ws.ErrBufferLimit):
		return 1013, "buffer capacity exhausted", ws.ErrBufferLimit
	case errors.Is(r.err, errWSUsageLimit):
		return 1008, "usage record capacity exhausted", errWSUsageLimit
	case errors.Is(r.err, errWSRelayPolicy), errors.Is(r.err, errWSUsageID), errors.Is(r.err, errWSUsageFinished):
		return 1008, "invalid event correlation", errWSRelayPolicy
	case !r.upstream:
		return ws.CloseInternalError, "downstream connection failed", errWSRelayDownstream
	default:
		return ws.CloseInternalError, "upstream connection failed", canonical.Newf(canonical.ClassInternal, "Realtime upstream connection failed")
	}
}
