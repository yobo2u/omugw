# Realtime 最小会话语义：同契约直通 + 显式建模，不建通用 IR

**状态**：accepted（方向已确认，未实现）。契约细节与验收标准见
[Realtime 最小会话契约](../superpowers/specs/2026-09-28-realtime-minimal-session-contract.md)。

Realtime 是双向长会话：配置可以中途改，音频参数要协商，打断分取消与截断两种
语义，用量散落在多个事件里，HTTP 侧的转换经验不能直接搬。已确认的方向有两条
腿：**同契约直通在负载层面保住任意字段**；**跨协议转换只做最小的、显式的、
presence-aware 的有状态映射**。不建通用 realtime IR，也不做盲目的 JSON 改名表。

结构决定沿用两张已关闭的决策票，本 ADR 不重开：[realtime 会话事件的 IR 建模](https://github.com/yobo2u/omugw/issues/6)
定了上行指令用独立的 `RealtimeCommand` 类型、同协议直通路径 bypass IR、同源
直通解帧窥探 `type` 以观测 usage；下行复用既有的 `canonical.Event` 与 `Usage`，
10 个 realtime 事件常量已在 `internal/canonical/stream.go` 里，保留不删。
[双向长连接的出站适配器抽象](https://github.com/yobo2u/omugw/issues/5) 定了平行的 `StreamProvider` 接口、
先 Dial 上游再 Accept 下游、101 发出即关闭重试窗口、`total` 对 WS 不适用。

## 决定

1. **同契约直通**：两端版本、字段语义及有效编码参数一致时，消息负载原样转发，
   未建模字段按不透明负载保全；不承诺网络帧相同，掩码与分片由两段连接分别处理。
   握手凭据（含承载 key 的子协议 token）须清洗并替换为授权的出站凭据。
   不透明字段被转发不等于已验证其语义，更不能作为跨 Provider 等价性的证据。
2. **跨协议最小显式转换**：建模本期兑现能力涉及的命令、结果与关联状态，包括
   文本、工具、会话配置、媒体、错误和用量；不局限于五个 realtime 专属能力。
   逐项显式映射，状态记录保持 missing / null / value
   三分（`false`、`0`、空集合都是值，不是「没设」）；配置分 requested/pending
   与 confirmed 两态，确认须核对更新关联、实际值及契约语义，不能只查字段存在；
   服务端错误或含糊回显不得静默 commit；缓冲与队列必须有界；
   重采样器状态按会话维护。
3. **矩阵仍是唯一处置权威**：会话中途的配置更新同样受矩阵裁决，转换模块不得
   自行降级或忽略任何字段。跨语义的未知字段/事件可见地失败，不从 `Extensions`
   或 `Raw` 猜测（原则 2.1）。
4. **`MarkHomogeneous` 保持现状**：`openai.realtime → dashscope.ws.realtime`
   标了，反向没标。本 ADR 不改这些 flag，也不把它们当作「负载等价」的授权。

## 原则 2.2 澄清草案（尚未并入 principles.md）

> 快通道的对照义务在 realtime 上按**建模事件子集**执行：关掉快通道跑同一组
> fixture，与转换路径逐项对照。未建模的不透明字段在转换路径一侧没有对应物，
> 不参与对照，但它们只允许出现在同契约路径上，且任何时候都不被声称经过语义
> 验证。

采纳门槛：上文暂存于本 ADR 与 spec；并入 `docs/architecture/principles.md`
需要单独的 PR 与评审，此刻尚未采纳。

## Considered Options

- **通用 realtime IR**：否决。realtime 协议演进比 HTTP 快，全覆盖 IR 要么追着
  版本跑，要么丢字段；#6 已定同协议直通路径 bypass IR，通用 IR 对两条设计分
  1.000 的路径是空转的抽象。注意「omitempty 让 IR 不可能」这个理由不成立，不能拿来
  用：omitempty 是序列化注解的选择，显式 presence 可以用指针与 presence 位做到
  （`openaichat.Projection` 就是现成先例）。否决的真实理由是演进速度与语义
  对齐成本不划算，不是 Go 注解的限制。
- **盲目 JSON 改名表**：否决。「同名事件证明同语义」不成立：DashScope 对齐的
  是 OpenAI beta 命名，而活体探针显示中间代模型收到新式字段后照样回
  `session.updated`，回显的却是 legacy 结构。名字相同、事件相同都可能是假象，
  改名表会把没对齐的语义静默放行。
- **等调研补齐再定方向**：否决。2026-09-06 的两份 realtime 研究笔记都是带日期
  的快照，不是活体证据，GA/beta 命名差异在实现时必须按显式的受支持版本清单
  重新核对。但方向本身（直通 + 最小显式转换）不依赖任何一版事实，现在就能定。

## Consequences

- `dashscope.inference`（run-task 指令流）保持独立，直到拿到语义证据；不因与
  Native SSE 形状相似就共享 IR。
- 五项 realtime 能力的矩阵处置与分数一律不动；本 ADR 不产生新的「已支持」
  声明。
- 四场景验收表、能力映射表与测试证据边界见
  [spec](../superpowers/specs/2026-09-28-realtime-minimal-session-contract.md)。
