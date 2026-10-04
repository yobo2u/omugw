# S2 OpenAI Realtime GA Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 完成 OpenAI Realtime GA 同契约接线、只读多单位观测、SDK/TLS离线验收和独立录制工具，证据齐全前保持PLANNED。

**Architecture:** 在S1共享生命周期上增加静态协议profile，Provider和只读观测分别实现OpenAI契约。唯一registry/关闭仲裁/预算/Lease不分叉；观测事实仅用于计量，应用消息始终原字节转发。

**Tech Stack:** Go1.25，标准库及既有三个直接依赖；Node SDK仅用于隔离测试目录，固定版本与lockfile。

**Spec:** `docs/superpowers/specs/2026-10-02-same-contract-websocket-passthrough-design.md`；S1交接 `docs/research/2026-10-04-dashscope-realtime-s1-acceptance.md`。官方核验见本计划工作区 `preflight-research.md`（2026-10-04），实施时将使用的来源落入tracked研究文档。

## Global Constraints

- 注释、文档中文；不增加Go依赖，不做IR转换、DSP、工具执行、浏览器子协议、模型别名或WebRTC。
- 同协议同契约完整应用消息的opcode、字节、单向顺序保全，包括未知字段和missing/null/false/0/空集合。
- S2为 `openai.realtime → openai.realtime`、`GET /v1/realtime`；仅GA。任意OpenAI-Beta头和子协议提议均在Dial前拒绝。query仅唯一非空model；intent/call_id另案。
- 唯一请求头鉴权；Origin允许、压缩提议忽略；只透传OpenAI-Safety-Identifier，单值无控制字符；Org/Project/Cookie不转发。出站Bearer来自池，UA为omugw。
- model必须等于路由目标UpstreamModel；云端回显snapshot与请求alias不同不等于网关改写。每次拨号和启动注册均以完整ExpressibleSet检查同协议门。
- 复用32MiB消息（最大64MiB）、128个pending+active会话（最大4096）、256MiB全局预算（最大2GiB）；最低为2*M+min(M,32KiB)。4096笔去重、512字节ID、64KiB握手头/失败body。
- 共同first_byte绝对期限跨候选/首事件/下游101；进入Accept即承诺，之后不重试。HTTP total不套WS；idle/2心跳，写期限min(connect,idle)，关闭共同一秒I/O预算。
- Message/CloseError显式Release；registry与relay共用wsTermination，所有worker先join再结账，Lease恰好一次；中断不清零已结权威usage。
- 无真实凭据时仅离线；录制显式开关默认关闭，不裸跑smoke。S1八次已耗尽不重开。S2真实方案单列，不因合成/SDK成功兑现。
- 生产WS门清单保持空，routes/Redeem/两份兑现白名单不变。Mux未注册WS URL返回404；矩阵未兑现门返回501，不能混述。

**状态校正（2026-10-04）**：计划起草时 S2 独立分支基于 `91a8799`，当时 PR #16
尚未合并；目前 PR #16、#17 均已合并，远端 main=`5eaf1e2`，本分支通过
`8ee9710` 已包含该基线。任务勾选保留原计划，当前实现、最终审查与验证范围以
[S2 条件验收](../../research/2026-10-04-openai-realtime-s2-acceptance.md)为准。
本地 Node 26 SDK/TLS 已验；CI Node 24 待远端，云端及生产代理未验；九条路径仍未全部交付。

## Review Focus

1. 把同名DS字段或close文案套到OpenAI导致漏计量/错罚凭据：Task2/3/4独立字面数值与协议隔离测试。
2. ASR晚于response、同item多part、禁用或未知配置：Task4边界状态测试，不产生session或幽灵part账。
3. GA与Beta、Live产品和SDK URL混淆：Task1/2/6/7零拨号、GA身份及真实TLS负例。
4. profile抽取破坏一次关闭/初帧/取消：Task3保留S1实录回放、Task6双门共享限额/关停。
5. 表达性扩充顺带开门或把缺实现写成不支持：Task5独立矩阵差异、Task6生产不开门断言。

## 文件职责与顺序

Provider固定地址和安全头；openairealtime解析只读事实；realtimejson仅借用JSON字段扫描；gateway profile选择协议行为；共享wsUsage负责有界去重/发布；OpenAI observer关联有效ASR配置；degrade独立校正GA集合；SDK与录制器留在tests。
执行Task1→2→3→4→5→6→7→8；每任务独立提交和Spec+Quality复核。

### Task 1: OpenAI GA StreamProvider

**Files:** 新增 `internal/provider/openairealtime/provider.go`、`provider_test.go`；新增 `docs/research/2026-10-04-openai-realtime-ga-contract.md`；必要 `internal/config/gateway_test.go`。

**Interfaces:** 消费既有 `provider.StreamProvider`/Request；产生 `New(config.Timeouts, config.WebSocket, *ws.BufferBudget) *Provider`、`ValidateHeaders(http.Header) error`、`Kind()`/`Dial(context.Context,provider.Request)(*ws.Conn,*http.Response,error)`。

- [ ] Step 1: 新增TestOpenAIRealtimeProviderHandshake/RejectsBetaAndSubprotocol/URL/FailureBounds/TimeoutWiring；断言固定/v1/realtime和唯一model、转义前缀、全部安全头规则、Beta空值/大小写/重复亦零Dial、无秘密错误。401/429+Retry-After、未知5xx、坏101、64KiB边界、不重定向与TLS证书校验有真实本地TCP预期。
- [ ] Step 2: `go test ./internal/provider/openairealtime -count=1`明确RED。
- [ ] Step 3: 实现同坐标Provider与窄URL/错误清洗。仅HTTP非101用openaiwire.DecodeError，错误/body/response不泄露原message/code/param/URL/request ID；不凭close字符串重试。沿用共享预算/期限和原接口，不改DS生产行为。
- [ ] Step 4: 将官方GA URL、头、有效session形状、单数token_details、ASR单位及SDK固定源码来源写入研究契约；官方证据与未实测分列。
- [ ] Step 5: 定向普通/race与 `go vet ./internal/provider/openairealtime`通过，提交任务文件。

### Task 2: OpenAI只读事件与共享窄JSON扫描

**Files:** 新增 `internal/protocol/realtimejson/value.go`及测试；移动DS json.go纯扫描逻辑至该包并适配DS；新增 `internal/protocol/openairealtime/{event,usage}.go`及测试；更新GA契约文档。

**Interfaces:** `realtimejson.Parse([]byte)(Value,error)`验证JSON并借用切片，`Value.Field(string)(Value,error)`、`Text(int)(string,error)`、`Count()(int64,bool)`；Value不离开Inspect进入长驻状态。新协议产生 `Inspect([]byte)(Event,error)`、`ValidateReady([]byte) error`、`ClassifyClose(uint16,string)*canonical.Error`，`MaxIDBytes=512`。

Event为只读观测事实：Type/ID/Source/Status字符串，Started/Terminal布尔，ContentIndex *int64（0..2147483647），ItemPending bool；Usage canonical.Usage，Details TokenDetails，Seconds *float64，Diagnostic字符串，Failure *canonical.Error，Transcription *bool（nil=配置未知）。TokenDetails为带presence的固定指针字段TextInput/AudioInput/ImageInput/CachedInput/CachedTextInput/CachedAudioInput/CachedImageInput/TextOutput/AudioOutput（*int64），不含raw和大文本。

- [ ] Step 1: TestOpenAIRealtimeReadyGA要求type=session.created、session.type=realtime、object=realtime.session、非空有界id；error/binary由调用层拒绝，缺失/重复身份字段/legacy拒绝，snapshot model允许不同。TestOpenAIRealtimeResponseUsage字面132/121/253、text119/audio13/image0、cache64及output30/91；测试image>0、unknown reasoning、cancelled/failed/incomplete用量。
- [ ] Step 2: TestOpenAIRealtimeUsagePresenceAndInvalid覆盖单数details、缺失/null/0、负/小数/溢出/重复key/总量矛盾、可选明细缺失不毁总量；TestOpenAIRealtimeTranscriptionUsageUnits字面tokens13/9/22与duration1.25/0；相关JSON转义key/孤立surrogate/大未知字段不复制。执行两协议/scanner测试得到RED。
- [ ] Step 3: 抽纯scanner并保持DS语义，GA stateless Inspect识别response和会话内transcription；session.created/updated仅报告有效ASR配置，不标session计费pending。committed为item-pending候选，observer决定是否启用。输入转写source独立，输出transcript不计ASR；未带part的delta不虚构index。duration须有限非负，不能转token。
- [ ] Step 4: token总量独立严格校验，明细给出项≤父项，cache是子集；只在已知分项完整时核对和，不推算缺项。非法明细保留合法总量但不发布坏明细，固定diagnostic。未知包络字段保全；GA未核验close均安全nonretryable，不借DS限流文案；in-band error仅按明确code/type表，不能捏HTTP状态放大重试。
- [ ] Step 5: 两协议/scanner普通/race、5秒有界fuzz及vet通过；记录无raw持有/分配验证，提交。

### Task 3: 共享生命周期静态profile，保持S1行为

**Files:** 新增 `internal/gateway/ws_profile.go`、`ws_observer.go`及测试；改ws_handler/ws_dispatch/ws_relay/build_ws及受影响测试；保持registry生命周期。

**Interfaces:** `wsEventObserver { Observe([]byte) error; Finish() }`；`wsProfile`固定inbound degrade.Inbound、outbound degrade.Provider、validateHeaders func(http.Header)error、checkReady func([]byte)error、encodeError func(*canonical.Error)(int,[]byte,map[string]string)、newObserver func(*obs.Metrics,string,string)wsEventObserver、classifyClose func(uint16,string)*canonical.Error；`WSHandler`持私有profile。保留NewDashScopeRealtimeHandler(WSDeps)，不暴露任意profile构造入口。relayWS接收observer与classifyClose；wsRelayResult携带已经安全分类的upstreamFailure，仲裁器不再硬编码DS。

- [ ] Step 1: TestWSProfilesDoNotShareCloseClassification先以独立测试profile证明DS已知1011文案不能污染另一profile；失败attempt、relay、registry竞争均同一分类和owner。现有S1实录/分片中断/Lease/101后不重试回归不得修改业务期望。定向测试RED。
- [ ] Step 2: 将固定DS身份/ValidateHeaders/ready/EncodeError/Inspect调用移到DS profile+observer；observer包裹现wsUsage.Observe，暂不改ledger存储。共享协调器只消费固定协议事实，不复制锁、Close或Lease逻辑。readWSReady预读且仅一条；每条先Observe再写；unknown合法事件/binary保全。
- [ ] Step 3: checkWSDoor/connect同用profile.outbound。提前失败的close分类也经profile，传入wsRelayResult后安全分类不可持有已Release原因；仍转发原码/原因，IncompleteMessage仍为失败。
- [ ] Step 4: `go test ./internal/gateway -run 'TestWS' -count=1`和race通过，真实归档SHA不变；相关transport/provider回归按实际影响运行，提交。

### Task 4: OpenAI观测状态、共享账本和指标

**Files:** 改 `internal/gateway/ws_usage.go`及测试；新增 `ws_openai_observer.go`、`ws_openai_profile.go`及测试；改 `internal/obs/metrics.go`及测试；更新研究契约。

**Interfaces:** 产生 `NewOpenAIRealtimeHandler(WSDeps)*WSHandler`，绑定Task1/2与profile。shared `wsUsage.Observe(wsUsageEvent) error`使用gateway私有事实（source/id/part、pending/terminal、Usage/Details/Characters/Seconds、诊断/安全Failure），DS observer做等价映射。记录键含part与其presence；ASR未知part→已知part迁移由有界账本方法完成，不另外存无限表。

- [ ] Step 1: TestOpenAIRealtimeTranscriptionCorrelation覆盖enabled/disabled/unknown、committed→未知part→明确part、同item多个part、ASR先/晚于response.done、delta缺index、重复terminal/换event ID、冲突。TestOpenAIRealtimeUsageNoSessionPhantom断言纯session无账；TestOpenAIRealtimeUsageDedupAndCapacity总计4096含pending/item关联，到限1008且已结保留，ID/index非法拒绝。运行RED。
- [ ] Step 2: observer仅从有效上游session配置确定ASR开启状态；disabled committed不记账，unknown明确诊断；实际ASR事件无须开始也可结算。item pending在明确part出现时合并而非幽灵unavailable，不把response终态结清ASR。关联辅助数据也计4096限额，不淘汰旧键重复计费；Finish只结未结。
- [ ] Step 3: 所有单位复制成固定值+presence参与重复比较。新增 `ObserveWSSeconds(protocol,source string,seconds float64)`及固定细分发布入口；秒量表 `omugw_ws_audio_input_seconds_total`、unit=seconds；duration-only不得产生tokens记录。已有input/output不与audio/image/cache相加；cache_read/cached模态/image/text为固定kind，仅明确值发布，无ID/model标签。DS字符与token同时合法行为不变。
- [ ] Step 4: 独立字面metrics断言含显式0、缺失单位、duration1.25、ASR tokens13/9与response132/121各一笔；bad明细独立诊断但合法总量保留；下游写失败前仍记权威，重复/晚断流不翻账。
- [ ] Step 5: gateway/obs/protocol普通/race通过、S1真实回放保持；提交。

### Task 5: GA表达性与跨路径设计格对账

**Files:** 修改 `internal/degrade/expressibility_phase1.go`、`rules_phase1.go`及定向测试；生成 `docs/degradation-matrix.md`；更新GA研究契约及最小会话契约§7的buffer术语。

**Interfaces:** OpenAIRealtime ExpressibleSet从12增至15，增加vision_input/image_detail/reasoning；realtime_image_input仍独立buffer语义。不增加Capability常量，不改变当前门兑现。

- [ ] Step 1: 独立字面15项集合TestOpenAIRealtimeGACapabilities与受影响C路径格断言RED；继续检查两份生产兑现名单完全不变。
- [ ] Step 2: OpenAI→DashScope补vision_input=REJECT（独立无音频图像item无法保留到须先音频append并共同提交的buffer，拒绝隐式加音频/改轮次）、image_detail=DEGRADE（逐图auto/low/high丢弃，目标会话级视频聚合不等价；不抵消vision拒绝）、reasoning=REJECT（当前公开WS无reasoning.effort/思考开关落点，不宣称模型无内部推理）。依据工作区matrix-preflight.md正式来源落入tracked研究文档；保留公开文档不全的证据边界，不声称实测拒绝。反向parallel维持DEGRADE但说明两端通用调度语义无等价保证，不能再称OpenAI没开关；反向image维持REJECT但理由为音画共同提交buffer与独立item的关联/边界不等价，不能再称OpenAI无视觉输入。
- [ ] Step 3: `make matrix-update`并人工审阅生成diff；`make matrix`与degrade普通/race通过。研究文档列设计处置与当前501分离，提交。

### Task 6: 明确门清单与正式处理链离线验收

**Files:** 改 `internal/gateway/build.go`/build_ws.go及现测试调用；新增 `ws_openai_test.go`、`ws_openai_conformance_test.go`；必要测试fixture位于testdata/testkit，禁止routes。

**Interfaces:** `buildWithWS(cfg,m,metrics,log, endpoints ...degrade.Endpoint)`替代bool；生产Build传空列表。仅switch已知端点构造固定handler，重复/未知门拒绝。增加OpenAI provider装配；registry/budget每个Built唯一。

- [ ] Step 1: TestBuildOpenAIRealtimePlanned断言Provider可配、生产Mux未注册、无Redeem测试启门失败；明确S1/S2单门及双门、重复/未知门、reconcile双向不漂移。TestOpenAIRealtimePreflightRealModel涵盖别名/wildcard/foreign homogeneous/部分矩阵/无Hijacker/非法query零Dial；定向RED。
- [ ] Step 2: 接入明确门清单；GA ready校验后Accept，初帧完整先行。保留共同first_byte/101失败不重拨/有限凭据/关停与HTTPtotal隔离，以OpenAI profile验证。
- [ ] Step 3: 独立synthetic四点轨迹TestOpenAIRealtimeConformanceReplay：GA文本/audio/tool/image/detail/reasoning、未知字段/空白/presence、error后继续、cancelled与truncate。ReadWSFixture完整校验，独立prelude后严格字面tail；不得通过gateway生成期望。Go补binary、大消息、双门聚合限额与同时shutdown，断言所有worker退出/预算0/指标字面值。
- [ ] Step 4: gateway/config/degrade普通/race及 `make check`通过；S1归档不变，生产门/兑现不变，提交。

### Task 7: 官方Node SDK与真实本地WSS/TLS集成

**Files:** 新增 `tests/sdk/openai-realtime/{package.json,package-lock.json,client.mjs,README.md}`；新增带sdk标签 `internal/gateway/ws_openai_sdk_test.go`；改Makefile、CI显式测试步骤、必要gitignore。

**Interfaces:** Node `openai/realtime/ws`的OpenAIRealtimeWS，锁定openai7.27.0（Apache-2.0）/ws8.21.0（MIT），registry发行和直接许可证已核对，安装时核对lockfile传递依赖。Node>=22，CI Node24。独立 `make test-sdk`执行 `go test -tags=sdk ./internal/gateway -run '^TestOpenAIRealtimeNodeSDK' -count=1`，依赖缺失必须失败，默认Go测试不安装依赖。

- [ ] Step 1: TestOpenAIRealtimeNodeSDKWSS/RejectsUntrustedTLS先RED；临时测试CA、TLS反代保留Upgrade→正式gateway→正式Provider→本地独立上游。SDK baseURL为https://localhost:port/v1。证书校验开启；不可信CA负例必须失败，不用全局TLS绕过。
- [ ] Step 2: 官方SDK序列化发送，underlying socket核验原字节首事件/全部事件；Safety-Identifier来自options.headers，Org/Project忽略、假凭据替换。涵盖session.update提前pipeline、GA文本/音频/错误后继续/cancel/truncate/close、心跳沉默与子进程join、预算0。
- [ ] Step 3: `npm ci --ignore-scripts`仅测试目录安装；CI明确setup-node+锁定安装+test-sdk，不能静默Skip。锁包不进Go Clean Core，列传递许可证证据。
- [ ] Step 4: test-sdk普通及sdk标签race通过，make check与workflow差异检查；记录SDK/TLS仅离线验收，提交。

### Task 8: 独立OpenAI录制工具与条件验收交接

**Files:** 新增 `tests/smoke/record_openairealtime_live_test.go`及前缀 `openairealtime_recording_*_test.go`；新增 `docs/research/2026-10-04-openai-realtime-evidence.md`与S2验收文档。

**Interfaces:** smoke标签显式 `OMUGW_RECORD_OPENAI_REALTIME=1`，凭据取OMUGW_SMOKE_OPENAI_KEY或OPENAI_API_KEY（两者不同则失败且不输出值）；URL固定无query的wss://api.openai.com/v1/realtime，model显式gpt-realtime-2.1，独占输出 .local/recordings/openairealtime/<batch>/<run>。

- [ ] Step 1: 离线配置/序列/安全/失败落盘/候选回放RED；沿用testkit四点来源规则，不导入gateway构造预期。八次不可回收槽/每次一会话60秒（业务59+收尾1）/128输出tokens/8秒24k mono PCM/单消息与总轨迹各1MiB/500记录/最多4个response，无自动重试/换模型/换batch。
- [ ] Step 2: 三场景：text-tools-vision（独立记忆回合+同轮双工具结果+程序图片detail+reasoning配置）；audio-manual（确认24k格式及ASR、真实append/commit/response）；vad-interrupt（有效VAD与活跃音频cancelled、独立truncate ack）。音频须明确本地公开样本元数据/摘要/来源，不代生成真人声音或隐私内容。取得实际有效回显和真实业务/close才生成candidate；原始失败不补造ack，所有场景按实体关联。
- [ ] Step 3: 对每场景以本地TCP独立字面脚本验收成功与关键负例（错part/toolID/未取消/finish-only/丢配置/秘密JSON转义/无关闭）；候选完整testkit回放，内容缺核验的能力不填Coverage。工具结果前复述才计stateful，输入ASR晚到不能提前结束。
- [ ] Step 4: 仅运行显式禁live的定向普通/race和smoke vet。证据表15项逐项状态及预算/命令/资料；若凭据仍缺，真实调用数为0，记录阻塞并保持PLANNED，不构造成功生产fixture。实际live另列账户地域/型号/费用目标并确认凭据安全可用后执行，不能用存在位替代地区可用性。
- [ ] Step 5: 全库make check/test-race、SDK和录制离线必需检查通过后独立最终审查，按实际状态发布阶段PR并继续后续路径。不以S2离线验收代替全九路径完成。
