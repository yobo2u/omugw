# DashScope Realtime WebSocket 契约调研（`/api-ws/v1/realtime`）

- **日期**：2026-09-06（所有官方页面均于当日抓取核对）
- **目的**：为 `openai.realtime → dashscope.ws.realtime` 的 WebSocket 传输层设计提供线格式事实依据，
  并回答一个具体问题：**OpenAI Realtime 客户端零改动驱动 Qwen-Omni-Realtime，到底能对齐到什么程度。**
  本文件只记录官方第一手文档与官方 SDK 源码里写了什么、哪里没写。
- **范围**：DashScope Realtime WebSocket 端点（`wss://{host}/api-ws/v1/realtime`）的连接、鉴权、
  事件分类、音频编码、会话配置、错误与关闭语义。WebRTC 与 AOQ 两种传输只在影响选型判断处提及。
- **合规**：仅使用阿里云第一手文档（help.aliyun.com 中国站与 alibabacloud.com 国际站）与
  **Apache-2.0 的官方 SDK 源码**（`dashscope/dashscope-sdk-python`，已登记在 `docs/provenance.yaml`
  的 `planned_upstreams` 里，用途为 `protocol-reference`）。OpenAI 侧仅用其官方 Python SDK 的
  生成类型（Apache-2.0）做事件名对账。未读取任何被 `docs/provenance.yaml`
  `excluded_from_source_reading` 排除的仓库源码。

## 来源清单

| 编号 | 文档 / 源码 | URL |
|---|---|---|
| [R1] | 实时（Qwen-Omni-Realtime）（中国站，中文） | https://help.aliyun.com/zh/model-studio/realtime |
| [R2] | Qwen-Omni-Realtime（国际站，英文，页面标注 Last Updated: Sep 04, 2026） | https://www.alibabacloud.com/help/en/model-studio/realtime |
| [R3] | Realtime API 的客户端事件 | https://help.aliyun.com/zh/model-studio/client-events |
| [R4] | Realtime API 的服务端事件 | https://help.aliyun.com/zh/model-studio/server-events |
| [R5] | Realtime API 概述（AOQ / WebRTC / WebSocket 选型与模型支持矩阵） | https://help.aliyun.com/zh/model-studio/realtime-api-overview |
| [R6] | Qwen-Audio 实时语音对话 WebSocket API 参考（同一 `/api-ws/v1/realtime` 路径，握手头与错误分类写得最全） | https://help.aliyun.com/zh/model-studio/fun-audiochat-realtime-websocket-api |
| [R7] | 获取与配置 API Key | https://help.aliyun.com/zh/model-studio/get-api-key |
| [S1] | DashScope Python SDK，`dashscope/audio/qwen_omni/omni_realtime.py`（Apache-2.0，固定提交 `39c16c7`） | https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/audio/qwen_omni/omni_realtime.py |
| [O1] | OpenAI Python SDK GA Realtime 类型（Apache-2.0，固定提交 `be92815`） | https://github.com/openai/openai-python/tree/be928151372e4b62adb4a1571cda52ad759b38be/src/openai/types/realtime |
| [O2] | OpenAI Python SDK **beta** Realtime 类型（同提交） | https://github.com/openai/openai-python/tree/be928151372e4b62adb4a1571cda52ad759b38be/src/openai/types/beta/realtime |

> **本文最重要的一条结论先写在前面**：DashScope Realtime 对齐的是 OpenAI Realtime 的
> **beta 事件命名**，不是 OpenAI 当前 GA 的命名。四个高频事件名在 GA 侧已被重命名
> （`response.text.*` → `response.output_text.*`、`response.audio.*` → `response.output_audio.*` 等，
> 见 §4.3）。**「OpenAI Realtime 客户端零改动」这句话的真值取决于客户端 SDK 的版本**，
> 这直接决定传输层要不要做事件名双向改写，是本次调研对下游设计影响最大的发现。

## 1. 端点与地域

### 1.1 路径与查询参数

WebSocket 连接地址形如 [R1][R2]：

```
wss://{host}/api-ws/v1/realtime?model={model_name}
```

- 路径固定 `/api-ws/v1/realtime`，**与 HTTP 的 `/api/v1/...` 不同前缀**（`api-ws` 而非 `api`）[R1][R2]。
- **模型由查询参数 `model` 指定，不在任何事件体里**。[R6] 写得最直白：
  「URL 必须使用 `wss://` 协议。Authorization 在请求头中设置，模型通过 URL 查询参数 `model` 指定。」
  对网关的含义：**路由决策必须在 WebSocket 握手阶段完成**——此时还没有任何事件可读，
  模型名只能从 URL 查询串里取。这与 HTTP 路径「先收完 body 再路由」的形态根本不同。
- 注：`session.update` 的示例里确实出现了 `session.model` 字段 [R3]，但官方连接说明始终以查询参数
  为准；两者同时存在时以哪个为准**未文档化**（§9）。

### 1.2 地域域名

中国站 [R1] 与国际站 [R2] 在此完全一致，只列两个地域：

| 地域 | WebSocket 调用地址 |
|---|---|
| 华北2（北京） | `wss://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/api-ws/v1/realtime` |
| 新加坡 | `wss://{WorkspaceId}.ap-southeast-1.maas.aliyuncs.com/api-ws/v1/realtime` |

[R1] 明确：「**支持的地域：**北京、新加坡，需使用各地域的 API Key。」[R2] 同义。

**这与 HTTP 文本生成路径的地域清单不一致，是一处必须注意的收窄**：本仓库
`docs/research/2026-08-22-dashscope-native-text-generation-contract.md` §1.2/§1.3 记录 HTTP 侧有
北京、新加坡、美国（弗吉尼亚）、德国（法兰克福）、日本（东京）、中国香港六个地域；
**Realtime 只有两个**。配置层若照搬 HTTP 的地域枚举给 Realtime 用，会构造出官方从未承诺的 host。

### 1.3 业务空间专属域名是唯一被文档化的形态（但 SDK 默认值不是它）

[R1][R2] 给出的两个地址**都带 `{WorkspaceId}` 前缀**，文档没有为 Realtime 列出任何「旧域名」形态。

但官方 Python SDK 的默认 URL 是**不带 workspace 前缀的旧域名**
（[S1](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/audio/qwen_omni/omni_realtime.py#L228-L232)）：

```python
if url is None:
    url = f"wss://dashscope.aliyuncs.com/api-ws/v1/realtime?model={model}"  # noqa: E501
else:
    url = f"{url}?model={model}"
```

即 `wss://dashscope.aliyuncs.com/api-ws/v1/realtime` 这个 host **在官方 SDK 里是默认值，但在
Realtime 文档里从未出现**。同时 SDK 支持把业务空间通过**请求头** `X-DashScope-WorkSpace` 传递
（§2.2），这是与「把 WorkspaceId 编进 host」并列的第二种业务空间寻址方式。

对配置层的含义：Realtime 的 host 至少有三种在野形态——
`{WorkspaceId}.cn-beijing.maas.aliyuncs.com`（文档唯一形态）、
`{WorkspaceId}.ap-southeast-1.maas.aliyuncs.com`、
`dashscope.aliyuncs.com`（SDK 默认，文档未列）。
**不要把 host 硬编码成文档形态**，应当可配置；也不要假设 workspace 一定在 host 里。

### 1.4 传输协议不止 WebSocket

[R5] 给出三种传输：**AOQ（AI over QUIC）、WebRTC、WebSocket**，并明确 WebSocket 一列
「弱网对抗：差」「回声消除/降噪：无，需客户端自行处理」。WebRTC 的信令走
`POST https://{host}/api/v1/webrtc/realtime`（`Content-Type: application/sdp`，请求体是 Offer SDP，
成功 200 返回 Answer SDP）[R1][R2]。

**本仓库 Phase 1 明确只做 WebSocket**（README「不在 Phase 1」列出 WebRTC 传输），
此处记录只为说明：客户端若因弱网问题投诉，答案在传输选型而不在网关。

## 2. 鉴权与握手

### 2.1 必需头

```
Authorization: Bearer $DASHSCOPE_API_KEY
```

[R1][R2] 的连接配置表都只列这一个头，措辞为「使用 Bearer Token 鉴权」。
环境变量名 `DASHSCOPE_API_KEY`，[R1] 注明「DASHSCOPE_API_KEY 是您在百炼上申请的 API Key」。
Key 的地域隔离规则沿用平台通则：「需使用各地域的 API Key」[R1]，与 HTTP 侧一致 [R7]。

**与 OpenAI Realtime 的鉴权差异**：OpenAI Realtime 的浏览器直连路径依赖临时凭据
（`client_secret`，见 [O1] 中的 `client_secret_create_params.py`），DashScope Realtime 的官方文档
**没有对应的临时凭据签发接口**，WebSocket 直接携带长期 API Key。对网关的含义：
DashScope 侧不存在「短期 token 换发」这一层，凭据池只需处理长期 Key。

### 2.2 完整握手头（含官方文档化的可选头）

[R6] 是同一 `/api-ws/v1/realtime` 路径上写得最完整的一页，给出三个头：

| 参数 | 类型 | 是否必选 | 说明 |
|---|---|---|---|
| `Authorization` | string | **是** | 鉴权令牌，格式为 `Bearer <your_api_key>` |
| `user-agent` | string | 否 | 客户端标识，便于服务端追踪来源 |
| `X-DashScope-WorkSpace` | string | 否 | 阿里云百炼业务空间 ID |

官方 SDK 的实现与之逐条吻合
（[S1](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/audio/qwen_omni/omni_realtime.py#L257-L271)）：

```python
def _get_websocket_header(self):
    ua = get_user_agent()
    headers = {
        "user-agent": ua,
        "Authorization": "Bearer " + self.apikey,
        **get_sdk_headers(module="audio"),
    }
    if self.user_headers:
        headers = {**self.user_headers, **headers}
    if self.user_workspace:
        headers = {
            **headers,
            "X-DashScope-WorkSpace": self.user_workspace,
        }
    return headers
```

注意 SDK 的合并顺序：`{**self.user_headers, **headers}`——**用户自定义头会被 SDK 自带头覆盖**，
即用户无法通过 `headers=` 参数改写 `Authorization`。这是 SDK 行为，不是协议约束。

### 2.3 鉴权失败发生在握手阶段

[R6] 明确：「Authorization 鉴权在 WebSocket 握手阶段验证。如果 API Key 无效或缺失，
握手将失败并返回 **HTTP 401/403** 错误。」

**对网关的含义（重要）**：鉴权失败是 **HTTP 层**的失败，此时 WebSocket 尚未建立，
既没有 `error` 事件也没有关闭帧。所以出站鉴权错误的载体是 HTTP 状态码，
入站要回给客户端的却可能已经是一条已升级的 WebSocket——两侧错误信封不在同一层，
这个落差需要传输层显式处理。同时它也是**首字节前 failover 的最佳时机**：
握手 401/403 发生在任何下游字节写出之前，换凭据重试是安全的。

## 3. 事件传输形态

- 全部消息是 **JSON 文本帧**。SDK 收到二进制帧时直接记错误日志：
  `"should not receive binary message in omni realtime api"`
  （[S1](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/audio/qwen_omni/omni_realtime.py#L775-L783)）。
  即**音频不走二进制帧，一律 Base64 塞进 JSON**（与 OpenAI Realtime 的 WebSocket 传输同构）。
- 客户端事件带 `event_id`，SDK 生成规则为 `"event_" + uuid.uuid4().hex`
  （[S1](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/audio/qwen_omni/omni_realtime.py#L251-L255)）。
  [R3] 的示例里 `event_id` 均由客户端生成；服务端事件的 `event_id` 由服务端生成 [R4]。
- 服务端首个事件固定为 `session.created` [R4]，其中 `session.id` 形如 `sess_Ov7GOXoNXhNjlxXtOGKQS`。

## 4. 事件分类（本次调研的核心）

### 4.1 客户端 → 服务端（9 个）

出自 [R3]，并与 SDK 发送端逐一对账
（[S1](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/audio/qwen_omni/omni_realtime.py#L314-L690)）：

| 事件 | 作用 | 文档 | SDK 方法 |
|---|---|---|---|
| `session.update` | 更新会话配置；服务端校验后回 `session.updated` 或 `error` | [R3] | `update_session()` |
| `input_audio_buffer.append` | 追加 Base64 音频到输入缓冲区（字段名 `audio`） | [R3] | `append_audio()` |
| `input_audio_buffer.commit` | 提交音频缓冲区，创建用户消息项；**同时提交图像缓冲区** | [R3] | `commit()` |
| `input_audio_buffer.clear` | 清空音频缓冲区 | [R3] | `clear_appended_audio()` |
| `input_image_buffer.append` | **DashScope 独有**：追加 Base64 图像帧（字段名 `image`） | [R3] | `append_video()` |
| `conversation.item.create` | 回传工具执行结果；**当前仅支持 `function_call_output` 类型的 item** | [R3] | `create_item()` |
| `response.create` | 触发模型生成响应 | [R3] | `create_response()` |
| `response.cancel` | 取消进行中的响应 | [R3] | `cancel_response()` |
| `session.finish` | **DashScope 独有**：主动结束会话 | [R1] | `end_session()` / `end_session_async()` |

`session.finish` 未出现在 [R3] 的客户端事件页，但 [R1] 正文写明「每通会话结束后，发送
`session.finish` 事件关闭会话，或直接断开 WebSocket 连接」，SDK 也确实发送它
（[S1](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/audio/qwen_omni/omni_realtime.py#L541-L548)）。
**文档页与正文的覆盖不一致，本文以两者并集为准。**

### 4.2 服务端 → 客户端（26 个）

出自 [R4]（顺序即文档顺序）：

`error`、`session.created`、`session.updated`、
`input_audio_buffer.speech_started`、`input_audio_buffer.speech_stopped`、
`input_audio_buffer.committed`、`input_audio_buffer.cleared`、
`conversation.item.created`、
`conversation.item.input_audio_transcription.delta`、
`conversation.item.input_audio_transcription.completed`、
`conversation.item.input_audio_transcription.failed`、
`response.created`、`response.done`、
`response.text.delta`、`response.text.done`、
`response.audio.delta`、`response.audio.done`、
`response.audio_transcript.delta`、`response.audio_transcript.done`、
`response.function_call_arguments.delta`、`response.function_call_arguments.done`、
`response.output_item.added`、`response.output_item.done`、
`response.content_part.added`、`response.content_part.done`

外加 `session.finished`——**[R4] 未收录**，但 SDK 显式分支处理它
（[S1](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/audio/qwen_omni/omni_realtime.py#L725-L729)）：
收到 `session.finished` 才认为 `session.finish` 完成，否则 20 秒超时后强制 `close()`。
**这是一条只能从源码得知、文档没写的契约**。

### 4.3 与 OpenAI Realtime 的逐名对账（本节是下游设计的直接输入）

方法：取 DashScope 的 35 个事件名（§4.1 + §4.2），与 OpenAI 官方 Python SDK 生成类型里的
`Literal[...]` 事件名做集合运算。GA 命名取自 [O1]，beta 命名取自 [O2]，同一提交 `be92815`。

**结论一：32/35 与 OpenAI beta 命名逐字节相同。**

以下事件名在 DashScope 与 OpenAI beta 两侧完全一致（可直接透传，无需改写）：

```
error
session.update            session.created           session.updated
response.create           response.created          response.done
response.cancel
input_audio_buffer.append     input_audio_buffer.commit    input_audio_buffer.clear
input_audio_buffer.committed  input_audio_buffer.cleared
input_audio_buffer.speech_started   input_audio_buffer.speech_stopped
conversation.item.create      conversation.item.created
conversation.item.input_audio_transcription.delta
conversation.item.input_audio_transcription.completed
conversation.item.input_audio_transcription.failed
response.text.delta           response.text.done
response.audio.delta          response.audio.done
response.audio_transcript.delta   response.audio_transcript.done
response.function_call_arguments.delta   response.function_call_arguments.done
response.output_item.added    response.output_item.done
response.content_part.added   response.content_part.done
```

（`error` 事件两侧同名，且结构一致，其 `{type,code,message,param}` 子对象也逐字段对齐——见 §7。上表共 32 项。）

**结论二：只有 3 个是 DashScope 独有的。**

| 事件 | 方向 | OpenAI 对应物 |
|---|---|---|
| `input_image_buffer.append` | C→S | **无**。OpenAI Realtime 无独立图像缓冲区 |
| `session.finish` | C→S | **无** |
| `session.finished` | S→C | **无** |

**结论三（最关键）：DashScope 跟的是 beta 命名，OpenAI GA 已经改名。**

对同一提交 [O1] 的 GA 类型做差集，以下 6 个 DashScope 事件名**在 GA 命名里不存在**：

| DashScope（= OpenAI beta） | OpenAI GA 对应名 |
|---|---|
| `response.text.delta` | `response.output_text.delta` |
| `response.text.done` | `response.output_text.done` |
| `response.audio.delta` | `response.output_audio.delta` |
| `response.audio.done` | `response.output_audio.done` |
| `response.audio_transcript.delta` | `response.output_audio_transcript.delta` |
| `response.audio_transcript.done` | `response.output_audio_transcript.done` |

反向差集同样重要——以下 OpenAI（GA 与 beta 都有）的事件**DashScope 完全没有**，
说明「客户端能发但上游不认」的面比想象的大：

`conversation.item.delete` / `deleted`、`conversation.item.retrieve` / `retrieved`、
`conversation.item.truncate` / `truncated`、`output_audio_buffer.clear` / `cleared`、
`conversation.created`、`rate_limits.updated`、`input_audio_buffer.timeout_triggered`、
`input_audio_buffer.dtmf_event_received`、`conversation.item.added` / `done`、
`conversation.item.input_audio_transcription.segment`、
`output_audio_buffer.started` / `stopped`、以及全部 `mcp_list_tools.*` / `response.mcp_call*`。

其中 **`conversation.item.truncate` 的缺席对实时打断语义影响最大**：OpenAI 客户端用它告诉服务端
「音频只播到第 N 毫秒，请据此裁剪上下文」，DashScope 无此事件。

> **对降级矩阵的直接含义**（结论，非文档原文）：
> `realtime_interrupt_turns` 这一格不能想当然按 PASSTHROUGH 记。DashScope 侧的打断能力来自
> `semantic_vad` 与 `response.cancel`，而**不是** OpenAI 的 `conversation.item.truncate`；
> 两者语义不等价。同理 `realtime_commit_modes` 需要区分「VAD 自动提交」与「手动 commit」
> 两条路径（§6.2），而 `realtime_image_input` 在 DashScope 侧是独有能力
> （`input_image_buffer.append`），在 OpenAI Realtime 侧无对应线格式——按本仓库口径这属于
> 「协议表达不出」而非「路径丢失」，应记入 `Elsewhere`/`Impossible` 而非路径规则。
> 以上是本文的推断，落矩阵前需由设计者确认。

## 5. 音频编码与采样率

### 5.1 两代字段并存（且中英文档不同步）

**新式（推荐）**：`session.audio.input.format` / `session.audio.output.format` [R3]

| 字段 | 取值 | 默认 |
|---|---|---|
| `audio.input.format.type` | `pcm`（单声道、16 bit 裸 PCM）、`wav`（WAV 容器封装的单声道 16 bit PCM） | `pcm` |
| `audio.input.format.sample_rate` | `8000` / `16000` / `24000` / `48000` | `16000` |
| `audio.output.format.type` | `pcm`、`wav` | `pcm` |
| `audio.output.format.sample_rate` | `8000` / `16000` / `24000` / `48000` | `24000` |

**适用模型被明确限定**：[R3] 写「**适用模型：`qwen3.5-omni-plus-realtime`、
`qwen3.5-omni-flash-realtime`**」，[R1] 补充「音频格式与采样率可配置能力仅适用于
`qwen3.5-omni-plus-realtime` 和 `qwen3.5-omni-flash-realtime` 模型」。

**历史兼容（legacy）**：顶层 `input_audio_format` / `output_audio_format` [R3]，
[R1] 称「历史兼容字段 `input_audio_format` / `output_audio_format` 仍有效」。

**中英文档在此明显不同步，是本次调研发现的第二处硬差异**：

- 中国站 [R1][R3]：格式可选 `pcm` / `wav`，采样率四档可配。
- 国际站 [R2]：`session.update` 示例注释写
  「The input audio format. **Only "pcm" is supported.** The input audio must be a PCM audio stream at a **16 kHz** sample rate.」
  与「The output audio is a PCM audio stream at a **24 kHz** sample rate.」
- 服务端事件页 [R4] 的 `session.created` 字段说明同样是旧口径：
  「`input_audio_format`：当前仅支持设为 `pcm`。输入音频要求为 16 kHz 采样率的 PCM 音频流」、
  「`output_audio_format`：……输出音频为 24 kHz 采样率的 PCM 音频流。**当前不支持自定义输出采样率**。」

即**同一平台的三个官方页面对「输出采样率能不能改」给出相反答案**。安全口径：
**按 16 kHz 入 / 24 kHz 出 的 PCM 为基线**（三处文档都成立），
可配置采样率视为 `qwen3.5-omni-*-realtime` 上的增强能力，且必须实测复核（§9）。

### 5.2 SDK 侧的常量与互斥规则

SDK 的 `AudioFormat` 枚举只有两个成员
（[S1](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/audio/qwen_omni/omni_realtime.py#L64-L68)）：

```python
class AudioFormat(Enum):
    # format, sample_rate, channels, bit_rate, name
    PCM_16000HZ_MONO_16BIT = ("pcm", 16000, "mono", "16bit", "pcm16")
    PCM_24000HZ_MONO_16BIT = ("pcm", 24000, "mono", "16bit", "pcm16")
```

注意末位的 `format_str` 是 **`"pcm16"`**——这正是 OpenAI Realtime 的 `input_audio_format` 取值命名，
即 legacy 字段在线上传的值与 OpenAI 同名。新式 `AudioFormatType` 枚举则是 `"pcm"` / `"wav"`
（[S1](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/audio/qwen_omni/omni_realtime.py#L91-L118)）。
**同一概念在两代字段里取值命名不同（`pcm16` vs `pcm`），改写时不能直接搬运字符串。**

顺带一提，这里有一处对齐得很好的巧合：DashScope 的 legacy `input_audio_format: "pcm16"` 与
**OpenAI beta** 的 `pcm16` 取值同名 [O2]，而 OpenAI **GA** 已改用 MIME 风格的 `audio/pcm`
（类型注释写「Always `audio/pcm`」，固定 24 kHz）[O1]。即音频格式字段与 §4.3 的事件名一样，
**DashScope 对齐的是 beta 而非 GA**——同一条分界线，再次出现。

两代字段**互斥**，SDK 显式防止混发
（[S1](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/audio/qwen_omni/omni_realtime.py#L329-L361)）：

```python
if input_audio_config is None and output_audio_config is None:
    # old-style: keep backward compatibility
    self.config["input_audio_format"] = input_audio_format.format_str
    self.config["output_audio_format"] = output_audio_format.format_str
    return
```

docstring 补充：「When only one side is provided, the other side is omitted from the request and the
server-side default applies (input: pcm/16000, output: pcm/24000); **the legacy top-level fields are
NOT mixed in, to avoid sending both structures in one request.**」

### 5.3 配置时机

[R3] 对输入侧写：「建议在会话 IDLE 阶段（**首次发送音频前**）完成配置，**发送音频后不可再修改**」；
对输出侧写「建议在会话建立初期、尚未开始音频交互前完成配置」。
即音频格式不是任意时刻可改的会话参数，`session.update` 的幂等性在这一项上有时间窗约束。

### 5.4 图像输入约束

[R3] 对 `input_image_buffer.append` 给出硬约束：

- 格式必须 **JPG / JPEG**；建议 480p 或 720p，**最高不超过 1080p**。
- **单张图片 Base64 编码后不得超过 256 KB**，建议编码前原图不超过 190 KB。
- 建议 **1 张/秒** 的频率发送。
- **发送图像事件前，至少已发送过一次 `input_audio_buffer.append`**（图像不能独立于音频存在）。
- 图像缓冲区与音频缓冲区**由 `input_audio_buffer.commit` 一起提交**（没有独立的 image commit）。

## 6. 会话配置字段与 OpenAI 对照

### 6.1 `session.update` 字段全表（出自 [R3]）

| 字段 | 类型 | 取值 / 默认 | OpenAI Realtime 对应 |
|---|---|---|---|
| `modalities` | array | `["text"]` 或 `["text","audio"]`（默认后者） | 同名（beta）。**注意 [R4] 的 error 示例显示 `["audio"]` 单独传非法** |
| `voice` | string | 默认随模型：Qwen3.5 系 `Tina`、qwen3-omni-flash `Cherry`、qwen-omni-turbo `Chelsie`；共 55 种音色 [R1] | 同名，但取值集完全不同（OpenAI 是 alloy/echo/… ） |
| `instructions` | string | 系统消息 | 同名 |
| `audio.{input,output}.format.{type,sample_rate}` | object | §5.1 | **无对应**（OpenAI beta 用扁平 `input_audio_format`） |
| `input_audio_format` / `output_audio_format` | string | 历史兼容字段 | 同名（OpenAI beta），取值 `pcm16` 一致 |
| `turn_detection` | object / null | 见 §6.2 | 同名 |
| `input_audio_transcription` | object | `model` **固定 `qwen3-asr-flash-realtime`，不支持修改** [R4] | 同名，但 OpenAI 可选 whisper 等模型 |
| `tools` | array | `{type:"function", function:{name,description,parameters}}` | 同名但**结构不同**：OpenAI Realtime 的 tool 是**扁平**的 `{type,name,description,parameters}`，DashScope **嵌套在 `function` 下**（Chat Completions 风格） |
| `enable_search` | boolean | 默认 false，仅 Qwen3.5-Omni-Realtime 系 | **无对应**（DashScope 独有） |
| `search_options.enable_source` | boolean | 需先开 `enable_search` | **无对应** |
| `smooth_output` | boolean/null | 仅 Qwen3-Omni-Flash-Realtime 系；true=口语化（默认）、false=书面化、null=模型自选 | **无对应** |
| `temperature` | float | [0,2)；默认 Qwen3.5 系 0.7 / qwen3-omni-flash 0.9 / turbo 1.0 | 同名 |
| `top_p` | float | (0,1.0]；默认 0.8 / 1.0 / 0.01 | **无对应** |
| `top_k` | integer | ≥0，null 或 >100 禁用；默认 20 / 50 / 20 | **无对应** |
| `max_tokens` | integer | 默认与上限均为模型最大输出长度 | 近似 `max_response_output_tokens`（**名字不同**） |
| `repetition_penalty` | float | >0；默认 1.0 / 1.05 / 1.05 | **无对应** |
| `presence_penalty` | float | [-2.0,2.0]；默认 1.5 / 0.0 / 0.0 | **无对应** |
| `seed` | integer | **[0, 2^31−1]，默认 -1** | **无对应** |

`qwen-omni-turbo` 系列对 `temperature`/`top_p`/`top_k`/`max_tokens`/`repetition_penalty`/
`presence_penalty`/`seed` 一律标注「**不支持修改**」[R3]。

`seed` 默认 -1 而取值范围写 [0, 2^31−1]——**默认值落在合法范围之外**，说明 -1 是「未设置」哨兵。
这与本仓库「采样参数用指针以区分 0 与未设置」的约定同源，但 DashScope 用的是 -1 而非 null。

`max_response_output_token`（**单数，无 s**）出现在 [R4] 的 `session.updated` 响应示例里，取值 `"inf"`，
但**未出现在任何字段说明表**中——疑似服务端为兼容 OpenAI 而回显的字段（OpenAI 侧名为
`max_response_output_tokens`，复数）。拼写差异记入 §9。

### 6.2 `turn_detection`（VAD）

| 字段 | 取值 | 默认 |
|---|---|---|
| `type` | `server_vad`（基于声学特征）/ `semantic_vad`（基于语义，**仅 Qwen3.5-Omni-Realtime 系支持**） | `server_vad` |
| `threshold` | **[-1.0, 1.0]** | 0.5 |
| `silence_duration_ms` | [200, 6000] | 800 |
| `idle_timeout_ms` | [5000, 30000]；仅 `qwen3.5-omni-{plus,flash}-realtime` 且 `server_vad` 时生效 | — |

设为 `null` 即禁用 VAD，转为手动模式（客户端自行 `commit` + `response.create`）[R3]。

**`threshold` 范围 [-1.0, 1.0] 与 OpenAI Realtime 的 [0.0, 1.0] 不同**——负值区间是 DashScope 独有的，
直接透传 OpenAI 客户端的取值是安全的（是子集），反向则不然。

`idle_timeout_ms` 的语义值得注意：超时后**模型会主动发起一轮响应**引导用户继续对话
（[R3]：「模型将主动触发一轮响应，基于当前上下文引导用户继续对话」）。
这意味着**服务端可能在客户端毫无动作时凭空推出一个 response**，代理层不能假设
每个 `response.created` 都由某个客户端事件触发。

`session.created` / `session.updated` 回显的 `turn_detection` 里还包含
`prefix_padding_ms`、`create_response`、`interrupt_response` 三个字段（见 [R4] 两处示例 JSON），
但**它们不在 [R3] 的可配置字段表里**——即服务端回显了客户端无法配置的字段（这三个都是
OpenAI Realtime 的标准字段）。记入 §9。

## 7. 错误信封与关闭语义

### 7.1 `error` 事件结构

出自 [R4]，与 OpenAI **完全同构**：

```json
{
  "event_id": "event_RoUu4T8yExPMI37GKwaOC",
  "type": "error",
  "error": {
    "type": "invalid_request_error",
    "code": "invalid_value",
    "message": "Invalid modalities: ['audio']. Supported combinations are: ['text'] and ['audio', 'text'].",
    "param": "session.modalities"
  }
}
```

对照 OpenAI 官方 SDK 的 GA 类型
（[O1](https://github.com/openai/openai-python/blob/be928151372e4b62adb4a1571cda52ad759b38be/src/openai/types/realtime/realtime_error_event.py)）：
`RealtimeErrorEvent` 同样是 `{event_id, type:"error", error:{...}}`，
`RealtimeError` 同样含 `type` / `code` / `message` / `param`。
**这是全表对齐度最高的一处：`error` 事件可以字节级透传。**

### 7.2 错误是否中断连接（关键的可用性契约）

[R6] 给出明确的二分（同一 `/api-ws/v1/realtime` 路径）：

| 错误类型 | 连接状态 | 触发原因 |
|---|---|---|
| 客户端错误（`invalid_request_error`） | **连接保持，仅通知** | 参数不合法、状态不允许、`item_id` 重复 |
| 服务端错误（`server_error`） | **连接终止** | LLM 连接失败、存储故障 |

即 `error.type` 是**代理层判断「这条连接还能不能用」的唯一依据**。
Qwen-Omni-Realtime 的 [R4] 没有复述这张表，但两页描述的是同一端点的同一 `error` 事件结构。
（严格说，这张表出自 Qwen-Audio Realtime 页；是否逐字适用于 Qwen-Omni-Realtime 未经该页确认，
记入 §9。）

另有 `conversation.item.input_audio_transcription.failed` 作为**独立于 `error` 的转录失败通道**，
[R4] 明确「此事件独立于 `error` 事件，便于客户端识别」，其 `error` 子对象只有
`code` / `message` / `param`（**无 `type`**）。

### 7.3 WebSocket 关闭码：官方未文档化

抓取的全部页面都**没有给出 DashScope Realtime 的 WebSocket 关闭码表**。可确证的只有：

- SDK 把关闭码原样透传给回调，不做任何解释
  （[S1](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/audio/qwen_omni/omni_realtime.py#L785-L791)）：
  ```python
  def _on_close(self, ws, close_status_code, close_msg):
      self.callback.on_close(close_status_code, close_msg)
  ```
- 官方示例主动关闭时用 **1000**（正常关闭）：Java 示例 `conversation.close(1000, "bye")`
  与 `conversation.close(1000, "正常结束")` [R1][R2]。
- 会话时长上限 **120 分钟**，「达到此上限后服务将主动关闭连接」[R1][R2]——**但用什么关闭码没写**。

**结论：关闭码语义是未知量，必须实测归纳。** 不要在传输层基于关闭码做重试/降级判定，
否则等于在猜一个没有契约的值（§9）。

### 7.4 无文档化的心跳

Qwen-Omni-Realtime 的页面未提心跳。平台另一条 WebSocket 协议（多模态交互套件）有
「任意连续 60 秒服务端未发消息即断开并返回 `ResponseTimeout`，客户端应定期发送心跳」的规则
（https://help.aliyun.com/zh/model-studio/multimodal-interaction-protocol ），
**但那是另一套协议，不能直接套用到 `/api-ws/v1/realtime`**。此处仅作为「同平台存在此类规则」的
旁证，不作为本端点的契约（§9）。

## 8. 支持的模型

### 8.1 Qwen-Omni-Realtime 系列

[R1][R2] 的「使用限制」表列出三个模型及其上下文限制：

| 模型 | 音频最大轮次 | 视频最大轮次 | 音频最大时长 | 视频最大时长 |
|---|---|---|---|---|
| `qwen3.5-omni-plus-realtime` | 100 轮 | 50 轮 | 600 秒 | 240 秒 |
| `qwen3.5-omni-flash-realtime` | 80 轮 | 50 轮 | 480 秒 | 120 秒 |
| `qwen3-omni-flash-realtime` | 8 轮 | 8 轮 | — | — |

[R3] 的 `voice` 默认值说明另外确认了第四个系列的存在：**`qwen-omni-turbo-realtime`**（默认音色
`Chelsie`），且其大量采样参数「不支持修改」。

按能力分层（[R1][R3] 交叉）：

- **`semantic_vad`**：仅 Qwen3.5-Omni-Realtime 系列。
- **可配置音频格式/采样率**：仅 `qwen3.5-omni-plus-realtime`、`qwen3.5-omni-flash-realtime`。
- **`enable_search`（联网搜索）**：仅 Qwen3.5-Omni-Realtime 系列。
- **`idle_timeout_ms`**：仅 `qwen3.5-omni-{plus,flash}-realtime` 且 `server_vad`。
- **`smooth_output`**：仅 Qwen3-Omni-Flash-Realtime 系列。
- **声音复刻**：仅 `qwen3.5-omni-plus-realtime`、`qwen3.5-omni-flash-realtime` [R1]。

**能力是按模型分层的，不是端点统一的**——同一个 `/api-ws/v1/realtime` 端点，
换个 `model` 查询参数，可用的 session 字段集就变了。

### 8.2 同端点上的其他模型族

[R5] 的支持矩阵显示 `/api-ws/v1/realtime` 这条 WebSocket 通道并非 Omni 专属：

| 类型 | 模型 | WebSocket |
|---|---|---|
| 实时全模态 | `qwen3.5-omni-{plus,flash}-realtime` | 支持 |
| 实时语音翻译 | `qwen3.5-livetranslate-flash-realtime` | 支持 |
| 多模态开发套件 | `multimodal-dialog` | 支持 |
| 实时语音识别 | Qwen-Audio-3.0-ASR-Flash-Streaming、Fun-ASR-Realtime 系列 | 支持 |
| 实时语音合成 | CosyVoice 系列、`qwen-audio-3.0-tts-{flash,plus}` | 支持 |
| 实时语音对话 | `qwen-audio-3.0-realtime-{plus,flash}` | 支持 |

[R6] 证实 `qwen-audio-3.0-realtime-*` 走的正是同一个
`wss://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/api-ws/v1/realtime?model=<model_name>`。

**注意**：本仓库把 `dashscope.inference`（`/api-ws/v1/inference`，run-task 指令流）与
`dashscope.realtime`（`/api-ws/v1/realtime`）拆成两个协议，[R5] 的这张表印证了这个切分是必要的——
但也说明**同一条 realtime 路径上跑着多个能力差异极大的模型族**，
`realtime_session` 这类能力声明必须绑定到具体模型，不能只绑端点。

## 9. 未文档化 / 存疑清单

设计时必须为以下各点保留显式决策，不能默认它们有确定行为：

1. **WebSocket 关闭码语义完全未文档化**（§7.3）。含 120 分钟上限触发时用什么码。
   **不要基于关闭码做重试判定。**
2. **音频格式/采样率的中英文档互相矛盾**（§5.1）：中国站说 `pcm`/`wav` + 四档采样率可配，
   国际站与服务端事件页说「仅 pcm、输入 16 kHz、输出 24 kHz 且不支持自定义输出采样率」。
3. **`session.finish` / `session.finished` 未进事件参考页**（§4.1、§4.2），只在 [R1] 正文与 SDK 源码里。
   SDK 用 20 秒超时兜底，说明服务端不保证一定回 `session.finished`。
4. **`model` 同时出现在 URL 查询参数与 `session.update` 体内**（§1.1），冲突时以谁为准未写。
5. **SDK 默认 host `dashscope.aliyuncs.com` 未在 Realtime 文档中出现**（§1.3）；
   它与 workspace 专属域名是否等价、是否会下线，未说明。
6. **[R6] 的错误二分表（invalid_request_error 保持连接 / server_error 终止连接）
   出自 Qwen-Audio Realtime 页**，是否逐字适用于 Qwen-Omni-Realtime 未经 [R4] 确认（§7.2）。
7. **`turn_detection` 回显了三个不可配置字段**（`prefix_padding_ms`、`create_response`、
   `interrupt_response`），它们是否真正生效未文档化（§6.2）。
8. **`max_response_output_token`（单数）只出现在响应示例里**，无字段说明；
   与 OpenAI 的 `max_response_output_tokens`（复数）拼写不同（§6.1）。
9. **心跳/空闲超时**：Qwen-Omni-Realtime 页面未提；平台另一套 WebSocket 协议有 60 秒规则，
   但不可直接套用（§7.4）。
10. **`error` 事件的 `code` 取值集合未给出清单**；[R4] 只给了 `invalid_value` 一个样例。
    Realtime 专属错误码页在 help.aliyun.com 上未找到（尝试 `qwen-omni-realtime-error-code` 返回 404）。
11. **鉴权失败的 401 与 403 如何区分**：[R6] 写「返回 HTTP 401/403」，未说明何时 401、何时 403。
12. **并发/限流**：[R1] 指向「限流」页，本次未展开；Realtime 连接数限制未在本次抓取范围内确认。

## 10. 对 omugw 传输层设计的直接影响（结论区，非文档原文）

以下是本文的推断与建议，**不是官方文档的表述**，落地前需设计者裁定：

1. **路由必须在握手期完成。** 模型名只在 URL 查询参数里（§1.1），
   代理在收到第一个 WebSocket 帧之前就必须完成「模型 → 上游」的决策。
   这与 HTTP 路径的「解码 body 再路由」在时序上完全不同，`internal/gateway` 的
   `serve → dispatch` 主链路无法直接复用。

2. **事件名改写是必需项，不是可选项。** §4.3 的结论三意味着：
   面向 OpenAI **GA** SDK 的客户端会收到 `response.text.delta`，而它只认
   `response.output_text.delta`。要兑现 README 里「OpenAI Realtime 客户端零改动驱动
   Qwen-Omni-Realtime」的承诺，传输层**必须**做这 6 个事件名的双向改写，
   否则「零改动」只对 beta 版 SDK 成立。**建议把「目标客户端是 GA 还是 beta 命名」
   作为一个显式的配置维度或降级矩阵坐标，而不是隐式假设。**

3. **首字节前 failover 的窗口很清晰。** 握手 401/403（§2.3）发生在任何下游字节之前，
   可安全换凭据重试；一旦 `session.created` 已经转发给下游，就落入本仓库
   「首字节后不重试」的既有约束。

4. **`error.type` 是连接生死的唯一判据**（§7.2），而**关闭码不是**（§7.3）。
   错误分类应从 `error.type` 推导，不要从 close code 猜。

5. **能力声明要绑模型而非绑端点**（§8.1）。同一端点换 `model` 就换掉了可用字段集，
   把 `realtime_*` 五项能力按端点统一声明会失真。

6. **三项能力的矩阵处置需要重新审视**：`realtime_interrupt_turns`（DashScope 无
   `conversation.item.truncate`）、`realtime_commit_modes`（VAD 自动 vs 手动两条路径）、
   `realtime_image_input`（DashScope 独有，OpenAI Realtime 表达不出）。详见 §4.3 的引用块。

## 附录 A：最小连接样例（出自官方文档原文 [R1]）

```python
# pip install websocket-client
import json
import websocket
import os

API_KEY=os.getenv("DASHSCOPE_API_KEY")
# 以下为华北2（北京）地域的URL。请将 {WorkspaceId} 替换为您的百炼业务空间ID，各地域的URL不同。
API_URL = "wss://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/api-ws/v1/realtime?model=qwen3.5-omni-plus-realtime"

headers = [
    "Authorization: Bearer " + API_KEY
]

def on_open(ws):
    print(f"Connected to server: {API_URL}")
def on_message(ws, message):
    data = json.loads(message)
    print("Received event:", json.dumps(data, indent=2))

ws = websocket.WebSocketApp(
    API_URL,
    header=headers,
    on_open=on_open,
    on_message=on_message,
)
ws.run_forever()
```

## 附录 B：与 OpenAI Realtime 的关键差异速查

每条都能在上文找到出处。

| 维度 | OpenAI Realtime | DashScope Realtime |
|---|---|---|
| 端点 | `wss://api.openai.com/v1/realtime?model=...` | `wss://{WorkspaceId}.{region}.maas.aliyuncs.com/api-ws/v1/realtime?model=...` [R1] |
| 地域 | 单一全球端点 | 仅北京、新加坡两地域，Key 按地域隔离 [R1] |
| 鉴权 | `Authorization: Bearer`（+ 浏览器端临时 `client_secret`） | `Authorization: Bearer $DASHSCOPE_API_KEY`，**无临时凭据机制** [R1][R6] |
| 业务空间 | 无此概念（组织/项目用头） | host 前缀 **或** `X-DashScope-WorkSpace` 头 [R6][S1] |
| 事件命名基线 | GA 命名（`response.output_text.*` 等） | **beta 命名**（`response.text.*` 等），6 个事件名与 GA 不一致 [O1][O2] |
| 事件名重合度 | — | 35 个中 **32** 个与 OpenAI **beta** 逐字节相同；其余 3 个为 DashScope 独有 [R3][R4][O2] |
| DashScope 独有事件 | — | `input_image_buffer.append`、`session.finish`、`session.finished` [R1][R3][S1] |
| OpenAI 有而 DashScope 无 | `conversation.item.{delete,retrieve,truncate}`、`output_audio_buffer.*`、`rate_limits.updated`、`mcp_*` 等 | — [O1] |
| 图像输入 | 无独立图像缓冲区 | `input_image_buffer.append`，JPG/JPEG，单张 ≤256 KB，建议 1 fps [R3] |
| 音频格式 | **GA**：`audio/pcm` / `audio/pcmu` / `audio/pcma`；**beta**：`pcm16` / `g711_ulaw` / `g711_alaw` [O1][O2] | `pcm` / `wav`（新式）或 `pcm16`（legacy）；**无 g711/PCMU/PCMA 等电话编码** [R3][S1] |
| 采样率 | GA 的 `audio/pcm` 固定 24 kHz（类型注释：「Always `audio/pcm`」） | 入 16 kHz / 出 24 kHz 为基线；四档可配仅限 qwen3.5 系（文档矛盾，§5.1） |
| VAD 类型 | `server_vad` / `semantic_vad` | 同名两种，`semantic_vad` 仅 Qwen3.5 系 [R3] |
| VAD threshold | [0.0, 1.0] | **[-1.0, 1.0]** [R3] |
| tools 结构 | 扁平 `{type,name,description,parameters}` | **嵌套** `{type,function:{name,description,parameters}}` [R3] |
| 采样参数 | `temperature`、`max_response_output_tokens` | 另有 `top_p`/`top_k`/`repetition_penalty`/`presence_penalty`/`seed` [R3] |
| 联网搜索 | 无（Realtime 侧） | `enable_search` + `search_options` [R3] |
| 会话上限 | — | **单次会话最长 120 分钟**，超时服务端主动断开 [R1] |
| error 信封 | `{event_id,type:"error",error:{type,code,message,param}}` | **完全同构** [R4][O1] |
| 关闭码 | 仅客户端主动关闭的默认值 `1000`/`OK` 有据；**服务端关闭码同样未文档化** | **未文档化**（§7.3） |
