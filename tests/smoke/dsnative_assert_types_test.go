//go:build smoke

package smoke_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/degrade"
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

// recordClientModel 是真实录制时客户端侧提交的模型名。
//
// 与任何角色的真实模型名都不同：两边同名时，「出站体的 model 没被改写」与
// 「改写成了同一个名字」在证据里长得一模一样，模型改写这一环就再也证不出来。
const recordClientModel = "omugw-record-client-model"

// expectedDegradedCaps 声明每个用例应当出现在降级头里的能力。
//
// 逐项列出而不是只问「头非空」：矩阵把三项能力都判成 DEGRADE，只要头里有
// 任意一项就算过的话，一次把 web_search 误判成降级的改动照样全绿。
var expectedDegradedCaps = map[string][]canonical.Capability{
	"parallel_tool_calls": {canonical.CapParallelToolCalls},
	"structured_output":   {canonical.CapStructuredOutput},
	"web_search":          {canonical.CapWebSearch},
	"combined":            {canonical.CapStructuredOutput, canonical.CapWebSearch},
}

// downstreamChunk 是下游 chat.completion.chunk 的断言视图。
type downstreamChunk struct {
	Object  string `json:"object"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int `json:"index"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *downstreamUsage `json:"usage"`
}

// downstreamCompletion 是下游 chat.completion 的断言视图。
type downstreamCompletion struct {
	Object  string `json:"object"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *downstreamUsage `json:"usage"`
}

// downstreamUsage 是下游用量的断言视图。
//
// 收成指针型字段的宿主结构：Chat 编码器只在用量权威时才发 usage，
// 「字段整体缺席」正是「这次调用的用量不可计费」的判据。
type downstreamUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// wantsUsageChunk 报告这份客户端请求体是否要过流式末尾的 usage chunk。
//
// 从请求体现读而不是另立一张名单：网关只在客户端要过时才补这一帧，
// 名单与请求体漂移会让断言去等一帧本就不该出现的账单。
func wantsUsageChunk(body []byte) bool {
	var shape struct {
		StreamOptions *struct {
			IncludeUsage *bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		return false
	}
	return shape.StreamOptions != nil &&
		shape.StreamOptions.IncludeUsage != nil &&
		*shape.StreamOptions.IncludeUsage
}

// assertRecordedCase 断言一次真实录制在落盘之前必须成立的全部事实。
//
// 全部断言都发生在 Save 之前：一份没通过断言的证据比没有证据更糟——
// 它会被后续回放当成事实，而那时谁也不会再去问它当初是怎么来的。
func assertRecordedCase(t *testing.T, c caseMeta, model string,
	clientRec *httptest.ResponseRecorder, snap recordingSnapshot) {
	t.Helper()

	assertRecordedUpstream(t, c, model, snap)
	assertRecordedResponse(t, c, snap)
	assertDegradationHeader(t, c, clientRec)
	assertLiveDownstream(t, c, clientRec)
}

// assertRecordedUpstream 断言捕获到的出站请求确实打对了门、带对了凭据形态，
// 并且这次能力的字段真的搬到了上游。
func assertRecordedUpstream(t *testing.T, c caseMeta, model string, snap recordingSnapshot) {
	t.Helper()

	if snap.Upstream.Method != http.MethodPost {
		t.Errorf("上游 method = %q，期望 POST", snap.Upstream.Method)
	}
	// 门决定路径：推断出来的路径会把含媒体的请求打到文本门，
	// 症状是一个语焉不详的上游 400。
	if want := c.door.Path(); snap.Upstream.Path != want {
		t.Errorf("上游路径 = %q，期望门 %q 的 %q", snap.Upstream.Path, string(c.door), want)
	}
	// 只认精确的 <redacted>：任何别的值都意味着真实 Key 正走在通往磁盘的路上。
	if got := snap.Upstream.Headers["authorization"]; got != "<redacted>" {
		t.Errorf("上游 authorization 未按预期脱敏（期望 <redacted>）")
	}
	if got := snap.Upstream.Headers["content-type"]; !strings.HasPrefix(got, "application/json") {
		t.Errorf("上游 content-type = %q，期望 application/json", got)
	}

	sseKey := strings.ToLower(nativewire.SSEHeader)
	if c.stream {
		// 流式头是 Native 声明流式的唯一途径，体里没有等价字段。
		if got := snap.Upstream.Headers[sseKey]; !strings.EqualFold(got, "enable") {
			t.Errorf("上游 %s = %q，期望 enable", nativewire.SSEHeader, got)
		}
	} else if got, ok := snap.Upstream.Headers[sseKey]; ok && got != "" {
		t.Errorf("非流式用例的上游 %s = %q，期望不发", nativewire.SSEHeader, got)
	}

	if !json.Valid(snap.Upstream.Body) {
		t.Fatalf("上游请求体不是合法 JSON")
	}

	env, msgs := decodeOutbound(t, snap.Upstream.Body)
	if env.Model != model {
		t.Errorf("上游 model = %q，期望被改写成路由选定的 %q", env.Model, model)
	}
	if env.Model == recordClientModel {
		t.Error("上游 model 仍是客户端提交的名字，模型改写没有发生")
	}
	if len(msgs) == 0 {
		t.Fatal("上游 input.messages 为空")
	}

	// 复用本地预检那份逐用例断言：证明这项能力的字段在**真实**调用里也搬到了
	// 上游，而不只是在假源站前面走通过一次。
	assertion, ok := preflightAssertions()[c.name]
	if !ok {
		t.Fatalf("用例 %q 没有对应的预检断言", c.name)
	}
	assertion.assert(t, env, msgs)
}

// assertRecordedResponse 断言捕获到的上游响应形态与用例声称的流式与否自洽。
func assertRecordedResponse(t *testing.T, c caseMeta, snap recordingSnapshot) {
	t.Helper()

	if snap.Response.Status != http.StatusOK {
		t.Fatalf("上游响应状态码 = %d，期望 200", snap.Response.Status)
	}
	if got := snap.Response.Headers["content-type"]; got == "" {
		t.Error("上游响应缺少 content-type，无从判断这份录制是什么形态")
	}

	if !c.stream {
		if snap.Response.SSE != nil {
			t.Errorf("非流式用例捕获到了事件流，共 %d 条事件", len(snap.Response.SSE.Events))
		}
		if len(snap.Response.Body) == 0 {
			t.Fatal("非流式用例没有捕获到响应体")
		}
		if !json.Valid(snap.Response.Body) {
			t.Fatal("非流式响应体不是合法 JSON")
		}
		return
	}

	if snap.Response.Body != nil {
		t.Errorf("流式用例不该同时捕获整块响应体（与 sse 互斥）")
	}
	if snap.Response.SSE == nil {
		t.Fatal("流式用例没有捕获到事件流")
	}
	if len(snap.Response.SSE.Events) == 0 {
		t.Fatal("流式用例捕获到的事件流为空")
	}
	// 帧边界取自代理逐事件转发的节奏，因此每帧恒为一条；合计必须等于事件数，
	// 否则回放时会按一个对不上的边界重新分片。
	total := 0
	for i, n := range snap.Response.SSE.Frames {
		if n != 1 {
			t.Errorf("sse.frames[%d] = %d，期望逐事件登记的 1", i, n)
		}
		total += n
	}
	if total != len(snap.Response.SSE.Events) {
		t.Errorf("sse.frames 合计 = %d，与事件数 %d 不符", total, len(snap.Response.SSE.Events))
	}
}

// assertDegradationHeader 断言降级头的有无与内容都与用例声明一致。
func assertDegradationHeader(t *testing.T, c caseMeta, rec *httptest.ResponseRecorder) {
	t.Helper()

	got := rec.Header().Get(degrade.DegradationHeader)
	if !c.expectDegraded {
		if got != "" {
			t.Errorf("未声明降级的用例出现了 %s = %q", degrade.DegradationHeader, got)
		}
		return
	}
	if got == "" {
		t.Fatalf("声明降级的用例缺少 %s 响应头", degrade.DegradationHeader)
	}

	// 头值形如 "cap=说明; cap=说明"，只取能力名对账：说明文案属于矩阵，
	// 钉死它会让一次无关的措辞调整变成录制失败。
	names := make([]string, 0, 2)
	for _, part := range strings.Split(got, ";") {
		name, _, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found || name == "" {
			t.Errorf("%s 的分段 %q 不是 cap=说明 形态", degrade.DegradationHeader, part)
			continue
		}
		names = append(names, name)
	}
	want := make([]string, 0, len(expectedDegradedCaps[c.name]))
	for _, cap := range expectedDegradedCaps[c.name] {
		want = append(want, string(cap))
	}
	if len(want) == 0 {
		t.Fatalf("用例 %q 声明了降级，却没有登记应被降级的能力", c.name)
	}
	slices.Sort(names)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("%s 声明的能力 = %v，期望 %v", degrade.DegradationHeader, names, want)
	}
}
