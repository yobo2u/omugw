# OpenAI Chat 到 DashScope Compatible 路径投放设计

**日期**：2026-08-15
**状态**：已批准，待实现
**前序约束**：ADR-0001（声明必须由 fixture 兑现）、ADR-0002（设计分与可用分分离）

## 背景

`openai.chat -> dashscope.compatible` 是下一条要投放的异构路径。它的请求与响应
仍使用 OpenAI Chat Completions 线格式，但上游语义并不等同于 OpenAI：模型能力、
结构化输出保证与 Web Search 参数都有差异。因此这条路可以复用原始 wire，不能被
标成同源快通道，也不能绕开降级矩阵。

该路径是当前成本最低、用户价值最高的 HTTP tracer bullet：OpenAI Chat 入站解码、
HTTP transport、SSE relay、Chat usage 抽取与 OpenAI 错误信封都已经存在；相比
DashScope Native 转换，它不需要响应事件重编码；相比 Realtime 路径，它不需要先建设
WebSocket 与音频重采样。

官方契约依据：

- OpenAI Chat Completions 用 `web_search_options` 表达 Chat 搜索选项：
  <https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create>
- DashScope Compatible Chat 用顶层 `enable_search: true` 开启搜索，且 Compatible
  协议不返回搜索来源：
  <https://www.alibabacloud.com/help/en/model-studio/web-search>
- DashScope Compatible 的结构化输出能力依模型而异，JSON Schema 严格模式只由
  部分模型提供：
  <https://www.alibabacloud.com/help/en/model-studio/qwen-structured-output>

## 决策

采用**线格式兼容直通**，不新增 Canonical 出站编码器。

1. 入站仍由 `openaichat.Decode` 严格解码为 Canonical。Canonical 只服务请求校验、
   内联负载限制、能力识别与矩阵裁决。
2. `dashscope.compatible` 出站适配器以客户端原始 JSON 为基准，仅做必要 patch：
   改写逻辑模型名；用网关凭据覆盖鉴权；把 `web_search_options` 映射成
   `enable_search: true`。
3. 其余请求字段原样保留。尤其不能经 Canonical 往返，因为当前 IR 不承载 `n`、
   presence/frequency penalty、logprobs 等全部 Chat 参数，重编码会制造静默损失。
4. 上游响应 JSON/SSE 原样返回 Chat 客户端，复用现有 Chat usage 抽取、限流头转发、
   首字节跟踪与流内中断处理。
5. 路径不调用 `MarkHomogeneous`。wire-compatible 只说明不需要重编码，不能推导为
   语义零损失；`X-Omugw-Degraded` 仍由矩阵生成。

## 请求与响应流程

```text
POST /v1/chat/completions
  -> openaichat.Decode(raw)
       - 严格校验 Chat wire
       - 识别能力与内联负载
  -> Router.Resolve(model)
  -> Matrix.BestOutbound(Inbound{openai.chat, /v1/chat/completions}, candidates, caps)
       - REJECT / 未兑现能力在上游调用前失败
       - DEGRADE 产出 X-Omugw-Degraded
  -> dashscope.compatible Provider
       - model -> target.UpstreamModel
       - Authorization -> 网关凭据
       - web_search_options -> enable_search: true
       - 其余 JSON 字段保持
  -> DashScope Compatible /v1/chat/completions
  -> 现有 relayJSON / relayStream 原样返回
```

不改变 Gateway 的 failover 规则：只有下游首字节之前允许换凭据或 Provider；开始回写
后发生中断，只能发送终止错误事件并把 usage 标为不可用。

## 能力识别

当前 Chat 解码器没有把两项已声明能力报告给矩阵，投放前必须补齐：

- `parallel_tool_calls: true` -> `CapParallelToolCalls`
- 非空 `web_search_options` -> `CapWebSearch`

`parallel_tool_calls` 仍可保存在 OpenAI Extension 中，但能力识别不能依赖出站读取
Extension；`web_search_options` 的映射直接基于原始 JSON 完成，不把 Provider 专属参数
塞进 Canonical。

缺省或显式 false 的并行工具调用不报告 `CapParallelToolCalls`。不存在
`web_search_options` 时不报告搜索能力，也不得向 DashScope 注入 `enable_search`。

## Web Search 映射

OpenAI Chat 的 `web_search_options` 是一个带参数对象；DashScope Compatible Chat 只用
`enable_search` 开关启用搜索。适配器按以下规则转换：

1. 请求不存在 `web_search_options`：不写 `enable_search`。
2. 请求存在非空 `web_search_options`：删除该字段，写入 `enable_search: true`。
3. `search_context_size`、`user_location` 等选项不向 DashScope 猜测映射。
4. DashScope Compatible 不返回搜索来源；网关不伪造来源，也不从文本反推来源。

因此 `CapWebSearch` 保持 `DEGRADE`。降级说明必须明确丢失搜索上下文大小、用户位置
和来源列表，而不能只笼统地说“参数不兼容”。fixture 必须断言上游收到
`enable_search: true` 且不再收到 `web_search_options`。

## 结构化输出

`response_format` 原样转发，不降成提示词，不主动改写 `json_schema`。DashScope
Compatible 当前仅部分模型保证严格 JSON Schema；路径级矩阵不能根据某个具体模型
承诺全局 strict 保证，所以 `CapStructuredOutput` 保持 `DEGRADE`。

降级表示**可移植保证变弱**，不是网关主动删除 schema。支持该能力的上游模型仍可
完整执行原请求；不支持的模型返回自身协议化错误，由现有 OpenAI 错误解码处理。

## 兑现范围

在 `EndpointOpenAIChat` 上兑现 9 项能力：

| 能力 | 处置 | 实现证据 |
|---|---|---|
| `text_generation` | PASSTHROUGH | 基础同步 fixture |
| `streaming` | PASSTHROUGH | SSE + usage fixture |
| `tool_calling` | PASSTHROUGH | 工具声明与工具调用 fixture |
| `parallel_tool_calls` | PASSTHROUGH | 显式 true 的能力识别与 fixture |
| `structured_output` | DEGRADE | 同名 fixture + 降级头 |
| `reasoning` | PASSTHROUGH | `reasoning_effort` 原样保留 fixture |
| `vision_input` | PASSTHROUGH | image URL / data URI fixture |
| `audio_input` | PASSTHROUGH | 内联音频 fixture |
| `web_search` | DEGRADE | 同名 fixture + 定点映射 + 降级头 |

`file_input` 与 `audio_output` 维持 `REJECT`，在 Provider 调用前返回 422。它们不进入
兑现集合，也不存在“等待实现后自动开放”的含义。

投放后设计分仍为 0.727；当前可用分按这扇门的兑现集合计算，不把 REJECT 误算成
未投放。路径仍不是 homogeneous，选路顺序继续服从全局 OutboundPreference。

## Provider 边界

新增或收窄一个只负责 OpenAI Chat compatible wire 的出站适配器。它实现现有
`provider.Provider`，输入仍同时拿到 Raw 与 Canonical，但该路径只消费 Raw；Canonical
已在 Gateway 完成能力裁决，不在 Provider 内重复验证。

适配器职责：

- 生成 `/v1/chat/completions` POST 请求；
- patch model 与 Web Search；
- 设置 `Content-Type`、`Accept`、Bearer 鉴权；
- 调用 `httpx.Client`；
- 用 `openaiwire.DecodeError` 解码非 2xx；
- 成功时返回原始 `*httpx.Response`。

不把 DashScope Compatible 塞进“同源”概念。可以复用 passthrough 内部的 HTTP 与
错误处理结构，但公共命名与注释必须允许“wire-compatible、语义异构”这一事实，避免
以后有人因复用 raw body 就误加 `MarkHomogeneous`。

## 错误处理

- 入站 JSON、未知字段、非法工具或媒体 -> OpenAI `400` 信封。
- 路径未投放或能力未兑现 -> `501`，不调用上游。
- `file_input` / `audio_output` -> `422`，带矩阵 note，不调用上游。
- DashScope Compatible 非 2xx -> `openaiwire.DecodeError`，保留 Retry-After 与限流信息。
- `web_search_options` 形态非法 -> 入站 `400`，不得默认为开启搜索。
- 上游首字节前的 retry/failover 与现状相同；首字节后禁止重试。

## 测试设计

### 协议单测

- `web_search_options` 非空时报告 `CapWebSearch`。
- `parallel_tool_calls: true` 报告 `CapParallelToolCalls`；false/缺省不报告。
- 非法 `web_search_options` 被严格解码拒绝。

### Provider 单测

- model、Authorization、path、Accept 正确。
- 无搜索时不注入 `enable_search`。
- 搜索时删除 `web_search_options` 并写入 `enable_search: true`。
- 除上述 patch 外，`n`、penalty、logprobs、tools、stream options、多模态内容等
  原始 JSON 语义保持不变。
- OpenAI 风格错误、Retry-After 与限流头正确分类。

### 路径 fixture 与 golden

新增 `testdata/routes/openai.chat__dashscope.compatible/`：

| fixture | 证明 |
|---|---|
| `basic.json` | text_generation、模型与鉴权改写、同步 usage |
| `streaming.json` | SSE、分帧、最终 usage、`[DONE]` |
| `tool_calling.json` | 工具声明、工具结果、工具调用响应 |
| `parallel_tool_calls.json` | 并行能力识别与 raw 保留 |
| `structured_output.json` | response_format 原样保留、降级头 |
| `reasoning.json` | reasoning_effort 原样保留 |
| `vision_input.json` | image URL / data URI 与内联大小边界 |
| `audio_input.json` | 内联音频与 raw 保留 |
| `web_search.json` | options -> enable_search 映射、丢失项可见 |

Conformance harness 必须同时断言客户端响应 golden 与上游实际收到的 method、path、
鉴权、请求 JSON。只比客户端响应会漏掉“Provider 根本没做映射、fixture 仍返回成功”
这种假绿。

### 负例

- file input 与 audio output 均返回 422，fake upstream 调用次数为零。
- 路由只包含尚未实现候选时维持 501。
- 搜索映射不能覆盖客户端已提交的无关字段。

## 投放与文档

实现完成后必须同步：

1. `rules_phase1.go` 的端点级 `Redeem`；
2. `TestImplementedRoutesAreExplicit` 路径白名单；
3. `TestRedeemedCapabilitiesAreExplicit` 的“路径 @ 端点”能力白名单；
4. Gateway Provider 工厂装配；
5. 路径 fixture、golden 与 conformance 回放；
6. `make matrix-update` 生成的降级矩阵文档；
7. README 与相邻 AGENTS 知识库中的实现状态。

## 非目标

- `openai.responses -> dashscope.compatible`；它在本路径稳定后另行派生。
- `openai.chat -> dashscope.native` 或任何 Native 请求/响应转换。
- WebSocket / Realtime / 音频重采样。
- 新增 Canonical 字段来承载 Provider 专属 Web Search 选项。
- 搜索来源伪造、自动补 citation 或提示词改写。
- 跨 Provider 文件下载、上传或 FileRef 搬运。
- 真实凭据进入离线测试或 CI。

## 完成标准

- 9 项能力在 `EndpointOpenAIChat` 上有逐项 fixture 证据并被显式兑现。
- Web Search 的参数损失通过矩阵 note、响应头和 fixture 可见。
- 除明确 patch 外，Chat 原始请求字段保持；响应 JSON/SSE 不经重编码。
- file input 与 audio output 在触达上游前稳定返回 422。
- `make check`、`make test-race`、`go build ./...`、相关 LSP 诊断全部通过。
- 本地 fake upstream 的真实 HTTP 回放通过；真实 DashScope smoke 可选，不作为 CI 前提。
