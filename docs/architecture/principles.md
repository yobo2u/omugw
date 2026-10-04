# 核心设计原则

这七条不是风格偏好，是从「多协议网关会怎么坏掉」倒推出来的约束。每一条都有
对应的代码强制机制与测试，改动前请先读懂它防的是什么。

**修订状态（2026-10-04）**：2.2 / 2.4 / 2.7 的 WebSocket 澄清已在本地修改，
待独立复核与发布，尚未合并；提案与证据边界见
[澄清提案](../superpowers/specs/2026-10-03-websocket-principles-clarification-proposal.md)。

---

## 2.1 转换是有损的，损失必须显式

**约束**：维护代码化的降级矩阵，把每条 `(入站协议, 出站 Provider, 能力)`
的处置登记为 `PASSTHROUGH` / `DEGRADE` / `REJECT`。未登记的组合按 `REJECT`
处理，绝不静默放行。

**防的是什么**：Canonical 中间模型无法无损承载所有协议。真实的不可调和之处包括：

- **状态语义不对等**：OpenAI Responses 有服务端会话状态（`previous_response_id`、
  `store=true`），Anthropic Messages 无状态。
- **推理签名**：Anthropic 的 thinking 块带 `signature`，多轮 tool use 时必须原样
  回传。经 Canonical 拆解重编后签名失效，上游会拒绝整个会话。
- **prompt cache 三家互斥**：Anthropic 是消息序列上的显式断点（上限 4 个），
  OpenAI 是不可控的自动前缀缓存，Gemini 是带 TTL 的独立 `CachedContent` 资源。
  三者之间**不存在映射函数**。
- **usage 口径不同**：`cache_creation_input_tokens` / `cache_read_input_tokens` /
  `reasoning_tokens` 各家定义不同。

`extensions.*` 兜不住这些——它只适合同源快通道下的原样回填，异构路径不得从中
猜测语义。

**强制机制**：`internal/degrade`。`Route.Build()` 要求每条路径对
`canonical.AllCapabilities()` 的每一项表态，漏一格就编译不过测试。新增 Capability
常量时所有已注册路径同时失败——这是刻意的。

**失败方向**：漏配一格 → 请求被拒绝（可见）。而不是 → 请求丢了半数字段还返回 200
（不可见，用户在月底看到十倍账单才发现）。

---

## 2.2 同源走快通道

**约束**：同协议且同契约的原生路径可以旁路 Canonical，原样保全应用消息。
同契约要求两端的版本、字段语义、取值、默认值、更新规则及有效编码参数一致；
协议族相同、事件同名或历史 `MarkHomogeneous` 标记，都不能替代契约证据。

WebSocket 保全的是双向应用消息的类型、负载字节与各方向顺序，**不是网络帧**；
两段连接各自处理掩码、分片和 ping/pong。握手凭据必须清洗并换成授权的出站凭据，
包括可能承载密钥的子协议 token；不支持的鉴权或子协议形态须在拨号前显式拒绝。
原样保全未知字段只说明负载未丢失，不说明网关理解、验证了这些字段的语义。

`OpenAI Realtime → DashScope Realtime` 是跨协议路径，不能因事件名相似就获得
等价快通道授权。跨协议必须按矩阵对已建模语义逐项显式转换，未知语义可见地失败，
不得从 `Extensions` 或原始负载猜测。详见
[ADR-0003](../adr/0003-realtime-minimal-session-semantics.md)。

**收益**：保住 TTFT，绕开绝大多数转换 bug。

**验收要求**：

- 原生直通独立验证双向负载保全、握手凭据替换、原生错误与关闭语义、计量观测，
  并提供端点 fixture 与逐能力证据映射。上游模型支持范围与网关保全范围分别记录；
  合成接线测试不能替代真实上游能力证据，整门兑现仍须满足矩阵的端点与能力门槛。
- 有相应 Canonical / 转换路径时，快通道须能被旁路，对已建模语义使用相同上游
  fixture 轨迹独立验证转换预期，差异写进降级矩阵；预期不得由被测转换器自行生成。
  没有转换器的同协议路径不为对照制造虚假重编码器；不透明字段只在同契约侧验证保全。
- **原生保全通过**与**异构转换对照通过**分别记录。A 包的同契约证据不能降低 C 包的
  presence（missing / null / value）、配置确认、cancel / truncate 与 DSP 验收标准。

---

## 2.3 错误映射是一等设计

**约束**：统一错误类型 `canonical.Error` 携带 `Class` + `Retryable` +
`UpstreamStatus` + `UpstreamCode` + `RetryAfter` + `RateLimit`。每个入站协议有
独立的 error encoder，且必须还原 `Retry-After` 与 `X-RateLimit-*` 响应头。

**`Retryable` 的语义是「换一个凭据或 Provider 重试可能成功」**，不是「上游是否
临时故障」。所以 `auth` 和 `quota` 是 `true`（对当前凭据确定性失败，对池里另一个
凭据完全可能成功），`context_length` 和 `content_filter` 是 `false`（换谁都一样）。

**防的是什么**：客户端 SDK 的退避算法依赖 error type 和这些 header。丢掉它们，
SDK 就退化成固定间隔重试，把上游打得更惨。而分类错误会让 SDK 对着一个永远不会
成功的请求反复重试。

详见 [error-mapping.md](../error-mapping.md)。

---

## 2.4 流式 failover 只在首字节之前有效

**约束**：

- HTTP 由下游 `tracked.wrote` 判定边界：`WriteHeader` / `Write` 开始即置位，
  不以上游响应到达为准。置位前可按错误分类与候选规则 failover；置位后**不重试**，
  不再改 HTTP 状态码，连接仍可写时用入站协议的终止错误事件收尾。
- WebSocket 从下游 Hijack / 开始写 101 起不可重试，不能等第一条应用消息才封口。
  当前协调器在进入 `Accept` 前即标记 `committed`；即便 Hijack、101 短写或交接失败，
  也只能关闭与清理，不能重拨上游或补写 HTTP 错误。
- 下游 `Accept` 的前置按协议区分：Realtime 先完成上游握手，并预读、校验
  `session.created`，再 Accept 并交付这条首事件；首条应用消息是错误或非法事件时
  不得升级。Inference 先完成上游握手即可 Accept，**不能在下游 101 前等
  `task-started`**：它依赖客户端在 101 后提交 `run-task`，等待会形成因果死锁。
- 中断只把未结或不可知的用量标为 `unavailable`。多轮会话中已经取得的终态权威
  usage 必须保留，不能因后续中断清零或统一降为不可知，也不能把缺失用量记成 0。

**防的是什么**：重试会让客户端收到重复内容。这与 `Retryable` 无关——即使错误
本身可重试，流已经开始就不能重来。

**强制机制**：HTTP 的 `tracked` 与 `Handler.dispatch` 管理下游写出边界；当前
DashScope Realtime 的 `WSHandler.serve` / `readWSReady` 管理升级承诺与首事件，
`wsUsage.Finish` 只结算未结记录。Inference 的上述时序是实现约束，当前尚无对应
生产处理器与接线测试，不能拿 Realtime 测试冒充它的已实现证据。

**配置对应**：`timeouts.first_byte` 限定等待预算，不是“到期才关闭重试”的开关；
下游写出或升级承诺会提前关闭重试窗口。HTTP 与 WebSocket 的预算边界见 2.7。

---

## 2.5 用量必须声明可信等级

**约束**：`canonical.Usage.Fidelity` 三级——

| 等级 | 含义 | 可计费 |
|---|---|---|
| `authoritative` | 数值直接来自上游响应 | ✅ |
| `estimated` | 本地 tokenizer 估算，仅供限流与观测 | ❌ |
| `unavailable` | 无法获得（流式中断、订阅账号） | ❌ |

零值 `FidelityUnknown` 是**非法**的，`Usage.Validate()` 会拒绝它。

**防的是什么**：把三者混为一谈，计费就一定是错的。流式中断时上游不返回 usage，
默认零值会被当成「输入输出都是 0 token」计入账单；订阅凭据池根本没有 token
计费概念。

**可观测性对应**：`omugw_tokens_total` 带 `fidelity` 标签。把 estimated 和
authoritative 加进同一个计数器，得到的数字既不能计费也不能做容量规划。

---

## 2.6 多模态负载策略

**约束**：三种承载形态互斥（`canonical.Media.Validate()` 强制）。

| 形态 | 策略 |
|---|---|
| URL | **直接透传，网关不代下载** |
| 内联字节（base64） | 受 `limits.max_inline_bytes` 限制，超限显式报错 |
| 文件引用（FileRef） | 绑定具体 Provider，跨 Provider 时 **REJECT** |

**防的是什么**：代下载再上传会让网关变成流量黑洞——一个塞满 base64 视频的请求
就能把内存吃光，而代下载一批大文件能把出口带宽打满。文件引用跨 Provider 迁移
看起来「体贴」，实际上是把不可控的成本转嫁给网关。

---

## 2.7 超时分四层

**HTTP 约束**：`connect` / `first_byte` / `total` / `idle` 独立配置，且满足
`connect < first_byte ≤ total`、`idle ≤ total`（`config.Timeouts.Validate` 强制）。

**WebSocket 约束**：沿用共享配置校验，但 **HTTP `total` 不套在 WS 会话上**。

- `connect` 只限制每次 TCP/TLS 建连，同时受共同握手预算约束。
- `first_byte` 是从入站处理开始计算、跨所有凭据与 Provider 候选共用的**不可重置
  的绝对截止时间**。它覆盖上游建连与升级握手、协议要求的首事件等待，以及下游
  **101 的写入与交付**；候选切换、ping/pong 或阶段切换都不能续时。Realtime 要在
  此预算内等到 `session.created`；Inference 不把 101 后 `run-task` 触发的
  `task-started` 纳入升级前等待。
- 升级成功后解除握手期限，改由 `idle`、有限写期限及会话取消 / 停机约束连接。
  当前 DashScope Realtime 的有限写期限为 `min(connect, idle)`，防止慢读端挂住中继。
- 心跳只证明传输层存活，不能冒充业务进展或终态，也不能替代协议所需的任务超时。

**防的是什么**：只有一个总超时的话，一个思考 3 分钟的推理请求和一个挂死的连接
长得一模一样——要么把前者误杀，要么让后者拖住连接池。

HTTP 的 `idle` 用于发现流中停滞，`total` 到期只说明响应很长；WS 的传输空闲与
业务完成必须分开判断，持续心跳不能把“任务没有结束”改写成“任务已经完成”。

---

## 强制机制一览

| 原则 | 代码位置 | 测试 |
|---|---|---|
| 2.1 降级矩阵 | `internal/degrade` | `TestPhase1IsComplete`、`TestIncompleteRouteFailsBuild` |
| 2.2 同协议身份与干净握手（DashScope Realtime） | `internal/gateway/ws_dispatch.go` 的 `WSHandler.connect`；`internal/provider/dashscoperealtime/provider.go` 的 `ValidateHeaders` / `Provider.Dial` | `TestWSHandlerPreflightRejectsHomogeneousForeignTarget`、`TestRealtimeProviderHandshake`、`TestRealtimeProviderRejectsUnsafeHeadersAndURL` |
| 2.2 应用消息保全（DashScope Realtime） | `internal/gateway/ws_relay.go` 的 `relayWS` | `TestWSRelayPreservesMessagesAndUsage`、`TestWSRelayCloseMapping`、`TestWSConformanceReplay`（合成接线） |
| 2.3 错误映射 | `internal/canonical/error.go`、`internal/protocol/*wire` | 各 wire 包的 `TestDecodeErrorClassification` |
| 2.4 HTTP 下游写出边界 | `internal/gateway/relay.go` 的 `tracked`；`internal/gateway/handler.go` 的 `Handler.dispatch` / `Handler.fail` | `TestFailoverBeforeFirstByte`、`TestNoFailoverAfterFirstByte` |
| 2.4 WS 首事件与升级承诺（DashScope Realtime） | `internal/gateway/ws_dispatch.go` 的 `readWSReady`；`internal/gateway/ws_handler.go` 的 `WSHandler.serve` | `TestWSHandlerFailoverFirstEvent`、`TestWSHandlerFailoverCommitted` |
| 2.4 WS 中断保留已结权威用量 | `internal/gateway/ws_usage.go` 的 `wsUsage.Observe` / `wsUsage.Finish` | `TestWSUsageLedger`、`TestWSRelayObserveBeforeFailedWrite` |
| 2.5 用量分级 | `canonical.Fidelity` | `TestUsageFidelityMustBeExplicit`、`TestObserveUsageSeparatesFidelity` |
| 2.6 多模态负载 | `canonical.Media.Validate` | `TestMediaRequiresExactlyOneSource` |
| 2.7 HTTP 四层超时 | `config.Timeouts.Validate`；`internal/transport/httpx/client.go` | `TestTimeoutsValidate`、`TestFirstByteTimeoutIsSeparateFromTotal`、`TestTotalTimeoutStillApplies`、`TestIdleTimeoutCatchesStalledStream` |
| 2.7 WS 共同握手期限（含下游 101） | `internal/gateway/ws_handler.go` 的 `WSHandler.serve`；`internal/gateway/ws_accept.go` 的 `acceptWS`；`internal/transport/ws/handshake.go` 的 `AcceptOptions.HandshakeDeadline` | `TestWSHandlerFailoverCandidates`、`TestWSHandlerFailoverPingCannotExtendFirstByte`、`TestProductionAcceptHandshakeDeadline`、`TestWSHandlerLeaseAndShutdownDuring101` |
| 2.7 WS 建连、空闲与有限写 | `internal/provider/dashscoperealtime/provider.go` 的 `Provider.Dial`；`internal/gateway/ws_relay.go` 的 `relayWS`；`internal/transport/ws` | `TestRealtimeProviderTimeoutWiring`、`TestWSHandlerLeaseAndShutdownTotalDoesNotLimitSession`、`TestWSRelayRealSlowTCPReader` |

表中 WS 强制点与测试只证明当前 DashScope Realtime 实现及通用传输行为，不代表其他
WS 路径已投放，也不替代真实逐能力 fixture。历史 `IsHomogeneous()` 标记及
`TestRealtimeFastPathIsHomogeneous` 只固定矩阵旧声明，不是跨协议契约等价的证明。
