package degrade

import (
	"errors"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// TestBestOutboundKeepsMostDefinitiveError 固化候选间错误的取舍。
//
// 全部候选都跑不通时，返回哪一个错误决定了客户端会做什么：422 说「改请求」，
// 501 说「等实现」。早先返回的是**最后**一次错误——于是首选路径明确的
// 「这条路不支持音频」会被末位候选的「那扇门还没开」盖掉，客户端被告知去等
// 一个永远不会让它成功的实现，而真相是它该改请求。
//
// 端点闸门让这件事从理论变成现实：候选筛选是端点无感的，排在后面的候选
// 很容易恰好没开这扇门，凭空产出一堆 501。
func TestBestOutboundKeepsMostDefinitiveError(t *testing.T) {
	m := NewMatrix()

	var others []canonical.Capability
	for _, c := range ExpressibleSet(ProtoOpenAIChat) {
		if c != canonical.CapAudioInput {
			others = append(others, c)
		}
	}

	// 首选：门开着，但按设计拒绝音频输入——422，是个确定的答案。
	if err := m.Add(NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
		MarkHomogeneous().
		Pass(others...).
		Reject(noteNoAudioIn, canonical.CapAudioInput).
		Redeem(EndpointOpenAIChat, others...).
		Build()); err != nil {
		t.Fatal(err)
	}
	// 次选：全都支持，但只开了另一扇门——501，是个「再等等」。
	if err := m.Add(NewRoute(ProtoOpenAIChat, ProviderDashScopeCompatible).
		Pass(ExpressibleSet(ProtoOpenAIChat)...).
		Redeem(Endpoint("/v1/somewhere-else"), ExpressibleSet(ProtoOpenAIChat)...).
		Build()); err != nil {
		t.Fatal(err)
	}

	_, _, err := m.BestOutbound(
		Inbound{Protocol: ProtoOpenAIChat, Endpoint: EndpointOpenAIChat},
		[]Provider{ProviderOpenAICompat, ProviderDashScopeCompatible},
		[]canonical.Capability{canonical.CapTextGeneration, canonical.CapAudioInput})
	if err == nil {
		t.Fatal("两个候选都跑不通，应当失败")
	}

	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassUnsupported || cerr.HTTPStatus() != 422 {
		t.Errorf("确定的 REJECT（422）不该被末位候选的「门没开」（501）盖掉，实际 %q/%d：%s",
			cerr.Class, cerr.HTTPStatus(), cerr.Message)
	}
}

// TestBestOutboundPrefersDecoderBugOverNotImplemented 固化 internal 同样压得住 501。
//
// N/A 能力出现在请求里说明网关自己的解码器有问题。把它降格成「还没实现」，
// 这个 bug 就会被记进「等待投放」的账上，永远没人去查。
func TestBestOutboundPrefersDecoderBugOverNotImplemented(t *testing.T) {
	m := NewMatrix()

	if err := m.Add(NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoOpenAIChat)...).
		Redeem(EndpointOpenAIChat, ExpressibleSet(ProtoOpenAIChat)...).
		Build()); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(NewRoute(ProtoOpenAIChat, ProviderDashScopeCompatible).
		Pass(ExpressibleSet(ProtoOpenAIChat)...).
		Redeem(Endpoint("/v1/somewhere-else"), ExpressibleSet(ProtoOpenAIChat)...).
		Build()); err != nil {
		t.Fatal(err)
	}

	// reasoning_signature 是 openai.chat 表达不出来的 → N/A → ClassInternal。
	_, _, err := m.BestOutbound(
		Inbound{Protocol: ProtoOpenAIChat, Endpoint: EndpointOpenAIChat},
		[]Provider{ProviderOpenAICompat, ProviderDashScopeCompatible},
		[]canonical.Capability{canonical.CapReasoningSignature})
	if err == nil {
		t.Fatal("不可表达的能力应当失败")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassInternal {
		t.Errorf("解码器 bug（internal）不该被「门没开」盖掉，实际 %q：%s", cerr.Class, cerr.Message)
	}
}

// TestBestOutboundKeepsFirstRankedErrorWithinSameClass 固化同类错误取排序最前的那个。
//
// 同为 501 时，首选路径的说法离用户的实际请求最近；取末位候选的说法，
// 错误消息会点名一个用户根本没打算用的 Provider。
func TestBestOutboundKeepsFirstRankedErrorWithinSameClass(t *testing.T) {
	m := NewMatrix()

	// 两个候选都只开了别处的门，都会得 501。
	for _, out := range []Provider{ProviderOpenAICompat, ProviderDashScopeCompatible} {
		if err := m.Add(NewRoute(ProtoOpenAIChat, out).
			Pass(ExpressibleSet(ProtoOpenAIChat)...).
			Redeem(Endpoint("/v1/somewhere-else"), ExpressibleSet(ProtoOpenAIChat)...).
			Build()); err != nil {
			t.Fatal(err)
		}
	}

	ranked := m.RankOutbound(ProtoOpenAIChat,
		[]Provider{ProviderOpenAICompat, ProviderDashScopeCompatible})
	if len(ranked) == 0 {
		t.Fatal("两条路径都已通车，应当都是候选")
	}

	_, _, err := m.BestOutbound(
		Inbound{Protocol: ProtoOpenAIChat, Endpoint: EndpointOpenAIChat},
		[]Provider{ProviderOpenAICompat, ProviderDashScopeCompatible},
		[]canonical.Capability{canonical.CapTextGeneration})
	if err == nil {
		t.Fatal("两个候选的门都没开，应当失败")
	}
	if !strings.Contains(err.Error(), string(ranked[0])) {
		t.Errorf("同类错误应保留排序最前那个候选（%s）的说法，实际: %v", ranked[0], err)
	}
}

// TestBestOutboundStillPrefersViableCandidate 防的是「为了排错误优先级，
// 把能跑通的候选也一并否了」。
//
// 错误取舍只在**全部**候选都失败时才发生；只要还有一条跑得通，它就该赢。
func TestBestOutboundStillPrefersViableCandidate(t *testing.T) {
	m := NewMatrix()

	var others []canonical.Capability
	for _, c := range ExpressibleSet(ProtoOpenAIChat) {
		if c != canonical.CapAudioInput {
			others = append(others, c)
		}
	}

	// 首选拒绝音频（422），次选真能跑通。
	if err := m.Add(NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
		MarkHomogeneous().
		Pass(others...).
		Reject(noteNoAudioIn, canonical.CapAudioInput).
		Redeem(EndpointOpenAIChat, others...).
		Build()); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(NewRoute(ProtoOpenAIChat, ProviderDashScopeCompatible).
		Pass(ExpressibleSet(ProtoOpenAIChat)...).
		Redeem(EndpointOpenAIChat, ExpressibleSet(ProtoOpenAIChat)...).
		Build()); err != nil {
		t.Fatal(err)
	}

	got, _, err := m.BestOutbound(
		Inbound{Protocol: ProtoOpenAIChat, Endpoint: EndpointOpenAIChat},
		[]Provider{ProviderOpenAICompat, ProviderDashScopeCompatible},
		[]canonical.Capability{canonical.CapTextGeneration, canonical.CapAudioInput})
	if err != nil {
		t.Fatalf("次选真能跑通，不该失败: %v", err)
	}
	if got != ProviderDashScopeCompatible {
		t.Errorf("应选中真能跑通的次选，实际 %q", got)
	}
}
