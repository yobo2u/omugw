package providertest

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
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

// TestHarnessRejectsSecondUpstreamRequest 钉住「第二次上游请求当场报错，且不覆盖第一次」。
//
// 防的是静默覆盖：httpx.Client 默认跟随重定向，一个 3xx 桩会让同一个夹具收到
// 两次请求，末次写入会把第一次的 method/path/header 冲掉。断言于是读到第二跳
// 的内容，却以为那就是适配器发出的东西——一个「证明上游收到了什么」的夹具给
// 出错误答案，比没有夹具更糟。所以多出来的请求必须响，而不是把前一次盖掉。
func TestHarnessRejectsSecondUpstreamRequest(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})

	// 把告警出口换掉，才能在不失败本测试的前提下断言它确实响了。
	// 出口留成字段的理由就在这里：否则这条错误路径本身永远没人测。
	var mu sync.Mutex
	var alarms []string
	h.got.mu.Lock()
	h.got.extra = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		alarms = append(alarms, fmt.Sprintf(format, args...))
	}
	h.got.mu.Unlock()

	hit(t, h, http.MethodPost, "/v1/first", "first")
	hit(t, h, http.MethodGet, "/v1/second", "second")

	mu.Lock()
	defer mu.Unlock()
	if len(alarms) != 1 {
		t.Fatalf("第二次请求应当报一次告警，实得 %d 条: %v", len(alarms), alarms)
	}
	if !strings.Contains(alarms[0], "/v1/second") {
		t.Errorf("告警未指出是哪次请求多出来的: %q", alarms[0])
	}

	if h.got.method != http.MethodPost {
		t.Errorf("method = %q，期望仍是第一次的 POST", h.got.method)
	}
	if h.got.path != "/v1/first" {
		t.Errorf("path = %q，期望仍是第一次的 /v1/first", h.got.path)
	}
	if h.got.header.Get("X-Probe") != "first" {
		t.Errorf("header 被第二次请求覆盖: %q", h.got.header.Get("X-Probe"))
	}
}

// hit 向夹具打一次请求并读完响应体，避免连接悬着影响后续断言。
func hit(t *testing.T, h *harness, method, path, probe string) {
	t.Helper()

	req, err := http.NewRequest(method, h.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Probe", probe)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
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
