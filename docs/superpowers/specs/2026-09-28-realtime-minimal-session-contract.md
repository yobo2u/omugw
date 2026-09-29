# Realtime 最小会话契约

**日期**：2026-09-28
**状态**：方向已确认（用户批准），未实现。本文件是语义契约与验收标准，不是
实施计划。
**范围**：realtime 双向会话在网关内的语义边界：同契约直通、跨协议最小显式
转换、状态记录纪律、握手与首字节、打断、用量、版本协商、测试证据边界。
**前置决策**：[ADR-0003](../../adr/0003-realtime-minimal-session-semantics.md)、
[#5 双向长连接的出站适配器抽象](https://github.com/yobo2u/omugw/issues/5)
（已关闭）、[#6 realtime 会话事件的 IR 建模](https://github.com/yobo2u/omugw/issues/6)
（已关闭）。两票的决定本文件只复述、不重开。

## 1. 决定摘要

1. 同契约直通做**负载保全**：任意字段（含网关不认识的）按不透明负载原样保留。
2. 跨协议转换做**最小显式、presence-aware 的有状态映射**：只建模矩阵裁决所需
   的语义，逐项显式；不建通用 IR，不做盲目 JSON 改名表。
3. 状态记录纪律：missing/null/value 三分；显式 ID；配置分 requested/pending 与
   confirmed 两态；有界缓冲；重采样器状态按会话维护。
4. 运行时降级矩阵仍是唯一处置权威，会话中途更新同样受它裁决。
5. 上行指令用独立 `RealtimeCommand` 类型（#6 决定一，尚未实现）；下行复用既有
   `canonical.Event`（含 10 个 realtime 事件常量）与 `canonical.Usage`，既有
   常量不删除。

## 2. 边界与非目标

- 不改降级矩阵的任何处置、分数与 `MarkHomogeneous` 标记。当前不对称是事实
  状态：`openai.realtime → dashscope.ws.realtime` 标了 homogeneous，反向没标。
  本文件不改这些 flag，也不把它们当作「负载等价」的授权。
- 不新增 Capability 常量，不声明任何新能力「已支持」。
- 不修改 `docs/architecture/principles.md`。原则 2.2 的澄清以草案形式存在于
  ADR-0003；并入原则文件需要单独 PR 与评审，采纳门槛尚未通过。
- `dashscope.inference`（run-task 指令流）保持独立，直到拿到语义证据；不因与
  Native SSE 形状相似就共享 IR。
- 本文件不产生任何运行时代码改动。

## 3. 同契约直通

**同契约**要求版本、字段语义、取值、默认值、更新规则与有效音频参数一致，
不是只比事件名和 JSON 形状。版本必须由配置或文档化协商确定：

- 按 2026-09-06 研究快照，GA 与 beta 是两份不同契约：GA 把 `response.text.*` 改名
  `response.output_text.*`、`response.audio.*` 改名 `response.output_audio.*`，
  音频格式取值从 `pcm16` 改为 `audio/pcm`；DashScope 对齐的是 beta 命名。网关
  必须维护**显式的受支持版本清单**，每条路径声明自己接受哪份契约。
- **不做投机性自动 fallback**：声明为 beta 的路径收到 GA 事件，不得静默改写后
  转发，按可见失败处理。
- 握手信号（`OpenAI-Beta: realtime=v1` 头、`openai-beta.realtime-v1` 子协议）
  可作客户端预期的线索，但**单靠 header 不是无误判据**：GA 接口根本不带 beta
  头，缺席不证明客户端只认 GA 命名。最终判据是协商/配置后的精确契约。

直通的行为边界：

- 任意字段按不透明负载保全，包括上游新发布、网关尚不认识的参数。保全只允许
  发生在同契约路径；跨协议路径不得搬运未建模字段。
- 保全针对消息负载，不承诺网络帧逐字相同；两段 WS 连接各自处理掩码、分片和心跳。
- **保全不等于盲转特权字段**：下游握手鉴权头与承载 key 的子协议 token 不得
  原样送上游或落日志；出站凭据由授权的凭据池提供。
- **保全不等于语义验证**：不透明字段被转发，但不被声称理解。快通道的对照义务
  （原则 2.2：可关闭、关掉后与转换路径跑同一组 fixture 对照）按**建模事件
  子集**执行，澄清草案见 ADR-0003。

## 4. 跨协议最小显式转换

建模本期兑现能力涉及的语义，包括文本/工具命令与结果、五项 realtime 能力
（§7）、音频格式、关联 ID、错误与 usage。不为未来协议预造全覆盖 IR，也不将
尚未建模的跨协议语义不透明地放行。状态记录纪律：

1. **missing / null / value 三分**。`false`、`0`、空集合都是值，不等价于「没设」。
   presence 判定沿用 [projection.go](../../../internal/protocol/openaichat/projection.go)
   的先例：指针承载值，presence 位承载「是否显式提交」。
2. **显式 ID**。会话、响应、条目的 ID 显式映射登记，网关不得重新生成或猜测；
   `canonical.Event` 的 `SessionID` / `ResponseID` 是下行承载点。
3. **配置两态：requested/pending 与 confirmed**。先校验更新并获得矩阵裁决，
   再发送；确认须核对更新关联、回显结构、实际值与契约语义。出现 sample_rate
   字段只是结构证据，值为 16000 不等于接受了 24000。初始配置也须有协议证据。
   错误或超时结束本次 pending，不覆盖先前 confirmed；确认不了且旧配置也不能
   安全使用时显式失败。不能可靠关联并发更新的契约应串行化更新且限制队列，
   不猜哪份回显确认了哪次请求。依赖新格式的音频在确认前不得按新格式发送。
4. **有界缓冲与队列**。音频帧、未闭合工具参数、待确认配置等一切按事件累积的
   缓冲都有显式上限，超限行为可见。
5. **重采样器状态按会话维护**。采样率协商结果与滤波器状态属于会话级；不跨
   会话共享，也不按单帧重建。

**矩阵是唯一处置权威**，会话中途的配置更新同样受它裁决：`session.update`
请求了路径未兑现的能力时由矩阵给出未投放分类（HTTP 阶段为 501），不支持则为
拒绝分类（HTTP 阶段为 422）。101 后编码成入站协议的错误事件或关闭，不能再写
HTTP 状态码。动态降级也必须有可验证的通知承载方式，不能只依赖握手响应头。
转换模块不得自行降级或忽略字段。跨语义的未知字段/事件可见地失败；不从 `Extensions` 或 `Raw` 猜测
（原则 2.1）。

## 5. 握手与首字节边界（复述 #5，不重开）

- 顺序钉死：**先 Dial 上游，成功后才对下游 Accept**。failover 窗口只存在于
  拨号阶段，期间换凭据、换上游对客户端不可见。
- **101 发出即关闭重试窗口**，此后任何错误都不换凭据、不换上游。
- 101 交接期间的写失败、部分写入、`Hijack` 后的不确定状态一律按「已承诺」
  处理：不可重试，只能在 WS 协议内收尾或断开。
- `tracked.wrote` 不足以做 WS 判据：`Hijack` 之后 `ResponseWriter` 失效，该
  布尔在 WS 路径上永远是 false。首字节判据由 realtime handler 自持（#5 决定二）。
- 超时映射按 #5 决定三：`total` 对 WS 显式不适用，不设 HTTP 式会话总时长上限
  （会话时长类配置的细节留待未来的配置评审）；`first_byte` 映射为握手期限；
  `idle` 是长连接唯一真正的挂死判据。

## 6. 取消与截断是两种语义

`realtime_interrupt_turns` 在实现与验收上必须拆成两个独立操作：

| 操作 | 语义 | 失败后果 |
|---|---|---|
| cancel（`response.cancel`） | 请求停止进行中的响应生成 | 请求发出不等于上游已取消；按实际终止事件与 §8 处理用量 |
| truncate（`conversation.item.truncate`） | 修剪会话历史：把助手音频截到用户实际听到的位置 | 历史与听感脱节，后续轮次携带错误上下文 |

- 取消不是截断：cancel 成功不代表历史被修剪，反之亦然；两者的 ack 不可互相
  顶替。
- 2026-09-06 的研究快照显示 DashScope 侧没有 `conversation.item.truncate`
  落点，而当前矩阵对两条跨协议路径的 interrupt_turns 都记 PASS，且 PASS 未
  区分 cancel 与 truncate。实现时若证实截断无落点，必须回到矩阵做诚实的
  REJECT 或 DEGRADE 决定（本文件不预设结果，此刻也不改矩阵），并在受影响
  操作发生**之前**给出可靠的显式通知。
- **绝不编造截断成功的 ack**：客户端收到 `conversation.item.truncated` 就认为
  历史已修剪；转发一个上游从未确认的 ack，等于让客户端带着错误上下文继续
  会话。

## 7. 五项 realtime 能力映射表

| 能力 | OpenAI Realtime | DashScope Realtime | 矩阵现状与边界 |
|---|---|---|---|
| `realtime_session` | `session.update` / `session.created` / `session.updated`（GA 与 beta 同名） | 同名事件（beta 命名）；模型由握手查询参数指定，`session.model` 字段与查询参数孰先未文档化 | 两条跨协议路径均 PASS；配置遵守 §4 两态纪律 |
| `realtime_server_vad` | `turn_detection`（阈值、前缀填充、静音时长等子参数） | `server_vad`（另有 `semantic_vad`） | 两条跨协议路径均 PASS；VAD 子参数属建模配置，presence-aware |
| `realtime_interrupt_turns` | `response.cancel` 与 `conversation.item.truncate` 是两个独立操作 | `response.cancel` 有；`conversation.item.truncate` 无落点（研究快照） | 两条跨协议路径均 PASS，但 PASS 未区分 cancel/truncate，见 §6 |
| `realtime_image_input` | 无独立图像缓冲 | `input_image_buffer.append`（独有） | 仅 DashScope 可表达；`dashscope.realtime → openai.realtime` 已 REJECT |
| `realtime_commit_modes` | 无 | `server_commit` / `commit`（Qwen-TTS-Realtime 特有） | 仅 DashScope 可表达；`dashscope.realtime → openai.realtime` 已 REJECT |

本表只登记三侧命名与落点现状，不声明任何新能力已支持，不改动任何处置。

## 8. 用量记录

- **按 response 与转写来源分别记录**：为每笔 `Usage` 关联会话、响应或条目、
  上游计量来源；保持 token/秒等原始单位，不假定转写一定按时长计量。
- **既往轮次保留 authoritative 值**：长会话中已收到权威用量的轮次，不因后续
  中断被清零。
- **缺失权威结算的当前轮记 unavailable**：取消或失败若仍带权威 usage，照实
  保留；只有未收到该笔权威用量才记不可知，不用音频长度猜出账单。
- **不重复计数**：同契约直通原样转发 `response.done`；网关按 #6 决定三窥探
  `type` 后解析必要的 usage 子树；记录键须区分响应/转写来源及其稳定关联 ID，
  重复报告只入账一次，累计快照不能逐条相加。去重状态必须有界且不得因淘汰旧键
  重新计数；具体上限与达到上限后的显式处置由实施计划固定。
- **不含计费价格**：网关只记数量与可信等级（原则 2.5）；定价与账单归 omapi。

## 9. 四场景验收表

下表全部是**未来验收标准**：fixture 尚未建立，依赖 WS 录制回放机制落地。
仓库已有 `tests/smoke/ws_realtime_test.go` 及历史调研记录，本轮没有执行真实
探针；历史结果不能代替所选端点、模型与版本的实际互通证据。

| # | 场景 | 正向验收 | 负路径验收 |
|---|---|---|---|
| 1 | session.update 缺席、清空与设值 | 缺席维持旧值；契约允许的 null 清空与 false/0/空串/空数组独立往返 | 不允许的 null/未知跨协议字段在发送前可见失败；不能被 omitempty 吞掉 |
| 2 | 音频配置请求与回显不一致 | 核对实际值与更新关联后才确认；连续分块与整段转换结果在规定误差内一致 | 24k 请求收到 16k 回显不判为接受 24k；错误/超时不覆盖已确认值，排队有界 |
| 3 | 取消与上下文截断 | 对应操作有独立关联与确认；带权威 usage 的取消仍保留该笔用量 | truncate 无落点由矩阵给出诚实处置及操作前通知；不编造成功 ack |
| 4 | 会话内多轮响应与转写用量 | 各笔按来源/关联 ID/单位独立记录；重复事件不双计 | 后续断连不抹掉既往权威用量；缺失的当前笔标不可知，累计快照不重复相加 |

四场景还须分别检验建模子集的快通道/转换路径对照；同契约未建模字段单独检验
负载保真。握手负例涵盖出站鉴权替换、上游失败时不发下游 101、部分 101 不重试。

## 10. 测试证据边界

- **合成 chirp 音频只能测重采样**：它能断言采样率转换的正确性（频谱、混叠），
  不能证明 ASR 识别率、VAD 触发、工具调用链路或音频可懂度。那些结论需要别的
  证据，chirp 测试通过不构成投放依据。
- **真实录音补端到端**：经脱敏与授权的真实录音用于 ASR/VAD/打断的端到端验收；
  单元级边界（畸形帧、乱序事件、半截断开）由脚本化 fixture 覆盖。两者互补，
  **不禁止一切 mock**：离线回放（#7 机制）是矩阵兑现的合法证据，前提是录制
  来源真实、断言针对建模语义。
- **研究笔记是日期快照**：2026-09-06 的两份 realtime 契约笔记记录的是当时
  抓取的事实；GA/beta 事件名差集与音频格式取值表在实现时必须按 §3 的受支持
  版本清单重新核对，不得直接当作活体证据引用。

## 11. 协议轨迹契约（设计级伪代码）

WS fixture 录制要抓住双向会话的两个方向与结局。轨迹对象只有三个交互点：

```text
trace.feed(client_message)    // 交互点一：下游 → 上游的完整消息
trace.feed(upstream_message)  // 交互点二：上游 → 下游的完整消息
trace.close(outcome)          // 交互点三：会话结局，outcome ∈ {completed, interrupted, failed(close_code)}
```

握手元数据单独脱敏记录，不能伪装成应用消息；双向轨迹记录方向内顺序与必要的
因果约束，不把某一次录制的全局交错顺序当作全部合法执行的唯一顺序。
这是设计级伪代码，只表达「录制机制必须捕获什么」。它不承诺 #5 里
`StreamProvider` 的任何实现签名，也不规定轨迹的存储格式；那些属于 #7 的实施
细节。

## 12. 文件与议题索引

| 主题 | 位置 |
|---|---|
| 矩阵 realtime 路由 | [rules_phase1.go](../../../internal/degrade/rules_phase1.go)（OpenAI Realtime 入站、DashScope Realtime 入站两节） |
| 可表达性声明 | [expressibility_phase1.go](../../../internal/degrade/expressibility_phase1.go) |
| 下行事件与用量 | [stream.go](../../../internal/canonical/stream.go)、[usage.go](../../../internal/canonical/usage.go) |
| presence 投影先例 | [projection.go](../../../internal/protocol/openaichat/projection.go) |
| WS 握手与 101 | [handshake.go](../../../internal/transport/ws/handshake.go) |
| 原则 2.1 / 2.2 / 2.5 | [principles.md](../../architecture/principles.md) |
| 研究快照（非活体证据） | [OpenAI Realtime 契约](../../research/2026-09-06-openai-realtime-websocket-contract.md)、[DashScope Realtime 契约](../../research/2026-09-06-dashscope-realtime-websocket-contract.md) |
| 前置设计 | [WebSocket 传输层设计](2026-09-06-websocket-transport-design.md) |
| 已关闭议题 | [#5](https://github.com/yobo2u/omugw/issues/5)、[#6](https://github.com/yobo2u/omugw/issues/6) |
