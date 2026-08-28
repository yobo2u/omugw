//go:build smoke

package smoke_test

import (
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/degrade"
)

const (
	noteFileRefBound = "文件引用绑定具体 Provider，跨 Provider 不可迁移；" +
		"网关不代下载再上传（原则 2.6），请改用 URL 或内联字节"
	noteSearchSwitch = "DashScope 的 enable_search 是布尔开关，承载不了 OpenAI web_search 工具的" +
		"参数，仅开关本身被映射"
)

// recordMatrix 构造录制脚手架专用的降级矩阵。
//
// 生产矩阵（Phase1）中 openai.chat → dashscope.native 仍为 PLANNED。
// 录制器在此构建独立测试矩阵，严格镜像 Phase1 对 Chat 能力的处置声明，
// 并在 Chat 门兑现 9 项已交付能力；同时填充其余三扇已注册门以满足启动期对账。
func recordMatrix(t testing.TB) *degrade.Matrix {
	t.Helper()
	m := degrade.NewMatrix()

	chatToDSNative := degrade.NewRoute(degrade.ProtoOpenAIChat, degrade.ProviderDashScopeNative).
		Pass(
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapVisionInput,
			canonical.CapAudioInput,
			canonical.CapReasoning,
		).
		Degrade("DashScope Native 的并行工具调用行为由上游模型决定，无显式开关可映射",
			canonical.CapParallelToolCalls).
		Degrade("DashScope Native 支持 response_format=json_object，无 strict schema 校验",
			canonical.CapStructuredOutput).
		Degrade(noteSearchSwitch, canonical.CapWebSearch).
		Reject(noteFileRefBound, canonical.CapFileInput).
		Reject("音频输出需要 Qwen-Omni 的输出格式参数，Chat Completions 入站无法表达",
			canonical.CapAudioOutput).
		Redeem(degrade.EndpointOpenAIChat,
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapParallelToolCalls,
			canonical.CapStructuredOutput,
			canonical.CapReasoning,
			canonical.CapVisionInput,
			canonical.CapAudioInput,
			canonical.CapWebSearch,
		)

	if err := m.Add(chatToDSNative.Build()); err != nil {
		t.Fatalf("添加 Chat->Native 路径失败: %v", err)
	}

	respToOpenAI := degrade.NewRoute(degrade.ProtoOpenAIResponses, degrade.ProviderOpenAICompat).
		MarkHomogeneous().
		Pass(degrade.ExpressibleSet(degrade.ProtoOpenAIResponses)...).
		Redeem(degrade.EndpointOpenAIResponses, canonical.CapTextGeneration)

	if err := m.Add(respToOpenAI.Build()); err != nil {
		t.Fatalf("添加 Responses 填充路径失败: %v", err)
	}

	nativeToNative := degrade.NewRoute(degrade.ProtoDashScopeNative, degrade.ProviderDashScopeNative).
		MarkHomogeneous().
		Pass(degrade.ExpressibleSet(degrade.ProtoDashScopeNative)...).
		Redeem(degrade.EndpointDashScopeTextGeneration, canonical.CapTextGeneration).
		Redeem(degrade.EndpointDashScopeMultimodal, canonical.CapTextGeneration)

	if err := m.Add(nativeToNative.Build()); err != nil {
		t.Fatalf("添加 Native 填充路径失败: %v", err)
	}

	return m
}
