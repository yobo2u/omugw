# 出站适配器共享契约套件 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 建立 `internal/provider/providertest` 声明式契约套件，让每个出站适配器
用一份 `Subject` 声明差异、由套件统一断言 10 条不变量，消除各适配器测试之间
隐形的覆盖缺口。

**Architecture:** 测试侧独立声明契约（不给生产接口加 `Descriptor()`，不用 golden）。
不变量分「传输无关族」与「HTTP 传输族」两层，前者不引用 `httptest`，为 Phase 1
将要接入的三条 WebSocket 出站预留复用位。头转发用严格闭集断言：上游收到的头减去
标准库自动头后，必须恰好等于网关自造头 ∪ `Subject.ForwardedHeaders`。

**Tech Stack:** Go 1.25，标准库 `testing` / `net/http/httptest`，仓库内
`internal/canonical`、`internal/degrade`、`internal/transport/httpx`。无新依赖。

**设计文档:** `docs/superpowers/specs/2026-08-21-provider-contract-suite-design.md`

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/provider/providertest/providertest.go` | `Subject` / `Deps` 类型定义、`Run` 入口、必填项校验 |
| `internal/provider/providertest/harness.go` | `httptest` 服务器、请求捕获、调用辅助 |
| `internal/provider/providertest/transport_agnostic.go` | 不变量 1-4（传输无关族） |
| `internal/provider/providertest/http_contract.go` | 不变量 5-10（HTTP 传输族）+ 严格闭集头断言 |
| `internal/provider/passthrough/contract_test.go` | 两个 `Subject`（openai.compat / dashscope.native） |
| `internal/provider/dashscopecompat/contract_test.go` | 一个 `Subject`（dashscope.compatible） |

`providertest` 是普通包而非 `_test.go`，因为要被多个适配器的测试包导入。它 import
`testing`，但不被任何生产代码引用，`cmd/omugw` 构建不受影响。

按不变量族分文件而不是把全部断言堆进一个文件：WS 适配器接入时新增
`ws_contract.go`，`transport_agnostic.go` 原样复用，改动边界清晰。

## 提交序列

1. Task 1-8：套件本体 + 三个 `Subject` 接入（新旧测试并存，全绿）
2. Task 9：严格闭集若暴露真实行为差异则修复（无差异则此提交不存在）
3. Task 10：删除被覆盖的旧测试 + 注释搬运

第 3 步单独成提交，让 review 能直接对照「删掉的断言 ↔ 套件里的哪一条」。

**所有 git 命令必须以 `GIT_MASTER=1` 开头。**

---

### Task 1: Subject 与 Deps 类型 + 必填项校验

**Files:**
- Create: `internal/provider/providertest/providertest.go`
- Test: `internal/provider/providertest/providertest_test.go`

- [ ] **Step 1: 写失败测试**

```go
package providertest

import (
	"strings"
	"testing"
)

// TestValidateReportsMissingFields 固化「必填项缺失即失败」。
//
// 不静默跳过对应的不变量族——跳过等于把「没跑」伪装成「跑过了」，
// 而这个套件存在的全部理由就是消除那种隐形缺口。
func TestValidateReportsMissingFields(t *testing.T) {
	err := validate(Subject{Name: "empty"})
	if err == nil {
		t.Fatal("零值 Subject 应当报错")
	}
	for _, want := range []string{"Kind", "New", "ValidBody", "RateLimitEnvelope"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息未指出缺失项 %q: %v", want, err)
		}
	}
}

// TestValidateAcceptsCompleteSubject 保证填全后不再报错。
func TestValidateAcceptsCompleteSubject(t *testing.T) {
	s := Subject{
		Name:              "complete",
		Kind:              "test.provider",
		New:               func(*testing.T, Deps) provider.Provider { return nil },
		DefaultPath:       "/v1/x",
		ValidBody:         `{"model":"m"}`,
		RateLimitEnvelope: `{"error":{}}`,
	}
	if err := validate(s); err != nil {
		t.Errorf("完整 Subject 不应报错: %v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/provider/providertest/ -run TestValidate -v`
Expected: FAIL，`undefined: validate`、`undefined: Subject`

- [ ] **Step 3: 写实现**

```go
// Package providertest 是出站适配器的共享契约套件。
//
// 每个适配器的测试包填一份 Subject 声明自己的差异，由本包统一断言全部不变量。
// 契约集中在这里而不是散落在各包的测试函数里，才有单一位置能回答
// 「一个出站适配器必须满足什么」——否则漏掉哪一条谁都不知道。
package providertest

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// Subject 是一个待测适配器的声明。
//
// 用具名结构体而不是位置参数：将来加第七、第八项声明时，不必回到每个调用点
// 数位置。理由与 dashscopecompat 测试里 callInput 的收敛一致。
type Subject struct {
	// Name 出现在子测试名里，便于定位是哪个适配器失败。
	Name string

	// Kind 是该适配器应当返回的协议族。
	Kind degrade.Provider

	// New 构造适配器。套件按传输族注入不同的依赖。
	New func(t *testing.T, deps Deps) provider.Provider

	// DefaultPath 是请求未带 Path 时应落到的端点。
	DefaultPath string

	// ForwardedHeaders 是允许原样带给上游的客户端头。
	//
	// 零值即最严：留空表示一个客户端头都不许转发。要放宽必须显式列出来——
	// 默认宽松会让「忘记声明」和「确实不转发」不可区分。
	ForwardedHeaders []string

	// StreamHeaders 是流式时除 Accept 之外必须落位的额外头。
	// DashScope Native 把「是否流式」放在 X-DashScope-SSE 上，丢了它
	// 上游会按非流式返回，整条流的语义就变了。
	StreamHeaders map[string]string

	// ValidBody 是该协议下一份合法的请求体，含 model 字段。
	//
	// 各协议请求体形状不同（Native 是 input.messages 嵌套，Responses 是顶层
	// input，Chat 是顶层 messages），所以由 Subject 自带而非套件内置常量。
	ValidBody string

	// RateLimitEnvelope 是上游 429 的错误信封样例。
	// 各协议族信封不同形（DashScope 扁平 {code,message}，OpenAI 系嵌套），
	// 套件只断言解码结果，不关心信封长什么样。
	RateLimitEnvelope string
}

// Deps 是套件注入给适配器构造函数的依赖。
//
// 存在这一层间接而不是直接传 *httpx.Client：套件要控制 httptest 服务器与
// 固定时钟，且将来 WebSocket driver 接入时只扩展 Deps，不改任何调用点。
type Deps struct {
	HTTPClient *httpx.Client
	Now        func() time.Time
}

// validate 检查必填项。返回的错误列出全部缺失项，而不是遇到第一个就返回——
// 让填写的人一次看到所有要补的东西。
func validate(s Subject) error {
	var missing []string
	if s.Kind == "" {
		missing = append(missing, "Kind")
	}
	if s.New == nil {
		missing = append(missing, "New")
	}
	if s.ValidBody == "" {
		missing = append(missing, "ValidBody")
	}
	if s.RateLimitEnvelope == "" {
		missing = append(missing, "RateLimitEnvelope")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("Subject %q 缺少必填项: %s", s.Name, strings.Join(missing, ", "))
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/provider/providertest/ -run TestValidate -v`
Expected: PASS，两个测试均通过

- [ ] **Step 5: 提交**

```bash
GIT_MASTER=1 git add internal/provider/providertest/
GIT_MASTER=1 git commit -m "providertest：Subject 声明与必填项校验"
```

---

### Task 2: 测试夹具（httptest 服务器与请求捕获）

**Files:**
- Create: `internal/provider/providertest/harness.go`
- Test: `internal/provider/providertest/harness_test.go`

- [ ] **Step 1: 写失败测试**

```go
package providertest

import (
	"io"
	"net/http"
	"testing"
)

// TestHarnessCapturesUpstreamRequest 验证夹具确实记下了上游收到什么。
// 后面每一条不变量都建立在这份捕获之上。
func TestHarnessCapturesUpstreamRequest(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Probe", "v")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if h.got.method != http.MethodPost {
		t.Errorf("method = %q，期望 POST", h.got.method)
	}
	if h.got.path != "/v1/x" {
		t.Errorf("path = %q，期望 /v1/x", h.got.path)
	}
	if h.got.header.Get("X-Probe") != "v" {
		t.Errorf("头未被捕获: %v", h.got.header)
	}
}

// TestRefTimeIsFixed 固化时钟是固定的。
//
// Retry-After 的 HTTP-date 形式依赖当前时间，用真实时钟会让测试在跨秒边界
// 随机失败——那种失败最难查，因为重跑就好了。
func TestRefTimeIsFixed(t *testing.T) {
	if refTime.IsZero() {
		t.Fatal("refTime 不能是零值")
	}
	if !refTime.Equal(refTime) {
		t.Fatal("refTime 必须稳定")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/provider/providertest/ -run "TestHarness|TestRefTime" -v`
Expected: FAIL，`undefined: newHarness`、`undefined: refTime`

- [ ] **Step 3: 写实现**

```go
package providertest

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// refTime 是套件的固定时钟。
//
// Retry-After 的 HTTP-date 形式依赖当前时间；用真实时钟会让断言在跨秒边界
// 随机失败，而那种失败重跑就好，最难查。
var refTime = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

// gatewaySecret 是套件里网关自己的凭据。
// 断言上游必须收到它，而不是客户端发来的那份。
const gatewaySecret = "sk-gateway-own-key"

// clientSecret 是套件伪装的客户端凭据。它绝不该到达上游。
const clientSecret = "sk-client-must-not-leak"

// captured 记录上游实际收到了什么。
type captured struct {
	body   []byte
	header http.Header
	method string
	path   string
}

// harness 是一次契约断言的运行环境。
type harness struct {
	server *httptest.Server
	got    *captured
	deps   Deps
}

// newHarness 起一个记录请求的上游桩，并备好注入固定时钟的依赖。
func newHarness(t *testing.T, h http.HandlerFunc) *harness {
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

	now := func() time.Time { return refTime }
	return &harness{
		server: srv,
		got:    got,
		deps: Deps{
			HTTPClient: httpx.New(config.Default().Timeouts, now),
			Now:        now,
		},
	}
}

// okServer 是最常见的桩：收下请求，回一个空的 2xx JSON。
func okServer(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
}

// callOpts 是一次适配器调用的可变输入。零值即最常见的那一种。
type callOpts struct {
	body    string
	stream  bool
	path    string
	baseURL string
	header  http.Header

	// upstreamModel 留空时用 defaultUpstreamModel。
	// 显式传空要用 emptyUpstreamModel 标记，供「装配错误归网关」那条断言使用。
	upstreamModel       string
	emptyUpstreamModel  bool
}

// defaultUpstreamModel 是套件断言模型改写时期望上游收到的名字。
// 与 ValidBody 里的逻辑名不同，才能证明改写确实发生了。
const defaultUpstreamModel = "upstream-model-name"

// call 发起一次适配器调用，成功时把响应体的关闭登记进 t.Cleanup。
//
// 在这里登记而不是让每个断言自己关：httpx 的 Body 上挂着空闲计时器与整体
// 超时的 cancel，漏关会把它们一起漏掉；而且 httptest.Server.Close 会等未完成的
// 连接，漏关能让收尾挂住。t.Cleanup 是 LIFO，newHarness 先登记 srv.Close，
// 这里后登记 Body.Close，于是先关体、后关服务器——顺序正是要的那个。
func (h *harness) call(t *testing.T, s Subject, o callOpts) (*httpx.Response, error) {
	t.Helper()

	p := s.New(t, h.deps)

	body := o.body
	if body == "" {
		body = s.ValidBody
	}
	baseURL := o.baseURL
	if baseURL == "" {
		baseURL = h.server.URL
	}
	model := o.upstreamModel
	if model == "" && !o.emptyUpstreamModel {
		model = defaultUpstreamModel
	}

	resp, err := p.Call(context.Background(), provider.Request{
		Target: router.Target{
			Kind:           s.Kind,
			Endpoint:       "contract-test",
			BaseURL:        baseURL,
			UpstreamModel:  model,
			CredentialPool: "contract-test",
		},
		Credential: credential.Credential{ID: "k1", Secret: gatewaySecret},
		Raw:        []byte(body),
		Stream:     o.stream,
		Path:       o.path,
		Header:     o.header,
	})
	if err == nil && resp != nil {
		t.Cleanup(func() { resp.Body.Close() })
	}
	return resp, err
}
```

注意：上面用到了 `io` 与 `context`，import 块需包含 `"context"` 与 `"io"`。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/provider/providertest/ -run "TestHarness|TestRefTime" -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
GIT_MASTER=1 git add internal/provider/providertest/harness.go internal/provider/providertest/harness_test.go
GIT_MASTER=1 git commit -m "providertest：上游桩与固定时钟夹具"
```

---

### Task 3: 传输无关族（不变量 1-4）

**Files:**
- Create: `internal/provider/providertest/transport_agnostic.go`

- [ ] **Step 1: 写实现（本任务的「测试」就是断言函数本身，由 Task 6-8 的接入验证）**

```go
package providertest

import (
	"errors"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// runTransportAgnostic 跑对任何出站适配器都成立的不变量。
//
// 这一族不引用 httptest 之外的 HTTP 概念（method、Accept、路径拼接都不在这里），
// 是为了 Phase 1 将要接入的三条 WebSocket 出站能原样复用——届时新增 WS driver
// 与 WS 专属族，这四条不必重写。
func runTransportAgnostic(t *testing.T, s Subject) {
	t.Run("身份", func(t *testing.T) {
		h := okServer(t)
		p := s.New(t, h.deps)
		if p.Kind() != s.Kind {
			t.Errorf("Kind() = %q，期望 %q", p.Kind(), s.Kind)
		}
	})

	// 凭据隔离是安全约束，不是风格问题：把客户端发来的 Authorization 转给上游，
	// 等于把一份密钥泄露给一个没有理由知道它的第三方。
	t.Run("凭据隔离", func(t *testing.T) {
		h := okServer(t)

		clientHeader := http.Header{}
		clientHeader.Set("Authorization", "Bearer "+clientSecret)

		if _, err := h.call(t, s, callOpts{header: clientHeader}); err != nil {
			t.Fatal(err)
		}

		auth := h.got.header.Get("Authorization")
		if auth != "Bearer "+gatewaySecret {
			t.Errorf("Authorization = %q，期望网关自己的凭据", auth)
		}
		if strings.Contains(auth, clientSecret) {
			t.Error("客户端密钥被转发给了上游")
		}
	})

	// 路由目标缺上游模型名是网关自己的装配错误，不是客户端的错。
	// 判成 bad_request 会误导客户端去改一个它没写错的请求。
	t.Run("装配错误归网关", func(t *testing.T) {
		h := okServer(t)
		_, err := h.call(t, s, callOpts{emptyUpstreamModel: true})
		assertClass(t, err, canonical.ClassInternal)
	})

	t.Run("客户端错误归客户端", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			body string
		}{
			{"非 JSON", `not json at all`},
			{"JSON 数组", `[1,2,3]`},
			{"JSON 字符串", `"a string"`},
			{"缺 model", `{"input":"hi"}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := okServer(t)
				_, err := h.call(t, s, callOpts{body: tc.body})
				assertClass(t, err, canonical.ClassBadRequest)
			})
		}
	})
}

// assertClass 断言错误是 *canonical.Error 且分类正确。
func assertClass(t *testing.T, err error, want canonical.ErrorClass) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望 %q 错误，实际为 nil", want)
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T: %v", err, err)
	}
	if cerr.Class != want {
		t.Errorf("分类 = %q，期望 %q", cerr.Class, want)
	}
}
```

import 块需含 `"net/http"` 与 `"strings"`。

- [ ] **Step 2: 确认编译通过**

Run: `go build ./internal/provider/providertest/ && go vet ./internal/provider/providertest/`
Expected: 无输出（成功）

- [ ] **Step 3: 提交**

```bash
GIT_MASTER=1 git add internal/provider/providertest/transport_agnostic.go
GIT_MASTER=1 git commit -m "providertest：传输无关族四条不变量"
```

---

### Task 4: HTTP 传输族之请求形状（不变量 5-9）

**Files:**
- Create: `internal/provider/providertest/http_contract.go`

- [ ] **Step 1: 写实现**

```go
package providertest

import (
	"encoding/json"
	"net/http"
	"testing"
)

// runHTTPContract 跑 HTTP 出站特有的不变量。
// WS 适配器接入时不跑这一族，另建 ws_contract.go。
func runHTTPContract(t *testing.T, s Subject) {
	t.Run("请求形状", func(t *testing.T) {
		h := okServer(t)
		if _, err := h.call(t, s, callOpts{}); err != nil {
			t.Fatal(err)
		}
		if h.got.method != http.MethodPost {
			t.Errorf("method = %q，期望 POST", h.got.method)
		}
		if ct := h.got.header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q，期望 application/json", ct)
		}
	})

	// 流式信号丢了，上游会按非流式返回，整条流的语义就变了。
	// DashScope Native 把这个信号放在头上而非请求体里，所以除 Accept 之外
	// 还要查 Subject 声明的额外头。
	t.Run("流式信号落位", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			stream     bool
			wantAccept string
		}{
			{"非流式", false, "application/json"},
			{"流式", true, "text/event-stream"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := okServer(t)
				if _, err := h.call(t, s, callOpts{stream: tc.stream}); err != nil {
					t.Fatal(err)
				}
				if a := h.got.header.Get("Accept"); a != tc.wantAccept {
					t.Errorf("Accept = %q，期望 %q", a, tc.wantAccept)
				}
				for name, want := range s.StreamHeaders {
					got := h.got.header.Get(name)
					if tc.stream && got != want {
						t.Errorf("流式时 %s = %q，期望 %q", name, got, want)
					}
					if !tc.stream && got != "" {
						t.Errorf("非流式时不应带 %s，实际 %q", name, got)
					}
				}
			})
		}
	})

	// 按语义比对而不是按字节：重新序列化会改变键序，钉死字节等于钉死
	// encoding/json 的实现细节，而那不是我们要保证的契约。
	t.Run("模型改写", func(t *testing.T) {
		h := okServer(t)
		if _, err := h.call(t, s, callOpts{}); err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(h.got.body, &fields); err != nil {
			t.Fatalf("上游收到的不是 JSON 对象: %s", h.got.body)
		}
		var model string
		if err := json.Unmarshal(fields["model"], &model); err != nil {
			t.Fatalf("model 不是字符串: %s", fields["model"])
		}
		if model != defaultUpstreamModel {
			t.Errorf("model = %q，期望改写成 %q", model, defaultUpstreamModel)
		}
	})

	// 同一个适配器要服务多个上游端点，路径只能随请求走，不能写死在装配时；
	// 保留默认值是为了不影响既有单端点装配。
	t.Run("路径来源", func(t *testing.T) {
		t.Run("请求路径优先", func(t *testing.T) {
			h := okServer(t)
			const override = "/v1/override/endpoint"
			if _, err := h.call(t, s, callOpts{path: override}); err != nil {
				t.Fatal(err)
			}
			if h.got.path != override {
				t.Errorf("path = %q，期望 %q", h.got.path, override)
			}
		})

		t.Run("留空退回默认", func(t *testing.T) {
			h := okServer(t)
			if _, err := h.call(t, s, callOpts{}); err != nil {
				t.Fatal(err)
			}
			if h.got.path != s.DefaultPath {
				t.Errorf("path = %q，期望默认 %q", h.got.path, s.DefaultPath)
			}
		})
	})

	// 字符串拼接对 URL 组件没有概念，这里要的是结构化拼接的结果：
	// base_url 带前缀时端点必须追加在前缀之后，而不是把前缀截断或吞掉。
	t.Run("base_url 前缀保留", func(t *testing.T) {
		h := okServer(t)
		const prefix = "/proxy/upstream"
		if _, err := h.call(t, s, callOpts{
			baseURL: h.server.URL + prefix,
			path:    s.DefaultPath,
		}); err != nil {
			t.Fatal(err)
		}
		want := prefix + s.DefaultPath
		if h.got.path != want {
			t.Errorf("path = %q，期望前缀保留后追加端点 %q", h.got.path, want)
		}
	})

	runHeaderClosure(t, s)
	runErrorDecoding(t, s)
}
```

- [ ] **Step 2: 确认编译通过**

Run: `go build ./internal/provider/providertest/`
Expected: FAIL，`undefined: runHeaderClosure`、`undefined: runErrorDecoding`（Task 5、6 补上）

- [ ] **Step 3: 暂不提交，继续 Task 5**

---

### Task 5: 严格闭集头断言

**Files:**
- Modify: `internal/provider/providertest/http_contract.go`（追加）

- [ ] **Step 1: 写实现**

```go
// autoHeaders 是 net/http 自动附加、与适配器无关的头。
//
// 断言它们等于断言标准库的行为，不是我们的契约。适配器自造的头
//（Content-Type / Accept / Authorization / StreamHeaders）不在此列，
// 它们由前面几条不变量正面断言。
var autoHeaders = map[string]bool{
	"Host":            true, // net/http 从 URL 推导
	"User-Agent":      true, // net/http 默认填充
	"Content-Length":  true, // net/http 按 body 长度计算
	"Accept-Encoding": true, // Transport 自动协商
}

// gatewayOwnHeaders 是适配器自己设的头，允许出现在上游请求里。
var gatewayOwnHeaders = map[string]bool{
	"Content-Type":  true,
	"Accept":        true,
	"Authorization": true,
}

// probeHeaders 是套件注入的、必定不该被转发的客户端头。
//
// 除了明显的伪造凭据与自定义头，还刻意放进**其他 Subject 的转发白名单**里
// 出现过的头：那样才能抓到「A 适配器把 B 适配器的白名单也一起转了」这种错。
var probeHeaders = []string{
	"X-Client-Custom",
	"X-Forwarded-For",
	"X-DashScope-WorkSpace",
	"X-DashScope-DataInspection",
	"X-DashScope-Async",
	"X-DashScope-SSE",
}

// runHeaderClosure 用严格闭集断言头转发。
//
// 上游收到的头减去标准库自动头之后，必须恰好等于网关自造头 ∪ ForwardedHeaders。
// 不用「注入几个头再断言它们没出现」的抽样式黑名单：抽样只能证明想到的那几个
// 没漏，证明不了没想到的那些。新适配器悄悄多转一个客户端头，抽样抓不到，闭集能。
func runHeaderClosure(t *testing.T, s Subject) {
	t.Run("头转发闭集", func(t *testing.T) {
		h := okServer(t)

		allowed := map[string]bool{}
		for k := range gatewayOwnHeaders {
			allowed[http.CanonicalHeaderKey(k)] = true
		}
		for _, name := range s.ForwardedHeaders {
			allowed[http.CanonicalHeaderKey(name)] = true
		}
		for name := range s.StreamHeaders {
			allowed[http.CanonicalHeaderKey(name)] = true
		}

		// 注入全部探针头。声明为转发的那些，期望原样到达；其余一个都不该出现。
		clientHeader := http.Header{}
		clientHeader.Set("Authorization", "Bearer "+clientSecret)
		for _, name := range probeHeaders {
			clientHeader.Set(name, "probe-"+name)
		}

		if _, err := h.call(t, s, callOpts{header: clientHeader}); err != nil {
			t.Fatal(err)
		}

		for name := range h.got.header {
			canonical := http.CanonicalHeaderKey(name)
			if autoHeaders[canonical] {
				continue
			}
			if !allowed[canonical] {
				t.Errorf("上游收到了未声明的头 %s: %q（闭集之外）",
					canonical, h.got.header.Get(name))
			}
		}

		// 声明为转发的头必须真的到了——闭集只能证明「没多」，这里证明「没少」。
		for _, name := range s.ForwardedHeaders {
			want := "probe-" + name
			if got := h.got.header.Get(name); got != want {
				t.Errorf("声明转发的头 %s = %q，期望 %q", name, got, want)
			}
		}
	})
}
```

- [ ] **Step 2: 确认编译**

Run: `go build ./internal/provider/providertest/`
Expected: FAIL，仍缺 `runErrorDecoding`

- [ ] **Step 3: 暂不提交，继续 Task 6**

---

### Task 6: 错误解码断言（不变量 10）

**Files:**
- Modify: `internal/provider/providertest/http_contract.go`（追加）

- [ ] **Step 1: 写实现**

```go
// runErrorDecoding 断言非 2xx 被解成统一错误。
func runErrorDecoding(t *testing.T, s Subject) {
	// Retryable 为真的语义是「换一个凭据或 Provider 可能成功」，
	// 不是「上游临时故障」——这两者常被混为一谈。
	t.Run("限流错误解码", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, s.RateLimitEnvelope)
		})

		_, err := h.call(t, s, callOpts{})
		if err == nil {
			t.Fatal("上游 429 应当返回错误")
		}
		var cerr *canonical.Error
		if !errors.As(err, &cerr) {
			t.Fatalf("应返回 *canonical.Error，实际 %T", err)
		}
		if cerr.Class != canonical.ClassRateLimit {
			t.Errorf("分类 = %q，期望 rate_limit", cerr.Class)
		}
		if !cerr.Retryable {
			t.Error("限流应可重试（换一份凭据可能成功）")
		}
		// Retry-After 必须活着传到凭据池——它决定冷却多久。
		if cerr.RetryAfter != 7*time.Second {
			t.Errorf("RetryAfter = %v，期望 7s", cerr.RetryAfter)
		}
		if cerr.UpstreamStatus != http.StatusTooManyRequests {
			t.Errorf("UpstreamStatus = %d，期望 429", cerr.UpstreamStatus)
		}
	})

	// 一个故障上游可能在 500 里塞进一整个 HTML 页面，甚至更糟。
	// 适配器必须能读完就走、不被拖死，且**分类不因体不可解析而退化**——
	// 状态码本身就是可靠信号。
	//
	// 这里刻意不断言 len(Message) 的上限。实测过：不可解析的体走的是
	// 「解析失败 → 回退 http.StatusText(status)」这条路，Message 恒为 21 字节
	// （"Internal Server Error"），无论读取上限是 64 KiB、1 MiB 还是根本没有
	// 上限，长度断言都通过——它抓不到任何东西。读取上限是 wire 层的实现细节，
	// 该由 openaiwire / dashscopewire 自己的测试守住；适配器这一层真正的契约
	// 是「垃圾进来，分类不许乱」。
	t.Run("不可解析的超大体不让分类退化", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, strings.Repeat("x", 1<<20))
		})

		_, err := h.call(t, s, callOpts{})
		if err == nil {
			t.Fatal("上游 500 应当返回错误")
		}
		var cerr *canonical.Error
		if !errors.As(err, &cerr) {
			t.Fatalf("应返回 *canonical.Error，实际 %T", err)
		}
		if cerr.Class != canonical.ClassUpstreamUnavailable {
			t.Errorf("分类 = %q，期望 upstream_unavailable", cerr.Class)
		}
		if cerr.UpstreamStatus != http.StatusInternalServerError {
			t.Errorf("UpstreamStatus = %d，期望 500", cerr.UpstreamStatus)
		}
	})
}
```

import 块需含 `"errors"`、`"io"`、`"strings"`、`"time"`、`"github.com/yobo2u/omugw/internal/canonical"`。

- [ ] **Step 2: 确认编译与 vet**

Run: `go build ./internal/provider/providertest/ && go vet ./internal/provider/providertest/`
Expected: 无输出（成功）

- [ ] **Step 3: 提交**

```bash
GIT_MASTER=1 git add internal/provider/providertest/http_contract.go
GIT_MASTER=1 git commit -m "providertest：HTTP 传输族六条不变量与闭集头断言"
```

---

### Task 7: Run 入口

**Files:**
- Modify: `internal/provider/providertest/providertest.go`（追加）

- [ ] **Step 1: 写失败测试**

追加到 `internal/provider/providertest/providertest_test.go`：

```go
// TestRunFailsOnIncompleteSubject 保证必填项缺失时是硬失败，不是静默跳过。
func TestRunFailsOnIncompleteSubject(t *testing.T) {
	fake := &testing.T{}
	// 用一个子测试承接 Fatal，避免打断本测试。
	done := make(chan bool)
	go func() {
		defer func() { done <- true }()
		defer func() { _ = recover() }()
		Run(fake, Subject{Name: "incomplete"})
	}()
	<-done

	if !fake.Failed() {
		t.Error("零值 Subject 应当让 Run 失败")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/provider/providertest/ -run TestRunFails -v`
Expected: FAIL，`undefined: Run`

- [ ] **Step 3: 写实现**

```go
// Run 跑全部不变量族。
//
// 必填项缺失时立即失败并列出缺哪些，而不是跳过对应的族——跳过等于把「没跑」
// 伪装成「跑过了」，而消除那种隐形缺口正是这个套件存在的理由。
func Run(t *testing.T, s Subject) {
	t.Helper()

	if err := validate(s); err != nil {
		t.Fatal(err)
	}

	name := s.Name
	if name == "" {
		name = string(s.Kind)
	}

	t.Run(name, func(t *testing.T) {
		runTransportAgnostic(t, s)
		runHTTPContract(t, s)
	})
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/provider/providertest/ -v`
Expected: PASS，全部测试通过

- [ ] **Step 5: 提交**

```bash
GIT_MASTER=1 git add internal/provider/providertest/
GIT_MASTER=1 git commit -m "providertest：Run 入口串起两族不变量"
```

---

### Task 8: 三个 Subject 接入

**Files:**
- Create: `internal/provider/passthrough/contract_test.go`
- Create: `internal/provider/dashscopecompat/contract_test.go`

- [ ] **Step 1: 写 passthrough 的两个 Subject**

`internal/provider/passthrough/contract_test.go`：

```go
package passthrough

import (
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/provider/providertest"
)

// TestContractOpenAICompat 跑 openai.compat 侧的共享契约。
//
// passthrough 一个类型服务两个协议族，两族的契约差异是真实存在的
//（转发头白名单、流式信号位置、错误信封都不同），所以跑两次 Run，
// 而不是合成一个带变体的 Subject。
func TestContractOpenAICompat(t *testing.T) {
	providertest.Run(t, providertest.Subject{
		Name: "passthrough/openai.compat",
		Kind: degrade.ProviderOpenAICompat,
		New: func(t *testing.T, d providertest.Deps) provider.Provider {
			return New(degrade.ProviderOpenAICompat, "/v1/responses", d.HTTPClient, d.Now)
		},
		DefaultPath:       "/v1/responses",
		ValidBody:         `{"model":"logical","input":"hi"}`,
		RateLimitEnvelope: `{"error":{"type":"rate_limit_error","message":"slow down"}}`,
	})
}

// TestContractDashScopeNative 跑 dashscope.native 侧的共享契约。
//
// 这一侧有三个必须原样带走的租户/行为头：丢了 WorkSpace 请求会落到错误的
// 子租户，丢了 DataInspection / Async 会改变审查与异步语义。
func TestContractDashScopeNative(t *testing.T) {
	providertest.Run(t, providertest.Subject{
		Name: "passthrough/dashscope.native",
		Kind: degrade.ProviderDashScopeNative,
		New: func(t *testing.T, d providertest.Deps) provider.Provider {
			return New(degrade.ProviderDashScopeNative,
				dashscopenative.TextGenerationPath, d.HTTPClient, d.Now)
		},
		DefaultPath: dashscopenative.TextGenerationPath,
		ForwardedHeaders: []string{
			"X-DashScope-WorkSpace",
			"X-DashScope-DataInspection",
			"X-DashScope-Async",
		},
		StreamHeaders: map[string]string{
			dashscopenative.SSEHeader: "enable",
		},
		ValidBody:         `{"model":"logical","input":{"messages":[{"role":"user","content":"x"}]}}`,
		RateLimitEnvelope: `{"code":"Throttling.RateQuota","message":"slow down","request_id":"req-1"}`,
	})
}
```

- [ ] **Step 2: 运行确认（预期会暴露问题，先看结果）**

Run: `go test ./internal/provider/passthrough/ -run TestContract -v`
Expected: 可能 FAIL。注意两处：
- `X-DashScope-SSE` 同时出现在 `probeHeaders`（不该转发）与 `StreamHeaders`
  （网关自设）里。非流式时若上游收到了它，说明 passthrough 把客户端的这个头
  转发了——那是真实缺陷，记录下来留给 Task 9。
- 若失败原因是套件自身逻辑（如闭集把网关自设头误判为客户端头），修套件。

- [ ] **Step 3: 写 dashscopecompat 的 Subject**

`internal/provider/dashscopecompat/contract_test.go`：

```go
package dashscopecompat

import (
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/provider/providertest"
)

// TestContract 跑 dashscope.compatible 的共享契约。
//
// 这条路是 wire-compatible 而非同源：复用 Chat 线格式只说明不需要重编码，
// 语义仍是异构的。契约层面它与其他适配器没有区别——一个客户端头都不转发。
func TestContract(t *testing.T) {
	providertest.Run(t, providertest.Subject{
		Name: "dashscopecompat",
		Kind: degrade.ProviderDashScopeCompatible,
		New: func(t *testing.T, d providertest.Deps) provider.Provider {
			return New(d.HTTPClient, d.Now)
		},
		DefaultPath:       ChatCompletionsPath,
		ValidBody:         `{"model":"logical","messages":[{"role":"user","content":"hi"}]}`,
		RateLimitEnvelope: `{"error":{"type":"rate_limit_error","message":"slow down"}}`,
	})
}
```

- [ ] **Step 4: 运行全部契约测试**

Run: `go test ./internal/provider/... -run TestContract -v`
Expected: 三个 `Subject` 的结果。记录每一条失败：是套件 bug 还是适配器真实缺陷。

- [ ] **Step 5: 修掉套件自身的 bug（若有），直到失败只剩真实缺陷**

- [ ] **Step 6: 提交**

```bash
GIT_MASTER=1 git add internal/provider/passthrough/contract_test.go internal/provider/dashscopecompat/contract_test.go
GIT_MASTER=1 git commit -m "providertest：三个出站适配器接入共享契约"
```

---

### Task 9: 修复闭集暴露的真实缺陷

**前置：** 仅当 Task 8 暴露出适配器真实行为差异时执行。无差异则跳过，本提交不存在。

**Files:**
- Modify: `internal/provider/passthrough/passthrough.go`（视暴露的问题而定）

- [ ] **Step 1: 确认缺陷真实存在**

对每一条失败，先判断：这是契约该有的行为，还是套件断言过严？

已预见的一处：`X-DashScope-SSE` 在 `forwardedHeaders` 白名单里没有，但
`probeHeaders` 会注入它。若非流式请求下上游收到了 `X-DashScope-SSE: probe-...`，
说明客户端能通过这个头绕过网关直接操纵上游的流式行为——那是真实缺陷。

反之若上游没收到，说明 passthrough 本来就不转发它，套件通过。

- [ ] **Step 2: 最小修复**

修复只做最小改动，不顺手重构周边（BUGFIX RULE）。若确认是客户端头穿透，
在 `Call` 里显式剥离而非扩大白名单。

- [ ] **Step 3: 验证**

Run: `go test ./internal/provider/... -v`
Expected: 全绿

- [ ] **Step 4: 提交**

```bash
GIT_MASTER=1 git add internal/provider/passthrough/passthrough.go
GIT_MASTER=1 git commit -m "passthrough：修复契约套件暴露的头穿透"
```

---

### Task 10: 删除被覆盖的旧测试并搬运注释

**Files:**
- Modify: `internal/provider/passthrough/passthrough_test.go`
- Modify: `internal/provider/dashscopecompat/dashscopecompat_test.go`
- Modify: `internal/provider/providertest/*.go`（补注释）

**删除范围：只删纯重复的。** 判据是：旧测试的断言内容能否被 `Subject` 声明完整
表达。能，就是纯重复；不能——它带有套件看不见的**协议特有取值**——则保留。

套件断言的是「`ForwardedHeaders` 里声明的头都到了」，它不知道也不该知道那三个
DashScope 头**具体叫什么、丢了会怎样**。那份知识留在原包才有归属。

- [ ] **Step 1: 删除纯重复（18 个）**

下表每一行，打开旧测试与套件对应断言，确认**断的是同一件事**才划掉。
不能按名字相近就删。

| 旧测试 | 套件对应 |
|---|---|
| `passthrough.TestClientAuthorizationIsNotForwarded` | 传输无关族/凭据隔离 |
| `passthrough.TestAcceptHeaderMatchesStreaming` | HTTP 族/流式信号落位 |
| `passthrough.TestModelIsRewritten` | HTTP 族/模型改写 |
| `passthrough.TestPathIsAppendedToBaseURL` | HTTP 族/路径来源-留空退回默认 |
| `passthrough.TestBaseURLPathPrefixIsPreserved` | HTTP 族/base_url 前缀保留 |
| `passthrough.TestUpstreamErrorBecomesCanonical` | HTTP 族/限流错误解码 |
| `passthrough.TestMissingModelIsRejected` | 传输无关族/客户端错误归客户端-缺 model |
| `passthrough.TestNonObjectBodyIsRejected` | 传输无关族/客户端错误归客户端-JSON 数组 |
| `passthrough.TestRequestPathOverridesDefault` | HTTP 族/路径来源-请求路径优先 |
| `passthrough.TestDefaultPathUsedWhenRequestPathEmpty` | HTTP 族/路径来源-留空退回默认 |
| `dashscopecompat.TestKindIsDashScopeCompatible` | 传输无关族/身份 |
| `dashscopecompat.TestRequestShape` | HTTP 族/请求形状 + 流式信号 + 头闭集 |
| `dashscopecompat.TestDefaultPathWhenRequestPathEmpty` | HTTP 族/路径来源 |
| `dashscopecompat.TestBaseURLPathPrefixIsPreserved` | HTTP 族/base_url 前缀保留 |
| `dashscopecompat.TestUpstreamErrorDecoded` | HTTP 族/限流错误解码 |
| `dashscopecompat.TestMissingModelRejected` | 传输无关族/客户端错误归客户端 |
| `dashscopecompat.TestInvalidJSONRejected` | 传输无关族/客户端错误归客户端 |
| `dashscopecompat.TestMissingUpstreamModelIsInternal` | 传输无关族/装配错误归网关 |

- [ ] **Step 2: 保留带协议特有取值的（3 个）**

| 保留的旧测试 | 为什么套件替代不了 |
|---|---|
| `passthrough.TestDashScopeTenantHeadersForwarded` | 三个头的**具体名字**与「丢了会落到错误子租户」这条理由，是 DashScope 特有知识。套件只知道「白名单里的头要到」，不知道白名单该装哪三个。 |
| `passthrough.TestDashScopeStreamingSetsSSEHeader` | `X-DashScope-SSE: enable` 这个**具体取值**同理。套件读 `StreamHeaders` 声明，但声明写错了它照样通过。 |
| `passthrough.TestOversizedErrorBodyIsCapped` | 见下方说明 |

`TestOversizedErrorBodyIsCapped` 保留但**必须改**：实测证明它的
`len(cerr.Message) > 128<<10` 断言恒为假（不可解析的体让 `Message` 回退成 21 字节的
`"Internal Server Error"`），一直在空转。改成断言分类不退化，与套件那条对齐；
或者把它下沉到 `openaiwire` / `dashscopewire` 的测试里去真正验证读取上限。
两条路选一条，别留一个假装在测的测试。

这三个保留项与套件是**互补**而非重复：套件守形状，它们守取值。同一契约的两处
断言之所以危险，是因为两处断的是同一件事；这里不是。

- [ ] **Step 2: 搬运注释**

删除前，把旧测试上「防的是什么」的注释并入套件对应断言的注释。这些注释是本仓库
最有价值的部分——套件的注释密度应当不低于被它替代的那些测试。

具体至少搬这三条：
- `TestClientAuthorizationIsNotForwarded` 关于「泄露给没有理由知道它的第三方」
- `TestOversizedErrorBodyIsCapped` 关于「500 里塞一整个 HTML 页面」
- `TestDashScopeTenantHeadersForwarded` 关于「丢了会落到错误的子租户」

（Task 3-6 的实现里已预置这些注释，此步只需核对是否齐全、有无遗漏的独有理由。）

- [ ] **Step 3: 确认最终保留清单**

除 Step 2 的三个协议特有取值测试外，还保留这四个（单个适配器的独有语义）：
- `passthrough.TestUnknownFieldsSurvive`
- `passthrough.TestIdenticalModelIsByteExact`
- `dashscopecompat.TestOfficialBaseURLDoesNotRepeatVersion`
- dashscopecompat 的 web_search_options 映射测试

合计保留 7 个，删除 18 个。

删除后清理不再被引用的辅助函数（`serve`、`call`、`captured`、`okServer`、
`callInput`、`assertClass`、`upstreamFields`、`refTime`）——保留的测试仍在用的
就留下。`go vet` 会报出未使用的，按它的输出清理。

- [ ] **Step 4: 验证**

Run: `go test ./internal/provider/... -v && go vet ./internal/provider/...`
Expected: 全绿，无「declared and not used」

- [ ] **Step 5: 全量验证**

Run: `make check`
Expected: fmt-check + vet + test + matrix 全通过

Run: `make test-race`
Expected: PASS，无竞态

- [ ] **Step 6: 提交**

```bash
GIT_MASTER=1 git add internal/provider/
GIT_MASTER=1 git commit -m "provider：删除已被共享契约套件覆盖的重复测试"
```

---

### Task 11: 登记 provenance

**Files:**
- Modify: `docs/provenance.yaml`

- [ ] **Step 1: 读现有格式**

Run: `sed -n '80,95p' docs/provenance.yaml`
按现有条目的字段结构追加，不自创格式。

- [ ] **Step 2: 追加条目**

在 `references` 或同类列表下追加（字段名以文件现有结构为准）：

```yaml
  - name: litellm
    repository: BerriAI/litellm
    commit: e17988f4fe7891bd39335b7e2a4b405bbe7b5ddc
    modules: [provider-contract-test-suite]
    usage: concept-only
    note: >-
      共享 provider 契约测试基类的思路参考。概念借鉴，无源码复制：
      不变量清单来自本仓库现有两个适配器测试的交集，API 形状按本仓库
      Go 惯例独立设计。未读取、未参考 enterprise/ 目录。
```

- [ ] **Step 3: 验证**

Run: `make check`
Expected: 全通过（若有 provenance 校验会一并跑到）

- [ ] **Step 4: 提交**

```bash
GIT_MASTER=1 git add docs/provenance.yaml
GIT_MASTER=1 git commit -m "provenance：登记 providertest 的 LiteLLM 概念参考边界"
```

---

## 验收清单

- [ ] `make check` 通过
- [ ] `make test-race` 通过
- [ ] passthrough 两次 `Run` 与 dashscopecompat 一次 `Run` 全绿
- [ ] 无 skip、无红灯
- [ ] 删除的旧测试与套件覆盖清单一一对应
- [ ] `docs/provenance.yaml` 已登记
- [ ] 工作区中技能配置初始化的改动（`AGENTS.md` 的 `## Agent skills` 一节与
      `docs/agents/` 三份文档）**未**混入本次任何提交
