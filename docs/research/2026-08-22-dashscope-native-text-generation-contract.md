# DashScope Native HTTP 文本生成契约调研

- **日期**：2026-08-22（所有官方页面均于当日抓取核对）
- **目的**：为 `openai.chat → dashscope.native` 异构适配器设计提供线格式事实依据，杜绝凭猜测做字段映射。本文件只记录官方第一手文档里写了什么、哪里没写。
- **范围**：DashScope Native HTTP 文本生成接口（`/api/v1/services/aigc/text-generation/generation`）的请求/响应契约。多模态端点只在影响文本路径的地方提及。
- **合规**：仅使用阿里云第一手文档（help.aliyun.com 中国站与 alibabacloud.com 国际站）。未读取任何被 `docs/provenance.yaml` 排除的仓库源码。

## 来源清单

| 编号 | 文档 | URL |
|---|---|---|
| [S1] | DashScope API 参考（中国站，中文） | https://help.aliyun.com/zh/model-studio/qwen-api-via-dashscope |
| [S2] | DashScope API Reference（国际站，英文，页面标注 Last Updated: Jul 14, 2026） | https://www.alibabacloud.com/help/en/model-studio/qwen-api-via-dashscope |
| [S3] | 流式输出 | https://help.aliyun.com/zh/model-studio/stream |
| [S4] | 错误码 | https://help.aliyun.com/zh/model-studio/error-code |
| [S5] | 结构化输出 | https://help.aliyun.com/zh/model-studio/qwen-structured-output |
| [S6] | 文本生成模型 API 参考（入口页，列出四种接口） | https://help.aliyun.com/zh/model-studio/qwen-api-reference |
| [S7] | 获取与配置 API Key | https://help.aliyun.com/zh/model-studio/get-api-key |

[S6] 明确百炼文本生成有四套接口：OpenAI 兼容 Chat Completions、OpenAI 兼容 Responses、Anthropic 兼容 Messages、DashScope 原生；其中 DashScope 是「百炼原生接口，提供最完整的功能集和参数支持」。

## 1. 端点与 Base URL

### 1.1 路径

纯文本模型与多模态模型走**两个不同的路径**，二者不可混用 [S1][S2]：

| 模型类别 | 路径 |
|---|---|
| 纯文本模型（如 qwen-plus） | `POST {base}/api/v1/services/aigc/text-generation/generation` |
| 多模态模型（如 qwen3.7-plus、qwen3-vl-plus、qwen3.8-max） | `POST {base}/api/v1/services/aigc/multimodal-generation/generation` |

模型类别与端点不匹配时返回 `url error, please check url！`（见 §10 错误契约）[S4]。对适配器的直接影响：OpenAI Chat 请求里带图像内容时，出站必须切到 multimodal-generation 端点，且 content 必须改为数组形式（§3.2）。

### 1.2 地域域名（中国站文档 [S1]）

| 地域 | 业务空间专属域名（推荐） | 旧域名（仍可正常使用） |
|---|---|---|
| 华北2（北京） | `https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com` | `https://dashscope.aliyuncs.com` |
| 新加坡 | `https://{WorkspaceId}.ap-southeast-1.maas.aliyuncs.com` | `https://dashscope-intl.aliyuncs.com` |
| 美国（弗吉尼亚） | `https://{WorkspaceId}.us-east-1.maas.aliyuncs.com` | （[S1] 未列旧域名） |
| 德国（法兰克福） | `https://{WorkspaceId}.eu-central-1.maas.aliyuncs.com` | （[S1] 未列旧域名） |
| 日本（东京） | `https://{WorkspaceId}.ap-northeast-1.maas.aliyuncs.com` | （[S1] 未列旧域名） |

`{WorkspaceId}` 为业务空间 ID，在百炼控制台业务空间详情页查看 [S1]。官方声明为北京、新加坡地域推出了业务空间专属域名，「能够为推理请求提供卓越的性能和更高的稳定性」，建议迁移；**现有域名仍可正常使用** [S1]。

### 1.3 地域域名（国际站文档 [S2]）

国际站页面列出六个地域，与中国站页面**不完全一致**：

| 地域 | 域名 |
|---|---|
| 新加坡 | `https://{WorkspaceId}.ap-southeast-1.maas.aliyuncs.com`（旧：`https://dashscope-intl.aliyuncs.com`） |
| 美国（弗吉尼亚） | `https://dashscope-us.aliyuncs.com`（[S2] 只给了这个域名，未给 workspace 形式） |
| 华北2（北京） | `https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com`（旧：`https://dashscope.aliyuncs.com`） |
| 中国香港 | `https://{WorkspaceId}.cn-hongkong.maas.aliyuncs.com`（旧：`https://cn-hongkong.dashscope.aliyuncs.com`） |
| 德国（法兰克福） | `https://{WorkspaceId}.eu-central-1.maas.aliyuncs.com` |
| 日本（东京） | `https://{WorkspaceId}.ap-northeast-1.maas.aliyuncs.com` |

两站差异（均为官方文档原文，非推断）：

- 中国香港地域只出现在国际站页面 [S2]，中国站页面 [S1] 未列。
- 美国地域：中国站给的是 workspace 域名 `{WorkspaceId}.us-east-1.maas.aliyuncs.com` [S1]，国际站给的是 `dashscope-us.aliyuncs.com` [S2]。两者都出现在官方文档里，配置层最好都接受。
- 旧域名迁移声明：国际站说专属域名覆盖「北京、新加坡、中国香港」[S2]，中国站说覆盖「北京、新加坡」[S1]。

### 1.4 Base URL 结构

SDK 的 `base_http_api_url` 形如 `https://{host}/api/v1`，完整请求地址在其后拼 `services/aigc/text-generation/generation` [S1][S2]。网关配置里把 base 与路径分开建模是安全的。

## 2. 鉴权与请求头

### 2.1 必需头

所有官方 curl 示例都只带两个必需头 [S1][S3]：

```
Authorization: Bearer $DASHSCOPE_API_KEY
Content-Type: application/json
```

- API Key 在控制台创建，绑定归属账号与归属业务空间；单个业务空间最多 20 个 Key [S7]。
- Key 以 `sk-` 开头（错误码页在排查 401 时明确「阿里云百炼的 API Key 以 `sk-` 开头」）[S4]。
- **API Key 按地域隔离**：「各地域的API Key不同」[S3]；Key 与 Base URL 地域不匹配会返回 401 InvalidApiKey [S4]。对凭据池的含义：同一把 Key 不能同时用于北京与新加坡端点。
- 套餐专属 Key（`sk-sp-` 开头，Coding Plan / Token Plan）必须配套专属 Base URL，混用报 401 [S4]。

### 2.2 流式开关头（关键差异点）

HTTP 调用开启流式**不靠请求体字段，而靠请求头** [S1][S3]：

```
X-DashScope-SSE: enable
```

请求体里的 `stream` 参数**仅 Python SDK 支持**，HTTP 调用必须用上面的头 [S1]。这是与 OpenAI Chat 最显眼的线格式差异之一：OpenAI 的 `"stream": true` 在 DashScope Native 里没有对应的 body 字段。

### 2.3 其他文档化的请求头

| 头 | 作用 | 出处 |
|---|---|---|
| `X-DashScope-DataInspection: {"input":"cip","output":"cip"}` | 在基础内容安全之上进一步识别输入输出违规信息；不设置则不进一步识别 | [S1] |
| `X-DashScope-Async` | 异步调用开关。文本生成同步接口设置 `enable` 会报 403「Current user api does not support asynchronous calls」，移除或设为 `disable` 即可 | [S4] |
| `X-DashScope-OssResourceResolve: enable` | 通过 HTTP 传入临时 URL（OSS 签名 URL）时必须添加 | [S4] |

## 3. 请求信封

### 3.1 顶层结构

```json
{
  "model": "qwen-plus",
  "input": {
    "messages": [ ... ]
  },
  "parameters": { ... }
}
```

- `model` string，必选。模型名称是百炼模型 ID（如 `qwen3-235b-a22b-instruct-2507`），不是开源社区名（`Qwen/...` 会报 `Model not exist.`）[S1][S4]。
- `input.messages` array，必选。**通过 HTTP 调用时 messages 必须放入 `input` 对象**；放错位置（与 `model` 并列）会报 `Either "prompt" or "messages" must exist and cannot both be none` [S1][S4]。`input.prompt` 字段仍存在但「即将废弃」，官方建议只用 messages [S4]。
- `parameters` object，可选。全部采样/功能参数的容器；每个参数的说明里都反复强调「通过HTTP调用时，请将 xxx 放入 parameters 对象中」[S1]。

### 3.2 消息内容形态

四种角色：`system`、`user`、`assistant`、`tool` [S1]。

**System Message**（可选，一般放数组首位）：`content` 必为 string，`role` 固定 `system`。QwQ 不建议设置、QVQ 设置不生效 [S1]。

**User Message**（必选）：`content` 为 string 或 array [S1]。

- 纯文本输入：string。纯文本模型**只接受 string**，传数组会报 `input content must be a string.` [S4]。
- 多模态或显式缓存：array，元素是**按键名区分模态的对象**（注意：不是 OpenAI 的 `{"type": ...}` 风格）：
  - `{"text": "..."}`
  - `{"image": "公网URL | data:image/<format>;base64,<data> | 本地绝对路径"}`（Qwen-VL、QVQ）
  - `{"video": "视频文件URL"}` 或 `{"video": ["帧图URL", ...]}`，可配 `fps`（[0.1,10]，默认 2.0）、`max_frames`、`min_pixels`、`max_pixels`、`total_pixels`
  - `{"audio": "音频URL"}`（qwen-audio-turbo 等音频理解模型必选）
  - `cache_control: {"type": "ephemeral"}` 开启显式缓存（仅支持的模型）
  - 以上取值范围与模型相关的细节见 [S1]。
- 多模态 content 数组元素的键必须是 `image`/`video`/`audio`/`text` 之一，否则报 `The item of content should be a message of a certain modal.` [S4]。

**Assistant Message**（可选）：`role` 固定 `assistant`；`content` string，当消息携带 `tool_calls` 时 content 非必选 [S1]。另有：

- `partial` boolean（可选）：前缀续写开关 [S1]。
- `tool_calls` array（可选）：上一轮模型返回的工具调用信息，元素为 `{id, type: "function", function: {name, arguments(JSON字符串)}, index}` [S1]。

**Tool Message**（可选）：`role` 固定 `tool`；`content` 必选且**必须为字符串**；`tool_call_id` 在 [S1] 中标注为可选，用于标记该消息对应哪个工具调用。

消息序列的硬约束（由错误码反推的契约）[S4]：

- `tool` 消息必须跟在带 `tool_calls` 的 assistant 消息之后（`messages with role "tool" must be a response to a preceeding message with "tool_calls"`）。
- 带 `tool_calls` 的 assistant 消息之后，每个 `tool_call_id` 必须有且仅有一条 tool 消息响应，才能再发下一条 user 消息。
- messages 不能是空数组；最后一条消息需为 user 消息（`list index out of range` 条目）；必须包含 user 角色消息。

## 4. 工具调用（Function Calling）

- `parameters.tools`：数组，元素为 `{type: "function", function: {name, description, parameters}}`；`function.parameters` 是合法 JSON Schema，缺省 `{}` 表示无入参。name 限字母数字下划线短划线、最长 64 [S1]。
- **使用 tools 时必须 `result_format: "message"`**；发起调用与回传工具结果两轮都必须带 tools 参数 [S1]。
- `parameters.tool_choice`：`auto`（默认）/ `none` / `{"type":"function","function":{"name": ...}}` 强制调用。思考模式模型不支持强制调用某个工具 [S1]。取值错误报 `tool_choice is one of the strings that should be ["none", "auto"]` [S4]。
- `parameters.parallel_tool_calls`：boolean，默认 false [S1]。
- `parameters.tool_stream`：boolean，默认 false。只影响「复杂工具」（参数含 array/object 类型）的流式输出行为：false 时参数一次性输出（格式更准），true 时流式输出（无超时风险）。仅 Qwen 与 GLM 部分系列支持 [S1]。
- qwen-vl 与 qwen-audio 系列暂不支持 tools 参数 [S1]；工具名不允许为 `search` [S4]。
- 响应侧的 `tool_calls` 形状见 §7.2。

## 5. 采样控制

以下全部位于 `parameters` 对象，取值范围与默认值均引自 [S1]（默认值按模型差异极大，此处只记范围与少数代表值，完整清单见原文）：

| 参数 | 类型 | 范围 | 备注 |
|---|---|---|---|
| `temperature` | float | [0, 2) | 各模型默认值 0 ~ 1.0 不等（如多数 Qwen3 非思考 0.7、思考 0.6、qwen-math 0） |
| `top_p` | float | (0, 1.0] | 常见默认 0.8 / 0.95 |
| `top_k` | integer | ≥ 0 | None 或 >100 表示不启用，仅 top_p 生效；DeepSeek/Kimi/MiniMax 不支持 |
| `repetition_penalty` | float | > 0，无上限 | 1.0 表示不惩罚 |
| `presence_penalty` | float | [-2.0, 2.0] | Java SDK 不支持该参数 |
| `seed` | integer | 见下方矛盾说明 | 相同 seed + 相同参数「尽可能」复现结果 |
| `n` | integer | 1 ~ 4 | 当前仅 Qwen3（非思考）与 qwen-plus-character 支持；传 tools 时固定为 1 |
| `stop` | string 或 array | — | 支持 `str`、`list[str]`、`list[int]`、`list[list[int]]`；数组内不可混用 token_id 与字符串 |
| `max_tokens` | integer | [1, 模型最大输出] | **即将废弃**，新接入用 `max_completion_tokens`；超限时 `finish_reason` 为 `length` |
| `max_completion_tokens` | integer | — | 限制完整输出（思维链 + 回答）；与模型最大输出同默认/上限；实际输出与设定值最多差 10 个 Token；仅部分较新模型支持（清单见 [S1]） |
| `logprobs` | boolean | — | 默认 false；仅 qwen-plus/turbo 快照版、qwen3-vl-plus/flash、Qwen3 开源等支持 |
| `top_logprobs` | integer | [0, 5] | 仅 logprobs=true 时生效 |

**seed 范围存在官方文档自相矛盾**，设计时按宽口径处理并显式记录：

- [S1] 参数表写「取值范围：[0, 2^31−1]」；
- [S4] 错误码页写「使用DashScope协议时，seed 参数设置未在 [0, 9223372036854775807] 的范围内」报 400，而 OpenAI 兼容协议的 seed 越界条目单独给的是 [0, 2^31−1]。

即运行时校验对 DashScope 协议按 int64 上限执行，[S1] 的 2^31−1 表述与之冲突。本调研无法裁定哪个是最终事实，标记为「文档矛盾，按 [0, 2^63−1] 接受、超限报错可预期」。

思考相关参数（协议字段，行为与模型强相关）[S1]：

| 参数 | 说明 |
|---|---|
| `enable_thinking` | 混合思考模型的思考开关；开启后思考内容经 `reasoning_content` 返回。部分模型不可设为 false（如 qwen3-235b-a22b-thinking-2507，报错 `The value of the enable_thinking parameter is restricted to True.` [S4]） |
| `preserve_thinking` | 是否把历史 assistant 的 `reasoning_content` 拼入输入，默认 false，qwen3.8-max 默认 true 且**必须完整回传历史全部 reasoning_content**，不支持拼进 content 回传；开启后计入输入 Token 计费 |
| `thinking_budget` | 思考过程最大长度；qwen3.8-max 不支持与 `reasoning_effort` 同时设置，两者有文档化的互映规则（low=4096 / medium=16384 / xhigh=262144） |
| `reasoning_effort` | 推理力度档位，可选值与映射按模型不同（qwen3.8-max：xhigh/medium/low，max、high 映射为 xhigh，minimal 映射 low，none 映射为 enable_thinking=false） |
| `clear_thinking` | 仅 GLM glm-5.2/5.1/5/4.7：是否丢弃历史轮次的 reasoning_content |

思考模式的三条硬约束（错误码反推）[S4]：

1. 非流式调用不允许开思考：`parameter.enable_thinking must be set to false for non-streaming calls`；
2. 开思考必须 `incremental_output: true`：`The incremental_output parameter must be "true" when enable_thinking is true`；
3. 开思考必须 `result_format: "message"`：`The result_format parameter must be "message" when enable_thinking is true`。

其他功能参数：`enable_code_interpreter`（boolean，默认 false）[S1]；`skill`（仅 qwen-doc-turbo 的 PPT 生成，且必须 stream=true）[S1]；`vl_high_resolution_images`、`vl_enable_image_hw_output`（视觉专用）[S1]。

## 6. 联网搜索控制

均位于 `parameters`，仅当 `enable_search: true` 时 `search_options` 生效 [S1]：

| 参数 | 默认 | 说明 |
|---|---|---|
| `enable_search` | false | 模型「会基于其内部逻辑判断是否使用」搜索结果；未触发时可开 `forced_search` |
| `search_options.enable_source` | false | 响应中是否展示搜索来源（对应响应 `output.search_info`） |
| `search_options.enable_citation` | false | `[1]`/`[ref_1]` 角标，需 enable_source=true |
| `search_options.citation_format` | `[<number>]` | 或 `[ref_<number>]` |
| `search_options.forced_search` | false | 强制搜索 |
| `search_options.search_strategy` | `turbo` | `turbo` / `max` / `agent` / `agent_max`；后两者仅部分模型（agent 对 qwen3-max 思考模式仅支持流式），且启用时仅支持 enable_source，其余搜索功能不可用 |
| `search_options.enable_search_extension` | false | 特定领域增强（响应 `extra_tool_info`） |
| `search_options.prepend_search_result` | false | 流式且 enable_source=true 时，第一个数据包是否只含搜索来源 |

响应侧对应 `output.search_info.search_results[]`，元素含 `site_name / icon / index / title / url` [S1]。模型不支持联网搜索时传 `enable_search: true` 报 `This model does not support enable_search.` [S4]。

## 7. 响应

### 7.1 非流式响应：裸 HTTP 体 vs SDK 包装字段

官方给出的「chat响应对象」示例（标注流式与非流式格式一致）[S1]：

```json
{
  "status_code": 200,
  "request_id": "902fee3b-f7f0-9a8c-96a1-6b4ea25af114",
  "code": "",
  "message": "",
  "output": {
    "text": null,
    "finish_reason": null,
    "choices": [
      {
        "finish_reason": "stop",
        "message": {
          "role": "assistant",
          "content": "我是阿里云开发的一款超大规模语言模型，我叫千问。"
        }
      }
    ]
  },
  "usage": { "input_tokens": 22, "output_tokens": 17, "total_tokens": 39 }
}
```

**裸 HTTP 与 SDK 包装字段的区分（本调研的关键解读）**：

| 字段 | 归属 | 依据 |
|---|---|---|
| `output`、`usage`、`request_id` | **裸 HTTP 响应体顶层** | 流式文档里 curl 收到的原始 SSE data 负载只有这三个顶层字段 [S3] |
| `status_code` | **SDK 包装层** | [S1] 注明「Java SDK不会返回该参数。调用失败会抛出异常」 |
| `code`、`message` | **SDK 包装层** | [S1] 注明 code「只有Python SDK返回该参数」；成功时为空值 |

官方没有一句话明说裸 HTTP 非流式体的精确顶层集合，此划分是按上述两条文档证据的推断，适配器实现时应以真实抓包复核（标记为存疑，见 §11）。

输出两种形态由请求侧 `result_format` 决定 [S1]：

- `result_format: "text"`：回复在 `output.text`，`output.finish_reason` 非空。
- `result_format: "message"`（推荐，多轮对话必需）：回复在 `output.choices[]`。

`result_format` 的默认值与强制项 [S1]：

- 默认 `text`；Qwen3-Max、Qwen3-VL、QwQ、Qwen3 开源（除 qwen3-next-80b-a3b-instruct）、Qwen-Long 默认 `message`；
- Qwen3-Max、Qwen3-VL、思考模式下的 Qwen3 **只能**设为 `message`；
- 千问 VL/QVQ/Audio 模型设 `text` 不生效；
- 官方声明「平台后续将统一调整默认值为message」。

### 7.2 choices[].message 字段

引自 [S1]：

- `role`：固定 `assistant`。
- `content`：string；qwen-vl / qwen-audio 系列为 array（`text` 键，可选 `image_hw`）。**发生 Function Calling 时 content 为空**。
- `reasoning_content`：模型深度思考内容（思考模式）。
- `tool_calls[]`：`{function: {name, arguments(JSON字符串)}, index, id, type: "function"}`。官方提醒 arguments 不保证总是合法 JSON，调用函数前应校验 [S1]。
- `logprobs`：`content[]` 内为 `{token, bytes, logprob, top_logprobs[]}`，仅支持的模型 + `logprobs: true`。

### 7.3 finish_reason

文档化的取值恰好四种（`output.finish_reason` 与 `choices[].finish_reason` 同一套）[S1]：

| 值 | 含义 |
|---|---|
| `null` | 正在生成（流式中间包） |
| `stop` | 自然结束或触发 stop 词 |
| `length` | 生成长度超限（max_tokens / max_completion_tokens） |
| `tool_calls` | 发生工具调用 |

⚠️ 流式文档示例里中间包的 `finish_reason` 写作**字符串 `"null"`** 而非 JSON null [S3]。解析器必须同时容忍 `"null"` 字符串与 JSON null，此差异在文档中没有专门说明。

### 7.4 usage 字段

引自 [S1]：

- `input_tokens` / `output_tokens`：输入、输出 Token 数。
- `total_tokens`：**仅当输入为纯文本时返回**，等于两者之和。（多模态输入时不返回 total，需要自加。）
- `image_tokens` / `video_tokens` / `audio_tokens`：输入含对应模态时返回。
- `input_tokens_details`：`{text_tokens, image_tokens, video_tokens}`（可选）。
- `output_tokens_details`：`{text_tokens, reasoning_tokens, audio_tokens}`，仅部分模型返回；`reasoning_tokens` 仅推理模型返回。
- `prompt_tokens_details`：`{cached_tokens}`，上下文缓存命中数。
- `cache_creation`：`{ephemeral_5m_input_tokens, cache_creation_input_tokens}` 与 `cache_type`（显式缓存时 `ephemeral`）。

流式场景下 **每个 chunk 都携带截至当前的累计 usage**（见 §8 示例），最终值以最后一个包为准 [S3]。

### 7.5 request_id

- 格式为 UUID（如 `649b2bbc-c541-9e16-9845-db7fe4fe5b2d`），「包含在 API 响应的 Header 或 Body 中」[S4]。
- 非流式在响应体顶层（§7.1）；流式在**每个** SSE data 负载里 [S3]。
- 官方未文档化承载它的响应头名称（只说 Header 或 Body），标记为未知（§11）。

## 8. 流式输出（SSE）

### 8.1 触发方式

- 请求头 `X-DashScope-SSE: enable` [S1][S3]；body 里的 `stream` 参数仅 Python SDK 生效 [S1]。
- `parameters.incremental_output` 控制增量语义 [S1]：
  - `false`（默认）：每包输出**当前已生成的完整序列**，最后一包是全文；
  - `true`（官方推荐）：增量输出，后续包不含已输出内容；
  - Qwen3-Max、Qwen3-VL、Qwen3 开源、QwQ、QVQ 默认 true；QwQ 与思考模式 Qwen3 只能 true；Qwen3 开源版不支持 false。
- 仅支持流式的模型：Qwen3 商业版（思考模式）、Qwen3 开源版、QwQ、QVQ、Qwen-Omni 等，非流式调用报 `This model only support stream mode...` [S1][S4]。

### 8.2 SSE 帧格式

官方描述：「响应遵循 Server-Sent Events (SSE) 格式，每条消息包含：id（数据块编号）、event（事件类型，固定为 result）、HTTP 状态码信息、data（JSON 数据部分）」[S3]。

官方示例（qwen-plus，incremental_output=true）[S3]：

```
id:1
event:result
:HTTP_STATUS/200
data:{"output":{"choices":[{"message":{"content":"我是","role":"assistant"},"finish_reason":"null"}]},"usage":{"total_tokens":27,"output_tokens":1,"input_tokens":26,"prompt_tokens_details":{"cached_tokens":0}},"request_id":"d30a9914-ac97-9102-b746-ce0cb35e3fa2"}

id:2
event:result
:HTTP_STATUS/200
data:{"output":{"choices":[{"message":{"content":"通义千","role":"assistant"},"finish_reason":"null"}]},"usage":{"total_tokens":30,"output_tokens":4,"input_tokens":26,"prompt_tokens_details":{"cached_tokens":0}},"request_id":"d30a9914-ac97-9102-b746-ce0cb35e3fa2"}

...

id:15
event:result
:HTTP_STATUS/200
data:{"output":{"choices":[{"message":{"content":"","role":"assistant"},"finish_reason":"stop"}]},"usage":{"total_tokens":92,"output_tokens":66,"input_tokens":26,"prompt_tokens_details":{"cached_tokens":0}},"request_id":"d30a9914-ac97-9102-b746-ce0cb35e3fa2"}
```

要点：

- 帧由 `id:`（递增编号）、`event:result`、`:HTTP_STATUS/200`（SSE 注释行，承载状态码）、`data:`（单个 JSON 对象）组成 [S3]。
- data 负载与非流式裸体同构：`output.choices[].message` + `finish_reason`、累计 `usage`、`request_id` [S3]。
- 流结束于 `finish_reason` 非空的最后一帧。**官方示例中没有 `[DONE]` 之类的终止标记**（对比：同页 OpenAI 兼容流式示例以 `data: [DONE]` 收尾 [S3]）。是否存在显式终止帧未文档化，按「最后一帧 + 连接关闭」处理，标记为未知（§11）。
- 流式计费与非流式相同，按输入/输出 Token 计 [S3]。

## 9. JSON / 结构化输出

`parameters.response_format`，默认 `{"type": "text"}` [S1]。官方文档化两种 JSON 模式 [S5]：

| 模式 | 设置 | 提示词要求 | 支持面 |
|---|---|---|---|
| JSON Object | `{"type": "json_object"}` | System/User 消息必须含 "json"（不区分大小写），否则报 `'messages' must contain the word 'json'...` [S4][S5] | 大部分 Qwen 文本模型（按思考/非思考模式区分，清单见 [S5]）及 Kimi/GLM/DeepSeek/Stepfun 部分模型 |
| JSON Schema | `{"type": "json_schema", "json_schema": {..., "strict": true}}` | 无需 json 关键词 | **仅 Qwen3.7-Plus、Qwen3.7-Max、Qwen3.8-Max 系列** [S5] |

注意两点：

1. [S1] 的 response_format 参数表只列了 `text` 与 `json_object` 两个取值；`json_schema` 模式由 [S5] 专门文档化，且 [S4] 的报错条目（`response_format should be a dict, includes 'type' and an optional key 'json_schema'`）佐证服务端认识该键。
2. 思考模式与 JSON 模式冲突：报错条目 `Json mode response is not supported when enable_thinking is true` [S4]；但 [S5] 同时说明「标注为非思考模式的模型，在思考模式下设置 json_object 不会报错，但结构化输出可能失效」，且 [S5] 的 HTTP 示例本身就是 qwen3.8-max + `enable_thinking: true` + `json_object`。即**不同模型对该组合的行为不一致**：有的报错、有的静默降级。适配器不能假设统一行为，标记为模型相关风险。

另：JSON Object 模式「不保证键名与字段类型稳定」，如需固定结构必须用 JSON Schema 模式 [S5]。

## 10. 错误契约

### 10.1 形态

错误码页以「HTTP状态码-错误码」为条目标题组织（如 `400-InvalidParameter`、`429-Throttling.RateQuota`），即契约由 **HTTP 状态码 + 字符串 code + message** 三元组构成 [S4]。响应体中 `code`/`message`/`request_id` 的存在由 SDK 字段说明与流式 data 负载结构佐证（§7.1 的解读），裸 HTTP 错误体的完整 JSON 示例官方未给出，标记为未知（§11）。

### 10.2 与文本生成路径相关的错误码（摘自 [S4]）

| HTTP | code | 典型 message / 触发条件 |
|---|---|---|
| 400 | `InvalidParameter` | 参数越界/格式错误的总汇：temperature ∉ [0,2)、top_p ∉ (0,1]、top_k < 0、presence_penalty ∉ [-2,2]、n ∉ [1,4]、max_tokens 越界、stop 类型错误、思考模式非流式调用、`incremental_output` 未随思考开启、`enable_search` 模型不支持、tool_choice 取值错误、response_format 非法、json_object 缺 "json" 关键词等（逐条见 [S4]） |
| 400 | `InternalError.Algo.InvalidParameter` | 输入超长（`Range of input length should be [1, xxx]`）、content 数组混入非法元素、重复工具调用检测 |
| 400 | `DataInspectionFailed` / `data_inspection_failed` | 输入或输出疑似敏感内容被内容安全拦截 |
| 400 | `Arrearage` | **账号欠费**（`Access denied, please make sure your account is in good standing.`）。注意欠费是 400 而不是 402/403 |
| 400 | `BadRequest.*` | `BadRequest.EmptyInput` / `EmptyParameters` / `EmptyModel` / `IllegalInput` / `TooLarge` 等 |
| 401 | `InvalidApiKey` / `invalid_api_key` | Key 错误、读取了错误的环境变量、套餐专属 Key 与 Base URL 混用、**Key 与端点地域不匹配** |
| 401 | `NOT AUTHORIZED` | WorkspaceId 无效、账号非该业务空间成员、接入地址有误 |
| 403 | `AccessDenied` / `access_denied` | 无权访问模型（未申请、免费额度耗尽不支持付费、模型下线）；套餐专属模型用了通用 Key |
| 403 | `AccessDenied.Unpurchased` | 未开通百炼服务 |
| 403 | `Model.AccessDenied` / `Workspace.AccessDenied` / `Endpoint.AccessDenied` | 子业务空间无模型授权 / 无业务空间权限 / 模型已下线端点停服 |
| 403 | `AllocationQuota.FreeTierOnly` | 免费额度耗尽且开启「用完即停」 |
| 404 | `ModelNotFound` / `model_not_found` | model 参数不存在、大小写/空格错误、混用开源社区模型名 |
| 429 | `Throttling` / `Throttling.RateQuota` | 触发限流（RPS/RPM） |
| 429 | `Throttling.BurstRate` | 调用频率骤增触发稳定性保护 |
| 429 | `Throttling.AllocationQuota` / `insufficient_quota` | TPS/TPM 消耗限流 |
| 429 | `Throttling.Concurrency` | 并发请求数超动态上限 |
| 500 | `InternalError` / `internal_error` | 内部错误 |
| 500 | `InternalError.Algo` | 推理内部错误、模型生成非法 JSON 导致工具调用失败、`list index out of range`（messages 末位非 user） |
| 500 | `RequestTimeOut` | 请求超时，**超时报错时间为 300 秒**；对千问模型「响应体中会将已生成的内容返回，不再报超时错误」 |
| 500/503 | `ModelServingError` | 系统容量饱和 |
| 503 | `ModelUnavailable` | 模型暂不可用 |

### 10.3 对网关的含义

- `Arrearage`（欠费）归在 400：按本仓库错误分类语义，它不是客户端请求写错，也不是「换个凭据就能过」的临时故障，映射时需要单独考虑。
- `Throttling.*` 一族与 `ModelServingError`/`ModelUnavailable` 是「换凭据/稍后重试可能成功」的来源；`InvalidParameter`/`DataInspectionFailed`/`ModelNotFound` 则重试无意义。
- 流式中途失败如何下发（是否有一帧携带 code/message）在抓取的文档中**没有描述**，标记为未知（§11）。

## 11. 未文档化 / 存疑清单

设计时必须为以下各点保留显式决策，不能默认它们有确定行为：

1. **裸 HTTP 非流式响应体的顶层字段集合**：官方示例混入了 SDK 包装字段（`status_code`/`code`/`message` 注明仅部分 SDK 返回）；裸体按 `output/usage/request_id` 推断（§7.1）。
2. **错误响应体的精确 JSON 结构**：[S4] 未给裸 HTTP 示例，只有「状态码-错误码」条目与 SDK 字段。
3. **request_id 的响应头名称**：官方只说「包含在 API 响应的 Header 或 Body 中」[S4]，未给头名。
4. **SSE 响应头**：响应 Content-Type（预期 text/event-stream）与状态码在正常流中经 `:HTTP_STATUS/200` 注释行传递 [S3]，但响应头本身未在文档中列出。
5. **原生 SSE 是否有终止标记**：示例止于 `finish_reason: "stop"` 帧，无 `[DONE]` 等价物 [S3]。
6. **流式中间包 `finish_reason` 为字符串 `"null"`**（非 JSON null）[S3]：是文档示例如此还是服务端恒如此，未说明；解析需两者兼容。
7. **seed 取值范围文档矛盾**：[S1] 给 [0, 2^31−1]，[S4] 给 DashScope 协议 [0, 2^63−1]（§5）。
8. **json_object + enable_thinking 的组合行为**：报错 [S4] 与静默失效 [S5] 两种说法并存，且 [S5] 官方示例同时使用两者（§9）。
9. **流式中途错误的下发格式**：未文档化（§10.3）。
10. **美国（弗吉尼亚）端点**：中国站给 workspace 域名、国际站给 `dashscope-us.aliyuncs.com`（§1.3）；**中国香港**端点仅国际站文档列出。
11. **`input.prompt` 的下线时间**：仅注明「即将废弃」[S4]。
12. **非流式长耗时请求的心跳/超时细节**：仅知 300 秒超时与千问模型超时返回已生成内容 [S4]；`heartbeat` 参数是语音识别场景的，文本生成未文档化。

## 12. 模型专属注意事项（与协议契约分开）

以下是模型行为差异，不属于线格式本身，但适配器做能力声明时要逐模型处理：

- **端点匹配**：qwen3.8-max、qwen3.7-plus、qwen3-vl-plus 等是「多模态模型」，必须走 multimodal-generation 端点且 content 用数组；qwen-plus、qwen3-max 等纯文本模型走 text-generation 端点且 content 必须是字符串。错配报 `url error` 或 `input content must be a string.` [S1][S4]。
- **仅流式模型**：Qwen3 商业版思考模式、Qwen3 开源版、QwQ、QVQ、Qwen-Omni 只能流式调用 [S1][S3][S4]。
- **默认值按模型浮动**：temperature、top_p、top_k、presence_penalty、repetition_penalty、seed、result_format、incremental_output 的默认值都随模型不同（完整清单见 [S1] 各参数条目）。
- **n 的支持面**：仅 Qwen3（非思考）与 qwen-plus-character，传 tools 时固定 1 [S1]。
- **preserve_thinking**：qwen3.8-max 默认开启，要求历史 reasoning_content 全量回传 [S1]。
- **reasoning_effort 档位映射**：按模型有三套不同规则（§5）[S1]。
- **Qwen-Omni**：仅流式，输出可含音频，`'audio' output only support with stream=true` [S4]。
- **tools 支持面**：qwen-vl、qwen-audio 系列不支持 [S1]。

## 附录 A：最小请求/响应样例（均出自官方文档原文）

非流式请求（文本模型，北京地域）[S1]：

```bash
curl -X POST "https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/api/v1/services/aigc/text-generation/generation" \
-H "Authorization: Bearer $DASHSCOPE_API_KEY" \
-H "Content-Type: application/json" \
-d '{
    "model": "qwen-plus",
    "input": {
        "messages": [
            {"role": "system", "content": "You are a helpful assistant."},
            {"role": "user", "content": "你是谁？"}
        ]
    },
    "parameters": {
        "result_format": "message"
    }
}'
```

流式请求（增加 `X-DashScope-SSE: enable` 头与 `incremental_output`）[S3]：

```bash
curl -X POST "https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/api/v1/services/aigc/text-generation/generation" \
-H "Authorization: Bearer $DASHSCOPE_API_KEY" \
-H "Content-Type: application/json" \
-H "X-DashScope-SSE: enable" \
-d '{
    "model": "qwen-plus",
    "input": {
        "messages": [
            {"role": "system", "content": "You are a helpful assistant."},
            {"role": "user", "content": "你是谁？"}
        ]
    },
    "parameters": {
        "result_format": "message",
        "incremental_output": true
    }
}'
```

## 附录 B：与 OpenAI Chat 的关键线格式差异速查

供下游设计比对用，每条都能在上文找到出处：

| 维度 | OpenAI Chat | DashScope Native |
|---|---|---|
| 消息位置 | 顶层 `messages` | `input.messages` [S1] |
| 参数位置 | 顶层 | `parameters` 对象 [S1] |
| 流式开关 | body `stream: true` | 头 `X-DashScope-SSE: enable`（body `stream` 仅 Python SDK）[S1] |
| 增量语义 | 天然增量（delta） | `incremental_output` 开关，默认全量累计 [S1] |
| 多模态 content 元素 | `{"type": "image_url", ...}` | 按键名 `{"image": ...}`/`{"text": ...}` [S1] |
| 流式帧 | `data: {...}` + `data: [DONE]` | `id:`/`event:result`/`:HTTP_STATUS/200`/`data:`，无 [DONE] 记录 [S3] |
| usage 字段名 | prompt_tokens/completion_tokens | input_tokens/output_tokens（流式每包携带累计值）[S1][S3] |
| finish_reason 中间态 | JSON null | 文档示例为字符串 `"null"` [S3] |
| seed 范围 | [0, 2^31−1] | 文档矛盾：[0, 2^31−1] 与 [0, 2^63−1]（§5）[S1][S4] |
| JSON 结构化 | json_object / json_schema | json_object（需提示词含 "json"）/ json_schema（仅 3 个系列）[S5] |
| 联网搜索 | 无对应（Responses 另有 web_search 工具） | `enable_search` + `search_options` [S1] |
