package openairesponses

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

func mustDecode(t *testing.T, body string) *Decoded {
	t.Helper()
	d, err := Decode([]byte(body))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	return d
}

func decodeErr(t *testing.T, body string) *canonical.Error {
	t.Helper()
	_, err := Decode([]byte(body))
	if err == nil {
		t.Fatal("应当解码失败")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际 %T: %v", err, err)
	}
	return cerr
}

// TestInputAsBareString 覆盖最简形态：input 是一个裸字符串。
func TestInputAsBareString(t *testing.T) {
	d := mustDecode(t, `{"model":"gpt-5","input":"上海天气怎么样？"}`)

	if len(d.Request.Messages) != 1 {
		t.Fatalf("应解出 1 条消息，实际 %d 条", len(d.Request.Messages))
	}
	m := d.Request.Messages[0]
	if m.Role != canonical.RoleUser || m.TextContent() != "上海天气怎么样？" {
		t.Errorf("消息解码有误: %+v", m)
	}
}

// TestInstructionsBecomeSystem 固化一处实质差异。
//
// Responses 的 instructions 是**顶层参数**，不是 messages 里的一条。归到
// System 之后，各出站协议各自还原：Anthropic 放顶层 system，DashScope 用
// system role。混进 messages 会让这个还原无从做起。
func TestInstructionsBecomeSystem(t *testing.T) {
	d := mustDecode(t, `{"model":"m","instructions":"你是助手","input":"hi"}`)

	if len(d.Request.System) != 1 || d.Request.System[0].Text != "你是助手" {
		t.Errorf("instructions 应归到 System，实际 %+v", d.Request.System)
	}
	for _, m := range d.Request.Messages {
		if m.Role == canonical.RoleSystem {
			t.Error("instructions 不该变成一条 system 消息")
		}
	}
}

func TestInputItemsWithMixedContent(t *testing.T) {
	body := `{
	  "model": "m",
	  "input": [
	    {"role":"user","content":[
	      {"type":"input_text","text":"看图"},
	      {"type":"input_image","image_url":"https://x/a.png"}
	    ]}
	  ]
	}`
	d := mustDecode(t, body)

	parts := d.Request.Messages[0].Parts
	if len(parts) != 2 {
		t.Fatalf("应解出 2 个内容块，实际 %d 个", len(parts))
	}
	if parts[0].Kind != canonical.PartText || parts[1].Kind != canonical.PartMedia {
		t.Errorf("内容块类型有误: %+v", parts)
	}
	// URL 形态直接透传，网关不代下载（原则 2.6），因此不计入内联字节。
	if d.InlineBytes != 0 {
		t.Errorf("URL 形态不该计入内联字节，实际 %d", d.InlineBytes)
	}
}

// TestDataURICountsAsInline 固化「内联负载要计量」。
// 没有这个计数，入口处的大小上限就是摆设。
func TestDataURICountsAsInline(t *testing.T) {
	// "hello" 的 base64
	body := `{"model":"m","input":[{"role":"user","content":[
	  {"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}
	]}]}`
	d := mustDecode(t, body)

	if d.InlineBytes != 5 {
		t.Errorf("内联字节 = %d, 期望 5", d.InlineBytes)
	}
	media := d.Request.Messages[0].Parts[0].Media
	if media.URL != "" || len(media.Data) != 5 {
		t.Errorf("data: URI 应解成内联字节而非 URL，实际 %+v", media)
	}
	if media.MIMEType != "image/png" {
		t.Errorf("MIME 类型 = %q", media.MIMEType)
	}
}

// TestFileRefRecordsProvider 固化跨 Provider 拒绝的前提。
// 不记下文件属于谁，降级矩阵就无从判断它能不能迁移。
func TestFileRefRecordsProvider(t *testing.T) {
	d := mustDecode(t, `{"model":"m","input":[{"role":"user","content":[
	  {"type":"input_file","file_id":"file-abc"}
	]}]}`)

	ref := d.Request.Messages[0].Parts[0].Media.FileRef
	if ref == nil || ref.Provider != "openai" || ref.ID != "file-abc" {
		t.Errorf("文件引用应记下所属 Provider，实际 %+v", ref)
	}
}

func TestFunctionCallAndOutput(t *testing.T) {
	body := `{
	  "model": "m",
	  "input": [
	    {"role":"user","content":"天气"},
	    {"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"上海\"}"},
	    {"type":"function_call_output","call_id":"call_1","output":"多云"}
	  ]
	}`
	d := mustDecode(t, body)

	if len(d.Request.Messages) != 3 {
		t.Fatalf("应解出 3 条消息，实际 %d 条", len(d.Request.Messages))
	}

	call := d.Request.Messages[1]
	if call.Role != canonical.RoleAssistant || call.Parts[0].Kind != canonical.PartToolCall {
		t.Errorf("工具调用解码有误: %+v", call)
	}
	var args map[string]string
	if err := json.Unmarshal(call.Parts[0].ToolCall.Arguments, &args); err != nil {
		t.Fatalf("工具参数不是合法 JSON: %v", err)
	}
	if args["city"] != "上海" {
		t.Errorf("工具参数 = %v", args)
	}

	result := d.Request.Messages[2]
	if result.Role != canonical.RoleTool || result.Parts[0].ToolResult.CallID != "call_1" {
		t.Errorf("工具结果解码有误: %+v", result)
	}
}

// TestMalformedToolArgumentsFailFast 固化「半截 JSON 不往上游送」。
// 转过去只会换来一个难以定位的上游解析错误。
func TestMalformedToolArgumentsFailFast(t *testing.T) {
	cerr := decodeErr(t, `{"model":"m","input":[
	  {"type":"function_call","call_id":"c","name":"f","arguments":"{\"a\":"}
	]}`)
	if !strings.Contains(cerr.Message, "合法 JSON") {
		t.Errorf("错误消息应指出参数不合法，实际: %s", cerr.Message)
	}
}

// TestReasoningItemIsDropped 记录一处有意的丢弃。
//
// OpenAI 回传的推理条目内容是加密/删节的，网关不去解析它——原样丢弃比假装
// 理解安全。同源快通道走字节透传，不经过这条路径。
func TestReasoningItemIsDropped(t *testing.T) {
	d := mustDecode(t, `{"model":"m","input":[
	  {"role":"user","content":"hi"},
	  {"type":"reasoning","id":"rs_1"}
	]}`)

	if len(d.Request.Messages) != 1 {
		t.Errorf("推理条目应被丢弃，实际解出 %d 条消息", len(d.Request.Messages))
	}
}

// TestStoreSemantics 是这个包里最需要说清楚的一处。
//
// OpenAI 的 store 默认为 true。把省略也当成「要求服务端会话」，默认配置下
// 几乎每个请求都会被拒；而多数 SDK 不显式发送它，说明调用方并不在意。
// 因此只有**显式** true 才触发能力。
func TestStoreSemantics(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"省略 store", `{"model":"m","input":"hi"}`, false},
		{"显式 false", `{"model":"m","input":"hi","store":false}`, false},
		{"显式 true", `{"model":"m","input":"hi","store":true}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := mustDecode(t, tc.body)
			if d.WantsStore != tc.want {
				t.Errorf("WantsStore = %v, 期望 %v", d.WantsStore, tc.want)
			}
			if got := hasCap(d.Capabilities(), canonical.CapStatefulConversation); got != tc.want {
				t.Errorf("服务端会话能力触发 = %v, 期望 %v", got, tc.want)
			}
		})
	}
}

// TestPreviousResponseIDAlwaysTriggersCapability 覆盖会话的读取端。
// 读取一定是在依赖保管的历史，没有「默认值」的歧义。
func TestPreviousResponseIDAlwaysTriggersCapability(t *testing.T) {
	d := mustDecode(t, `{"model":"m","input":"hi","previous_response_id":"resp_1"}`)

	if d.PreviousResponseID != "resp_1" {
		t.Errorf("PreviousResponseID = %q", d.PreviousResponseID)
	}
	if !hasCap(d.Capabilities(), canonical.CapStatefulConversation) {
		t.Error("previous_response_id 必须触发服务端会话能力")
	}
}

func TestReasoningEffort(t *testing.T) {
	d := mustDecode(t, `{"model":"m","input":"hi","reasoning":{"effort":"high","summary":"auto"}}`)

	if d.Request.Reasoning == nil {
		t.Fatal("reasoning 未解出")
	}
	if d.Request.Reasoning.Effort != canonical.EffortHigh {
		t.Errorf("effort = %q", d.Request.Reasoning.Effort)
	}
	// summary 非空意味着客户端要看推理内容本身，而不只是消耗推理 token。
	if !d.Request.Reasoning.Visible {
		t.Error("指定了 summary 应当标记为需要可见推理")
	}
}

func TestStructuredOutput(t *testing.T) {
	d := mustDecode(t, `{"model":"m","input":"hi","text":{"format":{
	  "type":"json_schema","name":"weather","strict":true,
	  "schema":{"type":"object","properties":{"c":{"type":"string"}}}
	}}}`)

	f := d.Request.ResponseFormat
	if f == nil || f.Kind != canonical.FormatJSONSchema || !f.Strict || f.Name != "weather" {
		t.Fatalf("结构化输出解码有误: %+v", f)
	}
	if len(f.Schema) == 0 {
		t.Error("schema 丢失")
	}
}

func TestJSONSchemaWithoutSchemaFails(t *testing.T) {
	cerr := decodeErr(t, `{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"x"}}}`)
	if !strings.Contains(cerr.Message, "schema") {
		t.Errorf("错误消息应指出缺少 schema，实际: %s", cerr.Message)
	}
}

func TestToolChoiceForms(t *testing.T) {
	tests := []struct {
		raw      string
		wantMode canonical.ToolChoiceMode
		wantName string
	}{
		{`"auto"`, canonical.ToolChoiceAuto, ""},
		{`"none"`, canonical.ToolChoiceNone, ""},
		{`"required"`, canonical.ToolChoiceRequired, ""},
		{`{"type":"function","name":"get_weather"}`, canonical.ToolChoiceSpecific, "get_weather"},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			d := mustDecode(t, `{"model":"m","input":"hi","tool_choice":`+tc.raw+`}`)
			tc2 := d.Request.ToolChoice
			if tc2 == nil || tc2.Mode != tc.wantMode || tc2.Name != tc.wantName {
				t.Errorf("tool_choice = %+v, 期望 mode=%q name=%q", tc2, tc.wantMode, tc.wantName)
			}
		})
	}
}

// TestBuiltinToolIsRejected 固化「内建工具不做跨 Provider 映射」。
//
// 各家的 schema 不兼容，勉强映射只会让模型收到一个读不懂的定义。
// 分类是 unsupported（422）而非 bad_request——请求本身没错，是这条路不支持。
func TestBuiltinToolIsRejected(t *testing.T) {
	cerr := decodeErr(t, `{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`)
	if cerr.Class != canonical.ClassUnsupported {
		t.Errorf("分类 = %q, 期望 unsupported", cerr.Class)
	}
	if cerr.HTTPStatus() != 422 {
		t.Errorf("状态码 = %d, 期望 422", cerr.HTTPStatus())
	}
}

// TestUnknownFieldIsRejected 固化一处刻意的严格。
//
// Responses 还在演进。一个我们没实现的新参数若被静默忽略，客户端会以为它
// 生效了——报错至少让人知道网关落后了。
func TestUnknownFieldIsRejected(t *testing.T) {
	cerr := decodeErr(t, `{"model":"m","input":"hi","some_new_param":123}`)
	if cerr.Class != canonical.ClassBadRequest {
		t.Errorf("分类 = %q, 期望 bad_request", cerr.Class)
	}
}

func TestZeroTemperatureSurvives(t *testing.T) {
	d := mustDecode(t, `{"model":"m","input":"hi","temperature":0}`)

	// 0 是合法取值，与「未设置」语义不同。用非指针会让它变成上游默认值。
	if d.Request.Temperature == nil {
		t.Fatal("temperature=0 被当成了未设置")
	}
	if *d.Request.Temperature != 0 {
		t.Errorf("temperature = %v, 期望 0", *d.Request.Temperature)
	}
}

// TestParallelToolCallsGoesToExtensions 覆盖无 Canonical 对应字段的处理。
// 存进 Extensions 供同源快通道原样回填，而不是悄悄丢掉。
func TestParallelToolCallsGoesToExtensions(t *testing.T) {
	d := mustDecode(t, `{"model":"m","input":"hi","parallel_tool_calls":false}`)

	raw, ok := d.Request.Extensions.Get(canonical.ExtOpenAI)
	if !ok {
		t.Fatal("parallel_tool_calls 应存进 Extensions")
	}
	var ext map[string]bool
	if err := json.Unmarshal(raw, &ext); err != nil {
		t.Fatal(err)
	}
	if ext["parallel_tool_calls"] != false {
		t.Errorf("Extensions 内容 = %v", ext)
	}
}

func TestMissingModelAndInput(t *testing.T) {
	for _, body := range []string{
		`{"input":"hi"}`,
		`{"model":"m"}`,
		`{"model":"m","input":[]}`,
		`{"model":"m","input":""}`,
	} {
		t.Run(body, func(t *testing.T) {
			cerr := decodeErr(t, body)
			if cerr.Class != canonical.ClassBadRequest {
				t.Errorf("分类 = %q, 期望 bad_request", cerr.Class)
			}
		})
	}
}

func hasCap(caps []canonical.Capability, want canonical.Capability) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}
