package openairesponses

import (
	"encoding/json"
	"strings"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/transport/sse"
)

// Responses 的流式事件名。
const (
	evCreated    = "response.created"
	evInProgress = "response.in_progress"
	evCompleted  = "response.completed"
	evFailed     = "response.failed"
	evIncomplete = "response.incomplete"
	evError      = "error"

	evItemAdded = "response.output_item.added"
	evItemDone  = "response.output_item.done"

	evPartAdded = "response.content_part.added"
	evPartDone  = "response.content_part.done"

	evTextDelta = "response.output_text.delta"
	evTextDone  = "response.output_text.done"

	evArgsDelta = "response.function_call_arguments.delta"
	evArgsDone  = "response.function_call_arguments.done"

	evSummaryDelta = "response.reasoning_summary_text.delta"
	evSummaryDone  = "response.reasoning_summary_text.done"
)

// StreamEncoder 把 Canonical 事件流编成 Responses 事件流。
//
// 它必须是有状态的：Responses 的事件带 output_index / content_index / item_id
// 三层编号，而 Canonical 只有一个块序号。少了这层状态，就没法在
// response.output_item.done 里回填那一项的完整内容，也没法在
// response.completed 里附上整个 output 数组——而官方 SDK 正是靠后者拼出最终
// 结果的。
type StreamEncoder struct {
	model     string
	createdAt int64

	responseID string
	started    bool

	// done 是已经收尾的输出项，用于 response.completed 里的 output 数组。
	done []OutputItem

	// open 是当前正在写的输出项。Responses 同一时刻只有一项处于打开状态。
	open *openItem

	// blockToItem 把 Canonical 的块序号映射到当前打开的项，
	// 让乱序到达的 delta 能找到自己的归属。
	blockIndex int

	usage canonical.Usage
	stop  canonical.StopReason
}

type openItem struct {
	kind        string
	id          string
	outputIndex int

	// contentIndex 只对 message 有意义——function_call 与 reasoning 没有
	// content 数组这一层。
	contentIndex int
	partOpen     bool

	text strings.Builder
	args strings.Builder

	callID string
	name   string
}

// NewStreamEncoder 创建流式编码器。
func NewStreamEncoder(model string, createdAt int64) *StreamEncoder {
	return &StreamEncoder{
		model:     model,
		createdAt: createdAt,
		usage:     canonical.UnavailableUsage(),
	}
}

// Encode 消费一条 Canonical 事件，产出零条或多条 Responses 事件。
func (e *StreamEncoder) Encode(ev canonical.Event) ([]sse.Event, error) {
	switch ev.Type {
	case canonical.EventMessageStart:
		return e.begin()

	case canonical.EventContentStart:
		return e.openBlock(ev)

	case canonical.EventToolCallStart:
		// 有的上游不发 content.start，直接发 tool_call.start。
		if e.open != nil && e.open.kind == outFuncCall {
			return nil, nil
		}
		ev.ContentKind = canonical.PartToolCall
		return e.openBlock(ev)

	case canonical.EventTextDelta:
		return e.textDelta(ev)

	case canonical.EventThinkingDelta:
		return e.summaryDelta(ev)

	case canonical.EventSignatureDelta:
		// 签名不往外发。它是 Anthropic 的凭证，对 Responses 客户端毫无意义，
		// 发出去只会被原样回传，然后在下一轮被降级矩阵拒掉。
		return nil, nil

	case canonical.EventToolCallArgsDelta:
		return e.argsDelta(ev)

	case canonical.EventContentEnd, canonical.EventToolCallEnd:
		return e.closeBlock()

	case canonical.EventUsage:
		if ev.Usage != nil {
			e.usage = *ev.Usage
		}
		return nil, nil

	case canonical.EventMessageEnd, canonical.EventResponseDone:
		if ev.StopReason != "" {
			e.stop = ev.StopReason
		}
		return e.finish()

	case canonical.EventError:
		return e.fail(ev.Err)

	default:
		// 未知事件不静默丢弃也不报错：Canonical 会随协议演进增加事件类型，
		// 而一个还不认识它的编码器最合理的行为是不产出对应输出。报错会让
		// 一条本可正常完成的流因为一个无关紧要的新事件而中断。
		return nil, nil
	}
}

// begin 发出 response.created 与 response.in_progress。
func (e *StreamEncoder) begin() ([]sse.Event, error) {
	if e.started {
		return nil, nil
	}
	id, err := newID("resp_")
	if err != nil {
		return nil, err
	}
	e.responseID = id
	e.started = true

	snapshot := e.snapshot(statusInProgress, nil)
	created, err := e.wrap(evCreated, snapshot)
	if err != nil {
		return nil, err
	}
	inProgress, err := e.wrap(evInProgress, snapshot)
	if err != nil {
		return nil, err
	}
	return []sse.Event{created, inProgress}, nil
}

// openBlock 打开一个新的输出项。
func (e *StreamEncoder) openBlock(ev canonical.Event) ([]sse.Event, error) {
	out, err := e.begin()
	if err != nil {
		return nil, err
	}

	// Responses 同一时刻只有一项打开。上游若不发 content.end 就开下一块，
	// 这里替它收尾——丢一个 output_item.done 会让客户端的状态机一直卡在
	// 「这一项还没结束」。
	closed, err := e.closeBlock()
	if err != nil {
		return nil, err
	}
	out = append(out, closed...)

	e.blockIndex = ev.Index
	idx := len(e.done)

	switch ev.ContentKind {
	case canonical.PartToolCall:
		id, err := newID("fc_")
		if err != nil {
			return nil, err
		}
		it := &openItem{kind: outFuncCall, id: id, outputIndex: idx}
		if ev.ToolCall != nil {
			it.callID, it.name = ev.ToolCall.ID, ev.ToolCall.Name
		}
		e.open = it

		added, err := e.wrapItem(evItemAdded, idx, OutputItem{
			Type: outFuncCall, ID: id, Status: statusInProgress,
			CallID: it.callID, Name: it.name, Arguments: "",
		})
		if err != nil {
			return nil, err
		}
		return append(out, added), nil

	case canonical.PartThinking:
		id, err := newID("rs_")
		if err != nil {
			return nil, err
		}
		e.open = &openItem{kind: outReasoning, id: id, outputIndex: idx}

		added, err := e.wrapItem(evItemAdded, idx, OutputItem{
			Type: outReasoning, ID: id, Status: statusInProgress,
		})
		if err != nil {
			return nil, err
		}
		return append(out, added), nil

	default:
		id, err := newID("msg_")
		if err != nil {
			return nil, err
		}
		e.open = &openItem{kind: outMessage, id: id, outputIndex: idx, partOpen: true}

		added, err := e.wrapItem(evItemAdded, idx, OutputItem{
			Type: outMessage, ID: id, Status: statusInProgress,
			Role: "assistant", Content: []OutputPart{},
		})
		if err != nil {
			return nil, err
		}
		part, err := e.wrapPart(evPartAdded, OutputPart{
			Type: partOutputText, Text: "", Annotations: []any{},
		})
		if err != nil {
			return nil, err
		}
		return append(out, added, part), nil
	}
}

func (e *StreamEncoder) textDelta(ev canonical.Event) ([]sse.Event, error) {
	if e.open == nil || e.open.kind != outMessage {
		// 上游没发 content.start 就直接发 delta。补开一块，而不是丢掉内容。
		opened, err := e.openBlock(canonical.Event{
			Type: canonical.EventContentStart, Index: ev.Index,
			ContentKind: canonical.PartText,
		})
		if err != nil {
			return nil, err
		}
		d, err := e.textDelta(ev)
		if err != nil {
			return nil, err
		}
		return append(opened, d...), nil
	}

	e.open.text.WriteString(ev.Text)
	d, err := e.wrapDelta(evTextDelta, ev.Text, true)
	if err != nil {
		return nil, err
	}
	return []sse.Event{d}, nil
}

func (e *StreamEncoder) summaryDelta(ev canonical.Event) ([]sse.Event, error) {
	if e.open == nil || e.open.kind != outReasoning {
		opened, err := e.openBlock(canonical.Event{
			Type: canonical.EventContentStart, Index: ev.Index,
			ContentKind: canonical.PartThinking,
		})
		if err != nil {
			return nil, err
		}
		d, err := e.summaryDelta(ev)
		if err != nil {
			return nil, err
		}
		return append(opened, d...), nil
	}

	e.open.text.WriteString(ev.Text)
	d, err := e.wrapDelta(evSummaryDelta, ev.Text, false)
	if err != nil {
		return nil, err
	}
	return []sse.Event{d}, nil
}

func (e *StreamEncoder) argsDelta(ev canonical.Event) ([]sse.Event, error) {
	if e.open == nil || e.open.kind != outFuncCall {
		return nil, canonical.Newf(canonical.ClassInternal,
			"收到工具参数增量，但当前没有打开的 function_call 项")
	}

	e.open.args.WriteString(ev.ArgsDelta)
	d, err := e.wrapDelta(evArgsDelta, ev.ArgsDelta, false)
	if err != nil {
		return nil, err
	}
	return []sse.Event{d}, nil
}

// closeBlock 收尾当前输出项。幂等。
func (e *StreamEncoder) closeBlock() ([]sse.Event, error) {
	it := e.open
	if it == nil {
		return nil, nil
	}
	e.open = nil

	var out []sse.Event

	switch it.kind {
	case outMessage:
		text := it.text.String()
		part := OutputPart{Type: partOutputText, Text: text, Annotations: []any{}}

		e.open = it // wrapDelta / wrapPart 需要它，收尾后再清空
		done, err := e.wrapDelta(evTextDone, text, true)
		if err != nil {
			return nil, err
		}
		partDone, err := e.wrapPart(evPartDone, part)
		if err != nil {
			return nil, err
		}
		e.open = nil
		out = append(out, done, partDone)

		item := OutputItem{
			Type: outMessage, ID: it.id, Status: statusCompleted,
			Role: "assistant", Content: []OutputPart{part},
		}
		e.done = append(e.done, item)
		itemDone, err := e.wrapItem(evItemDone, it.outputIndex, item)
		if err != nil {
			return nil, err
		}
		out = append(out, itemDone)

	case outFuncCall:
		args := it.args.String()
		if args == "" {
			args = "{}"
		}
		e.open = it
		done, err := e.wrapDelta(evArgsDone, args, false)
		if err != nil {
			return nil, err
		}
		e.open = nil
		out = append(out, done)

		item := OutputItem{
			Type: outFuncCall, ID: it.id, Status: statusCompleted,
			CallID: it.callID, Name: it.name, Arguments: args,
		}
		e.done = append(e.done, item)
		itemDone, err := e.wrapItem(evItemDone, it.outputIndex, item)
		if err != nil {
			return nil, err
		}
		out = append(out, itemDone)

	case outReasoning:
		text := it.text.String()
		e.open = it
		done, err := e.wrapDelta(evSummaryDone, text, false)
		if err != nil {
			return nil, err
		}
		e.open = nil
		out = append(out, done)

		item := OutputItem{Type: outReasoning, ID: it.id}
		if text != "" {
			item.Summary = []SummaryPart{{Type: "summary_text", Text: text}}
		}
		e.done = append(e.done, item)
		itemDone, err := e.wrapItem(evItemDone, it.outputIndex, item)
		if err != nil {
			return nil, err
		}
		out = append(out, itemDone)
	}

	return out, nil
}

// finish 收尾整条流。
func (e *StreamEncoder) finish() ([]sse.Event, error) {
	out, err := e.closeBlock()
	if err != nil {
		return nil, err
	}

	status, details := statusFor(e.stop)
	name := evCompleted
	switch status {
	case statusIncomplete:
		name = evIncomplete
	case statusFailed:
		name = evFailed
	}

	final, err := e.wrap(name, e.snapshot(status, details))
	if err != nil {
		return nil, err
	}
	return append(out, final), nil
}

// fail 在流中途发出终止错误事件。
func (e *StreamEncoder) fail(cerr *canonical.Error) ([]sse.Event, error) {
	if cerr == nil {
		cerr = canonical.Newf(canonical.ClassInternal, "未知错误")
	}

	// 先把开着的项收尾，客户端的状态机才不会卡住。
	out, err := e.closeBlock()
	if err != nil {
		return nil, err
	}

	// 用量标记为不可知：上游不会再送 usage 了，任何非零数字都是编造的。
	e.usage = canonical.UnavailableUsage()

	snap := e.snapshot(statusFailed, nil)
	snap.Error = &ResponseError{Code: string(cerr.Class), Message: cerr.Message}

	failed, err := e.wrap(evFailed, snap)
	if err != nil {
		return nil, err
	}
	out = append(out, failed)

	data, err := json.Marshal(map[string]string{
		"type": "error", "code": string(cerr.Class), "message": cerr.Message,
	})
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "序列化错误事件失败")
	}
	return append(out, sse.Event{Event: evError, Data: string(data)}), nil
}

// snapshot 构造当前的响应对象。
func (e *StreamEncoder) snapshot(status string, details *IncompleteDetails) Response {
	return Response{
		ID:                e.responseID,
		Object:            "response",
		CreatedAt:         e.createdAt,
		Status:            status,
		Model:             e.model,
		Output:            e.done,
		Usage:             toWireUsage(e.usage),
		IncompleteDetails: details,
	}
}

// wrap 把响应对象包成一条事件。
func (e *StreamEncoder) wrap(name string, resp Response) (sse.Event, error) {
	if resp.Output == nil {
		resp.Output = []OutputItem{}
	}
	data, err := json.Marshal(map[string]any{"type": name, "response": resp})
	if err != nil {
		return sse.Event{}, canonical.Wrapf(err, canonical.ClassInternal, "序列化 %s 失败", name)
	}
	return sse.Event{Event: name, Data: string(data)}, nil
}

func (e *StreamEncoder) wrapItem(name string, index int, item OutputItem) (sse.Event, error) {
	data, err := json.Marshal(map[string]any{
		"type": name, "output_index": index, "item": item,
	})
	if err != nil {
		return sse.Event{}, canonical.Wrapf(err, canonical.ClassInternal, "序列化 %s 失败", name)
	}
	return sse.Event{Event: name, Data: string(data)}, nil
}

func (e *StreamEncoder) wrapPart(name string, part OutputPart) (sse.Event, error) {
	data, err := json.Marshal(map[string]any{
		"type": name, "item_id": e.open.id, "output_index": e.open.outputIndex,
		"content_index": e.open.contentIndex, "part": part,
	})
	if err != nil {
		return sse.Event{}, canonical.Wrapf(err, canonical.ClassInternal, "序列化 %s 失败", name)
	}
	return sse.Event{Event: name, Data: string(data)}, nil
}

// wrapDelta 构造增量或收尾事件。
//
// withContentIndex 区分两类：message 的文本事件带 content_index，
// function_call 与 reasoning 没有 content 数组那一层，带上会让严格解析的
// 客户端拒收。
func (e *StreamEncoder) wrapDelta(name, text string, withContentIndex bool) (sse.Event, error) {
	payload := map[string]any{
		"type": name, "item_id": e.open.id, "output_index": e.open.outputIndex,
	}
	if withContentIndex {
		payload["content_index"] = e.open.contentIndex
	}

	switch name {
	case evTextDelta, evSummaryDelta, evArgsDelta:
		payload["delta"] = text
	case evTextDone:
		payload["text"] = text
	case evSummaryDone:
		payload["text"] = text
	case evArgsDone:
		payload["arguments"] = text
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return sse.Event{}, canonical.Wrapf(err, canonical.ClassInternal, "序列化 %s 失败", name)
	}
	return sse.Event{Event: name, Data: string(data)}, nil
}

// ResponseID 返回本次流的响应标识，供会话存储登记使用。
func (e *StreamEncoder) ResponseID() string { return e.responseID }

// Usage 返回累积的用量。
func (e *StreamEncoder) Usage() canonical.Usage { return e.usage }
