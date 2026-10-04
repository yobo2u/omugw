package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	dsi "github.com/yobo2u/omugw/internal/protocol/dashscopeinference"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

const inferenceASR = "qwen-audio-3.0-asr-flash-streaming"
const inferenceTTS = "cosyvoice-v2"

func inferenceTarget() router.Target {
	return router.Target{Kind: degrade.ProviderDashScopeWSInference, Endpoint: "inference", BaseURL: "https://upstream.invalid/prefix", CredentialPool: "pool"}
}

func inferenceMatrix(t *testing.T, full bool) *degrade.Matrix {
	t.Helper()
	caps := degrade.ExpressibleSet(degrade.ProtoDashScopeInference)
	redeemed := caps
	if !full {
		redeemed = caps[:1]
	}
	m := degrade.NewMatrix()
	if err := m.Add(degrade.NewRoute(degrade.ProtoDashScopeInference, degrade.ProviderDashScopeWSInference).
		Pass(caps...).Redeem(degrade.EndpointDashScopeInference, redeemed...).MarkHomogeneous().Build()); err != nil {
		t.Fatal(err)
	}
	return m
}

func inferenceRouter(t *testing.T) *router.Router {
	t.Helper()
	var rules []router.Rule
	for _, model := range []string{inferenceASR, inferenceTTS, "sambert-zhichu-v1", "qwen-audio-3.1-asr-flash-streaming", "paraformer-realtime-v2", "qwen-audio-3.1-tts-flash", "future-audio", "multimodal-dialog", "tingwu-meeting-realtime"} {
		target := inferenceTarget()
		target.UpstreamModel = model
		rules = append(rules, router.Rule{Match: model, Targets: []router.Target{target}})
	}
	rt, err := router.New(rules)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func inferencePolicy(t *testing.T) (*wsInferencePolicy, *prometheus.Registry, *time.Time) {
	t.Helper()
	binding, err := newWSInferenceBinding(inferenceTarget())
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	now := time.Unix(1700000000, 0)
	p := newWSInferencePolicy(binding, inferenceRouter(t), inferenceMatrix(t, true), obs.NewMetrics(reg), config.Timeouts{Connect: time.Second, FirstByte: 2 * time.Second, Idle: 5 * time.Second, Total: 10 * time.Second}, func() time.Time { return now })
	return p, reg, &now
}

func inferenceRun(id, model, task, streaming string) []byte {
	function := "recognition"
	if task == "tts" {
		function = "SpeechSynthesizer"
	}
	return []byte(fmt.Sprintf(`{"header":{"action":"run-task","task_id":%q,"streaming":%q},"payload":{"model":%q,"task_group":"audio","task":%q,"function":%q,"input":{}}}`, id, streaming, model, task, function))
}

func inferenceClient(id, action, payload string) []byte {
	return []byte(fmt.Sprintf(`{"header":{"action":%q,"task_id":%q},"payload":%s}`, action, id, payload))
}

func inferenceServer(id, event, payload string) []byte {
	return []byte(fmt.Sprintf(`{"header":{"event":%q,"task_id":%q},"payload":%s}`, event, id, payload))
}

func inferenceBefore(t *testing.T, p *wsInferencePolicy, dir wsDirection, op ws.Opcode, raw []byte) wsForwardDecision {
	t.Helper()
	copy := bytes.Clone(raw)
	d, err := p.BeforeForward(context.Background(), dir, op, raw)
	if err != nil || d.Action != wsForward {
		t.Fatalf("未保全: decision=%+v err=%v", d, err)
	}
	if !bytes.Equal(raw, copy) {
		t.Fatal("改写负载")
	}
	return d
}

func inferenceSend(t *testing.T, p *wsInferencePolicy, dir wsDirection, raw []byte) wsForwardDecision {
	t.Helper()
	d := inferenceBefore(t, p, dir, ws.OpText, raw)
	p.AfterForward(d.Ticket, nil)
	return d
}

func inferenceStart(t *testing.T, p *wsInferencePolicy, id, model, task, streaming string) {
	t.Helper()
	inferenceSend(t, p, wsClientToUpstream, inferenceRun(id, model, task, streaming))
	inferenceSend(t, p, wsUpstreamToClient, inferenceServer(id, "task-started", `{}`))
}

func inferenceReject(t *testing.T, p *wsInferencePolicy, dir wsDirection, op ws.Opcode, raw []byte) {
	t.Helper()
	d, err := p.BeforeForward(context.Background(), dir, op, raw)
	if err == nil && (d.Action != wsRejectThenEnd || d.End == nil || d.End.Code != 1008) {
		t.Fatalf("错误准入: %+v", d)
	}
}

type inferenceResult struct {
	decision wsForwardDecision
	err      error
}

func inferenceWaiting(t *testing.T, p *wsInferencePolicy, raw []byte, op ws.Opcode) <-chan inferenceResult {
	t.Helper()
	ch := make(chan inferenceResult, 1)
	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		close(entered)
		d, err := p.BeforeForward(ctx, wsClientToUpstream, op, raw)
		ch <- inferenceResult{d, err}
	}()
	<-entered
	select {
	case r := <-ch:
		t.Fatalf("交付前越过门闩: %+v", r)
	case <-time.After(15 * time.Millisecond):
	}
	return ch
}

func inferenceWaitResult(t *testing.T, ch <-chan inferenceResult, forward bool) wsForwardDecision {
	t.Helper()
	select {
	case r := <-ch:
		if forward != (r.err == nil && r.decision.Action == wsForward) {
			t.Fatalf("门闩结果: %+v", r)
		}
		return r.decision
	case <-time.After(time.Second):
		t.Fatal("门闩未解除")
		return wsForwardDecision{}
	}
}

func TestInferencePolicyTaskTransitions(t *testing.T) {
	t.Run("预登记瞬回及终态交付门闩", func(t *testing.T) {
		p, _, _ := inferencePolicy(t)
		run := inferenceBefore(t, p, wsClientToUpstream, ws.OpText, inferenceRun("A", inferenceASR, "asr", "duplex"))
		started := inferenceBefore(t, p, wsUpstreamToClient, ws.OpText, inferenceServer("A", "task-started", `{}`))
		waiting := inferenceWaiting(t, p, []byte{1, 2, 3}, ws.OpBinary)
		p.AfterForward(started.Ticket, nil)
		binary := inferenceWaitResult(t, waiting, true)
		p.AfterForward(binary.Ticket, nil)
		p.AfterForward(run.Ticket, nil)
		finish := inferenceBefore(t, p, wsClientToUpstream, ws.OpText, inferenceClient("A", "finish-task", `{"input":{"cancel":true}}`))
		terminal := inferenceBefore(t, p, wsUpstreamToClient, ws.OpText, inferenceServer("A", "task-finished", `{"usage":{"duration":4}}`))
		next := inferenceWaiting(t, p, inferenceRun("B", inferenceTTS, "tts", "duplex"), ws.OpText)
		p.AfterForward(terminal.Ticket, nil)
		b := inferenceWaitResult(t, next, true)
		p.AfterForward(b.Ticket, nil)
		p.AfterForward(finish.Ticket, nil)
		inferenceSend(t, p, wsUpstreamToClient, inferenceServer("B", "task-started", `{}`))
		inferenceBefore(t, p, wsUpstreamToClient, ws.OpBinary, []byte{4, 5})
	})
	t.Run("started交付失败唤醒但不放行", func(t *testing.T) {
		p, _, _ := inferencePolicy(t)
		inferenceSend(t, p, wsClientToUpstream, inferenceRun("A", inferenceASR, "asr", "duplex"))
		s := inferenceBefore(t, p, wsUpstreamToClient, ws.OpText, inferenceServer("A", "task-started", `{}`))
		ch := inferenceWaiting(t, p, []byte{1}, ws.OpBinary)
		p.AfterForward(s.Ticket, errors.New("write failed"))
		inferenceWaitResult(t, ch, false)
		p.AfterForward(s.Ticket, nil)
		inferenceReject(t, p, wsClientToUpstream, ws.OpBinary, []byte{1})
	})
	for _, tc := range []struct {
		name, model, task, streaming string
		dir                          wsDirection
		action                       string
	}{
		{"ASR拒下行binary", inferenceASR, "asr", "duplex", wsUpstreamToClient, ""},
		{"TTS拒上行binary", inferenceTTS, "tts", "duplex", wsClientToUpstream, ""},
		{"out拒finish", "sambert-zhichu-v1", "tts", "out", wsClientToUpstream, "finish-task"},
		{"out拒continue", "sambert-zhichu-v1", "tts", "out", wsClientToUpstream, "continue-task"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, _ := inferencePolicy(t)
			inferenceStart(t, p, "A", tc.model, tc.task, tc.streaming)
			raw, op := []byte{1}, ws.OpBinary
			if tc.action != "" {
				raw, op = inferenceClient("A", tc.action, `{}`), ws.OpText
			}
			inferenceReject(t, p, tc.dir, op, raw)
		})
	}
	t.Run("out无需finish且保全尾音", func(t *testing.T) {
		p, _, _ := inferencePolicy(t)
		inferenceStart(t, p, "A", "sambert-zhichu-v1", "tts", "out")
		inferenceBefore(t, p, wsUpstreamToClient, ws.OpBinary, []byte{1})
		inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "task-finished", `{}`))
		inferenceStart(t, p, "B", inferenceASR, "asr", "duplex")
	})
	for _, action := range []string{"binary", "continue-task", "finish-task"} {
		t.Run("draining拒输入/"+action, func(t *testing.T) {
			p, _, _ := inferencePolicy(t)
			inferenceStart(t, p, "A", inferenceTTS, "tts", "duplex")
			inferenceSend(t, p, wsClientToUpstream, inferenceClient("A", "finish-task", `{}`))
			inferenceBefore(t, p, wsUpstreamToClient, ws.OpBinary, []byte{1})
			raw, op := inferenceClient("A", action, `{}`), ws.OpText
			if action == "binary" {
				raw, op = []byte{1}, ws.OpBinary
			}
			inferenceReject(t, p, wsClientToUpstream, op, raw)
		})
	}
}

func TestInferencePolicyDeliveryGenerations(t *testing.T) {
	t.Run("旧generation和空Step不能打开新started门闩", func(t *testing.T) {
		p, _, _ := inferencePolicy(t)
		inferenceStart(t, p, "A", inferenceASR, "asr", "duplex")
		old := inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "task-finished", `{}`))
		inferenceSend(t, p, wsClientToUpstream, inferenceRun("B", inferenceASR, "asr", "duplex"))
		started := inferenceBefore(t, p, wsUpstreamToClient, ws.OpText, inferenceServer("B", "task-started", `{}`))
		ch := inferenceWaiting(t, p, []byte{1}, ws.OpBinary)
		p.AfterForward(wsForwardTicket{Generation: old.Ticket.Generation, Step: wsStepStarted}, nil)
		p.AfterForward(wsForwardTicket{Generation: started.Ticket.Generation, Step: wsStepNone}, nil)
		p.AfterForward(wsForwardTicket{Generation: started.Ticket.Generation, Step: wsStepTerminal}, nil)
		select {
		case r := <-ch:
			t.Fatalf("过时/错误Step回执放行: %+v", r)
		case <-time.After(15 * time.Millisecond):
		}
		p.AfterForward(started.Ticket, nil)
		inferenceWaitResult(t, ch, true)
	})
	t.Run("历史terminal和failed不结束新任务", func(t *testing.T) {
		p, reg, _ := inferencePolicy(t)
		inferenceStart(t, p, "A", inferenceASR, "asr", "duplex")
		old := inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "task-finished", `{"usage":{"duration":6}}`))
		inferenceStart(t, p, "B", inferenceTTS, "tts", "duplex")
		deadline := p.Deadline()
		for _, event := range []string{"task-finished", "task-failed"} {
			inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", event, `{"usage":{"duration":6}}`))
		}
		p.AfterForward(old.Ticket, nil)
		if p.Deadline() != deadline {
			t.Fatal("历史消息修改新任务期限")
		}
		inferenceSend(t, p, wsClientToUpstream, inferenceClient("B", "continue-task", `{}`))
		if n := wsUsageMetricSum(t, reg, "omugw_upstream_errors_total", nil); n != 0 {
			t.Fatal("历史failed重记业务失败", n)
		}
	})
	t.Run("同代过期started回执不可复活", func(t *testing.T) {
		p, _, now := inferencePolicy(t)
		inferenceSend(t, p, wsClientToUpstream, inferenceRun("A", inferenceASR, "asr", "duplex"))
		s := inferenceBefore(t, p, wsUpstreamToClient, ws.OpText, inferenceServer("A", "task-started", `{}`))
		d := p.Deadline()
		*now = d.At
		if p.Expire(d, *now) == nil {
			t.Fatal("交付预算被清空")
		}
		p.AfterForward(s.Ticket, nil)
		inferenceReject(t, p, wsClientToUpstream, ws.OpBinary, []byte{1})
	})
	t.Run("Step与phase双核验", func(t *testing.T) {
		p, _, _ := inferencePolicy(t)
		r := inferenceSend(t, p, wsClientToUpstream, inferenceRun("A", inferenceASR, "asr", "duplex"))
		p.AfterForward(wsForwardTicket{Generation: r.Ticket.Generation, Step: wsStepStarted}, nil)
		inferenceReject(t, p, wsClientToUpstream, ws.OpBinary, []byte{1})
	})
	t.Run("Stop解Before与After并封客户端", func(t *testing.T) {
		for _, terminal := range []bool{false, true} {
			p, _, _ := inferencePolicy(t)
			inferenceSend(t, p, wsClientToUpstream, inferenceRun("A", inferenceASR, "asr", "duplex"))
			s := inferenceBefore(t, p, wsUpstreamToClient, ws.OpText, inferenceServer("A", "task-started", `{}`))
			raw, op := []byte{1}, ws.OpBinary
			if terminal {
				p.AfterForward(s.Ticket, nil)
				s = inferenceBefore(t, p, wsUpstreamToClient, ws.OpText, inferenceServer("A", "task-finished", `{}`))
				raw, op = inferenceRun("B", inferenceASR, "asr", "duplex"), ws.OpText
			}
			ch := inferenceWaiting(t, p, raw, op)
			p.Stop()
			p.Stop()
			inferenceWaitResult(t, ch, false)
			done := make(chan struct{})
			go func() { p.AfterForward(s.Ticket, nil); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("Stop后After挂住")
			}
			inferenceReject(t, p, wsClientToUpstream, ws.OpText, inferenceRun("B", inferenceASR, "asr", "duplex"))
		}
	})
	t.Run("failed宽限尾音保全且不重结", func(t *testing.T) {
		p, reg, _ := inferencePolicy(t)
		inferenceStart(t, p, "A", inferenceTTS, "tts", "duplex")
		d, err := p.BeforeForward(context.Background(), wsUpstreamToClient, ws.OpText, inferenceServer("A", "task-failed", `{"usage":{"characters":6}}`))
		if err != nil || d.Action != wsForwardThenEnd || d.End == nil || !d.End.PeerGrace {
			t.Fatalf("failed=%+v %v", d, err)
		}
		if wsUsageMetricSum(t, reg, "omugw_ws_characters_total", nil) != 6 || wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", nil) != 1 {
			t.Fatal("failed未在原文交付前结算")
		}
		p.Stop()
		p.AfterForward(d.Ticket, nil)
		inferenceBefore(t, p, wsUpstreamToClient, ws.OpBinary, []byte{0, 255})
		inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "future-tail", `{"model":"cosyvoice-v2"}`))
		inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "task-failed", `{"usage":{"characters":99}}`))
		p.Finish()
		p.Finish()
		if n := wsUsageMetricSum(t, reg, "omugw_ws_characters_total", nil); n != 6 {
			t.Fatal(n)
		}
		if n := wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", nil); n != 1 {
			t.Fatal(n)
		}
		if n := wsUsageMetricSum(t, reg, "omugw_upstream_errors_total", map[string]string{"class": "internal", "retryable": "false"}); n != 1 {
			t.Fatal(n)
		}
	})
	t.Run("failed在awaiting可接收但不授权尾binary", func(t *testing.T) {
		p, _, _ := inferencePolicy(t)
		inferenceSend(t, p, wsClientToUpstream, inferenceRun("A", inferenceTTS, "tts", "duplex"))
		d, err := p.BeforeForward(context.Background(), wsUpstreamToClient, ws.OpText, inferenceServer("A", "task-failed", `{}`))
		if err != nil || d.Action != wsForwardThenEnd {
			t.Fatalf("%+v %v", d, err)
		}
		p.Stop()
		inferenceReject(t, p, wsUpstreamToClient, ws.OpBinary, []byte{1})
	})
	t.Run("failed宽限仅保全当前任务尾消息", func(t *testing.T) {
		p, _, _ := inferencePolicy(t)
		inferenceStart(t, p, "A", inferenceASR, "asr", "duplex")
		inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "task-finished", `{}`))
		inferenceStart(t, p, "B", inferenceTTS, "tts", "duplex")
		d, err := p.BeforeForward(context.Background(), wsUpstreamToClient, ws.OpText, inferenceServer("B", "task-failed", `{}`))
		if err != nil || d.Action != wsForwardThenEnd {
			t.Fatalf("%+v %v", d, err)
		}
		p.Stop()
		inferenceReject(t, p, wsUpstreamToClient, ws.OpText, inferenceServer("A", "task-finished", `{}`))
	})
}

func TestInferencePolicyEveryModelRechecksWholeGate(t *testing.T) {
	for _, event := range []string{"client", "server", "historical-finished", "historical-failed"} {
		t.Run(event, func(t *testing.T) {
			p, _, _ := inferencePolicy(t)
			inferenceStart(t, p, "A", inferenceASR, "asr", "duplex")
			if strings.HasPrefix(event, "historical") {
				inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "task-finished", `{}`))
				inferenceStart(t, p, "B", inferenceTTS, "tts", "duplex")
			}
			payload := fmt.Sprintf(`{"model":%q}`, inferenceASR)
			raw, dir := inferenceServer("A", "future", payload), wsUpstreamToClient
			switch event {
			case "client":
				raw, dir = inferenceClient("A", "future", payload), wsClientToUpstream
			case "historical-finished":
				raw = inferenceServer("A", "task-finished", payload)
			case "historical-failed":
				raw = inferenceServer("A", "task-failed", payload)
			}
			inferenceSend(t, p, dir, raw)
			p.matrix = inferenceMatrix(t, false)
			inferenceReject(t, p, dir, ws.OpText, raw)
		})
	}
	for _, field := range []string{"endpoint", "url", "pool", "alias"} {
		t.Run("run写前拒绝/"+field, func(t *testing.T) {
			p, _, _ := inferencePolicy(t)
			targets, _ := p.router.Resolve(inferenceASR)
			switch field {
			case "endpoint":
				targets[0].Endpoint = "other"
			case "url":
				targets[0].BaseURL += "/other"
			case "pool":
				targets[0].CredentialPool = "other"
			case "alias":
				targets[0].UpstreamModel = "other"
			}
			inferenceReject(t, p, wsClientToUpstream, ws.OpText, inferenceRun("A", inferenceASR, "asr", "duplex"))
		})
	}
}

func TestInferencePolicyUnknownAndBinding(t *testing.T) {
	for _, stage := range []string{"awaiting", "active", "draining"} {
		t.Run("未知扩展保全/"+stage, func(t *testing.T) {
			p, _, _ := inferencePolicy(t)
			raw := inferenceRun("A", inferenceTTS, "tts", "duplex")
			inferenceSend(t, p, wsClientToUpstream, raw)
			for i := range raw {
				raw[i] = 'x'
			}
			if stage != "awaiting" {
				inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "task-started", `{}`))
			}
			if stage == "draining" {
				inferenceSend(t, p, wsClientToUpstream, inferenceClient("A", "finish-task", `{}`))
			}
			d := p.Deadline()
			for _, payload := range []string{`{"extension":{"text":"原样 ","array":[null,7]}}`, `{"model":"cosyvoice-v2","task_group":"audio","task":"tts","function":"SpeechSynthesizer","未知":true}`} {
				inferenceSend(t, p, wsClientToUpstream, inferenceClient("A", "future-action", payload))
				inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "future-event", payload))
			}
			if p.Deadline() != d {
				t.Fatal("未知消息续期")
			}
			if stage == "awaiting" {
				inferenceReject(t, p, wsClientToUpstream, ws.OpBinary, []byte{1})
			}
		})
	}
	for _, tc := range []struct {
		name string
		dir  wsDirection
		raw  []byte
		idle bool
	}{
		{"idle未知动作", wsClientToUpstream, inferenceClient("A", "future", `{}`), true},
		{"双run", wsClientToUpstream, inferenceRun("B", inferenceASR, "asr", "duplex"), false},
		{"异ID", wsClientToUpstream, inferenceClient("B", "future", `{}`), false},
		{"异model", wsClientToUpstream, inferenceClient("A", "future", `{"model":"cosyvoice-v2"}`), false},
		{"Null", wsClientToUpstream, inferenceClient("A", "future", `{"model":null}`), false},
		{"服务端Null", wsUpstreamToClient, inferenceServer("A", "future", `{"model":null}`), false},
		{"异streaming", wsUpstreamToClient, []byte(`{"header":{"event":"future","task_id":"A","streaming":"out"},"payload":{}}`), false},
		{"异三元组", wsUpstreamToClient, inferenceServer("A", "future", `{"task":"tts"}`), false},
		{"未知ID终态", wsUpstreamToClient, inferenceServer("B", "task-finished", `{}`), false},
		{"重复started", wsUpstreamToClient, inferenceServer("A", "task-started", `{}`), false},
		{"multimodal-dialog", wsClientToUpstream, inferenceRun("A", "multimodal-dialog", "asr", "duplex"), true},
		{"听悟", wsClientToUpstream, inferenceRun("A", "tingwu-meeting-realtime", "asr", "duplex"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, _ := inferencePolicy(t)
			if !tc.idle {
				inferenceStart(t, p, "A", inferenceASR, "asr", "duplex")
			}
			inferenceReject(t, p, tc.dir, ws.OpText, tc.raw)
		})
	}
	for _, historical := range []bool{false, true} {
		for _, server := range []bool{false, true} {
			t.Run(fmt.Sprintf("每次model重申重核路由/%t/%t", historical, server), func(t *testing.T) {
				p, _, _ := inferencePolicy(t)
				inferenceStart(t, p, "A", inferenceASR, "asr", "duplex")
				if historical {
					inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "task-finished", `{}`))
					inferenceStart(t, p, "B", inferenceTTS, "tts", "duplex")
				}
				target := inferenceTarget()
				target.UpstreamModel = inferenceASR
				target.CredentialPool = "different"
				// Router 返回其配置候选；测试在无并发时替换该候选，证明摘要不能替代再次 Resolve。
				targets, _ := p.router.Resolve(inferenceASR)
				targets[0] = target
				raw, dir := inferenceClient("A", "future", fmt.Sprintf(`{"model":%q}`, inferenceASR)), wsClientToUpstream
				if server {
					event := "future"
					if historical {
						event = "task-finished"
					}
					raw, dir = inferenceServer("A", event, fmt.Sprintf(`{"model":%q}`, inferenceASR)), wsUpstreamToClient
				}
				inferenceReject(t, p, dir, ws.OpText, raw)
			})
		}
	}
	t.Run("未知事件不冒充terminal", func(t *testing.T) {
		p, _, _ := inferencePolicy(t)
		inferenceStart(t, p, "A", inferenceASR, "asr", "duplex")
		inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "future", `{"usage":{"duration":7}}`))
		inferenceReject(t, p, wsClientToUpstream, ws.OpText, inferenceRun("B", inferenceASR, "asr", "duplex"))
	})
}

func TestInferencePolicyDeadlinesAndCapacity(t *testing.T) {
	t.Run("真实传输心跳与结果不续期", func(t *testing.T) {
		p, _, _ := inferencePolicy(t)
		inferenceStart(t, p, "A", inferenceASR, "asr", "duplex")
		inferenceSend(t, p, wsClientToUpstream, inferenceClient("A", "finish-task", `{}`))
		deadline := p.Deadline()
		budget := wsTestBudget(t, 1<<20)
		up, peer := wsTestLink(t, true, budget, 1<<16, 0, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			m, err := up.ReadOwnedMessage(ctx)
			if err == nil {
				var decision wsForwardDecision
				decision, err = p.BeforeForward(ctx, wsUpstreamToClient, m.Opcode, m.Payload)
				if err == nil && decision.Action != wsForward {
					err = errWSRelayPolicy
				}
				p.AfterForward(decision.Ticket, err)
				m.Release()
			}
			done <- err
		}()
		// 心跳故意携带像终态的正文；只有真实应用消息才能进入 policy。
		ping := inferenceServer("A", "task-finished", `{}`)
		peer.send(t, ws.OpPing, ping)
		pong := peer.read(t)
		if pong.Opcode != ws.OpPong || !bytes.Equal(pong.Payload, ping) {
			t.Fatal("未收到原样pong")
		}
		if p.Deadline() != deadline {
			t.Fatal("ping刷新阶段预算")
		}
		peer.send(t, ws.OpText, inferenceServer("A", "result-generated", `{"usage":{"duration":6}}`))
		if err := receiveWSTest(t, done); err != nil {
			t.Fatal(err)
		}
		if p.Deadline() != deadline {
			t.Fatal("结果刷新阶段预算")
		}
		if p.Expire(deadline, deadline.At) == nil {
			t.Fatal("心跳替代了业务终态")
		}
	})
	t.Run("同一绝对时间的旧revision不能击中新任务", func(t *testing.T) {
		p, _, _ := inferencePolicy(t)
		inferenceSend(t, p, wsClientToUpstream, inferenceRun("A", inferenceASR, "asr", "duplex"))
		old := p.Deadline()
		inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "task-started", `{}`))
		inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "task-finished", `{}`))
		inferenceSend(t, p, wsClientToUpstream, inferenceRun("B", inferenceASR, "asr", "duplex"))
		current := p.Deadline()
		if current.At != old.At || current.Revision == old.Revision {
			t.Fatal("测试未构成同时间异revision竞争")
		}
		if p.Expire(old, old.At) != nil {
			t.Fatal("旧timer击中新任务")
		}
		if p.Expire(current, current.At) == nil {
			t.Fatal("当前timer失效")
		}
	})
	t.Run("等待后预登记才启动新预算", func(t *testing.T) {
		for _, run := range []bool{true, false} {
			p, _, now := inferencePolicy(t)
			base := *now
			var nanos atomic.Int64
			nanos.Store(base.UnixNano())
			p.now = func() time.Time { return time.Unix(0, nanos.Load()) }
			inferenceSend(t, p, wsClientToUpstream, inferenceRun("A", inferenceASR, "asr", "duplex"))
			d := inferenceBefore(t, p, wsUpstreamToClient, ws.OpText, inferenceServer("A", "task-started", `{}`))
			raw := inferenceClient("A", "finish-task", `{}`)
			budget := 5 * time.Second
			if run {
				p.AfterForward(d.Ticket, nil)
				d = inferenceBefore(t, p, wsUpstreamToClient, ws.OpText, inferenceServer("A", "task-finished", `{}`))
				raw, budget = inferenceRun("B", inferenceASR, "asr", "duplex"), 2*time.Second
			}
			ch := inferenceWaiting(t, p, raw, ws.OpText)
			nanos.Store(base.Add(time.Second).UnixNano())
			p.AfterForward(d.Ticket, nil)
			inferenceWaitResult(t, ch, true)
			if got := p.Deadline().At; !got.Equal(base.Add(time.Second + budget)) {
				t.Fatalf("等待消耗下一阶段预算: run=%v got=%v want=%v", run, got, base.Add(time.Second+budget))
			}
		}
	})
	t.Run("绝对阶段期限与revision", func(t *testing.T) {
		p, reg, now := inferencePolicy(t)
		base := *now
		inferenceSend(t, p, wsClientToUpstream, inferenceRun("A", inferenceASR, "asr", "duplex"))
		start := p.Deadline()
		if start.At != base.Add(2*time.Second) {
			t.Fatal(start)
		}
		*now = base.Add(time.Second)
		inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "result-generated", `{"usage":{"duration":1}}`))
		if p.Deadline() != start {
			t.Fatal("结果续期")
		}
		inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "task-started", `{}`))
		if !p.Deadline().At.IsZero() {
			t.Fatal("active套用HTTP总期限")
		}
		if p.Expire(start, base.Add(100*time.Second)) != nil {
			t.Fatal("旧revision生效")
		}
		inferenceSend(t, p, wsClientToUpstream, inferenceClient("A", "finish-task", `{}`))
		drain := p.Deadline()
		if drain.At != now.Add(5*time.Second) || drain.Revision == start.Revision {
			t.Fatal(drain)
		}
		if p.Expire(drain, drain.At.Add(-time.Nanosecond)) != nil {
			t.Fatal("提前过期")
		}
		end := p.Expire(drain, drain.At)
		if end == nil || end.Code != 1011 || end.Local.Kind != dsi.LocalDrainTimeout || end.At != drain.At {
			t.Fatal(end)
		}
		var ce *canonical.Error
		if !errors.As(end.Failure, &ce) || ce.Class != canonical.ClassUpstreamUnavailable || ce.Retryable || strings.Contains(ce.Error(), "A") {
			t.Fatal(end.Failure)
		}
		if p.Expire(drain, drain.At) != nil {
			t.Fatal("重复过期")
		}
		p.Finish()
		if wsUsageMetricSum(t, reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "task_drain_timeout"}) != 1 {
			t.Fatal("超时诊断未进白名单")
		}
	})
	t.Run("start超时", func(t *testing.T) {
		p, _, _ := inferencePolicy(t)
		inferenceSend(t, p, wsClientToUpstream, inferenceRun("A", inferenceASR, "asr", "duplex"))
		d := p.Deadline()
		end := p.Expire(d, d.At)
		if end == nil || end.Local.Kind != dsi.LocalStartTimeout || end.Code != 1011 {
			t.Fatal(end)
		}
	})
	t.Run("out从started到达而非交付起算", func(t *testing.T) {
		p, _, now := inferencePolicy(t)
		inferenceSend(t, p, wsClientToUpstream, inferenceRun("A", "sambert-zhichu-v1", "tts", "out"))
		*now = now.Add(time.Second)
		s := inferenceBefore(t, p, wsUpstreamToClient, ws.OpText, inferenceServer("A", "task-started", `{}`))
		d := p.Deadline()
		if d.At != now.Add(5*time.Second) {
			t.Fatal(d)
		}
		*now = now.Add(time.Second)
		p.AfterForward(s.Ticket, nil)
		inferenceBefore(t, p, wsUpstreamToClient, ws.OpBinary, []byte{1})
		if p.Deadline() != d {
			t.Fatal("交付或尾音续期")
		}
	})
	t.Run("4096含已结且4097写前拒绝", func(t *testing.T) {
		p, _, _ := inferencePolicy(t)
		for i := 0; i < 4096; i++ {
			id := fmt.Sprint(i)
			inferenceStart(t, p, id, inferenceASR, "asr", "duplex")
			inferenceSend(t, p, wsUpstreamToClient, inferenceServer(id, "task-finished", `{}`))
		}
		inferenceReject(t, p, wsClientToUpstream, ws.OpText, inferenceRun("4096", inferenceASR, "asr", "duplex"))
	})
	t.Run("重复run不可回收ID", func(t *testing.T) {
		p, _, _ := inferencePolicy(t)
		inferenceStart(t, p, "A", inferenceASR, "asr", "duplex")
		inferenceSend(t, p, wsUpstreamToClient, inferenceServer("A", "task-finished", `{}`))
		inferenceReject(t, p, wsClientToUpstream, ws.OpText, inferenceRun("A", inferenceASR, "asr", "duplex"))
	})
}
