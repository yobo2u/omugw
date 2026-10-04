package gateway

import (
	"errors"

	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/router"
)

var errWSInferenceBinding = errors.New("websocket inference: invalid model binding")

type wsInferenceBinding struct {
	Kind                              degrade.Provider
	Endpoint, BaseURL, CredentialPool string
}

func newWSInferenceBinding(target router.Target) (wsInferenceBinding, error) {
	if target.Kind != degrade.ProviderDashScopeWSInference || target.Endpoint == "" || target.BaseURL == "" || target.CredentialPool == "" || target.UpstreamModel != "" || target.NativeEndpoint != "" {
		return wsInferenceBinding{}, errWSInferenceBinding
	}
	return wsInferenceBinding{target.Kind, target.Endpoint, target.BaseURL, target.CredentialPool}, nil
}

// model 原字节要去固定连接；别名、同族的另一部署或只开部分能力的门都不能顶替。
func validateWSInferenceModel(rt *router.Router, matrix *degrade.Matrix, binding wsInferenceBinding, model string) error {
	if rt == nil || matrix == nil || binding.Kind != degrade.ProviderDashScopeWSInference {
		return errWSInferenceBinding
	}
	targets, err := rt.Resolve(model)
	if err != nil {
		return errWSInferenceBinding
	}
	for _, target := range targets {
		if target.Kind != binding.Kind || target.Endpoint != binding.Endpoint || target.BaseURL != binding.BaseURL || target.CredentialPool != binding.CredentialPool || target.UpstreamModel != model || target.NativeEndpoint != "" {
			continue
		}
		in := degrade.Inbound{Protocol: degrade.ProtoDashScopeInference, Endpoint: degrade.EndpointDashScopeInference}
		if _, err := matrix.Check(in, binding.Kind, degrade.ExpressibleSet(in.Protocol)); err == nil {
			return nil
		}
	}
	return errWSInferenceBinding
}
