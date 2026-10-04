package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	dsi "github.com/yobo2u/omugw/internal/protocol/dashscopeinference"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 仅表示 policy 已封口，不是生命周期取消；原 End/写失败 owner 仍负责认领终止。
// 必须与 context.Canceled 不可混淆，否则唤醒的对向 worker 会抢走失败文本与结果。
var errWSPolicyStopped = errors.New("websocket policy stopped")

type wsDirection uint8

const (
	wsClientToUpstream wsDirection = iota
	wsUpstreamToClient
)

type wsForwardStep uint8

const (
	wsStepNone wsForwardStep = iota
	wsStepRun
	wsStepStarted
	wsStepFinish
	wsStepTerminal
	wsStepFailed
)

type wsForwardTicket struct {
	Generation uint64
	Step       wsForwardStep
}

type wsForwardAction uint8

const (
	wsForward wsForwardAction = iota
	wsForwardThenEnd
	wsRejectThenEnd
)

type wsLocalTaskFailure struct {
	TaskID string
	Kind   dsi.LocalFailureKind
}

type wsPolicyEnd struct {
	At        time.Time
	Failure   error
	Code      uint16
	Reason    string
	Local     *wsLocalTaskFailure
	PeerGrace bool
}

type wsForwardDecision struct {
	Ticket wsForwardTicket
	Action wsForwardAction
	End    *wsPolicyEnd
}

type wsPolicyDeadline struct {
	At       time.Time
	Revision uint64
}

// 消息只借用至 After 返回；policy 不能持有负载或另开网络读写生命周期。
type wsForwardPolicy interface {
	BeforeForward(context.Context, wsDirection, ws.Opcode, []byte) (wsForwardDecision, error)
	AfterForward(wsForwardTicket, error)
	Deadline() wsPolicyDeadline
	Changed() <-chan struct{}
	Expire(wsPolicyDeadline, time.Time) *wsPolicyEnd
	Stop()
	Finish()
}

type wsRelayOptions struct {
	Observer        wsEventObserver
	Policy          wsForwardPolicy
	ClassifyClose   func(uint16, string) *canonical.Error
	Timeouts        config.Timeouts
	Budget          *ws.BufferBudget
	MaxMessageBytes int64
}

func wsCloseBudget(t config.Timeouts) time.Duration { return min(time.Second, t.Connect, t.Idle) }

// 单一 timer 只认领到期结果；交付与网络收尾始终归原 termination owner。
func superviseWSPolicy(p wsForwardPolicy, t *wsTermination) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		d := p.Deadline()
		var tick <-chan time.Time
		if !d.At.IsZero() {
			timer.Reset(max(0, time.Until(d.At)))
			tick = timer.C
		} else {
			timer.Stop()
		}
		select {
		case <-t.selected:
			return
		case <-p.Changed():
		case <-tick:
			if end := p.Expire(d, time.Now()); end != nil {
				t.policyEnd(*end, false)
			}
		}
	}
}
