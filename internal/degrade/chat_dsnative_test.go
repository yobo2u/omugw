package degrade

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// redeemedChatDSNative 是 /v1/chat/completions 门在 dashscope.native 上兑现的八项，
// 按 AllCapabilities 顺序——与 RedeemedAt 输出顺序一致。
//
// audio_input 不在其中：设计处置仍是 PASS（协议能力没有消失），但
// qwen-audio-turbo 免费额度耗尽、没有真实 fixture——无证据不宣称可用
// （ADR-0001）。未来恢复投放的条件见 2026-08-30 部分投放设计。
var redeemedChatDSNative = []canonical.Capability{
	canonical.CapTextGeneration,
	canonical.CapStreaming,
	canonical.CapToolCalling,
	canonical.CapParallelToolCalls,
	canonical.CapStructuredOutput,
	canonical.CapReasoning,
	canonical.CapVisionInput,
	canonical.CapWebSearch,
}

// TestChatDSNativeRouteIsHeterogeneous 钉死身份与设计处置：
// 完整重编码的异构路径，非同源快通道；设计分 7.5/11；
// audio_input 的设计处置保持 PASS——本期不投放是证据问题，不是协议表达问题。
func TestChatDSNativeRouteIsHeterogeneous(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := m.Route(ProtoOpenAIChat, ProviderDashScopeNative)
	if !ok {
		t.Fatal("openai.chat -> dashscope.native 未注册")
	}
	if r.IsHomogeneous() {
		t.Error("该路径是完整重编码异构转换，不得标记为同源快通道")
	}

	// 设计处置：6 PASS + 3 DEGRADE + 2 REJECT = 11 项可表达能力，设计分 7.5/11。
	p := r.Preservation(m.Availability(), EndpointOpenAIChat)
	if p.Passthrough != 6 || p.Degrade != 3 || p.Reject != 2 {
		t.Errorf("设计处置 = pass %d deg %d rej %d，期望 6/3/2",
			p.Passthrough, p.Degrade, p.Reject)
	}
	if want := 7.5 / 11.0; p.DesignScore() != want {
		t.Errorf("设计保留度 = %.3f，期望 %.3f（7.5/11）", p.DesignScore(), want)
	}

	// audio_input 的设计处置必须是 PASS：把它改成 REJECT 或 N/A，
	// 等于把「缺少投放证据」误写成「协议无法表达」。
	rule, ok := m.Lookup(ProtoOpenAIChat, ProviderDashScopeNative, canonical.CapAudioInput)
	if !ok || rule.Disposition != Passthrough {
		t.Fatalf("audio_input 设计处置应为 PASSTHROUGH，实际 %v", rule.Disposition)
	}
}

// TestChatDSNativeRedemptionIsExactlyEightCapabilities 钉死兑现集合精确为八项：
// 门可用分 6.5/11，门保持 Gated（audio_input 设计上可交付、当前未投放）。
func TestChatDSNativeRedemptionIsExactlyEightCapabilities(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := m.Route(ProtoOpenAIChat, ProviderDashScopeNative)
	if !ok {
		t.Fatal("openai.chat -> dashscope.native 未注册")
	}

	if got := r.RedeemedAt(EndpointOpenAIChat); !reflect.DeepEqual(got, redeemedChatDSNative) {
		t.Errorf("兑现集合 = %v，期望 %v", got, redeemedChatDSNative)
	}
	if r.Redeems(EndpointOpenAIChat, canonical.CapAudioInput) {
		t.Error("audio_input 没有真实 fixture，本期不得兑现")
	}
	for _, c := range []canonical.Capability{canonical.CapFileInput, canonical.CapAudioOutput} {
		if r.Redeems(EndpointOpenAIChat, c) {
			t.Errorf("%q 是 REJECT，不应被兑现", c)
		}
	}

	// 八项兑现：5 PASS + 3 DEGRADE×0.5 = 6.5，分母 11。
	p := r.Preservation(m.Availability(), EndpointOpenAIChat)
	if want := 6.5 / 11.0; p.AvailableScore() != want {
		t.Errorf("门 %s 可用分 = %.3f，期望 %.3f（6.5/11）", EndpointOpenAIChat, p.AvailableScore(), want)
	}
	if !p.Gated() {
		t.Error("audio_input 设计上可交付、当前未投放，门必须保持 Gated")
	}
}

// TestChatDSNativeAudioInputStays501AtMatrix 用生产矩阵钉死闸门咬人：
// 未投放的 audio_input 在矩阵裁决阶段返回 501 not_implemented——不是 422，
// 客户端请求没有错；同一条 Check 去掉 audio_input 必须放行，
// 证明闸门挡的是能力而不是路径。
//
// 不做任何临时 Redeem 变更来「制造」这个 501：任务 18 兑现后的 Phase1
// 本身就是闸门生效的状态，直接对它断言。
func TestChatDSNativeAudioInputStays501AtMatrix(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	in := Inbound{Protocol: ProtoOpenAIChat, Endpoint: EndpointOpenAIChat}

	_, err = m.Check(in, ProviderDashScopeNative,
		[]canonical.Capability{canonical.CapTextGeneration, canonical.CapAudioInput})
	if err == nil {
		t.Fatal("未投放的 audio_input 必须被拦下")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassNotImplemented || cerr.HTTPStatus() != 501 {
		t.Errorf("应为 not_implemented/501，实际 %q/%d", cerr.Class, cerr.HTTPStatus())
	}
	// 点名能力，且是能力级 501（「尚未在端点」）而不是路径级 501（「实现尚未落地」）。
	if !strings.Contains(cerr.Message, "audio_input") || !strings.Contains(cerr.Message, "尚未在端点") {
		t.Errorf("错误应点名能力与端点: %s", cerr.Message)
	}

	// 已投放的八项照常放行，否则这道闸门把整条路径也一起关了。
	if _, err := m.Check(in, ProviderDashScopeNative, redeemedChatDSNative); err != nil {
		t.Errorf("已投放的八项不该被拦下: %v", err)
	}
}

// TestChatDoorStillPrefersCompatibleOverNative 钉死同门选路不变：
// dashscope.compatible（8/11 ≈ 0.727）仍优先于 dashscope.native（6.5/11 ≈ 0.591），
// OutboundPreference 不改。
func TestChatDoorStillPrefersCompatibleOverNative(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	compat := mustRoute(t, m, ProtoOpenAIChat, ProviderDashScopeCompatible)
	native := mustRoute(t, m, ProtoOpenAIChat, ProviderDashScopeNative)
	cs := compat.Preservation(m.Availability(), EndpointOpenAIChat).AvailableScore()
	ns := native.Preservation(m.Availability(), EndpointOpenAIChat).AvailableScore()
	if cs <= ns {
		t.Errorf("compatible 门可用分 %.3f 应严格高于 native 门 %.3f", cs, ns)
	}
}
