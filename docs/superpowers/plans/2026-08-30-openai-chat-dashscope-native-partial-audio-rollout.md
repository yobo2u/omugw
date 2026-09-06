# OpenAI Chat → DashScope Native 音频能力部分投放实施计划（任务 17 到 23）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 按已批准的部分投放设计完成 `openai.chat → dashscope.native` 的收尾：交付恰好 11 份真实 fixture 的举证（`audio_input` 与 `multi_candidate_stream` 退出本期名单；当前 11 份已全部真实录制并通过严格校验），在 `/v1/chat/completions` 门精确兑现 8 项能力（不兑现 `CapAudioInput`），门可用分 6.5/11 ≈ 0.591 且保持 `Gated()`，`audio_input` 请求由矩阵在触达上游前返回 501 `not_implemented`，`stream=true` 搭 `n>1` 由出站守卫在触达上游前拒成 422（OpenAI 线格式 `invalid_request_error` / `unsupported_capability`，`param` 点名 `n`），全程不新增 Capability、不新增依赖、不调用任何音频模型。

**Architecture:** 任务 1 到 16 已落地：入站 `openaichat.Decode` 严格解码，出站 `internal/provider/dashscopenative` Composite（Native 入站直通 + Chat 入站完整重编码），网关侧媒体过滤与 DashScope 专用 usage 回调，录制器（转发代理 + 脱敏 + 逐项在线断言 + 失败不落盘）与已录 fixture 均在仓库里。本计划只做证据与投放：录制名单与录制矩阵收窄到与生产兑现一致的 8 项（`audio_input` 由离线与 live 两个 501 探针看守；`multi_candidate_stream` 因出站守卫对 `stream=true` 搭 `n>1` 的上游前 422 而退出名单——录一个网关自己都不放行的请求产不出任何证据），11 份真实 fixture 已录齐，兑现、回放与负例已在工作树完成；真实 smoke 与知识库同步仍按后续任务执行。生产矩阵 `Phase1` 是裁决权威；录制矩阵 `recordMatrix` 是脚手架，收窄后与生产兑现逐字一致，不得替生产兑现任何能力。

**Tech Stack:** Go 1.25（仅现有三个直接依赖 `yaml.v3` / `prometheus client` / `go-cmp`，不新增），标准库 `testing` + `internal/testkit` 离线回放 + golden，Make 目标（`test` / `test-race` / `matrix` / `matrix-update` / `check` / `smoke`）。真实录制与真实 smoke 需要真实 DashScope 凭据；离线 CI 只回放脱敏后的真实录制。

**Spec:** [`docs/superpowers/specs/2026-08-30-openai-chat-dashscope-native-partial-audio-rollout-design.md`](../specs/2026-08-30-openai-chat-dashscope-native-partial-audio-rollout-design.md)（状态「已批准」，随提交 `3b479b7` 入库，下文简称「部分投放设计」）。原始计划 [`docs/superpowers/plans/2026-08-25-openai-chat-dashscope-native.md`](2026-08-25-openai-chat-dashscope-native.md)（未跟踪、只读参考，下文简称「原计划」）已执行完任务 1 到 16；本计划是任务 17 到 23 在部分投放设计下的决策完整展开。执行中若发现本计划与部分投放设计冲突，以设计为准并停下报告，不得自行改设计。

---

## 基线与现状（执行前必读）

- **工作树**：`/Users/yobo/.config/superpowers/worktrees/omugw/openai-chat-dashscope-native-design`，当前 HEAD `d547176`。
- **已完成**：原计划任务 1 到 16 全部落地（波次 0 到 3 的实现提交 `e3a1763..fc0c3f2`，录制器基建 `8ef707d..bf45dba`）。部分投放设计已批准并入库（`3b479b7`）。任务 17 已推进：名单首轮收窄（`audio_input` 退出）与离线音频探针（`65ce778`）、live 音频哨兵（`a55073f`）、`combined.json`（`d547176`）已提交；`basic`、`streaming`、`tool_calling`、`vision_input` 四份 fixture 已入库（`a79b687..1d6a7b2`）。
- **录制进度**：本期 11 份 fixture 已全部真实录制。`parallel_tool_calls_default` 已在单次付费授权下用精确子测试锚点录制；客户端请求未显式提交 `parallel_tool_calls`，上游请求显式注入 `true`，凭据字段均为 `<redacted>`。
- **工作树未提交变更**：任务 17 的名单再收窄、出站守卫、纵深防御与 dispatch 错误归属修正；任务 18 的 8 项兑现、白名单与矩阵文档；任务 19 的 11-fixture conformance、负例、固定时钟与 11 份 golden。任务 18/19 已分别通过独立规格与质量审查，审查 findings 已关闭。
- **待完成**：已完成内容仍待按提交总览整理本地提交；任务 20 的离线 smoke 骨架已实现并通过独立双审查，提交步骤尚未执行；任务 21 到 23 尚未执行。任何真实 smoke 仍需单独明确授权。
- **路径现状**：`chatToDSNative` 已在 `/v1/chat/completions` 门兑现 8 项，`CapAudioInput` 保持设计 `PASS` 但未兑现；两份显式白名单已同步，门可用分 6.5/11 ≈ 0.591 且保持 `Gated()`。
- **录制器现状**：`recordCases` 恰好 11 项，`recordMatrix` 兑现同一组 8 项。`TestRecordedFixturesAreValid` 已在允许唯一 `golden/` 子目录的同时对 11 份 JSON 双向严格对账并转绿；conformance 另对 fixture 文件名、内部 name 与 golden 名单逐项绑定。

## 两个计划文件的不变量（全程不得破坏）

1. **原计划文件只读**：`docs/superpowers/plans/2026-08-25-openai-chat-dashscope-native.md` 保持未跟踪、逐字节不变。任何步骤都不得编辑它、不得 `git add` 它。
2. **本计划文件不提交**：`docs/superpowers/plans/2026-08-30-openai-chat-dashscope-native-partial-audio-rollout.md` 是规划产物，与生产代码无关，同样保持未跟踪、不提交。
3. 任务 23 的仓库状态核对会把这两条当验收项：`git status --porcelain` 恰好两条未跟踪记录，都是计划文件。

## 前置约定（每个任务都适用）

1. **语言与注释**：代码注释与文档一律中文，写「防的是什么」而不是「做了什么」，沿用仓库既有风格（参见 `internal/degrade/matrix.go`、`tests/smoke/dsnative_fixture_test.go` 的现有注释）。
2. **严格 TDD**：先写失败测试（RED），运行并确认它因**正确的原因**失败（断言不满足、符号未定义，而不是拼写错误或导入缺失），再写最小实现（GREEN），再跑聚焦与更广验证。纯数据任务（fixture、golden）与纯生成任务（矩阵文档）按各自的验证方式执行，命令与预期输出在每个步骤里给出。
3. **每个提交必须绿**：任何提交落地前，`go build ./...` 与该提交影响的包测试必须通过（除非该步骤明确标注 RED 中间态且同一任务内收敛）。**注意**：`TestRecordedFixturesAreValid` 在第 11 份 fixture 入库前故意保持红；当前 11 份已齐并转绿。录制期间的逐用例闸门仍是录制器侧断言（`assertRecordedCase` + 失败不落盘），不得用删文件、改名单之外的方式制造假绿。
4. **提交纪律**：每条 `git` 命令以 `GIT_MASTER=1` 前缀执行。每个提交带 Sisyphus 署名脚注与 Co-authored-by trailer，模板：

   ```bash
   GIT_MASTER=1 git add <files>
   GIT_MASTER=1 git commit -m "<任务给出的提交信息>" \
     -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
     -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
   ```

5. **不许推送**：全程不得 `git push`。保持本地状态。
6. **golden 纪律**：重写 golden 后必须逐份人工审阅 diff，确认内容正是预期语义，再提交。本计划只允许用定向 `-update` 重写本路径的 golden，不得 `make golden-update` 全量重写。
7. **音频纪律（部分投放设计硬约束）**：
   - 不得为 Qwen-Omni 增加 Native 特例，不得更改 `dashscope.compatible` 路径，不得把 Compatible 证据用于 Native 兑现；
   - 不得调用 `qwen-audio-turbo` 或任何音频模型（录制名单里没有音频用例，模型角色表里也没有音频角色，live 501 探针的靶子模型是文本角色）；
   - 不得构造假 fixture，不得把多份互不相容的模型证据拼成「可组合」；
   - 不得删除或削弱生产音频编解码代码（`internal/protocol/dashscopenative/encode_messages.go` 的 audio 块编码原样保留，其协议层测试 `encode_request_test.go` 照常绿）；
   - 不得改动 `rules_phase1.go` 里 `audio_input` 的设计处置（保持在 `Pass(...)` 名单里），也不得改动 `CONTEXT.md`。
8. **范围纪律**：不做部分投放设计之外的任何事。不碰 `openai.responses → dashscope.native` 派生路径的处置，不给模型专属参数加隐式默认，不新增 Capability。
9. **凭证纪律**：真实录制与真实 smoke 需要真实 DashScope API Key。若执行环境没有凭据，相应步骤标注 **BLOCKED（缺凭据）** 并停住，**绝不**用 httptest 伪造录制来解锁。

## 十二个已采纳的绑定决策（执行中不得推翻）

1. **11 份 fixture 恰好是名单**：`basic`、`streaming`、`tool_calling`、`vision_input`、`reasoning`、`parallel_tool_calls`、`structured_output`、`web_search`、`combined`、`multi_candidate_nonstream`、`parallel_tool_calls_default`。两份退出名单各有其理由：`audio_input.json` 不在名单——设计处置仍是 PASS，但 `qwen-audio-turbo` 免费额度耗尽、没有真实 fixture，无证据不兑现（ADR-0001）；`multi_candidate_stream.json` 不在名单——本期不承诺流式多候选，`stream=true` 搭 `n>1` 在触达上游前就被出站守卫拒成 422，给它留一个录制格子，等于让录制器去打一个网关自己都不会放行的请求，录不出任何证据却先把能力写进名单。非流式多候选不受影响，`multi_candidate_nonstream` 仍在名单。严格目录校验器（`TestRecordedFixturesAreValid`）从 `recordCases` 派生，双向对账，只在恰好 11 份时转绿。
2. **兑现集合恰 8 项**：`text_generation`、`streaming`、`tool_calling`、`parallel_tool_calls`、`structured_output`、`reasoning`、`vision_input`、`web_search`。`CapAudioInput` 不兑现；`file_input` / `audio_output` 是 REJECT，不在兑现之列。
3. **设计处置不动**：6 PASS + 3 DEGRADE + 2 REJECT = 11 项可表达能力，设计分 `(6 + 3×0.5)/11 = 7.5/11 ≈ 0.682` 不变；`audio_input` 的设计处置保持 PASS。本期不兑现只影响可用列：门可用分 `(5 + 3×0.5)/11 = 6.5/11 ≈ 0.591`，`Gated() == true`（`NotRedeemed == 1`）。
4. **audio_input 请求 501**：由矩阵在裁决阶段返回 `ClassNotImplemented`（HTTP 501），上游零调用。它不是 422：客户端请求没有错，只是能力尚未投放。OpenAI 线格式信封由 `openaiwire.EncodeError` 产出：`error.type == "server_error"`、`error.code` 因空值被 `omitempty` 省略、`error.message` 点名 `audio_input`。负例必须同时钉住这四点。
5. **录制矩阵与生产兑现逐字一致**：`recordMatrix` 收窄为同样的 8 项兑现。脚手架替生产兑现任何能力，都会成为产出「生产用不了的证据」的后门。
6. **组合用例最先录**：`combined.json` 必须由**一个真实模型**在**同一次请求**中证明 vision + tools + web_search + structured_output 可组合。若没有任何单一真实模型支撑，任务 17 在该步停止，标 **BLOCKED（缺模型）** 并回到设计阶段收窄承诺，不得拼接不同模型的证据。
7. **live audio_input 501 探针保留**：录制会话里保留一个 live 负例，断言真实凭据在手时 audio_input 请求仍被 501 拦下、录制代理零请求。它的靶子模型是文本角色（`modelRoleText`），全程不调用任何音频模型。
8. **golden 的 created 由固定时钟钉死**：Chat → Native 重编码用 Provider 的 `now()` 生成 `chat.completion` / chunk 的 `created` 字段。harness 的 `dashScopeNativeFactory` 注入固定时钟 `time.Unix(1755216000, 0).UTC()`，golden 里的 `created` 恒为 `1755216000`。生产装配（`build.go`）继续用 `nil`（即 `time.Now`），不受影响。
9. **ADR-0001 窗口**：任务 18 的 `Redeem` 一落地路径即「已兑现」，证明兑现的回放在任务 19 才提交。**从任务 18 开始到任务 19 全绿为止，不得中途停下。**
10. **恰好 14 个提交**：全部本地、不推送，逐条带 `GIT_MASTER=1` 前缀与 Sisyphus 署名，提交信息与「提交总览」表逐字一致。第 1 行范围分两阶段落地（首轮收窄 `65ce778` 已入库，再收窄随工作树变更提交），其余每行一笔；任务 23 按范围逐行对账。
11. **证明闸门咬人只用重建矩阵与 degrade 层测试**：不得为「证明 501」去临时改动生产 `Redeem`。生产矩阵的闸门行为由任务 18 的 degrade 层聚焦测试（`Phase1` + `Check`）证明；录制矩阵的闸门行为由任务 17 的离线探针（`recordMatrix` + 假源站）证明。
12. **stream=true 搭 n>1 上游前 422**：DashScope Native 流式无法返回多候选，放过去只会被上游静默压回 n=1，候选丢失而客户端无感。出站守卫（`rejectUnmappable`）在触达上游前返回 422 `ClassUnsupported`、`Param` 点名 `n`，OpenAI 线格式信封为 `invalid_request_error` / `unsupported_capability`，上游零调用；Provider 层（`reject_test.go`）与网关装配层（`build_test.go`）各有回归钉死。非流式 `n>1` 不受该守卫影响，照常放行，由 `multi_candidate_nonstream` 举证；`n>1` 搭 `tools` 仍按既有规则拒绝。流式转换器内部的首帧候选数精确对账与多候选状态机不因守卫拆除：守卫是策略，状态机是机制，策略将来若重新开放，机制必须原样可用；`stream_test.go` 经 `callTranslateStream` 绕过守卫直测这些内部防御，作为纵深防御保持常绿。

## 提交总览（14 个提交，全部本地，不推送）

| # | 提交信息（中文 plain，仓库风格） | 主要文件 | 任务 |
|---|---|---|---|
| 1 | `fixture 名单收窄：audio_input 退出本期举证`（已随 `65ce778` 入库）；第二阶段 `fixture 名单再收窄：multi_candidate_stream 退出本期举证` | 第一阶段：tests/smoke 收窄（用例表、角色、请求体注释、预检断言、录制矩阵、元数据测试、注释）+ `dsnative_audio_probe_test.go`；第二阶段：tests/smoke 再收窄 + 出站守卫与纵深防御（`internal/provider/dashscopenative/`）+ 装配回归与 dispatch 错误归属（`internal/gateway/`） | 17 |
| 2 | `Chat 到 Native 真实录制哨兵：audio_input 501 不触上游` | `tests/smoke/record_dsnative_live_test.go` | 17 |
| 3 | `fixture Chat 到 Native：录入多能力组合真实证据` | `testdata/routes/openai.chat__dashscope.native/combined.json` | 17 |
| 4 | `fixture Chat 到 Native：录入深度思考真实证据` | `.../reasoning.json` | 17 |
| 5 | `fixture Chat 到 Native：录入并行工具调用真实证据` | `.../parallel_tool_calls.json` | 17 |
| 6 | `fixture Chat 到 Native：录入结构化输出真实证据` | `.../structured_output.json` | 17 |
| 7 | `fixture Chat 到 Native：录入联网搜索真实证据` | `.../web_search.json` | 17 |
| 8 | `fixture Chat 到 Native：录入多候选非流式真实证据` | `.../multi_candidate_nonstream.json` | 17 |
| 9 | `fixture Chat 到 Native：录入并行工具缺省注入真实证据` | `.../parallel_tool_calls_default.json` | 17 |
| 10 | `兑现 openai.chat 到 dashscope.native 八项能力：白名单与矩阵文档同步` | `rules_phase1.go`、`matrix_test.go`、`chat_dsnative_test.go`、`tests/smoke/dsnative_matrix_test.go`、`degradation-matrix.md` | 18 |
| 11 | `Chat 到 DashScope Native 一致性回放与负例：audio_input 501 与上游逐条对账` | `chat_dsnative_conformance_test.go`、`chat_dsnative_negative_test.go`、`harness_test.go`、`golden/*.txt` | 19 |
| 12 | `tests/smoke 冒烟骨架：真实上游端到端` | `tests/smoke/smoke_test.go` | 20 |
| 13 | `Chat 到 DashScope Native 真实 smoke 用例` | `tests/smoke/chat_dsnative_test.go` | 21 |
| 14 | `知识库跟上部分投放：八项兑现与 audio_input 待证` | `docs/provenance.yaml`、`README.md`、根 `AGENTS.md`、相关包 `AGENTS.md` | 22 |

任务 23 是最终闸门，不产生提交。提交 1 分两阶段的理由：首轮收窄（audio_input 退出）随 `65ce778` 入库后，设计修订批准了 11 份契约，再收窄（multi_candidate_stream 退出）与它引出的出站守卫、纵深防御回归作为同一范围的第二阶段落地；名单收窄是一个原子事实，用例表、角色表、预检断言表与录制矩阵任何一处单独改都会让 `TestPreflightCaseBodiesMapToNativeUpstream` 的双向对账或 `recordMatrix` 的镜像声明当场红掉。提交 10 含五个文件的理由：`Redeem` 与两份白名单由 CI 互相校验（只改一侧立即失败），矩阵文档由 `TestDegradationMatrixDocIsCurrent` 强制与代码同步，`recordMatrix` 的 note 必须与生产逐字一致，拆开会让任一中间提交测试红或留下镜像漂移。提交 11 把 `harness_test.go` 的固定时钟与 golden 放进同一提交的理由：golden 的 `created` 稳定性依赖该时钟，拆开提交会让中间状态的 golden 随时漂移。

---

## 任务 17：真实录制 + 路径 fixture（硬门槛）

**依赖**：任务 1 到 16 已落地（录制器、装配、过滤与 usage 全部就位，录制时网关行为与线上一致）。
**文件**：修改 `tests/smoke/` 的收窄相关文件；新建 `tests/smoke/dsnative_audio_probe_test.go`；修改 `tests/smoke/record_dsnative_live_test.go`；出站守卫与纵深防御落在 `internal/provider/dashscopenative/`，装配回归与 dispatch 错误归属落在 `internal/gateway/`；产出 `testdata/routes/openai.chat__dashscope.native/` 的 11 份真实 fixture（现已全部录制落盘）。

> **门槛**：本任务需要真实 DashScope API Key 与能支撑所声明组合的真实模型。
> 若执行环境没有凭据，或没有任何单一真实模型支撑组合，标 **BLOCKED（缺凭据/缺模型）**
> 并停在本任务；**不得**构造假 fixture，也不得把多份互不相容的模型证据拼成「可组合」。

> **进度（截至本计划更新时）**：提交总览表第 1 行第一阶段、第 2、3 行已落地
> （`65ce778`、`a55073f`、`d547176`）。其余 6 份 fixture 均已真实录制落盘并通过
> 人工审阅与严格校验；其中 `parallel_tool_calls_default` 使用用户授予的单次付费
> 录制授权完成。所有未提交 fixture 均不得无授权重录。
> 下文步骤按完整流程书写，已完成的步骤逐份标注状态，执行时照状态跳过录制动作。

### 步骤 17.1：写离线 501 探针（RED）

> **状态**：已落地（提交 `65ce778`）。以下为决策记录，执行时跳过。

- [ ] 新建 `tests/smoke/dsnative_audio_probe_test.go`。这个探针是「收窄后录制矩阵不再兑现 audio_input」的守卫：假源站在门后等着，一旦录制矩阵误兑现，请求就会穿过网关打到源站，断言立刻红。门刻意选 `multimodal-generation`（audio_input 若被兑现该走的门）：如果 501 其实来自媒体过滤的无门 422，这里拿到的就不是 501，两种拒绝不得混淆。

```go
//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

// TestAudioInputNotRedeemedStays501BeforeUpstream 离线钉死部分投放设计：
// audio_input 的设计处置仍是 PASS，但本期不兑现——请求必须被矩阵裁决以 501
// 拦下，假源站一次都不该被打到。
//
// 防的是录制矩阵替生产兑现：recordMatrix 一旦把 audio_input 兑回去，
// 请求会穿过网关打到源站，状态码与代理计数同时失守。
//
// 门刻意选 multimodal-generation——audio_input 若被兑现该走的门。
// 若 501 其实来自媒体过滤的无门可承载，拿到的是 422 而不是 501，
// 两种拒绝不得混淆。
func TestAudioInputNotRedeemedStays501BeforeUpstream(t *testing.T) {
	origin := newStubOrigin(t, stubOriginConfig{
		stream:   false,
		jsonBody: nativeJSONResponse(1),
	})
	proxy, state := newRecordingProxy(t, origin.URL)
	built := buildRecordingGateway(t, proxy.URL, nativewire.DoorMultimodalGeneration,
		modelForRole(modelRoleText), preflightUpstreamSecret)

	req := httptest.NewRequest(http.MethodPost, string(degrade.EndpointOpenAIChat),
		bytes.NewReader(bodyAudioInput(recordClientModel)))
	req.Header.Set("Authorization", "Bearer "+testGatewayAuthKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	built.Mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501: %s", rec.Code, rec.Body.String())
	}
	if snap := state.Snapshot(); snap.Requests != 0 {
		t.Errorf("录制代理收到 %d 次请求，期望 0——未兑现能力不得触达上游", snap.Requests)
	}

	// 501 的线格式由 openaiwire 决定：ClassNotImplemented 映射成 server_error，
	// code 为空被 omitempty 省略。三点齐断，缺一处都说明这个 501 是从别处漏过来的。
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("错误信封不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	if env.Error.Type != "server_error" {
		t.Errorf("error.type = %q，期望 server_error", env.Error.Type)
	}
	if env.Error.Code != "" {
		t.Errorf("error.code = %q，期望省略（ClassNotImplemented 无 code）", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "audio_input") {
		t.Errorf("错误应点名能力 audio_input: %s", env.Error.Message)
	}
}
```

运行：

```bash
go test ./tests/smoke/ -tags=smoke -run TestAudioInputNotRedeemedStays501BeforeUpstream -v
```

预期：**失败**。当前 `recordMatrix` 仍兑现 `CapAudioInput`，请求会穿过网关：状态码是 200（不是 501），录制代理收到 1 次请求（不是 0）。这是正确的 RED。

### 步骤 17.2：把元数据测试改成 11 份名单（RED）

> **状态**：变更已在工作树，随再收窄提交落地。以下为决策记录。

- [ ] 修改 `tests/smoke/dsnative_metadata_test.go`：文档注释「校验 13 个用例」改为「校验 11 个用例」，并把用例数断言改成下面这样（同时显式钉死 audio_input 与 multi_candidate_stream 都不在名单）：

```go
	if len(recordCases) != 11 {
		t.Fatalf("用例数 = %d，期望恰好 11——audio_input 与 multi_candidate_stream "+
			"退出本期举证，其余格子一项都不许少", len(recordCases))
	}
	for _, c := range recordCases {
		if c.name == "audio_input" {
			t.Fatal("audio_input 不得出现在本期 recordCases——" +
				"qwen-audio-turbo 额度耗尽，无证据不兑现（ADR-0001）")
		}
		if c.name == "multi_candidate_stream" {
			t.Fatal("multi_candidate_stream 不得出现在本期 recordCases——" +
				"本期不承诺多候选流式，stream=true 搭 n>1 在出门前就被拒成 422，" +
				"录制器打不出任何证据")
		}
	}
```

运行：

```bash
go test ./tests/smoke/ -tags=smoke -run TestRecordCaseMetadataIsComplete -v
```

预期：**失败**——`用例数 = 13，期望恰好 11`。正确的 RED。

### 步骤 17.3：收窄录制名单与录制矩阵，落出站守卫（GREEN）

> **状态**：audio_input 半边已随 `65ce778` 落地；multi_candidate_stream 半边、
> 出站守卫与纵深防御在工作树中，随再收窄提交落地。以下为决策记录。

- [ ] 逐文件收窄（以下每处都在 `tests/smoke/`，均带 `//go:build smoke`）：

**a) `dsnative_case_table_test.go`**：删除 `audio_input` 与 `multi_candidate_stream` 两个条目（`name: "audio_input"`、`name: "multi_candidate_stream"` 那两段），文件头注释改为：

```go
// recordCases 是 Chat -> DashScope Native 路径本期必须交付举证的 11 个测试用例元数据表。
// 降级用例（expectDegraded=true）必须包含显式降级举证。
//
// audio_input 不在本表：设计处置仍是 PASS，但 qwen-audio-turbo 免费额度耗尽、
// 没有真实 fixture——无证据不兑现（ADR-0001）。未来恢复投放的条件见
// 2026-08-30 部分投放设计的「后续恢复条件」。
//
// multi_candidate_stream 不在本表：本期不承诺多候选流式，stream=true 搭 n>1
// 在出门前就被拒成 422。给它留一个录制格子，等于让录制器去打一个网关自己
// 都不会放行的请求——录不出任何证据，却先把这项能力写进了名单。
// 非流式的 multi_candidate_nonstream 不受影响，仍在本表。
```

**b) `dsnative_cases_test.go`**：删除 `modelRoleAudio` 常量与 `modelForRole` 的 `case modelRoleAudio:` 分支；为 `reasoning` 用例增设专用角色（缺省模型可被 `OMUGW_SMOKE_MODEL_REASONING` 覆盖）。收窄后：

```go
const (
	modelRoleText      modelRole = "text"
	modelRoleVL        modelRole = "vl"
	modelRoleCombined  modelRole = "combined"
	modelRoleReasoning modelRole = "reasoning"
)
```

理由写进 `modelForRole` 的注释：本期没有任何音频角色——`qwen-audio-turbo` 额度耗尽，Qwen-Omni 只走 OpenAI 兼容；录制表里留下音频模型名，等于给一个永远不会成功的调用留账单入口。

**c) `dsnative_case_bodies_test.go`**：注释「13 份客户端请求体」改为「11 份举证请求体，另加一份音频负例请求体（`bodyAudioInput`，仅供 501 探针使用，不进 `recordCases`）」；删除 `bodyMultiCandidateStream`——本期 stream=true 搭 n>1 在出门前就被拒成 422，留一份流式多候选请求体等于给一个永远发不出去的请求备着弹药，并在 `bodyMultiCandidateNonStream` 的注释里写明只有非流式一份的理由。`tinyWAVBase64` 与 `bodyAudioInput` **保留原样**，探针要用。

**d) `dsnative_preflight_assertions_test.go`**：删除 `"audio_input": {...}` 与 `"multi_candidate_stream": {...}` 两个条目。`TestPreflightCaseBodiesMapToNativeUpstream` 会把断言表与 `recordCases` 双向对账，两边必须同时收窄。生产音频编码的覆盖由协议层单测守住（`internal/protocol/dashscopenative/encode_request_test.go` 的 audio 用例），不随预检条目删除。

**e) `dsnative_matrix_test.go`**：`recordMatrix` 的 `Redeem(degrade.EndpointOpenAIChat, ...)` 删除 `canonical.CapAudioInput`，收窄为 8 项；函数注释改为：

```go
// recordMatrix 构造录制脚手架专用的降级矩阵。
//
// 生产矩阵（Phase1）中 openai.chat → dashscope.native 在任务 18 兑现前仍是 PLANNED。
// 录制器在此构建独立测试矩阵，严格镜像 Phase1 对 Chat 能力的处置声明，
// 并在 Chat 门兑现与生产完全一致的 8 项能力；同时填充其余三扇已注册门以满足
// 启动期对账。audio_input 的设计处置仍是 PASS，但本期没有真实 fixture、
// 不兑现——脚手架替生产兑现任何能力，都会成为产出生产用不了的证据的后门。
```

`parallel_tool_calls` 的 Degrade note 在本步骤**保持原样**，任务 18 与生产同步改。

**f) 注释里的份数**：`dsnative_fixture_validate_test.go`（「十三份」→「十一份」）、`dsnative_stub_test.go`（「十三份证据」→「十一份证据」、「十三个用例」两处→「十一个用例」）、`dsnative_preflight_test.go`（「13 份」→「11 份」）、`dsnative_preflight_data_test.go`（「13 份」→「11 份」）。

**g) 出站守卫与纵深防御（`internal/provider/dashscopenative/` 与 `internal/gateway/`）**：`reject.go` 的 `rejectUnmappable` 追加流式多候选拦截——`stream=true` 搭 `n>1` 在触达上游前返回 422（`ClassUnsupported`，`Param` 点名 `n`）。DashScope Native 流式无法返回多候选，放过去只会被上游静默压回 n=1，候选丢失而客户端无感；与其录一份掩盖丢失的证据，不如在出门前显式拒绝。非流式 `n>1` 不受该守卫影响，照常放行（`multi_candidate_nonstream` 举证）；`n>1` 搭 `tools` 的既有拦截不变。

```go
	// 拦截流式下的多重生成，防止上游静默将 n 压回 1 导致候选丢失。
	if nGreaterThanOne && req.Stream {
		return unsupported("n")
	}
```

同时保留流式转换器内部的首帧候选数精确对账与多候选状态机（`stream.go`）：守卫是策略，状态机是机制，策略将来若重新开放（例如上游支持流式多候选），机制必须原样可用。纵深防御由两层测试钉死：

- `reject_test.go`：`TestRejectStreamNGreaterThanOne`（stream+n>1 即 422、`Param` 点名 `n`）、`TestRejectStreamNGreaterThanOneWithReasoning`（带 reasoning 的组合照拒），以及正例 `TestPositivePlainNGreaterThanOne`（非流式单纯 n>1 必须放行）——缺了正例，一条把非流式 n>1 也拒掉的回归改不出红；
- `stream_test.go`：`TestStreamFirstFrameCandidateCountMismatchFailsInCall` 经 `callTranslateStream` 绕过守卫直调转换器，上游首帧候选数与请求不符（少回或多回）必须在 Call 内报错——守卫将来若放开，这道内部对账仍是最后一道闸。

网关装配层回归在 `internal/gateway/build_test.go` 的 `TestBuildNativeRejectsStreamWithMultipleCandidates`：真实装配的网关对 `{"stream":true,"n":2}` 返回 422，上游零调用，信封 `invalid_request_error` / `unsupported_capability` / `param=n` 三点齐断。另在 `internal/gateway/` 的 `handler.go` / `dispatch_test.go` 修正 dispatch 错误归属：只要 provider 真正返回过错误，客户端就看到最后一次真实调用的原因，而不是凭据池耗尽这类循环终止条件——守卫的 422 原因由此不被掩盖。

**h) 录制代理的消费者取消判别（`dsnative_recording_proxy_test.go` 与 `dsnative_proxy_sse_test.go`）**：`relayRecordedSSE` 在读错记入代理错误之前，先过 `responseRequestEnded(resp, err)`——下游请求的 context 已终结、且读错误正是该 context 错误（`errors.Is(err, ctxErr)`）时，中继安静收尾、不记错误。防的是网关自己拒收上游流（例如首帧候选数对账在 Call 内失败而弃流）时，下游取消连锁成代理侧上游读取的 `context.Canceled`；不加判别，录制器会用连锁取消覆盖网关的真实错误，录出的证据把失败归因给代理而不是闸门。聚焦测试是 `TestRecordingProxyDistinguishesConsumerCancellation`（新增 `readErrorAfterBytes` 助手构造「数据先读完、错误随后到」的 body）：下游已取消时，连锁取消不得记成代理错误，且取消前已收到的事件原样保留；下游仍活跃时，同样的 `context.Canceled` 必须保留为代理错误——两侧缺一，判别条件放宽或收紧都改不出红。

**i) 名单再收窄的残留收窄（`dsnative_assert_live_test.go` 与 `record_dsnative_live_test.go`）**：前者删除 `streamObservation` 的 `indexes` 观测量与 `assertDistinctIndexes` 的 `multi_candidate_stream` 分支——该用例已退出名单，流式多候选序号观测不再有消费者；非流式 `multi_candidate_nonstream` 的候选序号断言不受影响。后者只改注释计数（「其余十二条已经花钱打出来的证据」→「其余十条」：11 个用例，一条失败，其余 10 条），行为不变。

运行：

```bash
go test ./tests/smoke/ -tags=smoke -run 'TestAudioInputNotRedeemedStays501BeforeUpstream|TestRecordCaseMetadataIsComplete|TestPreflightCaseBodiesMapToNativeUpstream|TestRecorderAssemblesValidFixtureForEveryCase|TestRecorderSkipsSaveWhenAssertionsFail' -v
go test ./tests/smoke/ -tags=smoke -run 'TestRecordingProxyDistinguishesConsumerCancellation|TestRecordingProxyRelaysSSEEventByEvent' -v
go test ./internal/provider/dashscopenative/ -run 'TestReject|TestPositive|TestStreamFirstFrameCandidateCountMismatch' -v
go test ./internal/gateway/ -run 'TestBuildNativeRejectsStreamWithMultipleCandidates|TestDispatchPreservesProviderError' -v
```

预期：全部 **PASS**。收窄后 `recordMatrix` 不再兑现 audio_input，探针拿到 501 且代理零请求；元数据测试数出 11 项；预检与装配链路在 11 项名单上全绿；守卫对 stream+n>1 返回 422 且非流式 n>1 放行，内部候选数对账绕过守卫照绿；录制代理对消费者取消的判别两侧成立。

### 步骤 17.4：确认严格校验器的红是预期的

- [ ] 运行：

```bash
go test ./tests/smoke/ -tags=smoke -run TestRecordedFixturesAreValid -v
```

预期：**失败**——名单对账在子测试之前就以 `t.Fatalf` 中止：
`fixture 文件名单 = [basic combined multi_candidate_nonstream parallel_tool_calls
reasoning streaming structured_output tool_calling vision_input web_search]，期望恰好
[basic combined multi_candidate_nonstream parallel_tool_calls parallel_tool_calls_default
reasoning streaming structured_output tool_calling vision_input web_search]`。
10 份对 11 份，缺口只剩 `parallel_tool_calls_default`；`TestRecordedFixturesAreValid/<case>`
各子测试此刻**根本不会运行**——目录不齐时谈不上任何一份用例通过校验。
**这是正确的、刻意的红**：严格目录校验器从 `recordCases` 派生，只在 11 份
fixture 全部入库后转绿。录制期间的逐用例闸门是录制器侧断言
（`assertRecordedCase` + 失败不落盘），不是这道目录校验。本步骤只确认红的
理由正确，不做任何「修复」，也不得在 11 份齐了之前声称该测试的任何子测试能通过。

- [ ] 更广验证：

```bash
go build ./...
go vet -tags=smoke ./tests/smoke/
```

预期：build 通过，vet 无输出。

### 步骤 17.5：提交（第 1 个，分两阶段）

- [ ] 第一阶段（audio_input 退出）已随 `65ce778` 入库，执行时跳过。
- [ ] 第二阶段：提交工作树中的名单再收窄、出站守卫、纵深防御与两层 422 回归：

```bash
GIT_MASTER=1 git add tests/smoke/dsnative_case_table_test.go tests/smoke/dsnative_cases_test.go \
  tests/smoke/dsnative_case_bodies_test.go tests/smoke/dsnative_preflight_assertions_test.go \
  tests/smoke/dsnative_metadata_test.go tests/smoke/dsnative_fixture_validate_test.go \
  tests/smoke/dsnative_stub_test.go tests/smoke/dsnative_preflight_test.go \
  tests/smoke/dsnative_preflight_data_test.go tests/smoke/dsnative_proxy_sse_test.go \
  tests/smoke/dsnative_recording_proxy_test.go tests/smoke/dsnative_assert_live_test.go \
  tests/smoke/record_dsnative_live_test.go \
  internal/provider/dashscopenative/reject.go internal/provider/dashscopenative/reject_test.go \
  internal/provider/dashscopenative/stream.go internal/provider/dashscopenative/stream_test.go \
  internal/gateway/build_test.go internal/gateway/handler.go internal/gateway/dispatch_test.go
GIT_MASTER=1 git commit -m "fixture 名单再收窄：multi_candidate_stream 退出本期举证" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

### 步骤 17.6：live audio_input 501 探针（第 2 个提交）

> **状态**：已落地（提交 `a55073f`，哨兵已在 combined 录制前单独跑通）。
> 以下为决策记录，执行时跳过；后续每次真实录制会话仍应复跑该哨兵。

- [ ] 在 `tests/smoke/record_dsnative_live_test.go` 末尾追加（并在 import 块补 `nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"`）：

```go
// TestRecordChatDSNativeAudioInputStays501 是录制会话的 live 负例：
// 真实凭据在手，audio_input 请求也必须被矩阵闸门以 501 拦下——
// 录制代理一次请求都不该收到，任何音频模型（qwen-audio-turbo 等）
// 自始至终不会被调用。
//
// 保留成独立探针而不是 recordCases 条目：它证明的是「闸门关着」，
// 应落盘的证据恰恰是上游调用为零——放进 recordCases 会要求产出一份
// audio_input fixture，而本期名单里没有它。
//
// 门选 multimodal-generation（audio_input 若被兑现该走的门），模型角色
// 刻意选文本：本探针不得调用任何音频模型，而矩阵会在模型被问到之前拦下请求。
func TestRecordChatDSNativeAudioInputStays501(t *testing.T) {
	if !*record {
		t.Skip("跳过 live 探针：未指定 -record 标志")
	}
	if os.Getenv("OMUGW_SMOKE") != "1" {
		t.Skip("跳过 live 探针：未设置 OMUGW_SMOKE=1 环境变量")
	}
	apiKey := strings.TrimSpace(os.Getenv("DASHSCOPE_API_KEY"))
	if apiKey == "" {
		t.Skip("跳过 live 探针：未设置 DASHSCOPE_API_KEY 环境变量")
	}

	baseURL := recordBaseURL(t)
	proxy, state := newRecordingProxy(t, baseURL)
	built := buildRecordingGateway(t, proxy.URL, nativewire.DoorMultimodalGeneration,
		modelForRole(modelRoleText), apiKey)

	req := httptest.NewRequest(http.MethodPost, string(degrade.EndpointOpenAIChat),
		bytes.NewReader(bodyAudioInput(recordClientModel)))
	req.Header.Set("Authorization", "Bearer "+testGatewayAuthKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	built.Mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501: %s", rec.Code, rec.Body.String())
	}
	if snap := state.Snapshot(); snap.Requests != 0 {
		t.Fatalf("live 探针触达上游 %d 次——audio_input 必须在矩阵裁决阶段被拦下", snap.Requests)
	}
	if !strings.Contains(rec.Body.String(), "audio_input") {
		t.Errorf("501 错误应点名 audio_input，证明它来自矩阵裁决而非上游错误: %s", rec.Body.String())
	}
}
```

验证（离线可跑的部分）：

```bash
go vet -tags=smoke ./tests/smoke/
go test ./tests/smoke/ -tags=smoke -run TestRecordChatDSNativeAudioInputStays501 -v
```

预期：vet 无输出；测试输出 `SKIP`（未指定 -record 标志）。

- [ ] 在第一笔付费录制（步骤 17.7 的 `combined`）之前，单独运行 live 哨兵：

```bash
OMUGW_SMOKE=1 DASHSCOPE_API_KEY=<真实密钥> \
  go test ./tests/smoke/ -tags=smoke \
  -run '^TestRecordChatDSNativeAudioInputStays501$' -record -v
```

预期：**PASS**，状态码为 501，录制代理请求数为 0；因此不会产生 DashScope
推理调用或账单。必须单独运行：后续 `-run 'TestRecordChatDSNative/combined'`
只匹配表驱动录制测试的 `combined` 子测试，不会顺带匹配这个独立哨兵。

- [ ] 提交（第 2 个）：

```bash
GIT_MASTER=1 git add tests/smoke/record_dsnative_live_test.go
GIT_MASTER=1 git commit -m "Chat 到 Native 真实录制哨兵：audio_input 501 不触上游" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

### 步骤 17.7：录制 combined（最先录，第 3 个提交）

> **状态**：已录制、已人工审阅、已提交（`d547176`）。以下为决策记录，执行时跳过。

> **组合优先**：先录 `combined`。它要求**一个真实模型**在**同一次请求**里同时扛住
> vision + tools + web_search + structured_output。默认角色模型 `qwen-vl-max`
> （可用 `OMUGW_SMOKE_MODEL_COMBINED` 覆盖）。若上游报错、能力缺失或模型不支持组合，
> 标 **BLOCKED（缺模型）**，停在本步骤并回到设计阶段收窄承诺——**不得**换模型拼接证据。

- [ ] 真实录制：

```bash
OMUGW_SMOKE=1 DASHSCOPE_API_KEY=<真实密钥> \
  go test ./tests/smoke/ -tags=smoke -run 'TestRecordChatDSNative/combined' -record -v
```

预期：子测试 **PASS**，`testdata/routes/openai.chat__dashscope.native/combined.json` 落盘；同一会话里 `TestRecordChatDSNativeAudioInputStays501` 若一并运行（`-run 'TestRecordChatDSNative'`）也应 PASS。无凭据时标 BLOCKED 停下。

- [ ] 人工审阅 `combined.json`：`request.headers.authorization` 与 `upstream.headers.authorization` 均为 `<redacted>`，无任何密钥残留；`upstream.path` 是 `/api/v1/services/aigc/multimodal-generation/generation`；`upstream.body` 同时含 `enable_search:true`、`response_format.json_schema`、`parameters.tools` 与图像内容块；`response` 是 200 的 Native 响应；全文搜索一遍真实密钥确认零残留。

- [ ] 提交（第 3 个）：

```bash
GIT_MASTER=1 git add testdata/routes/openai.chat__dashscope.native/combined.json
GIT_MASTER=1 git commit -m "fixture Chat 到 Native：录入多能力组合真实证据" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

### 步骤 17.8：录制 reasoning（第 4 个提交）

> **状态**：fixture 已真实录制落盘（未跟踪），本步骤只剩人工审阅与提交；
> 重录是付费调用，需显式批准。

- [ ] 真实录制（文本门，角色 `modelRoleReasoning`，缺省 `glm-5.2`，可用 `OMUGW_SMOKE_MODEL_REASONING` 覆盖；fixture 已在盘则无需重录）：

```bash
OMUGW_SMOKE=1 DASHSCOPE_API_KEY=<真实密钥> \
  go test ./tests/smoke/ -tags=smoke -run 'TestRecordChatDSNative/reasoning' -record -v
```

预期：子测试 **PASS**，`reasoning.json` 落盘。录制器已断言流式事件里出现 `reasoning_content`（`assertLiveStream` 的 reasoning 分支）；人工审阅再确认 `upstream.body` 含 `reasoning_effort:"low"` 与 `incremental_output:true`，`upstream.headers` 含 `x-dashscope-sse: enable`。

- [ ] 提交（第 4 个）：

```bash
GIT_MASTER=1 git add testdata/routes/openai.chat__dashscope.native/reasoning.json
GIT_MASTER=1 git commit -m "fixture Chat 到 Native：录入深度思考真实证据" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

### 步骤 17.9：录制 parallel_tool_calls（第 5 个提交）

> **状态**：fixture 已真实录制落盘（未跟踪），本步骤只剩人工审阅与提交；
> 重录是付费调用，需显式批准。

- [ ] 真实录制（fixture 已在盘，**不要重录**；命令只是全新录制的完整配方，重录需显式批准）：

```bash
OMUGW_SMOKE=1 DASHSCOPE_API_KEY=<真实密钥> \
  go test ./tests/smoke/ -tags=smoke -run 'TestRecordChatDSNative/^parallel_tool_calls$' -record -v
```

`-run` 子测试正则刻意用 `^...$` 锚定：`parallel_tool_calls` 是 `parallel_tool_calls_default` 的前缀，不锚定会让一条命令同时录两份用例。

预期：子测试 **PASS**，`parallel_tool_calls.json` 落盘。人工审阅：`upstream.body` 含 `parallel_tool_calls:true`（显式字段映射），客户端响应带 `X-Omugw-Degraded` 且含 `parallel_tool_calls=`（录制器的 `assertDegradationHeader` 已断言，人工复查措辞）。

- [ ] 提交（第 5 个）：

```bash
GIT_MASTER=1 git add testdata/routes/openai.chat__dashscope.native/parallel_tool_calls.json
GIT_MASTER=1 git commit -m "fixture Chat 到 Native：录入并行工具调用真实证据" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

### 步骤 17.10：录制 structured_output（第 6 个提交）

> **状态**：fixture 已真实录制落盘（未跟踪），本步骤只剩人工审阅与提交；
> 重录是付费调用，需显式批准。

- [ ] 真实录制（fixture 已在盘，**不要重录**；命令只是全新录制的完整配方，重录需显式批准）：

```bash
OMUGW_SMOKE=1 DASHSCOPE_API_KEY=<真实密钥> \
  go test ./tests/smoke/ -tags=smoke -run 'TestRecordChatDSNative/structured_output' -record -v
```

预期：子测试 **PASS**，`structured_output.json` 落盘。人工审阅：`upstream.body` 的 `response_format` 保留 `json_schema` 形态（含 `strict` 与 `schema`），降级头含 `structured_output=`。

- [ ] 提交（第 6 个）：

```bash
GIT_MASTER=1 git add testdata/routes/openai.chat__dashscope.native/structured_output.json
GIT_MASTER=1 git commit -m "fixture Chat 到 Native：录入结构化输出真实证据" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

### 步骤 17.11：录制 web_search（第 7 个提交）

> **状态**：fixture 已真实录制落盘（未跟踪），本步骤只剩人工审阅与提交；
> 重录是付费调用，需显式批准。

- [ ] 真实录制（fixture 已在盘，**不要重录**；命令只是全新录制的完整配方，重录需显式批准）：

```bash
OMUGW_SMOKE=1 DASHSCOPE_API_KEY=<真实密钥> \
  go test ./tests/smoke/ -tags=smoke -run 'TestRecordChatDSNative/web_search' -record -v
```

预期：子测试 **PASS**，`web_search.json` 落盘。人工审阅：`upstream.body` 含 `enable_search:true` 且**不含** `web_search_options`（选项降成开关，丢失项由降级头可见），降级头含 `web_search=`。

- [ ] 提交（第 7 个）：

```bash
GIT_MASTER=1 git add testdata/routes/openai.chat__dashscope.native/web_search.json
GIT_MASTER=1 git commit -m "fixture Chat 到 Native：录入联网搜索真实证据" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

### 步骤 17.12：录制 multi_candidate_nonstream（第 8 个提交）

> **状态**：fixture 已真实录制落盘（未跟踪），本步骤只剩人工审阅与提交；
> 重录是付费调用，需显式批准。非流式 n>1 照常受支持，本 fixture 正是它的举证。

- [ ] 真实录制（fixture 已在盘，**不要重录**；命令只是全新录制的完整配方，重录需显式批准）：

```bash
OMUGW_SMOKE=1 DASHSCOPE_API_KEY=<真实密钥> \
  go test ./tests/smoke/ -tags=smoke -run 'TestRecordChatDSNative/multi_candidate_nonstream' -record -v
```

预期：子测试 **PASS**，`multi_candidate_nonstream.json` 落盘。人工审阅：`upstream.body` 含 `"n":2`，下游 `chat.completion` 的两个 choices 序号为 0 与 1（`assertDistinctIndexes` 已断言——n 被上游静默压回 1 时正是这里失守）。

- [ ] 提交（第 8 个）：

```bash
GIT_MASTER=1 git add testdata/routes/openai.chat__dashscope.native/multi_candidate_nonstream.json
GIT_MASTER=1 git commit -m "fixture Chat 到 Native：录入多候选非流式真实证据" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

> 原「录制 multi_candidate_stream」步骤已整体删除：`stream=true` 搭 `n>1` 在触达
> 上游前就被出站守卫拒成 422，录不出任何证据（见绑定决策 1 与 12）。后续步骤
> 序号与提交号相应前移。

### 步骤 17.13：录制 parallel_tool_calls_default（第 9 个提交）

> **状态**：已在用户单次付费授权下真实录制、人工审阅并通过严格校验；不得无授权重录。

- [x] 真实录制：

```bash
OMUGW_SMOKE=1 DASHSCOPE_API_KEY=<真实密钥> \
  go test ./tests/smoke/ -tags=smoke -run 'TestRecordChatDSNative/^parallel_tool_calls_default$' -record -v
```

`^...$` 锚定同理：与 `parallel_tool_calls` 互为前缀关系，两条录制命令各录各的，谁也不许顺带匹配对方。

预期：子测试 **PASS**，`parallel_tool_calls_default.json` 落盘。人工审阅：客户端请求体**没有** `parallel_tool_calls` 字段，而 `upstream.body` 含 `"parallel_tool_calls":true`——缺省注入的直接证据。

- [ ] 提交（第 9 个）：

```bash
GIT_MASTER=1 git add testdata/routes/openai.chat__dashscope.native/parallel_tool_calls_default.json
GIT_MASTER=1 git commit -m "fixture Chat 到 Native：录入并行工具缺省注入真实证据" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

### 步骤 17.14：严格校验器转绿

- [x] 11 份 fixture 齐了，运行：

```bash
go test ./tests/smoke/ -tags=smoke -run TestRecordedFixturesAreValid -v
```

预期：**PASS**——11 个子测试全绿，名单双向对账通过。严格目录校验器自收窄以来第一次转绿。

- [x] 全量离线 smoke 与构建：

```bash
go test ./tests/smoke/ -tags=smoke -v
go build ./...
```

预期：smoke 包离线测试全绿（`TestRecordChatDSNative*` 无 `-record` 时 SKIP），build 通过。

---

## 任务 18：兑现八项能力（ADR-0001 窗口打开）

**依赖**：任务 17（11 份真实 fixture 已落地——先有证据，后有兑现）。
**文件**：修改 `internal/degrade/rules_phase1.go`、`internal/degrade/matrix_test.go`、`tests/smoke/dsnative_matrix_test.go`（note 同步）；新建 `internal/degrade/chat_dsnative_test.go`；生成 `docs/degradation-matrix.md`。

> **从本任务开始到任务 19 全绿为止，不得中途停下。** `Redeem` 一落地路径即「已兑现」，
> 证明兑现的回放在任务 19 才提交；窗口内每个已提交状态都是绿的（fixture 与白名单已就位），
> 但兑现的合法性靠随后立即落地的回放证据。
>
> **状态**：实现、生成文档、聚焦验证与独立双审查均已完成；提交步骤尚未执行。

### 步骤 18.1：写兑现形状与闸门聚焦测试（RED）

- [x] 新建 `internal/degrade/chat_dsnative_test.go`（对照 `chat_dscompat_test.go` 的形状）：

```go
package degrade

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// redeemedChatDSNative 是 /v1/chat/completions 门在 dashscope.native 上兑现的八项，
// 按 AllCapabilities 顺序——与 RedeemedAt 输出顺序一致。
//
// audio_input 不在其中：设计处置仍是 PASS（协议能力没有消失），但
// qwen-audio-turbo 免费额度耗尽、没有真实 fixture——无证据不宣称可用
//（ADR-0001）。未来恢复投放的条件见 2026-08-30 部分投放设计。
var redeemedChatDSNative = []canonical.Capability{
	canonical.CapTextGeneration,
	canonical.CapStreaming,
	canonical.CapToolCalling,
	canonical.CapParallelToolCalls,
	canonical.CapStructuredOutput,
	canonical.CapReasoning,
	canonical.CapVisionInput,
	canonical.CapWebSearch,
}

// TestChatDSNativeRouteIsHeterogeneous 钉死身份与设计处置：
// 完整重编码的异构路径，非同源快通道；设计分 7.5/11；
// audio_input 的设计处置保持 PASS——本期不投放是证据问题，不是协议表达问题。
func TestChatDSNativeRouteIsHeterogeneous(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := m.Route(ProtoOpenAIChat, ProviderDashScopeNative)
	if !ok {
		t.Fatal("openai.chat -> dashscope.native 未注册")
	}
	if r.IsHomogeneous() {
		t.Error("该路径是完整重编码异构转换，不得标记为同源快通道")
	}

	// 设计处置：6 PASS + 3 DEGRADE + 2 REJECT = 11 项可表达能力，设计分 7.5/11。
	p := r.Preservation(m.Availability(), EndpointOpenAIChat)
	if p.Passthrough != 6 || p.Degrade != 3 || p.Reject != 2 {
		t.Errorf("设计处置 = pass %d deg %d rej %d，期望 6/3/2",
			p.Passthrough, p.Degrade, p.Reject)
	}
	if want := 7.5 / 11.0; p.DesignScore() != want {
		t.Errorf("设计保留度 = %.3f，期望 %.3f（7.5/11）", p.DesignScore(), want)
	}

	// audio_input 的设计处置必须是 PASS：把它改成 REJECT 或 N/A，
	// 等于把「缺少投放证据」误写成「协议无法表达」。
	rule, ok := m.Lookup(ProtoOpenAIChat, ProviderDashScopeNative, canonical.CapAudioInput)
	if !ok || rule.Disposition != Passthrough {
		t.Fatalf("audio_input 设计处置应为 PASSTHROUGH，实际 %v", rule.Disposition)
	}
}

// TestChatDSNativeRedemptionIsExactlyEightCapabilities 钉死兑现集合精确为八项：
// 门可用分 6.5/11，门保持 Gated（audio_input 设计上可交付、当前未投放）。
func TestChatDSNativeRedemptionIsExactlyEightCapabilities(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := m.Route(ProtoOpenAIChat, ProviderDashScopeNative)
	if !ok {
		t.Fatal("openai.chat -> dashscope.native 未注册")
	}

	if got := r.RedeemedAt(EndpointOpenAIChat); !reflect.DeepEqual(got, redeemedChatDSNative) {
		t.Errorf("兑现集合 = %v，期望 %v", got, redeemedChatDSNative)
	}
	if r.Redeems(EndpointOpenAIChat, canonical.CapAudioInput) {
		t.Error("audio_input 没有真实 fixture，本期不得兑现")
	}
	for _, c := range []canonical.Capability{canonical.CapFileInput, canonical.CapAudioOutput} {
		if r.Redeems(EndpointOpenAIChat, c) {
			t.Errorf("%q 是 REJECT，不应被兑现", c)
		}
	}

	// 八项兑现：5 PASS + 3 DEGRADE×0.5 = 6.5，分母 11。
	p := r.Preservation(m.Availability(), EndpointOpenAIChat)
	if want := 6.5 / 11.0; p.AvailableScore() != want {
		t.Errorf("门 %s 可用分 = %.3f，期望 %.3f（6.5/11）", EndpointOpenAIChat, p.AvailableScore(), want)
	}
	if !p.Gated() {
		t.Error("audio_input 设计上可交付、当前未投放，门必须保持 Gated")
	}
}

// TestChatDSNativeAudioInputStays501AtMatrix 用生产矩阵钉死闸门咬人：
// 未投放的 audio_input 在矩阵裁决阶段返回 501 not_implemented——不是 422，
// 客户端请求没有错；同一条 Check 去掉 audio_input 必须放行，
// 证明闸门挡的是能力而不是路径。
//
// 不做任何临时 Redeem 变更来「制造」这个 501：任务 18 兑现后的 Phase1
// 本身就是闸门生效的状态，直接对它断言。
func TestChatDSNativeAudioInputStays501AtMatrix(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	in := Inbound{Protocol: ProtoOpenAIChat, Endpoint: EndpointOpenAIChat}

	_, err = m.Check(in, ProviderDashScopeNative,
		[]canonical.Capability{canonical.CapTextGeneration, canonical.CapAudioInput})
	if err == nil {
		t.Fatal("未投放的 audio_input 必须被拦下")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassNotImplemented || cerr.HTTPStatus() != 501 {
		t.Errorf("应为 not_implemented/501，实际 %q/%d", cerr.Class, cerr.HTTPStatus())
	}
	// 点名能力，且是能力级 501（「尚未在端点」）而不是路径级 501（「实现尚未落地」）。
	if !strings.Contains(cerr.Message, "audio_input") || !strings.Contains(cerr.Message, "尚未在端点") {
		t.Errorf("错误应点名能力与端点: %s", cerr.Message)
	}

	// 已投放的八项照常放行，否则这道闸门把整条路径也一起关了。
	if _, err := m.Check(in, ProviderDashScopeNative, redeemedChatDSNative); err != nil {
		t.Errorf("已投放的八项不该被拦下: %v", err)
	}
}

// TestChatDoorStillPrefersCompatibleOverNative 钉死同门选路不变：
// dashscope.compatible（8/11 ≈ 0.727）仍优先于 dashscope.native（6.5/11 ≈ 0.591），
// OutboundPreference 不改。
func TestChatDoorStillPrefersCompatibleOverNative(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	compat := mustRoute(t, m, ProtoOpenAIChat, ProviderDashScopeCompatible)
	native := mustRoute(t, m, ProtoOpenAIChat, ProviderDashScopeNative)
	cs := compat.Preservation(m.Availability(), EndpointOpenAIChat).AvailableScore()
	ns := native.Preservation(m.Availability(), EndpointOpenAIChat).AvailableScore()
	if cs <= ns {
		t.Errorf("compatible 门可用分 %.3f 应严格高于 native 门 %.3f", cs, ns)
	}
}
```

运行：

```bash
go test ./internal/degrade/ -run 'TestChatDSNative' -v
```

预期：**失败**——`RedeemedAt` 为空、可用分为 0、`Check` 对八项全部返回路径级 501（消息里没有 `audio_input` 字样，也没有「尚未在端点」）。正确的 RED。

### 步骤 18.2：修正 note 并兑现（GREEN 的第一步）

- [x] 修改 `internal/degrade/rules_phase1.go` 的 `chatToDSNative`：

1. 把 `parallel_tool_calls` 的 DEGRADE note 改为同时写明两件事（原句「无显式开关可映射」与编码器实际注入 `true` 的行为矛盾，必须修正）：

```go
		Degrade("DashScope Native 有显式 parallel_tool_calls 开关，但模型支持面与并行行为 "+
			"不具路径级全局保证；客户端未提交该字段时网关按 OpenAI 默认显式注入 true"+
			"（Native 默认 false，不注入会静默变成串行）",
			canonical.CapParallelToolCalls).
```

2. 在 `Reject(...)` 之后、`Build()` 之前追加（`Pass(...)` 名单**一字不动**，`audio_input` 留在原处）：

```go
		// 兑现门槛是端到端真实 fixture 通过（ADR-0001）：十一份用例在
		// testdata/routes/openai.chat__dashscope.native/，回放与上游请求断言在
		// internal/gateway/chat_dsnative_conformance_test.go。
		// file_input / audio_output 是 REJECT，不在兑现之列；audio_input 设计处置
		// 仍是 PASS，但 qwen-audio-turbo 免费额度耗尽、没有真实 fixture——
		// 无证据不兑现，门保持 Gated，含它的请求由矩阵以 501 拦下。
		Redeem(EndpointOpenAIChat,
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapParallelToolCalls,
			canonical.CapStructuredOutput,
			canonical.CapReasoning,
			canonical.CapVisionInput,
			canonical.CapWebSearch,
		)
```

运行：

```bash
go test ./internal/degrade/ -run 'TestChatDSNative' -v
```

预期：四条聚焦测试 **PASS**。但紧接着跑全量会看到白名单测试变红：

```bash
go test ./internal/degrade/ -run 'TestImplementedRoutesAreExplicit|TestRedeemedCapabilitiesAreExplicit' -v
```

预期：**失败**——`路径 openai.chat -> dashscope.native 被标记为已实现，但不在名单里`。这正是两份白名单的交叉校验在咬：兑现与名单必须同一提交里一起改。

### 步骤 18.3：更新两份白名单（GREEN）

- [x] 修改 `internal/degrade/matrix_test.go`：

`TestImplementedRoutesAreExplicit` 的 `want` 增加一行：

```go
		string(ProtoOpenAIChat) + " -> " + string(ProviderDashScopeNative): true,
```

`TestRedeemedCapabilitiesAreExplicit` 的 `want` 增加：

```go
		string(ProtoOpenAIChat) + " -> " + string(ProviderDashScopeNative) +
			" @ " + string(EndpointOpenAIChat): {
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapParallelToolCalls,
			canonical.CapStructuredOutput,
			canonical.CapReasoning,
			canonical.CapVisionInput,
			canonical.CapWebSearch,
		},
```

- [x] 同步 `tests/smoke/dsnative_matrix_test.go` 里 `recordMatrix` 的 `parallel_tool_calls` note，与生产改后的文案逐字一致（镜像声明不留漂移）。

运行：

```bash
go test ./internal/degrade/ -v
go test ./tests/smoke/ -tags=smoke -run 'TestPreflightCaseBodiesMapToNativeUpstream|TestAudioInputNotRedeemedStays501BeforeUpstream' -v
```

预期：degrade 全包绿；smoke 侧预检与音频探针照绿（note 是文案变化，行为不变）。

### 步骤 18.4：重新生成矩阵文档 + 人工审阅闸门

- [x] 生成：

```bash
make matrix-update
```

- [x] **人工审阅** `docs/degradation-matrix.md` 的 diff，逐条核对预期变化只有四处：

  1. 主表 `openai.chat | dashscope.native` 行：状态「规划中」→「已实现」，当前可用列从 `—` 变为 `0.591（9 项中 8 项已投放）`；
  2. 「端点细分」新增一行：`openai.chat | dashscope.native | /v1/chat/completions | text_generation, streaming, tool_calling, parallel_tool_calls, structured_output, reasoning, vision_input, web_search | 0.591（9 项中 8 项已投放）`；
  3. `## openai.chat → dashscope.native` 小节里 `parallel_tool_calls` 的说明更新为新 note；`audio_input` 行保持 `PASSTHROUGH` 不变；
  4. 其余行不受影响——特别核对派生的 `openai.responses -> dashscope.native` 行仍是「规划中」，**没有**继承兑现。

  任何超出这四处的变化都说明发生了意外漂移，停下来排查，不得直接提交。

### 步骤 18.5：更广验证

- [x] 运行：

```bash
go test ./internal/degrade/ -v
make matrix
go build ./...
```

预期：degrade 全包绿，`make matrix` 通过，build 通过。

### 步骤 18.6：提交（第 10 个）

- [ ] 提交：

```bash
GIT_MASTER=1 git add internal/degrade/rules_phase1.go internal/degrade/matrix_test.go \
  internal/degrade/chat_dsnative_test.go tests/smoke/dsnative_matrix_test.go \
  docs/degradation-matrix.md
GIT_MASTER=1 git commit -m "兑现 openai.chat 到 dashscope.native 八项能力：白名单与矩阵文档同步" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 19：一致性回放与负例（ADR-0001 窗口关闭）

**依赖**：任务 18（路径已兑现，回放才能拿到 200）。
**文件**：新建 `internal/gateway/chat_dsnative_conformance_test.go`、`internal/gateway/chat_dsnative_negative_test.go`；修改 `internal/gateway/harness_test.go`（固定时钟）；生成 `testdata/routes/openai.chat__dashscope.native/golden/*.txt`。

> **状态**：11-fixture 回放、精确文件名/内部 name/golden 对账、负例、固定时钟、11 份 golden 人工审阅及独立双审查均已完成；提交步骤尚未执行。

### 步骤 19.1：写回放测试与负例（部分 RED）

- [x] 新建 `internal/gateway/chat_dsnative_conformance_test.go`：

```go
package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/testkit"
)

// chatDSNativeRouteFixtures 是 Chat -> DashScope Native 异构路径的 fixture 目录。
const chatDSNativeRouteFixtures = "../../testdata/routes/openai.chat__dashscope.native"

// chatDSNativeFixtureCount 钉死 fixture 份数：严格名单是设计文档收窄后的 11 份，
// 一个不多一个不少。LoadDir 有几份读几份——不钉这一句，少一份 fixture 时
// 覆盖会静默缩水，测试照样全绿。
const chatDSNativeFixtureCount = 11

// chatDSNativeDegradedHeaders 按用例名钉死降级头必须包含的能力项，独立于 golden
// 断言：golden 整体漂移时，降级语义的丢失仍要在这里单独咬住。
// combined 是多能力组合，降级头必须逐项可见——缺任何一项都是漏报降级。
var chatDSNativeDegradedHeaders = map[string][]string{
	"parallel_tool_calls": {"parallel_tool_calls="},
	"structured_output":   {"structured_output="},
	"web_search":          {"web_search="},
	"combined":            {"structured_output=", "web_search="},
}

// doorFromUpstreamPath 按 fixture 声明的上游路径反推应装配的门。
func doorFromUpstreamPath(t *testing.T, path string) string {
	t.Helper()
	switch path {
	case nativewire.TextGenerationPath:
		return "text-generation"
	case nativewire.MultimodalGenerationPath:
		return "multimodal-generation"
	default:
		t.Fatalf("fixture 上游路径 %q 不是已知 Native 门", path)
		return ""
	}
}

// TestChatDSNativeRouteConformance 回放全部 11 份 fixture：除客户端响应 golden 外，
// 必须逐条断言上游实际收到的 method/path/鉴权/请求体——只比客户端响应会漏掉
// 「Provider 没做映射、fixture 仍返回成功」的假绿。
func TestChatDSNativeRouteConformance(t *testing.T) {
	fixtures := testkit.LoadDir(t, chatDSNativeRouteFixtures)
	if len(fixtures) != chatDSNativeFixtureCount {
		t.Fatalf("fixture 数 = %d，期望恰好 %d——名单是设计文档收窄后的 11 份",
			len(fixtures), chatDSNativeFixtureCount)
	}

	// golden 数量与 fixture 双向一致：每份 fixture 的 golden 由循环里的 Golden()
	// 钉住（缺文件即失败），这里再补反方向——golden 目录不许有孤儿。
	// 目录不存在（首次 -update 之前）时跳过，让逐用例的 RED 先说话。
	if goldens, err := os.ReadDir(filepath.Join(chatDSNativeRouteFixtures, "golden")); err == nil {
		var goldenCount int
		for _, g := range goldens {
			if !g.IsDir() && strings.HasSuffix(g.Name(), ".txt") {
				goldenCount++
			}
		}
		if goldenCount != chatDSNativeFixtureCount {
			t.Errorf("golden 数 = %d，期望与 %d 份 fixture 双向一致",
				goldenCount, chatDSNativeFixtureCount)
		}
	}

	for _, f := range fixtures {
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
			hs := newChatDSNativeHarness(t, doorFromUpstreamPath(t, f.Upstream.Path), up)

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
			// 流式声明只存在于请求头，JSON 请求体没有等价字段；漏掉这项断言，
			// streaming fixture 即使被错误地按非流式请求发出，body 对账仍可能照绿。
			wantSSE := f.Upstream.Headers[strings.ToLower(nativewire.SSEHeader)]
			if got := gotHeader.Get(nativewire.SSEHeader); !strings.EqualFold(got, wantSSE) {
				t.Errorf("上游收到 %s = %q，期望 %q", nativewire.SSEHeader, got, wantSSE)
			}
			testkit.AssertJSONEqual(t, f.Upstream.Body, gotBody, "上游收到的请求体语义不符")

			// 降级头独立于 golden 断言：该降级的必须逐项可见，不该降级的不许出现。
			wantDegraded := chatDSNativeDegradedHeaders[caseName(f.Name)]
			gotDegraded := rec.Header().Get(degrade.DegradationHeader)
			if wantDegraded == nil {
				if gotDegraded != "" {
					t.Errorf("该用例不应有降级头，实际 %q", gotDegraded)
				}
			} else {
				for _, want := range wantDegraded {
					if !strings.Contains(gotDegraded, want) {
						t.Errorf("%s 应包含 %q，实际 %q", degrade.DegradationHeader, want, gotDegraded)
					}
				}
			}

			golden := filepath.Join(chatDSNativeRouteFixtures, "golden", caseName(f.Name)+".txt")
			testkit.Golden(t, golden, []byte(renderResult(rec)))
		})
	}
}
```

- [x] 新建 `internal/gateway/chat_dsnative_negative_test.go`：

```go
package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// nativeOKUpstream 返回一份能让 Native 转换器走完全程的非流式响应。
func nativeOKUpstream(t *testing.T) *upstream {
	return jsonUpstream(t, `{"output":{"choices":[{"finish_reason":"stop",`+
		`"message":{"role":"assistant","content":"ok"}}]},`+
		`"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7},"request_id":"r-neg"}`)
}

// TestChatDSNativeAudioInputNotRedeemedIs501 钉死 audio_input 本期未投放：
// 501 说「等实现」，不说「改请求」；矩阵裁决阶段拦下，上游零调用。
//
// 错误信封由 openaiwire 编码：ClassNotImplemented 映射成 server_error，
// code 为空被 omitempty 省略——状态码、type、code 缺席、点名能力四点齐断，
// 缺一处都说明这个 501 是从别处（上游错误或路由兜底）漏过来的。
func TestChatDSNativeAudioInputNotRedeemedIs501(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSNativeHarness(t, "multimodal-generation", up)

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":[`+
		`{"type":"text","text":"这段音频里说了什么？"},`+
		`{"type":"input_audio","input_audio":{"data":"UklGRiQAAABXQVZFZm10IBAAAAABAAEAQB8AAIA+AAACABAAZGF0YQAAAAA=","format":"wav"}}]}]}`, true)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——未投放能力必须在矩阵裁决阶段拦下", n)
	}

	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("错误信封不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	if env.Error.Type != "server_error" {
		t.Errorf("error.type = %q，期望 server_error（not_implemented 的 OpenAI 线格式）", env.Error.Type)
	}
	if env.Error.Code != "" {
		t.Errorf("error.code = %q，期望省略（ClassNotImplemented 无 code）", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, string(canonical.CapAudioInput)) {
		t.Errorf("错误应点名能力 audio_input: %s", env.Error.Message)
	}
}

// TestChatDSNativeRejectsFileInput 固化 file_input 在这条路上是 REJECT：
// 422 说「改请求」，矩阵闸门拦下，一个字节都不出门。
func TestChatDSNativeRejectsFileInput(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSNativeHarness(t, "multimodal-generation", up)

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

// TestChatDSNativeRejectsAudioOutput 固化 audio_output 在这条路上是 REJECT：
// Chat 入站表达不出 Qwen-Omni 的输出格式参数。
func TestChatDSNativeRejectsAudioOutput(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSNativeHarness(t, "text-generation", up)

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

// TestChatDSNativeRejectsNoLandingFields 固化无落点字段显式提交即 422 且点名，
// 在 Provider 出门前拦下（上游零调用）。
func TestChatDSNativeRejectsNoLandingFields(t *testing.T) {
	for _, field := range []string{
		`"frequency_penalty":0.5`, `"logit_bias":{"a":1}`, `"service_tier":"default"`,
		`"store":true`, `"user":"u-1"`, `"metadata":{"k":"v"}`,
		`"audio":{"voice":"alloy","format":"wav"}`,
	} {
		t.Run(field, func(t *testing.T) {
			up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
			hs := newChatDSNativeHarness(t, "text-generation", up)

			rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],`+field+`}`, true)

			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("状态码 = %d，期望 422: %s", rec.Code, rec.Body.String())
			}
			if n := up.calls.Load(); n != 0 {
				t.Errorf("请求打到了上游 %d 次——无落点字段必须在出门前拦下", n)
			}
		})
	}
}

// TestChatDSNativeRejectsNonStreamReasoning 固化非流式 + 非 none 推理即 422：
// 官方硬约束，思考模式不允许非流式调用。
func TestChatDSNativeRejectsNonStreamReasoning(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSNativeHarness(t, "text-generation", up)

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"reasoning_effort":"high"}`, true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——硬约束必须在出门前拦下", n)
	}
}

// TestChatDSNativeNoneReasoningPasses 是 none 的正例：非流式 + reasoning_effort:none
// 必须放行并真的打到上游。缺了正例，一条把 none 也拒掉的回归改不出红。
func TestChatDSNativeNoneReasoningPasses(t *testing.T) {
	up := nativeOKUpstream(t)
	hs := newChatDSNativeHarness(t, "text-generation", up)

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"reasoning_effort":"none"}`, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 1 {
		t.Errorf("上游应收到 1 次请求，实际 %d", n)
	}
}

// TestChatDSNativeRejectsNWithTools 固化 n>1 + tools 即 422：
// Native 带 tools 时会把 n 静默压回 1，候选丢失必须入站拦截。
func TestChatDSNativeRejectsNWithTools(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSNativeHarness(t, "text-generation", up)

	rec := hs.do(t, `{"model":"m","n":2,
		"tools":[{"type":"function","function":{"name":"f",
		"parameters":{"type":"object"}}}],
		"messages":[{"role":"user","content":"hi"}]}`, true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——n>1 + tools 必须在出门前拦下", n)
	}
}

// TestChatDSNativeRejectsStreamWithMultipleCandidates 钉死 stream=true 搭 n>1
// 在触达上游前被拒成 422：Native 流式无法返回多候选，放过去只会被上游静默
// 压回 n=1，候选丢失而客户端无感。错误信封由 openaiwire 决定：
// ClassUnsupported 映射成 invalid_request_error，code 为 unsupported_capability，
// param 点名 n，上游零调用。与装配层回归 TestBuildNativeRejectsStreamWithMultipleCandidates
// 互为表里：那里证真实装配，这里证转换器语义。非流式 n>1 不受该守卫影响，
// 照常放行——那条路由 multi_candidate_nonstream 的回放举证，本文件不再重复。
func TestChatDSNativeRejectsStreamWithMultipleCandidates(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSNativeHarness(t, "text-generation", up)

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"stream":true,"n":2}`, true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——stream=true 搭 n>1 必须在出门前拦下", n)
	}

	var env struct {
		Error struct {
			Type  string `json:"type"`
			Code  string `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("错误信封不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	if env.Error.Type != "invalid_request_error" {
		t.Errorf("error.type = %q，期望 invalid_request_error", env.Error.Type)
	}
	if env.Error.Code != "unsupported_capability" {
		t.Errorf("error.code = %q，期望 unsupported_capability", env.Error.Code)
	}
	if env.Error.Param != "n" {
		t.Errorf("error.param = %q，期望点名 n", env.Error.Param)
	}
}
```

运行：

```bash
go test ./internal/gateway/ -run 'TestChatDSNative' -v
```

预期：**部分失败**——负例测试全绿（它们钉住的行为在原计划任务 11/12/18 与本计划任务 17 的再收窄中已落地，这里是回归钉桩，不是 RED）；`TestChatDSNativeRouteConformance` 红在 `读取 golden 文件 ... 失败（首次生成请加 -update）`。这是 golden 缺失的 RED。

### 步骤 19.2：生成 golden，观察真实时钟造成的漂移（RED）

- [x] 生成并复跑：

```bash
go test ./internal/gateway/ -run TestChatDSNativeRouteConformance -update
go test ./internal/gateway/ -run TestChatDSNativeRouteConformance -v
```

预期：`-update` 写出 11 份 golden；复跑**失败**，diff 集中在 `"created":` 字段（两次运行跨越了不同的秒，真实时钟 `time.Now` 让重编码的 created 漂移）。若两次运行恰好同秒而绿，等一秒再跑一次，漂移必然出现。这正是必须注入固定时钟的原因。

### 步骤 19.3：给 harness 注入固定时钟（GREEN）

- [x] 修改 `internal/gateway/harness_test.go`：新增固定时钟，并让 `dashScopeNativeFactory` 使用它（替换现有实现）：

```go
// harnessNow 是 harness 统一注入的固定时钟。
//
// Chat -> Native 的重编码用 Provider 的 now() 生成 chat.completion 的 created
// 字段（非流式）与流式首帧的建立时刻；golden 要逐字节稳定，时钟就必须钉死
// 在同一个瞬间。取一个固定的过去时刻而不是「测试开始时刻」：后者仍是变量。
func harnessNow() time.Time { return time.Unix(1755216000, 0).UTC() }

// dashScopeNativeFactory 构造 DashScope Native Composite 适配器。
//
// 与 build.go 用同一个构造函数：装配一旦在两处各写一份，harness 测过的就不是
// 生产装出来的那个东西——线上装成直通而 harness 装着 Composite，测试照绿。
// 与生产唯一的差别是固定时钟：生产用 nil（time.Now，真实部署不需要确定的
// created），harness 要 golden 稳定。
func dashScopeNativeFactory(_ degrade.Provider, client *httpx.Client) provider.Provider {
	return dsnativeprovider.New(client, harnessNow)
}
```

- [x] 重新生成 golden 并复跑两遍：

```bash
go test ./internal/gateway/ -run TestChatDSNativeRouteConformance -update
go test ./internal/gateway/ -run TestChatDSNativeRouteConformance -v
go test ./internal/gateway/ -run TestChatDSNativeRouteConformance -v
```

预期：11 个子测试两遍都 **PASS**——golden 里的 `created` 恒为 `1755216000`，回放逐字节稳定。

- [x] 确认既有 gateway 测试不受时钟影响：

```bash
go test ./internal/gateway/ -v
```

预期：全包绿（含 `TestChatDSNativeHarnessCarriesDoor`、过滤与 usage 测试）。

### 步骤 19.4：人工审阅 golden 闸门

- [x] 逐份审阅 `testdata/routes/openai.chat__dashscope.native/golden/` 的 11 份 `.txt`：

  1. 每份以 `status: OK` 开头；非流式用例的正文是 `relayJSON` 规范化后的 `chat.completion`，`"created":1755216000`，`id` 取自 fixture 的 `request_id`，usage 数值与 fixture 一致；
  2. 流式用例（`streaming`、`tool_calling`、`reasoning`）是逐条 `chat.completion.chunk`，末尾 usage chunk（客户端要过 `include_usage` 的用例）与 `[DONE]` 收尾；
  3. 四个降级用例的 `X-Omugw-Degraded` 头在 golden 里可见，`combined` 同时含 `structured_output=` 与 `web_search=`；
  4. 语义与对应 fixture 一致：空白与字段排布被规范化是预期的，语义差异不是。发现语义差异立即停下排查，不得直接提交。

### 步骤 19.5：更广验证

- [x] 运行：

```bash
go test ./internal/gateway/ -v
make test
go build ./...
```

预期：全包绿，`make test` 通过，build 通过。

### 步骤 19.6：提交（第 11 个，ADR-0001 窗口关闭）

- [ ] 提交（golden 已在步骤 19.4 审阅通过）：

```bash
GIT_MASTER=1 git add internal/gateway/chat_dsnative_conformance_test.go \
  internal/gateway/chat_dsnative_negative_test.go internal/gateway/harness_test.go \
  testdata/routes/openai.chat__dashscope.native/golden/
GIT_MASTER=1 git commit -m "Chat 到 DashScope Native 一致性回放与负例：audio_input 501 与上游逐条对账" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

> golden 与其引用方（回放测试）同一提交落地：golden 是纯数据，审阅已在步骤 19.4
> 完成，与测试文件同提交保证任一中间状态都有测试引用、不出现孤儿 golden。
> 至此 ADR-0001 窗口关闭：兑现与回放证据都已落地。

---

## 任务 20：`tests/smoke` 冒烟骨架（真实上游端到端）

**依赖**：任务 18/19（生产矩阵已兑现，smoke 才有可证的东西）。
**文件**：新建 `tests/smoke/smoke_test.go`（`-tags=smoke`）。

> 与录制器的区别：录制器经转发代理捕获证据、用脚手架矩阵；smoke 直接打真实
> DashScope、用**生产** `Phase1` 矩阵——它证的是真实部署形态。
>
> **状态**：离线骨架、生产矩阵坐标断言、双重 opt-in 回归与独立双审查均已完成；提交步骤尚未执行。双重 opt-in 比原骨架更严格：必须同时满足 `OMUGW_SMOKE=1` 与非空 `DASHSCOPE_API_KEY`。

### 步骤 20.1：写冒烟基建

- [x] 新建 `tests/smoke/smoke_test.go`：

```go
//go:build smoke

package smoke_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/gateway"
	"github.com/yobo2u/omugw/internal/obs"
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

// smokeAPIKey 取真实凭据；缺失时跳过全部真实冒烟——不得用假上游顶替。
func smokeAPIKey(t testing.TB) string {
	t.Helper()
	key := strings.TrimSpace(os.Getenv("DASHSCOPE_API_KEY"))
	if key == "" {
		t.Skip("跳过真实冒烟：未设置 DASHSCOPE_API_KEY 环境变量")
	}
	return key
}

// buildSmokeGateway 组装直打真实 DashScope 的冒烟网关。
//
// 用生产 Phase1 矩阵而不是录制脚手架：smoke 证的是真实部署形态。
// 任务 18 兑现后，生产矩阵在 Chat 门上兑现的正是本期那 8 项；
// 若哪天兑现回退而端点注册没变，reconcileDoors 会在 Build 阶段拦下。
func buildSmokeGateway(t testing.TB, door nativewire.Door, model, upstreamSecret string) *gateway.Built {
	t.Helper()

	cfg := config.Default()
	cfg.Auth = config.Auth{
		Keys: []config.AuthKey{{ID: "smoke-test", Key: testGatewayAuthKey}},
	}
	cfg.Credentials = map[string][]config.CredentialSpec{
		"pool-ds-native": {{ID: "cred-1", Secret: upstreamSecret}},
	}
	cfg.Providers = []config.ProviderSpec{{
		Endpoint:       "ep-ds-native",
		Kind:           string(degrade.ProviderDashScopeNative),
		BaseURL:        recordBaseURL(t),
		CredentialPool: "pool-ds-native",
	}}
	cfg.Models = []config.ModelSpec{{
		Match: "*",
		Targets: []config.TargetSpec{{
			Endpoint:       "ep-ds-native",
			UpstreamModel:  model,
			NativeEndpoint: string(door),
		}},
	}}
	cfg.Timeouts = gatewayRecordingTimeouts()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("校验网关配置失败: %v", err)
	}

	m, err := degrade.Phase1()
	if err != nil {
		t.Fatalf("构造生产矩阵失败: %v", err)
	}

	reg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(reg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	built, err := gateway.Build(cfg, m, metrics, logger)
	if err != nil {
		t.Fatalf("构建网关失败: %v", err)
	}
	return built
}

// doSmokeChat 打一次 Chat 请求，返回记录的响应。
func doSmokeChat(t *testing.T, built *gateway.Built, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, string(degrade.EndpointOpenAIChat),
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testGatewayAuthKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	built.Mux.ServeHTTP(rec, req)
	return rec
}

// TestSmokeGatewayBuildsWithProductionMatrix 离线钉死冒烟网关能用生产矩阵装配：
// 任务 18 兑现后 Build 必须成功。Build 不触网，无需凭据。
func TestSmokeGatewayBuildsWithProductionMatrix(t *testing.T) {
	built := buildSmokeGateway(t, nativewire.DoorTextGeneration, "qwen-plus",
		"sk-fake-smoke-build-check")
	if built.Implemented == 0 {
		t.Fatal("生产矩阵没有任何已实现路径——冒烟没有可证的东西")
	}
}
```

运行：

```bash
go test ./tests/smoke/ -tags=smoke -run TestSmokeGatewayBuildsWithProductionMatrix -v
OMUGW_SMOKE=1 go test ./tests/smoke/ -tags=smoke -run TestChatDSNativeSmoke -v
```

预期：第一条 **PASS**（离线可跑）；第二条此时尚无用例文件，命令输出 `no tests to run` 属正常（用例在任务 21 落地；无凭据时输出 SKIP）。

### 步骤 20.2：提交（第 12 个）

- [ ] 提交：

```bash
GIT_MASTER=1 git add tests/smoke/smoke_test.go
GIT_MASTER=1 git commit -m "tests/smoke 冒烟骨架：真实上游端到端" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 21：Chat 到 DashScope Native 真实 smoke 用例（硬门槛）

**依赖**：任务 20（冒烟基建）、任务 18/19（路径已兑现）。
**文件**：新建 `tests/smoke/chat_dsnative_test.go`（`-tags=smoke`）。

> **门槛**：必须真实打到 DashScope。没有凭据时标 **BLOCKED（缺凭据）**，
> 不得用 httptest 顶替，不得伪造。本任务**不调用任何音频模型**：
> 四个探针只用文本与视觉角色，audio_input 的 live 负例在任务 17 的录制会话里。

### 步骤 21.1：写真实 smoke 用例

- [ ] 新建 `tests/smoke/chat_dsnative_test.go`：

```go
//go:build smoke

package smoke_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/testkit"
)

// TestChatDSNativeSmoke 真实打 DashScope，证明已兑现的八项能力在真实模型上可用。
//
// 没有音频探针：audio_input 本期未兑现，其 live 负例是录制会话的
// TestRecordChatDSNativeAudioInputStays501；本测试不得调用任何额度耗尽的音频模型。
func TestChatDSNativeSmoke(t *testing.T) {
	apiKey := smokeAPIKey(t)

	// 组合放第一个：若单一真实模型扛不住组合，尽早发现，
	// 不要等其余探针付完钱才知道。
	t.Run("combined", func(t *testing.T) {
		model := modelForRole(modelRoleCombined)
		built := buildSmokeGateway(t, nativewire.DoorMultimodalGeneration, model, apiKey)
		rec := doSmokeChat(t, built, bodyCombined(model))
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200: %s", rec.Code, rec.Body.String())
		}
		degraded := rec.Header().Get(degrade.DegradationHeader)
		if !strings.Contains(degraded, "structured_output=") || !strings.Contains(degraded, "web_search=") {
			t.Errorf("组合用例降级头应逐项点名 structured_output 与 web_search，实际 %q", degraded)
		}
		var resp downstreamCompletion
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("下游响应不是合法 JSON: %v (%s)", err, rec.Body.String())
		}
		if resp.Usage == nil {
			t.Fatalf("下游缺少 usage，本次调用的用量不可计费: %s", rec.Body.String())
		}
	})

	t.Run("basic", func(t *testing.T) {
		model := modelForRole(modelRoleText)
		built := buildSmokeGateway(t, nativewire.DoorTextGeneration, model, apiKey)
		rec := doSmokeChat(t, built, bodyBasic(model))
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200: %s", rec.Code, rec.Body.String())
		}
		var resp downstreamCompletion
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("下游响应不是合法 JSON: %v (%s)", err, rec.Body.String())
		}
		if resp.Object != "chat.completion" {
			t.Errorf("下游 object = %q，期望 chat.completion", resp.Object)
		}
		if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == "" {
			t.Errorf("下游 choices 为空或内容为空: %s", rec.Body.String())
		}
		if resp.Usage == nil {
			t.Fatalf("下游缺少 usage，本次调用的用量不可计费: %s", rec.Body.String())
		}
		assertAuthoritativeUsage(t, *resp.Usage)
	})

	t.Run("streaming", func(t *testing.T) {
		model := modelForRole(modelRoleText)
		built := buildSmokeGateway(t, nativewire.DoorTextGeneration, model, apiKey)
		rec := doSmokeChat(t, built, bodyStreaming(model))
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200: %s", rec.Code, rec.Body.String())
		}
		events, err := testkit.ParseSSE(rec.Body)
		if err != nil {
			t.Fatalf("下游 SSE 无法解析: %v", err)
		}
		var sawDone, sawUsageChunk bool
		for _, ev := range events {
			if ev.Event == "error" {
				t.Fatalf("下游流内出现 error 事件: %s", ev.Data)
			}
			if ev.IsDone() {
				sawDone = true
				continue
			}
			var chunk downstreamChunk
			if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
				t.Fatalf("chunk 不是合法 JSON: %v (%s)", err, ev.Data)
			}
			if len(chunk.Choices) == 0 && chunk.Usage != nil {
				sawUsageChunk = true
			}
		}
		if !sawDone {
			t.Fatal("下游流缺少 [DONE] 哨兵")
		}
		if !sawUsageChunk {
			t.Fatal("客户端要了 include_usage，下游流却没有末尾 usage chunk")
		}
	})

	t.Run("reasoning", func(t *testing.T) {
		model := modelForRole(modelRoleText)
		built := buildSmokeGateway(t, nativewire.DoorTextGeneration, model, apiKey)
		rec := doSmokeChat(t, built, bodyReasoning(model))
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200: %s", rec.Code, rec.Body.String())
		}
		events, err := testkit.ParseSSE(rec.Body)
		if err != nil {
			t.Fatalf("下游 SSE 无法解析: %v", err)
		}
		var reasoningFrames int
		for _, ev := range events {
			if ev.IsDone() || ev.Event == "error" {
				continue
			}
			var chunk downstreamChunk
			if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
				continue
			}
			for _, ch := range chunk.Choices {
				if ch.Delta.ReasoningContent != "" {
					reasoningFrames++
				}
			}
		}
		// 按模型行为断言「思考落到了客户端」，不对具体文案断言。
		if reasoningFrames == 0 {
			t.Error("下游流没有任何带 reasoning_content 的 chunk，深度思考没有落到客户端")
		}
	})
}
```

运行：

```bash
OMUGW_SMOKE=1 DASHSCOPE_API_KEY=<真实密钥> \
  go test ./tests/smoke/ -tags=smoke -run TestChatDSNativeSmoke -v
```

预期：有凭据时四个子测试全部 **PASS**；无凭据时 SKIP（离线 CI 不跑）。

### 步骤 21.2：提交（第 13 个）

- [ ] 提交：

```bash
GIT_MASTER=1 git add tests/smoke/chat_dsnative_test.go
GIT_MASTER=1 git commit -m "Chat 到 DashScope Native 真实 smoke 用例" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 22：知识库同步（provenance / README / AGENTS）

**依赖**：任务 18/19（投放已落地，状态描述才有依据）。
**文件**：修改 `docs/provenance.yaml`、`README.md`、根 `AGENTS.md`、`internal/protocol/AGENTS.md`、`internal/degrade/AGENTS.md`、`internal/gateway/AGENTS.md`。**不改 `CONTEXT.md`**（本设计没有引入新领域术语）。

### 步骤 22.1：更新 `docs/provenance.yaml`

- [ ] 在 `modules:` 列表追加两条（`internal/protocol/dashscopenative` 的出站编码与 `internal/provider/dashscopenative` 的 Composite 都还没有登记）：

```yaml
  - path: internal/protocol/dashscopenative
    implementation_type: clean-room
    reference:
      - name: 阿里云百炼 API 参考
        kind: public-documentation
        url: https://help.aliyun.com/zh/model-studio/
      - name: DashScope 流式输出
        kind: public-documentation
        url: https://help.aliyun.com/zh/model-studio/stream
    note: |
      Native 出站请求编码与成功响应/事件解码，依据公开文档实现；
      未文档化事项（裸 HTTP 顶层、两种 null、终止帧）以真实录制复核。

  - path: internal/provider/dashscopenative
    implementation_type: original
    note: |
      Chat 到 DashScope Native 的 Composite 出站适配器（非流式完整转换 +
      同步 transforming reader），基于本仓库 Canonical 与降级矩阵自行设计。
```

### 步骤 22.2：更新 README 与根 AGENTS 的投放状态

- [ ] `README.md`：把「**其中 4 条已实现**」改为「**其中 5 条已实现**」，并在 `openai.chat → dashscope.compatible` 的描述之后补一句：

```markdown
> 以及首条请求与响应都完整重编码的异构路径 `openai.chat → dashscope.native`
>（在 `/v1/chat/completions` 门兑现 8 项能力；`audio_input` 设计处置是 PASS，
> 但缺少真实证据、本期不兑现，含它的请求由矩阵返回 501；设计分 7.5/11 ≈ 0.682，
> 门可用分 6.5/11 ≈ 0.591，`dashscope.compatible` 的 0.727 仍优先）。
```

- [ ] 根 `AGENTS.md` 的「当前状态」：把「4 条已通车」改为「5 条已通车」，并在 DashScope Compatible 描述之后补：

```markdown
Chat 到 DashScope Native 是首条请求与响应都完整重编码的异构路径，在
`/v1/chat/completions` 门兑现 8 项能力（text_generation / streaming /
tool_calling / parallel_tool_calls / structured_output / reasoning /
vision_input / web_search）；`audio_input` 设计处置保持 PASS 但无真实证据、
本期不兑现，含它的请求由矩阵在触达上游前返回 501；门可用分 6.5/11 ≈ 0.591，
`Gated()` 为真，设计分 7.5/11 ≈ 0.682 不变。
```

- [ ] 相关包 `AGENTS.md` 按既有体例补语义边界（只改已存在的文件，不新建）：
  - `internal/protocol/AGENTS.md`：`dashscopenative/` 条目补「出站请求编码 `encode_request.go`（两扇门信封与全参数映射，音频块编码保留——当前缺投放证据不等于协议无法表达）与成功响应解码 `result.go`」；
  - `internal/degrade/AGENTS.md`：补「部分投放形态：路径转正但门保持 `Gated()`（设计可交付、当前未投放的格子返回 501），可用列端点相对」；
  - `internal/gateway/AGENTS.md`：补「Chat → Native harness 注入固定时钟 `harnessNow`（golden 的 created 稳定性），生产装配仍用 `time.Now`」。

### 步骤 22.3：提交（第 14 个）

- [ ] 提交前用 `ls` 核对文件确实存在，然后提交：

```bash
GIT_MASTER=1 git add docs/provenance.yaml README.md AGENTS.md \
  internal/protocol/AGENTS.md internal/degrade/AGENTS.md internal/gateway/AGENTS.md
GIT_MASTER=1 git commit -m "知识库跟上部分投放：八项兑现与 audio_input 待证" \
  -m "Ultraworked with [Sisyphus](https://github.com/code-yeongyu/oh-my-openagent)" \
  -m "Co-authored-by: Sisyphus <clio-agent@sisyphuslabs.ai>"
```

---

## 任务 23：最终闸门（不产生提交）

**依赖**：任务 17 到 22 全部落地。

### 步骤 23.1：格式与静态检查

- [ ] 运行：

```bash
gofmt -l .
go vet ./...
go vet -tags=smoke ./tests/smoke/
go build ./...
```

预期：四条命令全部无输出、退出码 0。

### 步骤 23.2：全量测试与矩阵闸门

- [ ] 运行：

```bash
make test
make test-race
make matrix
make check
```

预期：全部通过。`make check` 覆盖 fmt-check + vet + test + matrix，等价 CI。`make test-race` 对 `internal/provider/dashscopenative` 与 `internal/gateway` 无竞态告警。

- [ ] 再跑一遍 smoke 包的离线测试与严格校验器，确认 11 份名单全绿：

```bash
go test ./tests/smoke/ -tags=smoke -v
```

预期：离线测试全绿，`TestRecordedFixturesAreValid` 的 11 个子测试全过，真实录制/冒烟类测试 SKIP（未传 `-record`/未设凭据）。

### 步骤 23.3：LSP 诊断

- [ ] 对全部改动过的包跑 `lsp_diagnostics`，严重度取 `error`，预期零错误：

  - `internal/degrade`
  - `internal/gateway`

  （`tests/smoke` 带 build tag，由步骤 23.1 的 `go vet -tags=smoke` 覆盖。）

### 步骤 23.4：文件大小复查

- [ ] 运行：

```bash
wc -l internal/degrade/chat_dsnative_test.go \
      internal/gateway/chat_dsnative_conformance_test.go \
      internal/gateway/chat_dsnative_negative_test.go \
      tests/smoke/dsnative_audio_probe_test.go tests/smoke/smoke_test.go \
      tests/smoke/chat_dsnative_test.go
```

预期：本计划新建的都是测试文件，与既有测试惯例持平；生产代码只改了 `rules_phase1.go`（追加 Redeem 与改 note）与 `harness_test.go`（固定时钟），两者远未及尺寸警戒线。任一文件越界，按职责拆分后重跑步骤 23.2。

### 步骤 23.5：真实 smoke（硬门槛）

- [ ] 运行：

```bash
OMUGW_SMOKE=1 DASHSCOPE_API_KEY=<真实密钥> make smoke
```

预期：有凭据时 `tests/smoke` 全部 PASS（含任务 21 的四个探针与严格校验器），无凭据时按 make 逻辑 SKIP，且相关任务已按 **BLOCKED（缺凭据）** 如实标注。**不得**用 httptest 顶替真实 smoke。核对全程没有任何音频模型被调用：名单里没有音频用例，live 音频探针的靶子模型是文本角色且上游零调用。

### 步骤 23.6：仓库状态核对

- [ ] 运行：

```bash
GIT_MASTER=1 git status --porcelain
GIT_MASTER=1 git log --oneline 3b479b7..HEAD
```

预期：

1. `git status --porcelain` 恰好两行，都是未跟踪的计划文件：

   ```
   ?? docs/superpowers/plans/2026-08-25-openai-chat-dashscope-native.md
   ?? docs/superpowers/plans/2026-08-30-openai-chat-dashscope-native-partial-audio-rollout.md
   ```

   原计划文件自始至终未被编辑、未被 `git add`（不变量成立）；本计划文件同样未提交。
2. `git log --oneline 3b479b7..HEAD` 与「提交总览」表按范围逐行对应：第 1 行范围
   分两笔落地（首轮收窄 `65ce778` 与再收窄提交），其余每行一笔，主题与表逐字
   一致、顺序一致。
3. 全程没有执行过任何 `git push`。

---

## 关键不变量自查（易漏点逐项对账）

| # | 不变量 | 证据位置 |
|---|---|---|
| 1 | `audio_input` 设计处置保持 PASS，`rules_phase1.go` 的 `Pass(...)` 名单一字不动 | 任务 18（`TestChatDSNativeRouteIsHeterogeneous` 断言 `Lookup == Passthrough`） |
| 2 | 生产音频编解码代码保留（audio 块编码、协议层音频测试照常绿） | 任务 17 不触碰 `internal/protocol/dashscopenative/`；`make test` 覆盖 `encode_request_test.go` 音频用例 |
| 3 | 兑现集合恰 8 项，`CapAudioInput` 不在其中 | 任务 18（`TestChatDSNativeRedemptionIsExactlyEightCapabilities` + `TestRedeemedCapabilitiesAreExplicit` 白名单） |
| 4 | 门可用分 6.5/11 且 `Gated() == true`；设计分 7.5/11 不变 | 任务 18（同一聚焦测试断言两列分数与 Gated） |
| 5 | audio_input 请求 501 `not_implemented`，上游零调用 | 任务 18（degrade 层 `TestChatDSNativeAudioInputStays501AtMatrix`）、任务 19（网关负例 `TestChatDSNativeAudioInputNotRedeemedIs501`）、任务 17（离线探针 + live 探针） |
| 6 | 501 的 OpenAI 线格式：`server_error` + code 省略 + 点名能力 | 任务 17/19 的探针与负例同时断言三点 |
| 7 | 严格 11 份 fixture 名单，双向对账，只在 11 份时转绿 | 任务 17（`recordCases` 收窄、`TestRecordedFixturesAreValid`）、任务 19（共享精确名单同时绑定 fixture 文件名、内部 name 与 golden 文件名） |
| 8 | combined 由单一真实模型一次请求证明四能力可组合，不拼接 | 任务 17 步骤 17.7（最先录，缺模型即 BLOCKED 回设计） |
| 9 | golden 的 `created` 恒为 1755216000（固定时钟注入 harness，生产不受影响） | 任务 19（`harnessNow` + 两遍回放稳定） |
| 10 | `file_input` / `audio_output` 的 422 行为不变 | 任务 19（负例 `TestChatDSNativeRejectsFileInput` / `TestChatDSNativeRejectsAudioOutput`） |
| 11 | `dashscope.compatible`（0.727）仍优先于 native（0.591），`OutboundPreference` 不改 | 任务 18（`TestChatDoorStillPrefersCompatibleOverNative`） |
| 12 | 不为「证明闸门咬人」临时改动生产 `Redeem` | 绑定决策 11；任务 18 直接对兑现后的 `Phase1` 断言，任务 17 用收窄后的 `recordMatrix` 断言 |
| 13 | ADR-0001 窗口不间断：任务 18 开始到任务 19 全绿为止 | 任务 18/19 的前置警告 |
| 14 | 恰好 14 个提交，全部 `GIT_MASTER=1` 前缀，不推送 | 提交总览表 + 任务 23 步骤 23.6 |
| 15 | 原计划文件未跟踪且逐字节不变；本计划文件不提交 | 任务 23 步骤 23.6 |
| 16 | `CONTEXT.md` 不改；不给 Qwen-Omni 加 Native 特例；不改 `dashscope.compatible` 路径 | 前置约定 7/8；全计划无相关编辑 |
| 17 | `stream=true` 搭 `n>1` 上游前 422（`invalid_request_error` / `unsupported_capability`、`param=n`），上游零调用；非流式 `n>1` 照常放行 | 任务 17 步骤 17.3 g（`TestRejectStreamNGreaterThanOne`、`TestBuildNativeRejectsStreamWithMultipleCandidates`）、任务 19（`TestChatDSNativeRejectsStreamWithMultipleCandidates`、`multi_candidate_nonstream` 回放） |
| 18 | 流式转换器内部首帧候选数精确对账与多候选状态机保留为纵深防御，绕过守卫直测常绿，为将来重新开放留路 | 任务 17 步骤 17.3 g（`callTranslateStream`、`TestStreamFirstFrameCandidateCountMismatchFailsInCall`） |

## 完成标准自查（对照部分投放设计逐条勾选）

| 设计要求 | 证据位置 |
|---|---|
| 决策 1：`audio_input` 设计处置保持 PASS | 任务 18（聚焦测试断言 `Lookup == Passthrough`） |
| 决策 2：`/v1/chat/completions` 门不兑现 `CapAudioInput` | 任务 18（`Redeem` 只列 8 项；`Redeems(audio_input) == false` 断言） |
| 决策 3：路径转正、门保持 `Gated()` | 任务 18（`Gated() == true` 断言；矩阵文档「9 项中 8 项已投放」） |
| 决策 4：audio_input 请求 501 `not_implemented`，非 422 | 任务 17/18/19（四层断言：离线探针、live 探针、degrade 层、网关负例） |
| 决策 5：不加 Qwen-Omni Native 特例、不改 compatible、不混用证据 | 前置约定 7；全计划无相关编辑 |
| 兑现集合与分数：8 项兑现、设计 7.5/11、可用 6.5/11、compatible 0.727 仍优先 | 任务 18（聚焦测试逐数断言） |
| 真实证据门：恰好 11 份、双向对账、逐项 fail-fast、combined 单一模型 | 任务 17（收窄 + 录制纪律 + 严格校验器） |
| 运行时行为：8 项请求进 Provider；audio_input 501 且上游零调用；stream=true 搭 n>1 上游前 422 且非流式 n>1 照常放行；file_input/audio_output 422 不变；音频编解码代码与流式内部防御保留 | 任务 17 步骤 17.3 g（出站守卫与纵深防御）、任务 19（回放 + 负例）、任务 17（不触碰编码代码） |
| 任务 17/18/19 实施影响逐条 | 任务 17（名单收窄 + 11 份录制）、任务 18（8 项兑现 + 白名单 + 文档）、任务 19（11 份回放 + 501 负例 + 422 负例 + golden 双向一致） |
| 验证门 1 到 8 | 严格校验（任务 17 步骤 17.14）、聚焦测试（任务 18）、501 负例（任务 19）、stream+n>1 上游前 422 两层回归（任务 17 步骤 17.3 g 与任务 19）、回放与 golden（任务 19）、`make matrix-update` 人工审阅（任务 18 步骤 18.4）、`make check`（任务 23）、真实 smoke 且不调音频模型（任务 23 步骤 23.5） |
| 后续恢复条件（未来兑现 `CapAudioInput` 的五条件） | 不在本计划执行范围；已记录于部分投放设计「后续恢复条件」，步骤 17.3 的注释引用它 |

## 执行注意（给 fresh agent 的最后叮嘱）

1. **任务 17 的录制顺序已完成**：11 份 fixture 均已真实录制并人工审阅；任何重录仍需明确授权。
2. **严格校验器已转绿**：`TestRecordedFixturesAreValid` 对 11 份 JSON 双向对账，同时只允许任务 19 的 `golden/` 子目录；不得用改名单或塞假文件维持绿色。
3. **任务 18 → 19 的 ADR-0001 窗口已关闭**：8 项兑现与 11 份回放证据均已在工作树完成并通过双审查，提交步骤仍待执行。
4. **任务 17 / 21 依赖真实凭据**：没有就标 BLOCKED 停下，绝不伪造。
5. **音频纪律**：全程不调用 `qwen-audio-turbo` 或任何音频模型，不删生产音频编解码代码，不改 `audio_input` 的设计处置。
6. **提交信息逐字使用「提交总览」表的中文文案**，每条带 `GIT_MASTER=1` 前缀与 Sisyphus 署名；两个计划文件都不提交。
7. 遇到本计划与部分投放设计冲突，以设计为准，停下报告，不自行改设计。

---

## 执行交接

计划已保存到 `docs/superpowers/plans/2026-08-30-openai-chat-dashscope-native-partial-audio-rollout.md`。两种执行方式：

**1. Subagent-Driven（推荐）**：每个任务派发一个全新的子代理执行，任务之间做评审，迭代快。

**2. Inline Execution**：在本会话内用 executing-plans 执行，按检查点批量推进。

选哪种？
