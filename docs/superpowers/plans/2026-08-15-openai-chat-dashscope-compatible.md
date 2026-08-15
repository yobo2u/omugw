# OpenAI Chat → DashScope Compatible 路径投放实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 投放 `openai.chat → dashscope.compatible` 异构路径：在 `/v1/chat/completions` 门上显式兑现 9 项可交付能力（7 PASS + 2 DEGRADE），`file_input` / `audio_output` 维持 REJECT 422，路径设计分与门可用分均为 8/11 ≈ 0.727，全程不新增 Canonical 出站编码器、不标记同源。

**Architecture:** 入站仍由 `openaichat.Decode` 严格解码为 Canonical，解码器新增两项 OpenAI 私有开关的能力识别（`web_search_options` 非 null 对象 → `CapWebSearch`；`parallel_tool_calls: true` → `CapParallelToolCalls`），识别不经过 Extensions。出站新建 `internal/provider/dashscopecompat` 包：以客户端原始 JSON 为基准只做两处定点修补（改写 `model`；非 null 的 `web_search_options` 整体删除并写入 `enable_search: true`），其余字段一律保留原始 `json.RawMessage` 字节，响应原样回传。矩阵在 `EndpointOpenAIChat` 上以 `Redeem` 兑现九项能力，两份显式白名单与生成文档同步更新。fixture 新增可选的 `upstream` 断言（method / path / body 三者必填），九份新 fixture 全部携带，conformance 回放独立于 golden 逐项断言上游实际收到的请求与降级头。

**Tech Stack:** Go 1.25（仅现有三个直接依赖，不新增），标准库 `testing` + `internal/testkit` 离线回放 + golden，Make 目标（`test` / `test-race` / `matrix` / `matrix-update` / `check`），手工 QA 用本地 `go run` 假上游 + 真实二进制，不接触任何真实凭据。

**Spec:** `docs/superpowers/specs/2026-08-15-openai-chat-dashscope-compatible-design.md`（批准提交 `5931c9f`，下文简称「设计文档」）。本计划是它的决策完整展开；执行中若发现计划与设计文档冲突，以设计文档为准并停下报告，不得自行改设计。

---

## 前置约定（每个任务都适用）

1. **语言与注释**：代码注释与文档一律中文，写「防的是什么」而不是「做了什么」，沿用仓库既有风格（参见 `internal/provider/passthrough/passthrough.go` 与 `internal/degrade/matrix.go` 现有注释）。
2. **严格 TDD**：先写失败测试（RED），确认它因正确的原因失败（符号未定义、断言不满足，而不是拼写错误），再写最小实现（GREEN）。命令与预期输出在每个步骤里给出。纯生成物任务（golden）与纯数据任务（fixture）按各自的验证方式执行。
3. **每个提交必须绿**：任何提交落地前，`go build ./...` 与 `go test ./...` 必须通过（除非该步骤明确标注 RED 中间态且同一任务内收敛）。
4. **提交纪律**：每条 `git` 命令以 `GIT_MASTER=1` 前缀执行。每个提交带 Sisyphus 署名脚注与 Co-authored-by trailer，模板：

   ```bash
   GIT_MASTER=1 git add <files>
   GIT_MASTER=1 git commit -m "<任务给出的提交信息>" \
     -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
     -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
   ```

5. **不许推送**：全程不得 `git push`。分支 `main` 已领先 origin 一个设计提交（`5931c9f`），保持本地状态。
6. **golden 纪律**：重写 golden 后必须逐份人工审阅 diff，确认内容正是预期语义，再提交。本计划只允许用定向命令重写本路径的九份 golden，不得 `make golden-update` 全量重写。
7. **范围纪律**：不做设计文档「非目标」一节列出的任何事——不碰 Responses 到 dashscope.compatible、不碰 Native 转换、不碰 WebSocket、不给 Canonical 加字段、不伪造搜索来源、不引入真实凭据。

## 十二个已采纳的绑定决策（执行中不得推翻）

1. **搜索开关语义**：`web_search_options: {}` 表示开启搜索；字段缺省或显式 `null` 表示关闭。
2. **严格嵌套解码**：新增 `WebSearchOptions` / `UserLocation` / `ApproximateLocation` 三层结构体严格解码；该对象在出站前被整体删除，任何未建模的子字段必须在入站以 400 拒绝，不得静默吞掉。
3. **能力识别落在解码器**：`Decoded` 新增两个私有布尔（`webSearch` / `parallelToolCalls`），由 `Capabilities()` 报告 `CapWebSearch` 与「仅显式 true」的 `CapParallelToolCalls`，输出保持 `AllCapabilities()` 顺序；能力识别不得依赖 Extensions（那是同源快通道专属的原样回填通道）。
4. **出站适配器形态**：新建 `internal/provider/dashscopecompat` 包，原始 JSON 修补器模型——`map[string]json.RawMessage` 上只改 `model` 与 `web_search_options → enable_search` 两处，其余字段保留原始字节；不写 Canonical 出站编码器；路径不调用 `MarkHomogeneous`。
5. **fixture 上游断言**：`testkit.Fixture` 新增可选声明字段 `Upstream *UpstreamExpectation`（method / path / body 三者必填才算合法）；九份新异构 fixture 全部必须携带，conformance 回放对缺失者直接失败。
6. **harness 工厂注入**：网关测试 harness 改为注入 Provider 工厂（不再硬编码 passthrough）；chat 入站的「未实现哨兵」在兑现前从 `ProviderDashScopeCompatible` 改指 `ProviderAnthropicMessages`（后者在 Phase 1 永远是 PLANNED）。
7. **Build 装配**：`gateway.Build` 的协议族 switch 接入 `ProviderDashScopeCompatible`，未实现协议族的启动错误名单同步更新为三个已实现族。
8. **九份 fixture**：恰好九份——`basic` / `streaming` / `tool_calling` / `parallel_tool_calls` / `structured_output` / `reasoning` / `vision_input` / `audio_input` / `web_search`，全部带 `upstream` 断言；`structured_output.json` 与 `web_search.json` 是 ADR-0001 门槛点名的 DEGRADE 举证文件名。
9. **兑现集合精确为九**：`Redeem(EndpointOpenAIChat, …)` 恰好九项可交付能力；`file_input` / `audio_output` 是 REJECT，不在兑现之列。同步更新 `TestImplementedRoutesAreExplicit`、`TestRedeemedCapabilitiesAreExplicit` 两份白名单，新增非同源与同门选路的聚焦断言，并重新生成矩阵文档。
10. **聚焦测试文件**：新增独立的 conformance 与负例测试文件（不改写已经很大的 `conformance_test.go`）；降级头与上游请求断言独立于 golden；422 / 501 负例必须断言上游零调用。
11. **知识库同步**：README、根 AGENTS、相关包 AGENTS 的实现状态与语义边界（wire-compatible ≠ 同源）同步更新。
12. **最终闸门**：gofmt / vet / build / test / race / matrix / check / LSP 诊断 / 文件大小复查全部通过，并用真实二进制 + 本地假上游跑六探针手工 QA；不接触真实凭据。

## 关键路径与 ADR-0001 窗口

任务依赖是线性的：**任务 1 → 2 → 3 → 4 → 5 → 6 → 7 → 8 → 9 → 10 → 11 → 12 → 13 → 14 → 15**。

- 任务 1 的 `Upstream` 字段被任务 6-8 的 fixture 与任务 13 的回放使用；任务 2 的能力识别被任务 9 的兑现依赖（`CapWebSearch` / `CapParallelToolCalls` 不报告，矩阵就会在运行时拦下对应 fixture）；任务 3 的适配器被任务 4 的工厂与任务 5 的 Build 引用；任务 6-8 的 fixture 必须在任务 9 兑现之前落地（ADR-0001：先有证据，后有兑现）。
- **任务 9 是 ADR-0001 窗口打开的时刻**：`Redeem` 一落地，这条路径在矩阵里就是「已兑现」，而证明兑现的 conformance 回放到任务 13 才提交。**从任务 9 开始到任务 13 全绿为止，不得中途停下**——中间任何时刻的已提交状态虽然是绿的（fixture 与白名单都已就位），但兑现的合法性靠的是随后立即落地的回放证据。
- 任务 10-12 的 golden 由任务 13 的测试文件生成：工作流是「先在工作树写好两个测试文件（不提交）→ 定向 `-update` 生成九份 golden → 审阅 → 先提交 golden（任务 10-12）→ 再提交测试文件（任务 13）」。golden 先于测试文件提交不影响任何已提交状态的绿（golden 是纯数据，没有测试引用它们时不参与构建）。

## 范围与文件地图

| 文件 | 动作 | 责任 | 任务 |
|---|---|---|---|
| `internal/testkit/fixture.go` | 修改 | `Fixture.Upstream` + `UpstreamExpectation` + `Validate` 校验 | 1 |
| `internal/testkit/testkit_test.go` | 修改 | 上游断言的合法性测试 | 1 |
| `internal/testkit/AGENTS.md` | 修改 | 新增「异构路径 fixture 必须带 upstream 断言」约定 | 1 |
| `internal/protocol/openaichat/wire.go` | 修改 | `Request.WebSearchOptions` 字段 + 三层嵌套结构体 | 2 |
| `internal/protocol/openaichat/decode.go` | 修改 | `Decoded` 私有布尔、严格嵌套解码、`Capabilities()` 扩展 | 2 |
| `internal/protocol/openaichat/decode_test.go` | 修改 | 能力识别与严格解码测试 | 2 |
| `internal/provider/dashscopecompat/dashscopecompat.go` | 新建 | wire-compatible 出站适配器（定点修补） | 3 |
| `internal/provider/dashscopecompat/dashscopecompat_test.go` | 新建 | 适配器单测 | 3 |
| `internal/gateway/gateway_test.go` | 修改 | Provider 工厂注入、chat 哨兵改指 Anthropic、`newChatDSCompatHarness` | 4 |
| `internal/gateway/build.go` | 修改 | 装配 `dashscopecompat`、更新未实现族名单 | 5 |
| `internal/gateway/build_test.go` | 修改 | 装配成功与错误名单测试 | 5 |
| `internal/gateway/AGENTS.md` | 修改 | build.go 职责行补三类适配器装配 | 5 |
| `testdata/routes/openai.chat__dashscope.compatible/{basic,streaming,tool_calling}.json` | 新建 | 第一组 fixture（含 upstream 断言） | 6 |
| `testdata/routes/openai.chat__dashscope.compatible/{parallel_tool_calls,structured_output,reasoning}.json` | 新建 | 第二组 fixture | 7 |
| `testdata/routes/openai.chat__dashscope.compatible/{vision_input,audio_input,web_search}.json` | 新建 | 第三组 fixture | 8 |
| `internal/degrade/rules_phase1.go` | 修改 | 搜索降级 note 收窄 + `Redeem(EndpointOpenAIChat, 九项)` | 9 |
| `internal/degrade/matrix_test.go` | 修改 | 两份显式白名单 | 9 |
| `internal/degrade/chat_dscompat_test.go` | 新建 | 非同源身份、兑现集合形状、同门选路断言 | 9 |
| `docs/degradation-matrix.md` | 生成 | `make matrix-update` 重新生成（禁手改） | 9 |
| `testdata/routes/openai.chat__dashscope.compatible/golden/*.txt`（九份） | 生成 | 定向 `-update` 生成后人工审阅 | 10-12 |
| `internal/gateway/chat_dscompat_conformance_test.go` | 新建 | 九份 fixture 回放 + 上游请求与降级头独立断言 | 13 |
| `internal/gateway/chat_dscompat_negative_test.go` | 新建 | 422 / 501 / 400 负例，上游零调用 | 13 |
| `README.md`、根 `AGENTS.md` | 修改 | 实现状态与语义边界同步 | 14 |

## 提交总览（14 个提交，全部本地，不推送）

| # | 提交信息（中文 plain，仓库风格） | 文件 | 任务 |
|---|---|---|---|
| 1 | `testkit 增加 fixture 上游断言：异构路径不得只比客户端响应` | fixture.go、testkit_test.go、testkit/AGENTS.md | 1 |
| 2 | `Chat 解码报告搜索与并行工具能力：识别不再依赖 Extensions` | wire.go、decode.go、decode_test.go | 2 |
| 3 | `DashScope Compatible 出站适配器：原始 JSON 定点修补，不经 Canonical` | dashscopecompat/（两文件） | 3 |
| 4 | `网关测试 harness 注入 Provider 工厂：Chat 未实现哨兵改指 Anthropic` | gateway_test.go | 4 |
| 5 | `Build 装配 dashscope.compatible：未实现协议族名单同步更新` | build.go、build_test.go、gateway/AGENTS.md | 5 |
| 6 | `Chat 到 DashScope Compatible 路径 fixture：基础、流式与工具调用` | basic/streaming/tool_calling 三份 | 6 |
| 7 | `Chat 到 DashScope Compatible 路径 fixture：并行工具、结构化输出与推理` | parallel_tool_calls/structured_output/reasoning 三份 | 7 |
| 8 | `Chat 到 DashScope Compatible 路径 fixture：视觉、音频与联网搜索` | vision_input/audio_input/web_search 三份 | 8 |
| 9 | `兑现 openai.chat 到 dashscope.compatible 九项能力：白名单与矩阵文档同步` | rules_phase1.go、matrix_test.go、chat_dscompat_test.go、degradation-matrix.md | 9 |
| 10 | `Chat 到 DashScope Compatible golden：基础、流式与工具调用` | golden 三份 | 10 |
| 11 | `Chat 到 DashScope Compatible golden：并行工具、结构化输出与推理` | golden 三份 | 11 |
| 12 | `Chat 到 DashScope Compatible golden：视觉、音频与联网搜索` | golden 三份 | 12 |
| 13 | `Chat 到 DashScope Compatible 一致性回放与负例：上游请求逐条对账` | 两个新测试文件 | 13 |
| 14 | `知识库跟上第四条通车路径：wire-compatible 不是同源快通道` | README.md、根 AGENTS.md | 14 |

提交 9 含四个文件的理由：`Redeem` 与两份白名单由 CI 互相校验（只改一侧，`TestImplementedRoutesAreExplicit` / `TestRedeemedCapabilitiesAreExplicit` 立即失败），矩阵文档由 `TestDegradationMatrixDocIsCurrent` 强制与代码同步，聚焦断言文件锁的正是这次兑现的形状——拆开提交会让任一中间提交测试红。提交 14 含两个文件的理由：README 与根知识库描述的是同一个实现状态，分开更新会让两处状态在窗口内互相矛盾。其余提交均满足「文件数 ÷ 3」下限。

---

## 任务 1：testkit 上游断言（UpstreamExpectation）

**依赖**：无。
**文件**：修改 `internal/testkit/fixture.go`、`internal/testkit/testkit_test.go`、`internal/testkit/AGENTS.md`。

- [ ] **步骤 1.1：写失败测试（RED）**

在 `internal/testkit/testkit_test.go` 末尾追加：

```go
// TestFixtureUpstreamExpectationValidation 钉死上游断言的合法性规则。
//
// method / path / body 三者缺一，断言就退化成半截检查——异构路径的假绿
// （Provider 没做映射、fixture 仍返回成功）恰恰是从半截检查里长出来的。
func TestFixtureUpstreamExpectationValidation(t *testing.T) {
	base := Fixture{
		Name:     "t",
		Response: Response{Status: 200},
	}

	t.Run("三者齐备应通过", func(t *testing.T) {
		f := base
		f.Upstream = &UpstreamExpectation{
			Method: "POST",
			Path:   "/v1/chat/completions",
			Body:   json.RawMessage(`{"model":"m"}`),
		}
		if err := f.Validate(); err != nil {
			t.Errorf("应通过校验: %v", err)
		}
	})

	t.Run("缺 method", func(t *testing.T) {
		f := base
		f.Upstream = &UpstreamExpectation{Path: "/x", Body: json.RawMessage(`{}`)}
		err := f.Validate()
		if err == nil || !strings.Contains(err.Error(), "method") {
			t.Errorf("应报缺少 method: %v", err)
		}
	})

	t.Run("缺 path", func(t *testing.T) {
		f := base
		f.Upstream = &UpstreamExpectation{Method: "POST", Body: json.RawMessage(`{}`)}
		err := f.Validate()
		if err == nil || !strings.Contains(err.Error(), "path") {
			t.Errorf("应报缺少 path: %v", err)
		}
	})

	t.Run("缺 body", func(t *testing.T) {
		f := base
		f.Upstream = &UpstreamExpectation{Method: "POST", Path: "/x"}
		err := f.Validate()
		if err == nil || !strings.Contains(err.Error(), "body") {
			t.Errorf("应报缺少 body: %v", err)
		}
	})
}
```

运行：

```bash
go test ./internal/testkit/ -run TestFixtureUpstreamExpectationValidation -v
```

预期：**编译失败**，`undefined: UpstreamExpectation`——这是正确的 RED（符号尚不存在）。

- [ ] **步骤 1.2：实现字段与校验（GREEN）**

在 `internal/testkit/fixture.go` 的 `Fixture` 结构体中，`Response` 字段之后追加：

```go
	// Upstream 声明上游应当收到的请求。
	//
	// 同源直通 fixture 只比客户端响应就够了——输入输出同形；异构（含
	// wire-compatible）路径必须同时断言上游请求，否则「Provider 根本没做
	// 映射、fixture 仍返回成功」的假绿查不出来。
	Upstream *UpstreamExpectation `json:"upstream,omitempty"`
```

并在 `Response` 类型定义之后新增：

```go
// UpstreamExpectation 声明 fixture 期望上游收到的请求。
//
// Method / Path / Body 三者必填：缺一即断言退化成半截检查。Body 按语义比对
// （忽略键序与空白）——网关的重新序列化会改变键序，不改变语义。
type UpstreamExpectation struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body"`
}
```

在 `Validate()` 的 SSE frames 校验之后、`return nil` 之前追加：

```go
	if u := f.Upstream; u != nil {
		if u.Method == "" {
			return fmt.Errorf("fixture %q 的 upstream 断言缺少 method", f.Name)
		}
		if u.Path == "" {
			return fmt.Errorf("fixture %q 的 upstream 断言缺少 path", f.Name)
		}
		if len(u.Body) == 0 {
			return fmt.Errorf("fixture %q 的 upstream 断言缺少 body", f.Name)
		}
	}
```

运行：

```bash
go test ./internal/testkit/ -run TestFixtureUpstreamExpectationValidation -v
```

预期：PASS（四个子测试全过）。

- [ ] **步骤 1.3：更新 testkit 知识库**

在 `internal/testkit/AGENTS.md` 的 `## CONVENTIONS` 一节末尾追加一条：

```markdown
- 异构（含 wire-compatible）路径的 fixture 必须携带 `upstream` 断言，声明上游应收到
  的 method / path / body（三者必填）；conformance 回放逐项断言，body 按语义比对。
  只比客户端响应查不出「Provider 根本没做映射、fixture 仍返回成功」的假绿。
```

- [ ] **步骤 1.4：全量验证**

```bash
go build ./... && go test ./internal/testkit/ && gofmt -l internal/testkit/
```

预期：构建通过、测试通过、`gofmt -l` 无输出。

- [ ] **步骤 1.5：提交**

```bash
GIT_MASTER=1 git add internal/testkit/fixture.go internal/testkit/testkit_test.go internal/testkit/AGENTS.md
GIT_MASTER=1 git commit -m "testkit 增加 fixture 上游断言：异构路径不得只比客户端响应" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 2：Chat 解码报告搜索与并行工具能力

**依赖**：无（与任务 1 独立，顺序执行即可）。
**文件**：修改 `internal/protocol/openaichat/wire.go`、`decode.go`、`decode_test.go`。

背景：`openai.chat` 的可表达集包含 `CapWebSearch` 与 `CapParallelToolCalls`，矩阵两条 chat 出站路径也已对它们表态，但解码器从不报告——兑现前必须补齐，否则这两项能力的 fixture 在运行时会被矩阵以「未请求」放过、以「请求了」拦下，两头都不对。

- [ ] **步骤 2.1：写失败测试（RED）**

在 `internal/protocol/openaichat/decode_test.go` 末尾追加（复用已有的 `mustDecode` 与 `hasCap` 助手，不要重新定义）：

```go
// ——— 能力识别：web_search_options 与 parallel_tool_calls ———
//
// 这两项是 OpenAI 特有开关，Canonical 没有对应字段。识别必须在解码器完成：
// 异构出站路径读不得 Extensions，矩阵只信 Capabilities() 的报告。

func TestDecodeReportsWebSearch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra string
		want  bool
	}{
		{"空对象即开启", `,"web_search_options":{}`, true},
		{"带参数即开启", `,"web_search_options":{"search_context_size":"high",` +
			`"user_location":{"type":"approximate","approximate":{` +
			`"country":"CN","city":"上海","region":"上海市",` +
			`"timezone":"Asia/Shanghai"}}}}`, true},
		{"缺省为关闭", ``, false},
		{"null 为关闭", `,"web_search_options":null`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":"hi"}]` + tc.extra + `}`
			d, err := Decode([]byte(body))
			if err != nil {
				t.Fatalf("Decode 失败: %v", err)
			}
			if got := hasCap(d.Capabilities(), canonical.CapWebSearch); got != tc.want {
				t.Errorf("CapWebSearch = %v，期望 %v（caps=%v）", got, tc.want, d.Capabilities())
			}
		})
	}
}

func TestDecodeReportsParallelToolCallsOnlyWhenTrue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra string
		want  bool
	}{
		{"显式 true 报告", `,"parallel_tool_calls":true`, true},
		{"显式 false 不报告", `,"parallel_tool_calls":false`, false},
		{"缺省不报告", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
				`"tools":[{"type":"function","function":{"name":"f"}}]` + tc.extra + `}`
			d := mustDecode(t, body)
			if got := hasCap(d.Capabilities(), canonical.CapParallelToolCalls); got != tc.want {
				t.Errorf("CapParallelToolCalls = %v，期望 %v（caps=%v）", got, tc.want, d.Capabilities())
			}
		})
	}
}

// TestDecodeCapabilitiesKeepAllCapabilitiesOrder 钉死输出顺序：
// golden 与矩阵日志都依赖能力清单稳定，追加识别不得打乱 AllCapabilities 顺序。
func TestDecodeCapabilitiesKeepAllCapabilitiesOrder(t *testing.T) {
	d := mustDecode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],`+
		`"stream":true,"tools":[{"type":"function","function":{"name":"f"}}],`+
		`"parallel_tool_calls":true,"web_search_options":{}}`)

	want := []canonical.Capability{
		canonical.CapTextGeneration,
		canonical.CapStreaming,
		canonical.CapToolCalling,
		canonical.CapParallelToolCalls,
		canonical.CapWebSearch,
	}
	got := d.Capabilities()
	if len(got) != len(want) {
		t.Fatalf("能力数 = %d，期望 %d：%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 项 = %q，期望 %q（完整 %v）", i, got[i], want[i], got)
		}
	}
}

// TestDecodeRejectsUnknownWebSearchSubfield 钉死嵌套严格：
// 该对象在异构出站前被整体删除，未建模的子字段必须 400，不得静默吞掉。
func TestDecodeRejectsUnknownWebSearchSubfield(t *testing.T) {
	_, err := Decode([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}],
	  "web_search_options":{"mystery_field":1}}`))
	if err == nil {
		t.Fatal("未知子字段应当被拒绝")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassBadRequest {
		t.Errorf("错误分类应为 bad_request，实际为 %q", cerr.Class)
	}
}

// TestDecodeRejectsMalformedWebSearchOptions 形态非法一律 400，
// 不得默认为开启搜索。
func TestDecodeRejectsMalformedWebSearchOptions(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":"x"}],"web_search_options":"yes"}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"web_search_options":[1,2]}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"web_search_options":{"search_context_size":"ultra"}}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"web_search_options":{"user_location":{"type":"exact","approximate":{"city":"上海"}}}}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"web_search_options":{"user_location":{"type":"approximate"}}}`,
	} {
		if _, err := Decode([]byte(body)); err == nil {
			t.Errorf("形态非法的 web_search_options 应当被拒绝: %s", body)
		}
	}
}
```

`decode_test.go` 的 import 块需要补 `"errors"`（其余 `strings` / `testing` / `canonical` 已存在）。

运行：

```bash
go test ./internal/protocol/openaichat/ -v
```

预期：新增测试**失败**——`TestDecodeReportsWebSearch` 的「空对象即开启」「带参数即开启」两例报 `CapWebSearch = false`（解码器尚未识别）；`TestDecodeRejectsUnknownWebSearchSubfield` 报「未知子字段应当被拒绝」（顶层 `DisallowUnknownFields` 管不到 RawMessage 子树）。这是正确的 RED。已有的 `TestDecodeParallelToolCallsGoToExtensions` 等旧测试仍应通过。

- [ ] **步骤 2.2：wire 结构与字段（GREEN 第一步）**

在 `internal/protocol/openaichat/wire.go` 的 `Request` 结构体「其余常见参数」块末尾（`ServiceTier` 字段之后）追加：

```go
	// 内建搜索选项。出现且非 null（哪怕是 {}）即报告 CapWebSearch；
	// 子字段在 Decode 里严格校验——它在异构出站前被整体删除，
	// 未建模的子字段必须 400，不能静默丢。
	WebSearchOptions json.RawMessage `json:"web_search_options,omitempty"`
```

在文件末尾（`JSONSchema` 之后）追加三层嵌套结构体：

```go
// WebSearchOptions 是 Chat 的搜索选项。
//
// DashScope Compatible 承载它的只有布尔开关 enable_search，但这里仍要解码出
// 完整结构：该对象在出站前被整体删除，任何未建模的子字段都会随之消失——
// 宁可 400 拒绝，也不静默吞掉。
type WebSearchOptions struct {
	SearchContextSize string        `json:"search_context_size,omitempty"`
	UserLocation      *UserLocation `json:"user_location,omitempty"`
}

// UserLocation 是搜索的用户位置。OpenAI 当前只接受 approximate 类型；
// 具体位置在 approximate 子对象里。
type UserLocation struct {
	Type        string               `json:"type"`
	Approximate *ApproximateLocation `json:"approximate,omitempty"`
}

// ApproximateLocation 是用户的大致位置。
type ApproximateLocation struct {
	Country  string `json:"country,omitempty"`
	City     string `json:"city,omitempty"`
	Region   string `json:"region,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}
```

- [ ] **步骤 2.3：解码器识别与严格嵌套解码（GREEN 第二步）**

在 `internal/protocol/openaichat/decode.go` 中做三处修改。

其一，`Decoded` 结构体整体替换为：

```go
// Decoded 是解码结果。
//
// 与 Responses 不同，Chat 是无状态协议——没有 previous_response_id / store，
// 所以只带出 Canonical 请求与内联负载大小。
type Decoded struct {
	Request canonical.Request

	// InlineBytes 是本次请求内联（base64）多模态负载的总字节数。
	InlineBytes int64

	// webSearch 与 parallelToolCalls 是 OpenAI 特有开关，Canonical 没有对应字段。
	// 能力识别必须由解码器完成：异构出站路径读不得 Extensions——
	// 那是同源快通道专属的原样回填通道。
	webSearch         bool
	parallelToolCalls bool
}
```

其二，`Decode` 中现有的 `parallel_tool_calls` 块整体替换为：

```go
	// parallel_tool_calls 无 Canonical 对应字段——它是 OpenAI 特有的开关。
	// 存进 Extensions 供同源快通道原样回填；能力识别则不依赖它——
	// 异构出站读不得 Extensions，由解码器直接报告。
	if w.ParallelToolCalls != nil {
		raw, _ := json.Marshal(map[string]bool{"parallel_tool_calls": *w.ParallelToolCalls})
		r.Extensions.Set(canonical.ExtOpenAI, raw)
		// 只有显式 true 才报告：缺省与显式 false 都不构成并行调用请求。
		out.parallelToolCalls = *w.ParallelToolCalls
	}

	// web_search_options：出现且非 null（哪怕是 {}）即开启搜索，缺省或 null 为关闭。
	// 严格解码先行：这个对象在出站前被整体删除，未建模的子字段必须 400，
	// 既不许静默吞掉，也不许默认开启搜索。
	if len(w.WebSearchOptions) > 0 && string(w.WebSearchOptions) != "null" {
		if _, err := decodeWebSearchOptions(w.WebSearchOptions); err != nil {
			return nil, err
		}
		out.webSearch = true
	}
```

（位置不变：紧跟 `decodeModalities` 之后、`r.Validate()` 之前。）

其三，`Capabilities` 方法整体替换为：

```go
// Capabilities 报告这次请求用到了哪些能力，供降级矩阵裁决。
//
// Chat 无状态，没有 Responses 那样的会话读写端，直接复用 Canonical 的推导，
// 再补上两项 OpenAI 特有开关：它们没有 Canonical 字段，异构出站又读不得
// Extensions，能力识别只能在解码阶段完成。结果统一按 AllCapabilities 的顺序
// 输出——golden 文件依赖这一点稳定。
func (d *Decoded) Capabilities() []canonical.Capability {
	caps := d.Request.UsedCapabilities()
	if !d.webSearch && !d.parallelToolCalls {
		return caps
	}
	seen := make(map[canonical.Capability]bool, len(caps)+2)
	for _, c := range caps {
		seen[c] = true
	}
	if d.webSearch {
		seen[canonical.CapWebSearch] = true
	}
	if d.parallelToolCalls {
		seen[canonical.CapParallelToolCalls] = true
	}
	out := make([]canonical.Capability, 0, len(seen))
	for _, c := range canonical.AllCapabilities() {
		if seen[c] {
			out = append(out, c)
		}
	}
	return out
}
```

并在文件末尾追加：

```go
// decodeWebSearchOptions 严格解码搜索选项。
//
// 另起一个 DisallowUnknownFields 的解码器：外层的严格模式只管顶层字段，
// RawMessage 子树绕过了它——而这个对象在出站前会被整体删除，
// 未知子字段必须在入站就拒掉，不能等它悄悄消失。
func decodeWebSearchOptions(raw json.RawMessage) (*WebSearchOptions, error) {
	var wso WebSearchOptions
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wso); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest,
			"web_search_options 无法解析")
	}
	if wso.SearchContextSize != "" {
		switch wso.SearchContextSize {
		case "low", "medium", "high":
		default:
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"不支持的 search_context_size %q", wso.SearchContextSize)
		}
	}
	if loc := wso.UserLocation; loc != nil {
		if loc.Type != "approximate" {
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"不支持的 user_location.type %q", loc.Type)
		}
		if loc.Approximate == nil {
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"user_location 缺少 approximate")
		}
	}
	return &wso, nil
}
```

（`strings` 已在 decode.go 的 import 中。）

- [ ] **步骤 2.4：验证**

```bash
go test ./internal/protocol/openaichat/ -v && go test ./internal/gateway/ && gofmt -l internal/protocol/openaichat/
```

预期：openaichat 全部测试通过（含五条新测试）；gateway 包测试通过（chat 同源直通路径对新增能力报告不受影响——`openai.compat` 门兑现了全部可表达能力）；`gofmt -l` 无输出。

- [ ] **步骤 2.5：提交**

```bash
GIT_MASTER=1 git add internal/protocol/openaichat/wire.go internal/protocol/openaichat/decode.go internal/protocol/openaichat/decode_test.go
GIT_MASTER=1 git commit -m "Chat 解码报告搜索与并行工具能力：识别不再依赖 Extensions" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 3：DashScope Compatible 出站适配器

**依赖**：无（用到 `openaiwire.DecodeError`、`httpx`、`provider.Request`，均已存在）。
**文件**：新建 `internal/provider/dashscopecompat/dashscopecompat.go`、`internal/provider/dashscopecompat/dashscopecompat_test.go`。

- [ ] **步骤 3.1：写失败测试（RED）**

新建 `internal/provider/dashscopecompat/dashscopecompat_test.go`：

```go
package dashscopecompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

var refTime = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

// captured 记录上游实际收到了什么。
type captured struct {
	body   []byte
	header http.Header
	method string
	path   string
}

func serve(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.body, _ = io.ReadAll(r.Body)
		got.header = r.Header.Clone()
		got.method = r.Method
		got.path = r.URL.Path
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func call(t *testing.T, srv *httptest.Server, raw, upstreamModel string, stream bool) (*httpx.Response, error) {
	t.Helper()

	p := New(httpx.New(config.Default().Timeouts, func() time.Time { return refTime }),
		func() time.Time { return refTime })

	return p.Call(context.Background(), provider.Request{
		Target: router.Target{
			Kind:           degrade.ProviderDashScopeCompatible,
			Endpoint:       "test",
			BaseURL:        srv.URL,
			UpstreamModel:  upstreamModel,
			CredentialPool: "test",
		},
		Credential: credential.Credential{ID: "k1", Secret: "sk-gateway-own-key"},
		Raw:        []byte(raw),
		Stream:     stream,
		Path:       ChatCompletionsPath,
	})
}

func TestKindIsDashScopeCompatible(t *testing.T) {
	p := New(httpx.New(config.Default().Timeouts, nil), nil)
	if p.Kind() != degrade.ProviderDashScopeCompatible {
		t.Errorf("Kind() = %q，期望 %q", p.Kind(), degrade.ProviderDashScopeCompatible)
	}
}

// TestRequestShape 钉死上游请求的协议事实：method、path、网关凭据、模型改写、
// Accept 随流式与否。
func TestRequestShape(t *testing.T) {
	for _, tc := range []struct {
		stream     bool
		wantAccept string
	}{
		{false, "application/json"},
		{true, "text/event-stream"},
	} {
		srv, got := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{}`)
		})

		resp, err := call(t, srv,
			`{"model":"logical","messages":[{"role":"user","content":"hi"}]}`,
			"qwen-plus", tc.stream)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		if got.method != http.MethodPost {
			t.Errorf("method = %q，期望 POST", got.method)
		}
		if got.path != ChatCompletionsPath {
			t.Errorf("path = %q，期望 %q", got.path, ChatCompletionsPath)
		}
		if auth := got.header.Get("Authorization"); auth != "Bearer sk-gateway-own-key" {
			t.Errorf("Authorization = %q，期望网关自己的凭据", auth)
		}
		if ct := got.header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		if a := got.header.Get("Accept"); a != tc.wantAccept {
			t.Errorf("stream=%v 时 Accept = %q，期望 %q", tc.stream, a, tc.wantAccept)
		}

		var fields map[string]json.RawMessage
		if err := json.Unmarshal(got.body, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["model"]) != `"qwen-plus"` {
			t.Errorf("model = %s，期望 qwen-plus", fields["model"])
		}
	}
}

// TestNoSearchNoEnableSearch：客户端没要搜索时不得替它开启。
func TestNoSearchNoEnableSearch(t *testing.T) {
	srv, got := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})

	if _, err := call(t, srv,
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "m", false); err != nil {
		t.Fatal(err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got.body, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["enable_search"]; ok {
		t.Errorf("无 web_search_options 时不得注入 enable_search: %s", got.body)
	}
}

// TestSearchOptionsMappedToEnableSearch：非空 web_search_options 整体删除，
// 换写 enable_search: true。
func TestSearchOptionsMappedToEnableSearch(t *testing.T) {
	srv, got := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})

	raw := `{"model":"m","messages":[{"role":"user","content":"新闻"}],` +
		`"web_search_options":{"search_context_size":"high",` +
		`"user_location":{"type":"approximate","approximate":{` +
		`"country":"CN","city":"上海","timezone":"Asia/Shanghai"}}}}`
	if _, err := call(t, srv, raw, "m", false); err != nil {
		t.Fatal(err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got.body, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["web_search_options"]; ok {
		t.Errorf("web_search_options 应被删除: %s", got.body)
	}
	if string(fields["enable_search"]) != "true" {
		t.Errorf("enable_search = %s，期望 true", fields["enable_search"])
	}
}

// TestEmptySearchOptionsStillEnables：{} 正是客户端的显式搜索请求。
func TestEmptySearchOptionsStillEnables(t *testing.T) {
	srv, got := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})

	if _, err := call(t, srv,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"web_search_options":{}}`,
		"m", false); err != nil {
		t.Fatal(err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got.body, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["enable_search"]) != "true" {
		t.Errorf("enable_search = %s，期望 true", fields["enable_search"])
	}
	if _, ok := fields["web_search_options"]; ok {
		t.Errorf("web_search_options 应被删除: %s", got.body)
	}
}

// TestNullSearchOptionsStayUntouched：null 与缺省同义——不开搜索，也不动字段。
func TestNullSearchOptionsStayUntouched(t *testing.T) {
	srv, got := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})

	if _, err := call(t, srv,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"web_search_options":null}`,
		"m", false); err != nil {
		t.Fatal(err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got.body, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["enable_search"]; ok {
		t.Errorf("null 等同缺省，不得注入 enable_search: %s", got.body)
	}
	if string(fields["web_search_options"]) != "null" {
		t.Errorf("null 的 web_search_options 应原样保留: %s", got.body)
	}
}

// TestUnrelatedFieldsPreserved：除两处修补点外全部字段保持原语义——
// 包括网关没有建模的字段。这是不经 Canonical 往返的理由。
func TestUnrelatedFieldsPreserved(t *testing.T) {
	srv, got := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})

	raw := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"n":2,"presence_penalty":0.5,"frequency_penalty":-0.5,"logprobs":true,"top_logprobs":3,` +
		`"tools":[{"type":"function","function":{"name":"f"}}],` +
		`"stream_options":{"include_usage":true},` +
		`"response_format":{"type":"json_object"},` +
		`"web_search_options":{},` +
		`"brand_new_param":{"nested":[1,2,3]}}`
	if _, err := call(t, srv, raw, "upstream", false); err != nil {
		t.Fatal(err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got.body, &fields); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"n":                 "2",
		"presence_penalty":  "0.5",
		"frequency_penalty": "-0.5",
		"logprobs":          "true",
		"top_logprobs":      "3",
		"tools":             `[{"type":"function","function":{"name":"f"}}]`,
		"stream_options":    `{"include_usage":true}`,
		"response_format":   `{"type":"json_object"}`,
		"brand_new_param":   `{"nested":[1,2,3]}`,
	}
	for k, w := range want {
		if string(fields[k]) != w {
			t.Errorf("字段 %s = %s，期望原样保留 %s", k, fields[k], w)
		}
	}
	if string(fields["enable_search"]) != "true" {
		t.Errorf("enable_search = %s，期望 true", fields["enable_search"])
	}
	if _, ok := fields["web_search_options"]; ok {
		t.Errorf("web_search_options 应被删除: %s", got.body)
	}
}

// TestClientEnableSearchPreservedWithoutOptions：客户端自己带的 enable_search
// （DashScope 原生参数）与搜索映射无关，不得被改动。
func TestClientEnableSearchPreservedWithoutOptions(t *testing.T) {
	srv, got := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})

	if _, err := call(t, srv,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"enable_search":false}`,
		"m", false); err != nil {
		t.Fatal(err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got.body, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["enable_search"]) != "false" {
		t.Errorf("客户端自带的 enable_search=false 应原样保留: %s", got.body)
	}
}

// TestUpstreamErrorDecoded：非 2xx 按 OpenAI 信封解码，Retry-After 保留。
// DashScope Compatible 的错误信封与 OpenAI 同形。
func TestUpstreamErrorDecoded(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)
	})

	_, err := call(t, srv,
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "m", false)
	if err == nil {
		t.Fatal("非 2xx 应当返回错误")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassRateLimit {
		t.Errorf("分类 = %q，期望 rate_limit", cerr.Class)
	}
	if !cerr.Retryable {
		t.Error("429 应可重试（换凭据可能成功）")
	}
	if cerr.RetryAfter != 7*time.Second {
		t.Errorf("Retry-After = %v，期望 7s", cerr.RetryAfter)
	}
	if cerr.UpstreamStatus != http.StatusTooManyRequests {
		t.Errorf("UpstreamStatus = %d，期望 429", cerr.UpstreamStatus)
	}
}

// TestMissingModelRejected：请求体缺 model 是入站解码就该拦下的，
// 适配器这里只做兜底，不能 panic。
func TestMissingModelRejected(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})

	_, err := call(t, srv, `{"messages":[{"role":"user","content":"hi"}]}`, "m", false)
	if err == nil {
		t.Fatal("缺少 model 应当返回错误")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) || cerr.Class != canonical.ClassBadRequest {
		t.Errorf("应为 bad_request 的 *canonical.Error，实际 %v", err)
	}
}
```

运行：

```bash
go test ./internal/provider/dashscopecompat/ -v
```

预期：**编译失败**，`undefined: New` / `undefined: ChatCompletionsPath`——正确的 RED（包尚不存在）。

- [ ] **步骤 3.2：实现适配器（GREEN）**

新建 `internal/provider/dashscopecompat/dashscopecompat.go`：

```go
// Package dashscopecompat 是 DashScope Compatible（OpenAI 线格式）的出站适配器。
//
// wire-compatible 而语义异构：请求与响应仍走 Chat Completions 线格式，因此不做
// Canonical 重编码；但上游语义与 OpenAI 并不等同——模型能力、结构化输出保证、
// Web Search 参数都有差异。所以这条路**不是**同源快通道，不得 MarkHomogeneous：
// wire-compatible 只说明不需要重编码，不能推导为语义零损失，X-Omugw-Degraded
// 仍由矩阵生成。
//
// 对客户端原始 JSON 只做两处定点修补：改写模型名；把非 null 的
// web_search_options 映射成 enable_search: true。其余字段保持原始字节——
// 当前 IR 不承载 n、presence/frequency penalty、logprobs 等全部 Chat 参数，
// 经 Canonical 往返会制造静默损失。
package dashscopecompat

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/protocol/openaiwire"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// ChatCompletionsPath 是 DashScope Compatible Chat 的上游端点。
const ChatCompletionsPath = "/v1/chat/completions"

// Provider 只负责 OpenAI Chat compatible wire 的出站。
type Provider struct {
	client *httpx.Client
	now    func() time.Time
}

// New 构造适配器。now 可注入以便测试，传 nil 用 time.Now。
func New(c *httpx.Client, now func() time.Time) *Provider {
	if now == nil {
		now = time.Now
	}
	return &Provider{client: c, now: now}
}

// Kind 返回协议族。
func (p *Provider) Kind() degrade.Provider { return degrade.ProviderDashScopeCompatible }

// Call 把请求发给 DashScope Compatible。
//
// 原始 JSON 只做两处定点修补，响应原样返回给网关转发。Canonical 已在网关层
// 完成请求校验与能力裁决，这里不消费——本路径没有 Canonical 出站编码器。
func (p *Provider) Call(ctx context.Context, req provider.Request) (*httpx.Response, error) {
	body, err := patch(req.Raw, req.Target.UpstreamModel)
	if err != nil {
		return nil, err
	}

	url := strings.TrimSuffix(req.Target.BaseURL, "/") + p.pathFor(req)
	hreq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "构造上游请求失败")
	}

	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", acceptFor(req.Stream))
	// 网关用自己的凭据。客户端发来的 Authorization 到此为止。
	hreq.Header.Set("Authorization", "Bearer "+req.Credential.Secret)

	resp, err := p.client.Do(ctx, hreq)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		return nil, p.decodeError(resp)
	}
	return resp, nil
}

// pathFor 优先用请求携带的路径，留空退回本适配器唯一的端点。
func (p *Provider) pathFor(req provider.Request) string {
	if req.Path != "" {
		return req.Path
	}
	return ChatCompletionsPath
}

func acceptFor(stream bool) string {
	if stream {
		return "text/event-stream"
	}
	return "application/json"
}

// decodeError 读出错误体并按 OpenAI 信封解码——DashScope Compatible 的
// 非 2xx 与 OpenAI 同形。读之前设上限：一个故障上游可能在 5xx 里塞进
// 一整个 HTML 页面，甚至更糟。
func (p *Provider) decodeError(resp *httpx.Response) error {
	defer resp.Body.Close()

	const maxErrorBody = 64 << 10
	body := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for len(body) < maxErrorBody {
		n, err := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	return openaiwire.DecodeError(resp.StatusCode, body, resp.Header, p.now())
}

// patch 对客户端原始 JSON 做仅有的两处修改：改写模型名；把非 null 的
// web_search_options 映射成 enable_search: true 并删除原字段。
//
// 解成 map[string]json.RawMessage 再写回，而不是整体反序列化：这样除修补点
// 之外的每个字段都保持原始字节，包括网关不认识的新参数。键序会随重新序列化
// 改变，语义不变——一致性断言按语义比对，不做字节比对。
//
// 两者同时出现时映射覆盖客户端自带的 enable_search：发了 web_search_options
// 就是客户端的显式搜索请求，而 enable_search 是 DashScope 原生参数，
// OpenAI 客户端本不会发它。
func patch(raw []byte, upstreamModel string) ([]byte, error) {
	if upstreamModel == "" {
		return nil, canonical.Newf(canonical.ClassInternal, "路由目标缺少上游模型名")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest, "请求体不是 JSON 对象")
	}
	if _, ok := fields["model"]; !ok {
		return nil, canonical.Newf(canonical.ClassBadRequest, "请求体缺少 model")
	}

	patched, err := json.Marshal(upstreamModel)
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "序列化模型名失败")
	}
	fields["model"] = patched

	// 仅当客户端发了非 null 对象时映射：缺省或 null 是「不要搜索」
	//（解码器已按此语义识别能力），此时不得替它注入 enable_search。
	// search_context_size / user_location 没有 DashScope 落点，不猜测映射——
	// 损失登记在降级矩阵，随响应头告知客户端。
	if wso, ok := fields["web_search_options"]; ok && string(wso) != "null" {
		delete(fields, "web_search_options")
		fields["enable_search"] = json.RawMessage("true")
	}

	out, err := json.Marshal(fields)
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "重新序列化请求体失败")
	}
	return out, nil
}
```

- [ ] **步骤 3.3：验证**

```bash
go test ./internal/provider/dashscopecompat/ -v && go build ./... && gofmt -l internal/provider/dashscopecompat/
```

预期：十条测试全部 PASS；构建通过；`gofmt -l` 无输出。

- [ ] **步骤 3.4：提交**

```bash
GIT_MASTER=1 git add internal/provider/dashscopecompat/dashscopecompat.go internal/provider/dashscopecompat/dashscopecompat_test.go
GIT_MASTER=1 git commit -m "DashScope Compatible 出站适配器：原始 JSON 定点修补，不经 Canonical" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 4：网关测试 harness 注入 Provider 工厂，Chat 哨兵改指 Anthropic

**依赖**：任务 3（`dashscopecompat.New` 已存在）。
**文件**：修改 `internal/gateway/gateway_test.go`。

背景：`newHarnessFor` 目前硬编码 `passthrough.New` 构造出站适配器，wire-compatible 路径有自己的适配器，测试基建必须能注入工厂。同时 `newChatHarness(t, false, …)` 的「未实现哨兵」目前指向 `ProviderDashScopeCompatible`——任务 9 兑现该路径后这个哨兵就失效（请求会得到 200 而不是 501），必须先改指 Phase 1 永远 PLANNED 的 `ProviderAnthropicMessages`。

- [ ] **步骤 4.1：先加哨兵锁定测试**

在 `internal/gateway/gateway_test.go` 的 `TestPlannedRouteReturns501` 之后追加：

```go
// TestChatPlannedRouteReturns501 固化 Chat 入站的未实现哨兵。
//
// 哨兵指向仍是 PLANNED 的 anthropic.messages：dashscope.compatible 转正之后，
// 「未实现路径 501」在 Chat 入站仍然可测。谁把哨兵指回已转正的路径，
// 这条测试就在转正当天变红。
func TestChatPlannedRouteReturns501(t *testing.T) {
	hs := newChatHarness(t, false, jsonUpstream(t, `{}`))

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, true)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d, 期望 501: %s", rec.Code, rec.Body.String())
	}
}
```

说明：此测试此刻即绿（兑现前旧哨兵也给出 501），它的咬合力在任务 9 之后显现——哨兵若不改，兑现后它会收到 200。

- [ ] **步骤 4.2：重构 newHarnessFor 为工厂注入**

在 `gateway_test.go` 中做以下修改（同一文件内，编译必须一次收敛）：

其一，在 `endpointName` 函数之前追加：

```go
// sentinelProviderPath 是 harness 给 passthrough 适配器故意挑的默认路径：
// /v1/responses 不属于任何被测入站协议的端点，只有当 handler 真的注入了
// provider.Request.Path，请求才会打到正确端点——否则 fixture 回放脱靶、测试变红。
const sentinelProviderPath = "/v1/responses"

// providerFactory 按协议族构造出站适配器。harness 不得硬编码 passthrough：
// wire-compatible 路径有自己的适配器。
type providerFactory func(kind degrade.Provider, client *httpx.Client) provider.Provider

// passthroughFactory 构造同源直通适配器，默认路径用哨兵路径。
func passthroughFactory(kind degrade.Provider, client *httpx.Client) provider.Provider {
	return passthrough.New(kind, sentinelProviderPath, client, nil)
}

// dashScopeCompatFactory 构造 DashScope Compatible 适配器。
func dashScopeCompatFactory(_ degrade.Provider, client *httpx.Client) provider.Provider {
	return dashscopecompat.New(client, nil)
}
```

其二，`newHarnessFor` 签名与内部构造改为：

```go
func newHarnessFor(t *testing.T, requestPath string, kind degrade.Provider, mk func(Deps) *Handler, limits config.Limits, factory providerFactory, ups ...*upstream) *harness {
```

（删去原 `providerDefaultPath string` 参数；函数体中 `provs[name] = passthrough.New(kind, providerDefaultPath, client, nil)` 一行改为 `provs[name] = factory(kind, client)`。其余不动。）

其三，四个调用点改为：

```go
func newHarness(t *testing.T, implemented bool, ups ...*upstream) *harness {
	t.Helper()
	kind := degrade.ProviderOpenAICompat
	if !implemented {
		kind = degrade.ProviderDashScopeCompatible
	}
	return newHarnessFor(t, "/v1/responses", kind, NewResponsesHandler,
		config.Default().Limits, passthroughFactory, ups...)
}
```

```go
func newChatHarness(t *testing.T, implemented bool, ups ...*upstream) *harness {
	t.Helper()
	kind := degrade.ProviderOpenAICompat
	if !implemented {
		// 未实现哨兵指向 Phase 1 永远 PLANNED 的 anthropic.messages：
		// dashscope.compatible 已转正，不能再当哨兵。
		kind = degrade.ProviderAnthropicMessages
	}
	return newHarnessFor(t, "/v1/chat/completions", kind, NewChatHandler,
		config.Default().Limits, passthroughFactory, ups...)
}
```

```go
func newDashScopeNativeHarness(t *testing.T, implemented bool, ups ...*upstream) *harness {
	t.Helper()
	kind := degrade.ProviderDashScopeNative
	if !implemented {
		kind = degrade.ProviderDashScopeCompatible
	}
	return newHarnessFor(t, dashscopenative.TextGenerationPath, kind,
		NewDashScopeNativeHandler, config.Default().Limits, passthroughFactory, ups...)
}
```

```go
func newDashScopeNativeHarnessWithLimits(t *testing.T, limits config.Limits, ups ...*upstream) *harness {
	t.Helper()
	return newHarnessFor(t, dashscopenative.TextGenerationPath,
		degrade.ProviderDashScopeNative, NewDashScopeNativeHandler, limits,
		passthroughFactory, ups...)
}
```

（注意：原 `newHarnessFor` 的注释里「provider 的默认路径故意用 /v1/responses」一段说明，移到 `sentinelProviderPath` 常量注释里表达，`newChatHarness` 上方的旧注释相应精简，避免两份重复说明漂移。）

其四，追加新 harness：

```go
// newChatDSCompatHarness 是 Chat -> DashScope Compatible wire-compatible 路径的 harness。
//
// 出站适配器是 dashscopecompat 而不是 passthrough——这正是 harness 注入
// Provider 工厂的原因：wire-compatible 而语义异构的路径有自己的适配器，
// 测试基建不能硬编码同源直通。
func newChatDSCompatHarness(t *testing.T, ups ...*upstream) *harness {
	t.Helper()
	return newHarnessFor(t, "/v1/chat/completions", degrade.ProviderDashScopeCompatible,
		NewChatHandler, config.Default().Limits, dashScopeCompatFactory, ups...)
}
```

其五，import 块追加 `"github.com/yobo2u/omugw/internal/provider/dashscopecompat"`。

- [ ] **步骤 4.3：验证**

```bash
go test ./internal/gateway/ -v 2>&1 | tail -30 && gofmt -l internal/gateway/
```

预期：gateway 全部测试通过（含新的 `TestChatPlannedRouteReturns501`）；`gofmt -l` 无输出。

- [ ] **步骤 4.4：提交**

```bash
GIT_MASTER=1 git add internal/gateway/gateway_test.go
GIT_MASTER=1 git commit -m "网关测试 harness 注入 Provider 工厂：Chat 未实现哨兵改指 Anthropic" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 5：Build 装配 dashscope.compatible

**依赖**：任务 3。
**文件**：修改 `internal/gateway/build.go`、`internal/gateway/build_test.go`、`internal/gateway/AGENTS.md`。

- [ ] **步骤 5.1：写失败测试（RED）**

在 `internal/gateway/build_test.go` 末尾追加：

```go
// TestBuildAcceptsDashScopeCompatibleProvider 固化 dashscope.compatible 的装配：
// 配置了这个协议族的网关必须能启动——它已在降级矩阵里设计，适配器也已写好。
func TestBuildAcceptsDashScopeCompatibleProvider(t *testing.T) {
	m, err := degrade.Phase1()
	if err != nil {
		t.Fatal(err)
	}

	cfg := buildTestConfig("http://127.0.0.1:0")
	cfg.Providers[0].Kind = "dashscope.compatible"
	if _, err := Build(cfg, m, obs.NewMetrics(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("dashscope.compatible 应已装配: %v", err)
	}
}

// TestBuildRejectsUnimplementedKindListsImplemented 固化未实现协议族的启动错误
// 必须列全已实现的协议族——漏列会让运维以为某个已实现的族不存在。
func TestBuildRejectsUnimplementedKindListsImplemented(t *testing.T) {
	m := degrade.NewMatrix()

	cfg := buildTestConfig("http://127.0.0.1:0")
	cfg.Providers[0].Kind = "anthropic.messages"
	_, err := Build(cfg, m, obs.NewMetrics(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("未实现的协议族应当启动失败")
	}
	for _, kind := range []string{"openai.compat", "dashscope.compatible", "dashscope.native"} {
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("错误应列出已实现的协议族 %s: %v", kind, err)
		}
	}
}
```

运行：

```bash
go test ./internal/gateway/ -run 'TestBuildAcceptsDashScopeCompatibleProvider|TestBuildRejectsUnimplementedKindListsImplemented' -v
```

预期：两条都**失败**——前者报「gateway: provider … 的协议族 "dashscope.compatible" 尚无出站适配器」，后者报错误里缺少 `dashscope.compatible`。正确的 RED。

- [ ] **步骤 5.2：装配（GREEN）**

在 `internal/gateway/build.go` 中：

其一，import 块追加：

```go
	"github.com/yobo2u/omugw/internal/provider/dashscopecompat"
```

其二，协议族 switch 改为：

```go
		kind := degrade.Provider(p.Kind)
		switch kind {
		case degrade.ProviderOpenAICompat:
			provs[p.Endpoint] = passthrough.New(kind, "/v1/responses", client, nil)
		case degrade.ProviderDashScopeNative:
			// 直通路径随请求走（handler 会注入实际路径），这里只是兜底默认值。
			provs[p.Endpoint] = passthrough.New(kind, dashscopenative.TextGenerationPath, client, nil)
		case degrade.ProviderDashScopeCompatible:
			// wire-compatible 而语义异构：请求仍走 Chat wire，路径由 handler 注入
			// /v1/chat/completions，适配器自带同名兜底默认值。
			provs[p.Endpoint] = dashscopecompat.New(client, nil)
		default:
			// 未实现的协议族在这里就拒绝，而不是等请求打进来才发现没有适配器。
			return nil, fmt.Errorf(
				"gateway: provider %q 的协议族 %q 尚无出站适配器（已实现 %s、%s 与 %s）",
				p.Endpoint, p.Kind, degrade.ProviderOpenAICompat,
				degrade.ProviderDashScopeCompatible, degrade.ProviderDashScopeNative)
		}
```

- [ ] **步骤 5.3：更新 gateway 知识库**

`internal/gateway/AGENTS.md` 的 `## STRUCTURE` 表中 `build.go` 一行，把开头句改为：

```markdown
| `build.go` | 从 `config.Config` 装配凭据池、Provider、Router、Handler、Mux。出站适配器按协议族装配：`openai.compat` / `dashscope.native` 两类同源直通走 passthrough，wire-compatible 的 `dashscope.compatible` 走专用定点修补器；未实现的协议族在启动阶段直接拒绝。Mux 注册先落四个精确端点（OpenAI 两门 + Native 文本 / 多模态两门，Native 两门复用同一 handler 实例），再挂 DashScope Native 命名空间的两条兜底：`POST /api/v1/` 返回协议化 501，不带方法的 `/api/v1/` 返回框架 404——未投放端点在进 Handler 主链路之前就被拦下。注册清单（`doors`）是端点与处理器的唯一事实来源：每行只写端点与 `*Handler`，协议由处理器身份派生；启动期按 `Inbound`（协议 + 端点）对矩阵兑现与 Mux 注册做双向对账，任一方向失败即启动失败 |
```

- [ ] **步骤 5.4：验证**

```bash
go test ./internal/gateway/ && go build ./... && gofmt -l internal/gateway/
```

预期：全部通过；`gofmt -l` 无输出。

- [ ] **步骤 5.5：提交**

```bash
GIT_MASTER=1 git add internal/gateway/build.go internal/gateway/build_test.go internal/gateway/AGENTS.md
GIT_MASTER=1 git commit -m "Build 装配 dashscope.compatible：未实现协议族名单同步更新" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 6：第一组 fixture（basic / streaming / tool_calling）

**依赖**：任务 1（`upstream` 字段已存在）。
**文件**：新建 `testdata/routes/openai.chat__dashscope.compatible/` 目录下三份 fixture。

约定（本任务与任务 7、8 共用）：

- `request` 是**客户端发给网关**的内容；`response` 是被打桩的上游返回；`upstream` 是上游应当收到的请求。三者不是一回事：网关改写鉴权与模型名，映射搜索开关。
- harness 的 `UpstreamModel` 固定是 `upstream-model`，凭据固定是 `Bearer sk-a`——`upstream.body.model` 必须是 `upstream-model`。
- 每份 fixture 必须写 `note`。
- `request.path` 必须是 `/v1/chat/completions`（门槛按它与门清单双向对账）。
- fixture 文件内容必须逐字按下述 JSON 写入（golden 的 body 与 fixture 的 `response.body` 字节一致，缩进不同会让 golden 漂移）。

- [ ] **步骤 6.1：basic.json**

新建 `testdata/routes/openai.chat__dashscope.compatible/basic.json`：

```json
{
  "name": "openai.chat__dashscope.compatible/basic",
  "note": "wire-compatible 的最小闭环：Chat 请求经定点修补（改写模型名与鉴权）后交给 DashScope Compatible，上游 Chat 响应原样回给客户端。upstream 声明钉死上游实际收到的形状——只比客户端响应会漏掉「Provider 没做改写」的假绿。",
  "request": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "headers": {
      "authorization": "<redacted>",
      "content-type": "application/json"
    },
    "body": {
      "model": "logical-fast",
      "messages": [{ "role": "user", "content": "上海天气怎么样？" }]
    }
  },
  "upstream": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "body": {
      "model": "upstream-model",
      "messages": [{ "role": "user", "content": "上海天气怎么样？" }]
    }
  },
  "response": {
    "status": 200,
    "headers": {
      "content-type": "application/json",
      "x-ratelimit-remaining-tokens": "1999918"
    },
    "body": {
      "id": "chatcmpl-ds01",
      "object": "chat.completion",
      "created": 1755216000,
      "model": "qwen-plus-upstream",
      "choices": [
        { "index": 0, "message": { "role": "assistant", "content": "今天多云，22 度。" }, "finish_reason": "stop" }
      ],
      "usage": {
        "prompt_tokens": 24,
        "completion_tokens": 11,
        "total_tokens": 35,
        "prompt_tokens_details": { "cached_tokens": 0 }
      }
    }
  }
}
```

- [ ] **步骤 6.2：streaming.json**

新建 `testdata/routes/openai.chat__dashscope.compatible/streaming.json`：

```json
{
  "name": "openai.chat__dashscope.compatible/streaming",
  "note": "流式转发。frames 复现上游把多个 chunk 挤在一次 Write 里的真实行为——缓冲 bug 只在分片边界上才会暴露。最后一个 chunk 的 usage（客户端需带 stream_options.include_usage）会被提取用于计费，其余 chunk 的 data 负载逐字保留。upstream 声明证明 stream 与 stream_options 原样带给上游。",
  "request": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "headers": {
      "authorization": "<redacted>",
      "content-type": "application/json"
    },
    "body": {
      "model": "logical-fast",
      "messages": [{ "role": "user", "content": "数到三" }],
      "stream": true,
      "stream_options": { "include_usage": true }
    }
  },
  "upstream": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "body": {
      "model": "upstream-model",
      "messages": [{ "role": "user", "content": "数到三" }],
      "stream": true,
      "stream_options": { "include_usage": true }
    }
  },
  "response": {
    "status": 200,
    "headers": { "content-type": "text/event-stream" },
    "sse": {
      "frames": [2, 3, 1],
      "events": [
        {
          "data": "{\"id\":\"chatcmpl-ds02\",\"object\":\"chat.completion.chunk\",\"created\":1755216000,\"model\":\"qwen-plus-upstream\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}"
        },
        {
          "data": "{\"id\":\"chatcmpl-ds02\",\"object\":\"chat.completion.chunk\",\"created\":1755216000,\"model\":\"qwen-plus-upstream\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"一\"},\"finish_reason\":null}]}"
        },
        {
          "data": "{\"id\":\"chatcmpl-ds02\",\"object\":\"chat.completion.chunk\",\"created\":1755216000,\"model\":\"qwen-plus-upstream\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"二\"},\"finish_reason\":null}]}"
        },
        {
          "data": "{\"id\":\"chatcmpl-ds02\",\"object\":\"chat.completion.chunk\",\"created\":1755216000,\"model\":\"qwen-plus-upstream\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"三\"},\"finish_reason\":null}]}"
        },
        {
          "data": "{\"id\":\"chatcmpl-ds02\",\"object\":\"chat.completion.chunk\",\"created\":1755216000,\"model\":\"qwen-plus-upstream\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3,\"total_tokens\":12}}"
        },
        {
          "data": "[DONE]"
        }
      ]
    }
  }
}
```

- [ ] **步骤 6.3：tool_calling.json**

新建 `testdata/routes/openai.chat__dashscope.compatible/tool_calling.json`：

```json
{
  "name": "openai.chat__dashscope.compatible/tool_calling",
  "note": "工具声明、工具结果与工具调用响应：请求带 tools 声明与 assistant tool_calls / tool 结果两段历史，上游收到原样的工具结构，响应是基于工具结果的最终回答。upstream 声明证明 tools / tool_choice / 工具消息全部原样保留。",
  "request": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "headers": {
      "authorization": "<redacted>",
      "content-type": "application/json"
    },
    "body": {
      "model": "logical-fast",
      "messages": [
        { "role": "user", "content": "上海天气怎么样？" },
        {
          "role": "assistant",
          "content": "",
          "tool_calls": [
            { "id": "call_1", "type": "function", "function": { "name": "get_weather", "arguments": "{\"city\":\"上海\"}" } }
          ]
        },
        { "role": "tool", "tool_call_id": "call_1", "content": "今天多云，22 度。" }
      ],
      "tools": [
        {
          "type": "function",
          "function": {
            "name": "get_weather",
            "description": "查询城市天气",
            "parameters": {
              "type": "object",
              "properties": { "city": { "type": "string" } },
              "required": ["city"]
            }
          }
        }
      ],
      "tool_choice": "auto"
    }
  },
  "upstream": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "body": {
      "model": "upstream-model",
      "messages": [
        { "role": "user", "content": "上海天气怎么样？" },
        {
          "role": "assistant",
          "content": "",
          "tool_calls": [
            { "id": "call_1", "type": "function", "function": { "name": "get_weather", "arguments": "{\"city\":\"上海\"}" } }
          ]
        },
        { "role": "tool", "tool_call_id": "call_1", "content": "今天多云，22 度。" }
      ],
      "tools": [
        {
          "type": "function",
          "function": {
            "name": "get_weather",
            "description": "查询城市天气",
            "parameters": {
              "type": "object",
              "properties": { "city": { "type": "string" } },
              "required": ["city"]
            }
          }
        }
      ],
      "tool_choice": "auto"
    }
  },
  "response": {
    "status": 200,
    "headers": { "content-type": "application/json" },
    "body": {
      "id": "chatcmpl-ds03",
      "object": "chat.completion",
      "created": 1755216000,
      "model": "qwen-plus-upstream",
      "choices": [
        { "index": 0, "message": { "role": "assistant", "content": "上海今天多云，22 度。" }, "finish_reason": "stop" }
      ],
      "usage": {
        "prompt_tokens": 48,
        "completion_tokens": 12,
        "total_tokens": 60
      }
    }
  }
}
```

- [ ] **步骤 6.4：验证**

```bash
go run - <<'EOF'
package main

import (
	"fmt"
	"os"

	"github.com/yobo2u/omugw/internal/testkit"
)

func main() {
	for _, name := range []string{"basic", "streaming", "tool_calling"} {
		path := "testdata/routes/openai.chat__dashscope.compatible/" + name + ".json"
		raw, err := os.ReadFile(path)
		if err != nil {
			fmt.Println("读取失败:", err)
			os.Exit(1)
		}
		_ = raw
	}
	fmt.Println("ok")
}
EOF
```

更简单的等价做法（推荐）——直接用现有测试基建加载校验：

```bash
go test ./internal/degrade/ -run TestImplementedRoutesHaveFixtures -v
```

预期：通过（该路径尚未转正，门槛跳过它；此步只验证 JSON 可解析）。另跑一次 fixture 自身合法性校验：

```bash
go test ./internal/testkit/ -v
```

预期：通过。三份 fixture 的 `upstream` 断言三者齐备，`Validate` 不报错。

- [ ] **步骤 6.5：提交**

```bash
GIT_MASTER=1 git add testdata/routes/openai.chat__dashscope.compatible/basic.json testdata/routes/openai.chat__dashscope.compatible/streaming.json testdata/routes/openai.chat__dashscope.compatible/tool_calling.json
GIT_MASTER=1 git commit -m "Chat 到 DashScope Compatible 路径 fixture：基础、流式与工具调用" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 7：第二组 fixture（parallel_tool_calls / structured_output / reasoning）

**依赖**：任务 6（目录已存在）。
**文件**：同目录三份 fixture。

- [ ] **步骤 7.1：parallel_tool_calls.json**

新建 `testdata/routes/openai.chat__dashscope.compatible/parallel_tool_calls.json`：

```json
{
  "name": "openai.chat__dashscope.compatible/parallel_tool_calls",
  "note": "显式 parallel_tool_calls: true 的能力识别与 raw 保留：解码器据此报告 CapParallelToolCalls，适配器原样保留该字段，上游在一次响应里并行发起两个工具调用。",
  "request": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "headers": {
      "authorization": "<redacted>",
      "content-type": "application/json"
    },
    "body": {
      "model": "logical-fast",
      "messages": [{ "role": "user", "content": "同时查上海和北京的天气" }],
      "tools": [
        {
          "type": "function",
          "function": {
            "name": "get_weather",
            "description": "查询城市天气",
            "parameters": {
              "type": "object",
              "properties": { "city": { "type": "string" } },
              "required": ["city"]
            }
          }
        }
      ],
      "parallel_tool_calls": true
    }
  },
  "upstream": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "body": {
      "model": "upstream-model",
      "messages": [{ "role": "user", "content": "同时查上海和北京的天气" }],
      "tools": [
        {
          "type": "function",
          "function": {
            "name": "get_weather",
            "description": "查询城市天气",
            "parameters": {
              "type": "object",
              "properties": { "city": { "type": "string" } },
              "required": ["city"]
            }
          }
        }
      ],
      "parallel_tool_calls": true
    }
  },
  "response": {
    "status": 200,
    "headers": { "content-type": "application/json" },
    "body": {
      "id": "chatcmpl-ds04",
      "object": "chat.completion",
      "created": 1755216000,
      "model": "qwen-plus-upstream",
      "choices": [
        {
          "index": 0,
          "message": {
            "role": "assistant",
            "content": "",
            "tool_calls": [
              { "id": "call_a", "type": "function", "function": { "name": "get_weather", "arguments": "{\"city\":\"上海\"}" } },
              { "id": "call_b", "type": "function", "function": { "name": "get_weather", "arguments": "{\"city\":\"北京\"}" } }
            ]
          },
          "finish_reason": "tool_calls"
        }
      ],
      "usage": {
        "prompt_tokens": 45,
        "completion_tokens": 24,
        "total_tokens": 69
      }
    }
  }
}
```

- [ ] **步骤 7.2：structured_output.json（DEGRADE 举证）**

新建 `testdata/routes/openai.chat__dashscope.compatible/structured_output.json`：

```json
{
  "name": "openai.chat__dashscope.compatible/structured_output",
  "note": "DEGRADE 举证（文件名即能力名，ADR-0001）：response_format 的 json_schema 原样转发，不降成提示词、不改写 schema；DashScope Compatible 不保证全局 strict 校验，降级头必须可见。upstream 声明证明 schema 一字未动。",
  "request": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "headers": {
      "authorization": "<redacted>",
      "content-type": "application/json"
    },
    "body": {
      "model": "logical-fast",
      "messages": [{ "role": "user", "content": "把「明天上海多云转晴 22 到 28 度」抽成结构化数据" }],
      "response_format": {
        "type": "json_schema",
        "json_schema": {
          "name": "weather_report",
          "strict": true,
          "schema": {
            "type": "object",
            "properties": {
              "city": { "type": "string" },
              "condition": { "type": "string" },
              "temp_low": { "type": "integer" },
              "temp_high": { "type": "integer" }
            },
            "required": ["city", "condition", "temp_low", "temp_high"],
            "additionalProperties": false
          }
        }
      }
    }
  },
  "upstream": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "body": {
      "model": "upstream-model",
      "messages": [{ "role": "user", "content": "把「明天上海多云转晴 22 到 28 度」抽成结构化数据" }],
      "response_format": {
        "type": "json_schema",
        "json_schema": {
          "name": "weather_report",
          "strict": true,
          "schema": {
            "type": "object",
            "properties": {
              "city": { "type": "string" },
              "condition": { "type": "string" },
              "temp_low": { "type": "integer" },
              "temp_high": { "type": "integer" }
            },
            "required": ["city", "condition", "temp_low", "temp_high"],
            "additionalProperties": false
          }
        }
      }
    }
  },
  "response": {
    "status": 200,
    "headers": { "content-type": "application/json" },
    "body": {
      "id": "chatcmpl-ds05",
      "object": "chat.completion",
      "created": 1755216000,
      "model": "qwen-plus-upstream",
      "choices": [
        {
          "index": 0,
          "message": {
            "role": "assistant",
            "content": "{\"city\":\"上海\",\"condition\":\"多云转晴\",\"temp_low\":22,\"temp_high\":28}"
          },
          "finish_reason": "stop"
        }
      ],
      "usage": {
        "prompt_tokens": 63,
        "completion_tokens": 27,
        "total_tokens": 90
      }
    }
  }
}
```

- [ ] **步骤 7.3：reasoning.json**

新建 `testdata/routes/openai.chat__dashscope.compatible/reasoning.json`：

```json
{
  "name": "openai.chat__dashscope.compatible/reasoning",
  "note": "reasoning_effort 原样保留：解码器识别 CapReasoning，适配器不动该字段，上游响应携带推理 token 明细。",
  "request": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "headers": {
      "authorization": "<redacted>",
      "content-type": "application/json"
    },
    "body": {
      "model": "logical-thinking",
      "messages": [{ "role": "user", "content": "一个房间里有三根蜡烛，吹灭一根，最后还剩几根？" }],
      "reasoning_effort": "high"
    }
  },
  "upstream": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "body": {
      "model": "upstream-model",
      "messages": [{ "role": "user", "content": "一个房间里有三根蜡烛，吹灭一根，最后还剩几根？" }],
      "reasoning_effort": "high"
    }
  },
  "response": {
    "status": 200,
    "headers": { "content-type": "application/json" },
    "body": {
      "id": "chatcmpl-ds06",
      "object": "chat.completion",
      "created": 1755216000,
      "model": "qwen-plus-upstream",
      "choices": [
        { "index": 0, "message": { "role": "assistant", "content": "剩一根——被吹灭的那根不会继续烧完。" }, "finish_reason": "stop" }
      ],
      "usage": {
        "prompt_tokens": 30,
        "completion_tokens": 82,
        "total_tokens": 112,
        "completion_tokens_details": { "reasoning_tokens": 64 }
      }
    }
  }
}
```

- [ ] **步骤 7.4：验证**

```bash
go test ./internal/degrade/ -run TestImplementedRoutesHaveFixtures -v && go test ./internal/testkit/
```

预期：通过（fixture 可解析、upstream 断言合法）。

- [ ] **步骤 7.5：提交**

```bash
GIT_MASTER=1 git add testdata/routes/openai.chat__dashscope.compatible/parallel_tool_calls.json testdata/routes/openai.chat__dashscope.compatible/structured_output.json testdata/routes/openai.chat__dashscope.compatible/reasoning.json
GIT_MASTER=1 git commit -m "Chat 到 DashScope Compatible 路径 fixture：并行工具、结构化输出与推理" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 8：第三组 fixture（vision_input / audio_input / web_search）

**依赖**：任务 7。
**文件**：同目录三份 fixture。

- [ ] **步骤 8.1：vision_input.json**

新建 `testdata/routes/openai.chat__dashscope.compatible/vision_input.json`：

```json
{
  "name": "openai.chat__dashscope.compatible/vision_input",
  "note": "image URL 与 data URI 两种图像承载：http(s) URL 不计内联字节，data URI 计入；两者都原样交给上游，网关不代下载。",
  "request": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "headers": {
      "authorization": "<redacted>",
      "content-type": "application/json"
    },
    "body": {
      "model": "logical-vision",
      "messages": [
        {
          "role": "user",
          "content": [
            { "type": "text", "text": "这两张图里分别是什么？" },
            { "type": "image_url", "image_url": { "url": "https://example.com/shanghai.png" } },
            { "type": "image_url", "image_url": { "url": "data:image/png;base64,QUJD", "detail": "low" } }
          ]
        }
      ]
    }
  },
  "upstream": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "body": {
      "model": "upstream-model",
      "messages": [
        {
          "role": "user",
          "content": [
            { "type": "text", "text": "这两张图里分别是什么？" },
            { "type": "image_url", "image_url": { "url": "https://example.com/shanghai.png" } },
            { "type": "image_url", "image_url": { "url": "data:image/png;base64,QUJD", "detail": "low" } }
          ]
        }
      ]
    }
  },
  "response": {
    "status": 200,
    "headers": { "content-type": "application/json" },
    "body": {
      "id": "chatcmpl-ds07",
      "object": "chat.completion",
      "created": 1755216000,
      "model": "qwen-vl-upstream",
      "choices": [
        { "index": 0, "message": { "role": "assistant", "content": "第一张是城市天际线，第二张是纯色测试图。" }, "finish_reason": "stop" }
      ],
      "usage": {
        "prompt_tokens": 128,
        "completion_tokens": 18,
        "total_tokens": 146
      }
    }
  }
}
```

- [ ] **步骤 8.2：audio_input.json**

新建 `testdata/routes/openai.chat__dashscope.compatible/audio_input.json`：

```json
{
  "name": "openai.chat__dashscope.compatible/audio_input",
  "note": "内联音频输入：base64 负载计入内联字节，请求体原样交给上游（wire 兼容，音频块不需要重编码）。",
  "request": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "headers": {
      "authorization": "<redacted>",
      "content-type": "application/json"
    },
    "body": {
      "model": "logical-audio",
      "messages": [
        {
          "role": "user",
          "content": [
            { "type": "text", "text": "这段音频里说了什么？" },
            { "type": "input_audio", "input_audio": { "data": "QUJDREVG", "format": "wav" } }
          ]
        }
      ]
    }
  },
  "upstream": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "body": {
      "model": "upstream-model",
      "messages": [
        {
          "role": "user",
          "content": [
            { "type": "text", "text": "这段音频里说了什么？" },
            { "type": "input_audio", "input_audio": { "data": "QUJDREVG", "format": "wav" } }
          ]
        }
      ]
    }
  },
  "response": {
    "status": 200,
    "headers": { "content-type": "application/json" },
    "body": {
      "id": "chatcmpl-ds08",
      "object": "chat.completion",
      "created": 1755216000,
      "model": "qwen-audio-upstream",
      "choices": [
        { "index": 0, "message": { "role": "assistant", "content": "音频里说的是「测试录音」。" }, "finish_reason": "stop" }
      ],
      "usage": {
        "prompt_tokens": 96,
        "completion_tokens": 9,
        "total_tokens": 105
      }
    }
  }
}
```

- [ ] **步骤 8.3：web_search.json（DEGRADE 举证）**

新建 `testdata/routes/openai.chat__dashscope.compatible/web_search.json`：

```json
{
  "name": "openai.chat__dashscope.compatible/web_search",
  "note": "DEGRADE 举证（文件名即能力名，ADR-0001）：web_search_options 整体删除、换写 enable_search: true；search_context_size 与 user_location 丢失、响应无搜索来源，降级头必须可见。请求体特意带 n 与 presence_penalty 两个无关字段——搜索映射不得覆盖客户端已提交的它们。",
  "request": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "headers": {
      "authorization": "<redacted>",
      "content-type": "application/json"
    },
    "body": {
      "model": "logical-fast",
      "messages": [{ "role": "user", "content": "今天上海有什么新闻？" }],
      "web_search_options": {
        "search_context_size": "high",
        "user_location": {
          "type": "approximate",
          "approximate": { "country": "CN", "city": "上海", "region": "上海市", "timezone": "Asia/Shanghai" }
        }
      },
      "n": 2,
      "presence_penalty": 0.5
    }
  },
  "upstream": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "body": {
      "model": "upstream-model",
      "messages": [{ "role": "user", "content": "今天上海有什么新闻？" }],
      "n": 2,
      "presence_penalty": 0.5,
      "enable_search": true
    }
  },
  "response": {
    "status": 200,
    "headers": { "content-type": "application/json" },
    "body": {
      "id": "chatcmpl-ds09",
      "object": "chat.completion",
      "created": 1755216000,
      "model": "qwen-plus-upstream",
      "choices": [
        { "index": 0, "message": { "role": "assistant", "content": "今天上午的要闻：……（示例内容，DashScope Compatible 不返回搜索来源）" }, "finish_reason": "stop" },
        { "index": 1, "message": { "role": "assistant", "content": "另一条摘要：……（n=2 的第二份）" }, "finish_reason": "stop" }
      ],
      "usage": {
        "prompt_tokens": 41,
        "completion_tokens": 58,
        "total_tokens": 99
      }
    }
  }
}
```

- [ ] **步骤 8.4：验证**

```bash
go test ./internal/degrade/ -run TestImplementedRoutesHaveFixtures -v && go test ./internal/testkit/ && ls testdata/routes/openai.chat__dashscope.compatible/
```

预期：通过；目录里恰好九份 fixture（basic / streaming / tool_calling / parallel_tool_calls / structured_output / reasoning / vision_input / audio_input / web_search），golden 子目录尚不存在。

- [ ] **步骤 8.5：提交**

```bash
GIT_MASTER=1 git add testdata/routes/openai.chat__dashscope.compatible/vision_input.json testdata/routes/openai.chat__dashscope.compatible/audio_input.json testdata/routes/openai.chat__dashscope.compatible/web_search.json
GIT_MASTER=1 git commit -m "Chat 到 DashScope Compatible 路径 fixture：视觉、音频与联网搜索" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 9：兑现九项能力（ADR-0001 窗口打开）

**依赖**：任务 2（能力识别）、任务 6-8（fixture 证据已就位）。
**文件**：修改 `internal/degrade/rules_phase1.go`、`internal/degrade/matrix_test.go`；新建 `internal/degrade/chat_dscompat_test.go`；生成 `docs/degradation-matrix.md`。

**窗口声明**：本任务的提交一落地，`openai.chat → dashscope.compatible` 在矩阵里就是已兑现路径。证明兑现的 conformance 回放到任务 13 才提交。**从本任务开始到任务 13 全绿，不得停下做别的工作。**

- [ ] **步骤 9.1：先写聚焦断言（RED）**

新建 `internal/degrade/chat_dscompat_test.go`：

```go
package degrade

import (
	"reflect"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// redeemedChatDSCompat 是 /v1/chat/completions 门在 dashscope.compatible 上
// 兑现的九项能力，按 AllCapabilities 顺序——与 RedeemedAt 的输出顺序一致。
var redeemedChatDSCompat = []canonical.Capability{
	canonical.CapTextGeneration,
	canonical.CapStreaming,
	canonical.CapToolCalling,
	canonical.CapParallelToolCalls,
	canonical.CapStructuredOutput,
	canonical.CapReasoning,
	canonical.CapVisionInput,
	canonical.CapAudioInput,
	canonical.CapWebSearch,
}

// TestChatDSCompatRouteIsWireCompatibleNotHomogeneous 钉死这条路径的身份：
// wire-compatible 只说明不需要重编码，不能推导为语义零损失——
// 它不是同源快通道，不享受快通道的选路特权，降级头由矩阵照常生成。
func TestChatDSCompatRouteIsWireCompatibleNotHomogeneous(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := m.Route(ProtoOpenAIChat, ProviderDashScopeCompatible)
	if !ok {
		t.Fatal("openai.chat -> dashscope.compatible 未注册")
	}
	if r.IsHomogeneous() {
		t.Error("该路径是 wire-compatible 异构转换，不得标记为同源快通道")
	}

	// 设计处置：7 PASS + 2 DEGRADE + 2 REJECT = 11 项可表达能力，设计分 8/11。
	p := r.Preservation(m.Availability(), Endpoint(""))
	if p.Passthrough != 7 || p.Degrade != 2 || p.Reject != 2 {
		t.Errorf("设计处置 = pass %d deg %d rej %d，期望 7/2/2",
			p.Passthrough, p.Degrade, p.Reject)
	}
	if want := 8.0 / 11.0; p.DesignScore() != want {
		t.Errorf("设计保留度 = %.3f，期望 %.3f（8/11）", p.DesignScore(), want)
	}
}

// TestChatDSCompatRedemptionIsExactlyNineCapabilities 钉死兑现集合的精确形状：
// 九项可交付能力；file_input / audio_output 是 REJECT，不在兑现之列。
func TestChatDSCompatRedemptionIsExactlyNineCapabilities(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := m.Route(ProtoOpenAIChat, ProviderDashScopeCompatible)
	if !ok {
		t.Fatal("openai.chat -> dashscope.compatible 未注册")
	}

	if got := r.RedeemedAt(EndpointOpenAIChat); !reflect.DeepEqual(got, redeemedChatDSCompat) {
		t.Errorf("兑现集合 = %v，期望 %v", got, redeemedChatDSCompat)
	}
	for _, c := range []canonical.Capability{canonical.CapFileInput, canonical.CapAudioOutput} {
		if r.Redeems(EndpointOpenAIChat, c) {
			t.Errorf("%q 是 REJECT，不应被兑现", c)
		}
	}

	// 这门此刻九项全兑：可用分与设计分合一，都是 8/11，没有未投放格子。
	p := r.Preservation(m.Availability(), EndpointOpenAIChat)
	if want := 8.0 / 11.0; p.AvailableScore() != want {
		t.Errorf("门 %s 可用分 = %.3f，期望 %.3f", EndpointOpenAIChat, p.AvailableScore(), want)
	}
	if p.Gated() {
		t.Error("九项可交付能力已全部兑现，这门不应再有未投放格子")
	}
}

// TestChatDoorRankingPrefersHomogeneousFastPath 钉死同门选路：
// /v1/chat/completions 这扇门同时被 openai.compat（同源，可用 1.000）与
// dashscope.compatible（wire-compatible，可用 8/11）兑现，选路必须同源优先。
func TestChatDoorRankingPrefersHomogeneousFastPath(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	ranked := m.RankOutbound(ProtoOpenAIChat, []Provider{
		ProviderDashScopeCompatible,
		ProviderOpenAICompat,
	})
	if len(ranked) != 2 || ranked[0] != ProviderOpenAICompat || ranked[1] != ProviderDashScopeCompatible {
		t.Fatalf("同门选路顺序 = %v，期望 [openai.compat dashscope.compatible]", ranked)
	}

	fast := mustRoute(t, m, ProtoOpenAIChat, ProviderOpenAICompat)
	compat := mustRoute(t, m, ProtoOpenAIChat, ProviderDashScopeCompatible)
	fs := fast.Preservation(m.Availability(), EndpointOpenAIChat).AvailableScore()
	cs := compat.Preservation(m.Availability(), EndpointOpenAIChat).AvailableScore()
	if fs <= cs {
		t.Errorf("同源门可用分 %.3f 应严格高于 wire-compatible 门 %.3f", fs, cs)
	}
}
```

运行：

```bash
go test ./internal/degrade/ -run 'TestChatDSCompat' -v
```

预期：`TestChatDSCompatRouteIsWireCompatibleNotHomogeneous` 通过（设计声明早已存在），`TestChatDSCompatRedemptionIsExactlyNineCapabilities` **失败**（兑现集合为空），`TestChatDoorRankingPrefersHomogeneousFastPath` **失败**（路径未通车，RankOutbound 只剩一个候选）。正确的 RED。

- [ ] **步骤 9.2：rules_phase1.go 兑现与 note 收窄**

在 `internal/degrade/rules_phase1.go` 中做两处修改。

其一，常量块追加（放在 `noteSearchSwitch` 之后）：

```go
	noteSearchSwitchCompat = "DashScope Compatible 只有 enable_search 布尔开关：搜索上下文大小 " +
		"search_context_size 与用户位置 user_location 在此丢失，响应也不返回搜索来源；" +
		"仅开关本身被映射"
```

（设计文档要求降级说明点名丢失的具体项目——搜索上下文大小、用户位置、来源列表——而不是笼统的「参数不兼容」；`noteSearchSwitch` 仍由 chat→dashscope.native 等路径使用，不改它。）

其二，`chatToDSCompat` 的构造整体替换为：

```go
	chatToDSCompat := NewRoute(ProtoOpenAIChat, ProviderDashScopeCompatible).
		Pass(
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapParallelToolCalls,
			canonical.CapVisionInput,
			canonical.CapAudioInput,
			canonical.CapReasoning,
		).
		Degrade("DashScope 兼容模式支持 json_object，但不保证 strict json_schema 校验",
			canonical.CapStructuredOutput).
		Degrade(noteSearchSwitchCompat, canonical.CapWebSearch).
		Reject(noteFileRefBound, canonical.CapFileInput).
		Reject("兼容模式不返回音频；音频输出须经 Qwen-Omni Realtime 或 Native 端点",
			canonical.CapAudioOutput).
		// 兑现门槛是端到端 fixture 通过（ADR-0001）：九份用例在
		// testdata/routes/openai.chat__dashscope.compatible/，回放与上游请求
		// 断言在 internal/gateway/chat_dscompat_conformance_test.go。
		// file_input / audio_output 是 REJECT，不在兑现之列。
		Redeem(EndpointOpenAIChat,
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapParallelToolCalls,
			canonical.CapStructuredOutput,
			canonical.CapReasoning,
			canonical.CapVisionInput,
			canonical.CapAudioInput,
			canonical.CapWebSearch,
		)
```

- [ ] **步骤 9.3：两份显式白名单**

在 `internal/degrade/matrix_test.go` 中：

其一，`TestImplementedRoutesAreExplicit` 的 `want` 改为：

```go
	// 已转正：OpenAI 族两条同源直通、DashScope Native 文本生成同源直通，
	// 以及 Chat 到 DashScope Compatible 的 wire-compatible 路径（非同质快通道）。
	// 其余仍为 PLANNED，anthropic 等排在其后。
	want := map[string]bool{
		string(ProtoOpenAIResponses) + " -> " + string(ProviderOpenAICompat):    true,
		string(ProtoOpenAIChat) + " -> " + string(ProviderOpenAICompat):         true,
		string(ProtoDashScopeNative) + " -> " + string(ProviderDashScopeNative): true,
		string(ProtoOpenAIChat) + " -> " + string(ProviderDashScopeCompatible):  true,
	}
```

其二，`TestRedeemedCapabilitiesAreExplicit` 的 `want` 追加一项（放在 Native 两门之前或之后均可，保持 map 字面量风格）：

```go
		string(ProtoOpenAIChat) + " -> " + string(ProviderDashScopeCompatible) +
			" @ " + string(EndpointOpenAIChat): {
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapParallelToolCalls,
			canonical.CapStructuredOutput,
			canonical.CapReasoning,
			canonical.CapVisionInput,
			canonical.CapAudioInput,
			canonical.CapWebSearch,
		},
```

并把该测试开头注释改为：

```go
	// OpenAI 两条同源直通各一扇门，字节级转发，可表达的全部兑现；
	// Chat 到 DashScope Compatible 兑现九项可交付能力（两项 REJECT 不在其列）；
	// Native 投放了文本生成与多模态生成两扇门，各 5 项，名单彼此独立。
```

- [ ] **步骤 9.4：重新生成矩阵文档**

```bash
make matrix-update
```

预期：`docs/degradation-matrix.md` 被重新生成。用 `GIT_MASTER=1 git diff --stat docs/degradation-matrix.md` 查看：变化应集中在 `openai.chat → dashscope.compatible` 一行（状态转为已实现、新增端点可用分）与 `openai.responses → dashscope.compatible` 一行的搜索 note（派生路径继承新 note）。若出现其他路径的变化，停下报告。

- [ ] **步骤 9.5：验证（本任务内收敛）**

```bash
go test ./internal/degrade/ -v 2>&1 | grep -E '^(=== RUN|--- FAIL|FAIL|ok)' | tail -20
go build ./... && go test ./...
```

预期：degrade 全部通过（含三条新断言、两份白名单、fixture 门槛、文档同步）；全仓测试绿。此时已提交状态绿，但 conformance 证据尚未落地——继续任务 10，不停。

- [ ] **步骤 9.6：提交**

```bash
GIT_MASTER=1 git add internal/degrade/rules_phase1.go internal/degrade/matrix_test.go internal/degrade/chat_dscompat_test.go docs/degradation-matrix.md
GIT_MASTER=1 git commit -m "兑现 openai.chat 到 dashscope.compatible 九项能力：白名单与矩阵文档同步" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 10-12 前置：写回放与负例测试文件（工作树状态，暂不提交）

**这一步是任务 10-13 的共同准备**：golden 由这两个测试文件生成，所以先写文件、生成并审阅 golden、提交 golden（任务 10-12），最后提交测试文件（任务 13）。

- [ ] **步骤 P.1：conformance 回放文件**

新建 `internal/gateway/chat_dscompat_conformance_test.go`：

```go
package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/testkit"
)

// chatDSCompatRouteFixtures 是 Chat -> DashScope Compatible wire-compatible 路径的 fixture 目录。
const chatDSCompatRouteFixtures = "../../testdata/routes/openai.chat__dashscope.compatible"

// chatDSCompatDegradedHeaders 按用例名钉死降级头必须包含的能力项。
// 独立于 golden 断言：golden 整体漂移时，降级语义的丢失仍要在这里单独咬住。
var chatDSCompatDegradedHeaders = map[string]string{
	"structured_output": "structured_output=",
	"web_search":        "web_search=",
}

// TestChatDSCompatRouteConformance 回放 wire-compatible 路径的全部 fixture。
//
// 与同源直通的回放不同：除了客户端响应 golden，还必须逐项断言上游实际收到的
// method / path / 鉴权 / 请求体——只比客户端响应会漏掉「Provider 根本没做映射、
// fixture 仍返回成功」这种假绿。fixture 的 upstream 声明就是为这件事存在的，
// 缺失即失败。
func TestChatDSCompatRouteConformance(t *testing.T) {
	for _, f := range testkit.LoadDir(t, chatDSCompatRouteFixtures) {
		t.Run(caseName(f.Name), func(t *testing.T) {
			if f.Upstream == nil {
				t.Fatal("异构路径的 fixture 必须带 upstream 断言，否则上游映射无从对账")
			}

			var (
				gotMethod  string
				gotPath    string
				gotHeader  http.Header
				gotBody    []byte
				gotBodyErr error
			)
			// 读 body 的错误只记录、不在这里终止：闭包跑在 httptest 服务器的
			// goroutine 上，t.Fatal 在非测试 goroutine 里只会结束该 goroutine。
			up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotPath = r.URL.Path
				gotHeader = r.Header.Clone()
				gotBody, gotBodyErr = io.ReadAll(r.Body)
				writeFixtureResponse(t, w, f)
			})
			hs := newChatDSCompatHarness(t, up)

			body, err := json.Marshal(f.Request.Body)
			if err != nil {
				t.Fatal(err)
			}
			rec := hs.do(t, string(body), true)

			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200: %s", rec.Code, rec.Body.String())
			}

			if gotBodyErr != nil {
				t.Fatalf("读取上游收到的 body 失败: %v", gotBodyErr)
			}
			if gotMethod != f.Upstream.Method {
				t.Errorf("上游收到 method %q，期望 %q", gotMethod, f.Upstream.Method)
			}
			if gotPath != f.Upstream.Path {
				t.Errorf("上游收到路径 %q，期望 %q", gotPath, f.Upstream.Path)
			}
			// harness 的第一个凭据池名是 "a"，secret 是 "sk-a"。
			if auth := gotHeader.Get("Authorization"); auth != "Bearer sk-a" {
				t.Errorf("上游收到 Authorization %q，期望网关凭据 Bearer sk-a", auth)
			}
			testkit.AssertJSONEqual(t, f.Upstream.Body, gotBody, "上游收到的请求体语义不符")

			// 降级头独立于 golden 断言：该降级的必须逐项可见，不该降级的不许出现。
			wantDegraded := chatDSCompatDegradedHeaders[caseName(f.Name)]
			gotDegraded := rec.Header().Get(degrade.DegradationHeader)
			if wantDegraded == "" {
				if gotDegraded != "" {
					t.Errorf("该用例不应有降级头，实际 %q", gotDegraded)
				}
			} else if !strings.Contains(gotDegraded, wantDegraded) {
				t.Errorf("%s 应包含 %q，实际 %q", degrade.DegradationHeader, wantDegraded, gotDegraded)
			}

			golden := filepath.Join(chatDSCompatRouteFixtures, "golden", caseName(f.Name)+".txt")
			testkit.Golden(t, golden, []byte(renderResult(rec)))
		})
	}
}
```

- [ ] **步骤 P.2：负例文件**

新建 `internal/gateway/chat_dscompat_negative_test.go`：

```go
package gateway

import (
	"net/http"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// TestChatDSCompatRejectsFileInput 固化 file_input 在这条路上是 REJECT：
// 422 说「改请求」，且必须在矩阵闸门就拦下，一个字节都不出门。
func TestChatDSCompatRejectsFileInput(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSCompatHarness(t, up)

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"处理这个文件"},
		{"type":"file","file":{"file_id":"file-abc"}}]}]}`, true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——REJECT 必须在矩阵闸门拦下", n)
	}
	if !strings.Contains(rec.Body.String(), string(canonical.CapFileInput)) {
		t.Errorf("错误应点名能力 file_input: %s", rec.Body.String())
	}
}

// TestChatDSCompatRejectsAudioOutput 固化 audio_output 在这条路上是 REJECT：
// 兼容模式不返回音频，想要音频输出请走 Realtime 或 Native 端点。
func TestChatDSCompatRejectsAudioOutput(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSCompatHarness(t, up)

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"modalities":["text","audio"]}`, true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——REJECT 必须在矩阵闸门拦下", n)
	}
	if !strings.Contains(rec.Body.String(), string(canonical.CapAudioOutput)) {
		t.Errorf("错误应点名能力 audio_output: %s", rec.Body.String())
	}
}

// TestChatDSCompatPlannedCandidatesStay501 固化「路由只包含尚未实现候选时维持 501」：
// dashscope.compatible 转正后，Chat 入站的未实现哨兵指向 anthropic.messages——
// 它仍是 PLANNED，501 说「等实现」，上游一个字节都不该收到。
func TestChatDSCompatPlannedCandidatesStay501(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatHarness(t, false, up)

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, true)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——PLANNED 路径不得出门", n)
	}
}

// TestChatDSCompatInvalidWebSearchOptionsIs400 固化形态非法的搜索选项在入站解码就拒：
// 不得默认为开启搜索，更不得打到上游。
func TestChatDSCompatInvalidWebSearchOptionsIs400(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSCompatHarness(t, up)

	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"web_search_options":{"unknown_field":1}}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"web_search_options":"yes"}`,
	} {
		rec := hs.do(t, body, true)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("状态码 = %d，期望 400: %s", rec.Code, rec.Body.String())
		}
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——入站解码失败不得出门", n)
	}
}
```

- [ ] **步骤 P.3：生成九份 golden**

```bash
go test ./internal/gateway/ -run 'TestChatDSCompatRouteConformance' -update -v
```

预期：九个用例全部 PASS，`testdata/routes/openai.chat__dashscope.compatible/golden/` 下生成九份 `.txt`。确认 `GIT_MASTER=1 git status --porcelain` 中新增的 golden 恰好九份，没有碰其他目录的 golden。

- [ ] **步骤 P.4：人工审阅 golden（强制）**

逐份检查（`GIT_MASTER=1 git diff --no-index /dev/null testdata/routes/openai.chat__dashscope.compatible/golden/basic.txt` 或直接读文件）：

- 全部九份第一行是 `status: OK`，随后是 `Content-Type` 行与 `---` 分隔线。
- `basic.txt`：含 `X-Ratelimit-Remaining-Tokens: 1999918`，body 与 fixture `response.body` 逐字节一致。参考形状：

  ```text
  status: OK
  Content-Type: application/json
  X-Ratelimit-Remaining-Tokens: 1999918
  ---
  {
        "id": "chatcmpl-ds01",
        ...（与 fixture response.body 逐字节一致）...
      }
  ```

- `streaming.txt`：`Content-Type: text/event-stream`，body 是规范化后的六条 `data:` 事件（五条 chunk + `data: [DONE]`），每条事件之间一个空行，data 负载与 fixture 逐字一致。
- `structured_output.txt`：含 `X-Omugw-Degraded: structured_output=DashScope 兼容模式支持 json_object，但不保证 strict json_schema 校验`。
- `web_search.txt`：含 `X-Omugw-Degraded: web_search=DashScope Compatible 只有 enable_search 布尔开关：搜索上下文大小 search_context_size 与用户位置 user_location 在此丢失，响应也不返回搜索来源；仅开关本身被映射`。
- 其余五份（tool_calling / parallel_tool_calls / reasoning / vision_input / audio_input）：只有 `Content-Type: application/json`，无降级头、无限流头，body 与各自 fixture 的 `response.body` 逐字节一致。

任何一处不符：停下排查，不得带疑点继续。

- [ ] **步骤 P.5：全量回放验证**

```bash
go test ./internal/gateway/ -run 'TestChatDSCompat' -v
```

预期：conformance 九用例 + 四条负例全部 PASS。

---

## 任务 10：第一组 golden（basic / streaming / tool_calling）

**依赖**：任务 9、前置步骤 P.1-P.5。

- [ ] **步骤 10.1：提交**

```bash
GIT_MASTER=1 git add testdata/routes/openai.chat__dashscope.compatible/golden/basic.txt testdata/routes/openai.chat__dashscope.compatible/golden/streaming.txt testdata/routes/openai.chat__dashscope.compatible/golden/tool_calling.txt
GIT_MASTER=1 git commit -m "Chat 到 DashScope Compatible golden：基础、流式与工具调用" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

## 任务 11：第二组 golden（parallel_tool_calls / structured_output / reasoning）

- [ ] **步骤 11.1：提交**

```bash
GIT_MASTER=1 git add testdata/routes/openai.chat__dashscope.compatible/golden/parallel_tool_calls.txt testdata/routes/openai.chat__dashscope.compatible/golden/structured_output.txt testdata/routes/openai.chat__dashscope.compatible/golden/reasoning.txt
GIT_MASTER=1 git commit -m "Chat 到 DashScope Compatible golden：并行工具、结构化输出与推理" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

## 任务 12：第三组 golden（vision_input / audio_input / web_search）

- [ ] **步骤 12.1：提交**

```bash
GIT_MASTER=1 git add testdata/routes/openai.chat__dashscope.compatible/golden/vision_input.txt testdata/routes/openai.chat__dashscope.compatible/golden/audio_input.txt testdata/routes/openai.chat__dashscope.compatible/golden/web_search.txt
GIT_MASTER=1 git commit -m "Chat 到 DashScope Compatible golden：视觉、音频与联网搜索" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 13：提交一致性回放与负例测试（ADR-0001 窗口关闭）

**依赖**：任务 10-12（golden 已就位）。

- [ ] **步骤 13.1：验证后提交**

```bash
go test ./internal/gateway/ -v 2>&1 | grep -E '^(--- FAIL|FAIL|ok)' | tail -5
gofmt -l internal/gateway/
```

预期：gateway 全部通过；`gofmt -l` 无输出。

```bash
GIT_MASTER=1 git add internal/gateway/chat_dscompat_conformance_test.go internal/gateway/chat_dscompat_negative_test.go
GIT_MASTER=1 git commit -m "Chat 到 DashScope Compatible 一致性回放与负例：上游请求逐条对账" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

- [ ] **步骤 13.2：窗口关闭确认**

```bash
go test ./... && GIT_MASTER=1 git status
```

预期：全仓测试绿；工作树干净（除任务 14 待做的文档外无未提交变更）。**至此 ADR-0001 窗口关闭**——兑现有了回放证据。在此之前不允许停下。

---

## 任务 14：知识库同步（README 与根 AGENTS）

**依赖**：任务 13。
**文件**：修改 `README.md`、根 `AGENTS.md`。

- [ ] **步骤 14.1：README 状态块**

把 `README.md` 顶部的状态引用块改为：

```markdown
> **状态：M1 进行中——OpenAI 族两条同源直通、DashScope Native 两个端点与 Chat → DashScope Compatible 已转正，网关可用。**
>
> 降级矩阵登记了 14 条转换路径，**其中 4 条已实现**：
> `openai.responses → openai.compat`、`openai.chat → openai.compat` 与
> `dashscope.native → dashscope.native`（均为同源直通），以及第一条异构路径
> `openai.chat → dashscope.compatible`（wire-compatible：请求仍是 Chat 线格式，
> 但语义异构，不是同源快通道；在 `/v1/chat/completions` 门兑现 9 项能力，
> 设计分与可用分均为 8/11 ≈ 0.727）。
> 其中 DashScope Native 路径投放了文本生成与多模态生成两个端点，
> 每扇门各兑现 5 项能力（多模态门为视觉 / 音频 / 视频输入加文本与流式）。
> 其余仍是 `PLANNED`，
> 打过去会得到 `501`——那是「还没建好」，不是「不支持」。
```

- [ ] **步骤 14.2：根 AGENTS 当前状态**

把根 `AGENTS.md` 的 `## NOTES` 第一条「当前状态」改为：

```markdown
- **当前状态**：14 条路径已登记，4 条已通车（`openai.responses →
  openai.compat`、`openai.chat → openai.compat`、`dashscope.native →
  dashscope.native`，均为同源直通；以及第一条异构路径 `openai.chat →
  dashscope.compatible`）。兑现粒度是**端点 × 能力**：OpenAI 两条直通在各自的
  唯一门上兑现全部可表达能力；DashScope Native 投放了两个端点——文本生成与
  多模态生成，每门 18 项中 5 项；Chat 到 DashScope Compatible 是 wire-compatible
  而非同源——请求保持 Chat 线格式、只做定点修补（model 改写与
  web_search_options → enable_search），在 `/v1/chat/completions` 门兑现 9 项
  可交付能力（7 PASS + 2 DEGRADE），file_input / audio_output 维持 REJECT 422，
  设计与可用均为 8/11 ≈ 0.727。其余 Native POST 端点由 `POST /api/v1/` 兜底
  返回 DashScope 协议化 501。其余 10 条路径打过去仍是 501。
```

- [ ] **步骤 14.3：根 AGENTS 反模式补一条语义边界**

在根 `AGENTS.md` 的 `## ANTI-PATTERNS (THIS PROJECT)` 一节末尾追加：

```markdown
- **不要**把 wire-compatible 读成同源：`openai.chat → dashscope.compatible` 复用 Chat
  线格式只是因为不需要重编码，语义是异构的（搜索选项降成开关、strict schema 无全局
  保证），不得 `MarkHomogeneous`，降级头由矩阵照常生成。
```

- [ ] **步骤 14.4：验证**

```bash
GIT_MASTER=1 git diff --stat README.md AGENTS.md
```

预期：只有这两个文件被修改；改动只涉及状态描述与反模式清单，不动其他章节。

- [ ] **步骤 14.5：提交**

```bash
GIT_MASTER=1 git add README.md AGENTS.md
GIT_MASTER=1 git commit -m "知识库跟上第四条通车路径：wire-compatible 不是同源快通道" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 15：最终闸门与手工 QA（不产生提交）

**依赖**：任务 1-14 全部落地。

- [ ] **步骤 15.1：格式与静态检查**

```bash
gofmt -l .
go vet ./...
go build ./...
```

预期：三条命令全部无输出、退出码 0。

- [ ] **步骤 15.2：全量测试与矩阵闸门**

```bash
make test
make test-race
make matrix
make check
```

预期：全部通过。`make check` 覆盖 fmt-check + vet + test + matrix，等价 CI。

- [ ] **步骤 15.3：LSP 诊断**

对全部改动过的包跑 `lsp_diagnostics`，严重度取 `error`：

- `internal/provider/dashscopecompat`
- `internal/gateway`
- `internal/protocol/openaichat`
- `internal/testkit`
- `internal/degrade`

预期：零错误。

- [ ] **步骤 15.4：文件大小复查**

```bash
wc -l internal/provider/dashscopecompat/*.go internal/gateway/chat_dscompat_*.go internal/degrade/chat_dscompat_test.go
```

预期：实现文件（`dashscopecompat.go`）低于 250 行；测试文件与既有惯例持平
（`passthrough_test.go` 为 425 行，`conformance_test.go` 为 344 行，测试文件不设硬上限）。
任一实现文件超限，拆分后再跑步骤 15.2。

- [ ] **步骤 15.5：手工 QA 准备——本地假上游**

```bash
mkdir -p /tmp/omugw-qa/fakeup
```

写入 `/tmp/omugw-qa/fakeup/main.go`：

```go
// fakeup 是手工 QA 用的本地假上游：打印收到的 method / path / 鉴权 / body，
// 按 Accept 头回 Chat 响应（流式或非流式）。不打任何真实上游。
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		log.Printf("upstream received: %s %s | Authorization: %s | body: %s",
			r.Method, r.URL.Path, r.Header.Get("Authorization"), body)

		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			f := w.(http.Flusher)
			for _, d := range []string{
				`{"id":"chatcmpl-qa","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"你"}}]}`,
				`{"id":"chatcmpl-qa","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"好"}}]}`,
				`{"id":"chatcmpl-qa","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
				`[DONE]`,
			} {
				fmt.Fprintf(w, "data: %s\n\n", d)
				f.Flush()
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Ratelimit-Remaining-Tokens", "12345")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-qa","object":"chat.completion","model":"qwen-plus","choices":[{"index":0,"message":{"role":"assistant","content":"你好！我是本地假上游。"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":9,"total_tokens":14}}`))
	})
	log.Println("fake upstream listening on 127.0.0.1:19099")
	log.Fatal(http.ListenAndServe("127.0.0.1:19099", nil))
}
```

写入 `/tmp/omugw-qa/config.yaml`（网关配置，凭据全部是本地假值）：

```yaml
server:
  addr: "127.0.0.1:18080"
  metrics_addr: "127.0.0.1:19090"
auth:
  keys:
    - id: qa
      key: qa-local-key-0123456789
credentials:
  dscompat-pool:
    - id: qa-cred
      secret: fake-upstream-secret
providers:
  - endpoint: dscompat-local
    kind: dashscope.compatible
    base_url: http://127.0.0.1:19099
    credential_pool: dscompat-pool
models:
  - match: "*"
    targets:
      - endpoint: dscompat-local
        upstream_model: qwen-plus
```

- [ ] **步骤 15.6：启动假上游与网关**

两个终端（或后台进程）：

```bash
cd /tmp/omugw-qa/fakeup && go run main.go 2>&1 | tee /tmp/omugw-qa/fakeup.log
```

```bash
make build && ./bin/omugw -config /tmp/omugw-qa/config.yaml
```

预期：网关日志出现「omugw 已启动」，`routes_implemented=4`；「已投放端点」条目里
`openai.chat -> dashscope.compatible @ /v1/chat/completions` 的 `available_score` 约为 0.727。

- [ ] **步骤 15.7：六探针**

探针 1——健康检查：

```bash
curl -s http://127.0.0.1:18080/healthz
```

预期：`{"status":"ok"}`。

探针 2——基础转发（模型改写 + 凭据覆盖）：

```bash
curl -s -X POST http://127.0.0.1:18080/v1/chat/completions \
  -H "Authorization: Bearer qa-local-key-0123456789" \
  -H "Content-Type: application/json" \
  -d '{"model":"any","messages":[{"role":"user","content":"你好"}]}'
```

预期：200，响应体是假上游的 chat.completion；`fakeup.log` 新增一行，其中 path 为
`/v1/chat/completions`、`Authorization: Bearer fake-upstream-secret`（网关凭据覆盖了
客户端凭据）、body 里 `"model":"qwen-plus"`（逻辑名被改写）。

探针 3——搜索映射与降级头：

```bash
curl -si -X POST http://127.0.0.1:18080/v1/chat/completions \
  -H "Authorization: Bearer qa-local-key-0123456789" \
  -H "Content-Type: application/json" \
  -d '{"model":"any","messages":[{"role":"user","content":"今天有什么新闻"}],"web_search_options":{"search_context_size":"high"}}'
```

预期：响应头包含 `X-Omugw-Degraded: web_search=DashScope Compatible 只有 enable_search 布尔开关：……`；
`fakeup.log` 里该请求 body 含 `"enable_search":true` 且**不含** `web_search_options`。

探针 4——结构化输出降级：

```bash
curl -si -X POST http://127.0.0.1:18080/v1/chat/completions \
  -H "Authorization: Bearer qa-local-key-0123456789" \
  -H "Content-Type: application/json" \
  -d '{"model":"any","messages":[{"role":"user","content":"抽取结构化数据"}],"response_format":{"type":"json_schema","json_schema":{"name":"x","strict":true,"schema":{"type":"object"}}}}'
```

预期：响应头包含 `X-Omugw-Degraded: structured_output=DashScope 兼容模式支持 json_object，但不保证 strict json_schema 校验`；
`fakeup.log` 里 `response_format` 原样保留（schema 未被改写）。

探针 5——file_input 422 且不出门：

```bash
before=$(grep -c 'upstream received' /tmp/omugw-qa/fakeup.log)
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:18080/v1/chat/completions \
  -H "Authorization: Bearer qa-local-key-0123456789" \
  -H "Content-Type: application/json" \
  -d '{"model":"any","messages":[{"role":"user","content":[{"type":"text","text":"处理文件"},{"type":"file","file":{"file_id":"file-1"}}]}]}'
after=$(grep -c 'upstream received' /tmp/omugw-qa/fakeup.log)
echo "upstream calls: $before -> $after"
```

预期：状态码 `422`；`upstream calls` 前后相等（零次出门）。

探针 6——流式：

```bash
curl -sN -X POST http://127.0.0.1:18080/v1/chat/completions \
  -H "Authorization: Bearer qa-local-key-0123456789" \
  -H "Content-Type: application/json" \
  -d '{"model":"any","messages":[{"role":"user","content":"你好"}],"stream":true,"stream_options":{"include_usage":true}}'
```

预期：逐条收到三条 chunk（「你」「好」+ 带 usage 的空 choices）与 `data: [DONE]` 收尾；
`fakeup.log` 里该请求 body 保留 `"stream":true` 与 `stream_options`。

- [ ] **步骤 15.8：清理**

停掉网关与假上游进程；`/tmp` 下的 QA 产物不入库。确认仓库状态：

```bash
GIT_MASTER=1 git status && GIT_MASTER=1 git log --oneline -16
```

预期：工作树干净；`5931c9f` 之上恰好 14 个新提交，主题与「提交总览」表逐条一致。

---

## 完成标准自查（对照设计文档逐条勾选）

| 设计文档要求 | 证据位置 |
|---|---|
| 9 项能力在 `EndpointOpenAIChat` 有逐项 fixture 证据并显式兑现 | 任务 6-9；`TestChatDSCompatRedemptionIsExactlyNineCapabilities` |
| Web Search 参数损失经矩阵 note、响应头与 fixture 可见 | `noteSearchSwitchCompat`、`web_search.txt` golden、探针 3 |
| 除明确 patch 外 Chat 原始请求字段保持；响应不经重编码 | `TestUnrelatedFieldsPreserved`、九份 fixture 的 upstream 断言 |
| file_input / audio_output 触达上游前稳定 422 | `TestChatDSCompatRejectsFileInput` / `TestChatDSCompatRejectsAudioOutput`、探针 5 |
| `make check`、`make test-race`、`go build ./...`、LSP 诊断全过 | 任务 15 步骤 15.1-15.3 |
| 本地 fake upstream 真实 HTTP 回放通过；真实 smoke 可选不入 CI | 任务 15 六探针 |
| 算术自洽：7 PASS + 2 DEGRADE + 2 REJECT = 11 项可表达能力；设计分与门可用分 = (7 + 2×0.5) / 11 = 8/11 ≈ 0.727 | `TestChatDSCompatRouteIsWireCompatibleNotHomogeneous`、`TestChatDSCompatRedemptionIsExactlyNineCapabilities` |
