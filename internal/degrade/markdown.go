package degrade

import (
	"fmt"
	"sort"
	"strings"

	"github.com/yobo2u/omugw/internal/canonical"
)

// Markdown 把矩阵渲染成文档。
//
// docs/degradation-matrix.md 由这个函数生成，并由 TestDegradationMatrixDocIsCurrent
// 校验是否与代码同步。文档和代码分头维护必然会漂移，而这份文档是用户判断
// 「我的请求会不会被悄悄改掉」的唯一依据，漂移的代价很高。
func (m *Matrix) Markdown() string {
	var b strings.Builder

	b.WriteString("# 降级矩阵\n\n")
	b.WriteString("> **本文件由 `internal/degrade` 自动生成，请勿手工编辑。**\n")
	b.WriteString("> 修改 `internal/degrade/rules_phase1.go` 后运行 `make matrix-update` 重新生成。\n\n")

	b.WriteString("每条转换路径都必须对入站协议**表达得出来**的每一项能力明确表态。" +
		"表态说的是**设计处置**——这条路最终该怎么对待这项能力；" +
		"它当前投放了没有是另一回事，见下文的「当前可用」列：\n\n")
	b.WriteString("| 处置 | 含义 | 计入设计保留度 |\n|---|---|---|\n")
	b.WriteString("| `PASSTHROUGH` | 能力完整传递给上游，无语义损失 | 满分 |\n")
	b.WriteString("| `EMULATE` | 上游不提供，由网关自行实现；客户端拿到的能力是完整的，" +
		"但带着网关侧的可用性边界 | 满分 |\n")
	b.WriteString("| `DEGRADE` | 请求仍然有效，但部分语义被丢弃；" +
		"网关通过 `" + DegradationHeader + "` 响应头告知客户端 | 半分 |\n")
	b.WriteString("| `REJECT` | 这条路径无法承载该能力，请求直接失败（HTTP 422） | 零分 |\n")
	b.WriteString("| `N/A` | 入站协议根本表达不出该能力，客户端连发都发不出来；" +
		"由可表达性声明自动推导，注明该去哪个协议 | **不进分母** |\n\n")

	b.WriteString("`N/A` 单列是有原因的。早先的版本把它和 `REJECT` 混为一谈，" +
		"结果一条零损失的字节直通路径只拿到 0.704 分——读起来像丢了三成能力，" +
		"实际一点没丢。**可表达性是协议的属性，不是路径的属性**：" +
		"OpenAI Chat 的客户端没有字段可以发出 Anthropic 的推理签名，" +
		"不该让每条 `openai.chat` 路径为此扣分。\n\n")

	b.WriteString("**未登记的组合按 `REJECT` 处理。** 这是刻意的失败方向：" +
		"漏配一格的后果是请求被拒绝，而不是请求丢了半数字段还返回 200。\n\n")

	m.writePreservation(&b)

	for _, r := range m.Routes() {
		fmt.Fprintf(&b, "## `%s` → `%s`\n\n", r.InProtocol(), r.OutProvider())
		if r.IsHomogeneous() {
			b.WriteString("**同源快通道。** 该路径可字节级透传，只改写鉴权，不进 Canonical。\n\n")
		}
		b.WriteString("| 能力 | 处置 | 说明 |\n|---|---|---|\n")
		for _, c := range canonical.AllCapabilities() {
			rule := r.rules[c]
			note := rule.Note
			if note == "" {
				note = "—"
			}
			fmt.Fprintf(&b, "| `%s` | `%s` | %s |\n", c, rule.Disposition, note)
		}
		b.WriteString("\n")
	}

	return b.String()
}

// writePreservation 输出选路偏好与原生能力保留度总表。
//
// 这张表是「尽量保留原生能力」这条原则的量化呈现：同一入站协议下，选路偏好
// 排得越靠前，保留度必须越高，否则 TestPreferenceMatchesPreservation 会失败。
func (m *Matrix) writePreservation(b *strings.Builder) {
	b.WriteString("## 选路偏好与原生能力保留度\n\n")
	b.WriteString("**入站协议族接入优先级**（依据是能表达多少原生能力）：\n\n")
	for i, f := range InboundPriority {
		if !f.Implemented() {
			fmt.Fprintf(b, "%d. **%s** — 尚未接入\n", i+1, f.Name)
			continue
		}
		fmt.Fprintf(b, "%d. **%s** — ", i+1, f.Name)
		for j, p := range f.Protocols {
			if j > 0 {
				b.WriteString("、")
			}
			fmt.Fprintf(b, "`%s`", p)
		}
		b.WriteString("（族内按表达力从强到弱排列）\n")
	}
	b.WriteString("\n同族协议共用编解码基础设施与错误信封，接入其中一个之后再接入另一个的" +
		"边际成本很低，因此按族而不是按单个协议排优先级。\n\n")
	b.WriteString("**出站选路偏好**（越靠前越优先，依据是原生能力保留度而非延迟或成本）：")
	for i, p := range OutboundPreference {
		if i > 0 {
			b.WriteString(" → ")
		}
		fmt.Fprintf(b, "`%s`", p)
	}
	b.WriteString("\n\n保留度 = (透传 + 模拟 + 0.5 × 降级) / **可表达能力数**。" +
		"分母只算入站协议表达得出来的能力：客户端发不出来的东西，" +
		"这条路径没有义务为它负责。降级不计零分，是因为请求仍然成功——" +
		"把它与「直接失败」等同看待，会让选路偏向一条谁都用不了的路径。\n\n")
	b.WriteString("**同源快通道永远排在最前，然后才轮到全局偏好序。** " +
		"固定的全局顺序表达不了「同源优先」，而后者依赖入站协议是谁——" +
		"对 `dashscope.realtime` 入站，DashScope 侧直通是零损失的，" +
		"可在全局序里 `openai.realtime` 排得更靠前。\n\n")

	b.WriteString("保留度分两列（见 ADR-0002）：**设计目标**假定全部实现、全部开关开启，" +
		"回答「这条路最终能做到什么」；**当前可用**受实现状态与默认配置影响，" +
		"是选路的唯一依据。尚未实现的路径没有当前可用分数——" +
		"给一条走不通的路打分，是在请人相信一个还不存在的东西。\n\n")

	b.WriteString("**上表每一格的处置都是设计事实，不是投放承诺。** " +
		"一条路径转正只说明它开始通车，说明不了它的每个上游端点都通：" +
		"DashScope Native 一个协议对应文本生成、多模态生成、embedding、rerank 多个端点，" +
		"当前投放了文本生成与多模态生成两个端点。未投放的能力在运行时返回 **501**（等实现），" +
		"而不是按处置声明放行——放行会让请求打到一个尚不存在的实现上，" +
		"客户端拿到的是一个语焉不详的 5xx。这与 `REJECT` 的 **422**（改请求）" +
		"是两件事：前者会变，后者不会。\n\n")

	b.WriteString("| 入站 | 出站 | 状态 | 快通道 | 透传 | 模拟 | 降级 | 拒绝 | N/A | 设计目标 | 当前可用 |\n")
	b.WriteString("|---|---|---|---|---:|---:|---:|---:|---:|---:|---:|\n")

	// 按入站分组，组内按选路偏好排序——与运行时的实际选路顺序一致。
	byInbound := map[Protocol][]Provider{}
	var order []Protocol
	for _, r := range m.Routes() {
		if _, seen := byInbound[r.InProtocol()]; !seen {
			order = append(order, r.InProtocol())
		}
		byInbound[r.InProtocol()] = append(byInbound[r.InProtocol()], r.OutProvider())
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	for _, in := range order {
		for _, out := range m.RankDesign(in, byInbound[in]) {
			r, _ := m.Route(in, out)
			// 设计列是路径级的，与敲哪扇门无关；这里只取设计列。
			// 可用列绝不从这条调用取——零值端点的可用列恒为零（见端点分支）。
			p := r.Preservation(m.avail, Endpoint(""))

			status := "规划中"
			available := "—"
			if r.Implemented() {
				status = "已实现"
				e := r.Endpoints()
				if len(e) == 1 {
					// 单门路径：可用列就是那扇门的端点相对分数，显示方式不变。
					available = formatAvailable(r.Preservation(m.avail, e[0]))
				} else {
					// 多门路径：并集分数不对应任何一扇真实的门，
					// 主表只指路，分数逐门列在端点细分小节。
					available = "见端点细分"
				}
			}
			fast := ""
			if r.IsHomogeneous() {
				fast = "✅"
			}

			// 模拟列计入被开关关掉的格子——它们仍然是模拟格子，只是当前不生效。
			// 把它们并进透传列会让一条靠网关垫着的路径看起来像是原生直通。
			emulate := fmt.Sprintf("%d", p.Emulate+p.EmulateOff)
			if p.EmulateOff > 0 {
				emulate += fmt.Sprintf("（%d 未开启）", p.EmulateOff)
			}

			fmt.Fprintf(b, "| `%s` | `%s` | %s | %s | %d | %s | %d | %d | %d | %.3f | %s |\n",
				in, out, status, fast,
				p.Passthrough, emulate, p.Degrade, p.Reject, p.NotApplicable,
				p.DesignScore(), available)
		}
	}
	m.writeEndpointBreakdown(b)
	b.WriteString("\n")
}

// writeEndpointBreakdown 输出端点细分：可用列永远端点相对。
//
// 不存在路径级「当前可用」聚合分——各门兑现集合的并集不对应任何一扇真实
// 存在的门，给不存在的东西记分正是矩阵要防的过度承诺。多门路径的主表可用列
// 只写「见端点细分」，这个小节就是那句话的落点：缺了它，主表会指向一处
// 根本不存在的小节。
func (m *Matrix) writeEndpointBreakdown(b *strings.Builder) {
	// 行按 Routes()（已排序）与 Endpoints()（字典序）逐层展开，
	// 两者都不依赖 map 遍历顺序——否则文档每次生成都不一样，
	// 同步断言会时绿时红，最后被当成 flaky 关掉。
	var body strings.Builder
	for _, r := range m.Routes() {
		for _, ep := range r.Endpoints() {
			caps := make([]string, 0, len(canonical.AllCapabilities()))
			for _, c := range r.RedeemedAt(ep) {
				caps = append(caps, string(c))
			}
			fmt.Fprintf(&body, "| %s | %s | %s | %s | %s |\n",
				r.InProtocol(), r.OutProvider(), ep, strings.Join(caps, ", "),
				formatAvailable(r.Preservation(m.avail, ep)))
		}
	}
	// 一门未开时不印表头：一张只有表头的空表会让人以为渲染坏了，
	// 而真相是「还没有任何东西投放」。
	if body.Len() == 0 {
		return
	}

	b.WriteString("\n### 端点细分\n\n")
	b.WriteString("「当前可用」一律端点相对：**不存在**路径级「当前可用」聚合分。" +
		"各门兑现集合的并集不对应任何一扇真实存在的门，" +
		"给不存在的东西记分正是矩阵要防的过度承诺。\n\n")
	b.WriteString("| 入站 | 出站 | 端点 | 已投放 | 当前可用 |\n")
	b.WriteString("|---|---|---|---|---:|\n")
	b.WriteString(body.String())
}

// formatAvailable 渲染「当前可用」列：分数本身，外加把前提写进数字本身的括号。
//
// 默认配置是绝大多数人的实际部署，按它计分才诚实；括号让读者查到那个数是怎么来的。
// 两种前提要分开写：「开关没开」运维改配置就能解决，「端点还没投放」只能等实现——
// 混成一句，会让人去开一个根本解决不了问题的开关。
func formatAvailable(p Preservation) string {
	available := fmt.Sprintf("%.3f", p.AvailableScore())
	switch {
	case p.NotRedeemed > 0:
		available += fmt.Sprintf("（%d 项中 %d 项已投放）",
			p.denominator()-p.Reject, p.Redeemed())
	case p.EmulateOff > 0:
		available += fmt.Sprintf("（开启 %s 后 %.3f）",
			FeatureConversationStore, p.DesignScore())
	}
	return available
}

// docOrder 给出文档中的路径排列顺序。
//
// 不能直接用 RankOutbound——它只返回已实现的路径，而文档要把规划中的也列出来。
// 已实现的排前面，其余按选路偏好序。
func (m *Matrix) docOrder(in Protocol, candidates []Provider) []Provider {
	ranked := m.RankOutbound(in, candidates)
	seen := map[Provider]bool{}
	for _, p := range ranked {
		seen[p] = true
	}

	rest := make([]Provider, 0, len(candidates))
	for _, c := range candidates {
		if !seen[c] {
			rest = append(rest, c)
		}
	}
	sort.Slice(rest, func(i, j int) bool {
		ri, rj := preferenceRank(rest[i]), preferenceRank(rest[j])
		if ri != rj {
			return ri < rj
		}
		return rest[i] < rest[j]
	})
	return append(ranked, rest...)
}

// FixtureDir 返回一条路径的 fixture 目录约定路径（相对仓库根）。
//
// 转正门槛靠它落地：一条路径要标记为已实现，这个目录下必须有测试用例
// （见 ADR-0001）。
func FixtureDir(in Protocol, out Provider) string {
	return "testdata/routes/" + string(in) + "__" + string(out)
}
