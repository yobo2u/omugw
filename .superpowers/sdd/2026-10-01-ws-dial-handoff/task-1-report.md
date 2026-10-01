# Task 1 — D1 Dial 握手 watchdog 同步交接报告

状态：**DONE_WITH_CONCERNS**（实现及验证通过；测试 seam 的可维护边界见第 7 节）。
执行跨 2026-10-01 / 2026-10-02，仅一个 native OpenCode leaf 任务。

## 1. 基线与授权边界

- 工作目录：`/Users/yobo/.config/superpowers/worktrees/omugw/ws-fixture-plan-20261001`。
- 固定 BASE 与开始时 HEAD 完全一致：`eac6141ca9d60e9a21cfdbfe5a628436a54421d6`。
- 分支：`docs/ws-fixture-plan-20261001`，开始时 `git status -sb` 只有分支行；
  `git status --porcelain=v1 --untracked-files=all` 无输出，index/worktree clean。
- 已先读批准 brief、CONTEXT、principles、原诊断、握手/Conn/生命周期测试及
  TDD `writing-good-tests.md`。Experience 仅作操作提醒，不复做旧关闭与回放修复。
- 开工命令均实际执行且 exit 0：
  `GIT_MASTER=1 git fetch origin`、`GIT_MASTER=1 git worktree list`、
  `GIT_MASTER=1 git status -sb`、`GIT_MASTER=1 git rev-parse HEAD`。
- worktree list 识别到 main `bacf106`、production-store `c5d3a63`、
  progress-audit `817d80a`、realtime-session-contract `bcb11f3` 及本树 `eac6141`；
  这些其它工作树仅识别，未切换、修改或清理。
- 所有 Git 命令用 `GIT_MASTER=1`，所有 Go/make 用
  `GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local`（本机 Go 1.25.0）。
- 无 Dots、子代理、review 派发、分支/worktree 切换、依赖安装、凭据读取、smoke、
  push、PR、Issues、merge、reset/amend 或清理。控制器 ledger 和旧诊断未改。

## 2. 根因与最窄修复

原代码关闭 `handshakeDone` 后直接返回；watchdog 没有 join。如果它在 `Dial` 返回、
调用方取消 context 后才评估 select，两通道都 ready，取消分支仍能关闭业务 TCP。
不能靠 helper 推迟 cancel 修复传输契约。

产品仅改 `internal/transport/ws/handshake.go`：

- `:147` 增加每次 Dial 私有的 `atomic.Bool handshakeClaimed`，没有测试 hook/global。
- **取消线性化点**为 `:158` 的 `CompareAndSwap(false, true)`。
  只有认领成功的 watchdog 可执行 `:159` 底层 Close，以唤醒阻塞握手读写。
- **成功交接线性化点**为完成原 101/摘要校验后的 `:201` 同一 CAS。
  成功先认领则 watchdog 以后即使选取消也不得 Close；取消先认领则返回 nil Conn、
  原 response 与 `ErrHandshake`，不交付一条即将被关的 Conn。
- `:150-153` 在所有拨号后返回路径关闭停止通道并 join `watchdogDone`；
  `:155` 在 watchdog 完成实际关闭或放弃关闭权后标记退出。
  成功返回前旧 goroutine 已退出，而非仅通知它将来退出。
- 因此 watchdog 不能在成功返回之后启动关闭，也不能返回成功同时保留关闭权。
  两个事件真正竞争时以 CAS 的顺序为准，不额外规定 cancel() 被调用即必须胜出；
  但任何成功结果必须未被旧握手 watchdog 关闭。

没有新私有生产 helper、公开 API/Options 变化、额外生产 sleep 或轮询。
只新增加 join，不增加 watchdog 的工作：正常停止只需一个 select 和 defer；
TCP Close 唤醒阻塞 I/O，wss 的 TLS Close 沿用 Go 1.25 自身 close-notify 最长 5 秒
写期限（`crypto/tls/conn.go:1474-1481`），未引入无限网络等待。
join 依赖正常 goroutine 调度与 Context 方法按约定返回，并非宣称实时调度期限。

## 3. 永久测试与所有权

新增 `internal/transport/ws/handshake_handoff_test.go`：

| 覆盖 | 精确位置 | 实际行为证据 |
|---|---|---|
| 成功返回立即取消 | `TestDialHandoffSurvivesContextCancellation :27-42` | 101/status/header、预读二进制首帧、上行完整文本、下行完整文本、Close 与实际 EOF |
| 请求已到达但握手不应答 | `TestDialBlockedHandshakeCancellation :45-68` | ready 门闩后 cancel，nil Conn + ErrHandshake + nil resp，有界返回，上游 request context/handler 归还 |
| 迟到 watchdog / join | `TestDialHandoffJoinsWatchdog :72-123` | 暂停真实 watchdog 的 Done 访问；101 已写完而未放行时 Dial 不得返回；取消后放行，只允许错误无连接或真实可用连接 |
| 取消与 101 竞争 | `TestDialHandoffCancellationRace :126-171` | 16 次本机 TCP 竞争/每轮；错误必须无 Conn，成功必须通过真实双向消息/关闭证据；取消与 Dial 工作者 join |
| 受控 context seam | `dialWatchdogGateContext :173-195` | 仅测试文件内 context，门闩模拟合法迟到调度；没有生产插桩 |
| 真 socket 对端 | `newDialHandoffPeer :208-285` | 原生 httptest Hijack、101 与首帧同一次 Flush、实际 Close 帧(code 1000/reason)与 EOF，底层 socket 清理、handler join |
| 消息与响应保全 | `assertDialHandoffExchange :289-311` | 独立字面 payload/opcode/header/关闭断言；实际 SetDeadline/Read/Write/Close |
| 即使失败也归还 worker | `startDialHandoff :314-335` | cleanup 取消、放行 gate、join Dial，未领取的缓冲结果若持有 Conn 则直接释放 |

所有测试只用真实本机 TCP/httptest，未用 net.Pipe。测试网络与 join 有 3 秒上界；
100 ms 只用于“门闩未放行时不应提前返回”的负观察窗，不用 sleep 修复产品竞态。
101 的真实写完成、watchdog 进入门闩均有 ready 通道，不靠盲等猜测握手阶段。

兼容边界未动：响应非 101、摘要错误仍返回原 response，继续以 `ErrHandshake` 分类；
`bufio.Reader` 仍传进 `newConnBuffered`；Accept、TLS 握手、URL/请求头、Idle、MaxPayload
与关闭帧语义不变。Background 无 Done、不合法摘要、401 的既有负例及互通测试
随两轮全仓普通/race 验证，不增加无 Done 的等待死锁。无取消时 defer 停止通道必 ready。

## 4. 先 RED 后 GREEN（真实执行顺序）

1. **生产未编辑时**先创建永久测试。`red-watchdog-join` exit **1**：
   `TestDialHandoffJoinsWatchdog` 因 Dial 在 watchdog 门闩未放行时已成功返回而失败。
   编译通过、真实 TCP 已完成 101；不是缺符号或编译 RED。
2. 同样原产品的 `red-all-handoff` exit **1**：
   join 测试除了上述关闭权失败，还实际捕获到成功后上行写
   `use of closed network connection`。成功后取消、阻塞取消与自然竞争其余测试首跑通过，
   不把自然通过当作原缺陷不存在，也不虚报它们各自先 RED。
3. 才改 handshake.go 的 CAS/join。`green-all-handoff` exit **0**。
4. GREEN 后加强 cleanup 与 join 测试：门闩放行之前 cancel，使取消和停止都 ready，
   防止只补 join 却仍允许取消关闭已交接 socket；随后全部最终重验通过。
5. **后验外部负控制**（不是冒充首次 TDD RED）：用 `git show BASE:...` 创建外部
   `baseline-handshake.go` + Go overlay，最终永久测试对原产品仍 exit **1**，
   捕获 join 失败及下行真实 TCP 已关闭。
6. 外部 `join-only-handshake.go` overlay 只保留 join，删除两个 CAS；
   `join-only-overlay-red` 24 轮中 **12 轮** `TestDialHandoffJoinsWatchdog` 因
   成功结果的实际 socket 已关闭而失败，exit **1**。说明不能只停通知/join、遗漏互斥仲裁。
   该负控制不改仓库产品，不是产品修复，也不声称固定 select 的随机选择。

本任务没有重跑或覆盖旧 controller overlay。旧报告的 13/24、16/16 因果探针与两次
无插桩自然失败历史保持；本次 RED/负控制只认证 D1 实际存在及本回归可抓住它，
不倒推旧两次自然失败的准确触发栈。

## 5. 全部命令、exit 与完整日志

完整 stdout/stderr 日志根目录（下表文件名均相对此目录）：

`/private/var/folders/r6/9bfgzbnj3kgdxb7h9x_37kmr0000gn/T/opencode/ws-dial-handoff-d1/`

每项有同名 `*-command.json`，包含 exact command、cwd、exit 与绝对 log 路径。
后续统一保存器 `verify.py` 捕获完整输出，tool 仅展示尾部，不删截日志。
下列所有 Go/make 命令开头均有 **`GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local`**；
为免表格过宽仅此公共前缀在表格中省略。

| 日志名 `.log` | 完整命令（加上述公共前缀） | exit / cache |
|---|---|---|
| red-watchdog-join | `go test ./internal/transport/ws -run TestDialHandoffJoinsWatchdog -count=1 -v -timeout=30s` | **1** / uncached |
| red-all-handoff | `go test ./internal/transport/ws -run "TestDial(Handoff\|BlockedHandshake)" -count=1 -v -timeout=45s` | **1** / uncached |
| green-all-handoff | `go test ./internal/transport/ws -run "TestDial(Handoff\|BlockedHandshake)" -count=1 -v -timeout=45s` | 0 / uncached |
| focused-repeat | `go test ./internal/transport/ws -run "TestDial(Handoff\|BlockedHandshake)" -count=100 -v -timeout=120s` | 0 / uncached |
| focused-race-repeat | `go test -race ./internal/transport/ws -run "TestDial(Handoff\|BlockedHandshake)" -count=100 -v -timeout=120s` | 0 / uncached |
| replay-close-repeat | `go test ./internal/testkit -run "TestWSReplay(Termination\|WaitsForClose\|Close\|UnexpectedClose\|ClosedEndpoint\|PassiveClose\|UnsentClose\|SendingClose\|RejectsPartialBeforeNormalClose\|LocalCloseRejectsIncompleteReply)" -count=200 -v -timeout=240s` | 0 / uncached |
| full-suite | `go test ./... -count=1 -timeout=240s` | 0 / uncached |
| full-race-suite | `go test -race ./... -count=1 -timeout=240s` | 0 / uncached |
| make-check | `make check` | 0 / 部分包 cached，ws/testkit 实跑；matrix cached |
| make-matrix | `make matrix` | 0 / cached |
| replay-close-race-repeat | `go test -race ./internal/testkit -run "TestWSReplay(Termination\|WaitsForClose\|Close\|UnexpectedClose\|ClosedEndpoint\|PassiveClose\|UnsentClose\|SendingClose\|RejectsPartialBeforeNormalClose\|LocalCloseRejectsIncompleteReply)" -count=100 -v -timeout=180s` | 0 / uncached |
| single-p-repeat | `GOMAXPROCS=1 go test ./internal/transport/ws -run "TestDial(Handoff\|BlockedHandshake)" -count=50 -v -timeout=90s` | 0 / uncached |
| final-baseline-overlay-red | `go test -overlay=/private/var/folders/r6/9bfgzbnj3kgdxb7h9x_37kmr0000gn/T/opencode/ws-dial-handoff-d1/baseline-overlay.json ./internal/transport/ws -run TestDialHandoffJoinsWatchdog -count=1 -v -timeout=30s` | **1** / uncached |
| join-only-overlay-red | `go test -overlay=/private/var/folders/r6/9bfgzbnj3kgdxb7h9x_37kmr0000gn/T/opencode/ws-dial-handoff-d1/join-only-overlay.json ./internal/transport/ws -run TestDialHandoffJoinsWatchdog -count=24 -v -timeout=45s` | **1** / uncached（12 失败） |
| final-focused-repeat | `go test ./internal/transport/ws -run "TestDial(Handoff\|BlockedHandshake)" -count=100 -v -timeout=120s` | 0 / uncached |
| final-focused-race-repeat | `go test -race ./internal/transport/ws -run "TestDial(Handoff\|BlockedHandshake)" -count=100 -v -timeout=120s` | 0 / uncached |
| final-full-suite | `go test ./... -count=1 -timeout=240s` | 0 / uncached |
| final-full-race-suite | `go test -race ./... -count=1 -timeout=240s` | 0 / uncached |
| final-make-check | `make check` | 0 / 大部分 cached，ws 实跑；matrix cached |
| final-single-p-repeat | `GOMAXPROCS=1 go test ./internal/transport/ws -run "TestDial(Handoff\|BlockedHandshake)" -count=50 -v -timeout=90s` | 0 / uncached |

Markdown 表格的 `\|` 是展示转义，不是 shell 正则中的反斜杠；exact 命令以 JSON 为准。
所有正常最终验证 exit 0，没有普通/race suite 的实际失败或 DATA RACE。
唯一失败都是上述具名永久回归的原产品 RED / 明确外部负控制；没有忽略既有 suite 失败。
旧 testkit helper 原样立即 cancel，关闭回放普通 200 次/race 100 次均无失败。

此外实际 `gofmt -w` 仅作用两个允许源码文件；`GIT_MASTER=1 git diff --check`、
`GIT_MASTER=1 git diff --cached --check` 均 exit 0，无输出。
一次 harness 工具传输 ECONNRESET 中断没有执行验证命令，也不冒充 suite 失败。

## 6. 自检与提交

自检逐项确认：

- CAS 是同一原子变量、仅一次认领；取消获胜的失败分支保留 response。
- watchdog 必在 Close 返回后发退出信号；成功获胜后不会再执行底层 Close。
- Background nil Done 由停止通道唤醒；所有拨号后 error return 都经过同一 defer join。
- 没有动 ErrHandshake 的既有包装风格或旧 response/预读职责。
- test owner 保留原 Close 证据，不把幂等 nil 当成功发送；实际对端检查 close/EOF。
- 测试 seam 不篡改 Done/Err 的值，只延迟调用；Context、worker、handler、socket 有清理。
- BASE 相比仅产品文件、新永久测试与本报告发生变化；没有 testkit/golden/matrix 绕过。

提交前 scoped stage 仅 `handshake.go` 与 `handshake_handoff_test.go`，
`git diff --cached --name-only` 严格仅两文件。产品提交：

`c6554cf0d21578dc36c0192204e51f34f4224a68` — `fix(ws): 同步交接 Dial 握手 watchdog 关闭权`。

此产品 HEAD 在写报告前已实际验证 clean：`GIT_MASTER=1 git status -sb` 仅分支行。
本报告以单独 docs scoped 提交交付；含报告的最终 HEAD 是该 docs 提交本身，
其精确 SHA 与最终 clean 结果在控制器回报以及外部 `final-git-state.json` 同步记录，
避免在报告自身写入不可能自引用的 commit hash。可由
`GIT_MASTER=1 git log -1 --format=%H -- .superpowers/sdd/2026-10-01-ws-dial-handoff/task-1-report.md`
定位。没有宣称已独立 review、已 merge 或已发布。

## 7. Concerns / 证据边界

1. **测试 seam 的维护边界**：`dialWatchdogGateContext.Done` 用 runtime caller 名
   `/ws.Dial.func` 区分生产 watchdog 与 net.Dialer 对 Done 的访问。
   不断言源码文本、没有生产 hook；断言结果是实际 TCP 行为。
   若以后把 watchdog 抽成命名函数或改包路径，需要同步 seam，否则进入门闩会有界失败；
   它不会在未进入 seam 时静默通过。普通、race、GOMAXPROCS=1 都已实际验证。
2. 100 ms 是门闩仍闭合时的负观察窗：极端负载可令“去掉 join”的变异尚未完成握手，
   使单次负控制漏报；原产品首次测试与最终 BASE overlay 均实际 RED。
   所有成功结果的消息/关闭断言不依赖这个窗口。
3. 明确迟到调度由测试局部 context 控制；自然竞争重复提供额外保护但不承诺网络顺序
   或两种胜者必定各发生多少次。join-only 的 12/24 是实际负控制结果，不宣称确定概率。
4. 全部为本机 TCP 机制证据，无真实上游/模型、凭据、音频、P3 接线或能力兑现证明。
   原诊断未认证的两次自然失败历史不被本任务追溯认证或删除。
5. 控制器另行独立 review 仍是下一步；本 leaf 未自派 reviewer，也不把本自检称作独立复核。
