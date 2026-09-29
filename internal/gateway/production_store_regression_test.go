package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
)

// 必须经过 Build：直接注入 Deps 的 harness 看不到 typed-nil 装配导致的崩溃。
func TestBuildResponsesStoreModes(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "false", "true"} {
			name := "disabled/" + mode
			if enabled {
				name = "enabled/" + mode
			}
			t.Run(name, func(t *testing.T) {
				up := jsonUpstream(t, `{"id":"upstream","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
				cfg := buildTestConfig(up.srv.URL)
				cfg.ConvStore = config.DefaultConvStore()
				cfg.ConvStore.Enabled = enabled
				matrix, err := degrade.Phase1()
				if err != nil {
					t.Fatal(err)
				}
				reg := prometheus.NewRegistry()
				built, err := Build(cfg, matrix, obs.NewMetrics(reg), slog.New(slog.NewTextHandler(io.Discard, nil)))
				if err != nil {
					t.Fatal(err)
				}
				field := ""
				if mode != "omitted" {
					field = `,"store":` + mode
				}
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"`+field+`}`))
				req.Header.Set("Authorization", "Bearer sk-test-1234567890")
				rec := httptest.NewRecorder()
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("生产 Build 请求 panic: %v", p)
					}
				}()
				built.Mux.ServeHTTP(rec, req)
				if !enabled && mode == "true" {
					if rec.Code != http.StatusUnprocessableEntity || up.calls.Load() != 0 {
						t.Fatalf("显式依赖未被拦截: status=%d calls=%d", rec.Code, up.calls.Load())
					}
					return
				}
				if rec.Code != http.StatusOK || up.calls.Load() != 1 {
					t.Fatalf("普通请求失败: status=%d calls=%d body=%s", rec.Code, up.calls.Load(), rec.Body.String())
				}
				var response struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				stored := enabled && mode != "false"
				if stored {
					if response.ID == "" || response.ID == "upstream" || built.ConversationStore.Len() != 1 {
						t.Fatalf("未保存本地会话: id=%q", response.ID)
					}
					if rec.Header().Get(EmulationHeader) != "stateful_conversation" {
						t.Errorf("本地会话缺模拟告知: %v", rec.Header())
					}
				} else if response.ID != "upstream" || rec.Header().Get(EmulationHeader) != "" {
					t.Errorf("未模拟时改写响应: id=%q headers=%v", response.ID, rec.Header())
				}
				families, err := reg.Gather()
				if err != nil {
					t.Fatal(err)
				}
				var emulated float64
				for _, family := range families {
					if family.GetName() == "omugw_emulations_total" {
						for _, metric := range family.GetMetric() {
							emulated += metric.GetCounter().GetValue()
						}
					}
				}
				want := 0.0
				if stored {
					want = 1
				}
				if emulated != want {
					t.Errorf("模拟计数=%v, 期望 %v", emulated, want)
				}
			})
		}
	}
}

func TestBuildConversationBudgetRejectsManySmallParts(t *testing.T) {
	up := jsonUpstream(t, `{"id":"upstream","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
	cfg := buildTestConfig(up.srv.URL)
	cfg.ConvStore = config.DefaultConvStore()
	cfg.ConvStore.Enabled = true
	cfg.ConvStore.MaxTotalBytes = 8192
	matrix, err := degrade.Phase1()
	if err != nil {
		t.Fatal(err)
	}
	built, err := Build(cfg, matrix, obs.NewMetrics(prometheus.NewRegistry()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.TrimSuffix(strings.Repeat(`{"type":"input_text","text":"a"},`, 128), ",")
	body := `{"model":"m","store":true,"input":[{"role":"user","content":[` + parts + `]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test-1234567890")
	rec := httptest.NewRecorder()
	built.Mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || built.ConversationStore.Len() != 0 {
		t.Fatalf("大量短块突破存储预算: status=%d len=%d body=%s", rec.Code, built.ConversationStore.Len(), rec.Body.String())
	}
	if up.calls.Load() != 1 {
		t.Fatalf("存储容量错误发生后不应重试生成: calls=%d", up.calls.Load())
	}
}
