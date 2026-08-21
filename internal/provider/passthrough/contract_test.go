package passthrough

import (
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/provider/providertest"
)

// TestContractOpenAICompat 跑 openai.compat 侧的共享契约。
//
// passthrough 一个类型服务两个协议族，两族的契约差异是真实存在的
// （转发头白名单、流式信号位置、错误信封都不同），所以跑两次 Run，
// 而不是合成一个带变体的 Subject。
func TestContractOpenAICompat(t *testing.T) {
	providertest.Run(t, providertest.Subject{
		Name: "passthrough/openai.compat",
		Kind: degrade.ProviderOpenAICompat,
		New: func(t *testing.T, d providertest.Deps) provider.Provider {
			return New(degrade.ProviderOpenAICompat, "/v1/responses", d.HTTPClient, d.Now)
		},
		DefaultPath:       "/v1/responses",
		ValidBody:         `{"model":"logical","input":"hi"}`,
		RateLimitEnvelope: `{"error":{"type":"rate_limit_error","message":"slow down"}}`,
	})
}

// TestContractDashScopeNative 跑 dashscope.native 侧的共享契约。
//
// 这一侧有三个必须原样带走的租户/行为头：丢了 WorkSpace 请求会落到错误的
// 子租户，丢了 DataInspection / Async 会改变审查与异步语义。
func TestContractDashScopeNative(t *testing.T) {
	providertest.Run(t, providertest.Subject{
		Name: "passthrough/dashscope.native",
		Kind: degrade.ProviderDashScopeNative,
		New: func(t *testing.T, d providertest.Deps) provider.Provider {
			return New(degrade.ProviderDashScopeNative,
				dashscopenative.TextGenerationPath, d.HTTPClient, d.Now)
		},
		DefaultPath: dashscopenative.TextGenerationPath,
		ForwardedHeaders: []string{
			"X-DashScope-WorkSpace",
			"X-DashScope-DataInspection",
			"X-DashScope-Async",
		},
		StreamHeaders: map[string]string{
			dashscopenative.SSEHeader: "enable",
		},
		ValidBody:         `{"model":"logical","input":{"messages":[{"role":"user","content":"x"}]}}`,
		RateLimitEnvelope: `{"code":"Throttling.RateQuota","message":"slow down","request_id":"req-1"}`,
	})
}
