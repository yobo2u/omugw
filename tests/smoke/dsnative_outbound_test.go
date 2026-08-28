//go:build smoke

package smoke_test

import (
	"encoding/json"
	"fmt"
	"testing"
)

// outboundEnvelope 是出站 Native 请求体的断言视图。
//
// parameters 收成 map[string]any 而不是具名结构体：这里要区分的是「字段存在
// 但为 false」与「字段根本没发」，而后者恰恰是 parallel_tool_calls 与
// search_context_size 两条断言的判据。具名结构体的零值把两者并成一种。
type outboundEnvelope struct {
	Model string `json:"model"`
	Input struct {
		Messages []json.RawMessage `json:"messages"`
	} `json:"input"`
	Parameters map[string]any `json:"parameters"`
}

// outboundMessage 是出站消息的断言视图。Content 形态按门而异，故收原始字节。
type outboundMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

// decodeOutbound 把捕获到的出站字节解成断言视图。
func decodeOutbound(t *testing.T, raw []byte) (outboundEnvelope, []outboundMessage) {
	t.Helper()

	var env outboundEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("出站请求体无法解析: %v (%s)", err, raw)
	}
	msgs := make([]outboundMessage, 0, len(env.Input.Messages))
	for i, raw := range env.Input.Messages {
		var m outboundMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("出站 input.messages[%d] 无法解析: %v (%s)", i, err, raw)
		}
		msgs = append(msgs, m)
	}
	return env, msgs
}

// contentBlocks 把多模态门的块数组解出来。单键块，故 map[string]string 够用。
func contentBlocks(t *testing.T, m outboundMessage) []map[string]string {
	t.Helper()

	var blocks []map[string]string
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		t.Fatalf("多模态 content 不是块数组: %v (%s)", err, m.Content)
	}
	return blocks
}

// contentString 把文本门的字符串 content 解出来。
func contentString(t *testing.T, m outboundMessage) string {
	t.Helper()

	var s string
	if err := json.Unmarshal(m.Content, &s); err != nil {
		t.Fatalf("文本 content 不是字符串: %v (%s)", err, m.Content)
	}
	return s
}

// wantParam 断言某个参数存在且等于期望值。
func wantParam(t *testing.T, params map[string]any, key string, want any) {
	t.Helper()

	got, ok := params[key]
	if !ok {
		t.Errorf("出站 parameters 缺少 %s，期望 %v", key, want)
		return
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("出站 parameters.%s = %v，期望 %v", key, got, want)
	}
}

// wantNoParam 断言某个参数根本没被发出去。
func wantNoParam(t *testing.T, params map[string]any, key string) {
	t.Helper()

	if got, ok := params[key]; ok {
		t.Errorf("出站 parameters 不该出现 %s，实际 = %v", key, got)
	}
}
