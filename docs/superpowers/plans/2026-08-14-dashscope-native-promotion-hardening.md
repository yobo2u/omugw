# DashScope Native 转正收口实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 DashScope Native 的矩阵可用声明与当前文本端点投放范围一致，未投放端点返回协议化 501，并完整保留上游 `request_id`。

**Architecture:** 保留路径处置作为设计事实，在 `Route` 上增加按能力记录的兑现集合，运行时裁决与 `AvailableScore` 只计算已兑现能力。HTTP 层用更宽的 `POST /api/v1/` 子树兜底未投放端点；错误层用专用 `UpstreamRequestID` 完成 DashScope 错误往返。

**Tech Stack:** Go 1.25、`net/http.ServeMux`、现有 `canonical` / `degrade` / `gateway` / `dashscopewire` 包、标准库 `testing`。

---

### Task 1: 保留 DashScope 上游 request ID

**Files:**
- Modify: `internal/canonical/error.go`
- Modify: `internal/protocol/dashscopewire/error.go`
- Test: `internal/protocol/dashscopewire/error_test.go`

- [ ] **Step 1: 写失败的往返测试**

将现有 request ID 测试改为：`DecodeError` 后 `UpstreamRequestID == "abc-123"`、`Param == ""`，再经 `EncodeError` 后 `Envelope.RequestID == "abc-123"`。

- [ ] **Step 2: 验证红灯**

Run: `go test ./internal/protocol/dashscopewire/`

Expected: 编译失败，提示 `UpstreamRequestID` 不存在。

- [ ] **Step 3: 最小实现**

在 `canonical.Error` 中增加：

```go
// UpstreamRequestID 是上游为一次失败分配的追踪 ID；它不是出错参数名。
UpstreamRequestID string
```

`dashscopewire.DecodeError` 写入该字段，`EncodeError` 把它写回 `Envelope.RequestID`，删除将 request ID 放入 `Param` 的逻辑。

- [ ] **Step 4: 验证绿灯**

Run: `go test ./internal/protocol/dashscopewire/ ./internal/canonical/`

Expected: PASS。

- [ ] **Step 5: 提交**

Commit: `canonical.Error 增加 UpstreamRequestID：DashScope 的 request_id 不再借用 Param 往返`

### Task 2: 将路径转正细化为能力兑现集合

**Files:**
- Modify: `internal/degrade/matrix.go`
- Modify: `internal/degrade/preference.go`
- Modify: `internal/degrade/rules_phase1.go`
- Modify: `internal/degrade/markdown.go`
- Modify: `internal/degrade/matrix_test.go`
- Modify: `internal/degrade/preference_test.go`
- Modify: `internal/gateway/build.go`
- Modify: `cmd/omugw/main.go`
- Generated: `docs/degradation-matrix.md`

- [ ] **Step 1: 写能力闸门失败测试**

新增测试证明：Native 的 `vision_input` 设计处置仍是 `PASSTHROUGH`，但当前未兑现，`Matrix.Check` 返回 `ClassNotImplemented` / 501；文本、流式、工具、推理、搜索五项仍通过。再逐条断言三条已转正路径的兑现集合。

- [ ] **Step 2: 验证红灯**

Run: `go test ./internal/degrade/`

Expected: Native `vision_input` 仍被放行，兑现集合 API 尚不存在。

- [ ] **Step 3: 实现兑现集合**

用 `Route.redeemed map[canonical.Capability]bool` 替换布尔字段，增加：

```go
func (r *Route) Redeem(caps ...canonical.Capability) *Route
func (r *Route) Implemented() bool
func (r *Route) Redeems(c canonical.Capability) bool
```

`Route.Build` 拒绝兑现不可表达、`REJECT` 或 `N/A` 的能力；`Derive` 不继承兑现集合；`Check` 对声明有效但未兑现的能力返回 501。OpenAI 两条路径兑现全部可表达能力，Native 仅兑现：

```go
canonical.CapTextGeneration,
canonical.CapStreaming,
canonical.CapToolCalling,
canonical.CapReasoning,
canonical.CapWebSearch,
```

同步所有 `Implemented` 字段和 `MarkImplemented()` 调用点。

- [ ] **Step 4: 写可用分数失败测试**

断言 Native `DesignScore() == 1`、`NotRedeemed == 13`、`AvailableScore() == 5.0/18.0` 且 `Gated()` 为真。

- [ ] **Step 5: 实现可用分数**

`Preservation` 增加 `NotRedeemed` 与 `AvailableWeight`；设计计数不变，可用权重只累加已兑现且开关可用的格子。

- [ ] **Step 6: 同步生成文档**

矩阵 Markdown 明确区分“设计处置”和“当前投放”，并显示 Native 当前可用 `0.278（18 项中 5 项已投放）`。

Run: `make matrix-update`

人工检查生成 diff 后运行：`make check`

Expected: PASS。

- [ ] **Step 7: 提交**

Commit: `降级矩阵区分设计处置与当前投放：未投放的能力返回 501 而非当作可用`

### Task 3: Native 命名空间协议化 501 兜底

**Files:**
- Modify: `internal/protocol/dashscopenative/wire.go`
- Modify: `internal/protocol/dashscopewire/error.go`
- Modify: `internal/protocol/dashscopewire/error_test.go`
- Modify: `internal/gateway/build.go`
- Create: `internal/gateway/build_test.go`

- [ ] **Step 1: 写 Mux 公共 seam 失败测试**

通过 `Built.Mux` 断言 multimodal、embedding、rerank 三个 POST 端点返回 501、`application/json`、扁平 `{code,message}`，且不调用上游；文本端点仍由精确路由处理；health-only 模式不注册该兜底。

- [ ] **Step 2: 验证红灯**

Run: `go test ./internal/gateway/ -run 'Unshipped|Fallback|Shipped'`

Expected: 未投放端点返回默认 404。

- [ ] **Step 3: 最小实现**

在 `dashscopenative` 定义：

```go
const NamespacePrefix = "/api/v1/"
```

在完整配置模式下注册 `POST /api/v1/` 子树 handler。它不鉴权、不读 body、不猜能力、不打上游，只经 `dashscopewire.EncodeError` 返回 `ClassNotImplemented`。为该错误类编码 `code: "Unsupported"`，并记录 not-implemented 指标。

- [ ] **Step 4: 验证绿灯**

Run: `go test ./internal/gateway/ ./internal/protocol/dashscopewire/`

Expected: PASS。

- [ ] **Step 5: 提交**

Commit: `DashScope Native 命名空间兜底：未投放端点返回协议化 501，不再落到纯文本 404`

### Task 4: 加强路径级请求契约证据

**Files:**
- Modify: `internal/gateway/conformance_test.go`
- Create: `testdata/routes/dashscope.native__dashscope.native/tools-and-search.json`
- Create: `testdata/routes/dashscope.native__dashscope.native/golden/tools-and-search.txt`

- [ ] **Step 1: 改写 conformance seam**

从 fixture 的 method/path/headers 驱动入站请求，捕获上游 method/path/header/body。断言上游使用网关凭据、保留 fixture 声明的 SSE 与 Workspace 头，body 除 `model` 改写为 `upstream-model` 外其余顶层字段语义相同。

- [ ] **Step 2: 增加兑现证据 fixture**

新增包含 `tools`、`enable_search`、`enable_thinking` 和 Workspace header 的文本生成用例，说明其用于兑现 tool calling、web search、reasoning。

- [ ] **Step 3: 验证红灯并生成 golden**

Run: `go test ./internal/gateway/ -run DashScopeNativeRouteConformance`

Expected: 新用例因缺少 golden 失败。

Run: `make golden-update`

人工审阅新 golden，再运行同一测试，Expected: PASS。

- [ ] **Step 4: 回归敏感性验证**

临时分别破坏 SSE header、Workspace header、Authorization 覆盖和 path 注入，确认对应断言失败，随后恢复生产代码。

- [ ] **Step 5: 提交**

Commit: `Native 路径级 conformance 断言上游实际收到的请求，并补上工具与搜索的投放证据`

### Task 5: 锁定上游错误与流内终止形状

**Files:**
- Modify: `internal/gateway/gateway_test.go`

- [ ] **Step 1: 增加上游错误测试**

上游返回 `400 {"code":"InvalidParameter","message":"x","request_id":"ups-req-77"}`，断言客户端收到扁平信封并保留 request ID，无嵌套 `error`。

- [ ] **Step 2: 增加流中断测试**

上游先写 Native `event: result`，再超过 idle timeout；断言客户端保留首帧并收到 `event: error`，其 data 为 DashScope 扁平信封，且不切换备用上游。

- [ ] **Step 3: 回归注入验证**

临时把 Native encoder 改回 OpenAI、删除流内 error 写入、或丢弃 request ID，确认相应测试失败，再恢复。

- [ ] **Step 4: 验证与提交**

Run: `go test ./internal/gateway/`

Expected: PASS。

Commit: `补 DashScope Native 上游错误与流内中断的错误形状测试，防止错误编码器注入被回退`

### Task 6: 知识库、完整验证与交付

**Files:**
- Modify: `AGENTS.md`
- Modify: `internal/degrade/AGENTS.md`
- Modify: `internal/gateway/AGENTS.md`

- [ ] **Step 1: 同步知识库**

把 `MarkImplemented` 更新为能力兑现机制，注明 Native 当前只投放文本生成端点与 5/18 能力，并记录 `/api/v1/` 协议化兜底职责。

- [ ] **Step 2: 提交知识库**

Commit: `更新知识库：转正粒度从路径细化到能力，Native 命名空间新增兜底`

- [ ] **Step 3: 完整门禁**

Run:

```bash
make check
make test-race
go build -o bin/omugw ./cmd/omugw
```

Expected: 全部退出 0。

- [ ] **Step 4: LSP 与手工 HTTP QA**

对所有变更 Go 文件运行 LSP diagnostics。启动本地 stub 上游与 `omugw`，验证：文本请求 200；multimodal/embedding POST 为扁平 JSON 501；文本端点 image capability 为 501；`enable_search` 仍为 200；非 POST 与非 Native 命名空间保持 404；启动日志可用分数为约 0.278。

- [ ] **Step 5: 两轴代码审查**

以 `7dd12d1` 为固定点运行标准/规格审查，修复本次引入的阻塞问题后重跑门禁。

- [ ] **Step 6: 最终 Git 审计**

检查 `git status`、`git diff 7dd12d1..HEAD --stat` 与提交历史，确认原子提交、无临时文件、工作树干净。
