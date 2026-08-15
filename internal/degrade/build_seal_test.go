package degrade

import (
	"errors"
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

// TestBuildAfterBuildIsIdempotent 封住 Build 自己这条写入口。
//
// 封口只挡住了 Pass/Override/Redeem，却漏了 Build 本身——而 Build 恰恰是
// 这个包里写得最狠的一处：它给全部 27 项能力补齐 N/A 格子。于是第二次 Build
// 会把上一次补进去的 N/A 当成「作者为一项表达不出来的能力写了声明」，
// 报一个与事实相反的 declared but not expressible，把一条已经过关的路径判死。
// 更要紧的是它在报错前已经又写了一遍 r.rules 与 r.errs：路径进了矩阵之后，
// 任何一次重复 Build 都在与并发的 Check 抢同一张 map。
//
// 封口之后 Build 必须幂等：原样交回同一个指针与 nil，一格不动。返回同一个
// 指针是承重的——调用点写的是 m.Add(x.Build())，换一个副本出去，矩阵里那条
// 和手上这条就成了两条路。
func TestBuildAfterBuildIsIdempotent(t *testing.T) {
	r, err := NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
		MarkHomogeneous().
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

	baseEndpoints := r.Endpoints()
	baseRedeemed := r.RedeemedAt(EndpointDashScopeTextGeneration)
	assertPassthrough(t, m, in, canonical.CapTextGeneration, "首次 Build 之后")
	assertNotImplemented(t, m, in, canonical.CapStreaming, "首次 Build 之后")

	again, againErr := r.Build()
	if againErr != nil {
		t.Fatalf("已封口路径重复 Build 应当原样返回，实际报错 %v", againErr)
	}
	if again != r {
		t.Errorf("重复 Build 应当交回同一个指针，实际 %p != %p", again, r)
	}

	// 裁决结果是唯一的观察口：只要重复 Build 动过任何一格，
	// 这三条断言里至少有一条会翻。
	if got := r.Endpoints(); !slices.Equal(got, baseEndpoints) {
		t.Errorf("重复 Build 改变了门清单：%v -> %v", baseEndpoints, got)
	}
	if got := r.RedeemedAt(EndpointDashScopeTextGeneration); !slices.Equal(got, baseRedeemed) {
		t.Errorf("重复 Build 改变了兑现集合：%v -> %v", baseRedeemed, got)
	}
	assertPassthrough(t, m, in, canonical.CapTextGeneration, "重复 Build 之后")
	assertNotImplemented(t, m, in, canonical.CapStreaming, "重复 Build 之后")
}

// TestBuildFailureDoesNotSealRoute 是上面那道闸门的对照组。
//
// 幂等只许对**成功**的 Build 生效。失败路径不能在第二次调用时直接返回成功；
// 当前 Build 会保留首次失败留下的错误和 N/A 补格，因此这里不承诺它可原地修复
// 后重试，只锁住「失败不能被封成成功」这条边界。
func TestBuildFailureDoesNotSealRoute(t *testing.T) {
	r := NewRoute(ProtoDashScopeNative, ProviderDashScopeNative)

	if _, err := r.Build(); err == nil {
		t.Fatal("零声明的路径本该 Build 失败，测试前提不成立")
	}
	if _, err := r.Build(); err == nil {
		t.Fatal("失败的路径重复 Build 仍该失败，不该被当成已封口")
	}
}

// TestAddRejectsNilRoute 防的是 Add 对着 nil 解引用。
//
// Add 的签名收的是 (r, err) 一对，调用点几乎都写成 m.Add(x.Build())——
// Build 失败时返回的正是 (nil, err)。err 非空的那条分支先返回，nil 路径
// 走不到解引用；但任何一个手写 m.Add(r, nil) 的调用点只要 r 是 nil，
// 就会在取 r.In 时崩掉整个进程。矩阵是启动期装配的东西，它该报错，不该 panic。
func TestAddRejectsNilRoute(t *testing.T) {
	m := NewMatrix()

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Add(nil, nil) 不该 panic，实际 %v", p)
		}
	}()

	err := m.Add(nil, nil)
	if err == nil {
		t.Fatal("Add(nil, nil) 应当报错，实际放行")
	}
	if !strings.Contains(err.Error(), "nil route") {
		t.Errorf("错误信息应当点名 nil route，实际: %v", err)
	}
	if len(m.Routes()) != 0 {
		t.Errorf("被拒的 nil 路径不该留在矩阵里，实际 %d 条", len(m.Routes()))
	}
}

// TestAddRejectsUnbuiltRoute 封死 Build 校验的绕过入口。
//
// Build 是这个包全部路径级校验的唯一执行点：能力表态是否完整、端点是否为零值、
// 兑现的格子是否可交付、门有没有错绑协议。Add 只看 err 而不问这条路径究竟
// 过没过 Build，m.Add(r, nil) 就成了一条平行入口——从未受检的路径直接落进
// 矩阵，被 Check 当成权威裁决依据。
//
// 这条路径刻意做得「看起来很完整」：全部可表达能力都 Pass 了，门也兑现了，
// 唯独没有 Build。看起来完整正是它危险的地方——漏的是校验，不是内容。
func TestAddRejectsUnbuiltRoute(t *testing.T) {
	m := NewMatrix()

	r := NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoDashScopeNative)...).
		Redeem(EndpointDashScopeTextGeneration, canonical.CapTextGeneration)

	err := m.Add(r, nil)
	if err == nil {
		t.Fatal("未 Build 的路径不该进得了矩阵")
	}
	if !strings.Contains(err.Error(), string(ProtoDashScopeNative)) ||
		!strings.Contains(err.Error(), string(ProviderDashScopeNative)) {
		t.Errorf("错误信息应当点名是哪条路径，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "Build") {
		t.Errorf("错误信息应当说明必须先 Build，实际: %v", err)
	}

	// 被拒的路径不能留下痕迹：留在 routes 里，Check 就照样能拿它裁决，
	// 报错也就只是句空话。
	if _, ok := m.Route(ProtoDashScopeNative, ProviderDashScopeNative); ok {
		t.Fatal("被拒的路径不该出现在矩阵里")
	}
	in := Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeTextGeneration}
	if _, err := m.Check(in, ProviderDashScopeNative,
		[]canonical.Capability{canonical.CapTextGeneration}); err == nil {
		t.Fatal("未 Build 的路径不该能被 Check 裁决通过")
	}
}

// TestAddStillAcceptsBuiltRouteAndPropagatesError 是上面两道闸门的对照组。
//
// 一道只会拒绝的闸门是没用的：它得放行合法的那条，也得原样保留
// m.Add(r.Build()) 的错误传播——Build 失败时返回 (nil, err)，
// Add 必须把那个 err 原样交出去，而不是换成自己的 nil 路径报错，
// 否则真正的失败原因（漏了哪格能力）就被盖掉了。
func TestAddStillAcceptsBuiltRouteAndPropagatesError(t *testing.T) {
	m := NewMatrix()

	built, err := NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoDashScopeNative)...).
		Redeem(EndpointDashScopeTextGeneration, canonical.CapTextGeneration).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Add(built, nil); err != nil {
		t.Fatalf("已 Build 的路径应当照常接收，实际 %v", err)
	}
	if _, ok := m.Route(ProtoDashScopeNative, ProviderDashScopeNative); !ok {
		t.Fatal("已 Build 的路径应当留在矩阵里")
	}

	// Build 失败这条：路径少了一格能力表态，返回 (nil, err)。
	incomplete := NewRoute(ProtoOpenAIChat, ProviderOpenAICompat)
	bad, buildErr := incomplete.Build()
	if buildErr == nil {
		t.Fatal("零声明的路径本该 Build 失败，测试前提不成立")
	}
	if bad != nil {
		t.Fatal("Build 失败时不该返回路径")
	}
	if got := m.Add(bad, buildErr); !errors.Is(got, buildErr) {
		t.Errorf("Add 应当原样传出 Build 的错误，实际 %v", got)
	}
}

// TestAddRejectsRoutesBuildWouldHaveRefused 证明绕过 Build 能带进来什么。
//
// 上一条只说「未 Build 的路径进不来」，这条说的是为什么要拦：这两条路径
// 各自踩了一格 Build 明令拒绝的东西——未知处置、门错绑入站协议。走 Add
// 这条平行入口，它们都能落进矩阵；Check 会拿一个自己不认识的 disposition
// 去裁决，或者按 openai.chat 的可表达性裁决一个由 Responses 解码器把守的请求。
func TestAddRejectsRoutesBuildWouldHaveRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		// 每次现造一条：Route 的 rules/redeemed 是 map，复制结构体等于共享底层，
		// 拿副本试 Build 会把补齐的 N/A 写回样本。
		newRoute func() *Route
	}{
		{
			// 未知处置：Check 的 switch 不认识它，会一路落到放行分支。
			name: "未知处置",
			newRoute: func() *Route {
				return NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
					Pass(ExpressibleSet(ProtoDashScopeNative)...).
					Override(canonical.CapTextGeneration, Disposition("UNKNOWN"), "test").
					Redeem(EndpointDashScopeTextGeneration, canonical.CapTextGeneration)
			},
		},
		{
			// 门错绑协议：/v1/responses 收的是 Responses 请求，
			// 兑现在 chat 路径上等于让 chat 的可表达性替它背书。
			name: "门错绑入站协议",
			newRoute: func() *Route {
				return NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
					Pass(ExpressibleSet(ProtoOpenAIChat)...).
					Redeem(EndpointOpenAIResponses, canonical.CapTextGeneration)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 前提：这两条路径确实是 Build 拦得下的，拦不下就说明测试选错了样本。
			if _, err := tc.newRoute().Build(); err == nil {
				t.Fatal("样本路径本该被 Build 拒绝，测试前提不成立")
			}

			m := NewMatrix()
			if err := m.Add(tc.newRoute(), nil); err == nil {
				t.Fatal("Build 拒绝的路径不该从 Add 这条路进来")
			}
			if len(m.Routes()) != 0 {
				t.Errorf("被拒的路径不该留在矩阵里，实际 %d 条", len(m.Routes()))
			}
		})
	}
}

// TestMutatorsAfterBuildIsRaceFree 防的是「封了口但还在写」。
//
// 这里是 set() 那道 built 闸门唯一的证人。拆掉它，Pass/Degrade/Reject/Emulate
// 会在重复声明检查里往 r.errs 追加——8 个 goroutine 并发 append 同一个切片
// 就是数据竞争；Override 与 Redeem 更直接，它们写的 map 与 Check 读的是同一张。
// 没有 -race 的生产环境不会报错，只会悄悄地坏。
//
// Build 也在写手一侧：它给全部能力补 N/A 格子，是这里写得最狠的一处，
// 却最容易被漏掉——它顶着「校验」的名字，读起来不像个写入口。
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
			_, _ = r.Build()
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
