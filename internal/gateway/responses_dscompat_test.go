package gateway

import (
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
)

// TestEmulatesConversationFollowsMatrix 防的是主链路自己决定谁该进 convstore：
// 判据只能是矩阵里该路径 stateful_conversation 的处置。写成按 Provider 名
// 硬编码，日后任一条路径改处置时主链路就会与矩阵分家——那种错在响应里看不
// 出来，只表现为会话悄悄换了保管人。
func TestEmulatesConversationFollowsMatrix(t *testing.T) {
	hs := newHarness(t, true, jsonUpstream(t, `{}`))

	cases := []struct {
		kind degrade.Provider
		want bool
	}{
		{degrade.ProviderOpenAICompat, true},
		{degrade.ProviderDashScopeNative, true},
		{degrade.ProviderAnthropicMessages, true},
		{degrade.ProviderDashScopeCompatible, false},
		// 未登记的组合没有可模拟的格子。
		{degrade.ProviderDashScopeWSRealtime, false},
	}
	for _, tc := range cases {
		if got := hs.h.emulatesConversation(tc.kind); got != tc.want {
			t.Errorf("emulatesConversation(%s) = %v，期望 %v", tc.kind, got, tc.want)
		}
	}
}

// TestChatHandlerNeverEmulatesConversation 防的是无状态协议被拖进会话逻辑：
// Chat 表达不出 stateful_conversation，该格是 N/A。
func TestChatHandlerNeverEmulatesConversation(t *testing.T) {
	hs := newChatHarness(t, true, jsonUpstream(t, `{}`))
	if hs.h.emulatesConversation(degrade.ProviderOpenAICompat) {
		t.Error("Chat 入站不该进入会话模拟")
	}
}
