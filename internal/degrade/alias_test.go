package degrade

import (
	"slices"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// aliasProbe 是一扇归属表里没有、也从未投放的门。
//
// 拿已投放的门当探针，「别名开不出新门」这条断言会因为「本来就开着」而恒真。
const aliasProbe = Endpoint("/api/v1/services/aigc/never-aliased/generation")

// Route 是导出类型，所以包外任何人都写得出 `copy := *r`——这不是滥用，
// 而是 Go 值语义的默认权利。本文件守的就是这一手不能成为绕过封口的后门。
//
// 浅拷贝把 rules 与 redeemed 两张 map 的**同一份底层存储**带了过去，却曾经把
// 锁、封口位、快通道位、错误清单各复制一份。于是别名手里拿着一把谁也不认识的
// 锁、一个永远为 false 的封口位，去写原件正在读的那两张 map：原件 Build 只封
// 得住自己，别名照样能追加兑现、翻转快通道、改写处置——而这些改动会原样出现在
// 已经进了矩阵的那条路径上。

// TestShallowCopyBeforeBuildCannotMutateSealedRoute 是这道闸门的主证人。
//
// 封口前拷一份，原件 Build 并进矩阵，然后从别名走遍全部写入口。矩阵能观察到的
// 一切——门清单、快通道事实、逐项裁决——都必须原封不动。
func TestShallowCopyBeforeBuildCannotMutateSealedRoute(t *testing.T) {
	r := freshFullyDeclaredRoute()

	// 封口之前拷贝：此刻原件与别名都还没封口，这正是最危险的时点。
	alias := *r

	built, err := r.Build()
	if err != nil {
		t.Fatal(err)
	}
	m := NewMatrix()
	if err := m.Add(built, nil); err != nil {
		t.Fatal(err)
	}

	in := Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeTextGeneration}
	wantEndpoints := r.Endpoints()
	wantHomogeneous := r.IsHomogeneous()

	// 别名走遍全部写入口，每一手都是 Build 明令拒绝或封口之后不该发生的。
	alias.Redeem(EndpointDashScopeTextGeneration, canonical.CapStreaming)
	alias.Redeem(aliasProbe, canonical.CapStreaming)
	alias.Redeem(Endpoint(""), canonical.CapTextGeneration)
	alias.Override(canonical.CapTextGeneration, Reject, "别名改写的处置")
	alias.Pass(canonical.CapStreaming)
	alias.Degrade("别名追加的降级", canonical.CapWebSearch)
	alias.MarkHomogeneous()

	// 原件视角：矩阵里那条路径一格没变。
	if got := r.Endpoints(); !slices.Equal(got, wantEndpoints) {
		t.Errorf("别名开出了新门：门清单从 %v 变成 %v", wantEndpoints, got)
	}
	if r.Redeems(EndpointDashScopeTextGeneration, canonical.CapStreaming) {
		t.Error("别名给已开门追加了 streaming")
	}
	if r.ImplementedAt(aliasProbe) {
		t.Error("别名开出了一扇从未受过校验的门")
	}
	if r.IsHomogeneous() != wantHomogeneous {
		t.Errorf("别名翻转了快通道事实：从 %v 变成 %v", wantHomogeneous, r.IsHomogeneous())
	}
	assertPassthrough(t, m, in, canonical.CapTextGeneration, "别名写入之后")
	assertNotImplemented(t, m, in, canonical.CapStreaming, "别名写入之后")

	// 别名视角：它读到的必须是同一份既成事实，而不是自己那份副本。
	if got := alias.Endpoints(); !slices.Equal(got, wantEndpoints) {
		t.Errorf("别名自己读到的门清单是 %v，应与原件一致的 %v", got, wantEndpoints)
	}
	if alias.IsHomogeneous() != wantHomogeneous {
		t.Errorf("别名自己读到的快通道事实是 %v，应为 %v", alias.IsHomogeneous(), wantHomogeneous)
	}
}

// TestShallowCopySharesSealRatherThanCopyingIt 把封口位单独钉死。
//
// 上一条从写入口观察，这条从入库口观察：原件 Build 之后，别名必须**已经是**
// 封口状态，Add 收得下它。封口位若是各拷一份，别名手里就永远是一条「没受过
// 校验」的路径——反过来说，只要它还能被当成未封口，写入口的闸门对它就一概不设防。
func TestShallowCopySharesSealRatherThanCopyingIt(t *testing.T) {
	r := freshFullyDeclaredRoute()
	alias := *r

	if _, err := r.Build(); err != nil {
		t.Fatal(err)
	}

	m := NewMatrix()
	if err := m.Add(&alias, nil); err != nil {
		t.Fatalf("原件封口之后别名也该是封口的，Add 却拒绝了它：%v", err)
	}
}

// TestShallowCopySharesHomogeneousFact 锁住快通道事实的两段共享。
//
// 两个方向都要验：只验一个方向，一个「拷贝时同步一次」的实现也能过，
// 而那种实现在另一个方向上照样各走各的。
func TestShallowCopySharesHomogeneousFact(t *testing.T) {
	t.Run("别名标记原件看得见", func(t *testing.T) {
		r := freshFullyDeclaredRoute()
		alias := *r
		alias.MarkHomogeneous()

		if !alias.IsHomogeneous() {
			t.Error("别名标记之后自己该读到 true")
		}
		if !r.IsHomogeneous() {
			t.Error("别名标的快通道，原件读不到——两边各拿着一份副本")
		}
	})

	t.Run("原件标记别名看得见", func(t *testing.T) {
		r := freshFullyDeclaredRoute()
		alias := *r
		r.MarkHomogeneous()

		if !alias.IsHomogeneous() {
			t.Error("原件标的快通道，别名读不到——两边各拿着一份副本")
		}
	})

	t.Run("封口之后两边都是空操作", func(t *testing.T) {
		r := freshFullyDeclaredRoute()
		alias := *r
		if _, err := r.Build(); err != nil {
			t.Fatal(err)
		}

		alias.MarkHomogeneous()
		if alias.IsHomogeneous() || r.IsHomogeneous() {
			t.Error("封口之后别名仍能翻转快通道位，选路偏好可以被运行时掀翻")
		}
	})
}

// TestConcurrentBuildThroughAliasSharesSeal 把两个别名的 Build 撞在一起。
//
// 锁若各拷一份，这两次 Build 就在两把互不相识的锁下并发写同一张 rules——
// 裸的数据竞争；而且后进来那个会把先进来那个补的 N/A 当成「声明了表达不出来
// 的能力」，给一条声明完整的路径判死。
//
// 包装指针不同是允许的：Build 交回的就是各自的接收者。共享的是封口与内容，
// 于是两个包装看到的身份、门清单与裁决必须一模一样，各自也都进得了矩阵。
func TestConcurrentBuildThroughAliasSharesSeal(t *testing.T) {
	for range raceAttempts {
		r := freshFullyDeclaredRoute()
		alias := *r

		var fromOriginal, fromAlias buildOutcome
		runConcurrently(
			func() {
				got, err := r.Build()
				fromOriginal = buildOutcome{route: got, err: err}
			},
			func() {
				got, err := alias.Build()
				fromAlias = buildOutcome{route: got, err: err}
			},
		)

		if fromOriginal.err != nil {
			t.Fatalf("声明完整的路径经原件 Build 不该失败，实际 %v", fromOriginal.err)
		}
		if fromAlias.err != nil {
			t.Fatalf("声明完整的路径经别名 Build 不该失败，实际 %v", fromAlias.err)
		}
		if fromOriginal.route != r {
			t.Fatalf("原件 Build 应当交回自己，实际 %p != %p", fromOriginal.route, r)
		}
		if fromAlias.route != &alias {
			t.Fatalf("别名 Build 应当交回自己，实际 %p != %p", fromAlias.route, &alias)
		}

		// 身份与门清单是同一份事实，两个包装读出来必须一致。
		if fromOriginal.route.InProtocol() != fromAlias.route.InProtocol() ||
			fromOriginal.route.OutProvider() != fromAlias.route.OutProvider() {
			t.Fatal("两个包装读出了不同的身份")
		}
		if !slices.Equal(fromOriginal.route.Endpoints(), fromAlias.route.Endpoints()) {
			t.Fatalf("两个包装读出了不同的门清单：%v vs %v",
				fromOriginal.route.Endpoints(), fromAlias.route.Endpoints())
		}

		// 任一包装都进得了矩阵，且裁决不会分叉。
		in := Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeTextGeneration}
		for _, tc := range []struct {
			name string
			r    *Route
		}{
			{"原件", fromOriginal.route},
			{"别名", fromAlias.route},
		} {
			m := NewMatrix()
			if err := m.Add(tc.r, nil); err != nil {
				t.Fatalf("%s 已封口却进不了矩阵：%v", tc.name, err)
			}
			assertPassthrough(t, m, in, canonical.CapTextGeneration, tc.name+"并发 Build 之后")
			assertNotImplemented(t, m, in, canonical.CapStreaming, tc.name+"并发 Build 之后")
		}
	}
}

// TestAliasMutatorsRaceFreeAgainstOriginalBuild 让别名的写入口与原件的首次
// Build 正面相撞。
//
// 锁各拷一份的话，这两边就是在两把互不相识的锁下并发写同一张 redeemed。
// 共享之后只剩两种合法结局：变更抢在 Build 之前（被同一套校验一并放行），
// 或落在封口之后（确定性的空操作）。
func TestAliasMutatorsRaceFreeAgainstOriginalBuild(t *testing.T) {
	for range raceAttempts {
		r := freshFullyDeclaredRoute()
		alias := *r

		outcomes := make([]buildOutcome, raceWorkers)
		fns := make([]func(), 0, raceWorkers+1)
		for i := range raceWorkers {
			fns = append(fns, func() {
				got, err := r.Build()
				outcomes[i] = buildOutcome{route: got, err: err}
			})
		}
		fns = append(fns, func() {
			alias.MarkHomogeneous()
			alias.Redeem(aliasProbe, canonical.CapStreaming)
		})
		runConcurrently(fns...)

		for _, o := range outcomes {
			if o.err != nil {
				t.Fatalf("别名的合法变更不该让 Build 失败，实际 %v", o.err)
			}
		}
		assertOutcomeShape(t, r, outcomes)

		// 探针门要么整扇开着（变更抢在前），要么整扇没开（落在封口后）；
		// 「开了却没兑现任何能力」只有半途写入才造得出来。
		if r.ImplementedAt(aliasProbe) != slices.Contains(r.Endpoints(), aliasProbe) {
			t.Fatal("探针门的开关状态与门清单对不上——封口时兑现集合正被别名改写")
		}
		// 别名与原件读的是同一份事实，不该有分歧。
		if alias.ImplementedAt(aliasProbe) != r.ImplementedAt(aliasProbe) {
			t.Fatal("别名与原件对探针门的看法不一致——兑现集合被拆成了两份")
		}
		if alias.IsHomogeneous() != r.IsHomogeneous() {
			t.Fatal("别名与原件对快通道事实的看法不一致——它被拆成了两份")
		}
	}
}

// TestSealedRouteAliasMutatorsRaceFreeAgainstCheck 是运行时那一半。
//
// 「进了矩阵的路径都是只读的」是 Check / Endpoints / Implemented 一概不加锁的
// 前提。别名手里若攥着一个独立的封口位，它就会在原件已经进了矩阵之后继续写
// redeemed——与并发的 Check 读同一张 map，-race 下会炸，没有 -race 的生产环境
// 只是悄悄地坏。
func TestSealedRouteAliasMutatorsRaceFreeAgainstCheck(t *testing.T) {
	r := freshFullyDeclaredRoute()
	alias := *r

	built, err := r.Build()
	if err != nil {
		t.Fatal(err)
	}
	m := NewMatrix()
	if err := m.Add(built, nil); err != nil {
		t.Fatal(err)
	}
	in := Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeTextGeneration}

	fns := make([]func(), 0, 2*raceWorkers)
	for range raceWorkers {
		fns = append(fns, func() {
			alias.Redeem(aliasProbe, canonical.CapStreaming)
			alias.MarkHomogeneous()
		})
		fns = append(fns, func() {
			_, _ = m.Check(in, ProviderDashScopeNative,
				[]canonical.Capability{canonical.CapTextGeneration})
			_ = r.Endpoints()
			_ = r.Implemented()
			_ = r.IsHomogeneous()
		})
	}
	runConcurrently(fns...)

	if r.ImplementedAt(aliasProbe) || alias.ImplementedAt(aliasProbe) {
		t.Error("封口之后别名仍开出了新门")
	}
	if r.IsHomogeneous() || alias.IsHomogeneous() {
		t.Error("封口之后别名仍翻转了快通道位")
	}
}
