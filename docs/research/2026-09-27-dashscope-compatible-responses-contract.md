# DashScope 兼容层 Responses API 契约（Wayfinder 研究工单 #10）

- **日期**：2026-09-27（工单日）；官方文档实际取得时间 2026-09-28。
- **基线**：`main` @ `f49f9f1`（`GIT_MASTER=1 git rev-parse HEAD` 核对）。工单上下文
  所写 `8d9ebaf` 已被该提交超越——`f49f9f1` 正是收录
  `2026-09-27-dashscope-compatible-responses-endpoint.md`（下称**探测记录**）的提交。
- **目的**：确立 DashScope OpenAI 兼容层是否存在真正的 **Responses 线格式端点**
  及其字段级契约，决定 `openai.responses → dashscope.compatible` 的实施形态
  （复用比例 85–90% 对 25–35% 的分叉，
  `docs/research/2026-09-27-responses-to-dashscope-reuse-boundary.md` §2.1、§7.3）。
  本文件**只确立契约，不提出实施方案**。
- **与探测记录的关系**：探测记录（`…-responses-endpoint.md`，已提交）是本工单
  早前执行的实测产物，含对生产端点的只读探测命令输出。本文件按工单要求的路径
  交付，以**官方文档为第一证据源**独立复核全部结论，并补上探测记录没有的两块：
  ①逐字段对照 `internal/protocol/openairesponses/wire.go`（网关实际能发出什么）；
  ②三个子资源端点（获取/删除/输入项列表）与官方文档**内部不一致**清单。
  本次执行**未发送任何真实请求**（工单纪律：文档调研 only）。
- **方法**：阿里云百炼官方文档（help.aliyun.com 中文站 + alibabacloud.com 英文站，
  均 2026-09-28 在线取得；另以 `bailian-docs-llm-wiki` 技能内 2026-09-19 的
  文档缓存交叉核对，缓存与在线版在本文引用的全部段落一致）+ 仓库只读审查
  （代码 `file:line` 以基线提交为准）+ 探测记录（作为「已观察到的命令输出」引用）。
- **约定**：每条结论标注【事实】（官方文档原文可指认 URL、仓库代码可指认
  `file:line`、或探测记录的命令输出）或【推断】（由证据推演，未直接验证）。
  无法核实的项集中在第 9 节，绝不以估算冒充实证。

> **更正（2026-10-12）**：本文是 9 月 28 日的文档调研，正文保持原样以留存当时
> 的依据。2026-10-11 的实测
> （`docs/research/2026-10-11-dashscope-compatible-responses-reprobe.md`）推翻或
> 补全了下列条目，凡冲突处以实测为准：
>
> - **§3.1 `parallel_tool_calls` 行、§9-U2**：原判「校验层面 ignored、行为
>   unknown」已过期。实测类型违例返回 400；`true`/省略 → 并行（4 个调用），
>   `false` → 1 个。9 月 27 日「200 原样回显」的记录与此相反，原因不明。
> - **§4.2**：Issue #12 已决定 `stateful_conversation` 在本路径改 PASSTHROUGH；
>   实测证实上游持久化会话，且省略 `store` 即默认存储。
> - **§9-U1**：`json_object` 已测——`qwen3.8-max` 输出合法 JSON，`qwen3.8-flash`
>   带代码围栏；`json_schema` 仍不生效。`text.format.type` 的取值**会**被校验
>   （编造值 400），与原文「编造值不报错」相反。
> - **§9-U3**：流式工具调用与思考的事件名已观察到
>   （`response.function_call_arguments.delta/done`、
>   `response.reasoning_text.delta/done`）。
> - **§9-U4**：`tools[].strict` 已测，不校验。
> - **§9-U6**：实测模型面已扩到 `qwen3.8-flash` 与 `qwen3.8-max`。
> - **§5.1 补充**：预览版类型 `web_search_preview` 实测 200 但不发生搜索；
>   `web_search` 生效。
>
> 仍未验证：§9-U5（音频裸 base64）、U7（静默截断）、U9（地域）、U10。

---

## 1. FORM VERDICT：**形态 A**

**结论：形态 A —— DashScope OpenAI 兼容层存在一个真正的 Responses 线格式端点
`POST /compatible-mode/v1/responses`，请求与响应都按 Responses wire 解析，
不是 Chat wire 的别名。**

【事实】判定依据（文档与实测双源一致）：

1. 官方文档《OpenAI Responses接口兼容》以独立页面记载该端点，curl 示例的请求体
   是 Responses 形态（`model` + `input` 字符串），响应示例是
   `object: "response"` + `output` 数组 + `status`。
   <https://help.aliyun.com/zh/model-studio/compatibility-with-openai-responses-api>
   （英文站同页，Last Updated: Sep 28, 2026：
   <https://www.alibabacloud.com/help/en/model-studio/compatibility-with-openai-responses-api>）
2. 官方参数参考《创建响应》逐字段定义 Responses 请求体与响应体，文档树位置
   「API参考（模型）→ 文本生成 → OpenAI兼容-Responses」。
   <https://help.aliyun.com/zh/model-studio/qwen-api-via-openai-responses>
3. 该资源族不止一个端点：《获取响应》`GET …/responses/{response_id}`、
   《删除响应》`DELETE …/responses/{response_id}`、《获取输入项列表》
   `GET …/responses/{response_id}/input_items` 各有独立官方页面（URL 见 §4）。
4. 探测记录 §1–§2、§6：路径存在、只收 POST（GET → 405）、按 Responses 线格式
   解析请求体、返回 `object: "response"` 标准信封。

【事实】对 omugw 的直接后果：形态 B 所需的「Canonical → Chat wire 请求编码器」
与「Chat wire 响应 → Responses wire 解码器」（工单 #9 估 550–750 行全新代码，
reuse-boundary §4.3 第 3、4 项确认两者在仓库里都不存在）**不必发生**。
现有 `dashscopecompat` 适配器（170 行）的 `pathFor` 行为——入站门
`/v1/responses` 原样用作上游路径（`internal/provider/dashscopecompat/dashscopecompat.go:90-96`；
门常量 `internal/degrade/endpoint.go:24`）——恰好指向这个真实存在的端点。

【推断】但形态 A 成立**不等于**矩阵声明与上游事实一致。本文档调研发现四处
矩阵声明与官方文档的正面冲突或无据可依（`stateful_conversation` 的「上游无
服务端会话」前提，§4.1；`web_search` note 的「响应不返回搜索来源」，§5；
`image_detail` 与 `audio_input` 的 PASSTHROUGH，§3.2），以及一处现有适配器代码
与上游错误信封的错配（§6.2）。形态 A 防住的是「重写一套编解码」；它没有防住
「表的态不是真的」——后者是 ADR-0001 的红线，兑现前必须逐格核对。

---

## 2. 端点、方法、鉴权与地域 base-URL

### 2.1 端点与方法

【事实】官方文档记载的 Responses 资源族（全部在 `/compatible-mode/v1` 前缀下）：

| 方法 | 路径 | 官方文档 |
|---|---|---|
| POST | `/compatible-mode/v1/responses` | 《创建响应》<https://help.aliyun.com/zh/model-studio/qwen-api-via-openai-responses> |
| GET | `/compatible-mode/v1/responses/{response_id}` | 《获取响应》<https://help.aliyun.com/zh/model-studio/retrieve-a-response> |
| DELETE | `/compatible-mode/v1/responses/{response_id}` | 《删除响应》<https://help.aliyun.com/zh/model-studio/delete-a-response> |
| GET | `/compatible-mode/v1/responses/{response_id}/input_items` | 《获取输入项列表》<https://help.aliyun.com/zh/model-studio/list-input-items> |

【事实】探测记录 §2 补充了方法级行为：POST 畸形体 → `400 InvalidParameter`
（进入参数校验，端点存在）；`GET /responses`（集合路径）→ `405 Method Not
Allowed`；未知子路径 → `404 Not Found`。**没有** list-responses 集合端点的
文档记载——与 OpenAI 官方 Responses API 一致（OpenAI 也无 GET 集合列举）。

【事实】旧路径 `/api/v2/apps/protocols/compatible-mode/v1/responses` 已停止
维护（§7 退役通知第 1 条）。

### 2.2 鉴权

【事实】`Authorization: Bearer <API Key>`，全部官方 curl 示例一致（《创建响应》
《获取响应》等页）。API Key 须先获取并配置（《OpenAI Responses接口兼容》
「前提条件」节，链接 <https://help.aliyun.com/zh/model-studio/get-api-key>）。

【事实】探测记录 §2：缺失 key → `401 {"code":"InvalidApiKey","message":"No
API-key provided.","request_id":"…"}`（扁平信封，见 §6.2）；同一把中国站 key
打国际站主机 `dashscope-intl.aliyuncs.com` → `401 InvalidApiKey`。
【推断】密钥按地域绑定，凭据池不能假设一把 key 跨地域可用。这不是本工单的
结论，是探测顺带暴露的事实；与百炼 wiki 层「api_key 严格绑定地域与计费方案」
的表述一致（该表述为技能内合成层，非原始官方页面，仅作旁证）。

### 2.3 地域 base-URL：两代主机形态，中英文档存在一处不一致

【事实】官方推荐的新形态是**业务空间专属域名**，中英两页均列出 6 个地域
（《OpenAI Responses接口兼容》「服务地址」节）：

| 地域 | SDK `base_url`（新形态） |
|---|---|
| 华北2（北京） | `https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/compatible-mode/v1` |
| 新加坡 | `https://{WorkspaceId}.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1` |
| 美国（弗吉尼亚） | `https://{WorkspaceId}.us-east-1.maas.aliyuncs.com/compatible-mode/v1`（中文站） |
| 德国（法兰克福） | `https://{WorkspaceId}.eu-central-1.maas.aliyuncs.com/compatible-mode/v1` |
| 日本（东京） | `https://{WorkspaceId}.ap-northeast-1.maas.aliyuncs.com/compatible-mode/v1` |
| 中国香港 | `https://{WorkspaceId}.cn-hongkong.maas.aliyuncs.com/compatible-mode/v1` |

【事实】**中英文档对弗吉尼亚的记载不一致**：英文站（2026-09-28 更新）该地域
给的是 `https://dashscope-us.aliyuncs.com/compatible-mode/v1`——一个不含
WorkspaceId 的旧式主机；中文站给的是 `us-east-1` 专属域名。英文站还多列了
香港的旧域名 `https://cn-hongkong.dashscope.aliyuncs.com`。
【推断】弗吉尼亚可能处于「专属域名尚未全量铺开」的过渡态；配置该地域时应以
英文站（国际站运营方视角、更新时间更近）为准或两者都试。本文不裁决。

【事实】旧形态主机仍然可用：「现有域名仍可正常使用」（中英两页原文）。
北京旧域名 `https://dashscope.aliyuncs.com`、新加坡旧域名
`https://dashscope-intl.aliyuncs.com`。探测记录 §2 的实测走的正是北京旧域名。

【推断】对 omugw 的含义：`base_url` 是部署配置事实，两代主机形态都必须能配；
适配器现有的「官方 base_url 已含 /compatible-mode/v1，只在版本段重叠时去重」
逻辑（`dashscopecompat.go:60-67`）对两代形态同样适用——它按后缀 `/v1` 判断，
与主机名无关。

【事实】`Content-Type`：非流式响应 `application/json`；流式
`text/event-stream;charset=UTF-8`（探测记录 §2）。

---

## 3. 请求线格式字段级支持表

### 3.0 判定口径与上游总则

支持口径（本表专用）：

- **accepted**：官方文档列出该参数（且探测记录证实生效或按文档校验非法值）。
- **ignored**：官方文档未列出，按上游总则被忽略；探测记录证实「200 + 原样
  回显但不生效/不校验」。
- **rejected**：实测返回 4xx。
- **unknown**：文档与实测均未覆盖，或两者矛盾到无法判定。

【事实】上游总则（《创建响应》「兼容性说明与限制」原文）：「**请求将仅处理
本文档明确列出的参数，任何未提及的 OpenAI 参数都会被忽略。**」同节还列出：
不支持异步参数 `background`（仅同步）；最大输入上下文约为模型窗口的 **80%**，
超出**自动截断、不报错**。
【推断】这条总则是本表全部 ignored 判定的依据，也是形态 A 下最危险的一类
事实——被忽略的参数**不报错**。客户端提交 `text.format=json_schema` 会拿到
200 和一段散文；80% 截断在网关侧不可观测，`context_length` 语义在这条路径上
没有上游信号可用。透传形态下网关若不作声，就把「静默丢语义」变成不可见的
错误——正是 `AGENTS.md`「漏配一格 → 请求被拒（可见），而不是丢半数字段
返回 200（不可见）」要防的形态。

### 3.1 顶层参数逐项判定（对照 `wire.go` Request 结构）

`wire.go` 列 = 网关入站解码后**实际能发出**的字段
（`internal/protocol/openairesponses/wire.go:14-51`）。凡文档支持而 wire.go
没有的字段，网关发不出去（入站 `DisallowUnknownFields` 会先拒掉客户端，
`internal/protocol/openairesponses/decode.go:58`）；凡 wire.go 有而文档未列的，
上游按总则忽略。两个方向都在表里注明。

| 参数 | wire.go 字段 | 判定 | 证据 |
|---|---|---|---|
| `model` | `Model`（:15） | **accepted**（必选） | 文档列为必选并给出支持模型清单（《创建响应》请求体节）；探测记录：非法值 → `400 "Unsupported model: '__nope__'"` |
| `input`（裸字符串） | `Input json.RawMessage`（:19，`decodeInput` 分派） | **accepted** | 文档：「支持直接传入字符串作为模型输入」；探测记录：`{"input":"你能做些什么？"}` → 200 |
| `input`（item 数组） | 同上 | **accepted** | 文档定义六类输入项：`EasyInputMessage`（role ∈ user/assistant/system/developer）、`ResponseOutputMessage`、`function_call`、`function_call_output`（必须紧跟对应 function_call，否则报错）、`reasoning`、`web_search_call`（《创建响应》「array 输入项类型」节）；探测记录：system+user 内容块数组 → 200 且系统指令被遵守 |
| `instructions` | `Instructions`（:23） | **accepted** | 文档：「作为系统指令插入到上下文的起始位置。**使用 previous_response_id 时，上一轮指定的 instructions 不会传入本轮上下文**」；探测记录：单词指令被遵守并回显 |
| `tools`（扁平 function） | `Tools []json.RawMessage`（:25）；`Tool` 结构（:55-61，`name` 在顶层） | **accepted** | 文档定义 `{"type":"function","name":…,"description":…（必选）,"parameters":…}`，name 仅允许字母/数字/下划线/短划线、最长 64 Token；《Function Calling》页有 Chat 嵌套 vs Responses 扁平的对照表（<https://help.aliyun.com/zh/model-studio/qwen-function-calling>「Responses API的工具调用」节）；探测记录：返回 `function_call` 输出项 |
| `tools`（Chat 式嵌套 `function:{…}`） | —（网关入站不会产生：`Tool` 结构是扁平的） | **rejected**（上游行为） | 探测记录：`400 InvalidParameter "…must confirm to a valid openai-compatible JSON…"`。扁平结构的强证据 |
| `tools[].strict`（function 工具的 strict 位） | `Tool.Strict`（:60） | **unknown** | 文档的 function 工具属性只有 type/name/description/parameters，未提 strict；按总则应 ignored，但无实测，不冒充判定 |
| `tool_choice` | `ToolChoice json.RawMessage`（:26） | **accepted**（有一处语义收窄） | 文档：字符串 `auto`/`none`/`required` + 对象模式 `{mode, tools[], type:"allowed_tools"}`；**「required：强制模型调用工具（仅当 tools 列表中只有一个工具时可用）」**——OpenAI 无此限制，多工具 + required 在上游是文档级不支持；探测记录：单工具 required 被回显且强制产生 function_call |
| `text.format` | `Text *TextOpts`（:35）、`TextFormat`（:76-81） | **ignored** | 《创建响应》请求体**完全没有 `text` 参数**；《结构化输出》页只覆盖 Chat 线格式的 `response_format`（<https://help.aliyun.com/zh/model-studio/qwen-structured-output>，全文无 Responses `text.format` 用法）；探测记录：①编造的 `BOGUS_FMT` → 200 不报错；②合法 `json_schema`+`strict:true` → 模型回散文，不合 schema，回显里 `schema` 被改名 `schema_`。既不校验也不生效 |
| `reasoning.effort` | `Reasoning.Effort`（:65） | **accepted** | 文档列 7 档 `none/minimal/low/medium/high/xhigh/max` + 逐模型默认值与档位映射表（qwen3.8 系默认 `xhigh`，`high`/`max` 映射为 `xhigh` 等）；探测记录：非法值 → 400 并列出支持档位 |
| `reasoning.summary` | `Reasoning.Summary`（:67） | **ignored** | 文档 `reasoning` 对象**只定义 `effort`**；探测记录：`"detailed"` 与编造值都 200 + 原样回显——不校验即不生效。响应方向的思考摘要由 `reasoning` 输出项的 `summary[]` 承载（文档），与请求参数无关 |
| `max_output_tokens` | `MaxOutputToks`（:29） | **accepted** | 文档：最小值 16；达上限提前停止、`status=incomplete`；**Qwen3.8 系列含思维链、其余型号只含回复**（同名参数两种计量口径）；探测记录：`1` → `400 "must be greater than or equal to 16"` |
| `temperature` | `Temperature *float64`（:31） | **accepted** | 文档取值 `[0,2)`；探测记录：`5` → 400 |
| `top_p` | `TopP *float64`（:32） | **accepted** | 文档取值 `(0,1.0]`；探测记录：`9` → 400 |
| `parallel_tool_calls` | `ParallelToolCalls *bool`（:37） | **ignored**（校验层面）；行为效果 **unknown** | 《创建响应》请求体未列该参数；《获取响应》却把它描述为「回显原创建响应请求中 parallel_tool_calls 参数的值」（文档自相矛盾，见 §8-D2）；探测记录：类型违例 `"NOT_A_BOOL"` → 200 + 原样回显（对比 `store` 同样违例被 400 拒——上游只校验它认识的字段）；多工具场景实际并行与否未验证（§9-U2） |
| `metadata` | `Metadata map[string]string`（:50） | **ignored**（不持久化） | 《创建响应》请求体未列；《获取响应》称「回显原创建响应请求中 metadata 参数的内容…无设置时为空对象 {}」（矛盾同 §8-D2）；探测记录：类型违例字符串 → 200 + POST 回显原值，但 `GET /responses/{id}` 取回为 `{}`——**回显是请求镜像，没有落库** |
| `store` | `Store *bool`（:48） | **accepted** | 文档：默认 `true`；`false` 时「对话内容不能被 previous_response_id 和后续 API 使用」；探测记录：类型违例 → `400 "Must be a boolean"`；语义实测见 §4 |
| `previous_response_id` | `PreviousResponseID`（:41） | **accepted** | 文档：id 有效期 **7 天**，「服务端会自动检索并组合该轮次的输入与输出作为上下文」，同时给 `input` 数组时新消息追加在历史后，不能与 `conversation` 同用；探测记录：续轮成功（§4）、不存在 id → 400、与 conversation 同传 → 400 互斥错误 |
| `stream` | `Stream *bool`（:28） | **accepted** | 文档：默认 `false`，`true` 时流式返回；事件枚举见 §6.1；探测记录：SSE 事件流 |

【事实】文档明确列出、而 wire.go **表达不出**的上游扩展参数（网关入站
`DisallowUnknownFields` 会在解码阶段拒掉携带它们的客户端请求，
`decode.go:53-62`）：

- `enable_thinking`（布尔；文档注明「非 OpenAI 标准参数…建议使用
  reasoning.effort 替代，后续将不再支持」，且 `reasoning.effort` 优先级更高）；
- `ocr_options`（仅 `qwen3.5-ocr`）；
- `conversation`（配合 Conversations API，与 `previous_response_id` 互斥）；
- 请求头 `x-dashscope-session-cache: enable|disable`（默认 disable；开启后
  服务端自动缓存对话上下文，最小可缓存 1024 Token，精确匹配命中，
  `usage.input_tokens_details.cached_tokens` / `cache_creation_input_tokens`
  可见命中量——《OpenAI Responses接口兼容》「Session 缓存」节）。

【推断】`tools` 是 `[]json.RawMessage`，工具对象在 wire 层原样透传，所以上游
自有内建工具（§5）客户端**可以**经网关表达；但顶层扩展参数（上表四项）经
网关不可达。这是「网关能发出什么」与「上游认识什么」的两个方向的差集，
矩阵若要让客户端用到 session-cache 或 conversation，得先动 wire 与解码器——
是否值得是设计决定，不在本工单范围。

### 3.2 内容块级判定（对照 `wire.go` ContentPart / InputAudio）

`input` 数组消息里的内容块，wire.go 建模了五种（:105-126）。逐项对照文档
（《创建响应》「content 数组元素」节）：

| 内容块 | wire.go 字段 | 判定 | 证据与含义 |
|---|---|---|---|
| `input_text` | `Text`（:108） | **accepted** | 文档：type ∈ input_text/input_image/input_file/output_text |
| `input_image` | `ImageURL`（:111） | **accepted** | 文档：「支持 URL 或者 Base64 编码…Base64 请传入完整的 Data URI」——与 OpenAI 的 data URI 形态一致 |
| `input_image.detail` | `Detail`（:112） | **ignored** | 文档的 content 元素属性只有 type/text/image_url/file_url（及 omni 音视频字段），**没有 `detail`**；按总则被忽略。【推断】矩阵该路径 `image_detail` 声明为 PASSTHROUGH（`docs/degradation-matrix.md:469`）——上游文档没有任何承载点，PASSTHROUGH 的「零损失」含义在此不成立，兑现前须核对该格（改判 DEGRADE 还是别的处置是设计决定） |
| `input_file`（`file_id` 形态） | `FileID`/`Filename`（:115-116） | **rejected by absence** | 文档的 `input_file` 用 **`file_url`**（公网 URL，PDF ≤100MB/图片 ≤20MB，「目前仅 qwen3.5-ocr 支持」），没有 OpenAI 的 `file_id` 形态。【推断】矩阵 `file_input` REJECT（`degradation-matrix.md:472`）与文档一致——file_id 绑定 Provider 文件体系，上游根本不接受这个键 |
| `input_audio` | `InputAudio{Data,Format}`（:119-126） | **accepted（限 qwen3.8-omni-flash），wire 形态有差异** | 文档：`format` 必填（wav/mp3 等），`audio_url` 与 `data` 二选一，**`data` 须为含 MIME 前缀的完整 Data URI**（`data:audio/wav;base64,…`）；OpenAI 的 `input_audio.data` 是裸 base64。【推断】矩阵 `audio_input` PASSTHROUGH（`degradation-matrix.md:470`）在两点上超出文档保证：①模型面窄（仅 omni-flash）；②裸 base64 原样透传是否被上游接受**未验证**（探测未覆盖，§9-U5）。兑现该格前需要真实证据 |
| （对照）`input_video` | —（wire.go 无） | 网关表达不出 | 文档支持（仅 omni-flash，`video_url`+`fps`）；OpenAI wire 无视频输入，矩阵 `video_input` N/A（`degradation-matrix.md:471`）与两侧文档都一致 |

### 3.3 响应回显字段的一个陷阱

【事实】《获取响应》把 `parallel_tool_calls`、`temperature`、`top_p`、
`frequency_penalty`、`presence_penalty`、`top_logprobs`、`store`、`metadata`
都描述为「回显原创建响应请求中…参数的值」，但其中前六项里除 temperature/top_p/
store 外都**不在《创建响应》的请求参数表里**。《创建响应》FAQ 对此有总解释：
「如果使用 OpenAI 的官方 SDK，它可能会根据其自身的模型结构输出一些额外的字段
（通常为 null）。这些字段是 OpenAI 协议本身定义的，我们的服务当前不支持，
所以它们为空值。」

【推断】**回显 ≠ 生效**。响应对象里出现某字段（哪怕带着客户端的原值）不构成
上游处理了它的证据——探测记录里 `text.format` 的 schema 实验正是「以回显为真
会误判」的实证（回显里 schema 改名 `schema_`，行为上完全不生效）。判定支持面
只能以《创建响应》请求参数表 + 行为实测为准。

---

## 4. `store` 与 `previous_response_id`：上游**确实**持久化会话

这是本契约对现有矩阵声明冲击最大的一节。

### 4.1 文档证据链（本次调研新增，探测记录之外）

【事实】服务端状态有**四个独立的文档化读取面**，全部以 `store=true` 为前提：

1. `GET /compatible-mode/v1/responses/{response_id}`——《获取响应》：「仅当原
   创建请求中 `store=true` 时返回的 Response ID 可被检索」，返回完整 Response
   对象（含 `store`、`previous_response_id` 回显）。
2. `GET …/responses/{response_id}/input_items`——《获取输入项列表》：「在多轮
   对话（使用 previous_response_id 串联）中，**会一并返回历史轮次的用户输入和
   模型回复**」，分页参数 after/limit(≤100)/order，返回体含
   `previous_response_id` 链接上一轮。这是服务端保管**整条会话链**的直接证据。
3. `DELETE …/responses/{response_id}`——《删除响应》：「根据 Response ID 删除
   一个已存储的模型响应」，返回 `{deleted, id}`。有删除语义即有存储实体。
4. Conversations API（`conversation` 参数 + 独立资源）——《OpenAI Conversations
   接口兼容》<https://help.aliyun.com/zh/model-studio/openai-compatible-conversations>：
   完整 CRUD + items 子资源；「会话中的历史项会自动作为上下文传入本次请求，
   本次请求的输入和输出也会在响应完成后自动添加到会话中」；使用限制：items
   单次 ≤20 条、metadata ≤16 对、「**会话信息保留最近 7 天内的最新 100 条**，
   超出时间或数量限制的内容将自动清理」。

【事实】《创建响应》对 `store`/`previous_response_id` 的定义（§3.1 表内已引）
与上述读取面互洽：`store` 默认 `true`；id 有效期 7 天；服务端自动检索并组合
上下文；`instructions` 不跨轮携带。

【事实】探测记录 §4 的行为实测与文档一致：默认存储续轮成功（第二轮不带历史
答出第一轮暗号，`input_tokens=82` 证明服务端拼接了上下文）；`store:false`
的 id 用作 `previous_response_id` → `400 "Not found previous_response_id"`；
`GET /responses/{id}` → 200。

### 4.2 对矩阵 `stateful_conversation = EMULATE` 的影响

【事实】矩阵当前在所有 Responses 派生路径上无条件叠加
`Emulate(FeatureConversationStore, noteEmulatedSession, CapStatefulConversation)`
（`internal/degrade/rules_phase1.go:181-183`），note 原文「**上游无服务端会话**，
由网关侧 ConversationStore 模拟提供」（`docs/degradation-matrix.md:481`）。

【事实】该 note 的前提对 `dashscope.compatible` **不成立**：上游有服务端会话、
7 天 TTL、`store` 开关、三个检索/删除端点、外加一套更重的 Conversations 设施。
文档与实测双源一致（§4.1）。

【事实】这条声明的代价是具体的：`convstore` 是内存态（单副本正确、重启丢失、
多副本不共享），`FeatureConversationStore` 默认关闭，EMULATE 格在默认配置下
不可用（`AGENTS.md` NOTES；`internal/gateway/build.go:47-49` 把开关写进
Availability）。即**默认配置下，这条路径的 `stateful_conversation` 不可用——
而上游原生支持且持久 7 天**。

【推断】工单 #9 的判断成立：`stateful_conversation` 的声明**需要重新审视**。
按 ADR-0002 两列口径，EMULATE 会把一项上游原生可用的能力在默认配置下从
`AvailableScore()` 里抹掉。改成 PASSTHROUGH 还是保留 EMULATE 作可选覆盖，是
设计决定，不在本工单范围；本工单把「上游是有状态的」钉死为文档级事实。

【事实】两处与 convstore 交互的文档细节，重新审视时必须一并考虑：

1. convstore 的请求重写会**主动往 raw 里写 `store:false`** 并删除
   `previous_response_id`（`internal/protocol/openairesponses/conversation.go:15-40`，
   `store:false` 写入在 `:33-34`；reuse-boundary §5.5 已引）。在上游有状态的现实下，这套重写的语义是
   「关掉上游持久化、用网关内存历史顶替」——功能上可行，但把 7 天 TTL 的
   上游存储换成了进程生命周期的内存存储，可用性边界反而变窄。
2. 文档明言「使用 previous_response_id 时，上一轮指定的 instructions 不会
   传入本轮上下文」——上游自己的续轮语义与 convstore「历史全量 prepend」的
   语义在 instructions 处理上**不同构**。两种机制混用时客户端看到的上下文
   取决于哪一层生效，note 必须写清。

【事实】另一处不对称的正面版本：Chat 路径把「显式提交 `store`」列为无落点
字段 422 拒绝（`internal/provider/dashscopenative/reject.go:25-27`），而
Responses 门下 `store` 是上游认识、校验、按语义执行的**真字段**（§3.1、§4.1）。
「无落点即拒」的规则在此前提不同，不能照抄（reuse-boundary §5.5 同旨）。

---

## 5. 内建工具处理

### 5.1 OpenAI 定义的三个内建工具在上游的命运

| 工具 | 上游处置 | 证据 |
|---|---|---|
| `web_search` | **accepted（原生支持，一等公民）** | 文档列为内建工具 `{"type":"web_search"}`（《创建响应》tools 节）；响应产生 `web_search_call` 输出项，含 `action.query`/`action.type`/`action.sources[]`；流式有 `response.web_search_call.in_progress/searching/completed` 事件；`usage.x_tools={"web_search":{"count":1}}` 计数。《联网搜索》页明言：「通过 tools 参数添加 web_search 工具即可启用联网搜索」（<https://help.aliyun.com/zh/model-studio/web-search>），且「**Responses API 的联网搜索：其搜索来源会在响应中自动返回**…但暂不支持 enable_citation 角标标注」 |
| `computer` | **ignored（文档未列，静默接受）** | 《创建响应》tools 节没有该工具；探测记录：`{"type":"computer",…}` → 200 原样回显不报错。按总则忽略。注意探测记录的对照组：纯编造的 `bogus_tool_xyz` 同样 200——**上游不校验 `tools[].type` 白名单**，所以 200 不能读成「被支持」 |
| `image_generation` | **ignored（文档未列，静默接受）** | 同上；探测记录：`{"type":"image_generation"}` → 200 原样回显 |

【事实】矩阵现状：`web_search` DEGRADE、`computer_use`/`image_generation`
REJECT 422（`docs/degradation-matrix.md:487-488`、`:474`）。

【推断】逐格核对结论：

- `computer_use`/`image_generation` 的 **REJECT 应保留**，且依据比原 note 更硬：
  上游对这两个工具既不实现也不报错，透传会让客户端拿到 200 和一个根本没用到
  该工具的回答——不可见的失败；REJECT 把它变成可见的 422。
- `web_search` 的 **DEGRADE note 三处与文档冲突**（`degradation-matrix.md:487`
  原文谈「只有 enable_search 布尔开关；search_context_size 与 user_location
  在此丢失，响应也不返回搜索来源」）：①此门走的是内建工具而非 `enable_search`
  开关——那是 Chat 线的映射（`dashscopecompat.go:158-163` 的 patch 只认
  `web_search_options`，Responses 请求体里根本没有这个键，reuse-boundary §2.1
  已指出该映射对 Responses 是死代码）；②OpenAI `web_search` 工具的
  `search_context_size`/`user_location` 子参数在上游文档确实无落点（这部分
  损失成立）；③「响应不返回搜索来源」**与官方文档正面冲突**——`action.sources[]`
  自动返回。note 须 Override，措辞是设计决定。
- 另有一层模型面限制要写进任何 note：「非列表中阿里云百炼直供文本生成模型仅
  支持基础兼容能力，**Agent 能力（内置工具等）受限**」（《创建响应》支持模型节
  「重要」提示框）；`qwen3.8-omni-flash` 的内建工具仅 `web_search`。

### 5.2 上游自有的内建工具（OpenAI 集合之外）

【事实】文档列出（《创建响应》tools 节 +《工具调用》索引
<https://help.aliyun.com/zh/model-studio/tool-calls>）：`web_extractor`
（须与 web_search 同用）、`code_interpreter`（部分模型须开思考模式）、
`web_search_image`（文搜图）、`image_search`（图搜图）、`file_search`
（知识库检索，`vector_store_ids` 必填且当前仅支持一个）、`mcp`
（`server_protocol`/`server_label`/`server_url` 必填，`headers` 可选）。
响应侧对应 `web_extractor_call`/`code_interpreter_call`/`web_search_image_call`/
`image_search_call`/`file_search_call`/`mcp_call` 输出项（§6）。
网页抓取与代码解释器「当前限时免费」（《OpenAI Responses接口兼容》内建工具节）。

【推断】这些工具经网关**可达**（`tools` 是 raw 字节透传，§3.1），但矩阵的
能力枚举没有对应格子（`canonical.Capability` 27 项里没有 code_interpreter/
mcp 等）。透传形态下客户端用了它们，矩阵看不见也裁决不了——这是能力枚举
与上游工具集的差集问题，属于设计决定，本工单只记录差集存在。

---

## 6. 响应形状

### 6.1 非流式：标准 Responses 信封 + 三处非标准扩展

【事实】《创建响应》「Response 响应对象」节逐字段定义，关键判据全部满足：

- `object` 固定 `"response"`；
- `status` 六值枚举：`completed`/`failed`/`in_progress`/`cancelled`/`queued`/
  `incomplete`；
- `output` 数组，元素 `type` 十种枚举：`message`、`reasoning`、`function_call`、
  `web_search_call`、`code_interpreter_call`、`web_extractor_call`、
  `web_search_image_call`、`image_search_call`、`mcp_call`、`file_search_call`；
- `usage` 是 Responses 口径：`input_tokens`/`output_tokens`/`total_tokens`/
  `input_tokens_details.cached_tokens`/`output_tokens_details.reasoning_tokens`；
- `error`：「当模型生成响应失败时返回的错误对象。成功时为 null」；
- `id`：《创建响应》称「UUID 格式…有效期 7 天」，《获取响应》与示例则显示
  `resp_` 前缀（`resp_801bc2c4-…`）；《OpenAI Responses接口兼容》多轮对话节
  特别提醒 `previous_response_id` 要传顶层 id（`resp_xxx`）而非 output 内的
  `msg_xxx`。探测记录实测顶层 id 为 `resp_<uuid>`。【推断】以 `resp_` 前缀
  为准，「UUID 格式」是文档措辞不严谨（§8-D4）。

【事实】非标准扩展三处（《创建响应》usage 节 + 探测记录 §6 一致）：
`usage.x_details[]`（逐项计费明细，`x_billing_type:"response_api"`，多模态
Token 拆分含 `audio_tokens`）、`usage.x_tools`（内建工具计数）、
`usage.plugins`（与 x_tools 同内容）。Session 缓存开启后另有
`cache_creation_input_tokens`/`cache_creation` 字段。
【推断】扩展键与 Responses 标准键不冲突；relay 从 `response.completed` 事件取
usage 的路径（`internal/gateway/relay.go:210-218`）按标准键名解析，`x_*` 是
额外键，不影响抽取。

【事实】响应对象还回显 `tools`/`tool_choice`（文档明言「回显请求中…参数的
完整内容」），以及一批 SDK 结构字段（`text`、`metadata`、`temperature`、
`top_p`、`background`、`truncation`、`user` 等，通常为 null——《创建响应》
FAQ，§3.3）。`background` 固定 `false`、`service_tier` 固定 `"default"`
（《获取响应》字段说明）。

### 6.2 流式：事件名与 OpenAI 同名，信封有三处已证实的差异、两处文档缺口

【事实】官方文档的流式事件枚举（《创建响应》「Response 响应 chunk 对象」节）：
`response.created`（status=queued）、`response.in_progress`、
`response.output_item.added/done`、`response.content_part.added/done`、
`response.output_text.delta/done`、`response.reasoning_text.delta/done`、
`response.custom_tool_call_input.delta/done`、
`response.web_search_call.in_progress/searching/completed`、
`response.code_interpreter_call.in_progress/interpreting/completed`、
`response.mcp_call_arguments.delta/done`、`response.mcp_call.in_progress/completed`、
`response.file_search_call.in_progress/searching/completed`、
`response.incomplete`、`response.completed`（「此事件标志流式传输结束」）。
每个事件带 `sequence_number`（从 0 递增）。

【事实】探测记录 §6.1 实测收到的九个事件名（created/in_progress/
output_item.added/content_part.added/output_text.delta/output_text.done/
content_part.done/output_item.done/completed）与 OpenAI Responses **逐字相同**。

对照 OpenAI 事件面（以 omugw 自己按 OpenAI 规范建模的事件常量为参照，
`internal/protocol/openairesponses/stream.go:12-34`；OpenAI 官方 API 参考页
本次抓取被 403 拦截，未能直接核对，见 §9-U8）：

- **差异一：无 `[DONE]` 哨兵**。文档全部流式示例以 `response.completed` 收尾、
  无哨兵；探测记录 `grep -c DONE` = 0。【推断】对 omugw 无害——`streamTerminal`
  判据用的正是事件名 `response.completed/incomplete/failed`
  （`internal/gateway/handler.go:151-157`），不依赖哨兵。
- **差异二：每个事件夹一行 `:HTTP_STATUS/200` SSE 注释**。这一行**在官方文档
  自己的原始 SSE 示例里**（《创建响应》「网页抓取」流式示例，`id:N` /
  `event:名称` / `:HTTP_STATUS/200` / `data:{…}` 四行一组），探测记录实测同形。
  【推断】omugw 的 SSE 读取器按注释跳过（`internal/transport/sse/sse.go:84-86`），
  不破坏解析；但 fixture 若手写而非实录会漏掉这一行。
- **差异三：流式错误走 HTTP 200**。探测记录 §6.1：`stream:true` + 非法 model →
  `HTTP 200` + `event:response.failed`，`response.error={"message":…,"code":
  "InvalidParameter"}`，`status:"failed"`；同一错误非流式是 400。
  【事实】`response.failed` 事件**不在《创建响应》的事件枚举里**（枚举只有
  `response.incomplete`），但 `status` 枚举含 `failed`、响应对象含 `error`
  字段——文档枚举不完备，实测行为有据。【推断】按 HTTP 状态码判成败的代码
  会把流式失败记成成功；omugw 的 `streamTerminal` 已含 `response.failed`
  （`handler.go:153`），流不会挂死，但「200 + 流内错误」的用量与失败归类
  需要 fixture 明确断言。
- **缺口一：流式 function call 的参数事件未文档化**。HTTP Responses 流式下
  function_call 的增量事件（OpenAI 的 `response.function_call_arguments.delta/
  done`）在《创建响应》枚举中**缺席**；仓库内该事件名只出现在 Qwen-Omni-
  **Realtime**（WebSocket）的文档语境（<https://help.aliyun.com/zh/model-studio/
  qwen-function-calling>「Qwen-Omni-Realtime 系列」节），不是本端点的证据。
  探测未覆盖流式工具调用。标 **unknown**（§9-U3）——这是形态 A 直通下
  `tool_calling` + `streaming` 组合兑现前必须补的一块实测。
- **缺口二：思考摘要事件名文档内部打架**。《创建响应》枚举写
  `response.reasoning_text.delta/done`；而《代码解释器》《网页抓取》两页的
  Responses API 流式示例代码用的是 `response.reasoning_summary_text.delta`
  （<https://help.aliyun.com/zh/model-studio/qwen-code-interpreter>、
  <https://help.aliyun.com/zh/model-studio/web-extractor>）——后者才是 OpenAI
  的事件名，也是 omugw StreamEncoder 发出的名字（`stream.go:31-32`）。
  探测记录的最小流未触发思考事件，实测缺位。标 **unknown**（§9-U3）。
  【推断】直通形态下网关原样转发上游字节，上游发哪个名字客户端就收到哪个；
  若上游发 `reasoning_text.*` 而客户端 SDK 只认 `reasoning_summary_text.*`，
  思考摘要在客户端侧会静默丢失。

### 6.3 错误信封：扁平为主，官方文档自身两种形状并存

【事实】探测记录 §7：POST 端点的全部实测错误（400/401/404/405）一律 DashScope
**扁平**信封 `{code, message, request_id}`，没有 OpenAI 的嵌套 `{"error":{…}}`；
键序不稳定；`code` 混用驼峰错误码（`InvalidParameter`/`InvalidApiKey`）与带
空格的 HTTP 原因短语（`Method Not Allowed`/`Not Found`）。

【事实】**官方文档在子资源端点上两种形状并存**（本次文档调研新发现）：

- 《获取响应》《删除响应》的「错误响应」节：id 不存在时返回**嵌套**
  `{"error":{"message":"Response with id 'resp_xxx' not found.","type":"InvalidParameter"}}`；
- 《获取输入项列表》的「错误响应」节：**同一个** not-found 错误返回**扁平**
  `{"request_id":"…","code":"InvalidParameter","message":"Response with id 'resp_xxx' not found."}`。

【事实】流式 `response.failed` 事件内部的 `response.error={message,code}` 是
Responses 响应对象的 `error` 字段（文档定义见 §6.1），不是 HTTP 层信封，
两者不要混为一谈（探测记录 §7 第 3 点同旨）。

【推断】对复用的影响，比探测记录的表述更收紧一步：错误信封**不能按端点族
一刀切**。现有适配器的 `decodeError` 走 `openaiwire.DecodeError`
（`dashscopecompat.go:105-122`），该解码器只解析嵌套 `{"error":{…}}`
（`internal/protocol/openaiwire/error.go:88-96`：解析失败静默、`msg` 回退
HTTP 状态文本、`UpstreamCode` 留空）。对扁平信封的 POST 错误，它的实际行为是
**按状态码降级分类、丢掉上游 message 与 code**——注释「DashScope Compatible
的非 2xx 与 OpenAI 同形」（`dashscopecompat.go:105-106`）对 Chat 门是否成立
仓库内无 fixture 级证据（`testdata/routes/openai.chat__dashscope.compatible/`
十一份 fixture 无一份错误录制），对 Responses 门则被探测记录**证伪**。
【推断】形态 A 兑现时，上游错误读取需要容忍双形状（仓库已有扁平信封解码器
`internal/protocol/dashscopewire/`），且 golden 断言必须按解析后的结构比对、
不能按字节序（键序不稳定）。具体怎么改是实施决定，不在本工单范围。

---

## 7. 退役与弃用通知

【事实】全部为官方文档原文（中英两页一致）：

1. **旧路径已停止维护**：「OpenAI 兼容接口 Responses API 的旧版路径
   `/api/v2/apps/protocols/compatible-mode/v1/responses` **已经停止维护，将不再
   保证功能可用性**，请尽快迁移至新版路径 `/compatible-mode/v1/responses`」
   （《OpenAI Responses接口兼容》《创建响应》两页的「重要」提示框；英文站
   "is no longer maintained and its functionality is no longer guaranteed"）。
   探测记录 §8：旧路径**目前仍响应**（同样 400）——「停止维护」≠「已下线」。
   【推断】不应把旧路径写进任何配置：文档已撤回可用性保证。
2. **`enable_thinking` 将被取代**：「建议使用 `reasoning.effort` 替代，
   `enable_thinking` **后续将不再支持**」，且「`reasoning.effort` 的优先级高于
   `enable_thinking`」（《创建响应》enable_thinking 参数说明）。
3. **Conversations 旧路径即将停止维护**：「旧版 URL 路径
   `/api/v2/apps/protocols/compatible-mode/v1/conversations` **即将停止维护**，
   请尽快迁移至新版路径」（《OpenAI Conversations接口兼容》「重要」提示框）。
   注意措辞差异：Responses 旧路径是「已经停止维护」，Conversations 是「即将」。
4. **域名迁移建议**（非退役）：从 `dashscope.aliyuncs.com`/
   `dashscope-intl.aliyuncs.com`（英文站另列 `cn-hongkong.dashscope.aliyuncs.com`）
   迁移到业务空间专属域名，理由是「卓越的性能和更高的稳定性」，并明确
   「现有域名仍可正常使用」（中英两页）。
5. **能力限制（当前态，非退役）**：「不支持部分 OpenAI Responses API 参数，
   例如异步执行参数 `background`（**当前仅支持同步调用**）」（《创建响应》
   兼容性说明）。响应回显 `background:false`（《获取响应》：「当前百炼仅支持
   同步调用，固定为 false」）。

---

## 8. 官方文档内部不一致清单（本次调研新增）

契约调研的一部分工作是记录**文档自身打架**的地方——每一处都是未来实测的
候选点，也是「以文档为唯一依据」会翻车的地方。

- **D1 弗吉尼亚主机**：中文站 `{WorkspaceId}.us-east-1.maas.aliyuncs.com` vs
  英文站 `dashscope-us.aliyuncs.com`（§2.3）。
- **D2 回显字段的存在性**：《获取响应》把 `parallel_tool_calls`/`metadata`/
  `frequency_penalty`/`presence_penalty`/`top_logprobs` 描述为「回显原创建
  请求中的参数」，但《创建响应》请求参数表没有这些参数，且其总则说未列参数
  被忽略（§3.1、§3.3）。探测记录支持「POST 回显是请求镜像、metadata 不
  持久化」的读法。
- **D3 错误信封**：同一个 not-found 错误，《获取响应》/《删除响应》给嵌套
  `{"error":{…}}`，《获取输入项列表》给扁平 `{request_id,code,message}`（§6.3）。
- **D4 id 格式**：《创建响应》说顶层 id 是「UUID 格式」，《获取响应》与全部
  示例用 `resp_` 前缀；探测记录实测 `resp_<uuid>`（§6.1）。
- **D5 思考摘要流式事件名**：《创建响应》枚举 `response.reasoning_text.*`，
  《代码解释器》/《网页抓取》的 Responses 示例用
  `response.reasoning_summary_text.*`（§6.2 缺口二）。
- **D6 流式事件枚举不完备**：`response.failed` 被实测观察到但不在枚举里
  （§6.2 差异三）。

【推断】D2–D6 的共同模式：**参数参考页滞后于实现、示例页滞后于参数页**。
凡文档与实测冲突，以实测为准并记录文档滞后（探测记录 §10-U8 同旨）；凡只有
文档没有实测的（本报告的全部 doc-only 判定），兑现前按 ADR-0001 补真实
fixture。

---

## 9. 无法核实项与开放问题（不以估算冒充实证）

- **U1 `text.format` 的 `json_object` 形态**：未单独验证。探测只测了
  `json_schema`（不生效）与编造值（不报错）。矩阵 `structured_output` DEGRADE
  note 称「兼容模式支持 json_object」——那是 Chat 线的 `response_format`
  （《结构化输出》页只有 Chat 用法）；Responses 门下 `text` 参数整个不在
  文档里，【推断】`json_object` 大概率同样无效，**未验证**，标 unknown。
- **U2 `parallel_tool_calls` 的实际并行行为**：已证明不被校验（§3.1），多工具
  场景究竟并行还是串行未验证；文档还有一处反向线索——全部非流式响应示例
  `"parallel_tool_calls": false`（《创建响应》响应示例），而《获取响应》示例
  是 `true`，默认值到底是什么也无文档定论。
- **U3 流式工具调用与思考摘要的事件名**：§6.2 两个缺口。兑现
  `tool_calling`+`streaming` 或 `reasoning` 流式前必须实测录制。
- **U4 `tools[].strict`（function 工具）**：文档未提，无实测（§3.1）。
- **U5 `input_audio` 裸 base64 透传**：文档要求完整 Data URI，OpenAI wire 是
  裸 base64 + format 字段；上游对 OpenAI 形态是拒绝、是容忍、还是静默错解析，
  未验证（§3.2）。`audio_input` PASSTHROUGH 兑现前必须补。
- **U6 逐模型差异**：探测统一用 `qwen3.5-flash`。文档明示模型间差异真实存在
  （`reasoning.effort` 档位与默认值逐模型不同、`max_output_tokens` 计量口径
  分两族、omni-flash 内建工具仅 web_search、input_file 仅 qwen3.5-ocr、
  非列表模型 Agent 能力受限）。**本文全部实测结论只对 qwen3.5-flash 直接
  成立**；文档结论按各页标注的模型面适用。
- **U7 80% 上下文静默截断**：文档明言「超出部分将自动触发截断，不会报错
  中断」，截断是否在响应中留痕未验证。若无痕，`context_length` 语义在此
  路径不可观测（§3.0）。
- **U8 OpenAI 官方事件面核对**：platform.openai.com 的 API 参考页本次抓取
  403（bot 拦截），OpenAI 侧事件名以 omugw `stream.go` 常量（仓库按 OpenAI
  规范自建模型）为参照。若需绝对权威核对，换网络环境重抓或查 OpenAI SDK
  源码。
- **U9 地域覆盖**：探测只打了北京旧域名 `dashscope.aliyuncs.com`。专属域名
  形态、其余 5 地域、国际站契约均未验证（探测记录 §10-U7 同）。文档层面
  中英两页已核对（§2.3 的 D1 是唯一分歧）。
- **U10 Conversations API 全量契约**：本工单只核实其存在、与
  `previous_response_id` 的互斥关系、保留策略（7 天/100 条）。它是第二套
  有状态设施，若未来网关要利用它，需要单独调研。

---

## 附：证据清单

**第一方文档**（在线取得 2026-09-28；另与 `bailian-docs-llm-wiki` 技能内
2026-09-19 缓存交叉核对，引用段落一致）：

- 《OpenAI Responses接口兼容》（CN）
  <https://help.aliyun.com/zh/model-studio/compatibility-with-openai-responses-api>
  （EN，Last Updated Sep 28, 2026）
  <https://www.alibabacloud.com/help/en/model-studio/compatibility-with-openai-responses-api>
  ——端点、地域主机、旧路径退役、代码示例、响应示例、原始 SSE 示例、内建
  工具示例、Session 缓存、Chat→Responses 迁移。
- 《创建响应》（参数参考）
  <https://help.aliyun.com/zh/model-studio/qwen-api-via-openai-responses>
  ——请求体逐字段、响应体逐字段、流式事件枚举、兼容性总则（「仅处理本文档
  明确列出的参数」）、80% 截断、reasoning.effort 逐模型档位表、FAQ（回显
  字段解释）。
- 《获取响应》<https://help.aliyun.com/zh/model-studio/retrieve-a-response>
  ——GET 端点、store=true 前提、回显字段、嵌套错误示例。
- 《删除响应》<https://help.aliyun.com/zh/model-studio/delete-a-response>
  ——DELETE 端点、{deleted,id}、嵌套错误示例。
- 《获取输入项列表》<https://help.aliyun.com/zh/model-studio/list-input-items>
  ——input_items 端点、多轮历史返回、分页、扁平错误示例。
- 《OpenAI Conversations接口兼容》
  <https://help.aliyun.com/zh/model-studio/openai-compatible-conversations>
  ——CRUD+items、自动上下文注入、7 天/100 条保留、旧路径即将停止维护。
- 《联网搜索》<https://help.aliyun.com/zh/model-studio/web-search>
  ——Responses 用 tools 启用、来源自动返回、enable_citation 不支持。
- 《Function Calling》<https://help.aliyun.com/zh/model-studio/qwen-function-calling>
  ——Chat 嵌套 vs Responses 扁平工具定义对照表。
- 《结构化输出》<https://help.aliyun.com/zh/model-studio/qwen-structured-output>
  ——全文仅 Chat `response_format` 用法（`text.format` 缺席的文档证据）。
- 《代码解释器》<https://help.aliyun.com/zh/model-studio/qwen-code-interpreter>、
  《网页抓取》<https://help.aliyun.com/zh/model-studio/web-extractor>
  ——Responses 流式示例用 `response.reasoning_summary_text.delta`（D5）。
- 《工具调用》索引 <https://help.aliyun.com/zh/model-studio/tool-calls>
- 《错误码》<https://help.aliyun.com/zh/model-studio/error-code>
  ——错误消息目录（未定义信封形状；429 限流码族）。

**仓库内探测记录**（已提交的观察命令输出，本次执行未发任何真实请求）：
`docs/research/2026-09-27-dashscope-compatible-responses-endpoint.md`
（`f49f9f1` 提交）——§2 存在性/鉴权/Content-Type、§3 参数逐项实测、
§4 store 三步证据链、§5 内建工具与 bogus 对照、§6 响应顶层键与流式实测、
§7 扁平错误信封四例、§8 旧路径仍响应。

**仓库内代码与文档**：
`internal/protocol/openairesponses/wire.go:14-126`（Request/Tool/Reasoning/
TextOpts/ContentPart/InputAudio 结构）、`decode.go:53-62`（DisallowUnknownFields）、
`stream.go:12-34`（OpenAI 事件名常量）、
`internal/provider/dashscopecompat/dashscopecompat.go:54-122`（Call/pathFor/
decodeError）、`:134-169`（patch 只认 web_search_options）、
`internal/protocol/openaiwire/error.go:88-96`（DecodeError 仅解析嵌套信封）、
`internal/gateway/handler.go:151-160`（streamTerminal/upstreamPath/encodeError）、
`internal/gateway/relay.go:210-218`（response.completed 取 usage）、
`internal/transport/sse/sse.go:84-86`（`:` 注释行跳过）、
`internal/degrade/endpoint.go:24`、`internal/degrade/rules_phase1.go:181-183`、
`internal/protocol/openairesponses/conversation.go:15-40`（ExpandConversationRequest，store:false 在 :33-34）、
`internal/provider/dashscopenative/reject.go:25-27`、
`docs/degradation-matrix.md:457-489`（本路径全部格子）、
`docs/research/2026-09-27-responses-to-dashscope-reuse-boundary.md`（§2.1 形态
分叉、§5.5 store 不对称、§7.3 复用比例）、
`docs/research/dashscope-compatible-get-v1-models.md:146-152`（最早的存在性
线索）、`testdata/routes/openai.chat__dashscope.compatible/`（无错误录制）、
`AGENTS.md`（convstore 边界、可见失败原则）。
