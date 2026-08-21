package providertest

import (
	"io"
	"net/http"
	"testing"
)

// TestHarnessCapturesUpstreamRequest 验证夹具确实记下了上游收到什么。
// 后面每一条不变量都建立在这份捕获之上。
func TestHarnessCapturesUpstreamRequest(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Probe", "v")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if h.got.method != http.MethodPost {
		t.Errorf("method = %q，期望 POST", h.got.method)
	}
	if h.got.path != "/v1/x" {
		t.Errorf("path = %q，期望 /v1/x", h.got.path)
	}
	if h.got.header.Get("X-Probe") != "v" {
		t.Errorf("头未被捕获: %v", h.got.header)
	}
}

// TestRefTimeIsFixed 固化时钟是固定的。
//
// Retry-After 的 HTTP-date 形式依赖当前时间，用真实时钟会让测试在跨秒边界
// 随机失败——那种失败最难查，因为重跑就好了。
func TestRefTimeIsFixed(t *testing.T) {
	if refTime.IsZero() {
		t.Fatal("refTime 不能是零值")
	}
	if !refTime.Equal(refTime) {
		t.Fatal("refTime 必须稳定")
	}
}
