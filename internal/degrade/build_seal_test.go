package degrade

import (
	"sync"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// TestRedeemAfterBuildDoesNotMutate 封住 Build 之后的兑现窗口。
//
// Build 返回的是同一个指针，早先 Redeem 对它照样生效——于是两条 Build 校验
// （零值端点、兑现不可交付格子）就都成了摆设：先 Build 过关，再 Redeem 一格
// REJECT 能力，矩阵照单全收，文档还会把它显示成「已投放」。
// 更糟的是矩阵装进 Matrix 之后仍可写，与并发的 Check 读同一张 map。
func TestRedeemAfterBuildDoesNotMutate(t *testing.T) {
	r, err := NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoDashScopeNative)...).
		Redeem(EndpointDashScopeTextGeneration, canonical.CapTextGeneration).
		Build()
	if err != nil {
		t.Fatal(err)
	}

	before := r.RedeemedAt(EndpointDashScopeTextGeneration)

	// 封上之后这三次调用都不该改变任何东西。
	r.Redeem(EndpointDashScopeTextGeneration, canonical.CapStreaming)
	r.Redeem(EndpointDashScopeMultimodal, canonical.CapVisionInput)
	r.Redeem(Endpoint(""), canonical.CapTextGeneration)

	if got := r.RedeemedAt(EndpointDashScopeTextGeneration); len(got) != len(before) {
		t.Errorf("Build 之后的 Redeem 不该给已开门追加能力，实际 %v", got)
	}
	if r.Redeems(EndpointDashScopeTextGeneration, canonical.CapStreaming) {
		t.Error("Build 之后不该能追加 streaming")
	}
	if r.ImplementedAt(EndpointDashScopeMultimodal) {
		t.Error("Build 之后不该能开出第二扇门——那扇门从未经过任何校验")
	}
	if eps := r.Endpoints(); len(eps) != 1 || eps[0] != EndpointDashScopeTextGeneration {
		t.Errorf("门清单应保持不变，实际 %v", eps)
	}
	// 零值端点是 Build 明令拒绝的，绝不能靠事后 Redeem 溜进来。
	for _, ep := range r.Endpoints() {
		if ep == Endpoint("") {
			t.Error("零值端点绝不该出现在门清单里")
		}
	}
}

// TestRedeemAfterBuildIsRaceFree 防的是「封了口但还在写」。
//
// 只要 Redeem 在 Build 之后仍然碰 map，它与并发的 Check / Endpoints 就是
// 数据竞争；-race 下会炸，而没有 -race 的生产环境只是悄悄地坏。
func TestRedeemAfterBuildIsRaceFree(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	r := mustRoute(t, m, ProtoDashScopeNative, ProviderDashScopeNative)
	in := Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeTextGeneration}

	// 探针必须是一扇真的没投放的门：拿已投放的门当探针，断言会因为
	// 「本来就开着」而恒真，封口有没有生效反而测不出来。
	const probe = Endpoint("/api/v1/services/aigc/never-opened/generation")

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			r.Redeem(probe, canonical.CapVisionInput)
		}()
		go func() {
			defer wg.Done()
			_, _ = m.Check(in, ProviderDashScopeNative,
				[]canonical.Capability{canonical.CapTextGeneration})
			_ = r.Endpoints()
			_ = r.Implemented()
		}()
	}
	wg.Wait()

	if r.ImplementedAt(probe) {
		t.Error("并发的事后 Redeem 不该开出新门")
	}
}

// TestImplementedDoesNotAllocate 固化 Implemented 的热路径成本。
//
// 它在每次 Check、每次选路排序里都要跑。早先它借道 Endpoints()——那要建切片
// 再排序，只为回答一个「有没有」的是非题。
func TestImplementedDoesNotAllocate(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	opened := mustRoute(t, m, ProtoDashScopeNative, ProviderDashScopeNative)
	planned := mustRoute(t, m, ProtoOpenAIChat, ProviderAnthropicMessages)

	for _, tc := range []struct {
		name string
		r    *Route
	}{
		{"已开门", opened},
		{"零开门", planned},
	} {
		if n := testing.AllocsPerRun(100, func() { _ = tc.r.Implemented() }); n != 0 {
			t.Errorf("%s 路径的 Implemented 分配了 %.0f 次内存，应为 0", tc.name, n)
		}
	}
}

// TestImplementedAgreesWithEndpoints 保证省掉排序不会改变答案。
//
// 提速的改法最容易在这里出错：快是快了，但对某些形状答错。
func TestImplementedAgreesWithEndpoints(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range m.Routes() {
		if got, want := r.Implemented(), len(r.Endpoints()) > 0; got != want {
			t.Errorf("路径 %s -> %s：Implemented()=%v，但门数为 %d",
				r.In, r.Out, got, len(r.Endpoints()))
		}
	}

	// 未 Build 的路径同样要一致：兑现过就算通车。
	fresh := NewRoute(ProtoOpenAIChat, ProviderOpenAICompat)
	if fresh.Implemented() {
		t.Error("零兑现的新路径不该算通车")
	}
	fresh.Redeem(EndpointOpenAIChat, canonical.CapTextGeneration)
	if !fresh.Implemented() {
		t.Error("Build 之前兑现过的路径应算通车")
	}
}

func BenchmarkImplemented(b *testing.B) {
	m, err := Phase1()
	if err != nil {
		b.Fatal(err)
	}
	r, ok := m.Route(ProtoDashScopeNative, ProviderDashScopeNative)
	if !ok {
		b.Fatal("dashscope.native 路径未注册")
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = r.Implemented()
	}
}
