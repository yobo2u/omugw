package degrade

import (
	"github.com/yobo2u/omugw/internal/canonical"
)

// 反复出现的处置说明，抽出来避免同一条理由在各路径里写出不同版本。
const (
	noteNoAudioIn    = "Anthropic Messages 不接受音频输入"
	noteNoAudioOut   = "该 Provider 不产生音频输出"
	noteFileRefBound = "文件引用绑定具体 Provider，跨 Provider 不可迁移；" +
		"网关不代下载再上传（原则 2.6），请改用 URL 或内联字节"
	noteStrictSchema = "该 Provider 无 strict json_schema 校验，schema 降级为提示词约束，" +
		"模型可能返回不合规 JSON"
	noteSearchSwitch = "DashScope 的 enable_search 是布尔开关，承载不了 OpenAI web_search 工具的" +
		"参数，仅开关本身被映射"
	noteBuiltinTool = "该内建工具在 Phase 1 不做跨 Provider 映射——各家的工具 schema 不兼容，" +
		"勉强映射只会让模型收到一个它读不懂的定义"
	noteImageViaJobs = "Responses 的内建 image_generation 工具在 Phase 1 不做路由，" +
		"请改用 /v1/jobs 端点"

	// 网关侧模拟服务端会话的代价。这句话必须跟着 EMULATE 一起出现——
	// 客户端拿到的能力是完整的，但这份完整性是网关垫出来的，
	// 运维得知道它的边界在哪。
	noteEmulatedSession = "上游无服务端会话，由网关侧 ConversationStore 模拟提供。" +
		"Phase 1 为内存态：单副本正确，进程重启后历史丢失，多副本部署下会话不共享。" +
		"默认关闭，需显式开启 convstore"
)

// Phase1 注册的全部路径当前均为 PLANNED。
//
// 这不是遗漏。M0 建立的是声明层——矩阵、可表达性、错误映射、测试基座——
// 而 codec 与 transport 属于 M1。让路径默认 PLANNED，是让这个事实在运行时
// 和文档里都藏不住（见 ADR-0001）。M1 转正前三条 openai.responses 路径时，
// 会在对应位置加上 Redeem，且必须先有 fixture。

// Phase1 构造 Phase 1 的降级矩阵。
//
// 只登记**已实现**的转换路径。后续里程碑新增路径时必须在这里补充完整声明，
// 否则该路径在 Check 时会以「未注册的转换路径」失败——这正是期望的行为。
//
// 每条路径只需为入站协议**表达得出来**的能力表态；其余由 Expressibility
// 自动补成 NotApplicable（见 expressibility_phase1.go）。
func Phase1() (*Matrix, error) {
	m := NewMatrix()

	// ————————————————————————————————
	// OpenAI Chat Completions 入站
	// ————————————————————————————————

	// 同源快通道。字节级透传，只改写鉴权，不进 Canonical。
	// 客户端能表达的每一项它都原样转发——包括我们没特别处理过的字段。
	//
	// 转正的第二条路径（第一条是 Responses 同源直通）。门槛同样是端到端
	// fixture 通过，见 testdata/routes/openai.chat__openai.compat/ 与 ADR-0001。
	chatToOpenAI := NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoOpenAIChat)...).
		Redeem(EndpointOpenAIChat, ExpressibleSet(ProtoOpenAIChat)...)
	if err := m.Add(chatToOpenAI.Build()); err != nil {
		return nil, err
	}

	chatToAnthropic := NewRoute(ProtoOpenAIChat, ProviderAnthropicMessages).
		Pass(
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapParallelToolCalls,
			canonical.CapVisionInput,
			canonical.CapReasoning,
		).
		Degrade(noteStrictSchema, canonical.CapStructuredOutput).
		Reject(noteNoAudioIn, canonical.CapAudioInput).
		Reject(noteNoAudioOut, canonical.CapAudioOutput).
		Reject(noteFileRefBound, canonical.CapFileInput).
		Reject(noteBuiltinTool, canonical.CapWebSearch)
	if err := m.Add(chatToAnthropic.Build()); err != nil {
		return nil, err
	}

	chatToDSCompat := NewRoute(ProtoOpenAIChat, ProviderDashScopeCompatible).
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
		Degrade(noteSearchSwitch, canonical.CapWebSearch).
		Reject(noteFileRefBound, canonical.CapFileInput).
		Reject("兼容模式不返回音频；音频输出须经 Qwen-Omni Realtime 或 Native 端点",
			canonical.CapAudioOutput)
	if err := m.Add(chatToDSCompat.Build()); err != nil {
		return nil, err
	}

	chatToDSNative := NewRoute(ProtoOpenAIChat, ProviderDashScopeNative).
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
			canonical.CapAudioOutput)
	if err := m.Add(chatToDSNative.Build()); err != nil {
		return nil, err
	}

	// ————————————————————————————————
	// OpenAI Responses 入站（入站优先级第一）
	// ————————————————————————————————
	//
	// 从对应的 Chat 路径派生，差异逐条 Override 出来。相对 Chat，Responses 多出
	// 三样真东西：服务端会话、内建工具（computer_use / image_generation）、
	// 更完整的推理配置。前者由网关模拟，后两者 Phase 1 不做跨 Provider 映射。

	responsesExtras := func(r *Route, homogeneous bool) *Route {
		r = r.Emulate(FeatureConversationStore, noteEmulatedSession,
			canonical.CapStatefulConversation)
		if homogeneous {
			// 字节直通路径原样转发一切，内建工具也不例外。
			// 早先把 computer_use 在直通路径上判成 REJECT 是错的：
			// 网关根本没碰这个字段，凭什么替上游拒绝。
			return r.Pass(canonical.CapComputerUse, canonical.CapImageGeneration)
		}
		return r.
			Reject(noteBuiltinTool, canonical.CapComputerUse).
			Reject(noteImageViaJobs, canonical.CapImageGeneration)
	}

	// M1 转正的第一条路径。
	//
	// 转正的门槛不是「有人认为写完了」，而是端到端 fixture 通过——
	// 见 testdata/routes/openai.responses__openai.compat/ 与 ADR-0001。
	// TestImplementedRoutesAreExplicit 要求每次转正都同步更新那份名单，
	// 让「这条路能用了」成为一个需要有人点头的动作。
	if err := m.Add(responsesExtras(
		chatToOpenAI.Derive(ProtoOpenAIResponses, ProviderOpenAICompat), true,
	).Redeem(EndpointOpenAIResponses, ExpressibleSet(ProtoOpenAIResponses)...).Build()); err != nil {
		return nil, err
	}
	for _, base := range []*Route{chatToAnthropic, chatToDSCompat, chatToDSNative} {
		r := responsesExtras(base.Derive(ProtoOpenAIResponses, base.OutProvider()), false)
		if err := m.Add(r.Build()); err != nil {
			return nil, err
		}
	}

	// ————————————————————————————————
	// DashScope Native 入站（入站优先级第二）
	// ————————————————————————————————
	//
	// 同源快通道。讲原生协议的客户端本来就不需要任何转换，让它们走兼容层是
	// 净损失。这条路径的保留度是满分，也应该是满分。
	//
	// 转正的第三条路径。DashScope Native 一个协议对应多个上游端点，当前投放了
	// 文本生成与多模态生成两个（/api/v1/services/aigc/{text-generation,
	// multimodal-generation}/generation）。
	//
	// 因此处置与投放在这里第一次分了家：上面的 Pass 是**设计处置**——这条同源
	// 直通路最终对每项能力都该原样转发；下面的 Redeem 是**当前投放**——只有这两扇
	// 门真的写了，且各自兑现各自的能力。embedding、rerank 那几个端点还没动工，
	// 它们的能力在运行时返回 501，而不是被当作可用。
	//
	// 两扇门的兑现集合互不相通，也不做并集：并集里的 8 项没有任何一扇真实存在
	// 的门同时提供，按并集记分就是在为一个不存在的门背书。
	// 门槛同样是端到端 fixture 通过，见
	// testdata/routes/dashscope.native__dashscope.native/ 与 ADR-0001。
	if err := m.Add(NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoDashScopeNative)...).
		// 文本门：既有 5 项，原样保留。reasoning 有 fixture 证据
		//（tools-and-search.json 带 enable_thinking: true），继续持有。
		Redeem(EndpointDashScopeTextGeneration,
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapReasoning,
			canonical.CapWebSearch,
		).
		// 多模态门：本期投放恰 5 项，证据是 multimodal-basic /
		// multimodal-streaming / multimodal-video-frames 三份 fixture。
		// file_input 不兑现：官方内容块词表是 text / image / audio / video，
		// 没有通用 file 块。tool_calling / reasoning / web_search 在这扇门上
		// 没有 fixture 证据——无证据不兑现，哪怕上游模型多半也支持。
		Redeem(EndpointDashScopeMultimodal,
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapVisionInput,
			canonical.CapAudioInput,
			canonical.CapVideoInput,
		).
		Build()); err != nil {
		return nil, err
	}

	// ————————————————————————————————
	// OpenAI Realtime 入站
	// ————————————————————————————————

	if err := m.Add(NewRoute(ProtoOpenAIRealtime, ProviderOpenAIRealtime).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoOpenAIRealtime)...).
		Build()); err != nil {
		return nil, err
	}

	// 近同构快通道——本项目的头牌能力。
	//
	// DashScope 的 /api-ws/v1/realtime 与 OpenAI Realtime 事件模型基本一致
	// （session.update / input_audio_buffer.append / response.create /
	// response.audio.delta ...），绝大多数事件原样转发即可。
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
		Build()); err != nil {
		return nil, err
	}

	// ————————————————————————————————
	// DashScope Realtime 入站（B 类，/api-ws/v1/realtime）
	// ————————————————————————————————

	if err := m.Add(NewRoute(ProtoDashScopeRealtime, ProviderDashScopeWSRealtime).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoDashScopeRealtime)...).
		Build()); err != nil {
		return nil, err
	}

	// 反向路径：DashScope Realtime 客户端 → OpenAI Realtime 上游。
	// 与上面那条对称，代价也对称——只是重采样方向反过来，
	// 且 DashScope 侧多出的两项能力在 OpenAI 侧没有落点。
	if err := m.Add(NewRoute(ProtoDashScopeRealtime, ProviderOpenAIRealtime).
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
		Degrade("输入音频需从 DashScope 的 16 kHz 重采样到 OpenAI 的 24 kHz；"+
			"上采样补不回原本就没采到的高频信息，只是满足格式要求",
			canonical.CapAudioInput).
		Degrade("OpenAI Realtime 未提供并行工具调用开关，行为由上游模型决定",
			canonical.CapParallelToolCalls).
		Reject("OpenAI Realtime 没有 input_image_buffer 事件，图像输入无处安放",
			canonical.CapRealtimeImageInput, canonical.CapVisionInput).
		Reject("server_commit / commit 是 Qwen-TTS-Realtime 特有的提交模式，"+
			"OpenAI Realtime 协议中没有对应字段",
			canonical.CapRealtimeCommitModes).
		Build()); err != nil {
		return nil, err
	}

	// ————————————————————————————————
	// DashScope Inference 入站（A 类，/api-ws/v1/inference）
	// ————————————————————————————————
	//
	// run-task 指令流，承载 Paraformer 实时 ASR 与 CosyVoice 流式 TTS。
	// 在此之前这个 Provider 没有任何入站路径指向它——两个模型从任何入口都
	// 到不了。这条路径就是补上那个洞。
	if err := m.Add(NewRoute(ProtoDashScopeInference, ProviderDashScopeWSInference).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoDashScopeInference)...).
		Build()); err != nil {
		return nil, err
	}

	// 最后一道校验：所有「该能力请去别处」的转介，目标必须真的存在。
	// 少了这一步，一句「realtime 请走 dashscope.realtime」可以指向一个从未
	// 注册的协议，用户按提示改了协议还是撞墙。
	if err := m.checkElsewhereTargets(); err != nil {
		return nil, err
	}

	return m, nil
}
