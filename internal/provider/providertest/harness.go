package providertest

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// refTime 是套件的固定时钟。
//
// Retry-After 的 HTTP-date 形式依赖当前时间；用真实时钟会让断言在跨秒边界
// 随机失败，而那种失败重跑就好，最难查。
var refTime = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

// gatewaySecret 是套件里网关自己的凭据。
// 断言上游必须收到它，而不是客户端发来的那份。
const gatewaySecret = "sk-gateway-own-key"

// clientSecret 是套件伪装的客户端凭据。它绝不该到达上游。
const clientSecret = "sk-client-must-not-leak"

// captured 记录上游实际收到了什么。
//
// 只记**第一次**请求。防的是静默覆盖：httpx.Client 默认跟随重定向，一个 3xx 桩
// 会让同一个夹具连收两次，末次写入把第一次的 method/path/header 冲掉，断言于是
// 拿第二跳的内容当成适配器发出的东西。一个「证明上游收到了什么」的夹具给出错误
// 答案，比没有夹具更糟，所以多出来的请求当场报错而不是覆盖。
type captured struct {
	// mu 只守护**桩这一侧的写**：桩跑在 httptest 的服务端 goroutine 上，多个
	// 请求会并发进来，没有它就是数据竞争。
	//
	// 读侧不加锁，断言直接读字段即可——防的是把这里读成「读也安全」：真要并发
	// 读，得等到写侧全部完成才行，而那个前提由上面的一次性契约保证，不是由 mu
	// 保证。测试读到的值之所以稳定，是因为请求已经往返完毕（happens-before），
	// 不是因为它们拿了这把锁。
	mu sync.Mutex

	body   []byte
	header http.Header
	method string
	path   string

	// hits 是收到的请求次数，第二次起即违约。
	hits int

	// extra 是多余请求的告警出口，默认写进当次 t.Error。
	// 留成字段是为了让「多打一次会响」这条路径自己也能被测到——
	// 否则验证它的唯一办法是让某个测试真的失败。
	extra func(format string, args ...any)
}

// harness 是一次契约断言的运行环境。
type harness struct {
	server *httptest.Server
	got    *captured
	deps   Deps
}

// newHarness 起一个记录请求的上游桩，并备好注入固定时钟的依赖。
func newHarness(t *testing.T, h http.HandlerFunc) *harness {
	t.Helper()

	got := &captured{extra: t.Errorf}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		// 读完得把体放回去：ReadAll 耗尽了 r.Body，不还就等于把空体交给桩，
		// 桩想按请求体分支或回显时会静默照着 "" 应答。
		r.Body = io.NopCloser(bytes.NewReader(body))

		got.mu.Lock()
		got.hits++
		if got.hits > 1 {
			// 在这里就报，才能失败到「多打了一次」的那个测试上；
			// 留到事后统一检查，报出来的会是下一个测试。
			got.extra("夹具收到第 %d 次上游请求（%s %s）——一个夹具只服务一次调用，"+
				"多出来的通常是跟随重定向；请为第二跳另起一个 newHarness",
				got.hits, r.Method, r.URL.Path)
			got.mu.Unlock()
			h(w, r)
			return
		}
		got.body = body
		got.header = r.Header.Clone()
		got.method = r.Method
		got.path = r.URL.Path
		got.mu.Unlock()

		h(w, r)
	}))
	t.Cleanup(srv.Close)

	now := func() time.Time { return refTime }
	return &harness{
		server: srv,
		got:    got,
		deps: Deps{
			HTTPClient: httpx.New(config.Default().Timeouts, now),
			Now:        now,
		},
	}
}

// okServer 是最常见的桩：收下请求，回一个空的 2xx JSON。
func okServer(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
}

// callOpts 是一次适配器调用的可变输入。零值即最常见的那一种。
type callOpts struct {
	body   string
	stream bool

	// inboundEndpoint 是这次调用敲的那扇门，留空即「请求没带门」，
	// 用来验证适配器退回自己的默认路径。
	inboundEndpoint degrade.Endpoint

	baseURL string
	header  http.Header

	// upstreamModel 留空时用 defaultUpstreamModel。
	// 显式传空要用 emptyUpstreamModel 标记，供「装配错误归网关」那条断言使用。
	upstreamModel      string
	emptyUpstreamModel bool
}

// defaultUpstreamModel 是套件断言模型改写时期望上游收到的名字。
// 与 ValidBody 里的逻辑名不同，才能证明改写确实发生了。
const defaultUpstreamModel = "upstream-model-name"

// call 发起一次适配器调用，成功时把响应体的关闭登记进 t.Cleanup。
//
// 在这里登记而不是让每个断言自己关：httpx 的 Body 上挂着空闲计时器与整体
// 超时的 cancel，漏关会把它们一起漏掉；而且 httptest.Server.Close 会等未完成的
// 连接，漏关能让收尾挂住。t.Cleanup 是 LIFO，newHarness 先登记 srv.Close，
// 这里后登记 Body.Close，于是先关体、后关服务器——顺序正是要的那个。
func (h *harness) call(t *testing.T, s Subject, o callOpts) (*httpx.Response, error) {
	t.Helper()

	p := s.New(t, h.deps)

	body := o.body
	if body == "" {
		body = s.ValidBody
	}
	baseURL := o.baseURL
	if baseURL == "" {
		baseURL = h.server.URL
	}
	model := o.upstreamModel
	if model == "" && !o.emptyUpstreamModel {
		model = defaultUpstreamModel
	}

	resp, err := p.Call(context.Background(), provider.Request{
		Target: router.Target{
			Kind:           s.Kind,
			Endpoint:       "contract-test",
			BaseURL:        baseURL,
			UpstreamModel:  model,
			CredentialPool: "contract-test",
		},
		Credential: credential.Credential{ID: "k1", Secret: gatewaySecret},
		Raw:        []byte(body),
		Stream:     o.stream,
		Inbound: degrade.Inbound{
			Protocol: s.InboundProtocol,
			Endpoint: o.inboundEndpoint,
		},
		Header: o.header,
	})
	if err == nil && resp != nil {
		t.Cleanup(func() { resp.Body.Close() })
	}
	return resp, err
}
