# internal/degrade

降级矩阵：把「这条路径对这项能力怎么处置」变成可查询、编译期强制完整的数据。
仓库最大的包，也是所有「不许静默丢字段」约束的落点。

## STRUCTURE

| 文件 | 职责 |
|---|---|
| `matrix.go` | `Protocol`/`Provider`/`Disposition`/`Rule`/`Route`/`Matrix`/`Verdict` 全部核心类型 + `Check()` |
| `endpoint.go` | `Endpoint` / `Inbound` 类型与四扇门常量（DashScope 门复用协议包路径常量） |
| `expressible.go` | 可表达性机制：能力落进 `Capabilities` / `Elsewhere` / `Impossible` 三桶之一 |
| `expressibility_phase1.go` | 六个入站协议的可表达性声明，`init()` 时注册 |
| `rules_phase1.go` | `Phase1()` 构建全部 14 条路径的处置 |
| `preference.go` | 入站优先级、出站选路偏好、Availability 开关、保留度打分 |
| `markdown.go` | 渲染 `docs/degradation-matrix.md`；`FixtureDir()` 决定 fixture 目录名 |

## WHERE TO LOOK

| 任务 | 位置 |
|---|---|
| 加一条路径 | `rules_phase1.go` 里 `NewRoute(...).Pass/Degrade/Reject/Emulate(...).Build()` |
| 从已有路径派生 | `Route.Derive()` (`matrix.go:284`) + `Override()` (`:304`) |
| 路径转正 / 能力投放 | `Route.Redeem(ep, caps...)` (`matrix.go:210`) — 显式列出指定端点已投放的能力，需先有该端点的 fixture 通过 |
| 查某能力是否已投放 | `Route.Redeems(ep, c)` (`matrix.go:224`)；`ImplementedAt(ep)` (`:257`) 答「这扇门开了没」，`Endpoints()` (`:245`) 列已开门，`Implemented()` (`:266`) 只答「路径通车了没」 |
| 端点级 fixture 证据 | `matrix_test.go` 的 `checkRouteFixtures` (`:449`) — 按 fixture 的 `request.path` 与门清单双向对账 |
| 选路排序逻辑 | `RankOutbound` (`preference.go:215`) / `BestOutbound(Inbound,…)` (`:271`) |
| 保留度算法 | `Preservation.DesignScore` (`preference.go:143`) / `AvailableScore` (`:155`) |
| 新增运行时开关 | `preference.go:86` 附近的 Feature 常量 + `DefaultAvailability()` |

## CONVENTIONS

- **五种处置**：`PASSTHROUGH` / `EMULATE` / `DEGRADE` / `REJECT` / `N/A`。
- **设计处置与当前投放分离**：`Pass`/`Degrade`/`Reject`/`Emulate` 说的是这条路径
  最终该怎么走；`Redeem` 说的是哪些能力的实现真的写完了。`Check` 对已声明但未投放
  的能力返回 501（`ClassNotImplemented`），而不是当作可用放过去。
- `Degrade` 的 `Note` 必须写清**丢了什么**——它会进 `X-Omugw-Degraded` 响应头。
- `Reject` 的 `Note` 必须写清**为什么**——它会成为客户端错误消息的一部分。
- `Emulate` 必须带非空 `RequiresFeature`，且必须在 `Note` 里写明运维代价。
- `N/A` **由 `Expressibility` 自动推导，不允许手工声明**，且不计入保留度分母。
- **兑现挂在端点上**：`Redeem(ep, caps...)` 以 (端点, 能力) 二元键登记投放，
  门与门的兑现集合互不相通，不做并集。门的存在只从兑现格子推导
  （`Endpoints()`：至少一格兑现才算开），没有独立的门注册机制。
  **不存在**路径级「当前可用」聚合分——可用列永远端点相对。
- 同源快通道（`MarkHomogeneous`）在选路时**永远优先于**全局 `OutboundPreference`。

## ANTI-PATTERNS

- **不要**用 `DesignScore()` 做运行时选路——它假装所有能力都在线。
- **不要**在没有 fixture 的情况下 `Redeem(ep, …)`；且必须同步改 `matrix_test.go` 的
  `TestImplementedRoutesAreExplicit`（路径）与 `TestRedeemedCapabilitiesAreExplicit`
  （「路径 @ 端点」能力）两份白名单。代码守的两道闸只到这个粒度：
  `TestImplementedRoutesHaveFixtures` 查路径有 fixture 目录、有损格子有同名用例、
  并按 fixture 的 `request.path` 与门清单双向对账（每扇已开门要有证据，每份证据
  要指向已开门），白名单查兑现集合有人点头。**没有**「每兑现一项能力就自动对上
  一份 fixture」这回事——对账只到「这扇门有人敲过」，逐项能力那一步靠人。
- **不要**用两门兑现集合的并集记「路径可用分」——并集不对应任何一扇真实存在
  的门；可用分一律逐门用 `Preservation(avail, ep)` 算。
- **不要**把 `Implemented()` 读成「每项能力都能走」——它只说明至少一扇门已开；
  单扇门能不能走要另问 `ImplementedAt(ep)`，单项能力能不能走要另问 `Redeems(ep, c)`。
- **不要**为「入站协议表达不出」的能力写路径级 `Reject`——那是可表达性问题，
  写进 `expressibility_phase1.go` 的 `Elsewhere`/`Impossible`。
- **不要**给未设计好可表达性声明的协议预留 `Protocol` 常量（见 `matrix.go:29` 的
  注释）——那会让后来者撞上「缺少可表达性声明」这种在说代码问题的假错误。
- **不要**在 `RankOutbound` 找不到路径时兜底到某个默认 Provider；返回空列表。

## GOTCHAS

- 请求带了一项该入站协议标为 `N/A` 的能力 → `Check()` 返回 `ClassInternal`（解码器
  bug），**不是** `ClassUnsupported`（客户端错误）。这个区分是刻意的。
- `EMULATE` 开关关闭时的错误必须说「开关没开」，而不是「路径不支持」。
- 新增 `canonical.Capability` 常量会让**所有**已注册路径同时构建失败——这是设计。
- `Derive()` 不继承兑现集合（见 `matrix.go:282` 注释）：派生路径默认仍是 PLANNED，
  投放必须逐条重新声明，否则一条还没动工的派生路径会宣称自己可用。
- 已声明但未投放的能力 → `Check()` 返回 501 而非 422：501 说「等实现」，422 说
  「改请求」，方向不能混。
- `Check` 敲未开门（含零值端点）→ 501 且消息点名端点；空能力集也不豁免——
  门开没开是入口约束，与请求带了什么能力无关。
- `Preservation(avail, Endpoint(""))` 的可用列恒为零：零值端点永远没有兑现，
  这条调用只可用于取设计列（设计计数与 DesignScore 是路径级的，与端点无关）。
- `docs/degradation-matrix.md` 由 `Markdown()` 生成，`make matrix` 会断言它与代码同步。
