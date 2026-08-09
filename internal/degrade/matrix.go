// Package degrade 实现降级矩阵：把「这条转换路径对这项能力怎么处置」变成
// 可查询、可测试、编译期强制完整的数据。
//
// 存在的理由（原则 2.1）：Canonical 转换是有损的。OpenAI Responses 有状态而
// Anthropic Messages 无状态；Anthropic 的 thinking 带签名而其他协议没有；
// 三家的 prompt cache 语义互斥。这些差异不可能靠一个「统一模型」抹平。
//
// 唯一可行的做法是把每一处损失显式登记下来，并让未登记的组合**失败**而不是
// 静默丢字段。一个被悄悄丢掉的 cache_control 不会报错，只会让用户在月底看到
// 十倍的账单。
package degrade

import (
	"fmt"
	"sort"
	"strings"

	"github.com/yobo2u/omugw/internal/canonical"
)

// Protocol 是入站协议标识。
type Protocol string

const (
	ProtoOpenAIChat      Protocol = "openai.chat"
	ProtoOpenAIResponses Protocol = "openai.responses"
	ProtoOpenAIRealtime  Protocol = "openai.realtime"
	ProtoOpenAIAudio     Protocol = "openai.audio"
	ProtoOpenAIImages    Protocol = "openai.images"
	ProtoDashScopeNative Protocol = "dashscope.native"
)

// Provider 是出站 Provider 标识。
//
// DashScope 的 WebSocket 拆成两个 Provider 而不是一个，因为它实际存在两代
// 互不兼容的协议：/api-ws/v1/inference 是 run-task 指令流（Paraformer、
// CosyVoice），/api-ws/v1/realtime 是与 OpenAI Realtime 同构的事件流
// （Qwen-Omni / Audio / TTS / ASR Realtime）。混为一谈会让转换器无从下手。
type Provider string

const (
	ProviderOpenAICompat         Provider = "openai.compat"
	ProviderOpenAIRealtime       Provider = "openai.realtime"
	ProviderAnthropicMessages    Provider = "anthropic.messages"
	ProviderDashScopeCompatible  Provider = "dashscope.compatible"
	ProviderDashScopeNative      Provider = "dashscope.native"
	ProviderDashScopeWSInference Provider = "dashscope.ws.inference"
	ProviderDashScopeWSRealtime  Provider = "dashscope.ws.realtime"
)

// Disposition 是对一项能力的处置。
type Disposition string

const (
	// Passthrough：能力被完整传递给上游，无语义损失。
	Passthrough Disposition = "PASSTHROUGH"

	// Degrade：请求仍然有效，但这项能力的某些语义被丢弃。
	// Note 必须写清楚**丢了什么**，因为它会出现在响应头里告知客户端。
	Degrade Disposition = "DEGRADE"

	// Reject：这条路径无法支持该能力，请求必须失败。
	// Note 必须写清楚**为什么**，它会成为错误消息的一部分。
	Reject Disposition = "REJECT"
)

// Rule 是矩阵中的一格。
type Rule struct {
	Disposition Disposition
	Note        string
}

type cell struct {
	in  Protocol
	out Provider
	cap canonical.Capability
}

// Route 是一条 (入站协议 → 出站 Provider) 转换路径。
type Route struct {
	In  Protocol
	Out Provider

	// Homogeneous 标记同源快通道（原则 2.2）。同源路径可以字节级透传，
	// 不进 Canonical——这既保住了 TTFT，也绕开了绝大多数转换 bug。
	Homogeneous bool

	rules map[canonical.Capability]Rule
	errs  []string
}

// NewRoute 开始声明一条路径。必须以 Build 结尾。
func NewRoute(in Protocol, out Provider) *Route {
	return &Route{In: in, Out: out, rules: map[canonical.Capability]Rule{}}
}

// Homogeneous 把这条路径标记为同源快通道。
func (r *Route) MarkHomogeneous() *Route {
	r.Homogeneous = true
	return r
}

func (r *Route) set(c canonical.Capability, rule Rule) {
	if _, dup := r.rules[c]; dup {
		r.errs = append(r.errs, fmt.Sprintf("capability %q declared twice", c))
		return
	}
	r.rules[c] = rule
}

// Pass 声明若干项能力可以无损透传。
func (r *Route) Pass(caps ...canonical.Capability) *Route {
	for _, c := range caps {
		r.set(c, Rule{Disposition: Passthrough})
	}
	return r
}

// Degrade 声明若干项能力会被降级，note 说明丢失了什么。
func (r *Route) Degrade(note string, caps ...canonical.Capability) *Route {
	for _, c := range caps {
		r.set(c, Rule{Disposition: Degrade, Note: note})
	}
	return r
}

// Reject 声明若干项能力不被支持，note 说明原因。
func (r *Route) Reject(note string, caps ...canonical.Capability) *Route {
	for _, c := range caps {
		r.set(c, Rule{Disposition: Reject, Note: note})
	}
	return r
}

// Build 校验这条路径已经对**每一项**能力表态。
//
// 这是整个包的核心约束。漏掉一格就编译不过测试，而不是等到线上才发现某个
// 字段被吞了。新增 Capability 常量时，所有已注册路径都会在这里失败——
// 这是刻意的设计，不是需要绕过的麻烦。
func (r *Route) Build() (*Route, error) {
	var missing []string
	for _, c := range canonical.AllCapabilities() {
		if _, ok := r.rules[c]; !ok {
			missing = append(missing, string(c))
		}
	}
	if len(missing) > 0 {
		r.errs = append(r.errs, "undeclared capabilities: "+strings.Join(missing, ", "))
	}
	for c, rule := range r.rules {
		if rule.Disposition != Passthrough && rule.Note == "" {
			r.errs = append(r.errs, fmt.Sprintf("capability %q is %s but carries no note", c, rule.Disposition))
		}
	}
	if len(r.errs) > 0 {
		sort.Strings(r.errs)
		return nil, fmt.Errorf("degrade: route %s -> %s: %s", r.In, r.Out, strings.Join(r.errs, "; "))
	}
	return r, nil
}

// Matrix 是全部路径的集合。
type Matrix struct {
	routes map[[2]string]*Route
}

// NewMatrix 创建空矩阵。
func NewMatrix() *Matrix { return &Matrix{routes: map[[2]string]*Route{}} }

// Add 注册一条已 Build 的路径。
func (m *Matrix) Add(r *Route, err error) error {
	if err != nil {
		return err
	}
	k := [2]string{string(r.In), string(r.Out)}
	if _, dup := m.routes[k]; dup {
		return fmt.Errorf("degrade: duplicate route %s -> %s", r.In, r.Out)
	}
	m.routes[k] = r
	return nil
}

// Route 取出一条路径。
func (m *Matrix) Route(in Protocol, out Provider) (*Route, bool) {
	r, ok := m.routes[[2]string{string(in), string(out)}]
	return r, ok
}

// Routes 返回全部已注册路径，按 (入站, 出站) 字典序排列，便于生成稳定文档。
func (m *Matrix) Routes() []*Route {
	out := make([]*Route, 0, len(m.routes))
	for _, r := range m.routes {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].In != out[j].In {
			return out[i].In < out[j].In
		}
		return out[i].Out < out[j].Out
	})
	return out
}

// Lookup 查询单个格子。
func (m *Matrix) Lookup(in Protocol, out Provider, c canonical.Capability) (Rule, bool) {
	r, ok := m.Route(in, out)
	if !ok {
		return Rule{}, false
	}
	rule, ok := r.rules[c]
	return rule, ok
}

// Verdict 是一次能力检查的结论。
type Verdict struct {
	// Degraded 列出被降级的能力及原因，应当作为响应头返回给客户端——
	// 客户端有权知道它请求的 cache_control 并没有生效。
	Degraded []CapabilityNote
}

// CapabilityNote 把一项能力与其处置说明绑在一起。
type CapabilityNote struct {
	Capability canonical.Capability
	Note       string
}

// Check 用请求实际用到的能力集去查矩阵。
//
// 任何一项判定为 Reject，或路径/格子未注册，都返回错误——**绝不静默放行**。
// 未注册按 Reject 处理是刻意的失败方向：漏配一格的后果是「这个请求被拒绝」，
// 而不是「这个请求丢了半数字段还返回了 200」。
func (m *Matrix) Check(in Protocol, out Provider, caps []canonical.Capability) (Verdict, error) {
	r, ok := m.Route(in, out)
	if !ok {
		return Verdict{}, canonical.Newf(canonical.ClassUnsupported,
			"未注册的转换路径 %s -> %s", in, out)
	}

	var v Verdict
	for _, c := range caps {
		rule, ok := r.rules[c]
		if !ok {
			return Verdict{}, canonical.Newf(canonical.ClassUnsupported,
				"转换路径 %s -> %s 未对能力 %q 作出声明", in, out, c)
		}
		switch rule.Disposition {
		case Reject:
			return Verdict{}, canonical.Newf(canonical.ClassUnsupported,
				"转换路径 %s -> %s 不支持 %q：%s", in, out, c, rule.Note)
		case Degrade:
			v.Degraded = append(v.Degraded, CapabilityNote{Capability: c, Note: rule.Note})
		}
	}
	return v, nil
}

// Header 把降级信息压成一个响应头值，形如
// "prompt_cache=缓存意图被丢弃; structured_output=strict 校验不可用"。
func (v Verdict) Header() string {
	if len(v.Degraded) == 0 {
		return ""
	}
	parts := make([]string, 0, len(v.Degraded))
	for _, d := range v.Degraded {
		parts = append(parts, fmt.Sprintf("%s=%s", d.Capability, d.Note))
	}
	return strings.Join(parts, "; ")
}

// DegradationHeader 是承载降级说明的响应头名。
const DegradationHeader = "X-Omugw-Degraded"
