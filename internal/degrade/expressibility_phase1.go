package degrade

import "github.com/yobo2u/omugw/internal/canonical"

// 反复出现的「不可能」理由。抽成常量是为了避免同一件事在各协议下被写成
// 几个略有出入的版本——那会让人以为它们说的不是同一回事。
const (
	// 推理签名是 Anthropic 特有的机制。其他协议不是「不支持」，而是线格式里
	// 根本没有承载它的字段——客户端连发都发不出来。
	whyNoSignature = "该协议的线格式没有承载推理签名的字段，客户端无从表达；" +
		"这项能力要到 Anthropic Messages 入站接入后（Phase 2）才可达"

	// 缓存意图能不能表达，取决于协议给不给客户端控制权，而不是上游有没有缓存。
	whyAutoCache = "该协议的缓存由上游自动管理，客户端没有可控的缓存断点字段；" +
		"显式缓存断点是 Anthropic 特有机制，Phase 2 随 Anthropic 入站一并接入"

	whyNoComputerUse = "该协议未提供 computer use 类的内建工具定义"
	whyNoVideoInput  = "OpenAI 的线格式不接受视频输入"
)

func init() {
	// —— OpenAI Chat Completions ——
	//
	// 覆盖面最广、表达力最弱的一个。它连推理配置都只有一个 reasoning_effort，
	// 没有内建工具、没有服务端会话。把它当主入口，等于在门口先砍一层语义。
	register(&Expressibility{
		Protocol: ProtoOpenAIChat,
		Capabilities: []canonical.Capability{
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
			canonical.CapWebSearch,
		},
		Elsewhere: map[canonical.Capability]Protocol{
			canonical.CapStatefulConversation:   ProtoOpenAIResponses,
			canonical.CapComputerUse:            ProtoOpenAIResponses,
			canonical.CapImageGeneration:        ProtoOpenAIResponses,
			canonical.CapVideoGeneration:        ProtoDashScopeNative,
			canonical.CapEmbedding:              ProtoDashScopeNative,
			canonical.CapRerank:                 ProtoDashScopeNative,
			canonical.CapSpeechSynthesis:        ProtoDashScopeInference,
			canonical.CapSpeechRecognition:      ProtoDashScopeInference,
			canonical.CapRealtimeSession:        ProtoOpenAIRealtime,
			canonical.CapRealtimeServerVAD:      ProtoOpenAIRealtime,
			canonical.CapRealtimeInterruptTurns: ProtoOpenAIRealtime,
			canonical.CapRealtimeImageInput:     ProtoDashScopeRealtime,
			canonical.CapRealtimeCommitModes:    ProtoDashScopeRealtime,
		},
		Impossible: map[canonical.Capability]string{
			canonical.CapReasoningSignature: whyNoSignature,
			canonical.CapPromptCache:        whyAutoCache,
			canonical.CapVideoInput:         whyNoVideoInput,
		},
	})

	// —— OpenAI Responses ——
	//
	// 入站优先级第一。相对 Chat 多出三样真东西：服务端会话、内建工具
	// （computer_use / image_generation）、更完整的推理配置。
	register(&Expressibility{
		Protocol: ProtoOpenAIResponses,
		Capabilities: []canonical.Capability{
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
			canonical.CapWebSearch,
			canonical.CapStatefulConversation,
			canonical.CapComputerUse,
			canonical.CapImageGeneration,
		},
		Elsewhere: map[canonical.Capability]Protocol{
			canonical.CapVideoGeneration:        ProtoDashScopeNative,
			canonical.CapEmbedding:              ProtoDashScopeNative,
			canonical.CapRerank:                 ProtoDashScopeNative,
			canonical.CapSpeechSynthesis:        ProtoDashScopeInference,
			canonical.CapSpeechRecognition:      ProtoDashScopeInference,
			canonical.CapRealtimeSession:        ProtoOpenAIRealtime,
			canonical.CapRealtimeServerVAD:      ProtoOpenAIRealtime,
			canonical.CapRealtimeInterruptTurns: ProtoOpenAIRealtime,
			canonical.CapRealtimeImageInput:     ProtoDashScopeRealtime,
			canonical.CapRealtimeCommitModes:    ProtoDashScopeRealtime,
		},
		Impossible: map[canonical.Capability]string{
			canonical.CapReasoningSignature: whyNoSignature,
			canonical.CapPromptCache:        whyAutoCache,
			canonical.CapVideoInput:         whyNoVideoInput,
		},
	})

	// —— OpenAI Realtime ——
	register(&Expressibility{
		Protocol: ProtoOpenAIRealtime,
		Capabilities: []canonical.Capability{
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
		},
		Elsewhere: map[canonical.Capability]Protocol{
			canonical.CapStructuredOutput:    ProtoOpenAIResponses,
			canonical.CapReasoning:           ProtoOpenAIResponses,
			canonical.CapVisionInput:         ProtoOpenAIResponses,
			canonical.CapFileInput:           ProtoOpenAIResponses,
			canonical.CapImageGeneration:     ProtoOpenAIResponses,
			canonical.CapWebSearch:           ProtoOpenAIResponses,
			canonical.CapComputerUse:         ProtoOpenAIResponses,
			canonical.CapVideoGeneration:     ProtoDashScopeNative,
			canonical.CapEmbedding:           ProtoDashScopeNative,
			canonical.CapRerank:              ProtoDashScopeNative,
			canonical.CapRealtimeImageInput:  ProtoDashScopeRealtime,
			canonical.CapRealtimeCommitModes: ProtoDashScopeRealtime,
		},
		Impossible: map[canonical.Capability]string{
			canonical.CapReasoningSignature: whyNoSignature,
			canonical.CapPromptCache:        whyAutoCache,
			canonical.CapVideoInput:         whyNoVideoInput,
		},
	})

	// —— DashScope Native（HTTP）——
	//
	// 入站优先级第二，也是表达力最强的一个：视频输入、图像/视频生成、
	// embedding、rerank 都只有它能表达。这正是「DashScope Native 必须是一级
	// 协议」的实证——不是偏好，是别的协议真的说不出这些话。
	register(&Expressibility{
		Protocol: ProtoDashScopeNative,
		Capabilities: []canonical.Capability{
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapParallelToolCalls,
			canonical.CapStructuredOutput,
			canonical.CapReasoning,
			canonical.CapVisionInput,
			canonical.CapAudioInput,
			canonical.CapVideoInput,
			canonical.CapFileInput,
			canonical.CapAudioOutput,
			canonical.CapImageGeneration,
			canonical.CapVideoGeneration,
			canonical.CapSpeechSynthesis,
			canonical.CapSpeechRecognition,
			canonical.CapEmbedding,
			canonical.CapRerank,
			canonical.CapWebSearch,
		},
		Elsewhere: map[canonical.Capability]Protocol{
			canonical.CapStatefulConversation:   ProtoOpenAIResponses,
			canonical.CapRealtimeSession:        ProtoDashScopeRealtime,
			canonical.CapRealtimeServerVAD:      ProtoDashScopeRealtime,
			canonical.CapRealtimeInterruptTurns: ProtoDashScopeRealtime,
			canonical.CapRealtimeImageInput:     ProtoDashScopeRealtime,
			canonical.CapRealtimeCommitModes:    ProtoDashScopeRealtime,
		},
		Impossible: map[canonical.Capability]string{
			canonical.CapReasoningSignature: whyNoSignature,
			canonical.CapPromptCache:        whyAutoCache,
			canonical.CapComputerUse:        whyNoComputerUse,
		},
	})

	// —— DashScope Realtime（B 类，/api-ws/v1/realtime）——
	//
	// 与 OpenAI Realtime 事件模型同构，但多出两样 OpenAI 没有的东西：
	// input_image_buffer 的图像输入，以及 Qwen-TTS 的 server_commit / commit
	// 提交模式。这两项是 DashScope 侧的净增量。
	register(&Expressibility{
		Protocol: ProtoDashScopeRealtime,
		Capabilities: []canonical.Capability{
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapParallelToolCalls,
			canonical.CapVisionInput,
			canonical.CapAudioInput,
			canonical.CapAudioOutput,
			canonical.CapSpeechSynthesis,
			canonical.CapSpeechRecognition,
			canonical.CapStatefulConversation,
			canonical.CapRealtimeSession,
			canonical.CapRealtimeServerVAD,
			canonical.CapRealtimeInterruptTurns,
			canonical.CapRealtimeImageInput,
			canonical.CapRealtimeCommitModes,
		},
		Elsewhere: map[canonical.Capability]Protocol{
			canonical.CapStructuredOutput: ProtoDashScopeNative,
			canonical.CapReasoning:        ProtoDashScopeNative,
			canonical.CapVideoInput:       ProtoDashScopeNative,
			canonical.CapFileInput:        ProtoDashScopeNative,
			canonical.CapImageGeneration:  ProtoDashScopeNative,
			canonical.CapVideoGeneration:  ProtoDashScopeNative,
			canonical.CapEmbedding:        ProtoDashScopeNative,
			canonical.CapRerank:           ProtoDashScopeNative,
			canonical.CapWebSearch:        ProtoDashScopeNative,
		},
		Impossible: map[canonical.Capability]string{
			canonical.CapReasoningSignature: whyNoSignature,
			canonical.CapPromptCache:        whyAutoCache,
			canonical.CapComputerUse:        whyNoComputerUse,
		},
	})

	// —— DashScope Inference（A 类，/api-ws/v1/inference）——
	//
	// run-task 指令流，承载 Paraformer 实时 ASR 与 CosyVoice 流式 TTS。
	// 能力集很窄，但这套线格式是 OpenAI 兼容层完全表达不了的——
	// 它没有 HTTP 状态码，失败以 task-failed 事件送达。
	register(&Expressibility{
		Protocol: ProtoDashScopeInference,
		Capabilities: []canonical.Capability{
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapAudioInput,
			canonical.CapAudioOutput,
			canonical.CapSpeechSynthesis,
			canonical.CapSpeechRecognition,
		},
		Elsewhere: map[canonical.Capability]Protocol{
			canonical.CapToolCalling:            ProtoDashScopeNative,
			canonical.CapParallelToolCalls:      ProtoDashScopeNative,
			canonical.CapStructuredOutput:       ProtoDashScopeNative,
			canonical.CapReasoning:              ProtoDashScopeNative,
			canonical.CapVisionInput:            ProtoDashScopeNative,
			canonical.CapVideoInput:             ProtoDashScopeNative,
			canonical.CapFileInput:              ProtoDashScopeNative,
			canonical.CapImageGeneration:        ProtoDashScopeNative,
			canonical.CapVideoGeneration:        ProtoDashScopeNative,
			canonical.CapEmbedding:              ProtoDashScopeNative,
			canonical.CapRerank:                 ProtoDashScopeNative,
			canonical.CapWebSearch:              ProtoDashScopeNative,
			canonical.CapStatefulConversation:   ProtoDashScopeRealtime,
			canonical.CapRealtimeSession:        ProtoDashScopeRealtime,
			canonical.CapRealtimeServerVAD:      ProtoDashScopeRealtime,
			canonical.CapRealtimeInterruptTurns: ProtoDashScopeRealtime,
			canonical.CapRealtimeImageInput:     ProtoDashScopeRealtime,
			canonical.CapRealtimeCommitModes:    ProtoDashScopeRealtime,
		},
		Impossible: map[canonical.Capability]string{
			canonical.CapReasoningSignature: whyNoSignature,
			canonical.CapPromptCache:        whyAutoCache,
			canonical.CapComputerUse:        whyNoComputerUse,
		},
	})
}
