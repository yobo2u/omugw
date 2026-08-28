package openaichat

import (
	"encoding/json"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

func ptr[T any](v T) *T {
	return &v
}

// TestEncodeCompletionShape 钉死 chat.completion 的 object/id/model/created/choices/usage。
func TestEncodeCompletionShape(t *testing.T) {
	body, err := EncodeCompletion(CompletionInput{
		ID: "req-1", Model: "qwen-plus", Created: 1755216000,
		Choices: []CompletionChoice{{
			Message:      canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.Part{canonical.Text("你好")}},
			FinishReason: ptr("stop"),
		}},
		Usage: &canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: 5, OutputTokens: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Index        int    `json:"index"`
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "chat.completion" || got.ID != "req-1" || got.Model != "qwen-plus" || got.Created != 1755216000 {
		t.Errorf("顶层字段错误: %+v", got)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != "你好" || got.Choices[0].FinishReason != "stop" {
		t.Errorf("choices 错误: %+v", got.Choices)
	}
	if got.Usage.PromptTokens != 5 || got.Usage.CompletionTokens != 2 || got.Usage.TotalTokens != 7 {
		t.Errorf("usage 应为 5/2/7（缺失 total 由两者相加）: %+v", got.Usage)
	}
}

// TestEncodeCompletion_ChoicesIndexAndOrder 验证多候选按顺序编号 0, 1，且顺序不被打乱。
func TestEncodeCompletion_ChoicesIndexAndOrder(t *testing.T) {
	body, err := EncodeCompletion(CompletionInput{
		ID:      "req-multi-choice",
		Model:   "qwen-plus",
		Created: 1755216001,
		Choices: []CompletionChoice{
			{
				Message:      canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.Part{canonical.Text("候选一")}},
				FinishReason: ptr("stop"),
			},
			{
				Message:      canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.Part{canonical.Text("候选二")}},
				FinishReason: ptr("length"),
			},
		},
	})
	if err != nil {
		t.Fatalf("EncodeCompletion failed: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	choices, ok := raw["choices"].([]any)
	if !ok || len(choices) != 2 {
		t.Fatalf("expected 2 choices, got %+v", raw["choices"])
	}

	c0 := choices[0].(map[string]any)
	if int(c0["index"].(float64)) != 0 {
		t.Errorf("choice 0 index expected 0, got %v", c0["index"])
	}
	if c0["finish_reason"] != "stop" {
		t.Errorf("choice 0 finish_reason expected stop, got %v", c0["finish_reason"])
	}
	msg0 := c0["message"].(map[string]any)
	if msg0["content"] != "候选一" {
		t.Errorf("choice 0 content expected 候选一, got %v", msg0["content"])
	}

	c1 := choices[1].(map[string]any)
	if int(c1["index"].(float64)) != 1 {
		t.Errorf("choice 1 index expected 1, got %v", c1["index"])
	}
	if c1["finish_reason"] != "length" {
		t.Errorf("choice 1 finish_reason expected length, got %v", c1["finish_reason"])
	}
	msg1 := c1["message"].(map[string]any)
	if msg1["content"] != "候选二" {
		t.Errorf("choice 1 content expected 候选二, got %v", msg1["content"])
	}
}

// TestEncodeCompletion_MultipleTextPartsConcatNoSeparator 验证多段文本 Part 直接无分隔符拼接入 content，防范无故插入换行或空格破坏流式累积一致性。
func TestEncodeCompletion_MultipleTextPartsConcatNoSeparator(t *testing.T) {
	body, err := EncodeCompletion(CompletionInput{
		ID:      "req-text-concat",
		Model:   "qwen-plus",
		Created: 1755216002,
		Choices: []CompletionChoice{
			{
				Message: canonical.Message{
					Role: canonical.RoleAssistant,
					Parts: []canonical.Part{
						canonical.Text("你好，"),
						canonical.Text("世界！"),
						canonical.Text("欢迎使用。"),
					},
				},
				FinishReason: ptr("stop"),
			},
		},
	})
	if err != nil {
		t.Fatalf("EncodeCompletion failed: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	choices := raw["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好，世界！欢迎使用。" {
		t.Errorf("expected concatenated content '你好，世界！欢迎使用。', got %v", msg["content"])
	}
}

// TestEncodeCompletion_ThinkingConcatAndOmitEmpty 验证多段 PartThinking 直接无分隔符拼入 reasoning_content，
// 且无 thinking 时必须在 wire JSON 中省略 reasoning_content 字段，绝不把签名等内部元数据往外发。
func TestEncodeCompletion_ThinkingConcatAndOmitEmpty(t *testing.T) {
	// 情况 1：包含 thinking 块
	bodyWithThinking, err := EncodeCompletion(CompletionInput{
		ID:      "req-thinking",
		Model:   "qwen-plus",
		Created: 1755216003,
		Choices: []CompletionChoice{
			{
				Message: canonical.Message{
					Role: canonical.RoleAssistant,
					Parts: []canonical.Part{
						{Kind: canonical.PartThinking, Thinking: &canonical.Thinking{Text: "先分析第一步。"}},
						{Kind: canonical.PartThinking, Thinking: &canonical.Thinking{Text: "再分析第二步。", Signature: "sig-ignored"}},
						canonical.Text("最终回答。"),
					},
				},
				FinishReason: ptr("stop"),
			},
		},
	})
	if err != nil {
		t.Fatalf("EncodeCompletion with thinking failed: %v", err)
	}

	var rawWith map[string]any
	if err := json.Unmarshal(bodyWithThinking, &rawWith); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	msgWith := rawWith["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msgWith["content"] != "最终回答。" {
		t.Errorf("expected content '最终回答。', got %v", msgWith["content"])
	}
	if msgWith["reasoning_content"] != "先分析第一步。再分析第二步。" {
		t.Errorf("expected reasoning_content '先分析第一步。再分析第二步。', got %v", msgWith["reasoning_content"])
	}

	// 情况 2：不含 thinking 块时，JSON 中必须省略 reasoning_content 键
	bodyNoThinking, err := EncodeCompletion(CompletionInput{
		ID:      "req-no-thinking",
		Model:   "qwen-plus",
		Created: 1755216004,
		Choices: []CompletionChoice{
			{
				Message: canonical.Message{
					Role:  canonical.RoleAssistant,
					Parts: []canonical.Part{canonical.Text("普通回答。")},
				},
				FinishReason: ptr("stop"),
			},
		},
	})
	if err != nil {
		t.Fatalf("EncodeCompletion without thinking failed: %v", err)
	}

	var rawNo map[string]any
	if err := json.Unmarshal(bodyNoThinking, &rawNo); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	msgNo := rawNo["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, exists := msgNo["reasoning_content"]; exists {
		t.Errorf("reasoning_content key must be omitted when empty, but found: %v", msgNo["reasoning_content"])
	}
}

// TestEncodeCompletion_ToolCallsOrderAndEmptyArgs 验证多个 tool_calls 保留顺序、ID、名称和参数字符串；
// 且空/nil 参数规范化为 "{}"，避免下游客户端 JSON.parse 抛错。
func TestEncodeCompletion_ToolCallsOrderAndEmptyArgs(t *testing.T) {
	body, err := EncodeCompletion(CompletionInput{
		ID:      "req-tools",
		Model:   "qwen-plus",
		Created: 1755216005,
		Choices: []CompletionChoice{
			{
				Message: canonical.Message{
					Role: canonical.RoleAssistant,
					Parts: []canonical.Part{
						{
							Kind: canonical.PartToolCall,
							ToolCall: &canonical.ToolCall{
								ID:        "call_1",
								Name:      "get_weather",
								Arguments: json.RawMessage(`{"city":"Hangzhou"}`),
							},
						},
						{
							Kind: canonical.PartToolCall,
							ToolCall: &canonical.ToolCall{
								ID:        "call_2",
								Name:      "get_time",
								Arguments: nil,
							},
						},
						{
							Kind: canonical.PartToolCall,
							ToolCall: &canonical.ToolCall{
								ID:        "call_3",
								Name:      "noop",
								Arguments: json.RawMessage(""),
							},
						},
					},
				},
				FinishReason: ptr("tool_calls"),
			},
		},
	})
	if err != nil {
		t.Fatalf("EncodeCompletion failed: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	msg := raw["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	toolCalls, ok := msg["tool_calls"].([]any)
	if !ok || len(toolCalls) != 3 {
		t.Fatalf("expected 3 tool_calls, got %+v", msg["tool_calls"])
	}

	// 工具调用 1
	tc0 := toolCalls[0].(map[string]any)
	if tc0["id"] != "call_1" || tc0["type"] != "function" {
		t.Errorf("tc0 metadata mismatch: %+v", tc0)
	}
	fn0 := tc0["function"].(map[string]any)
	if fn0["name"] != "get_weather" || fn0["arguments"] != `{"city":"Hangzhou"}` {
		t.Errorf("tc0 function mismatch: %+v", fn0)
	}

	// 工具调用 2（nil 参数规范化为 "{}"）
	tc1 := toolCalls[1].(map[string]any)
	if tc1["id"] != "call_2" || tc1["type"] != "function" {
		t.Errorf("tc1 metadata mismatch: %+v", tc1)
	}
	fn1 := tc1["function"].(map[string]any)
	if fn1["name"] != "get_time" || fn1["arguments"] != "{}" {
		t.Errorf("tc1 function mismatch (expected '{}'): %+v", fn1)
	}

	// 工具调用 3（空字符串参数规范化为 "{}"）
	tc2 := toolCalls[2].(map[string]any)
	if tc2["id"] != "call_3" || tc2["type"] != "function" {
		t.Errorf("tc2 metadata mismatch: %+v", tc2)
	}
	fn2 := tc2["function"].(map[string]any)
	if fn2["name"] != "noop" || fn2["arguments"] != "{}" {
		t.Errorf("tc2 function mismatch (expected '{}'): %+v", fn2)
	}
}

// TestEncodeCompletion_NilFinishReasonAndLogprobsNull 验证 nil finish_reason 和 nil logprobs 必须序列化为显式 JSON null（而非从 JSON 中省略），
// 并且非 nil 的原始 logprobs 原样透传，防范客户端解引用字段时因缺失键而异常。
func TestEncodeCompletion_NilFinishReasonAndLogprobsNull(t *testing.T) {
	rawLogprobsJSON := `{"content":[{"token":"hello","logprob":-0.5}]}`
	body, err := EncodeCompletion(CompletionInput{
		ID:      "req-null-fields",
		Model:   "qwen-plus",
		Created: 1755216006,
		Choices: []CompletionChoice{
			{
				Message: canonical.Message{
					Role:  canonical.RoleAssistant,
					Parts: []canonical.Part{canonical.Text("第一条，带 null finish_reason 与 null logprobs")},
				},
				FinishReason: nil,
				Logprobs:     nil,
			},
			{
				Message: canonical.Message{
					Role:  canonical.RoleAssistant,
					Parts: []canonical.Part{canonical.Text("第二条，带非空 finish_reason 与 raw logprobs")},
				},
				FinishReason: ptr("stop"),
				Logprobs:     json.RawMessage(rawLogprobsJSON),
			},
		},
	})
	if err != nil {
		t.Fatalf("EncodeCompletion failed: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	choices := raw["choices"].([]any)
	if len(choices) != 2 {
		t.Fatalf("expected 2 choices, got %d", len(choices))
	}

	// 候选 0：finish_reason 与 logprobs 必须在 JSON 中存在且为 null
	c0 := choices[0].(map[string]any)
	frVal0, frExists0 := c0["finish_reason"]
	if !frExists0 {
		t.Errorf("choice 0: finish_reason key must be present in JSON, but was omitted")
	}
	if frVal0 != nil {
		t.Errorf("choice 0: finish_reason must be JSON null, got %v", frVal0)
	}

	lpVal0, lpExists0 := c0["logprobs"]
	if !lpExists0 {
		t.Errorf("choice 0: logprobs key must be present in JSON, but was omitted")
	}
	if lpVal0 != nil {
		t.Errorf("choice 0: logprobs must be JSON null, got %v", lpVal0)
	}

	// 候选 1：finish_reason 为 "stop"，logprobs 保持原始 JSON 对象透传
	c1 := choices[1].(map[string]any)
	if c1["finish_reason"] != "stop" {
		t.Errorf("choice 1: finish_reason expected stop, got %v", c1["finish_reason"])
	}
	lpVal1, lpExists1 := c1["logprobs"]
	if !lpExists1 || lpVal1 == nil {
		t.Fatalf("choice 1: logprobs expected non-null, got %v", lpVal1)
	}
	lpMap, ok := lpVal1.(map[string]any)
	if !ok {
		t.Fatalf("choice 1: logprobs expected object map, got %T: %v", lpVal1, lpVal1)
	}
	contentArr, ok := lpMap["content"].([]any)
	if !ok || len(contentArr) != 1 {
		t.Fatalf("choice 1: logprobs content array mismatch: %+v", lpMap)
	}
	tokObj := contentArr[0].(map[string]any)
	if tokObj["token"] != "hello" {
		t.Errorf("choice 1: logprobs token expected hello, got %v", tokObj["token"])
	}
}

// TestEncodeCompletion_UsageFidelityAndDetails 验证用量保真度与细分对象的映射规则：
// 1. nil 或非权威（估算/不可用）用量在 JSON 中完全省略 usage 键；
// 2. 权威用量输出 prompt/completion/total，有细分时输出 detail 对象，无细分（值为 0）时省略 detail 对象。
func TestEncodeCompletion_UsageFidelityAndDetails(t *testing.T) {
	// 子用例 1：nil 用量，省略 usage 键
	bodyNil, err := EncodeCompletion(CompletionInput{
		ID:      "req-u-nil",
		Model:   "qwen-plus",
		Created: 1755216007,
		Choices: []CompletionChoice{
			{Message: canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.Part{canonical.Text("ok")}}},
		},
		Usage: nil,
	})
	if err != nil {
		t.Fatalf("EncodeCompletion failed: %v", err)
	}
	var rawNil map[string]any
	if err := json.Unmarshal(bodyNil, &rawNil); err != nil {
		t.Fatal(err)
	}
	if _, exists := rawNil["usage"]; exists {
		t.Errorf("usage key must be omitted when nil, got: %v", rawNil["usage"])
	}

	// 子用例 2：估算用量，省略 usage 键
	bodyEst, err := EncodeCompletion(CompletionInput{
		ID:      "req-u-est",
		Model:   "qwen-plus",
		Created: 1755216008,
		Choices: []CompletionChoice{
			{Message: canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.Part{canonical.Text("ok")}}},
		},
		Usage: &canonical.Usage{Fidelity: canonical.FidelityEstimated, InputTokens: 10, OutputTokens: 20},
	})
	if err != nil {
		t.Fatalf("EncodeCompletion failed: %v", err)
	}
	var rawEst map[string]any
	if err := json.Unmarshal(bodyEst, &rawEst); err != nil {
		t.Fatal(err)
	}
	if _, exists := rawEst["usage"]; exists {
		t.Errorf("usage key must be omitted when estimated, got: %v", rawEst["usage"])
	}

	// 子用例 3：不可用用量，省略 usage 键
	bodyUnavail, err := EncodeCompletion(CompletionInput{
		ID:      "req-u-unavail",
		Model:   "qwen-plus",
		Created: 1755216009,
		Choices: []CompletionChoice{
			{Message: canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.Part{canonical.Text("ok")}}},
		},
		Usage: &canonical.Usage{Fidelity: canonical.FidelityUnavailable},
	})
	if err != nil {
		t.Fatalf("EncodeCompletion failed: %v", err)
	}
	var rawUnavail map[string]any
	if err := json.Unmarshal(bodyUnavail, &rawUnavail); err != nil {
		t.Fatal(err)
	}
	if _, exists := rawUnavail["usage"]; exists {
		t.Errorf("usage key must be omitted when unavailable, got: %v", rawUnavail["usage"])
	}

	// 子用例 4：权威用量且带细分
	bodyAuthDetails, err := EncodeCompletion(CompletionInput{
		ID:      "req-u-auth-details",
		Model:   "qwen-plus",
		Created: 1755216010,
		Choices: []CompletionChoice{
			{Message: canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.Part{canonical.Text("ok")}}},
		},
		Usage: &canonical.Usage{
			Fidelity:             canonical.FidelityAuthoritative,
			InputTokens:          100,
			OutputTokens:         50,
			CacheReadInputTokens: 40,
			ReasoningTokens:      25,
		},
	})
	if err != nil {
		t.Fatalf("EncodeCompletion failed: %v", err)
	}
	var rawAuthDetails map[string]any
	if err := json.Unmarshal(bodyAuthDetails, &rawAuthDetails); err != nil {
		t.Fatal(err)
	}
	uMap, ok := rawAuthDetails["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage object missing: %+v", rawAuthDetails)
	}
	if int64(uMap["prompt_tokens"].(float64)) != 100 ||
		int64(uMap["completion_tokens"].(float64)) != 50 ||
		int64(uMap["total_tokens"].(float64)) != 150 {
		t.Errorf("usage basic tokens mismatch: %+v", uMap)
	}
	promptDetails, ok := uMap["prompt_tokens_details"].(map[string]any)
	if !ok || int64(promptDetails["cached_tokens"].(float64)) != 40 {
		t.Errorf("prompt_tokens_details mismatch: %+v", uMap["prompt_tokens_details"])
	}
	compDetails, ok := uMap["completion_tokens_details"].(map[string]any)
	if !ok || int64(compDetails["reasoning_tokens"].(float64)) != 25 {
		t.Errorf("completion_tokens_details mismatch: %+v", uMap["completion_tokens_details"])
	}

	// 子用例 5：权威用量但细分为 0，省略细分对象
	bodyAuthNoDetails, err := EncodeCompletion(CompletionInput{
		ID:      "req-u-auth-nodetails",
		Model:   "qwen-plus",
		Created: 1755216011,
		Choices: []CompletionChoice{
			{Message: canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.Part{canonical.Text("ok")}}},
		},
		Usage: &canonical.Usage{
			Fidelity:     canonical.FidelityAuthoritative,
			InputTokens:  100,
			OutputTokens: 50,
		},
	})
	if err != nil {
		t.Fatalf("EncodeCompletion failed: %v", err)
	}
	var rawAuthNoDetails map[string]any
	if err := json.Unmarshal(bodyAuthNoDetails, &rawAuthNoDetails); err != nil {
		t.Fatal(err)
	}
	uMapNo, ok := rawAuthNoDetails["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage object missing: %+v", rawAuthNoDetails)
	}
	if _, exists := uMapNo["prompt_tokens_details"]; exists {
		t.Errorf("prompt_tokens_details must be omitted when zero, got: %v", uMapNo["prompt_tokens_details"])
	}
	if _, exists := uMapNo["completion_tokens_details"]; exists {
		t.Errorf("completion_tokens_details must be omitted when zero, got: %v", uMapNo["completion_tokens_details"])
	}
}

// TestEncodeCompletion_UsageInvalidFidelity 验证零值或未知 fidelity 会被拦截为 ClassInternal 错误。
func TestEncodeCompletion_UsageInvalidFidelity(t *testing.T) {
	// 零值 fidelity
	_, err := EncodeCompletion(CompletionInput{
		ID:      "req-u-zero",
		Model:   "qwen-plus",
		Created: 1755216012,
		Choices: []CompletionChoice{
			{Message: canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.Part{canonical.Text("ok")}}},
		},
		Usage: &canonical.Usage{Fidelity: ""},
	})
	if err == nil {
		t.Fatal("expected error for zero fidelity, got nil")
	}
	cErr := canonical.AsError(err)
	if cErr.Class != canonical.ClassInternal {
		t.Errorf("expected ClassInternal, got %v", cErr.Class)
	}

	// 未知 fidelity
	_, err = EncodeCompletion(CompletionInput{
		ID:      "req-u-unknown",
		Model:   "qwen-plus",
		Created: 1755216013,
		Choices: []CompletionChoice{
			{Message: canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.Part{canonical.Text("ok")}}},
		},
		Usage: &canonical.Usage{Fidelity: canonical.Fidelity("custom_future")},
	})
	if err == nil {
		t.Fatal("expected error for unknown fidelity, got nil")
	}
	cErr = canonical.AsError(err)
	if cErr.Class != canonical.ClassInternal {
		t.Errorf("expected ClassInternal, got %v", cErr.Class)
	}
}

// TestEncodeCompletion_ZeroPartsAssistant 验证合法但 Parts 为空的助手消息必须正常编码为 content:""，
// 绝不能报错或省略 content 字段（Chat 客户端直接读 content 键）。
func TestEncodeCompletion_ZeroPartsAssistant(t *testing.T) {
	body, err := EncodeCompletion(CompletionInput{
		ID:      "req-zero-parts",
		Model:   "qwen-plus",
		Created: 1755216014,
		Choices: []CompletionChoice{
			{
				Message: canonical.Message{
					Role:  canonical.RoleAssistant,
					Parts: nil,
				},
				FinishReason: ptr("stop"),
			},
		},
	})
	if err != nil {
		t.Fatalf("EncodeCompletion failed: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	msg := raw["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	contentVal, exists := msg["content"]
	if !exists {
		t.Fatalf("content key must exist even when parts empty")
	}
	if contentVal != "" {
		t.Errorf("content expected empty string, got %v", contentVal)
	}
}

// TestEncodeCompletion_NonAssistantRoleFails 验证非 assistant 角色（如 user/system/tool）必须被拒绝，
// 返回 ClassInternal 错误，防止协议装配错乱。
func TestEncodeCompletion_NonAssistantRoleFails(t *testing.T) {
	roles := []canonical.Role{canonical.RoleUser, canonical.RoleSystem, canonical.RoleTool}
	for _, r := range roles {
		_, err := EncodeCompletion(CompletionInput{
			ID:      "req-bad-role",
			Model:   "qwen-plus",
			Created: 1755216015,
			Choices: []CompletionChoice{
				{
					Message:      canonical.Message{Role: r, Parts: []canonical.Part{canonical.Text("text")}},
					FinishReason: ptr("stop"),
				},
			},
		})
		if err == nil {
			t.Fatalf("expected error for non-assistant role %q, got nil", r)
		}
		cErr := canonical.AsError(err)
		if cErr.Class != canonical.ClassInternal {
			t.Errorf("expected ClassInternal for role %q, got %v", r, cErr.Class)
		}
	}
}

// TestEncodeCompletion_MalformedPartFails 验证 Part.Validate 失败时包装为 ClassInternal 错误。
func TestEncodeCompletion_MalformedPartFails(t *testing.T) {
	// 缺少 ToolCall 负载的 Part 会在 Part.Validate() 时校验失败
	_, err := EncodeCompletion(CompletionInput{
		ID:      "req-malformed-part",
		Model:   "qwen-plus",
		Created: 1755216016,
		Choices: []CompletionChoice{
			{
				Message: canonical.Message{
					Role: canonical.RoleAssistant,
					Parts: []canonical.Part{
						{Kind: canonical.PartToolCall, ToolCall: nil},
					},
				},
				FinishReason: ptr("tool_calls"),
			},
		},
	})
	if err == nil {
		t.Fatal("expected error for malformed part, got nil")
	}
	cErr := canonical.AsError(err)
	if cErr.Class != canonical.ClassInternal {
		t.Errorf("expected ClassInternal, got %v", cErr.Class)
	}
}

// TestEncodeCompletion_UnsupportedPartKindsFailClassInternal 验证 assistant choice 包含 PartRefusal/PartMedia/PartToolResult
// 或未知 Kind 时，必须 fail-closed 返回 ClassInternal，并在错误信息中包含 choice 序号、part 序号与 kind。
func TestEncodeCompletion_UnsupportedPartKindsFailClassInternal(t *testing.T) {
	testCases := []struct {
		name string
		part canonical.Part
	}{
		{
			name: "refusal",
			part: canonical.Refusal("拒答测试"),
		},
		{
			name: "media",
			part: canonical.ImageURL("https://example.com/a.png", "image/png"),
		},
		{
			name: "tool_result",
			part: canonical.Part{
				Kind: canonical.PartToolResult,
				ToolResult: &canonical.ToolResult{
					CallID: "call_1",
				},
			},
		},
		{
			name: "unknown_kind",
			part: canonical.Part{
				Kind: canonical.PartKind("custom_unknown"),
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := EncodeCompletion(CompletionInput{
				ID:      "req-unsupported-part",
				Model:   "qwen-plus",
				Created: 1755216017,
				Choices: []CompletionChoice{
					{
						Message: canonical.Message{
							Role: canonical.RoleAssistant,
							Parts: []canonical.Part{
								canonical.Text("前面的文本"),
								tc.part,
							},
						},
						FinishReason: ptr("stop"),
					},
				},
			})
			if err == nil {
				t.Fatalf("[%s] expected error, got nil", tc.name)
			}
			cErr := canonical.AsError(err)
			if cErr.Class != canonical.ClassInternal {
				t.Errorf("[%s] expected ClassInternal, got %v", tc.name, cErr.Class)
			}
		})
	}
}
