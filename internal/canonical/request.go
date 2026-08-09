package canonical

import (
	"encoding/json"
	"fmt"
	"time"
)

// Request 是网关内部的统一请求表示。
//
// 它是**有损**的：不是所有协议的所有字段都能在这里无歧义表达。无法表达的部分
// 走两条路——同源快通道下经 Extensions 原样回填，异构转换下由 internal/degrade
// 显式判定 DEGRADE 或 REJECT。任何「悄悄丢掉一个字段」的实现都是 bug。
type Request struct {
	// Model 是客户端请求的逻辑模型名，路由后才映射成上游真实模型。
	Model string `json:"model"`

	// System 是系统指令。OpenAI 放在 messages 里、Anthropic 放顶层 system、
	// DashScope 用 system role——解码器统一提取到这里。
	System []Part `json:"system,omitempty"`

	Messages []Message `json:"messages,omitempty"`

	Tools      []Tool      `json:"tools,omitempty"`
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`

	Stream bool `json:"stream,omitempty"`

	// 采样参数一律用指针：0 值是合法取值，与「未设置」语义不同。
	// 用非指针会让 temperature=0 在转换后变成上游默认值，是很难查的 bug。
	MaxOutputTokens *int     `json:"max_output_tokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	TopK            *int     `json:"top_k,omitempty"`
	Seed            *int64   `json:"seed,omitempty"`
	StopSequences   []string `json:"stop_sequences,omitempty"`

	Reasoning      *Reasoning      `json:"reasoning,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	Modalities     []Modality      `json:"modalities,omitempty"`
	Cache          *CacheHint      `json:"cache,omitempty"`

	Metadata   map[string]string `json:"metadata,omitempty"`
	Extensions Extensions        `json:"extensions,omitempty"`
}

// ReasoningEffort 是推理强度档位。
type ReasoningEffort string

const (
	EffortMinimal ReasoningEffort = "minimal"
	EffortLow     ReasoningEffort = "low"
	EffortMedium  ReasoningEffort = "medium"
	EffortHigh    ReasoningEffort = "high"
)

// Reasoning 描述推理请求。
//
// OpenAI 用 effort 档位，Anthropic 用 thinking budget token 数——两者不是线性
// 可换算的。转换器可以按映射表给出近似值，但必须在降级矩阵里登记为 DEGRADE，
// 让调用方知道这不是等价转换。
type Reasoning struct {
	Effort    ReasoningEffort `json:"effort,omitempty"`
	MaxTokens *int            `json:"max_tokens,omitempty"`

	// Visible 表示客户端要求返回推理内容本身，而不只是消耗推理 token。
	Visible bool `json:"visible,omitempty"`
}

// ResponseFormatKind 是结构化输出的形态。
type ResponseFormatKind string

const (
	FormatText       ResponseFormatKind = "text"
	FormatJSONObject ResponseFormatKind = "json_object"
	FormatJSONSchema ResponseFormatKind = "json_schema"
)

// ResponseFormat 是结构化输出约束。
type ResponseFormat struct {
	Kind   ResponseFormatKind `json:"kind"`
	Name   string             `json:"name,omitempty"`
	Schema json.RawMessage    `json:"schema,omitempty"`
	Strict bool               `json:"strict,omitempty"`
}

// Modality 是请求的输入/输出模态。
type Modality string

const (
	ModalityText  Modality = "text"
	ModalityAudio Modality = "audio"
	ModalityImage Modality = "image"
	ModalityVideo Modality = "video"
)

// CacheHint 描述 prompt 缓存意图。
//
// 三家的语义互不相容：Anthropic 是消息序列上的显式断点（上限 4 个），
// OpenAI 是不可控的自动前缀缓存，Gemini 是带 TTL 的独立 CachedContent 资源
// （有自己的 create/delete API）。它们之间不存在映射函数。
//
// 因此 CacheHint **只在同源快通道下有意义**。降级矩阵中 CapPromptCache 对所有
// 异构路径都是 DEGRADE（丢弃缓存意图，请求本身仍然有效）。
type CacheHint struct {
	// Breakpoints 是 Anthropic 风格断点，元素为 Messages 的下标。
	Breakpoints []int `json:"breakpoints,omitempty"`

	// TTL 与 ResourceID 是 Gemini CachedContent 风格。
	TTL        time.Duration `json:"ttl,omitempty"`
	ResourceID string        `json:"resource_id,omitempty"`
}

// AnthropicBreakpointLimit 是 Anthropic cache_control 断点数量上限。
const AnthropicBreakpointLimit = 4

// Validate 做请求级别的自洽性检查。它不判断上游是否支持某个能力——那是
// internal/degrade 的职责。
func (r *Request) Validate() error {
	if r.Model == "" {
		return fmt.Errorf("canonical: request.model is required")
	}
	for i, p := range r.System {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("canonical: request.system[%d]: %w", i, err)
		}
	}
	for i, m := range r.Messages {
		if err := m.Validate(); err != nil {
			return fmt.Errorf("canonical: request.messages[%d]: %w", i, err)
		}
	}
	if r.ToolChoice != nil {
		if r.ToolChoice.Mode == ToolChoiceSpecific && r.ToolChoice.Name == "" {
			return fmt.Errorf("canonical: tool_choice mode=specific requires a name")
		}
	}
	if r.ResponseFormat != nil && r.ResponseFormat.Kind == FormatJSONSchema && len(r.ResponseFormat.Schema) == 0 {
		return fmt.Errorf("canonical: response_format kind=json_schema requires a schema")
	}
	if r.Cache != nil {
		for _, idx := range r.Cache.Breakpoints {
			if idx < 0 || idx >= len(r.Messages) {
				return fmt.Errorf("canonical: cache breakpoint %d out of range (%d messages)", idx, len(r.Messages))
			}
		}
	}
	return nil
}

// UsedCapabilities 报告这个请求实际用到了哪些能力。
//
// 它是降级矩阵的输入：路由选定出站 Provider 后，用这个集合去查矩阵，
// 任何一项判定为 REJECT 就立刻返回错误，而不是发出一个会被上游静默忽略
// 半数字段的请求。
func (r *Request) UsedCapabilities() []Capability {
	seen := map[Capability]bool{CapTextGeneration: true}

	if r.Stream {
		seen[CapStreaming] = true
	}
	if len(r.Tools) > 0 {
		seen[CapToolCalling] = true
	}
	if r.ResponseFormat != nil && r.ResponseFormat.Kind != FormatText {
		seen[CapStructuredOutput] = true
	}
	if r.Reasoning != nil {
		seen[CapReasoning] = true
	}
	if r.Cache != nil {
		seen[CapPromptCache] = true
	}

	scan := func(parts []Part) {
		for _, p := range parts {
			switch p.Kind {
			case PartThinking:
				seen[CapReasoning] = true
				if p.Thinking != nil && p.Thinking.Signature != "" {
					seen[CapReasoningSignature] = true
				}
			case PartToolCall, PartToolResult:
				seen[CapToolCalling] = true
			case PartMedia:
				if p.Media == nil {
					continue
				}
				switch p.Media.Kind {
				case MediaImage:
					seen[CapVisionInput] = true
				case MediaAudio:
					seen[CapAudioInput] = true
				case MediaVideo:
					seen[CapVideoInput] = true
				case MediaFile:
					seen[CapFileInput] = true
				}
			}
		}
	}
	scan(r.System)
	for _, m := range r.Messages {
		scan(m.Parts)
	}

	for _, mod := range r.Modalities {
		switch mod {
		case ModalityAudio:
			seen[CapAudioOutput] = true
		case ModalityImage:
			seen[CapImageGeneration] = true
		case ModalityVideo:
			seen[CapVideoGeneration] = true
		}
	}

	// 按 AllCapabilities 的顺序输出，保证结果稳定——golden file 依赖这一点。
	out := make([]Capability, 0, len(seen))
	for _, c := range AllCapabilities() {
		if seen[c] {
			out = append(out, c)
		}
	}
	return out
}
