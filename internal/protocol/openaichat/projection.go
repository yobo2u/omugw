// 入站边界的严格 Chat 视图，供异构出站消费。
//
// Canonical 是有损的：采样与输出选项没有落点，而异构出站又读不得 Extensions
// ——那是同源快通道专属的原样回填通道。这两条一夹，客户端显式提交的字段就会
// 在转换途中无声消失。此文件把这些 wire 事实显式解出来，谁需要谁读。
package openaichat

import (
	"encoding/json"
	"strings"

	"github.com/yobo2u/omugw/internal/canonical"
)

// decodeStreamOptions 严格解码流式选项。
//
// 另起一个 DisallowUnknownFields 的解码器：外层的严格模式只管顶层字段，
// RawMessage 子树绕过了它。未知子字段必须在入站就拒掉。
func decodeStreamOptions(raw json.RawMessage) (*StreamOptions, error) {
	var so StreamOptions
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&so); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest,
			"stream_options 无法解析")
	}
	return &so, nil
}

// StreamOptionsIncludeUsage 报告客户端是否要求流式末尾的 usage chunk。
// 缺省或显式 false 都返回 false。
func (d *Decoded) StreamOptionsIncludeUsage() bool {
	return d.streamOptions != nil && d.streamOptions.IncludeUsage != nil && *d.streamOptions.IncludeUsage
}

// Projection 是 Chat 请求的具名严格投影，供异构出站消费。
//
// Canonical 是有损的，n / presence_penalty / logprobs / parallel_tool_calls 等
// 采样选项没有落点；异构 translator 又读不得 Extensions。投影在解码边界把这些
// 字段显式解出，并对「无 Native 落点」的字段只记录是否显式提交，供 422 判定。
type Projection struct {
	N                   *int
	PresencePenalty     *float64
	Logprobs            *bool
	TopLogprobs         *int
	ParallelToolCalls   *bool
	MaxTokens           *int
	MaxCompletionTokens *int

	// 以下字段在 DashScope Native 无落点。投影不搬运它们的值，只记录
	// 「客户端是否显式提交了它」——值本身由 422 规则点名拒绝，无需读取。
	FrequencyPenaltyPresent bool
	LogitBiasPresent        bool
	ServiceTierPresent      bool
	StorePresent            bool
	UserPresent             bool
	MetadataPresent         bool
	AudioPresent            bool

	// StreamOptionsIncludeUsage 供流式 usage chunk 决策。
	StreamOptionsIncludeUsage bool

	// WebSearch 报告客户端是否提交了非 null 的 web_search_options。
	// Native 编码器据此发 enable_search:true；选项本身的丢失登记在矩阵。
	WebSearch bool
}

// Project 严格解码 Chat 请求为投影。未知字段报错，不静默丢弃。
func Project(body []byte) (*Projection, error) {
	var w Request
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest,
			"无法解析 Chat Completions 请求")
	}
	// presence 只看顶层对象是否含 key；Request 字段的 Go 零值无法区分缺省、
	// 显式空串、空对象与 null。严格字段白名单仍由上面的 Request 解码负责。
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest,
			"无法解析 Chat Completions 请求字段")
	}
	p := &Projection{
		N:                   w.N,
		PresencePenalty:     w.PresencePenalty,
		Logprobs:            w.Logprobs,
		TopLogprobs:         w.TopLogprobs,
		ParallelToolCalls:   w.ParallelToolCalls,
		MaxTokens:           w.MaxTokens,
		MaxCompletionTokens: w.MaxCompletionTokens,
	}
	_, p.FrequencyPenaltyPresent = fields["frequency_penalty"]
	_, p.LogitBiasPresent = fields["logit_bias"]
	_, p.ServiceTierPresent = fields["service_tier"]
	_, p.StorePresent = fields["store"]
	_, p.UserPresent = fields["user"]
	_, p.MetadataPresent = fields["metadata"]
	_, p.AudioPresent = fields["audio"]
	if len(w.StreamOptions) > 0 && string(w.StreamOptions) != "null" {
		so, err := decodeStreamOptions(w.StreamOptions)
		if err != nil {
			return nil, err
		}
		p.StreamOptionsIncludeUsage = so.IncludeUsage != nil && *so.IncludeUsage
	}
	// web_search_options：出现且非 null 即开启搜索。严格子解码复用既有的
	// decodeWebSearchOptions（capability.go），未知子字段同样 400。
	if len(w.WebSearchOptions) > 0 && string(w.WebSearchOptions) != "null" {
		if _, err := decodeWebSearchOptions(w.WebSearchOptions); err != nil {
			return nil, err
		}
		p.WebSearch = true
	}
	return p, nil
}
