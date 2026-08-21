package providertest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
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
type captured struct {
	body   []byte
	header http.Header
	method string
	path   string
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

	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.body, _ = io.ReadAll(r.Body)
		got.header = r.Header.Clone()
		got.method = r.Method
		got.path = r.URL.Path
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
	body    string
	stream  bool
	path    string
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
		Path:       o.path,
		Header:     o.header,
	})
	if err == nil && resp != nil {
		t.Cleanup(func() { resp.Body.Close() })
	}
	return resp, err
}
