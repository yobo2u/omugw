package degrade

import (
	"fmt"
	"sort"

	"github.com/yobo2u/omugw/internal/canonical"
)

// Expressibility 声明一个入站协议**能够表达**哪些能力。
//
// 这一层的存在是为了修一个根本性的错误：早先的矩阵把「这条路承载不了该能力」
// 和「这个入站协议根本表达不出该能力」都记成 REJECT，于是
//
//   - OpenAI Chat 的客户端根本没有字段可以发出 Anthropic 的 thinking 签名，
//     却让每条 openai.chat 路径都为 reasoning_signature「扣分」；
//   - computer_use 是 Responses 独有的内建工具，Chat Completions 里不存在，
//     却同样被记成路径的损失；
//   - dashscope.native → dashscope.native 是零损失的字节直通，保留度却只有
//     0.704，读起来像丢了三成能力。
//
// 可表达性是**协议的属性**，不是路径的属性。分开之后：不可表达的能力自动记为
// NotApplicable，既不进入路径声明的负担，也不进入保留度的分母。
type Expressibility struct {
	Protocol Protocol

	// Capabilities 是这个协议的客户端**有字段可以表达**的能力。
	// 「上游模型支不支持」是另一回事，由路径声明处理。
	Capabilities []canonical.Capability

	// Elsewhere 说明不可表达的能力该去哪条路径找。
	//
	// 键是能力，值是承载它的入站协议。这个字段是本包最重要的一处强制：
	// 指向一个未注册的协议会让完整性测试失败——
	// 「这个能力没地方去」这件事再也藏不住。
	Elsewhere map[canonical.Capability]Protocol

	// Impossible 说明某项能力为什么在这个协议里根本不可能存在。
	// 与 Elsewhere 互斥：一个能力要么在别处有归宿，要么压根不存在。
	Impossible map[canonical.Capability]string
}

// expressible 是全部入站协议的可表达性声明。
var expressible = map[Protocol]*Expressibility{}

func register(e *Expressibility) {
	expressible[e.Protocol] = e
}

// Expressible 报告某个入站协议是否能表达某项能力。
func Expressible(p Protocol, c canonical.Capability) bool {
	e, ok := expressible[p]
	if !ok {
		return false
	}
	for _, x := range e.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

// ExpressibleSet 返回某个入站协议能表达的全部能力，顺序稳定。
func ExpressibleSet(p Protocol) []canonical.Capability {
	e, ok := expressible[p]
	if !ok {
		return nil
	}
	set := map[canonical.Capability]bool{}
	for _, c := range e.Capabilities {
		set[c] = true
	}
	out := make([]canonical.Capability, 0, len(set))
	for _, c := range canonical.AllCapabilities() {
		if set[c] {
			out = append(out, c)
		}
	}
	return out
}

// validateExpressibility 检查一份声明是否覆盖了全部能力，且没有自相矛盾。
func (e *Expressibility) validate() error {
	seen := map[canonical.Capability]int{}
	for _, c := range e.Capabilities {
		seen[c]++
	}
	for c := range e.Elsewhere {
		seen[c]++
	}
	for c := range e.Impossible {
		seen[c]++
	}

	var missing, dup []string
	for _, c := range canonical.AllCapabilities() {
		switch seen[c] {
		case 0:
			missing = append(missing, string(c))
		case 1:
		default:
			dup = append(dup, string(c))
		}
	}
	sort.Strings(missing)
	sort.Strings(dup)

	if len(missing) > 0 {
		return fmt.Errorf("degrade: 协议 %s 未对以下能力表态（可表达 / 在别处 / 不可能，三选一）: %v",
			e.Protocol, missing)
	}
	if len(dup) > 0 {
		return fmt.Errorf("degrade: 协议 %s 对以下能力重复表态: %v", e.Protocol, dup)
	}
	return nil
}

// checkElsewhereTargets 验证 Elsewhere 指向的协议真的存在于矩阵中。
//
// 这是把「缺口」变成「构建失败」的关键一步。dashscope.native 说它的 realtime
// 能力「在 dashscope.realtime 那边」，如果那个协议一条路径都没注册，
// 这里就会报错——而不是让用户在运行时撞上一句「未注册的转换路径」。
func (m *Matrix) checkElsewhereTargets() error {
	registered := map[Protocol]bool{}
	for _, r := range m.Routes() {
		registered[r.InProtocol()] = true
	}

	var problems []string
	for p, e := range expressible {
		if !registered[p] {
			// 该协议本身尚未接入，它的转介目标先不追究。
			continue
		}
		for c, target := range e.Elsewhere {
			if !registered[target] {
				problems = append(problems, fmt.Sprintf(
					"协议 %s 把能力 %q 转介给 %s，但 %s 没有任何已注册路径",
					p, c, target, target))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("degrade: 存在无处可去的能力:\n  - %s",
			joinLines(problems))
	}
	return nil
}

func joinLines(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += "\n  - "
		}
		out += x
	}
	return out
}
