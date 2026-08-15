package degrade

import (
	"slices"
	"sync"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// 并发场景的重复次数与并发度。
//
// 交错是概率事件：一次跑完全可能恰好串行，什么都测不出来。重复几百轮把窗口
// 撑开，让 -race 有机会撞上；但断言本身不依赖任何一种交错发生——每一轮的
// 断言在「变更抢在前」和「变更落在封口后」两种顺序下都必须成立。
const (
	raceAttempts = 200
	raceWorkers  = 8
)

// freshFullyDeclaredRoute 造一条声明完整、已兑现一扇门、尚未 Build 的路径。
//
// 每轮现造而不是复用：Route 的 rules/redeemed 是 map，Build 会往里补 N/A 格子，
// 复用一条就等于让上一轮的封口结果泄进下一轮，并发窗口也就不存在了。
func freshFullyDeclaredRoute() *Route {
	return NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
		Pass(ExpressibleSet(ProtoDashScopeNative)...).
		Redeem(EndpointDashScopeTextGeneration, canonical.CapTextGeneration)
}

// buildOutcome 记一次 Build 的返回对。
type buildOutcome struct {
	route *Route
	err   error
}

// runConcurrently 把 fns 里的每个函数各起一个 goroutine，用同一个发令闸放行。
//
// 闸门不是为了保证交错，而是为了不白白错过它：各 goroutine 依次创建时，先起的
// 那个往往已经跑完，后面的再进来就是纯串行。
func runConcurrently(fns ...func()) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fn()
		}()
	}
	close(start)
	wg.Wait()
}

// assertOutcomeShape 断言每一次 Build 的返回对形状合法。
//
// 承重的是「成功必须交回同一个指针」：调用点写的是 m.Add(x.Build())，
// 换一个副本出去，矩阵里那条和手上这条就成了两条路。
func assertOutcomeShape(t *testing.T, want *Route, got []buildOutcome) (anySucceeded bool) {
	t.Helper()
	for _, o := range got {
		switch {
		case o.err == nil && o.route != want:
			t.Fatalf("成功的 Build 应当交回同一个指针，实际 %p != %p", o.route, want)
		case o.err != nil && o.route != nil:
			t.Fatalf("失败的 Build 不该交回路径，实际 %p（err=%v）", o.route, o.err)
		case o.err == nil:
			anySucceeded = true
		}
	}
	return anySucceeded
}

// TestConcurrentFirstBuildIsAtomic 防的是「校验与封口之间有缝」。
//
// Build 是这个包写得最狠的一处写入口：它给全部 27 项能力补齐 N/A 格子，然后
// 才置 built。两个 goroutine 同时对一条从未 Build 的路径调用它，就会同时读到
// built==false 一起往下走——两边并发写同一张 rules、并发 append 同一个 errs，
// 这是裸的数据竞争；更荒唐的是后进来的那个会把先进来那个补进去的 N/A 当成
// 「声明了表达不出来的能力」，给一条声明完整的路径判一个 declared but not
// expressible。于是「Build 成功过」不再等于「这条路径通过了校验」。
//
// 声明完整的路径不论怎么交错，每一次 Build 都必须成功并交回同一个指针。
func TestConcurrentFirstBuildIsAtomic(t *testing.T) {
	for range raceAttempts {
		r := freshFullyDeclaredRoute()

		outcomes := make([]buildOutcome, raceWorkers)
		fns := make([]func(), 0, raceWorkers)
		for i := range raceWorkers {
			fns = append(fns, func() {
				got, err := r.Build()
				outcomes[i] = buildOutcome{route: got, err: err}
			})
		}
		runConcurrently(fns...)

		for _, o := range outcomes {
			if o.err != nil {
				t.Fatalf("声明完整的路径并发 Build 不该失败，实际 %v", o.err)
			}
		}
		assertOutcomeShape(t, r, outcomes)

		// 封口之后必须幂等：再来一次原样交回同一个指针。
		again, err := r.Build()
		if err != nil || again != r {
			t.Fatalf("并发 Build 之后的二次 Build 应当原样返回，实际 (%p, %v)", again, err)
		}

		assertValidatedShape(t, r)
	}
}

// assertValidatedShape 从外部观察这条路径是不是「通过了校验的那个状态」。
//
// 只从裁决结果与门清单观察，不读私有字段：换一种存储方式不该让这条断言重写。
// 三项各咬一种走样——门清单多一扇说明有人绕过端点校验，text_generation 不再
// 无损透传说明 rules 被并发改过，streaming 不再是 501 说明兑现集合被写脏。
func assertValidatedShape(t *testing.T, r *Route) {
	t.Helper()

	m := NewMatrix()
	if err := m.Add(r, nil); err != nil {
		t.Fatalf("已封口的路径应当进得了矩阵，实际 %v", err)
	}
	in := Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeTextGeneration}

	if got := r.Endpoints(); !slices.Equal(got, []Endpoint{EndpointDashScopeTextGeneration}) {
		t.Fatalf("门清单应当只有文本生成一扇，实际 %v", got)
	}
	assertPassthrough(t, m, in, canonical.CapTextGeneration, "并发 Build 之后")
	assertNotImplemented(t, m, in, canonical.CapStreaming, "并发 Build 之后")
}

// TestConcurrentFirstBuildWithMutatorIsAtomic 是这道闸门真正的证人。
//
// 每个用例的变更都是 Build 明令拒绝的东西。它与首次 Build 并发，只有两种
// 合法结局：
//
//   - 抢在 Build 之前 → 校验必须逮住它，每一次 Build 都失败，路径从未封口，
//     进不了矩阵；
//   - 落在封口之后 → 确定性的空操作，痕迹不该留下。
//
// 第三种结局是这里要防的：变更挤进校验与封口之间——校验那一刻还是干净的，
// 封口那一刻已经脏了。于是一条 Build 拒绝过的形状被盖上「已校验」的章进了
// 矩阵，Check 拿它当权威裁决依据。
func TestConcurrentFirstBuildWithMutatorIsAtomic(t *testing.T) {
	for _, tc := range []struct {
		name string
		// mutate 是 Build 会拒绝的那一手变更。
		mutate func(*Route)
		// tainted 从外部观察这手变更的痕迹还在不在；nil 表示这手变更
		// 只脏 errs、外部观察不到，靠 -race 与「全体失败」那条分支咬住。
		tainted func(*Route, *Matrix) bool
	}{
		{
			// 零值端点一旦被当成「整条路径」的通配，端点粒度就退回了路径粒度。
			name:   "零值端点兑现",
			mutate: func(r *Route) { r.Redeem(Endpoint(""), canonical.CapTextGeneration) },
			tainted: func(r *Route, _ *Matrix) bool {
				return slices.Contains(r.Endpoints(), Endpoint(""))
			},
		},
		{
			// /v1/responses 由 Responses 解码器把守，兑现在 dashscope 路径上
			// 等于让 dashscope 的可表达性替别人的门背书。
			name:   "门错绑入站协议",
			mutate: func(r *Route) { r.Redeem(EndpointOpenAIResponses, canonical.CapTextGeneration) },
			tainted: func(r *Route, _ *Matrix) bool {
				return r.ImplementedAt(EndpointOpenAIResponses)
			},
		},
		{
			// realtime_session 在 dashscope.native 上是 N/A，兑现它等于
			// 为一个客户端根本发不出来的能力背书。
			name:   "兑现不可交付格子",
			mutate: func(r *Route) { r.Redeem(EndpointDashScopeTextGeneration, canonical.CapRealtimeSession) },
			tainted: func(r *Route, _ *Matrix) bool {
				return r.Redeems(EndpointDashScopeTextGeneration, canonical.CapRealtimeSession)
			},
		},
		{
			// 没有说明的 REJECT：错误消息会退化成一句「不支持」，
			// 客户端无从知道为什么。Build 拒绝它。
			name:   "无说明的 REJECT 覆盖",
			mutate: func(r *Route) { r.Override(canonical.CapTextGeneration, Reject, "") },
			tainted: func(_ *Route, m *Matrix) bool {
				in := Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeTextGeneration}
				_, err := m.Check(in, ProviderDashScopeNative,
					[]canonical.Capability{canonical.CapTextGeneration})
				return err != nil
			},
		},
		{
			// 重复声明只脏 errs：set() 撞上重复检查会原地返回，rules 一格不动。
			// 外部观察不到痕迹，但并发 append 同一个切片是裸的数据竞争。
			name:    "重复声明同一项能力",
			mutate:  func(r *Route) { r.Pass(canonical.CapTextGeneration) },
			tainted: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for range raceAttempts {
				r := freshFullyDeclaredRoute()

				outcomes := make([]buildOutcome, raceWorkers)
				fns := make([]func(), 0, raceWorkers+1)
				for i := range raceWorkers {
					fns = append(fns, func() {
						got, err := r.Build()
						outcomes[i] = buildOutcome{route: got, err: err}
					})
				}
				fns = append(fns, func() { tc.mutate(r) })
				runConcurrently(fns...)

				succeeded := assertOutcomeShape(t, r, outcomes)

				// Add 是 built 唯一的公开探针：它收不收这条路径，
				// 就是「校验有没有真的走完并封口」的答案。
				m := NewMatrix()
				addErr := m.Add(r, nil)

				if !succeeded {
					// 变更抢在了 Build 之前，被校验逮住。那么封口绝不能发生：
					// 一条校验没过的路径进了矩阵，比它当场报错危险得多。
					if addErr == nil {
						t.Fatal("每一次 Build 都失败，路径却被封了口——校验与封口之间有缝")
					}
					continue
				}

				if addErr != nil {
					t.Fatalf("有 Build 成功，路径却没封口：%v", addErr)
				}
				if tc.tainted != nil && tc.tainted(r, m) {
					t.Fatal("Build 成功了，却带着一手 Build 本该拒绝的变更——校验与封口之间有缝")
				}

				again, err := r.Build()
				if err != nil || again != r {
					t.Fatalf("封口之后的二次 Build 应当原样返回，实际 (%p, %v)", again, err)
				}
			}
		})
	}
}

// TestConcurrentFirstBuildWithBenignMutatorsIsAtomic 走的是两种顺序都合法的那条。
//
// 上一条测的是「变更非法」——失败分支本身就是断言。这条挑的全是 Build 会放行的
// 变更：MarkHomogeneous、Pass 一项尚未声明的能力、兑现一扇合法的新门。无论它们
// 抢在 Build 之前（被校验一并放行）还是落在封口之后（空操作），Build 都必须成功。
//
// 于是失败分支不再兜底，任何一次 Build 报错都说明校验读到了半成品状态。
func TestConcurrentFirstBuildWithBenignMutatorsIsAtomic(t *testing.T) {
	// 一扇归属表里没有的门：未知门不在错绑检查之列，兑现一项可交付能力
	// 就是一手合法的开门。
	const probe = Endpoint("/api/v1/services/aigc/never-opened/generation")

	for range raceAttempts {
		// 少声明一项 web_search，好让并发的 Pass 有一格真的可写——
		// 拿一格已声明的去 Pass 只会撞上重复检查，什么都测不到。
		r := NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
			Pass(withoutCapability(ExpressibleSet(ProtoDashScopeNative), canonical.CapWebSearch)...).
			Redeem(EndpointDashScopeTextGeneration, canonical.CapTextGeneration)

		outcomes := make([]buildOutcome, raceWorkers)
		fns := make([]func(), 0, raceWorkers+1)
		for i := range raceWorkers {
			fns = append(fns, func() {
				got, err := r.Build()
				outcomes[i] = buildOutcome{route: got, err: err}
			})
		}
		fns = append(fns, func() {
			r.MarkHomogeneous()
			r.Pass(canonical.CapWebSearch)
			r.Redeem(probe, canonical.CapStreaming)
		})
		runConcurrently(fns...)

		succeeded := assertOutcomeShape(t, r, outcomes)

		// Pass 抢在 Build 之前是合法的一手，Build 必须放行；抢在之后是空操作，
		// 那 web_search 就缺一格声明，Build 必须以 undeclared 失败。两种都合法，
		// 但同一轮里必须整齐划一——半数成功半数失败说明各 goroutine 看到了
		// 不同的中间状态。
		for _, o := range outcomes {
			if (o.err == nil) != succeeded {
				t.Fatal("同一轮里 Build 有成功有失败——各 goroutine 读到了不同的中间状态")
			}
		}

		m := NewMatrix()
		addErr := m.Add(r, nil)
		if succeeded != (addErr == nil) {
			t.Fatalf("Build 成功与否（%v）和封口与否（%v）对不上——校验与封口之间有缝",
				succeeded, addErr == nil)
		}
		if !succeeded {
			continue
		}

		// 封口了就必须是个通过校验的状态：探针门要么整扇开着（变更抢在前），
		// 要么整扇没开（落在封口后），不该出现「开了却没兑现任何能力」这种
		// 只有半途写入才造得出来的形状。
		if r.ImplementedAt(probe) != slices.Contains(r.Endpoints(), probe) {
			t.Fatal("探针门的开关状态与门清单对不上——封口时兑现集合正被人改写")
		}
		in := Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeTextGeneration}
		assertPassthrough(t, m, in, canonical.CapTextGeneration, "并发良性变更之后")

		again, err := r.Build()
		if err != nil || again != r {
			t.Fatalf("封口之后的二次 Build 应当原样返回，实际 (%p, %v)", again, err)
		}
	}
}

// withoutCapability 从能力切片里剔掉一项，返回新切片。
func withoutCapability(caps []canonical.Capability, drop canonical.Capability) []canonical.Capability {
	out := make([]canonical.Capability, 0, len(caps))
	for _, c := range caps {
		if c != drop {
			out = append(out, c)
		}
	}
	return out
}

// TestConcurrentBuildAndAddIsRaceFree 封住 built 的发布语义。
//
// Add 是矩阵唯一的入口，它凭 built 判断这条路径受过校验没有。裸读一个正被
// Build 写着的 bool 是数据竞争：-race 下会炸，而没有 -race 的生产环境只是
// 悄悄地坏——Add 可能读到 true 却看不见 Build 写进 rules 的 N/A 格子，
// 一条只有一半内容的路径就此落进矩阵。
//
// 只起一个 Add：Matrix 的 routes 是普通 map，多个 Add 并发本就是另一回事，
// 而这里要单独钉住的是 built 的可见性。
func TestConcurrentBuildAndAddIsRaceFree(t *testing.T) {
	for range raceAttempts {
		r := freshFullyDeclaredRoute()
		m := NewMatrix()

		var addErr error
		fns := make([]func(), 0, raceWorkers+1)
		for range raceWorkers {
			fns = append(fns, func() { _, _ = r.Build() })
		}
		fns = append(fns, func() { addErr = m.Add(r, nil) })
		runConcurrently(fns...)

		// Add 抢在 Build 之前是合法的（拒绝未封口的路径），但只要它收下了，
		// 收下的就必须是一条内容完整、通过校验的路径。
		if addErr != nil {
			continue
		}
		in := Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeTextGeneration}
		assertPassthrough(t, m, in, canonical.CapTextGeneration, "并发 Build 与 Add 之后")
		assertNotImplemented(t, m, in, canonical.CapStreaming, "并发 Build 与 Add 之后")
	}
}
