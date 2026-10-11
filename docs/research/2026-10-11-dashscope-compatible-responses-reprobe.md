# DashScope 兼容层 Responses 端点复测摘要（2026-10-11）

- **目的**：为 `openai.responses → dashscope.compatible` 的 spec 提供逐字段实测
  依据，并复核 `2026-09-27-dashscope-compatible-responses-contract.md` 第 9 节的
  未决项。
- **端点**：`https://dashscope.aliyuncs.com/compatible-mode/v1/responses`（北京
  旧域名），单把 key。
- **模型**：`qwen3.8-flash`、`qwen3.8-max`；部分项以 `qwen3.5-flash` 对照。
- **方法**：只读探测脚本直连上游，凭据仅从环境变量读取，不落盘、不打印。脚本与
  原始响应体未入库（留在当次会话的临时目录），本文件是它们的摘要。
- **效力**：以下全部是【事实】——当日观察到的上游响应。它们**不是 fixture**，
  不能用来兑现矩阵格子；兑现用的录制在实施时重做。上游行为会漂移（见第 3 节）。

两模型结果一致，例外处单独标出。

## 1. 会话（06:02 UTC）

| 步骤 | 请求 | 结果 |
|---|---|---|
| 首轮，不传 `store` | 告知口令 `MANGO42` | 200，回显 `store: true` |
| 续轮，只传 `previous_response_id` | 问口令 | 200，答出 `MANGO42`；`input_tokens` 132 / 123 |
| 对照，不带 id | 问口令 | 200，答不出；`input_tokens` 103 / 94 |
| 首轮 `store: false` 后续轮 | 带该轮 id | 400 `InvalidParameter`：`Not found previous_response_id: resp_...` |
| `GET` 已存储 id | — | 200 |
| `GET` 未存储 id | — | 400 `Response with id '...' not found.` |

结论：上游持久化会话；省略 `store` 即默认存储；`store: false` 真的不存。

## 2. 并行工具调用（06:02 UTC）

提示词要求查 3 个城市天气加 1 个城市时间，两个 function 工具，每取值两次。

| `parallel_tool_calls` | function_call 数 | 回显 |
|---|---|---|
| `true` | 4 | `true` |
| 省略 | 4 | `true` |
| `false` | 1 | `false` |
| `"NOT_A_BOOL"` | — | 400 `Invalid type for 'parallel_tool_calls': expected a boolean.` |

`qwen3.5-flash` 对照（各一次，提示词只要 3 个调用）：`true` → 3 个，`false` → 1 个；
省略时的默认值未测。

## 3. 与 9 月 27 日记录的冲突

9 月 27 日探测记录：`qwen3.5-flash` 上 `parallel_tool_calls: "NOT_A_BOOL"` 返回
200 并原样回显。10 月 11 日同一模型返回 400。原因不明（上游变更或当时观察有误，
无法区分）。以较新的实测为准。

## 4. 逐字段复测（09:10 UTC）

| 字段 | 观察 |
|---|---|
| `text.format.type` 编造值 | 400 `Invalid 'text.format.type': BOGUS_FMT. Supported values are: ('text', 'json_object', 'json_schema').` |
| `text.format` = `json_schema` + `strict` | 200，输出是散文而非 JSON；回显里 `schema` 被改名为 `schema_` |
| `text.format` = `json_object` | max 输出合法 JSON；flash 输出带 ```` ```json ```` 围栏，不能直接解析 |
| 图片 `detail`（low / high / 省略 / 编造值） | `input_tokens` 全相同（flash 1118，max 1080）；编造值不报错 |
| `reasoning.summary`（编造值 / `detailed` / 省略） | 不校验；reasoning 项恒带 1 段 summary |
| `metadata` | 创建时回显；`GET` 取回 `{}`；类型违例不报错 |
| `tools[].strict` | 不校验（`"BOGUS"` → 200 并回显） |
| `tool_choice: "required"` + 两个工具 | 400 `<400> InternalError.Algo.InvalidParameter: Please set one tool in required mode.` |
| 内建 `web_search` | 200，输出含 `web_search_call` |
| 内建 `web_search_preview` | 200，但没有发生搜索 |
| `input` 回传 reasoning 项 | 200，续问答对（`18`） |

## 5. 错误信封

扁平形状：`{"request_id": "...", "code": "InvalidParameter", "message": "..."}`，
没有 OpenAI 的外层 `error` 对象。

## 6. 流式事件面

观察到的事件名：`response.created`、`response.in_progress`、
`response.output_item.added`、`response.reasoning_text.delta`、
`response.reasoning_text.done`、`response.output_item.done`、
`response.content_part.added`、`response.output_text.delta`、
`response.output_text.done`、`response.content_part.done`、
`response.function_call_arguments.delta`、`response.function_call_arguments.done`、
`response.completed`。

`data` 里出现的键包括 `sequence_number`、`item_id`、`output_index`、
`content_index`、`logprobs`。

## 7. 未验证

- 其他地域、专属域名、国际站。
- 跨 key 的 id 归属：只有一把 key，两把 key 交叉续轮没有测。
- 搜索工具的附带选项是否生效。
- 音频输入（上游文档限定 omni-flash，且要求带 MIME 前缀的 Data URI）。
- `qwen3.5-flash` 省略 `parallel_tool_calls` 时的默认值。
- `incomplete` / `failed` 终止事件与流内错误事件的形状（实测只见到
  `response.completed`）。

## 8. 2026-10-12 录制补充

2026-10-12 经网关录制 fixture 时的观察（模型 qwen3.8-flash，北京域名；
`testdata/routes/openai.responses__dashscope.compatible/`）。

1. **思考模式下对象形态的 `tool_choice` 被拒（实测）。** 录制
   `tool_choice: {"type":"web_search_preview"}`（经网关改写为 `web_search`）时上游
   返回 400，原话 `The tool_choice parameter does not support being set to required or object in thinking mode`。
   qwen3.8-flash 默认思考，所以这与工具名无关。因此 `web_search-preview` 用例
   最终不带 `tool_choice` 录制。
2. **`tool_choice` 改名没有真实上游证据（未证实）。** 网关把 `tool_choice` 中的
   `web_search_preview` 同步改名为 `web_search`，保留它是因为不改名必然指向一个
   不存在的工具；上游在非思考模式或其他模型上是否接受
   `tool_choice: {"type":"web_search"}` 未证实。
3. **上游输出项原样回传可行（实测）。** `reasoning-replay` 与 `tool_calling-result`
   两份录制经网关严格解码器回放通过（reasoning 项；function_call 项加
   function_call_output）。
4. **其余结果与 2026-10-11 一致（实测）。** 并行 `true` → 4 个调用、`false` → 1 个；
   `vision_input` 与 `image_detail` 的 `input_tokens` 相同，均为 158；`json_schema`
   返回自由文本；续轮答出口令；`store:false` 后续轮 400；省略 `store` 回显
   `store: true`。

第 7 节的未验证项没有因本次录制被验证：`incomplete` / `failed` 终止事件与流内错误
事件的形状仍只见到 `response.completed`，其余各项亦同。
