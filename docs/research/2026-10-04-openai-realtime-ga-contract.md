# OpenAI Realtime GA：S2 握手与观测契约

核验日期：2026-10-04。适用坐标：`openai.realtime → openai.realtime`，
`GET /v1/realtime`。本页固定官方证据、Task 1 Provider、Task 2 只读事实、Task 4 账本观测与 Task 5 表达性/跨路径设计处置的边界，防止将
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
这些条款由 Task 2 只读解析提供小事实，Task 4 observer 负责 pending/去重/指标发布。

### 4.1 Task 2 固定接口与观测值

共享包 `internal/protocol/realtimejson` 仅抽取 DashScope 已有的 JSON 扫描：
`Parse([]byte) (Value,error)`、`Value.Field(string) (Value,error)`、
`Value.Text(int) (string,error)`、`Value.Count() (int64,bool)`。
`Value` 借用已验证帧切片，仅可由 Parse/Field 取得，扫描期间不能改写底层字节；
不得进入 Event 或账本。Field 对所查询键拒绝重复（包括转义等价键），Text 拒绝
有损 UTF-8/孤立 surrogate 与超长字符串并返回独立副本。未知负载只跳过，不复制。
DashScope 的 Inspect、错误及计量规则不改变，两个协议不共享业务语义。

`internal/protocol/openairealtime` 导出：

```go
const MaxIDBytes = 512
func Inspect([]byte) (Event, error)
func ValidateReady([]byte) error
func ClassifyClose(uint16, string) *canonical.Error

type Event struct {
    Type, ID, Source, Status string
    Started, Terminal bool
    ContentIndex *int64
    ItemPending bool
    Usage canonical.Usage
    Details TokenDetails
    Seconds *float64
    Duration bool
    Diagnostic string
    Failure *canonical.Error
    Transcription *bool
}
type TokenDetails struct {
    TextInput, AudioInput, ImageInput *int64
    CachedInput, CachedTextInput, CachedAudioInput, CachedImageInput *int64
    TextOutput, AudioOutput *int64
}
```

Type/Status 最多 128 字节，ID 非空且最多 512 字节；ContentIndex 为
0..2147483647，nil 与显式 0 严格区分。Inspect 只解析事实，不自行改写消息；
返回错误表示包络无法安全关联，由 gateway observer/relay 执行既定 1008 policy，
释放该条消息并终止，不将其当成“忽略观测后照常转发”。未知但合法事件原字节转发；
usage 缺失/非法按 §4.2 记 unavailable/diagnostic，保全原事件（合法总量不因坏明细
丢失）。Usage 在所有返回路径均有显式 Fidelity；有界关联策略违规同样交 gateway 1008。

| 事件 | 固定事实 |
|---|---|
| session.created / session.updated | Source=session，ID=session.id，Started/Terminal=false；严格核验 session.type/object/id，Transcription 为有效回显配置 |
| response.created | Source=response，ID=response.id，Started=true；不从非终态 usage 计费 |
| response.done | Source=response，Terminal=true，Status 原值；任何终态状态都独立核验 usage，failed 从 status_details.error 分类 |
| input_audio_buffer.committed | Source=transcription，ID=item_id，ItemPending=true，Started=false，ContentIndex=nil；仅候选，由 observer 结合配置决定是否启用 |
| conversation.item.input_audio_transcription.delta / segment | Source=transcription，Started=true；可缺 content_index，缺时 ItemPending=true，不虚构 part 0 |
| conversation.item.input_audio_transcription.completed / failed | Source=transcription，Terminal=true，Status=completed/failed；必须有合法 content_index，failed 独立报告 Failure |
| error | 只报告安全 Failure，不凭 event_id 打开计费 pending |
| 其他（含输出 transcript 与 session.finished） | 仅保留 Type，Source 为空，无 pending/terminal/usage 推断 |

Transcription 只读 `session.audio.input.transcription`：null → false；含非空
有界 model 的对象 → true；缺失、错误类型、空对象、重复相关键 → nil（未知）。
legacy 顶层字段不替代 GA 配置；nil 不表示关闭，不沿用上一次 enabled 来猜费用。
配置未知本身不设置 usage Diagnostic，observer 据 nil 产生配置不可知诊断。
ValidateReady 复用身份校验并要求 type=session.created；操作码验证由调用方完成。

### 4.2 计量可信边界

- Diagnostic 固定为 `""`、`usage_missing`、`usage_unverified`、`usage_invalid`。
  usage 缺失/null 为 missing；非对象或未知 ASR 单位为 unverified；已识别字段重复、
  损坏、越界或矛盾为 invalid。不会把原文放进诊断。
- token 总量要求 input/output 同时存在、严格非负整数、相加不溢出；total 可省略，
  提供则须严格相等。总量非法时 Usage=unavailable，Details 全 nil。
- 可选明细缺失或未知明细为空不破坏总量，不推算缺项。给出的已知分项不得超过
  父项，已知分项和不得超过父项；已知模态齐全时才要求和相等。response 输入
  按 text/audio/image 三项，输出按 text/audio 两项；ASR tokens 输入按 text/audio。
- 任一已知明细非法时，保留合法 input/output 总量，Diagnostic=usage_invalid，
  整组 Details 不发布，Canonical 的 audio/cache 明细保持 0。合法明细通过后才投影
  audio in/out 与 cache read；消费者应以 Details 指针判断 presence。
- cached 分项同时受 cached 总量与已提供同模态 input 约束。cached 总量缺失时，
  仍检查已给分项和不超过 input 及同模态子集，但不推算 cached 总量，也不要求
  cache 分项和等于 input。未知 reasoning 明细不参与计量或补差。
- ASR `type=tokens` 与 `type=duration` 严格分支；唯一合法的 duration 判别式先置
  `Duration=true`，独立于 Seconds 数值是否有效；重复/错误/未知的 type 不确认单位。
  duration 数值仅产出 Seconds，
  Usage=unavailable，不写 AudioInputSeconds、不制造零 token 记录。seconds 必须是
  有限非负数，显式 0 有 presence；数字文本最多 128 字节，超出保守标 invalid。
  在浮点舍入前按十进制有效数字区分数学零与非零：负非零值一律 invalid，正非零值
  若下溢舍入为 0 也标 invalid，Seconds=nil，不虚构零秒。数学零（含 `-0` 与零的
  指数写法）归一为 +0；其余正数允许 float64 最近值舍入，但结果必须有限且大于 0，
  包括仍舍入为非零值的次正规数。该范围校验不改变秒与 token 的独立性。
- Event 不含原始负载、音频、转写全文或动态容器。返回标量可在归还帧缓冲后保留；
  原始应用消息与未知字段均由中继原字节保全，不从本观测重新编码。

### 4.3 Task 4 关联、容量与发布

- `NewOpenAIRealtimeHandler(WSDeps) *WSHandler` 固定绑定 GA Provider 头校验、
  ready 身份校验、OpenAI 错误信封、GA observer 与安全 close 分类。构造器存在不等于
  生产 `Build` 已注册，也不修改矩阵的门禁与兑现名单。
- observer 只在上游 reader 的观测点运行。`session.created/updated` 的合法有效配置
  覆盖已确认 ASR 状态；未知回显清除旧 enabled，记录固定
  `transcription_config_unknown` 诊断。客户端 `session.update` 不能确认配置。
  disabled commit 不建账；unknown commit 仅诊断；实际 delta/segment/终态事件
  自身足以开启或结算转写观测，不依赖曾见 commit 或 enabled。
- 共享入口为 gateway 私有 `wsUsage.Observe(wsUsageEvent) error`，两协议仅做
  事实映射，DS `Inspect/Event` API 和字符 + token 独立合法的行为不变。
  记录键为 `(source, id, part, hasPart)`；event_id、模型、状态文本与转写全文不入账。
  数值与 presence 复制到固定值记录；terminal 去重比较 Usage、全部九项明细、
  characters、seconds 及诊断，冲突只记录 `usage_conflict`，不重算已结用量。
- 关联只用一张最多 **4096** 项的表：response、明确 part、未明确 part 的 item
  pending、DS session 与已结键共用名额，无额外 item/part 辅助表。首次明确 part
  在同一方法内删除未结 item 占位并原位迁移，到限仍允许迁移与结算既有记录。
  迟到的无 index 事件在这张表内有界查找：已有明确 part 时不重新建 item 占位；
  多 part 的无 index delta/segment 只报 `usage_ambiguous`，不猜归属或另造费用。
  有界查找最坏扫描 4096 键，不持有原 payload 或新增动态索引。
- 未知 part 不是 part 0；id 非空且最多 512 字节，明确 part 为 0..2147483647。
  新关联超限触发既定 1008 policy；旧键不淘汰，防迟到重发重新计费。
  response.done 不结清同 item 的 ASR；Finish 幂等且仅将未结记录发布为 unavailable。
  全部权威观测先于下游 Write；随后写失败、断流或晚到重复不翻账。
- `ObserveWSSeconds(protocol, source string, seconds float64)` 发布
  `omugw_ws_audio_input_seconds_total{protocol,source,fidelity="authoritative"}` 和
  `omugw_ws_usage_records_total{unit="seconds",...}`；显式 0 有记录，非有限/负值拒绝。
  duration-only 不产生 token 记录，秒数不写 Canonical token 投影。
- 已确认 duration 但 seconds 缺失/null/非法/重复/越界时，`Duration=true` 且
  `Seconds=nil`、`usage_invalid`；observer 映射为 `SecondsUnit`，账本将单位事实
  纳入终态快照比较，防止 tokens 与 duration 同为 unavailable 时误去重。
  `ObserveWSSecondsUnavailable(protocol, source string)` 只增
  `omugw_ws_usage_records_total{unit="seconds",fidelity="unavailable",...}`，不发布
  token 记录或任何秒数样本（含假零）。重复不重记，迟到单位冲突诊断但不翻账。
- `obs.WSTokenCount{Value,Present}` / `WSTokenDetails` 与
  `ObserveWSTokenDetails(protocol, source string, details WSTokenDetails)` 固定发布
  `text_input`、`audio_input`、`image_input`、`cache_read`、`cached_text_input`、
  `cached_audio_input`、`cached_image_input`、`text_output`、`audio_output` 到
  `omugw_ws_tokens_total`。仅明确合法值发布，包括 0；缺失不补齐，明细不另增记录数，
  audio/image/cache 不与 input/output 总量相加。没有 ID/model/part 标签。
- 未知但合法事件及合法上游 error/failed 原字节转发；usage 缺失/非法仅按 §4.2
  记录 unavailable/diagnostic，保全事件及可用的合法总量。不安全包络/关联错误或
  有界关联策略违规由 observer 报错、relay 释放该条消息并按既定 1008 policy 终止；
  parser 不自行改写消息，gateway 不生成替代业务消息。

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

### 5.1 Task 2 带内错误与关闭

只读分类先按明确 code，再按明确 type；不调用 HTTP decoder，不制造 400/500
作为回退，不从 message/reason 猜 auth/quota。相关 code/type 重复或损坏时保守
internal；可选 code 的 missing/null 可回退到明确 type。只保存表内已识别字面量
到 UpstreamCode，UpstreamStatus=0，message 固定，param/request ID/cause 清空。

| code | Class |
|---|---|
| invalid_value | bad_request |
| invalid_api_key | auth |
| context_length_exceeded / string_above_max_length | context_length |
| content_filter / content_policy_violation | content_filter |
| insufficient_quota / billing_hard_limit_reached | quota |
| rate_limit_exceeded | rate_limit |

| type（无已识别 code 时） | Class |
|---|---|
| invalid_request_error | bad_request |
| authentication_error / permission_error | auth |
| rate_limit_error | rate_limit |
| insufficient_quota | quota |
| server_error | upstream_unavailable |

以上为显式分类白名单：通用 OpenAI 已知 code/type 的精确匹配加 GA 带内
invalid_request_error/server_error；并非宣称每个值均已在 GA 云端观察到。
Retryable 遵从 canonical 类别，未知为 internal/nonretryable。
ClassifyClose 仅 1000/1001 返回 nil，其余全部 internal/nonretryable；不套用
DashScope 的 1011 + `To many requests` 实录结论。此分类不能绕过首字节边界。

## 6. 证据边界

Task 1 的自动化证据是本地 TCP/TLS：握手头/地址、零拨号拒绝、64 KiB 精确
边界、原 socket 关闭、不重定向、不信任 TLS 证书拒绝、期限及预算接线。
`internal/config/websocket_test.go` 已覆盖 OpenAI 的四种配置 scheme 与共享默认值。

Task 2 的证据是字面合成 JSON 测试：官方示例 132/121/253、ASR 13/9/22、
1.25/0 秒、GA 身份、presence/duplicate/part/config 边界，以及 scanner/两协议
普通与 race 测试、有界 fuzz、4 MiB 未知字段分配上限和归还帧后标量所有权。
DS 网关回归仅验证纯扫描迁移未改变原行为；不代表 OpenAI handler 已接线。

**截至 Task 4** 的证据为合成事件的三态配置、item/part 迁移、4096 总容量、全单位及 presence
去重、1.25/0 秒与 132/121、13/9 的独立字面指标断言；真实本地 TCP 验证固定 GA
profile 的握手、双向原消息、带内错误与原 close 保全，以及下游写失败前的权威记账。
S1 DashScope 实录离线回放继续作为共享账本与中继的回归依据。以上均不是 OpenAI
云端证据；当时正式装配与 SDK/TLS 集成仍属后续任务。

**当前状态（2026-10-04）**：Task 6 已完成正式装配的离线接线验证；Task 7 官方
Node SDK + 网关 WSS/TLS 集成已在本地 **Node 26.8.1** 验证，包含普通/race 与
未信任 CA 必败反例。CI 配置的 **Node 24** 尚待远端实际运行，不能由本地结果代替。
全分支审查及控制器 `e7522fe` 验证见 [S2 条件验收](2026-10-04-openai-realtime-s2-acceptance.md)。

尚未做 OpenAI 云端调用、真实模型权限/地域核验、完整有效配置实录、逐能力实录或
生产代理验收。测试中的假凭据和合成消息不是官方成功 fixture。生产 Build 仍传空
WS 门，Mux 未注册的 WS URL 返回 404；矩阵未兑现门仍为 PLANNED/501，无整门 Redeem。

## 7. Task 5：GA 表达性与跨路径设计处置

本节将 2026-10-04 矩阵前置预研逐条核读的官方证据归入受版本控制的契约。
日期为检索日期，不冒充页面发布日期；Task 5 未另做云端调用或真实拒绝验证。

### 7.1 官方来源与表达性

| 编号 | 官方来源 | 采用的字段或章节 |
|---|---|---|
| O1 | [OpenAI 客户端事件参考](https://developers.openai.com/api/reference/resources/realtime/client-events.md) | `RealtimeConversationItemUserMessage`；session/response 的 reasoning 与 parallel_tool_calls |
| O2 | [OpenAI Realtime conversations](https://developers.openai.com/api/docs/guides/realtime-conversations.md) | “Image inputs”：独立图像用户 item 示例，明确可用于 WebSocket |
| D1 | [DashScope 客户端事件](https://help.aliyun.com/zh/model-studio/client-events) | `session.update`、`response.create`、`input_audio_buffer.commit`、`input_image_buffer.append`、`conversation.item.create` |
| D2 | [DashScope 服务端事件](https://help.aliyun.com/zh/model-studio/server-events) | session 配置回显、`input_audio_buffer.committed`、`conversation.item.created`、`response.done` |
| D3 | [DashScope Realtime 指南](https://help.aliyun.com/zh/model-studio/realtime) | “输入音频与图片 / WebSocket”、“Manual 模式”、“多通道音频、视频聚合与 MCP”、“Token 计算” |
| D4 | [DashScope WebSocket 接入指南](https://help.aliyun.com/zh/model-studio/omni-realtime-interaction-process) | VAD / Manual / Function Calling 流程 |

OpenAI Realtime 的可表达集合由 12 项增至以下 **15 项**，只调整已有 Capability
的三桶归属，不新增 Capability，不代表每个模型都支持或任何门已兑现：

```text
text_generation, streaming, tool_calling, parallel_tool_calls,
reasoning, vision_input, image_detail, audio_input, audio_output,
speech_synthesis, speech_recognition, stateful_conversation,
realtime_session, realtime_server_vad, realtime_interrupt_turns
```

新增 `vision_input` / `image_detail` / `reasoning` 从 Elsewhere 移入 Capabilities，
Impossible 不变。`realtime_image_input` 仍转介 DashScope：它表示专用图像 buffer
入口及其音画共同提交语义，不是所有实时图像输入的统称，更不是独立图像 commit。

### 7.2 OpenAI → DashScope 三格

| 能力 | 设计处置 | 来源与可明确命名的损失 |
|---|---|---|
| `vision_input` | **REJECT** | O1/O2 的独立无音频图像 item 无法按 D1/D3 的音画 buffer 契约保留；拒绝隐式增加音频、合并轮次或改提交边界 |
| `image_detail` | **DEGRADE** | O1 的逐图 `auto/low/high` 被丢弃，视觉处理与费用采用目标策略；D1/D3 的会话级视频聚合不等价 |
| `reasoning` | **REJECT** | O1 的显式 effort 在 D1/D2/D3 当前公开 WS 请求/确认契约中无已文档化落点，也无可确认的思考开关 |

**独立图像 item 与 buffer（O1/O2、D1/D2/D3/D4）**：

- OpenAI 使用 `conversation.item.create`，`item.type="message"`、`role="user"`，
  `content[].type="input_image"`，`image_url` 为 PNG/JPEG data URI；可选 `detail`
  为 `auto/low/high`，其中 auto 默认 high。O2 示例只有一个图像 part，不要求先
  append 音频；图像加文本示例后另发 `response.create`。
- 以上只取 O1 的 user-message 分支。不能把同页复用的
  `ResponsePrompt.variables/ResponseInputImage` 中 URL、file_id、`original`
  等值移植到该分支。
- D1 的图像入口为 `input_image_buffer.append.image`（裸 Base64），明确要求：
  “发送 input_image_buffer.append 事件前，至少已发送过一次
  input_audio_buffer.append 事件”；“图像缓冲区与音频缓冲区通过
  input_audio_buffer.commit 事件一起提交”；“若音频缓冲区为空，服务端将返回
  错误事件”。最后一句是官方说明，不是本项目实测拒绝证据。
- D1 只列 JPG/JPEG，Base64 后单图 ≤256KB；建议原图 ≤190KB、480p/720p、
  1 张/秒，最高不超过 1080p。建议值不改写成协议硬禁令。
- commit 创建用户消息项，本身不触发响应；Manual 另发 `response.create`，
  VAD 自动提交/响应（D1/D3/D4）。D2 有 `input_audio_buffer.committed.item_id`
  及 `conversation.item.created`；已读事件目录未给独立图像 commit/committed/
  clear/cleared。不能从音频 clear 推断图像清空，更不能伪造图像 ack。

**保留文档覆盖冲突**：D1 的 `conversation.item.create.item.type` 当前列
`function_call_output` 和 Qwen3.8 的 `mcp_approval_response`，没有
message/input_image；D3 却明确说可通过 `conversation.item.create` 发送纯文本
`input_text`。D2 可返回 message 也不能证明客户端可创建任意 message。
官网已含 Qwen3.8、MCP 和视频聚合，早期本地 raw 中“当前仅支持
function_call_output”不能作为当前全量枚举。可成立的窄结论是：**未找到独立图像
item 的正面请求/确认契约，明确文档化的图像路线与音频耦合**；不能声称所有
message item 均被真实上游拒绝。

**逐图 detail 与会话级聚合（O1、D1/D2/D3）**：

- D1 `session.video.input.representation_compact` 仅 Qwen3.8 可用；`none`
  为初始默认、保留细粒度表征，`normal` 聚合表征，官方称同视频输入 token 为
  none 的 1/4。它必须在首段音频前设置，音频开始后不可修改。
- D1 图像 append 无逐图 detail，D2 无该档位回显。不能说目标完全没有视觉
  精度/成本控制，但其作用域、时点、值域与 token 策略不等价于逐图 detail。
  本设计丢弃逐图档位，不生造 low→normal/high→none 映射，也不擅改会话设置。
- detail 的 DEGRADE 只记录一项独立损失，**不能抵消 vision 的 REJECT**，不意味
  图像请求可交付或应 Redeem detail。未来接受受限图像转换时仍需另审该损失。

**显式推理控制（O1、D1/D2/D3）**：

- O1 有 `session.update.session.reasoning.effort` 与
  `response.create.response.reasoning.effort`，值为
  `minimal/low/medium/high/xhigh`，限定 reasoning-capable Realtime 模型，
  如 `gpt-realtime-2`。
- 前置预研对 D1/D2 可见正文检索 reasoning、reasoning_effort、enable_thinking、
  thinking_budget 均 0 命中，D3 也未找到 reasoning/thinking 控制。D1 的
  `response.create` 仅文档化事件类型及 event_id 示例，未给承载此控制的 response
  配置对象；D2 会话/响应 schema 亦未确认该落点。
- 此结论仅限公开 WS 契约；不证明服务端一定报 unknown field，不声称模型没有
  内部推理。普通 HTTP Omni 的 enable_thinking、控制台家族能力或 SDK 任意 kwargs
  不替代 WS 契约；直接丢弃 effort 后得到普通回答不构成保留推理控制的证据。

### 7.3 反向路径说明与投放边界

`dashscope.realtime → openai.realtime` 保持既有处置，只订正理由：

- `parallel_tool_calls=DEGRADE`（O1、D1/D2）：两端没有通用并行调用策略的等价
  保证。DashScope 当前公开 Realtime 契约没有显式并行开关；OpenAI 已有
  session/response 的 `parallel_tool_calls`，但限定 reasoning Realtime 模型。
  并行行为采用目标模型语义，不能保证保留来源的调用调度；不能再称 OpenAI 无开关。
- `realtime_image_input/vision_input=REJECT`（O1/O2、D1/D2/D3）：OpenAI 有
  input_image 消息，却没有 DashScope 专用图像 buffer 与随音频共同提交的契约。
  当前拒绝把该音画 buffer 轮次隐式拆成独立图像消息，以免改变提交边界、生命周期
  与对话项关联；只针对从该 buffer 表达的视觉请求，不泛化为 OpenAI 无视觉输入。

这些设计 REJECT 源于已知契约差异与证据缺口，**不是因为转换器尚未实现**，也不
证明未来不可能实现受限转换。若官方补齐独立图像 item 或显式转换可保住关联/轮次，
须重新审阅；并行转换也须另验来源语义、目标有效配置与多 call_id 往返。

当前两条 C 路径无兑现端点，生产 `Matrix.Check` 先检查路径/门，再裁决能力，
因此仍为 **PLANNED / 501**。未来开门后的设计 REJECT 对应 unsupported/422，
可交付但未兑现仍为 501；下游 101 后只能用协议错误/关闭表达，不能再写 HTTP 状态。
同源 OpenAI 路径随表达性扩充为设计 PASS 15 项，但同样没有生产门兑现。
Task 5 不改变生产 Build、路由、Redeem 或两份兑现名单；历史跨协议
MarkHomogeneous 标记不构成同契约证据（见原则 2.2）。
