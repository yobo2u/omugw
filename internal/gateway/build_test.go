package gateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/gateway"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

func TestBuiltMux_DashScopeNativeFallback_WithUpstream(t *testing.T) {
	m, err := degrade.Phase1()
	if err != nil {
		t.Fatalf("加载 Phase1 矩阵失败: %v", err)
	}

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
			Connect: time.Second, FirstByte: 2 * time.Second, Total: 3 * time.Second, Idle: time.Second,
		},
		Limits: config.Limits{
			MaxRequestBytes: 1024 * 1024,
			MaxInlineBytes:  1024 * 1024,
		},
	}

	built, err := gateway.Build(cfg, m, metrics, log)
	if err != nil {
		t.Fatalf("构建失败: %v", err)
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
				t.Errorf("期望状态码 %d，实际 %d", tt.expectedStatus, rec.Code)
			}

			if tt.expectUpstream {
				if upstreamCalls != 1 {
					t.Errorf("期望 1 次上游调用，实际 %d", upstreamCalls)
				}
			} else {
				if upstreamCalls != 0 {
					t.Errorf("期望 0 次上游调用，实际 %d", upstreamCalls)
				}
			}

			if tt.expectedCode != "" {
				if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("期望 Content-Type application/json，实际 %q", ct)
				}

				var body struct {
					Code string `json:"code"`
					Msg  string `json:"message"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("解析响应体失败: %v", err)
				}
				if body.Code != tt.expectedCode {
					t.Errorf("期望 code %q，实际 %q", tt.expectedCode, body.Code)
				}

				if tt.expectedMsg != "" && body.Msg != tt.expectedMsg {
					t.Errorf("期望 message %q，实际 %q", tt.expectedMsg, body.Msg)
				}

			}
		})
	}

	// 验证指标
	metricsFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("收集指标失败: %v", err)
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
				if inbound == string(degrade.ProtoDashScopeNative) && outbound == "planned" {
					notImplementedCount += m.GetCounter().GetValue()
				} else {
					t.Errorf("意外的指标标签: inbound=%s, outbound=%s", inbound, outbound)
				}
			}
		}
	}
	if notImplementedCount != 3 {
		t.Errorf("期望 3 次未实现指标，实际 %v", notImplementedCount)
	}

}

func TestBuiltMux_HealthOnlyMode_NoFallback(t *testing.T) {
	m := degrade.NewMatrix()
	reg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(reg)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 空配置 = 仅健康检查模式
	cfg := config.Config{}

	built, err := gateway.Build(cfg, m, metrics, log)
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/v1/services/aigc/multimodal-generation/generation", nil)
	rec := httptest.NewRecorder()

	built.Mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("仅健康检查模式期望 404，实际 %d", rec.Code)
	}
}
