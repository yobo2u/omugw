package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/transport/sse"
)

// text 模式把 finish_reason 放在 output；不能误判中断再惩罚健康凭据。
func TestNativeTextStreamPreservesTerminalUsage(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "interrupted"
		finish := `"null"`
		if terminal {
			name, finish = "completed", `"stop"`
		}
		t.Run(name, func(t *testing.T) {
			up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: result\ndata: {\"output\":{\"text\":\"ok\",\"finish_reason\":"+finish+"},\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}\n\n")
			})
			hs := newDashScopeNativeHarness(t, true, up)
			reg := prometheus.NewRegistry()
			hs.h.deps.Metrics = obs.NewMetrics(reg)
			req := httptest.NewRequest(http.MethodPost, dashscopenative.TextGenerationPath,
				strings.NewReader(`{"model":"m","input":{"messages":[{"role":"user","content":"hi"}]},"parameters":{"result_format":"text"}}`))
			req.Header.Set("Authorization", "Bearer "+testKey)
			req.Header.Set(dashscopenative.SSEHeader, "enable")
			rec := httptest.NewRecorder()
			hs.h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || up.calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d body=%s", rec.Code, up.calls.Load(), rec.Body.String())
			}
			if got := strings.Contains(rec.Body.String(), "event: error"); got == terminal {
				t.Errorf("结束状态错误: terminal=%v body=%s", terminal, rec.Body.String())
			}
			wantTokens := 0.0
			if terminal {
				wantTokens = 3
			}
			families, err := reg.Gather()
			if err != nil {
				t.Fatal(err)
			}
			var inputTokens float64
			for _, family := range families {
				if family.GetName() != "omugw_tokens_total" {
					continue
				}
				for _, metric := range family.GetMetric() {
					var authoritative, input bool
					for _, label := range metric.GetLabel() {
						authoritative = authoritative || label.GetName() == "fidelity" && label.GetValue() == "authoritative"
						input = input || label.GetName() == "kind" && label.GetValue() == "input"
					}
					if authoritative && input {
						inputTokens += metric.GetCounter().GetValue()
					}
				}
			}
			if inputTokens != wantTokens {
				t.Errorf("权威输入用量=%v, 期望 %v", inputTokens, wantTokens)
			}
			for _, pool := range hs.h.deps.Pools {
				for _, stat := range pool.Stats() {
					if stat.Available != terminal {
						t.Errorf("凭据可用性=%v, 终态=%v", stat.Available, terminal)
					}
				}
			}
		})
	}
}

func TestNativeTerminalShapes(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"output":{"text":"ok","finish_reason":"stop"}}`, true},
		{`{"output":{"text":"ok","finish_reason":"length"}}`, true},
		{`{"output":{"text":"ok","finish_reason":null}}`, false},
		{`{"output":{"text":"ok","finish_reason":"null"}}`, false},
		{`{"output":{"text":"ok","finish_reason":""}}`, false},
		{`{"output":{"text":"ok"}}`, false},
		{`{"output":{"finish_reason":"stop","choices":[{"finish_reason":null}]}}`, false},
		{`{"output":{"choices":[{"finish_reason":"stop"},{"finish_reason":null}]}}`, false},
		{`{"output":{"choices":[{"finish_reason":"stop"},{"finish_reason":"stop"}]}}`, true},
	} {
		t.Run(tc.body, func(t *testing.T) {
			if got := dashScopeNativeInbound().streamTerminal(sse.Event{Event: "result", Data: tc.body}); got != tc.want {
				t.Fatalf("终态=%v, 期望 %v", got, tc.want)
			}
		})
	}
}

// 首事件前失败不得污染共享 Header，包括 SSE writer 自己附加的控制头。
func TestStreamRetryHeadersBelongToCommittedAttempt(t *testing.T) {
	for _, kind := range []string{"sse", "json", "error"} {
		t.Run(kind, func(t *testing.T) {
			bad := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-RateLimit-Remaining-Tokens", "0")
				w.Header().Set("Retry-After", "120")
			})
			good := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				switch kind {
				case "sse":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: response.completed\ndata: {\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
				case "json":
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"backup"}`)
				case "error":
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"bad request"}}`)
				}
			})
			hs := newHarness(t, true, bad, good)
			rec := hs.do(t, `{"model":"m","input":"hi","stream":true,"store":false}`, true)
			if bad.calls.Load() != 1 || good.calls.Load() != 1 {
				t.Fatalf("未完成一次 failover: bad=%d good=%d", bad.calls.Load(), good.calls.Load())
			}
			wantStatus := http.StatusOK
			if kind == "json" {
				// 客户端要求流式，备用却回 JSON，仍须失败且不带上一尝试的头。
				wantStatus = http.StatusBadGateway
			}
			if kind == "error" {
				wantStatus = http.StatusBadRequest
			}
			if rec.Code != wantStatus {
				t.Fatalf("status=%d, 期望 %d: %s", rec.Code, wantStatus, rec.Body.String())
			}
			for _, key := range []string{"Retry-After", "X-RateLimit-Remaining-Tokens"} {
				if value := rec.Header().Get(key); value != "" {
					t.Errorf("失败尝试残留 %s=%q", key, value)
				}
			}
			if kind != "sse" {
				for _, key := range []string{"Cache-Control", "Connection", "X-Accel-Buffering"} {
					if value := rec.Header().Get(key); value != "" {
						t.Errorf("非流式结果残留 %s=%q", key, value)
					}
				}
			}
		})
	}
}
