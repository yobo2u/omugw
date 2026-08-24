package dashscopecompat

import (
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/provider/providertest"
)

// TestContract 跑 dashscope.compatible 的共享契约。
//
// 这条路是 wire-compatible 而非同源：复用 Chat 线格式只说明不需要重编码，
// 语义仍是异构的。契约层面它与其他适配器没有区别——一个客户端头都不转发。
func TestContract(t *testing.T) {
	providertest.Run(t, providertest.Subject{
		Name: "dashscopecompat",
		Kind: degrade.ProviderDashScopeCompatible,
		New: func(t *testing.T, d providertest.Deps) provider.Provider {
			return New(d.HTTPClient, d.Now)
		},
		InboundProtocol:   degrade.ProtoOpenAIChat,
		DefaultEndpoint:   ChatCompletionsPath,
		ValidBody:         `{"model":"logical","messages":[{"role":"user","content":"hi"}]}`,
		RateLimitEnvelope: `{"error":{"type":"rate_limit_error","message":"slow down"}}`,
	})
}
