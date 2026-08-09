# internal/protocol

线格式编解码。命名分两类：`<name>wire` 只做**错误信封**（编码 + 解码 + 分类），
完整协议包（`openairesponses`、`openaichat`）做请求解码与能力识别。

## STRUCTURE

| 包 | 范围 |
|---|---|
| `openairesponses/` | `openai.responses` 完整编解码：`wire.go` 结构、`decode.go` 入站、`response.go` 非流式出站、`stream.go` 流式状态机 |
| `openaichat/` | `openai.chat` 入站解码：`wire.go` 结构、`decode.go` 入站。同源直通路径，响应/流字节原样转发，无需出站编码 |
| `openaiwire/` | OpenAI 族错误信封 `{"error":{message,type,param,code}}` + 限流头 |
| `dashscopewire/` | DashScope 扁平信封 `{code,message}` + `DecodeWSFailure`（`task-failed` 事件） |
| `anthropicwire/` | Anthropic 顶层判别式信封 `{"type":"error","error":{...}}` |

## WHERE TO LOOK

| 任务 | 位置 |
|---|---|
| 入站请求 → IR | `openairesponses/decode.go:34` (`Decode`)，返回带元信息的 `Decoded` |
| 客户端要了什么能力 | `openairesponses/decode.go:458` (`Capabilities`) |
| IR → 非流式响应 | `openairesponses/response.go:156` (`EncodeResponse`) |
| IR 事件 → SSE | `openairesponses/stream.go:91` (`StreamEncoder.Encode`) |
| 错误分类规则 | 各包的 `classify()`；按 code > type > HTTP status 依次判定 |

## CONVENTIONS

- 解码用 `DisallowUnknownFields`：客户端发了我们不认识的字段要报错，不要静默吞掉。
- `StreamEncoder` 是**有状态**的（`output_index`/`content_index`/item 生命周期），
  每次响应一个实例，不要复用。
- `toWireUsage()` 在 usage 不是 `authoritative` 时返回 `nil`——宁可不给也不给假数。
- 相邻文本 Part 在编码时合并成一条 message output（`encodeOutput`）。
- 错误解码必须还原 `Retry-After` 与 `X-RateLimit-*`，客户端 SDK 的退避算法依赖它们。

## ANTI-PATTERNS

- **不要**给新协议只写 `*wire` 错误包就以为接完了——请求/响应/流三段都要有。
- **不要**在错误信封里泄漏上游原始响应体（`passthrough` 里有体积上限，见
  `TestOversizedErrorBodyIsCapped`）。
- **不要**在 `Decode` 里做能力裁决；它只负责报告用到了哪些能力，裁决归 `degrade`。

## ADDING A PROTOCOL

1. `degrade/matrix.go` 加 `Protocol`（入站）或 `Provider`（出站）常量。
2. `degrade/expressibility_phase1.go` 声明可表达性（三桶必须覆盖全部能力）。
3. `degrade/rules_phase1.go` 登记全部路径处置。
4. 本目录建包：wire 结构 → `Decode` → `EncodeResponse` → `StreamEncoder` → 错误信封。
5. 出站还需 `internal/provider/<name>/` 实现 `provider.Provider`。
6. `gateway/build.go` 装配 + 注册 handler。
7. 写 `testdata/routes/<in>__<out>/` fixture，`MarkImplemented()`，改
   `TestImplementedRoutesAreExplicit` 白名单。跑 `make check`。
