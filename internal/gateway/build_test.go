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

func TestBuiltMux_DashScopeNativeFallback(t *testing.T) {
	m := degrade.NewMatrix()
	reg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(reg)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := config.Config{
		Auth: config.Auth{Keys: []config.AuthKey{{ID: "test", Key: "sk-test-1234567890"}}},
		Credentials: map[string][]config.CredentialSpec{
			"pool1": {{ID: "1", Secret: "sec1"}},
		},
		Providers: []config.ProviderSpec{
			{Endpoint: "ep1", Kind: "dashscope.native", BaseURL: "http://upstream", CredentialPool: "pool1"},
		},
		Models: []config.ModelSpec{
			{Match: "*", Targets: []config.TargetSpec{{Endpoint: "ep1", UpstreamModel: "test-model"}}},
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
	}{
		{
			name:           "Unshipped multimodal endpoint returns 501",
			method:         "POST",
			path:           "/api/v1/services/aigc/multimodal-generation/generation",
			expectedStatus: http.StatusNotImplemented,
			expectedCode:   "Unsupported",
		},
		{
			name:           "Unshipped embedding endpoint returns 501",
			method:         "POST",
			path:           "/api/v1/services/embeddings/text-embedding/text-embedding",
			expectedStatus: http.StatusNotImplemented,
			expectedCode:   "Unsupported",
		},
		{
			name:           "Unshipped rerank endpoint returns 501",
			method:         "POST",
			path:           "/api/v1/services/rerank/text-rerank/text-rerank",
			expectedStatus: http.StatusNotImplemented,
			expectedCode:   "Unsupported",
		},
		{
			name:           "Shipped text endpoint returns 401 (handled by exact route, not fallback)",
			method:         "POST",
			path:           dashscopenative.TextGenerationPath,
			expectedStatus: http.StatusUnauthorized, // Because we don't provide auth header in this test
			expectedCode:   "",
		},
		{
			name:           "Non-POST on Native namespace returns 405",
			method:         "GET",
			path:           "/api/v1/services/aigc/multimodal-generation/generation",
			expectedStatus: http.StatusMethodNotAllowed,
			expectedCode:   "",
		},
		{
			name:           "Non-Native namespace returns 404",
			method:         "POST",
			path:           "/api/v2/something",
			expectedStatus: http.StatusNotFound,
			expectedCode:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, bytes.NewReader([]byte(`{}`)))
			rec := httptest.NewRecorder()

			built.Mux.ServeHTTP(rec, req)

			if rec.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, rec.Code)
			}

			if tt.expectedCode != "" {
				if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("expected Content-Type application/json, got %q", ct)
				}

				var body struct {
					Code string `json:"code"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("failed to parse response body: %v", err)
				}
				if body.Code != tt.expectedCode {
					t.Errorf("expected code %q, got %q", tt.expectedCode, body.Code)
				}
			}
		})
	}
}

func TestBuiltMux_HealthOnlyMode(t *testing.T) {
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
