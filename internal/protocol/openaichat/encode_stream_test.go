package openaichat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// TestEncodeChunkDelta 钉死 chunk 的 object 与 delta 形态。
func TestEncodeChunkDelta(t *testing.T) {
	body, err := EncodeChunk(ChunkInput{
		ID: "req-1", Model: "qwen-plus", Created: 1755216000,
		Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{Content: "你"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Object  string `json:"object"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "chat.completion.chunk" {
		t.Errorf("object 应为 chat.completion.chunk，实际 %q", got.Object)
	}
	if got.Choices[0].Delta.Content != "你" {
		t.Errorf("delta.content 错误: %+v", got.Choices[0])
	}
	if got.Choices[0].FinishReason != nil {
		t.Errorf("生成中 finish_reason 应为 null，实际 %v", *got.Choices[0].FinishReason)
	}
}

// TestEncodeChunkToolArgsFragment 钉死工具 arguments 片段原样透传、不提前解析。
func TestEncodeChunkToolArgsFragment(t *testing.T) {
	body, err := EncodeChunk(ChunkInput{
		ID: "r", Model: "m", Created: 1,
		Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{ToolCalls: []ToolCallDelta{
			{Index: 0, Arguments: `{"loc`}, // 不闭合片段
		}}}},
	})
	if err != nil {
		t.Fatalf("不闭合的工具参数片段必须能编码: %v", err)
	}
	if !strings.Contains(string(body), `{\"loc`) {
		t.Errorf("arguments 片段应原样透传: %s", body)
	}
}

// TestEncodeChunkRoleOmission 钉死 role 的省略规则。
func TestEncodeChunkRoleOmission(t *testing.T) {
	// 首帧带 role
	body1, err := EncodeChunk(ChunkInput{
		Choices: []ChunkChoice{{Delta: ChunkDelta{Role: "assistant"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body1), `"role":"assistant"`) {
		t.Errorf("首帧应包含 role: %s", body1)
	}

	// 后续帧省略 role
	body2, err := EncodeChunk(ChunkInput{
		Choices: []ChunkChoice{{Delta: ChunkDelta{Content: "好"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body2), `"role"`) {
		t.Errorf("后续帧应省略 role: %s", body2)
	}
}

// TestEncodeChunkMultipleChoices 钉死多候选的顺序。
func TestEncodeChunkMultipleChoices(t *testing.T) {
	body, err := EncodeChunk(ChunkInput{
		Choices: []ChunkChoice{
			{Index: 0, Delta: ChunkDelta{Content: "A"}},
			{Index: 1, Delta: ChunkDelta{Content: "B"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got respChunk
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Choices) != 2 || got.Choices[0].Index != 0 || got.Choices[1].Index != 1 {
		t.Errorf("多候选顺序错误: %+v", got.Choices)
	}
}

// TestEncodeChunkFinalUsage 钉死最终 usage chunk 的形态。
func TestEncodeChunkFinalUsage(t *testing.T) {
	body, err := EncodeChunk(ChunkInput{
		Choices: []ChunkChoice{},
		Usage: &canonical.Usage{
			Fidelity:     canonical.FidelityAuthoritative,
			InputTokens:  10,
			OutputTokens: 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"choices":[]`) {
		t.Errorf("空 choices 应序列化为 []，实际: %s", body)
	}
	if !strings.Contains(string(body), `"usage":{"prompt_tokens":10`) {
		t.Errorf("应包含 usage: %s", body)
	}
}

// TestEncodeChunkFinishReason 钉死 finish_reason 的输出。
func TestEncodeChunkFinishReason(t *testing.T) {
	body, err := EncodeChunk(ChunkInput{
		Choices: []ChunkChoice{{FinishReason: "stop"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"finish_reason":"stop"`) {
		t.Errorf("应包含 finish_reason: %s", body)
	}
}

// TestEncodeChunkToolCallOmission 钉死 tool_call id/name 的省略规则。
func TestEncodeChunkToolCallOmission(t *testing.T) {
	// 首帧带 id/name
	body1, err := EncodeChunk(ChunkInput{
		Choices: []ChunkChoice{{Delta: ChunkDelta{ToolCalls: []ToolCallDelta{
			{ID: "call_1", Name: "func_1"},
		}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got1 respChunk
	if err := json.Unmarshal(body1, &got1); err != nil {
		t.Fatal(err)
	}
	if len(got1.Choices) == 0 || len(got1.Choices[0].Delta.ToolCalls) == 0 {
		t.Fatal("首帧解析失败")
	}
	tc1 := got1.Choices[0].Delta.ToolCalls[0]
	if tc1.ID != "call_1" || tc1.Type != "function" || tc1.Function == nil || tc1.Function.Name != "func_1" {
		t.Errorf("首帧应包含 id, type 和 name: %+v", tc1)
	}

	// 后续帧省略 id/name
	body2, err := EncodeChunk(ChunkInput{
		Choices: []ChunkChoice{{Delta: ChunkDelta{ToolCalls: []ToolCallDelta{
			{Arguments: "123"},
		}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got2 respChunk
	if err := json.Unmarshal(body2, &got2); err != nil {
		t.Fatal(err)
	}
	if len(got2.Choices) == 0 || len(got2.Choices[0].Delta.ToolCalls) == 0 {
		t.Fatal("后续帧解析失败")
	}
	tc2 := got2.Choices[0].Delta.ToolCalls[0]
	// 结构化断言：后续帧应省略 id 和 name，且保留 arguments
	if tc2.ID != "" || tc2.Type != "" || tc2.Function == nil || tc2.Function.Name != "" || tc2.Function.Arguments != "123" {
		t.Errorf("后续帧应省略 id 和 name，并保留 arguments: %+v", tc2)
	}
}

// TestEncodeChunkReasoningContent 钉死 reasoning_content 的输出。
func TestEncodeChunkReasoningContent(t *testing.T) {
	body, err := EncodeChunk(ChunkInput{
		Choices: []ChunkChoice{{Delta: ChunkDelta{ReasoningContent: "思考中"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"reasoning_content":"思考中"`) {
		t.Errorf("应包含 reasoning_content: %s", body)
	}
}

// TestDoneSentinel 钉死流结束哨兵。
func TestDoneSentinel(t *testing.T) {
	if DoneSentinel != "[DONE]" {
		t.Errorf("DoneSentinel 错误: %q", DoneSentinel)
	}
}
