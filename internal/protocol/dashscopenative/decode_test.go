package dashscopenative

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
	d := mustDecode(t, `{"model":"qwen-plus",
	  "input":{"messages":[{"role":"user","content":"你好"}]},
	  "parameters":{"result_format":"message"}}`)

	if d.Request.Model != "qwen-plus" {
		t.Errorf("Model = %q", d.Request.Model)
	}
	if len(d.Request.Messages) != 1 || d.Request.Messages[0].Role != canonical.RoleUser {
		t.Errorf("消息解码异常: %+v", d.Request.Messages)
	}
	if !hasCap(d.Capabilities(), canonical.CapTextGeneration) {
		t.Errorf("应报告 text_generation: %v", d.Capabilities())
	}
}

func TestDecodeExtractsSystem(t *testing.T) {
	d := mustDecode(t, `{"model":"m",
	  "input":{"messages":[
	    {"role":"system","content":"你是助手"},
	    {"role":"user","content":"hi"}]}}`)
	if len(d.Request.System) != 1 || d.Request.System[0].Text != "你是助手" {
		t.Errorf("system 应被提取到 System: %+v", d.Request.System)
	}
	if len(d.Request.Messages) != 1 {
		t.Errorf("system 不应留在 Messages: %+v", d.Request.Messages)
	}
}

func TestDecodeReportsToolCalling(t *testing.T) {
	d := mustDecode(t, `{"model":"m",
	  "input":{"messages":[{"role":"user","content":"天气"}]},
	  "parameters":{"tools":[{"type":"function","function":{
	    "name":"get_weather","parameters":{"type":"object"}}}]}}`)
	if len(d.Request.Tools) != 1 || d.Request.Tools[0].Name != "get_weather" {
		t.Fatalf("工具解码异常: %+v", d.Request.Tools)
	}
	if !hasCap(d.Capabilities(), canonical.CapToolCalling) {
		t.Errorf("应报告 tool_calling: %v", d.Capabilities())
	}
}

func TestDecodeReportsWebSearch(t *testing.T) {
	d := mustDecode(t, `{"model":"m",
	  "input":{"messages":[{"role":"user","content":"x"}]},
	  "parameters":{"enable_search":true}}`)
	if !hasCap(d.Capabilities(), canonical.CapWebSearch) {
		t.Errorf("应报告 web_search: %v", d.Capabilities())
	}
}

func TestDecodeReportsReasoning(t *testing.T) {
	d := mustDecode(t, `{"model":"m",
	  "input":{"messages":[{"role":"user","content":"x"}]},
	  "parameters":{"enable_thinking":true}}`)
	if d.Request.Reasoning == nil {
		t.Fatal("enable_thinking 应解出 Reasoning")
	}
	if !hasCap(d.Capabilities(), canonical.CapReasoning) {
		t.Errorf("应报告 reasoning: %v", d.Capabilities())
	}
}

func TestDecodeCountsInlineImageBytes(t *testing.T) {
	big := strings.Repeat("A", 40) // 合法 base64，解码后 30 字节
	d := mustDecode(t, `{"model":"m",
	  "input":{"messages":[{"role":"user","content":[
	    {"type":"text","text":"看图"},
	    {"type":"image","image":"data:image/png;base64,`+big+`"}]}]}}`)
	if d.InlineBytes != 30 {
		t.Errorf("InlineBytes = %d, 期望 30", d.InlineBytes)
	}
	if !hasCap(d.Capabilities(), canonical.CapVisionInput) {
		t.Errorf("应报告 vision_input: %v", d.Capabilities())
	}
}

// TestDecodeCountsKeyBasedMultimodal 固化官方多模态示例的纯键式内容块
// （{"image":"..."}，没有 type 字段）也要被识别——否则图片能力漏报、
// data URI 字节数不计入，入口的内联上限会被绕过。
func TestDecodeCountsKeyBasedMultimodal(t *testing.T) {
	big := strings.Repeat("A", 40) // 合法 base64，解码后 30 字节
	d := mustDecode(t, `{"model":"m",
	  "input":{"messages":[{"role":"user","content":[
	    {"text":"看图"},
	    {"image":"data:image/jpeg;base64,`+big+`"}]}]}}`)
	if d.InlineBytes != 30 {
		t.Errorf("纯键式 image 的 InlineBytes = %d, 期望 30", d.InlineBytes)
	}
	if !hasCap(d.Capabilities(), canonical.CapVisionInput) {
		t.Errorf("纯键式 image 应报告 vision_input: %v", d.Capabilities())
	}
}

// TestDecodeVideoArrayDoesNotDropSiblings 固化「一块解不出来不得牵连兄弟块」。
//
// video 官方是 array 或 string；把整段 content 一次性解进 []ContentPart 时，
// 数组形态的 video 会让整段解析失败，兄弟块（含带 data URI 的图片）一起丢，
// InlineBytes 归零、入口内联上限被绕过。这是评审点名的真实洞。
func TestDecodeVideoArrayDoesNotDropSiblings(t *testing.T) {
	big := strings.Repeat("A", 40) // 合法 base64，解码后 30 字节
	d := mustDecode(t, `{"model":"m","input":{"messages":[{"role":"user","content":[
	  {"text":"看图"},
	  {"image":"data:image/jpeg;base64,`+big+`"},
	  {"video":["data:image/jpeg;base64,`+big+`","https://example.com/2.jpg"]}
	]}]}}`)

	// 图片 30 + 视频首帧 30 = 60；URL 帧不计。
	if d.InlineBytes != 60 {
		t.Errorf("InlineBytes = %d, 期望 60（数组 video 不得吃掉兄弟块的字节）", d.InlineBytes)
	}
	if !hasCap(d.Capabilities(), canonical.CapVisionInput) {
		t.Errorf("兄弟图片块应仍报告 vision_input: %v", d.Capabilities())
	}
	if !hasCap(d.Capabilities(), canonical.CapVideoInput) {
		t.Errorf("数组形态 video 应报告 video_input: %v", d.Capabilities())
	}
}

// TestDecodeVideoFramesCountEveryFrame 钉死 video 帧数组的**逐帧**内联统计。
//
// 只算首帧的话，后面每一帧都白送——一个 200 帧的 base64 视频，入口只看见
// 第一帧那点字节，内联上限形同虚设，内存该吃的照吃（原则 2.6）。
//
// 三帧长度刻意各不相同（3 / 6 / 9），总和 18 只有「三帧都算」这一种拆法：
// 漏掉任何一帧、或把某帧重复计入，得到的都不是 18。用三个等长帧就没有这个
// 分辨力——漏一帧与算错一帧的结果可能撞在一起。
func TestDecodeVideoFramesCountEveryFrame(t *testing.T) {
	// base64("xxx") / base64("xxxxxx") / base64("xxxxxxxxx")，解码后 3 / 6 / 9 字节。
	const (
		frame3 = "eHh4"
		frame6 = "eHh4eHh4"
		frame9 = "eHh4eHh4eHh4"
	)
	d := mustDecode(t, `{"model":"m","input":{"messages":[{"role":"user","content":[
	  {"text":"概括视频"},
	  {"video":[
	    "data:video/mp4;base64,`+frame3+`",
	    "data:video/mp4;base64,`+frame6+`",
	    "data:video/mp4;base64,`+frame9+`"]}
	]}]}}`)

	if want := int64(3 + 6 + 9); d.InlineBytes != want {
		t.Errorf("InlineBytes = %d，期望 %d（三帧逐帧累加）——后续帧绕过了内联上限",
			d.InlineBytes, want)
	}
	if !hasCap(d.Capabilities(), canonical.CapVideoInput) {
		t.Errorf("帧数组形态应报告 video_input: %v", d.Capabilities())
	}
}

// TestDecodeVideoStringStillWorks 保证字符串形态没被改坏。
func TestDecodeVideoStringStillWorks(t *testing.T) {
	d := mustDecode(t, `{"model":"m","input":{"messages":[{"role":"user","content":[
	  {"video":"https://example.com/v.mp4"}]}]}}`)
	if !hasCap(d.Capabilities(), canonical.CapVideoInput) {
		t.Errorf("字符串形态 video 应报告 video_input: %v", d.Capabilities())
	}
}

// TestDecodeMalformedPartOnlyDropsItself 固化损失局限在解不出来的那一块。
func TestDecodeMalformedPartOnlyDropsItself(t *testing.T) {
	big := strings.Repeat("A", 40)
	d := mustDecode(t, `{"model":"m","input":{"messages":[{"role":"user","content":[
	  {"image":"data:image/png;base64,`+big+`"},
	  {"image":12345}
	]}]}}`)
	if d.InlineBytes != 30 {
		t.Errorf("InlineBytes = %d, 期望 30（坏块不得牵连好块）", d.InlineBytes)
	}
}

func TestDecodeAcceptsUnknownFields(t *testing.T) {
	// 同源直通的契约：没建模的字段也要透传，不能拒。
	d := mustDecode(t, `{"model":"m",
	  "input":{"messages":[{"role":"user","content":"x"}]},
	  "parameters":{"some_future_param":123}}`)
	if d.Request.Model != "m" {
		t.Errorf("未知字段不应导致拒绝: %q", d.Request.Model)
	}
}

func TestDecodeRejectsMissingModel(t *testing.T) {
	_, err := Decode([]byte(`{"input":{"messages":[{"role":"user","content":"x"}]}}`))
	if err == nil {
		t.Fatal("缺少 model 应当被拒绝")
	}
}

// TestDecodeAcceptsToolCallContinuation 固化同源直通不得被不完整的 Canonical 投影
// 否决：工具调用续轮里 assistant 的 content 为空、tool 角色携带结果，这些都是
// 合法的 DashScope 请求，字节会被原样转发，解码器不能因为它们投影不出完整
// Canonical 就拒绝。
func TestDecodeAcceptsToolCallContinuation(t *testing.T) {
	_, err := Decode([]byte(`{"model":"qwen-plus","input":{"messages":[
	  {"role":"user","content":"天气"},
	  {"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function",
	    "function":{"name":"get_weather","arguments":"{\"city\":\"上海\"}"}}]},
	  {"role":"tool","content":"晴","tool_call_id":"c1"}
	]},"parameters":{"result_format":"message"}}`))
	if err != nil {
		t.Fatalf("合法的工具调用续轮被拒绝: %v", err)
	}
}

// TestDecodeAcceptsEmptyAssistantContent 固化 assistant 空 content 不被拒绝。
func TestDecodeAcceptsEmptyAssistantContent(t *testing.T) {
	_, err := Decode([]byte(`{"model":"m","input":{"messages":[
	  {"role":"user","content":"x"},
	  {"role":"assistant","content":null}
	]}}`))
	if err != nil {
		t.Fatalf("assistant 空 content 被拒绝: %v", err)
	}
}
