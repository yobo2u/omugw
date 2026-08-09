package degrade

import (
	"fmt"
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
