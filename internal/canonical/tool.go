package canonical

import "encoding/json"

// Tool 是一个可调用工具的声明。
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`

	// Strict 对应 OpenAI 的严格 schema 校验。Anthropic 无对应概念——
	// 由降级矩阵的 CapStructuredOutput 处置，不在这里静默丢弃。
	Strict bool `json:"strict,omitempty"`
}

// ToolCall 是模型发起的一次工具调用。
//
// Arguments 保存**完整且已闭合**的 JSON。流式场景下的增量片段不放这里，
// 而是走 Event.ArgsDelta——三家的分片格式各不相同（OpenAI 是
// tool_calls[].function.arguments 字符串片段，Anthropic 是 input_json_delta，
// Gemini 直接给整块），重新分片必须在流层完成。
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolResult 是工具执行结果。
type ToolResult struct {
	CallID  string `json:"call_id"`
	Content []Part `json:"content,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

// ToolChoiceMode 描述工具选择策略。
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceSpecific ToolChoiceMode = "specific"
)

// ToolChoice 是工具选择约束。Mode 为 ToolChoiceSpecific 时 Name 必填。
type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode,omitempty"`
	Name string         `json:"name,omitempty"`
}
