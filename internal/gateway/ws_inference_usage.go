package gateway

import (
	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/obs"
	dsi "github.com/yobo2u/omugw/internal/protocol/dashscopeinference"
)

// 小事实仍属于同一任务记录；已发布数值和终态完整性不能合成一个 fidelity 位。
type wsInferenceUsage struct {
	last, terminalUsage          dsi.UsageSnapshot
	frozen, settled, hasTerminal bool
	terminalFailed               bool
}

// observation 只在一次状态变更与锁外发布之间存活，不能成为第二本 ledger。
type wsInferenceObservation struct {
	delta      obs.WSUsageDelta
	hasDelta   bool
	unit       obs.WSUsageUnit
	fidelity   canonical.Fidelity
	diagnostic string
	failure    *canonical.Error
}

func (r *wsInferenceTask) observeUsage(f dsi.ServerFacts) wsInferenceObservation {
	u := &r.usage
	terminal := f.Event == "task-finished" || f.Event == "task-failed"
	o := wsInferenceObservation{unit: obs.WSUsageUnit(r.contract.Unit)}
	if u.settled {
		if terminal && u.hasTerminal && (u.terminalUsage != f.Usage || u.terminalFailed != (f.Event == "task-failed")) {
			o.diagnostic = "usage_conflict"
		}
		return o
	}
	if r.contract.Mode == dsi.ModeCumulative {
		s := f.Usage
		switch s.Presence {
		case dsi.Invalid:
			u.frozen = true
			o.diagnostic = "usage_invalid"
		case dsi.Value:
			if !u.frozen {
				old := u.last
				if s.InputTokens < old.InputTokens || s.OutputTokens < old.OutputTokens || s.Characters < old.Characters || s.Seconds < old.Seconds {
					u.frozen = true
					o.diagnostic = "usage_conflict"
				} else {
					o.hasDelta = old.Presence != dsi.Value || old != s
					o.delta = obs.WSUsageDelta{Unit: o.unit, InputTokens: s.InputTokens - old.InputTokens, OutputTokens: s.OutputTokens - old.OutputTokens, Characters: s.Characters - old.Characters, Seconds: s.Seconds - old.Seconds}
					u.last = s
				}
			}
		}
	}
	if terminal {
		u.settled, u.hasTerminal = true, true
		u.terminalUsage, u.terminalFailed = f.Usage, f.Event == "task-failed"
		o.fidelity = canonical.FidelityUnavailable
		switch {
		case r.contract.Mode != dsi.ModeCumulative:
			o.diagnostic = "usage_unverified"
		case !u.frozen && f.Usage.Presence == dsi.Value:
			o.fidelity = canonical.FidelityAuthoritative
		case !u.frozen:
			o.diagnostic = "usage_missing"
		}
	}
	return o
}

func (r *wsInferenceTask) finishUsage() wsInferenceObservation {
	if r.usage.settled {
		return wsInferenceObservation{}
	}
	r.usage.settled = true
	return wsInferenceObservation{unit: obs.WSUsageUnit(r.contract.Unit), fidelity: canonical.FidelityUnavailable, diagnostic: "usage_unfinished"}
}

// 调用时只持 publication 锁，不持状态锁；先差额后记录，Finish 不可越过先前发布。
func (p *wsInferencePolicy) publish(o wsInferenceObservation) {
	if p.metrics == nil {
		return
	}
	const protocol = "dashscope.inference"
	if o.hasDelta {
		p.metrics.ObserveWSUsageDelta(protocol, "task", o.delta)
		if o.delta.Unit == obs.UnitTokens {
			p.metrics.ObserveUsage(string(p.binding.Kind), canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: o.delta.InputTokens, OutputTokens: o.delta.OutputTokens})
		}
	}
	if o.fidelity != canonical.FidelityUnknown {
		p.metrics.ObserveWSUsageRecord(protocol, "task", o.unit, o.fidelity)
	}
	if o.diagnostic != "" {
		p.metrics.ObserveWSDiagnostic(protocol, o.diagnostic)
	}
	if o.failure != nil {
		p.metrics.ObserveError(string(p.binding.Kind), o.failure)
		p.metrics.ObserveWSDiagnostic(protocol, "task_failed")
	}
}
