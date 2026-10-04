package degrade

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// 独立列出 GA 字段对应的集合，防止用实现反造预期而漏掉新增表达性。
func TestOpenAIRealtimeGACapabilities(t *testing.T) {
	want := []canonical.Capability{
		"text_generation",
		"streaming",
		"tool_calling",
		"parallel_tool_calls",
		"reasoning",
		"vision_input",
		"image_detail",
		"audio_input",
		"audio_output",
		"speech_synthesis",
		"speech_recognition",
		"stateful_conversation",
		"realtime_session",
		"realtime_server_vad",
		"realtime_interrupt_turns",
	}
	if got := ExpressibleSet(ProtoOpenAIRealtime); !slices.Equal(got, want) {
		t.Errorf("GA 可表达集合 = %v，期望独立列出的 15 项 %v", got, want)
	}
	e := expressible[ProtoOpenAIRealtime]
	if err := e.validate(); err != nil {
		t.Fatalf("三桶必须完整且互斥: %v", err)
	}
	if got := e.Elsewhere[canonical.CapRealtimeImageInput]; got != ProtoDashScopeRealtime {
		t.Errorf("图像 item 不应被当作专用图像 buffer，转介 = %q", got)
	}
}

// 裁决与对外说明一起固定语义损失，防止把未文档化写成模型无能力或上游实测拒绝。
func TestRealtimeGACrossProtocolRules(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		in    Protocol
		out   Provider
		cap   canonical.Capability
		want  Disposition
		notes []string
	}{
		{ProtoOpenAIRealtime, ProviderDashScopeWSRealtime, canonical.CapVisionInput, Reject,
			[]string{"须先追加音频", "共同提交", "独立图像消息", "提交边界"}},
		{ProtoOpenAIRealtime, ProviderDashScopeWSRealtime, canonical.CapImageDetail, Degrade,
			[]string{"auto/low/high", "丢弃", "会话级视频聚合不等价"}},
		{ProtoOpenAIRealtime, ProviderDashScopeWSRealtime, canonical.CapReasoning, Reject,
			[]string{"当前公开", "WebSocket", "没有已文档化", "reasoning.effort", "思考开关"}},
		{ProtoDashScopeRealtime, ProviderOpenAIRealtime, canonical.CapParallelToolCalls, Degrade,
			[]string{"没有通用的并行调用策略等价保证", "parallel_tool_calls 仅适用于 reasoning Realtime 模型", "调用调度"}},
		{ProtoDashScopeRealtime, ProviderOpenAIRealtime, canonical.CapRealtimeImageInput, Reject,
			[]string{"支持 input_image 消息", "随音频共同提交", "提交边界", "对话项关联"}},
		{ProtoDashScopeRealtime, ProviderOpenAIRealtime, canonical.CapVisionInput, Reject,
			[]string{"支持 input_image 消息", "随音频共同提交", "提交边界", "对话项关联"}},
	} {
		t.Run(string(tt.in)+"/"+string(tt.cap), func(t *testing.T) {
			rule, ok := m.Lookup(tt.in, tt.out, tt.cap)
			if !ok || rule.Disposition != tt.want {
				t.Errorf("处置 = %s（存在=%t），期望 %s", rule.Disposition, ok, tt.want)
			}
			for _, note := range tt.notes {
				if !strings.Contains(rule.Note, note) {
					t.Errorf("说明缺少 %q: %s", note, rule.Note)
				}
			}
		})
	}
}

// 测试专用门只隔离处置语义，不能把设计 REJECT 与生产仍为 501 混为一谈。
func TestRealtimeGADesignAndAvailability(t *testing.T) {
	production, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	design := implementedMatrix(t, nil)
	for _, tt := range []struct {
		name string
		in   Protocol
		ep   Endpoint
		out  Provider
		caps []canonical.Capability
		want canonical.ErrorClass
	}{
		{"独立图像", ProtoOpenAIRealtime, EndpointOpenAIRealtime, ProviderDashScopeWSRealtime,
			[]canonical.Capability{canonical.CapVisionInput}, canonical.ClassUnsupported},
		{"逐图档位", ProtoOpenAIRealtime, EndpointOpenAIRealtime, ProviderDashScopeWSRealtime,
			[]canonical.Capability{canonical.CapImageDetail}, ""},
		{"档位降级不能抵消图像拒绝", ProtoOpenAIRealtime, EndpointOpenAIRealtime, ProviderDashScopeWSRealtime,
			[]canonical.Capability{canonical.CapImageDetail, canonical.CapVisionInput}, canonical.ClassUnsupported},
		{"显式推理控制", ProtoOpenAIRealtime, EndpointOpenAIRealtime, ProviderDashScopeWSRealtime,
			[]canonical.Capability{canonical.CapReasoning}, canonical.ClassUnsupported},
		{"反向并行调度", ProtoDashScopeRealtime, EndpointDashScopeRealtime, ProviderOpenAIRealtime,
			[]canonical.Capability{canonical.CapParallelToolCalls}, ""},
		{"反向图像缓冲", ProtoDashScopeRealtime, EndpointDashScopeRealtime, ProviderOpenAIRealtime,
			[]canonical.Capability{canonical.CapRealtimeImageInput}, canonical.ClassUnsupported},
		{"反向音画关联", ProtoDashScopeRealtime, EndpointDashScopeRealtime, ProviderOpenAIRealtime,
			[]canonical.Capability{canonical.CapVisionInput}, canonical.ClassUnsupported},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := production.Check(Inbound{Protocol: tt.in, Endpoint: tt.ep}, tt.out, tt.caps)
			var cerr *canonical.Error
			if !errors.As(err, &cerr) || cerr.Class != canonical.ClassNotImplemented || cerr.HTTPStatus() != 501 {
				t.Fatalf("生产仍须返回 not_implemented/501，实际 %v", err)
			}
			v, err := design.Check(testInbound(tt.in), tt.out, tt.caps)
			if tt.want != "" {
				if !errors.As(err, &cerr) || cerr.Class != tt.want || cerr.HTTPStatus() != 422 {
					t.Fatalf("测试门应按设计返回 %s/422，实际 %v", tt.want, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(v.Degraded) != 1 || v.Degraded[0].Capability != tt.caps[0] || !strings.Contains(v.Header(), string(tt.caps[0])+"=") {
				t.Errorf("设计降级必须可见，实际 %+v", v)
			}
		})
	}
}
