package degrade

import (
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// 防止 WS 门按另一种入站协议的可表达性被裁决；只构建本地路径，不给 Phase1 投放。
func TestWSKnownDoorOwnership(t *testing.T) {
	doors := []struct {
		name     string
		endpoint Endpoint
		path     Endpoint
		owner    Protocol
	}{
		{"openai realtime", EndpointOpenAIRealtime, "/v1/realtime", ProtoOpenAIRealtime},
		{"dashscope realtime", EndpointDashScopeRealtime, "/api-ws/v1/realtime", ProtoDashScopeRealtime},
		{"dashscope inference", EndpointDashScopeInference, "/api-ws/v1/inference", ProtoDashScopeInference},
	}
	protocols := []Protocol{
		ProtoOpenAIChat, ProtoOpenAIResponses, ProtoOpenAIRealtime,
		ProtoDashScopeNative, ProtoDashScopeRealtime, ProtoDashScopeInference,
	}
	for _, door := range doors {
		t.Run(door.name, func(t *testing.T) {
			// 字面路径也必须受归属检查，防止常量写错后测试只检查同一个错误值。
			for _, ep := range []Endpoint{door.endpoint, door.path} {
				for _, in := range protocols {
					t.Run(string(ep)+"/"+string(in), func(t *testing.T) {
						r, err := NewRoute(in, ProviderOpenAICompat).
							Pass(ExpressibleSet(in)...).
							Redeem(ep, canonical.CapTextGeneration).
							Build()
						if in == door.owner {
							if err != nil {
								t.Fatalf("WS 门兑现给归属协议应通过: %v", err)
							}
							if !r.Redeems(door.path, canonical.CapTextGeneration) {
								t.Fatal("兑现未落在设计的 WS 门路径上")
							}
							return
						}
						if err == nil {
							t.Fatal("WS 门错绑未拒绝")
						}
						for _, want := range []string{string(door.path), string(door.owner), string(in)} {
							if !strings.Contains(err.Error(), want) {
								t.Errorf("错绑错误应包含 %q，实际: %v", want, err)
							}
						}
					})
				}
			}
		})
	}
	t.Run("unknown synthetic door", func(t *testing.T) {
		_, err := NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
			Pass(ExpressibleSet(ProtoOpenAIChat)...).
			Redeem(Endpoint("/v1/synthetic-door"), canonical.CapTextGeneration).
			Build()
		if err != nil {
			t.Fatalf("归属表不是准入名单，未知合成门应通过: %v", err)
		}
	})
}
