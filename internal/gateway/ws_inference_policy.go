package gateway

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	dsi "github.com/yobo2u/omugw/internal/protocol/dashscopeinference"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

type wsInferencePhase uint8

const (
	inferenceAwaiting wsInferencePhase = iota
	inferenceStartedDelivering
	inferenceActive
	inferenceDraining
	inferenceTerminalDelivering
	inferenceCompleted
	inferenceFailedClosing
	inferenceStopped
)

type wsInferenceTask struct {
	id         string
	generation uint64
	phase      wsInferencePhase
	modelHash  [sha256.Size]byte
	contract   dsi.ModelContract
	asr, out   bool
	started    bool
	usage      wsInferenceUsage
}

type wsInferencePolicy struct {
	// publication 只串行一次状态变更与其指标副作用；门闩等待从不持这两把锁。
	// Stop/After/Deadline 只取 mu，慢指标采集不能阻止交付取消。
	publication sync.Mutex
	mu          sync.Mutex
	tasks       map[string]*wsInferenceTask
	active      *wsInferenceTask
	model       string
	stopped     bool
	finished    bool
	gate        chan struct{}
	changed     chan struct{}
	deadline    wsPolicyDeadline
	binding     wsInferenceBinding
	router      *router.Router
	matrix      *degrade.Matrix
	metrics     *obs.Metrics
	timeouts    config.Timeouts
	now         func() time.Time
}

var _ wsForwardPolicy = (*wsInferencePolicy)(nil)

func newWSInferencePolicy(binding wsInferenceBinding, rt *router.Router, matrix *degrade.Matrix, metrics *obs.Metrics, timeouts config.Timeouts, now func() time.Time) *wsInferencePolicy {
	if now == nil {
		now = time.Now
	}
	return &wsInferencePolicy{binding: binding, router: rt, matrix: matrix, metrics: metrics, timeouts: timeouts, now: now, tasks: make(map[string]*wsInferenceTask), gate: make(chan struct{}), changed: make(chan struct{}, 1)}
}

func (p *wsInferencePolicy) BeforeForward(ctx context.Context, direction wsDirection, op ws.Opcode, raw []byte) (wsForwardDecision, error) {
	at := p.now()
	var client dsi.ClientFacts
	var server dsi.ServerFacts
	var id string
	var binding dsi.BindingFields
	var invalid error
	kind := dsi.LocalPolicy
	if op == ws.OpText {
		if direction == wsClientToUpstream {
			client, invalid = dsi.InspectClient(raw)
			id, binding = client.TaskID, client.Binding
			if invalid == nil && client.Action == "run-task" {
				if invalid = dsi.ValidateTaskContract(client); invalid != nil {
					kind = dsi.LocalUnsupportedTask
				}
			}
		} else {
			server, invalid = dsi.InspectServer(raw, dsi.ModelContract{})
			if invalid == nil {
				p.mu.Lock()
				r := p.tasks[server.TaskID]
				var contract dsi.ModelContract
				if r != nil {
					contract = r.contract
				}
				p.mu.Unlock()
				server, invalid = dsi.InspectServer(raw, contract)
			}
			id, binding = server.TaskID, server.Binding
		}
		if invalid != nil {
			id, _ = dsi.VerifiedTaskID(raw)
		} else if binding.Model.Presence == dsi.Value {
			// 历史 SHA 只核等值；每次重申仍必须重走路由与整门批准。
			invalid = validateWSInferenceModel(p.router, p.matrix, p.binding, binding.Model.Value)
		}
	} else if op != ws.OpBinary {
		invalid = errWSRelayPolicy
	}
	var waitingGeneration uint64
	for {
		if err := ctx.Err(); err != nil {
			return wsForwardDecision{}, err
		}
		p.publication.Lock()
		admittedAt := p.now()
		p.mu.Lock()
		if p.finished || p.stopped && !(direction == wsUpstreamToClient && p.active != nil && p.active.phase == inferenceFailedClosing) {
			p.mu.Unlock()
			p.publication.Unlock()
			return wsForwardDecision{}, errWSPolicyStopped
		}
		var d wsForwardDecision
		var o wsInferenceObservation
		var wait bool
		switch {
		case invalid != nil:
			d, o = p.rejectLocked(id, kind, at, "invalid_event")
		case waitingGeneration != 0 && (p.active == nil || p.active.generation != waitingGeneration):
			d, o = p.rejectLocked(id, dsi.LocalPolicy, at, "invalid_event")
		case op == ws.OpBinary:
			d, o, wait = p.binaryLocked(direction, at)
		case direction == wsClientToUpstream:
			d, o, wait = p.clientLocked(client, at, admittedAt)
		default:
			d, o = p.serverLocked(server, at)
		}
		gate := p.gate
		if wait && client.Action != "run-task" {
			waitingGeneration = p.active.generation
		}
		p.mu.Unlock()
		p.publish(o)
		p.publication.Unlock()
		if !wait {
			return d, nil
		}
		select {
		case <-ctx.Done():
			return wsForwardDecision{}, ctx.Err()
		case <-gate:
		}
	}
}

func (p *wsInferencePolicy) clientLocked(f dsi.ClientFacts, at, admittedAt time.Time) (wsForwardDecision, wsInferenceObservation, bool) {
	if f.Action == "run-task" {
		if p.tasks[f.TaskID] != nil || len(p.tasks) >= maxWSUsageRecords {
			diagnostic := "invalid_event"
			if len(p.tasks) >= maxWSUsageRecords {
				diagnostic = "ledger_limit"
			}
			d, o := p.rejectLocked(f.TaskID, dsi.LocalPolicy, at, diagnostic)
			return d, o, false
		}
		if p.active != nil {
			if p.active.phase == inferenceTerminalDelivering {
				return wsForwardDecision{}, wsInferenceObservation{}, true
			}
			d, o := p.rejectLocked(f.TaskID, dsi.LocalPolicy, at, "invalid_event")
			return d, o, false
		}
		r := &wsInferenceTask{id: f.TaskID, generation: uint64(len(p.tasks) + 1), phase: inferenceAwaiting, modelHash: sha256.Sum256([]byte(f.Binding.Model.Value)), contract: dsi.LookupModelContract(f.Binding.Model.Value), asr: f.Binding.Task.Value == "asr", out: f.Binding.Streaming.Value == "out"}
		p.tasks[r.id], p.active, p.model = r, r, f.Binding.Model.Value
		// 写前登记才能接住 run 的瞬回，After 不得把后来阶段倒回 awaiting。
		p.setDeadlineLocked(admittedAt.Add(p.timeouts.FirstByte))
		return inferenceTicket(r, wsStepRun), wsInferenceObservation{}, false
	}
	r := p.active
	if r == nil || r.id != f.TaskID || !p.matchesBinding(r, f.Binding) {
		d, o := p.rejectLocked(f.TaskID, dsi.LocalPolicy, at, "invalid_event")
		return d, o, false
	}
	switch f.Action {
	case "continue-task", "finish-task":
		if r.phase == inferenceStartedDelivering {
			return wsForwardDecision{}, wsInferenceObservation{}, true
		}
		if r.phase != inferenceActive || r.out {
			d, o := p.rejectLocked(f.TaskID, dsi.LocalPolicy, at, "invalid_event")
			return d, o, false
		}
		if f.Action == "finish-task" {
			r.phase = inferenceDraining
			p.setDeadlineLocked(admittedAt.Add(max(p.timeouts.FirstByte, p.timeouts.Idle)))
			return inferenceTicket(r, wsStepFinish), wsInferenceObservation{}, false
		}
	default:
		if r.phase != inferenceAwaiting && r.phase != inferenceActive && r.phase != inferenceDraining {
			d, o := p.rejectLocked(f.TaskID, dsi.LocalPolicy, at, "invalid_event")
			return d, o, false
		}
	}
	return wsForwardDecision{}, wsInferenceObservation{}, false
}

func (p *wsInferencePolicy) binaryLocked(direction wsDirection, at time.Time) (wsForwardDecision, wsInferenceObservation, bool) {
	r := p.active
	if r != nil {
		if direction == wsClientToUpstream && r.phase == inferenceStartedDelivering {
			return wsForwardDecision{}, wsInferenceObservation{}, true
		}
		if r.started && (direction == wsClientToUpstream && r.asr && r.phase == inferenceActive || direction == wsUpstreamToClient && !r.asr && (r.phase == inferenceActive || r.phase == inferenceDraining || r.phase == inferenceFailedClosing)) {
			return wsForwardDecision{}, wsInferenceObservation{}, false
		}
	}
	id := ""
	if r != nil {
		id = r.id
	}
	d, o := p.rejectLocked(id, dsi.LocalPolicy, at, "invalid_event")
	return d, o, false
}

func (p *wsInferencePolicy) serverLocked(f dsi.ServerFacts, at time.Time) (wsForwardDecision, wsInferenceObservation) {
	r := p.tasks[f.TaskID]
	if r == nil || p.stopped && r != p.active || !p.matchesBinding(r, f.Binding) {
		return p.rejectLocked(f.TaskID, dsi.LocalPolicy, at, "invalid_event")
	}
	terminal := f.Event == "task-finished" || f.Event == "task-failed"
	if r != p.active {
		if r.phase == inferenceCompleted && terminal {
			return wsForwardDecision{}, r.observeUsage(f)
		}
		return p.rejectLocked(f.TaskID, dsi.LocalPolicy, at, "invalid_event")
	}
	if r.phase == inferenceFailedClosing || r.phase == inferenceTerminalDelivering && terminal {
		// 失败后宽限只保全已绑定尾消息，不再推进状态或重复业务结账。
		return wsForwardDecision{}, r.observeUsage(f)
	}
	switch f.Event {
	case "task-started":
		if r.phase != inferenceAwaiting {
			return p.rejectLocked(f.TaskID, dsi.LocalPolicy, at, "invalid_event")
		}
		r.phase = inferenceStartedDelivering
		if r.out {
			p.setDeadlineLocked(at.Add(max(p.timeouts.FirstByte, p.timeouts.Idle)))
		}
		return inferenceTicket(r, wsStepStarted), wsInferenceObservation{}
	case "task-finished":
		if r.phase != inferenceActive && r.phase != inferenceDraining {
			return p.rejectLocked(f.TaskID, dsi.LocalPolicy, at, "invalid_event")
		}
		o := r.observeUsage(f)
		r.phase = inferenceTerminalDelivering
		p.setDeadlineLocked(time.Time{})
		return inferenceTicket(r, wsStepTerminal), o
	case "task-failed":
		o := r.observeUsage(f)
		o.failure = f.Failure
		r.phase = inferenceFailedClosing
		p.stopLocked()
		d := inferenceTicket(r, wsStepFailed)
		d.Action = wsForwardThenEnd
		d.End = &wsPolicyEnd{At: at, Failure: f.Failure, Code: ws.CloseInternalError, Reason: "upstream task failed", PeerGrace: true}
		return d, o
	case "result-generated":
		return wsForwardDecision{}, r.observeUsage(f)
	default:
		return wsForwardDecision{}, wsInferenceObservation{}
	}
}

func inferenceTicket(r *wsInferenceTask, step wsForwardStep) wsForwardDecision {
	return wsForwardDecision{Ticket: wsForwardTicket{Generation: r.generation, Step: step}}
}

func (p *wsInferencePolicy) matchesBinding(r *wsInferenceTask, b dsi.BindingFields) bool {
	streaming, task, function := "duplex", "tts", "SpeechSynthesizer"
	if r.out {
		streaming = "out"
	}
	if r.asr {
		task, function = "asr", "recognition"
	}
	for _, pair := range [...]struct {
		field    dsi.TextField
		expected string
	}{{b.Streaming, streaming}, {b.TaskGroup, "audio"}, {b.Task, task}, {b.Function, function}} {
		if pair.field.Presence != dsi.Missing && (pair.field.Presence != dsi.Value || pair.field.Value != pair.expected) {
			return false
		}
	}
	if b.Model.Presence == dsi.Missing {
		return true
	}
	if b.Model.Presence != dsi.Value {
		return false
	}
	if r == p.active {
		return b.Model.Value == p.model
	}
	return sha256.Sum256([]byte(b.Model.Value)) == r.modelHash
}

func (p *wsInferencePolicy) AfterForward(ticket wsForwardTicket, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.active
	if p.stopped || p.finished || r == nil || r.generation != ticket.Generation {
		return
	}
	valid := ticket.Step == wsStepRun && r.phase == inferenceAwaiting || ticket.Step == wsStepStarted && r.phase == inferenceStartedDelivering || ticket.Step == wsStepFinish && r.phase == inferenceDraining || ticket.Step == wsStepTerminal && r.phase == inferenceTerminalDelivering
	if !valid {
		return
	}
	if err != nil {
		p.stopLocked()
		return
	}
	switch ticket.Step {
	case wsStepStarted:
		r.started = true
		if r.out {
			r.phase = inferenceDraining
		} else {
			r.phase = inferenceActive
			p.setDeadlineLocked(time.Time{})
		}
		p.wakeLocked()
	case wsStepTerminal:
		r.phase = inferenceCompleted
		p.active, p.model = nil, ""
		p.wakeLocked()
	}
}

func (p *wsInferencePolicy) Deadline() wsPolicyDeadline {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.deadline
}

func (p *wsInferencePolicy) Changed() <-chan struct{} { return p.changed }

func (p *wsInferencePolicy) setDeadlineLocked(at time.Time) {
	p.deadline = wsPolicyDeadline{At: at, Revision: p.deadline.Revision + 1}
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

func (p *wsInferencePolicy) Expire(deadline wsPolicyDeadline, now time.Time) *wsPolicyEnd {
	p.publication.Lock()
	defer p.publication.Unlock()
	p.mu.Lock()
	if p.stopped || p.finished || p.active == nil || deadline.At.IsZero() || deadline != p.deadline || now.Before(deadline.At) {
		p.mu.Unlock()
		return nil
	}
	kind, diagnostic := dsi.LocalStartTimeout, "task_start_timeout"
	if p.active.phase == inferenceDraining || p.active.out && p.active.phase == inferenceStartedDelivering {
		kind, diagnostic = dsi.LocalDrainTimeout, "task_drain_timeout"
	}
	end := &wsPolicyEnd{At: now, Failure: &canonical.Error{Class: canonical.ClassUpstreamUnavailable, Message: diagnostic}, Code: ws.CloseInternalError, Reason: diagnostic, Local: &wsLocalTaskFailure{TaskID: p.active.id, Kind: kind}}
	p.stopLocked()
	p.mu.Unlock()
	p.publish(wsInferenceObservation{diagnostic: diagnostic})
	return end
}

func (p *wsInferencePolicy) rejectLocked(id string, kind dsi.LocalFailureKind, at time.Time, diagnostic string) (wsForwardDecision, wsInferenceObservation) {
	end := &wsPolicyEnd{At: at, Failure: errWSRelayPolicy, Code: 1008, Reason: "invalid task envelope or binding"}
	if id != "" {
		end.Local = &wsLocalTaskFailure{TaskID: id, Kind: kind}
	}
	p.stopLocked()
	return wsForwardDecision{Action: wsRejectThenEnd, End: end}, wsInferenceObservation{diagnostic: diagnostic}
}

func (p *wsInferencePolicy) wakeLocked() {
	close(p.gate)
	p.gate = make(chan struct{})
}

func (p *wsInferencePolicy) stopLocked() {
	if p.stopped {
		return
	}
	p.stopped = true
	if p.active != nil && p.active.phase != inferenceFailedClosing {
		p.active.phase = inferenceStopped
	}
	p.setDeadlineLocked(time.Time{})
	p.wakeLocked()
}

func (p *wsInferencePolicy) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
}

func (p *wsInferencePolicy) Finish() {
	p.publication.Lock()
	defer p.publication.Unlock()
	p.mu.Lock()
	if p.finished {
		p.mu.Unlock()
		return
	}
	p.finished = true
	p.stopLocked()
	var o wsInferenceObservation
	// 串行任务只有 active 可能未结；历史终态在释放 active 前已经结算。
	if p.active != nil {
		o = p.active.finishUsage()
	}
	p.active, p.model = nil, ""
	p.mu.Unlock()
	p.publish(o)
}
