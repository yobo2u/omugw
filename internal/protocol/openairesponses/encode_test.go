package openairesponses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/transport/sse"
)

func authoritative(in, out int64) canonical.Usage {
	return canonical.Usage{
		Fidelity: canonical.FidelityAuthoritative, InputTokens: in, OutputTokens: out,
	}
}

func decodeResponse(t *testing.T, raw []byte) Response {
	t.Helper()
	var r Response
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("编码结果不是合法 JSON: %v\n%s", err, raw)
	}
	return r
}

func TestEncodeTextResponse(t *testing.T) {
	msg := canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.Part{
		canonical.Text("今天多云。"),
	}}

	raw, err := EncodeResponse("qwen-max", msg, authoritative(24, 11), canonical.StopEndTurn, 1754740800)
	if err != nil {
		t.Fatal(err)
	}
	r := decodeResponse(t, raw)

	if r.Object != "response" || r.Status != statusCompleted || r.Model != "qwen-max" {
		t.Errorf("响应头部有误: %+v", r)
	}
	if !strings.HasPrefix(r.ID, "resp_") {
		t.Errorf("响应 ID 应带 resp_ 前缀: %q", r.ID)
	}
	if len(r.Output) != 1 || r.Output[0].Type != outMessage {
		t.Fatalf("output 应为一条 message，实际 %+v", r.Output)
	}
	if r.Output[0].Content[0].Text != "今天多云。" {
		t.Errorf("文本丢失: %+v", r.Output[0].Content)
	}
	if r.Usage == nil || r.Usage.TotalTokens != 35 {
		t.Errorf("用量有误: %+v", r.Usage)
	}
}

// TestAdjacentTextPartsMergeIntoOneMessage 固化一处编码判断。
//
// Responses 的 message 项本身就带 content 数组。为每个文本块单独开一项，
// 客户端会看到一串莫名其妙的碎片消息。
func TestAdjacentTextPartsMergeIntoOneMessage(t *testing.T) {
	msg := canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.Part{
		canonical.Text("第一段"), canonical.Text("第二段"),
	}}

	raw, err := EncodeResponse("m", msg, authoritative(1, 2), canonical.StopEndTurn, 0)
	if err != nil {
		t.Fatal(err)
	}
	r := decodeResponse(t, raw)

	if len(r.Output) != 1 {
		t.Fatalf("相邻文本块应合并成一条 message，实际 %d 项", len(r.Output))
	}
	if len(r.Output[0].Content) != 2 {
		t.Errorf("content 应有 2 块，实际 %d 块", len(r.Output[0].Content))
	}
}

// TestAnnotationsIsAlwaysAnArray 固化一处兼容性细节。
// OpenAI 官方 SDK 会直接读 annotations，给 null 会让部分客户端解析时崩掉。
func TestAnnotationsIsAlwaysAnArray(t *testing.T) {
	msg := canonical.Message{Role: canonical.RoleAssistant,
		Parts: []canonical.Part{canonical.Text("hi")}}

	raw, err := EncodeResponse("m", msg, authoritative(1, 1), canonical.StopEndTurn, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"annotations":[]`) {
		t.Errorf("annotations 应输出空数组而非省略或 null:\n%s", raw)
	}
}

func TestEncodeToolCall(t *testing.T) {
	msg := canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.Part{
		canonical.Text("我查一下"),
		{Kind: canonical.PartToolCall, ToolCall: &canonical.ToolCall{
			ID: "call_1", Name: "get_weather",
			Arguments: json.RawMessage(`{"city":"上海"}`),
		}},
	}}

	raw, err := EncodeResponse("m", msg, authoritative(5, 8), canonical.StopToolUse, 0)
	if err != nil {
		t.Fatal(err)
	}
	r := decodeResponse(t, raw)

	if len(r.Output) != 2 {
		t.Fatalf("应有 message 与 function_call 两项，实际 %d 项", len(r.Output))
	}
	fc := r.Output[1]
	if fc.Type != outFuncCall || fc.CallID != "call_1" || fc.Name != "get_weather" {
		t.Errorf("工具调用项有误: %+v", fc)
	}
	// arguments 是**字符串**字段，不是对象。
	if fc.Arguments != `{"city":"上海"}` {
		t.Errorf("arguments = %q", fc.Arguments)
	}
}

// TestEmptyToolArgumentsBecomeEmptyObject 防止客户端的 JSON.parse 抛错。
func TestEmptyToolArgumentsBecomeEmptyObject(t *testing.T) {
	msg := canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.Part{
		{Kind: canonical.PartToolCall, ToolCall: &canonical.ToolCall{ID: "c", Name: "f"}},
	}}

	raw, err := EncodeResponse("m", msg, authoritative(1, 1), canonical.StopToolUse, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeResponse(t, raw).Output[0].Arguments; got != "{}" {
		t.Errorf("空参数应编成 {}，实际 %q", got)
	}
}

// TestThinkingSignatureIsNotEmitted 固化一处刻意的丢弃。
//
// 签名是 Anthropic 的凭证，对 Responses 客户端毫无意义。发出去只会让它被原样
// 回传，然后在下一轮被降级矩阵拒掉——制造一个本可避免的失败。
func TestThinkingSignatureIsNotEmitted(t *testing.T) {
	msg := canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.Part{
		{Kind: canonical.PartThinking, Thinking: &canonical.Thinking{
			Text: "先看题目", Signature: "SIGNATURE_MUST_NOT_LEAK",
		}},
		canonical.Text("答案是 42"),
	}}

	raw, err := EncodeResponse("m", msg, authoritative(3, 9), canonical.StopEndTurn, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SIGNATURE_MUST_NOT_LEAK") {
		t.Errorf("推理签名被发给了客户端:\n%s", raw)
	}

	r := decodeResponse(t, raw)
	if r.Output[0].Type != outReasoning || r.Output[0].Summary[0].Text != "先看题目" {
		t.Errorf("推理摘要应保留: %+v", r.Output[0])
	}
}

// TestNonAuthoritativeUsageIsOmitted 固化原则 2.5 在编码侧的表现。
//
// 把估算值当成上游返回的用量呈现给客户端，等于替它做了一个它不知道的假设，
// 而客户端很可能拿这个数去对账。
func TestNonAuthoritativeUsageIsOmitted(t *testing.T) {
	msg := canonical.Message{Role: canonical.RoleAssistant,
		Parts: []canonical.Part{canonical.Text("hi")}}

	for _, u := range []canonical.Usage{
		{Fidelity: canonical.FidelityEstimated, InputTokens: 999, OutputTokens: 999},
		canonical.UnavailableUsage(),
	} {
		raw, err := EncodeResponse("m", msg, u, canonical.StopEndTurn, 0)
		if err != nil {
			t.Fatal(err)
		}
		if decodeResponse(t, raw).Usage != nil {
			t.Errorf("可信等级 %q 的用量不该输出给客户端", u.Fidelity)
		}
		if strings.Contains(string(raw), "999") {
			t.Errorf("估算的 token 数泄露到了响应里:\n%s", raw)
		}
	}
}

func TestStopReasonMapsToStatus(t *testing.T) {
	msg := canonical.Message{Role: canonical.RoleAssistant,
		Parts: []canonical.Part{canonical.Text("hi")}}

	tests := []struct {
		stop       canonical.StopReason
		wantStatus string
		wantReason string
	}{
		{canonical.StopEndTurn, statusCompleted, ""},
		{canonical.StopToolUse, statusCompleted, ""},
		{canonical.StopMaxTokens, statusIncomplete, "max_output_tokens"},
		{canonical.StopContentFilter, statusIncomplete, "content_filter"},
		// 流被中断时不能报 completed——那会让客户端以为拿到了完整回答。
		{canonical.StopInterrupted, statusFailed, ""},
	}
	for _, tc := range tests {
		t.Run(string(tc.stop), func(t *testing.T) {
			raw, err := EncodeResponse("m", msg, authoritative(1, 1), tc.stop, 0)
			if err != nil {
				t.Fatal(err)
			}
			r := decodeResponse(t, raw)
			if r.Status != tc.wantStatus {
				t.Errorf("status = %q, 期望 %q", r.Status, tc.wantStatus)
			}
			if tc.wantReason != "" {
				if r.IncompleteDetails == nil || r.IncompleteDetails.Reason != tc.wantReason {
					t.Errorf("incomplete_details = %+v, 期望 reason=%q",
						r.IncompleteDetails, tc.wantReason)
				}
			}
		})
	}
}

// TestMediaInOutputIsRejected 固化「不静默丢弃」。
// 客户端收到一个少了图的回答却毫不知情，比直接报错难查得多。
func TestMediaInOutputIsRejected(t *testing.T) {
	msg := canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.Part{
		canonical.ImageURL("https://x/a.png", "image/png"),
	}}

	if _, err := EncodeResponse("m", msg, authoritative(1, 1), canonical.StopEndTurn, 0); err == nil {
		t.Fatal("输出里的媒体块应当报错而非静默丢弃")
	}
}

// —— 流式 ——

// encodeStream 把一串 Canonical 事件跑完编码器。
func encodeStream(t *testing.T, evs ...canonical.Event) []sse.Event {
	t.Helper()
	enc := NewStreamEncoder("qwen-max", 1754740800)

	var out []sse.Event
	for i, ev := range evs {
		got, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("事件 %d (%s): %v", i, ev.Type, err)
		}
		out = append(out, got...)
	}
	return out
}

func names(evs []sse.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Event)
	}
	return out
}

func TestStreamTextSequence(t *testing.T) {
	got := encodeStream(t,
		canonical.Event{Type: canonical.EventMessageStart},
		canonical.Event{Type: canonical.EventContentStart, Index: 0, ContentKind: canonical.PartText},
		canonical.Event{Type: canonical.EventTextDelta, Index: 0, Text: "你"},
		canonical.Event{Type: canonical.EventTextDelta, Index: 0, Text: "好"},
		canonical.Event{Type: canonical.EventContentEnd, Index: 0},
		canonical.Event{Type: canonical.EventUsage, Usage: ptr(authoritative(7, 2))},
		canonical.Event{Type: canonical.EventMessageEnd, StopReason: canonical.StopEndTurn},
	)

	want := []string{
		evCreated, evInProgress,
		evItemAdded, evPartAdded,
		evTextDelta, evTextDelta,
		evTextDone, evPartDone, evItemDone,
		evCompleted,
	}
	assertNames(t, got, want)

	// response.completed 必须带上完整的 output 数组——官方 SDK 正是靠它拼出
	// 最终结果的。
	final := lastData(t, got)
	if len(final.Output) != 1 || final.Output[0].Content[0].Text != "你好" {
		t.Errorf("response.completed 未附上完整 output: %+v", final.Output)
	}
	if final.Usage == nil || final.Usage.TotalTokens != 9 {
		t.Errorf("response.completed 未附上用量: %+v", final.Usage)
	}
}

func TestStreamToolCallSequence(t *testing.T) {
	got := encodeStream(t,
		canonical.Event{Type: canonical.EventMessageStart},
		canonical.Event{Type: canonical.EventContentStart, Index: 0,
			ContentKind: canonical.PartToolCall,
			ToolCall:    &canonical.ToolCall{ID: "call_1", Name: "get_weather"}},
		canonical.Event{Type: canonical.EventToolCallArgsDelta, Index: 0, ArgsDelta: `{"ci`},
		canonical.Event{Type: canonical.EventToolCallArgsDelta, Index: 0, ArgsDelta: `ty":"上海"}`},
		canonical.Event{Type: canonical.EventToolCallEnd, Index: 0},
		canonical.Event{Type: canonical.EventMessageEnd, StopReason: canonical.StopToolUse},
	)

	assertNames(t, got, []string{
		evCreated, evInProgress,
		evItemAdded,
		evArgsDelta, evArgsDelta,
		evArgsDone, evItemDone,
		evCompleted,
	})

	// 工具调用项没有 content 数组那一层，带 content_index 会让严格解析的
	// 客户端拒收。
	for _, ev := range got {
		if ev.Event == evArgsDelta || ev.Event == evArgsDone {
			if strings.Contains(ev.Data, "content_index") {
				t.Errorf("%s 不该带 content_index: %s", ev.Event, ev.Data)
			}
		}
	}

	final := lastData(t, got)
	if final.Output[0].Arguments != `{"city":"上海"}` {
		t.Errorf("参数重组有误: %q", final.Output[0].Arguments)
	}
}

// TestStreamAutoOpensBlockOnBareDelta 覆盖上游不发 content.start 的情形。
// 丢掉内容比多发一个 item.added 糟糕得多。
func TestStreamAutoOpensBlockOnBareDelta(t *testing.T) {
	got := encodeStream(t,
		canonical.Event{Type: canonical.EventTextDelta, Index: 0, Text: "裸的"},
		canonical.Event{Type: canonical.EventMessageEnd, StopReason: canonical.StopEndTurn},
	)

	if final := lastData(t, got); len(final.Output) != 1 ||
		final.Output[0].Content[0].Text != "裸的" {
		t.Errorf("裸 delta 的内容应被保留: %+v", final.Output)
	}
}

// TestStreamClosesPreviousItemImplicitly 覆盖上游不发 content.end 就开下一块。
// 丢一个 output_item.done 会让客户端的状态机一直卡在「这一项还没结束」。
func TestStreamClosesPreviousItemImplicitly(t *testing.T) {
	got := encodeStream(t,
		canonical.Event{Type: canonical.EventMessageStart},
		canonical.Event{Type: canonical.EventContentStart, Index: 0, ContentKind: canonical.PartText},
		canonical.Event{Type: canonical.EventTextDelta, Index: 0, Text: "第一块"},
		// 不发 content.end，直接开第二块。
		canonical.Event{Type: canonical.EventContentStart, Index: 1,
			ContentKind: canonical.PartToolCall,
			ToolCall:    &canonical.ToolCall{ID: "c", Name: "f"}},
		canonical.Event{Type: canonical.EventToolCallEnd, Index: 1},
		canonical.Event{Type: canonical.EventMessageEnd, StopReason: canonical.StopToolUse},
	)

	var itemDones int
	for _, ev := range got {
		if ev.Event == evItemDone {
			itemDones++
		}
	}
	if itemDones != 2 {
		t.Errorf("两个输出项都应有 output_item.done，实际 %d 个", itemDones)
	}
	if final := lastData(t, got); len(final.Output) != 2 {
		t.Errorf("output 应有两项，实际 %+v", final.Output)
	}
}

// TestStreamSignatureDeltaIsDropped 覆盖签名在流式路径上的丢弃。
func TestStreamSignatureDeltaIsDropped(t *testing.T) {
	got := encodeStream(t,
		canonical.Event{Type: canonical.EventContentStart, Index: 0,
			ContentKind: canonical.PartThinking},
		canonical.Event{Type: canonical.EventThinkingDelta, Index: 0, Text: "思考中"},
		canonical.Event{Type: canonical.EventSignatureDelta, Index: 0, Text: "SIG_LEAK"},
		canonical.Event{Type: canonical.EventContentEnd, Index: 0},
		canonical.Event{Type: canonical.EventMessageEnd, StopReason: canonical.StopEndTurn},
	)

	for _, ev := range got {
		if strings.Contains(ev.Data, "SIG_LEAK") {
			t.Fatalf("推理签名泄露到流里: %s", ev.Data)
		}
	}
	if final := lastData(t, got); final.Output[0].Summary[0].Text != "思考中" {
		t.Errorf("推理摘要应保留: %+v", final.Output[0])
	}
}

// TestStreamErrorClosesOpenItemAndMarksUsageUnavailable 固化原则 2.4 / 2.5
// 在编码侧的表现。
func TestStreamErrorClosesOpenItemAndMarksUsageUnavailable(t *testing.T) {
	got := encodeStream(t,
		canonical.Event{Type: canonical.EventMessageStart},
		canonical.Event{Type: canonical.EventContentStart, Index: 0, ContentKind: canonical.PartText},
		canonical.Event{Type: canonical.EventTextDelta, Index: 0, Text: "半截"},
		canonical.Event{Type: canonical.EventUsage, Usage: ptr(authoritative(10, 5))},
		canonical.Event{Type: canonical.EventError,
			Err: canonical.Newf(canonical.ClassUpstreamUnavailable, "connection reset")},
	)

	n := names(got)
	// 开着的项要先收尾，客户端的状态机才不会卡住。
	if !contains(n, evItemDone) {
		t.Errorf("中断前应收尾已打开的输出项，实际序列 %v", n)
	}
	if !contains(n, evFailed) || !contains(n, evError) {
		t.Errorf("应发出 response.failed 与 error 事件，实际序列 %v", n)
	}

	// 中断后用量不可知——上游不会再送 usage，此前收到的那份也不再代表全程。
	for _, ev := range got {
		if ev.Event == evFailed && strings.Contains(ev.Data, `"usage"`) {
			t.Errorf("中断后不该保留用量数字: %s", ev.Data)
		}
	}
}

func TestStreamMaxTokensEmitsIncomplete(t *testing.T) {
	got := encodeStream(t,
		canonical.Event{Type: canonical.EventContentStart, Index: 0, ContentKind: canonical.PartText},
		canonical.Event{Type: canonical.EventTextDelta, Index: 0, Text: "被截断的"},
		canonical.Event{Type: canonical.EventMessageEnd, StopReason: canonical.StopMaxTokens},
	)

	if !contains(names(got), evIncomplete) {
		t.Errorf("达到长度上限应发 response.incomplete，实际 %v", names(got))
	}
}

func ptr(u canonical.Usage) *canonical.Usage { return &u }

func contains(s []string, want string) bool {
	for _, x := range s {
		if x == want {
			return true
		}
	}
	return false
}

func assertNames(t *testing.T, got []sse.Event, want []string) {
	t.Helper()
	g := names(got)
	if len(g) != len(want) {
		t.Fatalf("事件序列长度 = %d, 期望 %d:\n got %v\nwant %v", len(g), len(want), g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Errorf("第 %d 条事件 = %q, 期望 %q（完整序列 %v）", i, g[i], want[i], g)
		}
	}
}

// lastData 解出最后一条事件里的 response 对象。
func lastData(t *testing.T, evs []sse.Event) Response {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("没有任何事件")
	}
	var wrapper struct {
		Response Response `json:"response"`
	}
	last := evs[len(evs)-1]
	if err := json.Unmarshal([]byte(last.Data), &wrapper); err != nil {
		t.Fatalf("最后一条事件不含 response 对象: %v\n%s", err, last.Data)
	}
	return wrapper.Response
}
