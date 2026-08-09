package canonical

import (
	"encoding/json"
	"fmt"
)

// EventType 是统一流事件类型。
//
// 同一套事件同时服务 HTTP 流式（Chat / Responses）与 Realtime WebSocket。
// 这不是为了「优雅」——而是因为 DashScope 的 /api-ws/v1/realtime 与 OpenAI
// Realtime 事件模型本就同构，共用一套内部表示能让快通道退化成近乎恒等映射。
type EventType string

const (
	// —— HTTP 流式与 Realtime 共用 ——
	EventMessageStart EventType = "message.start"
	EventMessageEnd   EventType = "message.end"
	EventUsage        EventType = "usage"
	EventError        EventType = "error"

	// 内容块生命周期
	EventContentStart EventType = "content.start"
	EventContentEnd   EventType = "content.end"
	EventTextDelta    EventType = "text.delta"

	// 推理增量。SignatureDelta 单独成事件：Anthropic 把 thinking 签名作为
	// 独立的 signature_delta 送出，且签名必须完整拼接后原样回传才有效。
	EventThinkingDelta  EventType = "thinking.delta"
	EventSignatureDelta EventType = "signature.delta"

	// 工具调用。ArgsDelta 携带的是**未必闭合**的 JSON 片段——跨协议转换时
	// 必须缓冲到闭合才能重新分片，这正是异构路径会牺牲 TTFT 的原因。
	EventToolCallStart     EventType = "tool_call.start"
	EventToolCallArgsDelta EventType = "tool_call.args_delta"
	EventToolCallEnd       EventType = "tool_call.end"

	// —— Realtime 专用 ——
	EventSessionCreated  EventType = "session.created"
	EventSessionUpdated  EventType = "session.updated"
	EventResponseCreated EventType = "response.created"
	EventResponseDone    EventType = "response.done"
	EventAudioDelta      EventType = "audio.delta"
	EventAudioDone       EventType = "audio.done"
	EventTranscriptDelta EventType = "transcript.delta"
	EventTranscriptDone  EventType = "transcript.done"
	EventSpeechStarted   EventType = "speech.started"
	EventSpeechStopped   EventType = "speech.stopped"
)

// StopReason 是生成终止原因。
type StopReason string

const (
	StopEndTurn       StopReason = "end_turn"
	StopMaxTokens     StopReason = "max_tokens"
	StopStopSequence  StopReason = "stop_sequence"
	StopToolUse       StopReason = "tool_use"
	StopContentFilter StopReason = "content_filter"

	// StopInterrupted 表示流在首字节之后被上游中断。
	// 此时 usage 必须标记为 FidelityUnavailable，且**不得重试**。
	StopInterrupted StopReason = "interrupted"
)

// Event 是一个统一流事件。
//
// 只有与 Type 相关的字段有意义。Raw 是同源快通道的逃生舱：透传模式下
// 编码器直接回写 Raw，完全跳过重新序列化。
type Event struct {
	Type EventType `json:"type"`

	// Index 是内容块序号，用于把 delta 归位到正确的块。
	Index int `json:"index,omitempty"`

	// Text 承载 EventTextDelta / EventThinkingDelta / EventSignatureDelta /
	// EventTranscriptDelta 的增量文本。
	Text string `json:"text,omitempty"`

	// ContentKind 在 EventContentStart 时说明这个块是什么类型。
	ContentKind PartKind `json:"content_kind,omitempty"`

	// ToolCall 在 EventToolCallStart 时携带 ID 与 Name（此时 Arguments 为空）。
	ToolCall *ToolCall `json:"tool_call,omitempty"`

	// ArgsDelta 是工具参数的 JSON 片段，可能不闭合。
	ArgsDelta string `json:"args_delta,omitempty"`

	// Audio 承载 EventAudioDelta 的 PCM 数据，Format 描述其采样参数。
	Audio       []byte    `json:"audio,omitempty"`
	AudioFormat *AudioFmt `json:"audio_format,omitempty"`

	StopReason StopReason `json:"stop_reason,omitempty"`
	Usage      *Usage     `json:"usage,omitempty"`
	Err        *Error     `json:"error,omitempty"`

	// SessionID 与 ResponseID 是 Realtime 会话内的关联标识。
	SessionID  string `json:"session_id,omitempty"`
	ResponseID string `json:"response_id,omitempty"`

	// Raw 保存上游原始事件负载，仅供同源快通道原样回写。
	// 异构转换路径**不得**读取 Raw 猜测语义。
	Raw json.RawMessage `json:"-"`
}

// Accumulator 把事件流重新聚合成一个完整消息。
//
// 用途有二：一是 fixture 测试断言「流式与非流式结果一致」；二是异构转换时
// 缓冲未闭合的工具参数——OpenAI 的 arguments 片段与 Anthropic 的
// input_json_delta 分片边界不同，只能先拼完整再按目标协议重新切分。
type Accumulator struct {
	Parts      []Part
	StopReason StopReason
	Usage      Usage

	// toolArgs 按块序号缓冲未闭合的参数片段。
	toolArgs map[int]*[]byte
	// signatures 按块序号缓冲 thinking 签名片段。
	signatures map[int]*[]byte
	// blockIndex 把事件的 Index 映射到 Parts 的下标。
	blockIndex map[int]int
}

// NewAccumulator 创建一个聚合器。Usage 初始为 unavailable——只有真正收到
// usage 事件才会被覆盖，中途断流时保持「不可知」是正确的。
func NewAccumulator() *Accumulator {
	return &Accumulator{
		Usage:      UnavailableUsage(),
		toolArgs:   map[int]*[]byte{},
		signatures: map[int]*[]byte{},
		blockIndex: map[int]int{},
	}
}

// Add 消费一个事件。返回错误表示事件序列本身不自洽（例如 delta 先于对应的
// content.start 到达），这通常意味着上游解码器有 bug。
func (a *Accumulator) Add(ev Event) error {
	switch ev.Type {
	case EventContentStart:
		a.blockIndex[ev.Index] = len(a.Parts)
		switch ev.ContentKind {
		case PartThinking:
			a.Parts = append(a.Parts, Part{Kind: PartThinking, Thinking: &Thinking{}})
			buf := make([]byte, 0, 64)
			a.signatures[ev.Index] = &buf
		case PartToolCall:
			tc := &ToolCall{}
			if ev.ToolCall != nil {
				tc.ID, tc.Name = ev.ToolCall.ID, ev.ToolCall.Name
			}
			a.Parts = append(a.Parts, Part{Kind: PartToolCall, ToolCall: tc})
			buf := make([]byte, 0, 128)
			a.toolArgs[ev.Index] = &buf
		default:
			a.Parts = append(a.Parts, Part{Kind: PartText})
		}

	case EventToolCallStart:
		// 有的协议不发 content.start，直接发 tool_call.start。
		if _, ok := a.blockIndex[ev.Index]; !ok {
			a.blockIndex[ev.Index] = len(a.Parts)
			tc := &ToolCall{}
			if ev.ToolCall != nil {
				tc.ID, tc.Name = ev.ToolCall.ID, ev.ToolCall.Name
			}
			a.Parts = append(a.Parts, Part{Kind: PartToolCall, ToolCall: tc})
			buf := make([]byte, 0, 128)
			a.toolArgs[ev.Index] = &buf
		}

	case EventTextDelta:
		p, err := a.part(ev.Index, PartText)
		if err != nil {
			return err
		}
		p.Text += ev.Text

	case EventThinkingDelta:
		p, err := a.part(ev.Index, PartThinking)
		if err != nil {
			return err
		}
		p.Thinking.Text += ev.Text

	case EventSignatureDelta:
		buf, ok := a.signatures[ev.Index]
		if !ok {
			return fmt.Errorf("canonical: signature.delta for unknown block %d", ev.Index)
		}
		*buf = append(*buf, ev.Text...)

	case EventToolCallArgsDelta:
		buf, ok := a.toolArgs[ev.Index]
		if !ok {
			return fmt.Errorf("canonical: tool_call.args_delta for unknown block %d", ev.Index)
		}
		*buf = append(*buf, ev.ArgsDelta...)

	case EventContentEnd, EventToolCallEnd:
		return a.closeBlock(ev.Index)

	case EventMessageEnd, EventResponseDone:
		if ev.StopReason != "" {
			a.StopReason = ev.StopReason
		}
		// 收尾时把所有仍未关闭的块补上，容忍上游省略 content.end。
		for idx := range a.blockIndex {
			if err := a.closeBlock(idx); err != nil {
				return err
			}
		}

	case EventUsage:
		if ev.Usage != nil {
			a.Usage = *ev.Usage
		}

	case EventError:
		a.StopReason = StopInterrupted
		a.Usage = UnavailableUsage()
	}
	return nil
}

// closeBlock 把缓冲的工具参数与签名落到对应的 Part 上。幂等。
func (a *Accumulator) closeBlock(index int) error {
	pos, ok := a.blockIndex[index]
	if !ok {
		return nil
	}
	p := &a.Parts[pos]

	if buf, ok := a.toolArgs[index]; ok {
		if len(*buf) > 0 {
			if !json.Valid(*buf) {
				return fmt.Errorf("canonical: tool call arguments for block %d are not valid JSON after reassembly", index)
			}
			p.ToolCall.Arguments = json.RawMessage(*buf)
		}
		delete(a.toolArgs, index)
	}
	if buf, ok := a.signatures[index]; ok {
		if len(*buf) > 0 {
			p.Thinking.Signature = string(*buf)
		}
		delete(a.signatures, index)
	}
	delete(a.blockIndex, index)
	return nil
}

// part 取出指定块并校验类型。
func (a *Accumulator) part(index int, want PartKind) (*Part, error) {
	pos, ok := a.blockIndex[index]
	if !ok {
		return nil, fmt.Errorf("canonical: delta for unstarted block %d", index)
	}
	p := &a.Parts[pos]
	if p.Kind != want {
		return nil, fmt.Errorf("canonical: block %d is %q, got delta for %q", index, p.Kind, want)
	}
	return p, nil
}

// Message 返回聚合后的助手消息。
func (a *Accumulator) Message() Message {
	return Message{Role: RoleAssistant, Parts: a.Parts}
}
