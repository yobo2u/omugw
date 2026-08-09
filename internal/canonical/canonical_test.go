package canonical

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestUsageFidelityMustBeExplicit 固化「计费口径必须显式声明」。
//
// 零值是非法的，而不是默默当成 authoritative——后者会让流式中断时缺失的 usage
// 被当作「输入输出都是 0 token」计入账单。
func TestUsageFidelityMustBeExplicit(t *testing.T) {
	var u Usage
	if err := u.Validate(); err == nil {
		t.Fatal("零值 Usage 应当校验失败")
	}
	if !strings.Contains(u.Validate().Error(), "explicitly") {
		t.Errorf("错误信息应说明必须显式声明，实际: %v", u.Validate())
	}

	for _, f := range []Fidelity{FidelityAuthoritative, FidelityEstimated, FidelityUnavailable} {
		if err := (Usage{Fidelity: f}).Validate(); err != nil {
			t.Errorf("%q 应当合法: %v", f, err)
		}
	}
}

func TestOnlyAuthoritativeUsageIsBillable(t *testing.T) {
	if !FidelityAuthoritative.Billable() {
		t.Error("authoritative 应可计费")
	}
	for _, f := range []Fidelity{FidelityEstimated, FidelityUnavailable, FidelityUnknown} {
		if f.Billable() {
			t.Errorf("%q 不得作为计费依据", f)
		}
	}
}

// TestTotalTokensExcludesCache 记录一处有意的设计：缓存读写不计入 TotalTokens。
// 它们与普通 input token 计价不同，加总会掩盖差异。
func TestTotalTokensExcludesCache(t *testing.T) {
	u := Usage{
		Fidelity:              FidelityAuthoritative,
		InputTokens:           100,
		OutputTokens:          50,
		CacheReadInputTokens:  900,
		CacheWriteInputTokens: 200,
	}
	if got := u.TotalTokens(); got != 150 {
		t.Errorf("TotalTokens = %d, 期望 150（不含缓存读写）", got)
	}
}

// TestAccumulatorReassemblesSplitToolArgs 是这个包最关键的一条测试。
//
// 三家的工具参数分片边界完全不同，且分片可能切在 JSON 语法单元中间。
// 跨协议转换必须缓冲到闭合才能重新分片——这正是异构路径牺牲 TTFT 的原因。
func TestAccumulatorReassemblesSplitToolArgs(t *testing.T) {
	a := NewAccumulator()

	events := []Event{
		{Type: EventContentStart, Index: 0, ContentKind: PartToolCall,
			ToolCall: &ToolCall{ID: "call_1", Name: "get_weather"}},
		// 故意切在字符串字面量中间——上游真的会这么发。
		{Type: EventToolCallArgsDelta, Index: 0, ArgsDelta: `{"ci`},
		{Type: EventToolCallArgsDelta, Index: 0, ArgsDelta: `ty":"Shang`},
		{Type: EventToolCallArgsDelta, Index: 0, ArgsDelta: `hai","unit":"c"}`},
		{Type: EventContentEnd, Index: 0},
		{Type: EventMessageEnd, StopReason: StopToolUse},
	}
	for i, ev := range events {
		if err := a.Add(ev); err != nil {
			t.Fatalf("事件 %d: %v", i, err)
		}
	}

	if len(a.Parts) != 1 {
		t.Fatalf("应聚合成 1 个内容块，实际 %d", len(a.Parts))
	}
	tc := a.Parts[0].ToolCall
	if tc.ID != "call_1" || tc.Name != "get_weather" {
		t.Errorf("工具标识丢失: %+v", tc)
	}

	var args map[string]string
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("重组后的参数不是合法 JSON: %v", err)
	}
	if args["city"] != "Shanghai" || args["unit"] != "c" {
		t.Errorf("参数重组有误: %v", args)
	}
	if a.StopReason != StopToolUse {
		t.Errorf("StopReason = %q, 期望 tool_use", a.StopReason)
	}
}

// TestAccumulatorRejectsUnclosedToolArgs 保证半截 JSON 不会被当成有效参数
// 转发给上游——那会让上游返回一个难以定位的解析错误。
func TestAccumulatorRejectsUnclosedToolArgs(t *testing.T) {
	a := NewAccumulator()
	_ = a.Add(Event{Type: EventContentStart, Index: 0, ContentKind: PartToolCall,
		ToolCall: &ToolCall{ID: "c", Name: "f"}})
	_ = a.Add(Event{Type: EventToolCallArgsDelta, Index: 0, ArgsDelta: `{"a":`})

	err := a.Add(Event{Type: EventContentEnd, Index: 0})
	if err == nil {
		t.Fatal("未闭合的工具参数应当报错")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("错误信息应指出 JSON 不合法，实际: %v", err)
	}
}

// TestAccumulatorAssemblesThinkingSignature 覆盖 Anthropic 的 signature_delta。
// 签名必须完整拼接后原样回传，少一个字符整个多轮会话就会被上游拒绝。
func TestAccumulatorAssemblesThinkingSignature(t *testing.T) {
	a := NewAccumulator()
	events := []Event{
		{Type: EventContentStart, Index: 0, ContentKind: PartThinking},
		{Type: EventThinkingDelta, Index: 0, Text: "先看题目，"},
		{Type: EventThinkingDelta, Index: 0, Text: "再算。"},
		{Type: EventSignatureDelta, Index: 0, Text: "sIgNa"},
		{Type: EventSignatureDelta, Index: 0, Text: "tUrE=="},
		{Type: EventContentEnd, Index: 0},
	}
	for i, ev := range events {
		if err := a.Add(ev); err != nil {
			t.Fatalf("事件 %d: %v", i, err)
		}
	}

	th := a.Parts[0].Thinking
	if th.Text != "先看题目，再算。" {
		t.Errorf("推理文本 = %q", th.Text)
	}
	if th.Signature != "sIgNatUrE==" {
		t.Errorf("签名拼接有误 = %q", th.Signature)
	}
}

// TestAccumulatorInterruptedStreamReportsUnavailableUsage 固化原则 2.4 / 2.5：
// 流中途失败时上游不会再送 usage，任何非零数字都是编造的。
func TestAccumulatorInterruptedStreamReportsUnavailableUsage(t *testing.T) {
	a := NewAccumulator()
	_ = a.Add(Event{Type: EventContentStart, Index: 0, ContentKind: PartText})
	_ = a.Add(Event{Type: EventTextDelta, Index: 0, Text: "部分内容"})
	_ = a.Add(Event{Type: EventUsage, Usage: &Usage{
		Fidelity: FidelityAuthoritative, InputTokens: 10, OutputTokens: 5,
	}})
	_ = a.Add(Event{Type: EventError, Err: Newf(ClassUpstreamUnavailable, "connection reset")})

	if a.StopReason != StopInterrupted {
		t.Errorf("StopReason = %q, 期望 interrupted", a.StopReason)
	}
	if a.Usage.Fidelity != FidelityUnavailable {
		t.Errorf("中断后 usage 可信等级 = %q, 期望 unavailable", a.Usage.Fidelity)
	}
	if a.Usage.TotalTokens() != 0 {
		t.Errorf("中断后不得保留半截的 token 计数，实际 %d", a.Usage.TotalTokens())
	}
}

// TestAccumulatorNewIsUnavailableByDefault 保证「没收到 usage 事件」的默认结果
// 是「不可知」而不是「零用量」。
func TestAccumulatorNewIsUnavailableByDefault(t *testing.T) {
	if f := NewAccumulator().Usage.Fidelity; f != FidelityUnavailable {
		t.Errorf("初始 usage 可信等级 = %q, 期望 unavailable", f)
	}
}

func TestAccumulatorRejectsDeltaForUnstartedBlock(t *testing.T) {
	a := NewAccumulator()
	if err := a.Add(Event{Type: EventTextDelta, Index: 3, Text: "x"}); err == nil {
		t.Fatal("未开始的块收到 delta 应当报错")
	}
}

func TestPartValidateRejectsKindMismatch(t *testing.T) {
	bad := Part{Kind: PartText, ToolCall: &ToolCall{ID: "x"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("kind=text 却带着 tool_call 负载，应当校验失败")
	}
}

// TestMediaRequiresExactlyOneSource 固化原则 2.6 的前提：
// URL / 内联字节 / 文件引用三种承载形态互斥。
func TestMediaRequiresExactlyOneSource(t *testing.T) {
	cases := map[string]Media{
		"三者皆空":      {Kind: MediaImage},
		"URL 与内联并存": {Kind: MediaImage, URL: "https://x/a.png", Data: []byte{1}},
		"内联与文件引用并存": {Kind: MediaImage, Data: []byte{1}, FileRef: &FileRef{Provider: "openai", ID: "f-1"}},
	}
	for name, m := range cases {
		if err := m.Validate(); err == nil {
			t.Errorf("%s: 应当校验失败", name)
		}
	}

	ok := Media{Kind: MediaImage, URL: "https://x/a.png"}
	if err := ok.Validate(); err != nil {
		t.Errorf("单一来源应当合法: %v", err)
	}
}

// TestUsedCapabilitiesDetectsSignature 保证带签名的推理块能被识别出来——
// 降级矩阵靠这个信号在异构路径上拒绝请求。
func TestUsedCapabilitiesDetectsSignature(t *testing.T) {
	r := &Request{
		Model: "m",
		Messages: []Message{{
			Role: RoleAssistant,
			Parts: []Part{
				{Kind: PartThinking, Thinking: &Thinking{Text: "...", Signature: "sig"}},
			},
		}},
	}

	caps := map[Capability]bool{}
	for _, c := range r.UsedCapabilities() {
		caps[c] = true
	}
	if !caps[CapReasoningSignature] {
		t.Error("带签名的推理块应报告 reasoning_signature")
	}
	if !caps[CapReasoning] {
		t.Error("推理块应同时报告 reasoning")
	}
}

func TestUsedCapabilitiesCoversMultimodal(t *testing.T) {
	r := &Request{
		Model:      "m",
		Stream:     true,
		Tools:      []Tool{{Name: "f"}},
		Cache:      &CacheHint{},
		Modalities: []Modality{ModalityAudio},
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{Text("看图"), ImageURL("https://x/a.png", "image/png")},
		}},
	}

	caps := map[Capability]bool{}
	for _, c := range r.UsedCapabilities() {
		caps[c] = true
	}
	for _, want := range []Capability{
		CapTextGeneration, CapStreaming, CapToolCalling,
		CapVisionInput, CapAudioOutput, CapPromptCache,
	} {
		if !caps[want] {
			t.Errorf("应报告能力 %q", want)
		}
	}
}

// TestUsedCapabilitiesIsStable 保证输出顺序稳定——golden file 依赖这一点。
func TestUsedCapabilitiesIsStable(t *testing.T) {
	r := &Request{Model: "m", Stream: true, Tools: []Tool{{Name: "f"}}}
	first := r.UsedCapabilities()
	for i := 0; i < 20; i++ {
		got := r.UsedCapabilities()
		if len(got) != len(first) {
			t.Fatalf("第 %d 次调用长度不同", i)
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("第 %d 次调用顺序不同: %v vs %v", i, got, first)
			}
		}
	}
}

func TestRequestValidateRejectsBadCacheBreakpoint(t *testing.T) {
	r := &Request{
		Model:    "m",
		Messages: []Message{UserText("hi")},
		Cache:    &CacheHint{Breakpoints: []int{5}},
	}
	if err := r.Validate(); err == nil {
		t.Fatal("越界的缓存断点应当校验失败")
	}
}
