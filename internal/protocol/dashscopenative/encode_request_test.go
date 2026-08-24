package dashscopenative

import (
	"encoding/json"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// TestEncodeRequestEnvelope 钉死出站信封骨架：model / input.messages / parameters.result_format。
func TestEncodeRequestEnvelope(t *testing.T) {
	canon := &canonical.Request{
		Model:    "logical",
		System:   []canonical.Part{canonical.Text("you are helpful")},
		Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.Part{canonical.Text("hi")}}},
	}
	body, err := EncodeRequest(canon, ChatSampling{}, DoorTextGeneration, "qwen-plus")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Model string `json:"model"`
		Input struct {
			Messages []map[string]any `json:"messages"`
		} `json:"input"`
		Parameters struct {
			ResultFormat string `json:"result_format"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Model != "qwen-plus" {
		t.Errorf("model 应为上游名，实际 %q", env.Model)
	}
	if env.Parameters.ResultFormat != "message" {
		t.Errorf("result_format 应固定 message，实际 %q", env.Parameters.ResultFormat)
	}
	if len(env.Input.Messages) != 2 { // system + user
		t.Errorf("应有 system+user 两条消息，实际 %d", len(env.Input.Messages))
	}
}

// TestEncodeRequestDoorPath 钉死门到路径的单一事实来源。
func TestEncodeRequestDoorPath(t *testing.T) {
	if DoorTextGeneration.Path() != TextGenerationPath {
		t.Errorf("text-generation 门应指向 %s", TextGenerationPath)
	}
	if DoorMultimodalGeneration.Path() != MultimodalGenerationPath {
		t.Errorf("multimodal-generation 门应指向 %s", MultimodalGenerationPath)
	}
	if Door("embedding").Path() != "" {
		t.Error("未知门路径应为空")
	}
}

// textCanon 是最小可编码请求：一条 user 文本消息。
func textCanon() *canonical.Request {
	return &canonical.Request{
		Model:    "logical",
		Messages: []canonical.Message{canonical.UserText("hi")},
	}
}

// encodeParams 编码后取出 parameters 段，供逐字段断言。
func encodeParams(t *testing.T, canon *canonical.Request, extra ChatSampling, door Door) map[string]any {
	t.Helper()
	body, err := EncodeRequest(canon, extra, door, "qwen-plus")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Parameters map[string]any `json:"parameters"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	return env.Parameters
}

// TestEncodeRequestParallelToolCallsDefaultsToTrue 钉死缺省注入。
//
// Native 的 parallel_tool_calls 默认 false，OpenAI 默认 true。客户端没提交时
// 不注入，并行调用就静默退化成串行——请求照样 200，少掉的并行没人看得见。
func TestEncodeRequestParallelToolCallsDefaultsToTrue(t *testing.T) {
	p := encodeParams(t, textCanon(), ChatSampling{}, DoorTextGeneration)
	if got, ok := p["parallel_tool_calls"]; !ok || got != true {
		t.Errorf("缺省应显式注入 parallel_tool_calls:true，实际 %v（存在=%v）", got, ok)
	}
}

// TestEncodeRequestParallelToolCallsExplicitFalseSurvives 钉死显式 false 不被缺省覆盖。
func TestEncodeRequestParallelToolCallsExplicitFalseSurvives(t *testing.T) {
	no := false
	p := encodeParams(t, textCanon(), ChatSampling{ParallelToolCalls: &no}, DoorTextGeneration)
	if got, ok := p["parallel_tool_calls"]; !ok || got != false {
		t.Errorf("显式 false 应原样保留，实际 %v（存在=%v）", got, ok)
	}
}

// TestEncodeRequestReasoningNoneOnlyDisablesThinking 钉死 none 档位的翻译。
//
// 官方语义是 reasoning_effort:"none" → enable_thinking:false。把 none 原样当成
// 一个 effort 档位发出去，上游会拒绝整个请求；两个字段一起发则互相矛盾。
func TestEncodeRequestReasoningNoneOnlyDisablesThinking(t *testing.T) {
	canon := textCanon()
	canon.Reasoning = &canonical.Reasoning{Effort: canonical.EffortNone}
	p := encodeParams(t, canon, ChatSampling{}, DoorTextGeneration)

	if got, ok := p["enable_thinking"]; !ok || got != false {
		t.Errorf("none 应发 enable_thinking:false，实际 %v（存在=%v）", got, ok)
	}
	if _, ok := p["reasoning_effort"]; ok {
		t.Error("none 不得发 reasoning_effort")
	}
	if _, ok := p["thinking_budget"]; ok {
		t.Error("不得发 thinking_budget")
	}
}

// TestEncodeRequestReasoningHighSendsEffortOnly 钉死非 none 档位逐字写 reasoning_effort。
func TestEncodeRequestReasoningHighSendsEffortOnly(t *testing.T) {
	canon := textCanon()
	canon.Reasoning = &canonical.Reasoning{Effort: canonical.EffortHigh}
	p := encodeParams(t, canon, ChatSampling{}, DoorTextGeneration)

	if got := p["reasoning_effort"]; got != "high" {
		t.Errorf("reasoning_effort 应为 high，实际 %v", got)
	}
	if _, ok := p["enable_thinking"]; ok {
		t.Error("非 none 档位不得发 enable_thinking")
	}
	if _, ok := p["thinking_budget"]; ok {
		t.Error("不得发 thinking_budget")
	}
}

// TestEncodeRequestNoReasoningSendsNeitherField 钉死未请求推理时两个字段都不发。
func TestEncodeRequestNoReasoningSendsNeitherField(t *testing.T) {
	p := encodeParams(t, textCanon(), ChatSampling{}, DoorTextGeneration)
	if _, ok := p["reasoning_effort"]; ok {
		t.Error("未请求推理不得发 reasoning_effort")
	}
	if _, ok := p["enable_thinking"]; ok {
		t.Error("未请求推理不得发 enable_thinking")
	}
}

// TestEncodeRequestToolsAreFunctionList 钉死工具声明编码与 result_format 不被工具改写。
func TestEncodeRequestToolsAreFunctionList(t *testing.T) {
	canon := textCanon()
	canon.Tools = []canonical.Tool{{
		Name:        "get_weather",
		Description: "查天气",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
	}}
	body, err := EncodeRequest(canon, ChatSampling{}, DoorTextGeneration, "qwen-plus")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Parameters struct {
			ResultFormat string `json:"result_format"`
			Tools        []struct {
				Type     string `json:"type"`
				Function struct {
					Name        string          `json:"name"`
					Description string          `json:"description"`
					Parameters  json.RawMessage `json:"parameters"`
				} `json:"function"`
			} `json:"tools"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Parameters.ResultFormat != "message" {
		t.Errorf("有 tools 时 result_format 仍应为 message，实际 %q", env.Parameters.ResultFormat)
	}
	if len(env.Parameters.Tools) != 1 {
		t.Fatalf("应有 1 个工具，实际 %d", len(env.Parameters.Tools))
	}
	tool := env.Parameters.Tools[0]
	if tool.Type != "function" {
		t.Errorf("工具 type 应为 function，实际 %q", tool.Type)
	}
	if tool.Function.Name != "get_weather" || tool.Function.Description != "查天气" {
		t.Errorf("工具名/描述丢失：%+v", tool.Function)
	}
	if string(tool.Function.Parameters) != `{"type":"object","properties":{"city":{"type":"string"}}}` {
		t.Errorf("工具 schema 应原样透传，实际 %s", tool.Function.Parameters)
	}
}

// TestEncodeRequestToolChoiceModes 钉死四种工具选择策略的线格式。
//
// specific 必须保留工具名：只发 "required" 会让模型自由挑一个工具，
// 与客户端点名要求的那个未必是同一个——请求成功，语义已经变了。
func TestEncodeRequestToolChoiceModes(t *testing.T) {
	cases := []struct {
		name   string
		choice canonical.ToolChoice
		want   string
	}{
		{"auto", canonical.ToolChoice{Mode: canonical.ToolChoiceAuto}, `"auto"`},
		{"none", canonical.ToolChoice{Mode: canonical.ToolChoiceNone}, `"none"`},
		{"required", canonical.ToolChoice{Mode: canonical.ToolChoiceRequired}, `"required"`},
		{
			"specific",
			canonical.ToolChoice{Mode: canonical.ToolChoiceSpecific, Name: "get_weather"},
			`{"type":"function","function":{"name":"get_weather"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			canon := textCanon()
			choice := tc.choice
			canon.ToolChoice = &choice
			body, err := EncodeRequest(canon, ChatSampling{}, DoorTextGeneration, "qwen-plus")
			if err != nil {
				t.Fatal(err)
			}
			var env struct {
				Parameters struct {
					ToolChoice json.RawMessage `json:"tool_choice"`
				} `json:"parameters"`
			}
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatal(err)
			}
			if string(env.Parameters.ToolChoice) != tc.want {
				t.Errorf("tool_choice = %s，期望 %s", env.Parameters.ToolChoice, tc.want)
			}
		})
	}
}

// TestEncodeRequestNoToolsOmitsFields 钉死无工具时两个字段都不发。
func TestEncodeRequestNoToolsOmitsFields(t *testing.T) {
	p := encodeParams(t, textCanon(), ChatSampling{}, DoorTextGeneration)
	if _, ok := p["tools"]; ok {
		t.Error("无工具不得发 tools")
	}
	if _, ok := p["tool_choice"]; ok {
		t.Error("无工具选择不得发 tool_choice")
	}
}

// TestEncodeRequestMaxTokensIsExclusive 钉死两个上限字段二者择一。
//
// 只发客户端实际提交的那一个。两个都发等于给上游两个互相冲突的限制，
// 上游取哪个是它自己的实现细节，输出长度因此不可预期；Canonical 的
// MaxOutputTokens 也不得再制造第三个限制。
func TestEncodeRequestMaxTokensIsExclusive(t *testing.T) {
	n100, n200, n300 := 100, 200, 300

	t.Run("只提交 max_tokens", func(t *testing.T) {
		canon := textCanon()
		canon.MaxOutputTokens = &n300
		p := encodeParams(t, canon, ChatSampling{MaxTokens: &n100}, DoorTextGeneration)
		if got := p["max_tokens"]; got != float64(100) {
			t.Errorf("max_tokens = %v，期望 100", got)
		}
		if _, ok := p["max_completion_tokens"]; ok {
			t.Error("未提交 max_completion_tokens 时不得发它")
		}
	})

	t.Run("只提交 max_completion_tokens", func(t *testing.T) {
		canon := textCanon()
		canon.MaxOutputTokens = &n300
		p := encodeParams(t, canon, ChatSampling{MaxCompletionTokens: &n200}, DoorTextGeneration)
		if got := p["max_completion_tokens"]; got != float64(200) {
			t.Errorf("max_completion_tokens = %v，期望 200", got)
		}
		if _, ok := p["max_tokens"]; ok {
			t.Error("未提交 max_tokens 时不得发它")
		}
	})

	t.Run("两个都提交时 max_completion_tokens 胜出", func(t *testing.T) {
		p := encodeParams(t, textCanon(),
			ChatSampling{MaxTokens: &n100, MaxCompletionTokens: &n200}, DoorTextGeneration)
		if got := p["max_completion_tokens"]; got != float64(200) {
			t.Errorf("max_completion_tokens = %v，期望 200", got)
		}
		if _, ok := p["max_tokens"]; ok {
			t.Error("max_completion_tokens 在场时不得再发 max_tokens")
		}
	})

	t.Run("都未提交时不发任何上限", func(t *testing.T) {
		canon := textCanon()
		canon.MaxOutputTokens = &n300
		p := encodeParams(t, canon, ChatSampling{}, DoorTextGeneration)
		if _, ok := p["max_tokens"]; ok {
			t.Error("客户端未提交上限时不得凭 Canonical 造一个 max_tokens")
		}
		if _, ok := p["max_completion_tokens"]; ok {
			t.Error("客户端未提交上限时不得凭 Canonical 造一个 max_completion_tokens")
		}
	})
}

// encodedMessages 取出 input.messages 原样字节，供内容形态断言。
func encodedMessages(t *testing.T, body []byte) []json.RawMessage {
	t.Helper()
	var env struct {
		Input struct {
			Messages []json.RawMessage `json:"messages"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	return env.Input.Messages
}

// TestEncodeRequestTextDoorJoinsTextParts 钉死文本门的 content 是单个字符串。
func TestEncodeRequestTextDoorJoinsTextParts(t *testing.T) {
	canon := &canonical.Request{
		Model: "logical",
		Messages: []canonical.Message{{
			Role:  canonical.RoleUser,
			Parts: []canonical.Part{canonical.Text("第一段"), canonical.Text("第二段")},
		}},
	}
	body, err := EncodeRequest(canon, ChatSampling{}, DoorTextGeneration, "qwen-plus")
	if err != nil {
		t.Fatal(err)
	}
	msgs := encodedMessages(t, body)
	if len(msgs) != 1 {
		t.Fatalf("应有 1 条消息，实际 %d", len(msgs))
	}
	if want := `{"role":"user","content":"第一段\n第二段"}`; string(msgs[0]) != want {
		t.Errorf("文本门 content 应为拼接字符串，实际 %s", msgs[0])
	}
}

// TestEncodeRequestTextDoorRejectsMedia 钉死文本门遇媒体 fail-closed。
//
// 媒体只能走多模态门。文本门静默丢掉图片，模型看不见图却照常作答——
// 请求 200，答案却是凭空编的，这种失败最难发现。
func TestEncodeRequestTextDoorRejectsMedia(t *testing.T) {
	canon := &canonical.Request{
		Model: "logical",
		Messages: []canonical.Message{{
			Role: canonical.RoleUser,
			Parts: []canonical.Part{
				canonical.Text("这是什么"),
				canonical.ImageURL("https://example.com/a.png", "image/png"),
			},
		}},
	}
	if _, err := EncodeRequest(canon, ChatSampling{}, DoorTextGeneration, "qwen-plus"); err == nil {
		t.Fatal("文本门含媒体应报错，不得静默丢弃")
	}
}

// TestEncodeRequestMultimodalDoorEncodesBlocks 钉死多模态门的单键块数组。
func TestEncodeRequestMultimodalDoorEncodesBlocks(t *testing.T) {
	canon := &canonical.Request{
		Model: "logical",
		Messages: []canonical.Message{{
			Role: canonical.RoleUser,
			Parts: []canonical.Part{
				canonical.Text("这是什么"),
				canonical.ImageURL("https://example.com/a.png", "image/png"),
				canonical.AudioData([]byte("RIFF"), canonical.AudioFmt{Encoding: "pcm16", SampleRate: 16000, Channels: 1}),
			},
		}},
	}
	body, err := EncodeRequest(canon, ChatSampling{}, DoorMultimodalGeneration, "qwen3-vl-plus")
	if err != nil {
		t.Fatal(err)
	}
	msgs := encodedMessages(t, body)
	if len(msgs) != 1 {
		t.Fatalf("应有 1 条消息，实际 %d", len(msgs))
	}
	var m struct {
		Content []map[string]string `json:"content"`
	}
	if err := json.Unmarshal(msgs[0], &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Content) != 3 {
		t.Fatalf("应有 3 个内容块（顺序保持），实际 %d：%v", len(m.Content), m.Content)
	}
	if m.Content[0]["text"] != "这是什么" {
		t.Errorf("第一块应是 text，实际 %v", m.Content[0])
	}
	if m.Content[1]["image"] != "https://example.com/a.png" {
		t.Errorf("URL 图片应原样透传（网关不代下载），实际 %v", m.Content[1])
	}
	if got := m.Content[2]["audio"]; got != "data:audio/pcm16;base64,UklGRg==" {
		t.Errorf("内联音频应编成 data URI，实际 %q", got)
	}
	for i, b := range m.Content {
		if len(b) != 1 {
			t.Errorf("内容块 %d 必须是单键形态，实际 %v", i, b)
		}
	}
}

// TestEncodeRequestMultimodalInlineImageBecomesDataURI 钉死内联图片的 data URI 形态。
func TestEncodeRequestMultimodalInlineImageBecomesDataURI(t *testing.T) {
	canon := &canonical.Request{
		Model: "logical",
		Messages: []canonical.Message{{
			Role:  canonical.RoleUser,
			Parts: []canonical.Part{canonical.ImageData([]byte{0x89, 0x50}, "image/png")},
		}},
	}
	body, err := EncodeRequest(canon, ChatSampling{}, DoorMultimodalGeneration, "qwen3-vl-plus")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Content []map[string]string `json:"content"`
	}
	if err := json.Unmarshal(encodedMessages(t, body)[0], &m); err != nil {
		t.Fatal(err)
	}
	if got := m.Content[0]["image"]; got != "data:image/png;base64,iVA=" {
		t.Errorf("内联图片应编成 data URI，实际 %q", got)
	}
}

// TestEncodeRequestRejectsFileRefAndMediaFile 钉死两条不可搬运的媒体形态。
//
// FileRef 绑定具体 Provider，跨 Provider 搬运是把不可控成本转嫁给网关；
// Native 内容块词表到 video 为止，根本没有通用 file 块的落点。
func TestEncodeRequestRejectsFileRefAndMediaFile(t *testing.T) {
	cases := map[string]canonical.Part{
		"跨 Provider 的 FileRef": {
			Kind: canonical.PartMedia,
			Media: &canonical.Media{
				Kind:    canonical.MediaImage,
				FileRef: &canonical.FileRef{Provider: "openai", ID: "file-123"},
			},
		},
		"没有落点的 file 媒体": {
			Kind: canonical.PartMedia,
			Media: &canonical.Media{
				Kind:     canonical.MediaFile,
				MIMEType: "application/pdf",
				Data:     []byte("%PDF"),
			},
		},
	}
	for name, part := range cases {
		t.Run(name, func(t *testing.T) {
			canon := &canonical.Request{
				Model:    "logical",
				Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.Part{part}}},
			}
			if _, err := EncodeRequest(canon, ChatSampling{}, DoorMultimodalGeneration, "qwen3-vl-plus"); err == nil {
				t.Fatal("应显式报错，不得静默丢弃")
			}
		})
	}
}

// TestEncodeRequestResponseFormat 钉死三种结构化输出形态的线格式。
func TestEncodeRequestResponseFormat(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"n":{"type":"number"}}}`)
	cases := []struct {
		name   string
		format canonical.ResponseFormat
		want   string
	}{
		{"json_object", canonical.ResponseFormat{Kind: canonical.FormatJSONObject}, `{"type":"json_object"}`},
		{
			"json_schema",
			canonical.ResponseFormat{Kind: canonical.FormatJSONSchema, Name: "reply", Schema: schema, Strict: true},
			`{"type":"json_schema","json_schema":{"name":"reply","schema":{"type":"object","properties":{"n":{"type":"number"}}},"strict":true}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			canon := textCanon()
			f := tc.format
			canon.ResponseFormat = &f
			body, err := EncodeRequest(canon, ChatSampling{}, DoorTextGeneration, "qwen-plus")
			if err != nil {
				t.Fatal(err)
			}
			var env struct {
				Parameters struct {
					ResponseFormat json.RawMessage `json:"response_format"`
				} `json:"parameters"`
			}
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatal(err)
			}
			if string(env.Parameters.ResponseFormat) != tc.want {
				t.Errorf("response_format = %s，期望 %s", env.Parameters.ResponseFormat, tc.want)
			}
		})
	}

	t.Run("text 不发", func(t *testing.T) {
		canon := textCanon()
		canon.ResponseFormat = &canonical.ResponseFormat{Kind: canonical.FormatText}
		p := encodeParams(t, canon, ChatSampling{}, DoorTextGeneration)
		if _, ok := p["response_format"]; ok {
			t.Error("text 是默认形态，不得发 response_format")
		}
	})

	t.Run("缺省不发", func(t *testing.T) {
		p := encodeParams(t, textCanon(), ChatSampling{}, DoorTextGeneration)
		if _, ok := p["response_format"]; ok {
			t.Error("未请求结构化输出不得发 response_format")
		}
	})
}

// TestEncodeRequestToolHistoryKeepsLinkage 钉死工具历史的三处关联。
//
// assistant 的 tool_calls[].id 与随后 tool 消息的 tool_call_id 必须一一对上，
// arguments 必须是 JSON 字符串。任一处断了，上游都无从判断这条结果回应的是
// 哪次调用——多工具并行时会张冠李戴，而请求本身仍然成功。
func TestEncodeRequestToolHistoryKeepsLinkage(t *testing.T) {
	canon := &canonical.Request{
		Model: "logical",
		Messages: []canonical.Message{
			canonical.UserText("北京天气如何"),
			{
				Role: canonical.RoleAssistant,
				Parts: []canonical.Part{
					canonical.Text("我查一下"),
					{
						Kind: canonical.PartToolCall,
						ToolCall: &canonical.ToolCall{
							ID:        "call_abc",
							Name:      "get_weather",
							Arguments: json.RawMessage(`{"city":"北京"}`),
						},
					},
				},
			},
			{
				Role: canonical.RoleTool,
				Parts: []canonical.Part{{
					Kind: canonical.PartToolResult,
					ToolResult: &canonical.ToolResult{
						CallID:  "call_abc",
						Content: []canonical.Part{canonical.Text("晴 25 度")},
					},
				}},
			},
		},
	}
	body, err := EncodeRequest(canon, ChatSampling{}, DoorTextGeneration, "qwen-plus")
	if err != nil {
		t.Fatal(err)
	}
	msgs := encodedMessages(t, body)
	if len(msgs) != 3 {
		t.Fatalf("应有 user/assistant/tool 三条消息，实际 %d", len(msgs))
	}

	var assistant struct {
		Role      string `json:"role"`
		Content   string `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(msgs[1], &assistant); err != nil {
		t.Fatal(err)
	}
	if assistant.Content != "我查一下" {
		t.Errorf("助手文本应保留，实际 %q", assistant.Content)
	}
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("应有 1 次工具调用，实际 %d", len(assistant.ToolCalls))
	}
	call := assistant.ToolCalls[0]
	if call.ID != "call_abc" || call.Type != "function" || call.Function.Name != "get_weather" {
		t.Errorf("工具调用标识丢失：%+v", call)
	}
	if call.Function.Arguments != `{"city":"北京"}` {
		t.Errorf("arguments 应是完整 JSON 字符串，实际 %q", call.Function.Arguments)
	}

	var tool struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
	}
	if err := json.Unmarshal(msgs[2], &tool); err != nil {
		t.Fatal(err)
	}
	if tool.Role != "tool" || tool.ToolCallID != "call_abc" {
		t.Errorf("工具结果应通过 tool_call_id 关联到 call_abc，实际 %+v", tool)
	}
	if tool.Content != "晴 25 度" {
		t.Errorf("工具结果内容应保留，实际 %q", tool.Content)
	}
}

// TestEncodeRequestToolCallWithoutArgumentsSendsEmptyObject 钉死无参数调用发 "{}"。
// 空串会让上游把这次调用当成参数缺失而拒绝整轮。
func TestEncodeRequestToolCallWithoutArgumentsSendsEmptyObject(t *testing.T) {
	canon := &canonical.Request{
		Model: "logical",
		Messages: []canonical.Message{
			canonical.UserText("现在几点"),
			{
				Role: canonical.RoleAssistant,
				Parts: []canonical.Part{{
					Kind:     canonical.PartToolCall,
					ToolCall: &canonical.ToolCall{ID: "call_1", Name: "now"},
				}},
			},
		},
	}
	body, err := EncodeRequest(canon, ChatSampling{}, DoorTextGeneration, "qwen-plus")
	if err != nil {
		t.Fatal(err)
	}
	var assistant struct {
		ToolCalls []struct {
			Function struct {
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(encodedMessages(t, body)[1], &assistant); err != nil {
		t.Fatal(err)
	}
	if got := assistant.ToolCalls[0].Function.Arguments; got != "{}" {
		t.Errorf("无参数调用应发 {}，实际 %q", got)
	}
}

// TestEncodeRequestToolResultWithoutCallIDFails 钉死缺失关联时 fail-closed。
func TestEncodeRequestToolResultWithoutCallIDFails(t *testing.T) {
	canon := &canonical.Request{
		Model: "logical",
		Messages: []canonical.Message{{
			Role: canonical.RoleTool,
			Parts: []canonical.Part{{
				Kind:       canonical.PartToolResult,
				ToolResult: &canonical.ToolResult{Content: []canonical.Part{canonical.Text("x")}},
			}},
		}},
	}
	if _, err := EncodeRequest(canon, ChatSampling{}, DoorTextGeneration, "qwen-plus"); err == nil {
		t.Fatal("tool_result 缺 call_id 应报错，不得发出无从关联的结果")
	}
}

// TestEncodeRequestSamplingPassthrough 钉死采样参数逐字段映射。
func TestEncodeRequestSamplingPassthrough(t *testing.T) {
	temp, topP := 0.7, 0.95
	seed := int64(42)
	n, topLogprobs := 2, 5
	presence := 1.5
	logprobs := true

	canon := textCanon()
	canon.Temperature = &temp
	canon.TopP = &topP
	canon.Seed = &seed

	p := encodeParams(t, canon, ChatSampling{
		N:               &n,
		PresencePenalty: &presence,
		Logprobs:        &logprobs,
		TopLogprobs:     &topLogprobs,
	}, DoorTextGeneration)

	want := map[string]any{
		"temperature":      0.7,
		"top_p":            0.95,
		"seed":             float64(42),
		"n":                float64(2),
		"presence_penalty": 1.5,
		"logprobs":         true,
		"top_logprobs":     float64(5),
	}
	for k, v := range want {
		if got, ok := p[k]; !ok || got != v {
			t.Errorf("%s = %v（存在=%v），期望 %v", k, got, ok, v)
		}
	}
}

// TestEncodeRequestExplicitZeroSurvives 钉死显式零值不被当成未设置。
//
// temperature=0 是合法取值（要求确定性输出），抹掉它请求照样成功，
// 但模型换回了默认温度——输出变随机，没有任何错误可看。
func TestEncodeRequestExplicitZeroSurvives(t *testing.T) {
	zero := 0.0
	zeroInt := 0
	no := false

	canon := textCanon()
	canon.Temperature = &zero
	canon.TopP = &zero
	p := encodeParams(t, canon, ChatSampling{
		PresencePenalty: &zero,
		N:               &zeroInt,
		Logprobs:        &no,
		TopLogprobs:     &zeroInt,
	}, DoorTextGeneration)

	for _, k := range []string{"temperature", "top_p", "presence_penalty", "n", "logprobs", "top_logprobs"} {
		if _, ok := p[k]; !ok {
			t.Errorf("%s 的显式零值应保留，实际字段缺失", k)
		}
	}
	if p["temperature"] != 0.0 || p["n"] != float64(0) || p["logprobs"] != false {
		t.Errorf("显式零值被改写：temperature=%v n=%v logprobs=%v",
			p["temperature"], p["n"], p["logprobs"])
	}
}

// TestEncodeRequestOmitsUnsubmittedSampling 钉死未提交的采样参数一个都不发。
func TestEncodeRequestOmitsUnsubmittedSampling(t *testing.T) {
	p := encodeParams(t, textCanon(), ChatSampling{}, DoorTextGeneration)
	for _, k := range []string{
		"temperature", "top_p", "seed", "n",
		"presence_penalty", "logprobs", "top_logprobs",
	} {
		if _, ok := p[k]; ok {
			t.Errorf("未提交的 %s 不得出现在出站体里", k)
		}
	}
}

// TestEncodeRequestStopShape 钉死 stop 的两种形态。
// 单元素发字符串、多元素发数组，是 Native 侧的线格式事实。
func TestEncodeRequestStopShape(t *testing.T) {
	cases := []struct {
		name string
		stop []string
		want string
	}{
		{"单个发字符串", []string{"END"}, `"END"`},
		{"多个发数组", []string{"END", "STOP"}, `["END","STOP"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			canon := textCanon()
			canon.StopSequences = tc.stop
			body, err := EncodeRequest(canon, ChatSampling{}, DoorTextGeneration, "qwen-plus")
			if err != nil {
				t.Fatal(err)
			}
			var env struct {
				Parameters struct {
					Stop json.RawMessage `json:"stop"`
				} `json:"parameters"`
			}
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatal(err)
			}
			if string(env.Parameters.Stop) != tc.want {
				t.Errorf("stop = %s，期望 %s", env.Parameters.Stop, tc.want)
			}
		})
	}

	t.Run("空列表不发", func(t *testing.T) {
		p := encodeParams(t, textCanon(), ChatSampling{}, DoorTextGeneration)
		if _, ok := p["stop"]; ok {
			t.Error("未提交停止词不得发 stop")
		}
	})
}

// TestEncodeRequestWebSearchIsSwitchOnly 钉死联网搜索只保留开关。
func TestEncodeRequestWebSearchIsSwitchOnly(t *testing.T) {
	p := encodeParams(t, textCanon(), ChatSampling{WebSearch: true}, DoorTextGeneration)
	if got, ok := p["enable_search"]; !ok || got != true {
		t.Errorf("enable_search 应为 true，实际 %v（存在=%v）", got, ok)
	}

	off := encodeParams(t, textCanon(), ChatSampling{}, DoorTextGeneration)
	if _, ok := off["enable_search"]; ok {
		t.Error("未请求搜索不得发 enable_search")
	}
}

// TestEncodeRequestIncrementalOutputOnlyWhenStreaming 钉死增量输出开关。
//
// 流式必须显式开：不开的话 Native 每帧回全量文本，Chat 侧按 delta 逐帧
// 拼接就会把内容重复放大成 O(n²)。非流式则一律不发。
func TestEncodeRequestIncrementalOutputOnlyWhenStreaming(t *testing.T) {
	on := encodeParams(t, textCanon(), ChatSampling{IncrementalOutput: true}, DoorTextGeneration)
	if got, ok := on["incremental_output"]; !ok || got != true {
		t.Errorf("流式应发 incremental_output:true，实际 %v（存在=%v）", got, ok)
	}

	off := encodeParams(t, textCanon(), ChatSampling{}, DoorTextGeneration)
	if _, ok := off["incremental_output"]; ok {
		t.Error("非流式不得发 incremental_output")
	}
}

// TestEncodeRequestFailsClosed 钉死编码器在输入不可信时一律报错。
//
// 这些都是网关自身的装配错误（门没配、模型没解析出来、Part 判别字段与负载
// 对不上）。放行的话会发出一个语义残缺却语法合法的请求，上游返回 200，
// 缺掉的那一半没人看得见。
func TestEncodeRequestFailsClosed(t *testing.T) {
	t.Run("Canonical 为 nil", func(t *testing.T) {
		if _, err := EncodeRequest(nil, ChatSampling{}, DoorTextGeneration, "qwen-plus"); err == nil {
			t.Fatal("nil 请求应报错")
		}
	})

	t.Run("未知门", func(t *testing.T) {
		_, err := EncodeRequest(textCanon(), ChatSampling{}, Door("embedding"), "qwen-plus")
		if err == nil {
			t.Fatal("未知门应报错，不得猜一个路径发出去")
		}
		if cerr := canonical.AsError(err); cerr.Class != canonical.ClassInternal {
			t.Errorf("门配错是网关装配问题，应为 internal，实际 %s", cerr.Class)
		}
	})

	t.Run("缺上游模型名", func(t *testing.T) {
		if _, err := EncodeRequest(textCanon(), ChatSampling{}, DoorTextGeneration, ""); err == nil {
			t.Fatal("缺上游模型名应报错")
		}
	})

	// 下面两条走 assistant / tool 分支：编码器在那里直接取 ToolCall 与
	// ToolResult 的字段，判别字段与负载对不上就会读到 nil 指针。
	t.Run("assistant 的 tool_call 判别字段与负载不符", func(t *testing.T) {
		canon := &canonical.Request{
			Model: "logical",
			Messages: []canonical.Message{{
				Role:  canonical.RoleAssistant,
				Parts: []canonical.Part{{Kind: canonical.PartToolCall}},
			}},
		}
		if _, err := EncodeRequest(canon, ChatSampling{}, DoorTextGeneration, "qwen-plus"); err == nil {
			t.Fatal("判别字段与负载不符应报错，不得读到 nil 负载后静默编出空调用")
		}
	})

	t.Run("tool 消息携带非 tool_result 块", func(t *testing.T) {
		canon := &canonical.Request{
			Model: "logical",
			Messages: []canonical.Message{{
				Role:  canonical.RoleTool,
				Parts: []canonical.Part{canonical.Text("不该出现在这里")},
			}},
		}
		if _, err := EncodeRequest(canon, ChatSampling{}, DoorTextGeneration, "qwen-plus"); err == nil {
			t.Fatal("tool 消息只能携带 tool_result，应报错")
		}
	})

	t.Run("媒体同时给了两种承载形态", func(t *testing.T) {
		canon := &canonical.Request{
			Model: "logical",
			Messages: []canonical.Message{{
				Role: canonical.RoleUser,
				Parts: []canonical.Part{{
					Kind: canonical.PartMedia,
					Media: &canonical.Media{
						Kind: canonical.MediaImage,
						URL:  "https://example.com/a.png",
						Data: []byte{0x89},
					},
				}},
			}},
		}
		if _, err := EncodeRequest(canon, ChatSampling{}, DoorMultimodalGeneration, "qwen3-vl-plus"); err == nil {
			t.Fatal("URL 与内联字节互斥，应报错")
		}
	})
}

// TestEncodeRequestIgnoresExtensions 钉死异构编码不读 Extensions。
//
// Extensions 只服务同源快通道的原样回填。异构路径从里面猜语义，等于绕开
// 降级矩阵——损失既不会被登记，也不会被拒绝。
func TestEncodeRequestIgnoresExtensions(t *testing.T) {
	canon := textCanon()
	canon.Extensions.Set(canonical.ExtOpenAI,
		json.RawMessage(`{"parallel_tool_calls":false,"frequency_penalty":1.5}`))

	p := encodeParams(t, canon, ChatSampling{}, DoorTextGeneration)
	if p["parallel_tool_calls"] != true {
		t.Errorf("Extensions 里的值不得覆盖缺省注入，实际 %v", p["parallel_tool_calls"])
	}
	if _, ok := p["frequency_penalty"]; ok {
		t.Error("不得从 Extensions 搬运字段到出站体")
	}
}
