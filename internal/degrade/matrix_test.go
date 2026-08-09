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
		pass, degrade, reject := r.Stats()
		total := pass + degrade + reject
		if want := len(canonical.AllCapabilities()); total != want {
			t.Errorf("路径 %s -> %s 覆盖 %d 项能力，应为 %d 项", r.In, r.Out, total, want)
		}
		t.Logf("%-22s -> %-26s pass=%2d degrade=%2d reject=%2d", r.In, r.Out, pass, degrade, reject)
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
	if !strings.Contains(err.Error(), string(canonical.CapPromptCache)) {
		t.Errorf("错误信息应逐项列出漏掉的能力，实际为: %v", err)
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

	// thinking 签名跨协议后失效，必须拒绝而不是静默丢弃。
	_, err = m.Check(ProtoOpenAIChat, ProviderAnthropicMessages,
		[]canonical.Capability{canonical.CapTextGeneration, canonical.CapReasoningSignature})
	if err == nil {
		t.Fatal("携带 thinking 签名的异构请求应当被拒绝")
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
	if !strings.Contains(cerr.Message, "签名") {
		t.Errorf("错误消息应包含拒绝理由，实际为: %s", cerr.Message)
	}
}

func TestCheckReportsDegradation(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}

	v, err := m.Check(ProtoOpenAIChat, ProviderAnthropicMessages,
		[]canonical.Capability{canonical.CapTextGeneration, canonical.CapPromptCache})
	if err != nil {
		t.Fatalf("prompt cache 应当被降级而非拒绝: %v", err)
	}
	if len(v.Degraded) != 1 {
		t.Fatalf("应报告 1 项降级，实际 %d 项", len(v.Degraded))
	}
	if v.Degraded[0].Capability != canonical.CapPromptCache {
		t.Errorf("降级项应为 prompt_cache，实际为 %q", v.Degraded[0].Capability)
	}
	if !strings.Contains(v.Header(), "prompt_cache=") {
		t.Errorf("降级响应头应包含能力名，实际为: %s", v.Header())
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
