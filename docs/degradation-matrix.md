# 降级矩阵

> **本文件由 `internal/degrade` 自动生成，请勿手工编辑。**
> 修改 `internal/degrade/rules_phase1.go` 后运行 `make matrix-update` 重新生成。

每条转换路径都必须对**每一项**能力明确表态。三种处置：

| 处置 | 含义 |
|---|---|
| `PASSTHROUGH` | 能力完整传递给上游，无语义损失 |
| `DEGRADE` | 请求仍然有效，但部分语义被丢弃；网关通过 `X-Omugw-Degraded` 响应头告知客户端 |
| `REJECT` | 这条路径无法支持该能力，请求直接失败（HTTP 422） |

**未登记的组合按 `REJECT` 处理。** 这是刻意的失败方向：漏配一格的后果是请求被拒绝，而不是请求丢了半数字段还返回 200。

## 选路偏好与原生能力保留度

**入站协议族接入优先级**（依据是能表达多少原生能力）：

1. **OpenAI** — `openai.responses`、`openai.chat`、`openai.realtime`（族内按表达力从强到弱排列）
2. **DashScope Native** — `dashscope.native`（族内按表达力从强到弱排列）
3. **Anthropic Messages** — 尚未接入
4. **Gemini** — 尚未接入

同族协议共用编解码基础设施与错误信封，接入其中一个之后再接入另一个的边际成本很低，因此按族而不是按单个协议排优先级。

**出站选路偏好**（越靠前越优先，依据是原生能力保留度而非延迟或成本）：`openai.compat` → `dashscope.compatible` → `dashscope.native` → `anthropic.messages` → `openai.realtime` → `dashscope.ws.realtime` → `dashscope.ws.inference`

保留度 = (透传 + 0.5 × 降级) / 总能力数。降级不计零分，是因为请求仍然成功，只是丢了部分语义——把它与「直接失败」等同看待，会让选路偏向一条谁都用不了的路径。

| 入站 | 出站 | 快通道 | 透传 | 降级 | 拒绝 | 保留度 |
|---|---|---|---:|---:|---:|---:|
| `dashscope.native` | `dashscope.native` | ✅ | 19 | 0 | 8 | 0.704 |
| `openai.chat` | `openai.compat` | ✅ | 12 | 0 | 15 | 0.444 |
| `openai.chat` | `dashscope.compatible` |  | 7 | 4 | 16 | 0.333 |
| `openai.chat` | `dashscope.native` |  | 7 | 4 | 16 | 0.333 |
| `openai.chat` | `anthropic.messages` |  | 6 | 2 | 19 | 0.259 |
| `openai.realtime` | `openai.realtime` | ✅ | 12 | 0 | 15 | 0.444 |
| `openai.realtime` | `dashscope.ws.realtime` | ✅ | 10 | 2 | 15 | 0.407 |
| `openai.responses` | `openai.compat` | ✅ | 12 | 0 | 15 | 0.444 |
| `openai.responses` | `dashscope.compatible` |  | 7 | 4 | 16 | 0.333 |
| `openai.responses` | `dashscope.native` |  | 7 | 4 | 16 | 0.333 |
| `openai.responses` | `anthropic.messages` |  | 6 | 2 | 19 | 0.259 |

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
| `reasoning_signature` | `REJECT` | DashScope 不产生带签名的推理块；出现签名说明数据来自其他协议 |
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
| `prompt_cache` | `PASSTHROUGH` | — |
| `stateful_conversation` | `REJECT` | DashScope Native 的 HTTP 端点无服务端会话状态 |
| `realtime_session` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_image_input` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_commit_modes` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_server_vad` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_interrupt_turns` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `web_search` | `PASSTHROUGH` | — |
| `computer_use` | `REJECT` | computer use 的工具 schema 各家不兼容，不在 Phase 1 范围 |

## `openai.chat` → `anthropic.messages`

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `PASSTHROUGH` | — |
| `structured_output` | `DEGRADE` | Anthropic 无 strict json_schema 校验，schema 降级为提示词约束，模型可能返回不合规 JSON |
| `reasoning` | `PASSTHROUGH` | — |
| `reasoning_signature` | `REJECT` | Anthropic thinking 签名经 Canonical 转换后失效，带失效签名的多轮 tool use 会被上游拒绝，因此在异构路径上直接拒绝而非静默丢弃 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `REJECT` | Anthropic Messages 不接受音频输入 |
| `video_input` | `REJECT` | Anthropic Messages 不接受视频输入 |
| `file_input` | `REJECT` | 文件引用绑定具体 Provider，跨 Provider 不可迁移；网关不代下载再上传（原则 2.6），请改用 URL 或内联字节 |
| `audio_output` | `REJECT` | Anthropic 不产生音频输出 |
| `image_generation` | `REJECT` | Anthropic 不提供图像/视频生成 |
| `video_generation` | `REJECT` | Anthropic 不提供图像/视频生成 |
| `speech_synthesis` | `REJECT` | Anthropic 不提供语音能力 |
| `speech_recognition` | `REJECT` | Anthropic 不提供语音能力 |
| `embedding` | `REJECT` | Anthropic 不提供 embedding / rerank |
| `rerank` | `REJECT` | Anthropic 不提供 embedding / rerank |
| `prompt_cache` | `DEGRADE` | OpenAI 自动前缀缓存与 Anthropic 显式 cache_control 断点语义互斥，缓存意图被丢弃（请求仍然有效，但不会命中缓存） |
| `stateful_conversation` | `REJECT` | Anthropic Messages 无服务端会话状态 |
| `realtime_session` | `REJECT` | Anthropic 无 Realtime API |
| `realtime_image_input` | `REJECT` | Anthropic 无 Realtime API |
| `realtime_commit_modes` | `REJECT` | Anthropic 无 Realtime API |
| `realtime_server_vad` | `REJECT` | Anthropic 无 Realtime API |
| `realtime_interrupt_turns` | `REJECT` | Anthropic 无 Realtime API |
| `web_search` | `REJECT` | OpenAI 与 Anthropic 的内建 web_search 工具参数不兼容，Phase 1 不做映射 |
| `computer_use` | `REJECT` | computer use 的工具 schema 各家不兼容，不在 Phase 1 范围 |

## `openai.chat` → `dashscope.compatible`

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `PASSTHROUGH` | — |
| `structured_output` | `DEGRADE` | DashScope 兼容模式支持 json_object，但不保证 strict json_schema 校验 |
| `reasoning` | `PASSTHROUGH` | — |
| `reasoning_signature` | `REJECT` | Anthropic thinking 签名经 Canonical 转换后失效，带失效签名的多轮 tool use 会被上游拒绝，因此在异构路径上直接拒绝而非静默丢弃 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `PASSTHROUGH` | — |
| `video_input` | `DEGRADE` | 兼容模式接受 video_url，但帧采样率与时长上限无 OpenAI 对应字段，采用上游默认值 |
| `file_input` | `REJECT` | 文件引用绑定具体 Provider，跨 Provider 不可迁移；网关不代下载再上传（原则 2.6），请改用 URL 或内联字节 |
| `audio_output` | `REJECT` | 兼容模式不返回音频；音频输出须经 Qwen-Omni Realtime 或 Native 端点 |
| `image_generation` | `REJECT` | 兼容模式的 chat 端点不承载该能力，须走 DashScope Native |
| `video_generation` | `REJECT` | 兼容模式的 chat 端点不承载该能力，须走 DashScope Native |
| `speech_synthesis` | `REJECT` | 兼容模式的 chat 端点不承载该能力，须走 DashScope Native |
| `speech_recognition` | `REJECT` | 兼容模式的 chat 端点不承载该能力，须走 DashScope Native |
| `embedding` | `REJECT` | 兼容模式的 chat 端点不承载该能力，须走 DashScope Native |
| `rerank` | `REJECT` | 兼容模式的 chat 端点不承载该能力，须走 DashScope Native |
| `prompt_cache` | `DEGRADE` | DashScope 兼容模式的缓存由上游自动管理，显式缓存意图被丢弃 |
| `stateful_conversation` | `REJECT` | DashScope 兼容模式无服务端会话状态 |
| `realtime_session` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_image_input` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_commit_modes` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_server_vad` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_interrupt_turns` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `web_search` | `DEGRADE` | DashScope 的 enable_search 是布尔开关，承载不了 OpenAI web_search 工具的参数，仅开关本身被映射 |
| `computer_use` | `REJECT` | computer use 的工具 schema 各家不兼容，不在 Phase 1 范围 |

## `openai.chat` → `dashscope.native`

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `DEGRADE` | DashScope Native 的并行工具调用行为由上游模型决定，无显式开关可映射 |
| `structured_output` | `DEGRADE` | DashScope Native 支持 response_format=json_object，无 strict schema 校验 |
| `reasoning` | `PASSTHROUGH` | — |
| `reasoning_signature` | `REJECT` | Anthropic thinking 签名经 Canonical 转换后失效，带失效签名的多轮 tool use 会被上游拒绝，因此在异构路径上直接拒绝而非静默丢弃 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `PASSTHROUGH` | — |
| `video_input` | `PASSTHROUGH` | — |
| `file_input` | `REJECT` | 文件引用绑定具体 Provider，跨 Provider 不可迁移；网关不代下载再上传（原则 2.6），请改用 URL 或内联字节 |
| `audio_output` | `REJECT` | 音频输出需要 Qwen-Omni 的输出格式参数，Chat Completions 入站无法表达 |
| `image_generation` | `REJECT` | 图像/视频生成是异步任务，须经 /v1/jobs 端点 |
| `video_generation` | `REJECT` | 图像/视频生成是异步任务，须经 /v1/jobs 端点 |
| `speech_synthesis` | `REJECT` | 语音须经 /v1/audio 入站端点 |
| `speech_recognition` | `REJECT` | 语音须经 /v1/audio 入站端点 |
| `embedding` | `REJECT` | embedding / rerank 须经各自的独立入站端点 |
| `rerank` | `REJECT` | embedding / rerank 须经各自的独立入站端点 |
| `prompt_cache` | `DEGRADE` | DashScope Native 的缓存由上游自动管理，显式缓存意图被丢弃 |
| `stateful_conversation` | `REJECT` | DashScope Native generation 无服务端会话状态 |
| `realtime_session` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_image_input` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_commit_modes` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_server_vad` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_interrupt_turns` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `web_search` | `DEGRADE` | DashScope 的 enable_search 是布尔开关，承载不了 OpenAI web_search 工具的参数 |
| `computer_use` | `REJECT` | computer use 的工具 schema 各家不兼容，不在 Phase 1 范围 |

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
| `reasoning_signature` | `REJECT` | OpenAI Chat Completions 不产生带签名的推理块；出现签名说明数据来自其他协议 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `PASSTHROUGH` | — |
| `video_input` | `REJECT` | OpenAI Chat Completions 不接受视频输入 |
| `file_input` | `PASSTHROUGH` | — |
| `audio_output` | `PASSTHROUGH` | — |
| `image_generation` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `video_generation` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `speech_synthesis` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `speech_recognition` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `embedding` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `rerank` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `prompt_cache` | `PASSTHROUGH` | — |
| `stateful_conversation` | `REJECT` | Chat Completions 无服务端会话状态，请改用 Responses 端点 |
| `realtime_session` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_image_input` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_commit_modes` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_server_vad` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_interrupt_turns` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `web_search` | `PASSTHROUGH` | — |
| `computer_use` | `REJECT` | computer use 的工具 schema 各家不兼容，不在 Phase 1 范围 |

## `openai.realtime` → `dashscope.ws.realtime`

**同源快通道。** 该路径可字节级透传，只改写鉴权，不进 Canonical。

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `DEGRADE` | DashScope Realtime 未提供并行工具调用开关，行为由上游模型决定 |
| `structured_output` | `REJECT` | Realtime 会话不支持 response_format |
| `reasoning` | `REJECT` | Realtime 会话不返回推理内容块 |
| `reasoning_signature` | `REJECT` | Realtime 会话不返回推理内容块 |
| `vision_input` | `REJECT` | Qwen-Omni-Realtime 的图像输入依赖 input_image_buffer，OpenAI 客户端无法产生 |
| `audio_input` | `DEGRADE` | 输入音频需从 OpenAI 的 24 kHz 重采样到 DashScope 的 16 kHz，高频信息丢失；输出侧两者同为 24 kHz，无需转换 |
| `video_input` | `REJECT` | Realtime 会话不接受视频输入 |
| `file_input` | `REJECT` | 文件引用绑定具体 Provider，跨 Provider 不可迁移；网关不代下载再上传（原则 2.6），请改用 URL 或内联字节 |
| `audio_output` | `PASSTHROUGH` | — |
| `image_generation` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `video_generation` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `speech_synthesis` | `PASSTHROUGH` | — |
| `speech_recognition` | `PASSTHROUGH` | — |
| `embedding` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `rerank` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `prompt_cache` | `REJECT` | Realtime 会话无 prompt cache 概念 |
| `stateful_conversation` | `PASSTHROUGH` | — |
| `realtime_session` | `PASSTHROUGH` | — |
| `realtime_image_input` | `REJECT` | input_image_buffer.append 是 DashScope 独有事件，OpenAI Realtime 客户端无法产生 |
| `realtime_commit_modes` | `REJECT` | Qwen-TTS-Realtime 的 server_commit / commit 模式在 OpenAI Realtime 协议中无对应字段，无法由客户端指定 |
| `realtime_server_vad` | `PASSTHROUGH` | — |
| `realtime_interrupt_turns` | `PASSTHROUGH` | — |
| `web_search` | `REJECT` | Realtime 会话不支持内建 web_search 工具 |
| `computer_use` | `REJECT` | computer use 的工具 schema 各家不兼容，不在 Phase 1 范围 |

## `openai.realtime` → `openai.realtime`

**同源快通道。** 该路径可字节级透传，只改写鉴权，不进 Canonical。

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `PASSTHROUGH` | — |
| `structured_output` | `REJECT` | Realtime 会话不支持 response_format |
| `reasoning` | `REJECT` | Realtime 会话不返回推理内容块 |
| `reasoning_signature` | `REJECT` | Realtime 会话不返回推理内容块 |
| `vision_input` | `REJECT` | Realtime 会话的图像输入须走 conversation item，Phase 1 不支持 |
| `audio_input` | `PASSTHROUGH` | — |
| `video_input` | `REJECT` | Realtime 会话不接受视频输入 |
| `file_input` | `REJECT` | 文件引用绑定具体 Provider，跨 Provider 不可迁移；网关不代下载再上传（原则 2.6），请改用 URL 或内联字节 |
| `audio_output` | `PASSTHROUGH` | — |
| `image_generation` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `video_generation` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `speech_synthesis` | `PASSTHROUGH` | — |
| `speech_recognition` | `PASSTHROUGH` | — |
| `embedding` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `rerank` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `prompt_cache` | `REJECT` | Realtime 会话无 prompt cache 概念 |
| `stateful_conversation` | `PASSTHROUGH` | — |
| `realtime_session` | `PASSTHROUGH` | — |
| `realtime_image_input` | `REJECT` | OpenAI Realtime 未提供 input_image_buffer 事件 |
| `realtime_commit_modes` | `REJECT` | server_commit / commit 是 Qwen-TTS-Realtime 特有的提交模式，OpenAI 无对应概念 |
| `realtime_server_vad` | `PASSTHROUGH` | — |
| `realtime_interrupt_turns` | `PASSTHROUGH` | — |
| `web_search` | `REJECT` | Realtime 会话不支持内建 web_search 工具 |
| `computer_use` | `REJECT` | computer use 的工具 schema 各家不兼容，不在 Phase 1 范围 |

## `openai.responses` → `anthropic.messages`

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `PASSTHROUGH` | — |
| `structured_output` | `DEGRADE` | Anthropic 无 strict json_schema 校验，schema 降级为提示词约束，模型可能返回不合规 JSON |
| `reasoning` | `PASSTHROUGH` | — |
| `reasoning_signature` | `REJECT` | Anthropic thinking 签名经 Canonical 转换后失效，带失效签名的多轮 tool use 会被上游拒绝，因此在异构路径上直接拒绝而非静默丢弃 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `REJECT` | Anthropic Messages 不接受音频输入 |
| `video_input` | `REJECT` | Anthropic Messages 不接受视频输入 |
| `file_input` | `REJECT` | 文件引用绑定具体 Provider，跨 Provider 不可迁移；网关不代下载再上传（原则 2.6），请改用 URL 或内联字节 |
| `audio_output` | `REJECT` | Anthropic 不产生音频输出 |
| `image_generation` | `REJECT` | Anthropic 不提供图像/视频生成 |
| `video_generation` | `REJECT` | Anthropic 不提供图像/视频生成 |
| `speech_synthesis` | `REJECT` | Anthropic 不提供语音能力 |
| `speech_recognition` | `REJECT` | Anthropic 不提供语音能力 |
| `embedding` | `REJECT` | Anthropic 不提供 embedding / rerank |
| `rerank` | `REJECT` | Anthropic 不提供 embedding / rerank |
| `prompt_cache` | `DEGRADE` | OpenAI 自动前缀缓存与 Anthropic 显式 cache_control 断点语义互斥，缓存意图被丢弃（请求仍然有效，但不会命中缓存） |
| `stateful_conversation` | `REJECT` | Phase 1 以无状态模式运行（store=false），previous_response_id 不受支持；ConversationStore 接口已预留，Phase 2 接入 |
| `realtime_session` | `REJECT` | Anthropic 无 Realtime API |
| `realtime_image_input` | `REJECT` | Anthropic 无 Realtime API |
| `realtime_commit_modes` | `REJECT` | Anthropic 无 Realtime API |
| `realtime_server_vad` | `REJECT` | Anthropic 无 Realtime API |
| `realtime_interrupt_turns` | `REJECT` | Anthropic 无 Realtime API |
| `web_search` | `REJECT` | OpenAI 与 Anthropic 的内建 web_search 工具参数不兼容，Phase 1 不做映射 |
| `computer_use` | `REJECT` | computer use 的工具 schema 各家不兼容，不在 Phase 1 范围 |

## `openai.responses` → `dashscope.compatible`

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `PASSTHROUGH` | — |
| `structured_output` | `DEGRADE` | DashScope 兼容模式支持 json_object，但不保证 strict json_schema 校验 |
| `reasoning` | `PASSTHROUGH` | — |
| `reasoning_signature` | `REJECT` | Anthropic thinking 签名经 Canonical 转换后失效，带失效签名的多轮 tool use 会被上游拒绝，因此在异构路径上直接拒绝而非静默丢弃 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `PASSTHROUGH` | — |
| `video_input` | `DEGRADE` | 兼容模式接受 video_url，但帧采样率与时长上限无 OpenAI 对应字段，采用上游默认值 |
| `file_input` | `REJECT` | 文件引用绑定具体 Provider，跨 Provider 不可迁移；网关不代下载再上传（原则 2.6），请改用 URL 或内联字节 |
| `audio_output` | `REJECT` | 兼容模式不返回音频；音频输出须经 Qwen-Omni Realtime 或 Native 端点 |
| `image_generation` | `REJECT` | 兼容模式的 chat 端点不承载该能力，须走 DashScope Native |
| `video_generation` | `REJECT` | 兼容模式的 chat 端点不承载该能力，须走 DashScope Native |
| `speech_synthesis` | `REJECT` | 兼容模式的 chat 端点不承载该能力，须走 DashScope Native |
| `speech_recognition` | `REJECT` | 兼容模式的 chat 端点不承载该能力，须走 DashScope Native |
| `embedding` | `REJECT` | 兼容模式的 chat 端点不承载该能力，须走 DashScope Native |
| `rerank` | `REJECT` | 兼容模式的 chat 端点不承载该能力，须走 DashScope Native |
| `prompt_cache` | `DEGRADE` | DashScope 兼容模式的缓存由上游自动管理，显式缓存意图被丢弃 |
| `stateful_conversation` | `REJECT` | Phase 1 以无状态模式运行（store=false），previous_response_id 不受支持；ConversationStore 接口已预留，Phase 2 接入 |
| `realtime_session` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_image_input` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_commit_modes` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_server_vad` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_interrupt_turns` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `web_search` | `DEGRADE` | DashScope 的 enable_search 是布尔开关，承载不了 OpenAI web_search 工具的参数，仅开关本身被映射 |
| `computer_use` | `REJECT` | computer use 的工具 schema 各家不兼容，不在 Phase 1 范围 |

## `openai.responses` → `dashscope.native`

| 能力 | 处置 | 说明 |
|---|---|---|
| `text_generation` | `PASSTHROUGH` | — |
| `streaming` | `PASSTHROUGH` | — |
| `tool_calling` | `PASSTHROUGH` | — |
| `parallel_tool_calls` | `DEGRADE` | DashScope Native 的并行工具调用行为由上游模型决定，无显式开关可映射 |
| `structured_output` | `DEGRADE` | DashScope Native 支持 response_format=json_object，无 strict schema 校验 |
| `reasoning` | `PASSTHROUGH` | — |
| `reasoning_signature` | `REJECT` | Anthropic thinking 签名经 Canonical 转换后失效，带失效签名的多轮 tool use 会被上游拒绝，因此在异构路径上直接拒绝而非静默丢弃 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `PASSTHROUGH` | — |
| `video_input` | `PASSTHROUGH` | — |
| `file_input` | `REJECT` | 文件引用绑定具体 Provider，跨 Provider 不可迁移；网关不代下载再上传（原则 2.6），请改用 URL 或内联字节 |
| `audio_output` | `REJECT` | 音频输出需要 Qwen-Omni 的输出格式参数，Chat Completions 入站无法表达 |
| `image_generation` | `REJECT` | 图像/视频生成是异步任务，须经 /v1/jobs 端点 |
| `video_generation` | `REJECT` | 图像/视频生成是异步任务，须经 /v1/jobs 端点 |
| `speech_synthesis` | `REJECT` | 语音须经 /v1/audio 入站端点 |
| `speech_recognition` | `REJECT` | 语音须经 /v1/audio 入站端点 |
| `embedding` | `REJECT` | embedding / rerank 须经各自的独立入站端点 |
| `rerank` | `REJECT` | embedding / rerank 须经各自的独立入站端点 |
| `prompt_cache` | `DEGRADE` | DashScope Native 的缓存由上游自动管理，显式缓存意图被丢弃 |
| `stateful_conversation` | `REJECT` | Phase 1 以无状态模式运行（store=false），previous_response_id 不受支持；ConversationStore 接口已预留，Phase 2 接入 |
| `realtime_session` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_image_input` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_commit_modes` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_server_vad` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_interrupt_turns` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `web_search` | `DEGRADE` | DashScope 的 enable_search 是布尔开关，承载不了 OpenAI web_search 工具的参数 |
| `computer_use` | `REJECT` | computer use 的工具 schema 各家不兼容，不在 Phase 1 范围 |

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
| `reasoning_signature` | `REJECT` | OpenAI Chat Completions 不产生带签名的推理块；出现签名说明数据来自其他协议 |
| `vision_input` | `PASSTHROUGH` | — |
| `audio_input` | `PASSTHROUGH` | — |
| `video_input` | `REJECT` | OpenAI Chat Completions 不接受视频输入 |
| `file_input` | `PASSTHROUGH` | — |
| `audio_output` | `PASSTHROUGH` | — |
| `image_generation` | `REJECT` | Responses 的内建 image_generation 工具在 Phase 1 不做路由，请改用 /v1/jobs 端点 |
| `video_generation` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `speech_synthesis` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `speech_recognition` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `embedding` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `rerank` | `REJECT` | 该能力不在这条端点上，须经对应的专用入站端点 |
| `prompt_cache` | `PASSTHROUGH` | — |
| `stateful_conversation` | `REJECT` | Phase 1 以无状态模式运行（store=false），previous_response_id 不受支持；ConversationStore 接口已预留，Phase 2 接入 |
| `realtime_session` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_image_input` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_commit_modes` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_server_vad` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `realtime_interrupt_turns` | `REJECT` | Realtime 能力仅在 WebSocket 会话中可用，HTTP 端点无法表达 |
| `web_search` | `PASSTHROUGH` | — |
| `computer_use` | `REJECT` | computer use 的工具 schema 各家不兼容，不在 Phase 1 范围 |

