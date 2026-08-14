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
