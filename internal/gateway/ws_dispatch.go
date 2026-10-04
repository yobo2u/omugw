package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

type wsReady struct {
	conn    *ws.Conn
	initial *ws.Message
	lease   *credential.Lease
}

func (h *WSHandler) connect(ctx context.Context, session *wsSession, model string, header http.Header) (*wsReady, string, error) {
	outbound := ""
	targets, err := h.d.Router.Resolve(model)
	if err != nil {
		return nil, outbound, safeWSError(err)
	}
	var last error
	for _, target := range targets {
		// 同名事件及历史 homogeneous 标记不是同契约证据，不走跨协议偏好排序。
		if target.Kind != h.profile.outbound {
			continue
		}
		if target.UpstreamModel != model {
			continue
		}
		if _, err := h.d.Matrix.Check(h.inbound(), target.Kind, degrade.ExpressibleSet(h.inbound().Protocol)); err != nil {
			last = err
			continue
		}
		p, pool := h.d.Providers[target.Endpoint], h.d.Pools[target.CredentialPool]
		if p == nil || p.Kind() != target.Kind || pool == nil {
			last = canonical.Newf(canonical.ClassInternal, "Realtime 出站未装配")
			continue
		}
		tried := map[string]bool{}
		for {
			if err := wsHandshakeContextError(ctx); err != nil {
				return nil, outbound, err
			}
			lease, err := pool.Acquire(tried)
			if err != nil {
				if last == nil {
					last = safeWSError(err)
				}
				break
			}
			tried[lease.Credential.ID] = true
			outbound = string(target.Kind)
			conn, resp, err := p.Dial(ctx, provider.Request{Target: target, Credential: lease.Credential, Inbound: h.inbound(), Header: header, Stream: true})
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if conn != nil && !session.Attach(conn) {
				releaseWSRelayError(err)
				err = wsSessionError(session)
				settleWSLease(lease, err)
				return nil, outbound, err
			}
			var initial *ws.Message
			if err == nil && conn == nil {
				err = canonical.Newf(canonical.ClassInternal, "Realtime 上游未返回连接")
			}
			if err == nil {
				initial, err = readWSReady(ctx, conn, h.profile.checkReady)
			}
			if err == nil {
				return &wsReady{conn: conn, initial: initial, lease: lease}, outbound, nil
			}
			failure := wsUpstreamResult(err, h.profile.classifyClose)
			last = wsAttemptError(failure)
			if conn != nil {
				retireWSAttempt(session, conn, failure)
			} else {
				releaseWSRelayError(err)
			}
			// 客户端取消、关停与本地共同预算到期，优先于 provider 的可重试猜测。
			if stopped := wsHandshakeContextError(ctx); stopped != nil {
				last = stopped
			}
			settleWSLease(lease, last)
			if h.d.Metrics != nil {
				var upstream *canonical.Error
				if errors.As(last, &upstream) {
					h.d.Metrics.ObserveError(string(target.Kind), upstream)
				}
			}
			if ctx.Err() != nil || errors.Is(last, ws.ErrBufferLimit) {
				return nil, outbound, last
			}
			var known *canonical.Error
			if !errors.As(last, &known) || !known.Retryable {
				break
			}
		}
	}
	if last == nil {
		last = canonical.Newf(canonical.ClassBadRequest, "没有匹配真实模型的同协议 Realtime 目标")
	}
	return nil, outbound, last
}

// 只预读一条应用消息；ping 由 transport 消化且不能延长 ctx 的共同截止时间。
func readWSReady(ctx context.Context, conn *ws.Conn, checkReady func([]byte) error) (*ws.Message, error) {
	m, err := conn.ReadOwnedMessage(ctx)
	if err != nil {
		return nil, err
	}
	err = errWSRelayPolicy
	if m.Opcode == ws.OpText {
		err = checkReady(m.Payload)
		if err == nil {
			return m, nil
		}
	}
	m.Release()
	return nil, err
}

// 失败尝试尚无下游段：在 registry 锁内把该段交给尝试级仲裁器，或者加入已经
// 胜出的 session 关停。不能直接 defer Close(1000/1001)，也不能把可重试失败
// 报给整个 session 的一次性终止槽（那会拒绝下一次 Attach）。Done 会等本函数返回。
func retireWSAttempt(s *wsSession, conn *ws.Conn, failure wsRelayResult) {
	r := s.registry
	r.mu.Lock()
	termination := s.shutdown.termination
	select {
	case <-termination.selected:
	default:
		delete(s.conns, conn)
		termination = newWSTermination(func(code uint16, reason string, deadline time.Time) {
			closeWSConnections([]*ws.Conn{conn}, code, reason, deadline)
		}, r.closeBudget)
	}
	r.mu.Unlock()
	termination.report(failure)
	_ = termination.close()
}

func wsAttemptError(failure wsRelayResult) error {
	var known *canonical.Error
	if errors.As(failure.err, &known) {
		return safeWSError(known)
	}
	_, _, classified := classifyWSRelay(failure)
	if classified == nil {
		return canonical.Newf(canonical.ClassInternal, "Realtime 上游未就绪即关闭")
	}
	return classified
}

func wsHandshakeContextError(ctx context.Context) error {
	// registry 先发 close 再取消 ctx；两者之间也已封口，不能再拨号或罚凭据。
	if notice, _ := ctx.Value(wsShutdownKey{}).(*wsShutdownNotice); notice != nil {
		select {
		case <-notice.started:
			return errWSShutdown
		default:
		}
	}
	if ctx.Err() == nil {
		return nil
	}
	if errors.Is(context.Cause(ctx), errWSShutdown) {
		return errWSShutdown
	}
	return ctx.Err()
}

func wsSessionError(s *wsSession) error {
	if err := wsHandshakeContextError(s.Context()); err != nil {
		return err
	}
	return errWSShutdown
}

// Lease 仅由 handler 协调者结算；未知断流结束借用但不猜 auth，取消也不冷却。
func settleWSLease(lease *credential.Lease, err error) {
	if err == nil {
		lease.Succeed()
		return
	}
	var known *canonical.Error
	if errors.As(err, &known) {
		lease.Fail(known)
		return
	}
	lease.Fail(canonical.Newf(canonical.ClassInternal, "Realtime 会话结束"))
}
