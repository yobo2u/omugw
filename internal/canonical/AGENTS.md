# internal/canonical

中间表示（IR）与共享词汇：请求/消息/内容/工具/用量/流事件/错误分类/能力枚举。
被除 `config`/`obs` 外的所有包依赖，改动半径最大。

## STRUCTURE

| 文件 | 内容 |
|---|---|
| `capability.go` | 27 项 `Capability` 常量 + `AllCapabilities()`（矩阵完整性依据） |
| `request.go` | `Request`、`Reasoning`、`ResponseFormat`、`CacheHint`、`UsedCapabilities()` |
| `message.go` | `Role`（含 `developer`/顶层 system 的归一）、`Message` |
| `content.go` | `Part` 标签联合、`Thinking`、`Media`、`FileRef`、`Extensions` |
| `tool.go` | `Tool`、`ToolCall`、`ToolResult`、`ToolChoice` |
| `usage.go` | `Fidelity` 三级 + `Usage` 分项 token |
| `stream.go` | 统一流事件（HTTP SSE 与 Realtime WS 共用）+ `Accumulator` |
| `error.go` | `ErrorClass`、`Error`、`RateLimitInfo` 与响应头往返 |

## CONVENTIONS

- **采样参数一律用指针**（`*float64`/`*int`），因为 `0` 与「未设置」必须可区分。
- `Part` 是标签联合：`Kind` 决定哪个负载字段有效，`Validate()` 强制只有一个。
- `Media` 的 `URL` / `Data` / `FileRef` 三选一，互斥由 `Validate()` 强制。
- `UsedCapabilities()` 必须按 `AllCapabilities()` 的顺序输出——golden 文件依赖它稳定。
- 新增 `Capability` 常量必须同时加进 `AllCapabilities()`，
  否则 `TestAllCapabilitiesRegistered` 失败。

## ANTI-PATTERNS

- **不要**让 `Usage.Fidelity` 走零值；`FidelityUnknown` 非法，`Validate()` 拒绝它。
  流式中断必须显式写 `FidelityUnavailable`，不许编造非零数字。
- **不要**把 cache 创建/读取的 token 加进 `TotalTokens()`——计价口径不同。
- **不要**在异构转换路径上读写 `Extensions`；它只服务同源快通道的原样回填。
- **不要**丢弃 `Thinking.Signature`，也不要解析 `Redacted` 块。异构路径上
  `CapReasoningSignature` 必须是 `REJECT`。
- **不要**把无法识别的上游错误分类成可重试；未知一律 `ClassInternal` + `Retryable=false`。
- **不要**在异构路径上使用 `CacheHint`（三家 prompt cache 语义互斥，不存在映射函数）。

## GOTCHAS

- `Retry-After` 必须**向上取整**到整秒——向下取整会让客户端提前重试。
- 上游的 `x-ratelimit-*` 头要解析成 `RateLimitInfo` 再重新编码，不是原样透传。
- `Accumulator` 默认 `FidelityUnavailable`，只有收到显式 usage 事件才升级。
- 工具参数分片（`EventToolCallArgsDelta`）必须缓冲到闭合才能重新分片；
  `ToolCall.Arguments` 永远是完整 JSON。
- `closeBlock()` 必须幂等。
