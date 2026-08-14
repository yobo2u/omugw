# DashScope Native 多模态生成端点投放设计

**日期**：2026-08-14
**状态**：已批准，待实现
**调研依据**：`docs/architecture/dashscope-endpoints-research.md`
**前序设计**：`docs/superpowers/specs/2026-08-14-dashscope-native-promotion-hardening-design.md`
（处置与兑现分离的起点）、ADR-0001（声明必须由 fixture 兑现）、ADR-0002（保留度分两列）

## 背景

`dashscope.native -> dashscope.native` 是同源快通道：一个入站协议族对应上游多个
端点，文本生成、多模态生成、embedding、rerank 各是一扇门。转正收口设计已经把
**设计处置**与**当前兑现**分了家，但兑现集合仍挂在路径上。文本门投放时这够用；
现在要投放多模态生成门，路径级集合装不下了：把新门的能力加进同一个集合，文本门
的 `tool_calling`、`reasoning`、`web_search` 就会混进多模态门的可用分数，仿佛
那扇门也提供这些能力。

**必须纠正的一个错误结论**：「多模态门投放后可用分数是 8/18」。那是两扇门兑现
集合的并集：{text_generation, streaming, tool_calling, reasoning, web_search} ∪
{text_generation, streaming, vision_input, audio_input, video_input} = 8 项。
没有任何一扇真实的门同时提供这 8 项：不存在既投放 `tool_calling` 又投放
`vision_input` 的门。给多模态门记 8/18（0.444），等于让选路拿一个不存在的分数，
把请求送进一扇会返回 501 的门。**正确口径是端点相对分数：多模态门 5/18 =
0.278**，除非 `tool_calling` / `reasoning` / `web_search` 将来在这扇门上有
fixture 显式证明。

官方契约（见调研）：多模态生成端点的 `input.messages[].content` 是
`text` / `image` / `audio` / `video` 四种单键块，**没有通用 file 块**。因此
`file_input` 不兑现。同一 URL 还被多模态交互套件（`multimodal-dialog`）、语音
识别、图像生成等无关产品 API 共用；本门的契约只认 Qwen 模型 API 形状。

用户已确认的投放面：同步（JSON）与 SSE 两种传输；兑现 `vision_input`、
`audio_input`、`video_input` 三项多模态输入能力，加上路径原有的
`text_generation` 与 `streaming`。

## 批准范围

- 在 `dashscope.native -> dashscope.native` 路径上投放第二扇端点门：
  `/api/v1/services/aigc/multimodal-generation/generation`。
- 该门兑现集合恰为 5 项：`text_generation`、`streaming`、`vision_input`、
  `audio_input`、`video_input`。同步与 SSE 都覆盖。
- 把兑现从路径级下沉到端点级：`internal/degrade` 引入 `Endpoint` 与 `Inbound`
  两个类型，`Redeem` / `Check` / `BestOutbound` / `Preservation` 改为端点相对。
- 文本门现状不变：仍是既有 5 项（text_generation、streaming、tool_calling、
  reasoning、web_search）。`reasoning` 的 fixture 证据已经存在
  （`tools-and-search.json` 带 `enable_thinking: true`，其 note 明言证明
  reasoning），继续持有。
- 解码器零改动：`dashscopenative.Decode` 已识别四种内容块（纯键式与带 type 两种
  形态）、按 data URI 统计内联字节、对 video 帧数组逐帧累加，且均有单测固化。

## 非目标

| 排除项 | 理由与去向 |
|---|---|
| embedding / rerank 端点门 | 各自独立的投放任务，契约材料见调研文档 |
| 图像 / 视频合成端点门 | 异步 Job 轮询（`X-DashScope-Async`、`GET /api/v1/tasks/{task_id}`）打破同步 + SSE 的转发模型，另行设计 |
| 语音端点 | Qwen 系实时语音归 `dashscope.realtime` / `dashscope.inference` 两条既有路径 |
| 多模态**输出**能力（audio_output、image_generation、video_generation）兑现 | 本期只兑现输入侧 |
| `file_input` 兑现 | 官方内容块无 file；设计处置保持 PASSTHROUGH，两扇门都不兑现，运行时 501 |
| 多模态门兑现 tool_calling / reasoning / web_search | 无 fixture 证据，不兑现 |
| 路径级「当前可用」聚合分数 | 并集不对应任何真实的门，见「分数」 |

## 术语：端点（Endpoint），协议族的门

**端点**是一个入站协议族门下的一扇门：客户端能直接敲响的某一个上游入口。
同源直通下，入站门与出站端点是同一个路径。

- **处置**挂在路径上，回答「这条路最终该怎么对待每项能力」。同源直通路径对 18 项
  可表达能力全部 PASSTHROUGH，这是设计事实，不因投放进度改变。
- **兑现**挂在端点上，回答「这扇门此刻真的投放了哪些能力」。
- 门与门的兑现集合互不相通，不做并集。

CONTEXT.md 已登记该术语。

## 架构决策：处置与兑现的两个粒度

矩阵从此有两个粒度：

| 层 | 数据 | 语义 | 消费者 |
|---|---|---|---|
| 路径 × 能力 | `rules`（Pass/Degrade/Reject/Emulate/N/A） | 设计处置：这条路最终的样子 | 文档设计列、REJECT/N/A 裁决 |
| 端点 × 能力 | `redeemed`，键为 **(endpoint, capability)** | 当前投放：这扇门此刻真的有什么 | 运行时放行裁决、文档可用列 |

三条结构性约束：

1. **端点的存在只从兑现格子推导**。有一格兑现，才算开了这扇门。没有独立的门
   注册机制，没有可变的 Endpoint 结构体，也没有「空门」可以声明。未投放的门
   本来就是 PLANNED，不需要占位；文档与 Mux 核对要门清单时，`Endpoints()`
   现场从兑现格子推导。
2. **兑现必须三证齐全**：路径、门、能力。缺一即 501。路径级集合无法回答
   「这个请求打的是哪扇门」，这是下沉的直接动因。
3. **不存在路径级的可用分数聚合**。并集分数（8/18）不对应任何一扇真实的门。
   给不存在的东西记分，正是降级矩阵要防的过度承诺。路径级保留度只有设计列；
   可用列一律端点相对。

## API 草案（internal/degrade）

### Endpoint 与 Inbound

```go
// Endpoint 是入站协议族下的一扇门：客户端能直接敲响的上游端点路径。
// 同源直通下，入站门与出站端点是同一个路径。
// 零值没有任何含义：不是通配，不是默认门。它在任何处出现都按「没有这扇门」
// 对待——Redeem 它会在 Build 失败，Check 它会在端点闸门得 501，
// Preservation 它的可用列恒为零。
type Endpoint string

// Inbound 钉住一次请求的入站坐标：从哪个协议进来，敲的哪扇门。
// 矩阵按这对坐标裁决。
type Inbound struct {
	Protocol Protocol
	Endpoint Endpoint
}
```

### Route 的变化

```go
type Route struct {
	// In / Out / Homogeneous / rules 不变。
	// 原路径级 redeemed 集合删除，兑现下沉为 (端点, 能力) 二元键。
	redeemed map[Endpoint]map[canonical.Capability]bool
}

// Redeem 登记指定端点已投放的能力。
// 纪律不变：只应在该端点的端到端 fixture 存在并通过之后调用。
// ep 为零值，或兑现到 REJECT / N/A 格子，都会在 Build 失败。
func (r *Route) Redeem(ep Endpoint, caps ...canonical.Capability) *Route

// Redeems 报告某项能力是否已投放到指定端点。
func (r *Route) Redeems(ep Endpoint, c canonical.Capability) bool

// RedeemedAt 返回指定端点已投放的能力集合，按 AllCapabilities 顺序，
// 供白名单对账与文档生成。
func (r *Route) RedeemedAt(ep Endpoint) []canonical.Capability

// Endpoints 返回全部已开门，字典序。
// 一门当且仅当有至少一格兑现才算开；存在是推导出来的，不是声明出来的。
func (r *Route) Endpoints() []Endpoint

// ImplementedAt 报告指定端点是否至少投放了一项能力。
func (r *Route) ImplementedAt(ep Endpoint) bool

// Implemented 语义不变：至少一扇门已开。路径白名单与选路候选继续用它。
func (r *Route) Implemented() bool

// Preservation 报告保留度。
// 设计列（透传/模拟/降级/拒绝/N/A 计数与 DesignScore）是路径级的，与 ep 无关；
// 可用列（availableWeight / NotRedeemed / AvailableScore）是端点相对的：
// 只计投放到 ep 的格子。ep 未开（含零值）时可用列为零——
// 没有门就是没有可用，只会少报，不会多报。
func (r *Route) Preservation(avail Availability, ep Endpoint) Preservation
```

`Route.Redeem` 旧签名（不带 ep）删除；`Route.Redeems` 改为二元。仓库未发布，
允许破坏性变更（见「迁移兼容」）。

**Build 新增校验**：

1. 对零值端点 Redeem → 报错 `redeem on zero endpoint`。
   防的是什么：零值一旦被当成「整条路径」的通配，端点粒度就悄悄退回了路径粒度。
2. 某端点兑现了 REJECT 或 N/A 格子 → 报错
   `endpoint %q redeemed but not deliverable: %s`。
   这是既有路径级 undeliverable 检查在端点粒度上的延续：兑现一格按设计就该失败
   或客户端根本发不出来的能力，是在为不存在的东西背书。

`Derive` 不继承兑现：二元键集合整体不带入派生路径，与既有「兑现集合不在继承之
列」同理。实现是一扇门一扇门写的，继承会让一条还没动工的派生路径宣称自己的门可用。

### Matrix 的变化

```go
// Check 用入站坐标（协议 + 端点）与请求实际用到的能力集裁决。
//
// 闸门顺序：路径注册 → 路径通车 → 端点通车 → 逐项能力处置
//（REJECT / N/A 先于投放）→ 端点投放 → EMULATE 开关。
func (m *Matrix) Check(in Inbound, out Provider, caps []canonical.Capability) (Verdict, error)
```

| 序 | 闸门 | 失败分类 | HTTP | 消息要求 |
|---:|---|---|---:|---|
| 1 | 路径 (in.Protocol, out) 未注册 | ClassUnsupported | 422 | 现状不变 |
| 2 | 路径没有任何已开门（PLANNED） | ClassNotImplemented | 501 | 路径级消息，与现状一致 |
| 3 | in.Endpoint 未开 | ClassNotImplemented | 501 | **消息点名端点** |
| 4a | 能力未声明 | ClassUnsupported | 422 | fail-closed，现状不变 |
| 4b | 处置 REJECT | ClassUnsupported | 422 | 带 note，现状不变 |
| 4c | 处置 N/A | ClassInternal | 500 | 解码器 bug，现状不变 |
| 4d | 该端点未兑现此能力 | ClassNotImplemented | 501 | 同时点名能力与端点 |
| 4e | EMULATE 开关未开 | ClassUnsupported | 422 | 说「开关没开」，现状不变 |

顺序约束：同一项能力上，REJECT / N/A（4b/4c）必须先于端点投放（4d）。先问投放
会把「这条路不支持」说成「还没建好」，方向就错了。501 说「等」，422 说「改」，
前者会变，后者不会。

**空能力集不豁免端点闸门**：`caps` 为空时，闸门 1 至 3 照常执行。请求带了什么
能力是一回事，敲的门开没开是另一回事；后者是入口约束。未开门上即使空能力集也
返回 501，消息点名端点。

```go
// BestOutbound 在候选中挑出最优 Provider。
// RankOutbound 保持端点无感：候选筛选只看路径是否通车（至少一门已开），
// 不看本次请求敲哪扇门。端点裁决发生在每个候选内部的 Check：
// 首选若恰因这扇门没开而失败，错误会点名缺哪扇门，
// 而后面开了这扇门的候选仍能接住请求。
func (m *Matrix) BestOutbound(in Inbound, candidates []Provider, caps []canonical.Capability) (Provider, Verdict, error)
```

`BestOutbound` 空结果时的两类错误（未注册 → ClassUnsupported；注册了但没实现
→ ClassNotImplemented）维持现状，`Implemented()` 已由门推导。

## 端点常量与登记

```go
// internal/degrade/endpoint.go（新文件）

const (
	// OpenAI 两扇门从 build.go 的字面量迁到命名常量，
	// 登记（rules）与注册（Mux）引用同一份事实。
	EndpointOpenAIChat      Endpoint = "/v1/chat/completions"
	EndpointOpenAIResponses Endpoint = "/v1/responses"
)

const (
	// DashScope 两扇门复用协议包的路径常量，线格式事实的单一来源留在协议包。
	EndpointDashScopeTextGeneration Endpoint = Endpoint(dashscopenative.TextGenerationPath)
	EndpointDashScopeMultimodal     Endpoint = Endpoint(dashscopenative.MultimodalGenerationPath)
)
```

`internal/protocol/dashscopenative/wire.go` 新增：

```go
// MultimodalGenerationPath 是多模态生成的上游端点路径。
const MultimodalGenerationPath = "/api/v1/services/aigc/multimodal-generation/generation"
```

依赖方向：`internal/degrade` 引入对 `internal/protocol/dashscopenative` 的依赖，
仅为两个路径常量。方向合法（dashscopenative 只依赖 canonical，无环），且是刻意
选择：端点路径是线格式事实，矩阵引用而不复写。

## 兑现集合

`internal/degrade/rules_phase1.go`：

```go
// 两条 OpenAI 同源直通路径：各一扇门，兑现全部可表达能力（现状迁移）。
Redeem(EndpointOpenAIChat, ExpressibleSet(ProtoOpenAIChat)...)
Redeem(EndpointOpenAIResponses, ExpressibleSet(ProtoOpenAIResponses)...)

// Native 路径：两扇门。
NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
	MarkHomogeneous().
	Pass(ExpressibleSet(ProtoDashScopeNative)...).
	// 文本门：既有 5 项，原样保留。reasoning 有 fixture 证据，继续持有。
	Redeem(EndpointDashScopeTextGeneration,
		canonical.CapTextGeneration,
		canonical.CapStreaming,
		canonical.CapToolCalling,
		canonical.CapReasoning,
		canonical.CapWebSearch,
	).
	// 多模态门：本期投放恰 5 项。file_input 不兑现：官方内容块没有 file。
	// tool_calling / reasoning / web_search 在这扇门上没有 fixture 证据，不兑现。
	Redeem(EndpointDashScopeMultimodal,
		canonical.CapTextGeneration,
		canonical.CapStreaming,
		canonical.CapVisionInput,
		canonical.CapAudioInput,
		canonical.CapVideoInput,
	)
```

带 `enable_thinking`、`tools` 或 `enable_search` 的请求打多模态门会得 501
（等实现），不是 422：设计处置仍是 PASSTHROUGH，只是该门尚未投放这些能力。
这是矩阵唯一权威的正确行为。

其余 11 条 PLANNED 路径没有任何兑现格子：`Endpoints()` 为空，`Implemented()`
为 false，行为不变。

### 分数

`dashscope.native` 可表达能力共 18 项（27 项全集中其余 9 项为 N/A，不进分母），
设计处置全部 PASSTHROUGH，设计分维持 1.000。

| 门 | 兑现 | 可用分 |
|---|---|---:|
| `/api/v1/services/aigc/text-generation/generation` | text_generation, streaming, tool_calling, reasoning, web_search | 5/18 = 0.278 |
| `/api/v1/services/aigc/multimodal-generation/generation` | text_generation, streaming, vision_input, audio_input, video_input | 5/18 = 0.278 |

**显式拒绝并集聚合**：两门并集是 8 项，8/18 = 0.444 不对应任何一扇真实存在的门。
矩阵不定义、不计算、不展示任何路径级的「当前可用」聚合分；`Preservation` 的
可用列永远端点相对。

## 请求流程

```
POST /api/v1/services/aigc/multimodal-generation/generation
  └ Mux 最长前缀命中精确注册的 native handler（不是 POST /api/v1/ 兜底）
      └ serve()
          Authenticate                                  → auth_failed
          readBody + dashscopenative.Decode（宽松解码）  → bad_request
            · 内容块识别 image/audio/video，报对应输入能力
            · X-DashScope-SSE: enable → Stream = true
            · 内联字节按 data URI 统计，video 帧数组逐帧累加
          InlineBytes > Limits.MaxInlineBytes           → bad_request（400）
          Router.Resolve(model)                         → 候选 targets
          Matrix.BestOutbound(Inbound{ProtoDashScopeNative, Endpoint(r.URL.Path)}, kinds, caps)
            └ 每个候选 Check：路径门 → 端点门 → 逐项能力（501/422 在此产生）
          └ dispatch()：凭据池 + 上游 failover（首字节前）
              passthrough（Path = r.URL.Path，即多模态端点）
              relay：非流式 relayJSON / SSE relayStream（usage 抽取不变）
```

handler 侧唯一改动是 `BestOutbound` 的入参：

```go
kind, verdict, err := h.deps.Matrix.BestOutbound(
	degrade.Inbound{
		Protocol: h.in.protocol,
		Endpoint: degrade.Endpoint(h.in.upstreamPath(r)),
	},
	router.Kinds(targets), decoded.Capabilities())
```

`upstreamPath` 对 DashScope Native 入站返回 `r.URL.Path`，对 OpenAI 入站返回
各自的固定路径，三个 handler 无需各自新增逻辑。

## Mux 注册与双向对账

`internal/gateway/build.go` 注册区：

```go
native := NewDashScopeNativeHandler(deps) // handler 无状态，两扇门复用同一实例
mux.Handle("POST "+string(degrade.EndpointOpenAIResponses), NewResponsesHandler(deps))
mux.Handle("POST "+string(degrade.EndpointOpenAIChat), NewChatHandler(deps))
mux.Handle("POST "+dashscopenative.TextGenerationPath, native)
mux.Handle("POST "+dashscopenative.MultimodalGenerationPath, native)
```

两条命名空间兜底**不变**：`POST /api/v1/` 前缀兜底返回 dashscopewire 协议化
501；不带方法的 `/api/v1/` 兜底返回框架 404。精确注册的两扇门都比 `/api/v1/`
更具体，凭最长前缀优先命中。

新增启动期**双向对账**（`Build()` 内，任一方向失败即启动失败）：

```go
registered := map[degrade.Endpoint]bool{
	degrade.EndpointOpenAIResponses:         true,
	degrade.EndpointOpenAIChat:              true,
	degrade.EndpointDashScopeTextGeneration: true,
	degrade.EndpointDashScopeMultimodal:     true,
}
opened := map[degrade.Endpoint]bool{}
for _, r := range m.Routes() {
	if !r.Implemented() {
		continue
	}
	for _, ep := range r.Endpoints() {
		opened[ep] = true
		if !registered[ep] {
			return nil, fmt.Errorf("gateway: 端点 %s 已在矩阵兑现，但没有注册处理器", ep)
		}
	}
}
for ep := range registered {
	if !opened[ep] {
		return nil, fmt.Errorf("gateway: 端点 %s 注册了处理器，但没有任何路径兑现它", ep)
	}
}
```

防的是两种漂移：兑现过的门忘了注册，请求落进 501 兜底；注册了的门忘了在矩阵
兑现，变成一处永远返回 501 的空头承诺。passthrough 适配器的装配默认路径维持
TextGenerationPath 不变，它只是兜底默认值，handler 永远注入实际请求路径。

## fixture 与白名单门槛

### 新 fixture（testdata/routes/dashscope.native__dashscope.native/）

| 文件 | 传输 | 内容块设计 | 证明 |
|---|---|---|---|
| `multimodal-basic.json` | 同步 | text + image（URL）+ audio（内联 data URI）+ video（URL 字符串），纯键式块 | 三种媒体能力、URL 与内联两种承载、纯键式解码分支 |
| `multimodal-streaming.json` | SSE（`x-dashscope-sse: enable`） | text + image（内联 data URI），带 type 块 | 多模态门的 streaming、SSE 头转发、带 type 解码分支 |
| `multimodal-video-frames.json` | 同步 | text + video 为 3 帧 data URI 数组 | video 帧数组逐帧内联累加、video_input |

请求体信封与文本 fixture 相同（model / input.messages / parameters），path 为
多模态端点，模型用逻辑名，上游响应为 DashScope 标准信封（output.choices +
usage.input_tokens/output_tokens + request_id）。流式 fixture 沿用
`streaming-with-usage.json` 的形态：`event:result` + 完整信封 data、用量逐帧
累计、`finish_reason=stop` 收尾、frames 复现上游把多帧挤进一次 Write 的节奏。

三个 fixture 各配 golden（`golden/multimodal-*.txt`），`make golden-update`
生成后人工审阅 diff。该路径全为 PASSTHROUGH 格子，无 DEGRADE/EMULATE，
无能力同名 fixture 的要求。

### 端点级 fixture 路径门槛

`checkRouteFixtures` 扩展为路径双向对账（需要 `testkit.LoadDir` 解析 fixture
的 `request.path`）：

1. 每一扇已开门必须至少有一个 fixture 的 `request.path` 等于该端点
   （每扇门都要有证据）；
2. 每个 fixture 的 `request.path` 必须是一扇已开门
   （给未开门写 fixture 不构成证据，也会让门槛误以为门已兑现）。

`TestDashScopeNativeRouteConformance` 整目录回放，新 fixture 自动纳入；其既有
严格断言（上游收到的 method / path / headers / body 语义与 fixture 完全一致）
会证明 Path 注入对多模态端点生效、SSE 头不被篡改。

### 白名单

`TestImplementedRoutesAreExplicit` 不变（路径级名单）。

`TestRedeemedCapabilitiesAreExplicit` 改为按 `in -> out @ endpoint` 键对账：

```go
want := map[string][]canonical.Capability{
	"openai.responses -> openai.compat @ /v1/responses":
		ExpressibleSet(ProtoOpenAIResponses),
	"openai.chat -> openai.compat @ /v1/chat/completions":
		ExpressibleSet(ProtoOpenAIChat),
	"dashscope.native -> dashscope.native @ /api/v1/services/aigc/text-generation/generation": {
		canonical.CapTextGeneration, canonical.CapStreaming,
		canonical.CapToolCalling, canonical.CapReasoning, canonical.CapWebSearch,
	},
	"dashscope.native -> dashscope.native @ /api/v1/services/aigc/multimodal-generation/generation": {
		canonical.CapTextGeneration, canonical.CapStreaming,
		canonical.CapVisionInput, canonical.CapAudioInput, canonical.CapVideoInput,
	},
}
// 遍历每条路径的每一扇已开门（Route.Endpoints / RedeemedAt），逐项对账：
// 多兑现与少兑现都失败。
```

纪律不变：代码只查到门级集合与 fixture 存在，逐项能力跑没跑通，靠改白名单的人
负责（ADR-0001）。

## 同步 + SSE 测试矩阵

| # | 用例 | 门 | 传输 | 期望 |
|---:|---|---|---|---|
| 1 | basic-non-streaming（既有） | 文本 | 同步 | 200，golden 一致 |
| 2 | streaming-with-usage（既有） | 文本 | SSE | 200，golden 一致 |
| 3 | tools-and-search（既有） | 文本 | 同步 | 200，golden 一致 |
| 4 | multimodal-basic（新） | 多模态 | 同步 | 200，上游收到路径 = 多模态端点，golden 一致 |
| 5 | multimodal-streaming（新） | 多模态 | SSE | 200，SSE 头原样转发，golden 一致 |
| 6 | multimodal-video-frames（新） | 多模态 | 同步 | 200，golden 一致 |
| 7 | 文本门 + image 内容块 | 文本 | 同步 | 501（vision_input 未投放到文本门），上游零调用 |
| 8 | 多模态门 + file 内容块 | 多模态 | 同步 | 501（file_input 两扇门都不兑现），上游零调用 |
| 9 | 多模态门 + enable_thinking | 多模态 | 同步 | 501（reasoning 未投放到该门），上游零调用 |
| 10 | 未登记的 /api/v1/… 端点 | 无 | 同步 | Mux 兜底协议化 501（既有行为） |
| 11 | 多模态门 + 超限内联负载 | 多模态 | 同步 | 400，上游零调用 |

用例 11 用自定义 `Limits`（`MaxInlineBytes: 64`）组装 Deps，发送 128 字节内联
图像，证明内联闸门先于矩阵裁决生效。用例 9 是「无证据不兑现」纪律的活体证明：
哪怕上游模型可能支持，矩阵没有证据就不放行。

`TestUnredeemedCapabilityIsRejectedAtRuntime` 更新为双向断言：文本门带
vision_input → ClassNotImplemented/501；多模态门带 vision_input → 放行。
设计处置断言不变（vision_input 仍是 PASSTHROUGH）。

## 变异测试（闸门必须咬人）

| 测试 | 咬住什么变异 |
|---|---|
| `TestEndpointGateActuallyBites` | 跨门并集：`Check(Inbound{native, 多模态门}, native, [text_generation, tool_calling])` 必须 501，即使 tool_calling 在文本门已兑现；未开门（含空能力集）必须 501 且消息点名端点 |
| `TestEmptyCapabilitiesStillFailAtUnopenedEndpoint` | 空能力集打未开门仍是 501：入口约束不因 caps 为空而豁免 |
| `TestEndpointScoreIsNotRouteAggregate` | `Preservation(avail, 文本门)` 与 `Preservation(avail, 多模态门)` 各报 5/18；显式断言两者都 != 8/18，错误消息写明并集口径已被否决 |
| `TestRedeemZeroEndpointFailsBuild` | 对零值端点 Redeem → Build 失败 |
| `TestRedeemUndeliverableAtEndpointFailsBuild` | 某端点兑现 REJECT / N/A 格子 → Build 失败（既有路径级测试的端点版） |
| `TestEndpointsDerivedFromRedemption` | `Endpoints()` 恰返回有至少一格兑现的端点，字典序；同一门追加 Redeem 不产生重复条目；零兑现路径返回空 |
| `TestDeriveDoesNotInheritRedemption` | 既有测试，在二元键下继续有效：派生路径不继承任何门的兑现 |
| gateway 负例（用例 7/8/9/11） | 每个都断言上游调用计数为 0：请求根本没有出门 |

`implementedMatrix` 测试助手迁移：为每条路径挂一扇测试专用门
`Endpoint("/test/"+string(r.In))` 并在其上兑现全部可表达能力，使「能力裁决语义
与投放进度正交」的既有测试意图原样保留；所有 `Check` / `BestOutbound` 调用点
传入对应 `Inbound`。

## 生成文档与端点细分

`markdown.go` 改动：

1. 主表「当前可用」列：单门已实现路径用 `Preservation(avail, 该门)` 计算，
   显示方式不变（如 `1.000`、`0.929（开启 convstore 后 1.000）`）；多门路径
   显示「见端点细分」。设计列不变，设计计数与端点无关。
2. 主表后新增「端点细分」小节：对每条已实现路径逐门列出端点、已投放能力
   （`RedeemedAt`）、可用分（`Preservation(avail, ep).AvailableScore()`）。
3. 小节带一句固定说明：**不存在**路径级「当前可用」聚合分；各门兑现集合的并集
   不对应任何一扇真实存在的门，给不存在的东西记分正是矩阵要防的过度承诺。
4. 「当前只投放了文本生成那一个」改为「当前投放了文本生成与多模态生成两个端点」。

预期生成效果（Native 路径）：

```
### 端点细分

| 入站 | 出站 | 端点 | 已投放 | 当前可用 |
|---|---|---|---|---:|
| dashscope.native | dashscope.native | /api/v1/services/aigc/text-generation/generation | text_generation, streaming, tool_calling, reasoning, web_search | 0.278（18 项中 5 项已投放） |
| dashscope.native | dashscope.native | /api/v1/services/aigc/multimodal-generation/generation | text_generation, streaming, vision_input, audio_input, video_input | 0.278（18 项中 5 项已投放） |
```

`TestDegradationMatrixDocIsCurrent` 继续守护同步，`make matrix-update` 重新生成。

分数对账测试迁移：

- `TestRuntimeRankingUsesAvailableScore` 改为端点粒度：断言每扇已开门的可用分
  在 (0, DesignScore] 区间；对同一入站协议下兑现了同一扇门的多条路径按偏好序
  比较。Phase 1 没有两条路径同开一扇门，当前半句生效，将来同门第二路径出现时
  排序对账自动生效，不会空转。
- `TestAvailableScoreCountsOnlyRedeemed` 重写为端点单位：两扇门各 5/18、
  设计分 1.000、显式断言 != 8/18。
- `TestPreferenceMatchesPreservation` 只用设计列，设计计数与端点无关，传
  `Endpoint("")` 即可；`Endpoint("")` 的可用列恒为零，误用只会少报不会多报。

## 迁移兼容

仓库未发布（M1 进行中），允许破坏性 API 变更，无对外承诺需要维持：

- `Route.Redeem` / `Redeems` 签名变化；`Check` / `BestOutbound` 改收 `Inbound`；
  `Preservation` 增加端点参数。全部调用点（rules、markdown、matrix_test、
  preference_test、gateway 与其测试助手）在同一变更内迁移。
- 配置文件格式零变化：门是代码声明，不进配置。
- 运行时行为的唯一可见变化：打多模态端点的请求从 Mux 兜底 501 变为对已兑现集合
  正常转发；未兑现能力仍是 501，错误信封形状不变（兜底与 handler 都走
  `dashscopewire.EncodeError`，同为 DashScope 扁平信封）。
- 指标口径不变：兜底记 `ObserveNotImplemented`，矩阵 501 记 outcome
  `not_implemented`，两者语义一致。
- README 状态块与 AGENTS.md 知识库在实现落地后同步更新（不属于本设计文档的
  改动）。

## 完成标准

1. `internal/degrade`：`Endpoint` / `Inbound` 类型落地；Route 的
   `Redeem(ep, caps...)` / `Redeems(ep, c)` / `RedeemedAt(ep)` / `Endpoints()` /
   `ImplementedAt(ep)` / `Implemented()` / `Preservation(avail, ep)` 落地；
   `Check(Inbound, out, caps)` / `BestOutbound(Inbound, candidates, caps)` 落地；
   Build 两项新校验生效；路径级 redeemed 删除。
2. `dashscopenative`：新增 `MultimodalGenerationPath` 常量；解码器零改动。
3. `rules_phase1.go`：OpenAI 两条路径各一扇门全量兑现；Native 路径两扇门
   5 + 5 项能力；11 条 PLANNED 路径无门。
4. `gateway`：Mux 双端点注册 + 双向对账；`serve()` 传 `Inbound`；两条命名空间
   兜底不变。
5. 三个新 fixture + 三份 golden（人工审阅过 diff）；端点级 fixture 路径门槛
   双向生效。
6. 白名单按 `in -> out @ endpoint` 对账并通过。
7. 测试矩阵 11 个用例全部落地；变异测试全部咬人（含 8/18 否决断言与空能力集
   端点闸门）。
8. `make matrix-update` 重新生成文档，端点细分小节呈现两个 0.278；README 同步。
9. `make check`（fmt-check + vet + test + matrix）全绿，`make test-race` 通过。

## 复核（针对常见误读）

- 本设计**不声称**「多模态门支持全部四种多模态输入」：`file_input` 不兑现，
  带 file 块的请求得 501。官方内容块词表是 text / image / audio / video，
  而本网关本期只兑现其中输入侧的 image / audio / video 三项加文本与流式。
- 多模态门的可用分是 5/18，不是 8/18；并集聚合被显式拒绝，路径级可用聚合分
  不存在。
- `reasoning` 只在文本门持有（fixture 证据在 `tools-and-search.json`）；
  多模态门不兑现 reasoning。
- 多模态输出能力（audio_output 等）不在本期范围。
- 共用同一 URL 的 multimodal-dialog、语音识别、图像生成等产品请求不是本门的
  兑现对象；同源直通约定下字节原样透传，受理由上游决定。
