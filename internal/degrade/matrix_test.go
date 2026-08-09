package degrade

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

var updateDoc = flag.Bool("update-matrix", false, "重新生成 docs/degradation-matrix.md")

const docPath = "../../docs/degradation-matrix.md"

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
		p := r.Preservation()

		// 每条路径必须为入站协议**表达得出来**的每一项能力表态。
		// 表达不出来的那些由 Expressibility 自动补成 N/A，不该由路径负责。
		declared := p.Passthrough + p.Emulate + p.Degrade + p.Reject
		if want := len(ExpressibleSet(r.In)); declared != want {
			t.Errorf("路径 %s -> %s 为 %d 项可表达能力表态，应为 %d 项",
				r.In, r.Out, declared, want)
		}
		// N/A 与已表态的加起来必须是全集，否则有格子凭空消失。
		if total := declared + p.NotApplicable; total != len(canonical.AllCapabilities()) {
			t.Errorf("路径 %s -> %s 的格子总数 %d，应为 %d",
				r.In, r.Out, total, len(canonical.AllCapabilities()))
		}

		t.Logf("%-22s -> %-24s pass=%2d emulate=%d degrade=%d reject=%d n/a=%2d  保留度=%.3f",
			r.In, r.Out, p.Passthrough, p.Emulate, p.Degrade, p.Reject, p.NotApplicable, p.Score())
	}
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
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	// Anthropic 不接受音频输入，带音频的请求必须被拒绝而不是静默丢掉音频。
	_, err = m.Check(ProtoOpenAIChat, ProviderAnthropicMessages,
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
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.Check(ProtoOpenAIChat, ProviderOpenAICompat,
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
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	// Anthropic 没有 strict json_schema 校验，结构化输出降级为提示词约束。
	v, err := m.Check(ProtoOpenAIChat, ProviderAnthropicMessages,
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
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	v, err := m.Check(ProtoOpenAIResponses, ProviderDashScopeNative,
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
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.Check(ProtoOpenAIImages, ProviderAnthropicMessages,
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
