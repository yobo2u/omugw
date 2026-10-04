package gateway

import "github.com/yobo2u/omugw/internal/protocol/dashscoperealtime"

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
		return o.usage.Observe(e)
	}
	return nil
}

func (o *dashScopeWSObserver) Finish() {
	if o.usage != nil {
		o.usage.Finish()
	}
}
