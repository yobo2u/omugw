package gateway

import (
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/transport/sse"
)

// TestExtractChatUsage 固化 Chat 的用量口径：prompt_tokens / completion_tokens，
// 缓存读取与推理明细挂在各自的 *_details 下。
func TestExtractChatUsage(t *testing.T) {
	body := []byte(`{"id":"chatcmpl_1","usage":{
	  "prompt_tokens":24,"completion_tokens":11,"total_tokens":35,
	  "prompt_tokens_details":{"cached_tokens":4},
	  "completion_tokens_details":{"reasoning_tokens":3}}}`)

	u := extractChatUsage(body)
	if u.Fidelity != canonical.FidelityAuthoritative {
		t.Errorf("Fidelity = %v, 期望 authoritative", u.Fidelity)
	}
	if u.InputTokens != 24 || u.OutputTokens != 11 {
		t.Errorf("tokens = %d/%d, 期望 24/11", u.InputTokens, u.OutputTokens)
	}
	if u.CacheReadInputTokens != 4 {
		t.Errorf("CacheReadInputTokens = %d, 期望 4", u.CacheReadInputTokens)
	}
	if u.ReasoningTokens != 3 {
		t.Errorf("ReasoningTokens = %d, 期望 3", u.ReasoningTokens)
	}
}

// TestExtractChatUsageUnavailable 固化「解不出来就是不可知」，不得编 0。
func TestExtractChatUsageUnavailable(t *testing.T) {
	if u := extractChatUsage([]byte(`{"id":"x"}`)); u.Fidelity != canonical.FidelityUnavailable {
		t.Errorf("缺少 usage 应为 unavailable, 实际 %v", u.Fidelity)
	}
}

// TestParseChatUsageEvent 固化流式用量只从最后一个带 usage 的 chunk 取。
func TestParseChatUsageEvent(t *testing.T) {
	final := sse.Event{Data: `{"id":"c","object":"chat.completion.chunk","choices":[],
	  "usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`}
	u, ok := parseChatUsageEvent(final)
	if !ok {
		t.Fatal("最后一个 chunk 的 usage 应当被取出")
	}
	if u.InputTokens != 7 || u.OutputTokens != 2 {
		t.Errorf("tokens = %d/%d, 期望 7/2", u.InputTokens, u.OutputTokens)
	}

	// 中间分片 usage 为 null。
	mid := sse.Event{Data: `{"id":"c","choices":[{"delta":{"content":"你"}}],"usage":null}`}
	if _, ok := parseChatUsageEvent(mid); ok {
		t.Error("usage 为 null 的中间分片不应产出用量")
	}

	// [DONE] 哨兵。
	if _, ok := parseChatUsageEvent(sse.Event{Data: "[DONE]"}); ok {
		t.Error("[DONE] 不应产出用量")
	}
}

// TestExtractResponsesUsageStillWorks 保证泛化没有改坏 Responses 的口径。
func TestExtractResponsesUsageStillWorks(t *testing.T) {
	body := []byte(`{"id":"resp_1","usage":{"input_tokens":10,"output_tokens":5,
	  "output_tokens_details":{"reasoning_tokens":3}}}`)
	u := extractUsage(body)
	if u.InputTokens != 10 || u.OutputTokens != 5 || u.ReasoningTokens != 3 {
		t.Errorf("Responses 用量解析被改坏: %+v", u)
	}
}
