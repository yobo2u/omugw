//go:build smoke

package smoke_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

// originCapture 是本地假源站捕获到的请求，或它自身失败的原因。
type originCapture struct {
	auth      string
	path      string
	query     string
	keepAlive string
	workspace string
	sseHeader string
	body      string
	err       error
}

// TestRecordingProxyRelaysJSONAndCapturesSanitizedUpstream 验证非流式路径：
// 真实凭据原样送达源站，落进快照的却是脱敏后的头，且源站的请求 ID 与 Cookie
// 一个都不许进快照。
func TestRecordingProxyRelaysJSONAndCapturesSanitizedUpstream(t *testing.T) {
	const (
		originSecret = "sk-fake-origin-secret-13579"
		originBody   = `{"output":{"text":"你好"},"request_id":"req-origin-json-001"}`
		gatewayBody  = `{"model":"qwen-plus","input":{"messages":[{"role":"user","content":"你好"}]}}`
		rawQuery     = "api-version=2024-01-01&trace=on"
	)

	// 容量 2 且非阻塞发送：源站写响应失败时还要再报一次，写满就丢弃，
	// 绝不能让服务端 goroutine 卡在 channel 上把测试拖成超时。
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
			query:     r.URL.RawQuery,
			keepAlive: r.Header.Get("Keep-Alive"),
			workspace: r.Header.Get("X-DashScope-WorkSpace"),
			body:      string(body),
		})

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-origin-json-001")
		w.Header().Set("Set-Cookie", "sid=must-not-be-recorded; Path=/")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, originBody); err != nil {
			send(originCapture{err: fmt.Errorf("写出源站响应失败: %w", err)})
		}
	}))
	defer origin.Close()

	proxy, state := newRecordingProxy(t, origin.URL)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		proxy.URL+nativewire.TextGenerationPath+"?"+rawQuery, strings.NewReader(gatewayBody))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+originSecret)
	req.Header.Set("X-DashScope-WorkSpace", "ws-smoke-1")
	// 逐跳头不该跨代理转发，这里塞一个用于举证。
	req.Header.Set("Keep-Alive", "timeout=5")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求录制代理失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	downstream, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取代理响应体失败: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("下游状态码 = %d，期望 200: %s", resp.StatusCode, downstream)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("下游 Content-Type = %q，期望 application/json", got)
	}
	if string(downstream) != originBody {
		t.Errorf("下游响应体 = %q，期望 %q", downstream, originBody)
	}

	select {
	case captured := <-originCh:
		if captured.err != nil {
			t.Fatalf("假源站失败: %v", captured.err)
		}
		if want := "Bearer " + originSecret; captured.auth != want {
			t.Errorf("源站 Authorization = %q，期望原样透传的 %q", captured.auth, want)
		}
		if captured.path != nativewire.TextGenerationPath {
			t.Errorf("源站路径 = %q，期望 %q", captured.path, nativewire.TextGenerationPath)
		}
		if captured.query != rawQuery {
			t.Errorf("源站查询串 = %q，期望 %q", captured.query, rawQuery)
		}
		if captured.body != gatewayBody {
			t.Errorf("源站请求体 = %q，期望 %q", captured.body, gatewayBody)
		}
		if captured.workspace != "ws-smoke-1" {
			t.Errorf("源站 X-DashScope-WorkSpace = %q，期望 ws-smoke-1", captured.workspace)
		}
		if captured.keepAlive != "" {
			t.Errorf("源站收到逐跳头 Keep-Alive = %q，期望被代理剥掉", captured.keepAlive)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待假源站收到请求超时")
	}

	snap := state.Snapshot()
	if snap.Err != nil {
		t.Fatalf("代理处理器出错: %v", snap.Err)
	}
	if snap.Upstream.Method != http.MethodPost {
		t.Errorf("快照 upstream.method = %q，期望 POST", snap.Upstream.Method)
	}
	if snap.Upstream.Path != nativewire.TextGenerationPath {
		t.Errorf("快照 upstream.path = %q，期望 %q", snap.Upstream.Path, nativewire.TextGenerationPath)
	}
	if got := snap.Upstream.Headers["authorization"]; got != "<redacted>" {
		t.Errorf("快照 upstream.headers[authorization] = %q，期望 <redacted>", got)
	}
	if got := snap.Upstream.Headers["x-dashscope-workspace"]; got != "ws-smoke-1" {
		t.Errorf("快照 upstream.headers[x-dashscope-workspace] = %q，期望 ws-smoke-1", got)
	}
	// 逐跳头确实抵达了代理——上面「源站没收到」才是剥离的证据，而不是发送端没发。
	if got := snap.Upstream.Headers["keep-alive"]; got != "timeout=5" {
		t.Errorf("快照 upstream.headers[keep-alive] = %q，期望 timeout=5", got)
	}
	if string(snap.Upstream.Body) != gatewayBody {
		t.Errorf("快照 upstream.body = %q，期望 %q", snap.Upstream.Body, gatewayBody)
	}

	if snap.Response.Status != http.StatusOK {
		t.Errorf("快照 response.status = %d，期望 200", snap.Response.Status)
	}
	if string(snap.Response.Body) != originBody {
		t.Errorf("快照 response.body = %q，期望 %q", snap.Response.Body, originBody)
	}
	if snap.Response.SSE != nil {
		t.Errorf("快照 response.sse 应为 nil，实际 %+v", snap.Response.SSE)
	}
	if got := snap.Response.Headers["content-type"]; got != "application/json" {
		t.Errorf("快照 response.headers[content-type] = %q，期望 application/json", got)
	}
	for k := range snap.Response.Headers {
		switch strings.ToLower(k) {
		case "x-request-id", "set-cookie":
			t.Errorf("快照 response.headers 混入了不该留存的 %q", k)
		}
	}
}
