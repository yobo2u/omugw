package gateway

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

func TestInferenceCumulativeUsage(t *testing.T) {
	for _, tc := range []struct {
		name, model, task, unit string
		payloads                []string
		terminal                string
		want                    float64
		fidelity                string
	}{
		{"6到13终态13", inferenceTTS, "tts", "characters", []string{`{"characters":6}`, `{"characters":13}`}, `{"characters":13}`, 13, "authoritative"},
		{"回退冻结", inferenceTTS, "tts", "characters", []string{`{"characters":6}`, `{"characters":3}`, `{"characters":6}`}, `{"characters":6}`, 6, "unavailable"},
		{"终态零", inferenceTTS, "tts", "characters", nil, `{"characters":0}`, 0, "authoritative"},
		{"null不冒充最终量", inferenceTTS, "tts", "characters", []string{`{"characters":6}`}, `null`, 6, "unavailable"},
		{"全null不造数", inferenceTTS, "tts", "characters", nil, `null`, 0, "unavailable"},
		{"终态缺单位不冒充最终量", inferenceTTS, "tts", "characters", []string{`{"characters":6}`}, `{}`, 6, "unavailable"},
		{"非法负值冻结", inferenceTTS, "tts", "characters", []string{`{"characters":6}`, `{"characters":-1}`}, `{"characters":9}`, 6, "unavailable"},
		{"溢出冻结", inferenceTTS, "tts", "characters", []string{`{"characters":6}`, `{"characters":9223372036854775808}`}, `{"characters":9}`, 6, "unavailable"},
		{"冲突单位冻结", inferenceTTS, "tts", "characters", []string{`{"characters":6}`, `{"characters":7,"duration":2}`}, `{"characters":9}`, 6, "unavailable"},
		{"秒", inferenceASR, "asr", "seconds", []string{`{"duration":1.25}`, `{"duration":2.5}`}, `{"duration":2.5}`, 2.5, "authoritative"},
		{"秒溢出", inferenceASR, "asr", "seconds", []string{`{"duration":1.25}`, `{"duration":1e999}`}, `{"duration":2.5}`, 1.25, "unavailable"},
		{"秒NaN冻结", inferenceASR, "asr", "seconds", []string{`{"duration":1.25}`, `{"duration":"NaN"}`}, `{"duration":2.5}`, 1.25, "unavailable"},
		{"未知不猜", "future-audio", "asr", "unknown", []string{`{"duration":6}`}, `{"duration":13}`, 0, "unavailable"},
		{"未证实秒不猜", "paraformer-realtime-v2", "asr", "seconds", []string{`{"duration":6}`}, `{"duration":13}`, 0, "unavailable"},
		{"未证实token不猜", "qwen-audio-3.1-tts-flash", "tts", "tokens", nil, `{"input_tokens":1,"output_tokens":2,"total_tokens":3}`, 0, "unavailable"},
		{"token兼容duration", "qwen-audio-3.1-asr-flash-streaming", "asr", "tokens", []string{`{"input_tokens":6,"output_tokens":0,"total_tokens":6,"duration":999}`}, `{"input_tokens":10,"output_tokens":3,"total_tokens":13,"duration":888}`, 13, "authoritative"},
		{"token分项回退冻结", "qwen-audio-3.1-asr-flash-streaming", "asr", "tokens", []string{`{"input_tokens":6,"output_tokens":0,"total_tokens":6}`, `{"input_tokens":3,"output_tokens":3,"total_tokens":6}`}, `{"input_tokens":6,"output_tokens":3,"total_tokens":9}`, 6, "unavailable"},
		{"token求和溢出冻结", "qwen-audio-3.1-asr-flash-streaming", "asr", "tokens", []string{`{"input_tokens":6,"output_tokens":0,"total_tokens":6}`, `{"input_tokens":9223372036854775807,"output_tokens":1,"total_tokens":9223372036854775807}`}, `{"input_tokens":6,"output_tokens":3,"total_tokens":9}`, 6, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, reg, _ := inferencePolicy(t)
			inferenceStart(t, p, "A", tc.model, tc.task, "duplex")
			for _, u := range tc.payloads {
				inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "result-generated", `{"output":{"event":"sentence-end"},"usage":`+u+`}`))
				if n := wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", nil); n != 0 {
					t.Fatal("中间快照伪装结账", n)
				}
			}
			terminal := inferenceServer("A", "task-finished", `{"usage":`+tc.terminal+`}`)
			inferenceSend(t, p, wsUpstreamToClient, terminal)
			inferenceSend(t, p, wsUpstreamToClient, terminal)
			p.Finish()
			p.Finish()
			metric := "omugw_ws_characters_total"
			switch tc.unit {
			case "seconds":
				metric = "omugw_ws_audio_input_seconds_total"
			case "tokens":
				metric = "omugw_ws_tokens_total"
			}
			if n := wsUsageMetricSum(t, reg, metric, nil); n != tc.want {
				t.Fatalf("数字=%v want=%v", n, tc.want)
			}
			if tc.want == 0 {
				families, err := reg.Gather()
				if err != nil {
					t.Fatal(err)
				}
				var samples int
				for _, family := range families {
					if family.GetName() == "omugw_ws_characters_total" || family.GetName() == "omugw_ws_audio_input_seconds_total" || family.GetName() == "omugw_ws_tokens_total" {
						samples += len(family.Metric)
					}
				}
				want := 0
				if tc.fidelity == "authoritative" {
					want = 1
				}
				if samples != want {
					t.Fatalf("显式零/无数据混淆: samples=%d want=%d", samples, want)
				}
			}
			if n := wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", map[string]string{"unit": tc.unit, "fidelity": tc.fidelity, "source": "task"}); n != 1 {
				t.Fatal("结算", n)
			}
			if n := wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", nil); n != 1 {
				t.Fatal("重复记录", n)
			}
			if tc.unit == "tokens" {
				if n := wsUsageMetricSum(t, reg, "omugw_tokens_total", nil); n != tc.want {
					t.Fatal("全局token差额", n)
				}
				if n := wsUsageMetricSum(t, reg, "omugw_ws_audio_input_seconds_total", nil); n != 0 {
					t.Fatal("兼容duration制造秒", n)
				}
			}
			if wsUsageMetricSum(t, reg, "omugw_requests_total", nil) != 0 {
				t.Fatal("快照增加请求数")
			}
		})
	}
	t.Run("A完成B中断保两者已发布数字", func(t *testing.T) {
		p, reg, _ := inferencePolicy(t)
		inferenceStart(t, p, "A", inferenceASR, "asr", "duplex")
		inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "task-finished", `{"usage":{"duration":13}}`))
		inferenceStart(t, p, "B", inferenceASR, "asr", "duplex")
		inferenceSend(t, p, wsUpstreamToClient, inferenceServer("B", "result-generated", `{"usage":{"duration":6}}`))
		p.Finish()
		if n := wsUsageMetricSum(t, reg, "omugw_ws_audio_input_seconds_total", nil); n != 19 {
			t.Fatal(n)
		}
		for _, f := range []string{"authoritative", "unavailable"} {
			if wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", map[string]string{"fidelity": f}) != 1 {
				t.Fatal(f)
			}
		}
		if wsUsageMetricSum(t, reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "usage_unfinished"}) != 1 {
			t.Fatal("缺中断诊断")
		}
	})
	t.Run("并发Finish与delta终态", func(t *testing.T) {
		for i := 0; i < 64; i++ {
			p, reg, _ := inferencePolicy(t)
			inferenceStart(t, p, "A", inferenceASR, "asr", "duplex")
			inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "result-generated", `{"usage":{"duration":6}}`))
			var wg sync.WaitGroup
			start := make(chan struct{})
			for n := 0; n < 8; n++ {
				wg.Add(1)
				go func() { defer wg.Done(); <-start; p.Finish() }()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				d, err := p.BeforeForward(context.Background(), wsUpstreamToClient, ws.OpText, inferenceServer("A", "task-finished", `{"usage":{"duration":13}}`))
				if err == nil {
					p.AfterForward(d.Ticket, nil)
				}
			}()
			close(start)
			wg.Wait()
			amount := wsUsageMetricSum(t, reg, "omugw_ws_audio_input_seconds_total", nil)
			auth := wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", map[string]string{"fidelity": "authoritative"})
			unavailable := wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", map[string]string{"fidelity": "unavailable"})
			if auth+unavailable != 1 || !(auth == 1 && amount == 13 || unavailable == 1 && amount == 6) {
				t.Fatal(fmt.Sprintf("结算时序错误 amount=%v auth=%v unavailable=%v", amount, auth, unavailable))
			}
		}
	})
	t.Run("非sentence-end与未知事件不发布", func(t *testing.T) {
		p, reg, _ := inferencePolicy(t)
		inferenceStart(t, p, "A", inferenceTTS, "tts", "duplex")
		inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "result-generated", `{"output":{"event":"sentence-begin"},"usage":{"characters":99}}`))
		inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "future", `{"output":{"event":"sentence-end"},"usage":{"characters":77}}`))
		inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "task-finished", `{"usage":{"characters":6}}`))
		if n := wsUsageMetricSum(t, reg, "omugw_ws_characters_total", nil); n != 6 {
			t.Fatal(n)
		}
	})
	t.Run("指标锁外发布且Finish等待差额和记录", func(t *testing.T) {
		for _, terminal := range []bool{false, true} {
			p, reg, _ := inferencePolicy(t)
			inferenceStart(t, p, "A", inferenceASR, "asr", "duplex")
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			// 使用真实 Prometheus constraint 暂停发布；policy 与计量方法均为真实实现。
			p.metrics.WSAudioInputSeconds = prometheus.V2.NewCounterVec(prometheus.CounterVecOpts{
				CounterOpts: prometheus.CounterOpts{Name: "inference_test_delta_total", Help: "测试差额发布顺序。"},
				VariableLabels: prometheus.ConstrainedLabels{
					{Name: "protocol", Constraint: func(s string) string { close(entered); <-release; return s }},
					{Name: "source"}, {Name: "fidelity"},
				},
			})
			reg.MustRegister(p.metrics.WSAudioInputSeconds)
			event := "result-generated"
			if terminal {
				event = "task-finished"
			}
			forward := make(chan inferenceResult, 1)
			go func() {
				d, err := p.BeforeForward(context.Background(), wsUpstreamToClient, ws.OpText, inferenceServer("A", event, `{"usage":{"duration":6}}`))
				forward <- inferenceResult{d, err}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("未进入真实指标发布")
			}
			finished := make(chan struct{})
			go func() { p.Finish(); close(finished) }()
			select {
			case <-finished:
				t.Fatal("Finish越过未完成的差额发布")
			case <-time.After(15 * time.Millisecond):
			}
			stopped := make(chan struct{})
			go func() {
				p.Stop()
				p.AfterForward(wsForwardTicket{Step: wsStepNone}, nil)
				_ = p.Deadline()
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("metrics仍持状态锁")
			}
			if n := wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", nil); n != 0 {
				t.Fatal("记录先于差额", n)
			}
			unblock()
			inferenceWaitResult(t, forward, true)
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("Finish未完成")
			}
			if n := wsUsageMetricSum(t, reg, "inference_test_delta_total", nil); n != 6 {
				t.Fatal(n)
			}
			fidelity := "unavailable"
			if terminal {
				fidelity = "authoritative"
			}
			if n := wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", map[string]string{"fidelity": fidelity}); n != 1 {
				t.Fatal(n)
			}
		}
	})
}
