# S3 当前子阶段：DashScope Inference 音频任务设计

日期：2026-10-04。状态：控制器已自审，作为既有 A 包音频子阶段实施依据；不是实施或投放证明。
代码参照 `0a533a5`；起草工作树 `82f9a31`，包含远端 main，后续差异仅为文档。
依据：[A 包已批准设计](2026-10-02-same-contract-websocket-passthrough-design.md)、
[tracked 契约研究](../../research/2026-10-04-dashscope-inference-s3-contract.md)、
控制器输入 `.superpowers/sdd/2026-10-04-dashscope-inference-s3/contract-closure-report.md`。
本文完整固化该报告的已采纳裁决，执行者不依赖 ignored 目录才能理解接口。

## 1. 交付范围与停止点

- 当前只接 `audio/asr/recognition` 与 `audio/tts/SpeechSynthesizer`，使用原生 run-task 外层。
- `multimodal-dialog`、`tingwu-meeting-realtime` 及其他任务三元组在 run 写上游前明确拒绝。
  前两者即使伪装 audio 三元组也拒绝；它们需要各自就绪/终态契约，不能借未知事件准入。
- 保留 Inference ExpressibleSet 六项：text_generation、streaming、audio_input、audio_output、
  speech_synthesis、speech_recognition。不缩集合、不 Redeem、不改兑现白名单或生产 routes。
  转录、翻译、TTS original_text 均不能充当 text_generation 的独立能力证据。
- 完成本音频子阶段实现、独立复核、验收后暂停。整个 S3、A 包与生产三门投放均仍未完成。
  真实调用仅由控制器执行；当前实现者交付离线实现和独立有界录制工具。
- Go 1.25；不增加直接依赖；中文注释说明所防故障。两段应用消息保全 opcode、原字节、方向内顺序。

长期维护点只有精确模型契约表与共享 relay 生命周期；新增型号需增加官方依据和离线反例，
共享 relay 改动须带 S1/S2、SDK/TLS 回归。120 秒 draining 政策可能截断大文本积压，验收中单列。

## 2. 文件责任与依赖

| 责任 | 文件（新增，除标“改”） | 实施任务 |
|---|---|---|
| 小事实、presence、精确模型/用量表、安全本地错误编码 | `internal/protocol/dashscopeinference/{inspect,contract,failure}.go` | 1 |
| 固定 URL、干净握手、唯一配置连接身份 | `internal/provider/dashscopeinference/provider.go`；改 `internal/config/gateway.go` | 2 |
| deadline-only 守卫、有界本地消息所有权 | 改 `internal/transport/ws/{close_handshake,conn,budget}.go` | 3 |
| 双向 hook、固定 supervisor、同一终止仲裁器 | 新 `internal/gateway/{ws_policy,ws_termination}.go`；改 `ws_relay.go`、`ws_registry.go` | 4 |
| 单一 task 表、绑定与累计计量 | `internal/gateway/{ws_inference_policy,ws_inference_usage,ws_inference_binding}.go`；`internal/obs/ws_cumulative.go`，改 `metrics.go` | 5 |
| 固定 profile、无预读握手、正式装配函数 | `internal/gateway/ws_inference_profile.go`；改 `ws_profile.go`、`ws_handler.go`、`ws_dispatch.go`、`build.go`、`build_ws.go` | 6 |
| 独立录制与来源/关闭证据 | `tests/smoke/inference_recording_*_test.go`、`record_inference_live_test.go`；研究验收文档 | 7 |

测试紧邻责任文件；完整接线样例在 `testdata/testkit/ws/inference-audio/`，真实审核后材料在
`testdata/fixtures/dashscope-inference/`。原 `ws_usage.go` 保持 S1/S2 fixed-terminal 语义。

## 3. Inspector 与精确契约

只借用 `realtimejson.Value` 扫描，不驻留 RawMessage、output、音频或任意字段 map。
顶层/header/payload 必须为对象。关联键含 JSON 转义等价重复均拒绝；usage 内重复/非法只诊断，
保全消息。字符串界限：task_id/model 各 512 字节，action/event/三元组各 128 字节，streaming 16 字节。
ID 有效 UTF-8、非空，不强制 UUID；其他身份值非空且无控制字符；error_code最多128字节。孤立代理项不作有损替换。

协议包的 producer 接口（别名 `dsi`）：

```go
type Presence uint8 // Missing, Null, Value, Invalid；零值 Missing
type TextField struct { Presence Presence; Value string }
type BindingFields struct { Streaming, Model, TaskGroup, Task, Function TextField }
type ClientFacts struct { Action, TaskID string; Binding BindingFields }
type UsageUnit string // UnitUnknown="unknown", UnitTokens="tokens", UnitCharacters="characters", UnitSeconds="seconds"
type UsageMode uint8 // ModeUnverified=0, ModeCumulative
type ModelContract struct { Unit UsageUnit; Mode UsageMode; SentenceEndOnly, CompatibilityDuration bool }
type UsageSnapshot struct {
    Presence Presence
    InputTokens, OutputTokens, TotalTokens, Characters int64
    Seconds float64
}
type ServerFacts struct {
    Event, TaskID string
    Binding BindingFields
    Usage UsageSnapshot
    Failure *canonical.Error
}
func InspectClient(raw []byte) (ClientFacts, error)
func InspectServer(raw []byte, contract ModelContract) (ServerFacts, error)
func VerifiedTaskID(raw []byte) (string, bool)
func LookupModelContract(model string) ModelContract
type LocalFailureKind uint8 // LocalPolicy, LocalUnsupportedTask, LocalStartTimeout, LocalDrainTimeout
func TaskFailureSize(id string, kind LocalFailureKind) (int, error)
func PutTaskFailure(dst []byte, id string, kind LocalFailureKind) error
```

`InspectClient` 校验包络和字段 presence；run 的五个绑定字段必须 Value，其他动作允许 Missing，
但显式 Null/空/错类型非法。非法时返回零 Facts；`VerifiedTaskID` 独立检查唯一 top/header/task_id，
仅为安全本地错误恢复 ID，不把其他非法字段变成准入。服务端绑定字段可 Missing，出现即核验。
`InspectServer` 只在 result-generated 与 task-finished/task-failed 读取 `payload.usage`；
SentenceEndOnly 仅限制中间 result-generated（须 output.event=sentence-end），终态不受该过滤。
未知事件不推进状态、不生成计量。token 三字段齐全、非负 int64、和无溢出且 total=input+output；
characters 非负 int64；seconds 有限非负 float64。缺项不补零，partial token 组为 Invalid。
已确认单单位契约出现另一种上述计费字段的非null值为Invalid；仅Qwen3.1 ASR明确豁免兼容duration。
未知字段不递归分类；未知/未证实模式不因同名字段获得累计授权。CompatibilityDuration仅在
精确登记的Qwen3.1 ASR两型号为true，不把“token单位”等同于“任意duration均可忽略”。

### 3.1 初始精确 model 表

以下每个反引号名字均为独立精确键，没有后缀/前缀推断。表不是可调用模型目录；未知型号经
任务三元组、路由等值验证后可保全，但计量为 unknown/unavailable。别名滚动只代表该精确名字
在引用文档下的契约；不由网关解析到猜测的快照版本。

| 精确模型键 | 原单位 / 模式 | 中间快照 | 官方依据（研究索引） |
|---|---|---|---|
| `qwen-audio-3.0-asr-flash-streaming` | seconds / cumulative | result-generated | Qwen ASR Streaming 服务端 |
| `qwen-audio-3.1-asr-flash-streaming`、`qwen-audio-3.1-asr-flash-message` | tokens / cumulative | result-generated；忽略兼容 duration | Streaming / Message 服务端 |
| `cosyvoice-v1`、`cosyvoice-v2`、`cosyvoice-v3-plus`、`cosyvoice-v3-flash`、`cosyvoice-v3.5-plus`、`cosyvoice-v3.5-flash` | characters / cumulative | sentence-end | CosyVoice 服务端 |
| `qwen-audio-3.0-tts-plus`、`qwen-audio-3.0-tts-flash` | characters / cumulative | sentence-end | Qwen TTS 服务端 |
| `sambert-zhichu-v1` | characters / cumulative | result-generated，无 sentence-end 前提 | Sambert 服务端 |
| `paraformer-realtime-v1`、`paraformer-realtime-v2`、`paraformer-realtime-8k-v1`、`paraformer-realtime-8k-v2` | seconds / unverified | 不发布数字 | Paraformer 服务端：累计口径未闭合 |
| `fun-asr-realtime`、`fun-asr-realtime-2025-11-07`、`fun-asr-realtime-2026-02-28`、`fun-asr-realtime-2025-09-15`、`fun-asr-flash-8k-realtime`、`fun-asr-flash-8k-realtime-2026-01-28` | seconds / unverified | 不发布数字 | Fun-ASR 服务端：累计口径未闭合 |
| `qwen-audio-3.1-tts-flash` | tokens / unverified | 不发布数字，终态也不猜重叠 | Qwen TTS 服务端 |
| 其他（包括 Gummy、未逐名登记的 Sambert） | unknown / unverified | 不发布数字 | 需逐型号补证；价格表不替代 wire usage |

任务准入由显式三元组与 streaming 决定：ASR 仅 duplex；TTS 支持 duplex/out。
上述已知 Sambert 键只允许 out；已列 CosyVoice/Qwen TTS 键只允许 duplex；已列 ASR 键只允许 ASR。
这些固定绑定事实放 `contract.go` 的 `ValidateTaskContract(f ClientFacts) error`，不把模型发现接进路由。

### 3.2 安全失败

上游完整合法 task-failed 原文保全。只将已核实 `InvalidParameter` 分为 bad_request/不可重试；
其他 header.error_code 暂统一安全 internal/不可重试，不记录原 message/code/ID。
Inference `ClassifyClose(code uint16, reason string) *canonical.Error` 仅1000/1001返回nil，
其余返回安全internal/不可重试；不从Realtime的1011 reason借鉴权/额度规则。
合法wire code/reason仍保全；失败分类不能因“reason没有已知含义”而误记成功，业务失败另槽保留。

本地文本固定为 `{"header":{"event":"task-failed","task_id":<合法ID>,"error_code":<固定码>,"error_message":<固定文本>},"payload":{}}`。
四种固定 code/message 分别为 `Gateway.InvalidTask` / `invalid task envelope or binding`、
`Gateway.UnsupportedTask` / `task contract not implemented`、`Gateway.TaskStartTimeout` /
`task did not start within gateway budget`、`Gateway.TaskDrainTimeout` / `task did not finish within gateway budget`。
只对 VerifiedTaskID 成功或已登记任务构造；ID 不合法只发 close。编码大小≤4096 且≤消息上限，
TaskFailureSize 计算 JSON 转义后的精确长度，Put 直接填已经计额的 dst，无预分配编码副本。

## 4. 固定连接与整门边界

`config.InferenceProvider(providers []ProviderSpec) (*ProviderSpec, error)` 返回独立值副本，零个返回 nil，
两个及以上直接错误（即使同 URL/pool）；`Config.validateGateway` 与 `buildWithWS` 均调用，防直接 Build 绕过。
新增 Provider 的 `New(config.Timeouts, config.WebSocket, *ws.BufferBudget) *Provider`、
`Kind() degrade.Provider`、`Dial(context.Context, provider.Request) (*ws.Conn,*http.Response,error)`、
`ValidateHeaders(http.Header) error` 与既有平行接口匹配。固定追加 `/api-ws/v1/inference`，
保留部署前缀、避免重复路径、无 model/query；握手 Target.UpstreamModel 必须空。
凭据替换、白名单头、Origin/扩展处理、64 KiB 头/失败体与 URL 拒绝规则沿用 A 包。

在 `WSDeps` 加 `InferenceTarget *router.Target`：唯一配置投影，含 kind/endpoint/BaseURL/pool，
UpstreamModel 为空。握手无任何 query（连空 `?` 也拒绝）；拨号前检查整个六项集合，
上游 101 后立即下游 Accept，不调用 readWSReady。Realtime 仍强制 query model+首事件预读。
固定内部枚举 `wsReadyMode` 仅 `wsReadyEvent`（零值）、`wsReadyHandshake`，由三个固定 profile 设置。

`type wsInferenceBinding struct { Kind degrade.Provider; Endpoint, BaseURL, CredentialPool string }`；
`newWSInferenceBinding(target router.Target) (wsInferenceBinding,error)` 拒绝缺字段、错 kind、非空 UpstreamModel。
`validateWSInferenceModel(rt *router.Router, matrix *degrade.Matrix, binding wsInferenceBinding, model string) error`
对每次声明 model（含未知动作/事件）Resolve，须至少一个候选全部连接字段相等、UpstreamModel==model、
NativeEndpoint 为空，再作整门 Matrix.Check；失败返回固定安全错误，不回显 Router 的动态消息。
同连接可以换真实模型，但只在前任务已交付终态后用新 ID run；不得换 endpoint、URL 或凭据池。

## 5. 同一 relay 的窄双向 policy

新增 policy 专用于串行任务准入/交付；没有任意消息改写、网络或插件注册能力。

```go
type wsDirection uint8 // wsClientToUpstream, wsUpstreamToClient
type wsForwardStep uint8 // wsStepNone, wsStepRun, wsStepStarted, wsStepFinish, wsStepTerminal, wsStepFailed
type wsForwardTicket struct { Generation uint64; Step wsForwardStep }
type wsForwardAction uint8 // wsForward=0, wsForwardThenEnd, wsRejectThenEnd
type wsLocalTaskFailure struct { TaskID string; Kind dsi.LocalFailureKind }
type wsPolicyEnd struct {
    At time.Time
    Failure error // 仅固定哨兵或不含原文的 canonical.Error
    Code uint16
    Reason string // 固定本地安全 reason
    Local *wsLocalTaskFailure
    PeerGrace bool
}
type wsForwardDecision struct { Ticket wsForwardTicket; Action wsForwardAction; End *wsPolicyEnd }
type wsPolicyDeadline struct { At time.Time; Revision uint64 }
type wsForwardPolicy interface {
    BeforeForward(context.Context, wsDirection, ws.Opcode, []byte) (wsForwardDecision, error)
    AfterForward(wsForwardTicket, error)
    Deadline() wsPolicyDeadline
    Changed() <-chan struct{}
    Expire(wsPolicyDeadline, time.Time) *wsPolicyEnd
    Stop()
    Finish()
}
type wsRelayOptions struct {
    Observer wsEventObserver
    Policy wsForwardPolicy
    ClassifyClose func(uint16, string) *canonical.Error
    Timeouts config.Timeouts
    Budget *ws.BufferBudget
    MaxMessageBytes int64
}
func relayWS(context.Context, *ws.Conn, *ws.Conn, *ws.Message, wsRelayOptions) error
```

顺序是 Before→观测（Realtime Observer，S3 则在 Before 内）→Write→After→Release；
每个成功返回的 ticket 都 After 一次，包括关闭竞争取消该次转发（传固定取消错误）。
After同时核对generation与该Step对应的交付阶段；同代任务已超时/Stop也不能被迟到成功回调重新激活。
wsStepNone 不改变状态。policy 只接收借用 bytes，不持有 Message；门闩等待时仅该方向 worker
持有这一个已计额消息，不读下一条。policy 锁内只做小状态变更，等待、网络、metrics 均在锁外。
Changed 是容量1的合并通知；Deadline 返回当前绝对期限及独立 revision，零时间表示无阶段期限。
Expire 只处理与当前 revision/期限相同且已到期的一次超时；旧 timer 不得击中下一阶段/任务。

原两个 readers、两心跳 worker 保留。仅 policy 非 nil 时多一个固定 supervisor：select Changed、
单个可复用 timer、stop；它只调用 Expire 并认领共享终止，不读 socket，不开每 task/message goroutine。
本地错误由唯一 termination.close owner 同步写出（可能是 relay 协调者或 registry 关闭者），
不是 supervisor 另起 reader/writer。downstream 应用写共用交付 mutex；锁外执行 After，
close owner 在仲裁中封住后续普通写、等待已有写完成，再写本地失败。绝对守卫覆盖等写锁。

Stop 幂等：关闭客户端准入并解除所有 policy 门闩，唤醒 supervisor；不取消 transport readCtx。
Before 对内部封口只返回专用 `errWSPolicyStopped`（不匹配 `context.Canceled`）；relay 只释放当前
Message 并退出该 worker，不 report 此信号。failed/reject/Expire 的原 End owner、After 写失败的
原 worker（携带原写错）仍负责认领唯一 termination；Stop 的关闭 owner 已有终止原因，Finish 在
join 后封口。真实 `ctx.Err()` 仍原路上报，不能用“忽略取消”代替内部封口区分。
task-failed 的匹配上游尾消息在被动等待内仍可保全；其他终止已封普通应用写。
Finish 在所有 forward/supervisor join 后调用一次，幂等结未结计量。Realtime Policy=nil，
仍由旧 Observer.Observe/Finish 处理，不能将 Realtime error 自动变为终止。

### 5.1 单一 task 表与交付阶段

`newWSInferencePolicy(binding wsInferenceBinding, rt *router.Router, matrix *degrade.Matrix, metrics *obs.Metrics,
timeouts config.Timeouts, now func() time.Time) *wsInferencePolicy` 实现上述接口，nil now 用 time.Now。
一张至多4096条 map 同时含历史/active/计量，active 仅引用一个记录。不同 ID 第4097次 run 在写前1008，
重复 run ID 一律1008；不淘汰、不增加 seen/terminal/sentence map。ID≤512字节。
历史只留 ID、generation、小计量事实、契约枚举和 model 的固定 SHA-256 身份摘要；当前任务才留完整模型/绑定。
摘要仅用于历史终态的可选 model 等值核验，不作日志、指标或凭据替代。

| 输入/竞争 | 写前状态 / 写后门闩 |
|---|---|
| idle + run | 核验并占位→awaiting-start；在写上游前可关联瞬回 started；run After 不回退状态 |
| 匹配 started | awaiting-start→started-delivering；写下游成功→active（out 则 draining）；失败唤醒等待者并终止 |
| started-delivering 的 binary/continue/finish | 在原客户端 worker 等交付，成功再准入；尚无 started 的 awaiting-start 则直接拒绝 |
| active duplex + finish（含 cancel） | 写前→draining，保留 active ID；After 只确认该 generation，不回退瞬回 terminal |
| 匹配 finished | 先记 usage→terminal-delivering；交付成功才释放 active；早到下一 run 在客户端 worker 等门闩 |
| 匹配 failed（可在 awaiting-start） | 先记 usage/安全失败→failed-closing；封客户端，原文交付后按§6等待真实 close |
| 已完成 ID 重复 terminal | 原文保全，去重/冲突诊断；不释放当前其他任务、不改 phase/期限；迟到 failed 不结束新任务 |

finished 可由上游自发结束 active/out，不能凭客户端 finish 才承认；started 重复/未知 ID 事件拒绝。
上行 binary 仅已交付 started 的 active ASR；下行 binary 仅 TTS 已 started 的 active/draining，含 finish 后尾音。
failed-closing的G宽限内可继续保全同一已started TTS的尾binary和绑定正确的文本，但不推进状态或重结账。
duplex active 接受 continue；draining 拒绝输入类 continue、binary、重复 finish；out 拒绝 continue/finish。
未知 action 仅绑定当前非终结任务，允许 awaiting-start/active/draining，但不能改变阶段、期限或 binary 许可。
未知 event 同样须绑定当前任务；terminal-delivering 的下一条上游消息自然受该方向顺序约束。
所有可选 model/streaming/三元组重申必须 Value 且等于原绑定，Missing 才表示未重申。

## 6. 阶段期限与唯一关闭仲裁

升级 `requestStart+FirstByte` 沿用共同期限；成功后不复用 handshake ctx。
awaiting-start 从 run 预登记起 `FirstByte`；draining 从 finish 写前起 `max(FirstByte,Idle)`，
out 从匹配 started 到达起同期限（含交付）。默认两者120秒；心跳、中间结果、尾音不刷新。
active 无 HTTP total 上限。上述是网关预算，不是云端 SLA。
阶段超时：task_start_timeout/task_drain_timeout，安全 ClassUpstreamUnavailable、Retryable=false，
本地 task-failed 后1011；不能返回裸 context.DeadlineExceeded 被旧分类折成1001。

传输层接口：`(*ws.Conn).ArmCloseDeadline(deadline time.Time) (finish func(), err error)`。
只安装/收紧共享 closeDeadline，不发帧、不置 closing、不封普通写；零 deadline 报错且返回空 finish。
每 Conn 仅一个守卫，重复 Arm/BeginClose/被动自动回复共用它；返回的 finish 幂等中止并 join 同一守卫。
普通 write、pong/ping、自动 close、BeginClose、TLS 强制释放均服从最早 deadline；重复装入不能续命。
调用者无论错误与否都 finish/join，守卫提前自然到期也必须 join；不以 readCtx 超时抢先断 TCP。
`ws.AllocateMessage(b *ws.BufferBudget, op ws.Opcode, size int, fill func([]byte) error) (*ws.Message,error)`
只接受完整业务消息 `OpText`/`OpBinary`；其他 opcode 在 acquire、make 或 fill 前返回 ErrProtocol。
在分配前 acquire 精确 size；失败不调用 fill；fill 错误/非法文本归还；Release 含浅拷贝幂等。

同一 wsTermination 持有：首次业务/生命周期结果、原文交付完成门闩、本地失败待交付项、
独立真实 peer CloseError 槽、共享绝对期限与唯一 physical-close owner。没有第二个 Inference termination。
`policyEnd(end wsPolicyEnd, originalPending bool) (won bool, delivered func(error))` 是 relay 内部入口；
forwardThenEnd 在写前认领并 arm 两段，After 后调用 delivered；reject/timer 的 Local 交 close owner。
End.At取Before入口或Expire传入的now，不能到实际close时重新起算；policyEnd成功是业务原因胜出的线性化点。
原 `report(wsRelayResult)`/`close() error` 保留；report 即使业务槽已占，仍能保存首次真实上游 close，
其余 CloseError 立即 Release。业务失败已胜出后 shutdown/1000 不改 outcome、metrics 或 Lease。

取 `W=min(Connect,Idle)`、`B=min(1s,Connect,Idle)`。收到 failed 于 tfailed：
1. 立即认领结果、封客户端，给两段 Arm `Dall=tfailed+W+B`，覆盖原文等锁/写出。
2. 原文成功交付于 t0 后收紧 `D=min(Dall,t0+B)`，被动等 `G=min(100ms,(D-t0)/4)`；G 在 B 内。
3. 原上游 reader 在 G 内继续读；收到合法 close（含空1005本地表示）保存原 code/reason，尽力转发。
   尾消息/ping 不续时；慢下游/EOF/写失败只用剩余 D。写失败不再被动等 G。
4. G 内无真实 close则本地1011、reason=`upstream task failed`；两段以已经arm的CloseWithResult收尾，
   共用D覆盖发送/释放，不另加等待或reader，到D强制释放，最后调用全部arm返回的finish/join。
   不能把此close标为上游事实，不能重开一秒。新task始终不放行。

本地失败也先 arm `At+W+B`、尽力交付后收紧至 `min(Dall,now+B)`，不等 G；无合法 ID只关闭。
普通关闭从首次选择起共用 B。registry 与 relay 共用该绝对值，并行关闭两段而非每段重新计时。
所有终止先 Stop/取消 policy 等待、停心跳，再完成已认领失败交付/close，最后 cancelRead、join 全部
worker/守卫、Finish、由原 handler 一次结 Lease。已观察上游业务失败与随后下游写失败分别保留，
前者仍是请求失败；无先前业务失败的下游写错归下游，不冷却上游凭据。

为让pending shutdown也有同一B，内部装配签名改为 `newWSRegistry(maxSessions int, closeBudget time.Duration) *wsRegistry`
与 `(*Built).initWebSockets(config.WebSocket, config.Timeouts) error`，后者传入上述B，不能到relay才补装预算。
`newWSTermination(closeConns func(uint16,string,time.Time), closeBudget time.Duration) *wsTermination`
用于session与尝试级终止；`closeWSConnections(conns []*ws.Conn,code uint16,reason string,deadline time.Time)`
给全部段同一D，不重新取now+B。`(*wsTermination).attachRelay(downstream,upstream *ws.Conn,options wsRelayOptions) error`
在启动workers前绑定local交付、policy.Stop及守卫所有权；已选终止则安全失败，不等待尚未启动的worker。
attach与registry shutdown竞态用同一仲裁锁，锁内只交换状态，不做Stop、I/O、等待或metrics。

## 7. 累计计量

任务记录同时保存最后接受/已发布快照、冻结位、settled 位、终态小事实；没有第二本 ledger。
`obs.WSUsageUnit` 为上述四个同名字符串枚举（obs.UnitUnknown/UnitTokens/UnitCharacters/UnitSeconds）。
`type WSUsageDelta struct { Unit WSUsageUnit; InputTokens, OutputTokens, Characters int64; Seconds float64 }`。
Unit 同时表达原单位/presence，其他单位字段必须零，unknown不能发布数字。
`(*Metrics).ObserveWSUsageDelta(protocol,source string,delta WSUsageDelta)` 仅增数字；
`(*Metrics).ObserveWSUsageRecord(protocol,source string,unit WSUsageUnit,fidelity canonical.Fidelity)` 仅增记录。
source 白名单增 task；unknown 只允许 unavailable。校验负数、NaN/Inf、非法 unit/fidelity，拒绝动态标签。
S3 token delta 另调用 ObserveUsage(outbound, authoritative tokens)，total 不作第二份计数。

- 已确认 cumulative 模式每个合法快照立刻在转发前发布非负差额；首次真实0也建立权威零时序。
- 6→13→terminal13：数字13、记录1；6→断线：保留权威6，unavailable记录1、usage_unfinished。
- 6→3→6、负值、溢出/矛盾单位：冻结该单位后续数字、诊断，不重置基线、不再加3；原文仍转发。
- terminal/Finish 每 task×原单位只结一次；有效最终快照且无冲突为 authoritative，其余 unavailable。
  terminal null/missing 不用此前快照冒充最终量。已发布数字仍 authoritative，与完整性记录区分。
- unverified 模式即便终态带值也不猜数字；已知单位在原单位结 unavailable，未知型号用 unknown。
  Qwen3.1 ASR 的兼容 duration 不触发 seconds 数字、记录或冲突。
- 重复终态相同不计，矛盾仅诊断；Finish 幂等，不清已结或已发布数字。仅当前任务的真实 failed
  计一次业务失败诊断。Requests 仍每连接一次，不随快照增加。

## 8. 验收、独立录制与未完成项

离线必须通过原正式装配函数的三门混合接线、整门负例、鉴权替换、无 query/无首事件、跨租户绑定、
双向字节/binary、out、串行复用、门闩竞争、假心跳、故障交付和共享预算、累计/容量测试。
所有 socket 测试本地 TCP/httptest；门闩用显式 barrier，policy deadline 用可注入 now/Expire。
网络时间断言采用小预算加固定调度容差（普通200ms、race500ms），同时断言绝对 deadline 不被重置，
不靠 sleep 安排竞争、不用 net.Pipe。退出检查 Message/CloseError 预算0、registry0、Lease一次、worker join。
默认 Build 三 WS 门仍404，默认矩阵仍501；测试矩阵批准完整六项只是隔离机制验证。

### 8.1 控制器独立有界实验方案

新批次固定 `s3-audio-20261004`，至多6次物理 Dial（失败也占槽），每调用一槽、无自动重试，
最多每连接2 task，整批≤10 task、每连接≤45s、全批墙钟≤270s（不含人工审核间隔）。
北京 workspace 的显式配置 wss 主机；录制前控制器核对该凭据地域与模型可用性，不自动换地域/型号。
输入 PCM16LE/16kHz/mono 的非敏感固定测试语音，单任务≤3s、整批上传音频≤18s；
每 TTS task 固定两段“这是网关测试。”“请确认声音清晰。”（out 合成连接后的整句），每 task≤40汉字，
整批 TTS≤240汉字，响应音频总量≤8MiB/连接；消息≤1MiB、节点≤1024、原始轨迹≤16MiB/连接。
估算最高费用≤人民币1元：控制器以当日地域价格与最坏 token/时长/字符上限预计算；无法证明上限则该槽
不发起，剩余预算不足即停。wire usage 不是实时人民币账单，工具不能假称已精确限住最终计费。
manifest固定字段：Batch、Region（`cn-beijing`）、Endpoint、SamplePath、SampleSHA256、PriceSource、
PriceCheckedAt（RFC3339）、WorstCaseFen（全批≤100），以及六项Slots（Slot、Model、Voice、MaxTasks、
MaxInputAudioSeconds、MaxInputCharacters、WorstCaseTokens、WorstCaseFen）；逐项必须等于场景表的模型/
音色及其硬上限内数值。未知计价边界阻止**该槽**reserve，不能以0作为已证明的token上限。每槽费用上限至少1分，
reserve预占本槽最坏费用/时长/task，失败不退款，持久槽总和也不能超批次上限。

**Task7复核后载体补全（控制器裁决，非新增真实费用授权）：** manifest可选新增 `CostEvidence`（至多六项、按Slot唯一），
仅本次指定槽须有完整证据；其他未选择token槽的WorstCaseTokens=0只表示未知，不阻断已证明槽，也不能授权自身。
证据绑定Slot/Region/Model/Voice/SampleSHA256/MaxTasks、官方PriceSource和独立CheckedAt（RFC3339，过去24小时内），
声明Unit、取整Rounding及RoundingSource；Components按input/output声明MaxPerTask、PriceFenPerUnit、Quantum、
MaxBasis和价格/最大量来源。数值用正int64分子/分母，内部精确有理数计算，不用float或可能溢出的int64中间乘加。
来源载体包含官方URL、非空原文摘录及摘录SHA；真实性/适用性由控制器对照官方材料核验，工具不联网认证文档语义。
seconds/characters按完整允许输入上限、`bounded_input`依据计算，另须输出不收费来源；token必须提供两路完整
`server_limit`最大量与价格，不能凭短音频、45秒或轨迹字节猜测。逐task按Quantum向上取整，随后按声明的
逐槽或逐task分币ceil规则计算费用，结果须落在本槽WorstCaseFen及批次持久预算内。正整数或verified布尔不足以授权。
具体字段及公式见[录制指南](../../research/2026-10-04-dashscope-inference-s3-recording.md#本槽costevidence载体)。
manifest文件及含HTML转义的紧凑重编码各≤64KiB；recording摘要同编码、独立≤65KiB，保全全部来源摘录。
固定摘要开销≤984B，预留1KiB（算术见录制指南）；输入级超限在reserve/Dial前拒绝，不能调用后才因缩进丢失摘要。

冻结的批次身份/共享预算投影不含可补齐的CostEvidence及token最大量；完整选中Slot、费用材料、计算分值随reserve封存，
Dial前逐字节重核。token最大量只可在该槽未占用前补齐，不改变固定人民币/task/墙钟预算；已占槽不可替换证明或退款。
顶层PriceCheckedAt记录批次初始参考日，本槽CheckedAt承担当前价格核对新鲜度。当前取整/token依据仍缺，真实调用保持0。

| 槽 | 固定场景 / 模型 | 目标证据 |
|---|---|---|
| 1 | ASR两 task：`qwen-audio-3.0-asr-flash-streaming` | duplex、串行新ID、seconds累计 |
| 2 | TTS两 task：`qwen-audio-3.0-tts-flash`，voice=`longanlingxi` | continue/finish、尾音、characters累计 |
| 3 | `sambert-zhichu-v1`，out单 task | 无continue/finish的正常自终结、characters |
| 4 | `qwen-audio-3.1-asr-flash-message` 单 task | token累计、兼容duration不计seconds |
| 5 | `qwen-audio-3.1-asr-flash-streaming` 两 task，第二次在首个真实快照后主动中断 | 首 task权威保留、第二 task增量与unavailable |
| 6 | `qwen-audio-3.0-tts-flash`，一次无效音色 `omugw-invalid-voice-s3` | 原生task-failed及真实close/EOF/静默；不伪造终态 |

槽6先被动观察至1秒或收到peer close，再以独立录制器剩余1秒主动收尾；这属于取证策略，
不能替换网关 G/B 政策。报告记录 failed→close 的实际间隔；只有G内close才证明网关可保全其原码。
不读、不改、不复用 S1 的八槽 ledger；S3 另建独占目录/持久槽，换输出名不恢复次数。
录制器直连上游，不 import gateway/协议 Inspector/Provider 生成预期；允许 transport/testkit，
发送请求和用量/终态判定独立写字面契约。原始时间、opcode、bytes、SHA、peer close 与本地主动close分开。
文件 exclusive-create、目录0700/文件0600，握手先脱敏，不保存 key/header secret，不把内容打到日志。
原始材料按上限流式落盘；候选另限testkit默认8MiB文件/4MiB轨迹/1MiB消息，超限保留私有原始材料并
报告候选未生成，不能删事件或修改字节挤进上限。记录发送完成与对端close接收是不同证据。
failed后的尾文本继续执行同一窄绑定检查；实际失败task ID及冻结终态usage不能被重复failed改写，矛盾只留raw。
主动BeginClose之前检查本地close及必要peer-close记录/编码额度；不足明确预算失败并强制释放、join，不能先发后拒。
只在专用 smoke tag+开关+批次/槽/manifest 完整时发起；普通/race 测试不能触公网。

验收表分列文档事实、synthetic机制通过、真实支持/未测、网关保全、计量证据、逐能力缺口。
缺真实证据如实条件验收，不用 synthetic/价格表/101 补齐。multimodal-dialog、听悟、text_generation独立
承载、其余型号精确计量、Paraformer/Fun-ASR累计、Qwen3.1 TTS重叠、生产整门投放列为未来未完成项。
控制器完成全分支 review/check/race、SDK普通/race、禁live录制器测试与PR验收；具备相应证据才merge，
本阶段验收后暂停，不自动推进以上未完成项。
