package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/provider"
	dsnativeprovider "github.com/yobo2u/omugw/internal/provider/dashscopenative"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// usageRig 是本文件自建的一套网关，与 harness 只差一处，而这一处全在要害上：
// Prometheus registry 由测试自己持有。
//
// harness 把 registry 建在构造函数里就丢了，而这里每条用例都要按 label 逐项
// 对账 omugw_tokens_total——拿不到 registry 就只能把断言降格成「有没有报错」，
// 那等于把「这次请求到底记了多少 token」整件事从测试里删掉，而它正是本任务的
// 全部内容。每条用例各持一个全新 registry 也是同一个理由：计数器单调累加，
// 共用一个的话相邻用例的数字会叠进来，绝对值断言只能靠运行顺序碰运气。
type usageRig struct {
	h    *Handler
	reg  *prometheus.Registry
	path string
}

// usageRigConfig 是一套 usageRig 的完整装配声明。
//
// 「矩阵 + 候选 + 各候选的适配器」是必须同时成立的一组选择，散成位置参数后
// 调用点读不出谁是谁，加一维就要改全部调用点。
type usageRigConfig struct {
	matrix  *degrade.Matrix
	targets []router.Target

	// provs 按 target.Endpoint 索引出站适配器，由用例自己挑：有的用例要真
	// Composite 走完整转换（证 relay 真的从响应体里取到了权威用量），有的要
	// 假适配器精确编排失败与回调（证失败尝试的回调状态不会漏出去）。
	provs map[string]provider.Provider

	// credIDs 是每个候选的凭据池里放哪几份凭据，留空即单份 "k1"。
	//
	// 留这个口子是因为「换候选」与「换凭据」是 dispatch 里两层不同的循环，
	// 而单份凭据的池子只走得到外层：内层那个 for 拿不到第二份凭据就直接
	// break 了。想证内层循环上的任何性质，就必须能往池子里多放一份。
	credIDs []string
}

func newUsageRig(t *testing.T, cfg usageRigConfig) *usageRig {
	t.Helper()

	ids := cfg.credIDs
	if len(ids) == 0 {
		ids = []string{"k1"}
	}

	pools := map[string]*credential.Pool{}
	for _, tg := range cfg.targets {
		creds := make([]credential.Credential, 0, len(ids))
		for _, id := range ids {
			// 密钥带上池名与凭据 ID：池内 ID 重复或密钥为空都会被 NewPool 拒收，
			// 而那种失败读起来像装配 bug，与用例要证的事毫无关系。
			creds = append(creds, credential.Credential{
				ID:     id,
				Secret: "sk-" + tg.CredentialPool + "-" + id,
			})
		}
		pool, err := credential.NewPool(tg.CredentialPool, creds,
			credential.DefaultPolicy(), nil)
		if err != nil {
			t.Fatalf("装配凭据池失败: %v", err)
		}
		pools[tg.CredentialPool] = pool
	}

	rt, err := router.New([]router.Rule{{Match: "*", Targets: cfg.targets}})
	if err != nil {
		t.Fatalf("装配路由失败: %v", err)
	}

	reg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(reg)

	return &usageRig{
		reg:  reg,
		path: string(degrade.EndpointOpenAIChat),
		h: NewChatHandler(Deps{
			Matrix:    cfg.matrix,
			Router:    rt,
			Auth:      NewAuthenticator([]config.AuthKey{{ID: "tester", Key: testKey}}),
			Limits:    config.Default().Limits,
			Metrics:   metrics,
			Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			Pools:     pools,
			Providers: cfg.provs,
		}),
	}
}

func (rig *usageRig) do(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, rig.path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rig.h.ServeHTTP(rec, req)
	return rec
}

// counters 采集本 rig 某个计数器的全部样本，键由 keyLabels 指定的标签值按序拼成。
//
// 按 label 取值而不是数样本个数：本任务要钉死的正是「记进去的是哪一个数字」，
// 只数样本个数的话，把回调值错当成 relay 值记进去照样是一个样本。
func (rig *usageRig) counters(t *testing.T, name string, keyLabels ...string) map[string]float64 {
	t.Helper()
	families, err := rig.reg.Gather()
	if err != nil {
		t.Fatalf("采集指标失败: %v", err)
	}
	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			values := map[string]string{}
			for _, l := range m.GetLabel() {
				values[l.GetName()] = l.GetValue()
			}
			parts := make([]string, 0, len(keyLabels))
			for _, k := range keyLabels {
				parts = append(parts, values[k])
			}
			out[strings.Join(parts, "/")] = m.GetCounter().GetValue()
		}
	}
	return out
}

// tokens 采集 omugw_tokens_total，键为 "fidelity/kind"。
func (rig *usageRig) tokens(t *testing.T) map[string]float64 {
	t.Helper()
	return rig.counters(t, "omugw_tokens_total", "fidelity", "kind")
}

// streamAborted 采集 omugw_stream_aborted_total，键为错误分类。
func (rig *usageRig) streamAborted(t *testing.T) map[string]float64 {
	t.Helper()
	return rig.counters(t, "omugw_stream_aborted_total", "class")
}

// decoyUsage 是刻意与真实用量不同的诱饵值。
//
// 用例把它塞进回调，好让「relay 结果压过回调」这条真的被证过一次：诱饵与真值
// 相同的话，断言无论优先级写反没写反都绿，等于没测。
func decoyUsage() canonical.Usage {
	return canonical.Usage{
		Fidelity:     canonical.FidelityAuthoritative,
		InputTokens:  999,
		OutputTokens: 888,
	}
}

// nativeTarget 造一个指向 door 门的 DashScope Native 候选。
func nativeTarget(name, baseURL, door string) router.Target {
	return router.Target{
		Kind:           degrade.ProviderDashScopeNative,
		Endpoint:       name,
		BaseURL:        baseURL,
		UpstreamModel:  "upstream-model",
		CredentialPool: name,
		NativeEndpoint: door,
	}
}

// nativeComposite 造一个真 Composite 适配器，与 build.go 用同一个构造函数。
func nativeComposite() provider.Provider {
	return dsnativeprovider.New(httpx.New(config.Timeouts{
		Connect:   200 * time.Millisecond,
		FirstByte: 1 * time.Second,
		Total:     10 * time.Second,
		Idle:      500 * time.Millisecond,
	}, nil), nil)
}

// usageProbe 把真适配器包一层，观测回调注入并在内层返回**之后**补发诱饵值。
//
// 补在后面是要害：Composite 自己会在 Call 内回调一次真实用量，探针若抢在前面，
// 「后值覆盖前值」会让真实值成为回调末值——于是即便 handler 错误地拿回调盖掉
// relay 的权威用量，断言也照样绿。
type usageProbe struct {
	inner provider.Provider

	// after 是内层返回后补发的诱饵值，必须与真实用量不同。
	after []canonical.Usage

	calls  atomic.Int32
	withCB atomic.Int32
}

func (p *usageProbe) Kind() degrade.Provider { return p.inner.Kind() }

func (p *usageProbe) Call(ctx context.Context, req provider.Request) (*httpx.Response, error) {
	p.calls.Add(1)
	if req.OnDashScopeUsage != nil {
		p.withCB.Add(1)
	}
	resp, err := p.inner.Call(ctx, req)
	if req.OnDashScopeUsage != nil {
		for _, u := range p.after {
			req.OnDashScopeUsage(u)
		}
	}
	return resp, err
}

// staticProvider 是可编排的假出站适配器：不打网络，按预置交出响应或错误，
// 并在回调非 nil 时按顺序发出用量。
//
// 用假的而不是真 Composite，是因为剩下两条用例要证的全是 handler 自己的行为
// ——回调注没注入、失败尝试的回调状态会不会漏进下一次尝试。真适配器会把线格式、
// 门与整套转换一起拖进来，任何一处不对都会红在与被测行为无关的地方。
type staticProvider struct {
	kind degrade.Provider

	// report 是 Call 内按顺序发给回调的用量。回调为 nil 时一个都不发——
	// 这正是「非 Native 不得注入」那条断言的行为面：注入错了，诱饵就会进指标。
	report []canonical.Usage

	// body 是成功时交给 relay 的响应体；err 非 nil 时直接失败，不产出响应。
	body string
	err  error

	calls  atomic.Int32
	withCB atomic.Int32
}

func (p *staticProvider) Kind() degrade.Provider { return p.kind }

func (p *staticProvider) Call(_ context.Context, req provider.Request) (*httpx.Response, error) {
	p.calls.Add(1)
	if req.OnDashScopeUsage != nil {
		p.withCB.Add(1)
		for _, u := range p.report {
			req.OnDashScopeUsage(u)
		}
	}
	if p.err != nil {
		return nil, p.err
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	return &httpx.Response{
		Response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(p.body)),
		},
	}, nil
}

// nativeStreamUpstream 起一个逐帧吐 Native SSE 的假上游。
//
// 帧一律按 event: result 发：Native 流只有这一个事件名，别的名字会被适配器
// fail-closed 挡掉，那时红的是「事件名不对」，而不是本文件要证的用量优先级。
func nativeStreamUpstream(t *testing.T, frames []string) *upstream {
	t.Helper()
	return newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, canFlush := w.(http.Flusher)
		for _, f := range frames {
			_, _ = io.WriteString(w, "event: result\ndata: "+f+"\n\n")
			if canFlush {
				flusher.Flush()
			}
		}
	})
}

// TestUsageFromResponseBody 钉死优先级①：relay 已给出权威用量就用它，不许被回调盖掉。
//
// 非流式 Native 响应自带 usage，Composite 把它翻译进 Chat 响应体，relay 再从体里
// 取出——这是真实链路。探针在 Composite 返回后再补一个**刻意不同**的诱饵值进回调，
// 于是「记进指标的到底是谁」有了唯一答案：记成 22/17 才说明 relay 赢了，
// 记成诱饵的 999/888 就说明优先级写反了。
func TestUsageFromResponseBody(t *testing.T) {
	up := jsonUpstream(t, `{"output":{"choices":[{"finish_reason":"stop",`+
		`"message":{"role":"assistant","content":"好"}}]},`+
		`"usage":{"input_tokens":22,"output_tokens":17},"request_id":"req-1"}`)

	probe := &usageProbe{inner: nativeComposite(), after: []canonical.Usage{decoyUsage()}}
	target := nativeTarget("a", up.srv.URL, "text-generation")
	rig := newUsageRig(t, usageRigConfig{
		matrix:  testLocalMatrix(t, canonical.CapTextGeneration),
		targets: []router.Target{target},
		provs:   map[string]provider.Provider{target.Endpoint: probe},
	})

	rec := rig.do(t, `{"model":"m","messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应体: %s", rec.Code, rec.Body.String())
	}
	if n := probe.withCB.Load(); n != 1 {
		t.Fatalf("Native 尝试观测到非 nil 回调 %d 次，期望 1——回调没被注入", n)
	}

	got := rig.tokens(t)
	if got["authoritative/input"] != 22 || got["authoritative/output"] != 17 {
		t.Errorf("token 记账 = %v，期望 authoritative 22/17（relay 从响应体取到的权威用量）", got)
	}
	if got["authoritative/input"] == float64(decoyUsage().InputTokens) {
		t.Error("记进去的是回调诱饵值：回调不得盖掉 relay 已给出的权威用量")
	}
}

// TestUsageFromCallbackWhenRelayLacks 钉死优先级②：relay 无错误却取不到用量时，
// 用回调**末值**。
//
// 客户端没要 stream_options.include_usage，转换后的 Chat 流因此不带 usage chunk，
// relay 只能报 unavailable；而 Native 每帧都携带累计用量，回调一路看得见。这正是
// 这个 Provider 专用回调存在的唯一理由——没有它，这次调用在账上就是免费的。
//
// 两帧的 output_tokens 刻意不同（1 → 5）：只有记成 5 才说明取的是末值。
func TestUsageFromCallbackWhenRelayLacks(t *testing.T) {
	up := nativeStreamUpstream(t, []string{
		`{"output":{"choices":[{"message":{"role":"assistant","content":"你"}}]},` +
			`"usage":{"input_tokens":10,"output_tokens":1},"request_id":"req-2"}`,
		`{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"好"}}]},` +
			`"usage":{"input_tokens":10,"output_tokens":5},"request_id":"req-2"}`,
	})

	target := nativeTarget("a", up.srv.URL, "text-generation")
	rig := newUsageRig(t, usageRigConfig{
		matrix:  testLocalMatrix(t, canonical.CapTextGeneration, canonical.CapStreaming),
		targets: []router.Target{target},
		provs:   map[string]provider.Provider{target.Endpoint: nativeComposite()},
	})

	rec := rig.do(t, `{"model":"m","messages":[{"role":"user","content":"你好"}],"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应体: %s", rec.Code, rec.Body.String())
	}
	// 前提校验：下游流里真的没有 usage，relay 无从取值。这一条不成立的话，
	// 后面那句断言就可能是 relay 自己取到的，与回调无关。
	if strings.Contains(rec.Body.String(), `"usage"`) {
		t.Fatalf("下游流出现了 usage chunk，本用例的前提不成立: %s", rec.Body.String())
	}

	got := rig.tokens(t)
	if got["authoritative/input"] != 10 || got["authoritative/output"] != 5 {
		t.Errorf("token 记账 = %v，期望 authoritative 10/5（回调末帧的累计用量）", got)
	}
	if got["authoritative/output"] == 1 {
		t.Error("记的是首帧用量：累计口径下取首帧等于少计了大半个回复")
	}
}

// TestUsageUnavailableOnStreamAbort 钉死优先级③：relay 报错时用量一律不可知，
// 不许拿回调值捡回来。
//
// 流中断后上游不会再送 usage，relay 已经**刻意**把它抹成 unavailable。此时回调
// 里躺着的是中断前那一帧的半截数字，用它记账等于把一次残缺的调用按完整的收费。
func TestUsageUnavailableOnStreamAbort(t *testing.T) {
	up := nativeStreamUpstream(t, []string{
		`{"output":{"choices":[{"message":{"role":"assistant","content":"你"}}]},` +
			`"usage":{"input_tokens":10,"output_tokens":1},"request_id":"req-3"}`,
		// 畸形帧：解不出来即上游故障，流在首字节之后中断。
		`{"output":{`,
	})

	target := nativeTarget("a", up.srv.URL, "text-generation")
	rig := newUsageRig(t, usageRigConfig{
		matrix:  testLocalMatrix(t, canonical.CapTextGeneration, canonical.CapStreaming),
		targets: []router.Target{target},
		provs:   map[string]provider.Provider{target.Endpoint: nativeComposite()},
	})

	rec := rig.do(t, `{"model":"m","messages":[{"role":"user","content":"你好"}],"stream":true}`)
	body := rec.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Errorf("流中断后应在流内发出终止错误事件，实际响应: %s", body)
	}

	aborted := rig.streamAborted(t)
	if aborted[string(canonical.ClassUpstreamUnavailable)] != 1 {
		t.Errorf("stream_aborted 计数 = %v，期望 upstream_unavailable 记 1 次", aborted)
	}

	if got := rig.tokens(t); len(got) != 0 {
		t.Errorf("流中断仍记了 token %v：回调值不得在 relay 报错后被捡回来", got)
	}
}

// TestUsageCallbackDoesNotLeakAcrossAttempts 钉死回调状态按**具体尝试**隔离。
//
// 首个候选回调过用量再失败，换下一个候选成功但既不回调也没有 relay 用量。
// 回调状态若声明在尝试之外（比如 dispatch 顶上或候选循环外），首次尝试那份
// 用量就会挂在第二次尝试的账上——一次失败的调用被计了费，而两条链路都返回 200，
// 谁也看不出账多了。
func TestUsageCallbackDoesNotLeakAcrossAttempts(t *testing.T) {
	failing := &staticProvider{
		kind:   degrade.ProviderDashScopeNative,
		report: []canonical.Usage{decoyUsage()},
		// 可重试错误：换凭据用尽后换下一个候选，正是要造的那条路径。
		err: canonical.Newf(canonical.ClassUpstreamUnavailable, "首个候选失败"),
	}
	// 成功候选的响应体不带 usage：relay 报 unavailable，指标里出现的任何数字
	// 都只可能来自上一次尝试的回调。
	succeeding := &staticProvider{
		kind: degrade.ProviderDashScopeNative,
		body: `{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`,
	}

	first := nativeTarget("a", "http://127.0.0.1:1", "text-generation")
	second := nativeTarget("b", "http://127.0.0.1:1", "text-generation")
	rig := newUsageRig(t, usageRigConfig{
		matrix:  testLocalMatrix(t, canonical.CapTextGeneration),
		targets: []router.Target{first, second},
		provs: map[string]provider.Provider{
			first.Endpoint:  failing,
			second.Endpoint: succeeding,
		},
	})

	rec := rig.do(t, `{"model":"m","messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应体: %s", rec.Code, rec.Body.String())
	}
	if n := failing.withCB.Load(); n != 1 {
		t.Fatalf("失败候选观测到非 nil 回调 %d 次，期望 1——回调没被注入", n)
	}
	if n := succeeding.calls.Load(); n != 1 {
		t.Fatalf("成功候选被调用 %d 次，期望 1——failover 没走到第二个候选", n)
	}

	if got := rig.tokens(t); len(got) != 0 {
		t.Errorf("记了 token %v：失败尝试的回调用量漏进了下一次尝试的账", got)
	}
}

// TestUsageCallbackDoesNotLeakAcrossCredentials 钉死回调状态也按**凭据尝试**隔离。
//
// 与上一条测的不是同一层：上一条走外层候选循环（换上游），这一条走内层凭据
// 循环（同一个上游换一份凭据）。两层各有各的作用域，覆盖了一层证明不了另一层
// ——把三个变量提到 `tried := map[string]bool{}` 之上，仍在候选循环内，上一条
// 用例照样全绿，而这条会红。
//
// 那个提法是一次真实的计费泄漏：第一份凭据回调过用量再失败，换第二份凭据成功、
// 却既不回调也没有 relay 用量，于是失败那次的数字挂到了成功那次的账上。两次
// 尝试对客户端都只表现为一个 200，多出来的那笔账在响应里看不出任何痕迹。
func TestUsageCallbackDoesNotLeakAcrossCredentials(t *testing.T) {
	prov := &credentialRetryProvider{
		// 首次尝试发诱饵再失败；可重试错误才会让 dispatch 换下一份凭据，
		// 不可重试的话内层循环直接 break，这条路径根本走不到。
		firstReport: decoyUsage(),
		firstErr:    canonical.Newf(canonical.ClassUpstreamUnavailable, "首份凭据失败"),
		// 第二次成功但响应体不带 usage：relay 报 unavailable，指标里出现的
		// 任何数字都只可能是上一份凭据留下的陈值。
		secondBody: `{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`,
	}

	target := nativeTarget("a", "http://127.0.0.1:1", "text-generation")
	rig := newUsageRig(t, usageRigConfig{
		matrix:  testLocalMatrix(t, canonical.CapTextGeneration),
		targets: []router.Target{target},
		provs:   map[string]provider.Provider{target.Endpoint: prov},
		// 两份凭据是本用例的前提：只有一份的话内层循环取不到第二份就 break，
		// 请求以 502 收场，断言变成一句永远成立的空话。
		credIDs: []string{"k1", "k2"},
	})

	rec := rig.do(t, `{"model":"m","messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应体: %s", rec.Code, rec.Body.String())
	}
	// 前提校验：换凭据这件事真的发生过。只调了一次就说明内层循环没走第二圈，
	// 后面那句「没漏账」于是无从谈起——没有第二次尝试，自然没有账可漏。
	if n := prov.calls.Load(); n != 2 {
		t.Fatalf("适配器被调用 %d 次，期望 2——凭据重试没有发生，本用例不成立", n)
	}
	if n := prov.firstWithCB.Load(); n != 1 {
		t.Fatalf("失败的那次凭据尝试观测到非 nil 回调 %d 次，期望 1——回调没被注入", n)
	}
	if n := prov.secondWithCB.Load(); n != 1 {
		t.Fatalf("成功的那次凭据尝试观测到非 nil 回调 %d 次，期望 1——"+
			"注入本该逐次尝试各来一遍", n)
	}
	if ids := prov.credentialIDs(); len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("两次尝试用的凭据是 %v，期望两份不同的——没换凭据就不是凭据重试", ids)
	}

	if got := rig.tokens(t); len(got) != 0 {
		t.Errorf("记了 token %v：失败凭据的回调用量漏进了下一份凭据的账", got)
	}
}

// credentialRetryProvider 是专为凭据重试编排的假适配器：首次调用发诱饵用量再
// 以可重试错误失败，第二次交出一份不带 usage 的 Chat 响应且不回调。
//
// 不复用 staticProvider：那个按实例固定行为，而这里要的恰恰是同一个实例在两次
// 调用间改变行为——凭据重试换的是凭据，Provider 实例自始至终是同一个。
type credentialRetryProvider struct {
	firstReport canonical.Usage
	firstErr    error
	secondBody  string

	calls        atomic.Int32
	firstWithCB  atomic.Int32
	secondWithCB atomic.Int32

	// mu 守 seenCreds：Call 与断言之间隔着 ServeHTTP，虽然本用例里全在同一个
	// goroutine 上跑，但 -race 下裸读写切片仍是要不得的示范。
	mu        sync.Mutex
	seenCreds []string
}

func (p *credentialRetryProvider) Kind() degrade.Provider {
	return degrade.ProviderDashScopeNative
}

func (p *credentialRetryProvider) Call(_ context.Context, req provider.Request) (*httpx.Response, error) {
	n := p.calls.Add(1)

	p.mu.Lock()
	p.seenCreds = append(p.seenCreds, req.Credential.ID)
	p.mu.Unlock()

	if n == 1 {
		if req.OnDashScopeUsage != nil {
			p.firstWithCB.Add(1)
			req.OnDashScopeUsage(p.firstReport)
		}
		return nil, p.firstErr
	}

	// 第二次一个字都不回调：指标里但凡出现数字，就只可能来自第一次尝试。
	if req.OnDashScopeUsage != nil {
		p.secondWithCB.Add(1)
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	return &httpx.Response{
		Response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(p.secondBody)),
		},
	}, nil
}

// credentialIDs 交出两次尝试各自用的凭据 ID 快照。
func (p *credentialRetryProvider) credentialIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seenCreds...)
}

// TestUsageCallbackAbsentForNonNativeProvider 钉死回调只给 DashScope Native。
//
// 这是一处已接受的 Provider 专用耦合，推广成通用 usage seam 就等于让每个适配器
// 都能绕过 relay 直接改写记账口径，而 relay 的口径是按入站协议定的。
//
// 断言分两层：字段层看适配器收到的回调是不是 nil；行为层看诱饵有没有进指标——
// 只看字段的话，日后有人把注入挪到别处仍可能漏掉。
func TestUsageCallbackAbsentForNonNativeProvider(t *testing.T) {
	// 响应体不带 usage：relay 报 unavailable。回调若被错误注入，诱饵就会补上来，
	// 于是指标里凭空出现 999/888。
	fake := &staticProvider{
		kind:   degrade.ProviderDashScopeCompatible,
		report: []canonical.Usage{decoyUsage()},
		body:   `{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`,
	}

	m, err := degrade.Phase1()
	if err != nil {
		t.Fatalf("构造矩阵失败: %v", err)
	}
	target := router.Target{
		Kind:           degrade.ProviderDashScopeCompatible,
		Endpoint:       "a",
		BaseURL:        "http://127.0.0.1:1",
		UpstreamModel:  "upstream-model",
		CredentialPool: "a",
	}
	rig := newUsageRig(t, usageRigConfig{
		matrix:  m,
		targets: []router.Target{target},
		provs:   map[string]provider.Provider{target.Endpoint: fake},
	})

	rec := rig.do(t, `{"model":"m","messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应体: %s", rec.Code, rec.Body.String())
	}
	if n := fake.calls.Load(); n != 1 {
		t.Fatalf("适配器被调用 %d 次，期望 1", n)
	}
	if n := fake.withCB.Load(); n != 0 {
		t.Errorf("非 Native 适配器收到了非 nil 回调 %d 次，期望 0", n)
	}
	if got := rig.tokens(t); len(got) != 0 {
		t.Errorf("记了 token %v：非 Native 路径不该有任何回调用量进账", got)
	}
}
