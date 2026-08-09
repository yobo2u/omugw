# 错误映射

网关最容易出 bug、也最影响体验的地方。代码是权威来源
（`internal/canonical/error.go` 与 `internal/protocol/*wire/error.go`），
本文说明设计意图。

---

## 为什么这件事值得单独设计

客户端 SDK 的重试逻辑依赖两样东西：**错误类型**和 **`Retry-After` /
`X-RateLimit-*` 响应头**。

- 丢掉 header → SDK 的指数退避退化成固定间隔重试，把上游打得更惨。
- 分类错误 → SDK 对着一个永远不会成功的请求反复退避重试，或者对着一个换个凭据
  就能成功的请求直接放弃。

所以这些 header 是**协议契约的一部分**，不是可选的调试信息。

---

## 统一错误分类

| Class | HTTP | Retryable | 说明 |
|---|---|---|---|
| `auth` | 401 | ✅ | 凭据无效。对**当前凭据**确定性失败，换池里另一个可能成功 |
| `rate_limit` | 429 | ✅ | 触发限流 |
| `quota` | 429 | ✅ | 配额耗尽 / 欠费。同上——换凭据可能成功 |
| `context_length` | 400 | ❌ | 超长上下文。换谁都一样失败 |
| `content_filter` | 400 | ❌ | 内容安全拦截。换谁都一样失败 |
| `bad_request` | 400 | ❌ | 请求本身有问题 |
| `unsupported` | 422 | ❌ | 这条转换路径不支持该能力（降级矩阵判定） |
| `upstream_unavailable` | 502 | ✅ | 上游临时故障 |
| `internal` | 500 | ❌ | 网关自身错误。来源不明的错误重试只会放大故障 |

### `Retryable` 的确切语义

**「换一个凭据或 Provider 重试可能成功」**，而不是「上游是否临时故障」。

这个定义解释了两处反直觉的取值：

- `auth` 和 `quota` 是 `true`——它们对当前凭据是确定性失败，但凭据池里还有别的
  凭据。这正是凭据池存在的意义。
- `internal` 是 `false`——网关自己出的错，重试只是把同一个 bug 再跑一遍。

### 与流式的关系

`Retryable` 只决定「能不能换凭据重试」。另有一条独立的硬约束（原则 2.4）：
**流式响应一旦发出首字节，无论 `Retryable` 为何都不得重试**——重试会让客户端
收到重复内容。

此时的正确行为是：发送协议对应的终止错误事件收尾，并把 usage 标记为
`unavailable`（`omugw_stream_aborted_total` 会记录这类请求）。

---

## 分类的判定顺序

**越具体的信号越可靠**：`code` → `type` → HTTP 状态码。

最典型的例子是超长上下文：它的 HTTP 状态是 400，与普通参数错误无法区分，只有
`code` 能把它认出来。误判不影响可重试性（两者都是 `false`），但会让客户端收不到
「该截断输入了」的正确提示。

DashScope 的情况更严重：**欠费（`Arrearage`）和参数非法（`InvalidParameter`）
都可能返回 400**，而两者的可重试性完全相反。只看状态码会让一个换凭据就能成功的
请求被直接放弃。

上游返回非 JSON（5xx 时常见的 HTML 错误页）或空体时，状态码是唯一可靠信号——
此时仍必须给出可用的分类，而不是退化成 `internal`。

---

## 各协议的线格式

### OpenAI 系（`openaiwire`）

```json
{ "error": { "message": "…", "type": "invalid_request_error", "param": "…", "code": "…" } }
```

`unsupported` 仍映射到 `type: invalid_request_error`，细节由
`code: unsupported_capability` 承载——**不为它编造新的 `error.type`**，
严格解析的 SDK 会直接崩掉。

上游原始 `code` 优先于网关自己推断的 `code`，否则排障时无法向上游追查。

### Anthropic（`anthropicwire`）

```json
{ "type": "error", "error": { "type": "rate_limit_error", "message": "…" } }
```

外层的 `"type": "error"` 判别字段是实质差异，官方 SDK 会检查它。

一处有意的信息损失：Anthropic 没有独立的配额错误类型，`quota` 只能并入
`rate_limit_error`。网关内部仍保留 `ClassQuota` 用于路由决策，只是对外无法区分。

### DashScope Native（`dashscopewire`）

```json
{ "code": "Throttling.RateQuota", "message": "…", "request_id": "…" }
```

信封是**平铺**的，不是 OpenAI 那种嵌套结构。`request_id` 必须保留——它是向阿里云
提工单时的唯一凭据。

DashScope Compatible 模式复用 OpenAI 信封，由 `openaiwire` 处理。这也是
「compatible 与 native 必须分成两个 Provider」的证据之一：连错误结构都不是同一套。

#### A 类 WebSocket 协议的错误

`/api-ws/v1/inference`（`run-task` 指令流，Paraformer / CosyVoice）的失败**不走
HTTP 状态码**，而是作为 `task-failed` 事件送达：

```json
{ "header": { "event": "task-failed", "error_code": "…", "error_message": "…" } }
```

由 `DecodeWSFailure` 单独处理。无法识别的 `error_code` 按
`upstream_unavailable` 处理（可重试），而不是 `internal`——把本可重试的上游故障
判成不可重试，代价更高。

---

## 已知映射对照

| 上游 | 信号 | Class |
|---|---|---|
| OpenAI | `code: context_length_exceeded` | `context_length` |
| OpenAI | `type: insufficient_quota` | `quota` |
| OpenAI | `code: content_filter` | `content_filter` |
| Anthropic | `type: request_too_large` | `context_length` |
| Anthropic | `type: overloaded_error` | `upstream_unavailable` |
| DashScope | `code: Throttling.*` | `rate_limit` |
| DashScope | `code: Arrearage` | `quota` |
| DashScope | `code: DataInspectionFailed` | `content_filter` |
| DashScope | `code: InvalidApiKey` | `auth` |
| DashScope | `code: InvalidParameter` + message 含长度关键词 | `context_length` |
| DashScope | `code: InvalidParameter` 其他情况 | `bad_request` |

最后两行是个不优雅但必要的启发式：`InvalidParameter` 是个大筐，超长上下文也走
这个 code，只能靠 message 关键词进一步区分。

---

## Retry-After 解析

RFC 7231 允许两种写法，都要支持：

- 秒数：`Retry-After: 20`
- HTTP-date：`Retry-After: Sat, 09 Aug 2026 12:00:45 GMT`

已过期的时间戳视为 0，避免算出负数退避。解析失败也返回 0——绝不因为一个 header
解析不了就丢掉整个错误。
