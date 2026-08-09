package openairesponses

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/yobo2u/omugw/internal/canonical"
)

// Response 是 Responses 的响应线格式。
type Response struct {
	ID        string       `json:"id"`
	Object    string       `json:"object"`
	CreatedAt int64        `json:"created_at"`
	Status    string       `json:"status"`
	Model     string       `json:"model"`
	Output    []OutputItem `json:"output"`
	Usage     *Usage       `json:"usage,omitempty"`

	// IncompleteDetails 说明为什么没生成完。
	IncompleteDetails *IncompleteDetails `json:"incomplete_details,omitempty"`

	Error *ResponseError `json:"error,omitempty"`
}

// IncompleteDetails 是未完成原因。
type IncompleteDetails struct {
	Reason string `json:"reason"`
}

// ResponseError 是响应内嵌的错误。
type ResponseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// OutputItem 是输出数组中的一项。
type OutputItem struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Status string `json:"status,omitempty"`

	// type=message
	Role    string       `json:"role,omitempty"`
	Content []OutputPart `json:"content,omitempty"`

	// type=function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// type=reasoning
	Summary []SummaryPart `json:"summary,omitempty"`
}

// OutputPart 是消息内容块。
type OutputPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// Annotations 即使为空也要输出空数组：OpenAI 官方 SDK 会直接读它，
	// 给 null 会让部分客户端在解析时崩掉。
	Annotations []any `json:"annotations"`
}

// SummaryPart 是推理摘要块。
type SummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Usage 是响应用量。
type Usage struct {
	InputTokens        int64               `json:"input_tokens"`
	InputTokenDetails  *InputTokenDetails  `json:"input_tokens_details,omitempty"`
	OutputTokens       int64               `json:"output_tokens"`
	OutputTokenDetails *OutputTokenDetails `json:"output_tokens_details,omitempty"`
	TotalTokens        int64               `json:"total_tokens"`
}

// InputTokenDetails 是输入侧细分。
type InputTokenDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

// OutputTokenDetails 是输出侧细分。
type OutputTokenDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// 状态与输出项类型。
const (
	statusCompleted  = "completed"
	statusIncomplete = "incomplete"
	statusInProgress = "in_progress"
	statusFailed     = "failed"

	outMessage   = "message"
	outFuncCall  = "function_call"
	outReasoning = "reasoning"
)

// newID 生成带前缀的标识。
//
// 用密码学随机数而不是自增：这些 ID 会出现在客户端手里并可能被回传
// （previous_response_id），可猜测的 ID 意味着别人能引用你的会话。
func newID(prefix string) (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", canonical.Wrapf(err, canonical.ClassInternal, "生成标识失败")
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// toWireUsage 把统一用量转成线格式。
//
// 只有 authoritative 才输出数字。estimated 与 unavailable 一律返回 nil——
// 把估算值当成上游返回的用量呈现给客户端，等于替它做了一个它不知道的假设，
// 而客户端很可能拿这个数去对账。
func toWireUsage(u canonical.Usage) *Usage {
	if !u.Fidelity.Billable() {
		return nil
	}
	w := &Usage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.TotalTokens(),
	}
	if u.CacheReadInputTokens > 0 {
		w.InputTokenDetails = &InputTokenDetails{CachedTokens: u.CacheReadInputTokens}
	}
	if u.ReasoningTokens > 0 {
		w.OutputTokenDetails = &OutputTokenDetails{ReasoningTokens: u.ReasoningTokens}
	}
	return w
}

// statusFor 把终止原因映射成响应状态。
func statusFor(stop canonical.StopReason) (status string, details *IncompleteDetails) {
	switch stop {
	case canonical.StopMaxTokens:
		return statusIncomplete, &IncompleteDetails{Reason: "max_output_tokens"}
	case canonical.StopContentFilter:
		return statusIncomplete, &IncompleteDetails{Reason: "content_filter"}
	case canonical.StopInterrupted:
		// 流被中断。不报 completed——那会让客户端以为拿到了完整回答。
		return statusFailed, nil
	default:
		return statusCompleted, nil
	}
}

// EncodeResponse 把一条助手消息编成非流式响应。
func EncodeResponse(model string, msg canonical.Message, usage canonical.Usage,
	stop canonical.StopReason, createdAt int64) ([]byte, error) {

	id, err := newID("resp_")
	if err != nil {
		return nil, err
	}

	items, err := encodeOutput(msg)
	if err != nil {
		return nil, err
	}

	status, details := statusFor(stop)
	resp := Response{
		ID:                id,
		Object:            "response",
		CreatedAt:         createdAt,
		Status:            status,
		Model:             model,
		Output:            items,
		Usage:             toWireUsage(usage),
		IncompleteDetails: details,
	}

	out, err := json.Marshal(resp)
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "序列化响应失败")
	}
	return out, nil
}

// encodeOutput 把消息内容块编成 output 数组。
//
// 相邻的文本块会合并进同一个 message 项——Responses 的 message 项本身就带
// content 数组，为每个文本块单独开一项会让客户端看到一串莫名其妙的碎片消息。
// 工具调用与推理各自独立成项，它们在 Responses 里就是平级的输出项。
func encodeOutput(msg canonical.Message) ([]OutputItem, error) {
	var (
		items   []OutputItem
		pending []OutputPart
	)

	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		id, err := newID("msg_")
		if err != nil {
			return err
		}
		items = append(items, OutputItem{
			Type:    outMessage,
			ID:      id,
			Status:  statusCompleted,
			Role:    "assistant",
			Content: pending,
		})
		pending = nil
		return nil
	}

	for i, p := range msg.Parts {
		switch p.Kind {
		case canonical.PartText:
			pending = append(pending, OutputPart{
				Type: partOutputText, Text: p.Text, Annotations: []any{},
			})

		case canonical.PartRefusal:
			pending = append(pending, OutputPart{
				Type: partRefusal, Text: p.Text, Annotations: []any{},
			})

		case canonical.PartToolCall:
			if err := flush(); err != nil {
				return nil, err
			}
			if p.ToolCall == nil {
				return nil, canonical.Newf(canonical.ClassInternal,
					"parts[%d] 标记为 tool_call 但没有负载", i)
			}
			id, err := newID("fc_")
			if err != nil {
				return nil, err
			}
			args := string(p.ToolCall.Arguments)
			if args == "" {
				// Responses 的 arguments 是字符串字段，空字符串会让部分客户端
				// 的 JSON.parse 抛错。给一个合法的空对象。
				args = "{}"
			}
			items = append(items, OutputItem{
				Type: outFuncCall, ID: id, Status: statusCompleted,
				CallID: p.ToolCall.ID, Name: p.ToolCall.Name, Arguments: args,
			})

		case canonical.PartThinking:
			if err := flush(); err != nil {
				return nil, err
			}
			if p.Thinking == nil {
				return nil, canonical.Newf(canonical.ClassInternal,
					"parts[%d] 标记为 thinking 但没有负载", i)
			}
			id, err := newID("rs_")
			if err != nil {
				return nil, err
			}
			item := OutputItem{Type: outReasoning, ID: id}
			// 签名不往外发。它是 Anthropic 的凭证，对 Responses 客户端毫无意义，
			// 发出去只会让它被原样回传，然后在下一轮被降级矩阵拒掉。
			if p.Thinking.Text != "" {
				item.Summary = []SummaryPart{{Type: "summary_text", Text: p.Thinking.Text}}
			}
			items = append(items, item)

		case canonical.PartMedia:
			// Responses 的 output 目前不承载媒体块。静默丢弃会让客户端收到一个
			// 少了图的回答却毫不知情，因此明确报错。
			return nil, canonical.Newf(canonical.ClassUnsupported,
				"parts[%d]: Responses 的输出不承载媒体块", i)

		default:
			return nil, canonical.Newf(canonical.ClassInternal,
				"parts[%d]: 未知内容块类型 %q", i, p.Kind)
		}
	}

	if err := flush(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("openairesponses: 助手消息没有任何可编码的内容")
	}
	return items, nil
}
