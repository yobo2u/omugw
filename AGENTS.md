# PROJECT KNOWLEDGE BASE

**Generated:** 2026-08-09
**Commit:** 6275d36
**Branch:** main

## OVERVIEW

omugw 是多协议双向转换的 AI 网关数据面（Go 1.25，仅 3 个直接依赖：yaml.v3 /
prometheus client / go-cmp）。核心命题不是「怎么调通上游」，而是**协议之间搬运
请求时哪些语义会丢、丢了要不要让人知道**——全部答案落在 `internal/degrade` 的
降级矩阵里。

**先读 `CONTEXT.md`**（术语表）**与 `docs/architecture/principles.md`**（七条原则
及其代码强制点）。本仓库的每一条奇怪设计都能在那两份文档里找到理由。

## STRUCTURE

```
cmd/omugw/          # 唯一二进制：加载配置 → 构建矩阵 → 起两个 HTTP server
internal/
├── canonical/      # 中间表示（IR）+ 能力枚举 + 错误分类 + 流事件
├── degrade/        # 降级矩阵：全部路径 × 能力的处置声明，编译期强制完整
├── protocol/       # 线格式编解码；*wire 包只做错误信封
├── provider/       # 出站适配器接口；passthrough 是同源快通道实现
├── gateway/        # 唯一把各层串起来的地方；跟踪下游首字节
├── router/         # 模型名 → 候选上游（精确 > 最长前缀 > `*`）
├── transport/      # httpx（四层超时）、sse（事件读写）
├── credential/     # 凭据池：加权轮询 + 按错误类型冷却
├── discovery/      # 问上游「你有哪些模型」；只充实 /v1/models 清单，绝不建路由
├── convstore/      # 内存态会话树，为无状态上游垫出 Responses 的服务端会话
├── config/         # YAML 加载 + ${ENV} 展开 + 跨字段校验
├── obs/            # 脱敏 slog + Prometheus 指标
└── testkit/        # fixture 录制/离线回放 + golden 断言
testdata/
├── fixtures/       # 按 provider 归档的上游响应录制
└── routes/<in>__<out>/  # 路径级端到端用例 + golden/
docs/               # principles.md、degradation-matrix.md（生成物）、adr/
```

## WHERE TO LOOK

| 任务 | 位置 | 备注 |
|---|---|---|
| 新增/修改一条转换路径的处置 | `internal/degrade/rules_phase1.go` | 改完必跑 `make matrix` |
| 转正路径 / 投放能力 | `internal/degrade/rules_phase1.go` 的 `Redeem(ep, ...)` | 按端点显式列出已投放的能力；同步 `matrix_test.go` 两份名单 |
| 新增一项 Capability | `internal/canonical/capability.go` | 常量 + `AllCapabilities()` 两处；所有已注册路径会同时失败，这是刻意的 |
| 某协议表达不出某能力 | `internal/degrade/expressibility_phase1.go` | 声明进 `Elsewhere` 或 `Impossible`，别写进路径规则 |
| 请求主链路 | `internal/gateway/handler.go:191` (`serve`) → `:263` (`dispatch`) | 鉴权→解码→路由→矩阵→凭据→Provider→回写 |
| 首字节后不许重试的实现点 | `internal/gateway/handler.go:426` (`fail`)、`relay.go:25` | `tracked.wrote` 是唯一判据 |
| 出站适配器接口 | `internal/provider/provider.go:47` | 同时拿 `Raw` 与 `Canonical`，适配器自己挑 |
| 新增一个协议族的模型枚举 | `internal/discovery/` + `Probers()` | 只登记**有官方列表接口**的协议族；`dashscope.compatible` 刻意缺席 |
| `/v1/models` 清单从哪来 | `internal/gateway/models.go` | 配置模型（权威）+ 发现快照（补充），按 ID 去重与排序 |
| 启动装配 | `internal/gateway/build.go:36` | 未实现的协议族在此直接拒绝启动 |
| DashScope Native 未投放端点兜底 | `internal/gateway/build.go:166` | `POST /api/v1/` 前缀兜底返回协议化 501，先于主链路拦下请求 |
| 错误分类与响应头 | `internal/canonical/error.go` + `internal/protocol/*wire/` | |
| 加/改 fixture | `testdata/routes/<in>__<out>/` | 目录名由 `degrade.FixtureDir()` 决定 |

## CODE MAP

| 符号 | 类型 | 位置 | 角色 |
|---|---|---|---|
| `degrade.Matrix.Check` | Method | `internal/degrade/matrix.go:682` | 全部处置裁决的唯一入口；按入站坐标（协议 + 端点）裁决，fail-closed |
| `degrade.Matrix.BestOutbound` | Method | `internal/degrade/preference.go:271` | 候选上游排序 + 能力校验，返回 Verdict |
| `degrade.Route.Build` | Method | `internal/degrade/matrix.go:430` | 路径级校验唯一执行点：缺一格能力声明、未知处置、已知门错绑协议都报错；成功后 Route 封口 |
| `degrade.Matrix.Add` | Method | `internal/degrade/matrix.go:605` | 路径入库唯一入口：只收已 Build 的路径，拒绝 nil——Build 校验绕不过去 |
| `degrade.Route.Redeem` | Method | `internal/degrade/matrix.go:307` | 登记指定端点已投放的能力；`Redeems(ep,c)` / `RedeemedAt(ep)` / `ImplementedAt(ep)` 由它派生 |
| `degrade.Route.Endpoints` | Method | `internal/degrade/matrix.go:344` | 已开门清单，字典序；门的存在只从兑现格子推导 |
| `degrade.routeState` | Type | `internal/degrade/matrix.go:105` | 锁 / 封口位 / 快通道位 / 错误清单，以指针由 Route 的全部浅拷贝共享；`copy := *r` 绕不过封口 |
| `canonical.Capability` | Type | `internal/canonical/capability.go:8` | 矩阵第三维，27 项 |
| `canonical.Usage.Fidelity` | Field | `internal/canonical/usage.go` | 零值非法，`Validate()` 拒绝 |
| `provider.Provider` | Interface | `internal/provider/provider.go:47` | `Kind()` + `Call()`，返回原始 `*httpx.Response` |
| `gateway.Handler.dispatch` | Method | `internal/gateway/handler.go:263` | Provider × 凭据两层 failover |
| `credential.Pool.Acquire` | Method | `internal/credential/credential.go:161` | 返回 Lease，必须 `Succeed()`/`Fail()` |
| `router.Router.Resolve` | Method | `internal/router/router.go:155` | 只给候选，不做能力裁决 |
| `discovery.Probers` | Func | `internal/discovery/discovery.go` | 协议族 → 枚举实现；查不到即跳过（不是错误） |
| `discovery.Refresher.RefreshOnce` | Method | `internal/discovery/refresh.go` | 单个 endpoint 失败不影响其余；失败保留上一次快照 |

## CONVENTIONS

- **注释与文档一律中文**，且写「防的是什么」而非「做了什么」。新增注释请沿用。
- **矩阵是唯一权威**：未登记的 `(入站协议, 出站 Provider, 能力)` 组合一律拒绝。
  漏配一格 → 请求被拒（可见），而不是丢半数字段返回 200（不可见）。
- **转正按端点 × 能力粒度兑现**（ADR-0001）：`Route.Redeem(ep, ...)` 显式列出
  该门**当前已投放**的能力，前提是该端点的端到端 fixture 已通过（fixture 门槛
  按 `request.path` 与门清单双向对账）。仍需三处同改：代码 +
  `testdata/routes/<in>__<out>/` fixture + `matrix_test.go` 的两份名单
  （`TestImplementedRoutesAreExplicit` 管路径、`TestRedeemedCapabilitiesAreExplicit`
  管「路径 @ 端点」能力）。少一处就过不了 CI。但 CI 只查到门级集合与 fixture
  存在；**逐项能力跑没跑通，靠改白名单的人负责**。
- **保留度有两列**（ADR-0002）：`DesignScore()` 是设计目标，`AvailableScore()` 是
  当前可用。**运行时选路只许用后者**。
- **采样参数用指针**（`*float64`），因为 `0` 与「未设置」必须可区分。
- `docs/degradation-matrix.md` 是生成物，改代码后跑 `make matrix-update`，别手改。
- 未实现的路径与未投放的能力都返回 **501**（还没建好，等实现，不该让客户端改请求），
  能力不支持返回 **422**。

## ANTI-PATTERNS (THIS PROJECT)

- **不要**把 OpenAI 格式当内部总线——Canonical 是独立 IR，不是 OpenAI 的别名。
- **不要**在异构路径上从 `Extensions` 猜测语义；它只服务同源快通道的原样回填。
- **不要**在首字节之后重试或 failover，无论错误多么 `Retryable`。
- **不要**把 `Retryable` 理解成「上游临时故障」；它的语义是「换一个凭据或 Provider
  可能成功」，所以 auth/quota 是 `true`，context_length/content_filter 是 `false`。
- **不要**让 `Usage` 走零值路径；流式中断必须显式标 `FidelityUnavailable`。
- **不要**代下载多模态 URL，也不要跨 Provider 搬运 `FileRef`（显式 REJECT）。
- **不要**在 `cmd/omugw/main.go` 的 `http.Server` 上设 `WriteTimeout`——会掐断长流式。
- **不要**在 core 里写 OAuth 刷新 / 账号冷却 / 指纹伪装，那些属于 omsub 仓库。
- **不要**在日志或错误里出现 `credential.Secret`。
- **不要**让 `internal/discovery` 的结果流进 `Router`：上游目录不等于本网关可调用
  的集合（DashScope 的列表返回「平台上可用的模型」，OpenAI 的列表也不带「这把 key
  能否调用」的位）。发现只充实 `/v1/models` 清单；发现即建路由等于让一次上游目录
  变更把请求送去一个降级矩阵从未审视过的目的地。
- **不要**计算或展示路径级「当前可用」聚合分：各门兑现集合的并集不对应任何
  一扇真实存在的门（两门并集 8/18 已被显式否决）；可用列一律端点相对
  （`Preservation(avail, ep)`），绝不从 `Preservation(avail, Endpoint(""))` 取可用列。
- **不要**把 wire-compatible 读成同源：`openai.chat → dashscope.compatible` 复用 Chat
  线格式只是因为不需要重编码，语义是异构的（搜索选项降成开关、strict schema 无全局
  保证），不得 `MarkHomogeneous`，降级头由矩阵照常生成。

## Agent skills

### Issue tracker

工作项使用 `yobo2u/omugw` 的 GitHub Issues；操作与 Wayfinder 约定见
`docs/agents/issue-tracker.md`。

### Triage labels

外部问题按五种标准角色流转；标签映射见 `docs/agents/triage-labels.md`。

### Domain docs

本仓库采用 single-context：根 `CONTEXT.md` 是术语表，系统级决策位于
`docs/adr/`；消费规则见 `docs/agents/domain.md`。

### 开工前置检查（worktree 工作流）

本仓库用 `superpowers` 的 worktree 工作流，工作树落在
`~/.config/superpowers/worktrees/omugw/` 下——**不在仓库目录里，`ls` 看不见**。
于是同一个人可以在两个工作树上并行推进两条线，而任一边的 `git status` 都显示
「干净且领先远端」，看不出另一条线的存在。

因此**任何 agent 动手前必须跑完这三条**，缺一条就可能在过期基线上开发：

```bash
GIT_MASTER=1 git fetch origin          # git status 只看本地，看不出远端已经走了
GIT_MASTER=1 git worktree list         # 别的工作树在推进什么
GIT_MASTER=1 git status -sb            # 带 ahead/behind，比裸 status 多这一半信息
```

看到 behind 就先同步再开工；看到第二个工作树就先确认它那条线的状态。
新工作一律开分支，**不要直接在 `main` 上堆提交**——那会让本地 `main` 同时
「有未推送内容」和「落后于远端」，是最容易撞车的形态。

分支合并后立刻 `git worktree remove` 并删分支：一条已合过 PR 却没清理的工作树
会继续长提交，下次合并时冲突面更大。

## COMMANDS

```bash
make test          # 单元测试 + fixture 矩阵（全离线，CI 不需要任何 API Key）
make test-race     # 竞态检测
make matrix        # 矩阵完整性 + 文档同步断言
make matrix-update # 重新生成 docs/degradation-matrix.md
make golden-update # 重写 golden（重写后必须人工审阅 diff）
make check         # fmt-check + vet + test + matrix，等价于 CI
make smoke         # 端到端冒烟，需 OMUGW_SMOKE=1 与真实凭据
```

## NOTES

- **当前状态**：14 条路径已登记，5 条已通车（`openai.responses →
  openai.compat`、`openai.chat → openai.compat`、`dashscope.native →
  dashscope.native`，均为同源直通；以及第一条异构路径 `openai.chat →
  dashscope.compatible`）。兑现粒度是**端点 × 能力**：OpenAI 两条直通在各自的
  唯一门上兑现全部可表达能力；DashScope Native 投放了两个端点——文本生成与
  多模态生成，每门 18 项中 5 项；Chat 到 DashScope Compatible 是 wire-compatible
  而非同源——请求保持 Chat 线格式、只做定点修补（model 改写与
  web_search_options → enable_search），在 `/v1/chat/completions` 门兑现 9 项
  可交付能力（7 PASS + 2 DEGRADE），file_input / audio_output 维持 REJECT 422，
  设计与可用均为 8/11 ≈ 0.727。Chat 到 DashScope Native 是首条请求与响应都完整
  重编码的异构路径，在 `/v1/chat/completions` 门兑现 8 项能力（text_generation /
  streaming / tool_calling / parallel_tool_calls / structured_output / reasoning /
  vision_input / web_search）；`audio_input` 设计处置保持 PASS 但无真实证据、
  本期不兑现，含它的请求由矩阵在触达上游前返回 501；门可用分 6.5/11 ≈ 0.591，
  `Gated()` 为真，设计分 7.5/11 ≈ 0.682 不变。其余 Native POST 端点由
  `POST /api/v1/` 兜底返回 DashScope 协议化 501。其余 9 条路径打过去仍是 501。
- 配置的 `auth`/`credentials`/`providers`/`models` 四块**要么全配要么全不配**；
  全不配 = 只提供 `/healthz`（合法形态），配一半直接启动失败。
- `convstore` 是内存态：单副本正确、重启丢失、多副本不共享。因此
  `FeatureConversationStore` 默认关闭，`EMULATE` 格子在默认配置下不可用。
- CI 拿 `OutboundPreference` 的顺序与矩阵里实际的透传格子数对账；新增一条「声称
  优先但实际丢更多」的路径会被 `TestPreferenceMatchesPreservation` 拦下。
- 依赖许可证是硬门槛：`forbidden`/`restricted`/`unknown` 一律拒绝进 Clean Core。
