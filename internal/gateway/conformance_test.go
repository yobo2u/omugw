package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/convstore"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/sse"
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
			// 入站坐标的门注入成 chat 端点，上游才会收到正确路径。
			// 这是 Inbound.Endpoint 注入的实证。
			if gotPath != "/v1/chat/completions" {
				t.Errorf("上游收到路径 %q，期望 /v1/chat/completions（入站坐标注入未生效）", gotPath)
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
				t.Errorf("上游收到路径 %q，期望 %q（入站坐标注入未生效或被篡改）", gotPath, path)
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
	var (
		mu       sync.Mutex
		requests [][]byte
	)
	responses := []string{
		`{"id":"upstream-1","object":"response","status":"completed","output":[{"type":"reasoning","id":"rs-1","encrypted_content":"opaque-reasoning","summary":[]},{"type":"message","id":"msg-1","role":"assistant","content":[{"type":"output_text","text":"红色"}]}]}`,
		`{"id":"upstream-2","object":"response","status":"completed","output":[{"type":"message","id":"msg-2","role":"assistant","content":[{"type":"output_text","text":"已记住"}]}]}`,
	}
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, append([]byte(nil), body...))
		response := responses[len(requests)-1]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	})
	hs := newHarness(t, true, up)
	hs.matrix.WithAvailability(degrade.Availability{
		degrade.FeatureConversationStore: true,
	})
	store := convstore.NewMemoryStore(convstore.DefaultLimits(), nil)
	hs.h.deps.ConversationStore = store

	first := hs.do(t, `{"model":"logical-fast","input":"我选红色","store":true}`, true)
	if first.Code != http.StatusOK {
		t.Fatalf("第一轮状态码 = %d: %s", first.Code, first.Body.String())
	}
	var firstResponse struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstResponse); err != nil {
		t.Fatal(err)
	}
	if firstResponse.ID == "" || firstResponse.ID == "upstream-1" {
		t.Fatalf("第一轮没有返回本地 response.id: %q", firstResponse.ID)
	}

	secondBody, _ := json.Marshal(map[string]any{
		"model": "logical-fast", "input": "我上一轮选了什么？",
		"previous_response_id": firstResponse.ID,
	})
	second := hs.do(t, string(secondBody), true)
	if second.Code != http.StatusOK {
		t.Fatalf("第二轮状态码 = %d: %s", second.Code, second.Body.String())
	}
	var secondResponse struct {
		PreviousResponseID string `json:"previous_response_id"`
		Store              bool   `json:"store"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondResponse); err != nil {
		t.Fatal(err)
	}
	if secondResponse.PreviousResponseID != firstResponse.ID || !secondResponse.Store {
		t.Errorf("第二轮响应没有反映本地会话关系: %s", second.Body.String())
	}
	if got := second.Header().Get(EmulationHeader); !strings.Contains(got, "stateful_conversation") {
		t.Errorf("%s = %q, 应告知客户端该能力由网关模拟", EmulationHeader, got)
	}

	mu.Lock()
	sent := append([]byte(nil), requests[1]...)
	mu.Unlock()
	var upstreamRequest struct {
		Input []struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"input"`
		PreviousResponseID string `json:"previous_response_id"`
		Store              *bool  `json:"store"`
	}
	if err := json.Unmarshal(sent, &upstreamRequest); err != nil {
		t.Fatal(err)
	}
	if len(upstreamRequest.Input) != 4 {
		t.Fatalf("第二轮上游 input 条数 = %d，期望历史用户、推理、历史助手、当前用户四条: %s",
			len(upstreamRequest.Input), sent)
	}
	if !strings.Contains(string(sent), `"encrypted_content":"opaque-reasoning"`) {
		t.Errorf("第二轮没有逐字回放未建模的推理条目: %s", sent)
	}
	if upstreamRequest.PreviousResponseID != "" || upstreamRequest.Store == nil || *upstreamRequest.Store {
		t.Errorf("本地模拟后仍把上游会话状态打开: %s", sent)
	}
	history, err := store.History(t.Context(), firstResponse.ID)
	if err != nil {
		t.Fatalf("本地 response.id 无法回查: %v", err)
	}
	if len(history) != 2 || history[0].TextContent() != "我选红色" || history[1].TextContent() != "红色" {
		t.Errorf("第一轮保存内容不符: %+v", history)
	}
}

func TestStreamingEmulationStoresCompletedTurn(t *testing.T) {
	var (
		mu       sync.Mutex
		requests [][]byte
	)
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, append([]byte(nil), body...))
		call := len(requests)
		mu.Unlock()
		if call == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			writer := sse.NewWriterTo(w)
			_ = writer.Write(sse.Event{Event: "response.created", Data: `{"type":"response.created","response":{"id":"upstream-stream","status":"in_progress","output":[]}}`})
			_ = writer.Write(sse.Event{Event: "response.output_text.delta", Data: `{"type":"response.output_text.delta","response_id":"upstream-stream","item_id":"msg-1","delta":"蓝色"}`})
			_ = writer.Write(sse.Event{Event: "response.completed", Data: `{"type":"response.completed","response":{"id":"upstream-stream","status":"completed","output":[{"type":"message","id":"msg-1","role":"assistant","content":[{"type":"output_text","text":"蓝色"}]}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"upstream-next","object":"response","status":"completed","output":[{"type":"message","id":"msg-2","role":"assistant","content":[{"type":"output_text","text":"已记住"}]}]}`)
	})
	hs := newHarness(t, true, up)
	hs.matrix.WithAvailability(degrade.Availability{degrade.FeatureConversationStore: true})
	store := convstore.NewMemoryStore(convstore.DefaultLimits(), nil)
	hs.h.deps.ConversationStore = store

	first := hs.do(t, `{"model":"logical-fast","input":"我选蓝色","stream":true,"store":true}`, true)
	if first.Code != http.StatusOK {
		t.Fatalf("流式第一轮状态码 = %d: %s", first.Code, first.Body.String())
	}
	if strings.Contains(first.Body.String(), "upstream-stream") {
		t.Fatalf("流式事件泄露了上游 response.id: %s", first.Body.String())
	}
	reader := sse.NewReader(strings.NewReader(first.Body.String()))
	created, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	var createdEnvelope struct {
		Response struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(created.Data), &createdEnvelope); err != nil {
		t.Fatal(err)
	}
	responseID := createdEnvelope.Response.ID
	if responseID == "" {
		t.Fatal("流式 response.created 缺少本地 ID")
	}

	secondBody, _ := json.Marshal(map[string]any{
		"model": "logical-fast", "input": "我选了什么？",
		"previous_response_id": responseID,
	})
	second := hs.do(t, string(secondBody), true)
	if second.Code != http.StatusOK {
		t.Fatalf("引用流式响应的第二轮状态码 = %d: %s", second.Code, second.Body.String())
	}
	mu.Lock()
	sent := append([]byte(nil), requests[1]...)
	mu.Unlock()
	var upstreamRequest struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(sent, &upstreamRequest); err != nil {
		t.Fatal(err)
	}
	if len(upstreamRequest.Input) != 3 {
		t.Fatalf("流式轮次未回放到下一请求: %s", sent)
	}
}

func TestConversationStoreHonorsDefaultAndExplicitFalse(t *testing.T) {
	up := jsonUpstream(t, `{"id":"upstream","object":"response","status":"completed",`+
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
	hs := newHarness(t, true, up)
	hs.matrix.WithAvailability(degrade.Availability{degrade.FeatureConversationStore: true})
	hs.h.deps.ConversationStore = convstore.NewMemoryStore(convstore.DefaultLimits(), nil)

	first := hs.do(t, `{"model":"logical-fast","input":"first"}`, true)
	var firstMeta struct {
		ID    string `json:"id"`
		Store *bool  `json:"store"`
	}
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &firstMeta) != nil ||
		firstMeta.ID == "" || firstMeta.Store == nil || !*firstMeta.Store {
		t.Fatalf("省略 store 没有按默认 true 保存: status=%d body=%s", first.Code, first.Body.String())
	}

	secondBody, _ := json.Marshal(map[string]any{
		"model": "logical-fast", "input": "second", "store": false,
		"previous_response_id": firstMeta.ID,
	})
	second := hs.do(t, string(secondBody), true)
	var secondMeta struct {
		ID    string `json:"id"`
		Store *bool  `json:"store"`
	}
	if second.Code != http.StatusOK || json.Unmarshal(second.Body.Bytes(), &secondMeta) != nil ||
		secondMeta.ID == "" || secondMeta.Store == nil || *secondMeta.Store {
		t.Fatalf("显式 store:false 未保留: status=%d body=%s", second.Code, second.Body.String())
	}

	thirdBody, _ := json.Marshal(map[string]any{
		"model": "logical-fast", "input": "third", "store": false,
		"previous_response_id": secondMeta.ID,
	})
	third := hs.do(t, string(thirdBody), true)
	if third.Code != http.StatusBadRequest {
		t.Fatalf("未保存的 response.id 仍可引用: status=%d body=%s", third.Code, third.Body.String())
	}
	if got := up.calls.Load(); got != 2 {
		t.Fatalf("无效会话引用触达上游 %d 次，期望总调用 2", got)
	}
}

func TestConversationStoreIsScopedToCaller(t *testing.T) {
	up := jsonUpstream(t, `{"id":"upstream","object":"response","status":"completed",`+
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"private"}]}]}`)
	hs := newHarness(t, true, up)
	hs.matrix.WithAvailability(degrade.Availability{degrade.FeatureConversationStore: true})
	hs.h.deps.ConversationStore = convstore.NewMemoryStore(convstore.DefaultLimits(), nil)
	hs.h.deps.Auth = NewAuthenticator([]config.AuthKey{
		{ID: "caller-a", Key: "omugw-caller-a-key-0123456789"},
		{ID: "caller-b", Key: "omugw-caller-b-key-0123456789"},
	})

	do := func(body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		hs.h.ServeHTTP(rec, req)
		return rec
	}
	first := do(`{"model":"logical-fast","input":"caller-a-private","store":true}`,
		"omugw-caller-a-key-0123456789")
	var meta struct {
		ID string `json:"id"`
	}
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &meta) != nil || meta.ID == "" {
		t.Fatalf("调用方 A 首轮失败: status=%d body=%s", first.Code, first.Body.String())
	}
	body, _ := json.Marshal(map[string]any{
		"model": "logical-fast", "input": "steal", "store": false,
		"previous_response_id": meta.ID,
	})
	second := do(string(body), "omugw-caller-b-key-0123456789")
	if second.Code != http.StatusBadRequest {
		t.Fatalf("调用方 B 引用了 A 的会话: status=%d body=%s", second.Code, second.Body.String())
	}
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("跨调用方引用触达上游，总调用=%d", got)
	}
}

func TestConversationLimitsRejectBeforeUpstream(t *testing.T) {
	tests := []struct {
		name   string
		limits convstore.Limits
		seed   convstore.Turn
		body   string
		setup  func(*harness)
	}{
		{
			name: "链深",
			limits: convstore.Limits{TTL: convstore.DefaultLimits().TTL,
				MaxChainDepth: 1, MaxMessages: 100},
			seed: convstore.Turn{ID: "resp_root", Owner: "tester", Model: "logical-fast",
				Messages: []canonical.Message{canonical.UserText("root")}},
			body: `{"model":"logical-fast","input":"next","store":true,"previous_response_id":"resp_root"}`,
		},
		{
			name: "消息数",
			limits: convstore.Limits{TTL: convstore.DefaultLimits().TTL,
				MaxChainDepth: 100, MaxMessages: 2},
			seed: convstore.Turn{ID: "resp_root", Owner: "tester", Model: "logical-fast",
				Messages: []canonical.Message{canonical.UserText("root")}},
			body: `{"model":"logical-fast","input":"next","store":true,"previous_response_id":"resp_root"}`,
		},
		{
			name:   "累计内联字节",
			limits: convstore.DefaultLimits(),
			seed: convstore.Turn{ID: "resp_root", Owner: "tester", Model: "logical-fast",
				Messages: []canonical.Message{canonical.UserText("root")}, InlineBytes: 3},
			body: `{"model":"logical-fast","store":false,"previous_response_id":"resp_root",` +
				`"input":[{"role":"user","content":[{"type":"input_image",` +
				`"image_url":"data:image/png;base64,QUJD"}]}]}`,
			setup: func(hs *harness) { hs.h.deps.Limits.MaxInlineBytes = 4 },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := jsonUpstream(t, `{}`)
			hs := newHarness(t, true, up)
			hs.matrix.WithAvailability(degrade.Availability{degrade.FeatureConversationStore: true})
			store := convstore.NewMemoryStore(tc.limits, nil)
			if err := store.AppendWithID(t.Context(), tc.seed); err != nil {
				t.Fatal(err)
			}
			hs.h.deps.ConversationStore = store
			if tc.setup != nil {
				tc.setup(hs)
			}
			rec := hs.do(t, tc.body, true)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码=%d，期望上游调用前拒绝: %s", rec.Code, rec.Body.String())
			}
			if got := up.calls.Load(); got != 0 {
				t.Fatalf("资源超限仍调用上游 %d 次", got)
			}
		})
	}
}

func TestGeneratedImageHistoryCountsTowardInlineLimit(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			response := `{"id":"upstream","object":"response","status":"completed",` +
				`"output":[{"type":"image_generation_call","id":"ig_1",` +
				`"status":"completed","result":"QUJD"}]}`
			up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Stream bool `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatal(err)
				}
				if req.Stream {
					w.Header().Set("Content-Type", "text/event-stream")
					writer := sse.NewWriterTo(w)
					_ = writer.Write(sse.Event{Event: "response.completed",
						Data: `{"type":"response.completed","response":` + response + `}`})
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, response)
			})
			hs := newHarness(t, true, up)
			hs.matrix.WithAvailability(degrade.Availability{degrade.FeatureConversationStore: true})
			hs.h.deps.ConversationStore = convstore.NewMemoryStore(convstore.DefaultLimits(), nil)
			hs.h.deps.Limits.MaxInlineBytes = 2

			first := hs.do(t, fmt.Sprintf(
				`{"model":"logical-fast","input":"draw","store":true,"stream":%v}`, stream), true)
			if first.Code != http.StatusOK {
				t.Fatalf("生成图片的首轮失败: status=%d body=%s", first.Code, first.Body.String())
			}
			var id string
			if stream {
				ev, err := sse.NewReader(strings.NewReader(first.Body.String())).Next()
				if err != nil {
					t.Fatal(err)
				}
				var envelope struct {
					Response struct {
						ID string `json:"id"`
					} `json:"response"`
				}
				if err := json.Unmarshal([]byte(ev.Data), &envelope); err != nil {
					t.Fatal(err)
				}
				id = envelope.Response.ID
			} else {
				var envelope struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(first.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				id = envelope.ID
			}
			secondBody, _ := json.Marshal(map[string]any{
				"model": "logical-fast", "input": "continue", "store": false,
				"previous_response_id": id,
			})
			second := hs.do(t, string(secondBody), true)
			if second.Code != http.StatusBadRequest {
				t.Fatalf("超限历史仍被回放: status=%d body=%s", second.Code, second.Body.String())
			}
			if got := up.calls.Load(); got != 1 {
				t.Fatalf("超限续轮触达上游，总调用=%d", got)
			}
		})
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
