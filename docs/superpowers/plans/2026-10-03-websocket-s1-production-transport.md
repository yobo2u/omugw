# S1 首批：生产 WebSocket 传输契约 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** 为 DashScope Realtime 纵向交付补齐生产所需的握手、有限IO与有界消息所有权，保持已交付离线回放兼容。

**Architecture:** 沿用 ws.Conn/Dial/Accept，不另造WS库。先完成具有独立验收面的传输补丁；S1后续Provider/relay消费本批接口，真实录制与生产兑现不在本批宣称完成。

**Tech Stack:** Go 1.25、标准库、当前三项直接依赖不增加。

**Spec:** `docs/superpowers/specs/2026-10-02-same-contract-websocket-passthrough-design.md`（2026-10-03修订，§4–6）。

**实施状态（2026-10-03）**：本计划两项任务及整体复审修复完成，代码截至
`7088df5`。Task1 为 `0fb9f10`；Task2 为 `c108470`、`6a0d284`、`91e10c5`；
整体修复为 `7088df5`。逐任务复审及最终定向复审均通过。共享 ws/testkit 的
uncached 普通与 race、`make check` 全部通过。生产接线及真实录制属于后续计划。

## Global Constraints

- 注释与文档中文。Git均带`GIT_MASTER=1`，只在当前隔离分支本地提交。
- 不push/PR/merge/issue/清理其他工作树，不调用真实云模型、不读取或输出凭据。
- synthetic只用于本机测试，不放生产route目录、不改Redeem/白名单/生成矩阵。
- 本机TCP对，不用net.Pipe；以就绪通道同步关键时序，不靠sleep猜因果。
- D1 CAS/join、CloseWithResult.sent、IncompleteMessage、预读字节保全不能回退。
- 保留原有公开签名与合法调用；新增opts零值兼容。受控新API可显式要求budget。
- 所有实现TDD，记录真实RED/GREEN命令及输出。每任务 fresh implementer + fresh reviewer；不指定model、不再派子代理。
- 用户已明确要求启动开发；完成本计划自检后执行，无需重复征询执行方式。

## Review Focus

1. 官方Python客户端的Origin和扩展提议不能因传输预检失败：Task1正向fixture。
2. 合法101后预读的第一帧不能因握手头限额耗尽而截断：Task1头/帧同TCP写测试。
3. 出错HTTP返回慢body不应挂住或在socket关闭后只剩不可读Body：Task1期限与限额测试。
4. 写锁正被不读对端占用时cancel/close必须回收：Task2背压回归。
5. 分片暂存、旧/新扩容副本和掩码临时块必须有上界：Task2所有权和budget高水位测试。

## 文件与交接

| 文件 | 职责 |
|---|---|
| `internal/transport/ws/handshake.go` | Dial/Accept顺序与取消交接 |
| `internal/transport/ws/handshake_validation.go`（可新建） | 无副作用握手预检、受限头读取 |
| `internal/transport/ws/production_handshake_test.go`（新建） | Task1真实TCP握手边界 |
| `internal/transport/ws/frame.go` | mask/close校验与分块写 |
| `internal/transport/ws/conn.go` | role约束、有限写、读ctx、关闭 |
| `internal/transport/ws/budget.go`（新建） | 同步额度与显式Message所有权 |
| `internal/transport/ws/production_io_test.go`（新建） | Task2慢读/心跳/取消/byte预算 |

## Task 1: 生产握手预检与可读失败响应

**Files:** 修改handshake.go；可新建handshake_validation.go；新建production_handshake_test.go。

**Interfaces:**
- 消费：现有`Dial(ctx, rawURL, DialOptions) (*Conn, *http.Response, error)`及`Accept(w,r,AcceptOptions) (*Conn,error)`。
- 产出：`ValidateUpgrade(r *http.Request) error`，无写响应/无Hijack副作用。
- `AcceptOptions.HandshakeDeadline time.Time`，101写期限；完成后清除。
- `DialOptions.ConnectTimeout time.Duration`，仅TCP+TLS；ctx仍限制整个握手。
- `DialOptions.MaxHandshakeBytes int64`，零默认64KiB，限制上游HTTP状态行+头，不能限制后续WS流。
- `DialOptions.MaxErrorBodyBytes int64`，零默认64KiB；失败Body在有界读取后替换成内存可读Body。
- 错误满足errors.Is(err,ErrHandshake)，不回显请求key/body/header/原始URL秘密。

- [x] Step 1: 写`TestProductionUpgradeValidation`表测（GET/HTTP1.1/upgrade token、版本/nonce唯一且合法、无body/TE；bad method、重复key、base64不是16字节拒绝）。合法Origin和permessage-deflate提议放行，纯预检不写响应。
- [x] Step 2: 写TCP测试`TestProductionDialRejectsInvalid101`：缺Upgrade/Connection、重复Accept、上游擅自选扩展/子协议失败；正确101+预读首帧全部保留。
- [x] Step 3: 写`TestProductionDialErrorBody`：401有限body在返回后仍能读，status/Retry-After保留；大body只返回上限内前缀并有截断错误；无EOF慢body受ctx限制。写头超限、不完整响应、HEAD大小临界和后续大WS帧不受HTTP限额影响。
- [x] Step 4: `go test ./internal/transport/ws -run 'TestProduction(Upgrade|Dial)' -count=1`，确认预期RED。
- [x] Step 5: 最小实现预检/有限读/选项。Accept复用同一预检；handler日后先预检再Dial。忽略客户端扩展提议、不回显；本批不支持子协议协商，Dial不提供子协议并拒绝上游擅自选择。非101失败仍正确close网络资源，内存Body归调用方。
- [x] Step 6: ConnectTimeout用child ctx并立即cancel，不把TCP阶段期限留到业务；D1 CAS watchdog的归属不可改变。Accept设置明确写期限、错误路径关闭、成功清除；负时长/负限额输入明确拒绝。
- [x] Step 7: 包单测及race，另跑`go test ./internal/testkit -count=1`验证旧合法调用；保存完整输出。全仓普通测试一次。
- [x] Step 8: `GIT_MASTER=1 git diff --check`，只暂存本任务文件并本地提交；写报告，含接口与测试证据。

## Task 2: 有限双向IO与受控消息缓冲

**Files:** frame.go、conn.go；新建budget.go、production_io_test.go；handshake.go仅接入新opts（Task1交接以后）。

**Interfaces:**
- 消费：Task1所有签名，保留Dial交接CAS。
- `AcceptOptions`与`DialOptions`增加`WriteTimeout time.Duration`和`Budget *BufferBudget`。
- `NewBufferBudget(limit int64) (*BufferBudget,error)`，limit>0；共享额度，同步 acquire/release，公开`Used() int64`供验收观测。
- `Message { Opcode Opcode; Payload []byte }`，`(*Message).Release()`恰好归还一次（重复调用安全）；实现中可带非导出owned状态。
- `(*Conn).ReadOwnedMessage(ctx context.Context) (*Message,error)`：受ctx绝对取消/期限与idle共同限制；成功后由调用者Release，读取失败无泄漏。
- 复审补充：受控读的`CloseError`增加幂等`Release()`，非空Reason复制先预占、交接后持续计额；调用方通过errors.As取出并在处理完关闭原因后释放。旧无Budget调用不增加释放义务。ctx覆盖/丢弃该错误时由transport释放；先做无分配校验，不能在拒绝非法reason或只验证出站close时额外复制payload。
- `(*Conn).Ping(payload []byte) error`：复用写锁和有限期限，不得发送>125字节ping。
- 新增`ErrBufferLimit`、`ErrMessageTooLarge`用于上层区分全局容量与消息超限；保持已有ErrProtocol链兼容。
- 原`ReadMessage() (Opcode,[]byte,error)`行为兼容；有Budget的Conn禁止通过旧接口绕过所有权（明确报错），生产必须ReadOwnedMessage。

- [x] Step 1: 用TCP写`TestProductionRoleMask`、`TestProductionUTF8AndClose`，客户端/服务端反向mask拒绝；UTF8分片允许跨帧拆字；坏文本、非法关闭码拒绝。空close仍以1005本地表示，对外编码为空；不发1006/1005。
- [x] Step 2: 写`TestProductionBoundedWrite`、`TestProductionOwnedReadCancellation`，不读对端、自动pong/并发Ping、持锁时Close立即回收；read ctx在持续ping下仍到期；成功read后取消旧ctx不关闭下一笔读或业务连接。
- [x] Step 3: 写`TestProductionBufferBudget`：多连接共享小额度，未收到完整payload前分配必须被限住；分片扩容峰值计入；读错误/非法帧/关闭均归还；成功消息未Release时仍占额，Release幂等后Used==0；不足额度立刻错误不等候。
- [x] Step 4: 写`TestProductionChunkedMask`：跨固定块边界与3/4/5字节偏移往返精确、payload不变；短写返回io.ErrShortWrite（不能错误上报sent）；mask副本≤固定块（建议32KiB）而非整message。
- [x] Step 5: 跑以上定向测试，记录RED。实现最窄reader owned seam：预算计真实容量和并存副本，包括frame临时及重组缓冲；未实际释放前不可归还。无需通用allocator框架，旧纯帧API继续无budget，生产Conn必须受控读。若现有Reader必须分出内部 helper 可新建owned_reader.go并在报告列明。
- [x] Step 6: WriteTimeout只在写锁内设/清，零值保持旧合法调用。ctx取消可以关闭本次连接，但成功交接后watcher必须stop/join，不能迟到关闭。关闭reason固定安全文本由上层决定；transport只校验UTF8/码合法性与空close。
- [x] Step 7: 运行`go test ./internal/transport/ws ./internal/testkit -count=1`与对应race，覆盖所有D1/close/预读旧回归；再`make check`一次。
- [x] Step 8: 本地提交并报告。任务review通过后做本批整体review，处理发现，保留ledger。

## 整体复审交接补充

- 大首片后保持摊销扩容，接近上限时一次扩至上限；并存额度仍在分配前预占。
- `transport_close.go` 统一底层中止权：WS close与TLS close_notify共享绝对关闭期限，
  取消直接中止底层socket；所有期限守卫在返回前join，TLS不能自行延长预算。
- `ErrInvalidUTF8` 可独立识别1007，同时保留`errors.Is(err, ErrProtocol)`；relay
  先匹配UTF8再匹配一般协议错误。公共帧头拒绝非最小扩展长度编码。
- 回归分在`production_regression_test.go`与`tls_close_test.go`，覆盖真实TLS收尾、
  扩容放大、错误分类及长度临界。整体审查四项finding均已通过定向复审。

## 实施裁决及代价

1. 先固定生产传输接口，再做S1接线；代价是多一份分批计划，不能据此认定路径投放。
2. 保留32MiB和全局预算，Inference在run-task阶段核对模型；额度过紧会拒绝大消息，
   需明确报错与可配置，不能承诺101前识别尚未收到的模型。
3. 整门兑现仍需逐项真实证据映射；代价是证据不足时推迟投放。
4. 复用现有隔离工作树并保留证据；代价是旧工作树和本地工件仍需后续授权收尾。
5. 回放子协议测试改用raw TCP、生产Dial不发送不支持的提议；依赖旧通用Header
   行为的调用需迁移，matcher与fixture断言保留。
6. 未声明关闭码测试改用合法私用3999；非法2999另由传输层测试覆盖，避免把两层
   拒绝混成一个断言。
7. 关闭原因新增显式Release以守住payload预算；后续relay须释放错误所有权，遗漏
   会耗尽额度，因此接口和取消丢弃路径均有回归。

## 自检与开发后续

- 接口：Task1的opts由Task2加字段，签名不改；Task1通过后才实施Task2。
- §4合法Origin/扩展与秘密输出由Task1；§6有限写/role/close/预算由Task2。
- max_sessions、模型/矩阵、Provider/relay、usage、registry属于下一S1纵向接线计划；
  不在本批顺带添空实现。生产三门仍PLANNED，任何测试通过不等于路径转正。
- 本批是可独立发布的生产传输库增强；S1接线必须消费OwnedMessage并finally Release，
  不能用旧ReadMessage跳过额度。后续独立计划从这些已核验接口出发。
- 真实费用/模型/样本授权未取得；没有新增live调用。文档原则澄清单独暂存提案，
  本批不宣称其已正式采纳。
