//go:build smoke

package smoke_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/sse"
)

type readErrorAfterBytes struct {
	data *strings.Reader
	err  error
}

func (r *readErrorAfterBytes) Read(p []byte) (int, error) {
	if r.data.Len() > 0 {
		return r.data.Read(p)
	}
	return 0, r.err
}

// TestRecordingProxyRelaysSSEEventByEvent 验证流式路径：事件逐条转发，
// 帧边界按事件登记，而不是按底层字节块。
func TestRecordingProxyRelaysSSEEventByEvent(t *testing.T) {
	const originSecret = "sk-fake-origin-secret-24680"

	originEvents := []testkit.SSEEvent{
		{Event: "result", Data: `{"output":{"choices":[{"message":{"content":"你"}}]}}`},
		{Event: "result", Data: `{"output":{"choices":[{"message":{"content":"好"}}]}}`},
	}

	originCh := make(chan originCapture, 2)
	send := func(c originCapture) {
		select {
		case originCh <- c:
		default:
		}
	}

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			send(originCapture{err: fmt.Errorf("读取源站请求体失败: %w", err)})
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		send(originCapture{
			auth:      r.Header.Get("Authorization"),
			path:      r.URL.Path,
			sseHeader: r.Header.Get(nativewire.SSEHeader),
			body:      string(body),
		})

		sw, err := sse.NewWriter(w)
		if err != nil {
			send(originCapture{err: fmt.Errorf("源站构造 SSE writer 失败: %w", err)})
			http.Error(w, "sse writer error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		for _, ev := range originEvents {
			// Write 内部逐条 flush，两条事件因此分两次抵达代理。
			if err := sw.Write(ev); err != nil {
				send(originCapture{err: fmt.Errorf("源站写出事件失败: %w", err)})
				return
			}
		}
	}))
	defer origin.Close()

	proxy, state := newRecordingProxy(t, origin.URL)

	const gatewayBody = `{"model":"qwen-plus","input":{"messages":[{"role":"user","content":"你好"}]}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		proxy.URL+nativewire.TextGenerationPath, strings.NewReader(gatewayBody))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+originSecret)
	req.Header.Set(nativewire.SSEHeader, "enable")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求录制代理失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("下游状态码 = %d，期望 200: %s", resp.StatusCode, raw)
	}

	// 断言侧一次读完即可；被验证的「逐事件即时转发」由快照的 frames 举证。
	downstream, err := testkit.ParseSSE(resp.Body)
	if err != nil {
		t.Fatalf("解析下游 SSE 失败: %v", err)
	}
	if len(downstream) != len(originEvents) {
		t.Fatalf("下游事件数 = %d，期望 %d: %+v", len(downstream), len(originEvents), downstream)
	}
	for i, want := range originEvents {
		if downstream[i].Event != want.Event || downstream[i].Data != want.Data {
			t.Errorf("下游第 %d 条事件 = %+v，期望 %+v", i, downstream[i], want)
		}
	}

	select {
	case captured := <-originCh:
		if captured.err != nil {
			t.Fatalf("假源站失败: %v", captured.err)
		}
		if want := "Bearer " + originSecret; captured.auth != want {
			t.Errorf("源站 Authorization = %q，期望原样透传的 %q", captured.auth, want)
		}
		if captured.sseHeader != "enable" {
			t.Errorf("源站 %s = %q，期望 enable", nativewire.SSEHeader, captured.sseHeader)
		}
		if captured.body != gatewayBody {
			t.Errorf("源站请求体 = %q，期望 %q", captured.body, gatewayBody)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待假源站收到请求超时")
	}

	snap := state.Snapshot()
	if snap.Err != nil {
		t.Fatalf("代理处理器出错: %v", snap.Err)
	}
	if snap.Response.SSE == nil {
		t.Fatal("快照 response.sse 为 nil，期望捕获到事件流")
	}
	if len(snap.Response.SSE.Events) != len(originEvents) {
		t.Fatalf("快照事件数 = %d，期望 %d", len(snap.Response.SSE.Events), len(originEvents))
	}
	for i, want := range originEvents {
		if got := snap.Response.SSE.Events[i]; got.Event != want.Event || got.Data != want.Data {
			t.Errorf("快照第 %d 条事件 = %+v，期望 %+v", i, got, want)
		}
	}
	if want := []int{1, 1}; !slices.Equal(snap.Response.SSE.Frames, want) {
		t.Errorf("快照 frames = %v，期望 %v", snap.Response.SSE.Frames, want)
	}
	if snap.Response.Body != nil {
		t.Errorf("快照 response.body 应为 nil（与 sse 互斥），实际 %q", snap.Response.Body)
	}
	if got := snap.Upstream.Headers["authorization"]; got != "<redacted>" {
		t.Errorf("快照 upstream.headers[authorization] = %q，期望 <redacted>", got)
	}
	if got := snap.Upstream.Headers[strings.ToLower(nativewire.SSEHeader)]; got != "enable" {
		t.Errorf("快照 upstream.headers[%s] = %q，期望 enable", strings.ToLower(nativewire.SSEHeader), got)
	}
}

// TestRecordingProxyDistinguishesConsumerCancellation 防止网关主动拒收上游首帧时，
// 录制代理用连锁产生的 context.Canceled 覆盖真正的网关错误。
func TestRecordingProxyDistinguishesConsumerCancellation(t *testing.T) {
	event := "event: result\ndata: {\"output\":{\"choices\":[{}]}}\n\n"

	t.Run("下游已取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://origin.test", nil)
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Request:    req,
			Body: io.NopCloser(&readErrorAfterBytes{
				data: strings.NewReader(event),
				err:  context.Canceled,
			}),
		}
		state := &recordingState{}

		relayRecordedSSE(httptest.NewRecorder(), resp, state)

		snap := state.Snapshot()
		if snap.Err != nil {
			t.Fatalf("下游主动取消不应记成录制代理错误: %v", snap.Err)
		}
		if snap.Response.SSE == nil || len(snap.Response.SSE.Events) != 1 {
			t.Fatalf("取消前已收到的事件必须保留: %+v", snap.Response.SSE)
		}
	})

	t.Run("下游仍活跃", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://origin.test", nil)
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Request:    req,
			Body: io.NopCloser(&readErrorAfterBytes{
				data: strings.NewReader(event),
				err:  context.Canceled,
			}),
		}
		state := &recordingState{}

		relayRecordedSSE(httptest.NewRecorder(), resp, state)

		if snap := state.Snapshot(); snap.Err == nil {
			t.Fatal("下游仍活跃时的 context.Canceled 必须保留为录制代理错误")
		}
	})
}
