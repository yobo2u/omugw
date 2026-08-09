package degrade

import (
	"github.com/yobo2u/omugw/internal/canonical"
)

// 能力分组，只为让下面的路径声明可读。分组不改变语义——每一项能力仍然必须
// 被逐一处置，Route.Build 会验证这一点。
var (
	realtimeCaps = []canonical.Capability{
		canonical.CapRealtimeSession,
		canonical.CapRealtimeImageInput,
		canonical.CapRealtimeCommitModes,
		canonical.CapRealtimeServerVAD,
		canonical.CapRealtimeInterruptTurns,
	}
	mediaGenCaps = []canonical.Capability{
		canonical.CapImageGeneration,
		canonical.CapVideoGeneration,
	}
	speechCaps = []canonical.Capability{
		canonical.CapSpeechSynthesis,
		canonical.CapSpeechRecognition,
	}
	vectorCaps = []canonical.Capability{
		canonical.CapEmbedding,
		canonical.CapRerank,
	}
)

// 反复出现的处置说明，抽出来避免同一条理由在各路径里写出不同版本。
const (
	noteSignatureLost = "Anthropic thinking 签名经 Canonical 转换后失效，" +
		"带失效签名的多轮 tool use 会被上游拒绝，因此在异构路径上直接拒绝而非静默丢弃"
	noteFileRefBound = "文件引用绑定具体 Provider，跨 Provider 不可迁移；" +
		"网关不代下载再上传（原则 2.6），请改用 URL 或内联字节"
	noteWrongEndpoint = "该能力不在这条端点上，须经对应的专用入站端点"
	noteNoRealtime    = "Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达"
	noteComputerUse   = "computer use 的工具 schema 各家不兼容，不在 Phase 1 范围"
)

// Phase1 构造 Phase 1 的降级矩阵。
//
// 只登记**已实现**的转换路径。后续里程碑新增路径时必须在这里补充完整声明，
// 否则该路径在 Check 时会以「未注册的转换路径」失败——这正是期望的行为。
func Phase1() (*Matrix, error) {
	m := NewMatrix()

	// —— M1：OpenAI Chat 入站 ——

	// 同源快通道。字节级透传，只改写鉴权，不进 Canonical。
	if err := m.Add(NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
		MarkHomogeneous().
		Pass(
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapParallelToolCalls,
			canonical.CapStructuredOutput,
			canonical.CapReasoning,
			canonical.CapVisionInput,
			canonical.CapAudioInput,
			canonical.CapFileInput,
			canonical.CapAudioOutput,
			canonical.CapPromptCache,
			canonical.CapWebSearch,
		).
		Reject("OpenAI Chat Completions 不产生带签名的推理块；出现签名说明数据来自其他协议",
			canonical.CapReasoningSignature).
		Reject("OpenAI Chat Completions 不接受视频输入",
			canonical.CapVideoInput).
		Reject(noteWrongEndpoint, mediaGenCaps...).
		Reject(noteWrongEndpoint, speechCaps...).
		Reject(noteWrongEndpoint, vectorCaps...).
		Reject("Chat Completions 无服务端会话状态，请改用 Responses 端点",
			canonical.CapStatefulConversation).
		Reject(noteNoRealtime, realtimeCaps...).
		Reject(noteComputerUse, canonical.CapComputerUse).
		Build()); err != nil {
		return nil, err
	}

	if err := m.Add(NewRoute(ProtoOpenAIChat, ProviderAnthropicMessages).
		Pass(
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapParallelToolCalls,
			canonical.CapVisionInput,
			canonical.CapReasoning,
		).
		Degrade("Anthropic 无 strict json_schema 校验，schema 降级为提示词约束，"+
			"模型可能返回不合规 JSON",
			canonical.CapStructuredOutput).
		Degrade("OpenAI 自动前缀缓存与 Anthropic 显式 cache_control 断点语义互斥，"+
			"缓存意图被丢弃（请求仍然有效，但不会命中缓存）",
			canonical.CapPromptCache).
		Reject(noteSignatureLost, canonical.CapReasoningSignature).
		Reject("Anthropic Messages 不接受音频输入", canonical.CapAudioInput).
		Reject("Anthropic Messages 不接受视频输入", canonical.CapVideoInput).
		Reject(noteFileRefBound, canonical.CapFileInput).
		Reject("Anthropic 不产生音频输出", canonical.CapAudioOutput).
		Reject("Anthropic 不提供图像/视频生成", mediaGenCaps...).
		Reject("Anthropic 不提供语音能力", speechCaps...).
		Reject("Anthropic 不提供 embedding / rerank", vectorCaps...).
		Reject("Anthropic Messages 无服务端会话状态", canonical.CapStatefulConversation).
		Reject("Anthropic 无 Realtime API", realtimeCaps...).
		Reject("OpenAI 与 Anthropic 的内建 web_search 工具参数不兼容，Phase 1 不做映射",
			canonical.CapWebSearch).
		Reject(noteComputerUse, canonical.CapComputerUse).
		Build()); err != nil {
		return nil, err
	}

	if err := m.Add(NewRoute(ProtoOpenAIChat, ProviderDashScopeCompatible).
		Pass(
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapParallelToolCalls,
			canonical.CapVisionInput,
			canonical.CapAudioInput,
			canonical.CapReasoning,
		).
		Degrade("DashScope 兼容模式支持 json_object，但不保证 strict json_schema 校验",
			canonical.CapStructuredOutput).
		Degrade("DashScope 兼容模式的缓存由上游自动管理，显式缓存意图被丢弃",
			canonical.CapPromptCache).
		Degrade("兼容模式接受 video_url，但帧采样率与时长上限无 OpenAI 对应字段，采用上游默认值",
			canonical.CapVideoInput).
		Degrade("DashScope 的 enable_search 是布尔开关，承载不了 OpenAI web_search 工具的参数，"+
			"仅开关本身被映射",
			canonical.CapWebSearch).
		Reject(noteSignatureLost, canonical.CapReasoningSignature).
		Reject(noteFileRefBound, canonical.CapFileInput).
		Reject("兼容模式不返回音频；音频输出须经 Qwen-Omni Realtime 或 Native 端点",
			canonical.CapAudioOutput).
		Reject("兼容模式的 chat 端点不承载该能力，须走 DashScope Native", mediaGenCaps...).
		Reject("兼容模式的 chat 端点不承载该能力，须走 DashScope Native", speechCaps...).
		Reject("兼容模式的 chat 端点不承载该能力，须走 DashScope Native", vectorCaps...).
		Reject("DashScope 兼容模式无服务端会话状态", canonical.CapStatefulConversation).
		Reject(noteNoRealtime, realtimeCaps...).
		Reject(noteComputerUse, canonical.CapComputerUse).
		Build()); err != nil {
		return nil, err
	}

	if err := m.Add(NewRoute(ProtoOpenAIChat, ProviderDashScopeNative).
		Pass(
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapVisionInput,
			canonical.CapAudioInput,
			canonical.CapVideoInput,
			canonical.CapReasoning,
		).
		Degrade("DashScope Native 的并行工具调用行为由上游模型决定，无显式开关可映射",
			canonical.CapParallelToolCalls).
		Degrade("DashScope Native 支持 response_format=json_object，无 strict schema 校验",
			canonical.CapStructuredOutput).
		Degrade("DashScope Native 的缓存由上游自动管理，显式缓存意图被丢弃",
			canonical.CapPromptCache).
		Degrade("DashScope 的 enable_search 是布尔开关，承载不了 OpenAI web_search 工具的参数",
			canonical.CapWebSearch).
		Reject(noteSignatureLost, canonical.CapReasoningSignature).
		Reject(noteFileRefBound, canonical.CapFileInput).
		Reject("音频输出需要 Qwen-Omni 的输出格式参数，Chat Completions 入站无法表达",
			canonical.CapAudioOutput).
		Reject("图像/视频生成是异步任务，须经 /v1/jobs 端点", mediaGenCaps...).
		Reject("语音须经 /v1/audio 入站端点", speechCaps...).
		Reject("embedding / rerank 须经各自的独立入站端点", vectorCaps...).
		Reject("DashScope Native generation 无服务端会话状态",
			canonical.CapStatefulConversation).
		Reject(noteNoRealtime, realtimeCaps...).
		Reject(noteComputerUse, canonical.CapComputerUse).
		Build()); err != nil {
		return nil, err
	}

	// —— M7：OpenAI Realtime 入站 ——

	// 同源快通道。
	if err := m.Add(NewRoute(ProtoOpenAIRealtime, ProviderOpenAIRealtime).
		MarkHomogeneous().
		Pass(
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapParallelToolCalls,
			canonical.CapAudioInput,
			canonical.CapAudioOutput,
			canonical.CapSpeechSynthesis,
			canonical.CapSpeechRecognition,
			canonical.CapStatefulConversation,
			canonical.CapRealtimeSession,
			canonical.CapRealtimeServerVAD,
			canonical.CapRealtimeInterruptTurns,
		).
		Reject("OpenAI Realtime 未提供 input_image_buffer 事件",
			canonical.CapRealtimeImageInput).
		Reject("server_commit / commit 是 Qwen-TTS-Realtime 特有的提交模式，OpenAI 无对应概念",
			canonical.CapRealtimeCommitModes).
		Reject("Realtime 会话不返回推理内容块",
			canonical.CapReasoning, canonical.CapReasoningSignature).
		Reject("Realtime 会话不支持 response_format", canonical.CapStructuredOutput).
		Reject("Realtime 会话无 prompt cache 概念", canonical.CapPromptCache).
		Reject("Realtime 会话的图像输入须走 conversation item，Phase 1 不支持",
			canonical.CapVisionInput).
		Reject("Realtime 会话不接受视频输入", canonical.CapVideoInput).
		Reject(noteFileRefBound, canonical.CapFileInput).
		Reject(noteWrongEndpoint, mediaGenCaps...).
		Reject(noteWrongEndpoint, vectorCaps...).
		Reject("Realtime 会话不支持内建 web_search 工具", canonical.CapWebSearch).
		Reject(noteComputerUse, canonical.CapComputerUse).
		Build()); err != nil {
		return nil, err
	}

	// 近同构快通道——本项目的头牌能力。
	//
	// DashScope 的 /api-ws/v1/realtime 与 OpenAI Realtime 事件模型基本一致
	// （session.update / input_audio_buffer.append / response.create /
	// response.audio.delta ...），绝大多数事件原样转发即可。实际差异只有下面
	// 登记的三处，全部是可控工程量而非架构障碍。
	if err := m.Add(NewRoute(ProtoOpenAIRealtime, ProviderDashScopeWSRealtime).
		MarkHomogeneous().
		Pass(
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapAudioOutput,
			canonical.CapSpeechSynthesis,
			canonical.CapSpeechRecognition,
			canonical.CapStatefulConversation,
			canonical.CapRealtimeSession,
			canonical.CapRealtimeServerVAD,
			canonical.CapRealtimeInterruptTurns,
		).
		Degrade("输入音频需从 OpenAI 的 24 kHz 重采样到 DashScope 的 16 kHz，高频信息丢失；"+
			"输出侧两者同为 24 kHz，无需转换",
			canonical.CapAudioInput).
		Degrade("DashScope Realtime 未提供并行工具调用开关，行为由上游模型决定",
			canonical.CapParallelToolCalls).
		Reject("input_image_buffer.append 是 DashScope 独有事件，OpenAI Realtime 客户端无法产生",
			canonical.CapRealtimeImageInput).
		Reject("Qwen-TTS-Realtime 的 server_commit / commit 模式在 OpenAI Realtime 协议中"+
			"无对应字段，无法由客户端指定",
			canonical.CapRealtimeCommitModes).
		Reject("Realtime 会话不返回推理内容块",
			canonical.CapReasoning, canonical.CapReasoningSignature).
		Reject("Realtime 会话不支持 response_format", canonical.CapStructuredOutput).
		Reject("Realtime 会话无 prompt cache 概念", canonical.CapPromptCache).
		Reject("Qwen-Omni-Realtime 的图像输入依赖 input_image_buffer，OpenAI 客户端无法产生",
			canonical.CapVisionInput).
		Reject("Realtime 会话不接受视频输入", canonical.CapVideoInput).
		Reject(noteFileRefBound, canonical.CapFileInput).
		Reject(noteWrongEndpoint, mediaGenCaps...).
		Reject(noteWrongEndpoint, vectorCaps...).
		Reject("Realtime 会话不支持内建 web_search 工具", canonical.CapWebSearch).
		Reject(noteComputerUse, canonical.CapComputerUse).
		Build()); err != nil {
		return nil, err
	}

	return m, nil
}
