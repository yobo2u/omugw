# OpenAI Responses 到 DashScope Compatible 路径投放设计

**日期**：2026-10-12
**状态**：已批准，待实现
**前序约束**：ADR-0001（声明必须由 fixture 兑现）、ADR-0002（设计分与可用分分离）
**决策来源**：Issue #12（已关闭，会话处置）、#11（进展，解码器补报）、地图 #3
**证据**：`docs/research/2026-10-11-dashscope-compatible-responses-reprobe.md`（实测）、
`docs/research/2026-09-27-dashscope-compatible-responses-contract.md`（文档调研，已加更正）

## 背景

`openai.responses → dashscope.compatible` 的含义是：客户端讲 OpenAI Responses
协议，请求打到百炼兼容层，由百炼模型作答。它不是「用 GPT 模型」——那是同源路径
`openai.responses → openai.compat`。

百炼兼容层有真正的 Responses 线格式端点（`/compatible-mode/v1/responses`），
并且**自己持久化会话**：2026-10-11 实测中只传 `previous_response_id` 的续轮答出了
首轮口令，不带 id 的对照答不出。矩阵现在把这条路径的 `stateful_conversation` 记为
EMULATE（网关 convstore 垫会话），与上游事实不符：垫出来的会话是内存态、单副本、
重启即丢，而上游原生会话没有这些边界。

## 目标与成功标准

让 Responses 协议客户端经网关使用百炼模型，在 `/v1/responses` 门兑现有证据的能力。

- 每个兑现格都有这扇门自己的真实录制 fixture，不沿用 Chat 门或 9 月探测的结论。
- 矩阵处置与 2026-10-11 实测相符；实测推翻的声明在本次改正。
- `make check` 通过。

本期只对 `qwen3.8-flash` 与 `qwen3.8-max` 举证，只对北京旧域名
`dashscope.aliyuncs.com` 举证。

## 决策

采用**线格式兼容直通**（调研结论的形态 A），复用现有 `dashscopecompat` 适配器，
按入站门分支。

1. 入站仍由 `openairesponses.Decode` 严格解码，Canonical 只服务请求校验、能力识别
   与矩阵裁决。
2. 出站以客户端原始 JSON 为基准，只做两处定点修补：改写 `model`；把 `tools` 里
   `type` 为 `web_search_preview` 的项改成 `web_search`。其余字节不动。
3. 上游响应 JSON 与 SSE 原样回传，`response.id` 不改写。
4. 会话由上游承担：`store` 与 `previous_response_id` 原样转发，本路径不经
   convstore，即使运维开启了它。
5. 路径不调用 `MarkHomogeneous`。wire-compatible 只说明不需要重编码，降级头照常
   由矩阵生成。

否决的方案：

- **新建独立适配器包**：同一个 Provider 种类拆成两个包，鉴权、路径拼接与错误读取
  各写一遍，没有换来任何隔离。
- **重编码成 Chat 线格式打 `/chat/completions`**：调研估算复用只有 25–35%，还会
  丢掉上游原生会话与内建搜索，与决策 4 冲突。

## 矩阵规则

路径仍从 `openai.chat → dashscope.compatible` 派生，再逐格覆盖。Responses 可表达
15 项能力：

| 能力 | 处置 | 相对现状 | 本期兑现 | 依据 |
|---|---|---|---|---|
| text_generation、streaming、tool_calling、reasoning、vision_input | PASSTHROUGH | 不变 | 是 | 实测 200，事件面与 OpenAI 同名 |
| parallel_tool_calls | PASSTHROUGH | 不变 | 是 | `true`/省略 → 4 个调用，`false` → 1 个；类型违例 400 |
| stateful_conversation | PASSTHROUGH | 原为 EMULATE | 是 | 续轮答出口令；`store:false` 后续轮 400 |
| image_detail | DEGRADE | 原为 PASSTHROUGH | 是 | 四种取值 `input_tokens` 全相同，编造值不报错 |
| web_search | DEGRADE | 处置不变，note 重写 | 是 | `web_search` 产生 `web_search_call`；`web_search_preview` 200 但不搜索 |
| structured_output | DEGRADE | 处置不变，note 重写 | 是 | `json_schema` 返回散文；`json_object` 在 flash 上带代码围栏；`strict` 不校验 |
| file_input、audio_output、computer_use、image_generation | REJECT | 不变 | 不适用 | 既有规则 |
| audio_input | PASSTHROUGH | 不变 | **否** | 无证据；含它的请求返回 501 |

- 兑现 10 项（7 PASS + 3 DEGRADE），门是 `/v1/responses`。
- 设计分 (8 + 0.5 × 3) / 15 ≈ 0.633（现为 0.667）；门可用分 (7 + 0.5 × 3) / 15
  ≈ 0.567，`Gated()` 为真。以 `make matrix-update` 的输出为准。
- `audio_input` 沿用 `openai.chat → dashscope.native` 的先例：设计处置保留，本期
  不兑现。上游文档把音频输入限定在 omni-flash，且要求 `data` 是带 MIME 前缀的
  Data URI，与 OpenAI 的裸 base64 不同；原样透传能否被接受没有证据。
- `structured_output` 保持 DEGRADE，不改 REJECT。网关把两件事都算作这项能力
  （`internal/canonical/request.go:166-176`）：`text.format` 为 `json_schema` /
  `json_object`，以及任一 function 工具带 `strict: true`。一个格子只有一个处置，
  改 REJECT 会让带 `strict: true` 工具的请求一并 422，而主流 SDK 生成的工具通常
  带这个标志。代价是 `json_schema` 请求会拿到 200 加一段散文，只靠降级头提示；
  这一点写进 note 与路径文档。
- 把工具严格模式从 `structured_output` 拆成独立能力（使 `text.format` 可单独
  REJECT）语义更准，但新增能力会要求全部已注册路径补声明，不在本次范围，见 #22。

### `responsesExtras` 拆分

现在的 `responsesExtras`（`internal/degrade/rules_phase1.go:181`）把会话处置和
内建工具处置绑在一起，并对三条异构路径一律 `Emulate`。拆成两步：

- 会话处置单独一个函数，按出站区分：`dashscope.compatible` → `Pass`；
  `dashscope.native`、`anthropic.messages`、`openai.compat` → 保持 `Emulate`。
- 内建工具处置（`computer_use`、`image_generation`）保持原样。

`noteEmulatedSession` 现在以「上游无服务端会话」开头，替上游下了断言。改成只描述
网关自己做了什么（由 ConversationStore 提供、内存态的边界、默认关闭），不再断言
上游有没有会话。`openai.compat` 的处置是否也该改，属于 #21，不在本次范围。

### note 文本

- `stateful_conversation`（PASS，无降级头，写进矩阵文档与 README）：会话由上游
  持久化；归属由上游 key 决定，共用同一把 key 的调用方之间不隔离；省略 `store`
  时内容按上游默认留存 7 天；多 key 部署下续轮可能落到另一把 key 而得到上游 400。
- `web_search`（DEGRADE）：搜索由上游内建工具执行；预览版类型由网关改写为
  `web_search`；附带选项不保证生效。
- `image_detail`（DEGRADE）：上游不区分 `detail` 档位，精度与计费意图丢失。
- `structured_output`（DEGRADE）：上游在 Responses 门对 `json_schema` 不生效、对
  `json_object` 不保证输出合法 JSON、对工具的 `strict` 不校验；三者均原样透传。

## 请求链路

```text
POST /v1/responses
  -> openairesponses.Decode(raw)
       - 严格校验（未知字段拒绝）、识别能力
       - parallel_tool_calls 显式 true 时上报 CapParallelToolCalls（本次新增）
  -> Router.Resolve(model)
  -> Matrix.BestOutbound(Inbound{openai.responses, /v1/responses}, candidates, caps)
       - REJECT → 422；未兑现（audio_input）→ 501；均在触达上游前
       - DEGRADE → X-Omugw-Degraded
  -> 会话分支：该路径的 stateful_conversation 处置不是 EMULATE → 跳过 convstore
  -> dashscope.compatible Provider（入站门为 /v1/responses 时）
       - model -> target.UpstreamModel
       - tools[].type: web_search_preview -> web_search
       - 其余 JSON 字段保持
  -> POST {base_url}/responses
  -> relayJSON / relayStream 原样返回
```

### 会话分支

`serve`（`internal/gateway/handler.go:319`）在选路之后无条件调用
`prepareConversation`。它不看出站，装了 convstore 就展开历史、写 `store:false`、
删 `previous_response_id` 并生成本地 `response.id`；没装 convstore 而请求显式依赖
会话时返回内部错误（`:563`）——这条分支的前提是矩阵已经把请求拦下，改成 PASS 后
前提不再成立，带 `previous_response_id` 的请求会撞上 500。

判据取自矩阵，不硬编码 Provider 名：用 `Matrix.Lookup(in, out,
CapStatefulConversation)` 查选中路径的处置，只有 EMULATE 才进
`prepareConversation`。这样矩阵仍是唯一权威，#21 日后改 `openai.compat` 的处置时
主链路不用再动。

`:301` 处「装了 convstore 且省略 `store`」时追加 `CapStatefulConversation` 的逻辑
保留：对本路径该格是 PASS，不产生降级头，也不改变裁决。

### 适配器修补

`dashscopecompat.Call` 现有的 `patch` 面向 Chat 线格式（删 `web_search_options`、
映射 `enable_search`），对 Responses 请求是死代码。按 `req.Inbound.Endpoint` 分成
两个修补函数：Chat 门走现有逻辑不变；Responses 门只做 `model` 改写与工具类型改名。
`pathFor` 不改：入站端点 `/v1/responses` 与以 `/v1` 结尾的 `base_url` 去重后正好
落到上游 Responses 端点。

### 原样透传且不告知的字段

`reasoning.summary`、`metadata`、`tools[].strict` 被上游接受但静默忽略或弱化
（`summary` 不校验且 reasoning 项恒带一段摘要；`metadata` 创建时回显但不落库；
`strict` 不校验）。`reasoning.summary` 与 `metadata` 在矩阵里没有对应的能力格，
网关原样透传，损失写进路径文档。`tools[].strict` 为 `true` 时会触发
`structured_output` 格，由该格的降级头告知。

### 上游更窄的语义

`tool_choice: "required"` 配多个工具时上游返回 400
（`Please set one tool in required mode.`）。网关不预判，原样映射回客户端并写进
文档。

### 经网关不可达的上游扩展

Responses 解码器拒绝未知字段，上游扩展参数（`conversation`、`enable_thinking`
等）经网关不可达。本期不放开。

## 响应链路

- 响应体原样回传。用量抽取与流式终止判定沿用 Responses 入站现有实现：上游的
  `usage.input_tokens` / `output_tokens` / `input_tokens_details.cached_tokens` /
  `output_tokens_details.reasoning_tokens` 与 `response.completed` 等终止事件都和
  实测对得上。
- 上游回显里的非标准字段（如 `text.format` 的 `schema_`）原样回传，不清洗。

### 错误信封

这扇门返回扁平信封 `{"request_id", "code", "message"}`。现有适配器调
`openaiwire.DecodeError`，按 OpenAI 嵌套信封取 `error.{type,code,message}`，对扁平
信封取不到 `code` 与 `message`，只剩按状态码分类——客户端看到的是
`Bad Request` 而不是上游的原话，`context_length` 一类靠 `code` 才能认出的错误也
会被归错类。

Responses 门下改用扁平解码：先试扁平形状，取不到 `code`/`message` 再回落嵌套
形状（官方文档里两种形状并存）。解码结果仍是 `canonical.Error`，由入站侧按 OpenAI
信封编码给客户端。分类映射表在实施计划里按实测错误码逐条列出，至少覆盖已观察到
的 `InvalidParameter`（400，含 `Not found previous_response_id`）与模型不存在。

续轮未命中（上游 400）归为 bad_request：不重试、不冷却凭据。它不是凭据故障，
换凭据只会更糟。

## 解码器修复（先于新路径）

Responses 解码器（`internal/protocol/openairesponses/decode.go:122`）把
`parallel_tool_calls` 只放进 Extensions，没有上报能力，矩阵因此从未对它裁决。
补成与 Chat 解码器同一条件：显式 `true` 时上报 `CapParallelToolCalls`。

这会改变已通车路径 `openai.responses → openai.compat` 的裁决输入。该格已在它的
兑现名单内，白名单不用改，但要给它补一份覆盖该格的 fixture（现有三份都不带这个
字段）。作为实施计划的第一个任务，独立提交。

## 证据与 fixture

全部用真实上游录制，放在 `testdata/routes/openai.responses__dashscope.compatible/`。
录制入口沿用 `tests/smoke` 里的显式开关模式（新增专用环境变量，未设置即跳过），
不走裸 `make smoke`，不落盘、不打印凭据。主录模型 `qwen3.8-flash`。

正例（每项兑现能力至少一份）：

| 用例 | 覆盖 | 断言要点 |
|---|---|---|
| basic、streaming | text_generation、streaming | 上游请求 `model` 已改写，其余字段保持 |
| tool_calling、tool_calling-streaming | tool_calling | `function_call_arguments` 事件原样转发 |
| parallel_tool_calls-true、-false | parallel_tool_calls | 字段原样到达上游；无降级头 |
| reasoning、reasoning-replay | reasoning | 回传 reasoning 项的续问被上游接受 |
| vision_input | vision_input | 无降级头 |
| image_detail | image_detail | 出现降级头 |
| web_search、web_search_preview | web_search | 后者的上游请求里类型已改写；出现降级头 |
| structured_output-json_schema、-json_object、-tool-strict | structured_output | 字段原样到达上游；出现降级头 |
| stateful-continue | stateful_conversation | 上游请求保留 `previous_response_id`，`response.id` 未改写；装配 convstore 时同样不展开历史 |
| stateful-store-false | stateful_conversation | 续轮得到上游 400，映射为 bad_request，凭据未冷却 |
| stateful-store-omitted | stateful_conversation | 上游请求里没有被写入 `store` |

负例：

- `file_input` → 422；`audio_input` → 501；上游均未被调用。
- 上游扁平错误信封被解出 `code` 与 `message`。
- `tool_choice: required` 配两个工具 → 上游 400 原样映射。

白名单：`matrix_test.go` 的 `TestImplementedRoutesAreExplicit` 加入本路径，
`TestRedeemedCapabilitiesAreExplicit` 加入「本路径 @ `/v1/responses`」的 10 项。

### 未证实项

- **上游 id 的归属范围**：两把 key 交叉续轮没有测过（手头只有一把 key）。note 写
  「归属范围未证实」，列为投放前必测项；拿到第二把 key 后补一次探测。
- 其他地域、专属域名、国际站；搜索工具附带选项；`qwen3.5-flash` 省略
  `parallel_tool_calls` 时的默认值。
- 9 月 27 日记录过 `parallel_tool_calls` 类型违例返回 200，10 月 11 日同一模型
  返回 400，原因不明。上游行为会漂移，fixture 的录制日期要写进用例。

## 文档

- `make matrix-update` 重新生成 `docs/degradation-matrix.md`。
- 更新根 `AGENTS.md` 的状态段（路径数、通车数、本路径的兑现摘要）。
- README：省略 `store` 时内容默认在上游留存 7 天；共用上游 key 的调用方之间会话
  不隔离；本路径不经 convstore。
- 更正并提交 #10 契约调研文档，归档 10 月 11 日两轮实测摘要（随本 spec 提交）。
- 不新增 ADR：没有引入新的系统级约束。

## 不在本次范围

- 上游的 Conversations API 与会话缓存请求头。
- `GET` / `DELETE /responses/{id}` 子资源端点：网关不投放，客户端拿到的 id 只能
  用于续轮。
- `audio_input` 的兑现。
- 凭据亲和与调用方隔离（#12 已决定不做）。
- `openai.responses → dashscope.native`、思考历史在异构路径的处置（#11 余项），
  以及同源路径的会话处置（#21）。
