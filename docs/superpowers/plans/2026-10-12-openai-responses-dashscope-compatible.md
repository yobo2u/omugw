# OpenAI Responses → DashScope Compatible 路径投放 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Responses 协议客户端经网关使用百炼模型，在 `/v1/responses` 门兑现 10 项有真实录制证据的能力。

**Architecture:** 线格式兼容直通：复用 `dashscopecompat` 适配器并按入站门分支，Responses 门只改写 `model` 与预览版搜索工具类型；会话由上游承担，主链路按矩阵该格处置决定是否进 convstore；错误按扁平信封解码。

**Tech Stack:** Go 1.25，标准库 `encoding/json` / `net/http`，仓库内 `internal/testkit`（fixture + golden）。不新增依赖。

**Spec:** `docs/superpowers/specs/2026-10-12-openai-responses-dashscope-compatible-design.md`

## Global Constraints

- 工作树：`~/.config/superpowers/worktrees/omugw/responses-dscompat-20261012`，分支 `feat/responses-dscompat-20261012`。不在 `main` 上提交。
- 所有 Git 命令带 `GIT_MASTER=1`。提交信息末行固定为 `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`。
- 注释与文档一律中文，写「防的是什么」。
- 矩阵是唯一权威；改规则后跑 `make matrix`，文档用 `make matrix-update` 重新生成，不手改 `docs/degradation-matrix.md`。
- 转正三处同改：代码 + `testdata/routes/openai.responses__dashscope.compatible/` fixture + `internal/degrade/matrix_test.go` 两份名单。
- 兑现用的 fixture 必须是真实上游录制。**禁止裸跑 `make smoke`**；录制只走 Task 7 给出的精确入口，且需要用户在执行当时明确同意。
- 不打印、不落盘凭据；日志与错误里不得出现 `credential.Secret`。凭据只从环境变量 `DASHSCOPE_API_KEY` 读取。
- 不新增 `Capability`，不 `MarkHomogeneous`，不动 `openai.compat` 的会话处置（#21）。
- 本期只对 `qwen3.8-flash`（主录）举证，只打北京旧域名 `https://dashscope.aliyuncs.com/compatible-mode/v1`。
- 每个任务结束前跑 `go test ./...`；全部任务结束前跑 `make check`。

## Review Focus

1. **回传上游输出项被网关解码器拒绝**：客户端把上一轮 `output`（reasoning / function_call / message 项，可能带上游特有字段）原样放回 `input`，人会期望它能续问。Responses 解码器是严格解码，可能在触达上游前 400。Task 7 录制 `reasoning-replay` 与 `tool_calling-result`，Task 8 回放时暴露；若被拒，**停下报告用户**，不要删字段迎合。
2. **`tool_choice` 指向预览版搜索**：`tools` 里的 `web_search_preview` 被改名后，`tool_choice: {"type":"web_search_preview"}` 若不同步改名会指向一个不存在的工具。Task 3 同步改名并有测试；上游对 `tool_choice: {"type":"web_search"}` 的接受度未实测，Task 7 的 `web_search-preview` 用例带该字段以取得证据。
3. **上游扁平错误里的关键词误分类**：`dashscopewire.classify` 靠 `message` 关键词把 `InvalidParameter` 细分成上下文超长。一条含 `exceed` / `context` 的普通参数错误会被归成 `context_length`。两者都是 400 且都不重试，Task 2 的测试钉住「续轮未命中是 bad_request、不可重试」这一条最要紧的。
4. **运维开着 convstore 时本路径被悄悄展开历史**：人会期望开关只影响声明为 EMULATE 的路径。Task 8 有专门测试（装配 convstore 后上游仍收到 `previous_response_id`，且没有被写入 `store`）。
5. **没装 convstore 时续轮 500**：`prepareConversation` 的内部错误分支此前靠矩阵拦截兜着。Task 5 把判据移到它前面，Task 8 有端到端测试。

---

## File Structure

| 文件 | 动作 | 职责 |
|---|---|---|
| `internal/protocol/openairesponses/decode.go` | 改 | 上报 `CapParallelToolCalls` |
| `internal/protocol/openairesponses/decode_test.go` | 改 | 上述行为的测试 |
| `testdata/routes/openai.responses__openai.compat/parallel_tool_calls.json` + `golden/parallel_tool_calls.txt` | 新 | 同源路径该格的回放证据 |
| `internal/provider/dashscopecompat/dashscopecompat.go` | 改 | 按门选择修补函数与错误解码 |
| `internal/provider/dashscopecompat/responses.go` | 新 | Responses 门的请求修补与扁平错误解码 |
| `internal/provider/dashscopecompat/responses_test.go` | 新 | 上述两者的测试 |
| `internal/degrade/rules_phase1.go` | 改 | 拆 `responsesExtras`、逐格覆盖、note、Redeem |
| `internal/degrade/responses_dscompat_test.go` | 新 | 本路径处置与兑现集合的精确形状 |
| `internal/degrade/matrix_test.go` | 改 | 两份白名单 |
| `internal/gateway/handler.go` | 改 | 会话分支判据 |
| `internal/gateway/harness_test.go` | 改 | 新 harness；PLANNED 哨兵改指 |
| `internal/gateway/responses_dscompat_conformance_test.go` | 新 | fixture 回放 + 上游请求断言 |
| `internal/gateway/responses_dscompat_test.go` | 新 | 会话分支、触达上游前拒绝、错误不重试 |
| `tests/smoke/responses_dscompat_cases_test.go` | 新 | 录制用例表与 fixture 构造（离线可测） |
| `tests/smoke/record_responses_dscompat_live_test.go` | 新 | 录制入口（显式开关） |
| `testdata/routes/openai.responses__dashscope.compatible/*.json` + `golden/` | 新 | 真实录制与 golden |
| `docs/degradation-matrix.md`、`README.md`、`AGENTS.md`、`internal/gateway/AGENTS.md` | 改 | 文档同步 |

---

### Task 1: Responses 解码器上报 `parallel_tool_calls`

**Files:**
- Modify: `internal/protocol/openairesponses/decode.go`（`Decoded` 结构、约 `:122` 的赋值、`Capabilities()` 约 `:648`）
- Test: `internal/protocol/openairesponses/decode_test.go`
- Create: `testdata/routes/openai.responses__openai.compat/parallel_tool_calls.json`
- Create（生成）: `testdata/routes/openai.responses__openai.compat/golden/parallel_tool_calls.txt`

**Interfaces:**
- Consumes: 无
- Produces: `(*openairesponses.Decoded).Capabilities()` 在 `parallel_tool_calls` 显式为 `true` 时包含 `canonical.CapParallelToolCalls`

- [ ] **Step 1: 写失败测试**

追加到 `internal/protocol/openairesponses/decode_test.go` 末尾：

```go
// TestParallelToolCallsReportedOnlyWhenExplicitTrue 防的是矩阵从未对
// parallel_tool_calls 裁决：该字段此前只进 Extensions，异构路径读不得
// Extensions，于是任何路径都会把它当作不存在。条件与 Chat 解码器一致——
// 只有显式 true 才算客户端在要这项能力，省略与 false 都不算。
func TestParallelToolCallsReportedOnlyWhenExplicitTrue(t *testing.T) {
	const tools = `"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]`
	cases := []struct {
		name  string
		field string
		want  bool
	}{
		{"显式 true", `"parallel_tool_calls":true,`, true},
		{"显式 false", `"parallel_tool_calls":false,`, false},
		{"省略", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := mustDecode(t, `{"model":"m","input":"hi","store":false,`+tc.field+tools+`}`)
			if got := hasCap(d.Capabilities(), canonical.CapParallelToolCalls); got != tc.want {
				t.Fatalf("parallel_tool_calls 上报 = %v，期望 %v: %v", got, tc.want, d.Capabilities())
			}
		})
	}
}
```

- [ ] **Step 2: 确认失败**

Run: `go test ./internal/protocol/openairesponses/ -run TestParallelToolCallsReportedOnlyWhenExplicitTrue -v`
Expected: 子测试「显式 true」FAIL，「parallel_tool_calls 上报 = false，期望 true」。

- [ ] **Step 3: 实现**

在 `Decoded` 结构体的 `builtinCaps` 字段之后加：

```go
	// parallelToolCalls 记录客户端是否显式要了并行工具调用。它没有 Canonical
	// 字段，异构出站又读不得 Extensions，能力识别只能在解码阶段完成。
	parallelToolCalls bool
```

把现有的 `if w.ParallelToolCalls != nil { ... }` 块改成：

```go
	if w.ParallelToolCalls != nil {
		raw, _ := json.Marshal(map[string]bool{"parallel_tool_calls": *w.ParallelToolCalls})
		r.Extensions.Set(canonical.ExtOpenAI, raw)
		out.parallelToolCalls = *w.ParallelToolCalls
	}
```

在 `Capabilities()` 里 `if d.RequiresStatefulConversation { ... }` 之后加：

```go
	if d.parallelToolCalls {
		seen[canonical.CapParallelToolCalls] = true
	}
```

- [ ] **Step 4: 确认通过**

Run: `go test ./internal/protocol/openairesponses/ -v -run TestParallelToolCalls`
Expected: PASS。

- [ ] **Step 5: 给同源路径补 fixture**

创建 `testdata/routes/openai.responses__openai.compat/parallel_tool_calls.json`（该目录既有用例均为手写桩，这里沿用）：

```json
{
  "name": "openai.responses__openai.compat/parallel_tool_calls",
  "note": "parallel_tool_calls 显式 true：解码器自 2026-10 起把它上报给矩阵，这份用例证明同源直通在该格被裁决后仍原样通过。response 是被打桩的上游返回。",
  "request": {
    "method": "POST",
    "path": "/v1/responses",
    "headers": {
      "authorization": "<redacted>",
      "content-type": "application/json"
    },
    "body": {
      "model": "logical-fast",
      "input": "同时查北京和上海的天气",
      "store": false,
      "parallel_tool_calls": true,
      "tools": [
        {
          "type": "function",
          "name": "get_weather",
          "parameters": {
            "type": "object",
            "properties": {"city": {"type": "string"}},
            "required": ["city"]
          }
        }
      ]
    }
  },
  "response": {
    "status": 200,
    "headers": {"content-type": "application/json"},
    "body": {
      "id": "resp_68a4",
      "object": "response",
      "status": "completed",
      "model": "gpt-5-upstream",
      "parallel_tool_calls": true,
      "output": [
        {"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\":\"北京\"}"},
        {"type": "function_call", "id": "fc_2", "call_id": "call_2", "name": "get_weather", "arguments": "{\"city\":\"上海\"}"}
      ],
      "usage": {"input_tokens": 40, "output_tokens": 30, "total_tokens": 70}
    }
  }
}
```

- [ ] **Step 6: 生成并审阅 golden**

Run: `go test ./internal/gateway/ -run 'TestRouteConformance/parallel_tool_calls' -update`
然后 `cat testdata/routes/openai.responses__openai.compat/golden/parallel_tool_calls.txt`
Expected: 首行 `status: OK`，没有 `X-Omugw-Degraded` 行，正文是上面的上游响应体。若状态不是 OK，说明该格在同源路径被拒——停下排查，不要提交。

- [ ] **Step 7: 全量测试并提交**

Run: `go test ./...`
Expected: PASS。

```bash
GIT_MASTER=1 git add internal/protocol/openairesponses testdata/routes/openai.responses__openai.compat
GIT_MASTER=1 git commit -m "fix(openairesponses): 向矩阵上报 parallel_tool_calls

该字段此前只进 Extensions，矩阵从未对它裁决。

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Responses 门的扁平错误信封解码

**Files:**
- Create: `internal/provider/dashscopecompat/responses.go`
- Modify: `internal/provider/dashscopecompat/dashscopecompat.go`（`Call` 末尾与 `decodeError`）
- Modify: `internal/provider/dashscopecompat/dashscopecompat_test.go`（`TestDecodeErrorCapsReadVolumeAt64KiB` 的调用点）
- Test: `internal/provider/dashscopecompat/responses_test.go`

**Interfaces:**
- Consumes: `dashscopewire.DecodeError(status int, body []byte, h http.Header, now time.Time) *canonical.Error`、`openaiwire.DecodeError`（同签名）
- Produces: `decodeErrorBody(ep degrade.Endpoint, status int, body []byte, h http.Header, now time.Time) *canonical.Error`；`(*Provider).decodeError(resp *httpx.Response, ep degrade.Endpoint) error`

- [ ] **Step 1: 写失败测试**

创建 `internal/provider/dashscopecompat/responses_test.go`：

```go
package dashscopecompat

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/degrade"
)

// responsesInput 构造一次打 Responses 门的调用。
func responsesInput(raw string) callInput {
	return callInput{
		raw:             raw,
		upstreamModel:   "qwen-up",
		inboundEndpoint: degrade.EndpointOpenAIResponses,
	}
}

func asCanonical(t *testing.T, err error) *canonical.Error {
	t.Helper()
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("期望 *canonical.Error，实际 %T: %v", err, err)
	}
	return cerr
}

// TestResponsesDoorDecodesFlatEnvelope 防的是上游的原话被吞掉：这扇门返回
// 扁平信封 {request_id, code, message}，按 OpenAI 嵌套信封解码会取不到 code
// 与 message，客户端只能看到一句 Bad Request。
//
// 用例取自 2026-10-11 实测的续轮未命中响应。它必须是 bad_request 且不可重试：
// 这不是凭据故障，换凭据只会更糟。
func TestResponsesDoorDecodesFlatEnvelope(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"request_id":"req-1","code":"InvalidParameter",`+
			`"message":"Not found previous_response_id: resp_x."}`)
	})

	_, err := call(t, srv, responsesInput(`{"model":"m","input":"hi","previous_response_id":"resp_x"}`))
	e := asCanonical(t, err)

	if e.Class != canonical.ClassBadRequest {
		t.Errorf("分类 = %s，期望 bad_request", e.Class)
	}
	if e.Retryable {
		t.Error("续轮未命中不得标为可重试")
	}
	if e.Message != "Not found previous_response_id: resp_x." {
		t.Errorf("message = %q，上游原话丢了", e.Message)
	}
	if e.UpstreamCode != "InvalidParameter" {
		t.Errorf("upstream code = %q，期望 InvalidParameter", e.UpstreamCode)
	}
	if e.UpstreamRequestID != "req-1" {
		t.Errorf("request_id = %q，期望 req-1", e.UpstreamRequestID)
	}
	if e.UpstreamStatus != http.StatusBadRequest {
		t.Errorf("upstream status = %d，期望 400", e.UpstreamStatus)
	}
}

// TestResponsesDoorFallsBackToNestedEnvelope 防的是把两种形状写成二选一：
// 官方文档里嵌套信封同样出现过，取不到扁平字段时必须回落。
func TestResponsesDoorFallsBackToNestedEnvelope(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
	})

	_, err := call(t, srv, responsesInput(`{"model":"m","input":"hi"}`))
	e := asCanonical(t, err)

	if e.Class != canonical.ClassRateLimit {
		t.Errorf("分类 = %s，期望 rate_limit", e.Class)
	}
	if e.Message != "slow down" {
		t.Errorf("message = %q，期望 slow down", e.Message)
	}
}

// TestResponsesDoorNonJSONErrorUsesStatus 防的是 5xx 的 HTML 页面让解码崩掉：
// 此时状态码是唯一可靠的信息。
func TestResponsesDoorNonJSONErrorUsesStatus(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `<html>bad gateway</html>`)
	})

	_, err := call(t, srv, responsesInput(`{"model":"m","input":"hi"}`))
	e := asCanonical(t, err)

	if e.Class != canonical.ClassUpstreamUnavailable {
		t.Errorf("分类 = %s，期望 upstream_unavailable", e.Class)
	}
}

// TestResponsesDoorFlatAuthErrorIsRetryable 防的是把所有扁平错误都当参数错误：
// 鉴权失败换一把凭据可能成功，必须保持可重试。
func TestResponsesDoorFlatAuthErrorIsRetryable(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"request_id":"req-2","code":"InvalidApiKey","message":"Invalid API-key provided."}`)
	})

	_, err := call(t, srv, responsesInput(`{"model":"m","input":"hi"}`))
	e := asCanonical(t, err)

	if e.Class != canonical.ClassAuth || !e.Retryable {
		t.Errorf("分类 = %s retryable = %v，期望 auth 且可重试", e.Class, e.Retryable)
	}
}
```

- [ ] **Step 2: 确认失败**

Run: `go test ./internal/provider/dashscopecompat/ -run TestResponsesDoor -v`
Expected: `TestResponsesDoorDecodesFlatEnvelope` FAIL（message 为 `Bad Request`、upstream code 为空）；`TestResponsesDoorFlatAuthErrorIsRetryable` 可能已通过（状态码 401 兜住了），其余两条通过。

- [ ] **Step 3: 实现**

创建 `internal/provider/dashscopecompat/responses.go`：

```go
package dashscopecompat

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/protocol/dashscopewire"
	"github.com/yobo2u/omugw/internal/protocol/openaiwire"
)

// decodeErrorBody 按入站门挑错误信封。
//
// Responses 门的上游返回 DashScope 的扁平信封 {request_id, code, message}
//（2026-10-11 实测），与 Chat 门的 OpenAI 嵌套信封不同形。拿嵌套解码器去读
// 扁平体不会报错，只会静默取到空串——客户端看到的是一句 Bad Request，而
// 靠 code 才认得出的分类（限流、欠费、内容审核）会全部退化成按状态码猜。
//
// 先认扁平、取不到再回落嵌套：官方文档里两种形状并存，写成二选一迟早撞上
// 另一种。
func decodeErrorBody(ep degrade.Endpoint, status int, body []byte, h http.Header, now time.Time) *canonical.Error {
	if ep == degrade.EndpointOpenAIResponses && isFlatEnvelope(body) {
		return dashscopewire.DecodeError(status, body, h, now)
	}
	return openaiwire.DecodeError(status, body, h, now)
}

// isFlatEnvelope 判断错误体是否带顶层 code 或 message。
//
// 解析失败一律算「不是」：5xx 的 HTML 页面、空体、顶层 code 不是字符串的
// 怪形状，都交给回落分支按状态码处理。
func isFlatEnvelope(body []byte) bool {
	var probe struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Code != "" || probe.Message != ""
}
```

在 `dashscopecompat.go` 里：

把 `Call` 中的
```go
	if resp.StatusCode >= 400 {
		return nil, p.decodeError(resp)
	}
```
改成
```go
	if resp.StatusCode >= 400 {
		return nil, p.decodeError(resp, req.Inbound.Endpoint)
	}
```

把 `decodeError` 的注释与签名、末行改成：

```go
// decodeError 读出错误体并按入站门对应的信封解码。读之前设上限：一个故障
// 上游可能在 5xx 里塞进一整个 HTML 页面，甚至更糟。
func (p *Provider) decodeError(resp *httpx.Response, ep degrade.Endpoint) error {
```
```go
	return decodeErrorBody(ep, resp.StatusCode, body, resp.Header, p.now())
```

删掉 `dashscopecompat.go` 里不再使用的 `openaiwire` import。

在 `dashscopecompat_test.go` 的 `TestDecodeErrorCapsReadVolumeAt64KiB` 里把 `p.decodeError(resp)` 改成 `p.decodeError(resp, "")`。

- [ ] **Step 4: 确认通过**

Run: `go test ./internal/provider/dashscopecompat/ -v`
Expected: 全部 PASS，包括既有的 Chat 门测试与 `TestContract`。

- [ ] **Step 5: 提交**

```bash
GIT_MASTER=1 git add internal/provider/dashscopecompat
GIT_MASTER=1 git commit -m "feat(dashscopecompat): Responses 门按扁平信封解码上游错误

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Responses 门的请求修补

**Files:**
- Modify: `internal/provider/dashscopecompat/dashscopecompat.go`（包注释、`Call` 开头、`patch`）
- Modify: `internal/provider/dashscopecompat/responses.go`
- Test: `internal/provider/dashscopecompat/responses_test.go`

**Interfaces:**
- Consumes: Task 2 的 `responsesInput`、`call`、`upstreamFields`、`okServer`（测试辅助）
- Produces: `patchResponses(raw []byte, upstreamModel string) ([]byte, error)`；`withModel(raw []byte, upstreamModel string) (map[string]json.RawMessage, error)`

- [ ] **Step 1: 写失败测试**

追加到 `internal/provider/dashscopecompat/responses_test.go`（并在 import 里加 `"github.com/yobo2u/omugw/internal/testkit"`）：

```go
// TestResponsesDoorHitsResponsesEndpoint 防的是请求被送去 Chat 端点：
// 官方 base_url 以 /v1 结尾，入站门是 /v1/responses，去重后必须落在
// {base}/responses。
func TestResponsesDoorHitsResponsesEndpoint(t *testing.T) {
	srv, got := okServer(t)
	in := responsesInput(`{"model":"logical","input":"hi"}`)
	in.baseURL = srv.URL + "/compatible-mode/v1"

	if _, err := call(t, srv, in); err != nil {
		t.Fatal(err)
	}
	if got.path != "/compatible-mode/v1/responses" {
		t.Errorf("上游路径 = %q，期望 /compatible-mode/v1/responses", got.path)
	}
}

// TestResponsesDoorOnlyRewritesModel 防的是直通变成重编码：除 model 外每个
// 字段都必须原样到达上游，包括会话字段与网关不建模的参数。
func TestResponsesDoorOnlyRewritesModel(t *testing.T) {
	srv, got := okServer(t)
	const raw = `{"model":"logical","input":"hi","previous_response_id":"resp_1",` +
		`"store":true,"metadata":{"trace":"abc"},"reasoning":{"effort":"low","summary":"detailed"},` +
		`"parallel_tool_calls":false,"text":{"format":{"type":"json_object"}},` +
		`"tools":[{"type":"function","name":"f","strict":true,"parameters":{"type":"object"}},{"type":"web_search"}]}`

	if _, err := call(t, srv, responsesInput(raw)); err != nil {
		t.Fatal(err)
	}

	const want = `{"model":"qwen-up","input":"hi","previous_response_id":"resp_1",` +
		`"store":true,"metadata":{"trace":"abc"},"reasoning":{"effort":"low","summary":"detailed"},` +
		`"parallel_tool_calls":false,"text":{"format":{"type":"json_object"}},` +
		`"tools":[{"type":"function","name":"f","strict":true,"parameters":{"type":"object"}},{"type":"web_search"}]}`
	testkit.AssertJSONEqual(t, []byte(want), got.body, "Responses 门只应改写 model")
}

// TestResponsesDoorOmittedStoreIsNotInjected 防的是网关替客户端决定留存：
// 省略 store 时由上游默认值生效（实测默认存储 7 天），网关写入任何值都是
// 在改客户端没表达过的意图。
func TestResponsesDoorOmittedStoreIsNotInjected(t *testing.T) {
	srv, got := okServer(t)
	if _, err := call(t, srv, responsesInput(`{"model":"logical","input":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := upstreamFields(t, got)["store"]; ok {
		t.Errorf("上游请求里出现了 store，网关不该注入: %s", got.body)
	}
}

// TestResponsesDoorRenamesPreviewSearch 防的是一次不发生的搜索返回 200：
// 实测上游接受 web_search_preview 但不执行搜索，只有 web_search 生效。
// tool_choice 必须同步改名，否则它会指向一个改名后不存在的工具。
// 其余工具与搜索工具自带的选项保持原样。
func TestResponsesDoorRenamesPreviewSearch(t *testing.T) {
	srv, got := okServer(t)
	const raw = `{"model":"logical","input":"hi",` +
		`"tools":[{"type":"web_search_preview","search_context_size":"low"},` +
		`{"type":"function","name":"f","parameters":{"type":"object"}}],` +
		`"tool_choice":{"type":"web_search_preview"}}`

	if _, err := call(t, srv, responsesInput(raw)); err != nil {
		t.Fatal(err)
	}

	const want = `{"model":"qwen-up","input":"hi",` +
		`"tools":[{"type":"web_search","search_context_size":"low"},` +
		`{"type":"function","name":"f","parameters":{"type":"object"}}],` +
		`"tool_choice":{"type":"web_search"}}`
	testkit.AssertJSONEqual(t, []byte(want), got.body, "预览版搜索类型应被改写")
}

// TestResponsesDoorLeavesStringToolChoiceAlone 防的是改名逻辑误伤字符串形态
// 的 tool_choice（auto / required / none）。
func TestResponsesDoorLeavesStringToolChoiceAlone(t *testing.T) {
	srv, got := okServer(t)
	const raw = `{"model":"logical","input":"hi","tool_choice":"required",` +
		`"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`
	if _, err := call(t, srv, responsesInput(raw)); err != nil {
		t.Fatal(err)
	}
	if choice := string(upstreamFields(t, got)["tool_choice"]); choice != `"required"` {
		t.Errorf("tool_choice = %s，期望原样 \"required\"", choice)
	}
}

// TestResponsesDoorDoesNotApplyChatSearchMapping 防的是两扇门的修补串台：
// enable_search 是 Chat 门的映射产物，Responses 门不得注入。
func TestResponsesDoorDoesNotApplyChatSearchMapping(t *testing.T) {
	srv, got := okServer(t)
	if _, err := call(t, srv, responsesInput(`{"model":"logical","input":"hi","tools":[{"type":"web_search"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := upstreamFields(t, got)["enable_search"]; ok {
		t.Errorf("Responses 门不应出现 enable_search: %s", got.body)
	}
}

// TestResponsesDoorMissingUpstreamModel 防的是把逻辑模型名发给上游。
func TestResponsesDoorMissingUpstreamModel(t *testing.T) {
	srv, got := okServer(t)
	in := responsesInput(`{"model":"logical","input":"hi"}`)
	in.upstreamModel = ""

	_, err := call(t, srv, in)
	if e := asCanonical(t, err); e.Class != canonical.ClassInternal {
		t.Errorf("分类 = %s，期望 internal", e.Class)
	}
	if got.body != nil {
		t.Error("缺少上游模型名时不应触达上游")
	}
}
```

- [ ] **Step 2: 确认失败**

Run: `go test ./internal/provider/dashscopecompat/ -run TestResponsesDoor -v`
Expected: `TestResponsesDoorRenamesPreviewSearch` FAIL（`tools[0].type` 仍是 `web_search_preview`）。其余新测试通过——现有 `patch` 对 Responses 请求恰好只改 `model`。

- [ ] **Step 3: 实现**

在 `dashscopecompat.go` 的 `patch` 里，把从 `var fields map[string]json.RawMessage` 到 `fields["model"] = patched` 的一段抽成函数，放进 `dashscopecompat.go`：

```go
// withModel 把请求体解成字段表并改写模型名，是两扇门共用的第一步。
//
// 解成 map[string]json.RawMessage 再写回，而不是整体反序列化：这样除修补点
// 之外的每个字段都保持原始字节，包括网关不认识的新参数。
func withModel(raw []byte, upstreamModel string) (map[string]json.RawMessage, error) {
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
	return fields, nil
}
```

`patch` 的开头相应变成：

```go
func patch(raw []byte, upstreamModel string) ([]byte, error) {
	fields, err := withModel(raw, upstreamModel)
	if err != nil {
		return nil, err
	}
```
其后的 `web_search_options` 处理与序列化保持不变。

`Call` 的开头改成按门选择：

```go
	patchFn := patch
	if req.Inbound.Endpoint == degrade.EndpointOpenAIResponses {
		patchFn = patchResponses
	}
	body, err := patchFn(req.Raw, req.Target.UpstreamModel)
	if err != nil {
		return nil, err
	}
```

在 `responses.go` 追加（import 加 `"bytes"`）：

```go
const (
	typePreviewSearch = `"web_search_preview"`
	typeSearch        = `"web_search"`
)

// patchResponses 是 Responses 门的定点修补：改写模型名；把预览版内建搜索的
// 类型改成正式版。其余字段保持原始字节——会话字段（store、
// previous_response_id）尤其不能碰，本路径的会话由上游承担。
//
// 改名的理由：实测上游接受 web_search_preview 但不执行搜索，只有 web_search
// 生效。不改就是一次没发生的搜索配一个 200。
func patchResponses(raw []byte, upstreamModel string) ([]byte, error) {
	fields, err := withModel(raw, upstreamModel)
	if err != nil {
		return nil, err
	}

	if tools, ok := fields["tools"]; ok {
		fields["tools"] = renamePreviewSearchTools(tools)
	}
	// tool_choice 必须跟着改：工具改了名而选择器没改，它就指向一个不存在的工具。
	if choice, ok := fields["tool_choice"]; ok {
		fields["tool_choice"] = renamePreviewSearchType(choice)
	}

	out, err := json.Marshal(fields)
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "重新序列化请求体失败")
	}
	return out, nil
}

// renamePreviewSearchTools 逐项改名。形状不是数组（null 等）时原样返回：
// 解码器已在网关入口校验过请求，这里不重复裁决。
func renamePreviewSearchTools(raw json.RawMessage) json.RawMessage {
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return raw
	}
	changed := false
	for i, tool := range tools {
		renamed := renamePreviewSearchType(tool)
		if !bytes.Equal(renamed, tool) {
			tools[i] = renamed
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(tools)
	if err != nil {
		return raw
	}
	return out
}

// renamePreviewSearchType 在对象的 type 恰为预览版搜索时改名，其余情形
//（字符串形态的 tool_choice、别的工具类型、非对象）原样返回。
func renamePreviewSearchType(raw json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	if !bytes.Equal(bytes.TrimSpace(obj["type"]), []byte(typePreviewSearch)) {
		return raw
	}
	obj["type"] = json.RawMessage(typeSearch)
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}
```

把 `dashscopecompat.go` 的包注释改成同时描述两扇门：

```go
// Package dashscopecompat 是 DashScope Compatible（OpenAI 线格式）的出站适配器。
//
// wire-compatible 而语义异构：请求与响应仍走 OpenAI 线格式，因此不做
// Canonical 重编码；但上游语义与 OpenAI 并不等同——模型能力、结构化输出保证、
// Web Search 参数都有差异。所以这条路**不是**同源快通道，不得 MarkHomogeneous：
// wire-compatible 只说明不需要重编码，不能推导为语义零损失，X-Omugw-Degraded
// 仍由矩阵生成。
//
// 按入站门做定点修补，其余字段保持原始字节（经 Canonical 往返会制造静默损失）：
//   - Chat 门：改写模型名；删除 web_search_options，非 null 时映射成
//     enable_search: true。
//   - Responses 门：改写模型名；把预览版内建搜索类型改成正式版。会话字段
//     原样转发——上游自己持久化会话。错误按 DashScope 扁平信封解码。
package dashscopecompat
```

同时把 `Provider` 类型与 `Call` 上方「只负责 OpenAI Chat compatible wire」「两处定点修补」的措辞改成不限定 Chat 门：

```go
// Provider 负责 DashScope Compatible 两扇 OpenAI 线格式门的出站。
```
```go
// Call 把请求发给 DashScope Compatible。
//
// 原始 JSON 只做所在门的定点修补，响应原样返回给网关转发。Canonical 已在网关层
// 完成请求校验与能力裁决，这里不消费——本路径没有 Canonical 出站编码器。
```

- [ ] **Step 4: 确认通过**

Run: `go test ./internal/provider/dashscopecompat/ -v && go vet ./internal/provider/dashscopecompat/`
Expected: 全部 PASS；既有 Chat 门的六条搜索映射测试不受影响。

- [ ] **Step 5: 提交**

```bash
GIT_MASTER=1 git add internal/provider/dashscopecompat
GIT_MASTER=1 git commit -m "feat(dashscopecompat): Responses 门的请求修补

只改写 model 与预览版搜索类型，会话字段原样转发。

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: 矩阵处置（设计层，尚不兑现）

**Files:**
- Modify: `internal/degrade/rules_phase1.go`（note 常量约 `:17-35`；`responsesExtras` 约 `:181-211`）
- Create: `internal/degrade/responses_dscompat_test.go`
- Regenerate: `docs/degradation-matrix.md`

**Interfaces:**
- Consumes: `Route.Derive`、`Route.Override(c, d, note)`、`Route.Emulate`、`Matrix.Lookup(in, out, c) (Rule, bool)`、`Route.Preservation(avail, ep)`
- Produces: `Phase1()` 中 `openai.responses → dashscope.compatible` 的处置：`stateful_conversation` = `Passthrough`；`image_detail` / `web_search` / `structured_output` = `Degrade`；其余见测试。路径此时仍是 PLANNED（Redeem 在 Task 8）

- [ ] **Step 1: 写失败测试**

创建 `internal/degrade/responses_dscompat_test.go`：

```go
package degrade

import (
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

func responsesDSCompatRoute(t *testing.T) (*Matrix, *Route) {
	t.Helper()
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := m.Route(ProtoOpenAIResponses, ProviderDashScopeCompatible)
	if !ok {
		t.Fatal("openai.responses -> dashscope.compatible 未注册")
	}
	return m, r
}

// TestResponsesDSCompatDispositions 逐格钉死这条路径的设计处置。
//
// 每一格都对应 2026-10-11 的一条实测（见
// docs/research/2026-10-11-dashscope-compatible-responses-reprobe.md），
// 改任何一格都该先有新证据。
func TestResponsesDSCompatDispositions(t *testing.T) {
	m, _ := responsesDSCompatRoute(t)

	want := map[canonical.Capability]Disposition{
		canonical.CapTextGeneration:      Passthrough,
		canonical.CapStreaming:           Passthrough,
		canonical.CapToolCalling:         Passthrough,
		canonical.CapParallelToolCalls:   Passthrough,
		canonical.CapReasoning:           Passthrough,
		canonical.CapVisionInput:         Passthrough,
		canonical.CapAudioInput:          Passthrough,
		canonical.CapStatefulConversation: Passthrough,
		canonical.CapImageDetail:         Degrade,
		canonical.CapWebSearch:           Degrade,
		canonical.CapStructuredOutput:    Degrade,
		canonical.CapFileInput:           Reject,
		canonical.CapAudioOutput:         Reject,
		canonical.CapComputerUse:         Reject,
		canonical.CapImageGeneration:     Reject,
	}
	if len(want) != len(ExpressibleSet(ProtoOpenAIResponses)) {
		t.Fatalf("期望表覆盖 %d 项，Responses 可表达 %d 项——两者必须一一对应",
			len(want), len(ExpressibleSet(ProtoOpenAIResponses)))
	}
	for c, d := range want {
		rule, ok := m.Lookup(ProtoOpenAIResponses, ProviderDashScopeCompatible, c)
		if !ok {
			t.Errorf("%s 没有声明", c)
			continue
		}
		if rule.Disposition != d {
			t.Errorf("%s 处置 = %s，期望 %s", c, rule.Disposition, d)
		}
	}
}

// TestResponsesDSCompatDesignScore：8 PASS + 3 DEGRADE + 4 REJECT，
// 设计分 (8 + 0.5×3) / 15。
func TestResponsesDSCompatDesignScore(t *testing.T) {
	m, r := responsesDSCompatRoute(t)

	if r.IsHomogeneous() {
		t.Error("该路径是 wire-compatible 异构转换，不得标记为同源快通道")
	}
	p := r.Preservation(m.Availability(), Endpoint(""))
	if p.Passthrough != 8 || p.Degrade != 3 || p.Reject != 4 || p.Emulate != 0 || p.EmulateOff != 0 {
		t.Errorf("设计处置 = pass %d deg %d rej %d emu %d emuOff %d，期望 8/3/4/0/0",
			p.Passthrough, p.Degrade, p.Reject, p.Emulate, p.EmulateOff)
	}
	if want := 9.5 / 15.0; p.DesignScore() != want {
		t.Errorf("设计保留度 = %.3f，期望 %.3f（9.5/15）", p.DesignScore(), want)
	}
}

// TestResponsesDSCompatSessionNoteStatesBoundaries 防的是 PASS 让人以为会话
// 与 OpenAI 等价：归属、留存与多 key 续轮三条边界必须写在 note 里。
func TestResponsesDSCompatSessionNoteStatesBoundaries(t *testing.T) {
	m, _ := responsesDSCompatRoute(t)
	rule, _ := m.Lookup(ProtoOpenAIResponses, ProviderDashScopeCompatible, canonical.CapStatefulConversation)

	if rule.RequiresFeature != "" {
		t.Errorf("透传的会话不该依赖运行时开关，实际依赖 %q", rule.RequiresFeature)
	}
	for _, kw := range []string{"上游", "不隔离", "7 天", "400"} {
		if !strings.Contains(rule.Note, kw) {
			t.Errorf("会话 note 缺少关键词 %q: %s", kw, rule.Note)
		}
	}
}

// TestOtherResponsesRoutesStillEmulateSession 防的是拆分时顺手改了别的路径：
// 另外三条 Responses 路径的会话处置不在本次范围（openai.compat 见 #21）。
func TestOtherResponsesRoutesStillEmulateSession(t *testing.T) {
	m, _ := responsesDSCompatRoute(t)

	for _, out := range []Provider{ProviderOpenAICompat, ProviderDashScopeNative, ProviderAnthropicMessages} {
		rule, ok := m.Lookup(ProtoOpenAIResponses, out, canonical.CapStatefulConversation)
		if !ok {
			t.Errorf("openai.responses -> %s 未声明会话处置", out)
			continue
		}
		if rule.Disposition != Emulate || rule.RequiresFeature != FeatureConversationStore {
			t.Errorf("openai.responses -> %s 会话处置 = %s（开关 %q），期望 EMULATE + convstore",
				out, rule.Disposition, rule.RequiresFeature)
		}
		// note 只该描述网关做了什么，不替上游下断言：同一句话挂在 openai.compat
		// 上时，「上游无服务端会话」对 OpenAI 本尊就是错的。
		if strings.Contains(rule.Note, "上游无服务端会话") {
			t.Errorf("openai.responses -> %s 的会话 note 仍在替上游下断言: %s", out, rule.Note)
		}
	}
}

// TestResponsesDSCompatBuiltinToolsUnchanged 防的是拆分时丢了内建工具处置。
func TestResponsesDSCompatBuiltinToolsUnchanged(t *testing.T) {
	m, _ := responsesDSCompatRoute(t)

	for _, out := range []Provider{ProviderDashScopeCompatible, ProviderDashScopeNative, ProviderAnthropicMessages} {
		for _, c := range []canonical.Capability{canonical.CapComputerUse, canonical.CapImageGeneration} {
			rule, _ := m.Lookup(ProtoOpenAIResponses, out, c)
			if rule.Disposition != Reject {
				t.Errorf("openai.responses -> %s 的 %s = %s，期望 REJECT", out, c, rule.Disposition)
			}
		}
	}
	for _, c := range []canonical.Capability{canonical.CapComputerUse, canonical.CapImageGeneration} {
		rule, _ := m.Lookup(ProtoOpenAIResponses, ProviderOpenAICompat, c)
		if rule.Disposition != Passthrough {
			t.Errorf("同源路径的 %s = %s，期望 PASSTHROUGH", c, rule.Disposition)
		}
	}
}
```

运行 `gofmt -w internal/degrade/responses_dscompat_test.go`（上面 map 字面量的对齐交给 gofmt）。

- [ ] **Step 2: 确认失败**

Run: `go test ./internal/degrade/ -run 'TestResponsesDSCompat|TestOtherResponsesRoutes' -v`
Expected: `TestResponsesDSCompatDispositions` FAIL（`stateful_conversation` 为 EMULATE、`image_detail` 为 PASSTHROUGH）；`TestResponsesDSCompatDesignScore` FAIL；`TestResponsesDSCompatSessionNoteStatesBoundaries` FAIL；`TestOtherResponsesRoutesStillEmulateSession` FAIL（note 含「上游无服务端会话」）。

- [ ] **Step 3: 实现 note 常量**

在 `rules_phase1.go` 的 const 块里，把 `noteEmulatedSession` 的定义与其上方注释换成：

```go
	// 网关侧模拟服务端会话的代价。这句话必须跟着 EMULATE 一起出现——
	// 客户端拿到的能力是完整的，但这份完整性是网关垫出来的，
	// 运维得知道它的边界在哪。
	//
	// 只描述网关做了什么，不替上游下断言：这条 note 同时挂在 openai.compat
	// 上，而 OpenAI 本尊是有服务端会话的。
	noteEmulatedSession = "会话由网关侧 ConversationStore 模拟提供，不使用上游的服务端会话。" +
		"Phase 1 为内存态：单副本正确，进程重启后历史丢失，多副本部署下会话不共享。" +
		"默认关闭，需显式开启 convstore"

	// 上游承担会话时的边界。PASS 不产生降级头，这些话只出现在矩阵文档里——
	// 所以更要写全：透传的是协议形状，不是 OpenAI 的会话语义。
	noteUpstreamSession = "会话由上游持久化：store 与 previous_response_id 原样转发，" +
		"response.id 不改写，不经网关 convstore。会话归属由上游 key 决定" +
		"（跨 key 归属范围未证实），共用同一把 key 的调用方之间不隔离；" +
		"省略 store 时内容按上游默认留存 7 天；多 key 部署下续轮可能落到另一把 key，" +
		"得到上游 400，网关不重试"

	noteResponsesSearchCompat = "搜索由上游内建工具执行；预览版类型 web_search_preview " +
		"由网关改写为 web_search；search_context_size 等附带选项不保证生效"
	noteResponsesImageDetailCompat = "DashScope Compatible 的 Responses 端点不区分 detail 档位" +
		"（实测各档 input_tokens 相同、编造值不报错）；精度与计费意图丢失"
	noteResponsesStructuredCompat = "DashScope Compatible 的 Responses 端点对 text.format 的 " +
		"json_schema 不生效（返回自由文本）、对 json_object 不保证输出合法 JSON、" +
		"对 function 工具的 strict 不校验；三者均原样透传"
```

- [ ] **Step 4: 实现规则拆分**

把 `responsesExtras` 的定义（从 `responsesExtras := func(r *Route, homogeneous bool) *Route {` 到其闭合的 `}`）换成两个函数：

```go
	// 会话与内建工具分开声明：两者没有因果关系，绑在一起时想让一条路径
	// 只换会话处置，就得连内建工具一起重写。
	//
	// 会话按出站区分。dashscope.compatible 的 Responses 端点自己持久化会话
	//（2026-10-11 实测：只传 previous_response_id 的续轮答得出首轮内容，
	// store:false 后续轮 400），由网关再垫一层是用一份内存态、单副本的会话
	// 去顶替上游原生的那份。其余出站保持模拟；openai.compat 该不该也透传
	// 是另一个问题（#21）。
	responsesSession := func(r *Route) *Route {
		if r.OutProvider() == ProviderDashScopeCompatible {
			return r.Pass(canonical.CapStatefulConversation).
				Override(canonical.CapStatefulConversation, Passthrough, noteUpstreamSession)
		}
		return r.Emulate(FeatureConversationStore, noteEmulatedSession,
			canonical.CapStatefulConversation)
	}

	responsesBuiltins := func(r *Route, homogeneous bool) *Route {
		if homogeneous {
			// 字节直通路径原样转发一切，内建工具也不例外。
			// 早先把 computer_use 在直通路径上判成 REJECT 是错的：
			// 网关根本没碰这个字段，凭什么替上游拒绝。
			return r.Pass(canonical.CapComputerUse, canonical.CapImageGeneration)
		}
		return r.
			Reject(noteBuiltinTool, canonical.CapComputerUse).
			Reject(noteImageViaJobs, canonical.CapImageGeneration)
	}
```

把同源路径的注册从 `responsesExtras(chatToOpenAI.Derive(...), true)` 改成：

```go
	if err := m.Add(responsesBuiltins(responsesSession(
		chatToOpenAI.Derive(ProtoOpenAIResponses, ProviderOpenAICompat)), true,
	).Redeem(EndpointOpenAIResponses, ExpressibleSet(ProtoOpenAIResponses)...).Build()); err != nil {
		return nil, err
	}
```

把其后的 `for _, base := range []*Route{chatToAnthropic, chatToDSCompat, chatToDSNative} { ... }` 循环换成：

```go
	for _, base := range []*Route{chatToAnthropic, chatToDSNative} {
		r := responsesBuiltins(responsesSession(
			base.Derive(ProtoOpenAIResponses, base.OutProvider())), false)
		if err := m.Add(r.Build()); err != nil {
			return nil, err
		}
	}

	// Responses 到 DashScope Compatible：wire-compatible，打上游的 Responses
	// 端点而不是 Chat 端点，所以不能照搬 Chat 路径的三格处置——同一个上游，
	// 两扇门的行为不同（2026-10-11 实测，见
	// docs/research/2026-10-11-dashscope-compatible-responses-reprobe.md）：
	//   - image_detail：Chat 门透传，这扇门四种取值 input_tokens 全相同。
	//   - web_search：这扇门是内建工具而非 enable_search 开关，note 不同。
	//   - structured_output：这扇门 json_schema 返回自由文本。它与 function
	//     工具的 strict 共用一格（UsedCapabilities），改 REJECT 会连带拒绝
	//     带 strict 的工具调用，所以停在 DEGRADE；拆格见 #22。
	// audio_input 沿用继承来的 PASS，但没有这扇门的证据，不在兑现之列。
	responsesToDSCompat := responsesBuiltins(responsesSession(
		chatToDSCompat.Derive(ProtoOpenAIResponses, ProviderDashScopeCompatible)), false).
		Override(canonical.CapImageDetail, Degrade, noteResponsesImageDetailCompat).
		Override(canonical.CapWebSearch, Degrade, noteResponsesSearchCompat).
		Override(canonical.CapStructuredOutput, Degrade, noteResponsesStructuredCompat)
	if err := m.Add(responsesToDSCompat.Build()); err != nil {
		return nil, err
	}
```

同时把该段上方的引导注释里「前者由网关模拟，后两者 Phase 1 不做跨 Provider 映射」改成「前者按出站区分（上游有就透传，没有由网关模拟），后两者 Phase 1 不做跨 Provider 映射」。

- [ ] **Step 5: 确认新测试通过**

Run: `go test ./internal/degrade/ -run 'TestResponsesDSCompat|TestOtherResponsesRoutes' -v`
Expected: 五条全部 PASS。

- [ ] **Step 6: 重新生成矩阵文档并跑包内全部测试**

Run: `make matrix-update && go test ./internal/degrade/`
Expected: PASS。若 `TestPreferenceMatchesPreservation` 失败，说明出站优先级与新的透传格数对不上——把失败信息原样报告，不要改 `OutboundPreference` 迎合。

检查生成结果：
Run: `grep -n '`openai.responses` | `dashscope.compatible`' docs/degradation-matrix.md`
Expected: 一行，状态「规划中」，设计分 `0.633`。

- [ ] **Step 7: 全量测试并提交**

Run: `go test ./...`
Expected: PASS（路径仍是 PLANNED，网关行为未变）。

```bash
GIT_MASTER=1 git add internal/degrade docs/degradation-matrix.md
GIT_MASTER=1 git commit -m "feat(degrade): Responses 到 DashScope Compatible 的会话改为透传

拆开 responsesExtras；image_detail 改 DEGRADE，web_search 与
structured_output 换成本门实测的 note。路径仍为规划中。

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: 主链路按矩阵决定是否进 convstore

**Files:**
- Modify: `internal/gateway/handler.go`（`serve` 内约 `:319-325`）
- Test: `internal/gateway/responses_dscompat_test.go`（新建）

**Interfaces:**
- Consumes: Task 4 的处置；`Matrix.Lookup(in Protocol, out Provider, c canonical.Capability) (Rule, bool)`；`h.in.protocol`
- Produces: `(*Handler).emulatesConversation(kind degrade.Provider) bool`

- [ ] **Step 1: 写失败测试**

创建 `internal/gateway/responses_dscompat_test.go`：

```go
package gateway

import (
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
)

// TestEmulatesConversationFollowsMatrix 防的是主链路自己决定谁该进 convstore：
// 判据只能是矩阵里该路径 stateful_conversation 的处置。写成按 Provider 名
// 硬编码，日后任一条路径改处置时主链路就会与矩阵分家——那种错在响应里看不
// 出来，只表现为会话悄悄换了保管人。
func TestEmulatesConversationFollowsMatrix(t *testing.T) {
	hs := newHarness(t, true, jsonUpstream(t, `{}`))

	cases := []struct {
		kind degrade.Provider
		want bool
	}{
		{degrade.ProviderOpenAICompat, true},
		{degrade.ProviderDashScopeNative, true},
		{degrade.ProviderAnthropicMessages, true},
		{degrade.ProviderDashScopeCompatible, false},
		// 未登记的组合没有可模拟的格子。
		{degrade.ProviderDashScopeWSRealtime, false},
	}
	for _, tc := range cases {
		if got := hs.h.emulatesConversation(tc.kind); got != tc.want {
			t.Errorf("emulatesConversation(%s) = %v，期望 %v", tc.kind, got, tc.want)
		}
	}
}

// TestChatHandlerNeverEmulatesConversation 防的是无状态协议被拖进会话逻辑：
// Chat 表达不出 stateful_conversation，该格是 N/A。
func TestChatHandlerNeverEmulatesConversation(t *testing.T) {
	hs := newChatHarness(t, true, jsonUpstream(t, `{}`))
	if hs.h.emulatesConversation(degrade.ProviderOpenAICompat) {
		t.Error("Chat 入站不该进入会话模拟")
	}
}
```

- [ ] **Step 2: 确认失败**

Run: `go test ./internal/gateway/ -run 'TestEmulatesConversation|TestChatHandlerNeverEmulates' -v`
Expected: 编译失败，`hs.h.emulatesConversation undefined`。

- [ ] **Step 3: 实现**

在 `handler.go` 的 `prepareConversation` 定义之前加：

```go
// emulatesConversation 回答「选中的这条路径，会话是不是由网关垫的」。
//
// 判据取自矩阵而不是 Provider 名：只有 stateful_conversation 声明为 EMULATE
// 的路径才进 convstore。上游自己持久化会话的路径（该格 PASSTHROUGH）必须
// 原样转发 store 与 previous_response_id——对它展开历史、写 store:false，
// 等于用一份内存态的会话顶替上游原生的那份，而客户端无从得知。
func (h *Handler) emulatesConversation(kind degrade.Provider) bool {
	rule, ok := h.deps.Matrix.Lookup(h.in.protocol, kind, canonical.CapStatefulConversation)
	return ok && rule.Disposition == degrade.Emulate
}
```

把 `serve` 里的

```go
	conversation, err := h.prepareConversation(r, caller, decoded, raw)
	if err != nil {
		return "bad_request", outbound, err
	}
	if conversation != nil {
		raw = conversation.raw
	}
```

换成

```go
	// 必须先问矩阵再进 prepareConversation：后者在没装配存储而请求显式依赖
	// 会话时返回内部错误，那条分支的前提是矩阵已经把请求拦下。对会话透传的
	// 路径这个前提不成立，不加这道判断就是一个 500。
	var conversation *conversationRequest
	if h.emulatesConversation(kind) {
		conversation, err = h.prepareConversation(r, caller, decoded, raw)
		if err != nil {
			return "bad_request", outbound, err
		}
		if conversation != nil {
			raw = conversation.raw
		}
	}
```

- [ ] **Step 4: 确认通过**

Run: `go test ./internal/gateway/ -v -run 'TestEmulatesConversation|TestChatHandlerNeverEmulates|TestEmulation|TestConversation|TestGatedEmulation|TestStreamingEmulation'`
Expected: 全部 PASS——既有的 convstore 测试走 `openai.compat`，该格仍是 EMULATE，行为不变。

- [ ] **Step 5: 全量测试并提交**

Run: `go test ./...`
Expected: PASS。

```bash
GIT_MASTER=1 git add internal/gateway/handler.go internal/gateway/responses_dscompat_test.go
GIT_MASTER=1 git commit -m "feat(gateway): 会话是否由网关模拟改由矩阵决定

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: 录制用例表与 fixture 构造（离线部分）

**Files:**
- Create: `tests/smoke/responses_dscompat_cases_test.go`

**Interfaces:**
- Consumes: `testkit.Fixture` / `testkit.Request` / `testkit.Response` / `testkit.UpstreamExpectation` / `testkit.SSEBody` / `testkit.ParseSSE`；`openairesponses.Decode([]byte) (*Decoded, error)`
- Produces（供 Task 7 使用，均在 `package smoke_test`）:
  - `type rdcCase struct { name, note string; seed string; build func(seed rdcSeed) string; upstream func(client string) string; stream bool; wantStatus int }`
  - `type rdcSeed struct { ID string; Output json.RawMessage }`
  - `func rdcCases() []rdcCase`
  - `func rdcSetModel(body, model string) (json.RawMessage, error)`
  - `func rdcFixture(c rdcCase, clientBody string, status int, contentType string, respBody []byte) (testkit.Fixture, error)`
  - 常量 `rdcLogicalModel = "logical-fast"`、`rdcHarnessUpstreamModel = "upstream-model"`、`rdcFixtureDir = "../../testdata/routes/openai.responses__dashscope.compatible"`

- [ ] **Step 1: 写失败测试**

创建 `tests/smoke/responses_dscompat_cases_test.go`，先只放测试与它需要的最小声明：

```go
//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/protocol/openairesponses"
	"github.com/yobo2u/omugw/internal/testkit"
)

// 录制产物按 harness 的固定名字写入：request 里是逻辑模型名，upstream 断言里
// 是 harness 的默认上游模型名。真实模型名只在发给上游的那一份请求里出现。
const (
	rdcLogicalModel         = "logical-fast"
	rdcHarnessUpstreamModel = "upstream-model"
	rdcFixtureDir           = "../../testdata/routes/openai.responses__dashscope.compatible"
	rdcFixturePrefix        = "openai.responses__dashscope.compatible/"
)

// rdcFakeSeed 是离线校验用的假种子：形状与真实首轮响应一致，内容是桩。
var rdcFakeSeed = rdcSeed{
	ID: "resp_seed",
	Output: json.RawMessage(`[{"type":"message","id":"msg_1","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"3","annotations":[]}]}]`),
}

// TestResponsesDSCompatCasesAreWellFormed 在不触网的前提下校验用例表：
// 录制要花钱，表写错了应该在这里而不是在账单上发现。
func TestResponsesDSCompatCasesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range rdcCases() {
		t.Run(c.name, func(t *testing.T) {
			if seen[c.name] {
				t.Fatalf("用例名重复: %s", c.name)
			}
			seen[c.name] = true
			if c.note == "" {
				t.Error("缺少 note：半年后没人知道这份录制在证明什么")
			}
			client := c.build(rdcFakeSeed)
			// 客户端请求体必须过得了网关的严格解码，否则回放时连上游都到不了。
			if _, err := openairesponses.Decode([]byte(client)); err != nil {
				t.Fatalf("客户端请求体过不了网关解码: %v\n%s", err, client)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(client), &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["model"]) != `"`+rdcLogicalModel+`"` {
				t.Errorf("model = %s，期望逻辑模型名 %q", fields["model"], rdcLogicalModel)
			}
			if _, has := fields["stream"]; has != c.stream {
				t.Errorf("stream 字段存在 = %v，与用例声明 %v 不符", has, c.stream)
			}

			f, err := rdcFixture(c, client, c.status(), "application/json", []byte(`{"id":"resp_x"}`))
			if c.stream {
				f, err = rdcFixture(c, client, 200, "text/event-stream",
					[]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
			}
			if err != nil {
				t.Fatalf("构造 fixture 失败: %v", err)
			}
			if err := f.Validate(); err != nil {
				t.Fatalf("fixture 不合法: %v", err)
			}
			var up map[string]json.RawMessage
			if err := json.Unmarshal(f.Upstream.Body, &up); err != nil {
				t.Fatal(err)
			}
			if string(up["model"]) != `"`+rdcHarnessUpstreamModel+`"` {
				t.Errorf("upstream 断言的 model = %s，期望 %q", up["model"], rdcHarnessUpstreamModel)
			}
			if bytes.Contains(f.Upstream.Body, []byte("web_search_preview")) {
				t.Error("upstream 断言里不该还有 web_search_preview——适配器会把它改名")
			}
		})
	}

	// 有损格子按「文件名即能力名」举证（degrade 的 fixture 门槛），缺一份就过不了 CI。
	for _, name := range []string{"image_detail", "web_search", "structured_output"} {
		if !seen[name] {
			t.Errorf("缺少与能力同名的用例 %q", name)
		}
	}
}

// TestRDCFixtureRedactsRequestID 防的是上游工单号入库：它不是凭据，但能
// 关联到账号的调用记录，没有理由留在仓库里。
func TestRDCFixtureRedactsRequestID(t *testing.T) {
	c := rdcCases()[0]
	f, err := rdcFixture(c, c.build(rdcFakeSeed), 400, "application/json",
		[]byte(`{"request_id":"abc-123","code":"InvalidParameter","message":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(f.Response.Body, []byte("abc-123")) {
		t.Errorf("request_id 未脱敏: %s", f.Response.Body)
	}
	if !bytes.Contains(f.Response.Body, []byte(`"request_id"`)) {
		t.Error("request_id 键应保留——扁平信封的形状是证据的一部分")
	}
}
```

- [ ] **Step 2: 确认失败**

Run: `go vet -tags=smoke ./tests/smoke/`
Expected: 编译失败，`undefined: rdcSeed`、`rdcCases`、`rdcFixture`。

- [ ] **Step 3: 实现**

在同一文件追加：

```go
// rdcSeed 是种子轮（不入库的前一轮真实请求）留下的两样东西。
type rdcSeed struct {
	ID     string
	Output json.RawMessage
}

// rdcCase 是一份待录制的用例。
type rdcCase struct {
	name string
	note string

	// seed 非空时先把它发给上游，取回 id 与 output 交给 build。种子轮不入库。
	seed string
	// build 给出客户端发给网关的请求体（model 为逻辑名）。
	build func(seed rdcSeed) string
	// upstream 给出适配器修补后的期望形状；nil 表示与客户端请求体只差 model。
	upstream func(client string) string

	stream     bool
	wantStatus int
}

func (c rdcCase) status() int {
	if c.wantStatus == 0 {
		return 200
	}
	return c.wantStatus
}

func rdcStatic(body string) func(rdcSeed) string {
	return func(rdcSeed) string { return body }
}

// rdcSetModel 把请求体里的 model 换掉，其余字段保持。
func rdcSetModel(body, model string) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		return nil, fmt.Errorf("请求体不是 JSON 对象: %w", err)
	}
	m, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	fields["model"] = m
	return json.Marshal(fields)
}

// rdcPNG 生成一张 64×64 的渐变图，足够让视觉模型说出主色调，又小到不值得
// 为它操心内联上限。
func rdcPNG() string {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: uint8(200 + x/2), G: uint8(40 + y), B: 40, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err) // 内存编码一张固定图片不会失败
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

const (
	rdcWeatherTool = `{"type":"function","name":"get_weather","description":"查询指定城市的当前天气",` +
		`"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}}`
	rdcTimeTool = `{"type":"function","name":"get_time","description":"查询指定城市的当前时间",` +
		`"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}}`
	rdcHard   = `一个数除以 7 余 3，除以 5 余 2，最小的正整数是多少？`
	rdcSchema = `{"type":"object","additionalProperties":false,` +
		`"properties":{"city":{"type":"string"},"population_millions":{"type":"number"}},` +
		`"required":["city","population_millions"]}`
)

func rdcBody(rest string) string {
	return `{"model":"` + rdcLogicalModel + `",` + rest + `}`
}

func rdcImageInput(detail string) string {
	part := `{"type":"input_image","image_url":"` + rdcPNG() + `"`
	if detail != "" {
		part += `,"detail":"` + detail + `"`
	}
	part += `}`
	return `"store":false,"input":[{"role":"user","content":[` +
		`{"type":"input_text","text":"这张图的主色调是什么？一句话。"},` + part + `]}]`
}

// rdcCases 是这条路径的全部录制用例。文件名即用例名；有损格子的举证用例
// 必须与能力同名（image_detail / web_search / structured_output）。
func rdcCases() []rdcCase {
	return []rdcCase{
		{
			name:  "basic",
			note:  "最小闭环：只改写 model，其余字段原样到达上游；响应原样回传。",
			build: rdcStatic(rdcBody(`"input":"用一句话介绍杭州。","store":false`)),
		},
		{
			name:   "streaming",
			note:   "流式文本：事件名与 OpenAI 同名，终止事件 response.completed 携带 usage。",
			build:  rdcStatic(rdcBody(`"input":"Say hi in three words.","store":false,"stream":true`)),
			stream: true,
		},
		{
			name:  "tool_calling",
			note:  "function 工具调用（非流式）。",
			build: rdcStatic(rdcBody(`"input":"查一下北京天气。","store":false,"tools":[` + rdcWeatherTool + `]`)),
		},
		{
			name:   "tool_calling-streaming",
			note:   "function 工具调用（流式）：response.function_call_arguments.delta/done 原样转发。",
			build:  rdcStatic(rdcBody(`"input":"查一下北京天气。","store":false,"stream":true,"tools":[` + rdcWeatherTool + `]`)),
			stream: true,
		},
		{
			name: "tool_calling-result",
			note: "工具循环第二轮：把上一轮 output（含 function_call 项）原样回传并附 function_call_output。" +
				"证明网关解码器接受上游产出的输出项。",
			seed: rdcBody(`"input":"查一下北京天气。","store":false,"tools":[` + rdcWeatherTool + `]`),
			build: func(s rdcSeed) string {
				return rdcBody(`"store":false,"tools":[` + rdcWeatherTool + `],"input":` +
					rdcReplay(`{"role":"user","content":"查一下北京天气。"}`, s.Output,
						rdcToolOutputs(s.Output, `{"temp_c":21,"sky":"晴"}`)))
			},
		},
		{
			name: "parallel_tool_calls-true",
			note: "parallel_tool_calls 显式 true：上游并行发起多个调用。",
			build: rdcStatic(rdcBody(`"input":"请同时查询北京、上海、广州三个城市的天气，以及东京的当前时间。必须通过工具查询。",` +
				`"store":false,"parallel_tool_calls":true,"tools":[` + rdcWeatherTool + `,` + rdcTimeTool + `]`)),
		},
		{
			name: "parallel_tool_calls-false",
			note: "parallel_tool_calls 显式 false：上游一次只发起一个调用。与 -true 对照证明该字段被兑现。",
			build: rdcStatic(rdcBody(`"input":"请同时查询北京、上海、广州三个城市的天气，以及东京的当前时间。必须通过工具查询。",` +
				`"store":false,"parallel_tool_calls":false,"tools":[` + rdcWeatherTool + `,` + rdcTimeTool + `]`)),
		},
		{
			name:  "reasoning",
			note:  "推理：输出含 reasoning 项，usage 带 reasoning_tokens。",
			build: rdcStatic(rdcBody(`"input":"` + rdcHard + `","store":false,"reasoning":{"effort":"low"}`)),
		},
		{
			name:   "reasoning-streaming",
			note:   "推理（流式）：response.reasoning_text.delta/done 原样转发。",
			build:  rdcStatic(rdcBody(`"input":"` + rdcHard + `","store":false,"stream":true,"reasoning":{"effort":"low"}`)),
			stream: true,
		},
		{
			name: "reasoning-replay",
			note: "把上一轮 output（含 reasoning 项）原样回传后续问。#11 的录制项：思考历史在本路径原样到达上游。",
			seed: rdcBody(`"input":"` + rdcHard + `","store":false,"reasoning":{"effort":"low"}`),
			build: func(s rdcSeed) string {
				return rdcBody(`"store":false,"input":` + rdcReplay(
					`{"role":"user","content":"`+rdcHard+`"}`, s.Output,
					`{"role":"user","content":"把刚才的答案加 1 是多少？只回数字。"}`))
			},
		},
		{
			name:  "vision_input",
			note:  "图片输入，不带 detail：不应出现降级头。",
			build: rdcStatic(rdcBody(rdcImageInput(""))),
		},
		{
			name:  "image_detail",
			note:  "DEGRADE 举证（文件名即能力名）：detail 原样到达上游，但上游不区分档位——与 vision_input 的 input_tokens 相同。",
			build: rdcStatic(rdcBody(rdcImageInput("low"))),
		},
		{
			name: "web_search",
			note: "DEGRADE 举证（文件名即能力名）：内建 web_search 由上游执行，输出含 web_search_call。",
			build: rdcStatic(rdcBody(`"input":"今天杭州的天气如何？","store":false,` +
				`"tools":[{"type":"web_search"}]`)),
		},
		{
			name: "web_search-preview",
			note: "预览版类型：网关把 tools 与 tool_choice 里的 web_search_preview 改写为 web_search，上游因此真的执行搜索。",
			build: rdcStatic(rdcBody(`"input":"今天杭州的天气如何？","store":false,` +
				`"tools":[{"type":"web_search_preview"}],"tool_choice":{"type":"web_search_preview"}`)),
			upstream: func(client string) string {
				return strings.ReplaceAll(client, `"web_search_preview"`, `"web_search"`)
			},
		},
		{
			name: "structured_output",
			note: "DEGRADE 举证（文件名即能力名）：text.format=json_schema 原样到达上游，上游不兑现，返回自由文本。",
			build: rdcStatic(rdcBody(`"input":"上海是哪个国家的城市？人口大约多少？","store":false,` +
				`"text":{"format":{"type":"json_schema","name":"city_info","strict":true,"schema":` + rdcSchema + `}}`)),
		},
		{
			name: "structured_output-json_object",
			note: "text.format=json_object 原样到达上游；上游不保证输出是可直接解析的 JSON。",
			build: rdcStatic(rdcBody(`"input":"上海是哪个国家的城市？人口大约多少？请用 JSON 回答。","store":false,` +
				`"text":{"format":{"type":"json_object"}}`)),
		},
		{
			name: "structured_output-tool-strict",
			note: "function 工具带 strict:true：与 text.format 共用 structured_output 一格，工具调用照常可用，上游不校验 strict。",
			build: rdcStatic(rdcBody(`"input":"查一下北京天气。","store":false,` +
				`"tools":[` + strings.Replace(rdcWeatherTool, `"type":"function",`, `"type":"function","strict":true,`, 1) + `]`)),
		},
		{
			name: "stateful-continue",
			note: "会话续轮：只传 previous_response_id，上游答出首轮口令。previous_response_id 原样到达上游，response.id 不改写。",
			seed: rdcBody(`"input":"My secret codeword is MANGO42. Reply OK."`),
			build: func(s rdcSeed) string {
				return rdcBody(`"input":"What was my codeword? One word.","previous_response_id":"` + s.ID + `"`)
			},
		},
		{
			name: "stateful-store-false",
			note: "store:false 的一轮不可续：上游返回扁平信封的 400，网关映射为 bad_request，不重试、不冷却凭据。",
			seed: rdcBody(`"input":"My secret codeword is KIWI77. Reply OK.","store":false`),
			build: func(s rdcSeed) string {
				return rdcBody(`"input":"What was my codeword? One word.","previous_response_id":"` + s.ID + `"`)
			},
			wantStatus: 400,
		},
		{
			name:  "stateful-store-omitted",
			note:  "省略 store：网关不写入该字段，上游按默认值存储（响应回显 store:true，留存 7 天）。",
			build: rdcStatic(rdcBody(`"input":"Reply OK."`)),
		},
		{
			name: "error-required-two-tools",
			note: "上游更窄的语义：tool_choice=required 配多个工具时上游 400，网关原样映射。",
			build: rdcStatic(rdcBody(`"input":"你好。","store":false,"tool_choice":"required",` +
				`"tools":[` + rdcWeatherTool + `,` + rdcTimeTool + `]`)),
			wantStatus: 400,
		},
	}
}

// rdcReplay 拼出「前文 + 上一轮 output 的全部条目 + 后文」的 input 数组。
// output 条目逐字节保留：要证明的正是网关与上游都接受上游自己产出的形状。
func rdcReplay(before string, output json.RawMessage, after string) string {
	var items []json.RawMessage
	_ = json.Unmarshal(output, &items)
	parts := []string{before}
	for _, it := range items {
		parts = append(parts, string(it))
	}
	if after != "" {
		parts = append(parts, after)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// rdcToolOutputs 为 output 里的每个 function_call 生成一条 function_call_output。
// 没有任何调用时返回空串——用例随后会因上游答非所问而在录制审阅时被发现。
func rdcToolOutputs(output json.RawMessage, result string) string {
	var items []struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
	}
	_ = json.Unmarshal(output, &items)
	var outs []string
	for _, it := range items {
		if it.Type != "function_call" {
			continue
		}
		quoted, _ := json.Marshal(result)
		outs = append(outs, `{"type":"function_call_output","call_id":"`+it.CallID+`","output":`+string(quoted)+`}`)
	}
	return strings.Join(outs, ",")
}

// rdcFixture 把一次真实交互装成 fixture。
//
// request 是客户端发给网关的内容；upstream 是适配器修补后应当发出的形状；
// response 是真实上游对后者（model 换成真实模型名）的回答。响应头只留
// content-type：其余头要么是工单号、要么随时间变化，进了 golden 只会制造噪音。
func rdcFixture(c rdcCase, clientBody string, status int, contentType string, respBody []byte) (testkit.Fixture, error) {
	expect := clientBody
	if c.upstream != nil {
		expect = c.upstream(clientBody)
	}
	upstreamBody, err := rdcSetModel(expect, rdcHarnessUpstreamModel)
	if err != nil {
		return testkit.Fixture{}, err
	}

	f := testkit.Fixture{
		Name: rdcFixturePrefix + c.name,
		Note: c.note,
		Request: testkit.Request{
			Method:  "POST",
			Path:    "/v1/responses",
			Headers: map[string]string{"authorization": "<redacted>", "content-type": "application/json"},
			Body:    json.RawMessage(clientBody),
		},
		Upstream: &testkit.UpstreamExpectation{
			Method: "POST",
			Path:   "/v1/responses",
			Body:   upstreamBody,
		},
		Response: testkit.Response{Status: status},
	}

	if strings.HasPrefix(contentType, "text/event-stream") {
		events, err := testkit.ParseSSE(bytes.NewReader(respBody))
		if err != nil {
			return testkit.Fixture{}, fmt.Errorf("解析上游 SSE 失败: %w", err)
		}
		if len(events) == 0 {
			return testkit.Fixture{}, fmt.Errorf("上游 SSE 没有任何事件")
		}
		f.Response.Headers = map[string]string{"content-type": "text/event-stream"}
		f.Response.SSE = &testkit.SSEBody{Events: events}
		return f, nil
	}

	if !json.Valid(respBody) {
		return testkit.Fixture{}, fmt.Errorf("上游响应体不是 JSON（status %d）", status)
	}
	f.Response.Headers = map[string]string{"content-type": "application/json"}
	f.Response.Body = rdcRedactRequestID(respBody)
	return f, nil
}

// rdcRedactRequestID 把顶层 request_id 的值换成占位，键保留。
func rdcRedactRequestID(body []byte) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return body
	}
	if _, ok := fields["request_id"]; !ok {
		return body
	}
	fields["request_id"] = json.RawMessage(`"<redacted>"`)
	out, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return out
}
```

- [ ] **Step 4: 确认通过**

Run: `go vet -tags=smoke ./tests/smoke/ && go test -tags=smoke -count=1 -run '^(TestResponsesDSCompatCasesAreWellFormed|TestRDCFixtureRedactsRequestID)$' ./tests/smoke/`
Expected: PASS。该命令只跑这两条离线测试，不触网。

若某个用例报「客户端请求体过不了网关解码」：读错误信息，修正用例体使之成为合法的 Responses 请求。**不要**为此放宽解码器。

- [ ] **Step 5: 提交**

```bash
GIT_MASTER=1 git add tests/smoke/responses_dscompat_cases_test.go
GIT_MASTER=1 git commit -m "test(smoke): Responses 到 DashScope Compatible 的录制用例表

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: 真实录制

> **需要用户在执行当时明确同意。** 本任务向真实百炼端点发送 25 次请求（21 个用例 + 4 个种子轮），产生少量费用。未获同意前只做 Step 1–2。

**Files:**
- Create: `tests/smoke/record_responses_dscompat_live_test.go`
- Create（录制产物）: `testdata/routes/openai.responses__dashscope.compatible/*.json`

**Interfaces:**
- Consumes: Task 6 的 `rdcCases`、`rdcSetModel`、`rdcFixture`、`rdcSeed`、`rdcFixtureDir`；`testkit.Save(path string, f Fixture) error`
- Produces: 21 份 fixture 文件，文件名为 `<用例名>.json`

- [ ] **Step 1: 写录制入口**

创建 `tests/smoke/record_responses_dscompat_live_test.go`：

```go
//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/testkit"
)

const (
	rdcRecordEnv   = "OMUGW_RECORD_DSCOMPAT_RESPONSES"
	rdcModelEnv    = "OMUGW_RECORD_DSCOMPAT_RESPONSES_MODEL"
	rdcOnlyEnv     = "OMUGW_RECORD_DSCOMPAT_RESPONSES_ONLY"
	rdcKeyEnv      = "DASHSCOPE_API_KEY"
	rdcDefaultBase = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	rdcDefaultModel = "qwen3.8-flash"
)

// TestRecordResponsesDSCompat 向真实 DashScope Compatible 的 Responses 端点
// 录制 fixture。
//
// 必须显式设置 OMUGW_RECORD_DSCOMPAT_RESPONSES=1 才执行：它会产生账单。
// 凭据只从环境变量读，绝不写进日志、错误信息或产物——失败信息只报状态码与
// 响应体，而响应体里没有凭据。
func TestRecordResponsesDSCompat(t *testing.T) {
	if os.Getenv(rdcRecordEnv) != "1" {
		t.Skipf("跳过真实录制：未设置 %s=1", rdcRecordEnv)
	}
	key := strings.TrimSpace(os.Getenv(rdcKeyEnv))
	if key == "" {
		t.Fatalf("缺少环境变量 %s", rdcKeyEnv)
	}
	model := strings.TrimSpace(os.Getenv(rdcModelEnv))
	if model == "" {
		model = rdcDefaultModel
	}
	// 逗号分隔的用例名；留空录全部。单个用例重录时用它，免得其余用例跟着漂移。
	only := map[string]bool{}
	for _, name := range strings.Split(os.Getenv(rdcOnlyEnv), ",") {
		if name = strings.TrimSpace(name); name != "" {
			only[name] = true
		}
	}

	client := &http.Client{Timeout: 4 * time.Minute}
	send := func(t *testing.T, body string) (int, string, []byte) {
		t.Helper()
		payload, err := rdcSetModel(body, model)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, rdcDefaultBase+"/responses", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			// 只报错误类别：*url.Error 的文本含 URL 不含请求头，但不值得赌。
			t.Fatalf("请求上游失败（传输层）")
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("读取上游响应失败")
		}
		return resp.StatusCode, resp.Header.Get("Content-Type"), raw
	}

	for _, c := range rdcCases() {
		if len(only) > 0 && !only[c.name] {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			var seed rdcSeed
			if c.seed != "" {
				status, _, raw := send(t, c.seed)
				if status != http.StatusOK {
					t.Fatalf("种子轮状态码 = %d: %s", status, raw)
				}
				var parsed struct {
					ID     string          `json:"id"`
					Output json.RawMessage `json:"output"`
				}
				if err := json.Unmarshal(raw, &parsed); err != nil || parsed.ID == "" {
					t.Fatalf("种子轮响应没有 id: %s", raw)
				}
				seed = rdcSeed{ID: parsed.ID, Output: parsed.Output}
			}

			clientBody := c.build(seed)
			expect := clientBody
			if c.upstream != nil {
				expect = c.upstream(clientBody)
			}
			status, contentType, raw := send(t, expect)
			// 状态码与预期不符就不落盘：一份意外的 4xx 入库后会被当成证据。
			if status != c.status() {
				t.Fatalf("状态码 = %d，期望 %d: %s", status, c.status(), raw)
			}

			f, err := rdcFixture(c, clientBody, status, contentType, raw)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(rdcFixtureDir, c.name+".json")
			if err := testkit.Save(path, f); err != nil {
				t.Fatalf("保存 fixture 失败: %v", err)
			}
			t.Logf("已录制 %s（model=%s status=%d bytes=%d）", c.name, model, status, len(raw))
		})
	}
}
```

运行 `gofmt -w tests/smoke/record_responses_dscompat_live_test.go`。

- [ ] **Step 2: 离线确认入口默认跳过**

Run: `go vet -tags=smoke ./tests/smoke/ && go test -tags=smoke -count=1 -run '^TestRecordResponsesDSCompat$' ./tests/smoke/ -v`
Expected: `--- SKIP: TestRecordResponsesDSCompat`，提示未设置开关。没有任何网络请求。

提交入口：

```bash
GIT_MASTER=1 git add tests/smoke/record_responses_dscompat_live_test.go
GIT_MASTER=1 git commit -m "test(smoke): Responses 到 DashScope Compatible 的录制入口

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

- [ ] **Step 3: 取得用户同意后执行录制**

向用户说明将要发生的事（25 次真实请求、模型 `qwen3.8-flash`、北京域名、产物写入 `testdata/routes/openai.responses__dashscope.compatible/`），得到明确同意后运行：

```bash
OMUGW_RECORD_DSCOMPAT_RESPONSES=1 go test -tags=smoke -count=1 -timeout 30m \
  -run '^TestRecordResponsesDSCompat$' ./tests/smoke/ -v 2>&1 | grep -E '^(=== RUN|--- |\s+record_|ok|FAIL|PASS)'
```

Expected: 21 个子测试 PASS。某个子测试因状态码不符而 FAIL 时，读它打印的上游响应体：
- 若是上游瞬时故障（5xx、限流），用 `OMUGW_RECORD_DSCOMPAT_RESPONSES_ONLY=<用例名>` 单独重录。
- 若是上游行为与 2026-10-11 实测不同（例如 `stateful-store-false` 返回了 200），**停下报告用户**——spec 的依据变了，不要改 `wantStatus` 迎合。
- 若 `web_search-preview` 返回 400：上游不接受 `tool_choice: {"type":"web_search"}`（这一点此前没有实测，见 Review Focus 第 2 条）。**停下报告用户**，由用户决定 `tool_choice` 是否改名。

- [ ] **Step 4: 审阅产物**

```bash
ls testdata/routes/openai.responses__dashscope.compatible/ | wc -l
grep -rlE 'sk-[A-Za-z0-9]{8,}|Bearer [A-Za-z0-9]' testdata/routes/openai.responses__dashscope.compatible/ ; echo "凭据扫描退出码: $?"
```
Expected: 21 个文件；grep 无输出、退出码 1。

逐项核对这些录制确实在证明各自声称的事（用 `python3 -c` 或 `jq` 读，不要整份 cat 流式文件）：

| 用例 | 必须成立 |
|---|---|
| `parallel_tool_calls-true` | `response.body.output` 里 `function_call` 项 ≥ 2 |
| `parallel_tool_calls-false` | `function_call` 项恰为 1 |
| `reasoning` | `output` 含 `type: reasoning` 的项 |
| `reasoning-replay`、`tool_calling-result` | 状态 200，`output` 含 `message` 项 |
| `vision_input` 与 `image_detail` | 两者 `usage.input_tokens` 相等 |
| `web_search`、`web_search-preview` | `output` 含 `type: web_search_call` 的项 |
| `structured_output` | `output` 里 message 的文本**不是**符合 schema 的 JSON |
| `stateful-continue` | 回答文本含 `MANGO42` |
| `stateful-store-false` | 状态 400，body 有 `code` 与 `message`，`request_id` 为 `<redacted>` |
| `stateful-store-omitted` | `response.body.store` 为 `true`；`upstream.body` 没有 `store` 键 |
| `error-required-two-tools` | 状态 400 |
| 三份流式用例 | 最后一个事件是 `response.completed` |

任何一项不成立都**停下报告用户**，附上该用例的实际观察。不要手改 fixture。

- [ ] **Step 5: 提交录制产物**

```bash
GIT_MASTER=1 git add testdata/routes/openai.responses__dashscope.compatible
GIT_MASTER=1 git commit -m "test(fixtures): 录制 Responses 到 DashScope Compatible 的真实上游交互

模型 qwen3.8-flash，北京域名，录制日期见提交时间。

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

此时 `go test ./...` 会因 `internal/degrade` 的 fixture 门槛失败吗？不会：门槛只检查**已实现**路径，本路径尚未 Redeem。运行确认：
Run: `go test ./...`
Expected: PASS。

---

### Task 8: 转正——兑现、白名单、回放与行为测试

**Files:**
- Modify: `internal/degrade/rules_phase1.go`（Task 4 的 `responsesToDSCompat` 加 `Redeem`）
- Modify: `internal/degrade/matrix_test.go`（两份名单）
- Modify: `internal/degrade/responses_dscompat_test.go`（兑现集合）
- Modify: `internal/gateway/harness_test.go`（新 harness；`newHarness` 的 PLANNED 哨兵）
- Create: `internal/gateway/responses_dscompat_conformance_test.go`
- Modify: `internal/gateway/responses_dscompat_test.go`（行为测试）
- Create（生成）: `testdata/routes/openai.responses__dashscope.compatible/golden/*.txt`

**Interfaces:**
- Consumes: Task 1–7 的全部产出
- Produces: `newResponsesDSCompatHarness(t *testing.T, ups ...*upstream) *harness`；路径在 `/v1/responses` 门兑现 10 项

- [ ] **Step 1: 写兑现集合的失败测试**

追加到 `internal/degrade/responses_dscompat_test.go`（import 加 `"reflect"`）：

```go
// redeemedResponsesDSCompat 是 /v1/responses 门在 dashscope.compatible 上兑现
// 的十项能力，按 AllCapabilities 顺序——与 RedeemedAt 的输出顺序一致。
var redeemedResponsesDSCompat = []canonical.Capability{
	canonical.CapTextGeneration,
	canonical.CapStreaming,
	canonical.CapToolCalling,
	canonical.CapParallelToolCalls,
	canonical.CapStructuredOutput,
	canonical.CapReasoning,
	canonical.CapVisionInput,
	canonical.CapImageDetail,
	canonical.CapStatefulConversation,
	canonical.CapWebSearch,
}

// TestResponsesDSCompatRedemptionIsExactlyTenCapabilities 钉死兑现集合：
// 7 PASS + 3 DEGRADE。audio_input 设计处置是 PASS 但没有这扇门的证据，
// 不在其列——含它的请求由矩阵以 501 拦下。
func TestResponsesDSCompatRedemptionIsExactlyTenCapabilities(t *testing.T) {
	m, r := responsesDSCompatRoute(t)

	if eps := r.Endpoints(); len(eps) != 1 || eps[0] != EndpointOpenAIResponses {
		t.Fatalf("已开门 = %v，期望仅 %s", eps, EndpointOpenAIResponses)
	}
	if got := r.RedeemedAt(EndpointOpenAIResponses); !reflect.DeepEqual(got, redeemedResponsesDSCompat) {
		t.Errorf("兑现集合 = %v\n期望 %v", got, redeemedResponsesDSCompat)
	}
	if r.Redeems(EndpointOpenAIResponses, canonical.CapAudioInput) {
		t.Error("audio_input 没有证据，不得兑现")
	}

	p := r.Preservation(m.Availability(), EndpointOpenAIResponses)
	if !p.Gated() {
		t.Error("audio_input 未兑现，门应为 Gated")
	}
	if want := 8.5 / 15.0; p.AvailableScore() != want {
		t.Errorf("门可用分 = %.3f，期望 %.3f（8.5/15）", p.AvailableScore(), want)
	}
	if want := 9.5 / 15.0; p.DesignScore() != want {
		t.Errorf("设计分 = %.3f，期望 %.3f（不因投放进度改变）", p.DesignScore(), want)
	}
}
```

若 `RedeemedAt` 的实际输出顺序与上表不同（它按 `canonical.AllCapabilities()` 排），以实际顺序为准调整表内顺序，不改集合内容。

Run: `go test ./internal/degrade/ -run TestResponsesDSCompatRedemption -v`
Expected: FAIL，「已开门 = []」。

- [ ] **Step 2: 兑现并同步白名单**

在 `rules_phase1.go` 的 `responsesToDSCompat` 链末尾（三个 `Override` 之后）追加，并补注释：

```go
		// 兑现门槛是端到端真实 fixture 通过（ADR-0001）：二十一份真实上游录制在
		// testdata/routes/openai.responses__dashscope.compatible/，回放与上游
		// 请求断言在 internal/gateway/responses_dscompat_conformance_test.go。
		// 四项 REJECT 不在兑现之列；audio_input 无证据不兑现，门保持 Gated。
		Redeem(EndpointOpenAIResponses,
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapParallelToolCalls,
			canonical.CapStructuredOutput,
			canonical.CapReasoning,
			canonical.CapVisionInput,
			canonical.CapImageDetail,
			canonical.CapStatefulConversation,
			canonical.CapWebSearch,
		)
```

在 `matrix_test.go` 的 `TestImplementedRoutesAreExplicit` 的 `want` 里加一行，并把上方注释补一句「以及 Responses 到 DashScope Compatible 的 wire-compatible 路径」：

```go
		string(ProtoOpenAIResponses) + " -> " + string(ProviderDashScopeCompatible): true,
```

在 `TestRedeemedCapabilitiesAreExplicit` 的 `want` 里加一项，并把上方注释补一句「Responses 到 DashScope Compatible 兑现十项（四项 REJECT 与无证据的 audio_input 不在其列）」：

```go
		string(ProtoOpenAIResponses) + " -> " + string(ProviderDashScopeCompatible) +
			" @ " + string(EndpointOpenAIResponses): {
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapParallelToolCalls,
			canonical.CapStructuredOutput,
			canonical.CapReasoning,
			canonical.CapVisionInput,
			canonical.CapImageDetail,
			canonical.CapStatefulConversation,
			canonical.CapWebSearch,
		},
```

运行 `gofmt -w internal/degrade/` 后：

Run: `make matrix-update && go test ./internal/degrade/`
Expected: PASS——fixture 门槛此时会检查本路径：目录存在、`image_detail.json` / `web_search.json` / `structured_output.json` 三份同名举证存在、每份 fixture 的 `request.path` 是 `/v1/responses`。

- [ ] **Step 3: harness 与 PLANNED 哨兵**

在 `internal/gateway/harness_test.go`：

把 `newHarness` 里的
```go
	if !implemented {
		kind = degrade.ProviderDashScopeCompatible
	}
```
改成
```go
	if !implemented {
		// 哨兵指向仍是 PLANNED 的 anthropic.messages：dashscope.compatible
		// 已转正，继续拿它当「未实现」会让 501 测试在转正当天变红。
		kind = degrade.ProviderAnthropicMessages
	}
```

在 `newChatDSCompatHarness` 之后加：

```go
// newResponsesDSCompatHarness 是 Responses -> DashScope Compatible 路径的 harness。
//
// 与 Chat 那条共用同一个适配器工厂：适配器按入站门分支，装成两个不同的
// 东西就测不到分支选错的情形。
func newResponsesDSCompatHarness(t *testing.T, ups ...*upstream) *harness {
	t.Helper()
	return newHarnessFor(t, harnessConfig{
		requestPath: "/v1/responses",
		kind:        degrade.ProviderDashScopeCompatible,
		newHandler:  NewResponsesHandler,
		limits:      config.Default().Limits,
		factory:     dashScopeCompatFactory,
	}, ups...)
}
```

Run: `go test ./internal/gateway/ -run 'TestPlannedRouteReturns501' -v`
Expected: PASS。

- [ ] **Step 4: 写回放测试**

创建 `internal/gateway/responses_dscompat_conformance_test.go`：

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

// responsesDSCompatRouteFixtures 是 Responses -> DashScope Compatible 路径的
// fixture 目录。全部是真实上游录制（tests/smoke/record_responses_dscompat_live_test.go）。
const responsesDSCompatRouteFixtures = "../../testdata/routes/openai.responses__dashscope.compatible"

// responsesDSCompatDegradedHeaders 按用例名钉死降级头必须包含的能力项；
// 不在表里的用例不许出现降级头。独立于 golden：golden 整体漂移时，降级语义
// 的丢失仍要在这里单独咬住。
var responsesDSCompatDegradedHeaders = map[string]string{
	"image_detail":                  "image_detail=",
	"web_search":                    "web_search=",
	"web_search-preview":            "web_search=",
	"structured_output":             "structured_output=",
	"structured_output-json_object": "structured_output=",
	"structured_output-tool-strict": "structured_output=",
}

// responsesDSCompatStatus 是客户端应得的状态码；缺省 200。
var responsesDSCompatStatus = map[string]int{
	"stateful-store-false":     http.StatusBadRequest,
	"error-required-two-tools": http.StatusBadRequest,
}

// TestResponsesDSCompatRouteConformance 回放本路径的全部真实录制。
//
// 除客户端响应 golden 外，逐项断言上游实际收到的 method / path / 鉴权 /
// 请求体——只比客户端响应会漏掉「适配器没做修补、fixture 仍返回成功」这种
// 假绿。会话字段的原样转发也靠这条断言守住：stateful-continue 的 upstream
// 声明里有 previous_response_id，被网关展开历史就对不上。
func TestResponsesDSCompatRouteConformance(t *testing.T) {
	for _, f := range testkit.LoadDir(t, responsesDSCompatRouteFixtures) {
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
			hs := newResponsesDSCompatHarness(t, up)

			body, err := json.Marshal(f.Request.Body)
			if err != nil {
				t.Fatal(err)
			}
			rec := hs.do(t, string(body), true)

			wantStatus := responsesDSCompatStatus[caseName(f.Name)]
			if wantStatus == 0 {
				wantStatus = http.StatusOK
			}
			if rec.Code != wantStatus {
				t.Fatalf("状态码 = %d，期望 %d: %s", rec.Code, wantStatus, rec.Body.String())
			}
			if got := up.calls.Load(); got != 1 {
				t.Fatalf("上游被调用 %d 次，期望恰 1 次（4xx 不得重试）", got)
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

			wantDegraded := responsesDSCompatDegradedHeaders[caseName(f.Name)]
			gotDegraded := rec.Header().Get(degrade.DegradationHeader)
			if wantDegraded == "" {
				if gotDegraded != "" {
					t.Errorf("该用例不应有降级头，实际 %q", gotDegraded)
				}
			} else if !strings.Contains(gotDegraded, wantDegraded) {
				t.Errorf("%s 应包含 %q，实际 %q", degrade.DegradationHeader, wantDegraded, gotDegraded)
			}
			// 会话不是网关垫的，任何用例都不许出现模拟头。
			if got := rec.Header().Get(EmulationHeader); got != "" {
				t.Errorf("本路径不经 convstore，不应有 %s: %q", EmulationHeader, got)
			}

			golden := filepath.Join(responsesDSCompatRouteFixtures, "golden", caseName(f.Name)+".txt")
			testkit.Golden(t, golden, []byte(renderResult(rec)))
		})
	}
}
```

- [ ] **Step 5: 跑回放（不带 -update），读失败**

Run: `go test ./internal/gateway/ -run TestResponsesDSCompatRouteConformance -v 2>&1 | grep -E '^\s*(---|===|.*状态码|.*上游|.*降级|.*golden)' | head -80`
Expected: 每个子测试只因「读取 golden 文件失败（首次生成请加 -update）」而 FAIL。**其余任何失败都要先处理**：

- `reasoning-replay` 或 `tool_calling-result` 报状态码 400 且响应体是网关的解码错误（「无法解析 Responses 请求」之类）：这是 Review Focus 第 1 条——网关的严格解码器不接受上游产出的输出项形状。**停下报告用户**，附上错误信息与被拒的字段名。这是需要用户裁决的产品问题（放宽解码器的哪一部分），不要删 fixture 字段或跳过用例。
- 降级头断言失败：对照 `responsesDSCompatDegradedHeaders` 与该用例的请求体，确认是测试表写错还是解码器的能力识别与预期不同；后者同样报告用户。
- 「上游收到的请求体语义不符」：适配器的修补与 fixture 的 upstream 声明不一致，回到 Task 3 排查。

- [ ] **Step 6: 生成并审阅 golden**

Run: `go test ./internal/gateway/ -run TestResponsesDSCompatRouteConformance -update && go test ./internal/gateway/ -run TestResponsesDSCompatRouteConformance`
Expected: 第二次运行全部 PASS。

人工审阅（必须做）：

```bash
for f in testdata/routes/openai.responses__dashscope.compatible/golden/*.txt; do echo "== $(basename $f)"; sed -n '1,/^---$/p' "$f"; done
```
Expected: 除 `stateful-store-false` 与 `error-required-two-tools` 首行为 `status: Bad Request` 外，其余均为 `status: OK`；只有 Step 4 表里的六个用例带 `X-Omugw-Degraded` 行；没有任何 `X-Omugw-Emulated` 行。

再看两份错误用例的正文：
```bash
sed -n '/^---$/,$p' testdata/routes/openai.responses__dashscope.compatible/golden/stateful-store-false.txt
```
Expected: OpenAI 嵌套信封，`error.message` 是上游原话（含 `Not found previous_response_id`），`error.code` 为 `InvalidParameter`，`error.type` 为 `invalid_request_error`。

- [ ] **Step 7: 写行为测试**

追加到 `internal/gateway/responses_dscompat_test.go`（import 补 `"encoding/json"`、`"io"`、`"net/http"`、`"strings"`、`"sync"`、`"github.com/yobo2u/omugw/internal/convstore"`）：

```go
// captureUpstream 记下上游收到的每个请求体，并按次序返回给定响应。
func captureUpstream(t *testing.T, responses ...string) (*upstream, func() [][]byte) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies [][]byte
	)
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		i := len(bodies) - 1
		mu.Unlock()
		if i >= len(responses) {
			i = len(responses) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responses[i])
	})
	return up, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), bodies...)
	}
}

const rdcOKResponse = `{"id":"resp_up_2","object":"response","status":"completed","store":true,` +
	`"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"MANGO42"}]}]}`

// TestResponsesDSCompatBypassesConvstoreEvenWhenEnabled 防的是运维开着
// convstore 时本路径被悄悄换了会话保管人：开关只该影响声明为 EMULATE 的
// 路径。被展开的征兆是上游收不到 previous_response_id、多出 store:false、
// 客户端拿到的 id 不是上游的。
func TestResponsesDSCompatBypassesConvstoreEvenWhenEnabled(t *testing.T) {
	up, bodies := captureUpstream(t, rdcOKResponse)
	hs := newResponsesDSCompatHarness(t, up)
	hs.matrix.WithAvailability(degrade.Availability{degrade.FeatureConversationStore: true})
	hs.h.deps.ConversationStore = convstore.NewMemoryStore(convstore.DefaultLimits(), nil)

	rec := hs.do(t, `{"model":"m","input":"What was my codeword?","previous_response_id":"resp_up_1"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", rec.Code, rec.Body.String())
	}

	got := bodies()
	if len(got) != 1 {
		t.Fatalf("上游被调用 %d 次，期望 1 次", len(got))
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got[0], &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["previous_response_id"]) != `"resp_up_1"` {
		t.Errorf("previous_response_id = %s，期望原样转发", fields["previous_response_id"])
	}
	if _, ok := fields["store"]; ok {
		t.Errorf("客户端没发 store，上游却收到了: %s", fields["store"])
	}
	if string(fields["input"]) != `"What was my codeword?"` {
		t.Errorf("input 被改写（历史被展开？）: %s", fields["input"])
	}

	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != "resp_up_2" {
		t.Errorf("response.id = %q，期望上游的 resp_up_2（不得改写）", resp.ID)
	}
	if h := rec.Header().Get(EmulationHeader); h != "" {
		t.Errorf("不应有模拟头: %q", h)
	}
}

// TestResponsesDSCompatContinuationWorksWithoutConvstore 防的是默认部署下
// 续轮 500：prepareConversation 在没装配存储而请求显式依赖会话时返回内部
// 错误，那条分支不该被本路径走到。显式 store:true 同理。
func TestResponsesDSCompatContinuationWorksWithoutConvstore(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","input":"hi","previous_response_id":"resp_up_1"}`,
		`{"model":"m","input":"hi","store":true}`,
	} {
		up, _ := captureUpstream(t, rdcOKResponse)
		hs := newResponsesDSCompatHarness(t, up)

		rec := hs.do(t, body, true)
		if rec.Code != http.StatusOK {
			t.Errorf("请求 %s 状态码 = %d，期望 200: %s", body, rec.Code, rec.Body.String())
		}
	}
}

// TestResponsesDSCompatRejectsBeforeUpstream 钉死触达上游前的两种失败：
// REJECT 格是 422（改请求），未兑现格是 501（等实现）。上游一次都不该被调用——
// 发出去再让上游静默忽略，正是矩阵要防的那种不可见损失。
func TestResponsesDSCompatRejectsBeforeUpstream(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"file_input 是 REJECT", `{"model":"m","store":false,"input":[{"role":"user","content":[` +
			`{"type":"input_file","file_id":"file-abc"}]}]}`, http.StatusUnprocessableEntity},
		{"computer_use 是 REJECT", `{"model":"m","store":false,"input":"hi",` +
			`"tools":[{"type":"computer_use_preview"}]}`, http.StatusUnprocessableEntity},
		{"audio_input 未兑现", `{"model":"m","store":false,"input":[{"role":"user","content":[` +
			`{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`, http.StatusNotImplemented},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, _ := captureUpstream(t, rdcOKResponse)
			hs := newResponsesDSCompatHarness(t, up)

			rec := hs.do(t, tc.body, true)
			if rec.Code != tc.want {
				t.Fatalf("状态码 = %d，期望 %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if n := up.calls.Load(); n != 0 {
				t.Errorf("上游被调用 %d 次，期望 0 次", n)
			}
		})
	}
}

// TestResponsesDSCompatContinuationMissIsNotRetriedOrCooled 防的是把续轮
// 未命中当成凭据故障：它换凭据不会好，重试只会把同一个 400 再打一遍；冷却
// 凭据则会让一个客户端的过期 id 拖垮别人的请求。
func TestResponsesDSCompatContinuationMissIsNotRetriedOrCooled(t *testing.T) {
	var n int
	first := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"request_id":"req-1","code":"InvalidParameter",`+
				`"message":"Not found previous_response_id: resp_gone."}`)
			return
		}
		_, _ = io.WriteString(w, rdcOKResponse)
	})
	second, _ := captureUpstream(t, rdcOKResponse)
	hs := newResponsesDSCompatHarness(t, first, second)

	miss := hs.do(t, `{"model":"m","input":"hi","previous_response_id":"resp_gone"}`, true)
	if miss.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400: %s", miss.Code, miss.Body.String())
	}
	if !strings.Contains(miss.Body.String(), "Not found previous_response_id") {
		t.Errorf("上游原话没有到达客户端: %s", miss.Body.String())
	}
	if !strings.Contains(miss.Body.String(), `"code":"InvalidParameter"`) {
		t.Errorf("上游 code 没有到达客户端: %s", miss.Body.String())
	}
	if got := second.calls.Load(); got != 0 {
		t.Errorf("续轮未命中后 failover 到了第二个上游（%d 次），期望不重试", got)
	}

	// 紧接着的正常请求仍应落在第一个上游：它的凭据没有被冷却。
	ok := hs.do(t, `{"model":"m","input":"hi","store":false}`, true)
	if ok.Code != http.StatusOK {
		t.Fatalf("后续请求状态码 = %d: %s", ok.Code, ok.Body.String())
	}
	if got := second.calls.Load(); got != 0 {
		t.Errorf("后续请求落到了第二个上游（%d 次）——第一个上游的凭据被冷却了", got)
	}
}
```

- [ ] **Step 8: 跑行为测试**

Run: `go test ./internal/gateway/ -run 'TestResponsesDSCompat' -v`
Expected: 全部 PASS。

若 `computer_use_preview` 那条报 400 而不是 422：解码器不认识该类型名，把用例体里的类型换成 `computer`（`builtinToolCapability` 认 `computer` / `computer_use` / `computer_use_preview` 三者，以解码器实际接受的为准）。

- [ ] **Step 9: 全量检查并提交**

Run: `make check`
Expected: PASS。若 `make matrix` 报文档不同步，跑 `make matrix-update` 后重试。

```bash
GIT_MASTER=1 git add internal/degrade internal/gateway testdata/routes/openai.responses__dashscope.compatible docs/degradation-matrix.md
GIT_MASTER=1 git commit -m "feat: 投放 openai.responses -> dashscope.compatible

在 /v1/responses 门兑现十项能力（7 PASS + 3 DEGRADE），会话由上游承担。
audio_input 无证据不兑现。

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: 文档同步

**Files:**
- Modify: `README.md`（约 `:44-54` 的「上游没有的，网关能垫就垫」条目；开头的已通车路径清单约 `:9`）
- Modify: `AGENTS.md`（NOTES 的「当前状态」段）
- Modify: `internal/gateway/AGENTS.md`（`build.go` 行里对 `dashscope.compatible` 的描述）
- Modify: `internal/protocol/AGENTS.md`（`openairesponses/` 行）
- Modify: `docs/superpowers/specs/2026-10-12-openai-responses-dashscope-compatible-design.md`（状态行）

**Interfaces:**
- Consumes: Task 8 之后 `docs/degradation-matrix.md` 的实际数字
- Produces: 无代码接口

- [ ] **Step 1: 取实际数字**

Run: `grep -n '`openai.responses` | `dashscope.compatible`' docs/degradation-matrix.md; grep -c '已实现' docs/degradation-matrix.md; go test ./internal/degrade/ -run TestImplementedRoutesAreExplicit -v | tail -3`
把输出里的设计分、可用分记下，下面各处以它为准（预期设计 0.633、可用 0.567）。

- [ ] **Step 2: README 的会话段**

把 `README.md` 里这两行：

```markdown
  DashScope Compatible 的 Responses 端点另有原生会话契约，其路径尚未投放，
  会话处置待[专项决定](https://github.com/yobo2u/omugw/issues/12)，不能一概视为无状态。
```

换成：

```markdown
  DashScope Compatible 的 Responses 端点自己持久化会话，因此
  `openai.responses → dashscope.compatible` 上该格是 `PASSTHROUGH`：`store` 与
  `previous_response_id` 原样转发，`response.id` 不改写，**不经 `convstore`**
  （即使已开启）。由此带来三条与 OpenAI 不同的边界，部署前必须知道：
  省略 `store` 时内容按上游默认**留存 7 天**；会话归属由上游 key 决定，
  **共用同一把上游 key 的调用方之间不隔离**（跨 key 的归属范围尚未实测）；
  多 key 部署下续轮可能落到另一把 key，得到上游 400，网关不重试。
  网关不投放 `GET` / `DELETE /responses/{id}`，客户端拿到的 id 只能用于续轮。
```

- [ ] **Step 3: README 的已通车清单**

`README.md` 第 5 行的状态句里，把「Chat → DashScope Compatible 与 Chat → DashScope Native 已转正」改成「Chat → DashScope Compatible、Chat → DashScope Native 与 Responses → DashScope Compatible 已转正」。

第 7 行「**其中 5 条已实现**」改成「**其中 6 条已实现**」。

在该段描述 `openai.chat → dashscope.native` 的括号结束之后、句号之前，接上（分数以 Step 1 取到的为准）：

```markdown
> 以及 `openai.responses → dashscope.compatible`（wire-compatible：请求仍是
> Responses 线格式，会话由上游持久化、不经 `convstore`；在 `/v1/responses` 门
> 兑现 10 项能力，`audio_input` 无证据不兑现；设计分 9.5/15 ≈ 0.633，
> 可用分 8.5/15 ≈ 0.567）
```

- [ ] **Step 4: 根 AGENTS.md 的状态段**

把 NOTES 里「**当前状态**」一段的开头

```markdown
- **当前状态**：14 条路径已登记，5 条已通车（`openai.responses →
  openai.compat`、`openai.chat → openai.compat`、`dashscope.native →
  dashscope.native`，均为同源直通；以及第一条异构路径 `openai.chat →
  dashscope.compatible`）。
```

改成（路径总数以 `grep -c` 矩阵文档的路径表为准；通车数为 6）：

```markdown
- **当前状态**：14 条路径已登记，6 条已通车（`openai.responses →
  openai.compat`、`openai.chat → openai.compat`、`dashscope.native →
  dashscope.native`，均为同源直通；以及三条异构路径 `openai.chat →
  dashscope.compatible`、`openai.chat → dashscope.native`、`openai.responses →
  dashscope.compatible`）。
```

并在该段「Chat 到 DashScope Native 是首条……」一句之前插入：

```markdown
  Responses 到 DashScope Compatible 同为 wire-compatible：请求保持 Responses
  线格式、只做定点修补（model 改写与 web_search_preview → web_search），在
  `/v1/responses` 门兑现 10 项（7 PASS + 3 DEGRADE）；会话由上游持久化，该格
  PASSTHROUGH、不经 convstore；`audio_input` 无证据不兑现（501），
  file_input / audio_output / computer_use / image_generation 为 REJECT 422；
  设计分 9.5/15 ≈ 0.633，门可用分 8.5/15 ≈ 0.567。同一上游的两扇门行为不同
  （这扇门 `detail` 档位被忽略、`json_schema` 返回自由文本），处置不得互相照搬。
```

把同段末尾的「其余 9 条路径打过去仍是 501」改成「其余 8 条路径打过去仍是 501」。

在 ANTI-PATTERNS 末尾加一条：

```markdown
- **不要**把 `dashscope.compatible` 的两扇门当成一回事：Chat 门与 Responses
  门是同一个上游的两个端点，实测行为不同（图片 `detail`、结构化输出、搜索
  参数形态、错误信封都不一样）。一扇门的 fixture 不构成另一扇门的证据。
```

- [ ] **Step 5: 子目录 AGENTS.md**

`internal/gateway/AGENTS.md` 的 `build.go` 行里，把「wire-compatible 的 `dashscope.compatible` 走专用定点修补器」改成「wire-compatible 的 `dashscope.compatible` 走专用定点修补器（按入站门分支：Chat 门与 Responses 门各有修补与错误信封）」。

同文件第 11 行 `handler.go` 的描述末尾补：「；会话是否进 `convstore` 由 `emulatesConversation` 按矩阵该格处置决定，只有 EMULATE 才进」。

`internal/protocol/AGENTS.md` 的 `openairesponses/` 行末尾补：「`parallel_tool_calls` 显式 `true` 时由解码器上报给矩阵（与 `openaichat` 同条件）」。

- [ ] **Step 6: spec 状态**

把 spec 第 4 行 `**状态**：已批准，待实现` 改成 `**状态**：已实现`。

- [ ] **Step 7: 检查残留的过期表述**

Run: `grep -rn '其路径尚未投放\|上游无服务端会话' --include='*.md' --include='*.go' . | grep -v 'docs/superpowers\|docs/research'`
Expected: 无输出（`docs/degradation-matrix.md` 已由 `make matrix-update` 重新生成）。

- [ ] **Step 8: 最终检查并提交**

Run: `make check`
Expected: PASS。

```bash
GIT_MASTER=1 git add README.md AGENTS.md internal/gateway/AGENTS.md internal/protocol/AGENTS.md docs/superpowers/specs/2026-10-12-openai-responses-dashscope-compatible-design.md
GIT_MASTER=1 git commit -m "docs: 同步 Responses 到 DashScope Compatible 的投放状态

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

## 完成后

- 不推送、不开 PR，除非用户要求。
- 向用户报告：兑现的 10 项、未证实项（跨 key 归属、其他地域、`audio_input`）、录制中任何与 2026-10-11 实测不符的观察。
- 地图 #3 的 `Decisions so far` 与 #11 的后续评论属于共享外部操作，需用户授权后再做。
