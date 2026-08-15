package degrade

import (
	"sort"

	"github.com/yobo2u/omugw/internal/canonical"
)

// InboundFamily 是一组同源的入站协议。
//
// 按族而不是按单个协议排优先级，是因为同族协议共用编解码基础设施与错误信封
// （OpenAI Chat 与 Responses 共用 openaiwire，Responses 的矩阵路径本就从 Chat
// 派生），接入其中一个之后再接入另一个的边际成本很低。把它们拆到不同优先级
// 档位既不符合实现现实，也会让排期显得比实际更长。
type InboundFamily struct {
	Name string

	// Protocols 按族内表达力从强到弱排列。为空表示该族尚未接入。
	Protocols []Protocol
}

// Implemented 报告这个协议族是否已经接入。
func (f InboundFamily) Implemented() bool { return len(f.Protocols) > 0 }

// InboundPriority 是入站协议族的接入优先级。
//
// 排序依据是「能表达多少原生能力」，不是流行度。族内同样如此：OpenAI
// Responses 列在 Chat Completions 之前，因为它能承载推理配置、内建工具与更
// 丰富的内容类型——但两者同属一族、同一优先级档位。
//
// DashScope Native 排第二而不是最后：讲原生协议的客户端本来就不需要任何转换，
// 让它们走兼容层是净损失。
var InboundPriority = []InboundFamily{
	{Name: "OpenAI", Protocols: []Protocol{
		ProtoOpenAIResponses,
		ProtoOpenAIChat,
		ProtoOpenAIRealtime,
	}},
	// DashScope 有三个各自独立的线格式，能力集互不相同。
	// 族内按表达力排序：HTTP native 最全，realtime 次之，run-task 指令流最窄。
	{Name: "DashScope", Protocols: []Protocol{
		ProtoDashScopeNative,
		ProtoDashScopeRealtime,
		ProtoDashScopeInference,
	}},
	{Name: "Anthropic Messages"},
	{Name: "Gemini"},
}

// OutboundPreference 是出站 Provider 的默认选路偏好，越靠前越优先。
//
// 依据同样是原生能力保留度，不是延迟或成本：
//
//  1. OpenAI 兼容——字节透传，语义零损失
//  2. DashScope Compatible——同为透传，只是走 DashScope 侧的兼容层
//  3. DashScope Native——需要转换，但能表达 DashScope 独有能力
//  4. Anthropic Messages——异构转换，损失最多
//
// 这个顺序不是拍脑袋定的：TestPreferenceMatchesPreservation 会拿它与矩阵里
// 实际的透传格子数对账，两者矛盾时测试失败。也就是说，如果有人新增一条声称
// 优先、实际却丢更多能力的路径，CI 会拦下来。
var OutboundPreference = []Provider{
	ProviderOpenAICompat,
	ProviderDashScopeCompatible,
	ProviderDashScopeNative,
	ProviderAnthropicMessages,
	ProviderOpenAIRealtime,
	ProviderDashScopeWSRealtime,
	ProviderDashScopeWSInference,
}

// preferenceRank 给出 Provider 在偏好序中的位置。未登记的排在最后。
func preferenceRank(p Provider) int {
	for i, q := range OutboundPreference {
		if q == p {
			return i
		}
	}
	return len(OutboundPreference)
}

// FeatureConversationStore 是会话存储的功能开关名。
//
// 默认关闭：内存态会话在多副本部署下是错的，而这是个开源项目，用户很可能
// 那样部署，然后撞上「会话时有时无」这类最难查的 bug。
const FeatureConversationStore = "convstore"

// Availability 是部署侧的能力开关配置。
type Availability map[string]bool

// DefaultAvailability 返回默认配置：模拟类能力一律关闭。
func DefaultAvailability() Availability {
	return Availability{FeatureConversationStore: false}
}

// Enabled 报告某个开关是否开启。空开关名视为常开（无需开关的能力）。
func (a Availability) Enabled(feature string) bool {
	if feature == "" {
		return true
	}
	return a[feature]
}

// Preservation 是一条路径的原生能力保留度。
type Preservation struct {
	Passthrough int
	Emulate     int
	Degrade     int
	Reject      int

	// EmulateOff 是被运维关掉的模拟能力数，已从 Emulate 中扣除。
	EmulateOff int

	// NotRedeemed 是设计上有效、但当前尚未投放的能力数。
	//
	// 它**不从**上面几列扣除：那几列记的是设计处置，而设计不因为投放进度改变。
	// 它只影响当前可用那一列——一项还没写的能力，此刻一分都不该拿。
	NotRedeemed int

	// availableWeight 是当前真的能用到的权重和：已投放且开关可用的格子按
	// 透传/模拟计 1 分、降级计 0.5 分，其余计 0 分。
	//
	// 单独累加而不是从设计分里减，是因为两者的口径本就不同——设计分回答
	// 「这条路最终能做到什么」，它回答「此刻能做到什么」。
	availableWeight float64

	// NotApplicable 是入站协议压根表达不出来的能力数。
	//
	// 它**不进分母**。早先的版本把它算作损失，结果一条零损失的字节直通路径
	// 只拿到 0.704 分，读起来像丢了三成能力——那不是路径的问题，是分母选错了。
	NotApplicable int
}

// denominator 是可表达能力总数，即这条路径真正需要负责的范围。
func (p Preservation) denominator() int {
	return p.Passthrough + p.Emulate + p.EmulateOff + p.Degrade + p.Reject
}

// DesignScore 是**设计目标**保留度：假定全部实现、全部开关开启。
//
// 它回答「这条路最终能做到什么」，供文档读者与偏好序设计对账使用。
// 不可用于选路——按它选路会挑中一条尚未实现的路径（见 ADR-0002）。
func (p Preservation) DesignScore() float64 {
	total := p.denominator()
	if total == 0 {
		return 0
	}
	pass := p.Passthrough + p.Emulate + p.EmulateOff
	return (float64(pass) + 0.5*float64(p.Degrade)) / float64(total)
}

// AvailableScore 是**当前可用**保留度：未投放的能力与被关掉的模拟能力都不计分。
//
// 它回答「此刻真的能用到什么」，是选路的唯一依据。
func (p Preservation) AvailableScore() float64 {
	total := p.denominator()
	if total == 0 {
		return 0
	}
	return p.availableWeight / float64(total)
}

// Gated 报告这条路径的两列分数是否会不同——被开关关掉的，或还没投放的。
func (p Preservation) Gated() bool { return p.EmulateOff > 0 || p.NotRedeemed > 0 }

// Redeemed 是已投放的格子数。REJECT 按设计就该失败，不在投放之列。
func (p Preservation) Redeemed() int { return p.denominator() - p.Reject - p.NotRedeemed }

// Preservation 报告这条路径保留了多少原生能力，按矩阵当前的可用性配置计算。
//
// 设计计数与可用权重分两路累加：前者只看处置声明，是路径级的，与 ep 无关；
// 后者还要问这项能力投放到 ep 了没有——可用列永远端点相对。
// ep 未开（含零值）时可用列为零：没有门就是没有可用，只会少报，不会多报。
func (r *Route) Preservation(avail Availability, ep Endpoint) Preservation {
	var p Preservation
	for c, rule := range r.rules {
		var weight float64
		switch rule.Disposition {
		case Passthrough:
			p.Passthrough++
			weight = 1
		case Emulate:
			if avail.Enabled(rule.RequiresFeature) {
				p.Emulate++
				weight = 1
			} else {
				p.EmulateOff++
			}
		case Degrade:
			p.Degrade++
			weight = 0.5
		case Reject:
			p.Reject++
		case NotApplicable:
			p.NotApplicable++
			// N/A 不进分母，也就谈不上投放，跳过下面的计数。
			continue
		}
		if r.Redeems(ep, c) {
			p.availableWeight += weight
		} else if rule.Disposition != Reject {
			// REJECT 的格子按设计就该失败，没有「等它投放」一说，
			// 算进未投放数只会让人以为它将来会变得可用。
			p.NotRedeemed++
		}
	}
	return p
}

// RankOutbound 按选路偏好对候选 Provider 排序，并剔除没有注册路径的候选。
//
// 返回空切片表示这个入站协议对所有候选都没有可用路径——调用方必须报错，
// 不得退回到某个「默认」Provider。悄悄选一条未经声明的路径正是降级矩阵要
// 防的事。
func (m *Matrix) RankOutbound(in Protocol, candidates []Provider) []Provider {
	// 只在**已实现**的路径里选。把 PLANNED 路径纳入候选，等于让选路挑中
	// 一条注定失败的路——排序做得再对也没有意义。
	return m.rank(in, candidates, true)
}

// RankDesign 按同样的偏好序排列，但**不过滤**未实现的路径。
//
// 供两处使用：生成文档（规划中的路径也要列出来），以及对账偏好序的设计是否
// 自洽（见 ADR-0002 的「设计目标」列）。绝不可用于选路。
func (m *Matrix) RankDesign(in Protocol, candidates []Provider) []Provider {
	return m.rank(in, candidates, false)
}

func (m *Matrix) rank(in Protocol, candidates []Provider, onlyImplemented bool) []Provider {
	out := make([]Provider, 0, len(candidates))
	for _, c := range candidates {
		r, ok := m.Route(in, c)
		if !ok || (onlyImplemented && !r.Implemented()) {
			continue
		}
		out = append(out, c)
	}
	// 同源快通道永远排在最前，然后才轮到全局偏好序。
	//
	// 固定的全局顺序表达不了「同源优先」——它依赖入站协议是谁。举个真实的例子：
	// 对 dashscope.realtime 入站，DashScope 侧的直通是零损失的，而 openai.realtime
	// 需要反向重采样并丢掉 input_image_buffer；但在全局序里 openai.realtime 排得
	// 更靠前。不把同源提到最前，选路就会主动挑一条更差的路。
	homogeneous := func(p Provider) bool {
		r, ok := m.Route(in, p)
		return ok && r.IsHomogeneous()
	}
	sort.SliceStable(out, func(i, j int) bool {
		hi, hj := homogeneous(out[i]), homogeneous(out[j])
		if hi != hj {
			return hi
		}
		ri, rj := preferenceRank(out[i]), preferenceRank(out[j])
		if ri != rj {
			return ri < rj
		}
		return out[i] < out[j]
	})
	return out
}

// BestOutbound 在候选中挑出既有注册路径、又能承载全部所需能力的最优 Provider。
//
// 候选筛选保持端点无感：只看路径是否通车（至少一门已开），不看本次请求敲哪扇门。
// 端点裁决发生在每个候选内部的 Check：首选若恰因这扇门没开而失败，
// 错误会点名缺哪扇门，而后面开了这扇门的候选仍能接住请求。
//
// 全部候选都跑不通时返回**最确定**的那个错误，让调用方能告诉用户到底缺什么，
// 而不是一句笼统的「无可用 Provider」。取舍规则见 mostDefinite：
// 「这条路不支持」压过「那扇门还没开」。
func (m *Matrix) BestOutbound(in Inbound, candidates []Provider, caps []canonical.Capability) (Provider, Verdict, error) {
	ranked := m.RankOutbound(in.Protocol, candidates)
	if len(ranked) == 0 {
		// 区分「没注册」与「注册了但还没实现」：前者要改配置，后者只要等。
		for _, c := range candidates {
			if _, ok := m.Route(in.Protocol, c); ok {
				return "", Verdict{}, canonical.Newf(canonical.ClassNotImplemented,
					"入站协议 %s 的候选出站路径均已设计但尚未实现", in.Protocol)
			}
		}
		return "", Verdict{}, canonical.Newf(canonical.ClassUnsupported,
			"入站协议 %s 没有任何已注册的出站路径", in.Protocol)
	}

	var best error
	for _, p := range ranked {
		v, err := m.Check(in, p, caps)
		if err == nil {
			return p, v, nil
		}
		if mostDefinite(best, err) {
			best = err
		}
	}
	return "", Verdict{}, best
}

// mostDefinite 报告 next 是否比 cur 更该讲给用户听。
//
// 501 与 422 对用户是两条相反的指令：前者说「等」，后者说「改请求」。
// 候选筛选是端点无感的，排在后面的候选很容易恰好没开这扇门，凭空产出 501；
// 若按「最后一个错误」作答，首选那句确定的「这条路不支持音频」就会被盖掉，
// 用户被支去等一个永远不会让他成功的实现。
//
// 于是只有一条规则：确定的答案（REJECT 的 422、解码器 bug 的 500）压过
// 「还没建好」（501）。同类之间保留排序最前的那个——它离用户真正想走的路最近。
func mostDefinite(cur, next error) bool {
	if cur == nil {
		return true
	}
	return canonical.AsError(cur).Class == canonical.ClassNotImplemented &&
		canonical.AsError(next).Class != canonical.ClassNotImplemented
}
