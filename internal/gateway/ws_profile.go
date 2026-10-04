package gateway

import (
	"net/http"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/protocol/dashscoperealtime"
	"github.com/yobo2u/omugw/internal/protocol/dashscopewire"
	dsws "github.com/yobo2u/omugw/internal/provider/dashscoperealtime"
)

type wsReadyMode uint8

const (
	wsReadyEvent wsReadyMode = iota
	wsReadyHandshake
)

// 协议事实只由固定构造器绑定，防止身份、整门闸门、观测与关闭分类分别错绑。
// classifyClose 必须返回不借用 reason 的安全分类；负载与 reason 的释放仍归共享生命周期。
type wsProfile struct {
	inbound         degrade.Inbound
	outbound        degrade.Provider
	readyMode       wsReadyMode
	validateHeaders func(http.Header) error
	checkReady      func([]byte) error
	encodeError     func(*canonical.Error) (int, []byte, map[string]string)
	newObserver     func(*obs.Metrics, string, string) wsEventObserver
	classifyClose   func(uint16, string) *canonical.Error
}

func dashScopeRealtimeProfile() wsProfile {
	return wsProfile{
		inbound:         degrade.Inbound{Protocol: degrade.ProtoDashScopeRealtime, Endpoint: degrade.EndpointDashScopeRealtime},
		outbound:        degrade.ProviderDashScopeWSRealtime,
		validateHeaders: dsws.ValidateHeaders,
		checkReady: func(payload []byte) error {
			e, err := dashscoperealtime.Inspect(payload)
			if err == nil && e.Type == "session.created" {
				return nil
			}
			if err == nil && e.Failure != nil {
				return e.Failure
			}
			return errWSRelayPolicy
		},
		encodeError: dashscopewire.EncodeError,
		newObserver: func(m *obs.Metrics, inbound, outbound string) wsEventObserver {
			return &dashScopeWSObserver{usage: newWSUsage(m, inbound, outbound)}
		},
		classifyClose: dashscoperealtime.ClassifyClose,
	}
}
