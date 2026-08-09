package testkit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseSSEHandlesRealWorldQuirks(t *testing.T) {
	// 这段输入里的每一个古怪之处都是上游真的会发出来的：
	// 无空格前缀、注释心跳、多行 data、末尾缺空行。
	raw := "event: message_start\n" +
		"data:{\"type\":\"message_start\"}\n" +
		"\n" +
		": keep-alive\n" +
		"\n" +
		"event: content_block_delta\n" +
		"data: line one\n" +
		"data: line two\n" +
		"\n" +
		"data: [DONE]\n"

	evs, err := ParseSSE(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("解析出 %d 个事件，期望 3 个: %+v", len(evs), evs)
	}

	if evs[0].Event != "message_start" || evs[0].Data != `{"type":"message_start"}` {
		t.Errorf("无空格的 data: 前缀解析有误: %+v", evs[0])
	}
	if evs[1].Data != "line one\nline two" {
		t.Errorf("多行 data 应以换行连接，实际 %q", evs[1].Data)
	}
	if evs[2].Data != DoneSentinel {
		t.Errorf("末尾缺空行时最后一个事件仍应有效，实际 %+v", evs[2])
	}
}

// TestParseSSEPreservesExtraSpaces 固化规范细节：只剥掉一个前导空格，
// 多余的空格属于数据。base64 音频块对这类细节很敏感。
func TestParseSSEPreservesExtraSpaces(t *testing.T) {
	evs, err := ParseSSE(strings.NewReader("data:  two spaces\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if evs[0].Data != " two spaces" {
		t.Errorf("应只剥掉一个前导空格，实际 %q", evs[0].Data)
	}
}

func TestSSERoundTrip(t *testing.T) {
	want := []SSEEvent{
		{Event: "a", Data: `{"x":1}`},
		{Event: "b", Data: "multi\nline"},
		{Data: DoneSentinel},
	}

	got, err := ParseSSE(strings.NewReader(EncodeSSE(want)))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("往返后事件数 = %d, 期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("事件 %d 往返后不一致:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

func TestDataEventsStripsDoneSentinel(t *testing.T) {
	evs := []SSEEvent{{Data: "a"}, {Data: DoneSentinel}, {Data: "b"}}
	if got := DataEvents(evs); len(got) != 2 {
		t.Errorf("应剥掉 [DONE]，剩余 %d 个事件", len(got))
	}
}

// TestSanitizeHeadersRedacts 是 fixture 不泄密的第一道防线。
func TestSanitizeHeadersRedacts(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-secret-value")
	h.Set("X-Api-Key", "sk-ant-secret")
	h.Set("Content-Type", "application/json")

	got := SanitizeHeaders(h)

	for _, k := range []string{"authorization", "x-api-key"} {
		if got[k] != "<redacted>" {
			t.Errorf("%s 未脱敏: %q", k, got[k])
		}
	}
	// 留下占位而不是删掉：测试需要知道这里本来有个鉴权头。
	if _, ok := got["authorization"]; !ok {
		t.Error("脱敏后应保留占位，否则无法验证网关确实注入了鉴权")
	}
	if got["content-type"] != "application/json" {
		t.Errorf("非敏感头应保留，实际 %q", got["content-type"])
	}
}

// TestValidateCatchesUnredactedSecret 是第二道防线：即使录制脚本忘了脱敏，
// 入库时也要拦下来。
func TestValidateCatchesUnredactedSecret(t *testing.T) {
	f := Fixture{
		Name:     "leaky",
		Request:  Request{Method: "POST", Path: "/v1/x", Headers: map[string]string{"authorization": "Bearer sk-live-abc"}},
		Response: Response{Status: 200},
	}
	err := f.Validate()
	if err == nil {
		t.Fatal("未脱敏的凭据应当被拦下")
	}
	if !strings.Contains(err.Error(), "未脱敏") {
		t.Errorf("错误信息应指出未脱敏，实际: %v", err)
	}
}

func TestValidateRejectsBodyAndSSETogether(t *testing.T) {
	f := Fixture{
		Name: "both",
		Response: Response{
			Status: 200,
			Body:   json.RawMessage(`{}`),
			SSE:    &SSEBody{Events: []SSEEvent{{Data: "x"}}},
		},
	}
	if err := f.Validate(); err == nil {
		t.Fatal("body 与 sse 互斥，同时出现应当校验失败")
	}
}

func TestValidateRejectsFrameMismatch(t *testing.T) {
	f := Fixture{
		Name: "bad-frames",
		Response: Response{
			Status: 200,
			SSE: &SSEBody{
				Events: []SSEEvent{{Data: "a"}, {Data: "b"}},
				Frames: []int{1, 5},
			},
		},
	}
	if err := f.Validate(); err == nil {
		t.Fatal("frames 合计与事件数不符，应当校验失败")
	}
}

func TestServerReplaysJSON(t *testing.T) {
	f := Fixture{
		Name:    "chat",
		Request: Request{Method: "POST", Path: "/v1/chat/completions"},
		Response: Response{
			Status:  200,
			Headers: map[string]string{"X-Ratelimit-Remaining-Requests": "42"},
			Body:    json.RawMessage(`{"id":"chatcmpl-1"}`),
		},
	}
	srv := Server(t, f)

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("状态码 = %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Ratelimit-Remaining-Requests") != "42" {
		t.Error("录制的响应头未回放——限流头是客户端退避的输入，不能丢")
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"id":"chatcmpl-1"}` {
		t.Errorf("响应体 = %s", body)
	}
}

// TestServerReplaysSSEFrames 验证分片边界被如实复现。
// 转换器的缓冲 bug 只在分片边界上才会暴露，边界丢了测试就失去意义。
func TestServerReplaysSSEFrames(t *testing.T) {
	f := Fixture{
		Name:    "stream",
		Request: Request{Method: "POST", Path: "/v1/messages"},
		Response: Response{
			Status: 200,
			SSE: &SSEBody{
				Events: []SSEEvent{
					{Event: "message_start", Data: `{"type":"message_start"}`},
					{Event: "content_block_delta", Data: `{"delta":{"text":"你"}}`},
					{Event: "content_block_delta", Data: `{"delta":{"text":"好"}}`},
					{Event: "message_stop", Data: `{"type":"message_stop"}`},
				},
				// 前三个事件挤在一次 Write 里，最后一个单独发。
				Frames: []int{3, 1},
			},
		},
	}
	srv := Server(t, f)

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, 期望 text/event-stream", ct)
	}

	evs, err := ParseSSE(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("收到 %d 个事件，期望 4 个", len(evs))
	}
	if evs[1].Data != `{"delta":{"text":"你"}}` {
		t.Errorf("事件内容有误: %+v", evs[1])
	}
}

// TestHandlerFailsOnUnrecordedRequest 固化这个框架最重要的性质：
// 打了没录制的端点必须让测试失败，而不是静默返回 404。
func TestHandlerFailsOnUnrecordedRequest(t *testing.T) {
	var missMethod, missPath string
	h := Handler(func(m, p string) { missMethod, missPath = m, p }, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/unexpected", nil))

	if missMethod != http.MethodPost || missPath != "/v1/unexpected" {
		t.Errorf("未录制的请求应触发 onMiss，实际 %q %q", missMethod, missPath)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("状态码 = %d, 期望 500", rec.Code)
	}
}

func TestFramesDefaultsToOnePerEvent(t *testing.T) {
	sse := &SSEBody{Events: []SSEEvent{{Data: "a"}, {Data: "b"}, {Data: "c"}}}
	got := frames(sse)
	if len(got) != 3 {
		t.Fatalf("未指定 frames 时应逐事件写出，实际 %d 批", len(got))
	}
	for i, batch := range got {
		if len(batch) != 1 {
			t.Errorf("第 %d 批含 %d 个事件，期望 1 个", i, len(batch))
		}
	}
}

// TestLoadRoundTrip 用真实的 fixture 文件验证读写链路。
func TestLoadRoundTrip(t *testing.T) {
	f := Load(t, "../../testdata/fixtures/openai/chat-tool-call-split-args.json")

	if f.Response.SSE == nil {
		t.Fatal("这条 fixture 应当是流式的")
	}
	if len(f.Response.SSE.Events) == 0 {
		t.Fatal("事件列表为空")
	}
	if f.Request.Headers["authorization"] != "<redacted>" {
		t.Error("入库的 fixture 必须已脱敏")
	}

	path := t.TempDir() + "/copy.json"
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	again := Load(t, path)
	if again.Name != f.Name || len(again.Response.SSE.Events) != len(f.Response.SSE.Events) {
		t.Error("保存后重新读取的 fixture 与原件不一致")
	}
}
