package canonical

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestAllCapabilitiesRegistered 补上降级矩阵 fail-closed 保证的最后一道口子。
//
// 防的是什么：所有完整性检查——Route.Build、Expressibility.validate、
// TestPhase1IsComplete——都只遍历 AllCapabilities() 返回的那份清单。如果有人
// 新增了一个 Cap* 常量却忘了把它加进 AllCapabilities()，那么没有任何检查会
// 失败：这项能力对可表达性声明、路径处置和 UsedCapabilities() 全部不可见，
// 转换时会被静默丢弃。一个被悄悄丢掉的能力不会报错，只会让用户在月底看到
// 十倍的账单——这正是原则 2.1 要消灭的失败形态。
//
// Go 无法用反射枚举包级常量，所以这里直接解析 capability.go 的 AST，把「实际
// 声明出来的常量」与「登记进 AllCapabilities 的清单」做双向比对。两者是互相
// 独立的证据来源，清单漏登、多登或重复都会被点名。
func TestAllCapabilitiesRegistered(t *testing.T) {
	declared := declaredCapabilities(t)
	if len(declared) == 0 {
		t.Fatal("没有从 capability.go 解析出任何 Capability 常量，解析逻辑可能坏了")
	}
	if err := diffRegistry(declared, AllCapabilities()); err != nil {
		t.Error(err)
	}
}

// TestRegistryDiffDetectsOmission 验证比对逻辑真的会失败，而不是形同虚设。
//
// 与 TestIncompleteRouteFailsBuild 同一用意：没有这个测试，上面那个守卫通过
// 可能只是因为比对逻辑本身坏了。这里用一份人为构造的「声明了却没登记」的数据
// 喂进去，必须检出。
func TestRegistryDiffDetectsOmission(t *testing.T) {
	declared := map[Capability]string{
		Capability("registered"):   "CapRegistered",
		Capability("left_out"):     "CapLeftOut",
		Capability("phantom_pair"): "CapPhantomPair",
	}
	// registered 故意漏掉 left_out，并塞进一个没有对应常量的幽灵项。
	registered := []Capability{
		Capability("registered"),
		Capability("phantom_pair"),
		Capability("phantom"),
		Capability("registered"), // 重复登记
	}

	err := diffRegistry(declared, registered)
	if err == nil {
		t.Fatal("漏登记 / 幽灵项 / 重复登记都应当被检出，实际没有")
	}
	for _, want := range []string{"left_out", "phantom", "registered"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应当点名 %q，实际: %v", want, err)
		}
	}
}

// declaredCapabilities 解析 capability.go，返回其中声明的全部 Capability 常量，
// 按常量值索引到常量名（供错误信息指名道姓）。
func declaredCapabilities(t *testing.T) map[Capability]string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "capability.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 capability.go 失败: %v", err)
	}

	declared := map[Capability]string{}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// 只认 Capability 类型的常量，避开其它声明。
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != "Capability" {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					t.Fatalf("常量 %s 没有初始值，capability.go 的声明形式变了", name.Name)
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("常量 %s 的值不是字符串字面量，capability.go 的声明形式变了", name.Name)
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("常量 %s 的值无法解析: %v", name.Name, err)
				}
				if prev, dup := declared[Capability(val)]; dup {
					t.Fatalf("能力值 %q 被 %s 与 %s 重复声明", val, prev, name.Name)
				}
				declared[Capability(val)] = name.Name
			}
		}
	}
	return declared
}

// diffRegistry 比对「声明出来的常量」与「登记进 AllCapabilities 的清单」，
// 返回点名道姓的错误；一致时返回 nil。
func diffRegistry(declared map[Capability]string, registered []Capability) error {
	regSet := make(map[Capability]bool, len(registered))
	var dups []string
	for _, c := range registered {
		if regSet[c] {
			dups = append(dups, string(c))
		}
		regSet[c] = true
	}

	var unregistered, phantom []string
	for val, name := range declared {
		if !regSet[val] {
			unregistered = append(unregistered, fmt.Sprintf("%s(%q)", name, val))
		}
	}
	for c := range regSet {
		if _, ok := declared[c]; !ok {
			phantom = append(phantom, string(c))
		}
	}

	if len(unregistered) == 0 && len(phantom) == 0 && len(dups) == 0 {
		return nil
	}

	sort.Strings(unregistered)
	sort.Strings(phantom)
	sort.Strings(dups)

	var problems []string
	if len(unregistered) > 0 {
		problems = append(problems,
			"已声明但未登记进 AllCapabilities: "+strings.Join(unregistered, ", "))
	}
	if len(phantom) > 0 {
		problems = append(problems,
			"AllCapabilities 登记了没有对应常量的项: "+strings.Join(phantom, ", "))
	}
	if len(dups) > 0 {
		problems = append(problems,
			"AllCapabilities 重复登记: "+strings.Join(dups, ", "))
	}
	return fmt.Errorf("canonical: 能力注册表不一致——%s", strings.Join(problems, "；"))
}
