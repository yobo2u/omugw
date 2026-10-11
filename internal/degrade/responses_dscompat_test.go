package degrade

import (
	"reflect"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

func responsesDSCompatRoute(t *testing.T) (*Matrix, *Route) {
	t.Helper()
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := m.Route(ProtoOpenAIResponses, ProviderDashScopeCompatible)
	if !ok {
		t.Fatal("openai.responses -> dashscope.compatible 未注册")
	}
	return m, r
}

// TestResponsesDSCompatDispositions 逐格钉死这条路径的设计处置。
//
// 每一格都对应 2026-10-11 的一条实测（见
// docs/research/2026-10-11-dashscope-compatible-responses-reprobe.md），
// 改任何一格都该先有新证据。
func TestResponsesDSCompatDispositions(t *testing.T) {
	m, _ := responsesDSCompatRoute(t)

	want := map[canonical.Capability]Disposition{
		canonical.CapTextGeneration:       Passthrough,
		canonical.CapStreaming:            Passthrough,
		canonical.CapToolCalling:          Passthrough,
		canonical.CapParallelToolCalls:    Passthrough,
		canonical.CapReasoning:            Passthrough,
		canonical.CapVisionInput:          Passthrough,
		canonical.CapAudioInput:           Passthrough,
		canonical.CapStatefulConversation: Passthrough,
		canonical.CapImageDetail:          Degrade,
		canonical.CapWebSearch:            Degrade,
		canonical.CapStructuredOutput:     Degrade,
		canonical.CapFileInput:            Reject,
		canonical.CapAudioOutput:          Reject,
		canonical.CapComputerUse:          Reject,
		canonical.CapImageGeneration:      Reject,
	}
	if len(want) != len(ExpressibleSet(ProtoOpenAIResponses)) {
		t.Fatalf("期望表覆盖 %d 项，Responses 可表达 %d 项——两者必须一一对应",
			len(want), len(ExpressibleSet(ProtoOpenAIResponses)))
	}
	for c, d := range want {
		rule, ok := m.Lookup(ProtoOpenAIResponses, ProviderDashScopeCompatible, c)
		if !ok {
			t.Errorf("%s 没有声明", c)
			continue
		}
		if rule.Disposition != d {
			t.Errorf("%s 处置 = %s，期望 %s", c, rule.Disposition, d)
		}
	}
}

// TestResponsesDSCompatDesignScore：8 PASS + 3 DEGRADE + 4 REJECT，
// 设计分 (8 + 0.5×3) / 15。
func TestResponsesDSCompatDesignScore(t *testing.T) {
	m, r := responsesDSCompatRoute(t)

	if r.IsHomogeneous() {
		t.Error("该路径是 wire-compatible 异构转换，不得标记为同源快通道")
	}
	p := r.Preservation(m.Availability(), Endpoint(""))
	if p.Passthrough != 8 || p.Degrade != 3 || p.Reject != 4 || p.Emulate != 0 || p.EmulateOff != 0 {
		t.Errorf("设计处置 = pass %d deg %d rej %d emu %d emuOff %d，期望 8/3/4/0/0",
			p.Passthrough, p.Degrade, p.Reject, p.Emulate, p.EmulateOff)
	}
	if want := 9.5 / 15.0; p.DesignScore() != want {
		t.Errorf("设计保留度 = %.3f，期望 %.3f（9.5/15）", p.DesignScore(), want)
	}
}

// TestResponsesDSCompatSessionNoteStatesBoundaries 防的是 PASS 让人以为会话
// 与 OpenAI 等价：归属、留存与多 key 续轮三条边界必须写在 note 里。
func TestResponsesDSCompatSessionNoteStatesBoundaries(t *testing.T) {
	m, _ := responsesDSCompatRoute(t)
	rule, _ := m.Lookup(ProtoOpenAIResponses, ProviderDashScopeCompatible, canonical.CapStatefulConversation)

	if rule.RequiresFeature != "" {
		t.Errorf("透传的会话不该依赖运行时开关，实际依赖 %q", rule.RequiresFeature)
	}
	for _, kw := range []string{"上游", "不隔离", "7 天", "400"} {
		if !strings.Contains(rule.Note, kw) {
			t.Errorf("会话 note 缺少关键词 %q: %s", kw, rule.Note)
		}
	}
}

// TestOtherResponsesRoutesStillEmulateSession 防的是拆分时顺手改了别的路径：
// 另外三条 Responses 路径的会话处置不在本次范围（openai.compat 见 #21）。
func TestOtherResponsesRoutesStillEmulateSession(t *testing.T) {
	m, _ := responsesDSCompatRoute(t)

	for _, out := range []Provider{ProviderOpenAICompat, ProviderDashScopeNative, ProviderAnthropicMessages} {
		rule, ok := m.Lookup(ProtoOpenAIResponses, out, canonical.CapStatefulConversation)
		if !ok {
			t.Errorf("openai.responses -> %s 未声明会话处置", out)
			continue
		}
		if rule.Disposition != Emulate || rule.RequiresFeature != FeatureConversationStore {
			t.Errorf("openai.responses -> %s 会话处置 = %s（开关 %q），期望 EMULATE + convstore",
				out, rule.Disposition, rule.RequiresFeature)
		}
		// note 只该描述网关做了什么，不替上游下断言：同一句话挂在 openai.compat
		// 上时，「上游无服务端会话」对 OpenAI 本尊就是错的。
		if strings.Contains(rule.Note, "上游无服务端会话") {
			t.Errorf("openai.responses -> %s 的会话 note 仍在替上游下断言: %s", out, rule.Note)
		}
	}
}

// TestResponsesDSCompatBuiltinToolsUnchanged 防的是拆分时丢了内建工具处置。
func TestResponsesDSCompatBuiltinToolsUnchanged(t *testing.T) {
	m, _ := responsesDSCompatRoute(t)

	for _, out := range []Provider{ProviderDashScopeCompatible, ProviderDashScopeNative, ProviderAnthropicMessages} {
		for _, c := range []canonical.Capability{canonical.CapComputerUse, canonical.CapImageGeneration} {
			rule, _ := m.Lookup(ProtoOpenAIResponses, out, c)
			if rule.Disposition != Reject {
				t.Errorf("openai.responses -> %s 的 %s = %s，期望 REJECT", out, c, rule.Disposition)
			}
		}
	}
	for _, c := range []canonical.Capability{canonical.CapComputerUse, canonical.CapImageGeneration} {
		rule, _ := m.Lookup(ProtoOpenAIResponses, ProviderOpenAICompat, c)
		if rule.Disposition != Passthrough {
			t.Errorf("同源路径的 %s = %s，期望 PASSTHROUGH", c, rule.Disposition)
		}
	}
}

// redeemedResponsesDSCompat 是 /v1/responses 门在 dashscope.compatible 上兑现
// 的十项能力，按 AllCapabilities 顺序——与 RedeemedAt 的输出顺序一致。
var redeemedResponsesDSCompat = []canonical.Capability{
	canonical.CapTextGeneration,
	canonical.CapStreaming,
	canonical.CapToolCalling,
	canonical.CapParallelToolCalls,
	canonical.CapStructuredOutput,
	canonical.CapReasoning,
	canonical.CapVisionInput,
	canonical.CapImageDetail,
	canonical.CapStatefulConversation,
	canonical.CapWebSearch,
}

// TestResponsesDSCompatRedemptionIsExactlyTenCapabilities 钉死兑现集合：
// 7 PASS + 3 DEGRADE。audio_input 设计处置是 PASS 但没有这扇门的证据，
// 不在其列——含它的请求由矩阵以 501 拦下。
func TestResponsesDSCompatRedemptionIsExactlyTenCapabilities(t *testing.T) {
	m, r := responsesDSCompatRoute(t)

	if eps := r.Endpoints(); len(eps) != 1 || eps[0] != EndpointOpenAIResponses {
		t.Fatalf("已开门 = %v，期望仅 %s", eps, EndpointOpenAIResponses)
	}
	if got := r.RedeemedAt(EndpointOpenAIResponses); !reflect.DeepEqual(got, redeemedResponsesDSCompat) {
		t.Errorf("兑现集合 = %v\n期望 %v", got, redeemedResponsesDSCompat)
	}
	if r.Redeems(EndpointOpenAIResponses, canonical.CapAudioInput) {
		t.Error("audio_input 没有证据，不得兑现")
	}

	p := r.Preservation(m.Availability(), EndpointOpenAIResponses)
	if !p.Gated() {
		t.Error("audio_input 未兑现，门应为 Gated")
	}
	if want := 8.5 / 15.0; p.AvailableScore() != want {
		t.Errorf("门可用分 = %.3f，期望 %.3f（8.5/15）", p.AvailableScore(), want)
	}
	if want := 9.5 / 15.0; p.DesignScore() != want {
		t.Errorf("设计分 = %.3f，期望 %.3f（不因投放进度改变）", p.DesignScore(), want)
	}
}
