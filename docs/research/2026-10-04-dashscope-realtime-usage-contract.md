# DashScope Realtime 只读用量契约

核验日期：2026-10-04。范围为 S1 Task 2 的协议观测、去重账本与指标。

## 依据与证据边界

采用阿里云公开契约原文，不从 OpenAI 同名事件或模型名外推计费口径：

1. [Qwen-Omni-Realtime 服务端事件](https://help.aliyun.com/zh/model-studio/server-events)：
   `response.done` 的 `response.id/status/usage`；`usage.input_tokens`、
   `output_tokens`、`total_tokens` 与两侧 `*_tokens_details.audio_tokens/text_tokens`。
   官方示例：输入 336、输出 41、输入音频 108、输出音频 32，总计 377。
   音频是输入/输出的分项，不能再次加到总数中。
2. [Qwen-TTS-Realtime 服务端事件](https://help.aliyun.com/zh/model-studio/qwen-tts-realtime-server-events)：
   `response.done` 示例明确区分 Qwen3-TTS 的 `usage.characters:25` 与旧 Qwen-TTS 的
   `input_tokens:3/output_tokens:64`。两者都位于 `response.usage`。
   `session.finished` 示例只有 `event_id/type`，没有关联 session ID 或 usage。
3. 上述 Omni 页的 `input_audio_buffer.committed` 和
   `conversation.item.input_audio_transcription.completed/failed` 使用 `item_id`；
   转写成功/失败示例没有 usage。不能把其他协议的转写用量格式套到这里。
4. 两份服务端文档的错误示例确认 `error.code=invalid_value`，Omni 另有
   `error.type=invalid_request_error`。本次只对这两个固定值归类 `bad_request`。
5. 已有实测关闭线索见
   [WS transport 设计](../superpowers/specs/2026-09-06-websocket-transport-design.md)：
   1011 与 `To many requests. Your requests are being throttled...` 的组合。
   本次保留这一已有证据，没有再次发起上游调用。

本地官方 raw 快照核对位置（方便复核，不是新增的实测证据）：

- `bailian-docs-llm-wiki/raw/model-api-reference/omni-realtime-api/server-events.md`：
  7–50、748–831、901–1133 行。
- `bailian-docs-llm-wiki/raw/model-api-reference/audio-api-references/speech-synthesis-api-reference/qwen-tts-realtime-api-reference/qwen-tts-realtime-server-events.md`：
  7–40、594–754、757–774 行。

以上证明**公开契约**，不证明指定模型/快照的线上实现与账单。测试数据是人工构造的离线数据，
后续生产投放仍需独立直连录制：模型回显版本、真实 usage 形状、失败/取消是否携用量、
重复终态及转写开关的行为。合成 wiki 与 raw 有冲突时不采用其合成结论。

## 只读窥探器

`internal/protocol/dashscoperealtime.Inspect` 只输出有界标量与结构化观测值，
不输出重编码后的消息；原始 opcode、消息顺序与负载所有权仍属于调用方。

- 首先 `json.Valid` 检查全帧语法，再在原切片中扫描并跳过未知值；
  不用整帧 `map`、`RawMessage`、大字符串 `Decoder.Token` 或递归建树。
- 关联 ID 的**解码后 UTF-8 长度**最多 512 字节；type、status、错误 code/type 最多
  128 字节。字符串先限制源长度再解码，拒绝 ID 的有损 Unicode 替换。
- 只查固定层级，未知深层字段、音频、转写文本、工具参数、model、event_id 均不提取。
  返回字符串拥有独立内存，不延长大帧寿命。
- 用于关联的关键字段重复、缺失或错误类型，非法 JSON、超限 ID 返回固定包络错误，
  错误不含原文或 ID。未知业务事件保持非生命周期观测，交由调用方原样转发。
- 业务 `error` 是 `Event.Failure`，不是 `Inspect` 的 error；已知分类用于观测，
  未知分类是 `internal/Retryable=false`，不会把 message/param 放进错误或日志。

## 用量判据

| 形状 | 输出 | 诊断 |
|---|---|---|
| `response.usage` 中 input/output 都是非负 int64 整数 | `Usage.Authoritative` | 无 |
| 仅已知字符形状 `characters`，含合法 0 | `Characters` 非 nil；`Usage.Unavailable` | 无 |
| characters 与任一已知 token 总量/明细字段同时出现（包括 null） | 不发布数字 | `usage_ambiguous` |
| 缺失/null usage | unavailable | `usage_missing` |
| 空对象或未验证的形状、转写/session 的新 usage | unavailable | `usage_unverified` |
| 已识别形状含负数、浮点/指数、字符串数字、溢出、缺必要数字或重复关键字段 | unavailable | `usage_invalid` |

token 总计若存在必须等于 input + output，两者相加也不能溢出 int64。
可选明细对象若存在必须合法，audio/text 分项不能为负、溢出或超出该侧总量。
未给明细时不推算音频分项。缓存、推理、插件计次等未知明细不外推到 Canonical 用量。
`cancelled`、`failed` 终态带合法 usage 仍照实采集，不因状态丢掉账。
相同数值格式不代表相同计量单位；characters 永远不换算 token。

## 有界账本与接口选择

`newWSUsage(metrics, protocol, outbound)` 创建单会话账本；只在一个 goroutine 中
`Observe(Inspect(...))`，协调方待该 goroutine 退出后 `Finish()`。可传 nil metrics，
资源/去重策略仍生效。

- 键为 `(source, ID)`，来源固定为 `response/transcription/session`。
  session 用 `session.id`，response 用 `response.id`，转写用 `item_id`。
- `session.finished` 无 ID，关联账本唯一的先前 session；没有 session.created 或出现第二个
  不同 session ID 时返回固定策略错误，不能拿 event_id 猜关联。
- 已结和未结合计最多 **4096 笔**；第 4097 个新键返回 `errWSUsageLimit` 并记
  `ledger_limit`。旧键不淘汰，到限后已有未结项仍可结算、旧终态仍可去重。
  后续 relay 应把此本地策略错误作为 1008 终止原因；本任务没有接管连接关闭。
- 首份终态胜出；比较完整 `canonical.Usage` 与字符存在性/值，字符数按值保存。
  同值重复不再计数；不同值只报 `usage_conflict`，不补差、不撤销已发布数字。
  终态后的迟到 started 不能重开账。状态与原始 JSON 排序不参与计费比较。
- `Finish` 幂等，仅将仍未结项发布为 unavailable + `usage_unfinished`；已收权威记录
  不被断开清零。Finish 后 Observe 返回固定错误，不能重新开账。
- `Observe` 的输入契约是成功 Inspect 的 Event；合成跨来源权威记录只用于验证账本的
  来源隔离，不表示官方转写/session 已有可计费用量。

## 指标

| 指标 | 标签 | 口径 |
|---|---|---|
| `omugw_ws_usage_records_total` | protocol, source, unit, fidelity | 独立已结记录数；零值与不可知均可见 |
| `omugw_ws_tokens_total` | protocol, source, fidelity, kind | input/output/audio_input/audio_output 正数分项 |
| `omugw_ws_characters_total` | protocol, source, fidelity | 上游字符单位，fidelity 固定 authoritative |
| `omugw_ws_diagnostics_total` | protocol, reason | 固定诊断分类 |

合法字符事件只发布 `unit=characters/fidelity=authoritative` 的记录及字符数，
不会发布虚构的零 token 记录；正常 token 事件也同步现有 `omugw_tokens_total`，
两类 token 指标是不同观测视角，不应再相加。
缺少 session/transcription 用量时只发布 unavailable 记录，不发布 token 数字。
协议、来源、诊断均有白名单，标签没有动态 ID/model/message。
Prometheus 数值为 float64，极大 int64 的精确账单应另存上游原始证据，不能依赖浮点计数器。

## 关闭分类

- 1000/1001 视为正常关闭，不返回失败分类。
- 仅 `1011 + "To many requests"`（完整短句或后接 `.` 的已实测句式）归 `rate_limit`。
- 其他关闭统一 `internal/Retryable=false`，不根据 reason 推断 auth/quota，
  不在本地错误中保存 reason；`Retryable` 也不授予首字节后的重试权。
