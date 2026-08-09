# 降级矩阵

> **本文件由 `internal/degrade` 自动生成，请勿手工编辑。**
> 修改 `internal/degrade/rules_phase1.go` 后运行 `make matrix-update` 重新生成。

每条转换路径都必须对入站协议**表达得出来**的每一项能力明确表态：

| 处置 | 含义 | 计入保留度 |
|---|---|---|
| `PASSTHROUGH` | 能力完整传递给上游，无语义损失 | 满分 |
| `EMULATE` | 上游不提供，由网关自行实现；客户端拿到的能力是完整的，但带着网关侧的可用性边界 | 满分 |
| `DEGRADE` | 请求仍然有效，但部分语义被丢弃；网关通过 `X-Omugw-Degraded` 响应头告知客户端 | 半分 |
| `REJECT` | 这条路径无法承载该能力，请求直接失败（HTTP 422） | 零分 |
| `N/A` | 入站协议根本表达不出该能力，客户端连发都发不出来；由可表达性声明自动推导，注明该去哪个协议 | **不进分母** |

`N/A` 单列是有原因的。早先的版本把它和 `REJECT` 混为一谈，结果一条零损失的字节直通路径只拿到 0.704 分——读起来像丢了三成能力，实际一点没丢。**可表达性是协议的属性，不是路径的属性**：OpenAI Chat 的客户端没有字段可以发出 Anthropic 的推理签名，不该让每条 `openai.chat` 路径为此扣分。

**未登记的组合按 `REJECT` 处理。** 这是刻意的失败方向：漏配一格的后果是请求被拒绝，而不是请求丢了半数字段还返回 200。

## 选路偏好与原生能力保留度

**入站协议族接入优先级**（依据是能表达多少原生能力）：

1. **OpenAI** — `openai.responses`、`openai.chat`、`openai.realtime`（族内按表达力从强到弱排列）
2. **DashScope** — `dashscope.native`、`dashscope.realtime`、`dashscope.inference`（族内按表达力从强到弱排列）
3. **Anthropic Messages** — 尚未接入
4. **Gemini** — 尚未接入

同族协议共用编解码基础设施与错误信封，接入其中一个之后再接入另一个的边际成本很低，因此按族而不是按单个协议排优先级。

**出站选路偏好**（越靠前越优先，依据是原生能力保留度而非延迟或成本）：`openai.compat` → `dashscope.compatible` → `dashscope.native` → `anthropic.messages` → `openai.realtime` → `dashscope.ws.realtime` → `dashscope.ws.inference`

保留度 = (透传 + 模拟 + 0.5 × 降级) / **可表达能力数**。分母只算入站协议表达得出来的能力：客户端发不出来的东西，这条路径没有义务为它负责。降级不计零分，是因为请求仍然成功——把它与「直接失败」等同看待，会让选路偏向一条谁都用不了的路径。

**同源快通道永远排在最前，然后才轮到全局偏好序。** 固定的全局顺序表达不了「同源优先」，而后者依赖入站协议是谁——对 `dashscope.realtime` 入站，DashScope 侧直通是零损失的，可在全局序里 `openai.realtime` 排得更靠前。

保留度分两列（见 ADR-0002）：**设计目标**假定全部实现、全部开关开启，回答「这条路最终能做到什么」；**当前可用**受实现状态与默认配置影响，是选路的唯一依据。尚未实现的路径没有当前可用分数——给一条走不通的路打分，是在请人相信一个还不存在的东西。

| 入站 | 出站 | 状态 | 快通道 | 透传 | 模拟 | 降级 | 拒绝 | N/A | 设计目标 | 当前可用 |
|---|---|---|---|---:|---:|---:|---:|---:|---:|---:|
| `dashscope.inference` | `dashscope.ws.inference` | 规划中 | ✅ | 6 | 0 | 0 | 0 | 21 | 1.000 | — |
| `dashscope.native` | `dashscope.native` | 规划中 | ✅ | 18 | 0 | 0 | 0 | 9 | 1.000 | — |
| `dashscope.realtime` | `dashscope.ws.realtime` | 规划中 | ✅ | 15 | 0 | 0 | 0 | 12 | 1.000 | — |
| `dashscope.realtime` | `openai.realtime` | 规划中 |  | 10 | 0 | 2 | 3 | 12 | 0.733 | — |
| `openai.chat` | `openai.compat` | 规划中 | ✅ | 11 | 0 | 0 | 0 | 16 | 1.000 | — |
| `openai.chat` | `dashscope.compatible` | 规划中 |  | 7 | 0 | 2 | 2 | 16 | 0.727 | — |
| `openai.chat` | `dashscope.native` | 规划中 |  | 6 | 0 | 3 | 2 | 16 | 0.682 | — |
| `openai.chat` | `anthropic.messages` | 规划中 |  | 6 | 0 | 1 | 4 | 16 | 0.591 | — |
| `openai.realtime` | `openai.realtime` | 规划中 | ✅ | 12 | 0 | 0 | 0 | 15 | 1.000 | — |
| `openai.realtime` | `dashscope.ws.realtime` | 规划中 | ✅ | 10 | 0 | 2 | 0 | 15 | 0.917 | — |
| `openai.responses` | `openai.compat` | 已实现 | ✅ | 13 | 1（1 未开启） | 0 | 0 | 13 | 1.000 | 0.929（开启 convstore 后 1.000） |
| `openai.responses` | `dashscope.compatible` | 规划中 |  | 7 | 1（1 未开启） | 2 | 4 | 13 | 0.643 | — |
| `openai.responses` | `dashscope.native` | 规划中 |  | 6 | 1（1 未开启） | 3 | 4 | 13 | 0.607 | — |
| `openai.responses` | `anthropic.messages` | 规划中 |  | 6 | 1（1 未开启） | 1 | 6 | 13 | 0.536 | — |

## `dashscope.inference` → `dashscope.ws.inference`

**同源快通道。** 该路径可字节级透传，只改写鉴权，不进 Canonical。

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.native |
| `parallel_tool_calls` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.native |
| `structured_output` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.native |
| `reasoning` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.native |
| `reasoning_signature` | `N/A` | 该协议的线格式没有承载推理签名的字段，客户端无从表达；这项能力要到 Anthropic Messages 入站接入后（Phase 2）才可达 |
| `vision_input` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.native |
| `audio_input` | `PASSTHROUGH` | — |
| `video_input` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.native |
| `file_input` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.native |
| `audio_output` | `PASSTHROUGH` | — |
| `image_generation` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.native |
| `video_generation` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.native |
| `speech_synthesis` | `PASSTHROUGH` | — |
| `speech_recognition` | `PASSTHROUGH` | — |
| `embedding` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.native |
| `rerank` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.native |
| `prompt_cache` | `N/A` | 该协议的缓存由上游自动管理，客户端没有可控的缓存断点字段；显式缓存断点是 Anthropic 特有机制，Phase 2 随 Anthropic 入站一并接入 |
| `stateful_conversation` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_session` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_image_input` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_commit_modes` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_server_vad` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_interrupt_turns` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.realtime |
| `web_search` | `N/A` | dashscope.inference 表达不了该能力，请改用入站协议 dashscope.native |
| `computer_use` | `N/A` | 该协议未提供 computer use 类的内建工具定义 |

## `dashscope.native` → `dashscope.native`

**同源快通道。** 该路径可字节级透传，只改写鉴权，不进 Canonical。

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `PASSTHROUGH` | — |
| `structured_output` | `PASSTHROUGH` | — |
| `reasoning` | `PASSTHROUGH` | — |
| `reasoning_signature` | `N/A` | 该协议的线格式没有承载推理签名的字段，客户端无从表达；这项能力要到 Anthropic Messages 入站接入后（Phase 2）才可达 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `PASSTHROUGH` | — |
| `video_input` | `PASSTHROUGH` | — |
| `file_input` | `PASSTHROUGH` | — |
| `audio_output` | `PASSTHROUGH` | — |
| `image_generation` | `PASSTHROUGH` | — |
| `video_generation` | `PASSTHROUGH` | — |
| `speech_synthesis` | `PASSTHROUGH` | — |
| `speech_recognition` | `PASSTHROUGH` | — |
| `embedding` | `PASSTHROUGH` | — |
| `rerank` | `PASSTHROUGH` | — |
| `prompt_cache` | `N/A` | 该协议的缓存由上游自动管理，客户端没有可控的缓存断点字段；显式缓存断点是 Anthropic 特有机制，Phase 2 随 Anthropic 入站一并接入 |
| `stateful_conversation` | `N/A` | dashscope.native 表达不了该能力，请改用入站协议 openai.responses |
| `realtime_session` | `N/A` | dashscope.native 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_image_input` | `N/A` | dashscope.native 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_commit_modes` | `N/A` | dashscope.native 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_server_vad` | `N/A` | dashscope.native 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_interrupt_turns` | `N/A` | dashscope.native 表达不了该能力，请改用入站协议 dashscope.realtime |
| `web_search` | `PASSTHROUGH` | — |
| `computer_use` | `N/A` | 该协议未提供 computer use 类的内建工具定义 |

## `dashscope.realtime` → `dashscope.ws.realtime`

**同源快通道。** 该路径可字节级透传，只改写鉴权，不进 Canonical。

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `PASSTHROUGH` | — |
| `structured_output` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `reasoning` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `reasoning_signature` | `N/A` | 该协议的线格式没有承载推理签名的字段，客户端无从表达；这项能力要到 Anthropic Messages 入站接入后（Phase 2）才可达 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `PASSTHROUGH` | — |
| `video_input` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `file_input` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `audio_output` | `PASSTHROUGH` | — |
| `image_generation` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `video_generation` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `speech_synthesis` | `PASSTHROUGH` | — |
| `speech_recognition` | `PASSTHROUGH` | — |
| `embedding` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `rerank` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `prompt_cache` | `N/A` | 该协议的缓存由上游自动管理，客户端没有可控的缓存断点字段；显式缓存断点是 Anthropic 特有机制，Phase 2 随 Anthropic 入站一并接入 |
| `stateful_conversation` | `PASSTHROUGH` | — |
| `realtime_session` | `PASSTHROUGH` | — |
| `realtime_image_input` | `PASSTHROUGH` | — |
| `realtime_commit_modes` | `PASSTHROUGH` | — |
| `realtime_server_vad` | `PASSTHROUGH` | — |
| `realtime_interrupt_turns` | `PASSTHROUGH` | — |
| `web_search` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `computer_use` | `N/A` | 该协议未提供 computer use 类的内建工具定义 |

## `dashscope.realtime` → `openai.realtime`

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `DEGRADE` | OpenAI Realtime 未提供并行工具调用开关，行为由上游模型决定 |
| `structured_output` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `reasoning` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `reasoning_signature` | `N/A` | 该协议的线格式没有承载推理签名的字段，客户端无从表达；这项能力要到 Anthropic Messages 入站接入后（Phase 2）才可达 |
| `vision_input` | `REJECT` | OpenAI Realtime 没有 input_image_buffer 事件，图像输入无处安放 |
| `audio_input` | `DEGRADE` | 输入音频需从 DashScope 的 16 kHz 重采样到 OpenAI 的 24 kHz；上采样补不回原本就没采到的高频信息，只是满足格式要求 |
| `video_input` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `file_input` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `audio_output` | `PASSTHROUGH` | — |
| `image_generation` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `video_generation` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `speech_synthesis` | `PASSTHROUGH` | — |
| `speech_recognition` | `PASSTHROUGH` | — |
| `embedding` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `rerank` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `prompt_cache` | `N/A` | 该协议的缓存由上游自动管理，客户端没有可控的缓存断点字段；显式缓存断点是 Anthropic 特有机制，Phase 2 随 Anthropic 入站一并接入 |
| `stateful_conversation` | `PASSTHROUGH` | — |
| `realtime_session` | `PASSTHROUGH` | — |
| `realtime_image_input` | `REJECT` | OpenAI Realtime 没有 input_image_buffer 事件，图像输入无处安放 |
| `realtime_commit_modes` | `REJECT` | server_commit / commit 是 Qwen-TTS-Realtime 特有的提交模式，OpenAI Realtime 协议中没有对应字段 |
| `realtime_server_vad` | `PASSTHROUGH` | — |
| `realtime_interrupt_turns` | `PASSTHROUGH` | — |
| `web_search` | `N/A` | dashscope.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `computer_use` | `N/A` | 该协议未提供 computer use 类的内建工具定义 |

## `openai.chat` → `anthropic.messages`

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `PASSTHROUGH` | — |
| `structured_output` | `DEGRADE` | 该 Provider 无 strict json_schema 校验，schema 降级为提示词约束，模型可能返回不合规 JSON |
| `reasoning` | `PASSTHROUGH` | — |
| `reasoning_signature` | `N/A` | 该协议的线格式没有承载推理签名的字段，客户端无从表达；这项能力要到 Anthropic Messages 入站接入后（Phase 2）才可达 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `REJECT` | Anthropic Messages 不接受音频输入 |
| `video_input` | `N/A` | OpenAI 的线格式不接受视频输入 |
| `file_input` | `REJECT` | 文件引用绑定具体 Provider，跨 Provider 不可迁移；网关不代下载再上传（原则 2.6），请改用 URL 或内联字节 |
| `audio_output` | `REJECT` | 该 Provider 不产生音频输出 |
| `image_generation` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.responses |
| `video_generation` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.native |
| `speech_synthesis` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.inference |
| `speech_recognition` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.inference |
| `embedding` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.native |
| `rerank` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.native |
| `prompt_cache` | `N/A` | 该协议的缓存由上游自动管理，客户端没有可控的缓存断点字段；显式缓存断点是 Anthropic 特有机制，Phase 2 随 Anthropic 入站一并接入 |
| `stateful_conversation` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.responses |
| `realtime_session` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.realtime |
| `realtime_image_input` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_commit_modes` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_server_vad` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.realtime |
| `realtime_interrupt_turns` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.realtime |
| `web_search` | `REJECT` | 该内建工具在 Phase 1 不做跨 Provider 映射——各家的工具 schema 不兼容，勉强映射只会让模型收到一个它读不懂的定义 |
| `computer_use` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.responses |

## `openai.chat` → `dashscope.compatible`

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `PASSTHROUGH` | — |
| `structured_output` | `DEGRADE` | DashScope 兼容模式支持 json_object，但不保证 strict json_schema 校验 |
| `reasoning` | `PASSTHROUGH` | — |
| `reasoning_signature` | `N/A` | 该协议的线格式没有承载推理签名的字段，客户端无从表达；这项能力要到 Anthropic Messages 入站接入后（Phase 2）才可达 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `PASSTHROUGH` | — |
| `video_input` | `N/A` | OpenAI 的线格式不接受视频输入 |
| `file_input` | `REJECT` | 文件引用绑定具体 Provider，跨 Provider 不可迁移；网关不代下载再上传（原则 2.6），请改用 URL 或内联字节 |
| `audio_output` | `REJECT` | 兼容模式不返回音频；音频输出须经 Qwen-Omni Realtime 或 Native 端点 |
| `image_generation` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.responses |
| `video_generation` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.native |
| `speech_synthesis` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.inference |
| `speech_recognition` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.inference |
| `embedding` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.native |
| `rerank` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.native |
| `prompt_cache` | `N/A` | 该协议的缓存由上游自动管理，客户端没有可控的缓存断点字段；显式缓存断点是 Anthropic 特有机制，Phase 2 随 Anthropic 入站一并接入 |
| `stateful_conversation` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.responses |
| `realtime_session` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.realtime |
| `realtime_image_input` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_commit_modes` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_server_vad` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.realtime |
| `realtime_interrupt_turns` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.realtime |
| `web_search` | `DEGRADE` | DashScope 的 enable_search 是布尔开关，承载不了 OpenAI web_search 工具的参数，仅开关本身被映射 |
| `computer_use` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.responses |

## `openai.chat` → `dashscope.native`

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `DEGRADE` | DashScope Native 的并行工具调用行为由上游模型决定，无显式开关可映射 |
| `structured_output` | `DEGRADE` | DashScope Native 支持 response_format=json_object，无 strict schema 校验 |
| `reasoning` | `PASSTHROUGH` | — |
| `reasoning_signature` | `N/A` | 该协议的线格式没有承载推理签名的字段，客户端无从表达；这项能力要到 Anthropic Messages 入站接入后（Phase 2）才可达 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `PASSTHROUGH` | — |
| `video_input` | `N/A` | OpenAI 的线格式不接受视频输入 |
| `file_input` | `REJECT` | 文件引用绑定具体 Provider，跨 Provider 不可迁移；网关不代下载再上传（原则 2.6），请改用 URL 或内联字节 |
| `audio_output` | `REJECT` | 音频输出需要 Qwen-Omni 的输出格式参数，Chat Completions 入站无法表达 |
| `image_generation` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.responses |
| `video_generation` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.native |
| `speech_synthesis` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.inference |
| `speech_recognition` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.inference |
| `embedding` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.native |
| `rerank` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.native |
| `prompt_cache` | `N/A` | 该协议的缓存由上游自动管理，客户端没有可控的缓存断点字段；显式缓存断点是 Anthropic 特有机制，Phase 2 随 Anthropic 入站一并接入 |
| `stateful_conversation` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.responses |
| `realtime_session` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.realtime |
| `realtime_image_input` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_commit_modes` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_server_vad` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.realtime |
| `realtime_interrupt_turns` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.realtime |
| `web_search` | `DEGRADE` | DashScope 的 enable_search 是布尔开关，承载不了 OpenAI web_search 工具的参数，仅开关本身被映射 |
| `computer_use` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.responses |

## `openai.chat` → `openai.compat`

**同源快通道。** 该路径可字节级透传，只改写鉴权，不进 Canonical。

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `PASSTHROUGH` | — |
| `structured_output` | `PASSTHROUGH` | — |
| `reasoning` | `PASSTHROUGH` | — |
| `reasoning_signature` | `N/A` | 该协议的线格式没有承载推理签名的字段，客户端无从表达；这项能力要到 Anthropic Messages 入站接入后（Phase 2）才可达 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `PASSTHROUGH` | — |
| `video_input` | `N/A` | OpenAI 的线格式不接受视频输入 |
| `file_input` | `PASSTHROUGH` | — |
| `audio_output` | `PASSTHROUGH` | — |
| `image_generation` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.responses |
| `video_generation` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.native |
| `speech_synthesis` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.inference |
| `speech_recognition` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.inference |
| `embedding` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.native |
| `rerank` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.native |
| `prompt_cache` | `N/A` | 该协议的缓存由上游自动管理，客户端没有可控的缓存断点字段；显式缓存断点是 Anthropic 特有机制，Phase 2 随 Anthropic 入站一并接入 |
| `stateful_conversation` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.responses |
| `realtime_session` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.realtime |
| `realtime_image_input` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_commit_modes` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_server_vad` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.realtime |
| `realtime_interrupt_turns` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.realtime |
| `web_search` | `PASSTHROUGH` | — |
| `computer_use` | `N/A` | openai.chat 表达不了该能力，请改用入站协议 openai.responses |

## `openai.realtime` → `dashscope.ws.realtime`

**同源快通道。** 该路径可字节级透传，只改写鉴权，不进 Canonical。

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `DEGRADE` | DashScope Realtime 未提供并行工具调用开关，行为由上游模型决定 |
| `structured_output` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 openai.responses |
| `reasoning` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 openai.responses |
| `reasoning_signature` | `N/A` | 该协议的线格式没有承载推理签名的字段，客户端无从表达；这项能力要到 Anthropic Messages 入站接入后（Phase 2）才可达 |
| `vision_input` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 openai.responses |
| `audio_input` | `DEGRADE` | 输入音频需从 OpenAI 的 24 kHz 重采样到 DashScope 的 16 kHz，高频信息丢失；输出侧两者同为 24 kHz，无需转换 |
| `video_input` | `N/A` | OpenAI 的线格式不接受视频输入 |
| `file_input` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 openai.responses |
| `audio_output` | `PASSTHROUGH` | — |
| `image_generation` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 openai.responses |
| `video_generation` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `speech_synthesis` | `PASSTHROUGH` | — |
| `speech_recognition` | `PASSTHROUGH` | — |
| `embedding` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `rerank` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `prompt_cache` | `N/A` | 该协议的缓存由上游自动管理，客户端没有可控的缓存断点字段；显式缓存断点是 Anthropic 特有机制，Phase 2 随 Anthropic 入站一并接入 |
| `stateful_conversation` | `PASSTHROUGH` | — |
| `realtime_session` | `PASSTHROUGH` | — |
| `realtime_image_input` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_commit_modes` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_server_vad` | `PASSTHROUGH` | — |
| `realtime_interrupt_turns` | `PASSTHROUGH` | — |
| `web_search` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 openai.responses |
| `computer_use` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 openai.responses |

## `openai.realtime` → `openai.realtime`

**同源快通道。** 该路径可字节级透传，只改写鉴权，不进 Canonical。

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `PASSTHROUGH` | — |
| `structured_output` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 openai.responses |
| `reasoning` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 openai.responses |
| `reasoning_signature` | `N/A` | 该协议的线格式没有承载推理签名的字段，客户端无从表达；这项能力要到 Anthropic Messages 入站接入后（Phase 2）才可达 |
| `vision_input` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 openai.responses |
| `audio_input` | `PASSTHROUGH` | — |
| `video_input` | `N/A` | OpenAI 的线格式不接受视频输入 |
| `file_input` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 openai.responses |
| `audio_output` | `PASSTHROUGH` | — |
| `image_generation` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 openai.responses |
| `video_generation` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `speech_synthesis` | `PASSTHROUGH` | — |
| `speech_recognition` | `PASSTHROUGH` | — |
| `embedding` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `rerank` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 dashscope.native |
| `prompt_cache` | `N/A` | 该协议的缓存由上游自动管理，客户端没有可控的缓存断点字段；显式缓存断点是 Anthropic 特有机制，Phase 2 随 Anthropic 入站一并接入 |
| `stateful_conversation` | `PASSTHROUGH` | — |
| `realtime_session` | `PASSTHROUGH` | — |
| `realtime_image_input` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_commit_modes` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_server_vad` | `PASSTHROUGH` | — |
| `realtime_interrupt_turns` | `PASSTHROUGH` | — |
| `web_search` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 openai.responses |
| `computer_use` | `N/A` | openai.realtime 表达不了该能力，请改用入站协议 openai.responses |

## `openai.responses` → `anthropic.messages`

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `PASSTHROUGH` | — |
| `structured_output` | `DEGRADE` | 该 Provider 无 strict json_schema 校验，schema 降级为提示词约束，模型可能返回不合规 JSON |
| `reasoning` | `PASSTHROUGH` | — |
| `reasoning_signature` | `N/A` | 该协议的线格式没有承载推理签名的字段，客户端无从表达；这项能力要到 Anthropic Messages 入站接入后（Phase 2）才可达 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `REJECT` | Anthropic Messages 不接受音频输入 |
| `video_input` | `N/A` | OpenAI 的线格式不接受视频输入 |
| `file_input` | `REJECT` | 文件引用绑定具体 Provider，跨 Provider 不可迁移；网关不代下载再上传（原则 2.6），请改用 URL 或内联字节 |
| `audio_output` | `REJECT` | 该 Provider 不产生音频输出 |
| `image_generation` | `REJECT` | Responses 的内建 image_generation 工具在 Phase 1 不做路由，请改用 /v1/jobs 端点 |
| `video_generation` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.native |
| `speech_synthesis` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.inference |
| `speech_recognition` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.inference |
| `embedding` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.native |
| `rerank` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.native |
| `prompt_cache` | `N/A` | 该协议的缓存由上游自动管理，客户端没有可控的缓存断点字段；显式缓存断点是 Anthropic 特有机制，Phase 2 随 Anthropic 入站一并接入 |
| `stateful_conversation` | `EMULATE` | 上游无服务端会话，由网关侧 ConversationStore 模拟提供。Phase 1 为内存态：单副本正确，进程重启后历史丢失，多副本部署下会话不共享。默认关闭，需显式开启 convstore |
| `realtime_session` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 openai.realtime |
| `realtime_image_input` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_commit_modes` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_server_vad` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 openai.realtime |
| `realtime_interrupt_turns` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 openai.realtime |
| `web_search` | `REJECT` | 该内建工具在 Phase 1 不做跨 Provider 映射——各家的工具 schema 不兼容，勉强映射只会让模型收到一个它读不懂的定义 |
| `computer_use` | `REJECT` | 该内建工具在 Phase 1 不做跨 Provider 映射——各家的工具 schema 不兼容，勉强映射只会让模型收到一个它读不懂的定义 |

## `openai.responses` → `dashscope.compatible`

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `PASSTHROUGH` | — |
| `structured_output` | `DEGRADE` | DashScope 兼容模式支持 json_object，但不保证 strict json_schema 校验 |
| `reasoning` | `PASSTHROUGH` | — |
| `reasoning_signature` | `N/A` | 该协议的线格式没有承载推理签名的字段，客户端无从表达；这项能力要到 Anthropic Messages 入站接入后（Phase 2）才可达 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `PASSTHROUGH` | — |
| `video_input` | `N/A` | OpenAI 的线格式不接受视频输入 |
| `file_input` | `REJECT` | 文件引用绑定具体 Provider，跨 Provider 不可迁移；网关不代下载再上传（原则 2.6），请改用 URL 或内联字节 |
| `audio_output` | `REJECT` | 兼容模式不返回音频；音频输出须经 Qwen-Omni Realtime 或 Native 端点 |
| `image_generation` | `REJECT` | Responses 的内建 image_generation 工具在 Phase 1 不做路由，请改用 /v1/jobs 端点 |
| `video_generation` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.native |
| `speech_synthesis` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.inference |
| `speech_recognition` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.inference |
| `embedding` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.native |
| `rerank` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.native |
| `prompt_cache` | `N/A` | 该协议的缓存由上游自动管理，客户端没有可控的缓存断点字段；显式缓存断点是 Anthropic 特有机制，Phase 2 随 Anthropic 入站一并接入 |
| `stateful_conversation` | `EMULATE` | 上游无服务端会话，由网关侧 ConversationStore 模拟提供。Phase 1 为内存态：单副本正确，进程重启后历史丢失，多副本部署下会话不共享。默认关闭，需显式开启 convstore |
| `realtime_session` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 openai.realtime |
| `realtime_image_input` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_commit_modes` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_server_vad` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 openai.realtime |
| `realtime_interrupt_turns` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 openai.realtime |
| `web_search` | `DEGRADE` | DashScope 的 enable_search 是布尔开关，承载不了 OpenAI web_search 工具的参数，仅开关本身被映射 |
| `computer_use` | `REJECT` | 该内建工具在 Phase 1 不做跨 Provider 映射——各家的工具 schema 不兼容，勉强映射只会让模型收到一个它读不懂的定义 |

## `openai.responses` → `dashscope.native`

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `DEGRADE` | DashScope Native 的并行工具调用行为由上游模型决定，无显式开关可映射 |
| `structured_output` | `DEGRADE` | DashScope Native 支持 response_format=json_object，无 strict schema 校验 |
| `reasoning` | `PASSTHROUGH` | — |
| `reasoning_signature` | `N/A` | 该协议的线格式没有承载推理签名的字段，客户端无从表达；这项能力要到 Anthropic Messages 入站接入后（Phase 2）才可达 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `PASSTHROUGH` | — |
| `video_input` | `N/A` | OpenAI 的线格式不接受视频输入 |
| `file_input` | `REJECT` | 文件引用绑定具体 Provider，跨 Provider 不可迁移；网关不代下载再上传（原则 2.6），请改用 URL 或内联字节 |
| `audio_output` | `REJECT` | 音频输出需要 Qwen-Omni 的输出格式参数，Chat Completions 入站无法表达 |
| `image_generation` | `REJECT` | Responses 的内建 image_generation 工具在 Phase 1 不做路由，请改用 /v1/jobs 端点 |
| `video_generation` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.native |
| `speech_synthesis` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.inference |
| `speech_recognition` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.inference |
| `embedding` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.native |
| `rerank` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.native |
| `prompt_cache` | `N/A` | 该协议的缓存由上游自动管理，客户端没有可控的缓存断点字段；显式缓存断点是 Anthropic 特有机制，Phase 2 随 Anthropic 入站一并接入 |
| `stateful_conversation` | `EMULATE` | 上游无服务端会话，由网关侧 ConversationStore 模拟提供。Phase 1 为内存态：单副本正确，进程重启后历史丢失，多副本部署下会话不共享。默认关闭，需显式开启 convstore |
| `realtime_session` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 openai.realtime |
| `realtime_image_input` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_commit_modes` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_server_vad` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 openai.realtime |
| `realtime_interrupt_turns` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 openai.realtime |
| `web_search` | `DEGRADE` | DashScope 的 enable_search 是布尔开关，承载不了 OpenAI web_search 工具的参数，仅开关本身被映射 |
| `computer_use` | `REJECT` | 该内建工具在 Phase 1 不做跨 Provider 映射——各家的工具 schema 不兼容，勉强映射只会让模型收到一个它读不懂的定义 |

## `openai.responses` → `openai.compat`

**同源快通道。** 该路径可字节级透传，只改写鉴权，不进 Canonical。

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `PASSTHROUGH` | — |
| `structured_output` | `PASSTHROUGH` | — |
| `reasoning` | `PASSTHROUGH` | — |
| `reasoning_signature` | `N/A` | 该协议的线格式没有承载推理签名的字段，客户端无从表达；这项能力要到 Anthropic Messages 入站接入后（Phase 2）才可达 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `PASSTHROUGH` | — |
| `video_input` | `N/A` | OpenAI 的线格式不接受视频输入 |
| `file_input` | `PASSTHROUGH` | — |
| `audio_output` | `PASSTHROUGH` | — |
| `image_generation` | `PASSTHROUGH` | — |
| `video_generation` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.native |
| `speech_synthesis` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.inference |
| `speech_recognition` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.inference |
| `embedding` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.native |
| `rerank` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.native |
| `prompt_cache` | `N/A` | 该协议的缓存由上游自动管理，客户端没有可控的缓存断点字段；显式缓存断点是 Anthropic 特有机制，Phase 2 随 Anthropic 入站一并接入 |
| `stateful_conversation` | `EMULATE` | 上游无服务端会话，由网关侧 ConversationStore 模拟提供。Phase 1 为内存态：单副本正确，进程重启后历史丢失，多副本部署下会话不共享。默认关闭，需显式开启 convstore |
| `realtime_session` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 openai.realtime |
| `realtime_image_input` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_commit_modes` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 dashscope.realtime |
| `realtime_server_vad` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 openai.realtime |
| `realtime_interrupt_turns` | `N/A` | openai.responses 表达不了该能力，请改用入站协议 openai.realtime |
| `web_search` | `PASSTHROUGH` | — |
| `computer_use` | `PASSTHROUGH` | — |

