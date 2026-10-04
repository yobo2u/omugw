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
	if errors.As(err, &closed) && classifyClose != nil {
		r.upstreamFailure = classifyClose(closed.Code, closed.Reason)
	}
	return r
}

// relayWS 接管 initial；唯一上游 reader 先观测后转发，调用者只在返回后结算 Lease。
func relayWS(ctx context.Context, downstream, upstream *ws.Conn, initial *ws.Message, options wsRelayOptions) error {
	// 请求/registry 的取消先交给协调者发 close，不让 ReadOwnedMessage 的取消
	// 回调越过礼貌关闭。读 ctx 的取消权只在下方 close 完成之后使用。
	readCtx, cancelRead := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRead()
	termination := newWSTermination(func(code uint16, reason string, deadline time.Time) {
		closeWSConnections([]*ws.Conn{downstream, upstream}, code, reason, deadline)
	}, wsCloseBudget(options.Timeouts))
	notice, _ := ctx.Value(wsShutdownKey{}).(*wsShutdownNotice)
	var shutdown <-chan struct{}
	if notice != nil {
		shutdown = notice.started
		termination = notice.termination
	}
	report := func(err error, isUpstream bool) {
		r := wsRelayResult{err: err}
		if isUpstream {
			r = wsUpstreamResult(err, options.ClassifyClose)
		}
		termination.report(r)
	}
	finish := func() {
		if options.Observer != nil {
			options.Observer.Finish()
		}
		if options.Policy != nil {
			options.Policy.Finish()
		}
	}
	if err := termination.attachRelay(downstream, upstream, options); err != nil {
		if initial != nil {
			initial.Release()
		}
		if options.Policy != nil {
			options.Policy.Stop()
		}
		termination.report(wsRelayResult{err: err})
		result := termination.close()
		select {
		case <-shutdown:
			<-notice.finished
		default:
		}
		finish()
		return result
	}
	var workers sync.WaitGroup
	forward := func(src, dst *ws.Conn, fromUpstream bool, first *ws.Message) {
		defer workers.Done()
		direction := wsClientToUpstream
		if fromUpstream {
			direction = wsUpstreamToClient
		}
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
			var decision wsForwardDecision
			if options.Policy != nil {
				decision, err = options.Policy.BeforeForward(readCtx, direction, m.Opcode, m.Payload)
				if err != nil {
					m.Release()
					// 内部封口只退当前 worker；原 End/After 写错或既有 termination
					// owner 负责收尾。真实 ctx.Err 与其他错误仍须原路 report。
					if err != errWSPolicyStopped {
						report(err, fromUpstream)
					}
					return
				}
			}
			var delivered func(error)
			original := false
			if decision.Action != wsForward {
				if decision.End == nil {
					err = errWSRelayPolicy
				} else {
					won, receipt := termination.policyEnd(*decision.End, decision.Action == wsForwardThenEnd)
					if won && decision.Action == wsForwardThenEnd {
						original, delivered = true, receipt
					} else {
						err = context.Canceled
					}
				}
			}
			if err == nil && fromUpstream && m.Opcode == ws.OpText && options.Observer != nil {
				err = options.Observer.Observe(m.Payload)
			}
			writeFailure := false
			if err == nil {
				err = termination.write(dst, direction, m, original)
				writeFailure = err != nil
			}
			// 交付锁必须在 After 之前释放；Stop 解开 policy 门闩后，回执不等待 close。
			if options.Policy != nil {
				options.Policy.AfterForward(decision.Ticket, err)
			}
			m.Release()
			m = nil
			if delivered != nil {
				delivered(err)
			}
			if err != nil {
				isUpstream := fromUpstream
				if writeFailure {
					isUpstream = !fromUpstream
				}
				report(err, isUpstream)
				return
			}
			if !termination.allowed(direction, false) {
				return
			}
		}
	}
	heartbeat := func(c *ws.Conn, isUpstream bool) {
		defer workers.Done()
		if options.Timeouts.Idle <= 0 {
			<-termination.selected
			return
		}
		ticker := time.NewTicker(max(options.Timeouts.Idle/2, time.Nanosecond))
		defer ticker.Stop()
		for {
			select {
			case <-termination.selected:
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
	if options.Policy != nil {
		workers.Add(1)
		go func() { defer workers.Done(); superviseWSPolicy(options.Policy, termination) }()
	}
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
	result := termination.close()
	// 等待 registry 完成 context cause/退出交接，不能遗留它的关停工作者。
	select {
	case <-shutdown:
		<-notice.finished
	default:
	}
	cancelRead()
	workers.Wait()
	finish()
	return result
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
