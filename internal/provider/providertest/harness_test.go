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

	hit(t, h, http.MethodPost, "/v1/x", "v", `{"probe":"body"}`)

	if h.got.method != http.MethodPost {
		t.Errorf("method = %q，期望 POST", h.got.method)
	}
	if h.got.path != "/v1/x" {
		t.Errorf("path = %q，期望 /v1/x", h.got.path)
	}
	if h.got.header.Get("X-Probe") != "v" {
		t.Errorf("头未被捕获: %v", h.got.header)
	}
	if got := string(h.got.body); got != `{"probe":"body"}` {
		t.Errorf("body 未被捕获: %s", got)
	}
}

// TestHarnessHandsBodyToStub 钉住「桩 handler 拿得到原始请求体」。
//
// 防的是夹具把体读干净了才交给桩：捕获用的 io.ReadAll 会耗尽 r.Body，桩再读就
// 只剩空串。于是一个想按请求体分支应答（或原样回显）的 Subject 桩会静默拿到
// ""，照着空体做出错误响应——夹具自己成了给出错误答案的那个，正是本文件开头
// 那句「比没有夹具更糟」说的情形。
func TestHarnessHandsBodyToStub(t *testing.T) {
	var seen string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("桩读取请求体失败: %v", err)
		}
		seen = string(b)
		_, _ = io.WriteString(w, `{}`)
	})

	hit(t, h, http.MethodPost, "/v1/x", "v", `{"seq":"first"}`)

	if seen != `{"seq":"first"}` {
		t.Errorf("桩看到的请求体 = %q，期望 %q", seen, `{"seq":"first"}`)
	}
}

// TestHarnessRejectsSecondUpstreamRequest 钉住「第二次上游请求当场报错，且不覆盖第一次」。
//
// 防的是静默覆盖：httpx.Client 默认跟随重定向，一个 3xx 桩会让同一个夹具收到
// 两次请求，末次写入会把第一次的 method/path/header 冲掉。断言于是读到第二跳
// 的内容，却以为那就是适配器发出的东西——一个「证明上游收到了什么」的夹具给
// 出错误答案，比没有夹具更糟。所以多出来的请求必须响，而不是把前一次盖掉。
func TestHarnessRejectsSecondUpstreamRequest(t *testing.T) {
	// seen 逐次记下桩读到的请求体。防的是「只有第一跳把体交回桩」：还体那一行
	// 若挪到首跳的 Unlock 之后，多出来的那一跳就只剩空体，而此前整套测试照样
	// 全绿——一条声明过的契约没有任何护栏，正是本文件要消灭的那种隐形缺口。
	var mu sync.Mutex
	var seen []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("桩读取请求体失败: %v", err)
		}
		mu.Lock()
		seen = append(seen, string(b))
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	})

	// 把告警出口换掉，才能在不失败本测试的前提下断言它确实响了。
	// 出口留成字段的理由就在这里：否则这条错误路径本身永远没人测。
	var alarms []string
	// 这次替换发生在任何请求之前，本已 happens-before 桩的读；加锁是照着
	// 「extra 归 mu 管」的规矩走，免得日后有人挪动它时才发现少了同步。
	h.got.mu.Lock()
	h.got.extra = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		alarms = append(alarms, fmt.Sprintf(format, args...))
	}
	h.got.mu.Unlock()

	hit(t, h, http.MethodPost, "/v1/first", "first", `{"seq":"first"}`)
	hit(t, h, http.MethodPut, "/v1/second", "second", `{"seq":"second"}`)

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
	if got := string(h.got.body); got != `{"seq":"first"}` {
		t.Errorf("body = %q，期望仍是第一次的 %q", got, `{"seq":"first"}`)
	}

	// 两跳都得把体交回桩，第二跳读到的必须是它自己那份。
	if len(seen) != 2 {
		t.Fatalf("桩应当被调用两次，实得 %d 次: %q", len(seen), seen)
	}
	if seen[0] != `{"seq":"first"}` {
		t.Errorf("桩第一次读到 %q，期望 %q", seen[0], `{"seq":"first"}`)
	}
	if seen[1] != `{"seq":"second"}` {
		t.Errorf("桩第二次读到 %q，期望 %q", seen[1], `{"seq":"second"}`)
	}
}

// hit 向夹具打一次请求并读完响应体，避免连接悬着影响后续断言。
//
// 四项可辨识输入（method / path / probe / body）都由调用方给，且每次都传不同的
// 值：防的是「两次请求在某一维上长得一样」——那一维即便被覆盖了，断言也照样绿，
// 于是那一维的 first-write-wins 实际上没人验证过。
func hit(t *testing.T, h *harness, method, path, probe, body string) {
	t.Helper()

	req, err := http.NewRequest(method, h.server.URL+path, strings.NewReader(body))
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
