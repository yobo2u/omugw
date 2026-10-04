package gateway

import (
	"context"
	"errors"
	"sync"
	"time"

	dsi "github.com/yobo2u/omugw/internal/protocol/dashscopeinference"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

type wsDelivery struct {
	at  time.Time
	err error
}

// mu 仅保护仲裁事实；写锁、Stop、守卫和 CloseError 的释放都在它之外。
// 业务槽先到先得，peer 槽只保存真实上游关闭；不能拿 peer 的 1000 改写已选失败。
type wsTermination struct {
	mu          sync.Mutex
	selected    chan struct{}
	chosen      bool
	winner      wsRelayResult
	end         *wsPolicyEnd
	deadline    time.Time
	closeBudget time.Duration
	closeOnce   sync.Once
	closeConns  func(uint16, string, time.Time)
	result      error

	attached             bool
	downstream, upstream *ws.Conn
	options              wsRelayOptions
	// 原文 owner 只发一次非阻塞回执，不参与关闭者的 Once，避免 After/Stop 互等。
	armed           chan struct{}
	originalPending bool
	delivery        chan wsDelivery
	peer            *ws.CloseError
	peerAt          time.Time
	peerChanged     chan struct{}
	upstreamEnded   bool
	wireSealed      bool
	finishes        []func()
	deliveryMu      sync.Mutex
}

func newWSTermination(closeConns func(uint16, string, time.Time), closeBudget time.Duration) *wsTermination {
	return &wsTermination{selected: make(chan struct{}), closeConns: closeConns, closeBudget: closeBudget,
		delivery: make(chan wsDelivery, 1), peerChanged: make(chan struct{}, 1)}
}

func (t *wsTermination) attachRelay(downstream, upstream *ws.Conn, options wsRelayOptions) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.chosen || t.attached {
		return context.Canceled
	}
	if t.closeBudget <= 0 || wsCloseBudget(options.Timeouts) <= 0 {
		return errWSRelayPolicy
	}
	t.attached = true
	t.downstream, t.upstream, t.options = downstream, upstream, options
	return nil
}

func (t *wsTermination) report(r wsRelayResult) {
	t.mu.Lock()
	retained := false
	if !t.chosen {
		t.chosen = true
		t.winner = r
		t.deadline = time.Now().Add(t.closeBudget)
		close(t.selected)
		retained = true
	} else if !t.wireSealed && r.upstream && r.err != context.Canceled {
		// 已封口 ticket 的固定取消不是上游断流，不能据此提前结束真实 close 的 G。
		t.upstreamEnded = true
		// transport 直接交付 CloseError；此处不调用可阻塞的自定义 errors.As。
		if peer, ok := r.err.(*ws.CloseError); ok && t.peer == nil {
			t.peer, t.peerAt = peer, time.Now()
			retained = true
		}
		select {
		case t.peerChanged <- struct{}{}:
		default:
		}
	}
	t.mu.Unlock()
	if !retained {
		releaseWSRelayError(r.err)
	}
}

func (t *wsTermination) policyEnd(end wsPolicyEnd, originalPending bool) (bool, func(error)) {
	t.mu.Lock()
	if t.chosen || !t.attached {
		t.mu.Unlock()
		return false, func(error) {}
	}
	t.chosen = true
	t.end = &end
	t.deadline = end.At.Add(min(t.options.Timeouts.Connect, t.options.Timeouts.Idle) + t.closeBudget)
	t.originalPending = originalPending
	t.armed = make(chan struct{})
	deadline := t.deadline
	close(t.selected)
	t.mu.Unlock()
	// selected 可先唤醒 Stop，但 close 必须等待 arm 完成才能碰原文交付。
	t.arm(deadline)
	close(t.armed)
	var once sync.Once
	return true, func(err error) { once.Do(func() { t.delivery <- wsDelivery{at: time.Now(), err: err} }) }
}

// 只由首次 policyEnd 与其 armed 后的 close owner 顺序调用，句柄在 close 中全部 join。
func (t *wsTermination) arm(deadline time.Time) {
	for _, c := range []*ws.Conn{t.downstream, t.upstream} {
		finish, _ := c.ArmCloseDeadline(deadline)
		if finish != nil {
			t.finishes = append(t.finishes, finish)
		}
	}
}

func (t *wsTermination) allowed(direction wsDirection, original bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.chosen || !t.wireSealed && t.end != nil && direction == wsUpstreamToClient && (original || t.end.PeerGrace)
}

func (t *wsTermination) write(dst *ws.Conn, direction wsDirection, m *ws.Message, original bool) error {
	if direction == wsUpstreamToClient {
		t.deliveryMu.Lock()
		defer t.deliveryMu.Unlock()
	}
	if !t.allowed(direction, original) {
		return context.Canceled
	}
	return dst.WriteMessage(m.Opcode, m.Payload)
}

func (t *wsTermination) close() error {
	t.closeOnce.Do(func() {
		<-t.selected
		t.mu.Lock()
		end, options, deadline, winner := t.end, t.options, t.deadline, t.winner
		t.mu.Unlock()
		if options.Policy != nil {
			options.Policy.Stop()
		}
		var code uint16
		var reason string
		if end == nil {
			code, reason, t.result = classifyWSRelay(winner)
		} else {
			<-t.armed
			code, reason, t.result = end.Code, end.Reason, end.Failure
			delivered := wsDelivery{at: time.Now()}
			if t.originalPending {
				timer := time.NewTimer(max(0, time.Until(deadline)))
				select {
				case delivered = <-t.delivery:
				case <-timer.C:
					delivered = wsDelivery{at: time.Now(), err: context.DeadlineExceeded}
				}
				timer.Stop()
			} else if end.Local != nil {
				delivered.err = t.deliverLocal(*end.Local)
				delivered.at = time.Now()
			}
			deadline = minWSDeadline(deadline, delivered.at.Add(t.closeBudget))
			t.mu.Lock()
			t.deadline = deadline
			t.mu.Unlock()
			t.arm(deadline)
			if end.PeerGrace && t.originalPending && delivered.err == nil {
				until := delivered.at.Add(min(100*time.Millisecond, max(0, deadline.Sub(delivered.at)/4)))
				t.waitPeer(until)
				t.mu.Lock()
				t.wireSealed = true
				if t.peer != nil && !t.peerAt.After(until) {
					code, reason = t.peer.Code, t.peer.Reason
				}
				t.mu.Unlock()
			}
		}
		t.mu.Lock()
		t.wireSealed = true
		t.mu.Unlock()
		t.closeConns(code, reason, deadline)
		for _, finish := range t.finishes {
			finish()
		}
		// 只有物理关闭 owner 借用/释放 reason；join 后的调用者只读安全结果。
		releaseWSRelayError(winner.err)
		if t.peer != nil {
			t.peer.Release()
		}
		t.mu.Lock()
		t.winner = wsRelayResult{}
		t.peer = nil
		t.mu.Unlock()
	})
	return t.result
}

func (t *wsTermination) waitPeer(until time.Time) {
	timer := time.NewTimer(max(0, time.Until(until)))
	defer timer.Stop()
	for {
		t.mu.Lock()
		ended := t.upstreamEnded
		t.mu.Unlock()
		if ended {
			return
		}
		select {
		case <-t.peerChanged:
		case <-timer.C:
			return
		}
	}
}

func (t *wsTermination) deliverLocal(local wsLocalTaskFailure) error {
	t.deliveryMu.Lock()
	defer t.deliveryMu.Unlock()
	// 本地生成也必须走进程预算，不能把漏传 Budget 当作无上限分配许可。
	if t.options.Budget == nil {
		return ws.ErrBufferLimit
	}
	size, err := dsi.TaskFailureSize(local.TaskID, local.Kind)
	if err != nil {
		return err
	}
	if int64(size) > t.options.MaxMessageBytes {
		return ws.ErrMessageTooLarge
	}
	m, err := ws.AllocateMessage(t.options.Budget, ws.OpText, size, func(dst []byte) error { return dsi.PutTaskFailure(dst, local.TaskID, local.Kind) })
	if err != nil {
		return err
	}
	defer m.Release()
	return t.downstream.WriteMessage(m.Opcode, m.Payload)
}

func minWSDeadline(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func releaseWSRelayError(err error) {
	var closed *ws.CloseError
	if errors.As(err, &closed) {
		closed.Release()
	}
}
