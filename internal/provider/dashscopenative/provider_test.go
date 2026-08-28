package dashscopenative

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

func newTestProvider(t *testing.T) *Provider {
	t.Helper()
	client := httpx.New(config.Default().Timeouts, nil)
	return New(client, nil)
}

// TestKind 钉死 Provider 的 Kind 声明，防止被意外篡改。
func TestKind(t *testing.T) {
	p := newTestProvider(t)
	if got := p.Kind(); got != degrade.ProviderDashScopeNative {
		t.Errorf("Kind() = %q, 期望 %q", got, degrade.ProviderDashScopeNative)
	}
}

// TestNativeInboundDelegatesToPassthrough 钉死 Native 入站复用既有 passthrough。
func TestNativeInboundDelegatesToPassthrough(t *testing.T) {
	var gotPath string
	expectedBody := `{"output":{},"usage":{"input_tokens":1,"output_tokens":1},"request_id":"r"}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, expectedBody)
	}))
	defer up.Close()

	p := newTestProvider(t)
	resp, err := p.Call(context.Background(), provider.Request{
		Target:     router.Target{Kind: degrade.ProviderDashScopeNative, Endpoint: "e", BaseURL: up.URL, UpstreamModel: "qwen-plus", CredentialPool: "p"},
		Credential: credential.Credential{ID: "k", Secret: "sk-test"},
		Raw:        []byte(`{"model":"qwen-plus","input":{"messages":[{"role":"user","content":"hi"}]}}`),
		Stream:     false,
		Inbound:    degrade.Inbound{Protocol: degrade.ProtoDashScopeNative, Endpoint: degrade.EndpointDashScopeTextGeneration},
	})
	if err != nil {
		t.Fatalf("Native 入站应直通: %v", err)
	}
	if resp == nil || resp.Body == nil {
		t.Fatal("期望返回非空的响应和 Body")
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应 Body 失败: %v", err)
	}
	if string(bodyBytes) != expectedBody {
		t.Errorf("响应 Body = %q, 期望 %q", string(bodyBytes), expectedBody)
	}

	if gotPath != string(degrade.EndpointDashScopeTextGeneration) {
		t.Errorf("上游路径应为入站门 %s，实际 %q", degrade.EndpointDashScopeTextGeneration, gotPath)
	}
}

// TestChatInboundDispatchesTranslator 钉死 Chat 入站已接通真实 translator。
//
// 分派与实现各自被测过还不够：Call 的 switch 少一条 case、或把流式请求送去
// 非流式 translator，两边的单测仍然全绿，而客户端拿到的是 501 或一个不流的流。
func TestChatInboundDispatchesTranslator(t *testing.T) {
	t.Run("非流式", func(t *testing.T) {
		var gotSSE string
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotSSE = r.Header.Get("X-DashScope-SSE")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"output":{"choices":[{"finish_reason":"stop",`+
				`"message":{"role":"assistant","content":"你好"}}]},"request_id":"req-ns"}`)
		}))
		defer up.Close()

		p, _ := newClockedProvider(t)
		resp, err := p.Call(context.Background(), chatRequest(t, up.URL))
		if err != nil {
			t.Fatalf("Chat 非流式入站应接通 translator: %v", err)
		}
		_, got := drainCompletion(t, resp)
		// object 只有真实的非流式 translator 才编得出来。
		if got.Object != "chat.completion" {
			t.Errorf("应产出 chat.completion，实际 %q", got.Object)
		}
		if got.ID != "req-ns" {
			t.Errorf("id 应取上游 request_id，实际 %q", got.ID)
		}
		if gotSSE != "" {
			t.Errorf("非流式不得声明 SSE，实际 %q", gotSSE)
		}
	})

	t.Run("流式", func(t *testing.T) {
		up := newNativeSSEUpstream(t, []string{
			`{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"你好"}}]},` +
				`"request_id":"req-st"}`,
		})
		defer up.Close()

		p, _ := newClockedProvider(t)
		resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, false))
		if err != nil {
			t.Fatalf("Chat 流式入站应接通 translator: %v", err)
		}
		out := readAllStream(t, resp)
		chunks, done := parseChatStream(t, out)
		if !done {
			t.Fatalf("流式应以 [DONE] 收尾: %s", out)
		}
		if len(chunks) != 1 {
			t.Fatalf("一个 Native 帧应产出一条 chunk，实际 %d 条: %s", len(chunks), out)
		}
		// object 与非流式不同，误把流式送去非流式 translator 会被这条挡住。
		if chunks[0].Object != "chat.completion.chunk" {
			t.Errorf("应产出 chat.completion.chunk，实际 %q", chunks[0].Object)
		}
		if got := up.request(); got.header.Get("X-DashScope-SSE") != "enable" {
			t.Errorf("流式必须声明 SSE，实际 %q", got.header.Get("X-DashScope-SSE"))
		}
	})
}

// TestChatInboundRejectsUnmappable 钉死无落点字段在出门前被拒。
//
// 分派若漏掉 rejectUnmappable，客户端显式提交的 frequency_penalty 会被静默丢掉，
// 请求照样 200——它以为那个参数生效了。
func TestChatInboundRejectsUnmappable(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"choices":[]},"request_id":"r"}`)
	}))
	defer up.Close()

	p, _ := newClockedProvider(t)
	req := chatRequestWithBody(t, up.URL,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"frequency_penalty":0.5}`)

	_, err := p.Call(context.Background(), req)
	if err == nil {
		t.Fatal("无落点字段必须被拒")
	}
	cerr := canonical.AsError(err)
	if cerr.Class != canonical.ClassUnsupported {
		t.Fatalf("应分类为 unsupported，实际 %v", cerr)
	}
	if cerr.Param != "frequency_penalty" {
		t.Errorf("应点名出错字段，实际 %q", cerr.Param)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("被拒的请求不得触达上游，实际出门 %d 次", got)
	}
}

// TestUnknownInboundFailClosed 钉死未知入站坐标 fail-closed 且不出门。
func TestUnknownInboundFailClosed(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
	}))
	defer up.Close()

	p := newTestProvider(t)
	_, err := p.Call(context.Background(), provider.Request{
		Target:     router.Target{Kind: degrade.ProviderDashScopeNative, Endpoint: "e", BaseURL: up.URL, UpstreamModel: "m", CredentialPool: "p"},
		Credential: credential.Credential{ID: "k", Secret: "sk-test"},
		Raw:        []byte(`{}`),
		Inbound:    degrade.Inbound{Protocol: degrade.ProtoOpenAIResponses, Endpoint: degrade.EndpointOpenAIResponses},
	})
	if err == nil {
		t.Fatal("未知入站必须报错")
	}
	if canonical.AsError(err).Class != canonical.ClassUnsupported {
		t.Fatalf("应 fail-closed 为 unsupported，实际 %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("未知入站不得触达上游，实际出门 %d 次", got)
	}
}
