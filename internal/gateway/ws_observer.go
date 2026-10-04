package gateway

import (
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/protocol/dashscoperealtime"
)

// Observe 只借用上游文本，在转发/Release 前完成；Finish 只能在上游 reader join 后调用。
// 返回值用于本地包络/关联策略，不能把合法的上游 error 事件变成吞掉负载的控制信号。
type wsEventObserver interface {
	Observe([]byte) error
	Finish()
}

type dashScopeWSObserver struct{ usage *wsUsage }

func (o *dashScopeWSObserver) Observe(payload []byte) error {
	e, err := dashscoperealtime.Inspect(payload)
	if err != nil {
		return errWSRelayPolicy
	}
	if o.usage != nil {
		return o.usage.Observe(dashScopeUsageEvent(e))
	}
	return nil
}

func dashScopeUsageEvent(e dashscoperealtime.Event) wsUsageEvent {
	// DS Inspect 没有可选明细的 presence；保持既有仅发布正音频分项的行为。
	return wsUsageEvent{
		Source: e.Source, ID: e.ID, Started: e.Started, Terminal: e.Terminal,
		Usage: e.Usage, Characters: e.Characters, Diagnostic: e.Diagnostic, Failure: e.Failure,
		Details: obs.WSTokenDetails{
			AudioInput:  obs.WSTokenCount{Value: e.Usage.AudioInputTokens, Present: e.Usage.AudioInputTokens > 0},
			AudioOutput: obs.WSTokenCount{Value: e.Usage.AudioOutputTokens, Present: e.Usage.AudioOutputTokens > 0},
		},
	}
}

func (o *dashScopeWSObserver) Finish() {
	if o.usage != nil {
		o.usage.Finish()
	}
}
