package degrade

import (
	"reflect"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// redeemedChatDSCompat 是 /v1/chat/completions 门在 dashscope.compatible 上
// 兑现的九项能力，按 AllCapabilities 顺序——与 RedeemedAt 的输出顺序一致。
var redeemedChatDSCompat = []canonical.Capability{
	canonical.CapTextGeneration,
	canonical.CapStreaming,
	canonical.CapToolCalling,
	canonical.CapParallelToolCalls,
	canonical.CapStructuredOutput,
	canonical.CapReasoning,
	canonical.CapVisionInput,
	canonical.CapAudioInput,
	canonical.CapWebSearch,
}

// TestChatDSCompatRouteIsWireCompatibleNotHomogeneous 钉死这条路径的身份：
// wire-compatible 只说明不需要重编码，不能推导为语义零损失——
// 它不是同源快通道，不享受快通道的选路特权，降级头由矩阵照常生成。
func TestChatDSCompatRouteIsWireCompatibleNotHomogeneous(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := m.Route(ProtoOpenAIChat, ProviderDashScopeCompatible)
	if !ok {
		t.Fatal("openai.chat -> dashscope.compatible 未注册")
	}
	if r.IsHomogeneous() {
		t.Error("该路径是 wire-compatible 异构转换，不得标记为同源快通道")
	}

	// 设计处置：7 PASS + 2 DEGRADE + 2 REJECT = 11 项可表达能力，设计分 8/11。
	p := r.Preservation(m.Availability(), Endpoint(""))
	if p.Passthrough != 7 || p.Degrade != 2 || p.Reject != 2 {
		t.Errorf("设计处置 = pass %d deg %d rej %d，期望 7/2/2",
			p.Passthrough, p.Degrade, p.Reject)
	}
	if want := 8.0 / 11.0; p.DesignScore() != want {
		t.Errorf("设计保留度 = %.3f，期望 %.3f（8/11）", p.DesignScore(), want)
	}
}

// TestChatDSCompatRedemptionIsExactlyNineCapabilities 钉死兑现集合的精确形状：
// 九项可交付能力；file_input / audio_output 是 REJECT，不在兑现之列。
func TestChatDSCompatRedemptionIsExactlyNineCapabilities(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := m.Route(ProtoOpenAIChat, ProviderDashScopeCompatible)
	if !ok {
		t.Fatal("openai.chat -> dashscope.compatible 未注册")
	}

	if got := r.RedeemedAt(EndpointOpenAIChat); !reflect.DeepEqual(got, redeemedChatDSCompat) {
		t.Errorf("兑现集合 = %v，期望 %v", got, redeemedChatDSCompat)
	}
	for _, c := range []canonical.Capability{canonical.CapFileInput, canonical.CapAudioOutput} {
		if r.Redeems(EndpointOpenAIChat, c) {
			t.Errorf("%q 是 REJECT，不应被兑现", c)
		}
	}

	// 这门此刻九项全兑：可用分与设计分合一，都是 8/11，没有未投放格子。
	p := r.Preservation(m.Availability(), EndpointOpenAIChat)
	if want := 8.0 / 11.0; p.AvailableScore() != want {
		t.Errorf("门 %s 可用分 = %.3f，期望 %.3f", EndpointOpenAIChat, p.AvailableScore(), want)
	}
	if p.Gated() {
		t.Error("九项可交付能力已全部兑现，这门不应再有未投放格子")
	}
}

// TestChatDoorRankingPrefersHomogeneousFastPath 钉死同门选路：
// /v1/chat/completions 这扇门同时被 openai.compat（同源，可用 1.000）与
// dashscope.compatible（wire-compatible，可用 8/11）兑现，选路必须同源优先。
func TestChatDoorRankingPrefersHomogeneousFastPath(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	ranked := m.RankOutbound(ProtoOpenAIChat, []Provider{
		ProviderDashScopeCompatible,
		ProviderOpenAICompat,
	})
	if len(ranked) != 2 || ranked[0] != ProviderOpenAICompat || ranked[1] != ProviderDashScopeCompatible {
		t.Fatalf("同门选路顺序 = %v，期望 [openai.compat dashscope.compatible]", ranked)
	}

	fast := mustRoute(t, m, ProtoOpenAIChat, ProviderOpenAICompat)
	compat := mustRoute(t, m, ProtoOpenAIChat, ProviderDashScopeCompatible)
	fs := fast.Preservation(m.Availability(), EndpointOpenAIChat).AvailableScore()
	cs := compat.Preservation(m.Availability(), EndpointOpenAIChat).AvailableScore()
	if fs <= cs {
		t.Errorf("同源门可用分 %.3f 应严格高于 wire-compatible 门 %.3f", fs, cs)
	}
}
