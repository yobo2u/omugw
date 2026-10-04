# S3 音频任务子阶段 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 离线交付 DashScope Inference ASR/TTS 串行任务保全、累计观测及独立有界录制器，独立复核验收后暂停。

**Architecture:** 使用原 relay、registry、termination 和 Lease，增加双向写前/写后 policy 与一个固定 supervisor。
唯一4096 task表同时承担关联、交付状态和累计观测；Inference 握手无 model、无预读，整门六项门槛保留。

**Tech Stack:** Go 1.25、现有零依赖 WebSocket transport、Prometheus、realtimejson、testkit、本地 TCP/httptest。

**Spec:** `docs/superpowers/specs/2026-10-04-dashscope-inference-s3-audio-design.md`（接口/枚举的唯一详细定义）。

## Global Constraints

- 仅 `audio/asr/recognition`、`audio/tts/SpeechSynthesizer`；拒绝 multimodal-dialog/听悟，六项 ExpressibleSet不降。
- 不 Redeem、不改兑现白名单/生产routes；默认 Build三WS门404、默认矩阵501；验收后暂停，不称整个S3投放。
- Go 1.25；不增加直接依赖；中文注释说明所防故障；本地TCP，不用net.Pipe。
- 一个4096 task总表（含历史/active/计量），ID/model≤512字节；未知模型不按后缀猜计量。
- W=min(Connect,Idle)，B=min(1s,Connect,Idle)，G=min(100ms,(D-t0)/4)；Dall=tfailed+W+B，只收紧。
- awaiting-start=FirstByte，draining=max(FirstByte,Idle)，默认120s；是网关预算，不是SLA；HTTP total不套WS。
- 原两readers不复制；最多一个额外固定supervisor；禁止每task/message goroutine、无界队列与临时大RawMessage。
- 所有原文、控制payload、CloseError、必要本地错误分配前计额，退出预算0；Stop policy等待先于close/join，不先断transport。
- 普通测试禁止live、凭据和公网；控制器执行真实实验；S1八槽永不复用。此计划中的测试/commit由实施者执行，起草不执行。

## Review Focus

- 客户端读完started/finished而写后回调未运行：等待交付门闩，不误拒绝/提前转发；Task4、5。
- 上游failed后1000、空close、EOF或shutdown竞争：原文优先、业务失败不被wire成功覆盖；Task4、6。
- 握手目标同名却换BaseURL/pool、wildcard改名：新任务零上游发送、固定身份不漂移；Task5、6。
- 累计回退6→3→6、末态null、token+兼容duration：不重复收费、不伪造零/seconds；Task1、5。
- timer旧revision/finish旧generation、容量满、budget不足本地错误：不影响下个task，退出无遗留；Task3、4、5。

---

## 文件布局、执行顺序与交接约定

| 任务 | 生产文件责任 | 测试/证据责任 |
|---|---|---|
| 1 | `internal/protocol/dashscopeinference/{inspect,contract,failure}.go` | 同名 `_test.go` |
| 2 | `internal/provider/dashscopeinference/provider.go`；`internal/config/gateway.go` | provider_test.go、gateway_test.go |
| 3 | `internal/transport/ws/{close_handshake,conn,budget}.go` | close_deadline_test.go、local_message_test.go |
| 4 | `internal/gateway/{ws_policy,ws_termination}.go`；ws_relay.go、ws_registry.go | ws_policy_test.go、ws_termination_test.go及旧relay/registry测试 |
| 5 | `internal/gateway/{ws_inference_policy,ws_inference_usage,ws_inference_binding}.go`；`internal/obs/ws_cumulative.go`、metrics.go | 三个对应gateway测试及obs/ws_cumulative_test.go |
| 6 | gateway ws_inference_profile.go、ws_profile.go、ws_handler.go、ws_dispatch.go、build.go、build_ws.go | ws_inference_handler_test.go、ws_inference_conformance_test.go、三门/SDK旧回归 |
| 7 | `tests/smoke/inference_recording_{config,io,capture,scenario,candidate}_test.go`、record_inference_live_test.go | inference_recording_offline_test.go、research验收/录制指南 |

顺序1→2→3→4→5→6→7；每任务可独立拒绝review，不能以邻任务尚未实现替当前公开签名写空壳。
所有 Task Interfaces 消费 spec 中同名固定类型，禁止重新发明变体。Task4以 fake policy 验证通用机制，
Task5直接调用真实policy验证状态，不依赖完整handler；Task6才汇合。Task4迁移旧relay调用点以保持可编译。
执行前按 AGENTS 三条git前置核对；当前已是工作分支，避免另开并行实现树覆盖同一接口。
每任务以下命令均须实际执行并保存RED/GREEN/race结果；review发现缺陷先修再推进。

### Task 1: 只读Inspector、精确contract表与安全本地信封

**Files:** Create `internal/protocol/dashscopeinference/{inspect,contract,failure}.go` 及各自 `_test.go`。

**Interfaces:**
- Consumes `realtimejson.Parse([]byte) (Value,error)`、`Value.Field(string) (Value,error)`、`Text(int)`、`Count()`；不改扫描器语义。
- Produces `InspectClient([]byte) (ClientFacts,error)`、`InspectServer([]byte,ModelContract) (ServerFacts,error)`、
  `VerifiedTaskID([]byte) (string,bool)`、`LookupModelContract(string) ModelContract`、`ValidateTaskContract(ClientFacts) error`。
- Produces `TaskFailureSize(string,LocalFailureKind) (int,error)`、`PutTaskFailure([]byte,string,LocalFailureKind) error`、
  `ClassifyClose(uint16,string) *canonical.Error`；所有Facts/枚举/字段按spec§3，供Task4/5/6消费。

- [ ] **RED：写 `TestInferenceInspectEnvelope`。** 表格断言：missing/null/value/错类型、转义重复header/task_id/model、
  512/513字节、孤立代理项、非UUID合法ID；未知input/output大字段原bytes不变，返回值不借用原切片。
- [ ] **RED：写 `TestInferenceExactContracts`、`TestInferenceInspectUsage`、`TestInferenceLocalFailureEncoding`。**
  对spec每个精确键逐项核Unit/Mode/SentenceEndOnly；近似后缀unknown；multimodal-dialog/听悟拒绝；
  tokens 2+3=5通过，缺项/溢出/负数/重复为Invalid；duration兼容不参与token；0≠null/missing；
  InvalidParameter安全bad_request、其他internal/不可重试；四种固定错误逐字断言、非法ID不编码、size精确含转义。
- [ ] **RED：写 `TestInferenceInspectBounded`、`TestInferenceCloseClassification`。** 4MiB未知字段分配增量≤256KiB；
  已释放/改写raw后Facts独立，畸形JSON/嵌套/重复字段不panic；1000/1001无额外失败，其余安全internal且不可重试，reason不被复制进错误。
- [ ] **运行RED：** `go test ./internal/protocol/dashscopeinference -run '^TestInference' -count=1`，预期缺符号/上述断言失败。
- [ ] **实现上述接口。** 只扫指定层，完整错误返回零Facts；usage坏值不拒绝合法事件。
  TaskFailureSize不分配编码payload，Put只写提供的精确dst；记录官方链接于contract.go，精确表不扩展成模型发现。
- [ ] **GREEN：** `go test ./internal/protocol/dashscopeinference ./internal/protocol/realtimejson -count=1`，全部PASS。
- [ ] **race：** `go test -race ./internal/protocol/dashscopeinference ./internal/protocol/realtimejson -count=1`，全部PASS。
- [ ] **自审/commit：** 查raw/任意map驻留、非法JSON悄悄零值、未知型号猜账；`git diff --check`通过后，
  `git add internal/protocol/dashscopeinference`；`git commit -m "feat: 新增 Inference 音频任务只读契约"`。

### Task 2: 唯一Inference连接配置与固定Provider

**Files:** Create `internal/provider/dashscopeinference/{provider,provider_test}.go`；Modify `internal/config/{gateway,gateway_test}.go`。

**Interfaces:**
- Consumes `provider.Request`、`ws.Dial(context.Context,string,ws.DialOptions)`、既有canonical/HTTP错误分类。
- Produces `config.InferenceProvider([]config.ProviderSpec) (*config.ProviderSpec,error)`；Config.validateGateway消费同一函数。
- Produces `New(config.Timeouts,config.WebSocket,*ws.BufferBudget) *Provider`、`(*Provider).Kind() degrade.Provider`、
  `(*Provider).Dial(context.Context,provider.Request) (*ws.Conn,*http.Response,error)`、`ValidateHeaders(http.Header) error`。

- [ ] **RED：写 `TestInferenceProviderHandshake`、`TestInferenceProviderRejectsUnsafeInput`、`TestInferenceProviderBounds`。**
  本地TCP断言固定路径/前缀无重复无query、空UpstreamModel；替换Authorization、UA=omugw、两个DS白名单；
  Origin通过、Cookie/未知头不转发；subprotocol/重复auth/坏URL/错inbound/kind非空model在拨号前拒绝；
  非101响应64KiB上限、无原body/URL/key回显，W/idle/connect接线，HTTP401/429分类保持。
- [ ] **RED：写 `TestInferenceProviderUniqueConfig`。** 零个合法、一个返回独立值、两个即使同URL/pool也失败；
  与Realtime/HTTP共存通过；不要从models反推唯一provider，配置而未引用的第二个也拒绝。
- [ ] **运行RED：** `go test ./internal/config ./internal/provider/dashscopeinference -run '^TestInference' -count=1`，预期缺接口/断言失败。
- [ ] **实现固定Provider及配置函数。** 不抽象万能协议Provider，不动Realtime首事件；对握手错误只保留安全分类。
- [ ] **GREEN：** `go test ./internal/config ./internal/provider/dashscopeinference -count=1`，全部PASS。
- [ ] **race：** `go test -race ./internal/config ./internal/provider/dashscopeinference -count=1`，全部PASS。
- [ ] **自审/commit：** 查零拨号负例计数、凭据清洗/失败body所有权，`git diff --check`；
  `git add internal/config/gateway.go internal/config/gateway_test.go internal/provider/dashscopeinference`；
  `git commit -m "feat: 增加唯一 Inference 出站握手"`。

### Task 3: 共享绝对关闭守卫与计额本地Message

**Files:** Modify `internal/transport/ws/{close_handshake,conn,budget}.go`；Create `close_deadline_test.go`、`local_message_test.go` 同目录。

**Interfaces:**
- Consumes既有 `Conn.closeDeadline`、`watchTransportClose`、`abortTransport`、BufferBudget.acquire/release、Message.Release。
- Produces `(*Conn).ArmCloseDeadline(time.Time) (func(),error)`、
  `AllocateMessage(*BufferBudget,Opcode,int,func([]byte) error) (*Message,error)`。
- 保持 `BeginClose(uint16,string,time.Time) (bool,func(),error)` 与 CloseWithResult签名；改为共用最早期限/同一守卫。

- [ ] **RED：写 `TestArmCloseDeadlineOnlyTightens`。** Arm不发close/不封普通写；后设更晚期限无效，
  更早生效；零deadline安全失败；重复Arm与BeginClose只一守卫；多finish幂等join。
- [ ] **RED：写 `TestArmCloseDeadlineCoversBlockedIO`。** barrier占写锁、慢本地TLS、自动pong/peer-close回应，
  均不能越过绝对D重新获得1s；不取消readCtx也到D释放；读close事实与reason所有权仍可交接。
- [ ] **RED：写 `TestAllocateMessageReservesBeforeFill`。** 不足额度fill从未调用；fill错/非法UTF8/负size无泄漏；
  size精确入账，浅拷贝Release一次；真实ID含转义的本地错误经Task1直接填充无预算外副本。
- [ ] **运行RED：** `go test ./internal/transport/ws -run 'TestArmCloseDeadline|TestAllocateMessage' -count=1`，预期FAIL。
- [ ] **实现两接口和旧close迁移。** write/ping/pong设置期限取min；守卫覆盖写锁等待/TLS，最早期限CAS与守卫更新同步。
  守卫生命周期固定每连接一份；无每Arm新goroutine。AllocateMessage在acquire后make，fill失败立即release。
- [ ] **GREEN：** `go test ./internal/transport/ws -count=1`，所有传输旧/新测试PASS。
- [ ] **race：** `go test -race ./internal/transport/ws -run 'TestArmCloseDeadline|TestAllocateMessage|Test.*Close' -count=1`，PASS。
- [ ] **自审/commit：** 查重复守卫、TryLock旁路/临时数组峰值、自动close重新计时，`git diff --check`；
  `git add internal/transport/ws`；`git commit -m "fix: 共用 WebSocket 绝对关闭期限与消息预算"`。

### Task 4: 双向relay hook与同一termination的失败/期限仲裁

**Files:** Create `internal/gateway/{ws_policy,ws_termination,ws_policy_test,ws_termination_test}.go`；
Modify `ws_relay.go`、`ws_registry.go`、`ws_handler.go`、`ws_dispatch.go`、`build.go`、`build_ws.go`
及旧relay/registry/termination调用测试（旧handler/Build仅签名迁移）。

**Interfaces:**
- Consumes Task1 `dsi.TaskFailureSize/PutTaskFailure`，Task3 `ArmCloseDeadline/AllocateMessage`。
- Produces spec§5全部 `wsForwardPolicy`方法及decision/ticket/deadline/end/options值类型；
  `relayWS(context.Context,*ws.Conn,*ws.Conn,*ws.Message,wsRelayOptions) error`（前两Conn依次downstream/upstream）。
- 保持 `wsTermination.report(wsRelayResult)`、`close() error`；新增 `policyEnd(wsPolicyEnd,bool) (bool,func(error))`。
- Produces `newWSTermination(func(uint16,string,time.Time),time.Duration) *wsTermination`、
  `(*wsTermination).attachRelay(*ws.Conn,*ws.Conn,wsRelayOptions) error`、`closeWSConnections([]*ws.Conn,uint16,string,time.Time)`。
- Produces `newWSRegistry(int,time.Duration) *wsRegistry`、`(*Built).initWebSockets(config.WebSocket,config.Timeouts) error`；
  duration为B，pending阶段也生效。registry/dispatch/relay/Build调用本task同改；Task5不调用termination。

- [ ] **RED：写 `TestWSRelayPolicyDeliveryBarriers`。** fake policy实现完整接口，阻塞started/finished After前的显式barrier：
  已收消息后的客户端输入只保留一条计额message；Before/After各一次，失败After收到writeErr，Release在After后。
  run Before已登记才Write，旧finish After不改下一generation；Realtime无policy继续原Observer语义。
- [ ] **RED：写 `TestWSRelayPolicyTimerAndStop`。** 无业务仅ping也触发Expire；Changed容量1无丢状态，
  旧revision不终止新阶段；等待policy门闩时shutdown先Stop，1001实收，不能提前断TCP或卡join。
- [ ] **RED：写 `TestWSRelayFailureDeliveryAndPeerClose`。** 原文failed先于close；上游静默/EOF/1000/自定义/空close，
  G内peer原码原reason保全，1005不上网；结果始终失败、Lease/业务诊断一次；G外本地1011不冒称peer。
- [ ] **RED：写 `TestWSRelayFailureAbsoluteBudget`、`TestWSRelayLocalFailureBudget`。** W=80ms、B=80ms、G≤20ms：
  慢下游/写锁/failed+shutdown/close竞争最迟Dall释放（调度容差普通200ms、race500ms）；记录期限确未重置；
  合法512字节ID可安全错误后close，非法ID只有close；额度不足不生成payload，全部Message/CloseError回收。
- [ ] **运行RED：** `go test ./internal/gateway -run '^TestWSRelay(Policy|Failure|LocalFailure)' -count=1`，预期FAIL。
- [ ] **实现新relay序列与共享仲裁。** Before→旧Observe→Write→After→Release；一个固定supervisor仅调期限。
  policyEnd写前抢业务槽并arm两段；原文delivery门闩完成后等G，原reader保留peer close；本地项由唯一close owner写。
  下游写mutex不包After；Stop解policy等待后才能等失败交付/close，cancelRead最后。保留第一业务槽+独立wire槽。
  所有守卫/worker join再Finish；迁移旧handler为Policy=nil、Observer原值、Timeouts/Budget/Limits真实值。
- [ ] **GREEN：** `go test ./internal/gateway -run 'TestWSRelay|TestWSRegistry|TestWSHandlerLease|TestWSProfile' -count=1`，PASS。
- [ ] **race：** `go test -race ./internal/gateway -run 'TestWSRelay|TestWSRegistry|TestWSHandlerLease|TestWSProfile' -count=1`，PASS。
- [ ] **自审/commit：** 画失败原文、local待交付、peer reason的唯一owner；查registry不抢物理close、无第二reader，
  `git diff --check`；`git add internal/gateway`；`git commit -m "feat: 共用双向任务门控与失败关闭仲裁"`。

### Task 5: Inference单表状态机、模型绑定与累计指标

**Files:** Create `internal/gateway/{ws_inference_policy,ws_inference_usage,ws_inference_binding}.go`及各 `_test.go`；
Create `internal/obs/{ws_cumulative,ws_cumulative_test}.go`；Modify `internal/obs/metrics.go`。

**Interfaces:**
- Consumes Task1全部Facts/contract、Task4完整wsForwardPolicy接口、`Router.Resolve`、`Matrix.Check`。
- Produces `newWSInferenceBinding(router.Target) (wsInferenceBinding,error)`、
  `validateWSInferenceModel(*router.Router,*degrade.Matrix,wsInferenceBinding,string) error`。
- Produces `newWSInferencePolicy(wsInferenceBinding,*router.Router,*degrade.Matrix,*obs.Metrics,config.Timeouts,func() time.Time) *wsInferencePolicy`，实现Task4接口。
- Produces `obs.WSUsageUnit`、`WSUsageDelta` 与
  `(*Metrics).ObserveWSUsageDelta(string,string,WSUsageDelta)`、`ObserveWSUsageRecord(string,string,WSUsageUnit,canonical.Fidelity)`。

- [ ] **RED：写 `TestInferencePolicyTaskTransitions`、`TestInferencePolicyDeliveryGenerations`。** 直接驱动Before/After，
  run预登记→瞬回started，started写失败不放binary；finish未After时finished可接收；terminal交付后才复用；
  迟到finish ticket、历史terminal/failed不改下一任务；同代超时后的started After不复活；out无finish、TTS尾音保全；
  ASR上行/TTS下行binary分别守门，draining只收尾音、不再接受上行输入，failed宽限不重结账。
- [ ] **RED：写 `TestInferencePolicyUnknownAndBinding`。** 同ID缺省/相同model未知扩展字节保全；idle未知动作、
  双run、异ID/model、Null重申、换endpoint/URL/pool、wildcard别名、multimodal-dialog/听悟均零上游交付；
  未知服务端事件不充started/terminal；所有出现model均经过路由与整门核验。
- [ ] **RED：写 `TestInferencePolicyDeadlinesAndCapacity`。** 注入now直接Expire：start=FirstByte、drain=max，
  out在started到达起算；ping/结果不续期；旧revision无效；4096含已结，4097/重复run写前拒绝；Stop解除等待。
- [ ] **RED：写 `TestInferenceCumulativeUsage`、`TestObserveWSCumulativeMetrics`。** 6→13→13数字13/记录1，
  6→3→6冻结6/unavailable1；terminal0有权威零，null不造数；A完成B中断保A及B增量；未知/未证实不发布数；
  token+兼容duration无seconds；并发Finish幂等，回退/overflow/NaN非法，Requests不随delta变；无动态ID/model标签。
- [ ] **运行RED：** `go test ./internal/gateway ./internal/obs -run 'TestInference|TestObserveWSCumulative' -count=1`，预期FAIL。
- [ ] **实现binding及policy。** 唯一4096表+active引用、小锁、generation/revision各司其职；锁外metrics与等待。
  历史只留spec的小事实，模型名仅当前保存；过时After不能写当前记录，Stop禁止新增task但保留失败关闭期尾消息。
- [ ] **实现累计指标及结算。** 合法快照即发布非负delta，终态/Finish一次record；冲突冻结，原事件不吞。
  增task/unknown白名单与准确Help；旧ObserveWSUsage/Characters/Seconds和ws_usage.go保持fixed-terminal语义。
- [ ] **GREEN：** `go test ./internal/gateway ./internal/obs -run 'TestInference|TestObserveWS|TestWSUsage|TestOpenAIWS' -count=1`，PASS。
- [ ] **race：** `go test -race ./internal/gateway ./internal/obs -run 'TestInference|TestObserveWS|TestWSUsage|TestOpenAIWS' -count=1`，PASS。
- [ ] **自审/commit：** 查第二索引、历史模型驻留、锁内metrics、累计伪terminal、冷却未知错误；`git diff --check`；
  `git add internal/gateway/ws_inference* internal/obs`；`git commit -m "feat: 实现串行 Inference 任务与累计计量"`。

### Task 6: 固定三门正式接线与全链路conformance

**Files:** Create `internal/gateway/{ws_inference_profile,ws_inference_handler_test,ws_inference_conformance_test}.go`；
Modify `ws_profile.go`、`ws_handler.go`、`ws_dispatch.go`、`build.go`、`build_ws.go`及相邻profile/build测试；
Create `testdata/testkit/ws/inference-audio/{asr-duplex,tts-duplex,sambert-out,two-tasks,failed-close}.json`。

**Interfaces:**
- Consumes Task2 config.InferenceProvider/Provider，Task4 relayWS/wsRelayOptions，Task5 binding/policy构造器。
- Produces `NewDashScopeInferenceHandler(WSDeps) *WSHandler`；WSDeps新增 `InferenceTarget *router.Target`。
- wsProfile新增 `readyMode wsReadyMode`（wsReadyEvent=0、wsReadyHandshake）；只有固定Inference profile取后者。
  原newObserver留给Realtime；serve按固定inbound构造Inference Policy，二者互斥。
- `wsReady`新增 `target router.Target`，initial可nil；connect先选择固定目标或Resolve，再复用同一凭据循环。

- [ ] **RED：写 `TestInferenceHandlerHandshakeAndBinding`。** 101前上游不发应用消息仍可升级；query/model/空问号拒绝，
  预检/整门失败零Dial；run后校验失败WS task-failed/1008；升级后不重拨；无模型别名，不串凭据/租户。
- [ ] **RED：写 `TestInferenceHandlerLifecycle`。** 首轮成功次轮failed、先finished后断线、慢交付/阶段超时、
  pending/active shutdown；failed+1000不记ok；HTTP total不截活跃WS，预算/registry0、Lease一次且未知失败不冷却。
- [ ] **RED：写 `TestInferenceConformanceReplay`、`TestInferenceThreeDoorBuild`、`TestInferenceProductionDoorsRemainClosed`。**
  五个独立字面synthetic样例用ReadWSFixture/ReplayWS走正式provider+handler；无虚构ready prelude，严格opcode/bytes；
  三门混合测试矩阵、错误profile拒绝、部分六项批准拒绝；默认三WS404、默认Inference Matrix.Check501。
- [ ] **运行RED：** `go test ./internal/gateway -run '^TestInference(Handler|Conformance|ThreeDoor|Production)' -count=1`，预期FAIL。
- [ ] **接线固定构造器。** Build装Provider与唯一Target；buildWSDoors增加Inference case且仍checkWSDoor；
  生产Build不传wsEndpoints。Inference跳过readWSReady，绝不填“永远成功”checkReady；nil initial安全Release。
  start+FirstByte涵盖两101，握手成功解除ctx；ready.target固定身份给policy，Lease仍原handler一次结算。
- [ ] **GREEN：** `go test ./internal/gateway ./internal/testkit ./internal/degrade -count=1`，全部PASS。
- [ ] **race：** `go test -race ./internal/gateway -run 'TestInference|TestWSRelay|TestWSRegistry|TestWSConformance|TestWSProfile' -count=1`，PASS。
- [ ] **自审/commit：** 查测试开门未流进默认Build/矩阵/生产routes、三门身份只由handler派生；`git diff --check`；
  `git add internal/gateway testdata/testkit/ws/inference-audio`；`git commit -m "feat: 接入 Inference 音频子契约离线链路"`。

### Task 7: 独立有界录制器、验收和证据缺口文档

**Files:** Create `tests/smoke/inference_recording_{config,io,capture,scenario,candidate}_test.go`、`inference_recording_offline_test.go`、
`record_inference_live_test.go`；Create `docs/research/2026-10-04-dashscope-inference-s3-{recording,acceptance}.md`。
Modify `.gitignore` 精确忽略 `/.local/ws-recordings/s3-audio-20261004/`，确认testdata仍可跟踪。
仅控制器审核真实材料后可增加 `testdata/fixtures/dashscope-inference/`，普通测试不依赖这些待录文件存在。

**Interfaces:**
- Consumes现有ws.Dial/BeginClose、testkit.ReadWSFixture/ValidateWSSession/WSContractDigest；禁止import被测gateway/Inspector/Provider。
- Produces `inferenceRecordConfig(root string,getenv func(string) string) (inferenceRecordingConfig,error)`、
  `inferenceReserve(cfg inferenceRecordingConfig) error`、`inferenceCapture(context.Context,inferenceRecordingConfig) (inferenceRecording,error)`、
  `inferenceSave(output string,recording inferenceRecording) error`，均包内固定工具接口。
- config字段：Root/Output/Batch/Scenario/Endpoint/Model/Voice/SamplePath/SampleSHA256/Key/ManifestPath string，Slot int，
  Manifest inferenceRecordingManifest（固定字段按spec§8.1，字符串时间用RFC3339、数值上限用int64、Slots固定[6]项）。
  固定限制写代码，不能环境扩额。recording含Records []inferenceRecord、Started/Ended time.Time、Outcome string；
  record含Direction string、At time.Duration、Opcode ws.Opcode、Payload []byte、CloseCode uint16、CloseReason string、PeerClose bool；持有量受spec限制。

- [ ] **RED：写 `TestInferenceRecorderOfflineLimits`。** 默认/普通smoke开关不拨号；固定6槽、10task/270s批次上限；
  Dial前持久占槽，失败消耗槽，换目录名不复活S1或S3；并发reserve唯一、symlink/已存在输出拒绝。
  消息/节点/轨迹/音频/文本上限按spec；超限在发送/分配前失败；45s整会话截止含关闭，无自动重试。
- [ ] **RED：写 `TestInferenceRecorderOfflineScenarios`、`TestInferenceRecorderOfflineEvidence`。** 六场景本地脚本独立字面期望：
  无ready预读、started后才音频、out无finish、两task因果；首完成后中断保真；failed后真实close/EOF/静默分开，
  原始bytes/时间/SHA不重造；鉴权脱敏；费用上界证明缺失/地域不符/未知模型禁止live，候选缺终态不能completed。
- [ ] **运行RED：** `go test ./tests/smoke -run '^TestInferenceRecorderOffline' -count=1`，预期FAIL且公网Dial=0。
- [ ] **实现固定配置、持久reserve与采集。** 新batch=`s3-audio-20261004`，spec§8.1六槽映射不可改名跳过；
  live只读环境：`OMUGW_RECORD_DS_INFERENCE=1`、`OMUGW_DS_INFERENCE_SLOT`（1..6）、
  `OMUGW_DS_INFERENCE_MANIFEST`（绝对路径）、`OMUGW_DS_INFERENCE_OUTPUT`、`DASHSCOPE_API_KEY`；Key永不落盘。
  root下 `.local/ws-recordings/s3-audio-20261004` 唯一reserve根，output只能为该根指定slot子目录，禁止改batch/绕槽。
  manifest写固定地域/model/voice/sample SHA、最坏费用计算≤1元/余量，缺失停；一次只跑指定槽，不批量自动补跑。
  直连独立请求、独占原始输出、peer与local close分槽；raw/candidate上限分别检查，不修改任何S1工具/ledger。
- [ ] **实现live入口 `TestRecordDashScopeInference`（仅 `//go:build smoke`）。** 先完整预检→reserve→一次capture→save；
  自测只编译禁live路径，不运行该测试。录制指南写控制器单槽命令及六槽输入/失败停机规则，不提供裸make smoke。
- [ ] **GREEN：** `go test ./tests/smoke -run '^TestInferenceRecorderOffline' -count=1`，PASS且无凭据/公网。
- [ ] **race/禁live：** `go test -race ./tests/smoke -run '^TestInferenceRecorderOffline' -count=1`，PASS；
  `go test -tags=smoke ./tests/smoke -run '^TestInferenceRecorderOffline' -count=1`，PASS，live测试未被选中。
- [ ] **写验收文档。** 分列文档/合成/live/原生保全/计量/六能力证据缺口；无实录写“未测”，
  multimodal-dialog、听悟、text_generation、未证实累计与生产投放列未完成；说明完成本子阶段后暂停。
- [ ] **自审/commit：** 检查控制器单槽上限可执行、同型号/样本不可暗换、来源不自证、费用估计不是账单硬限；
  `git diff --check`；`git add tests/smoke/inference_recording* tests/smoke/record_inference_live_test.go docs/research/2026-10-04-dashscope-inference-s3-recording.md docs/research/2026-10-04-dashscope-inference-s3-acceptance.md`；
  `git commit -m "test: 增加独立有界 Inference 录制与条件验收"`。

## 控制器最终关口与暂停

- [ ] 逐task独立复核后做跨task签名/所有权扫描：T1→T5解析、T2→T6固定目标、T3→T4绝对D、T4→T5门闩与timer、T5→T6一次结算。
- [ ] 全分支review后执行 `make check`、`make test-race`；确认矩阵生成文档无差异、旧HTTP/SSE与S1/S2固定终态账不变。
- [ ] 执行 `make test-sdk` 与 `go test -race -tags=sdk ./internal/gateway -run '^TestOpenAIRealtimeNodeSDK' -count=1`，PASS；
  SDK依赖按 `tests/sdk/openai-realtime` 锁文件准备，缺依赖不可记为验收通过。
- [ ] 重跑录制器禁live普通/race定向验收；控制器持现有DS凭据按已记录的独立S3有界方案取得证据，缺口如实保留。
- [ ] PR写清本子阶段条件验收/未完成项，具备相应证据后才merge；验收后暂停，不进入下一子契约或生产Redeem。
