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
| `n` | `parameters.n` | 等价映射；模型限制由上游协议化错误暴露 |
| `logprobs` / `top_logprobs` | 同名参数 | 逐项映射 |
| `tools` / `tool_choice` | 同名参数 | 同时强制 `result_format:message` |
| 工具调用历史 | `assistant.tool_calls` / `tool` | id、name、arguments、tool_call_id 保持关联 |
| `parallel_tool_calls` | `parameters.parallel_tool_calls` | 显式映射；路径仍因模型支持面不一致而 DEGRADE |
| `reasoning_effort` | `enable_thinking` / `reasoning_effort` | 按已文档化档位表达；开启思考时强制 stream 与 incremental |
| `web_search_options` | `enable_search:true` | 只保留开关，丢失位置、上下文大小等选项 |
| `response_format=json_object` | Native `response_format` | 原样表达模式，模型支持面仍受限 |
| `response_format=json_schema` | Native `response_format` | schema 与 strict 保留；路径级不承诺所有模型严格执行 |

`parallel_tool_calls` 的官方契约明确存在 Native 显式开关。本项修正旧矩阵中“无显式
开关”的事实错误；处置仍为 `DEGRADE`，但 note 必须改为“模型支持面与并行行为不具
路径级全局保证”，不能继续声称字段不存在。

OpenAI 图片 URL/data URI 转成 `{"image":...}`，音频转成 `{"audio":...}`，文本转成
`{"text":...}`。多模态门的 user content 使用数组；text-generation 门只接受 string。
网关不代下载 URL，不跨 Provider 搬运 `FileRef`。

以下无 Native 落点的 Chat 字段只要被客户端显式提交，就在上游调用前返回 422，并点名
字段：`frequency_penalty`、`logit_bias`、`service_tier`、`store`。实现时具名投影必须
枚举全部已接受字段；不能把未知字段静默丢弃。

能力可以组合。编码器必须在一次请求里同时保留消息、媒体、工具、搜索、结构化输出与
推理配置；不得按“主能力”选择互斥转换分支。

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
- `choices[].message.content/reasoning_content/tool_calls/logprobs` 逐项映射。
- `stop/length/tool_calls` 映射为同名 Chat `finish_reason`。
- `input_tokens/output_tokens` -> `prompt_tokens/completion_tokens`；缺失
  `total_tokens` 时由两者相加，缓存与推理明细按 Canonical Usage 映射。

读取、解析或编码失败发生在 `Call` 返回前，因此下游尚无首字节，现有凭据/Provider
failover 仍然有效。成功后返回替换过 body、Content-Type 与长度信息的
`httpx.Response`，Gateway 继续走现有 `relayJSON`。

## 流式成功响应

translator 返回同步 transforming `io.ReadCloser`。每次 `Read` 从 Native SSE reader
取得一帧，解成 Canonical 事件，再缓冲编码成一条或多条 Chat SSE：

1. 出站请求设置 `X-DashScope-SSE: enable` 与 `incremental_output:true`。
2. 第一条 Chat chunk 建立稳定的 `id/model/created` 与 assistant role。
3. 文本、`reasoning_content`、logprobs 和工具参数按事件增量输出；工具 arguments 保持
   字符串片段，允许 JSON 跨帧切断，不提前解析成完整对象。
4. JSON null 与字符串 `"null"` 都映射为 Chat null finish reason。
5. Native 最终 finish 帧先编码为带 finish_reason 的 Chat chunk。
6. 仅当客户端请求 `stream_options.include_usage` 时，再输出空 choices 的最终 usage chunk。
7. 最后合成 `data: [DONE]`，不等待 Native 提供未文档化的终止标记。

`Close` 必须立即关闭 Native body；客户端断开后不再读上游。转换器无 goroutine、无锁、
无跨请求共享状态，避免泄漏和异步错误丢失。

## DashScope 专用 usage 回调

`provider.Request.OnDashScopeUsage` 是请求级回调。Gateway 仅在
`in.kind == ProviderDashScopeNative` 时注入闭包，其他 Provider 必须为 nil。

Native 每帧携带累计 usage。transform reader 每成功解码一帧就同步调用回调，后值覆盖
前值；回调与 `Body.Read` 在同一 goroutine 执行，不需要锁。非流式转换也可在解码完成
后调用，以保持 Provider 契约一致。

relay 返回后，dispatch 按以下顺序确定指标 usage：

1. Chat response body/SSE 已提供 authoritative usage，采用 relay 结果。
2. relay 未取得 usage，但专用回调已有 authoritative usage，采用回调末值。
3. 两者都没有，使用 `FidelityUnavailable`。

该回调是用户明确选择的 DashScope 专用耦合。它不改造成通用 `UsageSource`，也不把
Provider 身份判断散到 relay；专用分支集中在 dispatch 请求构造与 usage 汇合点。

## 错误与首字节生命周期

- Native 非 2xx 先由 `dashscopewire.DecodeError` 解码，不进入成功响应转换。
- Chat 请求形态错误返回 400；显式不可映射字段、无媒体门、矩阵 REJECT 返回 422。
- 路径或能力尚未兑现返回 501，不调用上游。
- `Provider.Call` 返回前的 Native 读取/转换错误允许现有 failover。
- 下游首字节之后出现畸形 SSE 或编码错误，由 transforming body 的 `Read` 返回；
  `relayStream` 写 OpenAI `event:error`，usage 标记 unavailable，绝不重试。
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

## 测试与 fixture

### 协议单测

- Native 请求信封、两扇门的 content 形态、所有参数映射与显式不可映射字段。
- Native 非流式与 SSE 解码：finish reason 两种 null、tool args、reasoning、logprobs、
  request_id、累计 usage 与缺失 total_tokens。
- Chat 非流式与 SSE 编码：稳定 id/model/created、跨帧工具参数、usage chunk 与 `[DONE]`。

### Provider 与 Gateway 契约

- Native 入站继续 passthrough；Chat 入站走 translator；未知入站 fail-closed。
- `Inbound` 取代 `Path` 后，同一坐标同时用于矩阵与 Provider。
- target 门选择、媒体过滤、多目标顺序与过滤后零候选的 422。
- 专用 usage 回调只为 DashScope Native 注入，累计覆盖、relay 优先级与 unavailable 路径。
- 非流式转换失败可 failover；流式首字节后失败不可重试；Close 关闭上游 body。

### 路径 fixture

新增 `testdata/routes/openai.chat__dashscope.native/`，九项能力各有独立 fixture，并至少
增加一条同时包含 `vision + tools + web_search + structured_output` 的组合 fixture。
每份 fixture 必须同时断言：

- 上游 method、path、Authorization、关键请求头和完整请求 JSON；
- 下游状态、响应 wire、usage、finish reason、降级头与 golden；
- 流式 fixture 保留真实 Native frame 边界，不把分片重写成理想形态。

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
