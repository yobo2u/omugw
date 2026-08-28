package openaichat

import (
	"encoding/json"
	"strings"

	"github.com/yobo2u/omugw/internal/canonical"
)

// CompletionInput 是拼装 chat.completion 的 Canonical 素材，由 translator 从
// 上游解码结果填好。id/created 一律取输入：编码器自造标识或时间戳，会让
// 同一次上游调用在网关侧变成一个查不到的新事实。
type CompletionInput struct {
	ID      string
	Model   string
	Created int64
	Choices []CompletionChoice
	Usage   *canonical.Usage
}

// CompletionChoice 是一个候选，逐条编码为 choices[]。
type CompletionChoice struct {
	Message      canonical.Message
	FinishReason *string
	Logprobs     json.RawMessage
}

// respCompletion 是 chat.completion 的顶层线格式。
//
// 响应侧线类型一律不导出：本包的请求侧已经占用了 Message/ToolCall/Function
// 这些名字，重名会让读代码的人分不清手上这个结构是收进来的还是发出去的。
type respCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []respChoice `json:"choices"`
	Usage   *respUsage   `json:"usage,omitempty"`
}

// respChoice 是一个候选的线格式。
type respChoice struct {
	Index        int             `json:"index"`
	Message      respMessage     `json:"message"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs"`
}

// respMessage 是候选内的助手消息。
//
// Content 无 omitempty：Chat 客户端普遍直接读 message.content，
// 少这个键会让它们在「模型只调了工具、没说话」时解引用崩掉。
type respMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []respToolCall `json:"tool_calls,omitempty"`
}

// respToolCall 是响应里的一次工具调用。
type respToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function respFunction `json:"function"`
}

// respFunction 是工具调用的函数体。Arguments 是 JSON **字符串**，
// 按 OpenAI 约定由客户端自行 JSON.parse；发成对象会让解析直接抛错。
type respFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// respUsage 是响应用量线格式。
type respUsage struct {
	PromptTokens            int64                 `json:"prompt_tokens"`
	CompletionTokens        int64                 `json:"completion_tokens"`
	TotalTokens             int64                 `json:"total_tokens"`
	PromptTokensDetails     *respPromptDetails    `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *respCompletionDetail `json:"completion_tokens_details,omitempty"`
}

// respPromptDetails 是输入侧细分。
type respPromptDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

// respCompletionDetail 是输出侧细分。
type respCompletionDetail struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// EncodeCompletion 编码 chat.completion wire 字节。
func EncodeCompletion(in CompletionInput) ([]byte, error) {
	choices := make([]respChoice, 0, len(in.Choices))
	for i, c := range in.Choices {
		msg, err := encodeChoiceMessage(i, c.Message)
		if err != nil {
			return nil, err
		}
		choices = append(choices, respChoice{
			Index:        i,
			Message:      msg,
			FinishReason: c.FinishReason,
			Logprobs:     c.Logprobs,
		})
	}

	var usage *respUsage
	if in.Usage != nil {
		u, err := encodeUsage(in.Usage)
		if err != nil {
			return nil, err
		}
		usage = u
	}

	out := respCompletion{
		ID:      in.ID,
		Object:  "chat.completion",
		Created: in.Created,
		Model:   in.Model,
		Choices: choices,
		Usage:   usage,
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "chat completion 序列化失败")
	}
	return raw, nil
}

func encodeChoiceMessage(choiceIdx int, msg canonical.Message) (respMessage, error) {
	if msg.Role != canonical.RoleAssistant {
		return respMessage{}, canonical.Newf(
			canonical.ClassInternal,
			"choices[%d]: 助手响应消息角色必须为 assistant，实际为 %q",
			choiceIdx, msg.Role,
		)
	}

	var (
		contentBuilder   strings.Builder
		reasoningBuilder strings.Builder
		toolCalls        []respToolCall
	)

	for pIdx, p := range msg.Parts {
		if err := p.Validate(); err != nil {
			return respMessage{}, canonical.Wrapf(
				err,
				canonical.ClassInternal,
				"choices[%d].parts[%d]: Part 校验失败",
				choiceIdx, pIdx,
			)
		}

		switch p.Kind {
		case canonical.PartText:
			contentBuilder.WriteString(p.Text)
		case canonical.PartThinking:
			reasoningBuilder.WriteString(p.Thinking.Text)
		case canonical.PartToolCall:
			args := string(p.ToolCall.Arguments)
			if len(args) == 0 {
				args = "{}"
			}
			toolCalls = append(toolCalls, respToolCall{
				ID:   p.ToolCall.ID,
				Type: "function",
				Function: respFunction{
					Name:      p.ToolCall.Name,
					Arguments: args,
				},
			})
		case canonical.PartRefusal, canonical.PartMedia, canonical.PartToolResult:
			// 响应侧下游 Chat 线格式不支持在 assistant choice 里直接混装 Refusal/Media/ToolResult。
			// 此时出现属于内部或上游转换错乱，绝不是客户端请求语法错误（不能回 422），
			// 必须以 ClassInternal fail-closed 拦截，防范网关静默丢弃语义并返回伪造的 200。
			return respMessage{}, canonical.Newf(
				canonical.ClassInternal,
				"choices[%d].parts[%d]: 不支持的 Part 种类 %q",
				choiceIdx, pIdx, p.Kind,
			)
		default:
			return respMessage{}, canonical.Newf(
				canonical.ClassInternal,
				"choices[%d].parts[%d]: 未知 Part 种类 %q",
				choiceIdx, pIdx, p.Kind,
			)
		}
	}

	return respMessage{
		Role:             "assistant",
		Content:          contentBuilder.String(),
		ReasoningContent: reasoningBuilder.String(),
		ToolCalls:        toolCalls,
	}, nil
}

func encodeUsage(u *canonical.Usage) (*respUsage, error) {
	if err := u.Validate(); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "chat completion usage 校验失败")
	}

	switch u.Fidelity {
	case canonical.FidelityAuthoritative:
		total := u.TotalTokens()
		ru := &respUsage{
			PromptTokens:     u.InputTokens,
			CompletionTokens: u.OutputTokens,
			TotalTokens:      total,
		}
		if u.CacheReadInputTokens > 0 {
			ru.PromptTokensDetails = &respPromptDetails{
				CachedTokens: u.CacheReadInputTokens,
			}
		}
		if u.ReasoningTokens > 0 {
			ru.CompletionTokensDetails = &respCompletionDetail{
				ReasoningTokens: u.ReasoningTokens,
			}
		}
		return ru, nil

	case canonical.FidelityEstimated, canonical.FidelityUnavailable:
		// 估算或不可用的用量不可用于计费，按约定省略 usage 字段，防向下游传递失真数据
		return nil, nil

	default:
		return nil, canonical.Newf(
			canonical.ClassInternal,
			"chat completion 未知的 usage fidelity: %q",
			u.Fidelity,
		)
	}
}
