package canonical

import (
	"encoding/json"
	"errors"
	"fmt"
)

// PartKind 标识一个内容块的类型。Part 采用带判别字段的结构体而非接口，
// 因为 fixture 测试需要把它无歧义地序列化成 golden file 再读回来。
type PartKind string

const (
	PartText       PartKind = "text"
	PartThinking   PartKind = "thinking"
	PartRefusal    PartKind = "refusal"
	PartMedia      PartKind = "media"
	PartToolCall   PartKind = "tool_call"
	PartToolResult PartKind = "tool_result"
)

// Part 是消息内容的最小单元。
//
// 同一时刻只有与 Kind 对应的那个字段有意义；Validate 会强制这一点，
// 避免转换器读到本不该存在的字段。
type Part struct {
	Kind PartKind `json:"kind"`

	// Text 用于 PartText 和 PartRefusal。
	Text string `json:"text,omitempty"`

	Thinking   *Thinking   `json:"thinking,omitempty"`
	Media      *Media      `json:"media,omitempty"`
	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
}

// Thinking 是推理内容块。
//
// Signature 是 Anthropic extended thinking 的签名，多轮 tool use 时必须原样
// 回传才有效。一旦跨协议转换，签名无法重建——降级矩阵中
// CapReasoningSignature 对所有异构路径都必须是 REJECT，而不是悄悄丢弃。
type Thinking struct {
	Text      string `json:"text,omitempty"`
	Signature string `json:"signature,omitempty"`
	// Redacted 表示上游返回的是加密/删节后的推理内容，本网关不得尝试解析。
	Redacted bool `json:"redacted,omitempty"`
}

// MediaKind 区分媒体类别，决定它映射到各协议的哪种内容块。
type MediaKind string

const (
	MediaImage MediaKind = "image"
	MediaAudio MediaKind = "audio"
	MediaVideo MediaKind = "video"
	MediaFile  MediaKind = "file"
)

// Media 是多模态负载。三种承载形态互斥，对应原则 2.6 的三条策略：
//
//	URL     直接透传给上游，网关不代下载（否则会变成流量黑洞）
//	Data    内联字节，受 MaxInlineBytes 限制，超限显式报错
//	FileRef 上游文件 API 的引用，绑定具体 Provider；跨 Provider 时必须 REJECT
type Media struct {
	Kind     MediaKind `json:"kind"`
	MIMEType string    `json:"mime_type,omitempty"`

	URL     string    `json:"url,omitempty"`
	Data    []byte    `json:"data,omitempty"`
	FileRef *FileRef  `json:"file_ref,omitempty"`
	Audio   *AudioFmt `json:"audio,omitempty"`
}

// FileRef 指向某个 Provider 的文件 API 对象（OpenAI Files、Gemini File API、
// DashScope OSS 等）。Provider 字段存在的唯一目的，是让转换层能在跨 Provider
// 时立刻发现这个引用不可迁移。
type FileRef struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

// AudioFmt 描述 PCM 音频的采样参数。
//
// 这不是可选的元数据：OpenAI Realtime 输入为 24 kHz、DashScope Realtime 输入为
// 16 kHz，两者输出同为 24 kHz。转换路径必须知道当前采样率才能决定是否重采样。
type AudioFmt struct {
	Encoding   string `json:"encoding"` // "pcm16" | "g711_ulaw" | "g711_alaw"
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
}

var (
	ErrPartKindMismatch = errors.New("canonical: part payload does not match kind")
	ErrMediaSource      = errors.New("canonical: media must carry exactly one of url/data/file_ref")
)

// Validate 检查 Part 的判别字段与实际负载一致。转换器在信任一个 Part 之前
// 应当调用它——一个 Kind 为 text 却带着 ToolCall 的 Part 说明上游解码器有 bug，
// 早失败比把错误数据带进 Provider 请求便宜。
func (p Part) Validate() error {
	set := 0
	if p.Thinking != nil {
		set++
	}
	if p.Media != nil {
		set++
	}
	if p.ToolCall != nil {
		set++
	}
	if p.ToolResult != nil {
		set++
	}

	switch p.Kind {
	case PartText, PartRefusal:
		if set != 0 {
			return fmt.Errorf("%w: kind=%s carries a structured payload", ErrPartKindMismatch, p.Kind)
		}
	case PartThinking:
		if p.Thinking == nil || set != 1 {
			return fmt.Errorf("%w: kind=thinking requires exactly the thinking payload", ErrPartKindMismatch)
		}
	case PartMedia:
		if p.Media == nil || set != 1 {
			return fmt.Errorf("%w: kind=media requires exactly the media payload", ErrPartKindMismatch)
		}
		return p.Media.Validate()
	case PartToolCall:
		if p.ToolCall == nil || set != 1 {
			return fmt.Errorf("%w: kind=tool_call requires exactly the tool_call payload", ErrPartKindMismatch)
		}
	case PartToolResult:
		if p.ToolResult == nil || set != 1 {
			return fmt.Errorf("%w: kind=tool_result requires exactly the tool_result payload", ErrPartKindMismatch)
		}
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrPartKindMismatch, p.Kind)
	}
	return nil
}

// Validate 强制 Media 的三种承载形态互斥。
func (m *Media) Validate() error {
	set := 0
	if m.URL != "" {
		set++
	}
	if len(m.Data) > 0 {
		set++
	}
	if m.FileRef != nil {
		set++
	}
	if set != 1 {
		return fmt.Errorf("%w (got %d)", ErrMediaSource, set)
	}
	return nil
}

// InlineBytes 返回内联负载的大小，供入口处的大小上限检查使用。
func (m *Media) InlineBytes() int {
	return len(m.Data)
}

// Text 构造纯文本内容块。
func Text(s string) Part { return Part{Kind: PartText, Text: s} }

// Refusal 构造拒答内容块。OpenAI 有独立的 refusal 字段，Anthropic 没有——
// 这个差异由降级矩阵处理，而不是在这里抹平。
func Refusal(s string) Part { return Part{Kind: PartRefusal, Text: s} }

// ImageURL 构造引用式图片内容块。网关不会去下载这个 URL。
func ImageURL(url, mime string) Part {
	return Part{Kind: PartMedia, Media: &Media{Kind: MediaImage, MIMEType: mime, URL: url}}
}

// ImageData 构造内联图片内容块。调用方负责先做大小上限检查。
func ImageData(data []byte, mime string) Part {
	return Part{Kind: PartMedia, Media: &Media{Kind: MediaImage, MIMEType: mime, Data: data}}
}

// AudioData 构造内联音频内容块，必须带采样参数——Realtime 转换依赖它决定重采样。
func AudioData(data []byte, f AudioFmt) Part {
	return Part{Kind: PartMedia, Media: &Media{Kind: MediaAudio, Data: data, Audio: &f}}
}

// Extensions 保存无法无损映射到 Canonical 的 Vendor 私有字段，按 Provider 族分桶。
//
// 它的用途是**同源快通道下的原样回填**，不是「转换失败时的万能兜底」。
// 异构转换路径不得从 Extensions 里猜测语义——那正是降级矩阵存在的原因。
type Extensions map[string]json.RawMessage

const (
	ExtOpenAI    = "openai"
	ExtAnthropic = "anthropic"
	ExtDashScope = "dashscope"
	ExtGoogle    = "google"
)

// Get 取出某个 Provider 族的私有字段。
func (e Extensions) Get(family string) (json.RawMessage, bool) {
	if e == nil {
		return nil, false
	}
	v, ok := e[family]
	return v, ok
}

// Set 写入某个 Provider 族的私有字段。
func (e *Extensions) Set(family string, raw json.RawMessage) {
	if *e == nil {
		*e = Extensions{}
	}
	(*e)[family] = raw
}
