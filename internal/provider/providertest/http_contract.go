package providertest

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
)

// runHTTPContract 跑 HTTP 出站特有的不变量。
// WS 适配器接入时不跑这一族，另建 ws_contract.go。
func runHTTPContract(t *testing.T, s Subject) {
	t.Run("请求形状", func(t *testing.T) {
		h := okServer(t)
		if _, err := h.call(t, s, callOpts{}); err != nil {
			t.Fatal(err)
		}
		if h.got.method != http.MethodPost {
			t.Errorf("method = %q，期望 POST", h.got.method)
		}
		if ct := h.got.header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q，期望 application/json", ct)
		}
	})

	// 流式信号丢了，上游会按非流式返回，整条流的语义就变了。
	// DashScope Native 把这个信号放在头上而非请求体里，所以除 Accept 之外
	// 还要查 Subject 声明的额外头。
	t.Run("流式信号落位", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			stream     bool
			wantAccept string
		}{
			{"非流式", false, "application/json"},
			{"流式", true, "text/event-stream"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := okServer(t)
				if _, err := h.call(t, s, callOpts{stream: tc.stream}); err != nil {
					t.Fatal(err)
				}
				if a := h.got.header.Get("Accept"); a != tc.wantAccept {
					t.Errorf("Accept = %q，期望 %q", a, tc.wantAccept)
				}
				for name, want := range s.StreamHeaders {
					got := h.got.header.Get(name)
					if tc.stream && got != want {
						t.Errorf("流式时 %s = %q，期望 %q", name, got, want)
					}
					if !tc.stream && got != "" {
						t.Errorf("非流式时不应带 %s，实际 %q", name, got)
					}
				}
			})
		}
	})

	// 按语义比对而不是按字节：重新序列化会改变键序，钉死字节等于钉死
	// encoding/json 的实现细节，而那不是我们要保证的契约。
	t.Run("模型改写", func(t *testing.T) {
		h := okServer(t)
		if _, err := h.call(t, s, callOpts{}); err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(h.got.body, &fields); err != nil {
			t.Fatalf("上游收到的不是 JSON 对象: %s", h.got.body)
		}
		var model string
		if err := json.Unmarshal(fields["model"], &model); err != nil {
			t.Fatalf("model 不是字符串: %s", fields["model"])
		}
		if model != defaultUpstreamModel {
			t.Errorf("model = %q，期望改写成 %q", model, defaultUpstreamModel)
		}
	})

	// 同一个适配器要服务多个上游端点，路径只能随请求走，不能写死在装配时；
	// 保留默认值是为了不影响既有单端点装配。
	t.Run("路径来源", func(t *testing.T) {
		t.Run("请求路径优先", func(t *testing.T) {
			h := okServer(t)
			const override = "/v1/override/endpoint"
			if _, err := h.call(t, s, callOpts{path: override}); err != nil {
				t.Fatal(err)
			}
			if h.got.path != override {
				t.Errorf("path = %q，期望 %q", h.got.path, override)
			}
		})

		t.Run("留空退回默认", func(t *testing.T) {
			h := okServer(t)
			if _, err := h.call(t, s, callOpts{}); err != nil {
				t.Fatal(err)
			}
			if h.got.path != s.DefaultPath {
				t.Errorf("path = %q，期望默认 %q", h.got.path, s.DefaultPath)
			}
		})
	})

	// 字符串拼接对 URL 组件没有概念，这里要的是结构化拼接的结果：
	// base_url 带前缀时端点必须追加在前缀之后，而不是把前缀截断或吞掉。
	t.Run("base_url 前缀保留", func(t *testing.T) {
		h := okServer(t)
		const prefix = "/proxy/upstream"
		if _, err := h.call(t, s, callOpts{
			baseURL: h.server.URL + prefix,
			path:    s.DefaultPath,
		}); err != nil {
			t.Fatal(err)
		}
		want := prefix + s.DefaultPath
		if h.got.path != want {
			t.Errorf("path = %q，期望前缀保留后追加端点 %q", h.got.path, want)
		}
	})

	runHeaderClosure(t, s)
	runErrorDecoding(t, s)
}

// autoHeaders 是 net/http 自动附加、与适配器无关的头。
//
// 断言它们等于断言标准库的行为，不是我们的契约。适配器自造的头
// （Content-Type / Accept / Authorization / StreamHeaders）不在此列，
// 它们由前面几条不变量正面断言。
var autoHeaders = map[string]bool{
	"Host":            true, // net/http 从 URL 推导
	"User-Agent":      true, // net/http 默认填充
	"Content-Length":  true, // net/http 按 body 长度计算
	"Accept-Encoding": true, // Transport 自动协商
}

// gatewayOwnHeaders 是适配器自己设的头，允许出现在上游请求里。
var gatewayOwnHeaders = map[string]bool{
	"Content-Type":  true,
	"Accept":        true,
	"Authorization": true,
}

// probeHeaders 是套件注入的、必定不该被转发的客户端头。
//
// 除了明显的伪造凭据与自定义头，还刻意放进**其他 Subject 的转发白名单**里
// 出现过的头：那样才能抓到「A 适配器把 B 适配器的白名单也一起转了」这种错。
var probeHeaders = []string{
	"X-Client-Custom",
	"X-Forwarded-For",
	"X-DashScope-WorkSpace",
	"X-DashScope-DataInspection",
	"X-DashScope-Async",
	"X-DashScope-SSE",
}

// runHeaderClosure 用严格闭集断言头转发。
//
// 上游收到的头减去标准库自动头之后，必须恰好等于网关自造头 ∪ ForwardedHeaders。
// 不用「注入几个头再断言它们没出现」的抽样式黑名单：抽样只能证明想到的那几个
// 没漏，证明不了没想到的那些。新适配器悄悄多转一个客户端头，抽样抓不到，闭集能。
func runHeaderClosure(t *testing.T, s Subject) {
	t.Run("头转发闭集", func(t *testing.T) {
		h := okServer(t)

		allowed := map[string]bool{}
		for k := range gatewayOwnHeaders {
			allowed[http.CanonicalHeaderKey(k)] = true
		}
		for _, name := range s.ForwardedHeaders {
			allowed[http.CanonicalHeaderKey(name)] = true
		}
		// StreamHeaders 刻意**不**进允许集：这一次是非流式调用，流式头没有任何
		// 正当理由出现在上游请求里。而流式头也在 probeHeaders 里，客户端伪造的
		// 那一份一旦被适配器原样转走，闭集才抓得到；把它算进允许集等于给这条
		// 泄漏开一个后门，测试照样全绿。「非流式时适配器不自己设流式头」由
		// 「流式信号落位」那条不变量管，那条用的是空头，证明不了不转发。

		// 注入全部探针头。声明为转发的那些，期望原样到达；其余一个都不该出现。
		clientHeader := http.Header{}
		clientHeader.Set("Authorization", "Bearer "+clientSecret)
		for _, name := range probeHeaders {
			clientHeader.Set(name, "probe-"+name)
		}

		if _, err := h.call(t, s, callOpts{header: clientHeader}); err != nil {
			t.Fatal(err)
		}

		for name := range h.got.header {
			canonical := http.CanonicalHeaderKey(name)
			if autoHeaders[canonical] {
				continue
			}
			if !allowed[canonical] {
				t.Errorf("上游收到了未声明的头 %s: %q（闭集之外）",
					canonical, h.got.header.Get(name))
			}
		}

		// 声明为转发的头必须真的到了——闭集只能证明「没多」，这里证明「没少」。
		for _, name := range s.ForwardedHeaders {
			want := "probe-" + name
			if got := h.got.header.Get(name); got != want {
				t.Errorf("声明转发的头 %s = %q，期望 %q", name, got, want)
			}
		}
	})
}

// runErrorDecoding 断言非 2xx 被解成统一错误。
func runErrorDecoding(t *testing.T, s Subject) {
	// Retryable 为真的语义是「换一个凭据或 Provider 可能成功」，
	// 不是「上游临时故障」——这两者常被混为一谈。
	t.Run("限流错误解码", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, s.RateLimitEnvelope)
		})

		_, err := h.call(t, s, callOpts{})
		if err == nil {
			t.Fatal("上游 429 应当返回错误")
		}
		var cerr *canonical.Error
		if !errors.As(err, &cerr) {
			t.Fatalf("应返回 *canonical.Error，实际 %T", err)
		}
		if cerr.Class != canonical.ClassRateLimit {
			t.Errorf("分类 = %q，期望 rate_limit", cerr.Class)
		}
		if !cerr.Retryable {
			t.Error("限流应可重试（换一份凭据可能成功）")
		}
		// Retry-After 必须活着传到凭据池——它决定冷却多久。
		if cerr.RetryAfter != 7*time.Second {
			t.Errorf("RetryAfter = %v，期望 7s", cerr.RetryAfter)
		}
		if cerr.UpstreamStatus != http.StatusTooManyRequests {
			t.Errorf("UpstreamStatus = %d，期望 429", cerr.UpstreamStatus)
		}
	})

	// 一个故障上游可能在 500 里塞进一整个 HTML 页面，甚至更糟。
	// 适配器必须能读完就走、不被拖死，且**分类不因体不可解析而退化**——
	// 状态码本身就是可靠信号。
	//
	// 这里刻意不断言 len(Message) 的上限。实测过：不可解析的体走的是
	// 「解析失败 → 回退 http.StatusText(status)」这条路，Message 恒为 21 字节
	//（"Internal Server Error"），无论读取上限是 64 KiB、1 MiB 还是根本没有
	// 上限，长度断言都通过——它抓不到任何东西。读取上限是 wire 层的实现细节，
	// 该由 openaiwire / dashscopewire 自己的测试守住；适配器这一层真正的契约
	// 是「垃圾进来，分类不许乱」。
	t.Run("不可解析的超大体不让分类退化", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, strings.Repeat("x", 1<<20))
		})

		_, err := h.call(t, s, callOpts{})
		if err == nil {
			t.Fatal("上游 500 应当返回错误")
		}
		var cerr *canonical.Error
		if !errors.As(err, &cerr) {
			t.Fatalf("应返回 *canonical.Error，实际 %T", err)
		}
		if cerr.Class != canonical.ClassUpstreamUnavailable {
			t.Errorf("分类 = %q，期望 upstream_unavailable", cerr.Class)
		}
		if cerr.UpstreamStatus != http.StatusInternalServerError {
			t.Errorf("UpstreamStatus = %d，期望 500", cerr.UpstreamStatus)
		}
	})
}
