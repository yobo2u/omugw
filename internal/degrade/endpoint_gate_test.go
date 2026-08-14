package degrade

import (
	"errors"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// TestRedeemZeroEndpointFailsBuild 防的是零值端点退化成路径通配。
//
// 零值一旦被当成「整条路径」，端点粒度就悄悄退回了路径粒度——
// 这正是本次下沉要消灭的毛病，必须在 Build 就拦死。
func TestRedeemZeroEndpointFailsBuild(t *testing.T) {
	_, err := NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
		Pass(ExpressibleSet(ProtoOpenAIChat)...).
		Redeem(Endpoint(""), canonical.CapTextGeneration).
		Build()
	if err == nil {
		t.Fatal("对零值端点 Redeem 应当 Build 失败")
	}
	if !strings.Contains(err.Error(), "redeem on zero endpoint") {
		t.Errorf("错误信息应说明是零值端点问题，实际为: %v", err)
	}
}

// TestRedeemUndeliverableAtEndpointFailsBuild 是既有路径级检查在端点粒度的延续。
//
// 兑现一格按设计就该失败（REJECT）或客户端根本发不出来（N/A）的能力，
// 是在为不存在的东西背书——让它在 Build 失败，好过在文档里显示成「已投放」。
func TestRedeemUndeliverableAtEndpointFailsBuild(t *testing.T) {
	var others []canonical.Capability
	for _, c := range ExpressibleSet(ProtoOpenAIChat) {
		if c != canonical.CapAudioInput {
			others = append(others, c)
		}
	}

	// REJECT 格子。
	_, err := NewRoute(ProtoOpenAIChat, ProviderAnthropicMessages).
		Pass(others...).
		Reject(noteNoAudioIn, canonical.CapAudioInput).
		Redeem(EndpointOpenAIChat, canonical.CapAudioInput).
		Build()
	if err == nil {
		t.Fatal("兑现一个 REJECT 格子应当 Build 失败")
	}
	if !strings.Contains(err.Error(), string(canonical.CapAudioInput)) {
		t.Errorf("错误信息应指出是哪项能力，实际为: %v", err)
	}
	if !strings.Contains(err.Error(), string(EndpointOpenAIChat)) {
		t.Errorf("错误信息应指出是哪扇门，实际为: %v", err)
	}
	if !strings.Contains(err.Error(), "redeemed but not deliverable") {
		t.Errorf("错误信息应说明不可交付，实际为: %v", err)
	}

	// N/A 格子：openai.chat 表达不出 rerank。
	_, err = NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
		Pass(ExpressibleSet(ProtoOpenAIChat)...).
		Redeem(EndpointOpenAIChat, canonical.CapRerank).
		Build()
	if err == nil {
		t.Fatal("兑现一个不可表达的能力应当 Build 失败")
	}
}

// TestEndpointsDerivedFromRedemption 固化「门的存在只从兑现格子推导」。
//
// 没有独立的门注册机制，也没有「空门」可以声明：
// 有一格兑现才算开了这扇门，追加兑现不产生重复条目，零兑现路径没有门。
func TestEndpointsDerivedFromRedemption(t *testing.T) {
	r, err := NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoDashScopeNative)...).
		Redeem(EndpointDashScopeTextGeneration, canonical.CapTextGeneration).
		Build()
	if err != nil {
		t.Fatal(err)
	}

	if eps := r.Endpoints(); len(eps) != 1 || eps[0] != EndpointDashScopeTextGeneration {
		t.Fatalf("应恰有一扇文本门，实际 %v", eps)
	}
	if !r.ImplementedAt(EndpointDashScopeTextGeneration) {
		t.Error("文本门应已开")
	}
	if r.ImplementedAt(Endpoint("/api/v1/other")) {
		t.Error("未兑现的端点不应算开门")
	}
	if !r.Implemented() {
		t.Error("至少一扇门已开，路径应算通车")
	}

	// 同一门追加兑现不产生重复条目。
	r.Redeem(EndpointDashScopeTextGeneration, canonical.CapStreaming)
	if eps := r.Endpoints(); len(eps) != 1 {
		t.Fatalf("同一门追加兑现不应产生重复条目: %v", eps)
	}

	// 开第二扇门，字典序排列（multimodal 在 text 之前）。
	r.Redeem(EndpointDashScopeMultimodal, canonical.CapTextGeneration)
	if eps := r.Endpoints(); len(eps) != 2 ||
		eps[0] != EndpointDashScopeMultimodal || eps[1] != EndpointDashScopeTextGeneration {
		t.Fatalf("门应按字典序排列: %v", eps)
	}

	// 零兑现路径没有门。
	empty := NewRoute(ProtoOpenAIRealtime, ProviderOpenAIRealtime)
	if eps := empty.Endpoints(); len(eps) != 0 {
		t.Fatalf("零兑现路径不应有门: %v", eps)
	}
	if empty.Implemented() {
		t.Error("零兑现路径未通车")
	}
}

// TestCheckRejectsUnopenedEndpoint 固化端点闸门（闸门 3）。
//
// 敲没开的门，即使带着别处已兑现的能力，也必须 501，且消息点名端点——
// 入口约束与能力裁决是两件事。
func TestCheckRejectsUnopenedEndpoint(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.Check(Inbound{
		Protocol: ProtoDashScopeNative,
		Endpoint: Endpoint("/api/v1/services/aigc/not-opened"),
	}, ProviderDashScopeNative, []canonical.Capability{canonical.CapTextGeneration})
	if err == nil {
		t.Fatal("未开门必须 501，即使携带已兑现的能力")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassNotImplemented || cerr.HTTPStatus() != 501 {
		t.Errorf("应为 not_implemented/501，实际 %q/%d", cerr.Class, cerr.HTTPStatus())
	}
	if !strings.Contains(cerr.Message, "/api/v1/services/aigc/not-opened") {
		t.Errorf("错误消息应点名端点: %s", cerr.Message)
	}
}

// TestCheckDispositionBeforeRedemption 固化闸门顺序：REJECT 先于投放（4b 先于 4d）。
//
// 先问投放会把「这条路不支持」说成「还没建好」，方向就错了：
// 501 说「等」，422 说「改」，前者会变，后者不会。
func TestCheckDispositionBeforeRedemption(t *testing.T) {
	m := NewMatrix()
	var others []canonical.Capability
	for _, c := range ExpressibleSet(ProtoOpenAIChat) {
		if c != canonical.CapAudioInput {
			others = append(others, c)
		}
	}
	if err := m.Add(NewRoute(ProtoOpenAIChat, ProviderAnthropicMessages).
		Pass(others...).
		Reject(noteNoAudioIn, canonical.CapAudioInput).
		Redeem(EndpointOpenAIChat, canonical.CapTextGeneration). // 门开了，但 audio_input 未兑现
		Build()); err != nil {
		t.Fatal(err)
	}

	_, err := m.Check(Inbound{Protocol: ProtoOpenAIChat, Endpoint: EndpointOpenAIChat},
		ProviderAnthropicMessages,
		[]canonical.Capability{canonical.CapTextGeneration, canonical.CapAudioInput})
	if err == nil {
		t.Fatal("REJECT 能力必须失败")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassUnsupported || cerr.HTTPStatus() != 422 {
		t.Errorf("REJECT 应先于投放作答，返回 unsupported/422，实际 %q/%d", cerr.Class, cerr.HTTPStatus())
	}
}

// TestPreservationAvailableIsEndpointRelative 固化可用列端点相对、设计列路径级。
func TestPreservationAvailableIsEndpointRelative(t *testing.T) {
	r, err := NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoDashScopeNative)...).
		Redeem(EndpointDashScopeTextGeneration, canonical.CapTextGeneration, canonical.CapStreaming).
		Redeem(EndpointDashScopeMultimodal, canonical.CapTextGeneration).
		Build()
	if err != nil {
		t.Fatal(err)
	}

	avail := DefaultAvailability()

	if got := r.Preservation(avail, EndpointDashScopeTextGeneration).AvailableScore(); got != 2.0/18.0 {
		t.Errorf("文本门可用分 = %.3f，期望 2/18", got)
	}
	if got := r.Preservation(avail, EndpointDashScopeMultimodal).AvailableScore(); got != 1.0/18.0 {
		t.Errorf("多模态门可用分 = %.3f，期望 1/18", got)
	}

	// 设计列是路径级的，与敲哪扇门无关。
	if got := r.Preservation(avail, EndpointDashScopeTextGeneration).DesignScore(); got != 1.0 {
		t.Errorf("设计分应为 1.000，实际 %.3f", got)
	}

	// 未开门（含零值）：可用列恒为零，设计列不变——只会少报，不会多报。
	zero := r.Preservation(avail, Endpoint(""))
	if zero.AvailableScore() != 0 {
		t.Errorf("零值端点可用列应恒为零，实际 %.3f", zero.AvailableScore())
	}
	if zero.DesignScore() != 1.0 {
		t.Errorf("零值端点设计列应不受影响，实际 %.3f", zero.DesignScore())
	}
}
