// Package openairesponses 实现 OpenAI Responses 协议的入站编解码。
//
// Responses 是入站优先级第一的协议（见 README）：相对 Chat Completions，
// 它多出服务端会话、内建工具与更完整的推理配置——用表达力更弱的协议当主入口，
// 等于在网关门口就先砍掉一层语义。
package openairesponses

import "encoding/json"

// Request 是 Responses 的请求线格式。
//
// 采样参数一律用指针：0 是合法取值，与「未设置」语义不同。用非指针会让
// temperature=0 在转换后变成上游默认值——一个很难查的 bug。
type Request struct {
	Model string `json:"model"`

	// Input 可以是字符串，也可以是条目数组。用 RawMessage 接住，
	// 由 decodeInput 按实际形态分派。
	Input json.RawMessage `json:"input,omitempty"`

	// Instructions 是系统指令。它是顶层参数而不是一条消息——
	// 这与 Chat Completions 把 system 混在 messages 里是实质差异。
	Instructions string `json:"instructions,omitempty"`

	Tools      []Tool          `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`

	Stream        *bool `json:"stream,omitempty"`
	MaxOutputToks *int  `json:"max_output_tokens,omitempty"`

	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`

	Reasoning *Reasoning `json:"reasoning,omitempty"`
	Text      *TextOpts  `json:"text,omitempty"`

	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`

	// PreviousResponseID 是服务端会话的**读取**端。
	// 它一出现就意味着客户端在依赖网关或上游保管的历史。
	PreviousResponseID string `json:"previous_response_id,omitempty"`

	// Store 是服务端会话的**写入**端，用指针以区分「省略」与「显式 true」。
	//
	// OpenAI 的默认值是 true，若把省略也当成「要求服务端会话」，默认配置下
	// 几乎每个请求都会被拒。多数 SDK 不显式发送它，说明调用方并不在意；
	// 显式写 true 才是真的打算回头引用。
	Store *bool `json:"store,omitempty"`

	Metadata map[string]string `json:"metadata,omitempty"`
}

// Tool 是工具声明。Responses 的工具是扁平结构，与 Chat Completions 的
// {"type":"function","function":{...}} 嵌套形态不同。
type Tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// Reasoning 是推理配置。
type Reasoning struct {
	Effort string `json:"effort,omitempty"`
	// Summary 控制是否返回推理摘要（"auto" / "concise" / "detailed"）。
	Summary string `json:"summary,omitempty"`
}

// TextOpts 承载结构化输出配置。
type TextOpts struct {
	Format *TextFormat `json:"format,omitempty"`
}

// TextFormat 是输出格式约束。
type TextFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Strict *bool           `json:"strict,omitempty"`
}

// InputItem 是 input 数组中的一个条目。
//
// Responses 的 input 是异构数组：既有带 role 的消息，也有工具调用与工具结果
// 这类没有 role 的条目。用一个带判别字段的结构接住，比定义一堆接口更适合
// 这种「读进来立刻转成 canonical」的场景。
type InputItem struct {
	Type string `json:"type,omitempty"`
	Role string `json:"role,omitempty"`

	// Content 可以是字符串，也可以是内容块数组。
	Content json.RawMessage `json:"content,omitempty"`

	// 工具调用（type=function_call）
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// 工具结果（type=function_call_output）
	Output string `json:"output,omitempty"`
}

// ContentPart 是消息内容块。
type ContentPart struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	// 图像：image_url 可以是 http(s) URL，也可以是 data: URI。
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`

	// 文件引用
	FileID   string `json:"file_id,omitempty"`
	Filename string `json:"filename,omitempty"`

	// 音频
	InputAudio *InputAudio `json:"input_audio,omitempty"`
}

// InputAudio 是内联音频输入。
type InputAudio struct {
	Data   string `json:"data"`   // base64
	Format string `json:"format"` // "wav" / "mp3" / "pcm16"
}

// 内容块与条目的类型判别值。
const (
	partInputText  = "input_text"
	partInputImage = "input_image"
	partInputFile  = "input_file"
	partInputAudio = "input_audio"
	partOutputText = "output_text"
	partRefusal    = "refusal"

	itemMessage    = "message"
	itemFuncCall   = "function_call"
	itemFuncOutput = "function_call_output"
	itemReasoning  = "reasoning"
)
