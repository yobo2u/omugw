package gateway

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/protocol/dashscoperealtime"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

var (
	errWSRelayPolicy     = errors.New("websocket invalid event correlation")
	errWSRelayDownstream = errors.New("websocket downstream connection failed")
)

type wsRelayResult struct {
	err      error
	upstream bool
}

// relayWS 接管 initial；唯一上游 reader 先观测后转发，调用者只在返回后结算 Lease。
func relayWS(ctx context.Context, downstream, upstream *ws.Conn, initial *ws.Message, usage *wsUsage, idle time.Duration) error {
	// 请求/registry 的取消先交给协调者发 close，不让 ReadOwnedMessage 的取消
	// 回调越过礼貌关闭。读 ctx 的取消权只在下方 close 完成之后使用。
	readCtx, cancelRead := context.WithCancel(context.WithoutCancel(ctx))
	stop := make(chan struct{})
	// 只转交胜出的一次终止结果；失败竞争者在 worker 内归还关闭原因，不能
	// 让它们排队占住预算，也不能在协调者已选中原因后被取消/shutdown 改写。
	results := make(chan wsRelayResult, 1)
	var firstReason sync.Once
	notice, _ := ctx.Value(wsShutdownKey{}).(*wsShutdownNotice)
	var shutdown <-chan struct{}
	if notice != nil {
		shutdown = notice.started
	}
	report := func(err error, isUpstream bool) {
		// 关停已发起后的 socket 错误是关闭的结果，不能误报上游故障；已经
		// 交付给协调者的原因则不再改写，防后来 shutdown 覆盖先前业务关闭。
		select {
		case <-shutdown:
			releaseWSRelayError(err)
			err = errWSShutdown
		default:
		}
		won := false
		firstReason.Do(func() {
			won = true
			results <- wsRelayResult{err, isUpstream}
		})
		if !won {
			releaseWSRelayError(err)
		}
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
			if fromUpstream && m.Opcode == ws.OpText {
				e, inspectErr := dashscoperealtime.Inspect(m.Payload)
				if inspectErr != nil {
					err = errWSRelayPolicy
				} else if usage != nil {
					err = usage.Observe(e)
				}
				// Event.Failure 只是观测；error/failed/cancelled 仍原字节流过。
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
	var winner wsRelayResult
	select {
	case winner = <-results:
	case <-ctx.Done():
		err := ctx.Err()
		if errors.Is(context.Cause(ctx), errWSShutdown) {
			err = errWSShutdown
		}
		report(err, false)
		winner = <-results
	case <-shutdown:
		report(errWSShutdown, false)
		winner = <-results
	}
	close(stop)
	code, reason, result := classifyWSRelay(winner)
	closeWSConnections([]*ws.Conn{downstream, upstream}, code, reason)
	// 幂等 close 不 join 先前的关闭者；registry 先持有写锁时，必须等其真实
	// close 完成再取消读，否则取消回调仍会打断正在发送的 1001。
	select {
	case <-shutdown:
		<-notice.finished
	default:
	}
	// reason 仍由 winning CloseError 所有；两段有限 close 都结束后才可释放。
	releaseWSRelayError(winner.err)
	cancelRead()
	workers.Wait()
	if usage != nil {
		usage.Finish()
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
		if r.upstream {
			if failure := dashscoperealtime.ClassifyClose(closed.Code, closed.Reason); failure != nil {
				result = failure
			}
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
