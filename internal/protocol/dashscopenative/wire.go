// Package dashscopenative 是 dashscope.native 入站协议的解码器。
//
// DashScope Native 不是单一端点，而是一个协议族：文本生成、多模态生成、
// embedding、rerank、图像、视频、语音各自有独立端点与请求结构。本网关把它们
// 都归在 dashscope.native 这一个入站协议下，走同源直通——字节原样转发，解码器
// 只做路由所需的最小识别（模型名、是否流式、用到了哪些能力、内联负载大小）。
//
// 与 OpenAI 系不同，这里是**宽松解码**：不加 DisallowUnknownFields。同源直通的
// 契约就是「没建模的字段也要透传」，把未知字段拒在门外恰恰违背它。
package dashscopenative

import "encoding/json"

// SSEHeader 是声明流式的请求头。DashScope Native 把「是否流式」放在头上而非
// 请求体里——值为 "enable" 时上游以 SSE 返回。
const SSEHeader = "X-DashScope-SSE"

// TextGenerationPath 是文本生成的上游端点路径。DashScope Native 一个协议族
// 对应多个端点，本期只接入文本生成这一条。
const TextGenerationPath = "/api/v1/services/aigc/text-generation/generation"

// Request 是 DashScope Native 请求的顶层结构：model / input / parameters 三段。
type Request struct {
	Model      string     `json:"model"`
	Input      Input      `json:"input"`
	Parameters Parameters `json:"parameters"`
}

// Input 承载对话内容。文本生成用 messages；其余端点（如图像生成的 prompt）
// 字段不同，这里不建模，直通即可。
type Input struct {
	Messages []Message `json:"messages"`
}

// Message 是一条消息。Content 形态不一：纯文本是字符串，多模态是内容块数组。
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Parameters 是采样与行为参数。只建模能力识别需要的几项，其余直通。
type Parameters struct {
	Tools          json.RawMessage `json:"tools"`
	EnableSearch   *bool           `json:"enable_search"`
	EnableThinking *bool           `json:"enable_thinking"`
}

// ContentPart 是多模态内容块。Type 区分文本 / 图像 / 音频 / 视频 / 文件。
type ContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`

	Image string `json:"image"`
	Audio string `json:"audio"`
	File  string `json:"file"`

	// Video 官方定义是 **array 或 string**：传图像列表（视频帧）时是数组。
	// 用 string 建模会让带数组 video 的内容块整段解不出来，连同兄弟块一起丢，
	// 内联上限随之被绕过——所以这里收原始字节，按两种形态分别解。
	Video json.RawMessage `json:"video"`
}
