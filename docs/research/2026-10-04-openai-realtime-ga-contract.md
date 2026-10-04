# OpenAI Realtime GA：S2 握手与观测契约

核验日期：2026-10-04。适用坐标：`openai.realtime → openai.realtime`，
`GET /v1/realtime`。本页固定官方证据与 Task 1 Provider 的边界，防止将
DashScope、Beta 或 GPT-Live 的同名字段当作 GA 契约。

## 1. 官方证据

以下官方页面及固定提交源码于核验日读取。官网未提供统一修订日期，日期表示
检索日期；源码以提交定位，不以浮动 main 定位。

| 来源 | 证据 |
|---|---|
| [Realtime 指南](https://developers.openai.com/api/docs/guides/realtime.md) | GA 删除 `OpenAI-Beta: realtime=v1`，采用新 session/audio/event 结构；Safety-Identifier 推荐但非必需 |
| [WebSocket 指南，Realtime 部分](https://developers.openai.com/api/docs/guides/voice-websockets.md?api=realtime) | `wss://api.openai.com/v1/realtime?model=gpt-realtime-2.1`、Bearer 与安全标识头 |
| [客户端事件参考](https://developers.openai.com/api/reference/resources/realtime/client-events.md) | `session.update`、有效配置更新规则、嵌套音频格式与 `rate`、output_modalities |
| [服务端事件参考](https://developers.openai.com/api/reference/resources/realtime/server-events.md) | `session.created` 身份、`response.done.usage`、会话内 ASR tokens/duration 联合类型 |
| [Node SDK ws.ts](https://github.com/openai/openai-node/blob/11b9283f2a22737e273ccc1593d01af5cf584a0b/src/realtime/ws.ts) | `OpenAIRealtimeWS` 使用 Node `ws`、Bearer、`options.headers`，收发 JSON 事件 |
| [Node SDK internal-base.ts](https://github.com/openai/openai-node/blob/11b9283f2a22737e273ccc1593d01af5cf584a0b/src/realtime/internal-base.ts) | `buildRealtimeURL` 追加 `/realtime` 并使用 wss；model/intent/callID 三选一 |
| [Node SDK package.json](https://github.com/openai/openai-node/blob/11b9283f2a22737e273ccc1593d01af5cf584a0b/package.json) | 版本 `7.27.0`，Node `>=22.0.0`，ws `^8.21.0` |

共享 voice 页面同时含其他产品示例。这里只使用 Realtime 部分的 `/v1/realtime`；
`/v1/live/sessions` 和 `session.start/started/closed` 不能替代本门契约。

## 2. 地址、模型与干净握手

- 官方云端使用 wss；配置主机是唯一目标来源。Provider 接受无 userinfo、query、
  fragment（包括空 `?`/`#`）、opaque 的 http/https/ws/wss，分别归一为 ws/wss。
  TLS 沿用系统证书验证，不关闭验证，不跟随重定向。
- 配置路径是部署前缀：保留 `%2F` 等转义，追加固定 `/v1/realtime`；已完整包含
  该后缀时不重复追加。`base_url=https://api.openai.com` 才得到官方路径；
  `/v1` 前缀会得到 `/v1/v1/realtime`。这与 SDK 的 baseURL 约定不同。
- 出站 query 从 `Target.UpstreamModel` 构造，只有一个非空 `model`，按 query
  转义。SDK 支持的 `intent=transcription`、`call_id` 不扩大 S2 范围。
  入站唯一 model、附加 query 拒绝及 `model == target.UpstreamModel` 比较由后续
  handler 在 Dial 前执行；既有 `provider.Request` 不增设 query 参数袋。
- Provider 同时核对入站协议、端点与出站 kind；不接受跨协议目标。
  官方 alias 可能回显 snapshot，不能凭 `session.model` 字符串不同推断本地别名。
- 出站鉴权固定为池中凭据的 `Authorization: Bearer …`，UA 固定为 `omugw`。
  附加头仅透传 `OpenAI-Safety-Identifier`，要求单值、无控制字符；重复和大小写
  重复均拒绝。Authorization/Api-Key 同样拒绝重复、多来源和控制字符。
  实际密钥认证仍归网关 Authenticator，Provider 不验证下游密钥是否匹配。
- Organization/Project、Cookie、Origin、DashScope 头及其他未知头不转发。
  Origin 不阻碍头鉴权客户端；忽略压缩提议、不协商扩展。
- 任何出现的 `OpenAI-Beta` 或 `Sec-WebSocket-Protocol` 都在拨号前拒绝，包括
  空值、空列表、大小写变体与重复值。该策略是本门 GA 准入约束，不声称官方会
  拒绝所有其他 Beta 功能；错误不回显头值或子协议中的密钥。

## 3. GA 会话及有效配置

服务端首次事件为 `session.created`。官方响应 schema 的 session 身份为
`id`、`object="realtime.session"`、`type="realtime"`。初始事件含默认配置，
不证明未来客户端配置已被采纳。后续 handler 的 ready 校验和原字节首事件交付
独立实现；Provider 只建立连接，不预读或重编码应用消息。

下例仅展示 GA schema，不是云端成功样本：

```json
{
  "type": "session.update",
  "session": {
    "type": "realtime",
    "output_modalities": ["audio"],
    "audio": {
      "input": {
        "format": {"type": "audio/pcm", "rate": 24000},
        "transcription": {"model": "gpt-4o-mini-transcribe"},
        "turn_detection": null
      },
      "output": {
        "format": {"type": "audio/pcm", "rate": 24000},
        "voice": "marin"
      }
    }
  }
}
```

PCM 是 24 kHz，字段为 `rate`，不是 DashScope 的 `sample_rate`；G.711 类型为
`audio/pcmu` / `audio/pcma`。`output_modalities` 是 `["audio"]`（默认，含转录）
或 `["text"]`，不能照抄旧文档为两者同时启用。

`session.updated` 返回完整有效配置；缺失字段保持、instructions 空字符串清空、
tools 空数组清空、turn_detection null 关闭。model 不可中途修改；产生音频后
不能再改 voice。只读观测应查回显，不能从发送 update 推断生效。

## 4. 计量字段与单位

`response.done.response.usage` 的字段是 **token 单数**：

```text
input_tokens / output_tokens / total_tokens
input_token_details
  text_tokens / audio_tokens / image_tokens
  cached_tokens
  cached_tokens_details
    text_tokens / audio_tokens / image_tokens
output_token_details
  text_tokens / audio_tokens
```

不能复用 DashScope 的 `input_tokens_details` / `output_tokens_details` 拼写。
cached 是 input 的子集，不与 input 再相加；各模态细分也不是额外总量。
`response.done` 可以是 completed/cancelled/failed/incomplete；合法 usage 仍是
该轮权威终态，缺失不是零值，之后断流不能抹掉已结数值。当前参考没有正式输出
reasoning token 明细，不能从差额猜数。

`conversation.item.input_audio_transcription.completed.usage` 独立属于转写：

- `type="tokens"`：input_tokens / output_tokens / total_tokens，可选
  `input_token_details.{text_tokens,audio_tokens}`。
- `type="duration"`：`seconds`，单位为音频输入秒数，允许有限非负小数。

转写不能并入生成模型账单，seconds 不能变成 token，也不能按 PCM 字节推导账单。
关联保留 item_id/content_index；转写可晚于 response.done。默认未启用转写时，
不能把每次音频 commit 记为 ASR pending。OpenAI 无 DashScope 的
`session.finished` 计费终态，不能在 session.created 凭空登记 session 账。
这些条款为后续 observer 提供证据，Task 1 不实现计量解析。

## 5. 资源、期限与安全错误

复用 `config.Timeouts`、`config.WebSocket` 与调用方共享的 `*ws.BufferBudget`。
默认 32 MiB/128 会话/256 MiB 不变，最低预算仍为
`2*M + min(M, 32 KiB)`，不以预算缺省创建独立池。

connect 覆盖 TCP/TLS；握手 ctx 的共同 first_byte 截止由协调器传入，Provider
不重置；升级后 idle 与 `min(connect,idle)` 写期限生效，HTTP total 不套会话。
握手状态行加头限制 64 KiB，失败体最多保留/解析 64 KiB；共享 transport 为判定
截断会额外探读至多 1 字节，随后关闭原 socket，再交付有界内存体。

只有非 101 的 HTTP 4xx/5xx 使用 `openaiwire.DecodeError` 分类，401/403、
429、quota/context/filter 与未知 5xx 保留统一错误分类。200/重定向/坏 101
保守不可重试，不从其 body 或 WS close 字符串猜测限流。无 HTTP 响应时，仅
明确共同 ctx deadline 归为可重试 unavailable；主动取消与无法识别的网络失败
保守不可重试。

失败后的错误 message/code 固定，param/request ID/cause 清空；返回新 Response，
只保留状态码及重新编码的数值 Retry-After/RateLimit 头，Body 为 `http.NoBody`。
原 message/code/param、Location、任意头、Request/URL 和原体均不能流到调用方。

## 6. 证据边界

Task 1 的自动化证据是本地 TCP/TLS：握手头/地址、零拨号拒绝、64 KiB 精确
边界、原 socket 关闭、不重定向、不信任 TLS 证书拒绝、期限及预算接线。
`internal/config/websocket_test.go` 已覆盖 OpenAI 的四种配置 scheme 与共享默认值。

尚未做 OpenAI 云端调用、真实模型权限/地域核验、完整有效配置实录、逐能力实录、
官方 Node SDK + 网关 TLS 集成或生产代理验收。测试中的假凭据和合成消息不是
官方成功 fixture；本任务不构成生产注册或整门 Redeem 证据。
