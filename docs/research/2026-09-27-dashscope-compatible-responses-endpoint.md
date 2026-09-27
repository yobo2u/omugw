# DashScope 兼容层 Responses 端点契约调研（Wayfinder 研究工单 #10）

- **日期**：2026-09-27
- **基线**：`main` @ `8d9ebaf`（`GIT_MASTER=1 git rev-parse HEAD` 核对一致）
- **目的**：回答研究工单 #9 遗留的开放问题 Q1——DashScope OpenAI 兼容层是否存在
  真正的 **Responses 线格式端点**，以及它的字段级契约。这个答案决定
  `openai.responses → dashscope.compatible` 一条路径的**实施形态**，进而决定其
  复用比例在 25–35% 与 85–90% 之间取哪一头
  （`docs/research/2026-09-27-responses-to-dashscope-reuse-boundary.md:518-533`）。
  本文件**只确立契约，不提出实施方案**。
- **方法**：阿里云百炼官方文档（help.aliyun.com，第一方）+ 对生产端点的
  **只读存在性与参数校验探测**。探测一律用「畸形但极廉价」的请求体，目的是让上游
  在跑推理**之前**就返回校验错误；少数必须看真实行为的探测（`store` 持久化、
  `text.format` 是否被尊重）用 `reasoning.effort=none` +
  `max_output_tokens≤60` 压到最小 Token 量。
- **凭据纪律**：探测使用环境变量 `$DASHSCOPE_API_KEY`，仅以 shell 变量展开形式
  出现。密钥值未被打印、记录，也未写入本文件任何位置。
- **约定**：每条结论标注【事实】（官方文档原文或命令输出可指认）或【推断】
  （由证据推演，未直接验证）。无法核实的项集中在第 10 节。

---

## 1. FORM VERDICT：**形态 A**

**结论：形态 A —— 上游存在一个真正的 Responses 线格式端点。**

【事实】这个判定同时被文档与实测证实，两者**不冲突**：

1. 官方文档《OpenAI Responses接口兼容》明确记载
   `POST /compatible-mode/v1/responses`，并配有 curl 示例、Responses 线格式的
   请求体（`model` + `input`）与响应体（`object: "response"` + `output` 数组 +
   `status`）。
   来源：<https://help.aliyun.com/zh/model-studio/compatibility-with-openai-responses-api>
2. 官方参数参考页《创建响应》逐字段列出请求体与响应体，位置在文档树
   「API参考（模型）→ 文本生成 → OpenAI兼容-Responses → 创建响应」。
   来源：<https://help.aliyun.com/zh/model-studio/qwen-api-via-openai-responses>
3. 实测：该路径存在、只收 POST、按 Responses 线格式解析请求体、返回
   `object: "response"` 的标准信封（见第 2、6 节的命令输出）。

【事实】对 omugw 的直接后果：**不需要** Canonical→Chat wire 请求编码器，也
**不需要** Chat wire→Responses wire 响应解码器。工单 #9 估算的 550–750 行
全新代码（形态 B 的代价）**不必发生**。

【推断】但形态 A 成立**不等于**现有适配器零改动即可用。第 3 节发现三个
「静默接受但不生效」的参数，第 7 节发现一处流式信封差异，第 4 节发现上游
**是有状态的**——这三项都与矩阵当前的声明不一致。形态 A 防住的是「重写一套
编解码」，它**没有**防住「矩阵声明与上游事实脱节」。后者是 ADR-0001 的红线，
必须在兑现前逐格核对。

---

## 2. 端点、方法与鉴权

【事实】路径与方法（探测输出）：

```
POST /compatible-mode/v1/responses   → 400 InvalidParameter（进入了参数校验，端点存在）
GET  /compatible-mode/v1/responses   → 405 {"code":"Method Not Allowed",
     "message":"Method 'GET' is not allowed for '/compatible-mode/v1/responses'."}
GET  /compatible-mode/v1/responses/{id} → 200，返回完整 response 对象（检索端点存在）
POST /compatible-mode/v1/responses/bogus/sub → 404 {"code":"Not Found","message":"Not support"}
```

【事实】鉴权是 `Authorization: Bearer <API Key>`。缺失时：

```
401 {"code":"InvalidApiKey","message":"No API-key provided.","request_id":"..."}
```

对国际站主机 `dashscope-intl.aliyuncs.com` 用同一把 key 得
`401 InvalidApiKey / "Invalid API-key provided."`——【事实】密钥按地域绑定，
【推断】这意味着 omugw 的凭据池不能假设一把 key 跨地域可用；这不是本工单的
结论，只是探测顺带暴露的事实。

【事实】主机形态有两代。文档推荐迁移到业务空间专属域名
`https://{WorkspaceId}.{region}.maas.aliyuncs.com/compatible-mode/v1`，
列出 6 个地域（北京、新加坡、美国弗吉尼亚、德国法兰克福、日本东京、中国香港），
并称「现有域名仍可正常使用」。本次探测走的是现有域名
`https://dashscope.aliyuncs.com`，可用。

【事实】`Content-Type` 响应：非流式 `application/json`；流式
`text/event-stream;charset=UTF-8`。

---

## 3. 请求参数字段级支持表

支持口径定义（本表专用）：

- **supported**：文档列出**且**实测生效（或实测按文档拒绝非法值）。
- **ignored**：实测原样回显却**不影响行为**，且文档未列出——静默无效。
- **rejected**：实测返回 4xx。
- **unknown**：未验证或无法验证。

| 参数 | 判定 | 证据 |
|---|---|---|
| `model` | **supported**（必选） | 文档列为必选并给出支持模型清单；非法值 → `400 InvalidParameter "Unsupported model: '__nope__'"` |
| `input`（裸字符串） | **supported** | 文档「支持直接传入字符串」；`{"input":"你能做些什么？"}` 实测 200 |
| `input`（item 数组） | **supported** | 文档定义 `EasyInputMessage` / `ResponseOutputMessage` / `function_call` / `function_call_output` / `reasoning` / `web_search_call` 六类输入项；实测 `[{"role":"system","content":[{"type":"input_text","text":"Reply with one word only."}]},{"role":"user",...}]` → 200，且系统指令被遵守（回 `Apple`） |
| `instructions` | **supported** | 文档：作为系统指令插入上下文起始位置；实测 `"reply with exactly the single word: PINEAPPLE"` → 模型回 `PINEAPPLE`，且被响应回显 |
| `tools`（扁平 function） | **supported** | 文档定义 `{"type":"function","name":...,"description":...,"parameters":...}`（`name` 在**顶层**）；实测返回 `function_call` 输出项，`arguments={"city": "Beijing"}` |
| `tools`（Chat 式嵌套 `function:{...}`） | **rejected** | 实测 `400 InvalidParameter "<400> InternalError.Algo.InvalidParameter: The parameters, when provided as a dict, must confirm to a valid openai-compatible JSON..."`。这是**扁平结构的强证据**：上游不接受 Chat 的嵌套形状 |
| `tool_choice`（`auto`/`none`/`required`） | **supported** | 文档列出三值 + 对象模式 `allowed_tools`；实测 `"required"` 被回显且确实强制产生了 `function_call` |
| `text.format` | **ignored** | **文档完全未提及 `text` 参数**。实测 ①`{"format":{"type":"BOGUS_FMT"}}` → 200 不报错；②合法 `json_schema` + `strict:true`（要求 `{"zzz":integer}`）→ 模型回散文 `"Hi"`，**不符合 schema**。回显里 `schema` 还被改名成 `schema_`。既不校验也不生效 |
| `reasoning.effort` | **supported** | 文档列 7 档 `none/minimal/low/medium/high/xhigh/max`，并给出逐模型默认值与档位映射表；非法值 → `400 "Invalid 'reasoning.effort': bogus_level. Supported values are: ('none','minimal','low','medium','high','xhigh','max')."` |
| `reasoning.summary` | **ignored** | 文档的 `reasoning` 对象**只定义 `effort`**，无 `summary`。实测 `"detailed"` 与 `"BOGUS_SUMMARY"` **都**返回 200 并原样回显——不校验，即不生效 |
| `max_output_tokens` | **supported** | 文档：最小值 16，达上限时 `status=incomplete`；实测 `1` → `400 "Invalid 'max_output_tokens': must be greater than or equal to 16, got 1."` |
| `temperature` | **supported** | 文档取值 `[0,2)`；实测 `5` → `400 "Invalid 'temperature': 5. Expected a value between [0,2), but got 5 instead."` |
| `top_p` | **supported** | 文档取值 `(0,1.0]`；实测 `9` → `400 "Invalid 'top_p': 9. Expected a value between (0,1], but got 9 instead."` |
| `metadata` | **ignored** | 文档未提及。实测 `"metadata":"NOT_AN_OBJECT"`（类型违例）→ **200** 并原样回显字符串；而后续 `GET /responses/{id}` 取回时 `metadata` 为 `{}`——**回显是请求镜像，没有持久化**，见 §4 |
| `parallel_tool_calls` | **ignored** | 文档未提及。实测 `"parallel_tool_calls":"NOT_A_BOOL"`（类型违例）→ **200** 并原样回显字符串。对比 `store` 同样的类型违例被 400 拒——证明上游**校验它认识的字段**，`parallel_tool_calls` 不在其中 |
| `stream` | **supported** | 文档给出流式示例与完整事件枚举；实测 `true` 返回 SSE 事件流（§7） |
| `store` | **supported** | 文档：默认 `true`，`false` 时内容不可被 `previous_response_id` 使用。实测类型违例 → `400 "Invalid 'store': NOT_A_BOOL. Must be a boolean."`；语义实测见 §4 |
| `previous_response_id` | **supported** | 文档：有效期 7 天，服务端自动检索并组合上下文，不能与 `conversation` 同用。实测不存在的 id → `400 "Not found previous_response_id: ..."`；同时传 `conversation` → `400 "Mutually exclusive parameters: Ensure you are only providing one of: previous_response_id or conversation."` |

【事实】上游对此有一条**总则**，写在《创建响应》的「兼容性说明与限制」里，
原文：「**请求将仅处理本文档明确列出的参数，任何未提及的 OpenAI 参数都会被忽略。**」
这条总则正好解释了三个 ignored 判定：`text.format` / `reasoning.summary` /
`metadata` / `parallel_tool_calls` 都不在文档参数表里。实测的
`"totally_bogus_param":123` 同样被静默接受，与总则一致。

【推断】这三项 ignored 是形态 A 下最危险的一类——**它们不报错**。客户端提交
`text.format=json_schema` 会拿到 200 和一段散文；提交 `metadata` 会在响应里
看见自己的值原样回来，**误以为**已被存下。透传形态下网关若不作声，就把
「静默丢语义」变成了不可见的错误，正是 `AGENTS.md`「漏配一格 → 请求被拒
（可见），而不是丢半数字段返回 200（不可见）」要防的形态。这是矩阵格子必须
处置的事实依据，具体怎么处置不在本工单范围。

【事实】另有一批**非 OpenAI 标准**的扩展参数，文档明确列出：
`enable_thinking`（布尔，文档说「后续将不再支持」，建议改用 `reasoning.effort`，
且 `reasoning.effort` 优先级更高）、`ocr_options`（仅 `qwen3.5-ocr`）、
`conversation`（配合 Conversations API）、以及请求头
`x-dashscope-session-cache: enable|disable`（默认 `disable`）。

【事实】一条容量约束：文档称「为预留内置工具调用与推理生成空间，Responses API
的最大输入上下文约为模型窗口大小的 **80%**（预留约 20% 缓冲区），超出部分将
**自动触发截断，不会报错中断**」。【推断】静默截断对 omugw 的 `context_length`
语义是个隐患——不报错的截断在网关侧无法察觉，也就无法生成降级头。

---

## 4. `store` 与 `previous_response_id`：上游**确实**持久化会话

这是本工单对现有矩阵声明冲击最大的一节。

【事实】实测证据链（三步，全部命令输出）：

1. **默认存储且真能续轮**。第一轮 `input="My secret codeword is BANANA7. Reply OK."`
   （未传 `store`），响应顶层 `id` 形如 `resp_...`、`store=true`。第二轮只传
   `input="What was my codeword? One word."` + `previous_response_id=<id1>`，
   **不带任何历史**，模型回 **`BANANA7`**，且 `usage.input_tokens=82`（远大于
   该短问题本身，说明服务端把上一轮上下文拼了进来）。
2. **`store:false` 真的不存**。第一轮带 `"store":false`，响应回显 `store=false`；
   用它的 id 作 `previous_response_id` 发第二轮 →
   `400 InvalidParameter "Not found previous_response_id: resp_3cad..."`。
3. **存在独立的检索端点**。`GET /compatible-mode/v1/responses/{id}` → 200，
   返回完整 `response` 对象。这是服务端持久化的又一直接证据。

【事实】文档与实测**一致**：`store` 默认 `true`；响应 `id` 有效期 **7 天**；
`previous_response_id` 由**服务端**自动检索并组合该轮次的输入与输出作为上下文；
同时提供 `input` 数组时新消息追加在历史之后。此外还有一个更重的有状态设施：
`conversation` 参数 + 独立的 Conversations API
（<https://help.aliyun.com/zh/model-studio/openai-compatible-conversations>），
会话中的历史项自动入上下文、本轮输入输出自动写回会话。

### 4.1 对矩阵 `stateful_conversation = EMULATE` 的影响

【事实】矩阵当前在所有 Responses 派生路径上无条件叠加
`Emulate(FeatureConversationStore, noteEmulatedSession, CapStatefulConversation)`
（`internal/degrade/rules_phase1.go:181-183`），note 原文是
「**上游无服务端会话**，由网关侧 ConversationStore 模拟提供」
（`docs/degradation-matrix.md:481`）。

【事实】该 note 的前提对 `dashscope.compatible` **不成立**。上游有服务端会话、
有 7 天 TTL、有 `store` 开关、有检索端点。

【事实】这条声明的代价是具体的，不是措辞问题。`convstore` 是内存态：
「单副本正确、重启丢失、多副本不共享」，因此 `FeatureConversationStore`
默认关闭，「`EMULATE` 格在默认配置下不可用」（`AGENTS.md` NOTES）。也就是说
**默认配置下，这条路径的 `stateful_conversation` 是不可用的**——而上游原生
就支持它。用一个默认关闭的内存模拟，去顶替一个上游已经做好且持久的能力。

【推断】所以工单 #9 的判断成立：`stateful_conversation` 的声明**需要重新审视**。
按 ADR-0002 的两列口径，这不只是「note 写错了」——EMULATE 会把一项上游原生
可用的能力，在默认配置下从 `AvailableScore()` 里抹掉。具体改成 PASSTHROUGH
还是保留 EMULATE 作为可选覆盖，是设计决定，**不在本工单范围**；本工单只把
「上游是有状态的」这个事实钉死。

【事实】另有一处不对称值得记录：工单 #9 §5.5 指出「`store` 的 422 规则不能
照抄 Chat」。本次实测给出了原因的正面版本——`store` 在此**是个真字段**，
上游认识它、校验它的类型、并按语义执行。Chat 线格式没有这个字段，
所以 Chat 路径那套「无落点即拒」的规则在这里前提就不同。

---

## 5. 内建工具处理

| 工具 | 上游处置 | 证据 |
|---|---|---|
| `web_search` | **accepted（原生支持）** | 文档列为内建工具 `{"type":"web_search"}`，响应产生 `web_search_call` 输出项（含 `action.query` / `action.sources[]`），流式有 `response.web_search_call.in_progress/searching/completed` 事件，`usage.x_tools={"web_search":{"count":1}}` 统计 |
| `computer` | **accepted-but-ignored（静默接受）** | 文档未列该工具。实测 `{"type":"computer","display_width":1,"display_height":1,"environment":"linux"}` → **200**，原样回显在 `tools` 里，**不报错**。按 §3 的「未提及即忽略」总则，【推断】它被忽略 |
| `image_generation` | **accepted-but-ignored（静默接受）** | 文档未列。实测 `{"type":"image_generation"}` → **200**，原样回显，不报错。同上【推断】被忽略 |
| （对照）`bogus_tool_xyz` | **accepted（完全不校验）** | 实测纯属编造的 `{"type":"bogus_tool_xyz"}` → **200** 并原样回显。这条对照是关键：上游**根本不校验 `tools[].type` 的白名单**，所以 `computer` / `image_generation` 的 200 不能读成「被支持」 |

【事实】上游**自有**一批内建工具，超出 OpenAI 的集合：`web_search`、
`web_extractor`（须与 `web_search` 同用）、`code_interpreter`、
`web_search_image`（文搜图）、`image_search`（图搜图）、
`file_search`（知识库检索，需 `vector_store_ids`，当前仅支持传一个）、
以及 `mcp`（`server_protocol`/`server_label`/`server_url`/`headers`）。

【推断】矩阵当前把异构路径上的 `computer_use` / `image_generation` 判成
`REJECT`（422），理由是「Phase 1 不做跨 Provider 映射」
（`rules_phase1.go:190-192`）。本次实测**支持保留 REJECT**，且给出了比原理由
更硬的依据：上游对这两个工具既不实现也不报错。如果透传，客户端会拿到 200 和
一个完全没有使用该工具的回答——不可见的失败。REJECT 把它变成可见的 422。
（这是对既有声明的事实校验，不是新的实施建议。）

---

## 6. 响应形状：标准 Responses 线格式

【事实】非流式响应是标准 Responses 信封。实测某次响应的顶层键全集：

```
background, completed_at, created_at, frequency_penalty, id, max_output_tokens,
metadata, model, object, output, parallel_tool_calls, presence_penalty,
reasoning, service_tier, status, store, temperature, tool_choice, tools,
top_logprobs, top_p, usage
```

三项关键判据全部满足：

- 【事实】`object: "response"`；
- 【事实】`output` 是数组，元素带 `type`，文档枚举 10 种：`message`、`reasoning`、
  `function_call`、`web_search_call`、`code_interpreter_call`、
  `web_extractor_call`、`web_search_image_call`、`image_search_call`、
  `mcp_call`、`file_search_call`；
- 【事实】`status` 存在，文档枚举 6 值：`completed` / `failed` / `in_progress` /
  `cancelled` / `queued` / `incomplete`。

【事实】`id` 形如 `resp_<uuid>`（实测），文档也注明是 UUID 格式且**7 天有效**，
并特别提醒 `previous_response_id` 要传顶层 `id`（`resp_xxx`）而非 `output`
数组内的 `msg_xxx`。

【事实】`usage` 是 Responses 口径：`input_tokens` / `output_tokens` /
`total_tokens` / `input_tokens_details.cached_tokens` /
`output_tokens_details.reasoning_tokens`。

【事实】**非标准扩展**共三处：`usage.x_details[]`（逐项计费明细，含
`x_billing_type: "response_api"`、多模态 Token 拆分）、`usage.x_tools`
（内建工具调用次数）、`usage.plugins`（与 `x_tools` 同内容）。

【推断】这对 omugw 的 usage 抽取是好消息：relay 从 `response.completed` 事件的
`response.usage` 取用量（`internal/gateway/relay.go:213-215`），字段名与
Responses 口径一致，`x_*` 是额外的、不冲突的键。

### 6.1 流式事件名：与 OpenAI **同名**，但信封有一处差异

【事实】实测一次最小流式请求收到的事件类型（去重）：

```
response.created, response.in_progress, response.output_item.added,
response.content_part.added, response.output_text.delta,
response.output_text.done, response.content_part.done,
response.output_item.done, response.completed
```

这九个名字与 OpenAI Responses 的事件名**逐字相同**。文档另外枚举了
`response.reasoning_text.delta/done`、`response.incomplete`、
`response.failed`、`response.custom_tool_call_input.delta/done`、
`response.web_search_call.*`、`response.code_interpreter_call.*`、
`response.mcp_call*`、`response.file_search_call.*`。每个事件带
`sequence_number`（从 0 递增）。

【事实】差异一：**无 `[DONE]` 哨兵**。实测流以
`event:response.completed` 收尾，全文 `grep -c DONE` 为 `0`。
【推断】这对 omugw 无害——`streamTerminal` 判据用的正是事件名
`response.completed`/`response.incomplete`/`response.failed`
（`internal/gateway/handler.go:151-157`），不依赖哨兵。

【事实】差异二：**每个事件里夹了一行 `:HTTP_STATUS/200`**。实测原始线格式：

```
id:1
event:response.created
:HTTP_STATUS/200
data:{"sequence_number":0,"type":"response.created","response":{...}}
```

【推断】这一行是 SSE 注释（以 `:` 开头），omugw 的 SSE 读取器会把它当注释跳过
——`internal/transport/sse/sse.go:84-86` 的注释原文就是「冒号开头是注释，常用于
心跳保活」。所以它不会破坏解析。记录它是因为它**不是** OpenAI 的形状，
fixture 录制时若手写而非实录，就会漏掉这一行。

【事实】差异三（重要）：**流式模式下的错误走 HTTP 200**。`stream:true` +
非法 `model` 的实测结果：

```
HTTP 200  CT=text/event-stream;charset=UTF-8
id:1
event:response.failed
:HTTP_STATUS/200
data:{"sequence_number":0,"type":"response.failed","response":{...,
  "error":{"message":"Unsupported model: '__nope__'","code":"InvalidParameter"},
  "status":"failed"}}
```

同一个非法 `model` 在非流式下是 `400`。【推断】这是一个真实的坑：按 HTTP 状态码
判成败的代码会把这次调用记成成功。omugw 侧 `response.failed` 已在
`streamTerminal` 名单里（`handler.go:153`），所以流不会永远等待；但
「200 + 流内错误」的用量与失败归类如何处置，需要 fixture 明确断言。

---

## 7. 错误信封：DashScope **扁平** `{code, message, request_id}`

【事实】这是形态 A 下最违反直觉的一点：**端点是 OpenAI 线格式的，错误信封不是。**
实测全部错误响应，一律扁平，**没有** OpenAI 的嵌套 `{"error":{...}}`：

```
400 {"request_id":"90ad41f0-...","code":"InvalidParameter","message":"Unsupported model: '__no_such_model__'"}
401 {"code":"InvalidApiKey","message":"No API-key provided.","request_id":"15a7463e-..."}
405 {"request_id":"4b87f638-...","code":"Method Not Allowed","message":"Method 'GET' is not allowed for '/compatible-mode/v1/responses'."}
404 {"request_id":"6a7ac052-...","code":"Not Found","message":"Not support"}
```

【事实】三点观察：

1. 三个键恒定为 `code` / `message` / `request_id`，**键序不稳定**（`request_id`
   有时在首、有时在尾）——【推断】golden 断言必须按解析后的结构比对，不能按
   字节序。
2. `code` 混用两种风格：驼峰错误码（`InvalidParameter`、`InvalidApiKey`）与
   **带空格的 HTTP 原因短语**（`Method Not Allowed`、`Not Found`）。
   【推断】把 `code` 当枚举解析的代码需要容忍后者。
3. 【事实】**唯一**出现嵌套 `error` 对象的地方是**流式** `response.failed`
   事件内部的 `response.error = {"message":..., "code":...}`——那是 Responses
   响应对象的 `error` 字段（文档：「当模型生成响应失败时返回的错误对象，
   成功时为 `null`」），**不是** HTTP 层的错误信封。两者不要混为一谈。

【推断】结论对复用的影响：omugw 已有的 DashScope 错误信封处理可以沿用（形状与
Chat 兼容路径一致），**不能**沿用 OpenAI 的 `openaiwire.EncodeError` 去**解析**
上游错误。注意方向——Responses 入站的 `encodeError` 是
`openaiwire.EncodeError`（`handler.go:160`），那是**回写给下游**的方向，正确；
需要留心的是**读取上游**错误时不能假设嵌套形状。

---

## 8. 退役与弃用通知

【事实】两条明确的退役/弃用通知，均为官方文档原文：

1. **旧路径已停止维护**。《OpenAI Responses接口兼容》与《创建响应》两页都以
   「重要」提示框声明：「OpenAI 兼容接口 Responses API 的旧版路径
   `/api/v2/apps/protocols/compatible-mode/v1/responses` **已经停止维护，将不再
   保证功能可用性**，请尽快迁移至新版路径 `/compatible-mode/v1/responses`。」
   【事实】实测该旧路径**目前仍然响应**（同样的 `400 InvalidParameter
   "Unsupported model"`），即「停止维护」不等于「已下线」。
   【推断】不应把它作为可用路径写进任何配置——文档已撤回可用性保证。
2. **`enable_thinking` 将被取代**。《创建响应》对 `enable_thinking` 注明：
   「建议使用 `reasoning.effort` 替代，`enable_thinking` **后续将不再支持**」，
   且「`reasoning.effort` 的优先级高于 `enable_thinking`」。

【事实】一条非退役但同方向的迁移建议：文档建议从 `dashscope.aliyuncs.com` /
`dashscope-intl.aliyuncs.com` 迁移到业务空间专属域名
`{WorkspaceId}.{region}.maas.aliyuncs.com`，理由是性能与稳定性，并声明
「现有域名仍可正常使用」。

【事实】文档同时给出一条**当前**的能力限制：「不支持部分 OpenAI Responses API
参数，例如异步执行参数 `background`（**当前仅支持同步调用**）」。实测响应确实
回显 `background: false`。

---

## 9. 对工单 #9 开放问题的回答

| # | 问题 | 本工单结论 |
|---|---|---|
| Q1 | 兼容层 `/v1/responses` 的字段级契约 | **已回答**：形态 A 成立（§1），端点/鉴权见 §2，16 个参数逐项判定见 §3，`store` 语义见 §4，内建工具见 §5，响应与流见 §6，错误信封见 §7 |
| Q2 | 形态 A 下内建 `web_search` 是否被 honored、DEGRADE note 怎么写 | **部分回答**：`web_search` 是上游**原生**内建工具，被 honored，且返回 `web_search_call` 输出项与 `x_tools` 计数（§5）。因此 `docs/degradation-matrix.md:487` 那条 note（谈 `enable_search` 布尔开关、`search_context_size`/`user_location` 丢失、「响应也不返回搜索来源」）在 Responses 门下**三处都不成立**——此门走的是内建工具而非 `enable_search`，且响应**确实**返回 `action.sources[]`。note 须 Override。具体措辞是设计决定，不在本工单范围 |
| Q3 | `reasoning.summary` 档位是否有损失 | **已回答**：`summary` 在上游**不存在**——文档的 `reasoning` 对象只定义 `effort`，实测合法值与编造值**都**返回 200 并原样回显，不校验即不生效（§3）。这是一处静默丢失 |
| Q4 | `audio_output` 可表达性声明与解码器现实不一致 | **未涉及**，是 omugw 内部状态，不依赖上游契约 |
| Q5 | convstore 故障链复现 | **未涉及**（需端到端环境）。但 §4 改变了前提：上游有状态，这条路径是否还需要 convstore 本身成了待定问题 |
| Q6 | 复用比例的 LOC 口径 | **不适用**；形态既定为 A，适用工单 #9 的 85–90% 区间估算 |

---

## 10. 无法核实项与开放问题（不以估算冒充实证）

- **U1**：`text.format` 的 `json_object` 形态未单独验证。本工单只测了
  `json_schema`（不生效）与编造的 `BOGUS_FMT`（不报错）。矩阵 `structured_output`
  DEGRADE 的 note 称「兼容模式支持 `json_object`」——那是 **Chat** 线格式的
  `response_format` 字段；在 Responses 门下 `text` 参数整个不在文档里，
  【推断】`json_object` 大概同样无效，但**未验证**，标 unknown。
- **U2**：`parallel_tool_calls` 被忽略后的**实际并行行为**未验证。已证明该字段
  不被校验（§3），但上游在多工具场景下究竟并行还是串行，需要一次真实多工具
  调用才能观察，超出「畸形但廉价」的探测纪律，未做。
- **U3**：`metadata` 是否在**任何**读取面上可见，未穷举。已知 `POST` 响应回显
  请求值、`GET /responses/{id}` 返回 `{}`；是否存在第三个读取面（如
  Conversations API 的历史项）未查。
- **U4**：`conversation` 参数与 Conversations API 未探测。本工单只验证了它与
  `previous_response_id` 互斥（§3）。它是第二套有状态设施，对 §4.1 的结论
  可能还有影响，未展开。
- **U5**：逐模型差异未验证。探测统一用 `qwen3.5-flash`（最廉价的支持模型之一）。
  文档明示模型间差异真实存在：`reasoning.effort` 档位与默认值逐模型不同、
  `max_output_tokens` 对 Qwen3.8 系列含思维链而对其余只含回复、
  `qwen3.8-omni-flash` 的内建工具只支持 `web_search`、`input_file` 目前仅
  `qwen3.5-ocr` 支持。**本文所有实测结论只对 `qwen3.5-flash` 直接成立**；
  推广到其他模型是【推断】。
- **U6**：文档称超出 80% 上下文窗口会「自动截断、不报错」（§3），未验证，也未
  验证截断是否在响应中留下任何可检出的痕迹。若无痕迹，`context_length` 语义在
  这条路径上不可观测。
- **U7**：仅探测了 `dashscope.aliyuncs.com`（北京，现有域名）。业务空间专属域名
  `{WorkspaceId}.{region}.maas.aliyuncs.com` 与其余 5 个地域未验证（缺
  WorkspaceId 与对应地域凭据）。国际站 `dashscope-intl` 仅验证到手头凭据在该站
  无效，未能验证其契约。
- **U8**：文档与实测在本工单中**未出现分歧**。唯一需要表态的是「文档未提及的
  参数」这一类：文档的总则说「忽略」，实测表现为「200 + 原样回显」。二者一致，
  本文按「ignored」记录。若未来发现某个此类参数实际生效，应以**实测**为准并
  记为文档滞后——因为回显本身不构成生效证据（`text.format` 的 schema 实验正是
  以回显为真会误判的反例）。

---

## 附：证据清单

**第一方文档**（均 2026-09-27 取得）：

- 《OpenAI Responses接口兼容》
  <https://help.aliyun.com/zh/model-studio/compatibility-with-openai-responses-api>
  ——端点、地域主机、旧路径退役通知、代码示例、响应示例、流式原始事件示例、
  内建工具示例、Session 缓存、从 Chat Completions 迁移。
- 《创建响应》（参数参考）
  <https://help.aliyun.com/zh/model-studio/qwen-api-via-openai-responses>
  ——请求体逐字段、响应体逐字段、流式事件类型枚举、兼容性说明与限制
  （含「仅处理本文档明确列出的参数」总则）、`reasoning.effort` 逐模型档位表。
- 《Conversations API》（仅引用其存在）
  <https://help.aliyun.com/zh/model-studio/openai-compatible-conversations>
- 《工具调用》（内建工具索引，由上两页链接）
  <https://help.aliyun.com/zh/model-studio/tool-calls>

**实测探测**（`curl`，只读，目标
`https://dashscope.aliyuncs.com/compatible-mode/v1/responses`，模型
`qwen3.5-flash`，凭据以 `$DASHSCOPE_API_KEY` 展开、值未记录）：

存在性与方法：POST 400 / GET 405 / `GET {id}` 200 / 未知子路径 404 / 无鉴权 401 /
国际站 401 / 旧 `api/v2` 路径 400。
参数校验：`temperature=5`、`top_p=9`、`max_output_tokens=1`、
`reasoning.effort=bogus_level`、`store="NOT_A_BOOL"`、
不存在的 `previous_response_id`、`previous_response_id`+`conversation` 同传、
Chat 式嵌套 function tool ——**均 400**。
静默接受：`metadata="NOT_AN_OBJECT"`、`parallel_tool_calls="NOT_A_BOOL"`、
`text.format.type="BOGUS_FMT"`、`reasoning.summary="BOGUS_SUMMARY"`、
`tools=[computer]`、`tools=[image_generation]`、`tools=[bogus_tool_xyz]`、
`totally_bogus_param` ——**均 200**。
语义行为：`instructions` → 回 `PINEAPPLE`；item 数组 `input` → 回 `Apple`；
扁平 function tool + `tool_choice=required` → `function_call{"city":"Beijing"}`；
`text.format=json_schema(strict)` → 回散文 `"Hi"`（不合 schema）；
`store` 默认 + `previous_response_id` → 回 `BANANA7`（`input_tokens=82`）；
`store:false` + `previous_response_id` → 400 Not found；
`metadata` POST 回显 vs `GET {id}` 返回 `{}`；
`stream:true` → 9 种 `response.*` 事件、`:HTTP_STATUS/200` 注释行、无 `[DONE]`；
`stream:true` + 非法 model → **HTTP 200** + `event:response.failed`。

**仓库内引用**：
`docs/research/2026-09-27-responses-to-dashscope-reuse-boundary.md`（§2.1 形态分叉、
§7.3 复用比例、§8 Q1–Q6）、
`docs/research/dashscope-compatible-get-v1-models.md:144-152`（既有存在性证据与
旧路径退役记录）、
`internal/degrade/rules_phase1.go:181-193`（`responsesExtras` 派生）、
`docs/degradation-matrix.md:481`（`stateful_conversation` EMULATE note）、
`docs/degradation-matrix.md:487`（`web_search` DEGRADE note）、
`internal/gateway/handler.go:151-160`（`streamTerminal` 与 `encodeError`）、
`internal/gateway/relay.go:213-215`（`response.completed` 取 usage）、
`internal/transport/sse/sse.go:84-86`（`:` 开头按注释跳过）、
`AGENTS.md`（convstore 内存态边界、可见失败优于静默丢字段）。
