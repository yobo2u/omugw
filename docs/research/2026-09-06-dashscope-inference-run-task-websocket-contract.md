# DashScope `/api-ws/v1/inference` run-task WebSocket 契约调研

- **日期**：2026-09-06（所有官方页面均于当日抓取核对）
- **目的**：为 omugw 的 WebSocket 传输层与 `dashscope.inference` 协议设计提供线格式事实依据。
  本文件只记录官方第一手文档与官方 SDK 源码里写了什么、哪里没写。
- **范围**：`wss://{host}/api-ws/v1/inference` 上的 **run-task 指令流**（A 类），
  承载 Paraformer / Fun-ASR 实时 ASR 与 Qwen-Audio-TTS / CosyVoice 流式 TTS。
  **不含** `/api-ws/v1/realtime`（B 类，OpenAI Realtime 风格的 session/event 协议），
  两者是不同协议，边界见 §8。
- **合规**：仅使用阿里云第一手文档（help.aliyun.com 中国站、alibabacloud.com 国际站）
  与官方 Apache-2.0 SDK 源码（`dashscope/dashscope-sdk-python`，已登记在
  `docs/provenance.yaml` 的 `planned_upstreams`）。未读取任何
  `excluded_from_source_reading` 中的仓库。

## 来源清单

| 编号 | 文档 | URL |
|---|---|---|
| [W1] | Paraformer 实时语音识别 WebSocket API（接口地址 / 请求头 / 交互流程） | https://help.aliyun.com/zh/model-studio/websocket-for-paraformer-real-time-service |
| [W2] | Paraformer 客户端事件（run-task / finish-task） | https://help.aliyun.com/zh/model-studio/paraformer-client-events |
| [W3] | Paraformer 服务端事件（四类事件） | https://help.aliyun.com/zh/model-studio/paraformer-server-events |
| [W4] | Qwen-Audio-TTS/CosyVoice WebSocket API 参考 | https://help.aliyun.com/zh/model-studio/cosyvoice-websocket-api |
| [W5] | Qwen-Audio-TTS/CosyVoice 客户端事件（run-task / continue-task / finish-task） | https://help.aliyun.com/zh/model-studio/cosyvoice-client-events |
| [W6] | Qwen-Audio-TTS/CosyVoice 服务端事件（含 sentence-* 子事件） | https://help.aliyun.com/zh/model-studio/cosyvoice-server-events |
| [W7] | Fun-ASR-Realtime / Qwen-Audio-3.0-ASR-Flash-Streaming WebSocket API | https://help.aliyun.com/zh/model-studio/fun-asr-realtime-websocket-api |
| [W8] | Fun-ASR 客户端事件（run-task / continue-task / finish-task） | https://help.aliyun.com/zh/model-studio/fun-asr-client-events |
| [W9] | Paraformer WebSocket API（国际站英文，页面标注 Last Updated: Sep 02, 2026） | https://www.alibabacloud.com/help/en/model-studio/websocket-for-paraformer-real-time-service |
| [W10] | Qwen-ASR-Realtime WebSocket API（**对照组：另一套协议**） | https://help.aliyun.com/zh/model-studio/qwen-asr-realtime-interaction-process |
| [W11] | Qwen-ASR-Realtime 客户端事件（**对照组**） | https://help.aliyun.com/zh/model-studio/qwen-asr-realtime-client-events |
| [S1] | 错误码（WebSocket 协议层错误、23 秒超时、close 1007） | https://help.aliyun.com/zh/model-studio/error-code |
| [S2] | 实时语音识别用户指南（支持的模型与地域） | https://help.aliyun.com/zh/model-studio/real-time-speech-recognition-user-guide |
| [S3] | 实时语音合成用户指南（支持的模型与地域、原生 Go WebSocket 示例） | https://help.aliyun.com/zh/model-studio/realtime-tts-user-guide |
| [S4] | 语音合成模型列表（WebSocket vs HTTP 的模型划分） | https://help.aliyun.com/zh/model-studio/tts-model |
| [S5] | 语音识别 API 参考入口页（四个 ASR 家族的分栏） | https://help.aliyun.com/zh/model-studio/speech-recognition-api-reference/ |
| [C1] | 官方 Python SDK：WebSocket 协议常量 | https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/protocol/websocket.py |
| [C2] | 官方 Python SDK：WebSocket 传输实现 | https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/api_entities/websocket_request.py |
| [C3] | 官方 Python SDK：默认 WS base URL | https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/common/env.py#L24-L27 |
| [C4] | 官方 Python SDK：TTS v2 指令构造与 on_message 分派 | https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/audio/tts_v2/speech_synthesizer.py |

SDK 引用一律钉在提交 `39c16c758a46b6145ef2c1e13e076c15b869f281`（clone 时的 HEAD）。

## 1. 端点与地域

### 1.1 路径

路径**固定**为 `/api-ws/v1/inference`，且必须使用 `wss://`（三份 API 页面都以「重要」
强调「URL 必须使用 `wss://` 协议，且固定不变」）[W1][W4][W7]。与 HTTP 侧不同：
**模型不体现在路径里，也不体现在查询参数里**，而是写在 run-task 的
`payload.model`（§3.1）。这与 B 类 `/api-ws/v1/realtime?model=<model_name>` 正相反 [W10]。

### 1.2 地域域名

| 地域 | 业务空间专属域名（推荐） | 旧域名 | 出处 |
|---|---|---|---|
| 华北2（北京） | `wss://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/api-ws/v1/inference` | `dashscope.aliyuncs.com` | [W1][W4][W7] |
| 新加坡 | `wss://{WorkspaceId}.ap-southeast-1.maas.aliyuncs.com/api-ws/v1/inference` | `dashscope-intl.aliyuncs.com` | [W4][W7] |

要点：

- **Paraformer 仅在华北2（北京）可用**，其 WebSocket URL 只列了北京一个 [W1]；
  国际站英文页同样写明「This document applies only to the China (Beijing) region」，
  且要求使用北京地域的 API Key [W9]。TTS 与 Fun-ASR 两族则同时列出北京与新加坡 [W4][W7]。
  → 对网关的含义：**端点可用地域是按模型族收敛的**，不能按「协议支持两个地域」一刀切。
- 官方声明专属域名「能够为推理请求提供卓越的性能和更高的稳定性」，建议迁移；
  **现有域名仍可正常使用** [W1][W4][W7]。
- SDK 的默认值仍是旧域名，可用环境变量 `DASHSCOPE_WEBSOCKET_BASE_URL` 覆写 [C3]：
  ```python
  base_websocket_api_url = os.environ.get(
      "DASHSCOPE_WEBSOCKET_BASE_URL",
      f"wss://dashscope.aliyuncs.com/api-ws/{api_version}/inference",
  )
  ```
  官方 Go 与 Python 示例则直接把 base 写成 workspace 专属域名 [S3]。
- **API Key 按地域隔离**：模型/地域表明确「调用以下模型时，请选择<地域>的 API Key」
  [S2][S3]。同一把 Key 不能同时用于北京与新加坡端点。

## 2. 鉴权与握手

请求头在 **WebSocket 握手（HTTP Upgrade）阶段**发送，三份页面给出同一张表 [W1][W4][W7]：

| 头 | 类型 | 必选 | 说明 |
|---|---|---|---|
| `Authorization` | string | **是** | `Bearer <your_api_key>` |
| `user-agent` | string | 否 | 客户端标识，便于服务端追踪来源 |
| `X-DashScope-WorkSpace` | string | 否 | 业务空间 ID |
| `X-DashScope-DataInspection` | string | 否 | 数据合规检测，默认不传或设 `enable`；「如非必要，请勿启用」 |

**鉴权失败发生在握手阶段，不是任务阶段**：三份页面均以「重要」标注
「Authorization 鉴权在 WebSocket 握手阶段验证。如果 API Key 无效或缺失，
握手将失败并返回 HTTP 401/403 错误」[W1][W4][W7]；英文页同义 [W9]。

SDK 侧一致：`Authorization: Bearer {api_key}` 与其余头一并传给 `ws_connect`，
并把握手异常 `WSServerHandshakeError` 的 401/403 映射为
`"Unauthorized, your api-key is invalid!"`、503 映射为服务不可用 [C2]。
SDK 还固定设置了 `heartbeat=30`（aiohttp 层的 WebSocket ping 间隔），
这是**客户端实现选择**，官方协议文档未规定 [C2]。

> 对网关的含义：这是与 SSE/HTTP 路径的关键差异——**鉴权错误在字节流开始之前
> 以 HTTP 状态码返回**，此时尚无 task_id、也没有 task-failed 事件可读。
> 凭据 failover 的可行窗口正好是「握手失败」这一段。

## 3. 客户端指令信封（run-task / continue-task / finish-task）

### 3.1 顶层结构

三条指令共享同一信封：顶层恒为 `header` + `payload` 两个对象 [W2][W5][W8]。
SDK 的构造函数把这一点写死 [C2]：

```python
def _build_up_message(self, headers, payload):
    message = {"header": headers, "payload": payload}
    return json.dumps(message, ensure_ascii=False)
```

`header` 的三个字段在**每一条**客户端指令上都必选 [W2][W5][W8]：

| 字段 | 类型 | 取值 |
|---|---|---|
| `action` | string | `run-task` / `continue-task` / `finish-task` |
| `task_id` | string | 客户端生成的 UUID |
| `streaming` | string | 固定 `duplex` |

SDK 常量确认了这套词表且**没有第四个 action** [C1]：

```python
class ActionType:
    START = "run-task"
    CONTINUE = "continue-task"
    FINISHED = "finish-task"
```

`streaming` 的合法枚举在 SDK 里是 `none` / `in` / `out` / `duplex`，
但 run-task 指令流的三份文档一律要求 `duplex` [W2][W5][W8][C1]。

### 3.2 `header.action` 与 `header.task_id` 的语义

- **`task_id` 由客户端生成，服务端原样回带**：服务端事件的 `header.task_id` 被文档
  逐条描述为「**客户端生成的**任务 ID」[W3][W6]。即它是关联键，不是服务端签发的句柄。
- **一个任务的三条指令必须用同一个 task_id**。TTS 页面以「重要」写明：
  「同一次合成任务中，run-task、所有 continue-task、finish-task 必须使用相同的 `task_id`。
  每次发起新任务时生成新的 task_id（如使用 UUID）。**使用不同 task\_id 会导致音频错乱或任务失败**」[W4]。
- **连接可复用、可跑多个任务**：「为提高资源利用率，建议复用 WebSocket 连接处理多个任务，
  而非为每个任务建立新连接」[W4]；`task-finished` 后「可以关闭 WebSocket 连接或复用连接开启新任务」[W6]。
  → 因此 **task_id 是连接内的任务分界**，一条连接上的事件必须按 task_id 归属，
  不能假设「一连接一任务」。
- SDK 默认用 `uuid.uuid4().hex`（32 位无短横 hex），文档示例则是标准带短横 UUID
  形如 `2bf83b9a-baeb-4fda-8d9a-xxxxxxxxxxxx` [C2][W2]。两种写法都被服务端接受，
  说明**服务端未强制标准 UUID 文本格式**；SDK 另有 `pre_task_id` 参数允许调用方指定 [C2]。

### 3.3 run-task 的 payload

`payload` 的五个字段在 ASR 与 TTS 上都必选，差异只在取值 [W2][W5][W8]：

| 字段 | ASR（Paraformer / Fun-ASR） | TTS（Qwen-Audio-TTS / CosyVoice） |
|---|---|---|
| `task_group` | `audio` | `audio` |
| `task` | `asr` | `tts` |
| `function` | `recognition` | `SpeechSynthesizer` |
| `model` | 模型名（§7） | 模型名（§7） |
| `input` | 固定 `{}`（Fun-ASR 可带 `context`） | 固定 `{}`，文本走 continue-task |
| `parameters` | 识别参数 | 合成参数 |

注意 `function` 的大小写不对称：ASR 是全小写 `recognition`，TTS 是驼峰
`SpeechSynthesizer` [W2][W5][W8]。SDK 源码同样如此
（`function="recognition"` 与 `function="SpeechSynthesizer"`）[C4]。

**`input` 不可省略**，即便是空对象。错误码页两条独立条目印证：
`Invalid payload data`「检查发送 run-task 指令时，payload 中是否有 `"input": {}`，若无，请添加」；
`InvalidParameter: task can not be null`「run-task 指令或 finish-task 指令的 payload 中缺少 input 字段」[S1]。

ASR run-task 官方样例 [W2]：

```json
{
    "header": {
        "action": "run-task",
        "task_id": "2bf83b9a-baeb-4fda-8d9a-xxxxxxxxxxxx",
        "streaming": "duplex"
    },
    "payload": {
        "task_group": "audio",
        "task": "asr",
        "function": "recognition",
        "model": "paraformer-realtime-v2",
        "parameters": {
            "format": "pcm",
            "sample_rate": 16000,
            "disfluency_removal_enabled": false,
            "language_hints": ["en"]
        },
        "input": {}
    }
}
```

TTS run-task 官方样例 [W5]：

```json
{
    "header": {
        "action": "run-task",
        "task_id": "2bf83b9a-baeb-4fda-8d9a-xxxxxxxxxxxx",
        "streaming": "duplex"
    },
    "payload": {
        "task_group": "audio",
        "task": "tts",
        "function": "SpeechSynthesizer",
        "model": "qwen-audio-3.0-tts-flash",
        "parameters": {
            "text_type": "PlainText",
            "voice": "longanlingxi",
            "format": "mp3",
            "sample_rate": 22050,
            "volume": 50,
            "rate": 1.0,
            "pitch": 1.0,
            "enable_ssml": false
        },
        "input": {}
    }
}
```

关键参数（完整表见原文，此处只记影响传输层的）：

- ASR：`format`（pcm/wav/mp3/opus/speex/aac/amr，opus/speex 须 Ogg 封装、wav 须 PCM 编码、
  amr 仅 AMR-NB）与 `sample_rate` **必选**；`heartbeat`（默认 false）决定持续送静音时
  连接是否保活 [W2][W8]。
- TTS：`text_type` 固定 `PlainText`、`voice` **必选**；`format` 默认 mp3；
  `enable_ssml=true` 时**只允许发送一次 continue-task** [W5]，违反报
  `Text request limit violated, expected 1.` [S1]。

### 3.4 continue-task 的两种语义（同一 action，不同含义）

这是本协议最容易误建模的一点：**`continue-task` 在 TTS 与 ASR 上语义不同**。

- **TTS：continue-task 是数据通道**，承载待合成文本 [W5]：
  ```json
  {
      "header": {"action": "continue-task", "task_id": "...", "streaming": "duplex"},
      "payload": {"input": {"text": "床前明月光，疑是地上霜"}}
  }
  ```
  限额：单次最多 20000 字符、累计最多 200000 字符、**发送间隔不得超过 23 秒**否则连接超时 [W5]。
  服务端对文本片段自动分句：完整语句立即合成，不完整语句缓存至完整后合成 [W4]。
- **ASR：continue-task 是可选的上下文更新**，不承载音频（音频走二进制帧，§4）[W8]。
  `payload.input.context` 携带对话上下文以提升识别准确率，且**仅
  `qwen-audio-3.0-asr-flash-streaming`、`fun-asr-realtime`、`fun-asr-realtime-2025-11-07` 支持** [W8]。
  Paraformer 的客户端事件页**只文档化了 run-task 与 finish-task 两条指令**，没有 continue-task [W2]。

> 对网关的含义：`continue-task` 不能被建模成一个统一的「继续」语义。
> 在 TTS 它是必经的输入通道，在 ASR 它是可选的、按模型收敛的旁路增强。

### 3.5 finish-task 与取消

基本形态在两族一致：`payload.input` 固定 `{}` [W2][W5][W8]。

TTS 额外定义了 **取消指令**：`payload.input.directive = "cancel"`，
「表示取消当前轮次的语音合成任务，服务端会立即返回 `task-finished` 事件，
且不会输出后续音频」；取消后可在同一连接上发新的 run-task，无需重建连接 [W5]。
支持面按地域收敛：北京地域 Qwen-Audio-TTS 全系支持、CosyVoice 仅 v2 及以上；
**新加坡地域 CosyVoice 系列不支持** [W5]。SDK 的 `get_finish_request(directive)`
与之对应 [C4]。

`finish-task` **不可省略**：TTS 交互流程写明「此步骤不可省略，否则可能导致语音数据不完整」[W4]。
ASR 侧发出 finish-task 后仍会继续收到 result-generated，直到 task-finished [W1][W7]。
错误码页另外禁止自造终止帧：「请勿发送自创内容（如 `{"input": {"end_of_stream": true}}`）」[S1]。

SDK 里还有一条**文档未记载**的用法：`get_flush_request()` 用
`action=continue-task` + `payload.input.flush=true` 触发强制合成 [C4]。
三份客户端事件页都没有 `flush` 字段，标记为未文档化（§9）。

## 4. 二进制音频帧与 JSON 控制帧如何交织

这是传输层设计的核心问题，官方在两个方向上给了不同答案。

### 4.1 方向与载荷

| 方向 | 载荷 | 帧类型 |
|---|---|---|
| 客户端 → 服务端（ASR） | 音频流 | **二进制帧**，须为单声道 [W1][W7] |
| 客户端 → 服务端（TTS） | 待合成文本 | **文本帧**（continue-task JSON）[W5] |
| 服务端 → 客户端（ASR） | 识别结果 | **文本帧**（result-generated JSON）[W3] |
| 服务端 → 客户端（TTS） | 音频流 | **二进制帧**，「通过 `binary` 通道接收音频流」[W4] |

即：**run-task 指令流是 A 类协议，音频永远走 WebSocket 原生 binary 帧，
不做 base64 内嵌**。这与 B 类 `/api-ws/v1/realtime` 相反——后者的
`input_audio_buffer.append` 把音频作为 **Base64 字符串**塞进 JSON 的 `audio` 字段 [W11]。

### 4.2 交织规则（顺序契约）

1. **握手后必须先 run-task，且必须等到 task-started 才能发数据。**
   ASR：「客户端发送 run-task 指令以开启任务，并接收服务端返回的 task-started 事件……
   可以进行后续步骤」，run-task 条目的「响应事件」写明「服务端返回 `task-started` 事件后
   **才能发送音频**」[W1][W2][W7]。TTS 同构：「服务端返回 task-started 事件后才能发送后续指令」[W5]。
   SDK 用一个独立的 `_wait_for_task_started` 阶段强制这个顺序，并且
   **在该阶段收到二进制帧会直接抛异常**（`"Receive unexpected binary message when wait for task-started"`）[C2]。
2. **run-task 之后，二进制帧与 JSON 事件在同一条连接上并发双向流动。**
   ASR 是「客户端开始发送二进制音频……并**同时**接收服务端持续返回的 result-generated 事件」[W1][W7]。
   SDK 在 duplex 模式下显式起了一个后台发送任务，与接收循环并行 [C2]：
   ```python
   else:  # duplex mode
       bg_task = asyncio.create_task(self._send_continue_task_data(ws))
       async for is_binary, message in self._receive_streaming_data_task(ws):
           yield self._to_DashScopeAPIResponse(task_id, is_binary, message)
       await bg_task
   ```
   → **没有请求-响应配对，收发是独立的两个流**。传输层必须按帧类型而非按轮次解复用。
3. **TTS 侧存在一条显式的「JSON 先导 → 二进制随后」配对**。
   `result-generated` 的子事件 `sentence-synthesis` 被定义为
   「标识音频数据块，**每个事件后立即通过 WebSocket binary 通道传输一个音频数据帧**」[W6]。
   这是文档里唯一一处把某个 JSON 事件与某个二进制帧绑定的说明；
   ASR 侧没有对应约束（音频是客户端发的，服务端只回 JSON）。
4. **接收端按帧类型分派，而不是按事件名。** SDK 的接收循环对
   `WSMsgType.TEXT` 走事件分派、对 `WSMsgType.BINARY` 直接产出音频字节 [C2]；
   TTS 的 `on_message` 用 `isinstance(message, str)` / `isinstance(message, (bytes, bytearray))`
   做同样的二分 [C4]。
5. **未知的文本事件是硬错误。** SDK 在接收循环里对不属于四类事件的 JSON
   抛 `UnknownMessageReceived` [C2]——即客户端**不被期望**静默忽略未知事件。

### 4.3 分片与背压

官方对音频分片给了一条明确建议（出现在错误码页的
`The decoded text message was too big for the output buffer...` 条目下）：
「请分段发送待识别音频，**建议每次发送的音频时长约为 100 毫秒，数据大小保持在 1KB 至 16KB 之间**」[S1]。
SDK 在每次发送之间插入 `await asyncio.sleep(0.000001)` 让出事件循环 [C2]。

## 5. 服务端事件分类学

四类事件，SDK 常量与文档完全一致 [C1][W3][W6]：

```python
class EventType:
    STARTED = "task-started"
    GENERATED = "result-generated"
    FINISHED = "task-finished"
    FAILED = "task-failed"
```

服务端事件的 `header` 恒有 `task_id` 与 `event`，通常还有 `attributes` [W3][W6]。

### 5.1 task-started

`payload` 为空对象；`header.attributes` 通常为空 [W3][W6]。

```json
{"header": {"task_id": "...", "event": "task-started", "attributes": {}}, "payload": {}}
```

### 5.2 result-generated（两族形状完全不同）

**ASR 形状** [W3]：`payload.output.sentence` + `payload.usage`。

- `sentence.begin_time` / `end_time`（ms，`end_time` 在句未结束时为 `null`）
- `sentence.text`：识别文本
- `sentence.sentence_end`：`true` = 最终结果，`false` = 中间结果
- `sentence.heartbeat`：`true` 时该结果可跳过（心跳包）
- `sentence.words[]`：`{begin_time, end_time, text, punctuation}` 字级时间戳
- `sentence.emo_tag` / `emo_confidence`：仅 `paraformer-realtime-8k-v2`，
  且须关闭语义断句，且仅在 `sentence_end=true` 时出现
- **`payload.usage` 是条件字段**：`sentence_end=false` 时 `usage` 为 `null`；
  `sentence_end=true` 时 `usage.duration` 为该任务计费时长（秒）

**TTS 形状** [W6]：`payload.output.type` 是三值子事件枚举。

| `output.type` | 含义 |
|---|---|
| `sentence-begin` | 句子开始，返回待合成文本 |
| `sentence-synthesis` | 标识音频数据块，其后紧跟一个 binary 音频帧 |
| `sentence-end` | 句子结束，返回文本与累计字符数 |

伴随字段：`output.sentence.index`（句子编号，从 0 开始）、
`output.sentence.words[]`（`text` / `begin_index` / `end_index` / `begin_time` / `end_time`）、
`output.original_text`（分句后的句子文本）、
`payload.usage.characters`（**在 sentence-end 事件中返回**的累计计费字符数）[W6]。

> 对网关的含义：`result-generated` 是一个**同名异构**的事件——ASR 用
> `output.sentence.sentence_end` 布尔量表达终局性，TTS 用 `output.type` 字符串枚举表达。
> 用一个结构体解两族会立刻失真。

### 5.3 task-finished

正常结束，**连接可复用**：「客户端可以关闭 WebSocket 连接或复用连接开启新任务」[W6]，
ASR 侧同义 [W3]。

两族的 payload 不同：ASR 示例是 `{"output": {}, "usage": null}` 且文档说
「无需关注其中内容，通常为 `{}`」[W3]；TTS 则在此携带
`payload.usage.characters` 累计计费字符数 [W6]。
TTS 的 `header.attributes` 在示例里出现了 `request_uuid` 字段 [W6] ——
这是文档中唯一一处出现服务端侧请求标识，且未在字段表里定义（§9）。

### 5.4 task-failed

错误信息在 **header** 上，不在 payload 里 [W3][W6]：

| 字段 | 说明 |
|---|---|
| `header.event` | 固定 `task-failed` |
| `header.task_id` | 客户端生成的任务 ID |
| `header.error_code` | 错误类型描述 / 错误码 |
| `header.error_message` | 具体错误原因 |
| `payload` | 固定 `{}` |

两族给的示例错误码取值风格不同：ASR 用 `CLIENT_ERROR`
（`"error_message": "request timeout after 23 seconds."`）[W3]，
TTS 用 `InvalidParameter`（`"error_message": "[tts:]Engine return error code: 418"`）[W6]。
即 **`error_code` 不是单一命名空间**，既有 SCREAMING_SNAKE 也有 PascalCase。

SDK 的解析与之一致，固定从 header 三个键取值 [C1][C2]：

```python
ERROR_NAME = "error_code"
ERROR_MESSAGE = "error_message"
...
def _on_failed(self, details):
    error = RequestFailure(
        request_id=details[HEADER][TASK_ID],
        http_code=WEBSOCKET_ERROR_CODE,   # 常量值 44，非 HTTP 状态码
        name=details[HEADER][ERROR_NAME],
        message=details[HEADER][ERROR_MESSAGE],
    )
```

注意 `WEBSOCKET_ERROR_CODE = 44` 是 SDK 内部哨兵值，**不是** HTTP 状态码，
不要透传到网关的 HTTP 错误信封里 [C2]。

## 6. 错误处理与关闭语义

### 6.1 三个截然不同的失败阶段

| 阶段 | 失败形态 | 是否有 task_id | 出处 |
|---|---|---|---|
| 握手 | HTTP 401/403（鉴权）、503 | 无 | [W1][W4][W7][C2] |
| 任务中 | `task-failed` 事件（header 带 error_code/error_message） | 有 | [W3][W6] |
| 传输层 | WebSocket close 帧 / 连接异常断开 | 视时机 | [S1][C2] |

### 6.2 task-failed 后连接不可复用

Paraformer 服务端事件页写明：「任务失败，**连接会被关闭，无法复用**」[W3]；
TTS 页写「客户端需要关闭 WebSocket 连接并处理错误」[W6]。
这与 task-finished 的「可复用」形成明确对照（§5.3）。

### 6.3 空闲超时：23 秒

错误码页把这条独立成目：`request timeout after 23 seconds.`
「原因：**超过 23 秒未向服务发送数据**。该报错信息在使用实时语音合成（Sambert）、
语音识别（Paraformer）和实时语音合成（Qwen-Audio-TTS/CosyVoice）时产生」[S1]。
TTS 的 continue-task 条目独立复述了同一约束（「发送间隔不得超过 23 秒，否则连接超时」）[W5]。
该错误以 §5.4 的 task-failed 形态下发（ASR 示例的 error_message 正是这句）[W3]。

规避手段是 ASR 的 `heartbeat: true` 参数：「在持续发送静音音频的情况下，
可保持与服务端的连接不中断」[W2][W8]；错误码页在 `ResponseTimeout` 条目下
也给出「将请求参数 `heartbeat` 设为 true 或及时结束识别任务」[S1]。
注意心跳结果会以 `sentence.heartbeat=true` 的 result-generated 出现，可跳过 [W3]。

> 注意区分两个 heartbeat：`parameters.heartbeat` 是**协议层**的业务心跳（[W2][W8]），
> SDK 的 `ws_connect(heartbeat=30)` 是**传输层**的 WebSocket ping（[C2]），两者无关。

### 6.4 WebSocket close code

错误码页文档化了一个 close code：
「**close code 1007** Model not found ——通过 WebSocket 协议调用实时模型时，
请求中的模型名称错误，或该模型服务尚未开通。**服务端以 close code 1007 关闭连接，
reason 为 `Model not found`**」[S1]。该条目的举例是 Qwen3-Omni-Flash-Realtime（B 类端点），
但它说的是「通过 WebSocket 协议调用实时模型」的通用行为。
**A 类端点上模型名错误是否也走 1007（而非 task-failed）没有被明确说明**，标记为存疑（§9）。

其余 close code 官方未列举。SDK 把任何非预期的 CLOSED / ERROR 帧
统一抛成 `UnexpectedMessageReceived`，不区分 code [C2]。

### 6.5 协议层错误（直连 WebSocket 才会遇到）

错误码页有一组只在**直接使用 WebSocket 协议**时出现的错误 [S1]：

| 错误 | 触发条件 |
|---|---|
| `Missing required parameter 'xxx'! Please follow the protocol!`（如 `payload.model`、`payload.task_group`） | JSON 结构层级错误、参数名拼写错误（如 `task_group` 写成 `taskgroup`）、值为空 |
| `Invalid payload data` | run-task 缺 `"input": {}`；或发送自造终止帧如 `{"input":{"end_of_stream":true}}` |
| `InvalidParameter: task can not be null` | run-task/finish-task 缺 `input` 字段，或 continue-task 缺 `input.text` |
| `Text request limit violated, expected 1.` | `enable_ssml=true` 后多次发送 continue-task |
| `The decoded text message was too big for the output buffer...` | 单次发送的音频块过大（建议 1KB–16KB / 100ms） |
| `Request voice is invalid!` / `[tts:]Engine return error code: 418` | TTS 未设 voice，或 model 与 voice 版本不匹配（v1/v2 不可混用） |

页面明确点出这是「服务端返回的**协议层错误，仅在直接使用 WebSocket 协议时出现**」，
而 SDK 会在客户端提前拦截同类误用 [S1]。**omugw 直连 WebSocket，因此会命中这一整类错误。**

## 7. 哪些模型走这套协议

### 7.1 ASR（`task=asr`、`function=recognition`）

来自「支持的模型与地域」[S2]：

| 家族 | 北京 | 新加坡 |
|---|---|---|
| Qwen-Audio-3.0-ASR-Flash-Streaming | `qwen-audio-3.0-asr-flash-streaming` | 同左 |
| Fun-ASR-Realtime | `fun-asr-realtime`(=`fun-asr-realtime-2025-11-07`)、`fun-asr-realtime-2026-02-28`、`fun-asr-realtime-2025-11-07`、`fun-asr-realtime-2025-09-15`、`fun-asr-flash-8k-realtime`(=`...-2026-01-28`)、`fun-asr-flash-8k-realtime-2026-01-28` | `fun-asr-realtime`、`fun-asr-realtime-2025-11-07` |
| Paraformer | `paraformer-realtime-v2`、`paraformer-realtime-v1`、`paraformer-realtime-8k-v2`、`paraformer-realtime-8k-v1` | **不可用** |
| Qwen3-ASR-Flash-Realtime | `qwen3-asr-flash-realtime` 等 | 同族快照 |

⚠️ **Qwen3-ASR-Flash-Realtime 与 Qwen-ASR-Realtime 出现在同一张模型表里，但走的是
B 类 `/api-ws/v1/realtime` 协议，不是 run-task**（§8）。模型表按「实时语音识别」这一
业务能力聚合，**不按线格式聚合**——不能拿这张表直接推断协议归属。

采样率约束按模型收敛：`paraformer-realtime-v2` 支持任意采样率、
`paraformer-realtime-v1` 仅 16000 Hz、两个 8k 型号仅 8000 Hz [W2]；
Fun-ASR 侧「8k 模型仅支持 8000 Hz，其他模型支持任意采样率」[W8]。

### 7.2 TTS（`task=tts`、`function=SpeechSynthesizer`）

来自「支持的模型与地域」[S3] 与模型列表 [S4]：

- **Qwen-Audio-TTS**：`qwen-audio-3.0-tts-plus`、`qwen-audio-3.0-tts-flash`
- **CosyVoice**：`cosyvoice-v3.5-plus`、`cosyvoice-v3.5-flash`（**仅北京，且仅声音复刻/设计，无系统音色**）、
  `cosyvoice-v3-plus`、`cosyvoice-v3-flash`、`cosyvoice-v2`、`cosyvoice-v1`

模型列表页给了一条判定规则：「Qwen-Audio-TTS/CosyVoice 系列模型**使用同一模型名称
同时支持 WebSocket 和 HTTP 两种接入方式**；Qwen 系列模型通过模型名称区分，
带 `-realtime` 后缀的为 WebSocket 接入，不带后缀的为 HTTP 接入」[S4]。

⚠️ 但「带 `-realtime` 后缀 = WebSocket」**不等于**「= run-task 协议」：
`qwen-tts-realtime`、`qwen3-tts-flash-realtime` 等属于 Qwen 系列 realtime 模型，
其接入面是 B 类端点，不是本文的 run-task 指令流（§8）。

### 7.3 官方 SDK 里走 WebSocket 协议的模块

`ApiProtocol.WEBSOCKET` 在 SDK 中只出现在三个业务模块 [C2 同仓库]：
`audio/asr/recognition.py`、`audio/asr/translation_recognizer.py`、
`audio/tts/speech_synthesizer.py`（另有 `tts_v2` 自建 WebSocket 客户端 [C4]）。
其中 `translation_recognizer`（Gummy 实时翻译）同样用
`task_group="audio"` / `function="recognition"` / `WebsocketStreamingMode.DUPLEX`，
即**它也是 run-task 家族成员**——但本次未能定位到其现行中文 API 参考页（旧链接已 404），
故不列入 §7.1 的模型清单，标记为待确认（§9）。

## 8. 协议边界：`/api-ws/v1/inference` ≠ `/api-ws/v1/realtime`

百炼的 WebSocket 面**有两套互不兼容的协议**，这是本次调研最重要的结构性结论：

| 维度 | A 类：`/api-ws/v1/inference` | B 类：`/api-ws/v1/realtime` |
|---|---|---|
| 模型指定位置 | `payload.model`（body） | URL 查询参数 `?model=<model_name>` [W10] |
| 信封 | `header{action,task_id,streaming} + payload` [W2] | `{type, event_id, ...}` 扁平 [W11] |
| 指令词表 | `run-task` / `continue-task` / `finish-task` [C1] | `session.update` / `input_audio_buffer.append` / `input_audio_buffer.commit` / `session.finish` [W11] |
| 音频上行 | **WebSocket binary 帧** [W1][W7] | **Base64 字符串**塞进 JSON `audio` 字段 [W11] |
| 事件词表 | `task-started` / `result-generated` / `task-finished` / `task-failed` [C1] | `input_audio_buffer.speech_started` / `.committed` / `conversation.item.input_audio_transcription.text` / `.completed` / `session.finished` [W10] |
| 关联键 | `task_id`（客户端生成） | `event_id` / session 隐式 |
| 代表模型 | Paraformer、Fun-ASR-Realtime、Qwen-Audio-TTS、CosyVoice | Qwen-ASR-Realtime、Qwen3-ASR-Flash-Realtime、Qwen-TTS-Realtime、Qwen3-Omni-Flash-Realtime |

两套协议的请求头表**完全相同**（Authorization / user-agent / X-DashScope-WorkSpace /
X-DashScope-DataInspection，且都在握手阶段验证）[W1][W10]，因此**不能靠请求头区分**，
只能靠路径 + 模型名。这正对应 README 里把 `dashscope.realtime` 与 `dashscope.inference`
拆成两个协议的判断——本次调研为该拆分提供了一手依据。

B 类还有一条 A 类没有的坑：VAD 模式下「如果客户端直接关闭 WebSocket 连接而未发送
`session.finish`，服务端将丢弃当前 in_progress item」[W10]。A 类的对应约束是
finish-task 不可省略 [W4]。两者形似而机制不同。

## 9. 未文档化 / 存疑清单

设计时必须为以下各点保留显式决策，不能默认它们有确定行为：

1. **A 类端点上「模型名错误」的失败形态**：是 close code 1007 还是 task-failed？
   [S1] 的 1007 条目举的是 B 类实时模型，未言明 A 类（§6.4）。
2. **完整的 WebSocket close code 表**：官方只文档化了 1007/`Model not found` 一个；
   正常结束、超时、限流各用什么 close code 未说明（§6.4）。
3. **`error_code` 的完整取值域**：文档只给了 `CLIENT_ERROR`(ASR 示例) 与
   `InvalidParameter`(TTS 示例) 两个样本，命名风格还不一致；
   没有一张 WebSocket 专属的 error_code 清单（§5.4）。
4. **`task-failed` 是否总伴随服务端主动 close**：文档说「连接会被关闭，无法复用」[W3]，
   但没说是服务端立即发 close 帧还是等客户端关。SDK 只是抛异常并退出上下文 [C2]。
5. **SDK 的 `flush` 指令**（`continue-task` + `payload.input.flush=true`）[C4]
   在三份客户端事件文档中均无记载，服务端语义未知（§3.5）。
6. **`header.attributes` 的字段定义**：文档一律描述为「附加属性（通常为空）」[W3]，
   但 TTS 的 task-finished 示例里出现了 `attributes.request_uuid` [W6]，
   该字段未在任何字段表中定义；是否稳定存在、能否用作服务端请求追踪 ID 未知（§5.3）。
7. **task_id 的格式约束**：SDK 用 32 位 hex、文档示例用带短横 UUID [C2][W2]，
   服务端是否校验格式、是否有长度上限未说明（§3.2）。
8. **一条连接上并发多个 task_id 是否合法**：文档只说「复用连接处理多个任务」[W4]，
   语气是串行复用（task-finished 后再开新任务）；**并发**多任务未被承诺（§3.2）。
9. **Gummy（translation_recognizer）的现行 API 参考页**：SDK 证明它属于 run-task 家族
   [§7.3]，但中文文档入口未定位到（旧 `gummy-*` 链接 404，ASR 入口页 [S5] 也未列该家族），
   其 `task` 取值与事件形状未经文档核对（§7.3）。
10. **二进制帧的大小上限**：只有「建议 1KB–16KB」的建议值 [S1]，没有硬上限；
    超限的确切错误只在错误码页以一句英文错误串出现（§4.3）。
11. **`streaming` 取值 `none`/`in`/`out` 在 A 类端点上的行为**：SDK 枚举里存在 [C1]，
    但三份文档都只要求 `duplex`；非 duplex 值是否被 `/api-ws/v1/inference` 接受未文档化。
12. **两族之外的 task_group**：SDK 的所有 WebSocket 调用点都用 `task_group="audio"` [§7.3]，
    是否存在非 audio 的 run-task 任务组（如视频/图像流）未见文档。

## 附录 A：一个最小 ASR 会话的帧序列（按官方交互流程与事件页拼接）

```
[client] HTTP Upgrade  + Authorization: Bearer sk-***            [W1]
[client] TEXT   {"header":{"action":"run-task","task_id":"<uuid>","streaming":"duplex"},
                 "payload":{"task_group":"audio","task":"asr","function":"recognition",
                            "model":"paraformer-realtime-v2",
                            "parameters":{"format":"pcm","sample_rate":16000},
                            "input":{}}}                          [W2]
[server] TEXT   {"header":{"task_id":"<uuid>","event":"task-started","attributes":{}},
                 "payload":{}}                                    [W3]
[client] BINARY <pcm chunk ~100ms / 1–16KB>            ─┐         [W1][S1]
[server] TEXT   result-generated (sentence_end=false)  ─┼─ 并发    [W3]
[client] BINARY <pcm chunk>                            ─┘
[server] TEXT   result-generated (sentence_end=true, usage.duration=3)   [W3]
[client] TEXT   {"header":{"action":"finish-task","task_id":"<uuid>","streaming":"duplex"},
                 "payload":{"input":{}}}                          [W2]
[server] TEXT   result-generated ...（finish 后仍可能继续到达）      [W1]
[server] TEXT   {"header":{"task_id":"<uuid>","event":"task-finished",...}}   [W3]
[client] CLOSE                                                    [W1]
```

## 附录 B：一个最小 TTS 会话的帧序列

```
[client] HTTP Upgrade + Authorization                             [W4]
[client] TEXT   run-task (task=tts, function=SpeechSynthesizer, voice=..., input={})  [W5]
[server] TEXT   task-started                                      [W6]
[client] TEXT   continue-task {"payload":{"input":{"text":"床前明月光，"}}}   [W5]
[server] TEXT   result-generated {"output":{"type":"sentence-begin",...}}     [W6]
[server] TEXT   result-generated {"output":{"type":"sentence-synthesis",...}}
[server] BINARY <audio frame>          ← 紧跟在每个 sentence-synthesis 之后   [W6]
[server] TEXT   result-generated {"output":{"type":"sentence-end",...},
                                  "usage":{"characters":6}}                   [W6]
[client] TEXT   finish-task {"payload":{"input":{}}}   （或 input.directive="cancel"）[W5]
[server] BINARY <剩余音频>              ← finish 后仍会继续输出                 [W4]
[server] TEXT   task-finished {"payload":{"usage":{"characters":13}}}         [W6]
[client] CLOSE  （或复用连接开新 task_id）                                     [W4][W6]
```

## 附录 C：对 omugw 传输层设计的直接影响（事实在上，推论在此）

1. **鉴权失败早于任何任务状态**：握手 401/403 时没有 task_id、没有事件可回放 [W1][W4][W7]。
   这正是凭据 failover 唯一安全的窗口——与「首字节后不重试」原则天然吻合：
   握手失败时下游尚未收到任何字节。
2. **`task-started` 是本协议的首字节等价物**：它之前失败可换凭据/上游重来；
   它之后（尤其已回吐 result-generated 或音频帧）必须按不可重试处理。
   SDK 把 `_wait_for_task_started` 单独成段，正是同一条边界 [C2]。
3. **task_id 必须由网关侧决定归属**：客户端生成、服务端回带、且一条连接可跑多个任务
   [W3][W4][W6]。若网关做连接池复用，task_id 是唯一的解复用键。
4. **`result-generated` 需要按 task 类型分两种解码器**：ASR 的 `sentence_end` 布尔
   与 TTS 的 `output.type` 三值枚举不可合并（§5.2）。
5. **usage 是条件出现的，且两族口径不同**：ASR 为 `usage.duration`（秒，仅句末）、
   TTS 为 `usage.characters`（累计字符，sentence-end 与 task-finished）[W3][W6]。
   流式中断时无法拿到完整 usage → 对应本仓库 `FidelityUnavailable` 的既有约定。
6. **23 秒空闲超时是硬约束**，网关若在中间做缓冲/整形，必须保证转发间隔不被拉长，
   或代为设置 ASR 的 `parameters.heartbeat` [W5][S1][W2]。
7. **协议层错误只有直连才会遇到**（§6.5）：官方 SDK 会客户端拦截，omugw 不会，
   因此这批 `Missing required parameter` / `Invalid payload data` 必须由网关自己在
   编码阶段杜绝，而不是指望上游给出友好提示。
