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

	for in, candidates := range byInbound {
		ranked := m.RankOutbound(in, candidates)

		for i := 1; i < len(ranked); i++ {
			prev, _ := m.Route(in, ranked[i-1])
			cur, _ := m.Route(in, ranked[i])

			ps, cs := prev.Preservation().Score(), cur.Preservation().Score()
			if ps < cs {
				t.Errorf("入站 %s：偏好序把 %s（保留度 %.3f）排在 %s（保留度 %.3f）之前，"+
					"但后者保留的原生能力更多。要么 OutboundPreference 写错了，"+
					"要么矩阵声明写错了",
					in, ranked[i-1], ps, ranked[i], cs)
			}
		}

		if len(ranked) > 0 {
			t.Logf("%-20s 首选 %-24s 保留度 %.3f", in, ranked[0],
				mustRoute(t, m, in, ranked[0]).Preservation().Score())
		}
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
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

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
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

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
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	// 视频输入：openai.compat 拒绝，dashscope.compatible 降级但可用。
	caps := []canonical.Capability{canonical.CapTextGeneration, canonical.CapVideoInput}

	got, verdict, err := m.BestOutbound(ProtoOpenAIResponses, []Provider{
		ProviderOpenAICompat,
		ProviderDashScopeCompatible,
		ProviderDashScopeNative,
	}, caps)
	if err != nil {
		t.Fatalf("应当能找到可用路径: %v", err)
	}
	if got != ProviderDashScopeCompatible {
		t.Errorf("选中 %q，期望跳过拒绝视频输入的 openai.compat 后选中 dashscope.compatible", got)
	}
	if len(verdict.Degraded) == 0 {
		t.Error("该路径对视频输入是降级，应当在 Verdict 中报告出来")
	}
}

// TestBestOutboundReportsWhyEverythingFailed 保证全部候选都跑不通时，
// 返回的是「缺什么」而不是笼统的「无可用 Provider」。
func TestBestOutboundReportsWhyEverythingFailed(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = m.BestOutbound(ProtoOpenAIChat,
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
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = m.BestOutbound(ProtoOpenAIImages,
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
	pass, deg, rej := r.Stats()
	if got, want := pass+deg+rej, len(canonical.AllCapabilities()); got != want {
		t.Errorf("派生路径覆盖 %d 项能力，应为 %d 项", got, want)
	}
}

// TestResponsesStatelessReasonDiffersFromChat 固化一处真实差异。
//
// Chat Completions 是协议本身没有服务端会话；Responses 是协议支持而网关
// 主动选择不用。对客户端而言这两句话含义完全不同——一个是「换个端点」，
// 一个是「等下个版本」。
func TestResponsesStatelessReasonDiffersFromChat(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	chat, _ := m.Lookup(ProtoOpenAIChat, ProviderOpenAICompat, canonical.CapStatefulConversation)
	resp, _ := m.Lookup(ProtoOpenAIResponses, ProviderOpenAICompat, canonical.CapStatefulConversation)

	if chat.Disposition != Reject || resp.Disposition != Reject {
		t.Fatalf("两者都应为 REJECT，实际 chat=%s responses=%s",
			chat.Disposition, resp.Disposition)
	}
	if chat.Note == resp.Note {
		t.Error("两者的拒绝理由不同，不应共用同一句说明")
	}
	if !strings.Contains(resp.Note, "store=false") {
		t.Errorf("Responses 的说明应指出是无状态模式的选择，实际: %s", resp.Note)
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
	best := native.Preservation().Score()

	for _, r := range m.Routes() {
		if r.In == ProtoDashScopeNative {
			continue
		}
		if s := r.Preservation().Score(); s > best {
			t.Errorf("路径 %s -> %s 的保留度 %.3f 高于 DashScope 原生直通的 %.3f，"+
				"原生直通本应是上限", r.In, r.Out, s, best)
		}
	}
	t.Logf("DashScope 原生直通保留度 %.3f（%d 项透传）", best, native.Preservation().Passthrough)
}

func mustRoute(t *testing.T, m *Matrix, in Protocol, out Provider) *Route {
	t.Helper()
	r, ok := m.Route(in, out)
	if !ok {
		t.Fatalf("路径 %s -> %s 未注册", in, out)
	}
	return r
}
