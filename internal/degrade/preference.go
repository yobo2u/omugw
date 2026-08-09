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

// Preservation 是一条路径的原生能力保留度。
type Preservation struct {
	Passthrough int
	Emulate     int
	Degrade     int
	Reject      int

	// NotApplicable 是入站协议压根表达不出来的能力数。
	//
	// 它**不进分母**。早先的版本把它算作损失，结果一条零损失的字节直通路径
	// 只拿到 0.704 分，读起来像丢了三成能力——那不是路径的问题，是分母选错了。
	NotApplicable int
}

// Score 把保留度压成一个可比较的数值。
//
// 分母只算入站协议**表达得出来**的能力：客户端发不出来的东西，这条路径没有
// 义务为它负责。
//
// 透传与网关模拟计满分（客户端拿到的能力是完整的），降级计半分，拒绝计零分。
// 降级不是零分是因为请求仍然成功，只是丢了部分语义——把它和「直接失败」等同
// 看待，会让选路偏向一条谁都用不了的路径。
func (p Preservation) Score() float64 {
	total := p.Passthrough + p.Emulate + p.Degrade + p.Reject
	if total == 0 {
		return 0
	}
	return (float64(p.Passthrough+p.Emulate) + 0.5*float64(p.Degrade)) / float64(total)
}

// Preservation 报告这条路径保留了多少原生能力。
func (r *Route) Preservation() Preservation {
	var p Preservation
	for _, rule := range r.rules {
		switch rule.Disposition {
		case Passthrough:
			p.Passthrough++
		case Emulate:
			p.Emulate++
		case Degrade:
			p.Degrade++
		case Reject:
			p.Reject++
		case NotApplicable:
			p.NotApplicable++
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
	out := make([]Provider, 0, len(candidates))
	for _, c := range candidates {
		if _, ok := m.Route(in, c); ok {
			out = append(out, c)
		}
	}
	// 同源快通道永远排在最前，然后才轮到全局偏好序。
	//
	// 固定的全局顺序表达不了「同源优先」——它依赖入站协议是谁。举个真实的例子：
	// 对 dashscope.realtime 入站，DashScope 侧的直通是零损失的，而 openai.realtime
	// 需要反向重采样并丢掉 input_image_buffer；但在全局序里 openai.realtime 排得
	// 更靠前。不把同源提到最前，选路就会主动挑一条更差的路。
	homogeneous := func(p Provider) bool {
		r, ok := m.Route(in, p)
		return ok && r.Homogeneous
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
// 与 RankOutbound 的区别是它会真的去查能力：一条排在前面但会 REJECT 掉本次
// 请求所需能力的路径，不如排在后面但真能跑通的路径。
//
// 全部候选都跑不通时返回最后一次的错误，让调用方能告诉用户到底缺什么，
// 而不是一句笼统的「无可用 Provider」。
func (m *Matrix) BestOutbound(in Protocol, candidates []Provider, caps []canonical.Capability) (Provider, Verdict, error) {
	ranked := m.RankOutbound(in, candidates)
	if len(ranked) == 0 {
		return "", Verdict{}, canonical.Newf(canonical.ClassUnsupported,
			"入站协议 %s 没有任何已注册的出站路径", in)
	}

	var lastErr error
	for _, p := range ranked {
		v, err := m.Check(in, p, caps)
		if err == nil {
			return p, v, nil
		}
		lastErr = err
	}
	return "", Verdict{}, lastErr
}
