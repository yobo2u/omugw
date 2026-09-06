//go:build smoke

package smoke_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/yobo2u/omugw/internal/testkit"
)

// assertLiveDownstream 断言客户端真正收到的是一份转换完整、内容可信的 Chat 响应。
//
// 只看状态码是不够的：流式的 200 在首帧之前就写出去了，一条转换半途失败的流
// 照样顶着 200 收场——错误只以流内 error 事件的形式出现。
func assertLiveDownstream(t *testing.T, c caseMeta, rec *httptest.ResponseRecorder) {
	t.Helper()

	if rec.Code != http.StatusOK {
		t.Fatalf("网关状态码 = %d，期望 200: %s", rec.Code, rec.Body.String())
	}
	if c.stream {
		assertLiveStream(t, c, rec)
		return
	}
	assertLiveCompletion(t, c, rec)
}

// assertLiveCompletion 断言非流式下游响应。
func assertLiveCompletion(t *testing.T, c caseMeta, rec *httptest.ResponseRecorder) {
	t.Helper()

	var resp downstreamCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("下游响应不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	if resp.Object != "chat.completion" {
		t.Errorf("下游 object = %q，期望 chat.completion", resp.Object)
	}
	if len(resp.Choices) == 0 {
		t.Fatalf("下游 choices 为空: %s", rec.Body.String())
	}
	for i, ch := range resp.Choices {
		// finish_reason 缺席意味着这条回复没有收尾理由，客户端无从判断它
		// 是说完了还是被截断了。
		if ch.FinishReason == nil || *ch.FinishReason == "" {
			t.Errorf("下游 choices[%d] 的 finish_reason 为空", i)
		}
	}
	// usage 字段的存在本身就是「用量权威、可计费」的判据：编码器在估算或
	// 不可用时整体省略它，绝不发一份失真的数字。
	if resp.Usage == nil {
		t.Fatalf("下游缺少 usage，本次调用的用量不可计费: %s", rec.Body.String())
	}
	assertAuthoritativeUsage(t, *resp.Usage)

	if c.name == "multi_candidate_nonstream" {
		assertDistinctIndexes(t, "下游 choices", collectCompletionIndexes(resp))
	}
}

// collectCompletionIndexes 取出非流式响应里各候选的序号。
func collectCompletionIndexes(resp downstreamCompletion) []int {
	out := make([]int, 0, len(resp.Choices))
	for _, ch := range resp.Choices {
		out = append(out, ch.Index)
	}
	return out
}

// assertDistinctIndexes 断言候选序号恰好是 0 与 1 两个互不相同的值。
func assertDistinctIndexes(t *testing.T, what string, got []int) {
	t.Helper()

	uniq := slices.Clone(got)
	slices.Sort(uniq)
	uniq = slices.Compact(uniq)
	if want := []int{0, 1}; !slices.Equal(uniq, want) {
		t.Errorf("%s 的候选序号 = %v（去重后 %v），期望恰好 %v——"+
			"n=2 被上游静默压回 1 时正是这里失守", what, got, uniq, want)
	}
}

// assertAuthoritativeUsage 断言一份权威用量的数值自洽且非空。
func assertAuthoritativeUsage(t *testing.T, u downstreamUsage) {
	t.Helper()

	if u.PromptTokens <= 0 {
		t.Errorf("usage.prompt_tokens = %d，期望正数", u.PromptTokens)
	}
	if u.CompletionTokens <= 0 {
		t.Errorf("usage.completion_tokens = %d，期望正数", u.CompletionTokens)
	}
	if want := u.PromptTokens + u.CompletionTokens; u.TotalTokens != want {
		t.Errorf("usage.total_tokens = %d，期望 prompt+completion 的 %d", u.TotalTokens, want)
	}
}

// streamObservation 是一条下游事件流的解析结果。
type streamObservation struct {
	dataChunks   int
	sawDone      bool
	doneIsLast   bool
	usageChunk   *downstreamUsage
	toolArgFrags []string
	finishReason string
	reasoning    int
}

// observeStream 把下游事件流解析成断言所需的观测量。
func observeStream(t *testing.T, rec *httptest.ResponseRecorder) streamObservation {
	t.Helper()

	events, err := testkit.ParseSSE(rec.Body)
	if err != nil {
		t.Fatalf("下游 SSE 无法解析: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("下游事件流为空")
	}

	var obs streamObservation
	for i, ev := range events {
		if ev.Event == "error" {
			t.Fatalf("下游流内出现 error 事件，转换未成功: %s", ev.Data)
		}
		if ev.IsDone() {
			obs.sawDone = true
			obs.doneIsLast = i == len(events)-1
			continue
		}
		var chunk downstreamChunk
		if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
			t.Fatalf("下游第 %d 条 chunk 不是合法 JSON: %v (%s)", i, err, ev.Data)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Errorf("下游第 %d 条 chunk 的 object = %q，期望 chat.completion.chunk", i, chunk.Object)
		}
		// 空 choices 且带 usage 就是账单帧，不计入内容帧。
		if len(chunk.Choices) == 0 && chunk.Usage != nil {
			obs.usageChunk = chunk.Usage
			continue
		}
		obs.dataChunks++
		for _, ch := range chunk.Choices {
			if ch.Delta.ReasoningContent != "" {
				obs.reasoning++
			}
			for _, tc := range ch.Delta.ToolCalls {
				if tc.Function.Arguments != "" {
					obs.toolArgFrags = append(obs.toolArgFrags, tc.Function.Arguments)
				}
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				obs.finishReason = *ch.FinishReason
			}
		}
	}
	return obs
}

// assertLiveStream 断言流式下游响应。
func assertLiveStream(t *testing.T, c caseMeta, rec *httptest.ResponseRecorder) {
	t.Helper()

	obs := observeStream(t, rec)

	if obs.dataChunks == 0 {
		t.Error("下游流没有任何内容 chunk")
	}
	// [DONE] 由 translator 在全部候选 finish 后合成，缺了它 OpenAI 客户端
	// 会一直等一个永远不会到来的哨兵。
	if !obs.sawDone {
		t.Fatal("下游流缺少 [DONE] 哨兵，转换未走完")
	}
	if !obs.doneIsLast {
		t.Error("[DONE] 不是最后一条事件，哨兵之后还有内容")
	}
	if obs.finishReason == "" {
		t.Error("下游流没有任何候选给出 finish_reason")
	}

	// 账单帧只在客户端要过时才该出现，据请求体现判，不另立名单。
	if wantsUsageChunk(c.body(recordClientModel)) {
		if obs.usageChunk == nil {
			t.Fatal("客户端要了 include_usage，下游流却没有末尾 usage chunk")
		}
		assertAuthoritativeUsage(t, *obs.usageChunk)
	}

	switch c.name {
	case "tool_calling":
		// 跨帧拼接是本用例唯一要证的事：一条帧内参数就已闭合的流，
		// 根本跑不到那段续发逻辑，绿了也什么都没证明。
		if len(obs.toolArgFrags) < 2 {
			t.Errorf("下游工具参数只有 %d 个非空片段（%v），期望至少 2 个——"+
				"这才是跨帧拼接的真实证据", len(obs.toolArgFrags), obs.toolArgFrags)
		}
		if obs.finishReason != "tool_calls" {
			t.Errorf("下游 finish_reason = %q，期望 tool_calls", obs.finishReason)
		}
	case "reasoning":
		if obs.reasoning == 0 {
			t.Error("下游流没有任何 chunk 带 reasoning_content，深度思考没有落到客户端")
		}
	}
}
