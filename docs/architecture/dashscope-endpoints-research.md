# DashScope Native 候选端点调研

**日期**：2026-08-14
**目的**：厘清 DashScope Native 协议族各候选端点的官方契约与实现复杂度，
为 `dashscope.native -> dashscope.native` 路径选择下一个投放的端点。
**状态**：结论已采纳。下一个投放目标为多模态生成端点，理由见「结论」。

## 对初稿的更正

初稿（英文）有三处契约理解错误，本版本按官方文档更正：

1. **引用源用错**。初稿把 Kimi API 的文档页当作多模态生成端点的引用。Kimi 是
   百炼上的第三方直供模型，它的文档页不构成 DashScope Native 的多模态契约。
   本版本改引《DashScope API 参考》与 Qwen-Omni 文档。
2. **内容块描述不全**。初稿写 messages content 为「text、image、video」。官方
   契约是 `text` / `image` / `audio` / `video` 四种块，**没有通用 file 块**。
3. **混淆同 URL 的不同契约**。`/api/v1/services/aigc/multimodal-generation/generation`
   同时被多个互不相关的产品 API 使用（多模态交互套件的 `multimodal-dialog`、
   语音识别模型、图像生成模型）。「多模态生成门」只指其中的 Qwen 模型 API 契约，
   见「同一 URL，多份契约」。

## 候选端点复杂度排名

排名仅按**原始 API 复杂度**从低到高，不代表投放顺序。投放顺序另见「结论」。

### 1. 文本 Embedding（原始 API 最简单）

- 端点：`POST /api/v1/services/embeddings/text-embedding/text-embedding`
- 同步。批处理场景走异步任务，同步 API 不涉及。
- 请求：`model` + `input.texts` + `parameters`（dimension / output_type / text_type）。
- 响应：`output.embeddings` 数组，每项含 `embedding`（稠密浮点数组）、
  `sparse_embedding`、`text_index`。
- 无流式，无任务轮询，无嵌套内容块。
- 复杂度：**低**。JSON 进 JSON 出。
- 引用：[文本嵌入同步 API](https://help.aliyun.com/en/model-studio/text-embedding-synchronous-api)

### 2. Rerank

- 端点：`POST /api/v1/services/rerank/text-rerank/text-rerank`
- 同步，无流式。
- 请求：`model` + `input.query` / `input.documents`（文档可以是 text 块或 image
  块）+ `parameters`（top_n / return_documents）。
- 响应：按相关性排序的文档列表。
- 复杂度：**低**。唯一的新面是文档可带图像 URL。
- 引用：[重排 API](https://help.aliyun.com/en/model-studio/rerank)

### 3. 多模态 Embedding

- 端点：`POST /api/v1/services/embeddings/multimodal-embedding/multimodal-embedding`
  （embedding 路径族）。
- 同步，无流式。
- 请求：`model` + `input` 数组，元素为带 `text` / `image` / `video` 键的对象。
- 复杂度：**低到中**。媒体以 URL 或 Base64 承载。
- 引用：[Embedding 与 Rerank 模型](https://help.aliyun.com/en/model-studio/embedding-rerank-model/)

### 4. 多模态生成（本期投放目标）

- 端点：`POST /api/v1/services/aigc/multimodal-generation/generation`
- 同步，也支持 SSE 流式：`X-DashScope-SSE: enable` 请求头声明，与文本生成端点
  同一套约定。
- 请求：`model` + `input.messages`（content 为内容块数组）+ `parameters`
  （`incremental_output` 控制流式增量；`enable_thinking` 视模型而定）。
- **内容块契约**：`content` 数组里是四种单键块。
  - `text`：文本。
  - `image`：公网 URL、`data:image/;base64,` 编码，或本地文件路径。
  - `audio`：音频 URL 等承载形式。
  - `video`：**array 或 string** 两形。传图像列表（视频帧）时是数组，传视频
    文件时是字符串；另有可选 `fps` 抽帧参数。
  - **没有 file 块**。官方内容块词表到 video 为止。
- 复杂度：**中**。相对文本生成，新增面只有内容块；请求信封（model / input /
  parameters）、SSE 头、用量口径都与文本端点同形。
- 引用：
  - [DashScope API 参考](https://help.aliyun.com/zh/model-studio/qwen-api-via-dashscope)：
    明示多模态模型（如 qwen3.7-plus、qwen3-vl-plus）经
    `POST .../api/v1/services/aigc/multimodal-generation/generation` 调用，
    content 的 text / image / video 块与 video 两种形态在此定义，音频输入示例
    用 `{"audio": "..."}` 块。
  - [Qwen-Omni](https://help.aliyun.com/zh/model-studio/qwen-omni)：输入模态为
    文本、图片、音频、视频；视频可传文件或图像列表。注意新版 Qwen-Omni 系列
    （Qwen3.5-Omni 等）仅支持 OpenAI 兼容调用；走 DashScope 原生线的以
    qwen3-vl-plus、qwen3.7-plus、qwen-audio-turbo 等模型为代表。
  - [Omni 模型总览](https://help.aliyun.com/en/model-studio/omni/)：HTTP 模型
    输入列统一为 Text, audio, images, video。

### 同一 URL，多份契约

`/api/v1/services/aigc/multimodal-generation/generation` 至少被四类互不相关的
产品 API 共用。网关的多模态门只认第一行：

| 契约 | 识别特征 | 请求形状 | 证据 |
|---|---|---|---|
| **Qwen 模型 API（本门的契约）** | qwen3-vl-plus、qwen3.7-plus、qwen-audio-turbo 等模型名 | `model` + `input.messages` + `parameters` | [DashScope API 参考](https://help.aliyun.com/zh/model-studio/qwen-api-via-dashscope) |
| 多模态交互套件 | `model` 固定为 `multimodal-dialog` | `input.directive` / `app_id` / `text`，`parameters.client_info` / `images`，与 messages 无关 | [多模态交互 HTTP 协议](https://help.aliyun.com/zh/model-studio/multimodal-http-protocol) |
| 语音识别 | qwen3-asr-flash、fun-asr-flash 等 | content 为 `{"audio": ...}` 块；部分模型响应不走 `output.choices`，走 `output.output.sentence.text` | [语音文件识别](https://www.alibabacloud.com/help/en/model-studio/qwen-speech-recognition) |
| 图像生成 | qwen-image-2.0-pro 等 | content 为 `{"image"/"text"}` 块，`parameters.size` / `negative_prompt` | [模型与能力总览](https://www.alibabacloud.com/help/en/model-studio/models) |

这份区分对网关的意义：多模态门的能力兑现只针对 Qwen 模型 API 契约。其余共用
URL 的产品请求不属于该门的兑现对象；同源直通约定下，解码器按尽力而为识别能力，
字节原样透传，受理由上游决定。

### 5. 语音合成（TTS）

- 形态分两类：第三方 TTS（如 MiniMax）的同步 HTTP 契约，见引用页；Qwen 系实时
  语音走 WebSocket（网关已有 `dashscope.realtime` 与 `dashscope.inference` 两条
  独立路径承载）。
- 同步形态返回音频数据或 URL；流式形态经 SSE 返回音频分片。
- 复杂度：**中到高**。音频分片编码与 WebSocket 协议都是新机制。
- 引用：[MiniMax 同步语音合成 API](https://help.aliyun.com/en/model-studio/minimax-synchronous-speech-synthesis-api)

### 6. 图像 / 视频生成

- 端点分裂：老模型走 `text2image/image-synthesis` 且普遍需要异步任务轮询
  （`GET /api/v1/tasks/{task_id}`）；qwen-image-2.0-pro 走 multimodal-generation
  同步路径；视频合成走 `video-generation/video-synthesis` + `X-DashScope-Async`
  异步任务。
- 请求：`model` + `input.messages` 或 `input.prompt` + `parameters`（size / n /
  resolution 等）。
- 复杂度：**高**。异步任务轮询打破请求/响应模型，需要状态管理，与现有同步 +
  SSE 的转发机制不兼容。
- 引用：[Wan 图像生成与编辑 API](https://help.aliyun.com/en/model-studio/wan-image-generation-and-editing-api-reference)、
  [模型与能力总览](https://www.alibabacloud.com/help/en/model-studio/models)

## 结论：为什么选多模态生成，尽管它的原始 API 不是最简单

按原始 API 复杂度，文本 Embedding 是最容易的 tracer bullet。但投放顺序不是 API
复杂度的排名，而是「哪个端点让已有代码的投资回报最大」。结论是投放多模态生成：

1. **它的复杂度在代码库里已经付过了**。`internal/protocol/dashscopenative` 的
   解码器已经解析 text / image / audio / video 四种内容块（纯键式与带 type 两种
   形态都识别）、按 data URI 统计内联字节、对 video 帧数组逐帧累加，且都有单测
   固化。文本生成端点已经验证了 generation 形状的同步与 SSE 转发、用量抽取、
   兑现纪律。多模态门的增量工作只剩端点登记与能力兑现。
2. **它兑现的是本网关的存在理由**。vision_input / audio_input / video_input 在
   全部入站协议中只有 `dashscope.native` 表达得出来，Phase 1 的多模态轴把
   DashScope 排第一。Embedding 虽然简单，兑现的只是单项 embedding 能力，且请求
   形状（`input.texts`）与已建好的 generation 机制零交集，等于另起炉灶。
3. **文本 Embedding 门仍是后续候选**。它的简单性作为下一步投放仍有价值，
   本调研保留其契约材料。

**采纳范围**：多模态生成端点兑现 `text_generation`、`streaming`、`vision_input`、
`audio_input`、`video_input` 五项能力，覆盖同步与 SSE 两种传输。`file_input`
不兑现（官方内容块没有 file）；`tool_calling` / `reasoning` / `web_search` 在
该门没有 fixture 证据，不兑现。设计细节见
`docs/superpowers/specs/2026-08-14-dashscope-native-multimodal-endpoint-design.md`。
