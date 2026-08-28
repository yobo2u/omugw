package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

func TestFilterNativeTargets(t *testing.T) {
	t1 := router.Target{Kind: degrade.ProviderDashScopeNative, NativeEndpoint: "text-generation", Endpoint: "t1"}
	t2 := router.Target{Kind: degrade.ProviderDashScopeNative, NativeEndpoint: "multimodal-generation", Endpoint: "t2"}
	t3 := router.Target{Kind: degrade.ProviderDashScopeNative, NativeEndpoint: "", Endpoint: "t3"}
	t4 := router.Target{Kind: degrade.ProviderOpenAICompat, NativeEndpoint: "multimodal-generation", Endpoint: "t4"}

	tests := []struct {
		name     string
		targets  []router.Target
		caps     []canonical.Capability
		expected []router.Target
	}{
		{
			name:     "pure text retains all and exact order",
			targets:  []router.Target{t1, t2, t3, t4},
			caps:     []canonical.Capability{canonical.CapTextGeneration},
			expected: []router.Target{t1, t2, t3, t4},
		},
		{
			name:     "vision retains multimodal",
			targets:  []router.Target{t1, t2, t3, t4},
			caps:     []canonical.Capability{canonical.CapTextGeneration, canonical.CapVisionInput},
			expected: []router.Target{t2, t4},
		},
		{
			name:     "audio retains multimodal",
			targets:  []router.Target{t1, t2, t3, t4},
			caps:     []canonical.Capability{canonical.CapAudioInput},
			expected: []router.Target{t2, t4},
		},
		{
			name:     "empty targets",
			targets:  []router.Target{},
			caps:     []canonical.Capability{canonical.CapVisionInput},
			expected: nil,
		},
		{
			name:     "media capability ordering/duplicates do not alter behavior",
			targets:  []router.Target{t1, t2},
			caps:     []canonical.Capability{canonical.CapVisionInput, canonical.CapTextGeneration, canonical.CapVisionInput},
			expected: []router.Target{t2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := make([]router.Target, len(tt.targets))
			copy(input, tt.targets)

			got := filterNativeTargets(input, tt.caps)

			if !reflect.DeepEqual(input, tt.targets) {
				t.Errorf("input slice mutated: got %v, want %v", input, tt.targets)
			}

			if len(got) == 0 && len(tt.expected) == 0 {
				return
			}

			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("got %v, want %v", got, tt.expected)
			}
		})
	}
}

func testLocalMatrix(t *testing.T, caps ...canonical.Capability) *degrade.Matrix {
	m := degrade.NewMatrix()
	route := degrade.NewRoute(degrade.ProtoOpenAIChat, degrade.ProviderDashScopeNative).
		Pass(
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapVisionInput,
			canonical.CapAudioInput,
			canonical.CapReasoning,
		).
		Degrade("DashScope Native 的并行工具调用行为由上游模型决定，无显式开关可映射",
			canonical.CapParallelToolCalls).
		Degrade("DashScope Native 支持 response_format=json_object，无 strict schema 校验",
			canonical.CapStructuredOutput).
		Degrade("搜索开关", canonical.CapWebSearch).
		Reject("文件引用", canonical.CapFileInput).
		Reject("音频输出需要 Qwen-Omni 的输出格式参数，Chat Completions 入站无法表达",
			canonical.CapAudioOutput).
		Redeem(degrade.EndpointOpenAIChat, caps...)

	if err := m.Add(route.Build()); err != nil {
		t.Fatalf("build test matrix failed: %v", err)
	}
	return m
}

func newTestLocalHarness(t *testing.T, m *degrade.Matrix, targets []router.Target, ups ...*upstream) *harness {
	t.Helper()

	if len(ups) != len(targets) {
		t.Fatalf("newTestLocalHarness: 上游数量 (%d) 与目标数量 (%d) 不匹配", len(ups), len(targets))
	}

	timeouts := config.Timeouts{
		Connect:   200 * time.Millisecond,
		FirstByte: 1 * time.Second,
		Total:     10 * time.Second,
		Idle:      500 * time.Millisecond,
	}
	client := httpx.New(timeouts, nil)

	pools := map[string]*credential.Pool{}
	provs := map[string]provider.Provider{}

	for i, u := range ups {
		name := targets[i].Endpoint
		targets[i].BaseURL = u.srv.URL
		targets[i].CredentialPool = name

		pool, err := credential.NewPool(name,
			[]credential.Credential{{ID: "k1", Secret: "sk-" + name}},
			credential.DefaultPolicy(), nil)
		if err != nil {
			t.Fatal(err)
		}
		pools[name] = pool
		provs[name] = dashScopeNativeFactory(targets[i].Kind, client)
	}

	rt, err := router.New([]router.Rule{{Match: "*", Targets: targets}})
	if err != nil {
		t.Fatal(err)
	}

	metrics := obs.NewMetrics(prometheus.NewRegistry())
	return &harness{
		matrix:  m,
		metrics: metrics,
		path:    string(degrade.EndpointOpenAIChat),
		h: NewChatHandler(Deps{
			Matrix:    m,
			Router:    rt,
			Auth:      NewAuthenticator([]config.AuthKey{{ID: "tester", Key: "omugw-test-key-0123456789"}}),
			Limits:    config.Default().Limits,
			Metrics:   metrics,
			Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			Pools:     pools,
			Providers: provs,
			Now:       time.Now,
		}),
	}
}

func TestFilterIntegration(t *testing.T) {
	zeroDoorTests := []struct {
		name    string
		body    string
		targets []router.Target
		caps    []canonical.Capability
	}{
		{
			name: "image request with only text target returns 422",
			body: `{"model":"test","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/img.jpg"}}]}]}`,
			targets: []router.Target{
				{Kind: degrade.ProviderDashScopeNative, NativeEndpoint: "text-generation", Endpoint: "t1", UpstreamModel: "m1"},
			},
			caps: []canonical.Capability{canonical.CapTextGeneration, canonical.CapVisionInput},
		},
		{
			name: "audio request with only text target returns 422",
			body: `{"model":"test","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"UklGRgAAQABXQVZFZm10IBAAAAABAAEAQB8AAIA+AAACABAAZGF0YQAAAAA=","format":"wav"}}]}]}`,
			targets: []router.Target{
				{Kind: degrade.ProviderDashScopeNative, NativeEndpoint: "text-generation", Endpoint: "t1", UpstreamModel: "m1"},
			},
			caps: []canonical.Capability{canonical.CapTextGeneration, canonical.CapAudioInput},
		},
		{
			name: "media request with empty door returns 422",
			body: `{"model":"test","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/img.jpg"}}]}]}`,
			targets: []router.Target{
				{Kind: degrade.ProviderDashScopeNative, NativeEndpoint: "", Endpoint: "t1", UpstreamModel: "m1"},
			},
			caps: []canonical.Capability{canonical.CapTextGeneration, canonical.CapVisionInput},
		},
	}

	for _, tt := range zeroDoorTests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int
			var mu sync.Mutex
			up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"好"}}]},"request_id":"req-h"}`)
			})

			m := testLocalMatrix(t, tt.caps...)
			hs := newTestLocalHarness(t, m, tt.targets, up)

			req := httptest.NewRequest(http.MethodPost, hs.path, strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer omugw-test-key-0123456789")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			hs.h.ServeHTTP(rec, req)

			if rec.Code != 422 {
				t.Errorf("expected 422, got %d", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "模型路由没有可承载该媒体的 DashScope Native 门") {
				t.Errorf("unexpected body: %s", rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "unsupported") {
				t.Errorf("expected OpenAI error envelope with unsupported class, got: %s", rec.Body.String())
			}

			mu.Lock()
			if calls != 0 {
				t.Errorf("expected 0 calls, got %d", calls)
			}
			mu.Unlock()
		})
	}

	t.Run("media request with both targets calls multimodal", func(t *testing.T) {
		var calls1, calls2 int
		var mu sync.Mutex
		up1 := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls1++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"好"}}]},"request_id":"req-h"}`)
		})
		up2 := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls2++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"好"}}]},"request_id":"req-h"}`)
		})

		m := testLocalMatrix(t, canonical.CapTextGeneration, canonical.CapVisionInput)
		targets := []router.Target{
			{Kind: degrade.ProviderDashScopeNative, NativeEndpoint: "text-generation", Endpoint: "t1", UpstreamModel: "m1"},
			{Kind: degrade.ProviderDashScopeNative, NativeEndpoint: "multimodal-generation", Endpoint: "t2", UpstreamModel: "m2"},
		}
		hs := newTestLocalHarness(t, m, targets, up1, up2)

		body := `{"model":"test","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/img.jpg"}}]}]}`
		req := httptest.NewRequest(http.MethodPost, hs.path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer omugw-test-key-0123456789")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		hs.h.ServeHTTP(rec, req)

		if rec.Code != 200 {
			t.Errorf("expected 200, got %d", rec.Code)
		}

		mu.Lock()
		if calls1 != 0 {
			t.Errorf("expected 0 calls to text target, got %d", calls1)
		}
		if calls2 != 1 {
			t.Errorf("expected 1 call to multimodal target, got %d", calls2)
		}
		mu.Unlock()
	})

	t.Run("pure text request preserves order", func(t *testing.T) {
		var calls1, calls2 int
		var mu sync.Mutex
		up1 := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls1++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"好"}}]},"request_id":"req-h"}`)
		})
		up2 := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls2++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"好"}}]},"request_id":"req-h"}`)
		})

		m := testLocalMatrix(t, canonical.CapTextGeneration, canonical.CapVisionInput)
		targets := []router.Target{
			{Kind: degrade.ProviderDashScopeNative, NativeEndpoint: "text-generation", Endpoint: "t1", UpstreamModel: "m1"},
			{Kind: degrade.ProviderDashScopeNative, NativeEndpoint: "multimodal-generation", Endpoint: "t2", UpstreamModel: "m2"},
		}
		hs := newTestLocalHarness(t, m, targets, up1, up2)

		body := `{"model":"test","messages":[{"role":"user","content":"hello"}]}`
		req := httptest.NewRequest(http.MethodPost, hs.path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer omugw-test-key-0123456789")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		hs.h.ServeHTTP(rec, req)

		if rec.Code != 200 {
			t.Errorf("expected 200, got %d", rec.Code)
		}

		mu.Lock()
		if calls1 != 1 {
			t.Errorf("expected 1 call to text target, got %d", calls1)
		}
		if calls2 != 0 {
			t.Errorf("expected 0 calls to multimodal target, got %d", calls2)
		}
		mu.Unlock()
	})
}
