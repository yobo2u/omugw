package dashscopenative

import (
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/protocol/openaichat"
)

func mustInputs(t *testing.T, body string) (*openaichat.Projection, *canonical.Request) {
	t.Helper()
	proj, err := openaichat.Project([]byte(body))
	if err != nil {
		t.Fatalf("Project 失败: %v", err)
	}
	decoded, err := openaichat.Decode([]byte(body))
	if err != nil {
		t.Fatalf("Decode 失败: %v", err)
	}
	return proj, &decoded.Request
}

func assertUnsupported(t *testing.T, err error, expectedParam string) {
	t.Helper()
	if err == nil {
		t.Fatalf("预期 %s 触发 422 unsupported，实际放行", expectedParam)
	}
	cerr := canonical.AsError(err)
	if cerr.Class != canonical.ClassUnsupported {
		t.Fatalf("预期 ClassUnsupported，实际 %v", cerr.Class)
	}
	if cerr.Param != expectedParam {
		t.Fatalf("预期 Param %q，实际 %q", expectedParam, cerr.Param)
	}
	if !strings.Contains(cerr.Message, expectedParam) {
		t.Fatalf("预期 Message 包含 %q，实际为 %q", expectedParam, cerr.Message)
	}
}

// TestRejectNoLandingFields 钉死无落点字段显式提交即 422 且点名。
func TestRejectNoLandingFields(t *testing.T) {
	tests := []struct {
		name  string
		field string
		param string
	}{
		{"频率惩罚_常规值", `"frequency_penalty":0.5`, "frequency_penalty"},
		{"频率惩罚_零值", `"frequency_penalty":0`, "frequency_penalty"},
		{"Logit偏置_常规值", `"logit_bias":{"a":1}`, "logit_bias"},
		{"Logit偏置_空对象", `"logit_bias":{}`, "logit_bias"},
		{"服务层级_常规值", `"service_tier":"default"`, "service_tier"},
		{"存储_真值", `"store":true`, "store"},
		{"存储_假值", `"store":false`, "store"},
		{"用户_常规值", `"user":"u-1"`, "user"},
		{"用户_空字符串", `"user":""`, "user"},
		{"元数据_常规值", `"metadata":{"k":"v"}`, "metadata"},
		{"音频_常规值", `"audio":{"voice":"alloy","format":"wav"}`, "audio"},
		{"音频_空值", `"audio":null`, "audio"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proj, canon := mustInputs(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],`+tc.field+`}`)
			err := rejectUnmappable(proj, canon)
			assertUnsupported(t, err, tc.param)
		})
	}
}

// TestRejectMultipleNoLandingFields 钉死多字段同时提交时，按规则表顺序返回首个点名。
func TestRejectMultipleNoLandingFields(t *testing.T) {
	// frequency_penalty 在规则表中先于 user，必须先报 frequency_penalty
	proj, canon := mustInputs(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"user":"u-1","frequency_penalty":0.5}`)
	err := rejectUnmappable(proj, canon)
	assertUnsupported(t, err, "frequency_penalty")
}

// TestRejectNonStreamReasoning 钉死非流式 + 非 none 推理即 422。
func TestRejectNonStreamReasoning(t *testing.T) {
	proj, canon := mustInputs(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	err := rejectUnmappable(proj, canon)
	assertUnsupported(t, err, "reasoning_effort")
}

// TestRejectNGreaterThanOneWithTools 钉死 n>1 + tools 即 422，防止 Native 静默回落 n=1。
func TestRejectNGreaterThanOneWithTools(t *testing.T) {
	proj, canon := mustInputs(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"n":2,"tools":[{"type":"function","function":{"name":"f"}}]}`)
	err := rejectUnmappable(proj, canon)
	assertUnsupported(t, err, "n")
}

// TestRejectStreamNGreaterThanOneWithReasoning 钉死流式 + n>1 在带 reasoning 组合下同样被拒（由通用的 stream+n>1 规则拦截）。
func TestRejectStreamNGreaterThanOneWithReasoning(t *testing.T) {
	proj, canon := mustInputs(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"n":2,"reasoning_effort":"low","stream":true}`)
	err := rejectUnmappable(proj, canon)
	assertUnsupported(t, err, "n")
}

// TestRejectStreamNGreaterThanOne 钉死流式 + n>1 即 422，防止上游静默将 n 压回 1 导致候选丢失。
func TestRejectStreamNGreaterThanOne(t *testing.T) {
	proj, canon := mustInputs(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,"n":2}`)
	err := rejectUnmappable(proj, canon)
	assertUnsupported(t, err, "n")
}

// TestNonePositiveNonStreamReasoning 钉死非流式 + none 必须放行，防止按字段存在与否误判。
func TestNonePositiveNonStreamReasoning(t *testing.T) {
	proj, canon := mustInputs(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`)
	if err := rejectUnmappable(proj, canon); err != nil {
		t.Fatalf("非流式 + reasoning_effort:none 应放行: %v", err)
	}
}

// TestNonePositiveNWithNone 钉死 n>1 + none 必须放行，防止按字段存在与否误判。
func TestNonePositiveNWithNone(t *testing.T) {
	proj, canon := mustInputs(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"n":2,"reasoning_effort":"none"}`)
	if err := rejectUnmappable(proj, canon); err != nil {
		t.Fatalf("n>1 + reasoning_effort:none 应放行: %v", err)
	}
}

// TestPositiveStreamReasoning 钉死流式 + 推理必须放行。
func TestPositiveStreamReasoning(t *testing.T) {
	proj, canon := mustInputs(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,"reasoning_effort":"high"}`)
	if err := rejectUnmappable(proj, canon); err != nil {
		t.Fatalf("流式 + reasoning_effort:high 应放行: %v", err)
	}
}

// TestPositiveN1WithTools 钉死 n=1 + tools 必须放行。
func TestPositiveN1WithTools(t *testing.T) {
	proj, canon := mustInputs(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"n":1,"tools":[{"type":"function","function":{"name":"f"}}]}`)
	if err := rejectUnmappable(proj, canon); err != nil {
		t.Fatalf("n=1 + tools 应放行: %v", err)
	}
}

// TestPositivePlainNGreaterThanOne 钉死单纯 n>1 必须放行。
func TestPositivePlainNGreaterThanOne(t *testing.T) {
	proj, canon := mustInputs(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"n":5}`)
	if err := rejectUnmappable(proj, canon); err != nil {
		t.Fatalf("单纯 n>1 应放行: %v", err)
	}
}

// TestPositiveOmittedNWithTools 钉死未提交 n + tools 必须放行。
func TestPositiveOmittedNWithTools(t *testing.T) {
	proj, canon := mustInputs(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`)
	if err := rejectUnmappable(proj, canon); err != nil {
		t.Fatalf("未提交 n + tools 应放行: %v", err)
	}
}

// TestPositiveStreamReasoningN1 钉死流式 + 推理 + n=1 必须放行。
func TestPositiveStreamReasoningN1(t *testing.T) {
	proj, canon := mustInputs(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,"reasoning_effort":"high","n":1}`)
	if err := rejectUnmappable(proj, canon); err != nil {
		t.Fatalf("流式 + 推理 + n=1 应放行: %v", err)
	}
}
