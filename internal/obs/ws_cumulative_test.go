package obs

import (
	"math"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/canonical"
)

func TestObserveWSCumulativeMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	for _, n := range []int64{6, 7, 0} {
		m.ObserveWSUsageDelta("dashscope.inference", "task", WSUsageDelta{Unit: UnitCharacters, Characters: n})
	}
	if wsMetricSum(t, reg, "omugw_ws_characters_total", nil) != 13 || countSamples(t, reg, "omugw_ws_usage_records_total") != 0 {
		t.Fatal("delta制造记录或重复累计")
	}
	m.ObserveWSUsageRecord("dashscope.inference", "task", UnitCharacters, canonical.FidelityAuthoritative)
	m.ObserveWSUsageRecord("dashscope.inference", "task", UnitUnknown, canonical.FidelityUnavailable)
	m.ObserveWSUsageDelta("dashscope.inference", "task", WSUsageDelta{Unit: UnitTokens, InputTokens: 2, OutputTokens: 3})
	m.ObserveWSUsageDelta("dashscope.inference", "task", WSUsageDelta{Unit: UnitSeconds, Seconds: 1.25})
	for _, d := range []WSUsageDelta{
		{Unit: UnitTokens, InputTokens: -1}, {Unit: UnitTokens, InputTokens: math.MaxInt64, OutputTokens: 1}, {Unit: UnitTokens, Characters: 1},
		{Unit: UnitCharacters, Characters: -1}, {Unit: UnitCharacters, Seconds: 1}, {Unit: UnitSeconds, InputTokens: 1},
		{Unit: UnitSeconds, Seconds: -1}, {Unit: UnitSeconds, Seconds: math.NaN()}, {Unit: UnitSeconds, Seconds: math.Inf(1)},
		{Unit: UnitUnknown, Characters: 7}, {Unit: WSUsageUnit("dynamic-model"), Characters: 9},
	} {
		m.ObserveWSUsageDelta("dashscope.inference", "task", d)
	}
	for _, f := range []canonical.Fidelity{canonical.FidelityUnknown, canonical.Fidelity("invalid"), canonical.FidelityAuthoritative, canonical.FidelityEstimated} {
		m.ObserveWSUsageRecord("dashscope.inference", "task", UnitUnknown, f)
	}
	m.ObserveWSUsageRecord("dashscope.inference", "task", WSUsageUnit("dynamic-id"), canonical.FidelityUnavailable)
	for _, labels := range [][2]string{{"dynamic-model", "task"}, {"dashscope.inference", "dynamic-id"}} {
		m.ObserveWSUsageDelta(labels[0], labels[1], WSUsageDelta{Unit: UnitCharacters, Characters: 9})
		m.ObserveWSUsageRecord(labels[0], labels[1], UnitCharacters, canonical.FidelityAuthoritative)
	}
	if wsMetricSum(t, reg, "omugw_ws_characters_total", nil) != 13 || wsMetricSum(t, reg, "omugw_ws_tokens_total", nil) != 5 || wsMetricSum(t, reg, "omugw_ws_audio_input_seconds_total", nil) != 1.25 || wsMetricSum(t, reg, "omugw_ws_usage_records_total", nil) != 2 {
		t.Fatal("非法值/标签污染计量")
	}
	if countSamples(t, reg, "omugw_requests_total") != 0 {
		t.Fatal("delta增加请求数")
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		for _, metric := range f.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "model" || label.GetName() == "task_id" || label.GetValue() == "dynamic-model" || label.GetValue() == "dynamic-id" {
					t.Fatal("动态标签")
				}
			}
		}
	}
}
