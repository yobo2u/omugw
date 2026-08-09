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

	b.WriteString("每条转换路径都必须对**每一项**能力明确表态。三种处置：\n\n")
	b.WriteString("| 处置 | 含义 |\n|---|---|\n")
	b.WriteString("| `PASSTHROUGH` | 能力完整传递给上游，无语义损失 |\n")
	b.WriteString("| `DEGRADE` | 请求仍然有效，但部分语义被丢弃；" +
		"网关通过 `" + DegradationHeader + "` 响应头告知客户端 |\n")
	b.WriteString("| `REJECT` | 这条路径无法支持该能力，请求直接失败（HTTP 422） |\n\n")
	b.WriteString("**未登记的组合按 `REJECT` 处理。** 这是刻意的失败方向：" +
		"漏配一格的后果是请求被拒绝，而不是请求丢了半数字段还返回 200。\n\n")

	m.writePreservation(&b)

	for _, r := range m.Routes() {
		fmt.Fprintf(&b, "## `%s` → `%s`\n\n", r.In, r.Out)
		if r.Homogeneous {
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
	b.WriteString("\n\n保留度 = (透传 + 0.5 × 降级) / 总能力数。" +
		"降级不计零分，是因为请求仍然成功，只是丢了部分语义——" +
		"把它与「直接失败」等同看待，会让选路偏向一条谁都用不了的路径。\n\n")

	b.WriteString("| 入站 | 出站 | 快通道 | 透传 | 降级 | 拒绝 | 保留度 |\n")
	b.WriteString("|---|---|---|---:|---:|---:|---:|\n")

	// 按入站分组，组内按选路偏好排序——与运行时的实际选路顺序一致。
	byInbound := map[Protocol][]Provider{}
	var order []Protocol
	for _, r := range m.Routes() {
		if _, seen := byInbound[r.In]; !seen {
			order = append(order, r.In)
		}
		byInbound[r.In] = append(byInbound[r.In], r.Out)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	for _, in := range order {
		for _, out := range m.RankOutbound(in, byInbound[in]) {
			r, _ := m.Route(in, out)
			p := r.Preservation()
			fast := ""
			if r.Homogeneous {
				fast = "✅"
			}
			fmt.Fprintf(b, "| `%s` | `%s` | %s | %d | %d | %d | %.3f |\n",
				in, out, fast, p.Passthrough, p.Degrade, p.Reject, p.Score())
		}
	}
	b.WriteString("\n")
}

// Stats 汇总各处置的格子数，用于快速判断某条路径的「有损程度」。
func (r *Route) Stats() (pass, degrade, reject int) {
	for _, rule := range r.rules {
		switch rule.Disposition {
		case Passthrough:
			pass++
		case Degrade:
			degrade++
		case Reject:
			reject++
		}
	}
	return
}
