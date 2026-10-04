package gateway

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

var (
	errWSRegistryFull   = &wsRegistryError{http.StatusTooManyRequests, "websocket session capacity exhausted"}
	errWSRegistrySealed = &wsRegistryError{http.StatusServiceUnavailable, "websocket registry is shutting down"}
	errWSShutdown       = errors.New("websocket server shutdown")
)

// 独立于上游错误分类，避免本地准入失败触发凭据冷却。
type wsRegistryError struct {
	status  int
	message string
}

func (e *wsRegistryError) Error() string   { return e.message }
func (e *wsRegistryError) HTTPStatus() int { return e.status }

type wsRegistry struct {
	mu          sync.Mutex
	max         int
	closeBudget time.Duration
	sealed      bool
	sessions    map[*wsSession]struct{}
	drained     chan struct{}
	stopped     chan struct{}
}

type wsShutdownKey struct{}

type wsShutdownNotice struct {
	started     chan struct{}
	finished    chan struct{}
	termination *wsTermination
}

type wsSession struct {
	registry *wsRegistry
	ctx      context.Context
	cancel   context.CancelCauseFunc
	shutdown wsShutdownNotice
	// 受 registry.mu 保护；重试拨号所得连接也必须纳入关停，不能只记最后一段。
	conns    map[*ws.Conn]struct{}
	finished bool
	done     sync.Once
}

func newWSRegistry(maxSessions int, closeBudget time.Duration) *wsRegistry {
	return &wsRegistry{max: maxSessions, closeBudget: closeBudget, sessions: make(map[*wsSession]struct{}), drained: make(chan struct{}), stopped: make(chan struct{})}
}

func (r *wsRegistry) Register(parent context.Context) (*wsSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return nil, errWSRegistrySealed
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	if len(r.sessions) >= r.max {
		return nil, errWSRegistryFull
	}
	ctx, cancel := context.WithCancelCause(parent)
	s := &wsSession{registry: r, cancel: cancel, shutdown: wsShutdownNotice{started: make(chan struct{}), finished: make(chan struct{})}, conns: make(map[*ws.Conn]struct{})}
	s.shutdown.termination = newWSTermination(func(code uint16, reason string, deadline time.Time) {
		r.mu.Lock()
		conns := s.connectionsLocked()
		r.mu.Unlock()
		closeWSConnections(conns, code, reason, deadline)
	}, r.closeBudget)
	// 先通知 relay 关停原因，再有限关闭，最后取消 pending 读；直接先 cancel 会让
	// transport 的读取消回调抢先断 TCP，使本应发出的 1001 退化成 1006。
	s.ctx = context.WithValue(ctx, wsShutdownKey{}, &s.shutdown)
	r.sessions[s] = struct{}{}
	return s, nil
}

func (s *wsSession) Context() context.Context { return s.ctx }

func (s *wsSession) Attach(c *ws.Conn) bool {
	if c == nil {
		return false
	}
	r := s.registry
	r.mu.Lock()
	_, owned := s.conns[c]
	accepted := !r.sealed && !s.finished && s.ctx.Err() == nil
	select {
	case <-s.shutdown.termination.selected:
		accepted = false
	default:
	}
	if accepted {
		s.conns[c] = struct{}{}
	}
	r.mu.Unlock()
	if !accepted {
		if owned {
			// 迟到的重复 Attach 也不能绕过已登记连接的关闭 owner，抢发 1001。
			s.shutdown.termination.report(wsRelayResult{err: context.Canceled})
			_ = s.shutdown.termination.close()
		} else {
			// 迟到的新段也继承 session 首次选择的绝对 B，不能从 Attach 再续一段预算。
			termination := s.shutdown.termination
			termination.report(wsRelayResult{err: context.Canceled})
			termination.mu.Lock()
			deadline := termination.deadline
			termination.mu.Unlock()
			closeWSConnections([]*ws.Conn{c}, ws.CloseGoingAway, "", deadline)
		}
	}
	return accepted
}

// Done 只能在 handler 已 join 所有 worker 后调用；取消不是名额归还的证据。
func (s *wsSession) Done() {
	s.done.Do(func() {
		r := s.registry
		r.mu.Lock()
		s.finished = true
		r.mu.Unlock()
		s.shutdown.termination.report(wsRelayResult{err: context.Canceled})
		_ = s.shutdown.termination.close()
		s.cancel(context.Canceled)
		r.mu.Lock()
		s.conns = nil
		delete(r.sessions, s)
		if r.sealed && len(r.sessions) == 0 {
			close(r.drained)
		}
		r.mu.Unlock()
	})
}

func (s *wsSession) connectionsLocked() []*ws.Conn {
	conns := make([]*ws.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	return conns
}

// Shutdown 的调用期限只限制等待；封口后即使调用方超时，仍须完成关连接与排空。
func (r *wsRegistry) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	if !r.sealed {
		r.sealed = true
		var closing sync.WaitGroup
		for s := range r.sessions {
			close(s.shutdown.started)
			// 与 active relay 共用第一次原因和物理 close 权；不能另发 1001
			// 抢在已胜出的故障关闭前。report 不做分类/网络操作，锁内只封口。
			s.shutdown.termination.report(wsRelayResult{err: errWSShutdown})
			closing.Add(1)
			// 数量受会话准入上限约束；慢 active 不能串行拖延其余 pending 的取消。
			go func() {
				defer closing.Done()
				_ = s.shutdown.termination.close()
				s.cancel(errWSShutdown)
				close(s.shutdown.finished)
			}()
		}
		if len(r.sessions) == 0 {
			close(r.drained)
		}
		go func() {
			closing.Wait()
			<-r.drained
			close(r.stopped)
		}()
	}
	r.mu.Unlock()
	select {
	case <-r.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// 两个固定关闭工作者共用同一绝对期限，防止每段 Close 再起一轮预算。
func closeWSConnections(conns []*ws.Conn, code uint16, reason string, deadline time.Time) {
	var finishes []func()
	for _, c := range conns {
		finish, _ := c.ArmCloseDeadline(deadline)
		if finish != nil {
			finishes = append(finishes, finish)
		}
	}
	var wg sync.WaitGroup
	for worker := 0; worker < min(2, len(conns)); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := worker; i < len(conns); i += 2 {
				_, _ = conns[i].CloseWithResult(code, reason)
			}
		}()
	}
	wg.Wait()
	for _, finish := range finishes {
		finish()
	}
}
