package degrade

import (
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// TestPreferenceMatchesPreservation 是这个文件的核心。
//
// OutboundPreference 是人手写的一串顺序，很容易写成拍脑袋的结果。这条测试
// 拿它与矩阵里实际的处置格子对账：对同一个入站协议，排得越靠前的出站
// Provider，保留的原生能力必须不少于排在它后面的。
//
// 也就是说，如果有人新增一条声称优先、实际却丢更多能力的路径，CI 会拦下来。
// 「尽量保留原生能力」由此从口号变成可执行的约束。
func TestPreferenceMatchesPreservation(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	byInbound := map[Protocol][]Provider{}
	for _, r := range m.Routes() {
		byInbound[r.In] = append(byInbound[r.In], r.Out)
	}

	// 用 RankDesign 而不是 RankOutbound：这条测试验的是**偏好序的设计**是否
	// 自洽，与路径实现了没有无关。用 RankOutbound 会在 M0 阶段（零条已实现）
	// 退化成空转——而空转通过的检查正是这轮排查要消灭的东西。
	//
	// 只用设计列：设计计数与端点无关，传 Endpoint("") 合法（A3）。
	// 绝不从这条调用取可用列——零值端点的可用列恒为零。
	for in, candidates := range byInbound {
		ranked := m.RankDesign(in, candidates)

		for i := 1; i < len(ranked); i++ {
			prev, _ := m.Route(in, ranked[i-1])
			cur, _ := m.Route(in, ranked[i])

			ps := prev.Preservation(m.Availability(), Endpoint("")).DesignScore()
			cs := cur.Preservation(m.Availability(), Endpoint("")).DesignScore()
			if ps < cs {
				t.Errorf("入站 %s：偏好序把 %s（设计保留度 %.3f）排在 %s（%.3f）之前，"+
					"但后者保留的原生能力更多。要么 OutboundPreference 写错了，"+
					"要么矩阵声明写错了",
					in, ranked[i-1], ps, ranked[i], cs)
			}
		}

		if len(ranked) > 0 {
			t.Logf("%-20s 首选 %-24s 设计保留度 %.3f", in, ranked[0],
				mustRoute(t, m, in, ranked[0]).
					Preservation(m.Availability(), Endpoint("")).DesignScore())
		}
	}
}

// TestRuntimeRankingUsesAvailableScore 验证选路那一列的对账。
//
// 与 TestPreferenceMatchesPreservation 互补：这条验的是「运行时选路是否正确」，
// 因此必须用已实现路径的**端点相对**可用分数（见 ADR-0002）。
func TestRuntimeRankingUsesAvailableScore(t *testing.T) {
	m := implementedMatrix(t, nil)

	// 半句一：每扇已开门的可用分在 (0, DesignScore] 区间。
	for _, r := range m.Routes() {
		design := r.Preservation(m.Availability(), Endpoint("")).DesignScore()
		for _, ep := range r.Endpoints() {
			s := r.Preservation(m.Availability(), ep).AvailableScore()
			if s <= 0 || s > design {
				t.Errorf("路径 %s -> %s 门 %s 的可用分 %.3f 应在 (0, 设计 %.3f] 区间",
					r.In, r.Out, ep, s, design)
			}
		}
	}

	// 半句二：同一入站协议下兑现了同一扇门的多条路径按偏好序比较。
	// Phase 1 没有两条路径同开一扇门，当前此半句空转；将来同门第二路径出现时
	// 排序对账自动生效，不会漏。
	byInboundDoor := map[Protocol]map[Endpoint][]Provider{}
	for _, r := range m.Routes() {
		for _, ep := range r.Endpoints() {
			if byInboundDoor[r.In] == nil {
				byInboundDoor[r.In] = map[Endpoint][]Provider{}
			}
			byInboundDoor[r.In][ep] = append(byInboundDoor[r.In][ep], r.Out)
		}
	}
	for in, doors := range byInboundDoor {
		for ep, providers := range doors {
			ranked := m.RankOutbound(in, providers)
			for i := 1; i < len(ranked); i++ {
				prev, _ := m.Route(in, ranked[i-1])
				cur, _ := m.Route(in, ranked[i])
				ps := prev.Preservation(m.Availability(), ep).AvailableScore()
				cs := cur.Preservation(m.Availability(), ep).AvailableScore()
				if ps < cs {
					t.Errorf("入站 %s 门 %s：运行时选路把 %s（可用 %.3f）排在 %s（%.3f）之前",
						in, ep, ranked[i-1], ps, ranked[i], cs)
				}
			}
		}
	}
}

// TestPlannedRoutesAreNotRoutable 固化「选路只走已实现的路径」。
func TestPlannedRoutesAreNotRoutable(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	// 这两条都还是 PLANNED（openai.compat 那条已在 M1 转正，不在此列）。
	planned := []Provider{ProviderDashScopeCompatible, ProviderDashScopeNative}

	if ranked := m.RankOutbound(ProtoOpenAIResponses, planned); len(ranked) != 0 {
		t.Errorf("未实现的路径不该进入选路，实际 %v", ranked)
	}

	// 但设计视角下它们仍然存在，否则文档就没得写了。
	if len(m.RankDesign(ProtoOpenAIResponses, planned)) != 2 {
		t.Error("RankDesign 不应过滤未实现的路径")
	}
}

// TestGatedEmulationSplitsTheTwoColumns 固化两列分数的差异来源。
//
// convstore 默认关闭，因此带 EMULATE 格子的路径在两列下取值不同——
// 这正是 ADR-0002 里「0.929（开启 convstore 后 1.000）」那种写法的由来。
func TestGatedEmulationSplitsTheTwoColumns(t *testing.T) {
	m := implementedMatrix(t, nil) // 默认可用性：convstore 关闭

	r := mustRoute(t, m, ProtoOpenAIResponses, ProviderOpenAICompat)
	p := r.Preservation(m.Availability(), EndpointOpenAIResponses)

	if !p.Gated() {
		t.Fatal("该路径有 EMULATE 格子且开关默认关闭，两列分数应当不同")
	}
	if p.DesignScore() <= p.AvailableScore() {
		t.Errorf("设计目标应严格高于当前可用，实际 设计=%.3f 可用=%.3f",
			p.DesignScore(), p.AvailableScore())
	}

	// 开启之后两列应当合一。
	on := implementedMatrix(t, Availability{FeatureConversationStore: true})
	po := mustRoute(t, on, ProtoOpenAIResponses, ProviderOpenAICompat).
		Preservation(on.Availability(), EndpointOpenAIResponses)
	if po.Gated() {
		t.Error("开关开启后不应再有被关掉的模拟能力")
	}
	if po.AvailableScore() != po.DesignScore() {
		t.Errorf("开关全开时两列应当一致，实际 设计=%.3f 可用=%.3f",
			po.DesignScore(), po.AvailableScore())
	}
}

// TestAvailableScoreCountsOnlyRedeemed 防的是「设计满分被当成可用满分」。
//
// DashScope Native 的设计目标仍是 1.000——那条路最终该是零损失的同源直通，
// 这个结论不因为投放进度而改变。但此刻只投放了 18 项里的 5 项，若可用分数也
// 报 1.000，选路就会拿一个远超实际的分数去和别的路径比，把请求送进 501。
func TestAvailableScoreCountsOnlyRedeemed(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	p := mustRoute(t, m, ProtoDashScopeNative, ProviderDashScopeNative).
		Preservation(m.Availability(), EndpointDashScopeTextGeneration)

	if got := p.DesignScore(); got != 1.0 {
		t.Errorf("设计目标应保持 1.000（同源直通零损失），实际 %.3f", got)
	}
	if want := len(ExpressibleSet(ProtoDashScopeNative)) - 5; p.NotRedeemed != want {
		t.Errorf("未投放格子数 = %d，期望 %d", p.NotRedeemed, want)
	}
	if want := 5.0 / 18.0; p.AvailableScore() != want {
		t.Errorf("当前可用应为 %.3f（18 项中 5 项已投放），实际 %.3f", want, p.AvailableScore())
	}
	if !p.Gated() {
		t.Error("存在未投放的格子时两列分数应当不同")
	}
}

// TestInboundPriorityIsRegistered 保证优先级序列里已排到的协议真的有路径，
// 而不是只写在常量里好看。
func TestInboundPriorityIsRegistered(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	registered := map[Protocol]bool{}
	for _, r := range m.Routes() {
		registered[r.In] = true
	}
	for i, f := range InboundPriority {
		if !f.Implemented() {
			continue
		}
		for _, p := range f.Protocols {
			if !registered[p] {
				t.Errorf("入站优先级第 %d 位「%s」族的 %q 没有任何已注册路径", i+1, f.Name, p)
			}
		}
	}
}

// TestOpenAIProtocolsShareOneFamily 固化 OpenAI Chat 与 Responses 同族同档。
//
// 两者共用 openaiwire 的编解码与错误信封，Responses 的矩阵路径本就从 Chat
// 派生。把它们拆到不同优先级档位既不符合实现现实，也会让排期显得比实际更长。
func TestOpenAIProtocolsShareOneFamily(t *testing.T) {
	familyOf := map[Protocol]string{}
	for _, f := range InboundPriority {
		for _, p := range f.Protocols {
			if prev, dup := familyOf[p]; dup {
				t.Errorf("协议 %q 同时出现在「%s」和「%s」族中", p, prev, f.Name)
			}
			familyOf[p] = f.Name
		}
	}

	chat, resp := familyOf[ProtoOpenAIChat], familyOf[ProtoOpenAIResponses]
	if chat == "" || resp == "" {
		t.Fatalf("OpenAI Chat / Responses 未登记到任何协议族（chat=%q responses=%q）", chat, resp)
	}
	if chat != resp {
		t.Errorf("OpenAI Chat 在「%s」族、Responses 在「%s」族，两者应同族", chat, resp)
	}

	// 族内顺序仍要体现表达力差异：Responses 在前。
	var openai InboundFamily
	for _, f := range InboundPriority {
		if f.Name == resp {
			openai = f
		}
	}
	if len(openai.Protocols) < 2 || openai.Protocols[0] != ProtoOpenAIResponses {
		t.Errorf("OpenAI 族内应把表达力更强的 Responses 排在前，实际 %v", openai.Protocols)
	}
}

// TestUnimplementedFamiliesAreDeclared 保证尚未接入的协议族登记在优先级里，
// 而不是散落在文档的某句话里——排队顺序本身就是一项决策。
func TestUnimplementedFamiliesAreDeclared(t *testing.T) {
	var pending []string
	for _, f := range InboundPriority {
		if !f.Implemented() {
			pending = append(pending, f.Name)
		}
	}
	if len(pending) != 2 || pending[0] != "Anthropic Messages" || pending[1] != "Gemini" {
		t.Errorf("待接入协议族应为 [Anthropic Messages, Gemini]，实际 %v", pending)
	}
}

// TestOutboundPreferenceFavorsPassthrough 固化「出站优先 OpenAI 兼容，
// 次选 DashScope Compatible 穿透」。两者都是字节透传，语义零损失。
func TestOutboundPreferenceFavorsPassthrough(t *testing.T) {
	m := implementedMatrix(t, nil)

	// 故意打乱输入顺序，验证排序真的生效而不是碰巧。
	got := m.RankOutbound(ProtoOpenAIResponses, []Provider{
		ProviderAnthropicMessages,
		ProviderDashScopeNative,
		ProviderOpenAICompat,
		ProviderDashScopeCompatible,
	})

	want := []Provider{
		ProviderOpenAICompat,
		ProviderDashScopeCompatible,
		ProviderDashScopeNative,
		ProviderAnthropicMessages,
	}
	if len(got) != len(want) {
		t.Fatalf("排序结果长度 = %d, 期望 %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 位 = %q, 期望 %q（完整结果 %v）", i, got[i], want[i], got)
		}
	}
}

// TestRankOutboundDropsUnregistered 保证没有注册路径的候选被剔除，
// 而不是排在末尾等着被选中。
func TestRankOutboundDropsUnregistered(t *testing.T) {
	m := implementedMatrix(t, nil)

	got := m.RankOutbound(ProtoDashScopeNative, []Provider{
		ProviderAnthropicMessages, // 未注册
		ProviderDashScopeNative,   // 已注册
	})
	if len(got) != 1 || got[0] != ProviderDashScopeNative {
		t.Errorf("未注册的候选应被剔除，实际 %v", got)
	}
}

// TestBestOutboundSkipsRejectingRoute 验证选路会真的去查能力。
//
// 排在前面但会拒绝本次请求所需能力的路径，不如排在后面但真能跑通的路径——
// 光按偏好序选第一个，会让一个本可以成功的请求失败。
func TestBestOutboundSkipsRejectingRoute(t *testing.T) {
	// 用一个自造的两条路径矩阵，而不是 Phase1——Phase1 里的首选路径恰好是
	// 同源直通、什么都接受，构造不出「首选拒绝」的场景。测试要验的是选路逻辑，
	// 不该受真实矩阵当下形态的牵制。
	m := NewMatrix()

	var others []canonical.Capability
	for _, c := range ExpressibleSet(ProtoOpenAIChat) {
		if c != canonical.CapVisionInput {
			others = append(others, c)
		}
	}

	// 偏好序里排第一，但拒绝视觉输入。
	if err := m.Add(NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
		Pass(others...).
		Reject("测试用：这条路径不支持视觉输入", canonical.CapVisionInput).
		Redeem(EndpointOpenAIChat, others...).
		Build()); err != nil {
		t.Fatal(err)
	}
	// 排第二，但真能跑通。
	if err := m.Add(NewRoute(ProtoOpenAIChat, ProviderDashScopeCompatible).
		Pass(ExpressibleSet(ProtoOpenAIChat)...).
		Redeem(EndpointOpenAIChat, ExpressibleSet(ProtoOpenAIChat)...).
		Build()); err != nil {
		t.Fatal(err)
	}

	caps := []canonical.Capability{canonical.CapTextGeneration, canonical.CapVisionInput}
	got, _, err := m.BestOutbound(Inbound{Protocol: ProtoOpenAIChat, Endpoint: EndpointOpenAIChat},
		[]Provider{ProviderOpenAICompat, ProviderDashScopeCompatible}, caps)
	if err != nil {
		t.Fatalf("应当能找到可用路径: %v", err)
	}
	if got != ProviderDashScopeCompatible {
		t.Errorf("选中 %q，期望跳过拒绝视觉输入的首选路径后选中 dashscope.compatible", got)
	}
}

// TestBestOutboundPrefersHomogeneous 固化「同源优先于全局偏好序」。
//
// 反例是真实存在的：对 dashscope.realtime 入站，DashScope 侧直通是零损失的，
// 而 openai.realtime 要反向重采样并丢掉 input_image_buffer——可在全局偏好序里
// openai.realtime 排得更靠前。不把同源提到最前，选路就会主动挑一条更差的路。
func TestBestOutboundPrefersHomogeneous(t *testing.T) {
	m := implementedMatrix(t, nil)

	ranked := m.RankOutbound(ProtoDashScopeRealtime,
		[]Provider{ProviderOpenAIRealtime, ProviderDashScopeWSRealtime})
	if len(ranked) == 0 || ranked[0] != ProviderDashScopeWSRealtime {
		t.Fatalf("同源直通应排第一，实际顺序 %v", ranked)
	}

	homo := mustRoute(t, m, ProtoDashScopeRealtime, ProviderDashScopeWSRealtime)
	hetero := mustRoute(t, m, ProtoDashScopeRealtime, ProviderOpenAIRealtime)
	// 只比设计列：设计计数与端点无关，传 Endpoint("") 合法（A3）。
	if homo.Preservation(m.Availability(), Endpoint("")).DesignScore() <=
		hetero.Preservation(m.Availability(), Endpoint("")).DesignScore() {
		t.Error("同源直通的保留度应当严格高于异构转换，否则这条偏好没有依据")
	}
}

// TestBestOutboundReportsWhyEverythingFailed 保证全部候选都跑不通时，
// 返回的是「缺什么」而不是笼统的「无可用 Provider」。
func TestBestOutboundReportsWhyEverythingFailed(t *testing.T) {
	m := implementedMatrix(t, nil)
	var err error

	_, _, err = m.BestOutbound(testInbound(ProtoOpenAIChat),
		[]Provider{ProviderOpenAICompat, ProviderAnthropicMessages},
		[]canonical.Capability{canonical.CapTextGeneration, canonical.CapReasoningSignature})
	if err == nil {
		t.Fatal("所有候选都拒绝该能力时应当失败")
	}
	if !strings.Contains(err.Error(), "reasoning_signature") {
		t.Errorf("错误信息应指出是哪项能力过不去，实际: %v", err)
	}
}

func TestBestOutboundFailsWhenNothingRegistered(t *testing.T) {
	m := implementedMatrix(t, nil)
	var err error

	_, _, err = m.BestOutbound(Inbound{Protocol: protoNeverRegistered, Endpoint: Endpoint("/test/never")},
		[]Provider{ProviderOpenAICompat},
		[]canonical.Capability{canonical.CapTextGeneration})
	if err == nil {
		t.Fatal("没有任何已注册路径时必须失败，不得回落到某个默认 Provider")
	}
}

// TestDeriveRequiresExistingDeclaration 保证 Override 不能凭空造出声明——
// 那种情况说明基准路径选错了。
func TestDeriveRequiresExistingDeclaration(t *testing.T) {
	base := NewRoute(ProtoOpenAIChat, ProviderOpenAICompat)
	for _, c := range canonical.AllCapabilities() {
		base = base.Pass(c)
	}

	// 先删掉一项，模拟基准路径本身不完整的情形。
	delete(base.rules, canonical.CapRerank)

	_, err := base.Derive(ProtoOpenAIResponses, ProviderOpenAICompat).
		Override(canonical.CapRerank, Reject, "无关紧要").
		Build()
	if err == nil {
		t.Fatal("对从未声明过的能力做 Override 应当失败")
	}
	if !strings.Contains(err.Error(), "nothing to override") {
		t.Errorf("错误信息应指出无可覆盖，实际: %v", err)
	}
}

// TestDerivedRouteStaysComplete 保证派生不会绕过完整性校验。
func TestDerivedRouteStaysComplete(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	r, ok := m.Route(ProtoOpenAIResponses, ProviderOpenAICompat)
	if !ok {
		t.Fatal("openai.responses -> openai.compat 未注册")
	}
	// 只用设计计数，与端点无关（A3）。
	p := r.Preservation(m.Availability(), Endpoint(""))
	declared := p.Passthrough + p.Emulate + p.EmulateOff + p.Degrade + p.Reject
	if want := len(ExpressibleSet(ProtoOpenAIResponses)); declared != want {
		t.Errorf("派生路径为 %d 项可表达能力表态，应为 %d 项", declared, want)
	}
	if total := declared + p.NotApplicable; total != len(canonical.AllCapabilities()) {
		t.Errorf("派生路径的格子总数 %d，应为 %d", total, len(canonical.AllCapabilities()))
	}
}

// TestStatefulConversationIsNAOnChatButEmulatedOnResponses 固化一处真实差异，
// 也是分层之后才说得清的一处。
//
// Chat Completions 的线格式里根本没有 previous_response_id 这类字段，客户端
// 连发都发不出来——那不是「被拒绝」，是不可达，正确的提示是「请改用
// openai.responses」。Responses 表达得出来，于是网关用自己的
// ConversationStore 把它垫上。
//
// 旧模型把两者都记成 REJECT，等于告诉 Chat 用户「这条路不支持」，
// 而真相是「你走错门了」。
func TestStatefulConversationIsNAOnChatButEmulatedOnResponses(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	chat, ok := m.Lookup(ProtoOpenAIChat, ProviderOpenAICompat, canonical.CapStatefulConversation)
	if !ok {
		t.Fatal("openai.chat 的 stateful_conversation 未登记")
	}
	if chat.Disposition != NotApplicable {
		t.Errorf("Chat 应为 N/A（协议表达不出来），实际 %s", chat.Disposition)
	}
	if !strings.Contains(chat.Note, string(ProtoOpenAIResponses)) {
		t.Errorf("N/A 必须指明该去哪个协议，实际: %s", chat.Note)
	}

	resp, ok := m.Lookup(ProtoOpenAIResponses, ProviderOpenAICompat, canonical.CapStatefulConversation)
	if !ok {
		t.Fatal("openai.responses 的 stateful_conversation 未登记")
	}
	if resp.Disposition != Emulate {
		t.Errorf("Responses 应为 EMULATE（网关垫上），实际 %s", resp.Disposition)
	}
	if !strings.Contains(resp.Note, "重启") {
		t.Errorf("EMULATE 的说明必须写明可用性边界，实际: %s", resp.Note)
	}
}

// TestDashScopeNativeInboundPreservesMost 固化「尽量保留原生能力」的极值：
// 讲原生协议的客户端不需要任何转换，这条路径的透传格子数应当是全矩阵最高。
func TestDashScopeNativeInboundPreservesMost(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	native, ok := m.Route(ProtoDashScopeNative, ProviderDashScopeNative)
	if !ok {
		t.Fatal("dashscope.native 入站路径未注册")
	}
	// 只取设计列（A3）：循环里的 PLANNED 路径没有门，只能走零值端点。
	best := native.Preservation(m.Availability(), Endpoint("")).DesignScore()

	for _, r := range m.Routes() {
		if r.In == ProtoDashScopeNative {
			continue
		}
		if s := r.Preservation(m.Availability(), Endpoint("")).DesignScore(); s > best {
			t.Errorf("路径 %s -> %s 的保留度 %.3f 高于 DashScope 原生直通的 %.3f，"+
				"原生直通本应是上限", r.In, r.Out, s, best)
		}
	}
	t.Logf("DashScope 原生直通保留度 %.3f（%d 项透传）", best,
		native.Preservation(m.Availability(), Endpoint("")).Passthrough)
}

func mustRoute(t *testing.T, m *Matrix, in Protocol, out Provider) *Route {
	t.Helper()
	r, ok := m.Route(in, out)
	if !ok {
		t.Fatalf("路径 %s -> %s 未注册", in, out)
	}
	return r
}
