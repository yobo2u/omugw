// Package openaichat 是 openai.chat（Chat Completions）入站协议的编解码。
//
// 与 openairesponses 并列，同属 OpenAI 协议族。同源快通道下它只承担「解码出
// 能力与入口约束」这一职责——真正的字节由 passthrough 原样转发，不经过这里
// 重新编码。
package openaichat

import "encoding/json"

// Request 是 Chat Completions 的请求线格式。
//
// 字段尽量从全：入站解码是 DisallowUnknownFields 的严格模式，一个没建模的字段
// 会让整个请求被拒。宁可在这里把常见参数都列出来，也不要让合法请求撞墙。
type Request struct {
	Model    string          `json:"model"`
	Messages json.RawMessage `json:"messages"`
	Stream   *bool           `json:"stream,omitempty"`

	// 采样参数。max_tokens 是旧名，max_completion_tokens 是新名，两者择一。
	MaxTokens           *int               `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int               `json:"max_completion_tokens,omitempty"`
	Temperature         *float64           `json:"temperature,omitempty"`
	TopP                *float64           `json:"top_p,omitempty"`
	N                   *int               `json:"n,omitempty"`
	Stop                json.RawMessage    `json:"stop,omitempty"` // string 或 []string
	Seed                *int64             `json:"seed,omitempty"`
	PresencePenalty     *float64           `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64           `json:"frequency_penalty,omitempty"`
	LogitBias           map[string]float64 `json:"logit_bias,omitempty"`
	Logprobs            *bool              `json:"logprobs,omitempty"`
	TopLogprobs         *int               `json:"top_logprobs,omitempty"`

	// 工具。
	Tools             json.RawMessage `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`

	// 输出形态。
	ResponseFormat  json.RawMessage `json:"response_format,omitempty"`
	ReasoningEffort *string         `json:"reasoning_effort,omitempty"`
	Modalities      []string        `json:"modalities,omitempty"`
	Audio           json.RawMessage `json:"audio,omitempty"`

	// 其余常见参数。对能力裁决无关，但必须建模以免严格模式误拒。
	StreamOptions json.RawMessage   `json:"stream_options,omitempty"`
	User          string            `json:"user,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	Store         *bool             `json:"store,omitempty"`
	ServiceTier   string            `json:"service_tier,omitempty"`

	// 内建搜索选项。出现且非 null（哪怕是 {}）即报告 CapWebSearch；
	// 子字段在 Decode 里严格校验——它在异构出站前被整体删除，
	// 未建模的子字段必须 400，不能静默丢。
	WebSearchOptions json.RawMessage `json:"web_search_options,omitempty"`
}

// Message 是一条对话消息。Content 形态不一：字符串、内容块数组、或 null
// （assistant 只带 tool_calls 时），故用 RawMessage 延迟解码。
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Refusal    string          `json:"refusal,omitempty"`
}

// ToolCall 是 assistant 消息里的工具调用。Arguments 是 JSON 字符串。
type ToolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

// Function 是工具调用的函数体。
type Function struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ContentPart 是多模态内容块。
type ContentPart struct {
	Type       string      `json:"type"`
	Text       string      `json:"text,omitempty"`
	ImageURL   *ImageURL   `json:"image_url,omitempty"`
	InputAudio *InputAudio `json:"input_audio,omitempty"`
	File       *FilePart   `json:"file,omitempty"`
}

// ImageURL 是图片负载。URL 既可能是 http(s) 也可能是 data: URI。
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// InputAudio 是内联音频，base64 数据 + 容器格式。
type InputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

// FilePart 是文件负载：要么引用上游文件 ID，要么内联 data URI。
type FilePart struct {
	FileID   string `json:"file_id,omitempty"`
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
}

// Tool 是工具声明。Chat 的工具目前只有 function 一种。
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction 是 function 工具的定义。
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ResponseFormat 是结构化输出约束。
type ResponseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *JSONSchema `json:"json_schema,omitempty"`
}

// JSONSchema 是 json_schema 形态的具体约束。
type JSONSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict *bool           `json:"strict,omitempty"`
}

// WebSearchOptions 是 Chat 的搜索选项。
//
// DashScope Compatible 承载它的只有布尔开关 enable_search，但这里仍要解码出
// 完整结构：该对象在出站前被整体删除，任何未建模的子字段都会随之消失——
// 宁可 400 拒绝，也不静默吞掉。
type WebSearchOptions struct {
	SearchContextSize string        `json:"search_context_size,omitempty"`
	UserLocation      *UserLocation `json:"user_location,omitempty"`
}

// UserLocation 是搜索的用户位置。OpenAI 当前只接受 approximate 类型；
// 具体位置在 approximate 子对象里。
type UserLocation struct {
	Type        string               `json:"type"`
	Approximate *ApproximateLocation `json:"approximate,omitempty"`
}

// ApproximateLocation 是用户的大致位置。
type ApproximateLocation struct {
	Country  string `json:"country,omitempty"`
	City     string `json:"city,omitempty"`
	Region   string `json:"region,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}
