# DashScope Realtime 独立录制与证据表

日期：2026-10-04。本批真实尝试已达 **8/8，停止调用，禁止换 batch 绕过**。
取得 TTS commit、TTS server_commit、文本三轮双工具三份完整真实轨迹；其中文本候选
从第八次原录制离线恢复。本次源审查、原字节归档及正式 handler 链路离线回放已通过，
新增真实调用为 0。生产 Build 仍不开门、矩阵仍为 **PLANNED**，没有真实 gateway smoke。
已取得的原生负载保全证据与尚缺的逐能力语义证据分别列于下文，不能据此宣称整门可投放。

## 工具边界

- 入口：`tests/smoke/record_dsrealtime_live_test.go` 的 `TestRecordDSRealtime`。
- 独立编写请求，直接使用 WS transport 连接官方上游；不通过 gateway/provider 生成预期。
- 必须同时显式给出 `OMUGW_RECORD_DSREALTIME=1`、场景、模型、北京端点、输出目录及环境凭据。
  普通 `OMUGW_SMOKE=1` 不启用录制。
- 本批只接受精确 host `dashscope.aliyuncs.com`；intl/us 等其他地域在拨号前拒绝。
- 每次一个场景、一次拨号、一会话；无重试、无模型遍历。业务期限 59 秒，关闭最长 1 秒。
  同一 `batch` 下八个独占尝试槽，失败也消耗槽。控制器不得换批次绕过本批授权总量。
- Omni 每响应请求 `max_tokens=128`，确认回显后才提交输入；文本/工具场景最多三个
  response ID，其他场景最多一个。超额事件/用量导致停止。TTS 无相同 token 参数，
  只提交固定 13 字短句一次，音频累计最多 8 秒。
- 音频单声道、16 bit 小端裸 PCM、16000 Hz；输入最多 8 秒，VAD 另补最多 700 ms
  程序静音。输出累计最多 256000 字节。图片是程序绘制的 320×240 JPEG：左红圆、右蓝方块。
- 单消息最多 1 MiB，双向原始消息累计最多 1 MiB，消息记录最多 500 条。
  额外约束来自 testkit 默认预算：文件 8 MiB、轨迹 4 MiB、节点 4096 等。
- 人民币 10 元是保守预算目标，不能当实时账单硬保证；供应商异步结算、断连后的计算和
  文档所称截断行为不受本地计费器控制。应审阅原始 `response.done.response.usage`，不凭
  `session.finished` 或本地结束补造 usage。

## 隐私、来源和输出

录制器只允许固定公开短句及程序图形；本批未执行图形/音频输入场景。TTS 声音使用系统音色 Cherry，不使用个人录音、声音复刻、
用户照片或业务内容。音频输入只能读取带格式、公开短句、来源标记及 SHA-256 的
`audio-sample.json`，并核对真正存在的 `audio.pcm`。来源标记是自洽账本，不是认证；
控制器只使用本录制器从真实 TTS 取得的样本，不手写账本给 chirp 或个人录音换身份。

输出限制在仓库 `/.local/recordings/dsrealtime/<batch>/<run>/`（已 ignored）；目录独占
创建，文件独占创建且权限 0600。重复目录、链接目录、第九次尝试均在拨号前失败。
不写 `testdata/routes/`。

| 文件 | 内容 |
|---|---|
| `recording.json` | 请求模型、开始时间、实际握手状态、安全头、原始双向完整消息/opcode、实际 close、失败分类 |
| `candidate.json` | 成功取得必要协议证据后生成的同契约四点 fixture；经过 digest、ValidateWSSession 和 ReadWSFixture 校验 |
| `audio.pcm`、`audio-sample.json` | 成功 TTS 的可复用落盘样本；明确 pcm_s16le / 16000 / mono / 摘要 / 固定短句 |
| `input.pcm` | 音频场景实际使用的样本副本，`recording.json.input_sample.file` 引用它 |
| `image.jpeg` | 音频+图片场景上传的程序图形 |

只保留值完全符合白名单的 `Upgrade`、`Connection` 握手头；鉴权头、request ID、Cookie、
任意 Server 字段及握手错误 body 均不保存。事件中若发现凭据（包括 JSON 转义形式）或
鉴权字段，整条拒收，不能脱敏后还声称字节原样。日志只输出固定分类及计数，不输出
URL、原始头、secret、消息正文或 close reason。原始消息入库前须源审查；三份成功轨迹
已逐条解码审阅公开文本、固定工具、系统音色来源和安全头，未发现凭据或个人信息。
未试听或识别 PCM，不把来源审核冒充语音内容人工验收。独立审核状态见归档 README。

### 四点来源与关闭证据

- `client.send=authored`；`upstream.receive=upstream-accepted` 是独立请求预期，只在后继
  的真实配置回显、item ack、commit、response 或 session 终态支持时建立。音频 append
  的证明是整组提交，不是逐帧云端抓包。实际失败不会补造 ack。
- `upstream.send=recorded`；`client.receive=golden` 根据同契约保全要求派生。原消息字节
  不重编码，已知动态 ID 使用 testkit bind/reference；其他字段严格匹配，转发还检查字节。
- `After` 固化本次序列和四点接收关系，只声明本候选的保守调度，不将录制时序当协议通则。
- TTS 在配置、音频、response 终态及 `session.finished` 充分后，与 Omni 共用主动正常
  close 握手，不再假设业务完成后上游必主动关 socket。使用 transport `BeginClose`，
  将 `min(现在+1秒, 会话截止时间)` 传为绝对期限；同一期限覆盖等写锁、帧写、真实回应、
  TLS 清理及唤醒 worker。`finish` 中止剩余 I/O 并回收期限守卫，capture 再 join 读者和
  会话守卫；不会在 cleanup 重新获得一秒。只写出 close、raw EOF、超时、额外业务消息、
  非正常 close 或未确认配置都不能交付成功候选。
- `BeginClose`、自动 close 回应及最终 `CloseWithResult` 共享一次物理发送权；发送失败或
  半帧也不重新开始一帧，开始收尾后禁止新数据/控制帧写入，晚到 ping 不阻断继续读回应。
  普通未使用该接口的连接保留已验证的 `CloseWithResult` 生命周期。测试同时断言物理帧
  数、实际回应与 worker 退出；期限断言有 100ms 调度容差，不把该容差作为可续期预算。
- 对端先 close 时 transport 已自动回应，主动写可能返回 ErrClosed；录制器仍读取实际
  CloseError，原样保留收到的 code/reason，只在显式写成功时记 send。不能把幂等成功、
  自动回应或收到一帧推断成同码同 reason 的发送证据。自动回应不另造 send 记录。
- 原始轨迹保留可观察的 send/receive close；自动回应无独立发送结果，不补写 send。
  候选只表达主动关闭与对侧接收一对，自动 close 回应不重复
  排入回放；发送方主动关闭的接收证据来自匹配的实际回应。正常业务完成且关闭证据齐全
  才是 `completed`。取消响应场景为 `interrupted`，不伪造 completed 业务终态。
- 来源摘要只证明账本自洽。离线 TCP 脚本和回放通过不证明真实上游支持。

## 官方依据和待验证差异

采用官方客户端/服务端原文，不采用有冲突的 wiki 合成比较页：

- Omni：[客户端事件](https://help.aliyun.com/zh/model-studio/client-events)、
  [服务端事件](https://help.aliyun.com/zh/model-studio/server-events)、
  [实时使用指南](https://help.aliyun.com/zh/model-studio/realtime)。
- TTS：[实时语音合成指南](https://help.aliyun.com/zh/model-studio/realtime-tts-user-guide)，
  以及本机官方 raw 的 `qwen-tts-realtime-client-events.md` / `qwen-tts-realtime-server-events.md`。
- raw 根：`~/.config/opencode/skills/bailian-docs-llm-wiki/raw/`。Omni 参考位于
  `model-api-reference/omni-realtime-api/`；TTS 位于
  `model-api-reference/audio-api-references/speech-synthesis-api-reference/qwen-tts-realtime-api-reference/`。
- Omni 客户端事件页写 `conversation.item.create` 仅支持 `function_call_output`，但官方
  使用指南写可用 `message + input_text`。`text-tools-v3` 已取得这种输入的真实 ack 和
  完成回复；失败历史保持原样，没有回退输入方式。工具定义使用嵌套 `function` 对象。
- `session.updated` 名称不是配置采纳证明：`session.created` 与 `session.updated` 的
  model 必须明确等于请求模型；音频格式及采样率必须在新式 `audio` 结构回显一致，
  输出上限等请求字段仍逐项核对。只有下述 TTS 语言字段接受已观察到的一对大小写值。
- TTS `commit` 显式 commit；`server_commit` 不显式 commit，先等待服务端自动创建响应、
  同一 response_id 的合法非空音频，才发送一次 `session.finish` 收尾。completed done
  可在 finish 前或后，但必须属于同一 ID，且先于 session.finished；随后必须取得真实
  正常 close。只有 finish 后才出现音频、显式 commit、提前终结、跨 ID 或缺尾部均不算。
  驱动与后继见证/Coverage 共用这条证据链，缺少自动启动时按原期限失败并保留实际轨迹。
- text-tools 的作者请求省略可选 `item.id`，串行等待唯一 pending 的 type/role/content
  或 function_call_output 的 type/call_id/output 回显。ack 必须带非空、未复用的服务端 ID；
  显式提供的 ID 仍须严格相等。新增服务器字段只按 subset 核对，内容数组长度与顺序不放宽。
  错 ack、重叠 pending 或重复 ack 立即失败，不能跳过它们等待后面的同名事件；候选使用相同
  校验和真实 ID 的 bind/reference。官方客户端原文的 item.id 为可选，call_id/output 必选。
- 模型别名可能漂移，原始 `session.created` 的实际模型必须审阅；来源版本字符串是文档
  日期标签，不声称云端固定快照。本批固定北京端点，模型如下命令。

## 已执行批次与 Task 6 契约修正

批次固定为 `.local/recordings/dsrealtime/batch-20261004`，当前 **8/8**。八次尝试均保留，
失败也占槽，不存在本批剩余额度；本次归档/回放没有预留新槽。

| 次序 / 开始时间（北京时间，2026-10-04） | run | 结果及事实 |
|---|---|---|
| 1 / 12:38:52 | `tts-commit` | 101 后语言请求 Chinese、回显 chinese，配置未确认，未送业务短句 |
| 2 / 12:39:57 | `text-tools` | 101 后 turn_detection:null 被省略，配置未确认，未送文本 |
| 3 / 12:50:57 | `tts-commit-v2` | TTS 业务完成，字符25/token8+32；缺真实 close 回应，保留失败 |
| 4 / 13:22:52 | `tts-commit-v3` | 完整 commit：8 个音频 delta、1 个 completed done、finished、双向 close1000 |
| 5 / 13:23:12 | `tts-server-commit` | finish 前已有自动 created+2 个非空音频，旧驱动等 done 超时；失败原件不补尾 |
| 6 / 13:23:59 | `text-tools-v2` | 服务端重分配 item.id，旧驱动等作者 ID 超时；未发 response.create |
| 7 / 13:41:15 | `tts-server-commit-v2` | 完整自动提交：同 response 音频先于 finish，完成 done 后于 finish，双向 close1000 |
| 8 / 13:41:30 | `text-tools-v3` | 三轮业务、双工具及结果 ack、双向 close1000 完整；候选校验失败后仅离线修正 previous_item_id 规则并恢复 |

第 1/3/4/5/7 次请求及回显 `qwen3-tts-flash-realtime`，第 2/6/8 次为
`qwen3.5-omni-flash-realtime`，地域均为北京。八槽保持原样；没有第九次调用。
前两份 `recording.json` 的 payload 均已 base64 解码核验：各只有接收 created、发送 update、接收 updated 和本地 close，
101 成功后都以 `unconfirmed_config` 停止，`configuration_confirmed=false`。没有业务
输入、response、usage、成功候选或音频样本；usage 缺失不等于零费用。

| 真实调用（北京时间） | 请求及回显模型 | 业务输入前失败原因 |
|---|---|---|
| 12:38:52，`tts-commit/recording.json` | `qwen3-tts-flash-realtime` | 请求 `language_type=Chinese`，回显 `chinese`；mode=commit、voice=Cherry、response_format=pcm、sample_rate=16000 一致 |
| 12:39:57，`text-tools/recording.json` | `qwen3.5-omni-flash-realtime` | created 包含 server_vad；请求 `turn_detection:null` 后 updated 省略此字段；max_tokens/modalities/instructions/tools 一致 |

原文件 SHA-256（不改历史失败轨迹，不重新标记其配置已确认）：

- `tts-commit/recording.json`：`c739b82a7e14bd6734a016c9489d3924dbf89b1db64e63b53ca936b0cf504b3e`
- `text-tools/recording.json`：`422733c7d27dd5a94cc22dffa234d004d7ee4bea7c346b1e6d3db618ed775aa4`
- 第三次 `tts-commit-v2/recording.json`：`e058ed64c661109c893bc01fc120585cb50658cfba91d14734a3a210e9468977`

第三次真实调用为北京时间 12:50:57，模型仍为 `qwen3-tts-flash-realtime`。配置确认后
实际提交固定短句及 commit，收到了 8 条 audio.delta、completed response.done，随后
发送 session.finish 并收到了 session.finished。录制器继续等待供应商主动 close，约
15 秒后 `read_failed_or_raw_eof`；末尾只有本地 send close，没有实际 receive close。
历史文件保持失败，不能补造成功 candidate 或音频样本账本。

该次 response.done 原始计量为 characters=25、input/output/total_tokens=8/32/40，
input_tokens_details.text_tokens=8，output_tokens_details.text_tokens/audio_tokens=0/32。
字符数与固定短句的字数不同，照实保留 25；两种原始单位既不相加，也不推算成双份账单。
协议解析与账本的逐组校验、异常组保留策略详见
[用量契约](2026-10-04-dashscope-realtime-usage-contract.md)。

Task 6 的离线处理边界：

1. TTS 会话配置比较与请求后继见证共用窄规则，仅接受顶层 `Chinese → chinese`；
   请求仍发送 `Chinese`，原始消息、候选四点负载均不改写。其他语言大小写、voice、mode、
   model、格式仍严格匹配，通用字段比较与 item 见证不采用此特例。
2. text-tools 不再请求与文本输入无关的 `turn_detection`。回显省略或保留默认 server_vad
   都不影响文本配置确认；这不证明音频 manual 模式。
3. **audio-image 的 manual 确认证据仍有缺口**：继续发送 `turn_detection:null`，只接受
   显式 null 回显。若 updated 省略它，仍在业务输入前失败、Confirmed=false，不生成成功
   candidate；本次没有新增行为证据确认路径。不能以无输入时未发生 VAD、一次 committed
   或本文本轨迹推断 manual 已生效。
4. 官方 Omni `client-events.md:178` 规定请求 null 禁用 VAD、请求缺字段默认开启；
   `server-events.md:175` 起只称 updated 包含会话配置，未明确回显缺字段等于禁用。
   因此 missing/null/value 不做全局等同。缺少 max_tokens、model 或新式音频采样率，
   或仅有 legacy 音频格式仍拒绝。
5. 离线 TCP 回归使用两份实际回显的字面副本验证配置门禁，并保留失败负例、消息原字节、
   同 response 自动提交/取消证据链。后续合成业务事件仅验证工具，不填入真实能力账本。
6. 新增第三次业务形状的 TCP 回归：空 committed.item_id、response 输出嵌套结构、八条
   audio.delta、双单位 done、session.finished 后保持连接；只有客户端发 close 才回应。
   动态 ID 简化，音频缩为合成 PCM，关闭来自本地测试连接。成功候选经过完整 testkit 回放；
   同时保留 peer-first/竞争、raw EOF、无回应超时、额外消息、异常码负例。原实录未改写，
   合成关闭与本地回放成功不能升级其来源或证明真实服务已成功关闭。

### 后续两份失败实录：自动流与服务端 item ID

- `tts-server-commit/recording.json`：配置确认后 append，自动创建
  `resp_QioPULXAcyZPaOHfCkJqk`，两条同 ID audio.delta 分别严格 base64 解码为 **9788、9786 字节**。
  随后没有 done，也没有发送 finish，15 秒 idle 后失败；音频已经证明自动启动，旧驱动却
  在 finish 前等待完整终态。新驱动先核实自动启动，再 finish，并等待同 ID completed done、
  session.finished 和物理 close。原文件仍为失败，不补写尾部。
  SHA-256：`bae6fb047a6957689519c09eb577c264d73bacdaac902d89bce69e7860698ac4`。
- `text-tools-v2/recording.json`：请求 `record_user_1`，服务端 ack 为
  `item_Ji44D7s85jVY5djzY5naZ`，message/user/input_text 与固定文本完全一致，并新增
  object/status。旧驱动未发 response.create，等待指定 ID 至 idle 失败。新作者请求省略 ID，
  从唯一匹配 ack 绑定真实 ID；工具结果同样省略可选 ID，保留必需 call_id/output。
  SHA-256：`da1b0c6477c4f62202ecce03aeae623f9f7aac70e49aa9a4b85d8e5adea15951`。
- 回归使用上述形状与 ack 字面副本；TTS PCM 缩为合成数据，done/finished、工具后续轮次及
  正常 close 都来自独立本地 TCP 脚本。完整 candidate 回放只验证录制器，不冒充实录成功。

## 控制器显式命令

以下仅保留研究命令模板，**本批已经 8/8，不可再运行，不得换 batch 绕过**。
这些不是当前待执行队列，不覆盖任何历史目录，也不因缺口自动重试。
模板以仓库根为工作目录；`DASHSCOPE_API_KEY` 由安全环境提供，禁止把值写进命令或日志。

```bash
export OMUGW_SMOKE_WS_URL=wss://dashscope.aliyuncs.com/api-ws/v1/realtime
export DS_RECORD_BATCH="$PWD/.local/recordings/dsrealtime/batch-20261004"
```

### 1. TTS commit：先取得非个人短句音频

```bash
OMUGW_RECORD_DSREALTIME=1 OMUGW_RECORD_SCENARIO=tts-commit \
OMUGW_SMOKE_MODEL_REALTIME=qwen3-tts-flash-realtime \
OMUGW_RECORD_OUTPUT="$DS_RECORD_BATCH/tts-commit-retry1" \
go test -tags=smoke ./tests/smoke -run '^TestRecordDSRealtime$' -count=1 -timeout=75s -v
```

确认输出目录真正有 `audio.pcm` 和 `audio-sample.json`。检查 PCM 格式/采样率的实际回显
及 `response.done`，人工试听确为“请描述图片中的颜色和形状。”。可用本地音频工具按
`s16le / 16000 Hz / mono` 打开；没有这个实际文件，不执行后续音频场景。

### 2. TTS server_commit

验收顺序为 append → 自动 response.created → 同实体合法非空 audio.delta → 客户端 finish
→ 同 ID completed response.done → session.finished → 真实正常 close。done 先于 finish 也接受；
音频必须先于 finish，只有结束排空不算自动启动证据。

```bash
OMUGW_RECORD_DSREALTIME=1 OMUGW_RECORD_SCENARIO=tts-server-commit \
OMUGW_SMOKE_MODEL_REALTIME=qwen3-tts-flash-realtime \
OMUGW_RECORD_OUTPUT="$DS_RECORD_BATCH/tts-server-commit-retry1" \
go test -tags=smoke ./tests/smoke -run '^TestRecordDSRealtime$' -count=1 -timeout=75s -v
```

### 3. 文本多轮 + 同轮双工具 + 结果回传

```bash
OMUGW_RECORD_DSREALTIME=1 OMUGW_RECORD_SCENARIO=text-tools \
OMUGW_SMOKE_MODEL_REALTIME=qwen3.5-omni-flash-realtime \
OMUGW_RECORD_OUTPUT="$DS_RECORD_BATCH/text-tools-retry1" \
go test -tags=smoke ./tests/smoke -run '^TestRecordDSRealtime$' -count=1 -timeout=75s -v
```

第一轮记住“蓝色方块”；第二轮要求复述并同轮调用 `test_color` / `test_shape`；两个
不同 call_id 分别回传固定结果，再触发第三个响应。不会真的执行任何外部工具。
记忆能力只能由第二轮在收到工具结果**之前**的实际输出证明，不可用第三轮结果倒推。

### 4. Manual 音频 + ASR + 程序 JPEG

```bash
OMUGW_RECORD_DSREALTIME=1 OMUGW_RECORD_SCENARIO=audio-image \
OMUGW_SMOKE_MODEL_REALTIME=qwen3.5-omni-flash-realtime \
OMUGW_RECORD_AUDIO_SAMPLE="$DS_RECORD_BATCH/tts-commit-retry1/audio-sample.json" \
OMUGW_RECORD_OUTPUT="$DS_RECORD_BATCH/audio-image" \
go test -tags=smoke ./tests/smoke -run '^TestRecordDSRealtime$' -count=1 -timeout=75s -v
```

音频 append 后才发送 JPEG，以音频 commit 一并提交，再 response.create。按 committed
的 item_id 等待 ASR，即使转写晚于 response.done 也不漏录。人工核对转写与短句、回答
与红圆/蓝方块；单有 committed 不代表模型理解了图片。
若配置回显仍省略 `turn_detection`，当前录制器将提前失败；不得为继续录制而省略请求 null
或把缺字段解释为 manual。已有真实 TTS 样本但内容尚未试听/识别；本批额度已满，不能执行本场景。

### 5. server_vad + 音频输出期间取消

```bash
OMUGW_RECORD_DSREALTIME=1 OMUGW_RECORD_SCENARIO=vad-interrupt \
OMUGW_SMOKE_MODEL_REALTIME=qwen3.5-omni-flash-realtime \
OMUGW_RECORD_AUDIO_SAMPLE="$DS_RECORD_BATCH/tts-commit-retry1/audio-sample.json" \
OMUGW_RECORD_OUTPUT="$DS_RECORD_BATCH/vad-interrupt" \
go test -tags=smoke ./tests/smoke -run '^TestRecordDSRealtime$' -count=1 -timeout=75s -v
```

100 ms 一块按实时节奏输入，最多追加 700 ms 静音；真实 committed 后停止追加。收到
首个非空 audio.delta 的 response_id 必须等于尚未终结的 active response，才发送一次
response.cancel 并绑定取消目标。官方 cancel 不带 response_id，录制器不添加未公开字段；
它以发送时唯一活跃的音频响应确定目标，只接受该目标的 cancelled 终态和相同输入项的转写。
后继见证与 Coverage 也从同一条 created→audio→cancel→cancelled 链取节点，不按事件类型拼接。
不自动重新发起后续轮次。取消竞态失败、额外响应或未提交的
尾部音频均保留实际记录，不假装证明打断。

## 15 项能力的真实证据账本

### 三份归档与 handler 验收

归档根为 `testdata/fixtures/dashscope/realtime/`，不进入 routes。
文件全量摘要、原录制摘要、源审查和音频账本见
[归档 README](../../testdata/fixtures/dashscope/realtime/README.md)。下表别名用于节点引用：

| 别名 / 原字节候选 | SHA-256 | 节点 / response 终态 | 权威用量（输入/输出 token；音频输出 token；字符） |
|---|---|---|---|
| C / `tts-commit-v3.json` | `41eab869cd2456ce2244e887867306bf336c07ae403378842d5a291748f3e429` | 48 / `n0020_1` | 8/31；31；25 |
| A / `tts-server-commit-v2.json` | `a6140efd3485acda1d0b3dbbc0d6b9c6036f17bcddd92cc27c593efd68f80f52` | 46 / `n0020_1` | 8/36；36；25 |
| T / `text-tools-v3.json` | `2155931e0220da70a2693dbb45bb0fcf63a92fbd9eef6d516dfc7b41c7f277a2` | 102 / `n0016_1,n0030_1,n0049_1` | 三笔341/9、380/28、444/12，合计1165/49；无字符 |

三个原 recording 的全部消息分别按双点复制核对 opcode/payload，无重编码；
真实 send/receive close1000 均保留在 `recordings/` 原件。C/A 音频分别为 78294/91016 字节，
原 audio.delta 拼接、候选内嵌样本和源 audio.pcm 完全一致。

`TestWSRecordedConformance` 使用显式测试整门矩阵、`buildWithWS(..., true)`、正式 Mux、
WSHandler 与真实 provider 连接本地 WSReplayUpstream，由独立 ReplayWS 驱动双端。
首个 session.created 在下游 101 前送出，逐字节核验后才运行深拷贝 tail；tail 删除动态规则，
全部 ID 改为严格字面匹配，终态符号交接为原消息 response.id。原 fixture 完整加载校验，
仅测试内 tail 重算摘要，归档不变。详情见 README 的身份/终态交接说明。
46/44/100 个剩余节点及 1/1/3 个终态全部完成，原始因果顺序、鉴权替换、路径/模型、
音频及消息字节通过；所有工作者和 handler join、共享预算归零。计量预期使用上表独立字面值，
不调用 Inspect 生成预期；不把字符与 token 相加或据此推算账单。

### 逐项边界

下表“离线通过”只指这些已录制轨迹经过 handler 的保全，不代表所有模式都支持。
`nXXXX_0` 是原记录相应方向发送点，`nXXXX_1` 是另一侧接收点。

| 能力 | 真实证据 / 节点 | gateway 离线结果 | 当前缺口 |
|---|---|---|---|
| text_generation | T `n0010_1–n0012_1`、`n0040_1–n0045_1` 非空文本及3个 done | opcode、全文字节、response ID 通过 | 未做真实 gateway smoke |
| streaming | C `n0009_1–n0016_1`，A `n0007_1,n0009_1–n0016_1`，T 上述文本分片 | 顺序、字节、对应 done 通过 | 仅本次轨迹，不穷举调度 |
| tool_calling | T `n0021_1–n0030_1` 名称/参数/call_id；`n0031_0,n0033_0` 回传，`n0032_1,n0034_1` ack；`n0049_1` 最终 done | 原字节、call_id/前驱身份通过 | 仅固定公开工具，未执行外部工具 |
| parallel_tool_calls | T `n0030_1` 同一 response.output 含 test_color/test_shape 两个不同 call_id | 同轮完整 output 原字节通过 | 不代表工具执行并发性 |
| vision_input | 本批未执行 audio-image | 无真实轨迹可验 | 缺 JPEG 提交、真实回答及红圆/蓝方块人工核对 |
| audio_input | 本批未发送 PCM 输入 | 无真实轨迹可验 | 缺新式16000 Hz回显、committed/item/输入样本链；manual 确认仍缺 |
| audio_output | C/A `n0002_1` PCM/16000/Cherry 回显、非空 audio.delta 与 `n0020_1` | 原始 PCM、内嵌样本及 SHA 通过 | 只证明 TTS 输出，未验 Omni 音频输出 |
| speech_synthesis | C/A `n0003_0` 固定短句 → 上述 PCM/done | 输入/音色/格式/输出字节通过 | **未试听、未 ASR；不能声称合成内容人工通过** |
| speech_recognition | 本批未执行 ASR | 无真实轨迹可验 | 缺 transcription.completed、item_id 及短句内容对应 |
| stateful_conversation | T 第一轮确认口令；第二轮 `n0030_1` 只有双工具、无工具结果前复述 | 三轮消息保全通过，不能据此判语义通过 | **缺证据**；第三轮已获“蓝色/方块”工具结果，不可倒推记忆 |
| realtime_session | C/A/T `n0000_1,n0002_1` 配置，完整业务及close；C `n0022_1`/A `n0021_1` finished | 首事件、session 身份、更新及完整关闭通过 | 仅所请求配置；T 的默认 VAD 回显不算 VAD 行为证据 |
| realtime_server_vad | 本批未执行 vad-interrupt | 无真实轨迹可验 | 缺 speech_started/stopped/committed 同 item 因果链 |
| realtime_interrupt_turns | 本批未发送 cancel | 无真实轨迹可验 | 缺活跃同 response 音频→cancel→cancelled；合成拒绝测试不能代替 |
| realtime_image_input | 本批未发送 image append | 无真实轨迹可验 | 缺音频→图像→commit 及视觉理解；不能用文本回答替代 |
| realtime_commit_modes | C `n0004_0` commit→`n0005_1` committed；A `n0004_1` 自动created→`n0007_1` 同ID音频→`n0008_0` finish→`n0020_1` completed→`n0021_1` finished→close | 两模式配置、严格同ID及完整尾部通过 | 自动模式真正先音频后finish已证明；不外推其他模型 |

Coverage 不等于投放；八项有局部或完整原生轨迹，剩余七项缺真实闭环。
此外 speech_synthesis 的内容验证也未完成。未来仍需补齐上述缺口、逐项验收及真实 gateway smoke，
再独立评审整门投放。当前生产 Build=false、Phase1 PLANNED、Redeem/白名单均未改动。
失败轨迹保留诊断身份，不补造尾部或改成成功 fixture；源摘要不能靠重算洗白。

## 离线验证命令

```bash
OMUGW_RECORD_DSREALTIME=0 OMUGW_SMOKE=0 \
go test -tags=smoke ./tests/smoke -run '^TestDSRealtimeRecorderOffline' -count=1 -v

OMUGW_RECORD_DSREALTIME=0 OMUGW_SMOKE=0 \
go test -race -tags=smoke ./tests/smoke -run '^TestDSRealtimeRecorderOffline' -count=1

OMUGW_RECORD_DSREALTIME=0 OMUGW_SMOKE=0 \
go vet -tags=smoke ./tests/smoke

go test ./internal/gateway -run '^TestWS(Recorded|BuildGate)' -count=1 -v
go test -race ./internal/gateway ./internal/testkit ./internal/protocol/dashscoperealtime ./internal/provider/dashscoperealtime ./internal/transport/ws -count=1

OMUGW_RECORD_DSREALTIME=0 OMUGW_SMOKE=0 \
go test -tags=smoke ./tests/smoke -run '^TestRecordDSRealtime$' -count=1 -v
```

最后一条必须显示 SKIP。五种场景的本地合成脚本只证明录制工具；新增三份真实归档的
handler 回放证明对应原生负载保全。两者都没有生产 routes 文件、矩阵兑现或门注册变更。
