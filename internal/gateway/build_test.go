package gateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/gateway"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

func TestBuiltMux_DashScopeNativeFallback_WithUpstream(t *testing.T) {
	m, _ := degrade.Phase1()

	reg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(reg)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":{"text":"ok"}}`))
	}))
	defer upstream.Close()

	cfg := config.Config{
		Auth: config.Auth{Keys: []config.AuthKey{{ID: "test", Key: "sk-test-1234567890"}}},
		Credentials: map[string][]config.CredentialSpec{
			"pool1": {{ID: "1", Secret: "sec1"}},
		},
		Providers: []config.ProviderSpec{
			{Endpoint: "ep1", Kind: "dashscope.native", BaseURL: upstream.URL, CredentialPool: "pool1"},
		},
		Models: []config.ModelSpec{
			{Match: "*", Targets: []config.TargetSpec{{Endpoint: "ep1", UpstreamModel: "test-model"}}},
		},
		Timeouts: config.Timeouts{
			Connect: 1000000000, FirstByte: 2000000000, Total: 3000000000, Idle: 1000000000,
		},
		Limits: config.Limits{
			MaxRequestBytes: 1024 * 1024,
			MaxInlineBytes:  1024 * 1024,
		},
	}

	built, err := gateway.Build(cfg, m, metrics, log)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	tests := []struct {
		name           string
		method         string
		path           string
		expectedStatus int
		expectedCode   string
		expectedMsg    string
		expectUpstream bool
	}{
		{
			name:           "未投放的多模态端点返回 501",
			method:         "POST",
			path:           "/api/v1/services/aigc/multimodal-generation/generation",
			expectedStatus: http.StatusNotImplemented,
			expectedCode:   "Unsupported",
			expectedMsg:    "DashScope Native 端点 /api/v1/services/aigc/multimodal-generation/generation 尚未实现",
		},
		{
			name:           "未投放的 embedding 端点返回 501",
			method:         "POST",
			path:           "/api/v1/services/embeddings/text-embedding/text-embedding",
			expectedStatus: http.StatusNotImplemented,
			expectedCode:   "Unsupported",
			expectedMsg:    "DashScope Native 端点 /api/v1/services/embeddings/text-embedding/text-embedding 尚未实现",
		},
		{
			name:           "未投放的 rerank 端点返回 501",
			method:         "POST",
			path:           "/api/v1/services/rerank/text-rerank/text-rerank",
			expectedStatus: http.StatusNotImplemented,
			expectedCode:   "Unsupported",
			expectedMsg:    "DashScope Native 端点 /api/v1/services/rerank/text-rerank/text-rerank 尚未实现",
		},
		{
			name:           "已投放的文本端点由精确路由处理，打到上游",
			method:         "POST",
			path:           dashscopenative.TextGenerationPath,
			expectedStatus: http.StatusOK,
			expectedCode:   "",
			expectUpstream: true,
		},
		{
			name:           "Native 命名空间下的非 POST 请求返回 405",
			method:         "GET",
			path:           "/api/v1/services/aigc/multimodal-generation/generation",
			expectedStatus: http.StatusMethodNotAllowed,
			expectedCode:   "",
		},
		{
			name:           "非 Native 命名空间返回 404",
			method:         "POST",
			path:           "/api/v2/something",
			expectedStatus: http.StatusNotFound,
			expectedCode:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreamCalls = 0
			req := httptest.NewRequest(tt.method, tt.path, bytes.NewReader([]byte(`{"model":"test-model","input":{"messages":[{"role":"user","content":"hello"}]}}`)))
			req.Header.Set("Authorization", "Bearer sk-test-1234567890")
			rec := httptest.NewRecorder()

			built.Mux.ServeHTTP(rec, req)

			if rec.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, rec.Code)
			}

			if tt.expectUpstream {
				if upstreamCalls != 1 {
					t.Errorf("expected 1 upstream call, got %d", upstreamCalls)
				}
			} else {
				if upstreamCalls != 0 {
					t.Errorf("expected 0 upstream calls, got %d", upstreamCalls)
				}
			}

			if tt.expectedCode != "" {
				if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("expected Content-Type application/json, got %q", ct)
				}

				var body struct {
					Code string `json:"code"`
					Msg  string `json:"message"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("failed to parse response body: %v", err)
				}
				if body.Code != tt.expectedCode {
					t.Errorf("expected code %q, got %q", tt.expectedCode, body.Code)
				}

				if tt.expectedMsg != "" && body.Msg != tt.expectedMsg {
					t.Errorf("expected message %q, got %q", tt.expectedMsg, body.Msg)
				}

			}
		})
	}

	// 验证指标
	metricsFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather failed: %v", err)
	}
	var notImplementedCount float64
	for _, mf := range metricsFamilies {
		if mf.GetName() == "omugw_not_implemented_total" {
			for _, m := range mf.GetMetric() {
				var inbound, outbound string
				for _, lp := range m.GetLabel() {
					if lp.GetName() == "inbound" {
						inbound = lp.GetValue()
					}
					if lp.GetName() == "outbound" {
						outbound = lp.GetValue()
					}
				}
				if inbound == string(degrade.ProviderDashScopeNative) && outbound == "planned" {
					notImplementedCount += m.GetCounter().GetValue()
				} else {
					t.Errorf("unexpected metric label: inbound=%s, outbound=%s", inbound, outbound)
				}
			}
		}
	}
	if notImplementedCount != 3 {
		t.Errorf("expected 3 not implemented metrics, got %v", notImplementedCount)
	}

}

func TestBuiltMux_HealthOnlyMode_NoFallback(t *testing.T) {
	m := degrade.NewMatrix()
	reg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(reg)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Empty config = health only
	cfg := config.Config{}

	built, err := gateway.Build(cfg, m, metrics, log)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/v1/services/aigc/multimodal-generation/generation", nil)
	rec := httptest.NewRecorder()

	built.Mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 in health-only mode, got %d", rec.Code)
	}
}
