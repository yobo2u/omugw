//go:build smoke

package smoke_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

// TestRecordingProxyRefusesSecondCapture 验证一个代理只服务一次捕获：
// 第二次请求既不能顶掉首次的证据，也不能被静默放过。
func TestRecordingProxyRefusesSecondCapture(t *testing.T) {
	const firstBody = `{"model":"qwen-plus","seq":1}`

	var originHits atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// 回声请求体，让「第二次是否顶掉了第一次」在快照里可辨。
		_, _ = w.Write(body)
	}))
	defer origin.Close()

	proxy, state := newRecordingProxy(t, origin.URL)
	client := &http.Client{Timeout: 10 * time.Second}

	post := func(body string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			proxy.URL+nativewire.TextGenerationPath, strings.NewReader(body))
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("请求录制代理失败: %v", err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatalf("读取代理响应失败: %v", err)
		}
		return resp
	}

	if got := post(firstBody).StatusCode; got != http.StatusOK {
		t.Fatalf("首次请求状态码 = %d，期望 200", got)
	}
	if got := post(`{"model":"qwen-plus","seq":2}`).StatusCode; got == http.StatusOK {
		t.Errorf("第二次请求状态码 = %d，期望代理拒绝而不是再捕获一次", got)
	}

	if got := originHits.Load(); got != 1 {
		t.Errorf("源站被调用 %d 次，期望 1 次——第二次请求不该打到真实上游", got)
	}

	snap := state.Snapshot()
	if snap.Err == nil {
		t.Error("快照 err 为 nil，期望记下「一个代理只允许捕获一次」")
	}
	if snap.Requests != 2 {
		t.Errorf("快照 requests = %d，期望 2", snap.Requests)
	}
	if string(snap.Upstream.Body) != firstBody {
		t.Errorf("快照 upstream.body = %q，期望仍是首次的 %q", snap.Upstream.Body, firstBody)
	}
	if string(snap.Response.Body) != firstBody {
		t.Errorf("快照 response.body = %q，期望仍是首次的 %q", snap.Response.Body, firstBody)
	}
}
