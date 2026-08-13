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

	// OpenAI 的 /v1/audio 与 /v1/images 端点尚未设计可表达性声明，因此这里
	// 不预留常量。留一个没有声明的协议常量是陷阱：谁拿它建路径，会撞上
	// 「缺少可表达性声明」这种在说代码问题的错误，而真相是「这个协议还没设计」。
	// 真要做时连同声明一起加，一次想清楚。roadmap 见 README。

	// DashScope 有三个各自独立的入站协议，对应三种线格式。
	// 把它们合成一个「DashScope 协议」会让转换器无从下手——
	// 它们的消息模型、传输方式与能力集都不一样。
	ProtoDashScopeNative    Protocol = "dashscope.native"    // HTTP
	ProtoDashScopeRealtime  Protocol = "dashscope.realtime"  // /api-ws/v1/realtime，事件流
	ProtoDashScopeInference Protocol = "dashscope.inference" // /api-ws/v1/inference，run-task 指令流
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

	// Emulate：上游不提供该能力，由网关自行实现。
	//
	// 与 Passthrough 分开是因为它有真实的运维代价——网关要替上游保管状态，
	// 于是有了存储、过期、多副本一致性这些新问题。客户端看到的能力是完整的，
	// 但运维要知道这份完整性是网关垫出来的。Note 必须写明代价。
	Emulate Disposition = "EMULATE"

	// NotApplicable：该能力在这条路径上不可达。
	//
	// 它不是「拒绝」——入站协议根本没有字段可以表达它，客户端连发都发不出来。
	// 由 Expressibility 自动推导，不需要也不允许手工声明。不计入保留度分母：
	// 一条字节直通的路径不该因为「客户端表达不了 computer_use」而被扣分。
	NotApplicable Disposition = "N/A"
)

// Rule 是矩阵中的一格。
type Rule struct {
	Disposition Disposition
	Note        string

	// RequiresFeature 是这项能力依赖的运行时开关名，仅 Emulate 使用。
	//
	// 网关垫出来的能力不一定默认开着——会话存储在多副本部署下是错的，
	// 所以默认关闭。这个字段让「当前可用保留度」能算出配置生效后的真实结果，
	// 而不是假装所有能力都在线。
	RequiresFeature string
}

// Route 是一条 (入站协议 → 出站 Provider) 转换路径。
type Route struct {
	In  Protocol
	Out Provider

	// Homogeneous 标记同源快通道（原则 2.2）。同源路径可以字节级透传，
	// 不进 Canonical——这既保住了 TTFT，也绕开了绝大多数转换 bug。
	Homogeneous bool

	rules map[canonical.Capability]Rule

	// redeemed 是这条路径**当前真的投放了**的能力集合。
	//
	// 与 rules 的分工是整个包最容易混淆、也最要紧的一处：rules 是**设计处置**
	// ——这条路最终该怎么对待每项能力；redeemed 是**当前投放**——此刻哪些能力
	// 背后真的有实现。
	//
	// 早先这里是一个布尔字段，粒度是整条路径。DashScope Native 撞破了它：
	// 一个协议对应文本生成、multimodal、embedding、rerank 多个上游端点，
	// 本期只写了文本生成那一个，而那个布尔一置位，矩阵就替另外三个端点
	// 也作了保证。默认为空，投放一项登记一项。
	redeemed map[canonical.Capability]bool

	errs []string
}

// NewRoute 开始声明一条路径。必须以 Build 结尾。
func NewRoute(in Protocol, out Provider) *Route {
	return &Route{
		In: in, Out: out,
		rules:    map[canonical.Capability]Rule{},
		redeemed: map[canonical.Capability]bool{},
	}
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

// Emulate 声明若干项能力由网关自行实现。
//
// feature 是它依赖的运行时开关名；note 必须写明运维代价。两者都是必填——
// 一个没有开关的模拟能力意味着运维无法拒绝它带来的风险，一个没有说明的模拟
// 能力意味着运维不知道风险是什么。
func (r *Route) Emulate(feature, note string, caps ...canonical.Capability) *Route {
	for _, c := range caps {
		r.set(c, Rule{Disposition: Emulate, Note: note, RequiresFeature: feature})
	}
	return r
}

// Redeem 登记若干项能力**当前已经投放**。
//
// 只应在这些能力的端到端 fixture 已经存在并通过之后调用；
// TestImplementedRoutesHaveFixtures 与 TestRedeemedCapabilitiesAreExplicit
// 会强制这一点。
func (r *Route) Redeem(caps ...canonical.Capability) *Route {
	for _, c := range caps {
		r.redeemed[c] = true
	}
	return r
}

// Implemented 报告这条路径是否至少投放了一项能力，即它是否已经通车。
//
// 选路与文档用它区分「已实现」和「规划中」；单项能力能不能走，要另问 Redeems。
func (r *Route) Implemented() bool { return len(r.redeemed) > 0 }

// Redeems 报告某一项能力当前是否已投放。
func (r *Route) Redeems(c canonical.Capability) bool { return r.redeemed[c] }

// Derive 以另一条已声明的路径为基准创建新路径，用于协议族内部的近似路径
// （例如 OpenAI Chat 与 OpenAI Responses 面对同一个出站 Provider）。
//
// 它不是省事的手段。逐字复制 27 条声明不会让人多想一遍，只会增加复制粘贴的
// 出错面；而 Derive + Override 把「这两条路径究竟哪里不同」变成代码里能一眼
// 读到的答案。Build 的完整性校验对派生路径同样生效。
//
// 兑现集合不在继承之列：它说的是「这条路径的实现写好了」，而实现是逐条写的。
// 继承它会让一条还没动工的派生路径宣称自己可用。
func (r *Route) Derive(in Protocol, out Provider) *Route {
	n := NewRoute(in, out)
	n.Homogeneous = r.Homogeneous
	for c, rule := range r.rules {
		// 不继承自动补上的 NotApplicable。它反映的是**基准协议**的表达力，
		// 换一个入站协议就未必成立——Responses 表达得出 computer_use，
		// 而 Chat 表达不出来。继承它会让派生路径既无法为该能力表态，
		// 又在 Build 时被判成「声明了表达不出来的能力」。
		if rule.Disposition == NotApplicable {
			continue
		}
		n.rules[c] = rule
	}
	return n
}

// Override 覆盖继承来的声明。note 必须说明为什么这条路径与基准不同。
//
// 只能覆盖已存在的声明——对一个从未声明过的能力谈「覆盖」没有意义，
// 那种情况说明基准路径选错了。
func (r *Route) Override(c canonical.Capability, d Disposition, note string) *Route {
	if _, ok := r.rules[c]; !ok {
		r.errs = append(r.errs,
			fmt.Sprintf("capability %q was never declared, nothing to override", c))
		return r
	}
	r.rules[c] = Rule{Disposition: d, Note: note}
	return r
}

// Build 校验这条路径已经对**每一项**能力表态。
//
// 这是整个包的核心约束。漏掉一格就编译不过测试，而不是等到线上才发现某个
// 字段被吞了。新增 Capability 常量时，所有已注册路径都会在这里失败——
// 这是刻意的设计，不是需要绕过的麻烦。
func (r *Route) Build() (*Route, error) {
	spec, ok := expressible[r.In]
	if !ok {
		r.errs = append(r.errs,
			fmt.Sprintf("inbound protocol %q has no expressibility declaration", r.In))
		sort.Strings(r.errs)
		return nil, fmt.Errorf("degrade: route %s -> %s: %s", r.In, r.Out, strings.Join(r.errs, "; "))
	}

	// 只有入站协议表达得出来的能力才需要路径声明。其余的由 Expressibility
	// 自动补成 NotApplicable——让每条路径为客户端根本发不出来的字段辩解，
	// 既是无谓的负担，也会把「不适用」误算成「有损失」。
	want := map[canonical.Capability]bool{}
	for _, c := range ExpressibleSet(r.In) {
		want[c] = true
	}

	var missing, extra []string
	for c := range want {
		if _, ok := r.rules[c]; !ok {
			missing = append(missing, string(c))
		}
	}
	for c := range r.rules {
		if !want[c] {
			extra = append(extra, string(c))
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		r.errs = append(r.errs, "undeclared capabilities: "+strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		// 为一个客户端表达不出来的能力写声明，说明作者对协议的理解有偏差。
		// 与其静默忽略，不如让它失败。
		r.errs = append(r.errs, "declared but not expressible by "+string(r.In)+": "+
			strings.Join(extra, ", "))
	}

	for _, c := range canonical.AllCapabilities() {
		if want[c] {
			continue
		}
		note := ""
		if target, ok := spec.Elsewhere[c]; ok {
			note = fmt.Sprintf("%s 表达不了该能力，请改用入站协议 %s", r.In, target)
		} else if why, ok := spec.Impossible[c]; ok {
			note = why
		}
		r.rules[c] = Rule{Disposition: NotApplicable, Note: note}
	}

	// 兑现一个 REJECT 或 N/A 的格子没有内容可言——前者按设计就该失败，
	// 后者客户端连发都发不出来。让它在这里失败，好过在文档里显示成「已投放」，
	// 那是在为一个不存在的东西背书。
	var undeliverable []string
	for c := range r.redeemed {
		rule, ok := r.rules[c]
		if !ok || rule.Disposition == Reject || rule.Disposition == NotApplicable {
			undeliverable = append(undeliverable, string(c))
		}
	}
	if len(undeliverable) > 0 {
		sort.Strings(undeliverable)
		r.errs = append(r.errs, "redeemed but not deliverable: "+strings.Join(undeliverable, ", "))
	}

	for c, rule := range r.rules {
		if rule.Disposition != Passthrough && rule.Note == "" {
			r.errs = append(r.errs, fmt.Sprintf("capability %q is %s but carries no note", c, rule.Disposition))
		}
		// 没有开关的模拟能力意味着运维无法拒绝它带来的风险。
		if rule.Disposition == Emulate && rule.RequiresFeature == "" {
			r.errs = append(r.errs,
				fmt.Sprintf("capability %q is EMULATE but declares no feature gate", c))
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
	avail  Availability
}

// NewMatrix 创建空矩阵，采用默认可用性配置。
func NewMatrix() *Matrix {
	return &Matrix{routes: map[[2]string]*Route{}, avail: DefaultAvailability()}
}

// WithAvailability 应用部署侧的能力开关配置，返回自身以便链式调用。
//
// 可用性挂在矩阵上而不是每次 Check 传参：它是部署期常量，而 Check 在每个请求
// 上都会跑。把一个不变的东西反复传进热路径，只会让调用点变吵。
func (m *Matrix) WithAvailability(a Availability) *Matrix {
	m.avail = a
	return m
}

// Availability 返回当前生效的能力开关配置。
func (m *Matrix) Availability() Availability { return m.avail }

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

	// Emulated 列出由网关代为实现的能力。
	//
	// 它同样要告知客户端，但含义与降级相反：能力是完整的，只是这份完整性由
	// 网关垫出来，因而带着网关侧的可用性边界（比如内存态会话在重启后丢失）。
	Emulated []CapabilityNote
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

	// 已设计但未实现的路径必须明确报错。放行会让请求走进一个空壳，
	// 客户端拿到的是一个语焉不详的 5xx，而真相是「这条路还没建」。
	if !r.Implemented() {
		return Verdict{}, canonical.Newf(canonical.ClassNotImplemented,
			"转换路径 %s -> %s 已在降级矩阵中设计，但实现尚未落地", in, out)
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
		case NotApplicable:
			// 入站协议表达不出这项能力，却在请求里出现了——说明入站解码器
			// 把某个字段解成了它不该解成的东西。这是网关自己的 bug，
			// 不能当成客户端的问题放行。
			return Verdict{}, canonical.Newf(canonical.ClassInternal,
				"入站协议 %s 不应产生能力 %q，解码器可能有误：%s", in, c, rule.Note)
		}

		// 走到这里的处置在设计上都是有效的，于是才轮到问「投放了没有」。
		// 顺序不能反：REJECT 与 N/A 有各自更确切的错误分类，先问投放会把
		// 「这条路不支持」说成「这条路还没建」。
		//
		// 501 而不是 422：未投放的能力该等实现，不该让客户端改请求。
		if !r.Redeems(c) {
			return Verdict{}, canonical.Newf(canonical.ClassNotImplemented,
				"转换路径 %s -> %s 已声明能力 %q，但当前尚未投放", in, out, c)
		}

		switch rule.Disposition {
		case Degrade:
			v.Degraded = append(v.Degraded, CapabilityNote{Capability: c, Note: rule.Note})
		case Emulate:
			// 模拟能力可以被运维关掉。关掉时它的行为与 Reject 一致——
			// 但错误消息必须说清是「开关没开」而不是「这条路不支持」，
			// 否则运维会去查一个根本没问题的转换路径。
			if !m.avail.Enabled(rule.RequiresFeature) {
				return Verdict{}, canonical.Newf(canonical.ClassUnsupported,
					"能力 %q 在路径 %s -> %s 上由网关模拟提供，但功能开关 %q 未开启：%s",
					c, in, out, rule.RequiresFeature, rule.Note)
			}
			v.Emulated = append(v.Emulated, CapabilityNote{Capability: c, Note: rule.Note})
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
