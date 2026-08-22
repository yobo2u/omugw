# OpenAI Chat 到 DashScope Native 路径投放设计

**日期**：2026-08-22
**状态**：已批准，待实现
**前序约束**：ADR-0001（声明必须由 fixture 兑现）、ADR-0002（设计分与可用分分离）

## 背景

`openai.chat -> dashscope.native` 是第五条投放路径，也是首条请求与响应都必须完整
重编码的异构路径。OpenAI Chat 与 DashScope Native 在请求信封、消息内容、流式开关、
SSE 帧、usage 和结束标记上都不同，不能复用 wire-compatible 直通方案。

本设计的官方事实依据记录在
[`docs/research/2026-08-22-dashscope-native-text-generation-contract.md`](../../research/2026-08-22-dashscope-native-text-generation-contract.md)。
该笔记只使用阿里云官方文档，并把裸 HTTP 响应顶层、流中错误形态、Native 终止帧等
未文档化事项单列出来。首次投放必须用真实抓包复核这些存疑点。

## 官方事实与存疑项

- Native 请求为 `{model,input:{messages},parameters}`；流式由
  `X-DashScope-SSE: enable` 请求头开启，body `stream` 不是 HTTP 契约。
- text-generation 与 multimodal-generation 是两扇不同的门；模型与门不匹配会失败。
- 流式 Native SSE 使用 `event:result`，每帧含累计 usage，没有文档化的 `[DONE]`。
- `incremental_output:true` 才提供增量内容；思考模式强制流式、增量输出和
  `result_format:message`。
- 中间帧 `finish_reason` 的官方样例既可能是 JSON null，也出现字符串 `"null"`。
- `response_format=json_schema` 只由有限模型系列保证；思考与 JSON 模式的组合行为
  在官方文档间不一致。
- 裸 HTTP 非流式响应顶层按 `output/usage/request_id` 推断，必须由真实抓包确认。
- 流中错误事件、SSE 响应头和显式终止帧均未被官方文档完整定义。
- `output.choices[]` 没有官方 `index` 字段，跨帧的顺序与长度是否稳定未文档化；
  多候选编码完全依赖这一假定。
- `incremental_output` 在 `n > 1` 时是否按候选各自增量，官方未说明。
- `n > 1` 与 `reasoning_effort` 并存时是静默回落还是报错，官方未说明；本设计
  fail-closed 处理。

以上三项与前述存疑点一样，必须由首批真实录制解答，不能靠推断放行。

## 决策

采用 **Composite Provider**：`dashscope.native` 的一个出站适配器在同一接口后隐藏两种
实现。Native 入站复用既有 passthrough；OpenAI Chat 入站进入 translator。Gateway 不
承担协议转换，只负责入站解码、矩阵裁决、目标/凭据 failover 与下游首字节生命周期。

1. `provider.Request` 增加明确的 `Inbound degrade.Inbound`，删除 `Path`。矩阵与 Provider
   消费同一份入站坐标，避免把裸路径暗当协议标签。
2. `provider.Request` 增加请求级 `OnDashScopeUsage func(canonical.Usage)`。Gateway 只在
   `ProviderDashScopeNative` 分支注入；这是已接受的 Provider 专用耦合，不推广成通用
   usage seam。
3. Canonical 是消息、工具、媒体和能力的权威表示。Canonical 尚未承载的 Chat 采样与
   输出选项由具名、严格解码的 Chat wire 投影补齐；不读取 `Extensions`，也不从任意
   JSON 猜语义。
4. Native 门是目标的部署事实，由 `TargetSpec.native_endpoint` 明确声明；不按模型名
   或本次请求是否含媒体推断。
5. 非流式响应在 `Provider.Call` 返回前完成转换；流式响应由同步 transforming
   `io.ReadCloser` 按需转换，不使用 `io.Pipe`、goroutine 或跨请求状态。

## 模块与接口

新增 `internal/provider/dashscopenative` Composite Provider，继续满足唯一的
`provider.Provider` 接口：

```text
provider.Request
  Inbound == dashscope.native -> passthrough.Call
  Inbound == openai.chat      -> translator.Call
  其他入站坐标                -> fail-closed
```

模块内部 seam：

- `internal/protocol/dashscopenative`：Native 出站请求编码、成功响应与 `event:result`
  解码，只处理 wire 与 Canonical 的关系。
- `internal/protocol/openaichat`：Chat 非流式成功响应与 SSE chunk 编码。
- `internal/provider/dashscopenative`：选择实现、建立 HTTP 请求、解码 Native 错误、串起
  两端 codec，并掌管 response body 生命周期。
- `internal/gateway`：构造一次 `degrade.Inbound`，过滤目标，注入专用 usage 回调，转发
  已经是入站协议 wire 的响应。

`openaichat/decode.go`、`gateway/handler.go` 与 `canonical/stream.go` 已接近或超过模块尺寸
警戒线。新职责必须进入聚焦的新文件或新包，不继续堆入这些文件。

## 请求映射

出站信封固定为：

```json
{
  "model": "<target.UpstreamModel>",
  "input": {"messages": []},
  "parameters": {"result_format": "message"}
}
```

| OpenAI Chat | DashScope Native | 规则 |
|---|---|---|
| `messages` | `input.messages` | Canonical 角色、文本、工具历史逐项编码 |
| `max_completion_tokens` / `max_tokens` | 同名参数 | 保留客户端实际提交的字段，不同时制造两个限制 |
| `temperature` | `parameters.temperature` | 等价映射 |
| `top_p` | `parameters.top_p` | 等价映射 |
| `seed` | `parameters.seed` | 采用 Native `int64` 范围；官方范围矛盾留给真实验证 |
| `stop` | `parameters.stop` | 保留 string 或 string array 语义 |
| `presence_penalty` | `parameters.presence_penalty` | 等价映射 |
| `n` | `parameters.n` | 逐字映射；响应侧由 translator 实现多 choice 编码（见下） |
| `logprobs` / `top_logprobs` | 同名参数 | 逐项映射 |
| `tools` / `tool_choice` | 同名参数 | 同时强制 `result_format:message` |
| 工具调用历史 | `assistant.tool_calls` / `tool` | id、name、arguments、tool_call_id 保持关联 |
| `parallel_tool_calls` | `parameters.parallel_tool_calls` | 缺省时显式注入 `true` 对齐 OpenAI 默认；仍 DEGRADE |
| `reasoning_effort` | `parameters.reasoning_effort` / `enable_thinking` | 档位逐字透传，网关不做档位重映射；非流式请求提交该字段即 422 |
| `web_search_options` | `enable_search:true` | 只保留开关，丢失位置、上下文大小等选项 |
| `response_format=json_object` | Native `response_format` | 原样表达模式，模型支持面仍受限 |
| `response_format=json_schema` | Native `response_format` | schema 与 strict 保留；路径级不承诺所有模型严格执行 |
| `modalities` | — | 能力门：`audio` 经 `CapAudioOutput` REJECT 422；`text` 无出站落点；其他值在解码期 400 |
| `stream_options` | — | 网关消费，不转发上游；须按 `web_search_options` 的先例严格子解码 |

`parallel_tool_calls` 的官方契约明确存在 Native 显式开关。本项修正旧矩阵中“无显式
开关”的事实错误；处置仍为 `DEGRADE`，note 必须同时写明两件事：模型支持面与并行行为
不具路径级全局保证，以及**客户端未提交该字段时网关按 OpenAI 默认显式注入 `true`**
（Native 默认为 `false`，不注入会让行为静默变成串行）。

`reasoning_effort` 逐字透传，不在网关侧建档位映射表：官方档位到思考预算的换算是
按模型、在服务端完成的，网关再维护一份只会成为第二个过期的事实源。同理不发
`thinking_budget`——官方明确它与 `reasoning_effort` 不得并存。

OpenAI 图片 URL/data URI 转成 `{"image":...}`，音频转成 `{"audio":...}`，文本转成
`{"text":...}`。多模态门的 user content 使用数组；text-generation 门只接受 string。
网关不代下载 URL，不跨 Provider 搬运 `FileRef`。

### 入站 422 规则

以下情形在上游调用前返回 422 并点名字段。实现时具名投影必须枚举全部已接受字段；
不能把未知字段静默丢弃。

| 条件 | 理由 |
|---|---|
| `frequency_penalty`、`logit_bias`、`service_tier`、`store` 显式提交 | Native 无落点 |
| `user`、`metadata` 显式提交 | Native 无落点；`metadata` 是 `store` 的伴生字段，而 `store` 已 422 |
| `audio` 显式提交 | 音频输出请求；不带 `modalities` 时能力门拦不住，会静默丢失 |
| `reasoning_effort` 提交且 `stream != true` | 官方硬约束：思考模式不允许非流式调用 |
| `n > 1` 且同时提交 `tools` | 官方契约：带 tools 时 Native 把 `n` 强制为 1，**且不报错** |
| `n > 1` 且同时提交 `reasoning_effort` | `n` 仅非思考模式支持；强制回落与否未文档化，fail-closed |

`n > 1` 与 tools/推理的组合必须在入站拦截，而不是透传等上游报错：官方契约里这是
**静默强制回落**，客户端会收到一个 200、少了候选、没有降级头、矩阵也看不见——正是
本仓库最忌讳的那种不可见损失。相对地，模型完全不支持 `n` 属于上游会明确报错的情形，
照常透传，让 Native 的 `InvalidParameter` 浮上来。

`stream_options` 是本路径第一次真正消费它。当前它是 `json.RawMessage`，外层的
`DisallowUnknownFields` 进不到子树，因此必须按 `web_search_options` 的先例补一个严格
子解码器，显式枚举接受的子字段（`include_usage`、`include_obfuscation`），未知子字段
返回 400。它只服务网关自身的 usage chunk 决策，不进出站请求。

能力可以组合。编码器必须在一次请求里同时保留消息、媒体、工具、搜索、结构化输出与
推理配置；不得按“主能力”选择互斥转换分支。

### 推理档位保真

档位继续走 Canonical IR，**不**从具名投影另取一份原始字符串。并存两个来源会让矩阵与
编码器看到互相矛盾的事实——`UsedCapabilities` 从 `Reasoning != nil` 推导
`CapReasoning`，矩阵按其中一份裁决、编码器却用另一份，本质上就是换了名字的
`Extensions` 反模式。

当前 IR 有两处缺口需要一并补上：

1. `canonical.ReasoningEffort` 已声明 `EffortMinimal`，但 Chat 解码器只接受
   `low/medium/high`，`minimal` 今天会被 400 拒掉。解码器补上 `minimal`。
2. 新增 `EffortNone`，承载官方文档里 `none -> enable_thinking:false` 的语义。
3. `EffortNone` **不得**报告 `CapReasoning`：它是显式关闭推理，若报告成使用推理，会
   撞上本路径“非流式 + 推理即 422”的规则，把一个合法请求拒掉。守卫加在
   `UsedCapabilities` 的推理判定上。

translator 的编码规则：`EffortNone` 只发 `parameters.enable_thinking:false`，别的什么
都不发；其余档位逐字写入 `parameters.reasoning_effort`。

## 目标门声明与过滤

`config.TargetSpec` 与 `router.Target` 增加 `NativeEndpoint`，YAML 名称为
`native_endpoint`，枚举仅有：

- `text-generation`
- `multimodal-generation`

校验规则：

1. target 所指 Provider `kind == dashscope.native` 时必填。
2. target 所指其他 Provider kind 时必须为空。
3. 未知枚举值在启动期失败。
4. 同一逻辑模型可以配置多个 DashScope Native target，保留配置顺序作为 failover 顺序。

```yaml
models:
  - match: qwen-*
    targets:
      - endpoint: dashscope-prod
        upstream_model: qwen3.7-plus
        native_endpoint: multimodal-generation
```

纯文本请求保留 text 与 multimodal 两类目标；含图片或音频时过滤掉 text 目标。过滤发生
在矩阵选出 `ProviderDashScopeNative` 后、进入凭据循环前，且只影响该 Provider 的候选。
过滤后无目标返回 422，说明模型路由没有可承载媒体的 Native 门，不触达上游。

Provider 严格使用 target 声明选择 `TextGenerationPath` 或
`MultimodalGenerationPath`。媒体只用于候选过滤和内容编码，不用于猜测上游路径。

## 非流式成功响应

translator 在 `Provider.Call` 内完整读取 Native body，解码为 Canonical 完整结果，再编码
为 `chat.completion`：

- Native `request_id` -> Chat `id`；同一响应内保持稳定。
- `target.UpstreamModel` -> Chat `model`。
- `output.choices[]` **逐条**编码为 Chat `choices[]`，候选序号取数组下标；
  `message.content/reasoning_content/tool_calls/logprobs` 在每个候选内逐项映射。
- `stop/length/tool_calls` 映射为同名 Chat `finish_reason`。
- `input_tokens/output_tokens` -> `prompt_tokens/completion_tokens`；缺失
  `total_tokens` 时由两者相加，缓存与推理明细按 Canonical Usage 映射。
- Native 的 `image_tokens`/`video_tokens`/`audio_tokens` 在 `canonical.Usage` 里没有
  对应字段，多模态 token 明细就此丢失。本期不为此新增 Usage 字段。

读取、解析或编码失败发生在 `Call` 返回前，因此下游尚无首字节，现有凭据/Provider
failover 仍然有效。成功后返回替换过 body、Content-Type 与长度信息的
`httpx.Response`，Gateway 继续走现有 `relayJSON`。

## 流式成功响应

translator 返回同步 transforming `io.ReadCloser`。每次 `Read` 从 Native SSE reader
取得一帧，编码成一条或多条 Chat SSE：

1. 出站请求设置 `X-DashScope-SSE: enable` 与 `incremental_output:true`。
2. 第一条 Chat chunk 建立稳定的 `id/model/created` 与 assistant role。
3. 文本、`reasoning_content`、logprobs 和工具参数按事件增量输出；工具 arguments 保持
   字符串片段，允许 JSON 跨帧切断，不提前解析成完整对象。
4. JSON null 与字符串 `"null"` 都映射为 Chat null finish reason。
5. Native 最终 finish 帧先编码为带 finish_reason 的 Chat chunk。
6. 仅当客户端请求 `stream_options.include_usage` 时，再输出空 choices 的最终 usage chunk。
7. 最后合成 `data: [DONE]`，不等待 Native 提供未文档化的终止标记。

### 多候选（n > 1）

一个 Native 帧同时携带**全部候选**的 `output.choices[]`，而 `canonical.Event.Index`
是内容块序号、`Accumulator` 只产出一条消息，两者都没有候选维度。因此：

1. **候选序号取 `output.choices` 的数组下标**，并假定跨帧顺序与长度稳定。某一帧的
   候选数少于此前帧即视为契约违例，按流内错误处理，不做补齐猜测。该假定尚未被官方
   文档确认，已列入存疑清单，首份真实录制必须覆盖 `n=2` 的流。
2. **候选状态是 translator 的内部状态**：按候选序号维护各自的解码状态，若内部使用
   Canonical 事件，必须成对携带 `(候选序号, 事件)`，且**不得**经过共享的
   `canonical.Accumulator`——它在闭合时要求重组后的工具参数是合法 JSON，与本路径
   “片段原样透传、不提前解析”的要求直接冲突。
3. **每个 Native 帧编码成一条 Chat chunk**，其中该帧出现的每个候选各占一个 `choices`
   条目，带自己的 `index`、`delta` 与 `finish_reason`。不按候选拆成多条 chunk：那会
   成倍放大帧数且没有收益。
4. **全部候选都发出非 null 的 `finish_reason` 之后**才合成 `[DONE]`。若 Native 末帧
   只给出部分候选的 finish_reason，translator 不得替其余候选编造。
5. `incremental_output` 在多候选下是否按候选各自增量，官方未文档化，列入存疑清单。

### Read 契约

`Read` **不得**返回 `(0, nil)`。下游 `relayStream` 用 `bufio.Scanner` 再解析一次
本转换器的输出，连续的空读会触发 `io.ErrNoProgress`，变成一个语焉不详的流内错误。
而零字节帧是现实存在的：全空 delta 的保活帧、或纯粹用于携带 usage 的帧。

因此 `Read` 内部循环消费 Native 帧，直到手上有字节可交、或遇到 EOF/错误为止；并持有
待输出缓冲，跨多次 `Read` 按调用方给的任意缓冲区大小分批交付——一个 Native 帧可能
产出多条 Chat SSE 事件。

`Close` 必须立即关闭 Native body；客户端断开后不再读上游。转换器无 goroutine、无锁、
无跨请求共享状态，避免泄漏和异步错误丢失。

## DashScope 专用 usage 回调

`provider.Request.OnDashScopeUsage` 是请求级回调。Gateway 仅在
`in.kind == ProviderDashScopeNative` 时注入闭包，其他 Provider 必须为 nil。

Native 每帧携带累计 usage。transform reader 每成功解码一帧就同步调用回调，后值覆盖
前值；回调与 `Body.Read` 在同一 goroutine 执行，不需要锁。非流式转换**必须**在解码
成功后调用回调恰好一次，让 Provider 契约在两种形态下一致、也让测试有确定断言点；
但它在非流式下是惰性的——译出的 Chat body 自带 usage，下面的优先级 1 永远先命中。
这份惰性要写明，以免日后有人“修正”优先级顺序。

relay 返回后，dispatch 按以下顺序确定指标 usage：

1. Chat response body/SSE 已提供 authoritative usage，采用 relay 结果。
2. relay **无错误返回**且未取得 usage 时，采用回调末值。
3. 其余情况使用 `FidelityUnavailable`。

第 2 条必须限定在 relay 无错误返回上。`relayStream` 在流中断路径上是**故意**把 usage
抹成 unavailable 的——上游不会再送 usage，任何非零数字都是编造的。不限定条件的话，
这条优先级恰好在中断路径上生效，把刚抹掉的数字又捡回来。若要在中断时保留部分计费
数据，那是一次独立的口径变更，必须单独决策，不能作为引入回调的副作用发生。

该回调是用户明确选择的 DashScope 专用耦合。它不改造成通用 `UsageSource`，也不把
Provider 身份判断散到 relay；专用分支集中在 dispatch 请求构造与 usage 汇合点。

## 错误与首字节生命周期

- Native 非 2xx 先由 `dashscopewire.DecodeError` 解码，不进入成功响应转换。
- Chat 请求形态错误返回 400；显式不可映射字段、无媒体门、矩阵 REJECT 返回 422。
- 路径或能力尚未兑现返回 501，不调用上游。
- `Provider.Call` 返回前的 Native 读取/转换错误允许现有 failover。
- 流式路径必须在 `Call` 内**预读并解码 Native 首帧**，缓冲起来交给 transforming reader
  重放。`relayStream` 在读第一个上游事件之前就写下响应头，`tracked.wrote` 随之置位、
  failover 永久关闭；没有这一步，“200 但首帧畸形”“200 但首帧是错误帧”都会落进不可
  恢复区，而后者按官方文档属于未定义行为，必须假定可能发生。代价是几毫秒 TTFT。
- 下游首字节之后出现畸形 SSE 或编码错误，由 transforming body 的 `Read` 返回；
  `relayStream` 写 OpenAI `event:error`，usage 标记 unavailable，绝不重试。
- transforming reader 返回的错误必须是 `*canonical.Error`。SSE reader 会用 `%w` 包一层
  再交给 `canonical.AsError` 解开，返回裸 error 会让分类与状态码在这一层丢失。
- 客户端断开时 relay 结束并关闭上游 body，不把它归为上游故障。

## 能力兑现

在 `EndpointOpenAIChat` 一次兑现 9 项：

| 能力 | 处置 | 兑现证据 |
|---|---|---|
| `text_generation` | PASS | 同步基本 fixture |
| `streaming` | PASS | Native SSE -> Chat SSE、usage 与 `[DONE]` fixture |
| `tool_calling` | PASS | 工具声明、历史、调用与跨帧 arguments fixture |
| `vision_input` | PASS | multimodal 门 image URL/data URI fixture |
| `audio_input` | PASS | multimodal 门 audio fixture |
| `reasoning` | PASS | `enable_thinking` 与 reasoning_content fixture |
| `parallel_tool_calls` | DEGRADE | 显式字段映射、模型支持差异与降级头 fixture |
| `structured_output` | DEGRADE | json_object/json_schema 保留与模型保证差异 fixture |
| `web_search` | DEGRADE | options -> enable_search 与丢失项可见 fixture |

`file_input` 与 `audio_output` 保持 422 REJECT，不进入兑现集合。设计分与这扇门的可用分
均为 `(6 + 0.5 * 3) / 11 = 0.682`。`dashscope.compatible` 的 0.727 仍优先，不修改
`OutboundPreference`。

多候选（`n > 1`）**不新增 Capability**：它是输出基数，不是矩阵需要裁决的能力，
`AllCapabilities()` 保持不变。它搭载在 `text_generation` 与 `streaming` 两项上兑现，
因此这两项的证据清单里必须各加一份 `n=2` 的 fixture（非流式一份、流式一份）。没有这两
份证据，多候选就是一段没有兑现义务的实现。

## 测试与 fixture

### 协议单测

- Native 请求信封、两扇门的 content 形态、所有参数映射与显式不可映射字段。
- 入站 422 规则逐条：四个无落点字段、`user`/`metadata`、`audio`、非流式 + 推理、
  `n>1` + tools、`n>1` + 推理，每条都断言点名了字段且上游调用次数为零。
- `stream_options` 严格子解码：未知子字段 400；`include_usage` 决定是否发 usage chunk。
- 推理档位：`minimal` 不再被拒；`none` 只发 `enable_thinking:false` 且不报告
  `CapReasoning`（否则非流式会被 422 误伤）。
- `parallel_tool_calls` 缺省时出站体含 `true`。
- Native 非流式与 SSE 解码：finish reason 两种 null、tool args、reasoning、logprobs、
  request_id、累计 usage 与缺失 total_tokens。
- Chat 非流式与 SSE 编码：稳定 id/model/created、跨帧工具参数、usage chunk 与 `[DONE]`。
- 多候选编码：候选序号取数组下标；一帧一条 chunk、每候选一个 `choices` 条目；
  帧内候选数缩水按流内错误处理；全部候选 finish 后才发 `[DONE]`。

### Provider 与 Gateway 契约

- Native 入站继续 passthrough；Chat 入站走 translator；未知入站 fail-closed。
- `Inbound` 取代 `Path` 后，同一坐标同时用于矩阵与 Provider。
- target 门选择、媒体过滤、多目标顺序与过滤后零候选的 422。
- 专用 usage 回调只为 DashScope Native 注入，累计覆盖、relay 优先级与 unavailable 路径；
  流中断时**不**用回调值覆盖已抹除的 usage。
- 非流式转换失败可 failover；首帧预读让“200 但首帧畸形”仍可 failover；首帧之后
  不可重试；`Close` 关闭上游 body。
- transforming reader 的 `Read` 永不返回 `(0, nil)`，且错误是 `*canonical.Error`。

### 路径 fixture

新增 `testdata/routes/openai.chat__dashscope.native/`，九项能力各有独立 fixture，另加：

- 一条同时包含 `vision + tools + web_search + structured_output` 的组合 fixture；
- `n=2` 的非流式 fixture 与 `n=2` 的流式 fixture（多候选的兑现证据）；
- `parallel_tool_calls` 缺省注入 `true` 的 fixture。

每份 fixture 必须同时断言：

- 上游 method、path、Authorization、关键请求头和完整请求 JSON；
- 下游状态、响应 wire、usage、finish reason、降级头与 golden；
- 流式 fixture 的**上游录制**保留真实 Native frame 边界，不把分片重写成理想形态。
  下游 golden 则是 `relayStream` 重新解析并重新序列化之后的结果，空白与字段排布已被
  规范化——这是预期的，审阅时不必追查这类差异。

首批 Native fixture 必须由真实 DashScope 录制，组合 smoke 也必须在真实模型上通过。
若找不到一个真实模型支持所声明组合，必须回到设计阶段收窄能力承诺，不能构造假 fixture
或把多份互不相容的模型证据拼成“可组合”。离线 CI 回放脱敏后的真实录制，不携带凭据。

## 文档与投放

实现完成后同步：

1. `rules_phase1.go` 的 `Redeem(EndpointOpenAIChat, ...)` 与修正后的降级 note；
2. `TestImplementedRoutesAreExplicit` 路径白名单；
3. `TestRedeemedCapabilitiesAreExplicit` 的“路径 @ 端点”能力白名单；
4. Gateway Provider 装配、示例配置与配置校验测试；
5. 路径 fixture、golden、conformance 与 provenance；
6. `make matrix-update` 生成的 `docs/degradation-matrix.md`；
7. README 与相邻 AGENTS 知识库中的当前投放状态。

本设计还需要以下既有代码的小改动，实现时不得遗漏：

- `canonical.ReasoningEffort` 新增 `EffortNone`；
- `openaichat.decodeReasoning` 接受 `minimal` 与 `none`；
- `canonical.Request.UsedCapabilities` 对 `EffortNone` 不报告 `CapReasoning`；
- `openaichat` 新增 `stream_options` 严格子解码器；
- `provider.Request` 增 `Inbound`、删 `Path`，同步改 `passthrough` 与 `dashscopecompat`
  两个已通车适配器的取路径方式。

本设计没有引入新的领域术语，`CONTEXT.md` 保持不变。

## 非目标

- `openai.responses -> dashscope.native`。
- DashScope Realtime、音频输出、视频输入或通用 file 输入。
- 从模型名、媒体存在性或上游报错反推 Native 门。
- 代下载媒体 URL、跨 Provider 上传文件或搬运 FileRef。
- 为模型专属参数增加隐式默认值、兼容 shim 或自动重试。
- 将 DashScope usage 回调推广为所有 Provider 的公共抽象。
- 在路径级承诺某一个具体模型才支持的组合。

## 完成标准

- 九项能力均有逐项真实 fixture；组合 fixture 与真实 smoke 证明能力可组合。
- `native_endpoint` 在配置、router、过滤与 Provider 路径选择中只有一个事实来源。
- 显式不可映射字段在触达上游前返回点名字段的 422。
- 非流式转换保留 failover；首字节后的任何流式错误均不重试。
- Native 累计 usage 可供 Chat 响应与指标使用，缺失时不伪造零值。
- Native 同源两扇门 passthrough 无回归。
- `make check`、`make test-race`、`go build ./...`、相关 LSP 诊断与真实 smoke 全部通过。
