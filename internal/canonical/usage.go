package canonical

import "fmt"

// Fidelity 标记用量数据的可信等级。这是计费正确性的前提。
//
// 三家的 token 口径本就不同（cache_creation_input_tokens /
// cache_read_input_tokens / reasoning_tokens 各有定义），跨协议转换后与上游
// 账单必然存在系统性偏差；流式请求中断时上游根本不会返回 usage；订阅凭据池
// 更是没有 token 计费概念。把这三种情况混为一谈，计费就一定是错的。
type Fidelity string

const (
	// FidelityUnknown 是零值。它是**非法**的——Usage.Validate 会拒绝它，
	// 迫使每条产生 Usage 的代码路径显式声明自己的可信等级。
	FidelityUnknown Fidelity = ""

	// FidelityAuthoritative 表示数值直接来自上游响应，可用于计费。
	FidelityAuthoritative Fidelity = "authoritative"

	// FidelityEstimated 表示数值由本地 tokenizer 估算，仅供限流与观测，
	// 不可作为账单依据。
	FidelityEstimated Fidelity = "estimated"

	// FidelityUnavailable 表示无法获得用量（流式中断、订阅账号等）。
	FidelityUnavailable Fidelity = "unavailable"
)

// Billable 报告这份用量是否可以作为计费依据。
// 只有 authoritative 返回 true——estimated 绝不能当作 authoritative 呈现。
func (f Fidelity) Billable() bool { return f == FidelityAuthoritative }

// Usage 是一次调用的资源用量。
type Usage struct {
	Fidelity Fidelity `json:"fidelity"`

	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`

	// ReasoningTokens 已包含在 OutputTokens 内（与 OpenAI 口径一致）。
	// Anthropic 不单列此项，转换时保持 0 而非猜测。
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`

	// 缓存相关。Anthropic 区分 creation 与 read 且两者计价不同；
	// OpenAI 只报 cached_tokens（等价于 read）。转换时不得把 read 当 creation。
	CacheReadInputTokens  int64 `json:"cache_read_input_tokens,omitempty"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens,omitempty"`

	// 多模态计量。这些维度不能折算成 token——上游按各自单位计价。
	AudioInputTokens  int64   `json:"audio_input_tokens,omitempty"`
	AudioOutputTokens int64   `json:"audio_output_tokens,omitempty"`
	AudioInputSeconds float64 `json:"audio_input_seconds,omitempty"`
	ImageCount        int64   `json:"image_count,omitempty"`
	VideoSeconds      float64 `json:"video_seconds,omitempty"`
}

// TotalTokens 返回输入与输出 token 之和。
//
// 刻意不把缓存读写计入：它们与普通 input token 的计价不同，加总会掩盖差异。
// 需要计费时应当分项处理，而不是用这个数。
func (u Usage) TotalTokens() int64 { return u.InputTokens + u.OutputTokens }

// Validate 拒绝未声明可信等级的用量数据。
func (u Usage) Validate() error {
	switch u.Fidelity {
	case FidelityAuthoritative, FidelityEstimated, FidelityUnavailable:
		return nil
	case FidelityUnknown:
		return fmt.Errorf("canonical: usage.fidelity must be set explicitly (authoritative/estimated/unavailable)")
	default:
		return fmt.Errorf("canonical: unknown usage.fidelity %q", u.Fidelity)
	}
}

// UnavailableUsage 构造一份「用量不可知」的记录。
//
// 流式请求在首字节之后失败时必须用它——此时上游不会再送出 usage 事件，
// 任何非零数字都是编造的。
func UnavailableUsage() Usage { return Usage{Fidelity: FidelityUnavailable} }
