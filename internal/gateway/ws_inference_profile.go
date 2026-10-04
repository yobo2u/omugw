package gateway

import (
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/protocol/dashscopeinference"
	"github.com/yobo2u/omugw/internal/protocol/dashscopewire"
	dsiws "github.com/yobo2u/omugw/internal/provider/dashscopeinference"
)

func NewDashScopeInferenceHandler(d WSDeps) *WSHandler {
	// 配置投影不能由调用方后续改指针而换掉已经选定的连接身份。
	if d.InferenceTarget != nil {
		target := *d.InferenceTarget
		d.InferenceTarget = &target
	}
	return &WSHandler{d: d, profile: dashScopeInferenceProfile()}
}

func dashScopeInferenceProfile() wsProfile {
	return wsProfile{
		inbound:         degrade.Inbound{Protocol: degrade.ProtoDashScopeInference, Endpoint: degrade.EndpointDashScopeInference},
		outbound:        degrade.ProviderDashScopeWSInference,
		readyMode:       wsReadyHandshake,
		validateHeaders: dsiws.ValidateHeaders,
		encodeError:     dashscopewire.EncodeError,
		classifyClose:   dashscopeinference.ClassifyClose,
	}
}
