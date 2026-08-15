package openaichat

import (
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

func mustDecode(t *testing.T, body string) *Decoded {
	t.Helper()
	d, err := Decode([]byte(body))
	if err != nil {
		t.Fatalf("Decode 失败: %v\n请求: %s", err, body)
	}
	return d
}

func hasCap(caps []canonical.Capability, want canonical.Capability) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

func TestDecodeBasicText(t *testing.T) {
	d := mustDecode(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"你好"}]}`)

	if d.Request.Model != "gpt-4o" {
		t.Errorf("Model = %q", d.Request.Model)
	}
	if d.Request.Stream {
		t.Error("未声明 stream 不应为流式")
	}
	if len(d.Request.Messages) != 1 || d.Request.Messages[0].Role != canonical.RoleUser {
		t.Errorf("消息解码异常: %+v", d.Request.Messages)
	}
	caps := d.Capabilities()
	if !hasCap(caps, canonical.CapTextGeneration) {
		t.Errorf("应报告 text_generation: %v", caps)
	}
	if hasCap(caps, canonical.CapStreaming) {
		t.Errorf("非流式不应报告 streaming: %v", caps)
	}
}

func TestDecodeReportsStreaming(t *testing.T) {
	d := mustDecode(t, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !d.Request.Stream {
		t.Fatal("Stream 应为 true")
	}
	if !hasCap(d.Capabilities(), canonical.CapStreaming) {
		t.Errorf("应报告 streaming: %v", d.Capabilities())
	}
}

func TestDecodeReportsToolCalling(t *testing.T) {
	d := mustDecode(t, `{"model":"m","messages":[{"role":"user","content":"天气"}],
	  "tools":[{"type":"function","function":{"name":"get_weather",
	    "parameters":{"type":"object"}}}]}`)
	if len(d.Request.Tools) != 1 || d.Request.Tools[0].Name != "get_weather" {
		t.Fatalf("工具解码异常: %+v", d.Request.Tools)
	}
	if !hasCap(d.Capabilities(), canonical.CapToolCalling) {
		t.Errorf("应报告 tool_calling: %v", d.Capabilities())
	}
}

func TestDecodeReportsStructuredOutput(t *testing.T) {
	d := mustDecode(t, `{"model":"m","messages":[{"role":"user","content":"x"}],
	  "response_format":{"type":"json_schema",
	    "json_schema":{"name":"out","schema":{"type":"object"},"strict":true}}}`)
	if d.Request.ResponseFormat == nil || d.Request.ResponseFormat.Kind != canonical.FormatJSONSchema {
		t.Fatalf("ResponseFormat 解码异常: %+v", d.Request.ResponseFormat)
	}
	if !hasCap(d.Capabilities(), canonical.CapStructuredOutput) {
		t.Errorf("应报告 structured_output: %v", d.Capabilities())
	}
}

func TestDecodeReportsReasoning(t *testing.T) {
	d := mustDecode(t, `{"model":"m","messages":[{"role":"user","content":"x"}],
	  "reasoning_effort":"high"}`)
	if d.Request.Reasoning == nil || d.Request.Reasoning.Effort != canonical.EffortHigh {
		t.Fatalf("Reasoning 解码异常: %+v", d.Request.Reasoning)
	}
	if !hasCap(d.Capabilities(), canonical.CapReasoning) {
		t.Errorf("应报告 reasoning: %v", d.Capabilities())
	}
}

func TestDecodeExtractsSystemMessage(t *testing.T) {
	d := mustDecode(t, `{"model":"m","messages":[
	  {"role":"system","content":"你是助手"},
	  {"role":"user","content":"hi"}]}`)
	if len(d.Request.System) != 1 || d.Request.System[0].Text != "你是助手" {
		t.Errorf("system 应被提取到 System: %+v", d.Request.System)
	}
	if len(d.Request.Messages) != 1 {
		t.Errorf("system 不应留在 Messages: %+v", d.Request.Messages)
	}
}

func TestDecodeCountsInlineImageBytes(t *testing.T) {
	// 40 个 'A' 是合法 base64，解码后 30 字节。
	big := strings.Repeat("A", 40)
	d := mustDecode(t, `{"model":"m","messages":[{"role":"user","content":[
	  {"type":"text","text":"看图"},
	  {"type":"image_url","image_url":{"url":"data:image/png;base64,`+big+`"}}]}]}`)
	if d.InlineBytes != 30 {
		t.Errorf("InlineBytes = %d, 期望 30", d.InlineBytes)
	}
	if !hasCap(d.Capabilities(), canonical.CapVisionInput) {
		t.Errorf("应报告 vision_input: %v", d.Capabilities())
	}
}

func TestDecodePassesThroughHTTPImageURL(t *testing.T) {
	d := mustDecode(t, `{"model":"m","messages":[{"role":"user","content":[
	  {"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`)
	if d.InlineBytes != 0 {
		t.Errorf("URL 图片不是内联负载，InlineBytes = %d", d.InlineBytes)
	}
	if !hasCap(d.Capabilities(), canonical.CapVisionInput) {
		t.Errorf("应报告 vision_input: %v", d.Capabilities())
	}
}

func TestDecodeAssistantToolCallsBecomeParts(t *testing.T) {
	d := mustDecode(t, `{"model":"m","messages":[
	  {"role":"user","content":"天气"},
	  {"role":"assistant","content":null,"tool_calls":[
	    {"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"上海\"}"}}]},
	  {"role":"tool","tool_call_id":"call_1","content":"晴"}]}`)

	if len(d.Request.Messages) != 3 {
		t.Fatalf("应有 3 条消息: %+v", d.Request.Messages)
	}
	asst := d.Request.Messages[1]
	if len(asst.Parts) != 1 || asst.Parts[0].Kind != canonical.PartToolCall {
		t.Fatalf("assistant 的 tool_calls 应归一成 PartToolCall: %+v", asst.Parts)
	}
	tool := d.Request.Messages[2]
	if tool.Role != canonical.RoleTool || tool.Parts[0].Kind != canonical.PartToolResult {
		t.Fatalf("tool 消息应是 PartToolResult: %+v", tool)
	}
	if !hasCap(d.Capabilities(), canonical.CapToolCalling) {
		t.Errorf("应报告 tool_calling: %v", d.Capabilities())
	}
}

func TestDecodeRejectsUnknownField(t *testing.T) {
	// 严格模式：未建模的字段必须报错，而不是静默忽略。
	_, err := Decode([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}],
	  "some_future_param":1}`))
	if err == nil {
		t.Fatal("未知字段应当被拒绝")
	}
}

func TestDecodeRejectsMissingModel(t *testing.T) {
	_, err := Decode([]byte(`{"messages":[{"role":"user","content":"x"}]}`))
	if err == nil {
		t.Fatal("缺少 model 应当被拒绝")
	}
}

func TestDecodeRejectsBadRole(t *testing.T) {
	_, err := Decode([]byte(`{"model":"m","messages":[{"role":"alien","content":"x"}]}`))
	if err == nil {
		t.Fatal("无法识别的角色应当被拒绝")
	}
}

func TestDecodeParallelToolCallsGoToExtensions(t *testing.T) {
	d := mustDecode(t, `{"model":"m","messages":[{"role":"user","content":"x"}],
	  "parallel_tool_calls":false}`)
	raw, ok := d.Request.Extensions.Get(canonical.ExtOpenAI)
	if !ok || !strings.Contains(string(raw), "parallel_tool_calls") {
		t.Errorf("parallel_tool_calls 应存入 Extensions: %q", raw)
	}
}
