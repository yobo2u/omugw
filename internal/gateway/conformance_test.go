package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/testkit"
)

// 路径级 fixture 的目录。文件名即用例名；以能力名命名的那些是 ADR-0001 要求的
// 有损格子举证。
const routeFixtures = "../../testdata/routes/openai.responses__openai.compat"

// chatRouteFixtures 是 Chat Completions 同源直通路径的 fixture 目录。
const chatRouteFixtures = "../../testdata/routes/openai.chat__openai.compat"

// dashScopeNativeRouteFixtures 是 DashScope Native 同源直通路径的 fixture 目录。
const dashScopeNativeRouteFixtures = "../../testdata/routes/dashscope.native__dashscope.native"

// TestRouteConformance 回放路径 fixture。
//
// 这条测试的存在是 ADR-0001 转正门槛的另一半：门槛只检查 fixture **存在**，
// 而存在却从不回放的 fixture 是纯粹的摆设——它能让一条根本跑不通的路径通过
// 转正检查。所以每个 fixture 都要真的走一遍完整链路，输出与 golden 比对。
//
// fixture 的语义：request 是**客户端发给网关**的内容，response 是被打桩的
// 上游返回。同源直通下两者形状接近，但不是同一件事——网关会改写鉴权与模型名。
func TestRouteConformance(t *testing.T) {
	for _, f := range testkit.LoadDir(t, routeFixtures) {
		t.Run(caseName(f.Name), func(t *testing.T) {
			up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				writeFixtureResponse(t, w, f)
			})
			hs := newHarness(t, true, up)

			body, err := json.Marshal(f.Request.Body)
			if err != nil {
				t.Fatal(err)
			}
			rec := hs.do(t, string(body), true)

			golden := filepath.Join(routeFixtures, "golden", caseName(f.Name)+".txt")
			testkit.Golden(t, golden, []byte(renderResult(rec)))
		})
	}
}

// TestChatRouteConformance 回放 Chat Completions 同源直通路径的 fixture。
//
// 与 TestRouteConformance 同理：转正门槛只查 fixture 存在，这里负责证明它真的
// 跑得通——解码、矩阵裁决、字节透传、用量抽取全链路走一遍，输出与 golden 比对。
func TestChatRouteConformance(t *testing.T) {
	for _, f := range testkit.LoadDir(t, chatRouteFixtures) {
		t.Run(caseName(f.Name), func(t *testing.T) {
			var gotPath string
			up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				writeFixtureResponse(t, w, f)
			})
			hs := newChatHarness(t, true, up)

			body, err := json.Marshal(f.Request.Body)
			if err != nil {
				t.Fatal(err)
			}
			rec := hs.do(t, string(body), true)

			// harness 的 provider 默认路径是 /v1/responses；只有 handler 把
			// Path 注入成 chat 端点，上游才会收到正确路径。这是 Path 注入的实证。
			if gotPath != "/v1/chat/completions" {
				t.Errorf("上游收到路径 %q，期望 /v1/chat/completions（Path 注入未生效）", gotPath)
			}

			golden := filepath.Join(chatRouteFixtures, "golden", caseName(f.Name)+".txt")
			testkit.Golden(t, golden, []byte(renderResult(rec)))
		})
	}
}

// TestDashScopeNativeRouteConformance 回放 DashScope Native 同源直通路径的 fixture。
//
// 这里的断言极其严格：上游收到的 method、path、headers 必须与 fixture 声明的完全一致
// （除了网关主动改写的鉴权和模型名）。这防止了「网关自作主张给上游塞了不该塞的头」
// 或「漏传了客户端指定的关键头（如 Workspace、SSE）」的回归。
func TestDashScopeNativeRouteConformance(t *testing.T) {
	for _, f := range testkit.LoadDir(t, dashScopeNativeRouteFixtures) {
		t.Run(caseName(f.Name), func(t *testing.T) {
			var (
				gotMethod  string
				gotPath    string
				gotHeader  http.Header
				gotBody    []byte
				gotBodyErr error
			)
			// 读 body 的错误只记录、不在这里终止：这个闭包跑在 httptest 服务器的
			// goroutine 上，t.Fatal 在非测试 goroutine 里只会结束该 goroutine，
			// 测试自己会带着半截数据继续跑下去。留到 ServeHTTP 之后在主 goroutine 判。
			up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotPath = r.URL.Path
				gotHeader = r.Header.Clone()
				gotBody, gotBodyErr = io.ReadAll(r.Body)
				writeFixtureResponse(t, w, f)
			})
			hs := newDashScopeNativeHarness(t, true, up)

			body, err := json.Marshal(f.Request.Body)
			if err != nil {
				t.Fatal(err)
			}

			method := f.Request.Method
			if method == "" {
				t.Fatal("fixture 缺少 method 声明，必须显式指定以保证契约严格性")
			}
			path := f.Request.Path
			if path == "" {
				t.Fatal("fixture 缺少 path 声明，必须显式指定以保证契约严格性")
			}

			req := httptest.NewRequest(method, path, bytes.NewReader(body))

			var expectedSSE string
			var expectedWS string
			for k, v := range f.Request.Headers {
				lk := strings.ToLower(k)
				if lk == "x-dashscope-sse" {
					expectedSSE = v
				}
				if lk == "x-dashscope-workspace" {
					expectedWS = v
				}
				if v == "<redacted>" {
					continue
				}
				req.Header.Set(k, v)
			}
			req.Header.Set("Authorization", "Bearer "+testKey)
			if req.Header.Get("Content-Type") == "" {
				req.Header.Set("Content-Type", "application/json")
			}

			rec := httptest.NewRecorder()
			hs.h.ServeHTTP(rec, req)

			if gotBodyErr != nil {
				t.Fatalf("读取上游收到的 body 失败: %v", gotBodyErr)
			}

			if gotMethod != method {
				t.Errorf("上游收到 method %q，期望 %q", gotMethod, method)
			}
			if gotPath != path {
				t.Errorf("上游收到路径 %q，期望 %q（Path 注入未生效或被篡改）", gotPath, path)
			}

			if auth := gotHeader.Get("Authorization"); auth != "Bearer sk-a" {
				t.Errorf("上游收到 Authorization %q，期望 Bearer sk-a", auth)
			}

			if sse := gotHeader.Get(dashscopenative.SSEHeader); sse != expectedSSE {
				t.Errorf("上游收到 SSE 头 %q，期望 %q（网关不应自作主张推断或篡改）", sse, expectedSSE)
			}

			if expectedWS != "" && expectedWS != "<redacted>" {
				if got := gotHeader.Get("X-DashScope-WorkSpace"); got != expectedWS {
					t.Errorf("上游收到 Workspace 头 %q，期望 %q", got, expectedWS)
				}
			}

			var gotJSON map[string]any
			if err := json.Unmarshal(gotBody, &gotJSON); err != nil {
				t.Fatalf("上游收到非 JSON body: %v", err)
			}
			var wantJSON map[string]any
			if err := json.Unmarshal(body, &wantJSON); err != nil {
				t.Fatalf("fixture body 非 JSON: %v", err)
			}

			if gotModel, _ := gotJSON["model"].(string); gotModel != "upstream-model" {
				t.Errorf("上游收到 model %q，期望 upstream-model", gotModel)
			}

			delete(gotJSON, "model")
			delete(wantJSON, "model")

			gotBytes, err := json.Marshal(gotJSON)
			if err != nil {
				t.Fatalf("序列化 gotJSON 失败: %v", err)
			}
			wantBytes, err := json.Marshal(wantJSON)
			if err != nil {
				t.Fatalf("序列化 wantJSON 失败: %v", err)
			}
			testkit.AssertJSONEqual(t, wantBytes, gotBytes, "上游收到的 body 语义不符")

			golden := filepath.Join(dashScopeNativeRouteFixtures, "golden", caseName(f.Name)+".txt")
			testkit.Golden(t, golden, []byte(renderResult(rec)))
		})
	}
}

// TestGatedEmulationIsRejectedByDefault 单独固化 EMULATE 那条 fixture 的语义。
//
// 服务端会话由网关侧 ConversationStore 提供，而它**默认关闭**——内存态会话在
// 多副本部署下是错的。因此带 previous_response_id 的请求在默认配置下会被矩阵
// 拦下，**根本不会打到上游**。这条测试断言的正是「没打到上游」。
func TestGatedEmulationIsRejectedByDefault(t *testing.T) {
	f := testkit.Load(t, filepath.Join(routeFixtures, "stateful_conversation.json"))

	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFixtureResponse(t, w, f)
	})
	hs := newHarness(t, true, up)

	body, _ := json.Marshal(f.Request.Body)
	rec := hs.do(t, string(body), true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d, 期望 422（开关未开启）: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——开关未开启时不该发出去", n)
	}
	// 错误必须说清是「开关没开」而不是「这条路不支持」，
	// 否则运维会去查一个根本没问题的转换路径。
	if !strings.Contains(rec.Body.String(), "convstore") {
		t.Errorf("错误应指明是哪个开关未开启: %s", rec.Body.String())
	}
}

// TestEmulationWorksWhenEnabled 覆盖开关打开后的行为。
func TestEmulationWorksWhenEnabled(t *testing.T) {
	f := testkit.Load(t, filepath.Join(routeFixtures, "stateful_conversation.json"))

	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFixtureResponse(t, w, f)
	})
	hs := newHarness(t, true, up)
	hs.matrix.WithAvailability(degrade.Availability{
		degrade.FeatureConversationStore: true,
	})

	body, _ := json.Marshal(f.Request.Body)
	rec := hs.do(t, string(body), true)

	if rec.Code != 200 {
		t.Fatalf("开关开启后应当正常转发，状态码 = %d: %s", rec.Code, rec.Body.String())
	}
	// 客户端有权知道这份完整性是网关垫出来的——它带着网关自己的可用性边界。
	if got := rec.Header().Get(EmulationHeader); !strings.Contains(got, "stateful_conversation") {
		t.Errorf("%s = %q, 应告知客户端该能力由网关模拟", EmulationHeader, got)
	}
}

// caseName 从 fixture 名里取出用例部分。
func caseName(full string) string {
	if _, after, ok := strings.Cut(full, "/"); ok {
		return after
	}
	return full
}

// writeFixtureResponse 把 fixture 里录制的上游响应写出去。
func writeFixtureResponse(t *testing.T, w http.ResponseWriter, f testkit.Fixture) {
	t.Helper()

	for k, v := range f.Response.Headers {
		w.Header().Set(k, v)
	}

	if f.Response.SSE == nil {
		w.WriteHeader(f.Response.Status)
		_, _ = w.Write(f.Response.Body)
		return
	}

	w.WriteHeader(f.Response.Status)
	flusher, _ := w.(http.Flusher)

	// 按 frames 复现上游的真实分片节奏——转换器的缓冲 bug 只在分片边界上
	// 才会暴露。
	events := f.Response.SSE.Events
	frames := f.Response.SSE.Frames
	if len(frames) == 0 {
		frames = make([]int, len(events))
		for i := range frames {
			frames[i] = 1
		}
	}

	var i int
	for _, n := range frames {
		var buf strings.Builder
		if err := testkit.WriteSSE(&buf, events[i:i+n]); err != nil {
			// 这个函数跑在回放服务器的 goroutine 上，t.Fatal 在那里只会结束
			// 该 goroutine，测试自己不会停。用 Errorf 记账并收工。
			t.Errorf("写出 SSE 帧失败: %v", err)
			return
		}
		i += n
		_, _ = w.Write([]byte(buf.String()))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// renderResult 把网关的响应渲染成便于 golden 比对的文本。
//
// 状态码与关键响应头也进 golden：一次 200 变 502、或者限流头悄悄消失，
// 都是必须被看见的回归。
func renderResult(rec *httptest.ResponseRecorder) string {
	var b strings.Builder
	b.WriteString("status: ")
	b.WriteString(http.StatusText(rec.Code))
	b.WriteString("\n")

	for _, k := range []string{
		"Content-Type",
		"X-Ratelimit-Remaining-Tokens",
		degrade.DegradationHeader,
		EmulationHeader,
	} {
		if v := rec.Header().Get(k); v != "" {
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteString("\n")
		}
	}

	b.WriteString("---\n")
	b.WriteString(rec.Body.String())
	return b.String()
}
