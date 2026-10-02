# A 包：同契约 WebSocket 直通与三条路径投放设计

**日期**：2026-10-02

**状态**：用户已确认范围与总体方案；本规范待书面审阅，尚未进入实施计划或编码。

**基线**：`main@ee36d244fc8a5cbcd8c261a2f33dd09a1801c86e`。

**前置**：[ADR-0003](../../adr/0003-realtime-minimal-session-semantics.md)、
[Realtime 最小会话契约](2026-09-28-realtime-minimal-session-contract.md)、
[WS fixture 设计](2026-09-29-websocket-fixture-design.md)与
[离线基座实施计划](../plans/2026-10-01-websocket-fixture-offline-foundation.md)。
这些成果复用，不重新实施；历史调研是日期快照，不是本次真实调用证据。

## 1. 用户目标与交付范围

用户将剩余全部九条路径纳入后续交付，包括此前暂缓的两条 Anthropic 出站路径。
分三包推进：A 为三条同协议 WS，B 为四条 HTTP 异构转换，C 为两条跨协议 Realtime。
每包有独立规范、计划和验收；本文件只覆盖 A。

| 路径 | 入站门 | 出站协议 | 目标 |
|---|---|---|---|
| `openai.realtime → openai.realtime` | `GET /v1/realtime` | OpenAI Realtime | 同契约消息直通 |
| `dashscope.realtime → dashscope.ws.realtime` | `GET /api-ws/v1/realtime` | DashScope Realtime | 同契约消息直通 |
| `dashscope.inference → dashscope.ws.inference` | `GET /api-ws/v1/inference` | DashScope run-task | JSON 指令与 binary 消息直通 |

成功不是加三个 `Redeem`：必须有生产接线、安全握手、实际双向转发、生命周期、
可靠用量观测，以及逐项能力的真实上游证据和离线回归。

**不做**：同协议事件翻译、音频重采样、通用 Realtime IR、业务工具执行、伪造 ack、
WebRTC、临时上游凭据签发、跨 Provider 转换、Native 新端点、价格与账单。
OpenAI `call_id` sideband、无模型的 `intent=transcription` 独立会话不在本包；
模型会话内的音频转写在范围内。不把入口形状相似理解成允许代理任意 WS 服务。

## 2. 原样转发的准确含义

- 保全完整应用消息的 opcode 与负载字节；同一方向维持 FIFO。
- 不解析成 Canonical 后重编码，不重排未知字段、不删 null/false/0/空集合。
- 文本包络的合法性、已建模能力、模型位置及必要用量允许只读解析；不透明字段
  只被保全，不被声称经过语义验证。
- 两段连接分别处理掩码、分片与 ping/pong；不承诺网络帧逐字相同。
- 客户端凭据与租户特权字段不是任意透传对象；在握手中清洗并替换。
- 唯一应用消息改写例外是 Inference `run-task.payload.model` 的显式逻辑模型映射。
  仅替换该字符串 span，其余字节保全；若逻辑名与上游名一致，则整条消息不变。
  重复 model 键、非字符串或无法无歧义定位时拒绝，不用整对象 marshal 重建。
- Realtime 模型改名只发生在握手 query，不修改服务端回显的真实模型名。
  会话中显式 `session.model` 若与已绑定上游模型冲突，发送前可见拒绝；不悄悄
  将逻辑名写进音频/工具/会话事件。

**选型**：共享生产消息 relay + 协议专用握手/能力投影/只读观测。
否决三套完整代理（生命周期修复要改三次）、通用 IR 重编码（丢未知字段）、
纯 TCP 泵（无法替换握手凭据和可靠观测多笔用量）。

## 3. 模块职责与现有接口

| 模块 | 负责 | 不负责 |
|---|---|---|
| `internal/transport/ws` | RFC 握手、帧、限额、读写期限、关闭 | 路由、能力、模型与费用 |
| `internal/provider` 的 `StreamProvider` | 根据已授权 target/凭据 Dial，返回 WS 或握手响应 | 下游 Accept、业务事件转换 |
| `internal/gateway` WS handler | 鉴权→选路→矩阵→凭据→上游→下游承诺→会话管理 | 把 HTTP `tracked` 当 WS 首字节依据 |
| 共享 WS relay | 有界双向收发、背压、错误协调与回收 | Inference/Realtime 共用业务 IR |
| 协议专用观测模块 | 必要 ID/能力/终态/usage，原始单位及去重 | 执行工具、推测费用、重新序列化 payload |
| `internal/testkit` 与录制入口 | 真实契约采集、候选审核、四观测点回放 | 生产转发/真实来源认证 |

沿用已批准的平行接口，不扩现有 HTTP `Provider`：

```go
type StreamProvider interface {
    Kind() degrade.Provider
    Dial(ctx context.Context, req Request) (*ws.Conn, *http.Response, error)
}
```

`provider.Request` 增加 WS 所需的类型化握手信息（query、契约标识），不把 URL 或
字段含义藏进 `Raw`/`Extensions`；`Canonical` 在本包直通中不作为数据总线。
HTTP Call 的行为和字段约定保持不变。三个 kind 可复用同一 Dial 实现，通过
固定协议定义决定路径/许可 query/header/契约，不从模型名后缀猜协议。

生产代码不能依赖 `testkit`。共享能力/字段规则若需复用，放在生产协议模块，
不能把测试加载器、synthetic fixture 或 mock 当生产治理模块。

## 4. 配置与契约选择

### 4.1 显式版本

WS Provider 增加 `ws_contract`，本包候选值：
`openai-realtime-ga`、`openai-realtime-beta`、`dashscope-realtime-v1`、
`dashscope-inference-v1`。这些是本网关配置 profile 名，不伪称云端统一版本号。
必须与 Provider kind 对齐；WS Provider 缺失配置启动失败，HTTP Provider 不得填写。

生产支持清单只收已完成官方契约核对和真实举证的 profile。beta 若已退役或当前
无调用证据，保留规划但不进入有效配置清单，不自动降回 GA。GA/beta 不靠缺少
`OpenAI-Beta` 头猜测；target 的显式 profile 与客户端版本信号冲突时在 101 前拒绝。
DashScope 同一门的 Omni/TTS/ASR 不能仅凭路径声称同契约；profile 必须明确其
可观测包络，模型族的实际证据另存，不根据 SDK 模型总表推断线格式。

### 4.2 地址与受信任头

- 沿用 `base_url` 的主机/可选前缀语义，HTTP 为 http/https；WS kind 额外接受 ws/wss。
  在 WS Dial 时进行协议明确的 scheme 转换，HTTP kind 不接受 ws/wss。
- 路径由 kind 固定追加；OpenAI `/v1` 重叠段只按已存在的明确前缀规则去重。
  不允许 userinfo、query、fragment、opaque URL，不使用客户端 host/query 构造目的主机。
- WS Provider 增加最窄 `ws_headers` 配置：仅允许固定的 OpenAI organization/project
  与 DashScope workspace/data-inspection 头；Authorization、Cookie、连接/nonce/
  子协议/Host 等自动头不得配置。不能让任意动态客户端值选择出站租户。
  值必须单一且无CR/LF/控制字符，受握手预算限制；外来同名租户头与组织/项目
  token拒绝而不是静默改成另一个租户。其余客户端可选头默认不转发；契约所需
  的beta头由profile生成，User-Agent使用网关自己的稳定值，Cookie等秘密到此止步。
- Realtime 入站只接受唯一非空 `model` query；secret query 拒绝，不作为鉴权来源。
  本包不支持的 `call_id`/`intent` 返回明确 Unsupported，未知 query 可见拒绝。
- 共享 `/v1/realtime` 下每个逻辑model的 OpenAI target 必须属于同一profile；不能
  将GA/beta两个不同target随机failover。客户端有明确beta信号则必须匹配；无信号
  使用配置声明而不是猜测。为同一物理模型提供GA/beta需要显式不同逻辑路由名。
- DashScope `realtime-v1` 是包络基线，只有被录制核验的模型族进入配置支持清单。
  不支持的模型族即使同URL，也不被默认为同契约；新增族单独扩展profile观测规则。

### 4.3 生产资源预算

新增 `websocket` 配置，默认值：

| 项 | 默认 | 语义 |
|---|---:|---|
| `max_message_bytes` | 32 MiB | 单条完整重组消息；Dial/Accept 时生效 |
| `max_sessions` | 128 | 全进程，含待拨号/待升级的 WS 尝试 |
| `max_usage_records` | 4096 | 每会话去重和未结记录合计，不淘汰已记账键 |
| `max_identifier_bytes` | 512 | task/response/item/event 的必要关联键 |
| `max_handshake_bytes` | 16 KiB | handler 消费的头/query 总预算，解析前检查 |
| `max_buffered_bytes` | 256 MiB | 全进程 WS 活跃重组/待写/掩码副本的总预算 |

全部必须为正。配置硬上界：单消息64 MiB、会话4096、用量记录65536、ID4096字节、
握手64 KiB、全局缓冲2 GiB；`max_message_bytes ≤ max_buffered_bytes`。乘法/加法在
执行前检查溢出，不用会话数乘单消息限额假装真实内存占用。
已知内联媒体仍受现有 `limits.max_inline_bytes` 的**单消息**限额，不累积整通电话。
WS 会话不套 HTTP `max_request_bytes`/Total；用户可显式提高单消息上限，但启动
日志须显示配置预算。

最多一个完整待写消息/方向加一个 reader 正在重组的消息，不排无界音频队列。
全局预算必须在分配前取得实际字节额度；重组扩容、受控model patch与客户端掩码
副本同时活跃时都记账，归还在真实不再持有后，不是消息入槽时提前释放。
不足额度立即以资源超限终止该会话，**不持有现有buffer等更多额度**，避免多个
半条消息互等形成预算死锁。传输选项提供最窄分配/归还 seam，不能读完整后补验。

这是 payload 缓冲预算，不是任意 JSON 树解析的峰值堆保证；解析器须避免为音频
base64和未知大数组再建多份通用树。关联键受数量/字节双限，元数据/Go容器开销
如实说明，不把256 MiB标成进程RSS硬上限。

## 5. 两种握手时序：Realtime 与 Inference

### 5.1 Realtime：模型在握手时已知

1. 验证 method/path/Upgrade/nonce/body/query 与预算，完成下游鉴权。
2. `Router.Resolve(model)` 给候选；矩阵按 `Inbound` 与已投放能力裁决。
3. 仅选同协议、同 profile 的 target；本包不会误选同矩阵存在但未投放的跨协议路径。
4. 候选内借凭据、构造干净握手、Dial。握手 auth/quota/rate-limit 等按统一
   Retryable 处置；同一target非Retryable不换key、可尝试下一同契约target。
   所有失败保留阶段/分类，安全清理后才能尝试下一候选。上游有HTTP响应并成功
   解码为可重试类型才据此冷却；裸握手协议错误与未知网络错误不可猜成auth。
5. 上游101/accept摘要/协商检查成功，再有界读取初始session.created并核验实际
   profile与初始能力（见§8）；其原始消息保留。均成功才向下游Accept。
6. 进入relay，先送出保留的初始消息；服务端在下游升级前已发出的后续消息仍
   保留在同一有界背压内，不丢预读字节。

### 5.2 Inference：不能凭空把模型挪进握手

原生客户端握手无 model，模型在**升级之后**的首条 `run-task.payload.model`。
不能既按首条消息选上游，又声称始终先 Dial 后 Accept。

本包采用**部署级显式绑定**：`websocket.inference_endpoint` 指向一个
`dashscope.ws.inference` Provider。仅配置一个这种 Provider 时可唯一推导；多个时
必须显式选择，不能根据配置顺序取第一个。该 endpoint 决定握手的主机、地域、
空间、profile 和凭据池；只有凭据可在握手期 failover，不假装已按模型选择 Provider。

- 下游先鉴权，按该协议入口的已兑现集合核对绑定 target，先 Dial 固定上游，再 Accept。
- 首个业务消息必须是合法 `run-task`；已读入消息不发出去前，按其逻辑 model
  调用 Router、矩阵。Resolved targets 必须包含**当前绑定 endpoint/profile**；
  否则发送关联 task_id 的可见错误并结束连接，绝不在 101 后换 Provider。
- 显式 alias 只改 `payload.model` 字符串 span；如 target 是通配规则，仍遵守现有
  UpstreamModel 配置，不自行把客户任意字符串当真实模型。
- 每个后续 `run-task` 都重新检查路由与能力，但只能使用同一绑定 endpoint/profile。
  同连接允许**串行不同模型/task**，前提是部署路由允许且上游契约具备证据。
- 新 task 必须在前一个 terminal 后开始；不承诺并发多个 task_id。run-task先注册
  有界pending；只有收到相同ID的task-started才进入active，active前的binary或
  continue-task拒绝，不由网关自动补顺序。finish-task发出不等于结束，仍允许
  该task的剩余结果/音频直到task-finished。已终结ID不复用，也不拿新run-task覆盖。
- task-finished不自动关 WS；task-failed原事件先完整转发，再按该已核验契约结束
  连接，未知错误码不触发retry。没有task_id的binary只按唯一active任务观测；
  如果关联不唯一则可见失败，不猜一个最近task。
- native 无 query 客户端不需要改连接代码；代价是一个入站部署绑定一个 Inference
  endpoint，不能按每轮模型跨地域/主机切换。以后多 endpoint 寻址是新设计，不偷偷加。

否决先 Accept 再等 run-task 才 Dial（丢失握手期 failover/鉴权错误语义）；否决
要求客户端新增 model query（改变原生协议、且后续 task 模型仍可改变）。

## 6. 鉴权、协商与首字节承诺

### 6.1 入站鉴权

服务端客户端沿用 Authorization/Api-Key。OpenAI 浏览器支持已知形式
`openai-insecure-api-key.<gateway-key>`，只能装**网关 key**，不能把云端临时/长期
key 当网关凭据使用。多个来源、重复头、多个 secret token 一律拒绝歧义；成功后
全部剥离，校验沿用常量时间逻辑。DashScope 本包不发明 key 子协议。

已知安全子协议仅 `realtime` 与明确 beta 信号；organization/project token 不从
客户端转发，未知变量 token 可见拒绝。浏览器 Origin 本包不提供通配跨域：增加
`websocket.allowed_origins`，缺省空表示拒绝带 Origin 的请求；服务端不带 Origin
仍可用。非空名单精确比对 scheme/host/port；不能以 Origin 替代 API Key。

### 6.2 RFC 握手需要补上的最窄生产约束

现有 `ws.Accept/Dial` 未完成子协议选择/校验；生产投放前必须补齐：

- Accept 校验唯一版本13、有效16字节 nonce、升级 token、无请求体/不支持扩展；
  验证必须在触达上游前完成，不能先消费一个上游会话再拒绝坏客户端。
- Dial 必核 101 的 Upgrade/Connection、唯一正确 Accept 摘要，拒绝未经协商扩展。
  若响应选择子协议，必须是本次确实提供的安全 token；不能靠状态101认定协商成功。
- 下游选择只使用客户端提出且配置允许的安全协议 token，绝不回显含 key 的 token。
  上下游是两段独立握手，不能假装对端已选择某值。无子协议客户端仍受版本profile治理。
- 失败握手响应体读取有界（64 KiB）并受握手期限约束；返回原 status/安全头/可分类
  code，禁止日志/网关生成错误回显原始带秘密的body或URL。
- 所有 dial/accept 前后的 nonce/bufio/取消职责保持；D1 CAS/join 修复不能退回未同步版。

具体新增 options 字段/失败响应缓冲方式由实施计划固定，现有签名不为这些要求
改成两套并存的 Dial/Accept。零值继续保持已有调用合法行为，生产明确启用限制。

### 6.3 承诺状态与重试

handler 显式区分未承诺、正在交接、已升级。开始 Hijack 或可能写101时即进入
不可重试状态；不能因 Accept 返回错误就重新拨号。没有 Hijacker 或验证失败
必须在上游尝试前失败。仅确定未发下游字节时才可换凭据/同契约target。

101 前本地失败按对应族HTTP错误信封回写；101 后不再写HTTP状态、不换上游。
Realtime 原生 `error` 消息原样转发，不把可恢复参数错误一律关会话；Inference
本地任务错误使用它自己的 task-failed 包络，不用 Realtime 的 type/error 信封。
未知上游错误只记 Internal/不可重试，不能以 close code 猜 auth/quota。

## 7. 双向转发、超时与进程回收

- 复用 `ws.Conn` 两段连接与完整消息接口；两 reader、两业务 writer、一个会话
  协调者固定数量，不每条消息起 goroutine；ping 发送器也为固定数量。
- 每方向一槽有界背压；业务 writer 顺序写。读取侧不持业务写锁，不等 append ack。
- Realtime 使用合法 UTF-8 JSON 文本包络；Inference 控制为 JSON 文本、音频为 binary。
  malformed/RFC方向掩码错误须可见终止；未知的合法同契约字段不被重序列化。
- `connect` 独立限制 TCP/TLS，`first_byte` 限制发请求到完整上游101及失败信封读取；
  从入站handler开始另设同一个`first_byte`整体建连预算，所有target/key尝试共用
  其剩余时间，不能每次重置而无限叠加。成功交接后清掉握手读写期限，再使用idle。
  **Total 对 WS 会话不适用**，不加会话总时长上限。
- `http.Server.ReadTimeout`可能在Hijack时留在socket上；Accept及错误清理需要显式
  设置/清除本阶段期限，真实生产Server用短Total但WS持续更长的回归证明不被误杀。
  下游101写有剩余握手预算，不能因对端不读而无界挂住Accept。
- `idle` 限读端帧间活性；生产写/pong/ping同样有有限写期限，避免对端不读而永久占锁。
  心跳每 `idle/2` 发传输 ping，pong由Conn消费；不伪造业务心跳或提交音频。
  音频两侧不同业务活性不能被误读成必须各方向都有业务消息。
- transport补最窄 Ping/写期限选项；不通过并发修改全局deadline打乱另一条写。
  客户端/上游发送的ping payload仍原样回应，非业务帧不计usage。写/pong/ping期限
  统一为`min(idle, connect)`且只在同一写锁内设置；Close礼貌收尾不超过现有1秒。
  所有read/ping/错误收尾共享写串行化；没有锁的前置状态查询来伪装发送成功。
- 已知合法JSON的schema/presence投影不是全对象重编码；文本完整消息须先通过
  UTF-8与基本结构/重复键检查，未知字段扫描跳过值不复制大base64。
  `ws.Conn`入站按角色校验mask方向，WriteFrame检测短写，close code/reason合法性
  在生成和接收两侧守RFC；现有纯frame单测语义不替真实角色连接检查。
- 真实 close 在另一段以相同合法code/reason尽力发送，再回收两端；本地错误用固定
  安全reason，不在日志记录原reason。raw EOF、半条消息+close不能记为完整结局。
  CloseWithResult只证明本次写帧，不能作对端接收证据。
- 一侧错误/取消/写超时立即终结会话、释放两端并join全部固定worker；纯backpressure
  不创建额外buffer。协调者必须避免在reader处理close的同时提前把另一段的指定
  close覆盖成1000，负例覆盖该竞争。
- 进程级会话registry先预占`max_sessions`（包含握手），拒绝后再拨号；封口和登记
  在同锁内，资源Close在锁外。`http.Server.Shutdown`不会关Hijacked连接，需
  `Built`显式WS关闭入口；main先停止新会话、取消pending、关闭active并join，再
  完成HTTP/metrics shutdown。禁止只依赖请求context或测试Server.Close。

## 8. 能力裁决与转正纪律

A 包不改三条路径的设计处置、Capability 或同源标记；后两包的矩阵不顺手改。
`IsHomogeneous`不是上游模型权限，也不是未经治理字段搬运的授权。

目标兑现集合沿用各协议现有 `ExpressibleSet`：OpenAI Realtime 12项、DashScope
Realtime 15项、Inference 6项。这是协议路径的保全目标，不说每个上游模型都能
完成所有业务；证据明确模型族/地域/profile。实际能力名称在投放时逐项列出，
不以数量或一条text hello自动背书其余项。

- 握手按门与基本会话能力裁决；`session.update`、`response.create`、媒体/工具和
  每条run-task发送前，按生产协议投影识别已建模能力并再次调用同一矩阵Check。
- 部分兑现时，未举证已知能力可见拒绝；不能通过unknown字段当作不透明负载绕过门。
  无法安全界定能力的未知命令或已知配置里的未知能力字段，在部分门上拒绝；
  **全目标集合已举证投放的同契约门**才允许未知合法命令/字段原样交上游处理，
  仍不声称其语义被本网关验证。
- 只核查client请求能力不够：session.created/updated可能带默认audio/VAD。
  上游初始配置必须在下游Accept之前有界读取并按实际启用能力裁决，保留原始
  消息待升级后送出；若初始状态不完整而门部分兑现则不开放该profile，避免
  用握手空能力集绕过默认能力。全目标兑现后仍核验profile包络与基本预算。
  session.created等待占同一first_byte建连预算，不新加会话总时长。
  后续服务端配置若新增未兑现能力，原配置不得被静默批准，101后可见终止。
  Inference无预发session配置，首run-task与每次task-started按其包络独立守门。
- profile不受支持、与客户端信号冲突或合同不同，在握手前拒绝，不静默翻译。
- 正式投放必须同时落生产注册、real recorded route fixture、两个显式白名单与
  `Redeem(ep,caps...)`、生成矩阵。矩阵/core gate正常工作，不用手工flag跳过。
- 矩阵对账当前只收`*Handler`/POST；应最窄扩展到携带协议身份与HTTP方法的
  WS handler，不让手写协议贴纸替实际handler身份背书。HTTP旧门/501兜底不变。
- 允许基础实现先合入但路径继续PLANNED；没有真实证据不能靠synthetic转正。

## 9. 用量与凭据生命周期

### 9.1 观测不参与负载重写

分别为 OpenAI、DashScope Realtime、Inference 提取必要usage，源消息原样送下游。
不能拿全部JSON填充泛IR，也不能因不能识别一种未来用量格式而编零值。
已知用量字段畸形/关联歧义应记录unavailable与固定诊断；不把opaque新字段当权威。

| 来源 | 记录键/单位 | 核算纪律 |
|---|---|---|
| Realtime `response.done` | 会话内 response_id + 来源；tokens及其原有分项 | 每笔只计一次；response.status为cancelled但有usage照实记 |
| 输入转写完成 | item_id/content_index + transcription来源；tokens或seconds | 与模型response独立，不因同item混账 |
| Inference ASR结果 | task_id + ASR来源；`usage.duration` seconds | 经当前契约确认是累计快照后取最大/差额，不逐事件相加 |
| Inference TTS句末/完成 | task_id + TTS来源；`usage.characters` characters | 累计值更新，同结束重复不双计，取消有权威值仍保留 |

此表按**已知消息来源/形状**列观测责任，不承诺三个profile返回相同字段。
尤其DashScope Realtime不是OpenAI usage结构的别名，需重新核官方contract与真实
response/transcription样本；未出现在该profile证据中的单位/字段保持unavailable，
不拿OpenAI字段名推定它存在。字段路径与存在性在实施计划的profile表精确固定，
新增模型族必须加相应记录/重复/中断用例，不靠同名response.done推断账单。

Seconds用现有Usage字段；Characters由最窄类型化观测记录独立承载，不塞入token。
`obs`新增非token计量入口及指标，标签仅固定协议/来源/单位/fidelity，不使用
session/task/model动态ID为Prometheus标签。原Tokens指标不改单位、不存价格。

每笔Fidelity显式；active未结记录在断连时标unavailable，不抹掉已收权威数字。
观测发生在完整上游消息实际收到之后，与下游写成功独立：客户端断开不能抹掉
已发生的上游权威用量。relay原始消息与记账事件分别确认，不以幂等close或业务
“done”代替权威usage；统计异常不能回写篡改上游payload。

累计账只发布增量或终值一次，保持相同recorded replay结果。累计值倒退、同一
终态重复却携带不同数字、非有限/负数等矛盾记录可见诊断并标不可知，保留已提交
权威历史，不发布负增量、不追加重复账。ASR计量是累计还是逐项由实际recorded
contract固定；无法判定时unavailable，不能仅见duration就默认累计。

去重键不LRU淘汰后再重计；**开启一笔生成/转写/task时就预留记录位置**，服务端
自动生成也在首次ID观测时预留。达到`max_usage_records`后拒绝开启下一笔；若
未知新ID的上游终态越过预留边界，明确终止并记录该笔unavailable/资源错误，不能
无声明漏账或偷偷多存一条。已经预留的终态完整观测/入账后尽力转发，不能为了
record上限丢掉其usage。没有开启事件而直接出现terminal仍按同一有界规则处理。
ID长度受限；并发ID/复用已terminal ID的含义按profile，不能猜task归属。

### 9.2 Lease

握手失败时当场`Fail(canonical.Error)`，每份tried只试一次。上游101成功后保留
Lease至会话结束：正常业务/客户端关闭/本地downstream失败按成功上游归还；
只有已明确分类的上游可重试故障使`Fail`冷却，未知close码不惩罚凭据。
若协议明确传回fatal auth/quota，可提前Fail但仍不在101后重试。Lease恰好结算一次，
沿用generation防乱序旧成功清掉新冷却；不新增并发数限制到凭据池。

## 10. 真实录制与投放证据

本包包含三条路径所需的**最小直连录制入口**，消费现有WS schema/来源账本。
录制器直连上游，不通过被测网关录取答案；只收独立写出的上游脚本及真实回复。
下游请求/预期、逻辑模型映射和四点来源断言独立声明；不由relay输出生成golden。

- 与现有smoke隔离：专用env opt-in + 精确record flag双门禁，默认不执行。命令、
  请求数/秒/字节/token/字符预算在录制计划里固定；所有调用前再取用户费用授权。
  本规范批准不等于授权云调用，不能承诺仅凭本地预算能强制限制云端美元账单。
- 模型/地域/profile必须逐项验证；`/v1/models`成功不代表WS权限、所有能力或beta可用。
- 样本默认使用经授权的短语音/图像与独立文本；真人录音、生成语音与图像的来源和
  使用许可明确。chirp只测DSP，不作为ASR/VAD/可懂度的证据。
- 握手去敏之外，消息文本、转录、工具参数、音频/图像、错误消息均需审核。只占位
  握手密钥不能证明具体出站凭据被替换；本机合成key负例另独立验证。
- 记录失败/中断/异常close与超预算；候选独占落盘、人工审核后入库，不覆盖旧fixture。
- fixture的`recorded`/SHA是自洽来源账本，不提供密码学云端认证。证据适用范围
  明确标在Note/Coverage，不能把单一模型参数回显当全平台支持。
- close.ForwardedFrom仍禁止；两侧code/reason独立精确断言，实际接收另外举证。
- Realtime完成轨迹以原 `response.done.response.id/status` 绑定终态；Inference以
  原 `header.task_id/event=task-finished` 绑定终态；同连接两task分别举证。当前
  WSTerminal允许指定ID/state pointer，不需要向真实payload注入虚构state。
  单纯session配置回显+close只能是机制证据，不能替代业务完成/音频能力。
- schema如果不能表达其它真实终态，先提出最窄schema修订并独立测试，不能
  篡改真实payload增加state或把TCP close冒充业务terminal。

## 11. 验收矩阵

| 场景 | 必须看到的证据 |
|---|---|
| 鉴权失败/重复key/secret query/子协议密钥 | 不触达上游；上游只收到池内凭据；下游与日志无secret |
| 浏览器Origin/子协议 | 精确许可，nonce与协商合法；token不回显；header客户端仍可用 |
| 路由/profile不符 | 101前可见错误，无投机版本fallback、无任意主机代理 |
| Inference无query原生客户端 | 先上游Dial再Accept；首run-task路由校验；task2仍绑定同endpoint |
| Inference跨endpoint/并发task/binary先于task | task关联拒绝，无101后重拨；无错task音频归属 |
| 失败握手后failover | 401/429与Retry-After分类，有限tried与期限；成功前下游没有101 |
| 部分101/Hijack失败 | 不再retry，释放已经取得的socket/Lease，不伪装HTTP可重试 |
| 同契约未知字段与presence | payload/opcode保全；缺席/null/false/0/空集合全不丢 |
| 允许的model alias | Realtime仅query；run-task仅一个model span，其余字节一致 |
| 音频、工具、VAD/会话配置、打断 | 每项真实轨迹+离线四点核对；配置有效值有证据，不伪造ack |
| 双向背压/ping/分片/写阻塞 | 有界槽、正确mask/pong、无原样frame承诺；取消能释放阻塞写 |
| 正常/异常close、半条消息 | 实际code/reason；不把partial/EOF当完整业务完成 |
| 多笔usage/重复/累计/取消/断连 | 不同来源单位、去重及权威历史保留；未结unavailable |
| record数/ID/消息/连接数超限 | 数字边界明确、可见失败，无无限buffer或无声明漏账 |
| SIGTERM、handler失败、迟到连接 | 停止登记、socket释放、所有worker join；HTTP Shutdown不掩盖Hijack |
| 全仓回归 | 原HTTP/SSE fixture/golden、矩阵/白名单/文档、普通/race均通过 |

对照义务按ADR-0003的建模事件子集维护：A包验证raw消息保全、独立语义预期与
只读投影/观测结果，不为三条同协议路径虚造生产Canonical重编码器。**这些断言
不冒充已经跑过“关闭快通道的跨协议转换路径”**。该实际对照属于C包转换实现，
需用同一组已录制上游轨迹逐项运行；A包交付记录明确这一后续义务，不将未完成
对照填PASS。不透明字段仅在同契约侧验证负载保全。Inference无已批准的异构路径，
按其独立命令/结果投影验证，不强造通用Realtime IR。

原则2.2/2.4/2.7与gateway/root说明需随实现独立文档提交澄清：不把OpenAI→DashScope
名字相似当同契约；HTTP tracked与WS承诺状态分开；Total不套WS。
其中2.2采纳门是A包投放的前置：书面说明同协议raw保全/独立语义对照及C包实际
转换对照的不同完成边界，不能不改原则却偷偷取消其当前对照要求。文档需独立评审，
本文不是已采纳的原则变更；如该边界未获批准，基础代码可完成但不宣告A包投放完成。

## 12. 交付顺序、成本与批准门

1. 先固定本规范和profile支持/Inference绑定规则；保持后两包与旧矩阵处置不变。
2. 实施计划分可独立验证任务：最窄传输补齐→StreamProvider/握手→共享relay与
   shutdown→协议观测/Inference纪律→录制与real fixture→逐能力兑现与生产注册。
   顺序是依赖关系，不预设一次PR全上；只加代码但未举证时仍PLANNED。
3. 沿用用户原生子代理实施＋独立复核，TDD与每任务账本；不使用Dots，不默认指定
   未获用户要求的模型。全部git带GIT_MASTER=1，离线CI不调用云端。
4. 实际录制前单独确认凭据可用性（不输出值）、样本许可、费用/数量预算。
5. 每条路径可独立投放，但必须逐项能力交付；遇上游不可录制保持缺口而非假绿。
6. 最后提交文档、fixture、规则和白名单；发布/合并/issue操作/树清理另需新授权。

维护成本主要集中在共享生命周期；profile与observer须随上游契约演进复验。
新增子协议/写期限/进程registry使传输与启动模块扩大，应以最窄接口守本机TCP回归；
不为未来跨协议路径提前创建泛状态机。Inference绑定限制是可见部署约束，未来
跨endpoint多模型会话需要新寻址设计，不能用一个参数默认掩盖。

**当前门**：请用户审阅本书面规范。批准后才写实施计划；计划审阅通过后才编码。
