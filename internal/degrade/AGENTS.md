# internal/degrade

降级矩阵：把「这条路径对这项能力怎么处置」变成可查询、编译期强制完整的数据。
仓库最大的包，也是所有「不许静默丢字段」约束的落点。

## STRUCTURE

| 文件 | 职责 |
|---|---|
| `matrix.go` | `Protocol`/`Provider`/`Disposition`/`Rule`/`Route`/`Matrix`/`Verdict` 全部核心类型 + `Check()` |
| `expressible.go` | 可表达性机制：能力落进 `Capabilities` / `Elsewhere` / `Impossible` 三桶之一 |
| `expressibility_phase1.go` | 六个入站协议的可表达性声明，`init()` 时注册 |
| `rules_phase1.go` | `Phase1()` 构建全部 14 条路径的处置 |
| `preference.go` | 入站优先级、出站选路偏好、Availability 开关、保留度打分 |
| `markdown.go` | 渲染 `docs/degradation-matrix.md`；`FixtureDir()` 决定 fixture 目录名 |

## WHERE TO LOOK

| 任务 | 位置 |
|---|---|
| 加一条路径 | `rules_phase1.go` 里 `NewRoute(...).Pass/Degrade/Reject/Emulate(...).Build()` |
| 从已有路径派生 | `Route.Derive()` (`matrix.go:193`) + `Override()` (`:213`) |
| 路径转正 | `Route.MarkImplemented()` (`matrix.go:182`) — 需先有 fixture |
| 选路排序逻辑 | `RankOutbound` (`preference.go:182`) / `BestOutbound` (`:236`) |
| 保留度算法 | `Preservation.DesignScore` (`preference.go:130`) / `AvailableScore` (`:142`) |
| 新增运行时开关 | `preference.go:86` 附近的 Feature 常量 + `DefaultAvailability()` |

## CONVENTIONS

- **五种处置**：`PASSTHROUGH` / `EMULATE` / `DEGRADE` / `REJECT` / `N/A`。
- `Degrade` 的 `Note` 必须写清**丢了什么**——它会进 `X-Omugw-Degraded` 响应头。
- `Reject` 的 `Note` 必须写清**为什么**——它会成为客户端错误消息的一部分。
- `Emulate` 必须带非空 `RequiresFeature`，且必须在 `Note` 里写明运维代价。
- `N/A` **由 `Expressibility` 自动推导，不允许手工声明**，且不计入保留度分母。
- 同源快通道（`MarkHomogeneous`）在选路时**永远优先于**全局 `OutboundPreference`。

## ANTI-PATTERNS

- **不要**用 `DesignScore()` 做运行时选路——它假装所有能力都在线。
- **不要**在没有 fixture 的情况下 `MarkImplemented()`；`TestImplementedRoutesHaveFixtures`
  会失败，且必须同步改 `matrix_test.go` 的 `TestImplementedRoutesAreExplicit` 白名单。
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
- `docs/degradation-matrix.md` 由 `Markdown()` 生成，`make matrix` 会断言它与代码同步。
