package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/protocol/openairealtime"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

const openAIReady = `{"type":"session.created","session":{"id":"s","object":"realtime.session","type":"realtime","audio":{"input":{"transcription":null}}}}`
const openAIResponseUsage = `{"input_tokens":132,"output_tokens":121,"total_tokens":253,"input_token_details":{"text_tokens":119,"audio_tokens":13,"image_tokens":0,"cached_tokens":64,"cached_tokens_details":{"text_tokens":64,"audio_tokens":0,"image_tokens":0}},"output_token_details":{"text_tokens":30,"audio_tokens":91}}`
const openAIASRUsage = `{"type":"tokens","input_tokens":13,"output_tokens":9,"total_tokens":22,"input_token_details":{"text_tokens":0,"audio_tokens":13}}`

func newOpenAIObserverTest(t *testing.T) (wsEventObserver, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	h := NewOpenAIRealtimeHandler(WSDeps{})
	return h.profile.newObserver(obs.NewMetrics(reg), "openai.realtime", "openai.realtime"), reg
}

func observeOpenAI(t *testing.T, o wsEventObserver, raw ...string) {
	t.Helper()
	for _, s := range raw {
		b := []byte(s)
		if err := o.Observe(b); err != nil {
			t.Fatalf("观测失败: %v", err)
		}
		if !bytes.Equal(b, []byte(s)) {
			t.Fatal("观测改写原消息")
		}
		// 归还帧后复用底层内存，防止账本借用音频/全文或 ID。
		clear(b)
	}
}

func openAIConfig(value string) string {
	return `{"type":"session.updated","session":{"id":"s","type":"realtime","object":"realtime.session","audio":{"input":{"transcription":` + value + `}}}}`
}

func openAIASR(item string, part int, usage string) string {
	return fmt.Sprintf(`{"type":"conversation.item.input_audio_transcription.completed","event_id":"terminal","item_id":%q,"content_index":%d,"usage":%s}`, item, part, usage)
}

func openAIResponse(id, usage string) string {
	return `{"type":"response.done","response":{"id":"` + id + `","status":"cancelled","usage":` + usage + `}}`
}

func assertOpenAIMetric(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string, want float64) {
	t.Helper()
	if got := wsUsageMetricSum(t, reg, name, labels); got != want {
		t.Errorf("%s %v = %v, want %v", name, labels, got, want)
	}
}

func TestOpenAIRealtimeUsageNoSessionPhantom(t *testing.T) {
	o, reg := newOpenAIObserverTest(t)
	observeOpenAI(t, o, openAIReady, openAIConfig(`{"model":"gpt-4o-mini-transcribe"}`), `{"type":"session.finished"}`)
	o.Finish()
	o.Finish()
	assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", nil, 0)
	assertOpenAIMetric(t, reg, "omugw_ws_diagnostics_total", nil, 0)
}

func TestOpenAIRealtimeTranscriptionCorrelation(t *testing.T) {
	for _, mode := range []string{"enabled", "disabled", "unknown", "unknown-after-enabled", "client-update", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			o, reg := newOpenAIObserverTest(t)
			switch mode {
			case "enabled":
				observeOpenAI(t, o, openAIConfig(`{"model":"gpt-4o-mini-transcribe"}`))
			case "disabled":
				observeOpenAI(t, o, openAIReady)
			case "unknown-after-enabled":
				observeOpenAI(t, o, openAIConfig(`{"model":"gpt-4o-mini-transcribe"}`), openAIConfig(`{}`))
			case "client-update":
				observeOpenAI(t, o, `{"type":"session.update","session":{"audio":{"input":{"transcription":{"model":"unconfirmed"}}}}}`)
			case "legacy":
				observeOpenAI(t, o, `{"type":"session.created","session":{"id":"s","object":"realtime.session","type":"realtime","input_audio_transcription":{"model":"legacy"}}}`)
			}
			observeOpenAI(t, o, `{"type":"input_audio_buffer.committed","item_id":"only-commit"}`)
			// 实际 ASR 即使未见开始，也不受先前 disabled/unknown 的限制。
			observeOpenAI(t, o, openAIASR("actual", 0, openAIASRUsage))
			o.Finish()
			want := float64(0)
			if mode == "enabled" {
				want = 1
			}
			assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"fidelity": "unavailable"}, want)
			assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"source": "transcription", "fidelity": "authoritative"}, 1)
			unknown := wsUsageMetricSum(t, reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "transcription_config_unknown"})
			if (unknown > 0) != (mode != "enabled" && mode != "disabled") {
				t.Errorf("未知配置诊断=%v", unknown)
			}
		})
	}
	for _, asrFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("asr-first=", asrFirst), func(t *testing.T) {
			o, reg := newOpenAIObserverTest(t)
			observeOpenAI(t, o, openAIConfig(`{"model":"gpt-4o-mini-transcribe"}`),
				`{"type":"input_audio_buffer.committed","item_id":"shared"}`,
				`{"type":"conversation.item.input_audio_transcription.delta","item_id":"shared","delta":"private text"}`,
				`{"type":"conversation.item.input_audio_transcription.segment","item_id":"shared","content_index":0,"text":"private text"}`,
				`{"type":"response.created","response":{"id":"shared"}}`)
			response := openAIResponse("shared", openAIResponseUsage)
			asr := openAIASR("shared", 0, openAIASRUsage)
			if asrFirst {
				observeOpenAI(t, o, asr, response)
			} else {
				observeOpenAI(t, o, response)
				assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"source": "transcription"}, 0)
				observeOpenAI(t, o, asr)
			}
			observeOpenAI(t, o, asr, strings.Replace(asr, `"terminal"`, `"different-event-id"`, 1),
				`{"type":"conversation.item.input_audio_transcription.delta","item_id":"shared","delta":"late"}`,
				`{"type":"input_audio_buffer.committed","item_id":"shared"}`,
				openAIASR("shared", 1, `{"type":"duration","seconds":1.25}`),
				`{"type":"conversation.item.input_audio_transcription.delta","item_id":"shared","delta":"ambiguous"}`)
			o.Finish()
			o.Finish()
			assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"source": "response", "unit": "tokens"}, 1)
			assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"source": "transcription", "unit": "tokens"}, 1)
			assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"unit": "seconds"}, 1)
			assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"fidelity": "unavailable"}, 0)
			assertOpenAIMetric(t, reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "usage_conflict"}, 0)
			assertOpenAIMetric(t, reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "usage_ambiguous"}, 1)
		})
	}
	t.Run("unknown-part-migrates-at-terminal", func(t *testing.T) {
		o, reg := newOpenAIObserverTest(t)
		observeOpenAI(t, o, `{"type":"conversation.item.input_audio_transcription.delta","item_id":"i"}`,
			openAIASR("i", 3, openAIASRUsage))
		o.Finish()
		assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", nil, 1)
		assertOpenAIMetric(t, reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "usage_unfinished"}, 0)
	})
}

func TestOpenAIRealtimeUsageMetrics(t *testing.T) {
	o, reg := newOpenAIObserverTest(t)
	observeOpenAI(t, o, openAIReady, openAIResponse("r", openAIResponseUsage), openAIASR("i", 0, openAIASRUsage),
		openAIASR("i", 1, `{"type":"duration","seconds":1.25}`), openAIASR("i", 2, `{"type":"duration","seconds":0}`))
	o.Finish()
	for source, want := range map[string]map[string]float64{
		"response":      {"input": 132, "output": 121, "text_input": 119, "audio_input": 13, "image_input": 0, "cache_read": 64, "cached_text_input": 64, "cached_audio_input": 0, "cached_image_input": 0, "text_output": 30, "audio_output": 91},
		"transcription": {"input": 13, "output": 9, "text_input": 0, "audio_input": 13},
	} {
		for kind, value := range want {
			assertOpenAIMetric(t, reg, "omugw_ws_tokens_total", map[string]string{"source": source, "kind": kind}, value)
		}
		assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"source": source, "unit": "tokens", "fidelity": "authoritative"}, 1)
	}
	assertOpenAIMetric(t, reg, "omugw_ws_audio_input_seconds_total", map[string]string{"source": "transcription", "fidelity": "authoritative"}, 1.25)
	assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"unit": "seconds"}, 2)
	assertOpenAIMetric(t, reg, "omugw_tokens_total", map[string]string{"kind": "input"}, 145)
	assertOpenAIMetric(t, reg, "omugw_tokens_total", map[string]string{"kind": "output"}, 130)
	// 零明细必须有样本，ASR 没报告的 cache/image/output 明细不能补成零。
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]float64{}
	for _, f := range families {
		for _, metric := range f.Metric {
			source, kind := "", ""
			for _, label := range metric.Label {
				if label.GetName() == "source" {
					source = label.GetValue()
				}
				if label.GetName() == "kind" {
					kind = label.GetValue()
				}
				if label.GetName() == "id" || label.GetName() == "model" || label.GetName() == "part" {
					t.Fatal("高基数标签进入指标")
				}
			}
			if f.GetName() == "omugw_ws_tokens_total" {
				seen[source+"/"+kind] = metric.GetCounter().GetValue()
			}
		}
	}
	for _, key := range []string{"response/image_input", "response/cached_audio_input", "response/cached_image_input", "transcription/text_input"} {
		if value, ok := seen[key]; !ok || value != 0 {
			t.Errorf("显式零缺失: %s", key)
		}
	}
	for _, key := range []string{"transcription/cache_read", "transcription/image_input", "transcription/audio_output", "response/reasoning"} {
		if _, ok := seen[key]; ok {
			t.Errorf("制造了缺失明细: %s", key)
		}
	}
}

func TestOpenAIRealtimeUsageDedupAndCapacity(t *testing.T) {
	t.Run("presence-and-conflicts", func(t *testing.T) {
		o, reg := newOpenAIObserverTest(t)
		observeOpenAI(t, o,
			openAIResponse("r", `{"input_tokens":0,"output_tokens":0}`),
			openAIResponse("r", `{"input_tokens":0,"output_tokens":0,"input_token_details":{"audio_tokens":0}}`),
			openAIASR("i", 0, `{"type":"duration","seconds":0}`),
			openAIASR("i", 0, `{"type":"duration","seconds":1.25}`),
			openAIASR("i", 0, `{"type":"tokens","input_tokens":0,"output_tokens":0}`),
			openAIASR("i", 0, `null`))
		o.Finish()
		assertOpenAIMetric(t, reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "usage_conflict"}, 4)
		assertOpenAIMetric(t, reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "usage_missing"}, 1)
		assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"unit": "tokens"}, 1)
		assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"unit": "seconds"}, 1)
	})
	t.Run("shared-capacity-migration-and-no-eviction", func(t *testing.T) {
		o, reg := newOpenAIObserverTest(t)
		observeOpenAI(t, o, openAIConfig(`{"model":"gpt-4o-mini-transcribe"}`))
		for i := 0; i < 2048; i++ {
			observeOpenAI(t, o, openAIResponse(fmt.Sprint(i), `{"input_tokens":1,"output_tokens":0}`),
				fmt.Sprintf(`{"type":"input_audio_buffer.committed","item_id":"%d"}`, i))
		}
		// 到限仍允许未知 part 原位迁移；重复和迟到无 index 事件不新占名额。
		observeOpenAI(t, o, openAIASR("0", 0, openAIASRUsage), openAIASR("0", 0, openAIASRUsage),
			`{"type":"conversation.item.input_audio_transcription.delta","item_id":"0"}`,
			openAIResponse("0", `{"input_tokens":1,"output_tokens":0}`))
		for _, raw := range []string{openAIASR("0", 1, openAIASRUsage), `{"type":"input_audio_buffer.committed","item_id":"new"}`} {
			err := o.Observe([]byte(raw))
			code, _, _ := classifyWSRelay(wsRelayResult{err: err, upstream: true})
			if !errors.Is(err, errWSUsageLimit) || code != 1008 {
				t.Fatalf("容量策略=%v/%d", err, code)
			}
		}
		o.Finish()
		assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", nil, 4096)
		assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"fidelity": "unavailable"}, 2047)
		assertOpenAIMetric(t, reg, "omugw_ws_tokens_total", map[string]string{"source": "response", "kind": "input"}, 2048)
		assertOpenAIMetric(t, reg, "omugw_ws_tokens_total", map[string]string{"source": "transcription", "kind": "input"}, 13)
	})
	for _, fields := range []string{`"item_id":"","content_index":0`, `"item_id":"` + strings.Repeat("i", 513) + `","content_index":0`, `"item_id":"i"`, `"item_id":"i","content_index":null`, `"item_id":"i","content_index":-1`, `"item_id":"i","content_index":2147483648`, `"item_id":"i","content_index":0,"content_index":0`} {
		o, reg := newOpenAIObserverTest(t)
		err := o.Observe([]byte(`{"type":"conversation.item.input_audio_transcription.completed",` + fields + `,"usage":` + openAIASRUsage + `}`))
		if !errors.Is(err, errWSRelayPolicy) {
			t.Fatalf("非法关联未拒绝: %v", err)
		}
		o.Finish()
		assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", nil, 0)
	}
}

func TestOpenAIRealtimeUsageInvalidDetailsAndFailure(t *testing.T) {
	o, reg := newOpenAIObserverTest(t)
	bad := openAIResponse("r", `{"input_tokens":132,"output_tokens":121,"input_token_details":{"audio_tokens":133}}`)
	observeOpenAI(t, o, bad, bad, `{"type":"error","event_id":"not-an-item","error":{"code":"invalid_value","message":"private"}}`,
		`{"type":"conversation.item.input_audio_transcription.failed","item_id":"failed","content_index":0,"error":{"type":"server_error","message":"private"}}`)
	o.Finish()
	assertOpenAIMetric(t, reg, "omugw_ws_tokens_total", map[string]string{"kind": "input"}, 132)
	assertOpenAIMetric(t, reg, "omugw_ws_tokens_total", map[string]string{"kind": "audio_input"}, 0)
	assertOpenAIMetric(t, reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "usage_invalid"}, 1)
	assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", nil, 2)
	assertOpenAIMetric(t, reg, "omugw_upstream_errors_total", map[string]string{"class": "bad_request"}, 1)
	assertOpenAIMetric(t, reg, "omugw_upstream_errors_total", map[string]string{"class": "upstream_unavailable"}, 1)
}

func TestOpenAIRealtimeObserveBeforeFailedWrite(t *testing.T) {
	b := wsTestBudget(t, 1<<20)
	var gate *wsTestGate
	down, client := wsTestLink(t, false, b, 1<<18, 0, func(c net.Conn) net.Conn { gate = newWSTestGate(c, true); return gate })
	up, server := wsTestLink(t, true, b, 1<<18, 0, nil)
	o, reg := newOpenAIObserverTest(t)
	done := make(chan error, 1)
	go func() {
		done <- relayWS(context.Background(), down, up, nil, o, NewOpenAIRealtimeHandler(WSDeps{}).profile.classifyClose, 0)
	}()
	server.send(t, ws.OpText, []byte(openAIReady))
	if f := client.read(t); string(f.Payload) != openAIReady {
		t.Fatal("首事件未原样转发")
	}
	gate.armed.Store(true)
	server.send(t, ws.OpText, []byte(openAIResponse("r", openAIResponseUsage)))
	awaitWSTest(t, gate.entered)
	if err := receiveWSTest(t, done); !errors.Is(err, errWSRelayDownstream) {
		t.Fatal(err)
	}
	assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"fidelity": "authoritative"}, 1)
	assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"fidelity": "unavailable"}, 0)
	assertOpenAIMetric(t, reg, "omugw_ws_tokens_total", map[string]string{"kind": "input"}, 132)
	if b.Used() != 0 {
		t.Fatal("消息预算未归还")
	}
}

func TestOpenAIRealtimeUsageSnapshotAllDetails(t *testing.T) {
	// 每个值及其 presence 都必须进入去重指纹；不能仅比较 Canonical 中的投影。
	for field := 0; field < 9; field++ {
		for _, changePresence := range []bool{false, true} {
			t.Run(fmt.Sprintf("field=%d/presence=%v", field, changePresence), func(t *testing.T) {
				reg := prometheus.NewRegistry()
				u := newWSUsage(obs.NewMetrics(reg), "openai.realtime", "openai.realtime")
				e, err := openairealtime.Inspect([]byte(openAIResponse("r", openAIResponseUsage)))
				if err != nil {
					t.Fatal(err)
				}
				fact := openAIUsageEvent(e)
				if err := u.Observe(fact); err != nil {
					t.Fatal(err)
				}
				value := reflect.ValueOf(&e.Details).Elem().Field(field)
				if changePresence {
					value.SetZero()
				} else {
					value.Elem().SetInt(value.Elem().Int() + 1)
				}
				if err := u.Observe(openAIUsageEvent(e)); err != nil {
					t.Fatal(err)
				}
				if err := u.Observe(fact); err != nil {
					t.Fatal(err)
				}
				u.Finish()
				assertOpenAIMetric(t, reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "usage_conflict"}, 1)
				assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", nil, 1)
			})
		}
	}
	reg := prometheus.NewRegistry()
	u := newWSUsage(obs.NewMetrics(reg), "openai.realtime", "openai.realtime")
	e, err := openairealtime.Inspect([]byte(openAIASR("i", 0, `{"type":"duration","seconds":1.25}`)))
	if err != nil {
		t.Fatal(err)
	}
	fact := openAIUsageEvent(e)
	if err := u.Observe(fact); err != nil {
		t.Fatal(err)
	}
	*e.Seconds = 2.5
	if err := u.Observe(fact); err != nil {
		t.Fatal(err)
	}
	u.Finish()
	assertOpenAIMetric(t, reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "usage_conflict"}, 1)
	assertOpenAIMetric(t, reg, "omugw_ws_audio_input_seconds_total", nil, 1.25)
}

func TestOpenAIRealtimeObserverFinished(t *testing.T) {
	o, _ := newOpenAIObserverTest(t)
	observeOpenAI(t, o, openAIReady)
	o.Finish()
	for _, raw := range []string{`{"type":"input_audio_buffer.committed","item_id":"i"}`, openAIConfig(`{"model":"late"}`), openAIResponse("r", openAIResponseUsage)} {
		if err := o.Observe([]byte(raw)); !errors.Is(err, errWSUsageFinished) {
			t.Fatalf("Finish 后仍接受观测: %v", err)
		}
	}
}
