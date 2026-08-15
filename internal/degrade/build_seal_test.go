package degrade

import (
	"slices"
	"strings"
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

// TestBuildRejectsUnknownDisposition 验证 Build 会拒绝未知的 Disposition。
func TestBuildRejectsUnknownDisposition(t *testing.T) {
	r := NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
		Pass(ExpressibleSet(ProtoDashScopeNative)...)

	// 强行注入一个未知的 Disposition
	r.Override(canonical.CapTextGeneration, Disposition("UNKNOWN"), "test")

	_, err := r.Build()
	if err == nil {
		t.Fatal("Build 应该拒绝未知的 Disposition，但它放行了")
	}

	if !strings.Contains(err.Error(), "unknown disposition") || !strings.Contains(err.Error(), "UNKNOWN") {
		t.Errorf("错误信息应该包含 unknown disposition 和 UNKNOWN，实际: %v", err)
	}
}

// TestRouteIsImmutableAfterBuild 封住 Build 之后剩下的写入口。
//
// 只从裁决结果观察：Check 的错误分类、Verdict 的降级/模拟清单、门清单、
// Homogeneous。读 r.rules 断言 disposition 会把测试焊死在私有字段上——
// 换一种存储方式就得重写测试，而封口这件事本身根本没变。
//
// 每一次尝试修改都挑了「拆掉闸门就会换一个裁决」的格子：
//   - Override(text_generation → DEGRADE)：拆掉就多出一条 Degraded 记录
//   - Override(streaming → REJECT)：拆掉就从 501 未投放翻成 422 不支持
//   - Redeem(文本门 + streaming/vision)：拆掉就把 501 变成放行
//   - Redeem(多模态门)：拆掉就凭空多开一扇门
//
// Pass/Degrade/Reject/Emulate 走的 set() 是例外，它在这里**观察不到**：
// Build 会给全部 27 项能力补齐 rules，事后再 set 同一格会先撞上重复声明检查
// 而原地返回，只往 errs 里追加一条谁也读不到的记录（Build 不可重入，errs
// 再无出口）。所以 set() 的 built 闸门是靠 -race 咬住的，见
// TestMutatorsAfterBuildIsRaceFree；别把这里的绿灯记在它头上。
func TestRouteIsImmutableAfterBuild(t *testing.T) {
	// text_generation / streaming / vision_input 都是 PASSTHROUGH，
	// 但只有 text_generation 兑现在文本门上——于是「可表达但未投放」
	// 与「已投放」两种形状都在同一条路径上，事后写入往哪边偏都看得见。
	r, err := NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
		Pass(ExpressibleSet(ProtoDashScopeNative)...).
		Redeem(EndpointDashScopeTextGeneration, canonical.CapTextGeneration).
		Build()
	if err != nil {
		t.Fatal(err)
	}

	m := NewMatrix()
	if err := m.Add(r, nil); err != nil {
		t.Fatal(err)
	}
	in := Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeTextGeneration}

	baseHomogeneous := r.Homogeneous
	baseEndpoints := r.Endpoints()

	assertPassthrough(t, m, in, canonical.CapTextGeneration, "基准状态")
	assertNotImplemented(t, m, in, canonical.CapStreaming, "基准状态")
	assertNotImplemented(t, m, in, canonical.CapVisionInput, "基准状态")

	r.MarkHomogeneous()
	r.Pass(canonical.CapVisionInput)
	r.Degrade("test degrade", canonical.CapTextGeneration)
	r.Reject("test reject", canonical.CapTextGeneration)
	r.Emulate("feature", "test emulate", canonical.CapTextGeneration)
	r.Override(canonical.CapTextGeneration, Degrade, "test override degrade")
	r.Override(canonical.CapStreaming, Reject, "test override reject")
	r.Redeem(EndpointDashScopeTextGeneration, canonical.CapStreaming, canonical.CapVisionInput)
	r.Redeem(EndpointDashScopeMultimodal, canonical.CapVisionInput)

	if r.Homogeneous != baseHomogeneous {
		t.Errorf("MarkHomogeneous 在 Build 之后仍然生效：Homogeneous 从 %v 变成 %v",
			baseHomogeneous, r.Homogeneous)
	}
	if got := r.Endpoints(); !slices.Equal(got, baseEndpoints) {
		t.Errorf("Redeem 在 Build 之后开出了新门：门清单从 %v 变成 %v", baseEndpoints, got)
	}

	assertPassthrough(t, m, in, canonical.CapTextGeneration, "Build 之后")
	assertNotImplemented(t, m, in, canonical.CapStreaming, "Build 之后")
	assertNotImplemented(t, m, in, canonical.CapVisionInput, "Build 之后")
}

// assertPassthrough 断言这项能力被放行，且裁决里没有降级或模拟记录。
//
// 只断言「没报错」是不够的：Override 成 DEGRADE 之后 Check 照样返回 nil，
// 差别全在 Verdict 的清单里。
func assertPassthrough(t *testing.T, m *Matrix, in Inbound, c canonical.Capability, stage string) {
	t.Helper()
	v, err := m.Check(in, ProviderDashScopeNative, []canonical.Capability{c})
	if err != nil {
		t.Fatalf("%s：%s 应当放行，实际报错 %v", stage, c, err)
	}
	if len(v.Degraded) > 0 || len(v.Emulated) > 0 {
		t.Fatalf("%s：%s 应当无损透传，实际裁决 %+v", stage, c, v)
	}
}

// assertNotImplemented 断言这项能力因「尚未投放」被挡下，分类必须是 501。
//
// 分类是承重的：REJECT 给的是 422，把它当成 501 的同类会让「事后 Override
// 成 REJECT」这种改动悄悄溜过去。
func assertNotImplemented(t *testing.T, m *Matrix, in Inbound, c canonical.Capability, stage string) {
	t.Helper()
	_, err := m.Check(in, ProviderDashScopeNative, []canonical.Capability{c})
	cerr := canonical.AsError(err)
	if cerr == nil {
		t.Fatalf("%s：%s 尚未投放，应当被挡下，实际 %v", stage, c, err)
	}
	if cerr.Class != canonical.ClassNotImplemented {
		t.Fatalf("%s：%s 应当因未投放返回 %s，实际 %s：%s",
			stage, c, canonical.ClassNotImplemented, cerr.Class, cerr.Message)
	}
}

// TestMutatorsAfterBuildIsRaceFree 防的是「封了口但还在写」。
//
// 这里是 set() 那道 built 闸门唯一的证人。拆掉它，Pass/Degrade/Reject/Emulate
// 会在重复声明检查里往 r.errs 追加——8 个 goroutine 并发 append 同一个切片
// 就是数据竞争；Override 与 Redeem 更直接，它们写的 map 与 Check 读的是同一张。
// 没有 -race 的生产环境不会报错，只会悄悄地坏。
func TestMutatorsAfterBuildIsRaceFree(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	r := mustRoute(t, m, ProtoDashScopeNative, ProviderDashScopeNative)
	in := Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeTextGeneration}

	// 探针门必须是一扇真没投放的门：拿已投放的门当探针，断言会因为
	// 「本来就开着」而恒真，封口有没有生效反而测不出来。
	const probe = Endpoint("/api/v1/services/aigc/never-opened/generation")

	baseHomogeneous := r.Homogeneous
	baseEndpoints := r.Endpoints()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			r.MarkHomogeneous()
			r.Pass(canonical.CapVisionInput)
			r.Degrade("test", canonical.CapVisionInput)
			r.Reject("test", canonical.CapVisionInput)
			r.Emulate("feature", "test", canonical.CapVisionInput)
			r.Override(canonical.CapTextGeneration, Reject, "test")
			r.Redeem(EndpointDashScopeTextGeneration, canonical.CapStreaming)
			r.Redeem(probe, canonical.CapVisionInput)
		}()
		go func() {
			defer wg.Done()
			_, _ = m.Check(in, ProviderDashScopeNative,
				[]canonical.Capability{canonical.CapTextGeneration})
			_ = r.Endpoints()
			_ = r.Implemented()
			_ = r.RedeemedAt(EndpointDashScopeTextGeneration)
		}()
	}
	wg.Wait()

	if r.Homogeneous != baseHomogeneous {
		t.Errorf("并发的事后 MarkHomogeneous 改变了 Homogeneous：%v -> %v",
			baseHomogeneous, r.Homogeneous)
	}
	if got := r.Endpoints(); !slices.Equal(got, baseEndpoints) {
		t.Errorf("并发的事后 Redeem 改变了门清单：%v -> %v", baseEndpoints, got)
	}
	if r.ImplementedAt(probe) {
		t.Error("并发的事后 Redeem 开出了新门")
	}
}
