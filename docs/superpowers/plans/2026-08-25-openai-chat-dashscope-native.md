# OpenAI Chat → DashScope Native 路径投放实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 投放 `openai.chat → dashscope.native` 异构路径（第五条通车路径，也是首条请求与响应都必须完整重编码的路径）：新建 `internal/provider/dashscopenative` Composite Provider，在同一 `provider.Provider` 接口后隐藏「Native 入站复用 passthrough」与「Chat 入站走 translator」两种实现；在 `/v1/chat/completions` 门上显式兑现 9 项能力（6 PASS + 3 DEGRADE），`file_input` / `audio_output` 维持 REJECT 422，门的设计分与可用分均为 `(6 + 0.5×3) / 11 = 0.682`，全程不新增 Capability、不新增依赖、不读 `Extensions` 做异构翻译。

**Architecture:** 入站仍由 `openaichat.Decode` 严格解码为 Canonical；解码器补 `minimal`/`none` 两个推理档位，并为异构出站新增两样东西——`stream_options` 严格子解码（只服务网关自身 usage chunk 决策，不进出站请求）与一份具名、严格解码的 Chat wire 投影（补齐 Canonical 尚未承载的采样/输出选项，供 translator 读取与做 422 判定）。出站新建 `internal/provider/dashscopenative` 包：Native 入站原样交给既有 passthrough；Chat 入站进入 translator——非流式在 `Provider.Call` 返回前完整读取 Native body、经 Canonical 转成 `chat.completion`；流式由同步 transforming `io.ReadCloser` 按需把 Native `event:result` 帧编成 Chat SSE，并在 `Call` 内预读首帧以保住首字节前的 failover。Gateway 只负责构造入站坐标、按媒体过滤 Native 候选、注入 DashScope 专用 usage 回调、转发已换成入站协议 wire 的响应。矩阵在 `EndpointOpenAIChat` 上以 `Redeem` 兑现九项能力，两份显式白名单与生成文档同步更新。

**Tech Stack:** Go 1.25（仅现有三个直接依赖 `yaml.v3` / `prometheus client` / `go-cmp`，不新增），标准库 `testing` + `internal/testkit` 离线回放 + golden，Make 目标（`test` / `test-race` / `matrix` / `matrix-update` / `check` / `smoke`）。真实录制与真实 smoke 是硬性投放门槛，需要真实 DashScope 凭据；离线 CI 只回放脱敏后的真实录制。

**Spec:** [`docs/superpowers/specs/2026-08-22-openai-chat-dashscope-native-design.md`](../specs/2026-08-22-openai-chat-dashscope-native-design.md)（状态「已批准，待实现」，下文简称「设计文档」）。官方事实依据见 [`docs/research/2026-08-22-dashscope-native-text-generation-contract.md`](../../research/2026-08-22-dashscope-native-text-generation-contract.md)（下文简称「契约调研」）。本计划是设计文档的决策完整展开；执行中若发现计划与设计文档冲突，以设计文档为准并停下报告，不得自行改设计。

---

## 前置约定（每个任务都适用）

1. **语言与注释**：代码注释与文档一律中文，写「防的是什么」而不是「做了什么」，沿用仓库既有风格（参见 `internal/provider/passthrough/passthrough.go`、`internal/degrade/matrix.go` 的现有注释）。
2. **严格 TDD**：先写失败测试（RED），运行并确认它因**正确的原因**失败（符号未定义、断言不满足，而不是拼写错误或导入缺失），再写最小实现（GREEN），再跑聚焦测试与更大范围测试。每个行为任务都按 RED → 验证 RED → GREEN → 聚焦验证 → 更广验证 → 提交 的顺序走。命令与预期输出在每个步骤里给出。纯数据任务（fixture、golden）与纯生成任务（矩阵文档）按各自的验证方式执行。
3. **每个提交必须绿**：任何提交落地前，`go build ./...` 与受影响包的 `go test` 必须通过（除非该步骤明确标注 RED 中间态且同一任务内收敛）。
4. **提交纪律**：每条 `git` 命令以 `GIT_MASTER=1` 前缀执行。每个提交带 Sisyphus 署名脚注与 Co-authored-by trailer，模板：

   ```bash
   GIT_MASTER=1 git add <files>
   GIT_MASTER=1 git commit -m "<任务给出的提交信息>" \
     -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
     -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
   ```

   **本计划文件本身（规划任务）不产生任何生产代码提交**；下列 22 个提交全部是未来执行步骤。规划阶段只允许写入本计划文件，不得 `git commit`。
5. **不许推送**：全程不得 `git push`。保持本地状态。
6. **golden 纪律**：重写 golden 后必须逐份人工审阅 diff，确认内容正是预期语义，再提交。本计划只允许用定向 `-update` 重写本路径的 golden，不得 `make golden-update` 全量重写。
7. **尺寸纪律**：`openaichat/decode.go`、`dashscopenative/decode.go`、`gateway/handler.go`、`gateway/relay.go`、`canonical/stream.go` 已接近或超过模块尺寸警戒线，新职责一律进**聚焦的新文件**，不继续堆进这些文件；允许的只是下面逐任务点名的「行级集成编辑」。任何新建实现文件控制在 250 纯代码行以内，超了就在同任务内拆分。
8. **范围纪律**：不做设计文档「非目标」一节列出的任何事——不碰 `openai.responses → dashscope.native`、不碰 Realtime/音频输出/视频输入/通用 file 输入、不从模型名或媒体存在性反推 Native 门、不代下载媒体、不给模型专属参数加隐式默认/兼容 shim/自动重试、不把 DashScope usage 回调推广成通用 `UsageSource`、不在路径级承诺某个具体模型才支持的组合。
9. **反模式禁令**：不把 OpenAI 格式当内部总线；不在异构路径读 `Extensions` 猜语义；不在首字节后重试或 failover；不让 `Usage` 走零值路径；不新增依赖、不新增 Capability、不改 `CONTEXT.md`、不发明兼容 shim。

## 十四个已采纳的绑定决策（执行中不得推翻）

1. **Composite Provider**：`dashscope.native` 的出站适配器只有一个，仍满足唯一 `provider.Provider` 接口；`Inbound == dashscope.native` 走 passthrough，`Inbound == openai.chat` 走 translator，其他入站坐标 fail-closed。Gateway 不承担协议转换。
2. **`provider.Request` 迁移是原子的**：新增 `Inbound degrade.Inbound`、删除 `Path`、新增请求级 `OnDashScopeUsage func(canonical.Usage)`，三者在同一提交完成，不做双字段 shim；同步改 `passthrough` 与 `dashscopecompat` 两个已通车适配器的取路径方式，以及 gateway 的注入点。
3. **`reasoning_effort:none` 是 Canonical 语义**：新增 `canonical.EffortNone` 承载官方 `none → enable_thinking:false`，`UsedCapabilities` 对它**不**报告 `CapReasoning`；不做任何兼容 shim。这是跨路径的 Canonical 决定，不是本路径私有的补丁。
4. **路由中立的严格投影落在 `internal/protocol/openaichat`**：`stream_options` 严格子解码器与具名 Chat 投影都属于 openaichat；**Native 专属的 422 规则**（无落点字段、非流式 + 推理、`n>1` + tools、`n>1` + 推理、`audio`）落在 `internal/provider/dashscopenative`。
5. **Native 门是部署事实**：由 `config.TargetSpec` 与 `router.Target` 的 `NativeEndpoint`（YAML 名 `native_endpoint`）声明，枚举仅 `text-generation` / `multimodal-generation`；不按模型名或本次请求是否含媒体推断。Provider 严格用它选 `TextGenerationPath` / `MultimodalGenerationPath`。
6. **非流式在 `Call` 内完成转换**：读取、解析、编码失败都发生在 `Provider.Call` 返回前，下游尚无首字节，现有凭据/Provider failover 仍然有效。
7. **流式用同步 transforming `io.ReadCloser`**：不用 `io.Pipe`、不起 goroutine、无跨请求状态；`Read` 永不返回 `(0, nil)`，返回的错误必须是 `*canonical.Error`；`Close` 立即关闭 Native body。
8. **流式首帧在 `Call` 内预读**：预读并解码 Native 首帧、缓冲起来交给 transforming reader 重放，让「200 但首帧畸形/是错误帧」仍落在首字节前、可 failover。代价是几毫秒 TTFT。
9. **多候选不新增 Capability**：`n > 1` 是输出基数不是能力，`AllCapabilities()` 保持不变；它搭载在 `text_generation` 与 `streaming` 两项上兑现，因此这两项的证据清单里各加一份 `n=2` fixture（非流式一份、流式一份）。候选序号取 `output.choices` 数组下标；**不得**经过共享的 `canonical.Accumulator`。
10. **DashScope usage 回调是专用耦合**：`OnDashScopeUsage` 只在 `in.kind == ProviderDashScopeNative` 分支注入，其他 Provider 必须为 nil；不改造成通用 `UsageSource`，不把 Provider 身份判断散到 relay。
11. **usage 优先级**：relay 返回后按「① Chat 响应已提供 authoritative usage → 用 relay 结果；② relay **无错误返回**且未取得 usage → 用回调末值；③ 其余 → `FidelityUnavailable`」。第 ② 条必须限定在 relay 无错误返回上——流中断路径上 usage 已被故意抹成 unavailable，不得用回调值捡回来。
12. **`parallel_tool_calls` 缺省注入 `true`**：客户端未提交该字段时，网关按 OpenAI 默认显式注入 `true`（Native 默认 `false`，不注入会静默变成串行）；处置仍为 DEGRADE，note 同时写明「模型支持面与并行行为不具路径级全局保证」与「缺省注入 true」两件事。
13. **`stream_options` 验证但不剥离**：严格子解码发生在 `openaichat.Decode`（对所有 Chat 入站生效，含 Compatible），但 Compatible 路径保持原始字节不剥离；只有 Native 路径消费它（决定是否发 usage chunk）且不转发上游。
14. **真实录制与真实 smoke 是硬门槛**：首批 Native fixture 必须由真实 DashScope 录制，组合 smoke 必须在真实模型上通过；**不得**用 httptest 构造假录制充数，也不得把多份互不相容的模型证据拼成「可组合」。若执行时没有真实凭据，相应任务标注 **BLOCKED（缺凭据）** 并停在那一步，不得伪造。

## 关键路径与依赖波次

任务按波次组织：**波次 0（地基）→ 波次 1（编解码）→ 波次 2（Composite Provider）→ 波次 3（Gateway）→ 波次 4（证据与投放）**。波次内任务彼此独立可并行，波次间严格顺序。

- **波次 0**：任务 1（`EffortNone`）、2（`minimal`/`none` 解码）、3（`provider.Request` 迁移）、4（`stream_options` + 具名投影）、5（`native_endpoint` 配置与路由）。任务 2 依赖任务 1（要引用 `EffortNone`）；任务 4 的投影被波次 2 的 translator 消费；任务 3 被波次 2/3 消费；任务 5 被波次 2 的 Provider 路径选择与波次 3 的过滤消费。
- **波次 1**：任务 6（Native 请求编码）、7（Native 响应/`event:result` 解码）、8（Chat 非流式编码）、9（Chat SSE 编码）。6 消费任务 4 的投影与任务 5 的门；7/8/9 互相独立，但都被波次 2 的 translator 串起来。
- **波次 2**：任务 10（Composite 骨架的 Native 分派）、11（入站 422）、12（可独立测试的非流式 translator）、13（流式 transform reader、多候选与 Chat 分支原子接线）。10 先行；11 依赖投影与 Canonical；12 依赖 10/11 与非流式 codec；13 最后消费 10–12 与流式 codec，待两种 translator 都是真实实现后才把 Chat 接进 `Provider.Call`。
- **波次 3**：任务 14（Build 装配与 harness）、15（目标过滤）、16（usage 回调与优先级）。依赖波次 2 的 Provider。顺序是**先装配后行为**：任务 14 提供的 harness 是任务 15/16/19 的测试基建，必须先落地。
- **波次 4**：任务 17（真实录制 + fixture）、18（兑现 + 白名单 + 矩阵文档）、19（conformance + 负例 + golden）、20（真实 smoke）、21（`tests/smoke` 目录）、22（文档与 provenance）。

**ADR-0001 窗口**：任务 18 的 `Redeem` 一落地，这条路径在矩阵里就是「已兑现」，而证明兑现的 conformance 回放到任务 19 才提交。**从任务 18 开始到任务 19 全绿为止，不得中途停下**——任务 17 的真实 fixture 已在任务 18 之前落地（先有证据，后有兑现），任务 19 紧随其后补齐回放与负例。窗口内任何已提交状态都是绿的（fixture 与白名单都已就位），但兑现的合法性靠随后立即落地的回放证据。

**真实凭据门槛**：任务 17（真实录制）与任务 20（真实 smoke）需要真实 DashScope API Key。若执行环境没有凭据，这两个任务标注 **BLOCKED（缺凭据）**，其后的任务 18/19（依赖真实 fixture）也随之挂起；**不得**用 httptest 伪造录制来解锁。波次 0–3 的全部离线任务不依赖凭据，可先行完成。

## 范围与文件地图

| 文件 | 动作 | 责任 | 任务 |
|---|---|---|---|
| `internal/canonical/request.go` | 修改 | 新增 `EffortNone`；`UsedCapabilities` 对 `EffortNone` 不报告 `CapReasoning` | 1 |
| `internal/canonical/canonical_test.go` | 修改 | `EffortNone` 与能力守卫测试 | 1 |
| `internal/protocol/openaichat/decode.go` | 修改（行级集成） | `decodeReasoning` 接受 `minimal` 与 `none` | 2 |
| `internal/protocol/openaichat/decode_test.go` | 修改 | `minimal`/`none` 解码与能力断言 | 2 |
| `internal/provider/provider.go` | 修改 | `Request` 增 `Inbound`、删 `Path`、增 `OnDashScopeUsage` | 3 |
| `internal/provider/passthrough/passthrough.go` | 修改 | `pathFor` 改从 `Inbound.Endpoint` 取路径 | 3 |
| `internal/provider/dashscopecompat/dashscopecompat.go` | 修改 | `pathFor` 改从 `Inbound.Endpoint` 取路径 | 3 |
| `internal/gateway/handler.go` | 修改（行级集成） | `dispatch` 注入 `Inbound` 取代 `Path` | 3 |
| `internal/provider/providertest/harness.go` | 修改 | 契约套件构造 `Request` 用 `Inbound` | 3 |
| `internal/gateway/harness_test.go` | 修改 | 哨兵注释与 `Inbound` 注入语义同步 | 3 |
| `internal/protocol/openaichat/wire.go` | 修改 | `StreamOptions` 结构体 | 4 |
| `internal/protocol/openaichat/projection.go` | 新建 | `stream_options` 严格子解码 + 具名 Chat 投影 | 4 |
| `internal/protocol/openaichat/projection_test.go` | 新建 | 严格子解码与投影测试 | 4 |
| `internal/config/gateway.go` | 修改 | `TargetSpec.NativeEndpoint` + 启动期校验 | 5 |
| `internal/config/gateway_test.go` | 修改 | `native_endpoint` 校验测试 | 5 |
| `internal/router/router.go` | 修改 | `Target.NativeEndpoint` + `validate` | 5 |
| `internal/router/router_test.go` | 修改 | `NativeEndpoint` 校验测试 | 5 |
| `internal/protocol/dashscopenative/encode_request.go` | 新建 | Native 出站请求编码（两扇门 + 全参数映射） | 6 |
| `internal/protocol/dashscopenative/encode_request_test.go` | 新建 | 请求信封与参数映射测试 | 6 |
| `internal/protocol/dashscopenative/result.go` | 新建 | Native 成功响应与 `event:result` 解码 | 7 |
| `internal/protocol/dashscopenative/result_test.go` | 新建 | 两种 null、usage、request_id、tool_calls 等解码测试 | 7 |
| `internal/protocol/openaichat/encode_response.go` | 新建 | Chat 非流式成功响应（`chat.completion`）编码 | 8 |
| `internal/protocol/openaichat/encode_response_test.go` | 新建 | 稳定 id/model/created、choices、usage 编码测试 | 8 |
| `internal/protocol/openaichat/encode_stream.go` | 新建 | Chat SSE chunk（`chat.completion.chunk`）编码 | 9 |
| `internal/protocol/openaichat/encode_stream_test.go` | 新建 | 跨帧工具参数、usage chunk、`[DONE]` 编码测试 | 9 |
| `internal/provider/dashscopenative/provider.go` | 新建/修改 | Composite 骨架、Native 分派、fail-closed；两种 translator 就绪后接入 Chat | 10、13 |
| `internal/provider/dashscopenative/provider_test.go` | 新建/修改 | Native/未知入站分派；最终 Chat 分派测试 | 10、13 |
| `internal/provider/dashscopenative/reject.go` | 新建 | 入站 422 规则（点名字段、上游零调用） | 11 |
| `internal/provider/dashscopenative/reject_test.go` | 新建 | 逐条 422 与 `none` 正例测试 | 11 |
| `internal/provider/dashscopenative/nonstream.go` | 新建 | 非流式 translator（`Call` 内完整转换 + usage 回调一次） | 12 |
| `internal/provider/dashscopenative/nonstream_test.go` | 新建 | 非流式转换、failover、回调测试 | 12 |
| `internal/provider/dashscopenative/stream.go` | 新建 | 流式 transforming reader、首帧预读、Read 契约 | 13 |
| `internal/provider/dashscopenative/candidates.go` | 新建 | 多候选（`n>1`）状态与编码 | 13 |
| `internal/provider/dashscopenative/stream_test.go` | 新建 | Read 契约、首帧、多候选、`[DONE]` 测试 | 13 |
| `internal/gateway/build.go` | 修改 | 装配 Composite Provider、更新未实现族名单、`NativeEndpoint` 接线 | 14 |
| `internal/gateway/build_test.go` | 修改 | 装配成功与错误名单测试 | 14 |
| `internal/gateway/harness_test.go` | 修改 | `newChatDSNativeHarness` 与工厂 | 14 |
| `internal/gateway/filter.go` | 新建 | Native 候选的媒体过滤 | 15 |
| `internal/gateway/filter_test.go` | 新建 | 过滤与零候选 422 测试 | 15 |
| `internal/gateway/handler.go` | 修改（行级集成） | 过滤接入点；`OnDashScopeUsage` 注入；usage 优先级 | 15、16 |
| `internal/gateway/usage_test.go` | 新建 | usage 回调注入与优先级测试 | 16 |
| `testdata/routes/openai.chat__dashscope.native/*.json` | 新建 | 九项能力 + 组合 + `n=2`×2 + 缺省注入 fixture（真实录制） | 17 |
| `internal/degrade/rules_phase1.go` | 修改 | 修正 `parallel_tool_calls` note + `Redeem(EndpointOpenAIChat, 九项)` | 18 |
| `internal/degrade/matrix_test.go` | 修改 | 两份显式白名单 | 18 |
| `docs/degradation-matrix.md` | 生成 | `make matrix-update` 重新生成（禁手改） | 18 |
| `internal/gateway/chat_dsnative_conformance_test.go` | 新建 | fixture 回放 + 上游请求与降级头独立断言 | 19 |
| `internal/gateway/chat_dsnative_negative_test.go` | 新建 | 422 / 501 / 400 负例，上游零调用 | 19 |
| `testdata/routes/openai.chat__dashscope.native/golden/*.txt` | 生成 | 定向 `-update` 生成后人工审阅 | 19 |
| `tests/smoke/`（新建目录 + 冒烟用例） | 新建 | `make smoke` 指向的真实端到端冒烟 | 20、21 |
| `docs/provenance.yaml`、`README.md`、根 `AGENTS.md`、相关包 `AGENTS.md` | 修改 | 实现状态、语义边界、模块来源同步 | 22 |

## 提交总览（22 个提交，全部本地，不推送；规划任务本身不提交）

| # | 提交信息（中文 plain，仓库风格） | 主要文件 | 任务 |
|---|---|---|---|
| 1 | `Canonical 新增推理档位 none：显式关思考不报告推理能力` | request.go、canonical_test.go | 1 |
| 2 | `Chat 解码接受 minimal 与 none 推理档位` | decode.go、decode_test.go | 2 |
| 3 | `provider.Request 用入站坐标取代裸路径：原子迁移 Inbound 并接入 usage 回调位` | provider.go、passthrough.go、dashscopecompat.go、handler.go、providertest/harness.go、harness_test.go | 3 |
| 4 | `Chat 严格投影：stream_options 子解码与具名出站投影` | wire.go、projection.go、projection_test.go | 4 |
| 5 | `native_endpoint 门声明：配置与路由单一事实来源` | config/gateway.go、config/gateway_test.go、router.go、router_test.go | 5 |
| 6 | `DashScope Native 出站请求编码：两扇门信封与全参数映射` | encode_request.go（+测试） | 6 |
| 7 | `DashScope Native 成功响应解码：两种 null 与累计 usage` | result.go（+测试） | 7 |
| 8 | `Chat 非流式响应编码：chat.completion` | encode_response.go（+测试） | 8 |
| 9 | `Chat SSE chunk 编码：跨帧工具参数与 usage 收尾` | encode_stream.go（+测试） | 9 |
| 10 | `DashScope Native Composite Provider：入站分派与 fail-closed` | provider.go（+测试） | 10 |
| 11 | `Chat 到 Native 入站 422：无落点字段与硬约束逐条点名` | reject.go（+测试） | 11 |
| 12 | `Chat 到 Native 非流式转换：Call 内完成保住 failover` | nonstream.go（+测试） | 12 |
| 13 | `Chat 到 Native 流式转换：同步 transforming reader 与多候选` | stream.go、candidates.go（+测试） | 13 |
| 14 | `Build 装配 dashscope.native Composite：未实现族名单同步` | build.go、build_test.go、harness_test.go | 14 |
| 15 | `网关按媒体过滤 Native 候选：无门可承载即 422` | filter.go（+测试）、handler.go | 15 |
| 16 | `DashScope 专用 usage 回调：注入与 relay 优先级` | handler.go、usage_test.go | 16 |
| 17 | `Chat 到 DashScope Native 真实录制 fixture：九项能力与多候选` | testdata/routes/openai.chat__dashscope.native/*.json | 17 |
| 18 | `兑现 openai.chat 到 dashscope.native 九项能力：白名单与矩阵文档同步` | rules_phase1.go、matrix_test.go、degradation-matrix.md | 18 |
| 19 | `Chat 到 DashScope Native 一致性回放与负例：上游请求逐条对账` | conformance/negative 测试 + golden | 19 |
| 20 | `tests/smoke 冒烟骨架：真实上游端到端` | tests/smoke/ | 20 |
| 21 | `Chat 到 DashScope Native 真实 smoke 用例` | tests/smoke/ | 21 |
| 22 | `知识库跟上第五条通车路径：首条完整重编码异构路径` | provenance.yaml、README.md、AGENTS.md | 22 |

提交 3 含六个文件的理由：`Path → Inbound` 是原子迁移，删字段与改全部消费方必须在同一提交，否则任一中间提交编译红（这正是「拆开提交会破坏编译」的合法合并情形）。提交 5 含 config 与 router 两个包的理由：`native_endpoint` 的校验规则（`kind == dashscope.native` 必填、其他 kind 必空）横跨两包的同一字段，配置侧校验需要路由侧字段同时存在才能编译，属同一原子单元。提交 18 含三个文件的理由：`Redeem` 与两份白名单由 CI 互相校验（只改一侧立即失败），矩阵文档由 `TestDegradationMatrixDocIsCurrent` 强制与代码同步——拆开会让任一中间提交测试红。其余提交均满足「文件数 ÷ 3」下限。

---

## 波次 0：地基

### 任务 1：Canonical 新增 `EffortNone`，且不被报告为推理能力

**依赖**：无。
**文件**：修改 `internal/canonical/request.go`、`internal/canonical/canonical_test.go`。

- [ ] **步骤 1.1：写失败测试（RED）**

在 `internal/canonical/canonical_test.go` 末尾追加：

```go
// TestEffortNoneDoesNotReportReasoning 钉死「显式关闭思考」不等于「使用推理」。
//
// none 是客户端主动关掉思考。若把它报告成 CapReasoning，本路径「非流式 + 推理
// 即 422」与「n>1 + 推理即 422」两条规则就会把一个合法请求拒掉——守卫必须长在
// UsedCapabilities 的推理判定上，而不是散在每个调用点。
func TestEffortNoneDoesNotReportReasoning(t *testing.T) {
	r := Request{
		Model:     "m",
		Reasoning: &Reasoning{Effort: EffortNone},
		Messages:  []Message{{Role: RoleUser, Parts: []Part{Text("hi")}}},
	}
	for _, c := range r.UsedCapabilities() {
		if c == CapReasoning {
			t.Fatalf("EffortNone 不应报告 %q", CapReasoning)
		}
	}
}

// TestEffortNonNoneStillReportsReasoning 防的是守卫写过头：低档位照常报告。
func TestEffortNonNoneStillReportsReasoning(t *testing.T) {
	r := Request{
		Model:     "m",
		Reasoning: &Reasoning{Effort: EffortLow},
		Messages:  []Message{{Role: RoleUser, Parts: []Part{Text("hi")}}},
	}
	found := false
	for _, c := range r.UsedCapabilities() {
		if c == CapReasoning {
			found = true
		}
	}
	if !found {
		t.Fatalf("EffortLow 应报告 %q", CapReasoning)
	}
}
```

运行：

```bash
go test ./internal/canonical/ -run 'TestEffortNone|TestEffortNonNone' -v
```

预期：**编译失败**，`undefined: EffortNone`——这是正确的 RED（常量尚不存在）。

- [ ] **步骤 1.2：实现常量与守卫（GREEN）**

在 `internal/canonical/request.go` 的 `ReasoningEffort` 常量块（`EffortMinimal` 所在块）追加：

```go
	// EffortNone 是显式关闭思考。它不是「最低档的推理」，而是「不要推理」——
	// 因此 UsedCapabilities 不得把它报告成 CapReasoning。
	EffortNone ReasoningEffort = "none"
```

并修改 `UsedCapabilities` 中的推理判定（原为 `if r.Reasoning != nil { seen[CapReasoning] = true }`）：

```go
	// none 是显式关闭思考，不构成推理请求；只有真正开启档位才算使用推理。
	if r.Reasoning != nil && r.Reasoning.Effort != EffortNone {
		seen[CapReasoning] = true
	}
```

运行：

```bash
go test ./internal/canonical/ -run 'TestEffortNone|TestEffortNonNone' -v
```

预期：两条测试 **PASS**。

- [ ] **步骤 1.3：聚焦与更广验证**

```bash
go test ./internal/canonical/ -v
go build ./...
```

预期：canonical 包全绿；`go build ./...` 退出码 0。（此时 `openaichat.decodeReasoning` 还不认识 `none`，但那是任务 2 的事，canonical 包自身不受影响。）

- [ ] **步骤 1.4：提交**

```bash
GIT_MASTER=1 git add internal/canonical/request.go internal/canonical/canonical_test.go
GIT_MASTER=1 git commit -m "Canonical 新增推理档位 none：显式关思考不报告推理能力" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

### 任务 2：Chat 解码接受 `minimal` 与 `none` 推理档位

**依赖**：任务 1。
**文件**：修改 `internal/protocol/openaichat/decode.go`（仅 `decodeReasoning` 一处，行级集成）、`internal/protocol/openaichat/decode_test.go`。

- [ ] **步骤 2.1：写失败测试（RED）**

在 `internal/protocol/openaichat/decode_test.go` 末尾追加（沿用现有 `TestDecodeReportsReasoning` 的 helper 风格）：

```go
// TestDecodeReasoningAcceptsMinimalAndNone 补齐 IR 已有而解码器漏掉的两个档位。
//
// minimal 今天会被 400 拒掉，是解码器落后于 IR；none 承载官方的
// enable_thinking:false 语义。两者都该放行，且 none 不得报告 CapReasoning，
// 否则非流式请求会被本路径的 422 规则误伤。
func TestDecodeReasoningAcceptsMinimalAndNone(t *testing.T) {
	d, err := Decode([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
	  "reasoning_effort":"minimal"}`))
	if err != nil {
		t.Fatalf("minimal 应被接受: %v", err)
	}
	if d.Request.Reasoning == nil || d.Request.Reasoning.Effort != canonical.EffortMinimal {
		t.Fatalf("minimal 解码异常: %+v", d.Request.Reasoning)
	}

	d, err = Decode([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
	  "reasoning_effort":"none"}`))
	if err != nil {
		t.Fatalf("none 应被接受: %v", err)
	}
	if d.Request.Reasoning == nil || d.Request.Reasoning.Effort != canonical.EffortNone {
		t.Fatalf("none 解码异常: %+v", d.Request.Reasoning)
	}
	if hasCap(d.Capabilities(), canonical.CapReasoning) {
		t.Errorf("none 不应报告 reasoning: %v", d.Capabilities())
	}
}
```

运行：

```bash
go test ./internal/protocol/openaichat/ -run TestDecodeReasoningAcceptsMinimalAndNone -v
```

预期：**失败**——`minimal 应被接受` 或 `none 应被接受`（当前 `decodeReasoning` 只认 `low/medium/high`，会返回 400）。这是正确的 RED。

- [ ] **步骤 2.2：扩展 `decodeReasoning`（GREEN）**

修改 `internal/protocol/openaichat/decode.go` 的 `decodeReasoning`，把 switch 的接受集合扩成五档：

```go
	switch canonical.ReasoningEffort(*effort) {
	case canonical.EffortMinimal, canonical.EffortLow,
		canonical.EffortMedium, canonical.EffortHigh, canonical.EffortNone:
		return &canonical.Reasoning{Effort: canonical.ReasoningEffort(*effort)}, nil
	default:
		return nil, canonical.Newf(canonical.ClassBadRequest,
			"不支持的 reasoning_effort %q", *effort)
	}
```

运行：

```bash
go test ./internal/protocol/openaichat/ -run TestDecodeReasoningAcceptsMinimalAndNone -v
```

预期：**PASS**。

- [ ] **步骤 2.3：聚焦与更广验证**

```bash
go test ./internal/protocol/openaichat/ -v
go build ./...
```

预期：openaichat 包全绿（含既有 `TestDecodeReportsReasoning`），build 通过。

- [ ] **步骤 2.4：提交**

```bash
GIT_MASTER=1 git add internal/protocol/openaichat/decode.go internal/protocol/openaichat/decode_test.go
GIT_MASTER=1 git commit -m "Chat 解码接受 minimal 与 none 推理档位" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

### 任务 3：`provider.Request` 原子迁移——`Inbound` 取代 `Path`，接入 `OnDashScopeUsage`

**依赖**：无（与任务 1/2 独立，但建议在波次 0 早期做，后续波次都消费它）。
**文件**：修改 `internal/provider/provider.go`、`internal/provider/passthrough/passthrough.go`、`internal/provider/dashscopecompat/dashscopecompat.go`、`internal/gateway/handler.go`（行级集成）、`internal/provider/providertest/harness.go`、`internal/gateway/harness_test.go`（注释）。

> 这是**原子迁移**：删 `Path` 与改全部消费方必须在同一提交，任何中间态都编译红。先改测试与接口让它红，再一次改完所有消费方让它绿。

- [ ] **步骤 3.1：写失败测试（RED）——契约套件改用 `Inbound`**

修改 `internal/provider/providertest/harness.go`：把 `callOpts` 的 `path string` 字段改为 `inboundEndpoint degrade.Endpoint`，并在 `call` 里构造 `provider.Request` 时用 `Inbound: degrade.Inbound{Protocol: s.InboundProtocol, Endpoint: o.inboundEndpoint}` 取代 `Path: o.path`。由于 `Subject` 尚未声明入站协议，先在 `Subject` 增加 `InboundProtocol degrade.Protocol` 字段，`call` 据此填 `Inbound.Protocol`。`Subject.DefaultPath` 的语义不变（请求未带 `Inbound.Endpoint` 时应落到的端点），只更新注释措辞。

三份契约声明同步补 `InboundProtocol`：
- `internal/provider/passthrough/contract_test.go` 的 `passthrough/openai.compat`：`InboundProtocol: degrade.ProtoOpenAIResponses`；`passthrough/dashscope.native`：`InboundProtocol: degrade.ProtoDashScopeNative`。
- `internal/provider/dashscopecompat/contract_test.go` 的 `dashscopecompat`：`InboundProtocol: degrade.ProtoOpenAIChat`。

同时把 `passthrough` / `dashscopecompat` 的既有单测里凡是显式构造 `provider.Request{Path: ...}` 的地方改为 `Inbound: degrade.Inbound{Endpoint: ...}`。

运行：

```bash
go build ./...
```

预期：**编译失败**——`provider.Request` 还没有 `Inbound` 字段、仍有 `Path`。这是正确的 RED。

- [ ] **步骤 3.2：迁移接口与全部消费方（GREEN）**

`internal/provider/provider.go`：`Request` 结构体把 `Path string` 字段删除，新增两个字段（`Inbound` 放在 `Stream` 之后、`Header` 之前；`OnDashScopeUsage` 放在 `Header` 之后）：

```go
	// Inbound 钉住这次上游调用对应的入站坐标：从哪个协议进来、敲的哪扇门。
	//
	// 矩阵与 Provider 消费同一份入站坐标，避免把裸路径暗当协议标签。同源直通
	// 用它取上游端点路径（入站门即出站端点）；异构适配器用它分派实现。
	Inbound degrade.Inbound

	// OnDashScopeUsage 是 DashScope Native 专用的请求级用量回调。
	//
	// 只有 ProviderDashScopeNative 分支注入，其他 Provider 必须为 nil。Native
	// 每帧携带累计 usage，回调被同步调用、后值覆盖前值。这是已接受的 Provider
	// 专用耦合，刻意不推广成通用 usage seam。
	OnDashScopeUsage func(canonical.Usage)
```

`internal/provider/passthrough/passthrough.go`：`pathFor` 改为从 `Inbound.Endpoint` 取路径：

```go
// pathFor 优先用入站坐标携带的门路径，缺省退回装配时的默认路径。
//
// 同源直通下入站门与出站端点同路径，门路径随请求走；保留默认值是为了
// 不影响既有单上游的装配。
func (p *Provider) pathFor(req provider.Request) string {
	if req.Inbound.Endpoint != "" {
		return string(req.Inbound.Endpoint)
	}
	return p.path
}
```

`internal/provider/dashscopecompat/dashscopecompat.go`：`pathFor` 同样改从 `req.Inbound.Endpoint` 取，缺省退回 `ChatCompletionsPath`。

`internal/gateway/handler.go`：`dispatch` 里构造 `provider.Request` 处，把 `Path: h.in.upstreamPath(r),` 一行替换为：

```go
				Inbound: degrade.Inbound{
					Protocol: h.in.protocol,
					Endpoint: degrade.Endpoint(h.in.upstreamPath(r)),
				},
```

（这与 `serve` 里传给 `Matrix.BestOutbound` 的入站坐标是同一对值，保证矩阵与 Provider 看到同一坐标。）

`internal/gateway/harness_test.go`：把 `sentinelProviderPath` 的注释里 `provider.Request.Path` 字样改为 `provider.Request.Inbound.Endpoint`，语义不变（哨兵默认路径只有在 handler 真的注入了入站坐标时才会被覆盖）。

运行：

```bash
go build ./...
go test ./internal/provider/... ./internal/gateway/ -run 'Contract|Path|Sentinel|Planned' -v
```

预期：build 通过；相关测试 **PASS**。

- [ ] **步骤 3.3：聚焦与更广验证**

```bash
go test ./internal/provider/... ./internal/gateway/ -v
go build ./...
```

预期：provider 与 gateway 全包绿（含 passthrough / dashscopecompat / providertest 契约套件与既有 gateway 测试），build 通过。

- [ ] **步骤 3.4：提交**

```bash
GIT_MASTER=1 git add internal/provider/provider.go internal/provider/passthrough/passthrough.go \
  internal/provider/dashscopecompat/dashscopecompat.go internal/gateway/handler.go \
  internal/provider/providertest/harness.go internal/gateway/harness_test.go \
  internal/provider/passthrough/passthrough_test.go internal/provider/passthrough/contract_test.go \
  internal/provider/dashscopecompat/dashscopecompat_test.go internal/provider/dashscopecompat/contract_test.go
GIT_MASTER=1 git commit -m "provider.Request 用入站坐标取代裸路径：原子迁移 Inbound 并接入 usage 回调位" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

> 注：`OnDashScopeUsage` 此时只是加了字段位，尚无人注入、无人调用；注入在任务 16，调用在任务 12/13。字段先落地是因为它属于 `Request` 的形状，必须随原子迁移一起定型。

---

### 任务 4：Chat 严格投影——`stream_options` 子解码与具名出站投影

**依赖**：无。
**文件**：修改 `internal/protocol/openaichat/wire.go`；新建 `internal/protocol/openaichat/projection.go`、`internal/protocol/openaichat/projection_test.go`；`decode.go` 行级集成（接入 `stream_options` 子解码）。

> 两件事都属「入站边界向异构出站提供严格 Chat 视图」，且都落在 openaichat（路由中立）：`stream_options` 严格子解码（只服务网关 usage chunk 决策，不进出站请求）；具名投影补齐 Canonical 未承载的采样/输出选项，供 translator 读取与做 422 判定。**不得**读 `Extensions`。

- [ ] **步骤 4.1：写失败测试（RED）——`stream_options` 严格子解码**

新建 `internal/protocol/openaichat/projection_test.go`，先写 `stream_options` 的两条：

```go
// TestStreamOptionsRejectsUnknownSubfield 钉死严格子解码。
//
// stream_options 是 json.RawMessage，外层 DisallowUnknownFields 进不到子树；
// 若不补严格子解码器，未知子字段会被静默吞掉。按 web_search_options 的先例，
// 未知子字段必须 400。
func TestStreamOptionsRejectsUnknownSubfield(t *testing.T) {
	_, err := Decode([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
	  "stream":true,"stream_options":{"include_usage":true,"bogus":1}}`))
	if err == nil {
		t.Fatal("stream_options 未知子字段应被拒绝")
	}
	if canonical.AsError(err).Class != canonical.ClassBadRequest {
		t.Fatalf("应为 400，实际 %v", err)
	}
}

// TestStreamOptionsIncludeUsageIsDecoded 钉死 include_usage 被解出，供 usage chunk 决策。
func TestStreamOptionsIncludeUsageIsDecoded(t *testing.T) {
	d, err := Decode([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
	  "stream":true,"stream_options":{"include_usage":true}}`))
	if err != nil {
		t.Fatalf("合法 stream_options 应被接受: %v", err)
	}
	if !d.StreamOptionsIncludeUsage() {
		t.Fatal("include_usage=true 应被解出")
	}
}
```

运行：

```bash
go test ./internal/protocol/openaichat/ -run 'TestStreamOptions' -v
```

预期：**编译失败**（`d.StreamOptionsIncludeUsage` 未定义）或第一条测试**通过不了**（未知子字段当前不报错）。正确的 RED。

- [ ] **步骤 4.2：实现 `stream_options` 严格子解码（GREEN）**

`internal/protocol/openaichat/wire.go` 在 `WebSearchOptions` 之后新增：

```go
// StreamOptions 是 Chat 的流式选项。
//
// 它只服务网关自身的 usage chunk 决策，不进出站请求。外层严格模式管不到
// RawMessage 子树，未知子字段必须在入站 400，不能静默吞掉。
type StreamOptions struct {
	IncludeUsage       *bool `json:"include_usage,omitempty"`
	IncludeObfuscation *bool `json:"include_obfuscation,omitempty"`
}
```

`internal/protocol/openaichat/projection.go` 新建，先放严格子解码器与 `Decoded` 的访问器（文件头注释写明职责：入站边界的严格 Chat 视图，供异构出站消费，不读 Extensions）：

```go
package openaichat

import (
	"encoding/json"
	"strings"

	"github.com/yobo2u/omugw/internal/canonical"
)

// decodeStreamOptions 严格解码流式选项。
//
// 另起一个 DisallowUnknownFields 的解码器：外层的严格模式只管顶层字段，
// RawMessage 子树绕过了它。未知子字段必须在入站就拒掉。
func decodeStreamOptions(raw json.RawMessage) (*StreamOptions, error) {
	var so StreamOptions
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&so); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest,
			"stream_options 无法解析")
	}
	return &so, nil
}
```

`internal/protocol/openaichat/decode.go`：在 `Decoded` 结构体加私有字段 `streamOptions *StreamOptions`，并在 `Decode` 里、`web_search_options` 处理之后接入：

```go
	// stream_options：严格子解码先行。它只服务网关自身的 usage chunk 决策，
	// 不进出站请求；未知子字段必须 400。
	if len(w.StreamOptions) > 0 && string(w.StreamOptions) != "null" {
		so, err := decodeStreamOptions(w.StreamOptions)
		if err != nil {
			return nil, err
		}
		out.streamOptions = so
	}
```

`projection.go` 里给 `Decoded` 加访问器：

```go
// StreamOptionsIncludeUsage 报告客户端是否要求流式末尾的 usage chunk。
// 缺省或显式 false 都返回 false。
func (d *Decoded) StreamOptionsIncludeUsage() bool {
	return d.streamOptions != nil && d.streamOptions.IncludeUsage != nil && *d.streamOptions.IncludeUsage
}
```

运行：

```bash
go test ./internal/protocol/openaichat/ -run 'TestStreamOptions' -v
```

预期：两条 **PASS**。

- [ ] **步骤 4.3：写失败测试（RED）——具名出站投影**

在 `projection_test.go` 追加：

```go
// TestProjectionEnumeratesAcceptedFields 钉死投影必须枚举全部已接受字段，
// 且显式暴露 Canonical 未承载的字段，供异构出站映射与 422 判定。
func TestProjectionEnumeratesAcceptedFields(t *testing.T) {
	p, err := Project([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
	  "n":2,"presence_penalty":0.5,"logprobs":true,"top_logprobs":3,
	  "parallel_tool_calls":false,"frequency_penalty":0.9,"user":"u-1",
	  "store":true,"service_tier":"default","metadata":{"k":"v"},
	  "audio":{"voice":"alloy","format":"wav"}}`))
	if err != nil {
		t.Fatalf("合法字段应被投影接受: %v", err)
	}
	if p.N == nil || *p.N != 2 {
		t.Errorf("N 应解出 2，实际 %+v", p.N)
	}
	if p.PresencePenalty == nil || *p.PresencePenalty != 0.5 {
		t.Errorf("PresencePenalty 应解出，实际 %+v", p.PresencePenalty)
	}
	if !p.FrequencyPenaltyPresent || !p.UserPresent || !p.StorePresent ||
		!p.ServiceTierPresent || !p.MetadataPresent || !p.AudioPresent {
		t.Errorf("无落点字段的显式提交必须可识别: %+v", p)
	}
}

// TestProjectionRejectsUnknownField 钉死投影不静默丢弃未知字段。
func TestProjectionRejectsUnknownField(t *testing.T) {
	if _, err := Project([]byte(`{"model":"m","messages":[],"totally_new":1}`)); err == nil {
		t.Fatal("投影未知字段应被拒绝")
	}
}

// TestProjectionPresenceUsesObjectKeys 钉死「显式提交」按 JSON key 是否存在判断，
// 不能把空串、空对象、零值或 null 误判为缺省。
func TestProjectionPresenceUsesObjectKeys(t *testing.T) {
	p, err := Project([]byte(`{"model":"m","messages":[],"frequency_penalty":0,
	  "logit_bias":{},"service_tier":"","store":false,"user":"",
	  "metadata":{},"audio":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if !p.FrequencyPenaltyPresent || !p.LogitBiasPresent || !p.ServiceTierPresent ||
		!p.StorePresent || !p.UserPresent || !p.MetadataPresent || !p.AudioPresent {
		t.Fatalf("显式零值/null 也必须算提交: %+v", p)
	}
}
```

运行：

```bash
go test ./internal/protocol/openaichat/ -run 'TestProjection' -v
```

预期：**编译失败**（`Project` / `Projection` 未定义）。正确的 RED。

- [ ] **步骤 4.4：实现具名投影（GREEN）**

在 `projection.go` 追加投影类型与构造函数。它只承载 Canonical 无法无损表达、且异构
出站确实需要的 wire 事实；`model/stream/temperature/top_p/seed/stop` 已有 Canonical 落点，
不得复制进投影形成第二事实源。无 Native 落点的字段额外暴露 `*Present` 布尔：

```go
// Projection 是 Chat 请求的具名严格投影，供异构出站消费。
//
// Canonical 是有损的，n / presence_penalty / logprobs / parallel_tool_calls 等
// 采样选项没有落点；异构 translator 又读不得 Extensions。投影在解码边界把这些
// 字段显式解出，并对「无 Native 落点」的字段只记录是否显式提交，供 422 判定。
type Projection struct {
	N        *int
	PresencePenalty *float64
	Logprobs        *bool
	TopLogprobs     *int
	ParallelToolCalls *bool
	MaxTokens       *int
	MaxCompletionTokens *int

	// 以下字段在 DashScope Native 无落点。投影不搬运它们的值，只记录
	// 「客户端是否显式提交了它」——值本身由 422 规则点名拒绝，无需读取。
	FrequencyPenaltyPresent bool
	LogitBiasPresent        bool
	ServiceTierPresent      bool
	StorePresent            bool
	UserPresent             bool
	MetadataPresent         bool
	AudioPresent            bool

	// StreamOptionsIncludeUsage 供流式 usage chunk 决策。
	StreamOptionsIncludeUsage bool

	// WebSearch 报告客户端是否提交了非 null 的 web_search_options。
	// Native 编码器据此发 enable_search:true；选项本身的丢失登记在矩阵。
	WebSearch bool
}

// Project 严格解码 Chat 请求为投影。未知字段报错，不静默丢弃。
func Project(body []byte) (*Projection, error) {
	var w Request
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest,
			"无法解析 Chat Completions 请求")
	}
	// presence 只看顶层对象是否含 key；Request 字段的 Go 零值无法区分缺省、
	// 显式空串、空对象与 null。严格字段白名单仍由上面的 Request 解码负责。
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest,
			"无法解析 Chat Completions 请求字段")
	}
	p := &Projection{
		N:                   w.N,
		PresencePenalty:     w.PresencePenalty,
		Logprobs:            w.Logprobs,
		TopLogprobs:         w.TopLogprobs,
		ParallelToolCalls:   w.ParallelToolCalls,
		MaxTokens:           w.MaxTokens,
		MaxCompletionTokens: w.MaxCompletionTokens,
	}
	_, p.FrequencyPenaltyPresent = fields["frequency_penalty"]
	_, p.LogitBiasPresent = fields["logit_bias"]
	_, p.ServiceTierPresent = fields["service_tier"]
	_, p.StorePresent = fields["store"]
	_, p.UserPresent = fields["user"]
	_, p.MetadataPresent = fields["metadata"]
	_, p.AudioPresent = fields["audio"]
	if len(w.StreamOptions) > 0 && string(w.StreamOptions) != "null" {
		so, err := decodeStreamOptions(w.StreamOptions)
		if err != nil {
			return nil, err
		}
		p.StreamOptionsIncludeUsage = so.IncludeUsage != nil && *so.IncludeUsage
	}
	// web_search_options：出现且非 null 即开启搜索。严格子解码复用既有的
	// decodeWebSearchOptions（capability.go），未知子字段同样 400。
	if len(w.WebSearchOptions) > 0 && string(w.WebSearchOptions) != "null" {
		if _, err := decodeWebSearchOptions(w.WebSearchOptions); err != nil {
			return nil, err
		}
		p.WebSearch = true
	}
	return p, nil
}
```

运行：

```bash
go test ./internal/protocol/openaichat/ -run 'TestProjection|TestStreamOptions' -v
```

预期：全部 **PASS**。

- [ ] **步骤 4.5：聚焦与更广验证**

```bash
go test ./internal/protocol/openaichat/ -v
go build ./...
```

预期：openaichat 全包绿，build 通过。

- [ ] **步骤 4.6：提交**

```bash
GIT_MASTER=1 git add internal/protocol/openaichat/wire.go internal/protocol/openaichat/projection.go \
  internal/protocol/openaichat/projection_test.go internal/protocol/openaichat/decode.go
GIT_MASTER=1 git commit -m "Chat 严格投影：stream_options 子解码与具名出站投影" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

### 任务 5：`native_endpoint` 门声明——配置与路由的单一事实来源

**依赖**：无。
**文件**：修改 `internal/config/gateway.go`、`internal/config/gateway_test.go`、`internal/router/router.go`、`internal/router/router_test.go`。

> Native 门是**部署事实**，由 `native_endpoint` 声明，枚举仅 `text-generation` / `multimodal-generation`。校验规则：① target 所指 Provider `kind == dashscope.native` 时必填；② 其他 kind 必空；③ 未知枚举启动期失败。

- [ ] **步骤 5.1：写失败测试（RED）——config 校验**

在 `internal/config/gateway_test.go` 追加（构造最小合法配置再逐条破坏；沿用现有测试里构造 `Config` 的 helper 风格）：

```go
// TestNativeEndpointValidation 钉死 native_endpoint 的三条启动期校验。
func TestNativeEndpointValidation(t *testing.T) {
	// 构造一份最小合法配置：一个 dashscope.native provider + 一个指向它的 target。
	base := func(nativeEndpoint string) Config {
		c := fullGateway()
		c.Providers[0].Kind = "dashscope.native"
		c.Providers[0].BaseURL = "https://dashscope.aliyuncs.com"
		c.Models[0].Targets[0].NativeEndpoint = nativeEndpoint
		return c
	}

	t.Run("native kind 缺 native_endpoint 应失败", func(t *testing.T) {
		c := base("")
		if err := c.validateGateway(); err == nil {
			t.Fatal("dashscope.native target 必须声明 native_endpoint")
		}
	})
	t.Run("非 native kind 带 native_endpoint 应失败", func(t *testing.T) {
		c := base("text-generation")
		c.Providers[0].Kind = "openai.compat"
		if err := c.validateGateway(); err == nil {
			t.Fatal("非 dashscope.native target 不得声明 native_endpoint")
		}
	})
	t.Run("未知枚举应失败", func(t *testing.T) {
		c := base("embedding")
		if err := c.validateGateway(); err == nil {
			t.Fatal("未知 native_endpoint 枚举必须在启动期拒绝")
		}
	})
	t.Run("合法枚举应通过", func(t *testing.T) {
		c := base("text-generation")
		if err := c.validateGateway(); err != nil {
			t.Fatalf("text-generation 应合法: %v", err)
		}
	})
}
```

运行：

```bash
go test ./internal/config/ -run TestNativeEndpointValidation -v
```

预期：**编译失败**（`TargetSpec` 无 `NativeEndpoint` 字段）。正确的 RED。

- [ ] **步骤 5.2：实现 config 字段与校验（GREEN）**

`internal/config/gateway.go`：`TargetSpec` 增加字段：

```go
	// NativeEndpoint 声明该 target 的 DashScope Native 门，枚举仅
	// text-generation / multimodal-generation。只有 kind == dashscope.native
	// 的 target 才填；门是部署事实，不按模型名或本次请求是否含媒体推断。
	NativeEndpoint string `yaml:"native_endpoint"`
```

在 `validateGateway` 的 target 循环里（`t.UpstreamModel` 校验之后）追加：

```go
			kind := endpoints[t.Endpoint].Kind
			switch {
			case kind == "dashscope.native" && t.NativeEndpoint == "":
				return fmt.Errorf("config: 规则 %q 的候选 %q 是 dashscope.native，必须声明 native_endpoint",
					m.Match, t.Endpoint)
			case kind != "dashscope.native" && t.NativeEndpoint != "":
				return fmt.Errorf("config: 规则 %q 的候选 %q 非 dashscope.native，不得声明 native_endpoint",
					m.Match, t.Endpoint)
			case kind == "dashscope.native" &&
				t.NativeEndpoint != "text-generation" && t.NativeEndpoint != "multimodal-generation":
				return fmt.Errorf("config: 规则 %q 的候选 %q native_endpoint %q 非法（仅 text-generation / multimodal-generation）",
					m.Match, t.Endpoint, t.NativeEndpoint)
			}
```

运行：

```bash
go test ./internal/config/ -run TestNativeEndpointValidation -v
```

预期：**PASS**。

- [ ] **步骤 5.3：写失败测试（RED）——router 字段与校验**

在 `internal/router/router_test.go` 追加：

```go
// TestTargetNativeEndpointValidate 钉死路由侧对 native_endpoint 的枚举校验。
func TestTargetNativeEndpointValidate(t *testing.T) {
	mk := func(ep string) Target {
		return Target{Kind: degrade.ProviderDashScopeNative, Endpoint: "e",
			BaseURL: "https://x", UpstreamModel: "m", CredentialPool: "p", NativeEndpoint: ep}
	}
	if err := mk("text-generation").validate(); err != nil {
		t.Fatalf("text-generation 应合法: %v", err)
	}
	if err := mk("multimodal-generation").validate(); err != nil {
		t.Fatalf("multimodal-generation 应合法: %v", err)
	}
	if err := mk("embedding").validate(); err == nil {
		t.Fatal("未知 native_endpoint 应被拒绝")
	}
}
```

运行：

```bash
go test ./internal/router/ -run TestTargetNativeEndpointValidate -v
```

预期：**编译失败**（`Target` 无 `NativeEndpoint`）。正确的 RED。

- [ ] **步骤 5.4：实现 router 字段与校验（GREEN）**

`internal/router/router.go`：`Target` 增加字段：

```go
	// NativeEndpoint 是 DashScope Native target 的门，text-generation 或
	// multimodal-generation。Provider 严格用它选上游路径，不猜测。
	NativeEndpoint string
```

`validate()` 末尾追加：

```go
	if t.NativeEndpoint != "" &&
		t.NativeEndpoint != "text-generation" && t.NativeEndpoint != "multimodal-generation" {
		return fmt.Errorf("native_endpoint %q 非法", t.NativeEndpoint)
	}
```

运行：

```bash
go test ./internal/router/ -run TestTargetNativeEndpointValidate -v
```

预期：**PASS**。

- [ ] **步骤 5.5：聚焦与更广验证**

```bash
go test ./internal/config/ ./internal/router/ -v
go build ./...
```

预期：两包全绿，build 通过。（`build.go` 把 `TargetSpec.NativeEndpoint` 接线到 `router.Target` 的动作放在任务 14，与 Composite Provider 装配一起做。）

- [ ] **步骤 5.6：提交**

```bash
GIT_MASTER=1 git add internal/config/gateway.go internal/config/gateway_test.go \
  internal/router/router.go internal/router/router_test.go
GIT_MASTER=1 git commit -m "native_endpoint 门声明：配置与路由单一事实来源" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 波次 1：编解码

> 波次 1 的四个 codec 互相独立，可并行。为了让 `openaichat` 与 `dashscopenative`
> 两个协议包**互不 import**（各自只依赖 `canonical`），Native 侧产出
> `dashscopenative.Result`，Chat 侧定义自己的输入结构（用 `canonical` 类型），
> 两者的桥接放在波次 2 的 translator（`internal/provider/dashscopenative`）里。

### 任务 6：DashScope Native 出站请求编码

**依赖**：任务 4（投影提供 Chat 特有采样字段）、任务 5（`Door` 选路）。
**文件**：新建 `internal/protocol/dashscopenative/encode_request.go`、`internal/protocol/dashscopenative/encode_request_test.go`。若单文件超 250 行，把消息/内容编码拆到同包 `encode_messages.go`。

- [ ] **步骤 6.1：定义门与输入类型，写失败测试（RED）**

先在 `encode_request.go` 声明类型（让测试能编译引用），再写测试。类型定义：

```go
// Door 是 DashScope Native 的门（端点族）。门是部署事实，由配置声明，
// 不按模型名或本次请求是否含媒体推断。
type Door string

const (
	DoorTextGeneration       Door = "text-generation"
	DoorMultimodalGeneration Door = "multimodal-generation"
)

// Path 返回门对应的上游端点路径。未知门返回空串，由调用方报错。
func (d Door) Path() string {
	switch d {
	case DoorTextGeneration:
		return TextGenerationPath
	case DoorMultimodalGeneration:
		return MultimodalGenerationPath
	default:
		return ""
	}
}

// ChatSampling 承载 Canonical 不承载、而 Native 出站需要的 Chat 采样选项。
// 由 translator 从 openaichat 严格投影填入——协议包不互相 import，
// 桥接在 provider 层。
type ChatSampling struct {
	N                   *int
	PresencePenalty     *float64
	Logprobs            *bool
	TopLogprobs         *int
	// ParallelToolCalls 为 nil 表示客户端未提交，编码器按 OpenAI 默认注入 true。
	ParallelToolCalls   *bool
	// MaxTokens / MaxCompletionTokens 保留客户端实际提交的字段，不同时制造两个限制。
	MaxTokens           *int
	MaxCompletionTokens *int
	// WebSearch 为真时编码器发 parameters.enable_search:true（只保留开关）。
	WebSearch           bool
	// IncrementalOutput 仅流式 translator 置 true；false 时不发该字段。
	IncrementalOutput   bool
}

// EncodeRequest 把 Chat 请求编码成 DashScope Native 出站信封。
// 出站信封固定为 {model, input:{messages}, parameters:{result_format:message,...}}。
func EncodeRequest(canon *canonical.Request, extra ChatSampling, door Door, upstreamModel string) ([]byte, error)
```

在 `encode_request_test.go` 写最小 RED 测试（断言信封骨架与门选择）：

```go
// TestEncodeRequestEnvelope 钉死出站信封骨架：model / input.messages / parameters.result_format。
func TestEncodeRequestEnvelope(t *testing.T) {
	canon := &canonical.Request{
		Model:    "logical",
		System:   []canonical.Part{canonical.Text("you are helpful")},
		Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.Part{canonical.Text("hi")}}},
	}
	body, err := EncodeRequest(canon, ChatSampling{}, DoorTextGeneration, "qwen-plus")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Model string `json:"model"`
		Input struct {
			Messages []map[string]any `json:"messages"`
		} `json:"input"`
		Parameters struct {
			ResultFormat string `json:"result_format"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Model != "qwen-plus" {
		t.Errorf("model 应为上游名，实际 %q", env.Model)
	}
	if env.Parameters.ResultFormat != "message" {
		t.Errorf("result_format 应固定 message，实际 %q", env.Parameters.ResultFormat)
	}
	if len(env.Input.Messages) != 2 { // system + user
		t.Errorf("应有 system+user 两条消息，实际 %d", len(env.Input.Messages))
	}
}

// TestEncodeRequestDoorPath 钉死门到路径的单一事实来源。
func TestEncodeRequestDoorPath(t *testing.T) {
	if DoorTextGeneration.Path() != TextGenerationPath {
		t.Errorf("text-generation 门应指向 %s", TextGenerationPath)
	}
	if DoorMultimodalGeneration.Path() != MultimodalGenerationPath {
		t.Errorf("multimodal-generation 门应指向 %s", MultimodalGenerationPath)
	}
	if Door("embedding").Path() != "" {
		t.Error("未知门路径应为空")
	}
}
```

运行：

```bash
go test ./internal/protocol/dashscopenative/ -run 'TestEncodeRequest' -v
```

预期：**失败**——`EncodeRequest` 尚未实现（返回 nil/空 body，断言不满足）。正确的 RED。

- [ ] **步骤 6.2：实现请求编码（GREEN）**

实现 `EncodeRequest`。参数映射表（**逐字段枚举，不得遗漏**）：

| 来源（Canonical / ChatSampling） | Native 目标 | 规则 |
|---|---|---|
| `canon.System` + `canon.Messages` | `input.messages` | system 提取为首条 `{"role":"system"}`；角色/文本/工具历史逐项编码（见消息编码） |
| `upstreamModel` | `model` | 上游真实模型名 |
| 固定 | `parameters.result_format` | 恒为 `"message"` |
| `extra.MaxCompletionTokens` 非 nil | `parameters.max_completion_tokens` | 保留客户端实际提交字段 |
| `extra.MaxTokens` 非 nil 且 `MaxCompletionTokens` nil | `parameters.max_tokens` | 两者择一，不同时制造两个限制 |
| `canon.Temperature` | `parameters.temperature` | 等价 |
| `canon.TopP` | `parameters.top_p` | 等价 |
| `canon.Seed` | `parameters.seed` | int64 原样 |
| `canon.StopSequences` | `parameters.stop` | 单元素发 string，多元素发数组 |
| `extra.PresencePenalty` | `parameters.presence_penalty` | 等价 |
| `extra.N` | `parameters.n` | 逐字 |
| `extra.Logprobs` | `parameters.logprobs` | 逐字 |
| `extra.TopLogprobs` | `parameters.top_logprobs` | 逐字 |
| `canon.Tools` / `canon.ToolChoice` | `parameters.tools` / `parameters.tool_choice` | 编码为 OpenAI 风格 function 列表；有 tools 时强制 `result_format:message`（已恒为 message） |
| `extra.ParallelToolCalls` | `parameters.parallel_tool_calls` | 非 nil 逐字；**nil 时显式注入 `true`** |
| `canon.Reasoning` | `parameters.reasoning_effort` 或 `parameters.enable_thinking` | `EffortNone` → `enable_thinking:false` 且**不发** `reasoning_effort`；其余档位逐字写 `reasoning_effort`，不发 `thinking_budget` |
| `extra.WebSearch == true` | `parameters.enable_search:true` | 只保留开关；位置/上下文大小等选项丢失（登记在矩阵，不在编码器） |
| `extra.IncrementalOutput == true` | `parameters.incremental_output:true` | 流式 translator 必设；非流式不发 |
| `canon.ResponseFormat` | `parameters.response_format` | `json_object` → `{"type":"json_object"}`；`json_schema` → `{"type":"json_schema","json_schema":{name,schema,strict}}`；`text` 不发 |
| `stream_options` | — | **不转发**（网关消费） |
| `canon.Modalities` | — | 不在编码器处理；`audio` 由能力门 REJECT（波次 2），`text` 无落点 |

消息编码（`input.messages` 逐项）：

- **角色**：`system` / `user` / `assistant` / `tool` 直接对应。
- **文本门（`DoorTextGeneration`）**：user/assistant 的 content **必须是 string**——把该消息的文本 Part 连接成单个字符串；出现媒体 Part 即报错（媒体只能走多模态门，由波次 2 的过滤保证不到这里，编码器仍要 fail-closed）。
- **多模态门（`DoorMultimodalGeneration`）**：user content 编码为**数组**，逐 Part：文本 → `{"text":...}`；图片 → `{"image":...}`（URL 或 data URI）；音频 → `{"audio":...}`。网关不代下载 URL，不跨 Provider 搬运 FileRef（遇到 `MediaFile` 报错）。
- **工具历史**：assistant 消息的 `PartToolCall` 编码为 `tool_calls:[{id,type:"function",function:{name,arguments}}]`（arguments 是 JSON 字符串）；`tool` 角色消息的 `PartToolResult` 编码为 `{"role":"tool","content":<字符串>,"tool_call_id":...}`，id/name/arguments/tool_call_id 保持关联。

运行：

```bash
go test ./internal/protocol/dashscopenative/ -run 'TestEncodeRequest' -v
```

预期：**PASS**。

- [ ] **步骤 6.3：补齐参数映射与两扇门内容形态的聚焦测试（RED→GREEN 逐项）**

按上表逐组补测试并实现至全绿，至少覆盖：
1. `parallel_tool_calls` 缺省（`extra.ParallelToolCalls == nil`）时出站体含 `"parallel_tool_calls":true`；显式 `false` 时保持 `false`。
2. `EffortNone` 只发 `enable_thinking:false`、不发 `reasoning_effort`；`EffortHigh` 发 `reasoning_effort:"high"`、不发 `enable_thinking`、不发 `thinking_budget`。
3. 有 `canon.Tools` 时 `parameters.tools` 为 function 列表且 `result_format` 仍为 `message`。
4. `max_tokens` 与 `max_completion_tokens` 二者择一（只发客户端提交的那个）。
5. 文本门 user 含媒体 Part → 报错；多模态门 user 图片/音频编码为 `{"image"}`/`{"audio"}` 数组元素。
6. `response_format` json_object / json_schema 原样表达；`text` 不发。
7. 工具调用历史：assistant `tool_calls` 与后续 `tool` 消息 `tool_call_id` 关联。

每组先写测试看红，再实现看绿。

- [ ] **步骤 6.4：更广验证**

```bash
go test ./internal/protocol/dashscopenative/ -v
go build ./...
```

预期：全包绿，build 通过。

- [ ] **步骤 6.5：提交**

```bash
GIT_MASTER=1 git add internal/protocol/dashscopenative/encode_request.go internal/protocol/dashscopenative/encode_request_test.go
GIT_MASTER=1 git commit -m "DashScope Native 出站请求编码：两扇门信封与全参数映射" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

### 任务 7：DashScope Native 成功响应与 `event:result` 解码

**依赖**：无。
**文件**：新建 `internal/protocol/dashscopenative/result.go`、`internal/protocol/dashscopenative/result_test.go`。

- [ ] **步骤 7.1：定义 `Result` 类型，写失败测试（RED）**

`result.go` 声明类型：

```go
// Result 是 Native 成功响应解码后的投影。非流式裸 body 与流式 SSE data 负载
// 同构（output.choices[] + usage + request_id），共用这一结构。
type Result struct {
	RequestID string
	Choices   []Choice
	Usage     *canonical.Usage
}

// Choice 是一个候选。候选序号取数组下标；Native 没有官方 index 字段，
// 跨帧顺序与长度是否稳定未文档化，由流式侧按契约违例处理。
type Choice struct {
	Role             string
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	FinishReason string          // 归一后：""=生成中，stop/length/tool_calls
	Logprobs     json.RawMessage // 原样透传
}

// ToolCall 保留 Native wire 的工具参数字符串。流式帧里 Arguments 可能只是未闭合
// JSON 片段，因此不能提前塞进「完整且已闭合 JSON」契约的 canonical.ToolCall。
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// DecodeResult 解码非流式裸 body。
func DecodeResult(body []byte) (*Result, error)

// DecodeFrame 解码一条流式 SSE data 负载。
func DecodeFrame(data string) (*Result, error)
```

在 `result_test.go` 写 RED 测试，覆盖契约调研 §7/§8 的关键形态：

```go
// TestDecodeResultFinishReasonNulls 钉死 JSON null 与字符串 "null" 都归一为「生成中」。
func TestDecodeResultFinishReasonNulls(t *testing.T) {
	for _, body := range []string{
		`{"output":{"choices":[{"finish_reason":null,"message":{"role":"assistant","content":"a"}}]},"request_id":"r"}`,
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":"a"}}]},"request_id":"r"}`,
	} {
		res, err := DecodeResult([]byte(body))
		if err != nil {
			t.Fatalf("解码失败: %v", err)
		}
		if res.Choices[0].FinishReason != "" {
			t.Errorf("null/\"null\" 应归一为生成中，实际 %q", res.Choices[0].FinishReason)
		}
	}
}

// TestDecodeResultUsage 钉死 usage 映射与缺失 total_tokens 不伪造。
func TestDecodeResultUsage(t *testing.T) {
	res, err := DecodeResult([]byte(`{"output":{"choices":[{"finish_reason":"stop",
	  "message":{"role":"assistant","content":"ok"}}]},
	  "usage":{"input_tokens":5,"output_tokens":3},
	  "request_id":"r"}`))
	if err != nil {
		t.Fatal(err)
	}
	u := res.Usage
	if u == nil || u.Fidelity != canonical.FidelityAuthoritative {
		t.Fatalf("usage 应为 authoritative: %+v", u)
	}
	if u.InputTokens != 5 || u.OutputTokens != 3 {
		t.Errorf("token 映射错误: %+v", u)
	}
}

// TestDecodeFrameCumulativeUsage 钉死流式帧携带累计 usage 且 request_id 稳定。
func TestDecodeFrameCumulativeUsage(t *testing.T) {
	res, err := DecodeFrame(`{"output":{"choices":[{"finish_reason":"stop",
	  "message":{"role":"assistant","content":""}}]},
	  "usage":{"input_tokens":26,"output_tokens":66,"total_tokens":92},
	  "request_id":"d30a9914"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequestID != "d30a9914" {
		t.Errorf("request_id 应解出: %q", res.RequestID)
	}
	if res.Usage == nil || res.Usage.OutputTokens != 66 {
		t.Errorf("累计 usage 应解出: %+v", res.Usage)
	}
}
```

运行：

```bash
go test ./internal/protocol/dashscopenative/ -run 'TestDecodeResult|TestDecodeFrame' -v
```

预期：**失败**（`DecodeResult`/`DecodeFrame` 未实现）。正确的 RED。

- [ ] **步骤 7.2：实现解码（GREEN）**

实现 `DecodeResult` / `DecodeFrame`（后者解同一信封，只是入参是 SSE data 字符串）。要点：

- 顶层解 `{output:{choices:[{finish_reason,message:{role,content,reasoning_content,tool_calls,logprobs}}]},usage,request_id}`。**容忍** SDK 包装字段 `status_code`/`code`/`message`（裸 HTTP 不出现，但解码要宽松，用结构体而非 `DisallowUnknownFields`）。
- `finish_reason` 归一：JSON null、字符串 `"null"`、缺省 → `""`；`stop`/`length`/`tool_calls` 原样。
- `message.content`：string 直接取；数组（多模态）取 `text` 键拼接。
- `message.content` 与 `message.reasoning_content` 保持 Native-specific 字符串字段；
  `message.tool_calls[]` 解成上面的 Native-specific `ToolCall`，arguments 字符串原样保留。
  **不得**在这一层构造 `canonical.ToolCall`：流式 arguments 可能未闭合，而 Canonical
  契约要求完整 JSON。非流式 translator 在 `Call` 内先 `json.Valid`，成功后才转成
  `canonical.Message`；流式 candidate state 直接消费字符串片段。
- `usage` 映射：`input_tokens→InputTokens`、`output_tokens→OutputTokens`、`prompt_tokens_details.cached_tokens→CacheReadInputTokens`、`output_tokens_details.reasoning_tokens→ReasoningTokens`；`Fidelity=Authoritative`。`total_tokens` 缺失（多模态）**不伪造**；`image_tokens`/`video_tokens`/`audio_tokens` 无 Canonical 落点，丢弃（本期不新增 Usage 字段）。`usage` 整体缺省 → `Result.Usage` 为 nil。
- `DecodeResult` 与 `DecodeFrame` 共用内部解码函数，仅入参形态不同。

运行：

```bash
go test ./internal/protocol/dashscopenative/ -run 'TestDecodeResult|TestDecodeFrame' -v
```

预期：**PASS**。

- [ ] **步骤 7.3：补齐聚焦测试（RED→GREEN 逐项）**

逐组补测试至全绿：tool_calls 的 id/name/arguments 关联；reasoning_content → PartThinking；logprobs 原样透传；多模态 content 数组取 text；usage 缺省 → nil；`DecodeFrame` 与 `DecodeResult` 对同一负载结果一致。

- [ ] **步骤 7.4：更广验证**

```bash
go test ./internal/protocol/dashscopenative/ -v
go build ./...
```

预期：全包绿，build 通过。

- [ ] **步骤 7.5：提交**

```bash
GIT_MASTER=1 git add internal/protocol/dashscopenative/result.go internal/protocol/dashscopenative/result_test.go
GIT_MASTER=1 git commit -m "DashScope Native 成功响应解码：两种 null 与累计 usage" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

### 任务 8：Chat 非流式成功响应编码（`chat.completion`）

**依赖**：无（输入结构只依赖 `canonical`）。
**文件**：新建 `internal/protocol/openaichat/encode_response.go`、`internal/protocol/openaichat/encode_response_test.go`。

- [ ] **步骤 8.1：定义输入结构，写失败测试（RED）**

`encode_response.go` 声明（输入用 `canonical` 类型，避免 openaichat import dashscopenative）：

```go
// CompletionInput 是拼装 chat.completion 的 Canonical 素材，由 translator 从
// Native 解码结果填好。id 取 Native request_id，model 取 target.UpstreamModel。
type CompletionInput struct {
	ID      string
	Model   string
	Created int64
	Choices []CompletionChoice
	Usage   *canonical.Usage
}

// CompletionChoice 是一个候选，逐条编码为 choices[]。
type CompletionChoice struct {
	Message      canonical.Message
	FinishReason *string // stop/length/tool_calls；nil 编码为 JSON null，不猜上游语义
	Logprobs     json.RawMessage
}

// EncodeCompletion 编码 chat.completion wire 字节。
func EncodeCompletion(in CompletionInput) ([]byte, error)
```

`encode_response_test.go` RED 测试：

```go
// TestEncodeCompletionShape 钉死 chat.completion 的 object/id/model/created/choices/usage。
func TestEncodeCompletionShape(t *testing.T) {
	body, err := EncodeCompletion(CompletionInput{
		ID: "req-1", Model: "qwen-plus", Created: 1755216000,
		Choices: []CompletionChoice{{
			Message:      canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.Part{canonical.Text("你好")}},
			FinishReason: ptr("stop"),
		}},
		Usage: &canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: 5, OutputTokens: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Index        int    `json:"index"`
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "chat.completion" || got.ID != "req-1" || got.Model != "qwen-plus" || got.Created != 1755216000 {
		t.Errorf("顶层字段错误: %+v", got)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != "你好" || got.Choices[0].FinishReason != "stop" {
		t.Errorf("choices 错误: %+v", got.Choices)
	}
	if got.Usage.PromptTokens != 5 || got.Usage.CompletionTokens != 2 || got.Usage.TotalTokens != 7 {
		t.Errorf("usage 应为 5/2/7（缺失 total 由两者相加）: %+v", got.Usage)
	}
}
```

运行：

```bash
go test ./internal/protocol/openaichat/ -run TestEncodeCompletionShape -v
```

预期：**失败**（`EncodeCompletion` 未实现）。正确的 RED。

- [ ] **步骤 8.2：实现编码（GREEN）**

实现 `EncodeCompletion`。要点：
- `object` 恒为 `"chat.completion"`；`id`/`model`/`created` 取自输入（同一响应内稳定）。
- `choices[]` **逐条**编码，候选序号 `index` 取数组下标；`message.content`/`reasoning_content`/`tool_calls` 在每个候选内逐项映射：文本 Part → `content`；PartThinking → `reasoning_content`；PartToolCall → `tool_calls[]`（arguments 为 JSON 字符串）。
- `finish_reason` 仅映射 Native 明确给出的 `stop/length/tool_calls`；Native 缺失或 null
  时输出 JSON null，绝不凭空编造 `stop`。测试 helper `ptr` 只放在 `_test.go`。
- `usage`：`InputTokens→prompt_tokens`、`OutputTokens→completion_tokens`；`total_tokens` 缺失时由两者相加；`CacheReadInputTokens→prompt_tokens_details.cached_tokens`、`ReasoningTokens→completion_tokens_details.reasoning_tokens`。`Usage` 为 nil 时不输出 usage 字段。

运行：

```bash
go test ./internal/protocol/openaichat/ -run TestEncodeCompletionShape -v
```

预期：**PASS**。

- [ ] **步骤 8.3：补齐聚焦测试（RED→GREEN 逐项）**

多候选（两条 choices，index 0/1）；tool_calls 编码；reasoning_content 编码；usage nil 不输出；缓存/推理明细映射。逐项先红后绿。

- [ ] **步骤 8.4：更广验证**

```bash
go test ./internal/protocol/openaichat/ -v
go build ./...
```

预期：全包绿，build 通过。

- [ ] **步骤 8.5：提交**

```bash
GIT_MASTER=1 git add internal/protocol/openaichat/encode_response.go internal/protocol/openaichat/encode_response_test.go
GIT_MASTER=1 git commit -m "Chat 非流式响应编码：chat.completion" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

### 任务 9：Chat SSE chunk 编码（`chat.completion.chunk`）

**依赖**：无。
**文件**：新建 `internal/protocol/openaichat/encode_stream.go`、`internal/protocol/openaichat/encode_stream_test.go`。

- [ ] **步骤 9.1：定义输入结构，写失败测试（RED）**

`encode_stream.go` 声明：

```go
// ChunkInput 是拼装一条 chat.completion.chunk 的 Canonical 素材。
// 一个 Native 帧编码成一条 chunk，其中该帧出现的每个候选各占一个 choices 条目。
type ChunkInput struct {
	ID      string
	Model   string
	Created int64
	Choices []ChunkChoice
	Usage   *canonical.Usage // nil = 本 chunk 不带 usage
}

// ChunkChoice 是一个候选的增量。
type ChunkChoice struct {
	Index        int
	Delta        ChunkDelta
	FinishReason string // ""=finish_reason 输出 null
}

// ChunkDelta 是增量内容。工具 arguments 保持字符串片段，允许 JSON 跨帧切断。
type ChunkDelta struct {
	Role             string // ""=省略 role
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCallDelta
}

// ToolCallDelta 是一个工具调用的增量。ID/Name 只在该调用首帧出现。
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string // JSON 片段，可能不闭合
}

// EncodeChunk 编码一条 chat.completion.chunk 的 JSON（不含 SSE 分帧）。
func EncodeChunk(in ChunkInput) ([]byte, error)

// DoneSentinel 是流结束哨兵，由流式侧在全部候选 finish 后合成。
const DoneSentinel = "[DONE]"
```

`encode_stream_test.go` RED 测试：

```go
// TestEncodeChunkDelta 钉死 chunk 的 object 与 delta 形态。
func TestEncodeChunkDelta(t *testing.T) {
	body, err := EncodeChunk(ChunkInput{
		ID: "req-1", Model: "qwen-plus", Created: 1755216000,
		Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{Content: "你"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Object  string `json:"object"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "chat.completion.chunk" {
		t.Errorf("object 应为 chat.completion.chunk，实际 %q", got.Object)
	}
	if got.Choices[0].Delta.Content != "你" {
		t.Errorf("delta.content 错误: %+v", got.Choices[0])
	}
	if got.Choices[0].FinishReason != nil {
		t.Errorf("生成中 finish_reason 应为 null，实际 %v", *got.Choices[0].FinishReason)
	}
}

// TestEncodeChunkToolArgsFragment 钉死工具 arguments 片段原样透传、不提前解析。
func TestEncodeChunkToolArgsFragment(t *testing.T) {
	body, err := EncodeChunk(ChunkInput{
		ID: "r", Model: "m", Created: 1,
		Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{ToolCalls: []ToolCallDelta{
			{Index: 0, Arguments: `{"loc`}, // 不闭合片段
		}}}},
	})
	if err != nil {
		t.Fatalf("不闭合的工具参数片段必须能编码: %v", err)
	}
	if !strings.Contains(string(body), `{"loc`) {
		t.Errorf("arguments 片段应原样透传: %s", body)
	}
}
```

运行：

```bash
go test ./internal/protocol/openaichat/ -run 'TestEncodeChunk' -v
```

预期：**失败**（`EncodeChunk` 未实现）。正确的 RED。

- [ ] **步骤 9.2：实现编码（GREEN）**

实现 `EncodeChunk`。要点：
- `object` 恒为 `"chat.completion.chunk"`；`id`/`model`/`created` 取自输入（首帧建立后跨帧稳定，由流式侧保证）。
- `choices[]` 逐条：`index`、`delta`（role/content/reasoning_content/tool_calls）、`finish_reason`（`""` → JSON null）。
- `delta.tool_calls[]`：`index`、`id`/`name`（非空才输出）、`function.arguments`（字符串片段原样，**不**做 `json.Valid` 校验）。
- `usage` 非 nil 时输出（供 `include_usage` 的最终空 choices usage chunk）；nil 时省略。
- 不在此处做 SSE 分帧（`data: ...\n\n` 由波次 2 的 transforming reader 负责）。

运行：

```bash
go test ./internal/protocol/openaichat/ -run 'TestEncodeChunk' -v
```

预期：**PASS**。

- [ ] **步骤 9.3：补齐聚焦测试（RED→GREEN 逐项）**

首帧带 role、后续帧省略 role；多候选一条 chunk 多个 choices 条目；最终 usage chunk（空 choices + usage）；finish_reason 输出；`DoneSentinel` 常量值。逐项先红后绿。

- [ ] **步骤 9.4：更广验证**

```bash
go test ./internal/protocol/openaichat/ -v
go build ./...
```

预期：全包绿，build 通过。

- [ ] **步骤 9.5：提交**

```bash
GIT_MASTER=1 git add internal/protocol/openaichat/encode_stream.go internal/protocol/openaichat/encode_stream_test.go
GIT_MASTER=1 git commit -m "Chat SSE chunk 编码：跨帧工具参数与 usage 收尾" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 波次 2：Composite Provider

> 新包 `internal/provider/dashscopenative` 与协议包 `internal/protocol/dashscopenative`
> 同名，引用协议包时一律用 import 别名 `nativewire`，避免包名歧义。所有新文件
> 控制在 250 纯代码行内：分派在 `provider.go`、422 在 `reject.go`、非流式在
> `nonstream.go`、流式在 `stream.go`、多候选状态在 `candidates.go`。

### 任务 10：Composite 骨架、Native 分派与 fail-closed

**依赖**：任务 3（`provider.Request.Inbound`）。
**文件**：新建 `internal/provider/dashscopenative/provider.go`、`internal/provider/dashscopenative/provider_test.go`。

- [ ] **步骤 10.1：写失败测试（RED）**

`provider_test.go`（用 `httptest` 假上游验证 Native 入站被原样交给 passthrough；未知入站 fail-closed 且零出门）：

```go
package dashscopenative

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/httpx"
	"github.com/yobo2u/omugw/internal/config"
)

func newTestProvider(t *testing.T, up *httptest.Server) *Provider {
	t.Helper()
	client := httpx.New(config.Default().Timeouts, nil)
	return New(client, nil)
}

// TestNativeInboundDelegatesToPassthrough 钉死 Native 入站复用既有 passthrough。
func TestNativeInboundDelegatesToPassthrough(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{},"usage":{"input_tokens":1,"output_tokens":1},"request_id":"r"}`)
	}))
	defer up.Close()

	p := newTestProvider(t, up)
	_, err := p.Call(context.Background(), provider.Request{
		Target:     router.Target{Kind: degrade.ProviderDashScopeNative, Endpoint: "e", BaseURL: up.URL, UpstreamModel: "qwen-plus", CredentialPool: "p"},
		Credential: credential.Credential{ID: "k", Secret: "sk-test"},
		Raw:        []byte(`{"model":"qwen-plus","input":{"messages":[{"role":"user","content":"hi"}]}}`),
		Stream:     false,
		Inbound:    degrade.Inbound{Protocol: degrade.ProtoDashScopeNative, Endpoint: degrade.EndpointDashScopeTextGeneration},
	})
	if err != nil {
		t.Fatalf("Native 入站应直通: %v", err)
	}
	if gotPath != string(degrade.EndpointDashScopeTextGeneration) {
		t.Errorf("上游路径应为入站门 %s，实际 %q", degrade.EndpointDashScopeTextGeneration, gotPath)
	}
}

// TestUnknownInboundFailClosed 钉死未知入站坐标 fail-closed 且不出门。
func TestUnknownInboundFailClosed(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
	}))
	defer up.Close()

	p := newTestProvider(t, up)
	_, err := p.Call(context.Background(), provider.Request{
		Target:     router.Target{Kind: degrade.ProviderDashScopeNative, Endpoint: "e", BaseURL: up.URL, UpstreamModel: "m", CredentialPool: "p"},
		Credential: credential.Credential{ID: "k", Secret: "sk-test"},
		Raw:        []byte(`{}`),
		Inbound:    degrade.Inbound{Protocol: degrade.ProtoOpenAIResponses, Endpoint: degrade.EndpointOpenAIResponses},
	})
	if err == nil {
		t.Fatal("未知入站必须报错")
	}
	if canonical.AsError(err).Class != canonical.ClassUnsupported {
		t.Fatalf("应 fail-closed 为 unsupported，实际 %v", err)
	}
	if calls != 0 {
		t.Errorf("未知入站不得触达上游，实际出门 %d 次", calls)
	}
}
```

运行：

```bash
go test ./internal/provider/dashscopenative/ -run 'TestNativeInbound|TestUnknownInbound' -v
```

预期：**编译失败**（`New`/`Provider` 未定义）。正确的 RED。

- [ ] **步骤 10.2：实现骨架与分派（GREEN）**

`provider.go`：

```go
// Package dashscopenative 是 dashscope.native 的 Composite 出站适配器。
//
// 同一个 provider.Provider 接口后隐藏两种实现：Native 入站复用既有 passthrough
//（同源直通，保住字节与 TTFT）；OpenAI Chat 入站进入 translator（完整重编码）。
// 其他入站坐标 fail-closed——Gateway 不承担协议转换，这里也不猜。
package dashscopenative

import (
	"context"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/degrade"
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/provider/passthrough"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// Provider 是 dashscope.native 的 Composite 适配器。
type Provider struct {
	passthrough *passthrough.Provider
	client      *httpx.Client
	now         func() time.Time
}

// New 构造 Composite 适配器。内部自带一个 Native 同源 passthrough。
func New(c *httpx.Client, now func() time.Time) *Provider {
	if now == nil {
		now = time.Now
	}
	return &Provider{
		passthrough: passthrough.New(degrade.ProviderDashScopeNative, nativewire.TextGenerationPath, c, now),
		client:      c,
		now:         now,
	}
}

// Kind 返回协议族。
func (p *Provider) Kind() degrade.Provider { return degrade.ProviderDashScopeNative }

// Call 先钉死已完整实现的 Native 同源分支；Chat 分支要等非流式与流式
// translator 都落地后再由任务 13 原子接入，避免提交临时 501 桩。
func (p *Provider) Call(ctx context.Context, req provider.Request) (*httpx.Response, error) {
	switch req.Inbound.Protocol {
	case degrade.ProtoDashScopeNative:
		// 同源直通：入站门即出站端点，字节原样转发。
		return p.passthrough.Call(ctx, req)
	default:
		// fail-closed：未登记的入站坐标一律拒绝，不猜实现。
		return nil, canonical.Newf(canonical.ClassUnsupported,
			"dashscope.native 出站不支持入站协议 %q", req.Inbound.Protocol)
	}
}
```

> 本任务**不**声明 `translateChat`、`rejectUnmappable`、`translateNonStream` 或
> `translateStream`，也不提交任何“稍后替换”的 501 函数体。任务 11/12 先分别完成
> 可独立测试的真实实现；任务 13 在流式实现也完成后，一次性把 Chat 分支接进 `Call`。

运行：

```bash
go test ./internal/provider/dashscopenative/ -run 'TestNativeInbound|TestUnknownInbound' -v
```

预期：**PASS**。

- [ ] **步骤 10.3：聚焦与更广验证**

```bash
go test ./internal/provider/dashscopenative/ -v
go build ./...
```

预期：全包绿，build 通过。

- [ ] **步骤 10.4：提交**

```bash
GIT_MASTER=1 git add internal/provider/dashscopenative/provider.go internal/provider/dashscopenative/provider_test.go
GIT_MASTER=1 git commit -m "DashScope Native Composite Provider：入站分派与 fail-closed" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

### 任务 11：Chat 到 Native 入站 422 规则

**依赖**：任务 4（投影）、任务 2（Canonical `EffortNone`）。
**文件**：新建 `internal/provider/dashscopenative/reject.go`、`internal/provider/dashscopenative/reject_test.go`。

> 无落点字段按 JSON key presence 判定；推理与 tools 则只读 Canonical IR，绝不从
> 投影复制第二份语义源。所有推理规则都要排除 `EffortNone`（它是显式关思考）。

- [ ] **步骤 11.1：写失败测试（RED）**

`reject_test.go` 逐条覆盖设计文档「入站 422 规则」表。用 `httptest` 假上游计数，断言 422 时出门次数为 0。每条规则一个子测试，下面给出代表性两条与 `none` 的两条正例：

```go
// TestRejectNoLandingFields 钉死无落点字段显式提交即 422 且点名。
func TestRejectNoLandingFields(t *testing.T) {
	for _, field := range []string{
		`"frequency_penalty":0.5`, `"logit_bias":{"a":1}`, `"service_tier":"default"`,
		`"store":true`, `"user":"u-1"`, `"metadata":{"k":"v"}`, `"audio":{"voice":"alloy","format":"wav"}`,
	} {
		t.Run(field, func(t *testing.T) {
			proj, canon := mustInputs(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],`+field+`}`)
			err := rejectUnmappable(proj, canon)
			if err == nil {
				t.Fatalf("%s 应 422", field)
			}
			cerr := canonical.AsError(err)
			if cerr.Class != canonical.ClassUnsupported {
				t.Fatalf("应为 422 unsupported，实际 %v", cerr)
			}
		})
	}
}

// TestRejectNonStreamReasoning 钉死非流式 + 非 none 推理即 422。
func TestRejectNonStreamReasoning(t *testing.T) {
	proj, canon := mustInputs(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	if err := rejectUnmappable(proj, canon); err == nil {
		t.Fatal("非流式 + 推理应 422")
	}
}

// TestNonePositiveNonStreamReasoning 是 none 的正例：非流式 + none 必须放行。
//
// 缺了它，按字段存在与否误判的回归改不出红——这是守卫的证据。
func TestNonePositiveNonStreamReasoning(t *testing.T) {
	proj, canon := mustInputs(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`)
	if err := rejectUnmappable(proj, canon); err != nil {
		t.Fatalf("非流式 + reasoning_effort:none 应放行: %v", err)
	}
}

// TestNonePositiveNWithNone 是 none 的第二条正例：n>1 + none 放行。
func TestNonePositiveNWithNone(t *testing.T) {
	proj, canon := mustInputs(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"n":2,"reasoning_effort":"none"}`)
	if err := rejectUnmappable(proj, canon); err != nil {
		t.Fatalf("n>1 + reasoning_effort:none 应放行: %v", err)
	}
}
```

（`mustInputs` 同时调用 `openaichat.Project` 与 `openaichat.Decode`，返回投影和
`&decoded.Request`；这样测试也会阻止实现偷偷从投影读取 reasoning/tools。）

运行：

```bash
go test ./internal/provider/dashscopenative/ -run 'TestReject|TestNonePositive' -v
```

预期：**编译失败**（`rejectUnmappable` 尚不存在）。正确的 RED。

- [ ] **步骤 11.2：实现 422 规则（GREEN）**

`reject.go` 实现
`rejectUnmappable(proj *openaichat.Projection, req *canonical.Request) error`。
`Projection` 只提供 `N` 与无落点字段 presence；流式、推理与 tools 全取 Canonical：
流式取 `req.Stream`，推理是否开启取
`req.Reasoning != nil && req.Reasoning.Effort != canonical.EffortNone`，tools 是否存在取
`len(req.Tools) > 0`。规则表（**逐条点名，返回 `ClassUnsupported` 且 `Param` 指向字段**）：

| 条件 | 理由 |
|---|---|
| `FrequencyPenaltyPresent` / `LogitBiasPresent` / `ServiceTierPresent` / `StorePresent` / `UserPresent` / `MetadataPresent` / `AudioPresent` 任一为真 | Native 无落点（`metadata` 是 `store` 伴生；`audio` 不带 `modalities` 时能力门拦不住，会静默丢失） |
| Canonical reasoning 已开启且 `!req.Stream` | 官方硬约束：思考模式不允许非流式调用 |
| `N != nil && *N > 1 && len(req.Tools) > 0` | 官方契约：带 tools 时 Native 把 `n` 强制为 1 且不报错（静默回落，必须入站拦截） |
| `N != nil && *N > 1` 且 Canonical reasoning 已开启 | `n` 仅非思考模式支持；强制回落与否未文档化，fail-closed |

> 不给 `Projection` 增加 `ReasoningEffort` 或 `HasTools`。这两项已有 Canonical 落点，
> 复制到投影会形成互相漂移的双事实源。`n>1` 与「模型完全不支持 n」不同——后者
> 上游会明确报错，照常透传，不在这里拦。

每条返回形如：

```go
return &canonical.Error{
	Class:   canonical.ClassUnsupported,
	Message: "字段 frequency_penalty 在 DashScope Native 无落点",
	Param:   "frequency_penalty",
}
```

运行：

```bash
go test ./internal/provider/dashscopenative/ -run 'TestReject|TestNonePositive' -v
```

预期：**PASS**（含两条 `none` 正例）。

- [ ] **步骤 11.3：补齐其余规则聚焦测试（RED→GREEN 逐项）**

补齐：`n>1 + tools` 422；`n>1 + 非 none 推理` 422；`audio` 单独提交 422；各规则断言 `Param` 正确点名；组合（多个无落点字段同时提交）返回首个点名。逐项先红后绿。

- [ ] **步骤 11.4：更广验证**

```bash
go test ./internal/provider/dashscopenative/ -v
go build ./...
```

预期：全包绿，build 通过。

- [ ] **步骤 11.5：提交**

```bash
GIT_MASTER=1 git add internal/provider/dashscopenative/reject.go internal/provider/dashscopenative/reject_test.go
GIT_MASTER=1 git commit -m "Chat 到 Native 入站 422：无落点字段与硬约束逐条点名" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

### 任务 12：Chat 到 Native 非流式转换（`Call` 内完成，保住 failover）

**依赖**：任务 6（请求编码）、任务 7（响应解码）、任务 8（Chat 编码）、任务 10/11。
**文件**：新建 `internal/provider/dashscopenative/nonstream.go`、`internal/provider/dashscopenative/nonstream_test.go`。

- [ ] **步骤 12.1：写失败测试（RED）**

`nonstream_test.go`：假上游按 `upstream` 期望校验收到的 Native 信封，返回 Native 非流式响应；断言网关侧拿到的是 `chat.completion`、Content-Type 正确、`OnDashScopeUsage` 恰被调用一次：

```go
// TestNonStreamTransform 钉死非流式在 Call 内完整转换：Native 进、chat.completion 出。
func TestNonStreamTransform(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"choices":[{"finish_reason":"stop",
		  "message":{"role":"assistant","content":"你好"}}]},
		  "usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7},"request_id":"req-abc"}`)
	}))
	defer up.Close()

	p := newTestProvider(t, up)
	var usageCalls int
	var gotUsage canonical.Usage
	req := provider.Request{
		Target:     router.Target{Kind: degrade.ProviderDashScopeNative, Endpoint: "e", BaseURL: up.URL, UpstreamModel: "qwen-plus", CredentialPool: "p", NativeEndpoint: "text-generation"},
		Credential: credential.Credential{ID: "k", Secret: "sk-test"},
		Raw:        []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
		Canonical:  mustCanonical(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
		Stream:     false,
		Inbound:    degrade.Inbound{Protocol: degrade.ProtoOpenAIChat, Endpoint: degrade.EndpointOpenAIChat},
		OnDashScopeUsage: func(u canonical.Usage) { usageCalls++; gotUsage = u },
	}
	resp, err := p.translateNonStream(context.Background(), req, mustProject(t, req.Raw))
	if err != nil {
		t.Fatalf("非流式转换应成功: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type 应为 application/json，实际 %q", ct)
	}
	if !strings.Contains(string(body), `"object":"chat.completion"`) || !strings.Contains(string(body), `"id":"req-abc"`) {
		t.Errorf("应编码为 chat.completion 且 id 取 request_id: %s", body)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("Content-Length 应重置为新 body 长度 %d，实际 %d", len(body), resp.ContentLength)
	}
	if usageCalls != 1 {
		t.Errorf("OnDashScopeUsage 应恰调用一次，实际 %d", usageCalls)
	}
	if gotUsage.OutputTokens != 2 || gotUsage.Fidelity != canonical.FidelityAuthoritative {
		t.Errorf("回调用量应为 authoritative 且 output=2: %+v", gotUsage)
	}
}

// TestNonStreamUpstreamErrorFailover 钉死上游非 2xx 在 Call 内解码为 *canonical.Error（可 failover）。
func TestNonStreamUpstreamErrorFailover(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"code":"Throttling","message":"rate limit","request_id":"r"}`)
	}))
	defer up.Close()
	p := newTestProvider(t, up)
	req := chatRequest(t, up.URL)
	_, err := p.translateNonStream(context.Background(), req, mustProject(t, req.Raw))
	if err == nil {
		t.Fatal("上游 429 应返回错误")
	}
	cerr := canonical.AsError(err)
	if cerr.Class != canonical.ClassRateLimit {
		t.Fatalf("应分类为 rate_limit，实际 %v", cerr)
	}
}
```

（`mustCanonical` / `chatRequest` 为 helper：前者 `openaichat.Decode` 取 `&d.Request`，后者构造最小 Chat `provider.Request`。）

运行：

```bash
go test ./internal/provider/dashscopenative/ -run 'TestNonStream' -v
```

预期：**编译失败**（`translateNonStream` 尚不存在）。正确的 RED。

- [ ] **步骤 12.2：实现非流式 translator（GREEN）**

`nonstream.go` 实现 `translateNonStream(ctx, req, proj)`。步骤（全部在 `Call` 返回前，下游无首字节）：

> 本任务只完成并直接测试真实的非流式实现，不修改 `Provider.Call`。Chat 分支由任务 13
> 在流式实现也完成后原子接入，因而任何提交点都不需要临时 501 桩。

1. 由 `req.Target.NativeEndpoint` 得 `nativewire.Door`；`Door.Path()` 为空 → `ClassInternal` 报错。
2. 从 `proj` 填 `nativewire.ChatSampling`（N/PresencePenalty/Logprobs/TopLogprobs/ParallelToolCalls/MaxTokens/MaxCompletionTokens/WebSearch）。
3. `nativewire.EncodeRequest(req.Canonical, extra, door, req.Target.UpstreamModel)` 得 Native body。
4. 构造 `http.Request`：`POST baseURL+door.Path()`；头 `Content-Type: application/json`、`Accept: application/json`、`Authorization: Bearer <secret>`；白名单转发 `X-DashScope-WorkSpace` 等（复用与 passthrough 相同的头名单语义）。非流式**不**设 `X-DashScope-SSE`。
5. `p.client.Do`。`StatusCode >= 400` → 读体（设上限）→ `dashscopewire.DecodeError(status, body, header, p.now())` 返回（可 failover）。
6. `io.ReadAll` 成功 body → `nativewire.DecodeResult`。逐候选把 Native-specific 字段转成
   `canonical.Message`：tool arguments 必须先 `json.Valid`，无效即返回 `*canonical.Error`
   （仍在 `Call` 返回前，可 failover）；再组装
   `openaichat.CompletionInput{ID: res.RequestID, Model: req.Target.UpstreamModel, Created: p.now().Unix(), Choices: <逐条>, Usage: res.Usage}` → `openaichat.EncodeCompletion`。
7. **解码成功后**调用 `req.OnDashScopeUsage(res.Usage)` 恰好一次（回调非 nil 时）；`res.Usage` 为 nil 时不调用。
8. 返回 `*httpx.Response`：`Body` 换成 `io.NopCloser(bytes.NewReader(chatBody))`、`ContentLength` 与 `Content-Length` 头重置为 `len(chatBody)`、`Content-Type` 设 `application/json`、`StatusCode` 保持 200、保留 `UpstreamFirstByte`/`Latency`。

运行：

```bash
go test ./internal/provider/dashscopenative/ -run 'TestNonStream' -v
```

预期：**PASS**。

- [ ] **步骤 12.3：补齐聚焦测试（RED→GREEN 逐项）**

多候选非流式（`n=2`，两条 choices）；usage 缺失 → 回调不被调用、`chat.completion` 无 usage 字段；`request_id` 稳定映射到 `id`；`Content-Length` 与原 Native body 长度不同（证明重置）。逐项先红后绿。

- [ ] **步骤 12.4：更广验证**

```bash
go test ./internal/provider/dashscopenative/ -v
go build ./...
```

预期：全包绿，build 通过。

- [ ] **步骤 12.5：提交**

```bash
GIT_MASTER=1 git add internal/provider/dashscopenative/nonstream.go internal/provider/dashscopenative/nonstream_test.go
GIT_MASTER=1 git commit -m "Chat 到 Native 非流式转换：Call 内完成保住 failover" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

### 任务 13：Chat 到 Native 流式转换（同步 transforming reader 与多候选）

**依赖**：任务 7（帧解码）、任务 9（chunk 编码）、任务 10–12。
**文件**：新建 `internal/provider/dashscopenative/stream.go`、`internal/provider/dashscopenative/candidates.go`、`internal/provider/dashscopenative/stream_test.go`；修改 `internal/provider/dashscopenative/provider.go`、`internal/provider/dashscopenative/provider_test.go`。

> 这是本计划最复杂的一块。三条铁律：`Read` 永不返回 `(0, nil)`；`Read` 返回的错误
> 必须是 `*canonical.Error`；`Close` 立即关闭 Native body。无 goroutine、无锁、无跨请求状态。

测试 helper（`stream_test.go` 内，均为小函数）：
- `newNativeSSEUpstream(t, datas []string)`：`httptest.Server`，把每个 `datas[i]` 作为一帧
  SSE 的 `data:` 负载写出，帧头按 Native 形态 `event:result` + `:HTTP_STATUS/200`，
  用 `http.Flusher` 逐帧 flush，`Content-Type: text/event-stream`。
- `streamChatRequest(t, baseURL, includeUsage bool)`：构造最小 Chat `provider.Request`
  （`Stream:true`、`Inbound:{ProtoOpenAIChat, EndpointOpenAIChat}`、`Canonical` 取自
  `openaichat.Decode`），`includeUsage` 为真时原始 body 带 `stream_options.include_usage`。
- `streamChatRequestN(t, baseURL, n int)`：同上，另带 `"n":n`。

- [ ] **步骤 13.1：写失败测试（RED）——Read 契约与首帧预读**

`stream_test.go` 用「逐帧可控」的假上游（`http.Flusher` 分帧写出 Native SSE）。代表性两条：

```go
// TestStreamTransformBasic 钉死 Native SSE → Chat SSE：增量内容、[DONE] 收尾。
func TestStreamTransformBasic(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":"你"}}]},"usage":{"input_tokens":5,"output_tokens":1},"request_id":"r"}`,
		`{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"好"}}]},"usage":{"input_tokens":5,"output_tokens":2},"request_id":"r"}`,
	})
	defer up.Close()
	p := newTestProvider(t, up)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, true))
	if err != nil {
		t.Fatalf("流式转换应成功: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	s := string(out)
	if !strings.Contains(s, `"object":"chat.completion.chunk"`) {
		t.Errorf("应产出 chat.completion.chunk: %s", s)
	}
	if !strings.Contains(s, "data: [DONE]") {
		t.Errorf("应以 [DONE] 收尾: %s", s)
	}
}

// TestStreamReadNeverZeroNil 钉死 Read 永不返回 (0, nil)——空 delta 保活帧也得推进。
func TestStreamReadNeverZeroNil(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":""}}]},"usage":{"input_tokens":1,"output_tokens":0},"request_id":"r"}`,
		`{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]},"usage":{"input_tokens":1,"output_tokens":1},"request_id":"r"}`,
	})
	defer up.Close()
	p := newTestProvider(t, up)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, true))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 1) // 任意小缓冲，强制分批
	for {
		n, err := resp.Body.Read(buf)
		if n == 0 && err == nil {
			t.Fatal("Read 不得返回 (0, nil)")
		}
		if err != nil {
			break
		}
	}
}

// TestStreamFirstFrameMalformedFailover 钉死首帧畸形在 Call 内报错（可 failover）。
func TestStreamFirstFrameMalformedFailover(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{`{not-json`})
	defer up.Close()
	p := newTestProvider(t, up)
	_, err := p.Call(context.Background(), streamChatRequest(t, up.URL, true))
	if err == nil {
		t.Fatal("首帧畸形必须在 Call 内报错")
	}
}
```

运行：

```bash
go test ./internal/provider/dashscopenative/ -run 'TestStream' -v
```

预期：**失败**：`translateStream`/Chat 分派尚不存在，测试不能得到成功流。正确的 RED。

- [ ] **步骤 13.2：实现 `translateStream` 与首帧预读（GREEN）**

`stream.go` 实现 `translateStream(ctx, req, proj)`：

1. 门/路径、`ChatSampling`、`EncodeRequest` 同任务 12；把任务 6 已定义的
   `ChatSampling.IncrementalOutput` 置为 `true`，确保 parameters 明确含
   `incremental_output:true`，满足流式增量与思考模式硬约束。
2. 出站请求头：`X-DashScope-SSE: enable`、`Accept: text/event-stream`、`Content-Type: application/json`、`Authorization`。
3. `p.client.Do`。`StatusCode >= 400` → `dashscopewire.DecodeError` 返回（可 failover）。
4. **首帧预读**：用 `sse.NewReader(resp.Body)` 读第一条 `event:result` 事件，`nativewire.DecodeFrame` 解码。解码失败或该帧是错误帧 → 关闭 body、返回 `*canonical.Error`（此时下游无首字节，可 failover）。成功则把该帧 `*Result` 缓冲，交给 transform reader 重放。
5. 构造 `transformReader`（见下），返回 `*httpx.Response`：`Body=transformReader`、`Content-Type: text/event-stream`、`StatusCode` 200、`ContentLength=-1`（流式无定长，去掉 `Content-Length` 头）。

流式实现与首帧预读通过后，才在 `provider.go` 原子接入完整 Chat 分支；此时
`rejectUnmappable`、非流式、流式都已是真实实现：

```go
case degrade.ProtoOpenAIChat:
	proj, err := openaichat.Project(req.Raw)
	if err != nil {
		return nil, err
	}
	if err := rejectUnmappable(proj, req.Canonical); err != nil {
		return nil, err
	}
	if req.Stream {
		return p.translateStream(ctx, req, proj)
	}
	return p.translateNonStream(ctx, req, proj)
```

同时在 `provider_test.go` 增加 `TestChatInboundDispatchesTranslator`，分别用非流式与流式
子测试证明 `Provider.Call` 已接通；保留任务 10 的 Native 直通与未知入站 fail-closed 测试。

`transformReader` 结构（`stream.go`）：

```go
// transformReader 是同步 transforming io.ReadCloser：从 Native SSE 逐帧读，
// 按需编码成 Chat SSE。无 goroutine、无锁、无跨请求状态。
type transformReader struct {
	native    *sse.Reader
	body      io.ReadCloser
	first     *nativewire.Result // Call 内预读的首帧，重放一次
	replayed  bool
	pending   []byte // 待输出缓冲，跨多次 Read 按调用方缓冲大小分批交付
	cands     *candidateState
	includeUsage bool
	id, model string
	created   int64
	lastUsage *canonical.Usage
	usageCB   func(canonical.Usage)
	finished  bool
}

func (r *transformReader) Read(p []byte) (int, error)
func (r *transformReader) Close() error
```

`Read` 的循环契约：消费 Native 帧（先重放 `first`，其后 `native.Next()`），把每帧编码成 Chat SSE 追加进 `pending`；**直到 `pending` 有字节可交**才 `copy` 给 `p` 返回；遇到 EOF/错误则处理收尾（发 usage chunk（若 `includeUsage`）与 `[DONE]`，或返回 `*canonical.Error`）。**绝不**返回 `(0, nil)`：空 delta 保活帧/纯 usage 帧也要么产出字节、要么推进到下一帧。`Close` 直接 `r.body.Close()`。

运行：

```bash
go test ./internal/provider/dashscopenative/ -run 'TestStream' -v
```

预期：三条 **PASS**。

- [ ] **步骤 13.3：写失败测试（RED）——多候选与 usage chunk**

`candidates.go` 承载多候选状态。补测试：

```go
// TestStreamMultiCandidate 钉死一帧一条 chunk、每候选一个 choices 条目、全 finish 后才 [DONE]。
func TestStreamMultiCandidate(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[
		   {"finish_reason":"null","message":{"role":"assistant","content":"A1"}},
		   {"finish_reason":"null","message":{"role":"assistant","content":"B1"}}]},
		   "usage":{"input_tokens":5,"output_tokens":2},"request_id":"r"}`,
		`{"output":{"choices":[
		   {"finish_reason":"stop","message":{"role":"assistant","content":"A2"}},
		   {"finish_reason":"stop","message":{"role":"assistant","content":"B2"}}]},
		   "usage":{"input_tokens":5,"output_tokens":4},"request_id":"r"}`,
	})
	defer up.Close()
	p := newTestProvider(t, up)
	resp, err := p.Call(context.Background(), streamChatRequestN(t, up.URL, 2))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	s := string(out)
	if strings.Count(s, `"index":1`) == 0 {
		t.Errorf("应出现候选 1 的 choices 条目: %s", s)
	}
	if !strings.Contains(s, "data: [DONE]") {
		t.Errorf("全部候选 finish 后应有 [DONE]: %s", s)
	}
}

// TestStreamCandidateShrinkIsError 钉死帧内候选数缩水按流内错误处理。
func TestStreamCandidateShrinkIsError(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[
		   {"finish_reason":"null","message":{"role":"assistant","content":"A"}},
		   {"finish_reason":"null","message":{"role":"assistant","content":"B"}}]},"request_id":"r"}`,
		`{"output":{"choices":[
		   {"finish_reason":"stop","message":{"role":"assistant","content":"A"}}]},"request_id":"r"}`,
	})
	defer up.Close()
	p := newTestProvider(t, up)
	resp, err := p.Call(context.Background(), streamChatRequestN(t, up.URL, 2))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	if err == nil {
		t.Fatal("候选数缩水应产生流内错误")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("错误链应包含 *canonical.Error，实际 %T", err)
	}
}

// TestStreamUsageChunkOnlyWhenRequested 钉死 usage chunk 只在 include_usage 时输出。
func TestStreamUsageChunkOnlyWhenRequested(t *testing.T) {
	// include_usage=true：末尾有空 choices 的 usage chunk；false 时没有。
	// 两个子测试分别构造 stream_options。
}
```

运行：

```bash
go test ./internal/provider/dashscopenative/ -run 'TestStreamMultiCandidate|TestStreamCandidateShrink|TestStreamUsageChunk' -v
```

预期：**失败**（多候选/usage chunk 尚未实现）。正确的 RED。

- [ ] **步骤 13.4：实现多候选与 usage chunk（GREEN）**

`candidates.go` 实现 `candidateState`：

- 候选序号取 `output.choices` **数组下标**；记录「已见最大候选数」。某帧候选数**少于**此前帧 → 返回 `*canonical.Error`（契约违例，不补齐猜测）。
- 每个 Native 帧编码成**一条** Chat chunk，该帧出现的每个候选各占一个 `choices` 条目（自己的 `index`/`delta`/`finish_reason`）。不按候选拆成多条 chunk。
- 每候选维护：role 是否已发（首帧发 `role:assistant`）、进行中的 tool_calls（id/name 只在该调用首帧发，arguments 片段原样续发）。**不得**经过共享 `canonical.Accumulator`（它闭合时要求工具参数是合法 JSON，与「片段原样透传」冲突）。
- 全部候选都发出非 null `finish_reason` 后才合成 `[DONE]`；Native 末帧只给部分候选 finish 时，不替其余候选编造。
- `includeUsage` 为真时，`[DONE]` 前先发一条空 choices + 累计 usage 的 chunk；usage 取最后一帧的累计值（`lastUsage`），缺失则不发、不伪造。
- 每成功解码一帧，同步调用 `usageCB`（后值覆盖前值）——与 `Body.Read` 同 goroutine，无需锁。

运行：

```bash
go test ./internal/provider/dashscopenative/ -run 'TestStream' -v
```

预期：全部 **PASS**。

- [ ] **步骤 13.5：补齐聚焦测试（RED→GREEN 逐项）**

跨帧工具参数片段（arguments 被切断、原样透传、不提前解析）；`reasoning_content` 增量；首帧之后畸形帧 → `*canonical.Error` 且不重试；`Close` 关闭上游 body（用可控上游断言连接关闭）；`Read` 在任意小缓冲下分批交付不丢字节。逐项先红后绿。

- [ ] **步骤 13.6：更广验证（含竞态）**

```bash
go test ./internal/provider/dashscopenative/ -v
go test ./internal/provider/dashscopenative/ -race
go build ./...
```

预期：全包绿（含 race），build 通过。

- [ ] **步骤 13.7：提交**

```bash
GIT_MASTER=1 git add internal/provider/dashscopenative/provider.go internal/provider/dashscopenative/provider_test.go \
  internal/provider/dashscopenative/stream.go internal/provider/dashscopenative/candidates.go \
  internal/provider/dashscopenative/stream_test.go
GIT_MASTER=1 git commit -m "Chat 到 Native 流式转换：同步 transforming reader 与多候选" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 波次 3：Gateway

> 波次 3 的顺序是**先装配、后行为**：任务 14 把 Composite 接入 Build 并提供测试
> harness（任务 15/16/19 的测试基建），任务 15/16 才在其上实现过滤与 usage 优先级。
> 每个任务的提交都必须能独立编译通过，因此 harness 必须先于使用它的任务落地。

### 任务 14：Build 装配 Composite Provider 与 harness

**依赖**：任务 5（config/router 的 `NativeEndpoint`）、任务 13（完整 Composite Provider）。
**文件**：修改 `internal/gateway/build.go`、`internal/gateway/build_test.go`、`internal/gateway/harness_test.go`。

> harness（`newChatDSNativeHarness`）是任务 15/16/19 的测试基建，必须先落地。Build
> 装配与 harness 属同一原子单元：Composite 接入 Build 的同时，harness 用同一个工厂
> 构造它——两者共享 `dashScopeNativeFactory`，拆开会出现两份不一致的装配。

- [ ] **步骤 14.1：写失败测试（RED）**

`build_test.go`：构造一份含 `dashscope.native` provider + `native_endpoint` 的最小配置，断言 `Build` 成功、该 endpoint 的适配器 `Kind() == ProviderDashScopeNative` 且是 Composite（能对 Chat 入站分派）；再构造 `native_endpoint` 缺失的配置，断言 `Build` 失败（配置校验在 `config.Validate`，此处验证装配不吞错）。

运行：

```bash
go test ./internal/gateway/ -run 'TestBuildNative' -v
```

预期：**失败**（build 仍装配 passthrough，或 `NativeEndpoint` 未接线）。正确的 RED。

- [ ] **步骤 14.2：装配 Composite 并接线 `NativeEndpoint`、提供 harness（GREEN）**

`build.go`：
1. import 新增 `dsnativeprovider "github.com/yobo2u/omugw/internal/provider/dashscopenative"`。
2. provider switch 的 `case degrade.ProviderDashScopeNative:` 改为：

```go
		case degrade.ProviderDashScopeNative:
			// Composite：Native 入站复用内部 passthrough，Chat 入站走 translator。
			provs[p.Endpoint] = dsnativeprovider.New(client, nil)
```

3. router.Target 构造处补 `NativeEndpoint: t.NativeEndpoint`：

```go
			targets = append(targets, router.Target{
				Kind:           degrade.Provider(ep.Kind),
				Endpoint:       ep.Endpoint,
				BaseURL:        ep.BaseURL,
				UpstreamModel:  t.UpstreamModel,
				CredentialPool: ep.CredentialPool,
				NativeEndpoint: t.NativeEndpoint,
			})
```

4. 未实现协议族的启动错误名单已是三个已实现族（`openai.compat` / `dashscope.compatible` / `dashscope.native`），无需改动；若文案提到「直通」措辞，核对不误导。

`harness_test.go`：新增工厂与 harness（import `dsnativeprovider "github.com/yobo2u/omugw/internal/provider/dashscopenative"`）：

```go
// dashScopeNativeFactory 构造 DashScope Native Composite 适配器。
func dashScopeNativeFactory(_ degrade.Provider, client *httpx.Client) provider.Provider {
	return dsnativeprovider.New(client, nil)
}

// newChatDSNativeHarness 是 Chat -> DashScope Native 异构路径的 harness。
// door 决定候选目标的门：文本 fixture 用 text-generation，媒体 fixture 用 multimodal-generation。
func newChatDSNativeHarness(t *testing.T, door string, ups ...*upstream) *harness {
	t.Helper()
	return newHarnessFor(t, harnessConfig{
		requestPath:    "/v1/chat/completions",
		kind:           degrade.ProviderDashScopeNative,
		newHandler:     NewChatHandler,
		limits:         config.Default().Limits,
		factory:        dashScopeNativeFactory,
		nativeEndpoint: door,
	}, ups...)
}
```

并在 `harnessConfig` 增加 `nativeEndpoint string` 字段、`newHarnessFor` 构造 `router.Target` 时填入 `NativeEndpoint: cfg.nativeEndpoint`。

运行：

```bash
go test ./internal/gateway/ -run 'TestBuildNative' -v
```

预期：**PASS**。

- [ ] **步骤 14.3：更广验证**

```bash
go test ./internal/gateway/ -v
go build ./...
```

预期：全包绿，build 通过。

- [ ] **步骤 14.4：提交**

```bash
GIT_MASTER=1 git add internal/gateway/build.go internal/gateway/build_test.go internal/gateway/harness_test.go
GIT_MASTER=1 git commit -m "Build 装配 dashscope.native Composite：未实现族名单同步" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

### 任务 15：网关按媒体过滤 Native 候选

**依赖**：任务 5（`Target.NativeEndpoint`）、任务 10（Composite Provider）、任务 14（harness）。
**文件**：新建 `internal/gateway/filter.go`、`internal/gateway/filter_test.go`；`internal/gateway/handler.go` 行级集成。

> 过滤发生在矩阵选出 `ProviderDashScopeNative` 后、进入凭据循环前，且**只**影响该
> Provider 的候选。纯文本请求保留 text 与 multimodal 两类目标；含图片或音频时过滤掉
> text 目标。过滤后无目标返回 422（模型路由没有可承载媒体的 Native 门），不触达上游。

- [ ] **步骤 15.1：写失败测试（RED）**

`filter_test.go`：

```go
// TestFilterNativeTargetsByMedia 钉死媒体过滤：含媒体去掉 text 门，纯文本全保留。
func TestFilterNativeTargetsByMedia(t *testing.T) {
	text := router.Target{Kind: degrade.ProviderDashScopeNative, Endpoint: "t", NativeEndpoint: "text-generation"}
	mm := router.Target{Kind: degrade.ProviderDashScopeNative, Endpoint: "m", NativeEndpoint: "multimodal-generation"}

	t.Run("纯文本保留两门", func(t *testing.T) {
		got := filterNativeTargets([]router.Target{text, mm}, []canonical.Capability{canonical.CapTextGeneration})
		if len(got) != 2 {
			t.Fatalf("纯文本应保留两个目标，实际 %d", len(got))
		}
	})
	t.Run("含视觉去掉 text 门", func(t *testing.T) {
		got := filterNativeTargets([]router.Target{text, mm}, []canonical.Capability{canonical.CapTextGeneration, canonical.CapVisionInput})
		if len(got) != 1 || got[0].NativeEndpoint != "multimodal-generation" {
			t.Fatalf("含媒体应只留 multimodal 门，实际 %+v", got)
		}
	})
	t.Run("含音频去掉 text 门", func(t *testing.T) {
		got := filterNativeTargets([]router.Target{text, mm}, []canonical.Capability{canonical.CapAudioInput})
		if len(got) != 1 || got[0].NativeEndpoint != "multimodal-generation" {
			t.Fatalf("含音频应只留 multimodal 门，实际 %+v", got)
		}
	})
}
```

运行：

```bash
go test ./internal/gateway/ -run TestFilterNativeTargetsByMedia -v
```

预期：**编译失败**（`filterNativeTargets` 未定义）。正确的 RED。

- [ ] **步骤 15.2：实现过滤（GREEN）**

`filter.go`：

```go
package gateway

import (
	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/router"
)

// filterNativeTargets 按媒体过滤 DashScope Native 候选。
//
// 门是部署事实：text-generation 门只接受 string content，承载不了图片/音频。
// 含媒体时去掉 text 门，只留 multimodal 门；纯文本两门都保留。过滤只影响
// Native 候选，顺序即 failover 顺序，保持原有配置顺序。
func filterNativeTargets(targets []router.Target, caps []canonical.Capability) []router.Target {
	hasMedia := false
	for _, c := range caps {
		if c == canonical.CapVisionInput || c == canonical.CapAudioInput {
			hasMedia = true
			break
		}
	}
	if !hasMedia {
		return targets
	}
	out := make([]router.Target, 0, len(targets))
	for _, t := range targets {
		if t.NativeEndpoint == "multimodal-generation" {
			out = append(out, t)
		}
	}
	return out
}
```

运行：

```bash
go test ./internal/gateway/ -run TestFilterNativeTargetsByMedia -v
```

预期：**PASS**。

- [ ] **步骤 15.3：接入 `serve` 并写零候选 422 的集成测试（RED→GREEN）**

`handler.go` 的 `serve` 里，把 `targets: router.OfKind(targets, kind),` 一段改为先过滤再传入（行级集成编辑）：

```go
	candidates := router.OfKind(targets, kind)
	if kind == degrade.ProviderDashScopeNative {
		candidates = filterNativeTargets(candidates, decoded.Capabilities())
		if len(candidates) == 0 {
			return "unsupported", outbound, canonical.Newf(canonical.ClassUnsupported,
				"模型路由没有可承载该媒体的 DashScope Native 门")
		}
	}

	return h.dispatch(w, r, dispatchInput{
		caller:  caller,
		raw:     raw,
		decoded: decoded,
		targets: candidates,
		kind:    kind,
		headers: verdictHeaders(verdict),
	})
```

在 `filter_test.go` 补一条端到端：含图片的请求、模型路由只有 text 门 → 422 且上游零调用。用 `newChatDSNativeHarness(t, "text-generation", up)`（任务 14 已提供），断言 `rec.Code == 422` 且 `up.calls == 0`。

运行：

```bash
go test ./internal/gateway/ -run 'TestFilterNative|TestNativeMediaNoDoor' -v
```

预期：**PASS**。

- [ ] **步骤 15.4：更广验证**

```bash
go test ./internal/gateway/ -v
go build ./...
```

预期：全包绿，build 通过。

- [ ] **步骤 15.5：提交**

```bash
GIT_MASTER=1 git add internal/gateway/filter.go internal/gateway/filter_test.go internal/gateway/handler.go
GIT_MASTER=1 git commit -m "网关按媒体过滤 Native 候选：无门可承载即 422" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

### 任务 16：DashScope 专用 usage 回调——注入与 relay 优先级

**依赖**：任务 3（`OnDashScopeUsage` 字段）、任务 12/13（回调被 translator 调用）、任务 14（harness）。
**文件**：修改 `internal/gateway/handler.go`（`dispatch` 行级集成）；新建 `internal/gateway/usage_test.go`。

> 优先级：① Chat 响应已有 authoritative usage → 用 relay 结果；② relay **无错误返回**
> 且未取得 usage → 用回调末值；③ 其余 → `FidelityUnavailable`。第②条必须限定在 relay
> 无错误返回上——流中断时 usage 已被故意抹成 unavailable，不得用回调值捡回来。

- [ ] **步骤 16.1：写失败测试（RED）**

`usage_test.go` 三条代表性用例（用 `newChatDSNativeHarness`，任务 14 已提供）：

```go
// TestUsageFromResponseBody 钉死优先级①：非流式响应自带 usage，用 relay 结果。
// TestUsageFromCallbackWhenRelayLacks 钉死优先级②：流式不带 usage chunk 时，用回调末值。
// TestUsageUnavailableOnStreamAbort 钉死优先级③：流中断时保持 unavailable，不用回调值。
```

每条断言 `obs.Metrics` 观测到的 usage fidelity 与数值（通过 `hs.metrics` 或注入的 registry 采集）。

运行：

```bash
go test ./internal/gateway/ -run 'TestUsage' -v
```

预期：**失败**（尚未注入回调、未做优先级）。正确的 RED。

- [ ] **步骤 16.2：实现注入与优先级（GREEN）**

`handler.go` 的 `dispatch` 里，`prov.Call` 之前声明回调闭包并注入（**仅** `ProviderDashScopeNative`）：

```go
			// DashScope 专用 usage 回调：只为 Native 注入，其他 Provider 必须为 nil。
			var dsUsage canonical.Usage
			var hasDSUsage bool
			var onDSUsage func(canonical.Usage)
			if in.kind == degrade.ProviderDashScopeNative {
				onDSUsage = func(u canonical.Usage) { dsUsage = u; hasDSUsage = true }
			}

			resp, err := prov.Call(r.Context(), provider.Request{
				Target:         target,
				Credential:     lease.Credential,
				Raw:            in.raw,
				Canonical:      &in.decoded.Request,
				Stream:         in.decoded.Request.Stream,
				Inbound:        degrade.Inbound{Protocol: h.in.protocol, Endpoint: degrade.Endpoint(h.in.upstreamPath(r))},
				Header:         r.Header,
				OnDashScopeUsage: onDSUsage,
			})
```

`relay` 返回后、`ObserveUsage` 之前，按优先级确定最终 usage：

```go
			usage, rerr := h.relay(w, resp, in)
			// usage 优先级：relay 已给出 authoritative 就用它；仅在 relay 无错误返回
			// 且未取得 usage 时才采用回调末值；流中断一律 unavailable，不捡回调值。
			if rerr == nil && usage.Fidelity == canonical.FidelityUnavailable && hasDSUsage {
				usage = dsUsage
			}
			h.deps.Metrics.ObserveUsage(string(in.kind), usage)
```

运行：

```bash
go test ./internal/gateway/ -run 'TestUsage' -v
```

预期：**PASS**。

- [ ] **步骤 16.3：更广验证（含竞态）**

```bash
go test ./internal/gateway/ -v
go test ./internal/gateway/ -race
go build ./...
```

预期：全包绿（含 race），build 通过。

- [ ] **步骤 16.4：提交**

```bash
GIT_MASTER=1 git add internal/gateway/handler.go internal/gateway/usage_test.go
GIT_MASTER=1 git commit -m "DashScope 专用 usage 回调：注入与 relay 优先级" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 波次 4：证据与投放

> 本波次受 ADR-0001 与「真实凭据」双重约束。任务 17（真实录制）与任务 20/21（真实
> smoke）是**硬投放门槛**：没有真实 DashScope 凭据就标 **BLOCKED（缺凭据）** 停住，
> **绝不**用 httptest 伪造录制充数。波次 0–3 的离线任务不依赖凭据，可先行完成。

### 任务 17：真实录制 + 路径 fixture（硬门槛）

**依赖**：任务 14–16（harness、装配、过滤与 usage 全部就位，录制时网关行为与线上一致）。
**文件**：新建录制器 `tests/smoke/record_dsnative_test.go`（`-tags=smoke`）；产出 `testdata/routes/openai.chat__dashscope.native/*.json`。

> **门槛**：本任务需要真实 DashScope API Key 与一个能支撑所声明组合的真实模型。
> 若执行环境没有凭据，或找不到支撑组合的真实模型，标 **BLOCKED（缺凭据/缺模型）**
> 并停在本任务；**不得**构造假 fixture，也不得把多份互不相容的模型证据拼成「可组合」。
> 真遇到「没有一个真实模型支持组合」，必须回到设计阶段收窄能力承诺，而不是降级证据标准。

- [ ] **步骤 17.1：写录制器（转发代理 + 脱敏 + 落盘）**

`tests/smoke/record_dsnative_test.go`（build tag `smoke`，仅在 `OMUGW_SMOKE=1` 且提供
`DASHSCOPE_API_KEY`、传 `-record` 时运行，否则 `t.Skip`）。录制器职责：

先在文件级声明 `var record = flag.Bool("record", false, "录制真实 DashScope fixture")`；
测试在 `!*record` 时 `t.Skip`，避免 `go test ... -record` 因未知 flag 直接失败。

1. 起一个**转发录制代理**（`httptest.Server`）：把收到的请求原样转发到真实
   DashScope base URL（`https://dashscope.aliyuncs.com`），同时捕获
   「网关发来的上游请求」（method/path/headers/body）与「DashScope 返回的响应」
   （status/headers/body 或 SSE）。流式响应用生产同款 `sse.NewReader` 逐事件解析，
   每解析出一条事件就立刻经 `sse.Writer` 转发并记入 `sse.events`；`sse.frames`
   记录每次 flush 覆盖的事件数。**不得**把 `http.Response.Body.Read` 返回的任意网络
   chunk 当成 SSE 帧边界——HTTP/TCP 不保留上游 `Write` 调用边界。该录制忠实保留
   协议事件边界与到达顺序；若未来必须研究事件内 TCP 切分，应另建 raw-byte fixture，
   不能滥用当前 `Frames []int`（它表达的是每次回放 Write 含多少完整事件）。
2. 用 `gateway.Build` 组装一套网关，`dashscope.native` provider 的 `base_url` 指向
   录制代理，`credential_pool` 放真实 `DASHSCOPE_API_KEY`，target 声明对应
   `native_endpoint`。
3. 对每个能力用例，构造客户端 Chat 请求打网关，取三样东西拼成 `testkit.Fixture`：
   `Request`=客户端请求（path 恒为 `/v1/chat/completions`）、`Upstream`=代理捕获的
   上游请求（method/path/body）、`Response`=代理捕获的上游响应。
4. 用 `testkit.SanitizeHeaders` 脱敏（`authorization` 等一律 `<redacted>`），
   `testkit.Save` 落到 `testdata/routes/openai.chat__dashscope.native/<name>.json`。

- [ ] **步骤 17.2：真实运行录制器，产出全部 fixture**

```bash
OMUGW_SMOKE=1 DASHSCOPE_API_KEY=sk-xxxx \
  go test ./tests/smoke/ -tags=smoke -run TestRecordChatDSNative -record -v
```

需产出（文件名即能力名，DEGRADE 能力必须有同名举证文件）：

| 文件 | 能力/场景 | 门 |
|---|---|---|
| `basic.json` | text_generation（非流式基础） | text-generation |
| `streaming.json` | streaming（Native SSE → Chat SSE + usage + `[DONE]`） | text-generation |
| `tool_calling.json` | tool_calling（声明、历史、调用与跨帧 arguments） | text-generation |
| `vision_input.json` | vision_input（image URL/data URI） | multimodal-generation |
| `audio_input.json` | audio_input | multimodal-generation |
| `reasoning.json` | reasoning（`enable_thinking` 与 reasoning_content） | text-generation |
| `parallel_tool_calls.json` | parallel_tool_calls DEGRADE（显式字段映射 + 降级头） | text-generation |
| `structured_output.json` | structured_output DEGRADE（json_object/json_schema 保留 + 降级头） | text-generation |
| `web_search.json` | web_search DEGRADE（options → enable_search + 丢失项可见 + 降级头） | text-generation |
| `combined.json` | vision + tools + web_search + structured_output 组合（证明可组合） | multimodal-generation |
| `multi_candidate_nonstream.json` | `n=2` 非流式（多候选搭载在 text_generation 的证据） | text-generation |
| `multi_candidate_stream.json` | `n=2` 流式（多候选搭载在 streaming 的证据） | text-generation |
| `parallel_tool_calls_default.json` | `parallel_tool_calls` 缺省时出站体含 `true` | text-generation |

每份 fixture 必须同时断言（录制时即填好 `upstream`）：上游 method、path、Authorization
（`<redacted>`）、关键请求头（流式带 `X-DashScope-SSE: enable`）与完整请求 JSON；下游
状态、响应 wire、usage、finish reason、降级头。

- [ ] **步骤 17.3：校验 fixture 合法性与离线回放**

```bash
go test ./internal/degrade/ -run TestImplementedRoutesHaveFixtures -v   # 此时路径未兑现，仅验证目录/文件形状
go vet ./...
```

并逐份人工检查：`testkit.Validate` 通过（无 body+sse 并存、upstream 三要素齐、无未脱敏
头）；流式 fixture 的 `sse.frames` 合计等于事件数。确认**没有**任何凭据残留。

- [ ] **步骤 17.4：提交**

```bash
GIT_MASTER=1 git add testdata/routes/openai.chat__dashscope.native/ tests/smoke/record_dsnative_test.go
GIT_MASTER=1 git commit -m "Chat 到 DashScope Native 真实录制 fixture：九项能力与多候选" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

### 任务 18：兑现九项能力（ADR-0001 窗口打开）

**依赖**：任务 17（真实 fixture 已落地——先有证据，后有兑现）。
**文件**：修改 `internal/degrade/rules_phase1.go`、`internal/degrade/matrix_test.go`；新建 `internal/degrade/chat_dsnative_test.go`；生成 `docs/degradation-matrix.md`。

> **从本任务开始到任务 19 全绿为止，不得中途停下。** `Redeem` 一落地路径即「已兑现」，
> 证明兑现的回放在任务 19 才提交；窗口内每个已提交状态都是绿的（fixture 与白名单已就位），
> 但兑现的合法性靠随后立即落地的回放证据。

- [ ] **步骤 18.1：写失败测试（RED）——兑现形状与分数**

新建 `internal/degrade/chat_dsnative_test.go`（对照 `chat_dscompat_test.go` 的形状）：

```go
package degrade

import (
	"reflect"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// redeemedChatDSNative 是 /v1/chat/completions 门在 dashscope.native 上兑现的九项，
// 按 AllCapabilities 顺序——与 RedeemedAt 输出顺序一致。
var redeemedChatDSNative = []canonical.Capability{
	canonical.CapTextGeneration,
	canonical.CapStreaming,
	canonical.CapToolCalling,
	canonical.CapParallelToolCalls,
	canonical.CapStructuredOutput,
	canonical.CapReasoning,
	canonical.CapVisionInput,
	canonical.CapAudioInput,
	canonical.CapWebSearch,
}

// TestChatDSNativeRouteIsHeterogeneous 钉死身份：完整重编码的异构路径，非同源快通道。
func TestChatDSNativeRouteIsHeterogeneous(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := m.Route(ProtoOpenAIChat, ProviderDashScopeNative)
	if !ok {
		t.Fatal("openai.chat -> dashscope.native 未注册")
	}
	if r.IsHomogeneous() {
		t.Error("该路径是完整重编码异构转换，不得标记为同源快通道")
	}
	// 设计处置：6 PASS + 3 DEGRADE + 2 REJECT = 11 项可表达能力，设计分 7.5/11。
	p := r.Preservation(m.Availability(), Endpoint(""))
	if p.Passthrough != 6 || p.Degrade != 3 || p.Reject != 2 {
		t.Errorf("设计处置 = pass %d deg %d rej %d，期望 6/3/2", p.Passthrough, p.Degrade, p.Reject)
	}
	if want := 7.5 / 11.0; p.DesignScore() != want {
		t.Errorf("设计保留度 = %.3f，期望 %.3f（7.5/11）", p.DesignScore(), want)
	}
}

// TestChatDSNativeRedemptionIsExactlyNineCapabilities 钉死兑现集合精确为九项。
func TestChatDSNativeRedemptionIsExactlyNineCapabilities(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := m.Route(ProtoOpenAIChat, ProviderDashScopeNative)
	if !ok {
		t.Fatal("openai.chat -> dashscope.native 未注册")
	}
	if got := r.RedeemedAt(EndpointOpenAIChat); !reflect.DeepEqual(got, redeemedChatDSNative) {
		t.Errorf("兑现集合 = %v，期望 %v", got, redeemedChatDSNative)
	}
	for _, c := range []canonical.Capability{canonical.CapFileInput, canonical.CapAudioOutput} {
		if r.Redeems(EndpointOpenAIChat, c) {
			t.Errorf("%q 是 REJECT，不应被兑现", c)
		}
	}
	// 九项全兑：门可用分与设计分合一，都是 7.5/11 ≈ 0.682。
	p := r.Preservation(m.Availability(), EndpointOpenAIChat)
	if want := 7.5 / 11.0; p.AvailableScore() != want {
		t.Errorf("门 %s 可用分 = %.3f，期望 %.3f", EndpointOpenAIChat, p.AvailableScore(), want)
	}
	if p.Gated() {
		t.Error("九项可交付能力已全部兑现，这门不应再有未投放格子")
	}
}

// TestChatDoorStillPrefersCompatibleOverNative 钉死同门选路不变：
// dashscope.compatible（8/11 ≈ 0.727）仍优先于 dashscope.native（7.5/11 ≈ 0.682），
// OutboundPreference 不改。
func TestChatDoorStillPrefersCompatibleOverNative(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	compat := mustRoute(t, m, ProtoOpenAIChat, ProviderDashScopeCompatible)
	native := mustRoute(t, m, ProtoOpenAIChat, ProviderDashScopeNative)
	cs := compat.Preservation(m.Availability(), EndpointOpenAIChat).AvailableScore()
	ns := native.Preservation(m.Availability(), EndpointOpenAIChat).AvailableScore()
	if cs <= ns {
		t.Errorf("compatible 门可用分 %.3f 应严格高于 native 门 %.3f", cs, ns)
	}
}
```

运行：

```bash
go test ./internal/degrade/ -run 'TestChatDSNative' -v
```

预期：**失败**——`RedeemedAt` 为空（尚未 `Redeem`），`TestImplementedRoutesAreExplicit`
也尚未收录本路径。正确的 RED。

- [ ] **步骤 18.2：修正 `parallel_tool_calls` note 并兑现（GREEN）**

`rules_phase1.go` 的 `chatToDSNative`：
1. 把 `parallel_tool_calls` 的 DEGRADE note 从「无显式开关可映射」修正为同时写明两件事：

```go
		Degrade("DashScope Native 有显式 parallel_tool_calls 开关，但模型支持面与并行行为 "+
			"不具路径级全局保证；客户端未提交该字段时网关按 OpenAI 默认显式注入 true"+
			"（Native 默认 false，不注入会静默变成串行）",
			canonical.CapParallelToolCalls).
```

2. 在 `Reject(...)` 之后、`Build()` 之前追加兑现：

```go
		// 兑现门槛是端到端真实 fixture 通过（ADR-0001）：十三份用例在
		// testdata/routes/openai.chat__dashscope.native/，回放与上游请求断言在
		// internal/gateway/chat_dsnative_conformance_test.go。
		// file_input / audio_output 是 REJECT，不在兑现之列。
		Redeem(EndpointOpenAIChat,
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
```

`matrix_test.go`：
- `TestImplementedRoutesAreExplicit` 的 `want` 增加：

```go
		string(ProtoOpenAIChat) + " -> " + string(ProviderDashScopeNative): true,
```

- `TestRedeemedCapabilitiesAreExplicit` 的 `want` 增加：

```go
		string(ProtoOpenAIChat) + " -> " + string(ProviderDashScopeNative) +
			" @ " + string(EndpointOpenAIChat): {
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapParallelToolCalls,
			canonical.CapStructuredOutput,
			canonical.CapReasoning,
			canonical.CapVisionInput,
			canonical.CapAudioInput,
			canonical.CapWebSearch,
		},
```

运行：

```bash
go test ./internal/degrade/ -run 'TestChatDSNative|TestImplementedRoutes|TestRedeemedCapabilities|TestImplementedRoutesHaveFixtures' -v
```

预期：**PASS**（fixture 已在任务 17 落地，`TestImplementedRoutesHaveFixtures` 通过）。

- [ ] **步骤 18.3：重新生成矩阵文档**

```bash
make matrix-update
```

人工审阅 `docs/degradation-matrix.md` 的 diff：`openai.chat -> dashscope.native` 行从
PLANNED 变为已兑现、九项能力标记投放；**派生的 `openai.responses -> dashscope.native`
行可能随之变化**（派生路径不继承兑现，但文档重排可能移动该行）——确认变化只来自本次
兑现，`CONTEXT.md` 一律不动。

- [ ] **步骤 18.4：更广验证**

```bash
go test ./internal/degrade/ -v
make matrix
go build ./...
```

预期：degrade 全包绿，`make matrix` 通过，build 通过。

- [ ] **步骤 18.5：提交**

```bash
GIT_MASTER=1 git add internal/degrade/rules_phase1.go internal/degrade/matrix_test.go \
  internal/degrade/chat_dsnative_test.go docs/degradation-matrix.md
GIT_MASTER=1 git commit -m "兑现 openai.chat 到 dashscope.native 九项能力：白名单与矩阵文档同步" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

### 任务 19：一致性回放与负例（ADR-0001 窗口关闭）

**依赖**：任务 18（路径已兑现，回放才能拿到 200）。
**文件**：新建 `internal/gateway/chat_dsnative_conformance_test.go`、`internal/gateway/chat_dsnative_negative_test.go`；生成 `testdata/routes/openai.chat__dashscope.native/golden/*.txt`。

- [ ] **步骤 19.1：写回放测试（工作树状态，暂不提交）**

`chat_dsnative_conformance_test.go`（对照 `chat_dscompat_conformance_test.go`，但按
fixture 的 `upstream.path` 选门）：

```go
package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/testkit"
)

const chatDSNativeRouteFixtures = "../../testdata/routes/openai.chat__dashscope.native"

// chatDSNativeDegradedHeaders 按用例名钉死降级头必须包含的能力项，独立于 golden。
var chatDSNativeDegradedHeaders = map[string]string{
	"parallel_tool_calls": "parallel_tool_calls=",
	"structured_output":   "structured_output=",
	"web_search":          "web_search=",
}

// doorFromUpstreamPath 按 fixture 声明的上游路径反推应装配的门。
func doorFromUpstreamPath(t *testing.T, path string) string {
	t.Helper()
	switch path {
	case nativewire.TextGenerationPath:
		return "text-generation"
	case nativewire.MultimodalGenerationPath:
		return "multimodal-generation"
	default:
		t.Fatalf("fixture 上游路径 %q 不是已知 Native 门", path)
		return ""
	}
}

// TestChatDSNativeRouteConformance 回放全部 fixture：除客户端响应 golden 外，
// 必须逐条断言上游实际收到的 method/path/鉴权/请求体——只比客户端响应会漏掉
// 「Provider 没做映射、fixture 仍返回成功」的假绿。
func TestChatDSNativeRouteConformance(t *testing.T) {
	for _, f := range testkit.LoadDir(t, chatDSNativeRouteFixtures) {
		t.Run(caseName(f.Name), func(t *testing.T) {
			if f.Upstream == nil {
				t.Fatal("异构路径的 fixture 必须带 upstream 断言，否则上游映射无从对账")
			}
			var (
				gotMethod string
				gotPath   string
				gotHeader http.Header
				gotBody   []byte
				gotErr    error
			)
			up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotPath = r.URL.Path
				gotHeader = r.Header.Clone()
				gotBody, gotErr = io.ReadAll(r.Body)
				writeFixtureResponse(t, w, f)
			})
			hs := newChatDSNativeHarness(t, doorFromUpstreamPath(t, f.Upstream.Path), up)

			body, err := json.Marshal(f.Request.Body)
			if err != nil {
				t.Fatal(err)
			}
			rec := hs.do(t, string(body), true)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200: %s", rec.Code, rec.Body.String())
			}
			if gotErr != nil {
				t.Fatalf("读取上游收到的 body 失败: %v", gotErr)
			}
			if gotMethod != f.Upstream.Method {
				t.Errorf("上游收到 method %q，期望 %q", gotMethod, f.Upstream.Method)
			}
			if gotPath != f.Upstream.Path {
				t.Errorf("上游收到路径 %q，期望 %q", gotPath, f.Upstream.Path)
			}
			if auth := gotHeader.Get("Authorization"); auth != "Bearer sk-a" {
				t.Errorf("上游收到 Authorization %q，期望网关凭据 Bearer sk-a", auth)
			}
			testkit.AssertJSONEqual(t, f.Upstream.Body, gotBody, "上游收到的请求体语义不符")

			wantDegraded := chatDSNativeDegradedHeaders[caseName(f.Name)]
			gotDegraded := rec.Header().Get(degrade.DegradationHeader)
			if wantDegraded == "" {
				if gotDegraded != "" {
					t.Errorf("该用例不应有降级头，实际 %q", gotDegraded)
				}
			} else if !strings.Contains(gotDegraded, wantDegraded) {
				t.Errorf("%s 应包含 %q，实际 %q", degrade.DegradationHeader, wantDegraded, gotDegraded)
			}

			golden := filepath.Join(chatDSNativeRouteFixtures, "golden", caseName(f.Name)+".txt")
			testkit.Golden(t, golden, []byte(renderResult(rec)))
		})
	}
}
```

`chat_dsnative_negative_test.go`：422/501/400 负例，逐条断言上游零调用。至少覆盖：
无落点字段（`frequency_penalty` 等）422 且点名；非流式 + `reasoning_effort:"high"` 422；
非流式 + `reasoning_effort:"none"` **放行**（none 正例）；`n=2 + tools` 422；`n=2 + none`
放行；含图片但只有 text 门 → 422（媒体过滤）；`modalities:["audio"]` 422（能力门）。每条
用 `up.calls` 断言出门次数为 0（放行用例除外）。

- [ ] **步骤 19.2：定向生成 golden 并人工审阅**

```bash
go test ./internal/gateway/ -run TestChatDSNativeRouteConformance -update
```

逐份审阅 `golden/*.txt` 的 diff：确认是 `relayStream`/`relayJSON` 重新解析、重新序列化
后的规范化结果（空白与字段排布被规范化是预期的），语义与 fixture 一致。流式 golden 的
分片差异不必追查。

- [ ] **步骤 19.3：提交（golden 已在步骤 19.2 审阅通过）**

```bash
GIT_MASTER=1 git add testdata/routes/openai.chat__dashscope.native/golden/ \
  internal/gateway/chat_dsnative_conformance_test.go internal/gateway/chat_dsnative_negative_test.go
GIT_MASTER=1 git commit -m "Chat 到 DashScope Native 一致性回放与负例：上游请求逐条对账" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

> golden 与其引用方（回放测试）同一提交落地：golden 是纯数据，审阅已在步骤 19.2
> 完成，与测试文件同提交保证任一中间状态都有测试引用、不出现孤儿 golden。
> 至此 ADR-0001 窗口关闭：兑现与回放证据都已落地。

- [ ] **步骤 19.4：更广验证**

```bash
go test ./internal/gateway/ -v
make test
go build ./...
```

预期：全包绿，`make test` 通过，build 通过。

---

### 任务 20：`tests/smoke` 冒烟骨架（真实上游端到端）

**依赖**：任务 14（网关已装配，真实上游可打通）。`make smoke` 已指向 `./tests/smoke/... -tags=smoke`，目录必须存在。
**文件**：新建 `tests/smoke/smoke_test.go`（`-tags=smoke`）。

- [ ] **步骤 20.1：写冒烟基建**

`tests/smoke/smoke_test.go`（build tag `smoke`）：
- 读取环境变量：`OMUGW_SMOKE`（make 已校验）、`DASHSCOPE_API_KEY`、可选
  `OMUGW_SMOKE_BASE_URL`（缺省 `https://dashscope.aliyuncs.com`）、`OMUGW_SMOKE_MODEL`。
- 缺凭据时 `t.Skip("缺少 DASHSCOPE_API_KEY，跳过真实冒烟")`——**不得**用假上游顶替。
- 提供 helper：用 `gateway.Build` 组装一套指向真实 DashScope 的网关（`dashscope.native`
  provider + `native_endpoint`），返回可发请求的入口（`httptest.Server` 包装 `Built.Mux`）。
- 提供断言 helper：状态码、`chat.completion`/`chat.completion.chunk` 形状、usage
  authoritative、降级头。

运行（无凭据时应跳过而非失败）：

```bash
OMUGW_SMOKE=1 go test ./tests/smoke/ -tags=smoke -v
```

预期：缺 `DASHSCOPE_API_KEY` 时输出 `SKIP`。

- [ ] **步骤 20.2：提交**

```bash
GIT_MASTER=1 git add tests/smoke/smoke_test.go
GIT_MASTER=1 git commit -m "tests/smoke 冒烟骨架：真实上游端到端" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

### 任务 21：Chat 到 DashScope Native 真实 smoke 用例（硬门槛）

**依赖**：任务 20（冒烟基建）、任务 18/19（路径已兑现）。
**文件**：新建 `tests/smoke/chat_dsnative_test.go`（`-tags=smoke`）。

> **门槛**：必须真实打到 DashScope。**没有凭据或缺乏支撑组合的真实模型时标
> BLOCKED（缺凭据/缺模型）**，不得用 httptest 顶替，不得伪造。

- [ ] **步骤 21.1：写真实 smoke 用例**

`tests/smoke/chat_dsnative_test.go`（build tag `smoke`），至少四探针：
1. **非流式基础**：纯文本 → 200，`chat.completion`，content 非空，usage authoritative。
2. **流式 + usage**：`stream:true` + `stream_options.include_usage:true` → 逐条 chunk、
   末尾空 choices usage chunk、`[DONE]` 收尾。
3. **推理（流式）**：`reasoning_effort:"low"` + `stream:true` → 200，`reasoning_content`
   或思考帧出现（按模型行为断言不报错即可，避免对具体文案断言）。
4. **组合**：在**同一个真实模型**上发 `vision + tools + web_search + structured_output`
   组合 → 200 且各能力可见。若没有任何真实模型支撑该组合，标 BLOCKED 并按设计文档
   回到设计阶段收窄承诺，**不得**拼凑。

运行：

```bash
OMUGW_SMOKE=1 DASHSCOPE_API_KEY=sk-xxxx OMUGW_SMOKE_MODEL=<真实模型> \
  go test ./tests/smoke/ -tags=smoke -run TestChatDSNativeSmoke -v
```

预期：有凭据时全部 **PASS**；无凭据时 SKIP（由 make/环境决定，不在离线 CI 跑）。

- [ ] **步骤 21.2：提交**

```bash
GIT_MASTER=1 git add tests/smoke/chat_dsnative_test.go
GIT_MASTER=1 git commit -m "Chat 到 DashScope Native 真实 smoke 用例" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

### 任务 22：知识库同步（provenance / README / AGENTS）

**依赖**：任务 18/19（投放已落地，状态描述才有依据）。
**文件**：修改 `docs/provenance.yaml`、`README.md`、根 `AGENTS.md`、相关包 `AGENTS.md`。

- [ ] **步骤 22.1：更新 `docs/provenance.yaml`**

在 `modules:` 列表追加（`implementation_type: clean-room`，引用契约调研所用的阿里云官方
文档；思路与 `internal/protocol/dashscopewire` 条目一致）：

```yaml
  - path: internal/protocol/dashscopenative
    implementation_type: clean-room
    reference:
      - name: 阿里云百炼 API 参考
        kind: public-documentation
        url: https://help.aliyun.com/zh/model-studio/
      - name: DashScope 流式输出
        kind: public-documentation
        url: https://help.aliyun.com/zh/model-studio/stream
    note: |
      Native 出站请求编码与成功响应/事件解码，依据公开文档实现；
      未文档化事项（裸 HTTP 顶层、两种 null、终止帧）以真实录制复核。

  - path: internal/provider/dashscopenative
    implementation_type: original
    note: |
      Chat 到 DashScope Native 的 Composite 出站适配器（非流式完整转换 +
      同步 transforming reader），基于本仓库 Canonical 与降级矩阵自行设计。
```

- [ ] **步骤 22.2：更新 README 与根 AGENTS 的投放状态**

- `README.md`：把「其中 4 条已实现」改为「5 条已实现」，补一句 `openai.chat →
  dashscope.native`（首条请求与响应都完整重编码的异构路径，`/v1/chat/completions`
  门兑现 9 项能力，设计分与可用分均 7.5/11 ≈ 0.682；`dashscope.compatible` 的 0.727
  仍优先）。
- 根 `AGENTS.md` 的「当前状态」与相关表格同步：路径数、通车清单、`native_endpoint`
  门声明、`OnDashScopeUsage` 专用耦合、`EffortNone` 语义。**不改 `CONTEXT.md`**
  （本设计没有引入新领域术语）。
- 相关包 `AGENTS.md`（`internal/provider/`、`internal/gateway/`、`internal/degrade/`、
  `internal/protocol/openaichat/`、`internal/protocol/dashscopenative/`）：按既有体例补
  新文件职责与语义边界（异构 ≠ 同源、none 不报告推理、usage 回调专用、流式首字节后不重试）。

- [ ] **步骤 22.3：提交**

```bash
GIT_MASTER=1 git add docs/provenance.yaml README.md AGENTS.md \
  internal/provider/AGENTS.md internal/gateway/AGENTS.md internal/degrade/AGENTS.md \
  internal/protocol/openaichat/AGENTS.md internal/protocol/dashscopenative/AGENTS.md
GIT_MASTER=1 git commit -m "知识库跟上第五条通车路径：首条完整重编码异构路径" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

> 注：并非每个包都必有 `AGENTS.md`，提交前用 `ls` 核对实际存在的文件，只加确实存在
>（或本计划新建）的那些；不存在的包不强行创建，除非该包已有 `AGENTS.md` 体例。

---

## 任务 23：最终闸门（不产生提交）

**依赖**：任务 1–22 全部落地。

- [ ] **步骤 23.1：格式与静态检查**

```bash
gofmt -l .
go vet ./...
go build ./...
```

预期：三条命令全部无输出、退出码 0。

- [ ] **步骤 23.2：全量测试与矩阵闸门**

```bash
make test
make test-race
make matrix
make check
```

预期：全部通过。`make check` 覆盖 fmt-check + vet + test + matrix，等价 CI。
`make test-race` 必须对 `internal/provider/dashscopenative` 与 `internal/gateway` 无竞态告警。

- [ ] **步骤 23.3：LSP 诊断**

对全部改动过的包跑 `lsp_diagnostics`，严重度取 `error`，预期零错误：

- `internal/canonical`
- `internal/protocol/openaichat`
- `internal/protocol/dashscopenative`
- `internal/provider`、`internal/provider/passthrough`、`internal/provider/dashscopecompat`、`internal/provider/dashscopenative`、`internal/provider/providertest`
- `internal/config`、`internal/router`
- `internal/gateway`
- `internal/degrade`

- [ ] **步骤 23.4：文件大小复查**

```bash
wc -l internal/provider/dashscopenative/*.go \
      internal/protocol/dashscopenative/encode_request.go internal/protocol/dashscopenative/result.go \
      internal/protocol/openaichat/projection.go internal/protocol/openaichat/encode_response.go \
      internal/protocol/openaichat/encode_stream.go internal/gateway/filter.go
```

预期：每个**实现**文件（非 `_test.go`）低于 250 纯代码行；`stream.go`/`candidates.go`
拆分后各自不超限。任一实现文件超限，按职责拆分后再跑步骤 23.2。测试文件不设硬上限，
但与既有惯例持平。

- [ ] **步骤 23.5：真实 smoke（硬门槛）**

```bash
OMUGW_SMOKE=1 DASHSCOPE_API_KEY=sk-xxxx OMUGW_SMOKE_MODEL=<真实模型> make smoke
```

预期：有凭据时 `tests/smoke` 全部 PASS；无凭据时按 make 逻辑 SKIP，且任务 17/21 已按
**BLOCKED（缺凭据）** 如实标注。**不得**用 httptest 顶替真实 smoke。

- [ ] **步骤 23.6：仓库状态核对**

```bash
GIT_MASTER=1 git status && GIT_MASTER=1 git log --oneline -24
```

预期：工作树干净；基线之上恰好 22 个新提交，主题与「提交总览」表逐条一致；无推送。

---

## 关键不变量自查（易漏点逐项对账）

| # | 不变量 | 证据位置 |
|---|---|---|
| 1 | `none` 两条正例：非流式 + `reasoning_effort:"none"` 放行且发 `enable_thinking:false`；`n>1 + none` 放行 | 任务 2、任务 6（编码）、任务 11（`TestNonePositiveNonStreamReasoning` / `TestNonePositiveNWithNone`） |
| 2 | `stream_options` 严格验证但**不**为 Compatible 剥离 | 绑定决策 13；任务 4 在 `openaichat.Decode` 做严格子解码；`dashscopecompat` 不触碰该字段，既有 `openai.chat__dashscope.compatible/streaming.json` 的 upstream 断言已钉死原样带给上游 |
| 3 | `parallel_tool_calls` 缺省显式注入 `true` | 绑定决策 12；任务 6（编码器注入）、任务 17（`parallel_tool_calls_default.json`）、任务 18（note 同时写明两件事） |
| 4 | 多候选**不**经过共享 `canonical.Accumulator` | 绑定决策 9；任务 13（`candidates.go` 自维护按候选状态） |
| 5 | 流式首帧在 `Call` 内预读，畸形首帧仍可 failover | 绑定决策 8；任务 13（`TestStreamFirstFrameMalformedFailover`） |
| 6 | transforming reader 的 `Read` 永不返回 `(0, nil)` | 绑定决策 7；任务 13（`TestStreamReadNeverZeroNil`，任意小缓冲分批） |
| 7 | transforming reader 返回的错误必须是 `*canonical.Error` | 绑定决策 7；任务 13（`TestStreamCandidateShrinkIsError` 断言类型） |
| 8 | 非流式转换后 `Content-Length` 重置为新 body 长度 | 任务 12（`TestNonStreamTransform` 断言 `resp.ContentLength`） |
| 9 | usage 回调末值**仅**在 relay 无错误返回且未取得 usage 时采用；流中断不捡回调值 | 绑定决策 11；任务 16（`TestUsageUnavailableOnStreamAbort`） |
| 10 | 全部候选都发出非 null `finish_reason` 后才合成 `[DONE]` | 绑定决策 9；任务 13（`TestStreamMultiCandidate`） |
| 11 | 帧内候选数缩水按流内错误处理，不补齐猜测 | 任务 13（`TestStreamCandidateShrinkIsError`） |
| 12 | DEGRADE 能力有同名举证 fixture | 任务 17（`parallel_tool_calls.json` / `structured_output.json` / `web_search.json`）+ `checkRouteFixtures` |
| 13 | `AllCapabilities()` 与 `OutboundPreference` 保持不变 | 绑定决策 9/14；全计划不编辑这两处；任务 18（`TestChatDoorStillPrefersCompatibleOverNative` 钉死 0.727 仍优先 0.682） |
| 14 | 派生的 `openai.responses -> dashscope.native` 矩阵文档行可能变化，须人工审阅 | 任务 18.3（`make matrix-update` 后核对，派生路径不继承兑现） |
| 15 | `CONTEXT.md` 保持不变（无新领域术语） | 任务 22 显式不改 `CONTEXT.md` |
| 16 | 异构翻译不读 `Extensions` | 绑定决策 4；任务 4 投影、任务 6/12/13 均从 Canonical + 投影取值 |

## 完成标准自查（对照设计文档逐条勾选）

| 设计文档要求 | 证据位置 |
|---|---|
| 九项能力在 `EndpointOpenAIChat` 有逐项真实 fixture 证据并显式兑现 | 任务 17（真实录制）、任务 18（`Redeem`）、任务 19（回放）；`TestChatDSNativeRedemptionIsExactlyNineCapabilities` |
| 组合 fixture 与真实 smoke 证明能力可组合 | 任务 17（`combined.json`）、任务 21（组合探针） |
| `native_endpoint` 在配置、router、过滤与 Provider 路径选择中只有一个事实来源 | 任务 5（config/router 字段 + 校验）、任务 6（`Door.Path()`）、任务 14（接线）、任务 15（过滤） |
| 显式不可映射字段在触达上游前返回点名字段的 422 | 任务 11（`reject.go`）、任务 19（负例断言上游零调用） |
| 非流式转换保留 failover；首字节后的任何流式错误均不重试 | 任务 12（`Call` 内完成）、任务 13（首帧预读 + Read 错误）；既有 `tracked.wrote` 禁区不变 |
| Native 累计 usage 可供 Chat 响应与指标使用，缺失时不伪造零值 | 任务 7（解码）、任务 12/13（回调一次/逐帧）、任务 16（优先级） |
| Native 同源两扇门 passthrough 无回归 | 任务 10（`TestNativeInboundDelegatesToPassthrough`）、任务 3（迁移不破坏既有直通） |
| `reasoning_effort:none` 不触发入站 422、不报告 `CapReasoning` | 任务 1（守卫）、任务 2（解码）、任务 11（正例） |
| `make check`、`make test-race`、`go build ./...`、LSP 诊断、真实 smoke 全部通过 | 任务 23 步骤 23.1–23.5 |
| 算术自洽：6 PASS + 3 DEGRADE + 2 REJECT = 11 项可表达能力；设计分与门可用分 = (6 + 3×0.5)/11 = 7.5/11 ≈ 0.682 | 任务 18（`TestChatDSNativeRouteIsHeterogeneous` / `TestChatDSNativeRedemptionIsExactlyNineCapabilities`） |

## 执行注意（给 fresh agent 的最后叮嘱）

1. **严格按波次与依赖执行**；波次内任务可并行，波次间顺序不可乱。
2. **每个行为任务先 RED 后 GREEN**，RED 必须确认是因正确原因失败；提交信息用「提交总览」表里的中文 plain 文案，逐字一致。
3. **任务 18 → 19 是 ADR-0001 窗口**，一鼓作气做完，中间不停。
4. **任务 17 / 21 依赖真实凭据**：没有就标 BLOCKED 停下，绝不伪造；波次 0–3 可先全部完成。
5. **规划任务（写本文件）不产生任何生产提交**；上面 22 个提交都是未来执行步骤，逐条带 `GIT_MASTER=1` 前缀与 Sisyphus 署名。
6. 遇到计划与设计文档冲突，以设计文档为准，停下报告，不自行改设计。
