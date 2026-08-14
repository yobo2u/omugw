# DashScope Native 多模态端点级兑现实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 `dashscope.native → dashscope.native` 路径的兑现从路径级下沉到端点级，并投放第二扇门——多模态生成门（vision / audio / video 输入，同步 + SSE）。两扇门的可用分各自独立计算（各 5/18 = 0.278），显式拒绝任何路径级并集聚合分（8/18 = 0.444 口径作废）。

**Architecture:** `internal/degrade` 引入 `Endpoint` 与 `Inbound` 两个类型，兑现下沉为 **(端点, 能力)** 二元键；`Check` / `BestOutbound` 按入站坐标裁决，闸门顺序为「路径注册 → 路径通车 → 端点通车 → 逐项能力处置（REJECT / N/A 先于投放）→ 端点投放 → EMULATE 开关」；fixture 门槛按 `request.path` 与门清单双向对账；文档与启动日志只呈现端点级可用分，不存在路径级「当前可用」聚合分。

**Tech Stack:** Go 1.25（仅现有三个直接依赖，不新增），标准库 `testing` + `internal/testkit` 离线回放 + golden，Make 目标（`test` / `matrix` / `matrix-update` / `golden-update` / `check` / `test-race`）。

**Spec:** `docs/superpowers/specs/2026-08-14-dashscope-native-multimodal-endpoint-design.md`（批准提交 `237636a`，下文简称「设计文档」）。本计划是它的决策完整展开；执行中若发现计划与设计文档冲突，以设计文档为准并停下报告，不得自行改设计。

---

## 前置约定（每个任务都适用）

1. **语言与注释**：代码注释与文档一律中文，写「防的是什么」而不是「做了什么」，沿用仓库既有风格（参见 `internal/degrade/matrix.go` 现有注释）。
2. **严格 TDD**：先写失败测试（RED），确认它因正确的原因失败（符号未定义、断言不满足，而不是拼写错误），再写最小实现（GREEN）。命令与预期输出在每个步骤里给出。
3. **每个提交必须绿**：任何提交落地前，`go build ./...` 与 `go test ./...` 必须通过（除非该步骤明确标注 RED 中间态且同一任务内收敛）。任务 3 是唯一一次不可避免的宽幅原子迁移，其余任务按常规纵切。
4. **提交纪律**：每条 `git` 命令以 `GIT_MASTER=1` 前缀执行。每个提交带 Sisyphus 署名脚注与 Co-authored-by trailer，模板：

   ```bash
   GIT_MASTER=1 git add <files>
   GIT_MASTER=1 git commit -m "<任务给出的提交信息>" \
     -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
     -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
   ```

5. **不许推送**：全程不得 `git push`。分支 `main` 已领先 origin 两个文档提交（`b04ff37`、`237636a`），保持本地状态。
6. **golden 纪律**：`make golden-update` 重写 golden 后必须逐份人工审阅 diff，确认内容正是预期语义，再提交。
7. **解码器零改动**：设计文档已确认 `dashscopenative.Decode` 识别四种内容块（纯键式与带 type 两种形态）、按 data URI 统计内联字节、对 video 帧数组逐帧累加，且均有单测固化。本计划任何任务都**不改** `internal/protocol/dashscopenative` 的解码逻辑（只加一个路径常量）。若某个负例测试因解码识别不符而失败，停下报告，不得顺手改解码器。

## 三个已采纳的决策（A1 / A2 / A3）

**A1 — fixture 门槛本地解析 `request.path`。**
`checkRouteFixtures`（`internal/degrade/matrix_test.go`）扩展为端点级双向对账时，用 `encoding/json` 就地解析每个 fixture 文件的 `request.path`，**不**调用 `testkit.LoadDir`。理由：`checkRouteFixtures` 的签名是返回 `error`（调用方按路径归因），手里没有 `testing.T` 可以交给 `LoadDir`；`LoadDir` 出错走 `t.Fatalf`，会把「某条路径证据不全」变成「整个测试中止」，归因信息也丢了。

**A2 — 启动日志按端点记可用分。**
`cmd/omugw/main.go` 的启动日志改为：路径级「已注册转换路径」条目保留设计数据（`design_score` 等）但**不再**携带任何可用分（路径级可用聚合分不存在）；另对每扇已开门打一条「已投放端点」条目，携带该门的 `available_score`。一门一条，有几扇门打几条。

**A3 — 绝不从 `Preservation(avail, Endpoint(""))` 取可用列。**
`Preservation` 的可用列是端点相对的：零值端点永远没有兑现，可用列恒为零——误用它只会**少报**（显示 0.000），不会多报，但仍是错的。因此一切需要可用分的地方（文档主表、端点细分、启动日志、选路对账）必须**先** `Endpoints()` 拿到真实门，再逐门 `Preservation(avail, ep)`。唯一允许传 `Endpoint("")` 的场景是**只取设计列**（设计计数与 DesignScore 是路径级的，与 ep 无关），且必须在注释里写明「只取设计列」。`TestPreferenceMatchesPreservation` 就是这种场景：它只比 DesignScore，传 `Endpoint("")` 合法。

## 关键路径与并行性

任务依赖是线性的：**任务 1 → 2 → 3 → 4 → 5 → 6 → 7**。

- 任务 2 的 DashScope 门常量引用任务 1 的 `MultimodalGenerationPath`；任务 3 的迁移用任务 2 的类型；任务 4 的门槛用任务 3 的 `Endpoints()`；任务 5 的多模态门依赖任务 4 的门槛来锁证据；任务 6 的并集否决断言依赖任务 5 的两门状态；任务 7 的知识库同步依赖前面全部落地后的最终代码形态（行号、门数）。
- **唯一可并行的准备**：任务 5 的三份 fixture JSON 与任务 7 的文档文案不动 Go 代码，可以在任务 3 进行中提前起草，但**不得**在所属任务之前提交。
- 任务 3 是唯一不可避免的宽幅原子迁移：`Redeem` / `Check` / `BestOutbound` / `Preservation` 签名破坏性变更。仓库未发布（M1 进行中），允许破坏性变更；全部调用点必须在同一提交内迁完，否则任何中间提交都编译不过。

## 范围与文件地图

| 文件 | 动作 | 责任 | 任务 |
|---|---|---|---|
| `internal/protocol/dashscopenative/wire.go` | 修改 | 新增 `MultimodalGenerationPath` 常量（线格式事实单一来源留在协议包） | 1 |
| `internal/protocol/dashscopenative/wire_test.go` | 新建 | 钉死路径常量与官方契约一致 | 1 |
| `internal/degrade/endpoint.go` | 新建 | `Endpoint` / `Inbound` 类型 + 四扇门常量 | 2 |
| `internal/degrade/endpoint_test.go` | 新建 | 门常量对账（复用协议包常量，不许复写第二份） | 2 |
| `internal/degrade/matrix.go` | 修改 | `Route.redeemed` 二元键；`Redeem(ep,…)` / `Redeems(ep,c)` / `RedeemedAt(ep)` / `Endpoints()` / `ImplementedAt(ep)` / `Implemented()`；`Check(Inbound,…)` 闸门序；Build 两项新校验 | 3 |
| `internal/degrade/preference.go` | 修改 | `Preservation(avail, ep)`；`BestOutbound(Inbound,…)`（`RankOutbound` 保持端点无感） | 3 |
| `internal/degrade/rules_phase1.go` | 修改 | 三处 `Redeem` 迁到端点形（任务 5 再加多模态门） | 3、5 |
| `internal/degrade/markdown.go` | 修改 | 可用列端点相对（A3）；「见端点细分」分支；`formatAvailable` 抽取；端点细分小节；文案更新 | 3、5 |
| `internal/degrade/markdown_test.go` | 新建 | 端点细分呈现 + 并集分永不出现的锁 | 5 |
| `internal/degrade/matrix_test.go` | 修改 | `implementedMatrix` 迁移；白名单改 `in -> out @ endpoint`；`checkRouteFixtures` 双向对账（A1）；全部 `Check` 调用点 | 3、4、5 |
| `internal/degrade/preference_test.go` | 修改 | 分数对账迁到端点粒度（A3 注记） | 3、5 |
| `internal/degrade/endpoint_gate_test.go` | 新建 | 迁移引入的新语义的 RED 测试（Build 校验、门推导、闸门序） | 3 |
| `internal/degrade/endpoint_mutation_test.go` | 新建 | 变异锁：跨门并集、空能力集、8/18 否决 | 6 |
| `internal/gateway/handler.go` | 修改 | `serve()` 传 `Inbound{Protocol, Endpoint(upstreamPath(r))}` | 3 |
| `internal/gateway/build.go` | 修改 | Mux 注册用命名常量；启动期双向对账；多模态门注册 | 3、5 |
| `internal/gateway/build_test.go` | 修改 | 对账双向咬人测试；多模态用例翻转（501 兜底 → 200 转发） | 3、5 |
| `internal/gateway/gateway_test.go` | 修改 | `newHarnessFor` 增加 `limits` 参数；`newDashScopeNativeHarnessWithLimits` | 5 |
| `internal/gateway/multimodal_test.go` | 新建 | 测试矩阵负例 7 / 8 / 9 / 11（上游零调用断言） | 5 |
| `cmd/omugw/main.go` | 修改 | A2：路径日志去可用分，每门一条可用分日志 | 3 |
| `testdata/routes/dashscope.native__dashscope.native/multimodal-basic.json` | 新建 | 同步，三种媒体 + 两种承载 + 纯键式块 | 5 |
| `testdata/routes/dashscope.native__dashscope.native/multimodal-streaming.json` | 新建 | SSE，带 type 块，SSE 头转发证据 | 5 |
| `testdata/routes/dashscope.native__dashscope.native/multimodal-video-frames.json` | 新建 | video 帧数组逐帧内联累加 | 5 |
| `testdata/routes/dashscope.native__dashscope.native/golden/multimodal-*.txt` | 生成 | 三份 golden（`make golden-update` 后人工审阅） | 5 |
| `docs/degradation-matrix.md` | 生成 | `make matrix-update` 重新生成（禁手改） | 5 |
| `README.md`、根 `AGENTS.md`、`internal/{degrade,gateway,protocol}/AGENTS.md` | 修改 | 知识库同步（状态、术语、行号、反模式） | 7 |

---

## 任务 1：多模态端点路径常量

**依赖**：无。
**文件**：修改 `internal/protocol/dashscopenative/wire.go`；新建 `internal/protocol/dashscopenative/wire_test.go`。

- [ ] **步骤 1.1：写失败测试（RED）**

新建 `internal/protocol/dashscopenative/wire_test.go`：

```go
package dashscopenative

import "testing"

// TestMultimodalGenerationPathMatchesOfficialContract 钉死多模态生成的端点路径。
//
// 这个字符串是线格式事实，出自 docs/architecture/dashscope-endpoints-research.md。
// 用测试钉住，防的是端点门悄悄漂到另一个路径上——门敲错了，后面所有裁决全错。
func TestMultimodalGenerationPathMatchesOfficialContract(t *testing.T) {
	const want = "/api/v1/services/aigc/multimodal-generation/generation"
	if MultimodalGenerationPath != want {
		t.Errorf("MultimodalGenerationPath = %q，期望 %q", MultimodalGenerationPath, want)
	}
}
```

运行：

```bash
go test ./internal/protocol/dashscopenative/ -run TestMultimodalGenerationPathMatchesOfficialContract -v
```

预期：**编译失败**，`undefined: MultimodalGenerationPath`——这是正确的 RED（符号尚不存在）。

- [ ] **步骤 1.2：加常量（GREEN）**

在 `internal/protocol/dashscopenative/wire.go` 的 `TextGenerationPath` 常量之后追加：

```go
// MultimodalGenerationPath 是多模态生成的上游端点路径。
// 与 TextGenerationPath 共用 Qwen 模型 API 的请求信封（model / input.messages /
// parameters），内容块为 text / image / audio / video 四种单键形态，没有通用 file 块。
const MultimodalGenerationPath = "/api/v1/services/aigc/multimodal-generation/generation"
```

运行：

```bash
go test ./internal/protocol/dashscopenative/ -run TestMultimodalGenerationPathMatchesOfficialContract -v
```

预期：PASS。

- [ ] **步骤 1.3：全量验证**

```bash
go build ./... && go test ./internal/protocol/dashscopenative/ && gofmt -l internal/protocol/dashscopenative/
```

预期：构建通过、测试通过、`gofmt -l` 无输出。

- [ ] **步骤 1.4：提交**

```bash
GIT_MASTER=1 git add internal/protocol/dashscopenative/wire.go internal/protocol/dashscopenative/wire_test.go
GIT_MASTER=1 git commit -m "新增 DashScope Native 多模态生成端点路径常量" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 2：Endpoint 与 Inbound 类型、四扇门常量

**依赖**：任务 1。
**文件**：新建 `internal/degrade/endpoint.go`、`internal/degrade/endpoint_test.go`。

依赖方向说明：`internal/degrade` 引入对 `internal/protocol/dashscopenative` 的依赖，仅为两个路径常量。方向合法（`dashscopenative` 只依赖 `canonical` 与 `encoding/json`，无环），且是刻意选择：端点路径是线格式事实，矩阵引用而不复写。

- [ ] **步骤 2.1：写失败测试（RED）**

新建 `internal/degrade/endpoint_test.go`：

```go
package degrade

import (
	"testing"

	"github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

// TestDashScopeEndpointsReuseWirePaths 钉死 DashScope 两扇门就是协议包的路径常量本身。
//
// 线格式事实的单一来源在协议包；矩阵若复写第二份字符串，两处就可能漂移——
// 门常量漂了，Mux 注册与裁决各敲各的门。
func TestDashScopeEndpointsReuseWirePaths(t *testing.T) {
	if string(EndpointDashScopeTextGeneration) != dashscopenative.TextGenerationPath {
		t.Errorf("文本门 = %q，应等于 dashscopenative.TextGenerationPath %q",
			EndpointDashScopeTextGeneration, dashscopenative.TextGenerationPath)
	}
	if string(EndpointDashScopeMultimodal) != dashscopenative.MultimodalGenerationPath {
		t.Errorf("多模态门 = %q，应等于 dashscopenative.MultimodalGenerationPath %q",
			EndpointDashScopeMultimodal, dashscopenative.MultimodalGenerationPath)
	}
}

// TestOpenAIEndpointConstants 钉死 OpenAI 两扇门的字面值。
//
// 这两个值原先散落在 gateway/build.go 的 Mux 注册里；提成常量后，
// 登记（rules）与注册（Mux）必须引用同一份事实。
func TestOpenAIEndpointConstants(t *testing.T) {
	if EndpointOpenAIChat != "/v1/chat/completions" {
		t.Errorf("EndpointOpenAIChat = %q", EndpointOpenAIChat)
	}
	if EndpointOpenAIResponses != "/v1/responses" {
		t.Errorf("EndpointOpenAIResponses = %q", EndpointOpenAIResponses)
	}
}
```

运行：

```bash
go test ./internal/degrade/ -run 'TestDashScopeEndpointsReuseWirePaths|TestOpenAIEndpointConstants' -v
```

预期：**编译失败**，`undefined: EndpointDashScopeTextGeneration` 等——正确的 RED。

- [ ] **步骤 2.2：实现类型与常量（GREEN）**

新建 `internal/degrade/endpoint.go`：

```go
package degrade

import "github.com/yobo2u/omugw/internal/protocol/dashscopenative"

// Endpoint 是入站协议族下的一扇门：客户端能直接敲响的上游端点路径。
// 同源直通下，入站门与出站端点是同一个路径。
//
// 零值没有任何含义：不是通配，不是默认门。它在任何处出现都按「没有这扇门」
// 对待——Redeem 它会在 Build 失败，Check 它会在端点闸门得 501，
// Preservation 它的可用列恒为零。
type Endpoint string

// Inbound 钉住一次请求的入站坐标：从哪个协议进来，敲的哪扇门。
// 矩阵按这对坐标裁决。
type Inbound struct {
	Protocol Protocol
	Endpoint Endpoint
}

const (
	// OpenAI 两扇门从 build.go 的字面量迁到命名常量，
	// 登记（rules）与注册（Mux）引用同一份事实。
	EndpointOpenAIChat      Endpoint = "/v1/chat/completions"
	EndpointOpenAIResponses Endpoint = "/v1/responses"
)

const (
	// DashScope 两扇门复用协议包的路径常量，线格式事实的单一来源留在协议包，
	// 矩阵引用而不复写。
	EndpointDashScopeTextGeneration Endpoint = Endpoint(dashscopenative.TextGenerationPath)
	EndpointDashScopeMultimodal     Endpoint = Endpoint(dashscopenative.MultimodalGenerationPath)
)
```

运行：

```bash
go test ./internal/degrade/ -run 'TestDashScopeEndpointsReuseWirePaths|TestOpenAIEndpointConstants' -v
```

预期：PASS。

- [ ] **步骤 2.3：全量验证**

```bash
go build ./... && go test ./internal/degrade/ && gofmt -l internal/degrade/
```

预期：全绿（新文件尚未被任何生产代码引用，其余测试不受影响）。

- [ ] **步骤 2.4：提交**

```bash
GIT_MASTER=1 git add internal/degrade/endpoint.go internal/degrade/endpoint_test.go
GIT_MASTER=1 git commit -m "降级矩阵引入 Endpoint 与 Inbound 类型，钉死四扇门常量" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 3：兑现下沉到端点粒度——原子破坏性迁移（行为保持）

**依赖**：任务 2。
**这是全计划唯一一次宽幅原子迁移**：`Redeem` / `Redeems` / `Check` / `BestOutbound` / `Preservation` 签名破坏性变更，全部调用点（生产 + 测试）在同一提交内迁完。仓库未发布，允许破坏性变更（设计文档「迁移兼容」）。

**行为保持命题**（本任务结束时必须仍然成立）：

- 三扇已开门不变：OpenAI 两扇门各兑现全部可表达能力；Native 文本门兑现既有 5 项。
- 11 条 PLANNED 路径无门，`Endpoints()` 为空，`Implemented()` 为 false。
- 运行时可见行为零变化：既有 gateway / conformance / build 测试全部原样通过；`docs/degradation-matrix.md` 逐字节不变（`make matrix` 绿，无需重新生成）。

**文件**（全部在同一提交）：修改 `internal/degrade/matrix.go`、`internal/degrade/preference.go`、`internal/degrade/rules_phase1.go`、`internal/degrade/markdown.go`、`internal/degrade/matrix_test.go`、`internal/degrade/preference_test.go`、`internal/gateway/handler.go`、`internal/gateway/build.go`、`internal/gateway/build_test.go`、`cmd/omugw/main.go`；新建 `internal/degrade/endpoint_gate_test.go`。

### 3A. 先写新语义的 RED 测试

- [ ] **步骤 3.1：写端点粒度新语义测试（RED）**

新建 `internal/degrade/endpoint_gate_test.go`：

```go
package degrade

import (
	"errors"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// TestRedeemZeroEndpointFailsBuild 防的是零值端点退化成路径通配。
//
// 零值一旦被当成「整条路径」，端点粒度就悄悄退回了路径粒度——
// 这正是本次下沉要消灭的毛病，必须在 Build 就拦死。
func TestRedeemZeroEndpointFailsBuild(t *testing.T) {
	_, err := NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
		Pass(ExpressibleSet(ProtoOpenAIChat)...).
		Redeem(Endpoint(""), canonical.CapTextGeneration).
		Build()
	if err == nil {
		t.Fatal("对零值端点 Redeem 应当 Build 失败")
	}
	if !strings.Contains(err.Error(), "redeem on zero endpoint") {
		t.Errorf("错误信息应说明是零值端点问题，实际为: %v", err)
	}
}

// TestRedeemUndeliverableAtEndpointFailsBuild 是既有路径级检查在端点粒度的延续。
//
// 兑现一格按设计就该失败（REJECT）或客户端根本发不出来（N/A）的能力，
// 是在为不存在的东西背书——让它在 Build 失败，好过在文档里显示成「已投放」。
func TestRedeemUndeliverableAtEndpointFailsBuild(t *testing.T) {
	var others []canonical.Capability
	for _, c := range ExpressibleSet(ProtoOpenAIChat) {
		if c != canonical.CapAudioInput {
			others = append(others, c)
		}
	}

	// REJECT 格子。
	_, err := NewRoute(ProtoOpenAIChat, ProviderAnthropicMessages).
		Pass(others...).
		Reject(noteNoAudioIn, canonical.CapAudioInput).
		Redeem(EndpointOpenAIChat, canonical.CapAudioInput).
		Build()
	if err == nil {
		t.Fatal("兑现一个 REJECT 格子应当 Build 失败")
	}
	if !strings.Contains(err.Error(), string(canonical.CapAudioInput)) {
		t.Errorf("错误信息应指出是哪项能力，实际为: %v", err)
	}
	if !strings.Contains(err.Error(), string(EndpointOpenAIChat)) {
		t.Errorf("错误信息应指出是哪扇门，实际为: %v", err)
	}
	if !strings.Contains(err.Error(), "redeemed but not deliverable") {
		t.Errorf("错误信息应说明不可交付，实际为: %v", err)
	}

	// N/A 格子：openai.chat 表达不出 rerank。
	_, err = NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
		Pass(ExpressibleSet(ProtoOpenAIChat)...).
		Redeem(EndpointOpenAIChat, canonical.CapRerank).
		Build()
	if err == nil {
		t.Fatal("兑现一个不可表达的能力应当 Build 失败")
	}
}

// TestEndpointsDerivedFromRedemption 固化「门的存在只从兑现格子推导」。
//
// 没有独立的门注册机制，也没有「空门」可以声明：
// 有一格兑现才算开了这扇门，追加兑现不产生重复条目，零兑现路径没有门。
func TestEndpointsDerivedFromRedemption(t *testing.T) {
	r, err := NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoDashScopeNative)...).
		Redeem(EndpointDashScopeTextGeneration, canonical.CapTextGeneration).
		Build()
	if err != nil {
		t.Fatal(err)
	}

	if eps := r.Endpoints(); len(eps) != 1 || eps[0] != EndpointDashScopeTextGeneration {
		t.Fatalf("应恰有一扇文本门，实际 %v", eps)
	}
	if !r.ImplementedAt(EndpointDashScopeTextGeneration) {
		t.Error("文本门应已开")
	}
	if r.ImplementedAt(Endpoint("/api/v1/other")) {
		t.Error("未兑现的端点不应算开门")
	}
	if !r.Implemented() {
		t.Error("至少一扇门已开，路径应算通车")
	}

	// 同一门追加兑现不产生重复条目。
	r.Redeem(EndpointDashScopeTextGeneration, canonical.CapStreaming)
	if eps := r.Endpoints(); len(eps) != 1 {
		t.Fatalf("同一门追加兑现不应产生重复条目: %v", eps)
	}

	// 开第二扇门，字典序排列（multimodal 在 text 之前）。
	r.Redeem(EndpointDashScopeMultimodal, canonical.CapTextGeneration)
	if eps := r.Endpoints(); len(eps) != 2 ||
		eps[0] != EndpointDashScopeMultimodal || eps[1] != EndpointDashScopeTextGeneration {
		t.Fatalf("门应按字典序排列: %v", eps)
	}

	// 零兑现路径没有门。
	empty := NewRoute(ProtoOpenAIRealtime, ProviderOpenAIRealtime)
	if eps := empty.Endpoints(); len(eps) != 0 {
		t.Fatalf("零兑现路径不应有门: %v", eps)
	}
	if empty.Implemented() {
		t.Error("零兑现路径未通车")
	}
}

// TestCheckRejectsUnopenedEndpoint 固化端点闸门（闸门 3）。
//
// 敲没开的门，即使带着别处已兑现的能力，也必须 501，且消息点名端点——
// 入口约束与能力裁决是两件事。
func TestCheckRejectsUnopenedEndpoint(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.Check(Inbound{
		Protocol: ProtoDashScopeNative,
		Endpoint: Endpoint("/api/v1/services/aigc/not-opened"),
	}, ProviderDashScopeNative, []canonical.Capability{canonical.CapTextGeneration})
	if err == nil {
		t.Fatal("未开门必须 501，即使携带已兑现的能力")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassNotImplemented || cerr.HTTPStatus() != 501 {
		t.Errorf("应为 not_implemented/501，实际 %q/%d", cerr.Class, cerr.HTTPStatus())
	}
	if !strings.Contains(cerr.Message, "/api/v1/services/aigc/not-opened") {
		t.Errorf("错误消息应点名端点: %s", cerr.Message)
	}
}

// TestCheckDispositionBeforeRedemption 固化闸门顺序：REJECT 先于投放（4b 先于 4d）。
//
// 先问投放会把「这条路不支持」说成「还没建好」，方向就错了：
// 501 说「等」，422 说「改」，前者会变，后者不会。
func TestCheckDispositionBeforeRedemption(t *testing.T) {
	m := NewMatrix()
	var others []canonical.Capability
	for _, c := range ExpressibleSet(ProtoOpenAIChat) {
		if c != canonical.CapAudioInput {
			others = append(others, c)
		}
	}
	if err := m.Add(NewRoute(ProtoOpenAIChat, ProviderAnthropicMessages).
		Pass(others...).
		Reject(noteNoAudioIn, canonical.CapAudioInput).
		Redeem(EndpointOpenAIChat, canonical.CapTextGeneration). // 门开了，但 audio_input 未兑现
		Build()); err != nil {
		t.Fatal(err)
	}

	_, err := m.Check(Inbound{Protocol: ProtoOpenAIChat, Endpoint: EndpointOpenAIChat},
		ProviderAnthropicMessages,
		[]canonical.Capability{canonical.CapTextGeneration, canonical.CapAudioInput})
	if err == nil {
		t.Fatal("REJECT 能力必须失败")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassUnsupported || cerr.HTTPStatus() != 422 {
		t.Errorf("REJECT 应先于投放作答，返回 unsupported/422，实际 %q/%d", cerr.Class, cerr.HTTPStatus())
	}
}

// TestPreservationAvailableIsEndpointRelative 固化可用列端点相对、设计列路径级。
func TestPreservationAvailableIsEndpointRelative(t *testing.T) {
	r, err := NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoDashScopeNative)...).
		Redeem(EndpointDashScopeTextGeneration, canonical.CapTextGeneration, canonical.CapStreaming).
		Redeem(EndpointDashScopeMultimodal, canonical.CapTextGeneration).
		Build()
	if err != nil {
		t.Fatal(err)
	}

	avail := DefaultAvailability()

	if got := r.Preservation(avail, EndpointDashScopeTextGeneration).AvailableScore(); got != 2.0/18.0 {
		t.Errorf("文本门可用分 = %.3f，期望 2/18", got)
	}
	if got := r.Preservation(avail, EndpointDashScopeMultimodal).AvailableScore(); got != 1.0/18.0 {
		t.Errorf("多模态门可用分 = %.3f，期望 1/18", got)
	}

	// 设计列是路径级的，与敲哪扇门无关。
	if got := r.Preservation(avail, EndpointDashScopeTextGeneration).DesignScore(); got != 1.0 {
		t.Errorf("设计分应为 1.000，实际 %.3f", got)
	}

	// 未开门（含零值）：可用列恒为零，设计列不变——只会少报，不会多报。
	zero := r.Preservation(avail, Endpoint(""))
	if zero.AvailableScore() != 0 {
		t.Errorf("零值端点可用列应恒为零，实际 %.3f", zero.AvailableScore())
	}
	if zero.DesignScore() != 1.0 {
		t.Errorf("零值端点设计列应不受影响，实际 %.3f", zero.DesignScore())
	}
}
```

运行：

```bash
go test ./internal/degrade/ -run 'TestRedeemZeroEndpointFailsBuild|TestRedeemUndeliverableAtEndpointFailsBuild|TestEndpointsDerivedFromRedemption|TestCheckRejectsUnopenedEndpoint|TestCheckDispositionBeforeRedemption|TestPreservationAvailableIsEndpointRelative' -v
```

预期：**编译失败**（`Redeem` 签名不符、`Check` 不收 `Inbound` 等）——正确的 RED。本任务余下步骤让这些测试变绿。

### 3B. 重写 internal/degrade 核心

- [ ] **步骤 3.2：`matrix.go` — Route 的兑现下沉与 Check 闸门**

对 `internal/degrade/matrix.go` 做如下精确变换：

(a) `Route` 结构体的 `redeemed` 字段与其注释整体替换为：

```go
	// redeemed 是这条路径**当前真的投放了**的能力，键为 (端点, 能力) 二元组。
	//
	// 与 rules 的分工是整个包最容易混淆、也最要紧的一处：rules 是**设计处置**
	// ——这条路最终该怎么对待每项能力；redeemed 是**当前投放**——这扇门此刻
	// 真的有什么能力。
	//
	// 早先是路径级集合，DashScope Native 投放第二扇门时撞破了它：一个协议对应
	// 文本生成、多模态、embedding、rerank 多个端点，把新门的能力加进同一个集合，
	// 文本门的 tool_calling 就会混进多模态门的可用分数，仿佛那扇门也提供这些能力。
	// 门与门的兑现集合互不相通，不做并集。
	redeemed map[Endpoint]map[canonical.Capability]bool
```

(b) `NewRoute` 初始化改为 `redeemed: map[Endpoint]map[canonical.Capability]bool{}`。

(c) `Redeem` / `Implemented` / `Redeems` 三个方法整体替换为（`RedeemedAt` / `Endpoints` / `ImplementedAt` 为新增）：

```go
// Redeem 登记指定端点已投放的能力。
//
// 只应在该端点的端到端 fixture 已经存在并通过之后调用。代码守的闸门有三道：
// checkRouteFixtures 按 request.path 与门清单双向对账（每扇门要有证据，
// 每个证据要指向已开门），TestRedeemedCapabilitiesAreExplicit 要求
// 「路径 @ 端点」的兑现集合逐项写进白名单。闸门都不会把某一项 PASSTHROUGH
// 能力自动对上 fixture——「这项能力真的跑通了」是改白名单那个人担的责。
//
// ep 为零值，或兑现到 REJECT / N/A 格子，都会在 Build 失败。
func (r *Route) Redeem(ep Endpoint, caps ...canonical.Capability) *Route {
	if r.redeemed[ep] == nil {
		r.redeemed[ep] = map[canonical.Capability]bool{}
	}
	for _, c := range caps {
		r.redeemed[ep][c] = true
	}
	return r
}

// Redeems 报告某项能力是否已投放到指定端点。
func (r *Route) Redeems(ep Endpoint, c canonical.Capability) bool {
	return r.redeemed[ep][c]
}

// RedeemedAt 返回指定端点已投放的能力集合，按 AllCapabilities 顺序，
// 供白名单对账与文档生成。
func (r *Route) RedeemedAt(ep Endpoint) []canonical.Capability {
	set := r.redeemed[ep]
	out := []canonical.Capability{}
	for _, c := range canonical.AllCapabilities() {
		if set[c] {
			out = append(out, c)
		}
	}
	return out
}

// Endpoints 返回全部已开门，字典序。
//
// 一门当且仅当有至少一格兑现才算开；存在是推导出来的，不是声明出来的。
// 没有独立的门注册机制——未投放的门本来就是 PLANNED，不需要占位。
func (r *Route) Endpoints() []Endpoint {
	out := []Endpoint{}
	for ep, caps := range r.redeemed {
		if len(caps) > 0 {
			out = append(out, ep)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ImplementedAt 报告指定端点是否至少投放了一项能力。
func (r *Route) ImplementedAt(ep Endpoint) bool { return len(r.redeemed[ep]) > 0 }

// Implemented 报告这条路径是否至少一扇门已开，即它是否已经通车。
//
// 选路与文档用它区分「已实现」和「规划中」；单扇门能不能走，要另问 ImplementedAt；
// 单项能力能不能走，要另问 Redeems。
func (r *Route) Implemented() bool { return len(r.Endpoints()) > 0 }
```

(d) `Derive` 无需改动（`NewRoute` 从空兑现开始，二元键集合整体不带入派生路径）；其注释中「兑现集合不在继承之列」一段保留，语义与新结构一致。

(e) `Build` 中既有的路径级 undeliverable 检查块（`var undeliverable []string` 起）整体替换为端点粒度 + 零值端点两项校验：

```go
	// 零值端点 Redeem：零值一旦被当成「整条路径」的通配，
	// 端点粒度就悄悄退回了路径粒度。
	for ep := range r.redeemed {
		if ep == Endpoint("") {
			r.errs = append(r.errs, "redeem on zero endpoint")
		}
	}

	// 兑现一个 REJECT 或 N/A 的格子没有内容可言——前者按设计就该失败，
	// 后者客户端连发都发不出来。让它在 Build 失败，好过在文档里显示成「已投放」，
	// 那是在为一个不存在的东西背书。这是既有路径级检查在端点粒度上的延续。
	for ep, caps := range r.redeemed {
		for c := range caps {
			rule, ok := r.rules[c]
			if !ok || rule.Disposition == Reject || rule.Disposition == NotApplicable {
				r.errs = append(r.errs,
					fmt.Sprintf("endpoint %q redeemed but not deliverable: %s", ep, c))
			}
		}
	}
```

(f) `Check` 整体替换为（签名收 `Inbound`，闸门顺序按设计文档的表）：

```go
// Check 用入站坐标（协议 + 端点）与请求实际用到的能力集裁决。
//
// 闸门顺序：路径注册 → 路径通车 → 端点通车 → 逐项能力处置
//（REJECT / N/A 先于投放）→ 端点投放 → EMULATE 开关。
//
// 任何一项判定为 Reject，或路径/格子未注册，都返回错误——**绝不静默放行**。
// 未注册按 Reject 处理是刻意的失败方向：漏配一格的后果是「这个请求被拒绝」，
// 而不是「这个请求丢了半数字段还返回了 200」。
func (m *Matrix) Check(in Inbound, out Provider, caps []canonical.Capability) (Verdict, error) {
	r, ok := m.Route(in.Protocol, out)
	if !ok {
		return Verdict{}, canonical.Newf(canonical.ClassUnsupported,
			"未注册的转换路径 %s -> %s", in.Protocol, out)
	}

	// 闸门 2：已设计但一门未开的路径必须明确报错。放行会让请求走进一个空壳，
	// 客户端拿到的是一个语焉不详的 5xx，而真相是「这条路还没建」。
	if !r.Implemented() {
		return Verdict{}, canonical.Newf(canonical.ClassNotImplemented,
			"转换路径 %s -> %s 已在降级矩阵中设计，但实现尚未落地", in.Protocol, out)
	}

	// 闸门 3：门必须开着。空能力集不豁免——请求带了什么能力是一回事，
	// 敲的门开没开是另一回事；后者是入口约束。
	if !r.ImplementedAt(in.Endpoint) {
		return Verdict{}, canonical.Newf(canonical.ClassNotImplemented,
			"转换路径 %s -> %s 尚未投放端点 %s", in.Protocol, out, in.Endpoint)
	}

	var v Verdict
	for _, c := range caps {
		rule, ok := r.rules[c]
		if !ok {
			return Verdict{}, canonical.Newf(canonical.ClassUnsupported,
				"转换路径 %s -> %s 未对能力 %q 作出声明", in.Protocol, out, c)
		}
		switch rule.Disposition {
		case Reject:
			return Verdict{}, canonical.Newf(canonical.ClassUnsupported,
				"转换路径 %s -> %s 不支持 %q：%s", in.Protocol, out, c, rule.Note)
		case NotApplicable:
			// 入站协议表达不出这项能力，却在请求里出现了——说明入站解码器
			// 把某个字段解成了它不该解成的东西。这是网关自己的 bug，
			// 不能当成客户端的问题放行。
			return Verdict{}, canonical.Newf(canonical.ClassInternal,
				"入站协议 %s 不应产生能力 %q，解码器可能有误：%s", in.Protocol, c, rule.Note)
		}

		// 闸门 4d：走到这里的处置在设计上都是有效的，于是才轮到问
		// 「这扇门投放了没有」。顺序不能反：REJECT 与 N/A 有各自更确切的
		// 错误分类，先问投放会把「这条路不支持」说成「这条路还没建」。
		//
		// 501 而不是 422：未投放的能力该等实现，不该让客户端改请求。
		if !r.Redeems(in.Endpoint, c) {
			return Verdict{}, canonical.Newf(canonical.ClassNotImplemented,
				"转换路径 %s -> %s 的能力 %q 尚未在端点 %s 上投放",
				in.Protocol, out, c, in.Endpoint)
		}

		switch rule.Disposition {
		case Degrade:
			v.Degraded = append(v.Degraded, CapabilityNote{Capability: c, Note: rule.Note})
		case Emulate:
			// 模拟能力可以被运维关掉。关掉时它的行为与 Reject 一致——
			// 但错误消息必须说清是「开关没开」而不是「这条路不支持」，
			// 否则运维会去查一个根本没问题的转换路径。
			if !m.avail.Enabled(rule.RequiresFeature) {
				return Verdict{}, canonical.Newf(canonical.ClassUnsupported,
					"能力 %q 在路径 %s -> %s 上由网关模拟提供，但功能开关 %q 未开启：%s",
					c, in.Protocol, out, rule.RequiresFeature, rule.Note)
			}
			v.Emulated = append(v.Emulated, CapabilityNote{Capability: c, Note: rule.Note})
		}
	}
	return v, nil
}
```

- [ ] **步骤 3.3：`preference.go` — Preservation 端点参数与 BestOutbound 入站坐标**

(a) `Preservation` 方法签名与注释替换为（方法体循环与原实现逐行相同，唯一变化是 `r.Redeems(c)` → `r.Redeems(ep, c)` 与签名）：

```go
// Preservation 报告这条路径保留了多少原生能力，按矩阵当前的可用性配置计算。
//
// 设计计数与可用权重分两路累加：前者只看处置声明，是路径级的，与 ep 无关；
// 后者还要问这项能力投放到 ep 了没有——可用列永远端点相对。
// ep 未开（含零值）时可用列为零：没有门就是没有可用，只会少报，不会多报。
func (r *Route) Preservation(avail Availability, ep Endpoint) Preservation {
	var p Preservation
	for c, rule := range r.rules {
		var weight float64
		switch rule.Disposition {
		case Passthrough:
			p.Passthrough++
			weight = 1
		case Emulate:
			if avail.Enabled(rule.RequiresFeature) {
				p.Emulate++
				weight = 1
			} else {
				p.EmulateOff++
			}
		case Degrade:
			p.Degrade++
			weight = 0.5
		case Reject:
			p.Reject++
		case NotApplicable:
			p.NotApplicable++
			// N/A 不进分母，也就谈不上投放，跳过下面的计数。
			continue
		}
		if r.Redeems(ep, c) {
			p.availableWeight += weight
		} else if rule.Disposition != Reject {
			// REJECT 的格子按设计就该失败，没有「等它投放」一说，
			// 算进未投放数只会让人以为它将来会变得可用。
			p.NotRedeemed++
		}
	}
	return p
}
```

(b) `BestOutbound` 签名改为收 `Inbound`；`RankOutbound` / `RankDesign` / `rank` **保持端点无感**（候选筛选只看路径是否通车，不看本次请求敲哪扇门）：

```go
// BestOutbound 在候选中挑出既有注册路径、又能承载全部所需能力的最优 Provider。
//
// 候选筛选保持端点无感：只看路径是否通车（至少一门已开），不看本次请求敲哪扇门。
// 端点裁决发生在每个候选内部的 Check：首选若恰因这扇门没开而失败，
// 错误会点名缺哪扇门，而后面开了这扇门的候选仍能接住请求。
//
// 全部候选都跑不通时返回最后一次的错误，让调用方能告诉用户到底缺什么，
// 而不是一句笼统的「无可用 Provider」。
func (m *Matrix) BestOutbound(in Inbound, candidates []Provider, caps []canonical.Capability) (Provider, Verdict, error) {
	ranked := m.RankOutbound(in.Protocol, candidates)
	if len(ranked) == 0 {
		// 区分「没注册」与「注册了但还没实现」：前者要改配置，后者只要等。
		for _, c := range candidates {
			if _, ok := m.Route(in.Protocol, c); ok {
				return "", Verdict{}, canonical.Newf(canonical.ClassNotImplemented,
					"入站协议 %s 的候选出站路径均已设计但尚未实现", in.Protocol)
			}
		}
		return "", Verdict{}, canonical.Newf(canonical.ClassUnsupported,
			"入站协议 %s 没有任何已注册的出站路径", in.Protocol)
	}

	var lastErr error
	for _, p := range ranked {
		v, err := m.Check(in, p, caps)
		if err == nil {
			return p, v, nil
		}
		lastErr = err
	}
	return "", Verdict{}, lastErr
}
```

- [ ] **步骤 3.4：`rules_phase1.go` — 三处 Redeem 迁到端点形**

(a) `chatToOpenAI` 的 `Redeem(ExpressibleSet(ProtoOpenAIChat)...)` 改为：

```go
		Redeem(EndpointOpenAIChat, ExpressibleSet(ProtoOpenAIChat)...)
```

(b) Responses 同源直通的 `.Redeem(ExpressibleSet(ProtoOpenAIResponses)...)` 改为：

```go
	).Redeem(EndpointOpenAIResponses, ExpressibleSet(ProtoOpenAIResponses)...).Build()); err != nil {
```

(c) Native 路径的 `Redeem` 改为（本任务结束时「本期只投放文本门」的注释叙述仍属实，保留）：

```go
	if err := m.Add(NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoDashScopeNative)...).
		// 文本门：既有 5 项，原样保留。reasoning 有 fixture 证据
		//（tools-and-search.json 带 enable_thinking: true），继续持有。
		Redeem(EndpointDashScopeTextGeneration,
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapReasoning,
			canonical.CapWebSearch,
		).
		Build()); err != nil {
		return nil, err
	}
```

- [ ] **步骤 3.5：`markdown.go` — 可用列端点相对（A3）**

(a) `writePreservation` 的路径循环中，`p := r.Preservation(m.avail)` 起至 `available` 计算结束的块替换为：

```go
			// 设计列是路径级的，与敲哪扇门无关；这里只取设计列。
			// 可用列绝不从这条调用取——零值端点的可用列恒为零（见端点分支）。
			p := r.Preservation(m.avail, Endpoint(""))

			status := "规划中"
			available := "—"
			if r.Implemented() {
				status = "已实现"
				e := r.Endpoints()
				if len(e) == 1 {
					// 单门路径：可用列就是那扇门的端点相对分数，显示方式不变。
					available = formatAvailable(r.Preservation(m.avail, e[0]))
				} else {
					// 多门路径：并集分数不对应任何一扇真实的门，
					// 主表只指路，分数逐门列在端点细分小节。
					available = "见端点细分"
				}
			}
```

（循环内其余部分——`fast`、`emulate` 列、`fmt.Fprintf` 行——不变。）

(b) 新增 `formatAvailable`，把原先内联的括号逻辑原样搬进去（语义零变化）：

```go
// formatAvailable 渲染「当前可用」列：分数本身，外加把前提写进数字本身的括号。
//
// 默认配置是绝大多数人的实际部署，按它计分才诚实；括号让读者查到那个数是怎么来的。
// 两种前提要分开写：「开关没开」运维改配置就能解决，「端点还没投放」只能等实现——
// 混成一句，会让人去开一个根本解决不了问题的开关。
func formatAvailable(p Preservation) string {
	available := fmt.Sprintf("%.3f", p.AvailableScore())
	switch {
	case p.NotRedeemed > 0:
		available += fmt.Sprintf("（%d 项中 %d 项已投放）",
			p.denominator()-p.Reject, p.Redeemed())
	case p.EmulateOff > 0:
		available += fmt.Sprintf("（开启 %s 后 %.3f）",
			FeatureConversationStore, p.DesignScore())
	}
	return available
}
```

本任务结束时所有已实现路径都是单门，文档输出与迁移前逐字节一致。端点细分小节在任务 5 随第二扇门一起落地（届时多门分支才可达）。

- [ ] **步骤 3.6：`matrix_test.go` — 测试助手与白名单迁移**

(a) `implementedMatrix` 整体替换为（测试专用门，意图不变），并新增两个助手：

```go
// implementedMatrix 返回一份全部路径都标记为已实现的 Phase1 矩阵。
//
// 用于测试 Check 的能力裁决语义——那部分逻辑与「路径实现了没有」正交，
// 不该因为投放进度就测不了。每条路径挂一扇测试专用门并在其上兑现全部
// 可表达能力，使「能力裁决语义与投放进度正交」的既有测试意图原样保留。
// PLANNED 本身的行为由 TestPlannedRouteIsRejectedAtRuntime 单独覆盖。
func implementedMatrix(t *testing.T, avail Availability) *Matrix {
	t.Helper()
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range m.Routes() {
		r.Redeem(testEndpoint(r.In), ExpressibleSet(r.In)...)
	}
	if avail != nil {
		m.WithAvailability(avail)
	}
	return m
}

// testEndpoint 是 implementedMatrix 的测试专用门，与真实门的路径刻意不同——
// 测试敲的必须是这扇门，而不是碰巧撞上真实门。
func testEndpoint(p Protocol) Endpoint { return Endpoint("/test/" + string(p)) }

// testInbound 与 implementedMatrix 的测试专用门配套。
func testInbound(p Protocol) Inbound { return Inbound{Protocol: p, Endpoint: testEndpoint(p)} }
```

(b) `TestPhase1IsComplete` 中 `p := r.Preservation(m.Availability())` 改为：

```go
		// 设计列是路径级的；可用列端点相对，逐门另打（见下方 Endpoints 循环）。
		p := r.Preservation(m.Availability(), Endpoint(""))
```

其 `t.Logf` 行去掉 `可用=%.3f`，并在循环体末尾追加逐门日志：

```go
		t.Logf("%-22s -> %-24s %s  pass=%2d emu=%d(off %d) deg=%d rej=%d n/a=%2d  设计=%.3f",
			r.In, r.Out, status,
			p.Passthrough, p.Emulate, p.EmulateOff, p.Degrade, p.Reject, p.NotApplicable,
			p.DesignScore())
		for _, ep := range r.Endpoints() {
			t.Logf("  门 %-58s 可用=%.3f", ep,
				r.Preservation(m.Availability(), ep).AvailableScore())
		}
```

(c) `TestRedeemedCapabilitiesAreExplicit` 整体替换为按 `in -> out @ endpoint` 键对账：

```go
// TestRedeemedCapabilitiesAreExplicit 把「哪扇门投放了哪些能力」也变成需要有人点头的事。
//
// 与 TestImplementedRoutesAreExplicit 同理，只是粒度从路径细到「路径 @ 端点」：
// 路径转正只说明这条路开始通车，说明不了每扇门都通。悄悄给某扇门多兑现一项能力，
// 等于宣称一个还没写的实现可用。
func TestRedeemedCapabilitiesAreExplicit(t *testing.T) {
	// OpenAI 两条同源直通各一扇门，字节级转发，可表达的全部兑现；
	// Native 本期只投放了文本生成那扇门。
	want := map[string][]canonical.Capability{
		string(ProtoOpenAIResponses) + " -> " + string(ProviderOpenAICompat) +
			" @ " + string(EndpointOpenAIResponses): ExpressibleSet(ProtoOpenAIResponses),
		string(ProtoOpenAIChat) + " -> " + string(ProviderOpenAICompat) +
			" @ " + string(EndpointOpenAIChat): ExpressibleSet(ProtoOpenAIChat),
		string(ProtoDashScopeNative) + " -> " + string(ProviderDashScopeNative) +
			" @ " + string(EndpointDashScopeTextGeneration): {
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapReasoning,
			canonical.CapWebSearch,
		},
	}

	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, r := range m.Routes() {
		routeKey := string(r.In) + " -> " + string(r.Out)
		for _, ep := range r.Endpoints() {
			key := routeKey + " @ " + string(ep)
			seen[key] = true
			caps, listed := want[key]
			if !listed {
				t.Errorf("路径 %s 开了门 %s，但不在名单里——投放请同步更新本测试与 fixture",
					routeKey, ep)
				continue
			}

			redeemed := map[canonical.Capability]bool{}
			for _, c := range caps {
				redeemed[c] = true
				if !r.Redeems(ep, c) {
					t.Errorf("%s 应已投放 %q，实际未兑现", key, c)
				}
			}
			for _, c := range canonical.AllCapabilities() {
				if !redeemed[c] && r.Redeems(ep, c) {
					t.Errorf("%s 多兑现了 %q，名单里没有它", key, c)
				}
			}
		}
	}

	// 名单声称的门必须真的开着，防止名单单边漂移。
	for key := range want {
		if !seen[key] {
			t.Errorf("名单声称 %s 已投放，实际没有这扇门", key)
		}
	}
}
```

（键字符串与设计文档完全一致，例如 `dashscope.native -> dashscope.native @ /api/v1/services/aigc/text-generation/generation`；这里用常量拼接而不是手写长字符串，防的是门常量改了名单没跟着改。）

(d) `TestRedeemingUndeliverableCapabilityFailsBuild` 整体删除——由步骤 3.1 的 `TestRedeemUndeliverableAtEndpointFailsBuild` 取代（两者不得并存）。

(e) `TestDeriveDoesNotInheritRedemption` 迁移：

```go
	base := NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
		Pass(ExpressibleSet(ProtoOpenAIChat)...).
		Redeem(EndpointOpenAIChat, ExpressibleSet(ProtoOpenAIChat)...)

	derived := base.Derive(ProtoOpenAIResponses, ProviderOpenAICompat)
	if derived.Implemented() {
		t.Error("派生路径不该继承兑现集合——实现是逐条写的，不是继承来的")
	}
	if derived.Redeems(EndpointOpenAIChat, canonical.CapTextGeneration) {
		t.Error("派生路径不该继承单项能力的兑现状态")
	}
```

(f) `TestPlannedRouteIsRejectedAtRuntime` 的 Check 调用改为：

```go
	_, err = m.Check(Inbound{Protocol: ProtoOpenAIResponses, Endpoint: EndpointOpenAIResponses},
		ProviderDashScopeCompatible,
		[]canonical.Capability{canonical.CapTextGeneration})
```

（闸门 2 先于闸门 3 命中，断言不变。）

(g) `TestUnredeemedCapabilityIsRejectedAtRuntime` 的全部 `m.Check(ProtoDashScopeNative, ProviderDashScopeNative, …)` 调用改为 `m.Check(Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeTextGeneration}, ProviderDashScopeNative, …)`，断言与注释不变（本任务只有文本门；多模态侧断言在任务 5 追加）。

(h) 其余 `Check` 调用点统一迁移（`implementedMatrix` 系用 `testInbound(...)`）：

- `TestCheckRejectsUnsupportedCapability`：`m.Check(testInbound(ProtoOpenAIChat), ProviderAnthropicMessages, …)`
- `TestCheckFlagsDecoderBugOnInexpressibleCapability`：`m.Check(testInbound(ProtoOpenAIChat), ProviderOpenAICompat, …)`
- `TestCheckReportsDegradation`：`m.Check(testInbound(ProtoOpenAIChat), ProviderAnthropicMessages, …)`
- `TestCheckReportsEmulation`：`m.Check(testInbound(ProtoOpenAIResponses), ProviderDashScopeNative, …)`
- `TestCheckFailsClosedOnUnknownRoute`：`m.Check(Inbound{Protocol: protoNeverRegistered, Endpoint: Endpoint("/test/never")}, ProviderAnthropicMessages, …)`
- `TestFixtureGateActuallyBites` 中 `Redeem(ExpressibleSet(ProtoOpenAIChat)...)` → `Redeem(EndpointOpenAIChat, ExpressibleSet(ProtoOpenAIChat)...)`

- [ ] **步骤 3.7：`preference_test.go` — 分数对账迁到端点粒度**

(a) `TestPreferenceMatchesPreservation`：两处 `Preservation(m.Availability()).DesignScore()` 与 `mustRoute(...).Preservation(m.Availability()).DesignScore()` 改为 `Preservation(m.Availability(), Endpoint("")).DesignScore()`，并在循环前加注释：

```go
	// 只用设计列：设计计数与端点无关，传 Endpoint("") 合法（A3）。
	// 绝不从这条调用取可用列——零值端点的可用列恒为零。
```

(b) `TestRuntimeRankingUsesAvailableScore` 整体替换为端点粒度：

```go
// TestRuntimeRankingUsesAvailableScore 验证选路那一列的对账。
//
// 与 TestPreferenceMatchesPreservation 互补：这条验的是「运行时选路是否正确」，
// 因此必须用已实现路径的**端点相对**可用分数（见 ADR-0002）。
func TestRuntimeRankingUsesAvailableScore(t *testing.T) {
	m := implementedMatrix(t, nil)

	// 半句一：每扇已开门的可用分在 (0, DesignScore] 区间。
	for _, r := range m.Routes() {
		design := r.Preservation(m.Availability(), Endpoint("")).DesignScore()
		for _, ep := range r.Endpoints() {
			s := r.Preservation(m.Availability(), ep).AvailableScore()
			if s <= 0 || s > design {
				t.Errorf("路径 %s -> %s 门 %s 的可用分 %.3f 应在 (0, 设计 %.3f] 区间",
					r.In, r.Out, ep, s, design)
			}
		}
	}

	// 半句二：同一入站协议下兑现了同一扇门的多条路径按偏好序比较。
	// Phase 1 没有两条路径同开一扇门，当前此半句空转；将来同门第二路径出现时
	// 排序对账自动生效，不会漏。
	byInboundDoor := map[Protocol]map[Endpoint][]Provider{}
	for _, r := range m.Routes() {
		for _, ep := range r.Endpoints() {
			if byInboundDoor[r.In] == nil {
				byInboundDoor[r.In] = map[Endpoint][]Provider{}
			}
			byInboundDoor[r.In][ep] = append(byInboundDoor[r.In][ep], r.Out)
		}
	}
	for in, doors := range byInboundDoor {
		for ep, providers := range doors {
			ranked := m.RankOutbound(in, providers)
			for i := 1; i < len(ranked); i++ {
				prev, _ := m.Route(in, ranked[i-1])
				cur, _ := m.Route(in, ranked[i])
				ps := prev.Preservation(m.Availability(), ep).AvailableScore()
				cs := cur.Preservation(m.Availability(), ep).AvailableScore()
				if ps < cs {
					t.Errorf("入站 %s 门 %s：运行时选路把 %s（可用 %.3f）排在 %s（%.3f）之前",
						in, ep, ranked[i-1], ps, ranked[i], cs)
				}
			}
		}
	}
}
```

(c) `TestGatedEmulationSplitsTheTwoColumns`：`p := r.Preservation(m.Availability())` → `p := r.Preservation(m.Availability(), EndpointOpenAIResponses)`；`po` 同理改为 `Preservation(on.Availability(), EndpointOpenAIResponses)`。

(d) `TestAvailableScoreCountsOnlyRedeemed` 迁移到文本门（两门版本在任务 5 重写）：

```go
	p := mustRoute(t, m, ProtoDashScopeNative, ProviderDashScopeNative).
		Preservation(m.Availability(), EndpointDashScopeTextGeneration)
```

（其余断言不变：设计分 1.000、`NotRedeemed = len(ExpressibleSet(ProtoDashScopeNative)) - 5`、可用分 5/18、`Gated()` 为真。）

(e) `TestBestOutboundSkipsRejectingRoute`：两处 `Redeem` 改为 `Redeem(EndpointOpenAIChat, others...)` / `Redeem(EndpointOpenAIChat, ExpressibleSet(ProtoOpenAIChat)...)`；`m.BestOutbound(ProtoOpenAIChat, …)` 改为 `m.BestOutbound(Inbound{Protocol: ProtoOpenAIChat, Endpoint: EndpointOpenAIChat}, …)`。

(f) `TestBestOutboundPrefersHomogeneous`：两处 `Preservation(m.Availability()).DesignScore()` 改为 `Preservation(m.Availability(), Endpoint("")).DesignScore()`（只比设计列，注释同 (a)）。

(g) `TestBestOutboundReportsWhyEverythingFailed`：`m.BestOutbound(ProtoOpenAIChat, …)` → `m.BestOutbound(testInbound(ProtoOpenAIChat), …)`。

(h) `TestBestOutboundFailsWhenNothingRegistered`：`m.BestOutbound(protoNeverRegistered, …)` → `m.BestOutbound(Inbound{Protocol: protoNeverRegistered, Endpoint: Endpoint("/test/never")}, …)`。

(i) `TestDashScopeNativeInboundPreservesMost`：三处 `Preservation(m.Availability())` 改为 `Preservation(m.Availability(), Endpoint(""))`（只取设计列，注释同 (a)；循环里的 PLANNED 路径没有门，只能走零值端点取设计列）。

(j) `TestDerivedRouteStaysComplete`：`p := r.Preservation(m.Availability())` → `p := r.Preservation(m.Availability(), Endpoint(""))`（只用设计计数）。

- [ ] **步骤 3.8：`gateway/handler.go` — serve() 传入站坐标**

`serve()` 中 `BestOutbound` 调用替换为：

```go
	// 路由给出候选，矩阵按入站坐标（协议 + 门）与能力裁决。两者分工，不互相包含。
	kind, verdict, err := h.deps.Matrix.BestOutbound(
		degrade.Inbound{
			Protocol: h.in.protocol,
			Endpoint: degrade.Endpoint(h.in.upstreamPath(r)),
		},
		router.Kinds(targets), decoded.Capabilities())
```

（`upstreamPath` 对 DashScope Native 入站返回 `r.URL.Path`，对 OpenAI 入站返回各自固定路径——三个 handler 无需各自新增逻辑，现状已满足。）

- [ ] **步骤 3.9：`gateway/build.go` — 命名常量注册 + 启动期双向对账**

(a) Mux 注册区替换为：

```go
	native := NewDashScopeNativeHandler(deps) // handler 无状态，多扇门复用同一实例
	mux.Handle("POST "+string(degrade.EndpointOpenAIResponses), NewResponsesHandler(deps))
	mux.Handle("POST "+string(degrade.EndpointOpenAIChat), NewChatHandler(deps))
	mux.Handle("POST "+dashscopenative.TextGenerationPath, native)
```

(b) 在 `return built, nil` 之前插入启动期双向对账（本任务 registered 只含三扇已投放的门，多模态门在任务 5 加入）：

```go
	// 启动期双向对账：矩阵兑现过的门必须注册了处理器，注册了处理器的门
	// 必须有路径兑现。防两种漂移：兑现过的门忘了注册，请求落进 501 兜底；
	// 注册了的门忘了在矩阵兑现，变成一处永远返回 501 的空头承诺。
	registered := map[degrade.Endpoint]bool{
		degrade.EndpointOpenAIResponses:         true,
		degrade.EndpointOpenAIChat:              true,
		degrade.EndpointDashScopeTextGeneration: true,
	}
	opened := map[degrade.Endpoint]bool{}
	for _, r := range m.Routes() {
		if !r.Implemented() {
			continue
		}
		for _, ep := range r.Endpoints() {
			opened[ep] = true
			if !registered[ep] {
				return nil, fmt.Errorf("gateway: 端点 %s 已在矩阵兑现，但没有注册处理器", ep)
			}
		}
	}
	for ep := range registered {
		if !opened[ep] {
			return nil, fmt.Errorf("gateway: 端点 %s 注册了处理器，但没有任何路径兑现它", ep)
		}
	}
```

（passthrough 适配器的装配默认路径维持 `TextGenerationPath` 不变——它只是兜底默认值，handler 永远注入实际请求路径。两条命名空间兜底不变。）

- [ ] **步骤 3.10：`gateway/build_test.go` — 对账双向咬人测试**

在 `build_test.go`（包 `gateway_test`）的 import 中补 `"log/slog"`、`"strings"`、`"github.com/yobo2u/omugw/internal/canonical"`（其余已存在），并追加：

```go
// buildTestConfig 组装一份最小可用的全量配置：对账发生在「模型路由已配置」
// 之后，空配置会在对账之前提前返回，测不到闸门。
func buildTestConfig(upstreamURL string) config.Config {
	return config.Config{
		Auth: config.Auth{Keys: []config.AuthKey{{ID: "test", Key: "sk-test-1234567890"}}},
		Credentials: map[string][]config.CredentialSpec{
			"pool1": {{ID: "1", Secret: "sec1"}},
		},
		Providers: []config.ProviderSpec{
			{Endpoint: "ep1", Kind: "openai.compat", BaseURL: upstreamURL, CredentialPool: "pool1"},
		},
		Models: []config.ModelSpec{
			{Match: "*", Targets: []config.TargetSpec{{Endpoint: "ep1", UpstreamModel: "m"}}},
		},
		Timeouts: config.Timeouts{
			Connect: time.Second, FirstByte: 2 * time.Second, Total: 3 * time.Second, Idle: time.Second,
		},
		Limits: config.Limits{MaxRequestBytes: 1024 * 1024, MaxInlineBytes: 1024 * 1024},
	}
}

// TestBuildFailsWhenRedeemedEndpointUnregistered 防「兑现过的门忘了注册」：
// 请求会落进 501 兜底，兑现承诺悄悄落空。
func TestBuildFailsWhenRedeemedEndpointUnregistered(t *testing.T) {
	m := degrade.NewMatrix()
	if err := m.Add(degrade.NewRoute(degrade.ProtoOpenAIChat, degrade.ProviderOpenAICompat).
		MarkHomogeneous().
		Pass(degrade.ExpressibleSet(degrade.ProtoOpenAIChat)...).
		Redeem(degrade.Endpoint("/v1/unregistered"), canonical.CapTextGeneration).
		Build()); err != nil {
		t.Fatal(err)
	}

	_, err := gateway.Build(buildTestConfig("http://127.0.0.1:0"), m,
		obs.NewMetrics(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("兑现了未注册端点的矩阵应当让启动失败")
	}
	if !strings.Contains(err.Error(), "没有注册处理器") {
		t.Errorf("错误应指出端点未注册处理器: %v", err)
	}
}

// TestBuildFailsWhenRegisteredEndpointUnredeemed 防「注册了的门没人兑现」：
// 那是一处永远返回 501 的空头承诺。
func TestBuildFailsWhenRegisteredEndpointUnredeemed(t *testing.T) {
	// 只兑现 chat 门：registered 名单里的 responses 门与 Native 文本门没人兑现。
	m := degrade.NewMatrix()
	if err := m.Add(degrade.NewRoute(degrade.ProtoOpenAIChat, degrade.ProviderOpenAICompat).
		MarkHomogeneous().
		Pass(degrade.ExpressibleSet(degrade.ProtoOpenAIChat)...).
		Redeem(degrade.EndpointOpenAIChat, degrade.ExpressibleSet(degrade.ProtoOpenAIChat)...).
		Build()); err != nil {
		t.Fatal(err)
	}

	_, err := gateway.Build(buildTestConfig("http://127.0.0.1:0"), m,
		obs.NewMetrics(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("注册了处理器却无路径兑现的端点应当让启动失败")
	}
	if !strings.Contains(err.Error(), "没有任何路径兑现它") {
		t.Errorf("错误应指出端点无人兑现: %v", err)
	}
}
```

（`buildTestConfig` 的 provider Kind 用 `openai.compat`，与矩阵中兑现的路径同族，避免撞上「未实现的协议族」拒绝。先写这两个测试再跑一遍：由于对账代码已在 3.9 落地，它们应直接 PASS；若你选择先写测试后写对账，则先 RED 后 GREEN，两种顺序都可，提交前必须绿。）

- [ ] **步骤 3.11：`cmd/omugw/main.go` — A2 日志改造**

`run()` 中路径日志循环（`var implemented int` 起至循环结束）整体替换为：

```go
	var implemented int
	for _, r := range matrix.Routes() {
		// 设计列是路径级的，与端点无关；这里只取 DesignScore。
		// 可用列没有路径级聚合——并集分数不对应任何一扇真实的门，
		// 绝不从这条调用取可用列（可用分一律按门另打，见下方循环）。
		design := r.Preservation(matrix.Availability(), degrade.Endpoint(""))
		if r.Implemented() {
			implemented++
		}
		attrs := []any{
			"inbound", string(r.In),
			"outbound", string(r.Out),
			"implemented", r.Implemented(),
			"fast_path", r.Homogeneous,
			"design_score", design.DesignScore(),
		}
		log.Info("已注册转换路径", attrs...)

		// 每扇已开门一条可用分条目：可用列永远端点相对（ADR-0002 的延伸）。
		for _, ep := range r.Endpoints() {
			log.Info("已投放端点",
				"inbound", string(r.In),
				"outbound", string(r.Out),
				"endpoint", string(ep),
				"available_score", r.Preservation(matrix.Availability(), ep).AvailableScore(),
			)
		}
	}
```

（未实现路径不打印可用分的纪律不变：没有门就没有条目，与矩阵文档的「—」一致。）

### 3C. 收敛验证与提交

- [ ] **步骤 3.12：RED 测试转绿**

```bash
go test ./internal/degrade/ -run 'TestRedeemZeroEndpointFailsBuild|TestRedeemUndeliverableAtEndpointFailsBuild|TestEndpointsDerivedFromRedemption|TestCheckRejectsUnopenedEndpoint|TestCheckDispositionBeforeRedemption|TestPreservationAvailableIsEndpointRelative' -v
```

预期：全部 PASS。

- [ ] **步骤 3.13：全量验证（行为保持证明）**

```bash
gofmt -l . && go vet ./... && go build ./... && go test ./...
make matrix
```

预期：`gofmt -l` 无输出；vet / build / 全部测试通过；`make matrix` 通过——**且 `docs/degradation-matrix.md` 未被修改**（`GIT_MASTER=1 git status` 中不应出现它；单门路径的文档输出与迁移前逐字节一致）。

- [ ] **步骤 3.14：提交（原子迁移）**

```bash
GIT_MASTER=1 git add internal/degrade/ internal/gateway/ cmd/omugw/
GIT_MASTER=1 git status   # 确认只有本任务涉及的文件
GIT_MASTER=1 git commit -m "兑现从路径级下沉到端点级，Check 与 BestOutbound 按入站坐标裁决" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 4：fixture 门槛扩展为端点路径双向对账（A1）

**依赖**：任务 3（用 `Route.Endpoints()`）。
**文件**：修改 `internal/degrade/matrix_test.go`。

门槛语义（设计文档「端点级 fixture 路径门槛」）：

1. 每一扇已开门必须至少有一个 fixture 的 `request.path` 等于该端点（每扇门都要有证据）；
2. 每个 fixture 的 `request.path` 必须是一扇已开门（给未开门写 fixture 不构成证据，也会让门槛误以为门已兑现）。

实现约束（A1）：本地用 `encoding/json` 解析 `request.path`，不用 `testkit.LoadDir`——`checkRouteFixtures` 必须返回 error 供调用方按路径归因，且手里没有 `testing.T`。

- [ ] **步骤 4.1：写咬人测试（RED）**

在 `matrix_test.go` 追加：

```go
// TestFixtureGateReconcilesEndpointPaths 用伪造仓库根证明门槛双向咬人，
// 不碰真实 fixture。
//
// checkRouteFixtures 接受 repoRoot 参数正是为了这个：门槛自身的有效性
// 必须可测，而不是只能拿真实目录碰运气。
func TestFixtureGateReconcilesEndpointPaths(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, FixtureDir(ProtoOpenAIChat, ProviderOpenAICompat))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, path string) {
		t.Helper()
		body := fmt.Sprintf(
			`{"name":%q,"request":{"method":"POST","path":%q},"response":{"status":200}}`,
			name, path)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := func() *Route {
		r, err := NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
			Pass(ExpressibleSet(ProtoOpenAIChat)...).
			Redeem(EndpointOpenAIChat, ExpressibleSet(ProtoOpenAIChat)...).
			Build()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// 正向：fixture 路径是已开门，且该门有证据。
	write("ok.json", string(EndpointOpenAIChat))
	if err := checkRouteFixtures(base(), root); err != nil {
		t.Errorf("正向用例应当通过: %v", err)
	}

	// 反方向一：fixture 指向未开门——不构成证据。
	write("stray.json", "/v1/other")
	err := checkRouteFixtures(base(), root)
	if err == nil || !strings.Contains(err.Error(), "不是一扇已开门") {
		t.Errorf("指向未开门的 fixture 应当失败: %v", err)
	}
	if rmErr := os.Remove(filepath.Join(dir, "stray.json")); rmErr != nil {
		t.Fatal(rmErr)
	}

	// 反方向二：已开门没有任何 fixture 证据。
	r := base()
	r.Redeem(Endpoint("/v1/extra"), canonical.CapTextGeneration)
	err = checkRouteFixtures(r, root)
	if err == nil || !strings.Contains(err.Error(), "/v1/extra") {
		t.Errorf("缺少证据的已开门应当失败: %v", err)
	}
}
```

运行：

```bash
go test ./internal/degrade/ -run TestFixtureGateReconcilesEndpointPaths -v
```

预期：FAIL——第一个失败点是反方向一（现有 `checkRouteFixtures` 不做路径对账，`err == nil`）。正确的 RED。

- [ ] **步骤 4.2：扩展 checkRouteFixtures（GREEN）**

在 `matrix_test.go` 的 import 中补 `"encoding/json"`。在 `checkRouteFixtures` 的既有有损格子检查**之后**、`return nil` 之前插入：

```go
	// 端点级 fixture 路径双向对账（ADR-0001 的深化）：
	// 1. 每扇已开门至少有一个 fixture 的 request.path 等于该端点；
	// 2. 每个 fixture 的 request.path 必须是一扇已开门。
	//
	// 本地用 encoding/json 解析 request.path，而不是 testkit.LoadDir：
	// 本函数必须返回 error 供调用方按路径归因，手里没有 testing.T。
	doors := r.Endpoints()
	opened := map[Endpoint]bool{}
	for _, ep := range doors {
		opened[ep] = true
	}
	covered := map[Endpoint]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("读取 fixture %s 失败: %w", name, err)
		}
		var meta struct {
			Request struct {
				Path string `json:"path"`
			} `json:"request"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return fmt.Errorf("解析 fixture %s 失败: %w", name, err)
		}
		ep := Endpoint(meta.Request.Path)
		if !opened[ep] {
			return fmt.Errorf("fixture %s 的 request.path %q 不是一扇已开门（已开门: %v）",
				name, meta.Request.Path, doors)
		}
		covered[ep] = true
	}
	var uncovered []string
	for _, ep := range doors {
		if !covered[ep] {
			uncovered = append(uncovered, string(ep))
		}
	}
	if len(uncovered) > 0 {
		return fmt.Errorf("以下已开门没有 request.path 匹配的 fixture: %s",
			strings.Join(uncovered, ", "))
	}
```

同时把 `checkRouteFixtures` 的 doc 注释更新为：

```go
// checkRouteFixtures 校验一条已实现路径的 fixture 证据是否齐备：
// 目录存在且有用例、有损格子（DEGRADE / EMULATE）有同名举证、
// 以及端点级双向对账——每扇已开门要有证据，每个证据要指向已开门。
```

- [ ] **步骤 4.3：验证**

```bash
go test ./internal/degrade/ -v
go test ./...
```

预期：`TestFixtureGateReconcilesEndpointPaths` PASS；`TestImplementedRoutesHaveFixtures` 对三条已转正路径全部通过（现有 8 份 fixture 的 `request.path` 已逐份核对：openai.responses 三份指向 `/v1/responses`，openai.chat 两份指向 `/v1/chat/completions`，Native 三份指向文本生成端点——与各路径唯一的门一致）；`TestFixtureGateActuallyBites` 仍咬人（无目录先失败）。

- [ ] **步骤 4.4：提交**

```bash
GIT_MASTER=1 git add internal/degrade/matrix_test.go
GIT_MASTER=1 git commit -m "fixture 门槛扩展为端点路径双向对账" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 5：投放多模态生成门——fixture、负例、端点级文档

**依赖**：任务 4。
**本任务是一个原子投放提交**：第二扇门一旦兑现，文档主表立即落入「见端点细分」分支，端点细分小节、白名单、Mux 注册、build_test 翻转必须同批落地，任何中间提交要么红要么文档误导。

**文件**：`rules_phase1.go`、`matrix_test.go`（白名单 + TestUnredeemed 双向）、`preference_test.go`（两门版本）、`markdown.go` + `markdown_test.go`（新建）、`build.go`、`build_test.go`、`gateway_test.go`（limits 参数）、`multimodal_test.go`（新建）、三份 fixture、三份 golden、`docs/degradation-matrix.md`（生成）。

### 5A. RED：fixture 先行

- [ ] **步骤 5.1：写三份 fixture**

新建 `testdata/routes/dashscope.native__dashscope.native/multimodal-basic.json`（同步；text + image URL + audio 内联 data URI + video URL 字符串，纯键式块；证明三种媒体能力、URL 与内联两种承载、纯键式解码分支）：

```json
{
  "name": "dashscope.native__dashscope.native/multimodal-basic",
  "note": "多模态门同步最小闭环：text + image（URL）+ audio（内联 data URI）+ video（URL 字符串）四种单键纯键式内容块，证明三种媒体能力、URL 与内联两种承载、纯键式解码分支。request 是**客户端发给网关**的内容，response 是被打桩的上游返回。",
  "request": {
    "method": "POST",
    "path": "/api/v1/services/aigc/multimodal-generation/generation",
    "headers": {
      "authorization": "<redacted>",
      "content-type": "application/json"
    },
    "body": {
      "model": "logical-omni",
      "input": {
        "messages": [
          {
            "role": "user",
            "content": [
              { "text": "图里有什么？音频说了什么？视频讲了什么？" },
              { "image": "https://example.com/cat.png" },
              { "audio": "data:audio/wav;base64,UklGRgAAQABXQVZFZm10IBAAAAABAAEAQB8AAIA+AAACABAAZGF0YQAAAAA=" },
              { "video": "https://example.com/clip.mp4" }
            ]
          }
        ]
      },
      "parameters": { "result_format": "message" }
    }
  },
  "response": {
    "status": 200,
    "headers": {
      "content-type": "application/json"
    },
    "body": {
      "output": {
        "choices": [
          {
            "finish_reason": "stop",
            "message": { "role": "assistant", "content": [{ "text": "图中是一只猫；音频说的是你好；视频是一段街景。" }] }
          }
        ]
      },
      "usage": { "input_tokens": 52, "output_tokens": 19, "total_tokens": 71 },
      "request_id": "3f2c1a90-8f4d-4b6a-9d2e-6a7b8c9d0e1f"
    }
  }
}
```

新建 `testdata/routes/dashscope.native__dashscope.native/multimodal-streaming.json`（SSE；text + image 内联 data URI，带 type 块；证明多模态门的 streaming、SSE 头转发、带 type 解码分支；frames 复现上游把多帧挤进一次 Write 的节奏，用量逐帧累计，`finish_reason=stop` 收尾）：

```json
{
  "name": "dashscope.native__dashscope.native/multimodal-streaming",
  "note": "多模态门 SSE 转发：X-DashScope-SSE: enable 声明流式，内容为 text + image（内联 data URI）的带 type 块，证明 streaming、SSE 头转发、带 type 解码分支。frames 复现上游把多帧挤进一次 Write 的节奏；用量逐帧累计，finish_reason=stop 收尾，无 [DONE] 哨兵。",
  "request": {
    "method": "POST",
    "path": "/api/v1/services/aigc/multimodal-generation/generation",
    "headers": {
      "authorization": "<redacted>",
      "content-type": "application/json",
      "x-dashscope-sse": "enable"
    },
    "body": {
      "model": "logical-omni",
      "input": {
        "messages": [
          {
            "role": "user",
            "content": [
              { "type": "text", "text": "描述这张图" },
              { "type": "image", "image": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYGBgAAAABQABh6FO1AAAAABJRU5ErkJggg==" }
            ]
          }
        ]
      },
      "parameters": { "result_format": "message", "incremental_output": true }
    }
  },
  "response": {
    "status": 200,
    "headers": { "content-type": "text/event-stream" },
    "sse": {
      "frames": [2, 1],
      "events": [
        {
          "event": "result",
          "data": "{\"output\":{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":[{\"text\":\"图中\"}]},\"finish_reason\":\"null\"}]},\"usage\":{\"input_tokens\":40,\"output_tokens\":2,\"total_tokens\":42},\"request_id\":\"9b7d5c31-2a4f-4e8d-b1c2-5d6e7f8091a2\"}"
        },
        {
          "event": "result",
          "data": "{\"output\":{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":[{\"text\":\"是一个\"}]},\"finish_reason\":\"null\"}]},\"usage\":{\"input_tokens\":40,\"output_tokens\":4,\"total_tokens\":44},\"request_id\":\"9b7d5c31-2a4f-4e8d-b1c2-5d6e7f8091a2\"}"
        },
        {
          "event": "result",
          "data": "{\"output\":{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":[{\"text\":\"红点。\"}]},\"finish_reason\":\"stop\"}]},\"usage\":{\"input_tokens\":40,\"output_tokens\":6,\"total_tokens\":46},\"request_id\":\"9b7d5c31-2a4f-4e8d-b1c2-5d6e7f8091a2\"}"
        }
      ]
    }
  }
}
```

新建 `testdata/routes/dashscope.native__dashscope.native/multimodal-video-frames.json`（同步；text + video 为 3 帧 data URI 数组；证明 video 帧数组逐帧内联累加、video_input）：

```json
{
  "name": "dashscope.native__dashscope.native/multimodal-video-frames",
  "note": "video 为帧数组：三帧 data URI 数组，证明 video 帧数组逐帧内联累加与 video_input。内联字节按帧累计，内联上限绕不过去。",
  "request": {
    "method": "POST",
    "path": "/api/v1/services/aigc/multimodal-generation/generation",
    "headers": {
      "authorization": "<redacted>",
      "content-type": "application/json"
    },
    "body": {
      "model": "logical-omni",
      "input": {
        "messages": [
          {
            "role": "user",
            "content": [
              { "text": "概括视频内容" },
              {
                "video": [
                  "data:video/mp4;base64,AAAAB3NzaC1lZHR1c2gtZnJhbWUtMQ==",
                  "data:video/mp4;base64,BBBBAAAAB3NzaC1lZHR1c2gtZnJhbWUtMg==",
                  "data:video/mp4;base64,CCCCAAAAB3NzaC1lZHR1c2gtZnJhbWUtMw=="
                ]
              }
            ]
          }
        ]
      },
      "parameters": { "result_format": "message" }
    }
  },
  "response": {
    "status": 200,
    "headers": { "content-type": "application/json" },
    "body": {
      "output": {
        "choices": [
          {
            "finish_reason": "stop",
            "message": { "role": "assistant", "content": [{ "text": "视频共三帧：圆形、方形、三角形。" }] }
          }
        ]
      },
      "usage": { "input_tokens": 66, "output_tokens": 14, "total_tokens": 80 },
      "request_id": "c4d5e6f7-0a1b-4c2d-9e8f-1a2b3c4d5e6f"
    }
  }
}
```

（三份 fixture 的信封与文本 fixture 相同：model / input.messages / parameters；模型用逻辑名，由 harness 的 `Match: "*"` 路由改写为 upstream-model；上游响应为 DashScope 标准信封 output.choices + usage.input_tokens/output_tokens + request_id。）

- [ ] **步骤 5.2：运行回放确认 RED**

```bash
go test ./internal/gateway/ -run TestDashScopeNativeRouteConformance -v
```

预期：三个新用例 FAIL——golden 缺失（「读取 golden 文件 … 失败（首次生成请加 -update）」）且网关对多模态门返回 501（门未开）。正确的 RED。

### 5B. 开门与迁移

- [ ] **步骤 5.3：`rules_phase1.go` 兑现多模态门**

Native 路径注释块中「本期只投放了文本生成那一个」的叙述更新为两门版本（保留「处置与投放分家」的解释），`Redeem` 链改为：

```go
	if err := m.Add(NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoDashScopeNative)...).
		// 文本门：既有 5 项，原样保留。reasoning 有 fixture 证据
		//（tools-and-search.json 带 enable_thinking: true），继续持有。
		Redeem(EndpointDashScopeTextGeneration,
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapReasoning,
			canonical.CapWebSearch,
		).
		// 多模态门：本期投放恰 5 项。file_input 不兑现：官方内容块没有 file；
		// tool_calling / reasoning / web_search 在这扇门上没有 fixture 证据——
		// 无证据不兑现，哪怕上游模型可能支持。
		Redeem(EndpointDashScopeMultimodal,
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapVisionInput,
			canonical.CapAudioInput,
			canonical.CapVideoInput,
		).
		Build()); err != nil {
		return nil, err
	}
```

- [ ] **步骤 5.4：白名单追加多模态门**

`TestRedeemedCapabilitiesAreExplicit` 的 `want` 映射追加一个键（紧接文本门键之后）：

```go
		string(ProtoDashScopeNative) + " -> " + string(ProviderDashScopeNative) +
			" @ " + string(EndpointDashScopeMultimodal): {
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapVisionInput,
			canonical.CapAudioInput,
			canonical.CapVideoInput,
		},
```

- [ ] **步骤 5.5：`TestUnredeemedCapabilityIsRejectedAtRuntime` 更新为双向断言**

在任务 3 迁移版的基础上：文本门 + vision_input → 501 的断言与文本门五项放行循环保留（均用 `Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeTextGeneration}`）；追加多模态门放行断言：

```go
	// 多模态门：vision_input 已投放，照常放行——闸门只挡未投放的，
	// 不把整条路径一起关上。
	if _, err := m.Check(Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeMultimodal},
		ProviderDashScopeNative,
		[]canonical.Capability{canonical.CapTextGeneration, canonical.CapVisionInput}); err != nil {
		t.Errorf("vision_input 在多模态门已投放，不该被拦下: %v", err)
	}
```

（设计处置断言不变：vision_input 仍是 PASSTHROUGH。）

- [ ] **步骤 5.6：`build.go` 注册多模态门**

(a) Mux 注册区，`native` 实例追加第二条注册：

```go
	mux.Handle("POST "+dashscopenative.MultimodalGenerationPath, native)
```

（两条命名空间兜底不变：`POST /api/v1/` 前缀兜底返回 dashscopewire 协议化 501；不带方法的 `/api/v1/` 兜底返回框架 404。精确注册的两扇门都比 `/api/v1/` 更具体，凭最长前缀优先命中。）

(b) `registered` 映射追加：

```go
		degrade.EndpointDashScopeMultimodal:     true,
```

- [ ] **步骤 5.7：`build_test.go` 翻转多模态用例**

`TestBuiltMux_DashScopeNativeFallback_WithUpstream` 的用例表中，原「未投放的多模态端点返回 501」条目替换为：

```go
		{
			name:           "已投放的多模态端点由精确路由处理，打到上游",
			method:         "POST",
			path:           dashscopenative.MultimodalGenerationPath,
			expectedStatus: http.StatusOK,
			expectedCode:   "",
			expectUpstream: true,
		},
```

（该用例的请求体是纯文本消息，能力集为 text_generation，多模态门已兑现，转发成立。）

末尾指标断言 `if notImplementedCount != 3` 改为 `if notImplementedCount != 2`（兜底 501 只剩 embedding 与 rerank 两个用例），注释同步。

- [ ] **步骤 5.8：`gateway_test.go` 增加 limits 注入点**

(a) `newHarnessFor` 签名增加 `limits config.Limits` 参数（放在 `mk` 之后、`ups` 之前），函数体 `Limits: config.Default().Limits` 改为 `Limits: limits`。

(b) 三个调用方传入默认值：`newHarness`、`newChatHarness`、`newDashScopeNativeHarness` 的 `newHarnessFor(...)` 调用各补 `config.Default().Limits` 实参。

(c) 追加：

```go
// newDashScopeNativeHarnessWithLimits 与 newDashScopeNativeHarness 相同，
// 但注入自定义 Limits——内联闸门先于矩阵裁决生效的性质（测试矩阵用例 11）
// 要用一个小到会被击穿的上限来证明。
func newDashScopeNativeHarnessWithLimits(t *testing.T, limits config.Limits, ups ...*upstream) *harness {
	t.Helper()
	return newHarnessFor(t, dashscopenative.TextGenerationPath, "/v1/responses",
		degrade.ProviderDashScopeNative, NewDashScopeNativeHandler, limits, ups...)
}
```

- [ ] **步骤 5.9：写负例测试（测试矩阵用例 7 / 8 / 9 / 11）**

新建 `internal/gateway/multimodal_test.go`：

```go
package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

// doPath 按指定端点路径直接打 handler，供多模态门的负例使用。
func doPath(t *testing.T, hs *harness, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	return rec
}

// 用例 7：文本门未兑现 vision_input。设计处置是 PASSTHROUGH，
// 但无证据不兑现——501 说「等」，请求根本没有出门。
func TestTextDoorRejectsVisionInput(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {})
	hs := newDashScopeNativeHarness(t, true, up)

	rec := doPath(t, hs, dashscopenative.TextGenerationPath,
		`{"model":"m","input":{"messages":[{"role":"user","content":[{"text":"图里有什么？"},{"image":"https://example.com/x.png"}]}]}}`)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501（vision_input 未投放到文本门）: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——应当被矩阵拦下，没有出门", n)
	}
	if !strings.Contains(rec.Body.String(), "vision_input") {
		t.Errorf("错误应点名能力: %s", rec.Body.String())
	}
}

// 用例 8：多模态门不兑现 file_input——官方内容块词表是 text / image / audio / video，
// 没有通用 file 块。两扇门都不兑现，运行时 501。
func TestMultimodalDoorRejectsFileInput(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {})
	hs := newDashScopeNativeHarness(t, true, up)

	rec := doPath(t, hs, dashscopenative.MultimodalGenerationPath,
		`{"model":"m","input":{"messages":[{"role":"user","content":[{"text":"处理这个文件"},{"file":"https://example.com/a.pdf"}]}]}}`)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501（file_input 两扇门都不兑现）: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——应当被矩阵拦下，没有出门", n)
	}
	if !strings.Contains(rec.Body.String(), "file_input") {
		t.Errorf("错误应点名能力: %s", rec.Body.String())
	}
}

// 用例 9：多模态门未兑现 reasoning。「无证据不兑现」纪律的活体证明：
// 哪怕上游模型可能支持，矩阵没有证据就不放行。
func TestMultimodalDoorRejectsReasoning(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {})
	hs := newDashScopeNativeHarness(t, true, up)

	rec := doPath(t, hs, dashscopenative.MultimodalGenerationPath,
		`{"model":"m","input":{"messages":[{"role":"user","content":[{"text":"想一想再回答"}]}]},"parameters":{"enable_thinking":true}}`)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501（reasoning 未投放到多模态门）: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——应当被矩阵拦下，没有出门", n)
	}
	if !strings.Contains(rec.Body.String(), "reasoning") {
		t.Errorf("错误应点名能力: %s", rec.Body.String())
	}
}

// 用例 11：内联闸门先于矩阵裁决生效。自定义上限 64 字节，
// 发送 128 字节内联图像：必须 400（而不是 501），且上游零调用。
func TestInlineLimitBeatsMatrixOnMultimodalDoor(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {})
	limits := config.Default().Limits
	limits.MaxInlineBytes = 64
	hs := newDashScopeNativeHarnessWithLimits(t, limits, up)

	rec := doPath(t, hs, dashscopenative.MultimodalGenerationPath,
		`{"model":"m","input":{"messages":[{"role":"user","content":[{"text":"x"},{"image":"data:image/png;base64,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}]}]}}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（内联负载超限）: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——内联闸门应先于矩阵拦下请求", n)
	}
}
```

（内联 data URI 的 base64 部分恰为 128 个 `A`；无论解码器按整个 data URI 还是仅 base64 段统计，都超过 64 字节上限。用例 10——未登记的 `/api/v1/…` 端点落 Mux 兜底协议化 501——由 `build_test.go` 既有的 embedding / rerank 用例覆盖，无需新增。）

运行：

```bash
go test ./internal/gateway/ -run 'TestTextDoorRejectsVisionInput|TestMultimodalDoorRejectsFileInput|TestMultimodalDoorRejectsReasoning|TestInlineLimitBeatsMatrixOnMultimodalDoor' -v
```

预期：门已开（5.3 已落地）时四个全部 PASS；若在 5.3 之前运行，用例 8 / 9 会 FAIL（门未开时消息点名端点而非能力）——属正确 RED。

### 5C. 端点级文档

- [ ] **步骤 5.10：写 markdown 锁测试（RED）**

新建 `internal/degrade/markdown_test.go`：

```go
package degrade

import (
	"strings"
	"testing"
)

// TestMarkdownShowsEndpointBreakdown 固化端点细分小节的呈现：
// 每扇已开门逐行列出端点、已投放能力与端点相对可用分。
func TestMarkdownShowsEndpointBreakdown(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	doc := m.Markdown()

	if !strings.Contains(doc, "### 端点细分") {
		t.Fatal("文档应包含端点细分小节")
	}
	for _, ep := range []string{
		string(EndpointDashScopeTextGeneration),
		string(EndpointDashScopeMultimodal),
	} {
		if !strings.Contains(doc, ep) {
			t.Errorf("端点细分应列出 %s", ep)
		}
	}
	if n := strings.Count(doc, "0.278（18 项中 5 项已投放）"); n != 2 {
		t.Errorf("两扇门应各报 0.278（18 项中 5 项已投放），实际出现 %d 次", n)
	}
}

// TestMarkdownNeverShowsRouteAggregateAvailable 固化并集口径已被否决：
// 多门路径的主表可用列只指路到端点细分，绝不出现 8/18 = 0.444 的并集分。
func TestMarkdownNeverShowsRouteAggregateAvailable(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	doc := m.Markdown()

	if strings.Contains(doc, "0.444") {
		t.Fatal("文档不得出现并集分 0.444——两门并集不对应任何一扇真实存在的门")
	}
	if !strings.Contains(doc, "见端点细分") {
		t.Fatal("多门路径的主表可用列应为「见端点细分」")
	}
}
```

运行：

```bash
go test ./internal/degrade/ -run 'TestMarkdownShowsEndpointBreakdown|TestMarkdownNeverShowsRouteAggregateAvailable' -v
```

预期：FAIL（端点细分小节尚不存在）。正确的 RED。

- [ ] **步骤 5.11：`markdown.go` 端点细分小节与文案**

(a) `writePreservation` 中「**上表每一格的处置都是设计事实，不是投放承诺。**」段落里，「当前只投放了文本生成那一个」改为「当前投放了文本生成与多模态生成两个端点」。

(b) 在 `writePreservation` 末尾（主表循环之后、结尾 `b.WriteString("\n")` 之前）调用新小节：

```go
	m.writeEndpointBreakdown(b)
```

(c) 新增方法：

```go
// writeEndpointBreakdown 输出端点细分：可用列永远端点相对。
//
// 不存在路径级「当前可用」聚合分——各门兑现集合的并集不对应任何一扇真实
// 存在的门，给不存在的东西记分正是矩阵要防的过度承诺。
func (m *Matrix) writeEndpointBreakdown(b *strings.Builder) {
	var any bool
	for _, r := range m.Routes() {
		if len(r.Endpoints()) > 0 {
			any = true
			break
		}
	}
	if !any {
		return
	}

	b.WriteString("### 端点细分\n\n")
	b.WriteString("「当前可用」一律端点相对：**不存在**路径级「当前可用」聚合分。" +
		"各门兑现集合的并集不对应任何一扇真实存在的门，" +
		"给不存在的东西记分正是矩阵要防的过度承诺。\n\n")
	b.WriteString("| 入站 | 出站 | 端点 | 已投放 | 当前可用 |\n")
	b.WriteString("|---|---|---|---|---:|\n")
	for _, r := range m.Routes() {
		for _, ep := range r.Endpoints() {
			caps := make([]string, 0, len(canonical.AllCapabilities()))
			for _, c := range r.RedeemedAt(ep) {
				caps = append(caps, string(c))
			}
			fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n",
				r.In, r.Out, ep, strings.Join(caps, ", "),
				formatAvailable(r.Preservation(m.avail, ep)))
		}
	}
}
```

（小节对全部已实现路径逐门列出：OpenAI 两条路径的独门也会出现，设计文档「预期生成效果」展示的是其中 Native 两行。Native 两行应为：`| dashscope.native | dashscope.native | /api/v1/services/aigc/text-generation/generation | text_generation, streaming, tool_calling, reasoning, web_search | 0.278（18 项中 5 项已投放） |` 与多模态门的对称行。）

运行：

```bash
go test ./internal/degrade/ -run 'TestMarkdown' -v
```

预期：两个 markdown 测试转绿。

- [ ] **步骤 5.12：`preference_test.go` 两门版本**

`TestAvailableScoreCountsOnlyRedeemed` 整体替换为：

```go
// TestAvailableScoreCountsOnlyRedeemed 防的是「设计满分被当成可用满分」。
//
// DashScope Native 的设计目标仍是 1.000——那条路最终该是零损失的同源直通，
// 这个结论不因为投放进度而改变。但两扇门此刻各投放了 18 项里的 5 项，
// 若可用分数也报 1.000，选路就会拿一个远超实际的分数去和别的路径比，
// 把请求送进 501。
func TestAvailableScoreCountsOnlyRedeemed(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	r := mustRoute(t, m, ProtoDashScopeNative, ProviderDashScopeNative)

	if got := r.Preservation(m.Availability(), Endpoint("")).DesignScore(); got != 1.0 {
		t.Errorf("设计目标应保持 1.000（同源直通零损失），实际 %.3f", got)
	}
	for ep, wantScore := range map[Endpoint]float64{
		EndpointDashScopeTextGeneration: 5.0 / 18.0,
		EndpointDashScopeMultimodal:     5.0 / 18.0,
	} {
		p := r.Preservation(m.Availability(), ep)
		if wantNotRedeemed := len(ExpressibleSet(ProtoDashScopeNative)) - 5; p.NotRedeemed != wantNotRedeemed {
			t.Errorf("门 %s 未投放格子数 = %d，期望 %d", ep, p.NotRedeemed, wantNotRedeemed)
		}
		if p.AvailableScore() != wantScore {
			t.Errorf("门 %s 当前可用应为 %.3f（18 项中 5 项已投放），实际 %.3f",
				ep, wantScore, p.AvailableScore())
		}
		if !p.Gated() {
			t.Errorf("门 %s 存在未投放的格子时两列分数应当不同", ep)
		}
	}
}
```

- [ ] **步骤 5.13：重新生成矩阵文档**

```bash
make matrix-update
GIT_MASTER=1 git diff docs/degradation-matrix.md
```

人工审阅 diff，预期变化恰为三处：

1. Native 路径主表行「当前可用」列由 `0.278（18 项中 5 项已投放）` 变为 `见端点细分`；
2. 主表后新增「### 端点细分」小节：OpenAI 两门各一行（全量兑现），Native 两门各一行 `0.278（18 项中 5 项已投放）`，小节带固定说明句；
3. 「当前只投放了文本生成那一个」变为「当前投放了文本生成与多模态生成两个端点」。

出现其他变化即停下来排查。

### 5D. 收敛与提交

- [ ] **步骤 5.14：生成并审阅三份 golden**

```bash
make golden-update
```

逐份审阅 `testdata/routes/dashscope.native__dashscope.native/golden/` 下的 `multimodal-basic.txt`、`multimodal-streaming.txt`、`multimodal-video-frames.txt`：

- `multimodal-basic.txt`：`status: OK` + `Content-Type: application/json` + `---` + 与 fixture `response.body` 逐字节一致的信封（同源直通，响应原样回传）；无 `X-Omugw-Degraded` / `X-Omugw-Emulated` 头（全 PASSTHROUGH，无降级无模拟）。
- `multimodal-streaming.txt`：`status: OK` + `Content-Type: text/event-stream` + `---` + 三帧 `event: result` / `data: …`，data 与 fixture 录制的字符串逐字一致，末帧 `finish_reason=stop`。
- `multimodal-video-frames.txt`：形态同 basic，信封为视频用例的响应。

- [ ] **步骤 5.15：全量验证**

```bash
gofmt -l . && go vet ./... && go build ./... && go test ./...
make matrix
```

预期：全绿。重点确认：

- `TestDashScopeNativeRouteConformance` 六个用例（3 既有 + 3 新）全部通过——其严格断言证明上游收到的 method / path / headers / body 与 fixture 完全一致：多模态端点的 Path 注入生效、SSE 头未被篡改、模型名被改写为 upstream-model；
- `TestBuiltMux_DashScopeNativeFallback_WithUpstream` 通过（多模态 200、兜底指标剩 2 次）；
- 四个负例通过且上游零调用；
- `TestImplementedRoutesHaveFixtures` 通过（新门槛下：文本门 3 份证据、多模态门 3 份证据、全部 fixture 指向已开门）。

- [ ] **步骤 5.16：提交（原子投放）**

```bash
GIT_MASTER=1 git add internal/degrade/ internal/gateway/ \
  testdata/routes/dashscope.native__dashscope.native/ docs/degradation-matrix.md
GIT_MASTER=1 git status   # 核对文件清单与本任务一致
GIT_MASTER=1 git commit -m "投放多模态生成门：fixture、负例与端点级文档" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 6：变异测试——闸门必须咬人

**依赖**：任务 5（并集否决断言需要两门状态）。
**文件**：新建 `internal/degrade/endpoint_mutation_test.go`。

本任务的测试是**锁**：它们守护的行为在前面的任务里已经正确实现，首次运行即应 PASS。每个锁随后执行「咬人证明」：临时施加它所命名的变异 → 测试必须 FAIL → 还原 → 测试恢复 PASS。咬人证明不改任何提交——变异只存在于工作区，用 `GIT_MASTER=1 git checkout -- <file>` 还原。

- [ ] **步骤 6.1：写三个变异锁（首次运行即绿）**

新建 `internal/degrade/endpoint_mutation_test.go`：

```go
package degrade

import (
	"errors"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// TestEndpointGateActuallyBites 咬住「跨门并集」变异。
//
// tool_calling 在文本门已兑现，但带着它敲多模态门必须 501——
// 若 Check 退化成「路径上某扇门兑现过就放行」，请求就会被送进
// 一扇会返回 501 的门。这正是设计文档要纠正的 8/18 错误结论的运行时形态。
func TestEndpointGateActuallyBites(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.Check(Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeMultimodal},
		ProviderDashScopeNative,
		[]canonical.Capability{canonical.CapTextGeneration, canonical.CapToolCalling})
	if err == nil {
		t.Fatal("多模态门必须对 tool_calling 返回 501——即使文本门已兑现它；跨门并集是过度承诺")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassNotImplemented || cerr.HTTPStatus() != 501 {
		t.Errorf("应为 not_implemented/501（等实现），实际 %q/%d", cerr.Class, cerr.HTTPStatus())
	}
	if !strings.Contains(cerr.Message, string(canonical.CapToolCalling)) ||
		!strings.Contains(cerr.Message, string(EndpointDashScopeMultimodal)) {
		t.Errorf("错误应同时点名能力与端点: %s", cerr.Message)
	}
}

// TestEmptyCapabilitiesStillFailAtUnopenedEndpoint 咬住「空能力集豁免端点闸门」变异。
//
// 请求带了什么能力是一回事，敲的门开没开是另一回事；后者是入口约束，
// 不因 caps 为空而豁免。
func TestEmptyCapabilitiesStillFailAtUnopenedEndpoint(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.Check(Inbound{
		Protocol: ProtoDashScopeNative,
		Endpoint: Endpoint("/api/v1/services/aigc/never-opened"),
	}, ProviderDashScopeNative, nil)
	if err == nil {
		t.Fatal("空能力集打未开门也必须 501：入口约束不因 caps 为空而豁免")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassNotImplemented || cerr.HTTPStatus() != 501 {
		t.Errorf("应为 not_implemented/501，实际 %q/%d", cerr.Class, cerr.HTTPStatus())
	}
	if !strings.Contains(cerr.Message, "/api/v1/services/aigc/never-opened") {
		t.Errorf("错误消息应点名端点: %s", cerr.Message)
	}
}

// TestEndpointScoreIsNotRouteAggregate 咬住「路径级并集聚合分」变异。
//
// 两扇门各报 5/18；显式断言两者都不等于 8/18——
// 并集 {text_generation, streaming, tool_calling, reasoning, web_search}
// ∪ {text_generation, streaming, vision_input, audio_input, video_input} = 8 项，
// 没有任何一扇真实的门同时提供这 8 项。
func TestEndpointScoreIsNotRouteAggregate(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	r := mustRoute(t, m, ProtoDashScopeNative, ProviderDashScopeNative)
	text := r.Preservation(m.Availability(), EndpointDashScopeTextGeneration).AvailableScore()
	multimodal := r.Preservation(m.Availability(), EndpointDashScopeMultimodal).AvailableScore()

	const union = 8.0 / 18.0
	if text != 5.0/18.0 {
		t.Errorf("文本门可用分 = %.3f，期望 5/18 = %.3f", text, 5.0/18.0)
	}
	if multimodal != 5.0/18.0 {
		t.Errorf("多模态门可用分 = %.3f，期望 5/18 = %.3f", multimodal, 5.0/18.0)
	}
	if text == union || multimodal == union {
		t.Fatalf("并集口径（8/18 = %.3f）已被否决：两门兑现集合的并集不对应任何一扇真实存在的门，见设计文档「分数」一节", union)
	}
}
```

运行：

```bash
go test ./internal/degrade/ -run 'TestEndpointGateActuallyBites|TestEmptyCapabilitiesStillFailAtUnopenedEndpoint|TestEndpointScoreIsNotRouteAggregate' -v
```

预期：三个全部 PASS（锁守护的行为已在前面的任务落地）。

- [ ] **步骤 6.2：咬人证明——跨门并集变异**

临时编辑 `internal/degrade/matrix.go` 的 `Check`，把闸门 4d 的

```go
		if !r.Redeems(in.Endpoint, c) {
```

替换为（模拟「路径上某扇门兑现过就放行」的变异）：

```go
		redeemedSomewhere := false
		for _, e := range r.Endpoints() {
			if r.Redeems(e, c) {
				redeemedSomewhere = true
			}
		}
		if !redeemedSomewhere {
```

运行：

```bash
go test ./internal/degrade/ -run TestEndpointGateActuallyBites -v
go test ./internal/gateway/ -run 'TestMultimodalDoorRejectsReasoning|TestTextDoorRejectsVisionInput' -v
```

预期：`TestEndpointGateActuallyBites` **FAIL**（tool_calling 在文本门兑现过，被误放行）；`TestMultimodalDoorRejectsReasoning` 与 `TestTextDoorRejectsVisionInput` 同样 **FAIL**（reasoning / vision_input 在另一扇门兑现过，请求被误放行到上游）。

还原：

```bash
GIT_MASTER=1 git checkout -- internal/degrade/matrix.go
go test ./internal/degrade/ ./internal/gateway/
```

预期：恢复全绿。

- [ ] **步骤 6.3：咬人证明——空能力集豁免变异**

临时编辑 `internal/degrade/matrix.go`，在 `Check` 的闸门 3（`if !r.ImplementedAt(in.Endpoint)`）**之前**插入：

```go
	if len(caps) == 0 {
		return Verdict{}, nil
	}
```

运行：

```bash
go test ./internal/degrade/ -run 'TestEmptyCapabilitiesStillFailAtUnopenedEndpoint|TestCheckRejectsUnopenedEndpoint' -v
```

预期：`TestEmptyCapabilitiesStillFailAtUnopenedEndpoint` **FAIL**。还原并复绿：

```bash
GIT_MASTER=1 git checkout -- internal/degrade/matrix.go
go test ./internal/degrade/
```

- [ ] **步骤 6.4：咬人证明——并集分变异**

临时编辑 `internal/degrade/rules_phase1.go`，给多模态门的 `Redeem` 追加三项（模拟「把文本门的能力顺手记到多模态门」的过度承诺）：

```go
		Redeem(EndpointDashScopeMultimodal,
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapVisionInput,
			canonical.CapAudioInput,
			canonical.CapVideoInput,
			canonical.CapToolCalling,
			canonical.CapReasoning,
			canonical.CapWebSearch,
		).
```

运行：

```bash
go test ./internal/degrade/ -run 'TestEndpointScoreIsNotRouteAggregate|TestRedeemedCapabilitiesAreExplicit' -v
```

预期：`TestEndpointScoreIsNotRouteAggregate` **FAIL**（两门都变 8/18）；白名单测试同时 FAIL（多兑现）。还原并复绿：

```bash
GIT_MASTER=1 git checkout -- internal/degrade/rules_phase1.go
go test ./internal/degrade/
```

- [ ] **步骤 6.5：咬人证明——Build 校验与门推导**

对任务 3 / 4 引入的其余闸门逐个做咬人证明，方法同前（变异 → FAIL → `git checkout --` 还原 → 全绿）：

| 变异 | 施加位置（临时） | 必须 FAIL 的测试 |
|---|---|---|
| 删除零值端点校验 | `matrix.go` Build 中 `if ep == Endpoint("")` 整块 | `TestRedeemZeroEndpointFailsBuild` |
| 删除端点级 undeliverable 校验 | `matrix.go` Build 中 `for ep, caps := range r.redeemed` 整块 | `TestRedeemUndeliverableAtEndpointFailsBuild` |
| `Endpoints()` 不再过滤零兑现门 | `matrix.go` 中 `if len(caps) > 0` 改为无条件 append | `TestEndpointsDerivedFromRedemption` |
| fixture 门槛跳过路径对账 | `matrix_test.go` `checkRouteFixtures` 中「端点级 fixture 路径双向对账」整块 | `TestFixtureGateReconcilesEndpointPaths` |

每步运行对应测试确认 FAIL，再还原复绿：

```bash
GIT_MASTER=1 git checkout -- internal/degrade/matrix.go internal/degrade/matrix_test.go
go test ./internal/degrade/
```

（`TestDeriveDoesNotInheritRedemption` 是既有测试，在二元键下继续有效：派生路径不继承任何门的兑现——任务 3 已迁移，此处只需确认它仍在套件中通过，不需要新写。）

- [ ] **步骤 6.6：全量复绿后提交**

```bash
go test ./... && GIT_MASTER=1 git status   # 工作区除新测试文件外必须干净
GIT_MASTER=1 git add internal/degrade/endpoint_mutation_test.go
GIT_MASTER=1 git commit -m "新增端点级变异测试，逐一证明闸门咬人" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 7：知识库同步、全量验证、双轴评审、git 审计

**依赖**：任务 1–6 全部落地。
**文件**：`README.md`、根 `AGENTS.md`、`internal/degrade/AGENTS.md`、`internal/gateway/AGENTS.md`、`internal/protocol/AGENTS.md`。

- [ ] **步骤 7.1：README 状态块同步**

将 README.md 的状态引用块（「**状态：M1 进行中——OpenAI 族两条同源直通路径已转正，网关可用。**」起，至「三处都动过一遍，就很难是无意的。」止）整体替换为：

```markdown
> **状态：M1 进行中——OpenAI 族两条同源直通路径与 DashScope Native 两个端点已转正，网关可用。**
>
> 降级矩阵登记了 14 条转换路径，**其中 3 条已实现**：
> `openai.responses → openai.compat`、`openai.chat → openai.compat` 与
> `dashscope.native → dashscope.native`（均为同源直通）。
> 其中 DashScope Native 路径投放了文本生成与多模态生成两个端点，
> 每扇门各兑现 5 项能力（多模态门为视觉 / 音频 / 视频输入加文本与流式）。
> 其余仍是 `PLANNED`，
> 打过去会得到 `501`——那是「还没建好」，不是「不支持」。
>
> 转正的门槛不是「有人认为写完了」，而是端到端 fixture 通过，
> 见 [ADR-0001](docs/adr/0001-declarations-must-be-redeemed-by-fixtures.md)。
> 已实现路径的清单登记在 `TestImplementedRoutesAreExplicit` 里，
> 门 × 能力的兑现清单登记在 `TestRedeemedCapabilitiesAreExplicit` 里，
> 每次转正都得同时改代码、写 fixture、改名单——三处都动过一遍，就很难是无意的。
```

- [ ] **步骤 7.2：根 AGENTS.md 同步**

(a) NOTES 的「**当前状态**」条目替换为：

```markdown
- **当前状态**：14 条路径已登记，3 条已通车（`openai.responses →
  openai.compat`、`openai.chat → openai.compat`、`dashscope.native →
  dashscope.native`，均为同源直通）。兑现粒度是**端点 × 能力**：OpenAI 两条
  直通在各自的唯一门上兑现全部可表达能力；DashScope Native 投放了两个端点——
  文本生成（text_generation / streaming / tool_calling / reasoning / web_search）
  与多模态生成（text_generation / streaming / vision_input / audio_input /
  video_input），每门 18 项中 5 项，不存在路径级可用聚合分。其余 Native POST
  端点由 `POST /api/v1/` 兜底返回 DashScope 协议化 501。其余 11 条路径打过去
  仍是 501。
```

(b) CONVENTIONS 的「**转正按能力粒度兑现**（ADR-0001）」条目替换为：

```markdown
- **转正按端点 × 能力粒度兑现**（ADR-0001）：`Route.Redeem(ep, ...)` 显式列出
  该门**当前已投放**的能力，前提是该端点的端到端 fixture 已通过（fixture 门槛
  按 `request.path` 与门清单双向对账）。仍需三处同改：代码 +
  `testdata/routes/<in>__<out>/` fixture + `matrix_test.go` 的两份名单
  （`TestImplementedRoutesAreExplicit` 管路径、`TestRedeemedCapabilitiesAreExplicit`
  管「路径 @ 端点」能力）。少一处就过不了 CI。但 CI 只查到门级集合与 fixture
  存在；**逐项能力跑没跑通，靠改白名单的人负责**。
```

(c) ANTI-PATTERNS 追加一条：

```markdown
- **不要**计算或展示路径级「当前可用」聚合分：各门兑现集合的并集不对应任何
  一扇真实存在的门（两门并集 8/18 已被显式否决）；可用列一律端点相对
  （`Preservation(avail, ep)`），绝不从 `Preservation(avail, Endpoint(""))` 取可用列。
```

(d) CODE MAP 表中行号会因迁移漂移。对下列每一行，用 `grep -n` 重新定位后更新（符号名不变，只改「文件:行号」）：

```bash
grep -n 'func (m \*Matrix) Check' internal/degrade/matrix.go
grep -n 'func (m \*Matrix) BestOutbound' internal/degrade/preference.go
grep -n 'func (r \*Route) Build' internal/degrade/matrix.go
grep -n 'func (r \*Route) Redeem' internal/degrade/matrix.go
grep -n 'func (r \*Route) Endpoints' internal/degrade/matrix.go
grep -n 'type Capability string' internal/canonical/capability.go
grep -n 'func (h \*Handler) dispatch' internal/gateway/handler.go
```

并追加一行（行号以 grep 结果为准）：

```markdown
| `degrade.Route.Endpoints` | Method | `internal/degrade/matrix.go:<行号>` | 已开门清单，字典序；门的存在只从兑现格子推导 |
```

同时把 `degrade.Route.Redeem` 行的「角色」更新为「登记指定端点已投放的能力；`Redeems(ep,c)` / `RedeemedAt(ep)` / `ImplementedAt(ep)` 由它派生」。

- [ ] **步骤 7.3：包级 AGENTS.md 同步**

(a) `internal/degrade/AGENTS.md`：

- STRUCTURE 表追加一行：`endpoint.go` ｜ `Endpoint` / `Inbound` 类型与四扇门常量（DashScope 门复用协议包路径常量）。
- WHERE TO LOOK：「路径转正 / 能力投放」改为 `Route.Redeem(ep, caps...)`；「查某能力是否已投放」改为 `Redeems(ep, c)`，并补 `ImplementedAt(ep)` / `Endpoints()`；追加「端点级 fixture 证据」→ `matrix_test.go` 的 `checkRouteFixtures`（按 request.path 双向对账）。
- CONVENTIONS 追加：兑现挂在端点上；门的存在只从兑现格子推导（`Endpoints()`），没有独立的门注册机制；不存在路径级可用聚合分。
- GOTCHAS 追加：`Check` 打未开门 → 501 且消息点名端点，空能力集也不豁免；`Preservation(avail, Endpoint(""))` 的可用列恒为零，只可取设计列。
- ANTI-PATTERNS 追加：不要用两门兑现集合的并集记「路径可用分」——并集不对应任何一扇真实存在的门。

(b) `internal/gateway/AGENTS.md`：

- STRUCTURE 的 `build.go` 行更新为「Mux 注册先落四个精确端点（OpenAI 两门 + Native 文本 / 多模态两门，Native 两门复用同一 handler 实例），再挂 DashScope Native 命名空间的两条兜底……；启动期对矩阵兑现与 Mux 注册做双向对账，任一方向失败即启动失败」。
- REQUEST PATH 段落更新：未投放的 DashScope Native 端点在 Mux 层被兜底拦下；精确注册的文本生成与多模态生成端点凭最长前缀优先命中同一 Native Handler。

(c) `internal/protocol/AGENTS.md`：`dashscopenative/` 行中「`wire.go` 声明 `NamespacePrefix` 与 `TextGenerationPath`」改为「`wire.go` 声明 `NamespacePrefix`、`TextGenerationPath` 与 `MultimodalGenerationPath`」。

- [ ] **步骤 7.4：手工 HTTP QA（离线，不需要真实凭据）**

用一个本地假上游 + 一份临时配置，亲手敲一遍两扇门与负例。全部命令照抄即可：

(a) 启动假上游（任意终端，保持运行）：

```bash
python3 - <<'EOF'
from http.server import HTTPServer, BaseHTTPRequestHandler

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = ('{"output":{"choices":[{"finish_reason":"stop",'
                '"message":{"role":"assistant","content":[{"text":"qa-ok"}]}}]},'
                '"usage":{"input_tokens":1,"output_tokens":1},"request_id":"qa"}').encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

HTTPServer(("127.0.0.1", 9901), H).serve_forever()
EOF
```

(b) 写临时配置 `/tmp/omugw-qa.yaml`：

```yaml
server:
  addr: 127.0.0.1:18080
  metrics_addr: 127.0.0.1:18081
log:
  level: info
  format: text
timeouts:
  connect: 2s
  first_byte: 5s
  total: 30s
  idle: 30s
limits:
  max_request_bytes: 10485760
  max_inline_bytes: 5242880
auth:
  keys:
    - id: qa
      key: qa-key-0123456789
credentials:
  qa-pool:
    - id: c1
      secret: upstream-secret
providers:
  - endpoint: ds-native
    kind: dashscope.native
    base_url: http://127.0.0.1:9901
    credential_pool: qa-pool
models:
  - match: logical-omni
    targets:
      - endpoint: ds-native
        upstream_model: qwen-omni-turbo
```

(c) 启动网关并检查启动日志：

```bash
go run ./cmd/omugw -config /tmp/omugw-qa.yaml
```

预期日志（A2 的活体检查）：每条路径一条「已注册转换路径」（**不含** available_score）；另有四条「已投放端点」条目——OpenAI 两门各一条、Native 文本门与多模态门各一条，各带 `available_score`（Native 两门均约 0.278）。

(d) 另开终端，逐一敲五个请求：

```bash
# 1. 文本门，纯文本 → 200，假上游信封
curl -s -X POST http://127.0.0.1:18080/api/v1/services/aigc/text-generation/generation \
  -H "Authorization: Bearer qa-key-0123456789" -H "Content-Type: application/json" \
  -d '{"model":"logical-omni","input":{"messages":[{"role":"user","content":"你好"}]}}'

# 2. 多模态门，image 内容块 → 200，假上游信封
curl -s -X POST http://127.0.0.1:18080/api/v1/services/aigc/multimodal-generation/generation \
  -H "Authorization: Bearer qa-key-0123456789" -H "Content-Type: application/json" \
  -d '{"model":"logical-omni","input":{"messages":[{"role":"user","content":[{"text":"图里有什么？"},{"image":"https://example.com/x.png"}]}]}}'

# 3. 多模态门 + enable_thinking → 501，DashScope 扁平信封，消息点名 reasoning 与端点
curl -s -X POST http://127.0.0.1:18080/api/v1/services/aigc/multimodal-generation/generation \
  -H "Authorization: Bearer qa-key-0123456789" -H "Content-Type: application/json" \
  -d '{"model":"logical-omni","input":{"messages":[{"role":"user","content":[{"text":"想一想"}]}]},"parameters":{"enable_thinking":true}}'

# 4. 文本门 + image 内容块 → 501，消息点名 vision_input 与端点
curl -s -X POST http://127.0.0.1:18080/api/v1/services/aigc/text-generation/generation \
  -H "Authorization: Bearer qa-key-0123456789" -H "Content-Type: application/json" \
  -d '{"model":"logical-omni","input":{"messages":[{"role":"user","content":[{"text":"看图"},{"image":"https://example.com/x.png"}]}]}}'

# 5. 未登记端点 → Mux 兜底协议化 501（「DashScope Native 端点 … 尚未实现」）
curl -s -X POST http://127.0.0.1:18080/api/v1/services/aigc/other/generation \
  -H "Authorization: Bearer qa-key-0123456789" -H "Content-Type: application/json" \
  -d '{"model":"logical-omni"}'
```

(e) 关闭网关与假上游，删除临时配置：`rm /tmp/omugw-qa.yaml`。

- [ ] **步骤 7.5：全量验证**

```bash
make check       # fmt-check + vet + test + matrix
make test-race
```

预期：全部通过，零失败。

- [ ] **步骤 7.6：双轴评审（固定基线 `237636a`）**

以设计文档所在提交 `237636a` 为基线评审本次全部变更（对应 code-review 技能的两个轴）：

```bash
GIT_MASTER=1 git diff 237636a..HEAD --stat
GIT_MASTER=1 git log --oneline 237636a..HEAD
```

- **轴一（Standards）**：逐文件核对仓库规范——注释中文且写「防的是什么」；矩阵是唯一权威，未登记组合拒绝；采样指针、错误分类、首字节后不重试等既有纪律未被迁移破坏；新增代码无 `TODO` / `TBD`。
- **轴二（Spec）**：对照设计文档「完成标准」九项逐条打勾（见下表「设计文档覆盖对照」），并核对「非目标」与「复核」两节没有被顺手实现或违背（例如 `file_input` 未被兑现、多模态输出能力未出现、没有路径级聚合分）。

评审发现的每个真实问题单独修复并单独提交（提交信息描述修复内容，同样带署名脚注），修复后重跑步骤 7.5。

- [ ] **步骤 7.7：git 审计**

```bash
GIT_MASTER=1 git status                                   # 工作区干净
GIT_MASTER=1 git log --oneline 237636a..HEAD              # 任务 1–7 提交按序在列
GIT_MASTER=1 git grep -nI 'TODO\|TBD\|FIXME' -- internal/ cmd/ testdata/ docs/superpowers/plans/   # 无新增
GIT_MASTER=1 git diff 237636a..HEAD | grep -inE 'api[_-]?key|secret|bearer'   # 只应出现测试常量与 fixture 的 <redacted>
```

确认：无意外文件、无新增占位符、无真实凭据泄漏、分支仍领先 origin 且**未推送**。

- [ ] **步骤 7.8：提交知识库同步**

```bash
GIT_MASTER=1 git add README.md AGENTS.md internal/degrade/AGENTS.md internal/gateway/AGENTS.md internal/protocol/AGENTS.md
GIT_MASTER=1 git status
GIT_MASTER=1 git commit -m "同步 README 与知识库：端点级兑现与多模态门" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 设计文档覆盖对照（自检表）

| 设计文档章节 / 完成标准 | 落地任务 |
|---|---|
| 端点常量与登记（`MultimodalGenerationPath`、四扇门常量、依赖方向） | 任务 1、2 |
| `Endpoint` / `Inbound` 类型与零值语义 | 任务 2（类型）、3（零值 Build 校验与闸门行为） |
| Route API：`Redeem(ep,…)` / `Redeems(ep,c)` / `RedeemedAt(ep)` / `Endpoints()` / `ImplementedAt(ep)` / `Implemented()` / `Preservation(avail, ep)`，路径级 redeemed 删除 | 任务 3 |
| Build 两项新校验（`redeem on zero endpoint`、`endpoint %q redeemed but not deliverable: %s`） | 任务 3 |
| `Check(Inbound, out, caps)` 闸门表（含空能力集不豁免、4b 先于 4d） | 任务 3 |
| `BestOutbound(Inbound, …)` 端点无感候选筛选；两类空结果错误维持现状 | 任务 3 |
| `Derive` 不继承兑现 | 任务 3（既有测试二元键迁移） |
| 兑现集合：OpenAI 各一门全量；Native 文本门 5 项 + 多模态门 5 项；11 条 PLANNED 无门 | 任务 3（前三）、5（多模态门） |
| 分数：两门各 5/18，显式拒绝 8/18 并集 | 任务 3（Preservation）、5（文档）、6（否决断言） |
| 请求流程：handler 传 `Inbound`，`upstreamPath` 随请求走 | 任务 3 |
| Mux 注册与启动期双向对账 | 任务 3（对账 + 三门）、5（多模态门） |
| 三份新 fixture 及其证明目标 | 任务 5 |
| 端点级 fixture 路径门槛（双向） | 任务 4（A1 本地解析） |
| 白名单按 `in -> out @ endpoint` 对账 | 任务 3（迁移）、5（多模态键） |
| 同步 + SSE 测试矩阵 11 个用例 | 用例 1–6：任务 5（conformance 回放）；7 / 8 / 9 / 11：任务 5 负例；10：既有 build_test 覆盖 |
| `TestUnredeemedCapabilityIsRejectedAtRuntime` 双向断言 | 任务 5 |
| 变异测试表（含 8/18 否决、空能力集闸门、上游零调用） | 任务 6（锁）+ 任务 5（负例零调用断言） |
| 生成文档：主表「见端点细分」、端点细分小节、固定说明句、文案更新 | 任务 5 |
| 分数对账测试迁移（`TestRuntimeRankingUsesAvailableScore` / `TestAvailableScoreCountsOnlyRedeemed` / `TestPreferenceMatchesPreservation`） | 任务 3、5 |
| 迁移兼容：配置零变化、运行时可见变化仅多模态端点、指标口径不变 | 任务 3（行为保持命题）、5（build_test 翻转） |
| 完成标准 9：`make check` 全绿 + `make test-race` | 任务 7 |
| README 状态块与 AGENTS.md 知识库同步 | 任务 7 |
| A1 / A2 / A3 三个决策 | A1：任务 4；A2：任务 3（main.go）；A3：任务 3 起贯穿（markdown / main / preference_test 注释） |

**计划结束。** 执行完毕后，分支应领先 origin 九个本地提交（两个既有文档提交 + 七个任务提交），工作区干净，未推送。
