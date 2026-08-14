package dashscopenative

import "testing"

// TestMultimodalGenerationPathMatchesOfficialContract 钉死多模态生成的端点路径。
//
// 这个字符串是线格式事实，出自 docs/architecture/dashscope-endpoints-research.md。
// 用测试钉住，防的是端点门悄悄漂到另一个路径上——门敲错了，后面所有裁决全错。
func TestMultimodalGenerationPathMatchesOfficialContract(t *testing.T) {
	const want = "/api/v1/services/aigc/multimodal-generation/generation"
	if MultimodalGenerationPath != want {
		t.Errorf("MultimodalGenerationPath = %q，期望 %q", MultimodalGenerationPath, want)
	}
}
