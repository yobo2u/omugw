# 核心设计原则

这七条不是风格偏好，是从「多协议网关会怎么坏掉」倒推出来的约束。每一条都有
对应的代码强制机制与测试，改动前请先读懂它防的是什么。

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

**约束**：入站协议族 == 出站 Provider 族时字节级透传，只改写鉴权，不进 Canonical。

覆盖范围：`OpenAI Chat → OpenAI 兼容`、`Anthropic → Anthropic`、
`DashScope Compatible → DashScope`、以及 **`OpenAI Realtime → DashScope Realtime`**
（两者事件模型基本一致，绝大多数事件原样转发）。

**收益**：保住 TTFT，绕开绝大多数转换 bug。

**要求**：这条路径必须可关闭。关掉快通道跑同一组 fixture，与 Canonical 路径对照，
差异写进降级矩阵——没有这个对照，快通道就成了一条没人验证过的分支。

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

- 首字节**前**上游失败 → 正常 failover 到下一个凭据 / Provider。
- 首字节**后**上游失败 → **不重试**，发送协议对应的终止错误事件收尾，
  并将 usage 标记为 `unavailable`。

**防的是什么**：重试会让客户端收到重复内容。这与 `Retryable` 无关——即使错误
本身可重试，流已经开始就不能重来。

**配置对应**：`timeouts.first_byte` 就是这个判定窗口。它必须与 `total` 分开配置，
否则窗口失效（`config.Timeouts.Validate` 会拒绝 `first_byte > total`）。

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

**约束**：`connect` / `first_byte` / `total` / `idle` 独立配置，且满足
`connect < first_byte ≤ total`、`idle ≤ total`。

**防的是什么**：只有一个总超时的话，一个思考 3 分钟的推理请求和一个挂死的连接
长得一模一样——要么把前者误杀，要么让后者拖住连接池。

`idle` 才是「上游挂死」的真正判据；`total` 到期只说明响应很长。

---

## 强制机制一览

| 原则 | 代码位置 | 测试 |
|---|---|---|
| 2.1 降级矩阵 | `internal/degrade` | `TestPhase1IsComplete`、`TestIncompleteRouteFailsBuild` |
| 2.2 同源快通道 | `degrade.Route.IsHomogeneous()` | `TestRealtimeFastPathIsHomogeneous` |
| 2.3 错误映射 | `internal/canonical/error.go`、`internal/protocol/*wire` | 各 wire 包的 `TestDecodeErrorClassification` |
| 2.4 流式 failover | `canonical.StopInterrupted` | `TestAccumulatorInterruptedStreamReportsUnavailableUsage` |
| 2.5 用量分级 | `canonical.Fidelity` | `TestUsageFidelityMustBeExplicit`、`TestObserveUsageSeparatesFidelity` |
| 2.6 多模态负载 | `canonical.Media.Validate` | `TestMediaRequiresExactlyOneSource` |
| 2.7 四层超时 | `config.Timeouts.Validate` | `TestTimeoutsValidate` |
