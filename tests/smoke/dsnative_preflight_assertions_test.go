//go:build smoke

package smoke_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// preflightAssertion 是一个用例的预检断言：假上游该给几个候选，以及出站体
// 必须满足什么。
type preflightAssertion struct {
	candidates int
	assert     func(t *testing.T, env outboundEnvelope, msgs []outboundMessage)
}

// preflightAssertions 逐用例给出出站体的本质断言。
//
// 只断言「这项能力确实被搬过去了」，不逐字节比对整个信封：后者会把一次无关的
// 字段顺序调整变成 12 处失败，而真正的映射漂移反而淹没在噪声里。
//
// 表里没有 audio_input：它退出了本期举证名单，TestPreflightCaseBodiesMapToNativeUpstream
// 会把本表与 recordCases 双向对账，多一条同样是失败。生产音频编码的覆盖由协议层
// 单测守住（internal/protocol/dashscopenative/encode_request_test.go 的 audio 用例），
// 不随本条目删除而消失。
func preflightAssertions() map[string]preflightAssertion {
	oneCandidate := 1
	twoCandidates := 2

	return map[string]preflightAssertion{
		"basic": {candidates: oneCandidate, assert: func(t *testing.T, env outboundEnvelope, msgs []outboundMessage) {
			// system 必须被还原成首条 system 消息：Native 没有顶层 system 字段，
			// 丢了它模型就换了人格。
			if msgs[0].Role != "system" {
				t.Errorf("首条消息角色 = %q，期望 system", msgs[0].Role)
			}
			if len(msgs) != 2 || msgs[1].Role != "user" {
				t.Fatalf("消息序列不是 system+user: %+v", msgs)
			}
			if got := contentString(t, msgs[1]); !strings.Contains(got, "杭州") {
				t.Errorf("user content = %q，期望包含原文", got)
			}
			// 缺省注入：OpenAI 默认并行，Native 默认串行，不注入就静默退化。
			wantParam(t, env.Parameters, "parallel_tool_calls", true)
		}},

		"streaming": {candidates: oneCandidate, assert: func(t *testing.T, env outboundEnvelope, msgs []outboundMessage) {
			if len(msgs) != 1 || msgs[0].Role != "user" {
				t.Fatalf("消息序列不是单条 user: %+v", msgs)
			}
			// stream_options 只服务网关自身的 usage chunk 决策，不进出站体。
			wantNoParam(t, env.Parameters, "stream_options")
		}},

		"tool_calling": {candidates: oneCandidate, assert: func(t *testing.T, env outboundEnvelope, msgs []outboundMessage) {
			tools, ok := env.Parameters["tools"].([]any)
			if !ok || len(tools) != 1 {
				t.Fatalf("出站 parameters.tools 不是单元素数组: %v", env.Parameters["tools"])
			}
			assertToolFunction(t, tools[0], "get_weather")
			wantParam(t, env.Parameters, "tool_choice", "auto")

			// 历史与结果都必须在：少一段，上游看到的就是一次全新的首轮调用。
			var assistant, tool *outboundMessage
			for i := range msgs {
				switch msgs[i].Role {
				case "assistant":
					assistant = &msgs[i]
				case "tool":
					tool = &msgs[i]
				}
			}
			if assistant == nil {
				t.Fatal("出站消息缺少 assistant 历史")
			}
			if len(assistant.ToolCalls) == 0 {
				t.Error("assistant 历史缺少 tool_calls")
			} else {
				assertHistoryToolCall(t, assistant.ToolCalls, "call_smoke_0001", "get_weather")
			}
			if tool == nil {
				t.Fatal("出站消息缺少 tool 结果")
			}
			if tool.ToolCallID != "call_smoke_0001" {
				t.Errorf("tool 结果的 tool_call_id = %q，期望 call_smoke_0001", tool.ToolCallID)
			}
			if got := contentString(t, *tool); !strings.Contains(got, "多云") {
				t.Errorf("tool 结果 content = %q，期望保留结果原文", got)
			}
		}},

		"vision_input": {candidates: oneCandidate, assert: func(t *testing.T, env outboundEnvelope, msgs []outboundMessage) {
			blocks := contentBlocks(t, msgs[0])
			if len(blocks) != 3 {
				t.Fatalf("多模态 content 块数 = %d，期望 3（文本 + URL + Data URI）: %v", len(blocks), blocks)
			}
			if _, ok := blocks[0]["text"]; !ok {
				t.Errorf("首块不是 text: %v", blocks[0])
			}
			// URL 原样透传：网关不代下载（原则 2.6），本预检也不发生任何远端抓取。
			if got := blocks[1]["image"]; got != preflightImageURL {
				t.Errorf("第二块 image = %q，期望 URL 原样透传的 %q", got, preflightImageURL)
			}
			if got := blocks[2]["image"]; got != tinyPNGDataURI {
				t.Errorf("第三块 image = %q，期望内联字节重编成同一个 data URI", got)
			}
		}},

		"reasoning": {candidates: oneCandidate, assert: func(t *testing.T, env outboundEnvelope, msgs []outboundMessage) {
			// 编码器契约：none 走 enable_thinking:false，其余档位走 reasoning_effort。
			// 两个字段互斥，同时发是自相矛盾的请求。
			wantParam(t, env.Parameters, "reasoning_effort", "low")
			wantNoParam(t, env.Parameters, "enable_thinking")
		}},

		"parallel_tool_calls": {candidates: oneCandidate, assert: func(t *testing.T, env outboundEnvelope, msgs []outboundMessage) {
			tools, ok := env.Parameters["tools"].([]any)
			if !ok || len(tools) != 2 {
				t.Fatalf("出站 parameters.tools 不是双元素数组: %v", env.Parameters["tools"])
			}
			// 显式 true 原样保留，不与缺省注入混为一谈。
			wantParam(t, env.Parameters, "parallel_tool_calls", true)
		}},

		"structured_output": {candidates: oneCandidate, assert: func(t *testing.T, env outboundEnvelope, msgs []outboundMessage) {
			format, ok := env.Parameters["response_format"].(map[string]any)
			if !ok {
				t.Fatalf("出站 parameters.response_format 不是对象: %v", env.Parameters["response_format"])
			}
			if format["type"] != "json_schema" {
				t.Errorf("response_format.type = %v，期望 json_schema", format["type"])
			}
			schema, ok := format["json_schema"].(map[string]any)
			if !ok {
				t.Fatalf("response_format.json_schema 不是对象: %v", format["json_schema"])
			}
			if schema["name"] != "city_brief" {
				t.Errorf("json_schema.name = %v，期望 city_brief", schema["name"])
			}
			// strict 显式为 true 才算搬到位：抹成默认等于把校验悄悄关掉。
			if schema["strict"] != true {
				t.Errorf("json_schema.strict = %v，期望 true", schema["strict"])
			}
			if _, ok := schema["schema"].(map[string]any); !ok {
				t.Errorf("json_schema.schema 丢失: %v", schema["schema"])
			}
		}},

		"web_search": {candidates: oneCandidate, assert: func(t *testing.T, env outboundEnvelope, msgs []outboundMessage) {
			// 开关搬过去，参数留在原地——这份损失登记在降级矩阵，
			// 不在出站体里假装还在。
			wantParam(t, env.Parameters, "enable_search", true)
			wantNoParam(t, env.Parameters, "search_context_size")
			wantNoParam(t, env.Parameters, "web_search_options")
		}},

		"combined": {candidates: oneCandidate, assert: func(t *testing.T, env outboundEnvelope, msgs []outboundMessage) {
			// 四族必须同时出现在同一个出站体里，这正是本用例要证明的事。
			blocks := contentBlocks(t, msgs[0])
			var hasImage bool
			for _, b := range blocks {
				if b["image"] == tinyPNGDataURI {
					hasImage = true
				}
			}
			if !hasImage {
				t.Errorf("combined 出站体缺少图像块: %v", blocks)
			}
			if tools, ok := env.Parameters["tools"].([]any); !ok || len(tools) != 1 {
				t.Errorf("combined 出站体缺少 tools: %v", env.Parameters["tools"])
			}
			wantParam(t, env.Parameters, "enable_search", true)
			if format, ok := env.Parameters["response_format"].(map[string]any); !ok ||
				format["type"] != "json_schema" {
				t.Errorf("combined 出站体缺少 json_schema response_format: %v",
					env.Parameters["response_format"])
			}
		}},

		"multi_candidate_nonstream": {candidates: twoCandidates, assert: func(t *testing.T, env outboundEnvelope, msgs []outboundMessage) {
			wantParam(t, env.Parameters, "n", 2)
			// 带 tools 或 reasoning 时 Native 会把 n 压回 1，本用例必须都不带。
			wantNoParam(t, env.Parameters, "tools")
			wantNoParam(t, env.Parameters, "reasoning_effort")
		}},

		"multi_candidate_stream": {candidates: twoCandidates, assert: func(t *testing.T, env outboundEnvelope, msgs []outboundMessage) {
			wantParam(t, env.Parameters, "n", 2)
			wantNoParam(t, env.Parameters, "tools")
			wantNoParam(t, env.Parameters, "reasoning_effort")
		}},

		"parallel_tool_calls_default": {candidates: oneCandidate, assert: func(t *testing.T, env outboundEnvelope, msgs []outboundMessage) {
			if tools, ok := env.Parameters["tools"].([]any); !ok || len(tools) != 1 {
				t.Fatalf("出站 parameters.tools 不是单元素数组: %v", env.Parameters["tools"])
			}
			// 客户端没提交时必须注入 true，否则并行调用静默退化成串行。
			wantParam(t, env.Parameters, "parallel_tool_calls", true)
		}},
	}
}

// assertToolFunction 断言一条出站工具声明的类型与函数名。
func assertToolFunction(t *testing.T, tool any, wantName string) {
	t.Helper()

	obj, ok := tool.(map[string]any)
	if !ok {
		t.Fatalf("工具声明不是对象: %v", tool)
	}
	if obj["type"] != "function" {
		t.Errorf("工具 type = %v，期望 function", obj["type"])
	}
	fn, ok := obj["function"].(map[string]any)
	if !ok {
		t.Fatalf("工具缺少 function: %v", obj)
	}
	if fn["name"] != wantName {
		t.Errorf("工具函数名 = %v，期望 %q", fn["name"], wantName)
	}
	// Schema 原样透传：重新编解码一轮会丢掉上游可能识别的扩展关键字。
	params, ok := fn["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("工具缺少 parameters schema: %v", fn)
	}
	if _, ok := params["properties"].(map[string]any); !ok {
		t.Errorf("工具 schema 缺少 properties: %v", params)
	}
}

// assertHistoryToolCall 断言 assistant 历史里的工具调用被完整搬运。
func assertHistoryToolCall(t *testing.T, raw json.RawMessage, wantID, wantName string) {
	t.Helper()

	var calls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
			// Arguments 必须是 JSON **字符串**：发成对象上游读不出参数，
			// 整轮调用被静默丢掉。用 string 收，形态不符时这里就解不出来。
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &calls); err != nil {
		t.Fatalf("assistant.tool_calls 无法解析: %v (%s)", err, raw)
	}
	if len(calls) != 1 {
		t.Fatalf("assistant.tool_calls 数量 = %d，期望 1", len(calls))
	}
	if calls[0].ID != wantID {
		t.Errorf("历史调用 id = %q，期望 %q", calls[0].ID, wantID)
	}
	if calls[0].Type != "function" {
		t.Errorf("历史调用 type = %q，期望 function", calls[0].Type)
	}
	if calls[0].Function.Name != wantName {
		t.Errorf("历史调用函数名 = %q，期望 %q", calls[0].Function.Name, wantName)
	}
	if !json.Valid([]byte(calls[0].Function.Arguments)) {
		t.Errorf("历史调用 arguments 不是合法 JSON 字符串: %q", calls[0].Function.Arguments)
	}
	if !strings.Contains(calls[0].Function.Arguments, "杭州") {
		t.Errorf("历史调用 arguments = %q，期望保留原参数", calls[0].Function.Arguments)
	}
}
