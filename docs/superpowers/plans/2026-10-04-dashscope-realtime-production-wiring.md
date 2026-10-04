# DashScope Realtime 生产接线 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 完成 S1 的有界生产接线、离线验收与直连录制工具；真实证据齐全后整门投放。

**Architecture:** 复用已完成的 WS transport；平行 StreamProvider 只负责出站拨号。网关协调握手承诺、受控双向 relay、会话注册、凭据结算；协议窥探器只读抽取必要观测，不重建应用消息。

**Tech Stack:** Go 1.25、标准库、现有 yaml.v3 / prometheus / go-cmp，不增依赖。

**Spec:** `docs/superpowers/specs/2026-10-02-same-contract-websocket-passthrough-design.md`；传输交接见 `2026-10-03-websocket-s1-production-transport.md`。

## 执行验收状态（2026-10-04）

以下汇总是当前状态；后面的分步清单保留初始实施要求。实录揭示的契约修订以
[验收记录](../../research/2026-10-04-dashscope-realtime-s1-acceptance.md)及
[证据账本](../../research/2026-10-04-dashscope-realtime-evidence.md)为准。

- [x] Task1–5：配置、Provider、只读用量、registry/relay、handler/Build/main、录制器均完成且经独立复核。
- [x] Task6.1：固定北京批次8/8次已执行，保留真实失败，取得三份完整候选。
- [x] Task6.2：原则2.2/2.4/2.7独立修订与独立复核通过，本地提交，尚未发布。
- [x] Task6.3：三份原字节归档及正式handler离线回放通过；缺少整门证据，按条件保持PLANNED。
- [x] Task6.4：`make check`及全库`make test-race`通过；未启门，不执行真实gateway冒烟。
- [x] Task6.5：本批最终跨层复核 `2704fd7..0a0976f` 通过；可合并条件验收，发布状态以PR/CI实际结果为准。

实施修订：双单位usage可同时合法，分别校验和计量；自动提交在finish前已有同实体音频，
完成终态允许在finish后到达；上游文本item ID由服务端生成。上述修订均以真实录制为依据，
并经独立复核，不修改网关应用消息字节或降低能力兑现门槛。

## Global Constraints

- 注释、文档用中文；同协议同契约完整应用消息的 opcode、字节、单向顺序原样保全，不经 IR/Extensions 重建。
- 整门矩阵检查使用 `degrade.ExpressibleSet(degrade.ProtoDashScopeRealtime)`；不以历史 homogeneous 标记接受跨协议候选。
- 默认消息32 MiB（最大64 MiB）、pending+active会话128（最大4096）、受控payload预算256 MiB（最大2 GiB）；最低预算为2*M+min(M,32 KiB)，M为消息上限，含双向完整消息与出站掩码工作区。多会话聚合、临时扩容、控制帧和关闭原因仍真实计额；“至少两倍”仅是下界，不保证任意128并发满额或进程RSS。
- 去重记录4096、关联ID512字节、失败握手body64 KiB、握手头64 KiB；日志与本地错误无消息正文、URL、secret、close reason。
- 仅唯一请求头鉴权；Origin允许、扩展提议忽略、所有子协议拒绝；租户白名单值唯一且无控制字符。
- 无模型别名；Dial前核验请求model等于target.UpstreamModel；所有候选共用first_byte，HTTP total不套会话。
- Hijack/开始写101即不可重试，包括Accept失败；只在之前对已确认Retryable故障换key，非Retryable可换同协议target。
- 每方向一个消息在途、无累计队列；ping间隔idle/2，写期限min(connect,idle)，关闭不超过1秒；所有worker必须join。
- 受控消息和关闭原因分别Release；取消丢弃结果由transport归还；读取后观测先于下游写，完整权威用量不被后续断开清零。
- registry封口与pending登记原子；关闭在锁外；单一协调者对每个Lease恰好结算一次。
- 本地TCP测试，不用net.Pipe；synthetic不进生产routes目录，不替代真实证据。只有证据覆盖才兑现并注册生产门。
- 用户2026-10-04已完整授权按目标连续开发，保留原生子代理逐任务实施和独立复核，不指定子代理模型。沿用当前隔离分支；不重复实施已完成传输批。

## Review Focus

- 官方客户端默认Origin与压缩提议不得误拒绝，重复认证/租户头须在触上游前拒绝（Task1、4）。
- failed/cancelled轮次仍可能有可信usage；重复终态与不同来源同ID不可重复/漏计（Task2）。
- 另一方向阻塞写或CloseError仍持预算时取消/关停，必须收回预算且不越过关闭期限（Task3）。
- 101写失败和首事件读取期间不停ping，不能触发重拨或重置first_byte（Task4）。
- 真实录制包含动态ID、音频与未知字段，审核必须保持因果与字节匹配，不能从网关输出生成期望（Task5、6）。

## 文件职责

`config/websocket.go`：资源策略与WS地址校验；`provider/stream.go`：平行接口；
`provider/dashscoperealtime/`：固定目标与干净握手；`protocol/dashscoperealtime/`：必要事件字段与错误分类；
`gateway/ws_usage.go`：有界去重和未结轮次；`gateway/ws_registry.go`：pending/active生命周期；
`gateway/ws_relay.go`：双向负载所有权及收尾；`gateway/ws_handler.go`：握手协调；
`gateway/build_ws.go`：共享依赖与测试/生产注册边界；`tests/smoke/record_dsrealtime_live_test.go`：显式启用的独立录制。

### Task 1: WS配置与DashScope出站适配器

**验收：已完成**（`a6ecdbb`，独立复核Spec/Quality通过；步骤与RED/GREEN证据见本计划SDD报告）。

**Files:** Create `internal/config/websocket.go`, `internal/config/websocket_test.go`, `internal/provider/stream.go`, `internal/provider/dashscoperealtime/{provider.go,provider_test.go}`；Modify `internal/config/{config.go,gateway.go}`, `config.example.yaml`。

**Interfaces:**
- `config.WebSocket{MaxMessageBytes int64, MaxSessions int, MaxBufferedBytes int64}`，`DefaultWebSocket() WebSocket`，`(WebSocket).Validate() error`，`Config.WebSocket`。
- `provider.StreamProvider`：`Kind() degrade.Provider; Dial(context.Context, provider.Request) (*ws.Conn, *http.Response, error)`。
- `dashscoperealtime.New(timeouts config.Timeouts, limits config.WebSocket, budget *ws.BufferBudget) *Provider`。
- `dashscoperealtime.ValidateHeaders(http.Header) error` 供handler在Dial前调用；Provider重复防御。

- [x] **Step 1:** 添加表测 `TestWebSocketConfig`（默认、零负值、上下限、双消息加掩码工作区的最低预算）；`TestRealtimeProviderHandshake` 用TCP服务端断言前缀/完整路径不重复、model转义、替换Authorization、固定UA、仅两个DS白名单头、Origin/压缩/secret子协议未转发；`TestRealtimeProviderRejectsUnsafeHeadersAndURL` 覆盖userinfo/query/fragment/opaque、重复/控制字符头；`TestRealtimeProviderFailure` 覆盖401/403/429/503、Retry-After、畸形101、重定向不跟随、无正文/URL/secret泄漏。
- [x] **Step 2:** `go test ./internal/config ./internal/provider/dashscoperealtime -count=1`，记录缺类型/行为失败。
- [x] **Step 3:** 实现上述接口；配置仅WS kind允许ws/wss，HTTP既有合法部署保持兼容。WS地址支持http→ws/https→wss；固定 `/api-ws/v1/realtime`，query仅target真实model；固定UA `omugw`。无Canonical要求；拒绝错误kind/inbound。Dial设置共享预算/ConnectTimeout/Idle/WriteTimeout及64 KiB上限。已知HTTP状态用dashscopewire分类但将本地message/上游code过滤为固定安全值，不传播原body；未知握手失败非重试，明确超时可归upstream unavailable。
- [x] **Step 4:** 定向普通测试与该包race通过，`git diff --check`。
- [x] **Step 5:** 提交 `feat: add bounded DashScope Realtime stream provider`，报告RED/GREEN证据。

### Task 2: 只读事件窥探与可信用量

**验收：全部步骤已完成**（`f8d8f08`，独立复核Spec/Quality通过；含token/字符分单位、4096去重与大字段不复制验收）。

**Files:** Create `internal/protocol/dashscoperealtime/{event.go,event_test.go}`（扫描辅助可另建 `json.go`）；Create `internal/gateway/{ws_usage.go,ws_usage_test.go}`；Modify `internal/obs/{metrics.go,metrics_test.go}`；Create `docs/research/2026-10-04-dashscope-realtime-usage-contract.md`。

**Interfaces:**
- `dashscoperealtime.Event{Type, ID, Source, Status string; Started, Terminal bool; Usage canonical.Usage; Characters *int64; Diagnostic string; Failure *canonical.Error}`，`Inspect([]byte) (Event, error)`、`ClassifyClose(code uint16, reason string) *canonical.Error`。
- `newWSUsage(metrics *obs.Metrics, protocol, outbound string) *wsUsage`，`(*wsUsage).Observe(event dashscoperealtime.Event) error`，`(*wsUsage).Finish()`；单goroutine调用，Finish幂等。
- `obs.Metrics.ObserveWSUsage(protocol, source string, u canonical.Usage)`、`ObserveWSCharacters(protocol, source string, characters int64)`、`ObserveWSDiagnostic(protocol, reason string)`；固定来源response/transcription/session与固定诊断值。字符计量独立authoritative记录，不发布虚构的0 token记录。

- [ ] **Step 1:** `TestInspectRealtimeEvent` 验证session.created、response.created/done（response.id/status/usage）、input_audio_buffer.committed与conversation.item.input_audio_transcription.completed/failed（item_id，独立来源），unknown事件、非法JSON、超长ID、负数/溢出/浮点/缺失/nullusage、合法0、cancelled带usage；`TestWSUsageLedger` 覆盖同来源同ID重复/矛盾、跨来源同ID、未结Finish、4096边界、不淘汰重新计数、权威总计不被断开清零；指标不存在动态ID/model标签。窥探大audio delta不能复制audio字符串。
- [ ] **Step 2:** `go test ./internal/protocol/dashscoperealtime ./internal/gateway ./internal/obs -run 'Test(InspectRealtime|WSUsage|ObserveWS)' -count=1` 记录RED。
- [ ] **Step 3:** 只读解析，不重编码；必要字段限长，忽略未知大字段避免整帧map/RawMessage复制；输入JSON保持原样。以阿里云server-events官方页为依据记录response.done的input/output与audio分项；Qwen3-TTS的`usage.characters`按官方qwen-tts-realtime-server-events单列字符，不转换为token，字符与token形状同时出现视歧义并诊断。字符0/负数/溢出/缺失和矛盾重复也要测试；token不适用保持Unavailable但只发authoritative字符记录。未验证usage形状标unavailable、固定诊断，不能套用OpenAI。缺ID无法安全去重时明确策略错误。安全关闭分类只认已实测1011+`To many requests`限流组合，其他close不推断auth/quota。事件error仅观察已知code/type，不能日志输出message。同ID重复比较结构化Usage与字符值；未结最多4096与已结共享有界记录。error/业务未知事件照原样流过。
- [ ] **Step 4:** 定向普通与race通过；研究文档明确官方依据与真实证据尚待录制的边界。
- [ ] **Step 5:** 提交 `feat: observe bounded Realtime usage without rewriting messages`。

### Task 3: 有界双向relay与会话关停

**验收：全部步骤已完成**（`d212c08` + `ecc5cd5`；独立复核发现的关闭原因竞争已修复并经同一回归旧版失败/新版通过核验）。

**Files:** Create `internal/gateway/{ws_registry.go,ws_registry_test.go,ws_relay.go,ws_relay_test.go}`。

**Interfaces:**
- `newWSRegistry(maxSessions int) *wsRegistry`；`(*wsRegistry).Register(context.Context) (*wsSession, error)`；`(*wsRegistry).Shutdown(context.Context) error`。
- `(*wsSession).Context() context.Context`、`Attach(*ws.Conn) bool`、`Done()`；Attach对pending握手已获得的上游和随后下游均可用，封口后立即有限关闭，Done由handler在所有worker退出后调用。
- `relayWS(ctx context.Context, downstream, upstream *ws.Conn, initial *ws.Message, usage *wsUsage, idle time.Duration) error` 接管initial所有权；返回只含安全分类的错误，Lease由调用方结算。

- [ ] **Step 1:** `TestWSRegistry` 覆盖pending占限、登记/封口竞争、Attach/关停竞争、锁外close、重复Shutdown/Done、取消pending、shutdown等待worker Done；`TestWSRelay` 覆盖双向text/binary原字节顺序、首session.created最先、分片、大消息、Origin建立连接、ping/pong存活、上游usage先观测后下游写失败、慢读、ctx取消、合法close理由/空close、EOF、UTF8→1007/RFC→1002/超限→1009/预算→1013、竞争close与预算归零。
- [ ] **Step 2:** `go test ./internal/gateway -run 'TestWS(Registry|Relay)' -count=1` 记录RED。
- [ ] **Step 3:** 实现固定worker（双向读取转发+固定heartbeat），每方向一条在途无消息队列；协调者第一次终止原因获胜。CloseError原因只用于另一段close，所有遗留message/error均释放后join。关闭先于取消读ctx以给合法close/1001发送机会；不能以cancel先杀连接导致实际总是1006。shutdown专用context cause区分1001；客户端取消映射1001、非法/上游EOF按规范表；wsBufferLimit→1013。注册满返回本地429，sealed返回503，两者不冷却凭据。用量只由上游方向观察，Finish在join后一次。
- [ ] **Step 4:** `go test ./internal/gateway -run 'TestWS(Registry|Relay)' -count=1` 及 `-race`，预算全部归零、worker退出有确定性同步断言。
- [ ] **Step 5:** 提交 `feat: relay and drain bounded WebSocket sessions`。

### Task 4: 握手协调、凭据结算与Build接线

**验收：全部步骤已完成**（`d8235d3`，独立复核Spec/Quality通过；含Accept中断窗口、TLS关闭、lease generation和prelude回放）。

**Files:** Create `internal/gateway/{ws_handler.go,ws_handler_test.go,build_ws.go,ws_conformance_test.go}`；Modify `internal/gateway/{auth.go,auth_test.go,build.go,build_provider_test.go}`、`cmd/omugw/main.go`。

**Interfaces:**
- `WSDeps{Matrix *degrade.Matrix; Router *router.Router; Auth *Authenticator; Metrics *obs.Metrics; Log *slog.Logger; Pools map[string]*credential.Pool; Providers map[string]provider.StreamProvider; Timeouts config.Timeouts; Limits config.WebSocket; Budget *ws.BufferBudget; Registry *wsRegistry}`。
- `NewDashScopeRealtimeHandler(WSDeps) *WSHandler`；`ServeHTTP(http.ResponseWriter,*http.Request)`，handler自身派生入站身份与GET方法。
- `(*Built).ShutdownWebSockets(context.Context) error`；包内 `buildWithWS(cfg config.Config,m *degrade.Matrix,metrics *obs.Metrics,log *slog.Logger,registerWS bool) (*Built,error)` 供正式Build委派及显式测试矩阵；当前正式Build传false，Task6证据齐全后改true。

- [ ] **Step 1:** `TestWSHandlerPreflight` 断言重复Authorization/Api-Key/双来源、子协议、坏RFC、未知query/重复model/空model、无Hijacker、部分兑现、模型别名/通配符不匹配均零Dial；Origin与扩展提议成功。`TestWSHandlerFailover` 覆盖已知401/429与未知错误、不重用key、不跨协议、候选共用时限、首error/close非就绪、持续ping不能续时、Accept Hijack失败/101短写均不重拨、先session.created后客户端pipeline。`TestWSHandlerLeaseAndShutdown` 验证成功/失败/客户端取消单次结算、generation不被旧会话重置、pending+active shutdown，HTTP total短于会话仍存活。
- [ ] **Step 2:** `go test ./internal/gateway -run 'TestWSHandler|TestWSBuild' -count=1` 记录RED。
- [ ] **Step 3:** 先auth/RFC/头/query/Hijacker再占名额、路由/整门矩阵、依次有限候选与Lease、Dial/Attach、有界预读session.created、Accept/Attach/relay。共享握手ctx来自会话context，Accept明确deadline并在进入前置committed。尝试失败关闭/释放，已确认Retryable才换key；同协议另target仍必须逐个矩阵与真模型匹配。单handler协调Lease.Succeed/Fail，不将客户端取消/本地容量/协议错误误分类为可重试凭据故障；未知上游断流结束Lease但不凭close猜auth。WS metrics请求/首字节/耗时用安全固定分类；本地HTTP错误使用DashScope信封及已过滤错误头。
- [ ] **Step 4:** Build创建全进程一个budget/registry，HTTP Provider表与StreamProvider表并行，门对账仍按handler身份；测试启门也必须通过整门检查和reconcileDoors，不能直接绕过。main关停先封WS（异步join与HTTP Shutdown并行使用同一退出预算），设置请求MaxHeaderBytes=64KiB，清理pending与Hijacked连接。正式门此阶段不注册，配置WS provider可以装配但默认矩阵不宣称可用。
- [ ] **Step 5:** 合成因果回放用testkit WSReplayUpstream/ReplayWS端到端覆盖未知字段/二进制/错误；留在gateway测试目录。启动须处理就绪预读：客户端Dial尚未返回时，上游先发送fixture中唯一首条session.created；客户端升级后先逐字节核验该初始事件，再对余下轨迹运行ReplayWS。测试专用tail视图移除已独立验证的两个prelude节点及关联边/coverage，重算其摘要；原fixture先整体Validate且不修改，prelude不绑定后续动态规则（S1回放使用录下的字面ID）。不发两次session.created，也不等待双端齐备才驱动首事件。`go test ./internal/gateway ./cmd/omugw -count=1`、相关race、`make check`。
- [ ] **Step 6:** 提交 `feat: wire DashScope Realtime handshake and lifecycle`。

### Task 5: 独立真实录制工具与候选审核

**验收：工具与离线步骤已完成**（`959522a` + `ea4e97e`；三项证据边界修复已独立复核，真实执行进入Task6）。

**Files:** Create `tests/smoke/record_dsrealtime_live_test.go`、`tests/smoke/dsrealtime_recording_test.go`（纯离线辅助必要时移入独立文件）；Create `docs/research/2026-10-04-dashscope-realtime-evidence.md`。

**Interfaces:** 显式 `OMUGW_RECORD_DSREALTIME=1` + `OMUGW_RECORD_OUTPUT` + `OMUGW_SMOKE_MODEL_REALTIME` + `OMUGW_SMOKE_WS_URL` + 安全环境key；固定场景参数 `OMUGW_RECORD_SCENARIO`，每次一个直连会话。

- [ ] **Step 1:** 离线测试独占文件输出、失败不伪造terminal、消息/opcode/因果与来源、隐私白名单、默认跳过、次数时长限制；运行显式测试名称验证RED。
- [ ] **Step 2:** 直连上游录制，不经被测网关；脚本场景覆盖文本多轮/tool+parallel、音频输入/ASR/VAD/打断、图像、TTS提交模式。样本由公开非个人测试短句与程序生成图形组成，不上传用户素材；固定每会话60秒、最多8会话，测试消息/输出token/音频时长有上限，不自动扫模型或重试放大。候选文件独占创建，握手头仅安全非秘密字段，动态ID通过testkit已有规范处理，原始候选留ignored工作目录审核，不直接写生产routes。
- [ ] **Step 3:** 编译smoke测试且不开真实开关，运行离线辅助；报告每个场景需求与调用命令。真实调用由控制器核对模型/地域/契约后执行；当前授权覆盖建议方案，保守预算目标人民币10元，账单异步无法硬实时保证则仅报告请求硬限与用量。
- [ ] **Step 4:** 提交工具与证据表模板；证据表中15项能力逐项写实际轨迹、字段与保全断言，未获得证据明确缺口。

### Task 6: 证据验收与独立原则澄清

**Files:** Modify `docs/architecture/principles.md` 与澄清提案（独立文档提交）；真实证据齐全时新增 `testdata/routes/dashscope.realtime__dashscope.ws.realtime/` 与能力表、修改 `internal/degrade/{rules_phase1.go,matrix_test.go}`、`docs/degradation-matrix.md`、`internal/gateway/{build.go,ws_conformance_test.go}`。

- [ ] **Step 1:** 控制器执行有界直连录制，检查服务端行为、模型/地域、用量形状和候选隐私；证据不足时记录真实失败与缺口，不能伪造ack或把能力标签当证据。
- [ ] **Step 2:** 原则澄清按已审提案修改2.2/2.4/2.7，独立提交与独立复核；说明只涵盖同契约保全，跨协议仍需转换对照。
- [ ] **Step 3:** 对审核后的真实轨迹做完整gateway回放并逐项保全断言；只有15项全覆盖才一起修改正式注册、Redeem、两白名单和生成矩阵。缺证据则保留PLANNED并列出具体后续输入，代码可离线验收。
- [ ] **Step 4:** `make check`、`make test-race`；如正式启门，再执行一个显式目标的真实gateway冒烟并保存安全证据。
- [ ] **Step 5:** 最终分支复核（本批2704fd7之后，既有transport作为调用契约检查），修复发现后复核差异，汇总实际通过与剩余缺口。发布操作按实际证据和现行授权单独执行，不把本地完成写成远端已发布。
