package obs

import (
	"math"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestObserveWSSeconds(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	for _, n := range []float64{1.25, 0, -1, math.NaN(), math.Inf(1)} {
		m.ObserveWSSeconds("openai.realtime", "transcription", n)
	}
	m.ObserveWSSeconds("dynamic-model", "transcription", 1)
	m.ObserveWSSeconds("openai.realtime", "dynamic-id", 1)
	if got := wsMetricSum(t, reg, "omugw_ws_audio_input_seconds_total", nil); got != 1.25 {
		t.Fatal(got)
	}
	if got := wsMetricSum(t, reg, "omugw_ws_usage_records_total", map[string]string{"unit": "seconds", "fidelity": "authoritative"}); got != 2 {
		t.Fatal(got)
	}
	if got := countSamples(t, reg, "omugw_ws_tokens_total"); got != 0 {
		t.Fatal("秒制造 token", got)
	}
	if got := wsMetricSum(t, reg, "omugw_ws_usage_records_total", map[string]string{"unit": "tokens"}); got != 0 {
		t.Fatal(got)
	}
}

func TestObserveWSTokenDetails(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	d := WSTokenDetails{TextInput: WSTokenCount{Value: 2, Present: true}, ImageInput: WSTokenCount{Value: 0, Present: true}, CachedInput: WSTokenCount{Value: 1, Present: true}, CachedImageInput: WSTokenCount{Value: 0, Present: true}, AudioOutput: WSTokenCount{Value: -1, Present: true}}
	m.ObserveWSTokenDetails("openai.realtime", "response", d)
	m.ObserveWSTokenDetails("dynamic-model", "response", d)
	m.ObserveWSTokenDetails("openai.realtime", "dynamic-id", d)
	if got := countSamples(t, reg, "omugw_ws_tokens_total"); got != 4 {
		t.Fatalf("仅明确合法明细应发布（包括 0）: %d", got)
	}
	if got := wsMetricSum(t, reg, "omugw_ws_tokens_total", map[string]string{"kind": "text_input"}); got != 2 {
		t.Fatal(got)
	}
	if got := wsMetricSum(t, reg, "omugw_ws_tokens_total", map[string]string{"kind": "cache_read"}); got != 1 {
		t.Fatal(got)
	}
	if got := wsMetricSum(t, reg, "omugw_ws_usage_records_total", nil); got != 0 {
		t.Fatal("明细重复计总量", got)
	}
}
