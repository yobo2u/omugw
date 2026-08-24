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
