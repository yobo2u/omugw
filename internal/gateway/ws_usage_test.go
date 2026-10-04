package gateway

import (
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/protocol/dashscoperealtime"
)

func TestWSUsageLedger(t *testing.T) {
	reg := prometheus.NewRegistry()
	ledger := newWSUsage(obs.NewMetrics(reg), "dashscope.realtime", "dashscope.realtime")
	observe := func(raw string) {
		t.Helper()
		e, err := dashscoperealtime.Inspect([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := ledger.Observe(dashScopeUsageEvent(e)); err != nil {
			t.Fatal(err)
		}
	}
	observe(`{"type":"session.created","session":{"id":"shared"}}`)
	observe(`{"type":"response.created","response":{"id":"shared"}}`)
	observe(`{"type":"input_audio_buffer.committed","item_id":"shared"}`)
	observe(`{"type":"response.done","response":{"id":"shared","status":"cancelled","usage":{"input_tokens":3,"output_tokens":2}}}`)
	observe(`{"type":"response.done","response":{"id":"shared","usage":{"output_tokens":2,"input_tokens":3}}}`)
	observe(`{"type":"response.done","response":{"id":"shared","usage":{"input_tokens":100,"output_tokens":2}}}`)
	observe(`{"type":"response.created","response":{"id":"shared"}}`)
	observe(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"shared"}`)
	observe(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"shared"}`)
	observe(`{"type":"response.created","response":{"id":"pending"}}`)
	observe(`{"type":"future.event","model":"must-not-be-label","usage":{"input_tokens":999}}`)
	ledger.Finish()
	ledger.Finish()
	for _, tt := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"omugw_ws_usage_records_total", map[string]string{"source": "response", "fidelity": "authoritative"}, 1},
		{"omugw_ws_usage_records_total", map[string]string{"source": "response", "fidelity": "unavailable"}, 1},
		{"omugw_ws_usage_records_total", map[string]string{"source": "transcription", "fidelity": "unavailable"}, 1},
		{"omugw_ws_usage_records_total", map[string]string{"source": "session", "fidelity": "unavailable"}, 1},
		{"omugw_ws_diagnostics_total", map[string]string{"reason": "usage_conflict"}, 1},
		{"omugw_ws_diagnostics_total", map[string]string{"reason": "usage_missing"}, 1},
		{"omugw_ws_diagnostics_total", map[string]string{"reason": "usage_unfinished"}, 2},
		{"omugw_tokens_total", map[string]string{"kind": "input", "fidelity": "authoritative"}, 3},
		{"omugw_ws_tokens_total", map[string]string{"kind": "output", "fidelity": "authoritative"}, 2},
	} {
		if got := wsUsageMetricSum(t, reg, tt.name, tt.labels); got != tt.want {
			t.Errorf("%s %v = %v, want %v", tt.name, tt.labels, got, tt.want)
		}
	}
	if err := ledger.Observe(wsUsageEvent{Source: "response", ID: "late", Started: true}); err == nil {
		t.Fatal("Finish 后不应重新开账")
	}
}

func TestWSUsageLedgerCharacters(t *testing.T) {
	reg := prometheus.NewRegistry()
	ledger := newWSUsage(obs.NewMetrics(reg), "dashscope.realtime", "dashscope.realtime")
	chars := int64(25)
	e := dashscoperealtime.Event{Source: "response", ID: "r", Terminal: true, Usage: canonical.UnavailableUsage(), Characters: &chars}
	for i := 0; i < 2; i++ {
		if err := ledger.Observe(dashScopeUsageEvent(e)); err != nil {
			t.Fatal(err)
		}
	}
	// 调用方复用指针不能改写账本中的已结值。
	chars = 30
	if err := ledger.Observe(dashScopeUsageEvent(e)); err != nil {
		t.Fatal(err)
	}
	e.ID, chars = "zero", 0
	if err := ledger.Observe(dashScopeUsageEvent(e)); err != nil {
		t.Fatal(err)
	}
	ledger.Finish()
	if got := wsUsageMetricSum(t, reg, "omugw_ws_characters_total", nil); got != 25 {
		t.Fatal(got)
	}
	if got := wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", map[string]string{"unit": "characters", "fidelity": "authoritative"}); got != 2 {
		t.Fatal(got)
	}
	if got := wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", map[string]string{"unit": "tokens"}); got != 0 {
		t.Fatal(got)
	}
	if got := wsUsageMetricSum(t, reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "usage_conflict"}); got != 1 {
		t.Fatal(got)
	}
}

func TestWSUsageLedgerDualUnits(t *testing.T) {
	reg := prometheus.NewRegistry()
	ledger := newWSUsage(obs.NewMetrics(reg), "dashscope.realtime", "dashscope.realtime")
	chars := int64(25)
	e := dashscoperealtime.Event{Source: "response", ID: "r", Terminal: true, Usage: canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: 8, OutputTokens: 32, AudioOutputTokens: 32}, Characters: &chars}
	for _, next := range []dashscoperealtime.Event{e, e, {Source: "response", ID: "pending", Started: true}} {
		if err := ledger.Observe(dashScopeUsageEvent(next)); err != nil {
			t.Fatal(err)
		}
	}
	chars = 26
	if err := ledger.Observe(dashScopeUsageEvent(e)); err != nil {
		t.Fatal(err)
	}
	chars, e.Usage.OutputTokens = 25, 33
	if err := ledger.Observe(dashScopeUsageEvent(e)); err != nil {
		t.Fatal(err)
	}
	ledger.Finish()
	ledger.Finish()
	for _, tt := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"omugw_ws_characters_total", nil, 25},
		{"omugw_ws_tokens_total", map[string]string{"kind": "input"}, 8},
		{"omugw_ws_tokens_total", map[string]string{"kind": "output"}, 32},
		{"omugw_ws_tokens_total", map[string]string{"kind": "audio_output"}, 32},
		{"omugw_tokens_total", map[string]string{"kind": "input"}, 8},
		{"omugw_tokens_total", map[string]string{"kind": "output"}, 32},
		{"omugw_ws_usage_records_total", map[string]string{"unit": "characters", "fidelity": "authoritative"}, 1},
		{"omugw_ws_usage_records_total", map[string]string{"unit": "tokens", "fidelity": "authoritative"}, 1},
		{"omugw_ws_usage_records_total", map[string]string{"unit": "tokens", "fidelity": "unavailable"}, 1},
		{"omugw_ws_diagnostics_total", map[string]string{"reason": "usage_conflict"}, 2},
	} {
		if got := wsUsageMetricSum(t, reg, tt.name, tt.labels); got != tt.want {
			t.Errorf("%s %v = %v, want %v", tt.name, tt.labels, got, tt.want)
		}
	}
}

func TestWSUsageLedgerInspectedUnits(t *testing.T) {
	for _, tt := range []struct {
		name, usage            string
		tokens, chars, invalid float64
	}{
		{"observed", `{"characters":25,"input_tokens":8,"output_tokens":32,"total_tokens":40,"input_tokens_details":{"text_tokens":8},"output_tokens_details":{"text_tokens":0,"audio_tokens":32}}`, 1, 1, 0},
		{"zero", `{"characters":0,"input_tokens":0,"output_tokens":0}`, 1, 1, 0},
		{"only_characters", `{"characters":25}`, 0, 1, 0},
		{"bad_tokens", `{"characters":25,"input_tokens":8,"output_tokens":null}`, 0, 1, 1},
		{"bad_characters", `{"characters":null,"input_tokens":8,"output_tokens":32}`, 1, 0, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			ledger := newWSUsage(obs.NewMetrics(reg), "dashscope.realtime", "dashscope.realtime")
			e, err := dashscoperealtime.Inspect([]byte(`{"type":"response.done","response":{"id":"r","usage":` + tt.usage + `}}`))
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := ledger.Observe(dashScopeUsageEvent(e)); err != nil {
					t.Fatal(err)
				}
			}
			ledger.Finish()
			for unit, want := range map[string]float64{"tokens": tt.tokens, "characters": tt.chars} {
				if got := wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", map[string]string{"unit": unit}); got != want {
					t.Errorf("%s records=%v want=%v", unit, got, want)
				}
			}
			if got := wsUsageMetricSum(t, reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "usage_invalid"}); got != tt.invalid {
				t.Errorf("invalid=%v want=%v", got, tt.invalid)
			}
		})
	}
}

func TestWSUsageLedgerLateInvalidUnit(t *testing.T) {
	reg := prometheus.NewRegistry()
	ledger := newWSUsage(obs.NewMetrics(reg), "dashscope.realtime", "dashscope.realtime")
	for _, usage := range []string{`{"characters":25}`, `{"characters":25,"input_tokens":null}`} {
		e, err := dashscoperealtime.Inspect([]byte(`{"type":"response.done","response":{"id":"r","usage":` + usage + `}}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := ledger.Observe(dashScopeUsageEvent(e)); err != nil {
			t.Fatal(err)
		}
	}
	ledger.Finish()
	for _, reason := range []string{"usage_invalid", "usage_conflict"} {
		if got := wsUsageMetricSum(t, reg, "omugw_ws_diagnostics_total", map[string]string{"reason": reason}); got != 1 {
			t.Errorf("迟到非法单位被去重吞掉：%s=%v", reason, got)
		}
	}
	if got := wsUsageMetricSum(t, reg, "omugw_ws_characters_total", nil); got != 25 {
		t.Fatal(got)
	}
	if got := wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", map[string]string{"unit": "tokens"}); got != 0 {
		t.Fatal(got)
	}
}

func TestWSUsageLedgerBoundaries(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint("pending=", pending), func(t *testing.T) {
			reg := prometheus.NewRegistry()
			ledger := newWSUsage(obs.NewMetrics(reg), "dashscope.realtime", "dashscope.realtime")
			for i := 0; i < 4096; i++ {
				e := wsUsageEvent{Source: "response", ID: fmt.Sprint(i), Started: pending, Terminal: !pending, Usage: canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: 1}}
				if err := ledger.Observe(e); err != nil {
					t.Fatalf("第 %d 笔: %v", i+1, err)
				}
			}
			if err := ledger.Observe(wsUsageEvent{Source: "transcription", ID: "0", Started: true}); err == nil {
				t.Fatal("跨来源新记录应与已结/未结记录共享 4096 上限")
			}
			// 到限也不淘汰第一笔；已存在的未结项仍可收取最后的权威用量。
			for i := 0; i < 2; i++ {
				if err := ledger.Observe(wsUsageEvent{Source: "response", ID: "0", Terminal: true, Usage: canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: 1}}); err != nil {
					t.Fatal(err)
				}
			}
			ledger.Finish()
			want := float64(4096)
			if pending {
				want = 1
			}
			if got := wsUsageMetricSum(t, reg, "omugw_tokens_total", map[string]string{"kind": "input"}); got != want {
				t.Fatalf("已收权威用量 = %v, want %v", got, want)
			}
			if got := wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", nil); got != 4096 {
				t.Fatalf("记录数 = %v", got)
			}
		})
	}
	for _, id := range []string{"", strings.Repeat("s", 513)} {
		ledger := newWSUsage(nil, "dashscope.realtime", "dashscope.realtime")
		if err := ledger.Observe(wsUsageEvent{Source: "response", ID: id, Terminal: true}); err == nil {
			t.Fatal("关联 ID 不合法时不能默默漏账")
		}
	}
}

func TestWSUsageLedgerSessionFinishedAndFailure(t *testing.T) {
	reg := prometheus.NewRegistry()
	ledger := newWSUsage(obs.NewMetrics(reg), "dashscope.realtime", "dashscope.realtime")
	for _, raw := range []string{
		`{"type":"session.created","session":{"id":"s"}}`,
		`{"type":"error","error":{"code":"invalid_value","message":"private"}}`,
		`{"type":"session.finished"}`, `{"type":"session.finished"}`,
	} {
		e, err := dashscoperealtime.Inspect([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := ledger.Observe(dashScopeUsageEvent(e)); err != nil {
			t.Fatal(err)
		}
	}
	ledger.Finish()
	if got := wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", map[string]string{"source": "session"}); got != 1 {
		t.Fatal(got)
	}
	if got := wsUsageMetricSum(t, reg, "omugw_upstream_errors_total", map[string]string{"class": "bad_request"}); got != 1 {
		t.Fatal(got)
	}
	ledger = newWSUsage(nil, "dashscope.realtime", "dashscope.realtime")
	if err := ledger.Observe(wsUsageEvent{Source: "session", Terminal: true}); err == nil {
		t.Fatal("未创建会话时不能凭 event_id 代替 session.id")
	}
	ledger.Finish()
	ledger.Finish()
}

func TestWSUsageLedgerStructuredComparison(t *testing.T) {
	reg := prometheus.NewRegistry()
	ledger := newWSUsage(obs.NewMetrics(reg), "dashscope.realtime", "dashscope.realtime")
	e := dashscoperealtime.Event{Source: "response", ID: "shared", Terminal: true, Usage: canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: 3, OutputTokens: 2, AudioInputTokens: 1}}
	for _, source := range []string{"response", "transcription"} {
		e.Source = source
		if err := ledger.Observe(dashScopeUsageEvent(e)); err != nil {
			t.Fatal(err)
		}
	}
	e.Source = "response"
	e.Usage.AudioInputTokens = 2
	if err := ledger.Observe(dashScopeUsageEvent(e)); err != nil {
		t.Fatal(err)
	}
	chars := int64(3)
	e.Characters, e.Usage = &chars, canonical.UnavailableUsage()
	if err := ledger.Observe(dashScopeUsageEvent(e)); err != nil {
		t.Fatal(err)
	}
	ledger.Finish()
	if got := wsUsageMetricSum(t, reg, "omugw_tokens_total", map[string]string{"kind": "input"}); got != 6 {
		t.Fatalf("跨来源不能去重：%v", got)
	}
	if got := wsUsageMetricSum(t, reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "usage_conflict"}); got != 2 {
		t.Fatalf("分项或计价单位矛盾未被发现：%v", got)
	}
	if got := wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", nil); got != 2 {
		t.Fatal(got)
	}
}

func wsUsageMetricSum(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
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
