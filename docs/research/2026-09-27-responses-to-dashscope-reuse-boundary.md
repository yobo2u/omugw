# openai.responses → dashscope.* 复用边界调研（Wayfinder 研究工单 #9）

- **日期**：2026-09-27
- **基线**：`main` @ `5eaf36e`（`GIT_MASTER=1 git rev-parse HEAD` 核对一致）
- **目的**：为 `openai.responses → dashscope.compatible` 与 `openai.responses → dashscope.native`
  两条 PLANNED 路径的实施计划提供**复用/新建边界**的事实依据。本文件只回答
  「既有的 openai.chat 转换机器哪些能直接用、哪些耦合了入站线格式、哪些根本不存在」，
  **不提出实施方案**。
- **方法**：只读审查（文件阅读 + `rg`/`grep` + `GIT_MASTER=1 git log/show`）。
  每条结论标注【事实】（代码/文档/命令输出可指认）或【推断】（由代码路径推演，
  未运行验证）。无法核实的项集中在第 8 节，绝不以估算冒充实证。
- **约定**：所有 `file:line` 均相对仓库根，以基线提交为准。

## 0. 结论速览

| 问题 | 结论（一句话） |
|---|---|
| dashscopecompat 是否只依赖 Canonical | **否，且它根本不读 Canonical**——依赖 `Raw` 是 Chat 线格式、`Inbound.Endpoint` 是上游路径两个入站事实（§2.1） |
| dashscopenative 是否只依赖 Canonical | **核心编解码是，两端不是**——请求编码/响应解码走 Canonical 与 Native wire，可复用；分派、投影输入、Chat wire 输出三处 Chat 耦合（§2.2） |
| 两个入站解码器的 Canonical 差异 | 8 处字段级差异 + 2 处能力报告差异，其中 `parallel_tool_calls` 不被 Responses 报告、`metadata` 会被 Native 编码器静默丢弃是两个必须处置的坑（§3） |
| 响应侧覆盖 | Responses 出站编码器**已存在且单测覆盖，但全仓库零生产调用方**；缺的是「上游结果 → Canonical/事件」的桥与 `finish_reason → StopReason` 映射（§4） |
| convstore 能否原样用于异构出站 | **主体能，条件有二、雷区一**：出站必须回 Responses wire；`store` 字段的 422 规则不能照抄 Chat；思考历史会让第二轮请求在 Native 编码器上撞 400（§5） |
| 复用比例 | dsnative 路径按 LOC 口径约 **80–85%** 复用（新建集中在响应侧桥接与投影，约 600–800 行）；dscompat 路径**取决于一个尚未调研的上游契约分叉**，两个形态分别约 25–35% 与 85–90%（§7） |

---

## 1. 两条路径在矩阵中的现状

【事实】两条路径由 `Derive` 从对应 Chat 路径派生，`responsesExtras` 统一叠加
Responses 特有的三格（`internal/degrade/rules_phase1.go:181-193`）：

- `stateful_conversation` → `EMULATE`（开关 `convstore`，note 写明内存态边界）；
- 异构路径上 `computer_use` / `image_generation` → `REJECT`（内建工具 Phase 1 不做跨 Provider 映射）。

派生循环在 `rules_phase1.go:206-211`：对 `chatToAnthropic` / `chatToDSCompat` /
`chatToDSNative` 三个基准各派生一条 Responses 路径，**没有任何 `Redeem`**。

【事实】`Derive()` 不继承兑现集合——`matrix.go:381-382` 注释明说「兑现集合不在
继承之列：它说的是『这条路径的实现写好了』，而实现是逐条写的」；`matrix.go:389-391`
给派生路径配全新 state；`matrix_test.go:259-271`（`TestDeriveDoesNotInheritRedemption`）
钉住这一行为。派生时还过滤掉基准协议的 N/A 格与派生协议表达不出的格
（`matrix.go:394-401`），所以两条路径的格子是按 **Responses 的可表达性**
（`expressibility_phase1.go:69-106`，15 项可表达）重新补齐的。

【事实】运行时行为：路径未兑现 → `Implemented()` 为假 → `Check` 闸门 2 返回
`ClassNotImplemented`（`matrix.go:689-694`）；选路层 `RankOutbound` 直接过滤未实现
路径（`preference.go:215-219`），全部候选都不可用时 `BestOutbound` 返回 501
「候选出站路径均已设计但尚未实现」（`preference.go:275-280`）。
`matrix_test.go:277-301`（`TestPlannedRouteIsRejectedAtRuntime`）正是拿
`openai.responses → dashscope.compatible` 当 PLANNED 样例钉住 501 行为。

【事实】设计分（`docs/degradation-matrix.md:54-55`，公式见 `preference.go:143-150`，
分母 = 8+1+2+4 = 15 / 6+1+4+4 = 15）：

| 路径 | PASS | EMULATE | DEGRADE | REJECT | 设计分 |
|---|---:|---:|---:|---:|---:|
| `openai.responses → dashscope.compatible` | 8 | 1（默认关） | 2 | 4 | 0.667 |
| `openai.responses → dashscope.native` | 6 | 1（默认关） | 4 | 4 | 0.600 |

【事实】全矩阵 14 条路径、5 条已实现（`matrix_test.go:118-158` 白名单）。剩余
9 条 PLANNED 中：5 条是 WebSocket 传输（realtime 族），2 条出站指向
`anthropic.messages`（该协议族没有出站适配器，`build.go:104-110` 在启动阶段直接
拒绝装配；Anthropic 族按 README 属 Phase 2），**本工单的两条是仅有的出站适配器
已通车的纯 HTTP 路径**。

【事实】两份既有实施计划都显式把这两条路径排除在范围外——Chat→Native 计划的
范围纪律写明「不碰 `openai.responses → dashscope.native`」
（`docs/superpowers/plans/2026-08-25-openai-chat-dashscope-native.md:33`），其设计
文档把 `openai.responses -> dashscope.native` 列进「非目标」
（`docs/superpowers/specs/2026-08-22-openai-chat-dashscope-native-design.md:418`）；
Chat→Compatible 计划同样写明「不碰 Responses 到 dashscope.compatible」
（`docs/superpowers/plans/2026-08-15-openai-chat-dashscope-compatible.md:31`）。
即：既有工程是**刻意**为后续路径留好地基而非顺手带过，这解释了为什么大量
组件天然入站无关（§2、§7）。

---

## 2. 出站适配器解耦判定

判定问题：适配器是只消费 `provider.Request.Canonical`，还是也依赖入站特有形态？

`provider.Request` 的契约本身允许两种消费方式——「同时带上原始字节与 Canonical
表示，是刻意的：同源直通用前者……异构转换用后者。让适配器自己挑」
（`internal/provider/provider.go:15-19`）。`Inbound` 字段的注释同样明说
「异构适配器用它分派实现」（`provider.go:33-38`）。

### 2.1 dashscopecompat：双重入站耦合，且完全不读 Canonical

【事实】判定：**依赖入站特有形态，不依赖 Canonical**。三处证据：

1. **消费 `Raw` 而非 `Canonical`**：`Call` 第一步就是 `patch(req.Raw, …)`
   （`internal/provider/dashscopecompat/dashscopecompat.go:54-58`），注释明说
   「Canonical 已在网关层完成请求校验与能力裁决，这里不消费——本路径没有
   Canonical 出站编码器」（`:52-53`）。这是 Chat→Compatible 计划的绑定决策 4：
   「原始 JSON 修补器模型……不写 Canonical 出站编码器」
   （`plans/2026-08-15-…-compatible.md:38`）。
2. **`patch` 假设 Chat 线格式**：只改 `model`、把 `web_search_options` 映射成
   `enable_search:true`（`dashscopecompat.go:134-169`）。`web_search_options`
   是 Chat wire 字段（`openaichat/wire.go:54`）；Responses wire 没有它——
   Responses 的搜索是 `tools[]` 里的内建工具（`openairesponses/decode.go:504-515`）。
   对 Responses 请求体，`patch` 的搜索映射是死代码。
3. **`pathFor` 把入站门当上游路径**：`req.Inbound.Endpoint` 非空时原样用作
   上游路径（`dashscopecompat.go:90-96`）。Chat 入站门下这恰好正确
   （`/v1/chat/completions` 两侧同名）；Responses 入站门是 `/v1/responses`
   （`gateway/handler.go:159`），适配器会把 Responses 线格式的请求体 POST 到
   `{base}/v1/responses`。

【事实】由此产生一个**形态分叉**（这是判定复用比例的前置问题，不是实施建议）：

- **形态 A（Responses wire 直通 DashScope 兼容层）**：仓库自己的调研笔记已证实
  上游存在这个端点——「CN《OpenAI Responses接口兼容》documents
  `POST /compatible-mode/v1/responses`」
  （`docs/research/dashscope-compatible-get-v1-models.md:146-150`），且
  `docs/research/2026-08-22-dashscope-native-text-generation-contract.md:20`
  记录百炼文本生成有四套接口，其中含「OpenAI 兼容 Responses」。
  按 §2.1 的三处证据推演【推断】：现有适配器**零改动**即可把 Responses 请求体
  （改写 model 后）送到该端点，响应按 Responses wire 原样回传，relay 的
  usage 抽取（`relay.go:201-209`）、终止判定（`handler.go:151-158`）与 convstore
  重写（§5）全部对得上。**但**该端点的字段级契约（支持哪些参数、`store` /
  `previous_response_id` 是否生效、内建工具是否 honored、错误信封形态）在仓库内
  **没有任何调研记录**——见 §8 开放问题 Q1。矩阵格子的 note 也是按 Chat wire
  措辞继承来的（如 `web_search` 的 note 谈 `search_context_size`/`user_location`，
  `docs/degradation-matrix.md:487`），形态 A 下须逐格核对并 `Override`。
- **形态 B（重编码为 Chat wire，打 `/v1/chat/completions`）**：与 Chat 路径同构，
  矩阵格子措辞基本成立，但需要一整套「Canonical → Chat wire 请求」与
  「Chat wire 响应 → Responses wire」的编解码——两者在仓库里**都不存在**（§4.3）。
  且 `pathFor` 的「入站门即上游路径」行为要改（Responses 门下它指向
  `/v1/responses` 而非 `/v1/chat/completions`）。

【事实】无论哪个形态，有一条既有架构不变量约束响应侧：Chat→Native 计划明写
Gateway 只负责「转发**已换成入站协议 wire** 的响应」
（`plans/2026-08-25-…-native.md:7`）——即出站适配器必须返回入站线格式的响应体。
违反它的后果在 §5.3 用代码路径证明（usage 记成权威零值、流式永远等不到终止事件）。

### 2.2 dashscopenative：核心机器入站无关，分派与两端 Chat 耦合

【事实】判定：**分层**——按组件给出：

| 组件 | 消费什么 | 入站耦合？ | 证据 |
|---|---|---|---|
| `Provider.Call` 分派 | `Inbound.Protocol` switch，`ProtoOpenAIResponses` 落 default → `ClassUnsupported` fail-closed | **是**（Responses 今天进不来） | `internal/provider/dashscopenative/provider.go:44-68`（default 分支 `:64-67`） |
| `openaichat.Project(req.Raw)` 投影 | Chat wire 原始字节，`DisallowUnknownFields` | **是**（Responses 字节会 400） | `provider.go:49-55`；`openaichat/projection.go:69-76` |
| `rejectUnmappable` 422 规则 | `Projection` 的 presence 位 + Canonical | **是**（规则表按 Chat 字段名点名：`frequency_penalty`/`logit_bias`/`service_tier`/`store`/`user`/`metadata`/`audio`） | `provider.go:57`；`reject.go:14-59` |
| `nativewire.EncodeRequest` 请求编码 | `*canonical.Request` + `ChatSampling` extra + 门 + 上游模型名 | **Canonical 侧否 / extra 侧是**（extra 由 Chat 投影填入；「协议包不互相 import，桥接在 provider 层」） | `nonstream.go:59`、`stream.go:52`；`protocol/dashscopenative/encode_request.go:30-47`、`:125-174` |
| `encodeMessages` / `encodeParameters` | 纯 Canonical + extra | **否** | `encode_request.go:180-244`；`encode_messages.go:12-29` |
| `nativewire.DecodeResult` / `DecodeFrame` | Native wire 字节 → `Result`（含 `canonical.Usage`） | **否** | `protocol/dashscopenative/result.go:109-112`、`:119-143` |
| Native SSE 读取机器（`nextNativeResult`、`decodeFrameError`、`streamError`、`closeAfterFailure`、`readNativeBody`、`decodeNativeError`） | Native wire / `sse.Reader` | **否** | `stream.go:136-168`、`:328-386`；`nonstream.go:198-222` |
| 首帧预读 + `wantChoices` 契约 | `proj.N`（Chat 特有；Responses wire 无 `n`） | **半**（机制入站无关，期望候选数来源是 Chat 投影） | `stream.go:85-101`；`nonstream.go:110-121` |
| `transformReader` 骨架（Read/Close/pending/finished/err 粘滞） | 输出 wire 无关的同步泵 | **否**（骨架）/ **是**（`queueChunk`→`openaichat.EncodeChunk`、`queueTerminal`→`[DONE]` 哨兵） | `stream.go:175-243`（骨架）；`:286-326`（Chat 输出） |
| `candidateState` 多候选状态机 | 产出 `[]openaichat.ChunkChoice` | **是**（输出类型 Chat 专属；且 Responses 入站表达不出 `n`，多候选不可达） | `candidates.go:34-48`；Responses wire 无 `n` 字段：`openairesponses/wire.go:14-51` |
| 非流式响应重编码 | `Result` → canonical Parts → `openaichat.CompletionInput` → `EncodeCompletion` | **是**（Parts 映射段 `:123-162` 与 Chat 类型熔接；映射逻辑本身入站无关） | `nonstream.go:123-186` |
| usage 回调 `OnDashScopeUsage` | 由 gateway 按 `kind == ProviderDashScopeNative` 注入，与入站协议无关 | **否** | `provider.go:45-50`（接口注释）；`handler.go:397-410`、`:461-463` |
| 语义头白名单 / URL 构造 / 凭据改写 | `req.Header` / `req.Target` / `req.Credential` | **否** | `nonstream.go:21-27`、`:64-77` |

【事实】gateway 侧配套同样入站无关：`filterNativeTargets` 按能力集过滤
Native 候选（`gateway/filter.go:13-33`），Responses 解码器经
`UsedCapabilities` 报告 `vision_input`/`audio_input`（`canonical/request.go:196-212`），
过滤逻辑无需改动即可服务 Responses 入站。`native_endpoint` 门是部署事实、
由配置声明（`plans/2026-08-25-…-native.md:42` 绑定决策 5；`build.go:124-127`）。

【推断】综合判定：**请求方向（Canonical → Native wire）约八成机器可直接复用**；
需要新建的是 Responses 侧的投影/422 规则（对应 Chat 的
`projection.go`+`reject.go`）与 `ChatSampling` extra 的 Responses 填法。
**响应方向（Native wire → 入站 wire）的 Chat 输出端全部不可复用**，
但被替换的恰好是已存在的 Responses 编码器（§4.1）。

---

## 3. 两个入站解码器产出的 `canonical.Request` 逐字段差异

对照 `openairesponses/decode.go:53-132`（Responses）与 `openaichat/decode.go:39-134`
（Chat）。两者都严格解码（`DisallowUnknownFields`：`openairesponses/decode.go:58`、
`openaichat/decode.go:44`），都跑 `r.Validate()` 后交付。

### 3.1 `canonical.Request` 字段级对照

| Canonical 字段 | Responses 来源 | Chat 来源 | 对两条新路径的影响 |
|---|---|---|---|
| `Model` | `model`（`decode.go:76`） | `model`（`decode.go:57`） | 无差异 |
| `MaxOutputTokens` | `max_output_tokens`（`:77`） | `max_tokens`/`max_completion_tokens` 二择一（`:58`、`:137-142`） | 【事实】Native 编码器的 max tokens **只读 extra、不读 Canonical**（`encode_request.go:201-209`，注释：不得据 Canonical 制造第三个限制）——Responses 侧桥接必须把 `max_output_tokens` 填进 extra 的 `MaxTokens` 或 `MaxCompletionTokens` 之一，否则该参数静默丢失 |
| `Temperature` / `TopP` | ✓（`:78-79`） | ✓（`:59-60`） | 无差异，Native 编码器直接消费（`encode_request.go:186-187`） |
| `Seed` | **不设置**——Responses wire 无 `seed` 字段（`wire.go:14-51`） | `seed`（`decode.go:61`） | 无影响：`putIf` 对 nil 跳过（`encode_request.go:188`、`:320-325`） |
| `StopSequences` | **不设置**——wire 无 `stop` | `stop` 字符串/数组（`:68-72`、`:144-159`） | 同上，`encodeStop` 对空返回 nil（`encode_request.go:230-232`、`:246-256`） |
| `Metadata` | `metadata`（`:80`） | `metadata`（`:62`） | 【事实·坑】Native 请求编码器**从不消费** `canon.Metadata`（`encodeParameters` 全文无它，`encode_request.go:180-244`）。Chat 路径靠投影 presence 位在出门前 422 拒绝 `metadata`（`reject.go:31-33`）；Responses 解码器把 metadata 放进 Canonical 而**没有任何 presence 报告机制**——Responses 侧 422 规则若不补上同一条，`metadata` 会被静默丢弃，正是原则 2.1 要防的「丢半数字段还返回 200」 |
| `Stream` | `stream`（`:82-84`） | `stream`（`:64-66`） | 无差异 |
| `System` | 顶层 `instructions` → 单条文本 Part（`:86-90`，注释：各出站协议各自还原，DashScope 用 system role） | `messages` 中 system/developer 角色内容归并（`:74-79`、`:191-193`） | 无影响：`encodeMessages` 把 System 编成首条 system 消息（`encode_messages.go:12-20`），两种来源同形 |
| `Messages` | `input` 条目数组：message / function_call → assistant+ToolCall / function_call_output → tool+ToolResult；**reasoning 条目原样丢弃**（`:257-321`，丢弃理由 `:312-315`） | `messages`：assistant.tool_calls 归一 PartToolCall、refusal → PartRefusal、tool → PartToolResult（`:165-230`） | 结构同形，Native 编码器全部认得（`encode_messages.go:36-106`）。差异点见 3.2 的 `Name` 与 §5.4 的 PartThinking |
| `Message.Name` | **永不设置**——Responses wire 的条目没有 name（`wire.go:88-102`） | 保留（`decode.go:196`、`:209`、`:217`） | 一致于可表达性声明：`message_name` 对 Responses 是 N/A（Elsewhere → openai.chat，`expressibility_phase1.go:99`），矩阵无此格，无需处置 |
| `Tools` | 仅 function 工具进 Canonical；内建工具（web_search/computer/image_generation）转成 `builtinCaps` 只供矩阵裁决，**不进 Tools**（`:44-46`、`:450-502`、`:467-475`） | function 工具（嵌套形态，`:366-395`） | 【事实·坑】Native 编码器的 `enable_search` 只来自 `extra.WebSearch`（`encode_request.go:233-237`），而 Canonical **没有** web-search 字段（`canonical/request.go:14-45`）。Chat 靠投影从 `web_search_options` 提取（`projection.go:107-114`）；Responses 的搜索意图在 `builtinCaps` 里，矩阵看得见、编码器看不见——Responses 侧投影必须从 wire 的 `tools[]` 内建工具重新提取，否则 DEGRADE 格声明的「仅开关本身被映射」连开关都没映射 |
| `ToolChoice` | 字符串/具名对象；具名内建工具的 tool_choice 归 nil（`:550-586`） | 字符串/`{type:function,function:{name}}`（`:397-427`） | 无影响：`encodeToolChoice` 消费 Canonical（`encode_request.go:301-318`） |
| `Reasoning` | `reasoning.effort` + **`Visible = (summary != "")`**（`:588-609`，`:592`） | 仅 `reasoning_effort`，Visible 永不设置（`:463-475`） | 【事实】Native 编码器只消费 `Effort`（`encodeReasoning`，`encode_request.go:331-345`），`Visible` 在请求方向无落点。响应方向 Native 的 `reasoning_content` 会被编成 Responses reasoning 项的 summary（§4.1），所以「要摘要」的意图事实上由上游思考输出满足；但 `summary` 参数本身（auto/concise/detailed 档位）丢失与否**没有矩阵格子声明**——见 §8 Q3 |
| `ResponseFormat` | `text.format`（text/json_object/json_schema，`:611-638`） | `response_format`（`:429-461`） | 无差异，`encodeResponseFormat` 消费 Canonical（`encode_request.go:258-283`）；strict 无上游保证正是两条路径共同的 `structured_output` DEGRADE 格 |
| `Modalities` | **不设置**——wire 无 `modalities`/`audio` | `modalities`（`:95-97`、`:477-493`） | 【事实】`audio_output` 能力只能由 `Modalities` 触发（`canonical/request.go:224-228`），Responses 解码器永不设置它 → 两条路径的 `audio_output` REJECT 格（`degradation-matrix.md:473`、`:507`）**运行时不可达**。fail-closed 方向无害，但可表达性声明（`expressibility_phase1.go:82` 把 `audio_output` 列为 Responses 可表达）与解码现实不一致——见 §8 Q4 |
| `Extensions` | `parallel_tool_calls` 原样回填（`:120-125`） | 同（`:99-107`） | 异构路径禁读 Extensions（根 AGENTS 反模式；`encode_request.go:122-124` 注释同旨），故该字段对两条新路径**只能靠投影**，见 3.2 |
| `TopK` / `Cache` | 都不设置 | 都不设置 | 无差异 |

### 3.2 `Decoded` 元信息与能力报告差异

| 维度 | Responses（`decode.go:17-47`、`:648-667`） | Chat（`decode.go:16-34`、`capability.go:20-45`） |
|---|---|---|
| 会话元信息 | `PreviousResponseID` / `WantsStore` / `RequiresStatefulConversation` / `ReplayInlineBytes` | 无（Chat 无状态，`decode.go:13-15` 注释） |
| web_search | 经 `builtinCaps`（tools 内建工具或 `web_search_call` 输入条目，`:189-255`、`:504-515`） | 经 `webSearch` 布尔（`web_search_options` 非 null，`:109-117`） |
| parallel_tool_calls | **不报告**——`Capabilities()` 只并 `UsedCapabilities` + `builtinCaps` + stateful（`:648-666`）；wire 值只进 Extensions（`:120-125`）；`UsedCapabilities` 也没有它（`canonical/request.go:159-243`） | 显式 true 时报告（`capability.go:32-34`） |
| message_name | 不适用（N/A） | `messageName` 布尔报告（`capability.go:35-37`） |
| 内联字节 | `InlineBytes` + `ReplayInlineBytes` 分列（`:37-42`） | 仅 `InlineBytes` |

【事实·坑】`parallel_tool_calls` 不被 Responses 报告，意味着两条新路径的对应格子
（dscompat 是 PASS、dsnative 是 DEGRADE，`degradation-matrix.md:464`、`:498`）
**运行时永远不会被 `Check` 问到**：`X-Omugw-Degraded` 头不会出现
`parallel_tool_calls=` 条目，已转正的 `openai.responses → openai.compat` 对该能力的
兑现（`degradation-matrix.md:69`）同样是空转。这不是本工单要修的东西，但实施
计划必须知道：**要么 Responses 解码器补报告（对齐 Chat 的 `capability.go:32-34`），
要么接受降级头缺这一项**——两者都是行为决定，不能靠编码器缺省注入 `true`
（`encode_request.go:194-200`）悄悄替代，因为注入解决的是上游行为，头解决的是
客户端知情。

---

## 4. 响应侧覆盖盘点

### 4.1 已存在：Responses 出站编码器（但从未接线）

【事实】`openairesponses` 已有完整的「Canonical → Responses wire」两个方向：

- **非流式** `EncodeResponse(model, msg, usage, stop, createdAt)`
  （`response.go:156-187`）：消费单条 `canonical.Message`，text/refusal 合并进
  message 项、tool_call 与 thinking 各自成项（`encodeOutput`，`:194-293`）；
  media 块显式报错不静默丢（`:274-278`）；usage 只有 authoritative 才输出
  （`toWireUsage`，`:118-139`）；`stop → status/incomplete_details` 映射
  （`statusFor`，`:141-154`：max_tokens/content_filter → incomplete，
  interrupted → failed，其余 → completed）。
- **流式** `StreamEncoder`（`stream.go:36-145`）：消费 `canonical.Event`，产出
  Responses 全套 SSE 事件（created/in_progress/output_item/content_part/
  text.delta/args.delta/summary.delta/各 done/completed/incomplete/failed/error，
  事件名常量 `:12-34`）；有状态（三层编号 + `response.completed` 附完整 output
  数组，`:36-42` 注释）；`ResponseID()`/`Usage()` 访问器供会话存储登记（`:534-538`）。

【事实】两者**在生产代码里零调用方**——全仓库引用只出现在本包测试
（`grep -rn "EncodeResponse\|NewStreamEncoder" internal/ tests/` 过滤测试后为空；
测试调用方仅 `openairesponses/encode_test.go`）。responses→openai.compat 是字节
直通，不需要出站编码。它们的出身正是为异构路径预备的：提交 `6275d36`
（"M1：Responses 响应编码——非流式与流式状态机"）的提交信息明说
「第一次做『Canonical → 线格式』的方向。**异构路径绕不开它；直通路径不需要**」。

【推断】因此这两个编码器属于「已写好、已单测、未经任何端到端 fixture 验证」的
资产：复用成本低，但按 ADR-0001 的口径，它们的正确性证据要等本次路径的
fixture/golden 才算闭环。

### 4.2 已存在：Native 响应解码与流读取

【事实】`nativewire.DecodeResult`/`DecodeFrame`（`result.go:108-112`）把非流式裸
body 与流式 SSE data 解成同一个 `Result`（`request_id` + `choices[]` +
authoritative `usage`，`:15-19`、`:208-221`）；两种 "null" finish_reason 归一
（`:193-202`）；工具参数字符串原样保留、不提前校验（`:37-43`）。
流读取机器（首帧预读、错误帧先于成功解码、status_code 压过 code 的判级、
EOF/裸 error 归一成 `*canonical.Error`）全部入站无关（§2.2 表）。

【事实】`Result` → canonical Parts 的映射逻辑已存在于
`nonstream.go:123-162`（reasoning_content → PartThinking、content → Text、
tool_calls → PartToolCall 且参数必须合法 JSON），但**与 `openaichat.CompletionChoice`
类型熔接**，不是独立函数——Responses 侧要用它，得把这段映射从 Chat 类型里
剥出来或按同样规则重写（约 40 行的模式复制，非从零设计）。

### 4.3 不存在：必须新建的响应侧组件

逐项核实（均为【事实】，以 grep/目录清单为证）：

1. **`finish_reason → canonical.StopReason` 映射**：全仓库不存在。
   `StopReason` 的消费方只有 `statusFor`（`response.go:141-154`）与
   `Accumulator`；Chat 响应侧把 `FinishReason` 当不透明字符串原样透传
   （`encode_response.go:22-26`、`nonstream.go:149-152`）。Responses 的
   `status`/`incomplete_details` 语义要求把 `length`→`StopMaxTokens`、
   `content_filter`→`StopContentFilter`、`tool_calls`/`stop`→对应值——**新写**，
   且映射表要以真实录制为证（Native 的 finish_reason 取值集合见
   `docs/research/2026-08-22-…-contract.md`）。
2. **Native 帧 → `canonical.Event` 生产者**：不存在。`EventTextDelta` /
   `EventContentStart` / `EventToolCallStart` 等事件常量在测试外只有消费方
   `openairesponses/stream.go` 引用（grep 证实）。Chat 流式路径**刻意**不经过
   事件层——`candidates.go:114-119` 注释明说不能借道 `canonical.Accumulator`
   （它闭合时要求参数是合法 JSON，而 Native 流式参数是未闭合片段），绑定决策 9
   同旨（`plans/2026-08-25-…-native.md:46`）。Responses 流式要么新建
   「帧 → 事件」生产者喂 `StreamEncoder`（`StreamEncoder` 对 args delta 逐段
   转发、`closeBlock` 时才要求完整 JSON——`stream.go:299-311`、`:351-360`，
   与未闭合片段兼容），要么另写帧级状态机。**这是 dsnative 路径最大的一块新代码**。
3. **Chat wire 响应 → Canonical 解码**：不存在（`openaichat` 只有响应**编码**
   `encode_response.go`/`encode_stream.go`，没有任何 Chat 响应解码；gateway 的
   `extractChatUsage` 只抽 usage，`relay.go:266-307`）。仅 dscompat 形态 B 需要。
4. **Canonical → Chat wire 请求编码**：不存在（`openaichat` 目录清单：
   capability/decode/encode_response/encode_stream/projection/wire——无请求编码器；
   Chat→Compatible 计划的绑定决策 4 明说「不写 Canonical 出站编码器」，
   `plans/2026-08-15-…-compatible.md:38`）。仅 dscompat 形态 B 需要。
5. **Responses 侧投影与 422 规则**：不存在（§3 的两处坑——metadata、
   web_search 开关——都落在这里；Chat 对应物是 `projection.go`（116 行）+
   `reject.go`（67 行））。

### 4.4 relay 与终止判定：已按入站协议就位

【事实】Responses 入站的 relay 配套已经存在且与出站 Provider 无关：
usage 抽取按 Responses 口径（`extractUsage`/`parseUsageEvent`，
`relay.go:200-247`）、流终止判定认 `response.completed/incomplete/failed`
（`handler.go:151-158`）、错误信封走 `openaiwire.EncodeError`（`handler.go:160`）。
只要适配器交回 Responses wire，这一层零改动。

---

## 5. convstore（`stateful_conversation` EMULATE 格）对异构出站的适用性

矩阵现状：两条路径的 `stateful_conversation` 都是 EMULATE、开关 `convstore`
默认关（`rules_phase1.go:181-183`；`preference.go:86`、`:92-94`；
`build.go:47-49` 把 `cfg.ConvStore.Enabled` 写进 Availability）。文档汇总列
「1（1 未开启）」（`degradation-matrix.md:54-55`）。开关关闭时 `Check` 返回
`ClassUnsupported`（422）且错误必须说「开关没开」（`matrix.go:736-744`）。

### 5.1 请求侧：原样可用

【事实】convstore 的接线完全长在**入站侧**，与出站 Provider 无耦合：

- 触发条件是 Responses 解码器独有的元信息（`PreviousResponseID`/`WantsStore`，
  `handler.go:550-552`；Chat 入站永不设置它们，`handler.go:173`）。
- `prepareConversation` 做三件事：读历史（`TurnsOwned`，`:575-607`）、重写 raw
  （`ExpandConversationRequest`：历史条目 prepend 进 `input`、删
  `previous_response_id`、强制 `store:false`，`conversation.go:15-40`）、
  **把历史并进 Canonical**（`decoded.Request.Messages = append(history, current…)`，
  `handler.go:634`）。
- 异构出站消费 Canonical → 历史自动随 `EncodeRequest` 进 Native 信封
  （`encode_messages.go:12-29`）。convstore 的 `Turn.Opaque` 注释本来就写明分工：
  「它只允许原协议读取；**异构出站仍以 Messages 为唯一语义来源**」
  （`convstore/store.go:57-59`）。

### 5.2 响应侧：可用，但有一个硬前提

【事实】relay 的会话重写 transform 按 **Responses wire** 解析响应体：
`RewriteStoredResponse` 要求响应是 JSON 对象、`storedOutput` 要求
`status ∈ {completed, incomplete}` 并解析 `output[]`（`conversation.go:250-274`、
`:352-364`）；流式版 `RewriteStoredStreamEvent` 认 `response_id`/`response` 字段与
`response.completed/incomplete` 终止事件（`:278-340`）。它们挂在 relay 上对
Provider 返回的字节生效（`handler.go:506-546`）。

**硬前提**：出站适配器必须返回 Responses wire（§2.1 的架构不变量）。前提成立时，
convstore 全链路（含 `storeConversation` 把本轮 input 条目 + 输出项写回，
`handler.go:642-666`）对异构出站**零改动**。

### 5.3 前提不成立时的两个具体故障（代码路径推演，【推断】）

若适配器把 Chat wire 响应直接交回（即照抄 Chat 路径的输出端）：

1. **非流式 usage 记成「权威零值」**：`extractUsage` 对 Chat body 的 `usage`
   对象解析成功但 `input_tokens`/`output_tokens` 键不存在 → 全零且
   `FidelityAuthoritative`（`relay.go:200-209`、`:231-247`）；而 usage 回调补位
   只在 `Fidelity == FidelityUnavailable` 时生效（`handler.go:450-463`）——
   零值权威记录不会被 Native 每帧累计用量纠正，直接进
   `ObserveUsage`。这正是原则 2.5 防的「把没数据当成真的是 0」。
2. **流式永远等不到终止事件**：Responses 的 `streamTerminal` 认
   `response.completed/incomplete/failed`（`handler.go:151-158`），Chat SSE 的
   `[DONE]` 哨兵不匹配 → EOF 时 `relayStream` 判「上游流在协议终止事件之前结束」
   （`relay.go:136-147`）→ 首字节后只能发 error 事件收尾、记 stream_aborted
   （`handler.go:466-488`）。一条健康的流被记成中断。

### 5.4 雷区：思考历史会让第二轮异构请求撞 400（代码路径推演，【推断】）

【事实】链条各环都在代码里：

1. Responses 输出的 reasoning 项（含 summary 文本）被 `storedOutput` 存成
   `PartThinking` 消息（`conversation.go:420-433`）；
2. 下一轮 `prepareConversation` 把它并入 `decoded.Request.Messages`
   （`handler.go:590-606`、`:634`）；
3. Native 请求编码器对 assistant 消息的非 tool_call 块走 `encodeTextContent`，
   它只认 `PartText`，其余一律 `ClassBadRequest`「字符串 content 无法编码
   thinking 内容块」（`encode_messages.go:63-82`、`:129-139`）。

即：**convstore 开启 + 上游是思考模型 + 客户端续轮**的组合下，
responses→dashscope.native 的第二次请求会在出门前 400。系统对 wire 回放路径
已经认识到同类问题——`EncodeConversationHistory` 对 PartThinking 显式报
「会话历史中的推理条目尚无法无损回放」（`conversation.go:179-181`）——但
Canonical 路径（异构专用）没有等价处置。Chat 路径踩不到这颗雷：Chat 入站
没有 convstore，且 Chat wire 的输入侧产生不了 PartThinking（`decode.go:165-230`
无此映射）。dscompat 形态 B 同样会踩（任何消费 Canonical Messages 的编码器
都要面对 thinking 历史）；形态 A 不消费 Canonical，绕开。
处置选项（丢弃/降级为文本/拒绝续轮）是设计决定，超出本工单，但**实施计划
必须显式回答它**，否则 EMULATE 格的「客户端拿到的能力是完整的」这句 note
在思考模型上不成立。

### 5.5 小不对称：`store` 的 422 规则不能照抄

【事实】Chat 的 422 规则把「显式提交 `store`」列为无落点字段拒绝
（`reject.go:25-27`）；而 convstore 的请求重写**主动往 raw 里写 `store:false`**
（`conversation.go:33-34`）。Responses 侧投影若照抄 presence 判定，每个经过
会话展开的请求都会被自己的网关拒掉。Responses 的 `store`/`previous_response_id`
是 convstore 的输入而非无落点字段，422 规则须按「convstore 是否已消费」重新设计。

### 5.6 EMULATE 格的 fixture 形态（既有先例）

【事实】已转正的 responses→openai.compat 给出了模板：
`testdata/routes/openai.responses__openai.compat/stateful_conversation.json`
——note 明说默认关闭时带 `previous_response_id` 的请求「会被矩阵裁决拦下，
根本不会打到上游：下面的 response 只是为了让这条 fixture 结构完整，实际不会
被消费」。配套断言在 `conformance_test.go:224-244`：422 + 上游零调用 +
错误体点名 `convstore` 开关；开启态另有 `TestEmulationWorksWhenEnabled`
两轮合成回放（`:247` 起）。两条新路径的 EMULATE 举证可沿用同一形态
（拦截态 fixture + 开启态测试），但按 ADR-0001 口径，开启态在异构出站上的
真实证据（尤其 §5.4 的思考历史场景）没有现成覆盖。

---

## 6. 两条路径的 DEGRADE/EMULATE 格清单与 fixture 举证要求

### 6.1 格子清单（设计处置，来自 `docs/degradation-matrix.md:457-523`，与 `rules_phase1.go` 派生逻辑一致）

**`openai.responses → dashscope.compatible`**（门：`/v1/responses`）

| 能力 | 处置 | note 要点 | note 的线格式立场 |
|---|---|---|---|
| `structured_output` | DEGRADE | 兼容模式支持 json_object，不保证 strict json_schema 校验 | wire 中立 ✓ |
| `web_search` | DEGRADE | 只有 enable_search 布尔开关；`search_context_size`/`user_location` 丢失、响应不返回搜索来源 | **Chat wire 措辞**——Responses 的搜索是内建工具，没有这两个子字段；形态定下来后须核对/Override（§2.1、§8 Q1） |
| `stateful_conversation` | EMULATE | convstore 内存态边界，默认关 | wire 中立 ✓ |
| （对照）REJECT 4 格 | `file_input` / `audio_output` / `image_generation` / `computer_use` | 422，不兑现 | `audio_output` note「兼容模式不返回音频」wire 中立；其余中立 |

**`openai.responses → dashscope.native`**（门：`/v1/responses`；上游门由
`native_endpoint` 配置决定，文本/多模态两扇）

| 能力 | 处置 | note 要点 | 备注 |
|---|---|---|---|
| `parallel_tool_calls` | DEGRADE | Native 有开关但无路径级全局保证；客户端未提交时网关按 OpenAI 默认注入 true | 注入逻辑已在编码器（`encode_request.go:194-200`）；但能力报告缺失（§3.2）→ 降级头不会出现该条目，须先决定报告口径 |
| `structured_output` | DEGRADE | 支持 response_format=json_object，无 strict schema 校验 | 编码器已消费 Canonical ResponseFormat（`encode_request.go:258-283`） |
| `image_detail` | DEGRADE | Native 图片块没有 detail 档位，精度与计费意图丢失 | 与代码一致：`encodeMedia` 只编 URL/data URI，`Media.Detail` 无落点（`encode_messages.go:163-186`） |
| `web_search` | DEGRADE | enable_search 布尔开关承载不了 web_search 工具参数 | 开关来自 `extra.WebSearch`——Responses 侧投影必须供上（§3.2 坑 2） |
| `stateful_conversation` | EMULATE | convstore 内存态边界，默认关 | §5 全部适用 |
| （对照）REJECT 4 格 | `file_input` / `audio_output` / `image_generation` / `computer_use` | 422，不兑现 | `audio_output` note 措辞是「Chat Completions 入站无法表达」——**继承自 Chat 路径的错别**，Responses 入站同样表达不出（wire 无 modalities），但 note 点名了错误的协议，兑现前应 Override（`degradation-matrix.md:507`；`matrix.go:410-423` 提供 Override 机制） |

### 6.2 fixture 门槛的机械规则（全部【事实】）

- 目录名由 `FixtureDir` 决定：`testdata/routes/openai.responses__dashscope.compatible/`
  与 `testdata/routes/openai.responses__dashscope.native/`（`markdown.go:258-263`）。
- **每个 DEGRADE/EMULATE 格必须有同名 fixture**（文件名即能力名，
  `matrix_test.go:502-515`）；**每份 fixture 的 `request.path` 必须是一扇已开门，
  每扇已开门至少有一份 fixture 指向它**（双向对账，`matrix_test.go:517-569`）。
  两条路径的门都是 `/v1/responses`（`EndpointOpenAIResponses`，`endpoint.go:24`；
  门归属校验 `endpoint.go:47-57` 会拦「把 /v1/responses 兑现在 openai.chat 路径上」
  的错绑）。
- fixture 结构：`name`/`note`/`request`/`response`（上游录制）/`upstream`
  （网关发给上游的期望请求，method/path/body 三者必填——异构路径防「只比客户端
  响应」的伪绿，`testkit/fixture.go:23-58`；Chat→Compatible 计划绑定决策 5，
  `plans/2026-08-15-…:39`）。
- 兑现动作三处同改：`Redeem(EndpointOpenAIResponses, …)`（写在
  `rules_phase1.go:206-211` 的派生处）+ `TestImplementedRoutesAreExplicit`
  （`matrix_test.go:118`）+ `TestRedeemedCapabilitiesAreExplicit`
  （`matrix_test.go:160`）；随后 `make matrix-update` 重新生成文档。
- 真实录制是硬门槛：「不得用 httptest 构造假录制充数」（Chat→Native 计划
  绑定决策 14，`plans/2026-08-25-…:51`）。
- 代码闸只到「门有人敲过 + 有损格有同名用例」；**逐项 PASS 能力真的跑通，
  靠改白名单的人负责**（`internal/degrade/AGENTS.md:69-75`）。

### 6.3 每格需要的 fixture 场景（按既有 Chat 用例的形状类推，【推断】标注）

`openai.responses__dashscope.native/`（对照既有 14 份 Chat 用例
`testdata/routes/openai.chat__dashscope.native/` 与 conformance 的降级头钉法
`chat_dsnative_conformance_test.go:51-59`）：

| 文件 | 场景 | 断言要点 |
|---|---|---|
| `structured_output.json` | Responses 请求带 `text.format = json_schema`（含 name/schema/strict） | upstream：Native `parameters.response_format` 携带 json_schema 与 strict 原值（`encode_request.go:261-283`）；下游降级头含 `structured_output=` |
| `web_search.json` | Responses 请求带 `tools:[{"type":"web_search"}]`（**Responses 形态，不是 web_search_options**） | upstream：`parameters.enable_search:true`；降级头含 `web_search=`；依赖 Responses 侧投影把内建工具翻成 WebSearch 位（新建，§3.2） |
| `image_detail.json` | `input_image` 带 `detail:"high"`（多模态门） | upstream：Native image 块**没有** detail 键（`encode_messages.go:168-186`）；降级头含 `image_detail=` |
| `parallel_tool_calls.json` | 显式 `parallel_tool_calls:true` + 两个 function 工具 | upstream：`parameters.parallel_tool_calls:true`；**降级头是否出现该条目取决于 §3.2 的报告口径决定** |
| `stateful_conversation.json` | 带 `previous_response_id`、convstore 默认关 | 422 + 上游零调用 + 错误体点名 `convstore`（模板：`openai.responses__openai.compat/stateful_conversation.json` 与 `conformance_test.go:224-244`） |
| PASS 格举证（basic/streaming/tool_calling/reasoning/vision_input 等） | 按兑现集合逐能力一份（Chat 路径的先例是 9-10 项各一份 + combined） | 响应侧 golden 首次覆盖 `EncodeResponse`/`StreamEncoder` 的端到端行为（§4.1）；streaming 用例须证明 `response.completed` 携带完整 output 数组与 authoritative usage |
| （若兑现 audio_input） | `input_audio` 内联负载 | Chat 路径的先例是**设计 PASS 但无真实证据不兑现**（`rules_phase1.go:154-156`：qwen-audio-turbo 免费额度耗尽，门保持 Gated）——Responses 侧同样要防「无证据兑现」 |

`openai.responses__dashscope.compatible/`：DEGRADE 格同名 fixture 两份
（`structured_output.json`、`web_search.json`）+ `stateful_conversation.json`，
场景骨架同上，但 upstream 断言的目标路径与请求体**取决于 §2.1 的形态分叉**
（形态 A：upstream path `/v1/responses`、体为修补后的 Responses wire；
形态 B：upstream path `/v1/chat/completions`、体为重编码的 Chat wire）——
形态未定前无法写出这两份 fixture 的 upstream 段，这是该路径排期上的第一个
待决点。

---

## 7. 复用比例估算与新建清单

### 7.1 口径说明

【推断·全部为估算】复用比例没有唯一正确答案，给两个口径并说明算法：

- **口径一（组件块）**：把一条路径拆成功能块，数「原样复用 / 改造复用 / 新建」。
- **口径二（非测试 LOC）**：以既有对应物的行数为分母估计。共享基础设施
  （gateway 主链路、入站解码器）两条 Chat 路径已经付过成本，计入复用会抬高
  数字，因此单列「路径专属增量」一栏——只算 Chat 路径当年**为这条路新写**的
  那些组件（合计约 2200 行非测试代码：projection 116 + reject 67 + provider 69 +
  nonstream 222 + stream 386 + candidates 164 + encode_request 345 +
  encode_messages 216 + result 221 + encode_response 239 + encode_stream 156，
  `wc -l` 实测），对照 Responses 路径需要新写多少。

### 7.2 `openai.responses → dashscope.native`

| 功能块 | 处置 | 依据 |
|---|---|---|
| 入站解码 + 能力报告（808 行） | 原样复用（能力报告口径待 §3.2 决定，改动是加法） | §3 |
| 矩阵格 + 501/422 裁决 | 原样复用；追加 Redeem/白名单/文档三处同步 | §1、§6.2 |
| gateway 主链路 serve/dispatch/relay + usage 回调 + Native 候选过滤（handler.go 762 + relay.go 359 + filter.go 33） | 原样复用 | §2.2、§4.4 |
| convstore 会话模拟 | 复用，条件与雷区见 §5 | §5 |
| Native 请求编码（encode_request 345 + encode_messages 216） | 原样复用（Canonical 驱动） | §2.2 |
| Native 响应解码（result.go 221）+ SSE 读取机器（约 150 行） | 原样复用 | §4.2 |
| Responses 出站编码（response.go 293 + stream.go 538） | 复用（首次生产接线，需 fixture 闭环） | §4.1 |
| Responses 侧投影 + 422 规则 | **新建**（Chat 对应物 183 行可当模板；须处置 metadata/store/内建工具三处差异） | §3、§5.5 |
| `ChatSampling` extra 的 Responses 填法（max_output_tokens → MaxTokens 桥） | **新建**（小，约 30–50 行） | §3.1 |
| Provider 分派分支 + 非流式翻译（Result→Message→EncodeResponse） | **新建**（nonstream.go 的 Chat 版 222 行是结构模板；Parts 映射段可剥离复用） | §2.2、§4.2 |
| `finish_reason → StopReason` 映射 | **新建**（约 30 行 + 录制证据） | §4.3 |
| 流式翻译（Native 帧 → canonical.Event → StreamEncoder，或帧级状态机） | **新建，最大单体**（估 250–400 行；transformReader 骨架与首帧预读模式可参照） | §4.3 |
| fixture（≥5 份有损格 + PASS 举证）/golden/conformance/negative | 数据与测试新建，模式全部现成 | §6 |

**估算**：口径一 9/13 块复用 ≈ **70%**；口径二——路径专属增量约需新写
600–850 行，对照 Chat 路径当年的约 2200 行 ≈ **60–70% 的专属工作量已被既有
代码覆盖**；若把共享基础设施计入，总 LOC 复用率 ≈ **80–85%**。
新建代码全部集中在**响应侧桥接与 Responses 投影**——恰好是 Chat 路径刻意
绕开 canonical 事件层（绑定决策 9）而没有留下的部分。

### 7.3 `openai.responses → dashscope.compatible`

复用比例被 §2.1 的形态分叉劈成两半，**在 Q1 的上游契约调研完成前无法给出
单一数字**——这是本工单最重要的排期结论：

- **形态 A（Responses wire 直通 `/compatible-mode/v1/responses`）**：
  适配器（170 行）、relay、convstore、usage 抽取、错误信封全部原样复用；
  新增工作 ≈ 矩阵 note 逐格核对与 Override、内建工具在直通下的处置决定、
  fixture/golden。估算复用 **85–90%**。**前提**：上游契约调研（Q1）证明
  PASS 格逐格为真，否则矩阵声明与事实脱节，违反 ADR-0001 的「表的态是真的」。
- **形态 B（重编码 Chat wire）**：需要 §4.3 的第 3、4 项（Chat 请求编码器 +
  Chat 响应解码，估 550–750 行全新代码，Chat 路径没有留下任何可复用的
  对应物——它的适配器哲学就是「不经 Canonical」），加上 Responses 出站编码
  接线。估算复用 **25–35%**（复用的几乎全是共享基础设施与 Responses 编码器，
  「openai.chat 转换机器」本身——patcher——在此形态下基本无用）。

### 7.4 新建清单（按依赖序，两路径合并）

1. **上游契约调研**：DashScope 兼容层 `POST /compatible-mode/v1/responses`
   的字段级契约（阻塞 dscompat 形态决定；仓库现状只有存在性证据，
   `dashscope-compatible-get-v1-models.md:146-150`）。
2. **两个行为口径决定**（阻塞投影与 fixture 断言）：
   a) Responses 解码器是否补报 `CapParallelToolCalls`（§3.2）；
   b) 思考历史（PartThinking）在异构 Canonical 路径上的处置（§5.4）。
3. **Responses 侧严格投影**（openairesponses 包内，对应 `projection.go`）：
   parallel_tool_calls、内建 web_search 工具、max_output_tokens、
   metadata/store presence。
4. **Responses 侧 422 规则**（provider 层，对应 `reject.go`）：无落点字段点名；
   `store`/`previous_response_id` 按 convstore 消费状态豁免（§5.5）。
5. **`ChatSampling` 的 Responses 桥**（provider 层填 extra）。
6. **`finish_reason → StopReason` 映射**（协议层，需录制证据）。
7. **dsnative：Provider 分派分支 + 非流式翻译**（依赖 3–6；复用
   `EncodeRequest`/`DecodeResult`/`EncodeResponse`）。
8. **dsnative：流式翻译**（依赖 6、7 的错误口径；Native 帧 → canonical.Event
   生产者 + `StreamEncoder` 接线，或帧级状态机）。
9. **dscompat：形态 A 的 note 核对与 Override，或形态 B 的 Chat 请求编码器 +
   Chat 响应解码器**（依赖 1）。
10. **fixture（真实录制）→ Redeem → 双白名单 → conformance/negative/golden →
    矩阵文档再生成**（依赖 7/8/9；流程模板 = Chat 两条路径的波次 4）。

---

## 8. 无法核实项与开放问题（不以估算冒充实证）

- **Q1（阻塞 dscompat）**：DashScope 兼容层 `/v1/responses` 的字段级契约——
  仓库内只有存在性与旧路径退役警告的记录
  （`dashscope-compatible-get-v1-models.md:146-150`，转引阿里云官方文档
  《OpenAI Responses接口兼容》），没有参数支持面、`store`/`previous_response_id`
  行为、内建工具处理、错误信封形态的任何调研。本工单未做外部网络调研，
  该缺口如实上报。
- **Q2**：形态 A 下 Responses 内建 `web_search` 工具原样透传时上游是否 honored、
  以及 DEGRADE note 该如何措辞——依赖 Q1。
- **Q3**：`reasoning.summary` 档位（auto/concise/detailed）在两条路径上是否有
  损失、要不要格子——请求方向确认无落点（§3.1），响应方向 summary 文本可由
  reasoning_content 满足（§4.1），但「档位」语义无法核实上游行为。
- **Q4**：可表达性声明把 `audio_output` 列为 Responses 可表达
  （`expressibility_phase1.go:82`），而解码器产生不了它（§3.1）——声明与现实的
  这处不一致是既有状态，本工单只记录，不判断该改哪边。
- **Q5**：§5.3、§5.4 的故障链是代码路径推演（【推断】），未运行复现——
  复现它们需要 convstore 开启 + 思考模型 + 续轮的端到端环境，超出只读审查范围。
- **Q6**：复用比例的 LOC 口径把「模式可参照但需重写」的代码计为新建
  （如 nonstream/stream 的 Responses 版）；若按「设计决策可继承」口径，
  比例会更高。数字用于排期量级判断，不用于验收。

## 附：证据清单（主要）

代码：`internal/degrade/{rules_phase1,matrix,preference,endpoint,expressibility_phase1}.go`、
`internal/degrade/matrix_test.go`、`internal/provider/provider.go`、
`internal/provider/dashscopecompat/dashscopecompat.go`、
`internal/provider/dashscopenative/{provider,nonstream,stream,candidates,reject}.go`、
`internal/provider/passthrough/passthrough.go`、
`internal/protocol/dashscopenative/{encode_request,encode_messages,result}.go`、
`internal/protocol/openairesponses/{wire,decode,response,stream,conversation}.go`、
`internal/protocol/openaichat/{wire,decode,capability,projection,encode_response,encode_stream}.go`、
`internal/canonical/{request,capability,stream}.go`、
`internal/gateway/{build,handler,relay,filter}.go`、
`internal/gateway/{conformance_test,chat_dsnative_conformance_test}.go`、
`internal/convstore/store.go`、`internal/testkit/fixture.go`。

文档：`CONTEXT.md`、`docs/architecture/principles.md`、
`docs/adr/0001-declarations-must-be-redeemed-by-fixtures.md`、
`docs/degradation-matrix.md`、`internal/{degrade,protocol,gateway,canonical}/AGENTS.md`、
`docs/superpowers/plans/2026-08-15-openai-chat-dashscope-compatible.md`、
`docs/superpowers/plans/2026-08-25-openai-chat-dashscope-native.md`、
`docs/superpowers/specs/2026-08-22-openai-chat-dashscope-native-design.md`、
`docs/research/dashscope-compatible-get-v1-models.md`、
`docs/research/2026-08-22-dashscope-native-text-generation-contract.md`。

命令输出：`GIT_MASTER=1 git rev-parse HEAD`（= 5eaf36e…）；
`GIT_MASTER=1 git log -S responsesExtras`（= 5dc0596 引入派生）；
`GIT_MASTER=1 git show 6275d36`（Responses 编码器出身）；
`grep -rn "EncodeResponse\|NewStreamEncoder"`（零生产调用方）；
`grep EventTextDelta|EventContentStart|EventToolCallStart`（事件层无生产者）；
`wc -l`（LOC 口径分母）。
