package obs

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/canonical"
)

func TestObserveWSUsage(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.ObserveWSUsage("dashscope.realtime", "response", canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: 3, OutputTokens: 2, AudioInputTokens: 1, AudioOutputTokens: 2})
	m.ObserveWSUsage("dashscope.realtime", "response", canonical.Usage{Fidelity: canonical.FidelityAuthoritative})
	m.ObserveWSUsage("dashscope.realtime", "transcription", canonical.UnavailableUsage())
	m.ObserveWSUsage("dashscope.realtime", "session", canonical.UnavailableUsage())
	for _, tt := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"omugw_ws_usage_records_total", map[string]string{"source": "response", "unit": "tokens", "fidelity": "authoritative"}, 2},
		{"omugw_ws_usage_records_total", map[string]string{"source": "transcription", "fidelity": "unavailable"}, 1},
		{"omugw_ws_usage_records_total", map[string]string{"source": "session", "fidelity": "unavailable"}, 1},
		{"omugw_ws_tokens_total", map[string]string{"source": "response", "kind": "input"}, 3},
		{"omugw_ws_tokens_total", map[string]string{"source": "response", "kind": "output"}, 2},
		{"omugw_ws_tokens_total", map[string]string{"source": "response", "kind": "audio_input"}, 1},
		{"omugw_ws_tokens_total", map[string]string{"source": "response", "kind": "audio_output"}, 2},
	} {
		if got := wsMetricSum(t, reg, tt.name, tt.labels); got != tt.want {
			t.Errorf("%s %v = %v, want %v", tt.name, tt.labels, got, tt.want)
		}
	}
}

func TestObserveWSCharacters(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.ObserveWSCharacters("dashscope.realtime", "response", 25)
	m.ObserveWSCharacters("dashscope.realtime", "response", 0)
	m.ObserveWSCharacters("dashscope.realtime", "response", -1)
	if got := wsMetricSum(t, reg, "omugw_ws_characters_total", map[string]string{"fidelity": "authoritative"}); got != 25 {
		t.Fatal(got)
	}
	if got := wsMetricSum(t, reg, "omugw_ws_usage_records_total", map[string]string{"unit": "characters", "fidelity": "authoritative"}); got != 2 {
		t.Fatal(got)
	}
	if got := wsMetricSum(t, reg, "omugw_ws_usage_records_total", map[string]string{"unit": "tokens"}); got != 0 {
		t.Fatal("字符不能产生 token 记录", got)
	}
	if countSamples(t, reg, "omugw_ws_tokens_total") != 0 {
		t.Fatal("字符不能换算 token")
	}
}

func TestObserveWSBoundedLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.ObserveWSUsage("dashscope.realtime", "response", canonical.UnavailableUsage())
	m.ObserveWSDiagnostic("dashscope.realtime", "usage_conflict")
	for i := 0; i < 50; i++ {
		m.ObserveWSUsage("dynamic-model", "dynamic-id", canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: 3})
		m.ObserveWSUsage("dashscope.realtime", "dynamic-id", canonical.UnavailableUsage())
		m.ObserveWSUsage("dashscope.realtime", "response", canonical.Usage{Fidelity: "dynamic-fidelity"})
		m.ObserveWSCharacters("dashscope.realtime", "dynamic-id", 1)
		m.ObserveWSDiagnostic("dashscope.realtime", "secret-body")
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		for _, metric := range f.Metric {
			for _, label := range metric.Label {
				switch label.GetName() {
				case "protocol", "source", "unit", "fidelity", "kind", "reason":
				default:
					t.Errorf("意外标签: %s", label.GetName())
				}
				if label.GetValue() == "secret-body" || label.GetValue() == "dynamic-id" || label.GetValue() == "dynamic-model" || label.GetValue() == "dynamic-fidelity" {
					t.Errorf("动态标签: %v", label)
				}
			}
		}
	}
	if got := wsMetricSum(t, reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "usage_conflict"}); got != 1 {
		t.Fatal(got)
	}
}

func wsMetricSum(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, metric := range f.Metric {
			match := true
			for k, v := range labels {
				found := false
				for _, label := range metric.Label {
					found = found || label.GetName() == k && label.GetValue() == v
				}
				match = match && found
			}
			if match {
				total += metric.GetCounter().GetValue()
			}
		}
	}
	return total
}
