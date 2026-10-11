package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/yobo2u/omugw/internal/convstore"
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

// captureUpstream 记下上游收到的每个请求体，并按次序返回给定响应。
func captureUpstream(t *testing.T, responses ...string) (*upstream, func() [][]byte) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies [][]byte
	)
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		i := len(bodies) - 1
		mu.Unlock()
		if i >= len(responses) {
			i = len(responses) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responses[i])
	})
	return up, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), bodies...)
	}
}

const rdcOKResponse = `{"id":"resp_up_2","object":"response","status":"completed","store":true,` +
	`"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"MANGO42"}]}]}`

// TestResponsesDSCompatBypassesConvstoreEvenWhenEnabled 防的是运维开着
// convstore 时本路径被悄悄换了会话保管人：开关只该影响声明为 EMULATE 的
// 路径。被展开的征兆是上游收不到 previous_response_id、多出 store:false、
// 客户端拿到的 id 不是上游的。
func TestResponsesDSCompatBypassesConvstoreEvenWhenEnabled(t *testing.T) {
	up, bodies := captureUpstream(t, rdcOKResponse)
	hs := newResponsesDSCompatHarness(t, up)
	hs.matrix.WithAvailability(degrade.Availability{degrade.FeatureConversationStore: true})
	hs.h.deps.ConversationStore = convstore.NewMemoryStore(convstore.DefaultLimits(), nil)

	rec := hs.do(t, `{"model":"m","input":"What was my codeword?","previous_response_id":"resp_up_1"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", rec.Code, rec.Body.String())
	}

	got := bodies()
	if len(got) != 1 {
		t.Fatalf("上游被调用 %d 次，期望 1 次", len(got))
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got[0], &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["previous_response_id"]) != `"resp_up_1"` {
		t.Errorf("previous_response_id = %s，期望原样转发", fields["previous_response_id"])
	}
	if _, ok := fields["store"]; ok {
		t.Errorf("客户端没发 store，上游却收到了: %s", fields["store"])
	}
	if string(fields["input"]) != `"What was my codeword?"` {
		t.Errorf("input 被改写（历史被展开？）: %s", fields["input"])
	}

	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != "resp_up_2" {
		t.Errorf("response.id = %q，期望上游的 resp_up_2（不得改写）", resp.ID)
	}
	if h := rec.Header().Get(EmulationHeader); h != "" {
		t.Errorf("不应有模拟头: %q", h)
	}
}

// TestResponsesDSCompatContinuationWorksWithoutConvstore 防的是默认部署下
// 续轮 500：prepareConversation 在没装配存储而请求显式依赖会话时返回内部
// 错误，那条分支不该被本路径走到。显式 store:true 同理。
func TestResponsesDSCompatContinuationWorksWithoutConvstore(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","input":"hi","previous_response_id":"resp_up_1"}`,
		`{"model":"m","input":"hi","store":true}`,
	} {
		up, _ := captureUpstream(t, rdcOKResponse)
		hs := newResponsesDSCompatHarness(t, up)

		rec := hs.do(t, body, true)
		if rec.Code != http.StatusOK {
			t.Errorf("请求 %s 状态码 = %d，期望 200: %s", body, rec.Code, rec.Body.String())
		}
	}
}

// TestResponsesDSCompatRejectsBeforeUpstream 钉死触达上游前的两种失败：
// REJECT 格是 422（改请求），未兑现格是 501（等实现）。上游一次都不该被调用——
// 发出去再让上游静默忽略，正是矩阵要防的那种不可见损失。
func TestResponsesDSCompatRejectsBeforeUpstream(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"file_input 是 REJECT", `{"model":"m","store":false,"input":[{"role":"user","content":[` +
			`{"type":"input_file","file_id":"file-abc"}]}]}`, http.StatusUnprocessableEntity},
		{"computer_use 是 REJECT", `{"model":"m","store":false,"input":"hi",` +
			`"tools":[{"type":"computer_use_preview"}]}`, http.StatusUnprocessableEntity},
		{"audio_input 未兑现", `{"model":"m","store":false,"input":[{"role":"user","content":[` +
			`{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`, http.StatusNotImplemented},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, _ := captureUpstream(t, rdcOKResponse)
			hs := newResponsesDSCompatHarness(t, up)

			rec := hs.do(t, tc.body, true)
			if rec.Code != tc.want {
				t.Fatalf("状态码 = %d，期望 %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if n := up.calls.Load(); n != 0 {
				t.Errorf("上游被调用 %d 次，期望 0 次", n)
			}
		})
	}
}

// TestResponsesDSCompatContinuationMissIsNotRetriedOrCooled 防的是把续轮
// 未命中当成凭据故障：它换凭据不会好，重试只会把同一个 400 再打一遍；冷却
// 凭据则会让一个客户端的过期 id 拖垮别人的请求。
//
// 只配一个上游是刻意的：网关对不可重试错误的既有行为是「放弃当前上游的
// 其余凭据，换下一个候选上游」（见 gateway_test.go 里「400 触发 Provider 级
// failover」的说明），配了备用上游就会被它接走，那条行为属于主链路而不是
// 本路径，不在这里断言。
func TestResponsesDSCompatContinuationMissIsNotRetriedOrCooled(t *testing.T) {
	var n int
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"request_id":"req-1","code":"InvalidParameter",`+
				`"message":"Not found previous_response_id: resp_gone."}`)
			return
		}
		_, _ = io.WriteString(w, rdcOKResponse)
	})
	hs := newResponsesDSCompatHarness(t, up)

	miss := hs.do(t, `{"model":"m","input":"hi","previous_response_id":"resp_gone"}`, true)
	if miss.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400: %s", miss.Code, miss.Body.String())
	}
	if !strings.Contains(miss.Body.String(), "Not found previous_response_id") {
		t.Errorf("上游原话没有到达客户端: %s", miss.Body.String())
	}
	if !strings.Contains(miss.Body.String(), `"code":"InvalidParameter"`) {
		t.Errorf("上游 code 没有到达客户端: %s", miss.Body.String())
	}
	if got := up.calls.Load(); got != 1 {
		t.Errorf("续轮未命中后上游被调用 %d 次，期望 1 次（同一凭据不重试）", got)
	}

	// 紧接着的正常请求仍应能用同一把凭据：它没有被冷却。
	ok := hs.do(t, `{"model":"m","input":"hi","store":false}`, true)
	if ok.Code != http.StatusOK {
		t.Fatalf("后续请求状态码 = %d（凭据被冷却了？）: %s", ok.Code, ok.Body.String())
	}
}
