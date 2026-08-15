package degrade

import (
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// rebuiltMatrix 把一份矩阵按原样重新声明一遍，并在每条路径 Build **之前**
// 交给 mutate 追加兑现。
//
// 路径 Build 之后就封口了，测试不能再往里塞门——那正是生产代码要防的事，
// 给测试开一道后门等于把闸门拆了。要多一扇门，就得像真实代码一样在 Build
// 之前声明，然后接受同一套校验：零值端点、兑现不可交付格子，一样会失败。
//
// 只存在于 _test.go：生产代码没有、也不该有重新打开一条已封口路径的能力。
func rebuiltMatrix(t *testing.T, src *Matrix, mutate func(r *Route)) *Matrix {
	t.Helper()

	out := NewMatrix().WithAvailability(src.Availability())
	for _, r := range src.Routes() {
		n := NewRoute(r.InProtocol(), r.OutProvider())
		// 走 MarkHomogeneous 而不是赋值：快通道事实只有这一个写入口，
		// 助手若另开一条，它重建出来的路径就不再受同一套封口约束。
		if r.IsHomogeneous() {
			n.MarkHomogeneous()
		}
		for c, rule := range r.rules {
			// N/A 由 Build 依可表达性自动补；显式声明反而会被判成
			// 「声明了表达不出来的能力」。
			if rule.Disposition == NotApplicable {
				continue
			}
			n.rules[c] = rule
		}
		for _, ep := range r.Endpoints() {
			n.Redeem(ep, r.RedeemedAt(ep)...)
		}
		if mutate != nil {
			mutate(n)
		}
		if err := out.Add(n.Build()); err != nil {
			t.Fatalf("重建路径 %s -> %s 失败: %v", r.InProtocol(), r.OutProvider(), err)
		}
	}
	return out
}

// TestRebuiltMatrixPreservesDeclarations 保证重建助手本身是忠实的。
//
// 助手若悄悄丢掉或改写了某条声明，靠它搭出来的测试就都在验一个假矩阵——
// 那种测试全绿，却什么也没保证。
func TestRebuiltMatrixPreservesDeclarations(t *testing.T) {
	src, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	got := rebuiltMatrix(t, src, nil)

	if len(got.Routes()) != len(src.Routes()) {
		t.Fatalf("重建后路径数 %d，原为 %d", len(got.Routes()), len(src.Routes()))
	}
	for _, want := range src.Routes() {
		in, out := want.InProtocol(), want.OutProvider()
		have := mustRoute(t, got, in, out)
		if have.IsHomogeneous() != want.IsHomogeneous() {
			t.Errorf("%s -> %s 的快通道标记丢失", in, out)
		}
		for _, c := range canonical.AllCapabilities() {
			if have.rules[c] != want.rules[c] {
				t.Errorf("%s -> %s 的 %q 声明不一致: %+v vs %+v",
					in, out, c, have.rules[c], want.rules[c])
			}
		}
		wantEps, haveEps := want.Endpoints(), have.Endpoints()
		if len(wantEps) != len(haveEps) {
			t.Fatalf("%s -> %s 门数 %d，原为 %d", in, out, len(haveEps), len(wantEps))
		}
		for i, ep := range wantEps {
			if haveEps[i] != ep {
				t.Errorf("%s -> %s 第 %d 扇门 %s，原为 %s", in, out, i, haveEps[i], ep)
			}
			if len(have.RedeemedAt(ep)) != len(want.RedeemedAt(ep)) {
				t.Errorf("%s -> %s 门 %s 的兑现集合不一致", in, out, ep)
			}
		}
	}
}

// TestRebuiltMatrixStillEnforcesBuildChecks 保证重建这条路没有绕开校验。
//
// 助手要是让测试能造出一条 Build 本该拒绝的路径，它就成了那道后门本身。
func TestRebuiltMatrixStillEnforcesBuildChecks(t *testing.T) {
	src, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	// 借一条真实路径的声明，试着兑现一格 REJECT 能力。
	r := mustRoute(t, src, ProtoOpenAIChat, ProviderAnthropicMessages)
	n := NewRoute(r.InProtocol(), r.OutProvider())
	for c, rule := range r.rules {
		if rule.Disposition == NotApplicable {
			continue
		}
		n.rules[c] = rule
	}
	if _, err := n.Redeem(EndpointOpenAIChat, canonical.CapAudioInput).Build(); err == nil {
		t.Fatal("重建路径同样不该允许兑现 REJECT 格子")
	}
}
