package degrade

import (
	"testing"

	"github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

// TestDashScopeEndpointsReuseWirePaths 钉死 DashScope 两扇门就是协议包的路径常量本身。
//
// 线格式事实的单一来源在协议包；矩阵若复写第二份字符串，两处就可能漂移——
// 门常量漂了，Mux 注册与裁决各敲各的门。
func TestDashScopeEndpointsReuseWirePaths(t *testing.T) {
	if string(EndpointDashScopeTextGeneration) != dashscopenative.TextGenerationPath {
		t.Errorf("文本门 = %q，应等于 dashscopenative.TextGenerationPath %q",
			EndpointDashScopeTextGeneration, dashscopenative.TextGenerationPath)
	}
	if string(EndpointDashScopeMultimodal) != dashscopenative.MultimodalGenerationPath {
		t.Errorf("多模态门 = %q，应等于 dashscopenative.MultimodalGenerationPath %q",
			EndpointDashScopeMultimodal, dashscopenative.MultimodalGenerationPath)
	}
}

// TestOpenAIEndpointConstants 钉死 OpenAI 两扇门的字面值。
//
// 这两个值原先散落在 gateway/build.go 的 Mux 注册里；提成常量后，
// 登记（rules）与注册（Mux）必须引用同一份事实。
func TestOpenAIEndpointConstants(t *testing.T) {
	if EndpointOpenAIChat != "/v1/chat/completions" {
		t.Errorf("EndpointOpenAIChat = %q", EndpointOpenAIChat)
	}
	if EndpointOpenAIResponses != "/v1/responses" {
		t.Errorf("EndpointOpenAIResponses = %q", EndpointOpenAIResponses)
	}
}

// TestKnownEndpointsBelongToOwningProtocol 钉死四扇已知门各自的归属协议。
//
// 门不是一段自由字符串：/v1/responses 讲的是 Responses 线格式，
// 拿 openai.chat 的解码器去接它，解出来的东西与门后的实现对不上。
// 归属一旦漂了，Build 那道错绑闸门就会放错人进来——归属表本身必须先被咬住。
func TestKnownEndpointsBelongToOwningProtocol(t *testing.T) {
	for _, tc := range []struct {
		ep    Endpoint
		owner Protocol
	}{
		{EndpointOpenAIChat, ProtoOpenAIChat},
		{EndpointOpenAIResponses, ProtoOpenAIResponses},
		{EndpointDashScopeTextGeneration, ProtoDashScopeNative},
		{EndpointDashScopeMultimodal, ProtoDashScopeNative},
	} {
		owner, known := tc.ep.Protocol()
		if !known {
			t.Errorf("门 %s 应当是已知门", tc.ep)
			continue
		}
		if owner != tc.owner {
			t.Errorf("门 %s 归属 %q，应为 %q", tc.ep, owner, tc.owner)
		}
	}
}

// TestUnknownEndpointHasNoOwner 固化「归属表只覆盖已知门」。
//
// 未知门必须报「不知道」而不是猜一个协议：猜错会把一扇还没设计的门
// 焊死在某个协议上，而未来投放它的多半是另一个协议。零值同理——
// 它按定义就是「没有这扇门」，更不该有归属。
func TestUnknownEndpointHasNoOwner(t *testing.T) {
	for _, ep := range []Endpoint{
		Endpoint(""),
		Endpoint("/v1/embeddings"),
		Endpoint("/api/v1/services/embeddings/text-embedding/text-embedding"),
	} {
		if owner, known := ep.Protocol(); known {
			t.Errorf("未知门 %q 不该有归属，实际报 %q", ep, owner)
		}
	}
}
