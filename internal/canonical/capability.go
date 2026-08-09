package canonical

// Capability 是降级矩阵的第三维度。每个 (入站协议, 出站 Provider) 组合都必须
// 对这里列出的每一项能力给出明确处置——PASSTHROUGH、DEGRADE 或 REJECT。
//
// 新增能力时 internal/degrade 的完整性测试会失败，直到所有已注册的协议/Provider
// 组合都补上对应规则为止。这是刻意的：静默遗漏比编译失败昂贵得多。
type Capability string

const (
	// 文本与流式
	CapTextGeneration Capability = "text_generation"
	CapStreaming      Capability = "streaming"

	// 工具与结构化输出
	CapToolCalling       Capability = "tool_calling"
	CapParallelToolCalls Capability = "parallel_tool_calls"
	CapStructuredOutput  Capability = "structured_output"

	// 推理
	// CapReasoningSignature 单独成项：Anthropic 的 thinking block 带签名，
	// 跨协议转换后签名失效，必须与普通 reasoning 分开处置。
	CapReasoning          Capability = "reasoning"
	CapReasoningSignature Capability = "reasoning_signature"

	// 多模态输入
	CapVisionInput Capability = "vision_input"
	CapAudioInput  Capability = "audio_input"
	CapVideoInput  Capability = "video_input"
	CapFileInput   Capability = "file_input"

	// 多模态输出
	CapAudioOutput     Capability = "audio_output"
	CapImageGeneration Capability = "image_generation"
	CapVideoGeneration Capability = "video_generation"

	// 语音
	CapSpeechSynthesis   Capability = "speech_synthesis"
	CapSpeechRecognition Capability = "speech_recognition"

	// 向量
	CapEmbedding Capability = "embedding"
	CapRerank    Capability = "rerank"

	// 缓存与会话
	// 三家的 prompt cache 语义互斥（Anthropic 显式断点 / OpenAI 自动前缀 /
	// Gemini 独立 CachedContent 资源），不存在映射函数，只能同源透传。
	CapPromptCache            Capability = "prompt_cache"
	CapStatefulConversation   Capability = "stateful_conversation"
	CapRealtimeSession        Capability = "realtime_session"
	CapRealtimeImageInput     Capability = "realtime_image_input"
	CapRealtimeCommitModes    Capability = "realtime_commit_modes"
	CapRealtimeServerVAD      Capability = "realtime_server_vad"
	CapRealtimeInterruptTurns Capability = "realtime_interrupt_turns"

	// 上游内建工具
	CapWebSearch   Capability = "web_search"
	CapComputerUse Capability = "computer_use"
)

// AllCapabilities 是降级矩阵完整性检查的依据。新增常量后必须加进这个列表，
// TestAllCapabilitiesRegistered 会捕获遗漏。
func AllCapabilities() []Capability {
	return []Capability{
		CapTextGeneration,
		CapStreaming,
		CapToolCalling,
		CapParallelToolCalls,
		CapStructuredOutput,
		CapReasoning,
		CapReasoningSignature,
		CapVisionInput,
		CapAudioInput,
		CapVideoInput,
		CapFileInput,
		CapAudioOutput,
		CapImageGeneration,
		CapVideoGeneration,
		CapSpeechSynthesis,
		CapSpeechRecognition,
		CapEmbedding,
		CapRerank,
		CapPromptCache,
		CapStatefulConversation,
		CapRealtimeSession,
		CapRealtimeImageInput,
		CapRealtimeCommitModes,
		CapRealtimeServerVAD,
		CapRealtimeInterruptTurns,
		CapWebSearch,
		CapComputerUse,
	}
}
