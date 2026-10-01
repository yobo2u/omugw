# WS fixture 离线回放基座实施计划

> **供执行代理阅读：** REQUIRED SUB-SKILL：使用 `subagent-driven-development`
> （逐任务实施与复核）或 `executing-plans`（本会话实施）。按任务逐项完成
> `- [ ]` 检查。此计划需用户审阅并选择执行方式后才能开始编码。

**目标：** 为 testkit 建立有界的 WS fixture 格式、消息匹配和真实本地 TCP 双端回放，
为后续上游录制与生产网关接线提供可运行基座；本阶段不投放任何 WS 路径。

**架构：** 在现有 `Fixture.Response` 上增加 WS 分支，HTTP/SSE 格式和校验原样保留。
WS 数据包含四观测点、显式因果依赖与限定动态绑定；一个集中控制器管理状态，两端
各一个接收器读完整消息，借既有 `transport/ws` 处理帧层。上游请求与下游 golden
是独立预期，不能用被测转发器的输出生成输入答案。

**技术栈：** 仓库 `go.mod` 的 Go 版本，标准库 `net/http`、`encoding/json`、
`crypto/sha256`、`httptest`，已有 `transport/ws`；不新增模块依赖。

**设计来源：**
- `docs/superpowers/specs/2026-09-29-websocket-fixture-design.md`
- `docs/superpowers/specs/2026-09-29-websocket-fixture-acceptance.md`
- `docs/superpowers/specs/2026-09-28-realtime-minimal-session-contract.md`
- `docs/adr/0003-realtime-minimal-session-semantics.md`

**固定基线：** `bacf106ac00968df6acb6ab31e0e55d71be324a5`。
**执行前历史状态：** 此处在执行前是实施计划草案、未实现；当时不能把已合入设计文件算作 fixture 可用。

**2026-10-01 交付注记：** P1 八项本地任务已实现，统一最终修复尚待控制器 scoped 复核。
原清单保留执行前历史，不将 P2 真实录制器、P3 生产接线/能力投放或 DSP 写成完成。
当前覆盖与证据边界见 [分期交付状态](../../research/2026-10-01-ws-offline-foundation-delivery.md)。

## 全局约束

- CI 不需要真实凭据，网络仅使用本地 TCP。禁用裸 `make smoke` 与所有真实录制。
- 全部 Git 命令带 `GIT_MASTER=1`；开工 fetch、worktree list、status -sb，新分支执行。
- 注释与文档中文并解释所防故障；新增 ADR 节标题遵守仓库中文规范。
- 不修改 `rules_phase1.go` 的处置、`Redeem`、投放白名单或 `MarkHomogeneous`。
- 三个已知 WS 门纳入归属表，仅增强错绑校验，不代表 Mux 开门或协议已实现。
- `request.path` 不带 query；有损格子文件名约定保留。合成机制测试不进 routes 目录。
- 真实上游请求必须独立编写；来源标签/摘要只是可追溯性校验，不是密码学来源认证，
  101/200 也不能证明参数生效。没有真实记录与人工审核，不得宣称能力可投放。
- 同协议且同契约旁路验证消息负载，不能凭 `MarkHomogeneous` 允许未知跨协议语义。
- 合成扫频只测 DSP；参考重采样器与容差依赖音频议题，本阶段不提供音频宽松匹配。
- 本阶段仅读取和回放 fixture；录制代理/opt-in/独占落盘另写计划，生产 WS 接线另写计划。

## 评审重点

1. 假来源/篡改摘要不得凭标签通过投放证据检查；synthetic 只能做机制与负路径测试（任务 2、8）。
2. missing/null/false/0、重复 JSON 键、错误 ID 引用不能被“规范化”吞掉（任务 4）。
3. 双向消息等待不能被总线阻塞；只固定测试端调度，不承诺网关内部执行完全确定（任务 5、7）。
4. 关闭、超时、接收器错误与额外消息不能被“所有发送已完成”掩盖（任务 6、7）。
5. 内容、query、子协议中的敏感信息不能出现在失败日志，HTTP/SSE 的校验不能弱化（任务 1、3、8）。

## 阶段划分与覆盖账本

| 阶段 | 可验收的软件产物 | 本计划负责 |
|---|---|---|
| P1 离线基座 | 校验格式、因果调度、严格匹配、本地 TCP 回放、兼容原有 fixture | 是，任务 1–8 |
| P2 真实上游录制 | 精确 opt-in、直连上游代理、样本授权、去敏候选文件、不开费用默认 | 否，后续独立计划 |
| P3 网关端到端与投放 | StreamProvider/handler、101 边界、usage/矩阵通知、逐路径 fixture | 否，后续各路径计划 |

握手鉴权替换、语义确认/VAD/cancel/truncate/用量去重是 P3 的被测行为，基座提供
断言载体，不在本计划里实现这些生产语义。频谱容差/参考 DSP 尚未定案，异构音频
场景保留待证，不能用本阶段精确匹配替代它的验收。

## 文件职责

| 文件 | 职责 |
|---|---|
| `internal/testkit/ws_fixture.go` | WS schema 与有限数据类型 |
| `internal/testkit/ws_validate.go` | 图、大小、来源、互斥、完成条件校验 |
| `internal/testkit/ws_io.go` | 有界/严格 WS 文件与目录读取，不提供录制写入 |
| `internal/testkit/ws_handshake.go` | query/header/subprotocol 去敏与握手预期匹配 |
| `internal/testkit/ws_match.go` | 字节/JSON 匹配、限定改写与实体绑定 |
| `internal/testkit/ws_schedule.go` | 纯因果状态机与确定性测试端调度 |
| `internal/testkit/ws_upstream.go` | 本地上游回放握手与连接领取 |
| `internal/testkit/ws_replay.go` | 双接收器、调度控制、结束与资源回收 |
| 各文件对应 `*_test.go` | 小体积合成输入与真实 TCP 测试 |
| `testdata/testkit/ws/` | 明确 synthetic 的机制样例，绝不计入路径投放证据 |
| `internal/degrade/endpoint.go` | 三扇 WS 门常量与协议归属 |

不迁移/拆分无关 HTTP 代码；旧 `Fixture.Upstream` 不承载 WS 握手预期。

## 公共类型与界面（按所属任务交付）

任务 1 定义 `Response.WS *WSSession`，字段标签为 `json:"ws,omitempty"`。

```go
type WSPoint string
const (
    WSClientSend WSPoint = "client.send"
    WSUpstreamReceive WSPoint = "upstream.receive"
    WSUpstreamSend WSPoint = "upstream.send"
    WSClientReceive WSPoint = "client.receive"
)
type WSMessage struct { Opcode ws.Opcode; Payload []byte }
type WSNode struct {
    ID string
    Point WSPoint
    Source string
    After []string
    Kind string
    Message *WSMessage
    CloseCode *uint16
    CloseReason string
    Match string
    Fields []WSFieldRule
    ForwardedFrom string
}
type WSFieldRule struct {
    Pointer string
    Mode string
    Namespace string
    Symbol string
    Value json.RawMessage
}
type WSProvenance struct {
    Kind string
    RecordedAt string
    UpstreamProtocol string
    UpstreamVersion string
    Model string
    SourceSHA256 string
    SampleSHA256 map[string]string
}
type WSSample struct { Data []byte; SHA256 string }
type WSTerminal struct {
    Node string
    Namespace string
    Symbol string
    IDPointer string
    StatePointer string
    State string
}
type WSOutcome struct { Kind string; Terminal []WSTerminal }
type WSCoverage struct { Capability string; Nodes []string; Note string }
type WSSession struct {
    Version int
    ClientProtocol string
    ClientVersion string
    Upstream Request
    UpstreamExpectedStatus int
    UpstreamError json.RawMessage
    Provenance WSProvenance
    Samples map[string]WSSample
    Coverage []WSCoverage
    Nodes []WSNode
    Outcome WSOutcome
}
```

所有 JSON 标签写为 snake_case，字段必需性在校验器固定；`[]byte` 用标准 base64。
`Kind` 仅为 `message` / `close`；生硬断 TCP 的故障注入由本地测试的原始连接辅助器
实施，本阶段不为生产 Conn 增加 Abort 方法。握手失败测试用 WSUpstream 的配置返回
HTTP 错误，不混作应用消息。

模式必需性：Version=1；四个 point/Source/Kind/ID 必填；message 必须 Message
非 nil（text 非空且合法 JSON 或不透明 UTF8，binary 允许零长度）、CloseCode nil；
close 必须 CloseCode 非 nil、Message nil、Fields 空；After 可空。null 的结构指针
按缺席处理后执行该模式必需性，不把 null 自动变成合法 payload。WS session 必须
有非空 Note、ClientProtocol/Version、Upstream.Method/Path、Outcome.Kind。
message 的 Fields 只允许 Match=json，bytes 模式 Fields 为空。
completed 必须至少一项 Terminal + 两端的传输 close 结局已定义；failed 表示
预期非1000 close，必须有对应 send/receive close 节点；interrupted 表示没有
业务终态但预期有效 close，不等于 raw TCP EOF。handshake_failed 的 status
与错误对象必填、Nodes/Terminal 空、上下游均非101。本阶段 raw TCP 中断测试由
驱动注入并要求 ReplayWS 返回 error，不属于“合法 fixture 预期成功”的结局；
不伪造 close code=1006，也不造本阶段不定义的 error 节点。
Coverage 中 Capability 使用 canonical.Capability 的合法值，Nodes 非空且每个
引用存在、Note 非空；它只说明要验证什么，不会自行 Redeem 或证明实际能力。
synthetic 的 Coverage 描述机制场景，绝不能当真实能力来源。

默认预算由实现计划固定如下：文件 8 MiB、目录 64 MiB、完整消息解码后 1 MiB、
轨迹总解码负载 4 MiB、节点 4096、after 引用合计 8192、实体绑定 2048、字段规则
合计 8192、JSON 嵌套 64 层、全局回放 deadline 5 秒。它们只是 testkit 默认预算，
不改生产请求预算或 WS 会话时长。测试可注入更小值，不能设非正值关闭预算。

## 任务 1：WS schema 与门归属（独立可测）

**文件：** 创建 `ws_fixture.go`、`ws_fixture_test.go`；修改 `fixture.go` 的 Response
与 Validate；修改 `degrade/endpoint.go`，新增 `ws_endpoint_test.go`。
**输入：** 现有 Fixture/Request 与 `transport/ws.Opcode`。
**输出：** 上述类型；`Fixture.Validate` 在 WS 非 nil 时交任务 2 规则前先执行互斥、
GET/path/status 基本规则。三常量：`EndpointOpenAIRealtime`、
`EndpointDashScopeRealtime`、`EndpointDashScopeInference`，值是设计的三条路径。

最小红灯样例（其它字段用本任务的 `syntheticEnvelope` 测试助手填满，不调用生产编码器）：
```go
func TestWSKnownDoorOwnership(t *testing.T) {
    ep := EndpointOpenAIRealtime
    r := NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
        Pass(ExpressibleSet(ProtoOpenAIChat)...).
        Redeem(ep, canonical.CapTextGeneration)
    if _, err := r.Build(); err == nil { t.Fatal("WS 门错绑未拒绝") }
}
```
`Route.Build() (*Route, error)` 签名已核对。不要仅因不存在常量而止于编译红灯；
常量加入后必须观察错绑行为失败再实现归属规则。

- [ ] 写失败测试 `TestWSFixtureEnvelope`：字面量有效 synthetic WS，GET、不带 query
  的 path、101；Body/SSE/WS 任意双占拒绝；WS 与旧 HTTP Upstream 共存拒绝。
- [ ] 写 `TestWSKnownDoorOwnership`：每门兑给正确协议通过，兑给错误协议 Build
  失败；用本地 NewRoute，不给 Phase1 路径 Redeem。旧未知合成门继续通过。
- [ ] 运行 `go test ./internal/testkit ./internal/degrade -run 'TestWSFixtureEnvelope|TestWSKnownDoorOwnership' -count=1`，观察缺失类型/功能的红灯。
- [ ] 最小实现；HTTP/SSE Validate 分支不改，WS 完整校验随任务 2 接入。
- [ ] 运行两包全量测试；`make matrix` 仍通过且生成文档无差异。
- [ ] 提交 `testkit：增加 WS fixture 分支与三扇门的归属校验`。

## 任务 2：有界读取、图校验与来源账本

**文件：** `ws_validate.go`、`ws_io.go` 与各自测试；`ws_fixture.go` 加 Limits。
**输出：**
```go
type WSLimits struct {
    FileBytes int64; DirectoryBytes int64; MessageBytes int64; TraceBytes int64
    Nodes int; Edges int; Bindings int; FieldRules int; JSONDepth int
    Replay time.Duration
}
func DefaultWSLimits() WSLimits
func ReadWSFixture(path string, limits WSLimits) (Fixture, error)
func ReadWSFixtureDir(path string, limits WSLimits) ([]Fixture, error)
func ValidateWSSession(f Fixture, limits WSLimits) error
func WSContractDigest(s WSSession) (string, error)
```

- [ ] 写 `TestWSReadBudget`：用小限额，超文件/目录/decoded message/节点/边/规则/深度
  均拒绝；`-1`、0、无 WS、未知 schema、尾随第二个 JSON 对象拒绝；目录符号链接
  拒绝，按名称排序，无符合文件拒绝。文件读 `LimitReader(limit+1)`，解码前校验大小。
  WS 专用读取在结构体解码前执行重复 JSON key/JSONDepth 校验，再
  DisallowUnknownFields；所有模式必需字段缺席或不合法 null 拒绝；不改变旧 HTTP
  任意未知字段的既有读取纪律。
  同时修改旧 Load：先以通用 8 MiB 限额读，再分流，遇 WS 提示专用入口；现有
  HTTP/SSE fixture 均须通过此上限，不能只在文件已读完后才发现 WS。
- [ ] 写 `TestWSTraceValidation`：重复 ID、悬空 after、循环、after 与本流 FIFO
  矛盾、双 payload、非法 opcode/close code、query 混进 path、无终结条件拒绝。
  以点内出现顺序生成隐式 FIFO 边，与显式 after 一起检测环。覆盖映射的悬空
  引用/非法 capability/无说明拒绝；Terminal 的 IDPointer/StatePointer 指向合法
  字面预期或绑定，不能引用发送节点伪造“上游确认”。
- [ ] 写 `TestWSProvenanceDoesNotProveLiveSupport`：synthetic-negative 可运行机制
  测试，但不能声明 upstream-accepted/recorded 来源；recorded 要有时间、模型、
  两端版本和校验摘要。来源 kind 不合法拒绝，不从字符串内容猜来源。
- [ ] 运行 `go test ./internal/testkit -run 'TestWSReadBudget|TestWSTraceValidation|TestWSProvenance' -count=1` 观察红灯。
- [ ] 有效 success 轨迹要求 f.Response.Status=101、UpstreamExpectedStatus=101。
  独立握手失败 fixture 取 Outcome.Kind="handshake_failed"，两侧预期 status 显式
  记录，Nodes 和 Terminal 为空，UpstreamError 为固定的去敏字面错误对象。
  它验证的是本地握手错误载体，不承诺网关映射正确（P3 再验）。
- [ ] 实现。摘要只覆盖上游预期节点、上游回复节点、上游握手、终结声明及样本
  摘要的稳定 JSON 编码；排除 SourceSHA256 本身。收到“真实”标签但摘要不符拒绝，
  **摘要一致也不宣称来源真实**。样本存字节而非外部 URL，不引入路径下载；
  Samples 的每份字节做 SHA256，Provenance.SampleSHA256 必须与 Samples 对齐，
  样本也计入 TraceBytes。synthetic-negative 的四点 Source 全部为 synthetic；
  recorded 四点依次只许 authored / upstream-accepted / recorded / golden。
  synthetic 的 completed 表示机制成功，不表示上游能力得到证明。
- [ ] `Fixture.Validate` 将 WS 分支委托 `ValidateWSSession(f, DefaultWSLimits())`。
  旧 `Load` 保持 HTTP 入口；若遇 WS 则指示使用有界的 `ReadWSFixture`。不允许从旧
  Loader 未受限读入的数据绕过 WS 专用校验。
- [ ] 运行 testkit 全量；提交 `testkit：WS 轨迹有界读取与因果来源校验`。

## 任务 3：握手去敏和精确匹配

**文件：** `ws_handshake.go`、`ws_handshake_test.go`；不放宽 secretHeaders。
**输出：**
```go
func SanitizeWSHandshake(r Request) (Request, error)
func MatchWSHandshake(want Request, got *http.Request) error
```

- [ ] 写 `TestWSHandshakeSanitization`：Authorization/API key/Cookie 保留 redacted
  占位，重复 query token/key/access_token 全部剥离；URL userinfo 拒绝；子协议
  `openai-insecure-api-key.<secret>` 剥离，固定协议 token 保留。变量式未知敏感
  token 拒绝，不输出原串。安全 query/model 保留，头名不区分大小写。
- [ ] 写 `TestWSHandshakeMatch`：错 path/query/model/安全 header 缺失与额外 query
  均拒绝；自动 WS 必填头只验证合法性，不与录制 nonce 比较。fixture 的占位
  `<redacted>` 只证明存在，不能证明凭据正确；调用者在本地驱动中独立核对合成
  出站 credential 的字面值和下游不泄漏。基座不发送 `<redacted>` 作为真凭据。
  多值 Header 必须按原值集合处理，当前 map[string]string 无法承载的重要多值
  协商头在 Request.Headers 中用逗号 token 列表表达并逐 token 比较，不任意丢值。
- [ ] 运行两个失败测试；实现：query 按 ParseQuery 的键和值比较，保留重复值，
  不用字符串 contains 判断。错误仅输出字段路径。
- [ ] 全量 testkit 测试；提交 `testkit：WS 握手预期与凭据去敏`。

## 任务 4：严格消息匹配与限定 ID 绑定

**文件：** `ws_match.go`、`ws_match_test.go`。
**输入：** WSNode/WSFieldRule 与预算。
**输出：**
```go
type WSMatcher struct { /* 所有状态私有 */ }
func NewWSMatcher(limits WSLimits) (*WSMatcher, error)
func (m *WSMatcher) Match(node WSNode, actual WSMessage) error
func (m *WSMatcher) Materialize(node WSNode) (WSMessage, error)
func AssertWSForwardedPayload(sent, received WSMessage, rewrites []WSFieldRule) error
```

- [ ] 写 `TestWSMatchPreservesValues`：同 opcode/字节通过，截断 binary 失败；JSON
  missing/null/false/0/空串/空数组各异；额外字段、错误 usage、sample_rate、文本
  均失败。重复 JSON key/深度超限拒绝，数字用 UseNumber，不经 float64 舍入。
- [ ] 写 `TestWSBindingConsistency`：bind 首次登记，reference 必须已有绑定；call、
  task、item 命名空间隔离；同命名空间不同符号不绑定同实际 ID；预算 2 只允许
  两个实体，第三个失败。客户端产生 ID 与上游产生 ID 相同纪律。另测 client.send
  字面 task_id 首次 bind，随后 upstream.receive 必须 reference；错号失败，不能
  收到什么就重绑什么。
- [ ] 写 `TestWSOnlyDeclaredRewrite`：`/session/model` 的 equal-value 规则只准字面
  `logical` → `upstream-model`；其它字段不能一起删除或忽略；重复/相互覆盖的
  pointer 拒绝；ID 模式不能用于 usage、采样率或任意内容。
- [ ] 写 `TestWSForwardedPayloadKeepsUnknownBytes`：未知固定字段、键序与空白保留
  通过，整对象重新 marshal 改空白失败；只修改 /session/model 的 scalar span
  通过，改名单外任意 byte 失败；不定义规则时 bytes 全等。
- [ ] 运行红灯；实现三个规则模式 `equal` / `bind` / `reference`。
  `Match` 为 `bytes`（默认）或 `json`；二进制只允许 bytes。equal 允许改变的路径
  仍需精确期望值，不是 ignore。有动态绑定的 JSON 消息必须选择 json；
  bytes 规则不支持动态字段。json pointer 采用 RFC6901，无 wildcard，字段重写
  后剩余 JSON 仍须严格语义一致。string 模式不得匹配数字或对象；ID 路径只允许
  event_id/session_id/response_id/item_id/call_id/task_id/id 终端名，不能借命名空间
  去忽略 usage 或内容。timestamp 的具体规则不在本阶段，预期用固定值。
  同契约负载保全由接收节点的 ForwardedFrom 引用 send 节点，验证方向分别为
  client.send→upstream.receive 或 upstream.send→client.receive；原始 bytes 除
  equal 规则对应的 JSON 值 span 外全等（键序、空白也保留）。不能先 marshal
  整个对象再比较；须保留 token offset，单一 scalar 值才可 span 替换，本期拒绝
  object/array 改写。不支持的 span 必须显式失败，不能退回语义相等。
- [ ] 一次匹配失败不得写入部分新绑定；先验证临时结果，再原子提交。Materialize
  只替换显式 reference，bind 使用 fixture 的字面 ID（不生成随机ID）。发送动作
  在驱动中先标记 in-flight，Materialize 取已确认引用，WriteMessage 成功后调用
  matcher.Match(node, materialized) 原子提交发送端 bind，再 Complete；写失败不
  产生新绑定。对该发送因果依赖的接收暂存最多一条，等绑定提交后才核验。
  不调用网关转换器计算预期。
- [ ] 运行 testkit 全量；提交 `testkit：WS 消息严格匹配与有界关联绑定`。

## 任务 5：因果控制器与可重放的测试端调度

**文件：** `ws_schedule.go`、`ws_schedule_test.go`。
**输出：**
```go
type WSSchedule struct { /* graph, progress, seed 私有 */ }
func NewWSSchedule(nodes []WSNode, seed uint64) (*WSSchedule, error)
func (s *WSSchedule) Ready() []WSNode
func (s *WSSchedule) Start(id string) error
func (s *WSSchedule) InFlight(id string) bool
func (s *WSSchedule) Complete(id string) error
func (s *WSSchedule) Done() bool
```

- [ ] 写 `TestWSCausalScheduling`：配置链的四节点必须按必要因果完成，重复 Complete
  与未 ready 节点拒绝；独立客户端 send 与上游 send 用 seeds 1/2 可给不同发送
  次序；同一输入与 seed 的**纯调度**完全一致；严格链仅一种次序。状态固定
  pending→in-flight→completed；Start 只许 ready 节点（发送由控制器选中触发，
  接收由消息到达触发），Complete 需已开始；失败/取消一律终结回放不再次发送。
- [ ] 写 `TestWSScheduleDoesNotRequireReceiveOrderAcrossPoints`：两流各自 FIFO，
  跨流独立接收的完成顺序变化不导致跳过期望节点，Done 仅在全部节点消费后为真。
- [ ] 运行红灯；实现 Kahn 式就绪集合，稳定 ID 排序后用显式整数 PRNG 排列。
  不用全局 math/rand，不依据墙上时钟挑事件，不穷举全部拓扑序（4096 节点不能
  用指数计数）。报告 `serial` 或 `multiple`，不是伪精确的总执行序列数量。
- [ ] 网络驱动只能控制自己发出的次序。跨方向接收时机可能不同，不能因为相同
  seed 就断言实际到达日志完全一样；结果按节点归一后比较。额外消息立即失败，
  正确下一个消息若仅因发送 Write 尚在返回期间到达，可留一个有界未确认槽，
  发送成功后才能完成依赖；不得默默接受没有任何已开始前置动作的消息。
- [ ] 全量测试；提交 `testkit：WS 因果调度与方向内 FIFO`。

## 任务 6：本地上游握手回放与错误结局

**文件：** `ws_upstream.go`、`ws_upstream_test.go`。
**输出：**
```go
type WSReplayUpstream struct { /* 连接槽与错误槽有界，字段私有 */ }
func NewWSReplayUpstream(f Fixture, limits WSLimits) (*WSReplayUpstream, error)
func (u *WSReplayUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request)
func (u *WSReplayUpstream) Connection(ctx context.Context) (*ws.Conn, error)
func (u *WSReplayUpstream) Err() error
func (u *WSReplayUpstream) Close() error
```

- [ ] 写 `TestWSUpstreamHandshake`：httptest 本地 TCP，GET/path/query 精确命中升级
  101；wrong route 触发断言错误不是普通404；匹配失败不劫持；重复连接拒绝。
  握手 401/429 负例由任务1定义的 f.Response.WS.UpstreamExpectedStatus 固定，
  错误信封保留在单独 UpstreamError JSON 字段；不能自动回 101。
- [ ] 写 `TestWSUpstreamConnectionCancellation`：等待 Connection 可被 context
  取消；Close 重复安全，连接槽容量 1，Connection 只领取一次。Err 保存首次
  错配/重复连接错误，领取连接后再来的错误仍可读，最终 fixture 驱动必须检查
  Err；不能只有 Connection 前的错误影响结论。Close 汇总已有 Err，关闭所有
  已接受连接（即使还在槽里未领取），禁止 Close 后新连接进入。mutex 中登记
  所有权、标已关，锁外关闭 Conn，Accept 与 Close 竞争中晚到连接立即释放。
- [ ] 运行红灯；用既有 ws.Accept，不自行实现帧编解码；默认 idle=0，回放总
  deadline 由任务7控制。选用 httptest.NewServer 的真实 TCP，禁 net.Pipe。
  子协议握手的安全 token 按预期比较；当前 ws.Accept 不回协商子协议，因此本
  阶段测试不假装验证了子协议选择，协议选择扩展留生产接线计划。
- [ ] 全量测试；提交 `testkit：本地 WS 上游握手回放端`。

## 任务 7：双端回放、终态与有界收尾

**文件：** `ws_replay.go`、`ws_replay_test.go`。
**输出：**
```go
type WSReplayEndpoints struct { Client *ws.Conn; Upstream *ws.Conn }
type WSReplayResult struct { Nodes []WSNode; SendOrder []string; Outcome WSOutcome }
func ReplayWS(ctx context.Context, f Fixture, peers WSReplayEndpoints,
    seed uint64, limits WSLimits) (WSReplayResult, error)
```

- [ ] 写 `TestWSReplayFourPoints`：手写 4 个 message 节点，真实本地 TCP 双端
  bridge；每端只一 goroutine 调 ReadMessage，控制器唯一调用 matcher/schedule。
  正常 text 与 binary payload 原样通过，Message 节点使用有限 channel 容量 1。
  端点来自本计划的上游 Accept 和测试客户端 Dial，二者 MaxPayload 必须为
  limits.MessageBytes；ReplayWS 的调用方负责这一前置条件（现有 Conn 无 limit
  getter，不能声称事后匹配能防止提前分配）。加入实际超限消息的原始 frame TCP
  测试，MessageBytes=64、预期小消息、实际128，ReadMessage 必须拒绝。
- [ ] 写 `TestWSReplayTwoSeeds`：两个方向同时可发的独立命令，seeds 1/2 形成不同
  **控制器 SendOrder**，两者均通过并有相同按节点归一的结果；不比较网络到达
  时间戳。`-count=20` 不是此测试的替代。
- [ ] 写 `TestWSReplayDoesNotApproveBrokenBridge`：故意丢字段/发额外消息/错 opcode/
  改错 model 的测试 bridge 必须失败，给出 node/field 路径，不输出实际 payload。
- [ ] 写 `TestWSReplayTermination`：收齐消息还须完成显式 close 结局；正常 close
  无业务终态不算 completed。expected failed/interrupted 分开，未知 close code
  不默认为成功；raw TCP 网络断开/超时必须返回 error，不能匹配 interrupted 的
  预期有效 close；消费完后仍 drain 到有界预期关闭，
  禁止忽略尾部额外消息。
- [ ] 写 `TestWSReplayReleasesBlockedPeers`：一侧写阻塞时 100ms 注入 deadline，
  两端 Close、接收器归还完成信号，1 秒内结束，不用 time.Sleep 等“应该结束”。
- [ ] 运行红灯；在 parent deadline 与 limits.Replay 中取更早者。控制器不能同步
  阻塞在 WriteMessage：每端一个固定 writer worker，任务 channel 容量1，同时
  最多一个 in-flight 发送；控制器继续消费 read/write 结果及 ctx.Done。
  所有 worker 的 channel 投递都 select ctx.Done，不能 Close socket 后仍卡在
  channel send。独立取消监控收到 deadline 后先关两端唤醒写，等待2个 reader
  和2个 writer 退出；不要靠增加 sleep 或 timeout 躲避死锁。
- [ ] Close 的现有行为须明确：主动 ws.Conn.Close 发关闭帧后马上关闭本地 TCP；
  被动 ReadMessage 收到 close 会自动回应1000并关闭。轨迹以 send close 表示
  主动动作，以另一端的 receive close 观察为证据，不要求主动端再观察一个对端
  close。不能为无法观察的自动回应补造节点；自动回应码的传输精度归 ws 独立
  测试。本阶段 Completed close 固定1000，failed close 指定非1000有效码。
  本地双向 bridge 测试需分别覆盖客户端先 close、上游先 close，驱动观察 code
  与实际可见边界一致，且两个 read workers 的 EOF 不误写 completed。
- [ ] WSTerminal.Node 必须引用匹配过的接收 message 节点，要求的状态/关联值
  对应其 JSON pointer 字面预期；基座不自己决定各供应商的业务终态或用量。
  不能凭 Outcome.Kind="completed" 直接填 PASS。
- [ ] 运行 `go test ./internal/testkit -count=20` 与 testkit/ws race；提交
  `testkit：双端因果回放与失败收尾`。

## 任务 8：兼容门禁与阶段交付

**文件：** `internal/testkit/ws_integration_test.go`、`testdata/testkit/ws/`；
`internal/degrade/ws_fixture_gate_test.go`；更新 testkit AGENTS.md。
若机制加载器需要业务 mock，它放测试文件，不添加生产测试专用方法。

- [ ] 写机制字面 fixture 样例：hello/工具ID/二进制/同连接两 task，与非法轨迹
  样例。来源为 synthetic-negative，Note 写明证明的是机制而非上游能力，不能
  冒充 `recorded`；期望值手工固定，不从运行结果计算。
- [ ] 写 `TestWSFixtureFitsExistingGate`：临时目录、本地测试 Route。有效 `GET +
  request.path` 对门通过，带query/错绑失败；有损格子缺同名 JSON 失败。继续
  用旧 checkRouteFixtures 读 request.path，不靠 WS provenance 自动放行。
- [ ] 写 `TestHTTPFixturesUnaffectedByWSSupport`：回放全部既有 HTTP/SSE fixture
  与原 golden，保持 upstream 的 method/path/body 要求；不修改旧 golden。
- [ ] 运行 `make check`、`make test-race`、`make matrix`、git diff --check，查看
  完整退出码，不用 tail 管道掩盖失败。
- [ ] 更新职责列表与 WS 例外：握手无 body，消息预期在 WS 分支；保留旧 HTTP
  请求体门槛和脱敏名单。标注真实来源审核仍由录制计划/人工负责，加载器不能
  识别一个人手工伪造的相同哈希。
- [ ] 提交 `testkit：WS 基座整合回归与证据边界说明`；逐提交测试并完成整分支
  审查。不能因此关闭“WS 举证已交付”、宣告云互通或 Redeem。

## 自检与后续交接

本计划覆盖验收清单中格式/门禁/因果/匹配/预算/本地连接收尾；生产模型改写、
鉴权替换、配置生效、101 failover、cancel/truncate 与 usage 计量归 P3；来源实际
抓取、候选独占落盘与金额预算归 P2；异构音频容差归音频议题。后续计划必须读本
账本补齐缺口，不把 P1 完成当成全部 WS 方案已完成。

计划获批后推荐**本会话实施（Native）**：8 个任务共享类型与因果状态机，集中
实施可以降低接口漂移；任务中间频繁运行测试，整分支再用独立 reviewer 复核。
若用户选择 subagent-driven，则每任务给 fresh worker 的上下文包含本计划全部
公共类型/预算与此前任务 Interfaces，不复用带旧实现假设的 reviewer。

**执行前历史：当时等待用户审阅并选择执行方式，仅有计划、尚未编码。**
当前分期交付以开头 2026-10-01 注记为准；清单未勾选保留原实施步骤，不表示 P1 仍未实现。
