package degrade

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

var updateDoc = flag.Bool("update-matrix", false, "重新生成 docs/degradation-matrix.md")

const docPath = "../../docs/degradation-matrix.md"

// protoNeverRegistered 是测试专用的假协议。
//
// 拿一个真实规划中的协议名当「未注册」的反例，会让读者以为它本该注册；
// 用一个明显不存在的名字，测试意图才不会被误读。
const protoNeverRegistered Protocol = "test.never-registered"

// TestPhase1IsComplete 是这个包存在的理由。
//
// Route.Build 要求每条已注册路径对 canonical.AllCapabilities() 中的每一项都
// 明确表态。新增一个 Capability 常量而忘了更新路径声明时，这个测试会失败并
// 列出漏掉的格子——这正是期望的行为，不是需要绕过的麻烦。
func TestPhase1IsComplete(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatalf("Phase1 矩阵不完整: %v", err)
	}
	if len(m.Routes()) == 0 {
		t.Fatal("Phase1 矩阵为空，完整性检查会退化成空转")
	}

	for _, r := range m.Routes() {
		// 设计列是路径级的；可用列端点相对，逐门另打（见下方 Endpoints 循环）。
		p := r.Preservation(m.Availability(), Endpoint(""))

		// 每条路径必须为入站协议**表达得出来**的每一项能力表态。
		// 表达不出来的那些由 Expressibility 自动补成 N/A，不该由路径负责。
		declared := p.Passthrough + p.Emulate + p.EmulateOff + p.Degrade + p.Reject
		if want := len(ExpressibleSet(r.In)); declared != want {
			t.Errorf("路径 %s -> %s 为 %d 项可表达能力表态，应为 %d 项",
				r.In, r.Out, declared, want)
		}
		// N/A 与已表态的加起来必须是全集，否则有格子凭空消失。
		if total := declared + p.NotApplicable; total != len(canonical.AllCapabilities()) {
			t.Errorf("路径 %s -> %s 的格子总数 %d，应为 %d",
				r.In, r.Out, total, len(canonical.AllCapabilities()))
		}

		status := "规划中"
		if r.Implemented() {
			status = "已实现"
		}
		t.Logf("%-22s -> %-24s %s  pass=%2d emu=%d(off %d) deg=%d rej=%d n/a=%2d  设计=%.3f",
			r.In, r.Out, status,
			p.Passthrough, p.Emulate, p.EmulateOff, p.Degrade, p.Reject, p.NotApplicable,
			p.DesignScore())
		for _, ep := range r.Endpoints() {
			t.Logf("  门 %-58s 可用=%.3f", ep,
				r.Preservation(m.Availability(), ep).AvailableScore())
		}
	}
}

// implementedMatrix 返回一份全部路径都标记为已实现的 Phase1 矩阵。
//
// 用于测试 Check 的能力裁决语义——那部分逻辑与「路径实现了没有」正交，
// 不该因为投放进度就测不了。每条路径挂一扇测试专用门并在其上兑现全部
// 可表达能力，使「能力裁决语义与投放进度正交」的既有测试意图原样保留。
// PLANNED 本身的行为由 TestPlannedRouteIsRejectedAtRuntime 单独覆盖。
func implementedMatrix(t *testing.T, avail Availability) *Matrix {
	t.Helper()
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range m.Routes() {
		r.Redeem(testEndpoint(r.In), ExpressibleSet(r.In)...)
	}
	if avail != nil {
		m.WithAvailability(avail)
	}
	return m
}

// testEndpoint 是 implementedMatrix 的测试专用门，与真实门的路径刻意不同——
// 测试敲的必须是这扇门，而不是碰巧撞上真实门。
func testEndpoint(p Protocol) Endpoint { return Endpoint("/test/" + string(p)) }

// testInbound 与 implementedMatrix 的测试专用门配套。
func testInbound(p Protocol) Inbound { return Inbound{Protocol: p, Endpoint: testEndpoint(p)} }

// TestImplementedRoutesAreExplicit 要求已转正的路径逐条登记在这里。
//
// 转正是件需要有人明确点头的事，不该悄悄发生。任何一条路径被标记为已实现却
// 没进这份名单，这里就会失败——包括「顺手加个 Redeem 让测试过」
// 那种做法。想转正就得同时改代码、写 fixture、改这份名单，三处都动过一遍，
// 就很难是无意的。
func TestImplementedRoutesAreExplicit(t *testing.T) {
	// 已转正：OpenAI 族两条同源直通 + DashScope Native 文本生成同源直通。
	// 其余仍为 PLANNED，dashscope.compatible / anthropic 等排在其后。
	want := map[string]bool{
		string(ProtoOpenAIResponses) + " -> " + string(ProviderOpenAICompat):    true,
		string(ProtoOpenAIChat) + " -> " + string(ProviderOpenAICompat):         true,
		string(ProtoDashScopeNative) + " -> " + string(ProviderDashScopeNative): true,
	}

	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, r := range m.Routes() {
		if r.Implemented() {
			got[string(r.In)+" -> "+string(r.Out)] = true
		}
	}

	for k := range want {
		if !got[k] {
			t.Errorf("路径 %s 应已转正，实际仍是 PLANNED", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("路径 %s 被标记为已实现，但不在名单里——"+
				"转正请同步更新本测试与 fixture", k)
		}
	}
}

// TestRedeemedCapabilitiesAreExplicit 把「哪扇门投放了哪些能力」也变成需要有人点头的事。
//
// 与 TestImplementedRoutesAreExplicit 同理，只是粒度从路径细到「路径 @ 端点」：
// 路径转正只说明这条路开始通车，说明不了每扇门都通。悄悄给某扇门多兑现一项能力，
// 等于宣称一个还没写的实现可用。
func TestRedeemedCapabilitiesAreExplicit(t *testing.T) {
	// OpenAI 两条同源直通各一扇门，字节级转发，可表达的全部兑现；
	// Native 本期只投放了文本生成那扇门。
	want := map[string][]canonical.Capability{
		string(ProtoOpenAIResponses) + " -> " + string(ProviderOpenAICompat) +
			" @ " + string(EndpointOpenAIResponses): ExpressibleSet(ProtoOpenAIResponses),
		string(ProtoOpenAIChat) + " -> " + string(ProviderOpenAICompat) +
			" @ " + string(EndpointOpenAIChat): ExpressibleSet(ProtoOpenAIChat),
		string(ProtoDashScopeNative) + " -> " + string(ProviderDashScopeNative) +
			" @ " + string(EndpointDashScopeTextGeneration): {
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapReasoning,
			canonical.CapWebSearch,
		},
	}

	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, r := range m.Routes() {
		routeKey := string(r.In) + " -> " + string(r.Out)
		for _, ep := range r.Endpoints() {
			key := routeKey + " @ " + string(ep)
			seen[key] = true
			caps, listed := want[key]
			if !listed {
				t.Errorf("路径 %s 开了门 %s，但不在名单里——投放请同步更新本测试与 fixture",
					routeKey, ep)
				continue
			}

			redeemed := map[canonical.Capability]bool{}
			for _, c := range caps {
				redeemed[c] = true
				if !r.Redeems(ep, c) {
					t.Errorf("%s 应已投放 %q，实际未兑现", key, c)
				}
			}
			for _, c := range canonical.AllCapabilities() {
				if !redeemed[c] && r.Redeems(ep, c) {
					t.Errorf("%s 多兑现了 %q，名单里没有它", key, c)
				}
			}
		}
	}

	// 名单声称的门必须真的开着，防止名单单边漂移。
	for key := range want {
		if !seen[key] {
			t.Errorf("名单声称 %s 已投放，实际没有这扇门", key)
		}
	}
}

// TestDeriveDoesNotInheritRedemption 防的是「派生路径顺手继承了基准路径的投放」。
//
// 兑现集合说的是「这条路径的这个端点已经写好了」，而实现是一条路径一条路径写的。
// 继承它，等于让一条还没动工的派生路径宣称自己可用。
func TestDeriveDoesNotInheritRedemption(t *testing.T) {
	base := NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
		Pass(ExpressibleSet(ProtoOpenAIChat)...).
		Redeem(EndpointOpenAIChat, ExpressibleSet(ProtoOpenAIChat)...)

	derived := base.Derive(ProtoOpenAIResponses, ProviderOpenAICompat)
	if derived.Implemented() {
		t.Error("派生路径不该继承兑现集合——实现是逐条写的，不是继承来的")
	}
	if derived.Redeems(EndpointOpenAIChat, canonical.CapTextGeneration) {
		t.Error("派生路径不该继承单项能力的兑现状态")
	}
}

// TestPlannedRouteIsRejectedAtRuntime 固化「未实现的路径必须明确报错」。
//
// 501 而不是 422：前者告诉客户端「等」，后者告诉客户端「改请求」。
// 混成一个错误码，用户会去改一个本来就对的请求。
func TestPlannedRouteIsRejectedAtRuntime(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	// 用一条仍是 PLANNED 的路径。openai.compat 那条已在 M1 转正。
	_, err = m.Check(Inbound{Protocol: ProtoOpenAIResponses, Endpoint: EndpointOpenAIResponses},
		ProviderDashScopeCompatible,
		[]canonical.Capability{canonical.CapTextGeneration})
	if err == nil {
		t.Fatal("未实现的路径必须报错，不得静默放行到一个空壳")
	}

	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassNotImplemented {
		t.Errorf("错误分类应为 not_implemented，实际为 %q", cerr.Class)
	}
	if cerr.HTTPStatus() != 501 {
		t.Errorf("应映射到 501，实际为 %d", cerr.HTTPStatus())
	}
}

// TestUnredeemedCapabilityIsRejectedAtRuntime 防的是「路径转正连带把整条协议
// 的能力都算成可用」。
//
// DashScope Native 一个协议对应多个上游端点，本期只投放了文本生成那一个。
// 视觉输入的**设计处置**仍是 PASSTHROUGH——那是这条路最终的样子，不该改；
// 但它此刻没有落地，放行只会让请求打到一个不存在的实现上，客户端拿到的是
// 一个看不懂的 5xx，而真相是「这个端点还没投放」。
func TestUnredeemedCapabilityIsRejectedAtRuntime(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	// 设计处置不变：这是一条同源直通路径，视觉输入本就该原样转发。
	rule, ok := m.Lookup(ProtoDashScopeNative, ProviderDashScopeNative, canonical.CapVisionInput)
	if !ok || rule.Disposition != Passthrough {
		t.Fatalf("vision_input 的设计处置应为 PASSTHROUGH，实际 %v", rule.Disposition)
	}

	// 但当前未投放，运行时必须挡下来。
	_, err = m.Check(Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeTextGeneration},
		ProviderDashScopeNative,
		[]canonical.Capability{canonical.CapTextGeneration, canonical.CapVisionInput})
	if err == nil {
		t.Fatal("未投放的能力必须报错，不得放行到一个尚不存在的实现上")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassNotImplemented {
		t.Errorf("错误分类应为 not_implemented（等实现），而非 unsupported（改请求），实际 %q",
			cerr.Class)
	}
	if cerr.HTTPStatus() != 501 {
		t.Errorf("应映射到 501，实际为 %d", cerr.HTTPStatus())
	}

	// 已投放的五项必须照常放行，否则这道闸门就把整条路径也一起关了。
	for _, c := range []canonical.Capability{
		canonical.CapTextGeneration,
		canonical.CapStreaming,
		canonical.CapToolCalling,
		canonical.CapReasoning,
		canonical.CapWebSearch,
	} {
		if _, err := m.Check(
			Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeTextGeneration},
			ProviderDashScopeNative,
			[]canonical.Capability{c}); err != nil {
			t.Errorf("已投放的能力 %q 不该被拦下: %v", c, err)
		}
	}
}

// TestImplementedRoutesHaveFixtures 是 ADR-0001 的转正门槛。
//
// 一条路径标记为已实现，就必须有覆盖它的 fixture；每个 DEGRADE / EMULATE
// 格子还要有专门的 fixture——有损转换正是 bug 藏身之处。
func TestImplementedRoutesHaveFixtures(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range m.Routes() {
		if !r.Implemented() {
			continue
		}
		if err := checkRouteFixtures(r, "../.."); err != nil {
			t.Errorf("路径 %s -> %s 已转正但缺少证据: %v", r.In, r.Out, err)
		}
	}
}

// TestFixtureGateActuallyBites 验证上面那道门槛不是摆设。
//
// 没有这条测试，TestImplementedRoutesHaveFixtures 在 M0 阶段（零条已实现）
// 会空转通过——而「空转通过的检查」正是这轮排查要消灭的东西。
func TestFixtureGateActuallyBites(t *testing.T) {
	// 故意挑一条**没有** fixture 目录的路径来证明门槛会咬人。
	// 不能挑已转正的路径——它们有 fixture，门槛放行是正确的，测不出「咬」。
	fake := NewRoute(ProtoOpenAIChat, ProviderAnthropicMessages).
		Pass(ExpressibleSet(ProtoOpenAIChat)...).
		Redeem(EndpointOpenAIChat, ExpressibleSet(ProtoOpenAIChat)...)
	built, err := fake.Build()
	if err != nil {
		t.Fatal(err)
	}

	if err := checkRouteFixtures(built, "../.."); err == nil {
		t.Fatal("一条没有任何 fixture 的路径被标记为已实现，门槛却放行了")
	}
}

// checkRouteFixtures 校验一条已实现路径的 fixture 证据是否齐备。
func checkRouteFixtures(r *Route, repoRoot string) error {
	dir := filepath.Join(repoRoot, FixtureDir(r.In, r.Out))

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("缺少 fixture 目录 %s", FixtureDir(r.In, r.Out))
	}

	present := map[string]bool{}
	var count int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		count++
		present[strings.TrimSuffix(name, ".json")] = true
	}
	if count == 0 {
		return fmt.Errorf("fixture 目录 %s 中没有任何用例", FixtureDir(r.In, r.Out))
	}

	// 有损格子要单独举证。文件名即能力名，例如 structured_output.json。
	var missing []string
	for c, rule := range r.rules {
		if rule.Disposition != Degrade && rule.Disposition != Emulate {
			continue
		}
		if !present[string(c)] {
			missing = append(missing, string(c))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("以下有损能力缺少专门的 fixture: %s", strings.Join(missing, ", "))
	}
	return nil
}

// TestExpressibilityDeclarationsAreComplete 保证每个入站协议都对全部能力表态：
// 要么能表达，要么在别处有归宿，要么说明为什么根本不可能。
func TestExpressibilityDeclarationsAreComplete(t *testing.T) {
	if len(expressible) == 0 {
		t.Fatal("没有任何入站协议声明了可表达性")
	}
	for p, e := range expressible {
		if err := e.validate(); err != nil {
			t.Errorf("协议 %s 的可表达性声明有问题: %v", p, err)
		}
	}
}

// TestNoCapabilityIsHomeless 是补上 DashScope WebSocket 缺口的那道闸门。
//
// 「该能力请去 X 协议」的转介，目标必须真的注册过路径。少了这道检查，
// dashscope.native 可以理直气壮地说「realtime 请走 dashscope.realtime」，
// 而那个协议一条路径都没有——用户按提示改了协议，照样撞墙。
func TestNoCapabilityIsHomeless(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.checkElsewhereTargets(); err != nil {
		t.Error(err)
	}
}

// TestSpeechHasAnInboundPath 固化一个具体的历史缺口：
// Paraformer 实时 ASR 与 CosyVoice 流式 TTS 曾经从任何入口都到不了——
// dashscope.ws.inference 这个 Provider 没有任何入站路径指向它。
func TestSpeechHasAnInboundPath(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, r := range m.Routes() {
		if r.Out == ProviderDashScopeWSInference {
			found = true
		}
	}
	if !found {
		t.Fatal("dashscope.ws.inference 没有任何入站路径，Paraformer / CosyVoice 无法访问")
	}

	for _, c := range []canonical.Capability{
		canonical.CapSpeechSynthesis, canonical.CapSpeechRecognition,
	} {
		rule, ok := m.Lookup(ProtoDashScopeInference, ProviderDashScopeWSInference, c)
		if !ok || rule.Disposition != Passthrough {
			t.Errorf("A 类同源直通应当无损承载 %q，实际 %v", c, rule.Disposition)
		}
	}
}

// TestIncompleteRouteFailsBuild 验证完整性检查真的会失败，而不是形同虚设。
//
// 没有这个测试，TestPhase1IsComplete 通过可能只是因为检查逻辑本身坏了。
func TestIncompleteRouteFailsBuild(t *testing.T) {
	_, err := NewRoute(ProtoOpenAIChat, ProviderAnthropicMessages).
		Pass(canonical.CapTextGeneration).
		Build()
	if err == nil {
		t.Fatal("只声明一项能力的路径应当 Build 失败")
	}
	if !strings.Contains(err.Error(), "undeclared capabilities") {
		t.Errorf("错误信息应指出漏掉了哪些能力，实际为: %v", err)
	}
	// 漏掉的必须逐项列出来，而且只列这个协议表达得出来的那些。
	if !strings.Contains(err.Error(), string(canonical.CapVisionInput)) {
		t.Errorf("错误信息应逐项列出漏掉的能力，实际为: %v", err)
	}
	// prompt_cache 是 openai.chat 表达不出来的，不该出现在「漏掉」名单里——
	// 让路径为客户端根本发不出来的字段辩解，是旧模型的毛病。
	if strings.Contains(err.Error(), string(canonical.CapPromptCache)) {
		t.Errorf("不可表达的能力不该被要求声明，实际为: %v", err)
	}
}

// TestDeclaringInexpressibleCapabilityFails 从反方向固化同一条约束。
// 为客户端表达不出来的能力写声明，说明作者对协议的理解有偏差。
func TestDeclaringInexpressibleCapabilityFails(t *testing.T) {
	r := NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
		Pass(ExpressibleSet(ProtoOpenAIChat)...).
		Pass(canonical.CapRerank) // openai.chat 没有 rerank 端点

	_, err := r.Build()
	if err == nil {
		t.Fatal("为不可表达的能力写声明应当 Build 失败")
	}
	if !strings.Contains(err.Error(), "not expressible") {
		t.Errorf("错误信息应指出该能力不可表达，实际为: %v", err)
	}
}

func TestRejectWithoutNoteFailsBuild(t *testing.T) {
	r := NewRoute(ProtoOpenAIChat, ProviderAnthropicMessages)
	for _, c := range canonical.AllCapabilities() {
		r = r.Reject("", c)
	}
	_, err := r.Build()
	if err == nil {
		t.Fatal("没有说明理由的 REJECT 应当 Build 失败")
	}
	if !strings.Contains(err.Error(), "carries no note") {
		t.Errorf("错误信息应指出缺少说明，实际为: %v", err)
	}
}

func TestDuplicateDeclarationFailsBuild(t *testing.T) {
	r := NewRoute(ProtoOpenAIChat, ProviderAnthropicMessages)
	for _, c := range canonical.AllCapabilities() {
		r = r.Pass(c)
	}
	_, err := r.Pass(canonical.CapTextGeneration).Build()
	if err == nil {
		t.Fatal("同一项能力声明两次应当 Build 失败")
	}
	if !strings.Contains(err.Error(), "declared twice") {
		t.Errorf("错误信息应指出重复声明，实际为: %v", err)
	}
}

func TestCheckRejectsUnsupportedCapability(t *testing.T) {
	m := implementedMatrix(t, nil)
	var err error

	// Anthropic 不接受音频输入，带音频的请求必须被拒绝而不是静默丢掉音频。
	_, err = m.Check(testInbound(ProtoOpenAIChat), ProviderAnthropicMessages,
		[]canonical.Capability{canonical.CapTextGeneration, canonical.CapAudioInput})
	if err == nil {
		t.Fatal("带音频输入的 Anthropic 请求应当被拒绝")
	}

	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassUnsupported {
		t.Errorf("错误分类应为 %q，实际为 %q", canonical.ClassUnsupported, cerr.Class)
	}
	if cerr.HTTPStatus() != 422 {
		t.Errorf("不支持的能力应映射到 422，实际为 %d", cerr.HTTPStatus())
	}
	if !strings.Contains(cerr.Message, "音频") {
		t.Errorf("错误消息应包含拒绝理由，实际为: %s", cerr.Message)
	}
}

// TestCheckFlagsDecoderBugOnInexpressibleCapability 固化一处刻意的严格。
//
// 入站协议表达不出来的能力，却出现在了请求里——这不是客户端的问题，是网关
// 自己的解码器把某个字段解成了它不该解成的东西。归为 internal 而不是
// unsupported，是为了让这类 bug 出现在网关的错误率里，而不是被记成
// 「客户端发了不支持的请求」。
func TestCheckFlagsDecoderBugOnInexpressibleCapability(t *testing.T) {
	m := implementedMatrix(t, nil)
	var err error

	_, err = m.Check(testInbound(ProtoOpenAIChat), ProviderOpenAICompat,
		[]canonical.Capability{canonical.CapReasoningSignature})
	if err == nil {
		t.Fatal("openai.chat 不可能产生推理签名，出现即应报错")
	}

	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassInternal {
		t.Errorf("应归为 internal（网关解码器 bug），实际为 %q", cerr.Class)
	}
}

func TestCheckReportsDegradation(t *testing.T) {
	m := implementedMatrix(t, nil)

	// Anthropic 没有 strict json_schema 校验，结构化输出降级为提示词约束。
	v, err := m.Check(testInbound(ProtoOpenAIChat), ProviderAnthropicMessages,
		[]canonical.Capability{canonical.CapTextGeneration, canonical.CapStructuredOutput})
	if err != nil {
		t.Fatalf("结构化输出应当被降级而非拒绝: %v", err)
	}
	if len(v.Degraded) != 1 {
		t.Fatalf("应报告 1 项降级，实际 %d 项", len(v.Degraded))
	}
	if v.Degraded[0].Capability != canonical.CapStructuredOutput {
		t.Errorf("降级项应为 structured_output，实际为 %q", v.Degraded[0].Capability)
	}
	if !strings.Contains(v.Header(), "structured_output=") {
		t.Errorf("降级响应头应包含能力名，实际为: %s", v.Header())
	}
}

// TestCheckReportsEmulation 固化「网关垫出来的能力也要告知客户端」。
//
// 与降级相反：能力是完整的，但它由网关侧的 ConversationStore 提供，
// 因而带着网关的可用性边界（内存态、重启丢失、多副本不共享）。
// 客户端有权在做重试决策前知道这件事。
func TestCheckReportsEmulation(t *testing.T) {
	// 模拟能力默认关闭，这条测试要验的是开启后的行为。
	m := implementedMatrix(t, Availability{FeatureConversationStore: true})

	v, err := m.Check(testInbound(ProtoOpenAIResponses), ProviderDashScopeNative,
		[]canonical.Capability{canonical.CapTextGeneration, canonical.CapStatefulConversation})
	if err != nil {
		t.Fatalf("服务端会话应由网关模拟提供而非拒绝: %v", err)
	}
	if len(v.Emulated) != 1 || v.Emulated[0].Capability != canonical.CapStatefulConversation {
		t.Fatalf("应报告 1 项由网关模拟的能力，实际 %+v", v.Emulated)
	}
	if !strings.Contains(v.Emulated[0].Note, "重启") {
		t.Errorf("模拟能力的说明必须写明可用性边界，实际为: %s", v.Emulated[0].Note)
	}
}

// TestCheckFailsClosedOnUnknownRoute 固化「未注册按拒绝处理」这条约束。
func TestCheckFailsClosedOnUnknownRoute(t *testing.T) {
	m := implementedMatrix(t, nil)
	var err error

	_, err = m.Check(Inbound{Protocol: protoNeverRegistered, Endpoint: Endpoint("/test/never")},
		ProviderAnthropicMessages,
		[]canonical.Capability{canonical.CapTextGeneration})
	if err == nil {
		t.Fatal("未注册的路径必须失败，不得静默放行")
	}
	if !strings.Contains(err.Error(), "未注册的转换路径") {
		t.Errorf("错误信息应说明路径未注册，实际为: %v", err)
	}
}

// TestRealtimeFastPathIsHomogeneous 固化 M7 的核心结论：
// OpenAI Realtime → DashScope Realtime 是快通道，不是需要重建事件模型的异构转换。
func TestRealtimeFastPathIsHomogeneous(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	r, ok := m.Route(ProtoOpenAIRealtime, ProviderDashScopeWSRealtime)
	if !ok {
		t.Fatal("OpenAI Realtime -> DashScope Realtime 路径未注册")
	}
	if !r.Homogeneous {
		t.Error("该路径应标记为同源快通道：两者事件模型基本一致")
	}

	// 音频输入是唯一需要真实转码的一项（24 kHz -> 16 kHz）。
	rule, ok := m.Lookup(ProtoOpenAIRealtime, ProviderDashScopeWSRealtime, canonical.CapAudioInput)
	if !ok {
		t.Fatal("audio_input 未声明")
	}
	if rule.Disposition != Degrade {
		t.Errorf("audio_input 应为 DEGRADE（需重采样），实际为 %s", rule.Disposition)
	}
	if !strings.Contains(rule.Note, "16 kHz") {
		t.Errorf("说明应指出目标采样率，实际为: %s", rule.Note)
	}
}

// TestDegradationMatrixDocIsCurrent 防止文档与代码漂移。
// 用 `go test ./internal/degrade -update-matrix` 重新生成。
func TestDegradationMatrixDocIsCurrent(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	want := m.Markdown()

	if *updateDoc {
		if err := os.MkdirAll(filepath.Dir(docPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(docPath, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("已重新生成 %s", docPath)
		return
	}

	got, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("读取 %s 失败（首次生成请运行 make matrix-update）: %v", docPath, err)
	}
	if string(got) != want {
		t.Errorf("docs/degradation-matrix.md 与代码不同步，请运行 make matrix-update")
	}
}
