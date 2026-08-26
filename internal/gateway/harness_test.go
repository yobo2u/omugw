package gateway

import (
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

	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/protocol/openaichat"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/provider/dashscopecompat"
	dsnativeprovider "github.com/yobo2u/omugw/internal/provider/dashscopenative"
	"github.com/yobo2u/omugw/internal/provider/passthrough"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

const testKey = "omugw-test-key-0123456789"

// sentinelProviderPath 是 harness 给 passthrough 适配器故意挑的默认路径：
// /v1/responses 不属于 Chat 与 DashScope Native 的任何端点，只有当 handler
// 真的注入了 provider.Request.Inbound.Endpoint，请求才会打到正确端点——两者
// 若设成一样，即使入站坐标注入被删掉测试也照样通过，等于没测。
const sentinelProviderPath = "/v1/responses"

// upstream 是一个可编排的假上游。
type upstream struct {
	srv   *httptest.Server
	calls atomic.Int32
}

func newUpstream(t *testing.T, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		h(w, r)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// harness 组装一套可用的网关，targets 按顺序即 failover 顺序。
type harness struct {
	h       *Handler
	matrix  *degrade.Matrix
	metrics *obs.Metrics
	// path 是 do() 打入的入站路由，与所测入站协议一致。
	path string
}

// providerFactory 按协议族构造出站适配器。harness 不得硬编码 passthrough：
// wire-compatible 路径有自己的适配器。
type providerFactory func(kind degrade.Provider, client *httpx.Client) provider.Provider

// passthroughFactory 构造同源直通适配器，默认路径用哨兵路径。
func passthroughFactory(kind degrade.Provider, client *httpx.Client) provider.Provider {
	return passthrough.New(kind, sentinelProviderPath, client, nil)
}

// dashScopeCompatFactory 构造 DashScope Compatible 适配器。协议族由适配器
// 自身固定，不从参数取。
func dashScopeCompatFactory(_ degrade.Provider, client *httpx.Client) provider.Provider {
	return dashscopecompat.New(client, nil)
}

// dashScopeNativeFactory 构造 DashScope Native Composite 适配器。
//
// 与 build.go 用同一个构造函数：装配一旦在两处各写一份，harness 测过的就不是
// 生产装出来的那个东西——线上装成直通而 harness 装着 Composite，测试照绿。
func dashScopeNativeFactory(_ degrade.Provider, client *httpx.Client) provider.Provider {
	return dsnativeprovider.New(client, nil)
}

// harnessConfig 是一套网关 harness 的完整装配声明。
//
// 装配一套网关本就是「入站门 + 出站协议族 + 处理器 + 闸门上限 + 适配器工厂」
// 这一组必须同时成立的选择，散成位置参数后调用点读不出谁是谁，加一维就要改
// 全部调用点。
type harnessConfig struct {
	requestPath string
	kind        degrade.Provider
	newHandler  func(Deps) *Handler
	limits      config.Limits
	factory     providerFactory

	// nativeEndpoint 是 DashScope Native target 的门。零值即「不是 Native
	// target」，与生产配置一致（config.Validate 只允许 Native kind 声明它），
	// 因此既有 harness 无需改动。
	nativeEndpoint string
}

// newHarness 是 Responses 入站的 harness。
//
// 已实现的路径用 openai.compat（已转正）；要测 PLANNED 行为就把目标指到一条
// 仍未实现的路径上。不去「取消转正」——那需要给生产代码开一个只为测试存在的
// 后门，而后门迟早会被当成正常用法。
func newHarness(t *testing.T, implemented bool, ups ...*upstream) *harness {
	t.Helper()
	kind := degrade.ProviderOpenAICompat
	if !implemented {
		kind = degrade.ProviderDashScopeCompatible
	}
	return newHarnessFor(t, harnessConfig{
		requestPath: "/v1/responses",
		kind:        kind,
		newHandler:  NewResponsesHandler,
		limits:      config.Default().Limits,
		factory:     passthroughFactory,
	}, ups...)
}

// newChatHarness 是 Chat Completions 入站的 harness。
func newChatHarness(t *testing.T, implemented bool, ups ...*upstream) *harness {
	t.Helper()
	kind := degrade.ProviderOpenAICompat
	if !implemented {
		// 未实现哨兵指向 Phase 1 永远 PLANNED 的 anthropic.messages：
		// dashscope.compatible 一经转正就不能再当哨兵，否则「未实现路径 501」
		// 会在转正当天悄悄变成 200 而测试照绿。
		kind = degrade.ProviderAnthropicMessages
	}
	return newHarnessFor(t, harnessConfig{
		requestPath: "/v1/chat/completions",
		kind:        kind,
		newHandler:  NewChatHandler,
		limits:      config.Default().Limits,
		factory:     passthroughFactory,
	}, ups...)
}

// newChatDSCompatHarness 是 Chat -> DashScope Compatible wire-compatible 路径的 harness。
//
// 出站适配器是 dashscopecompat 而不是 passthrough——这正是 harness 注入
// Provider 工厂的原因：wire-compatible 而语义异构的路径有自己的适配器，
// 测试基建不能硬编码同源直通，否则测的是另一条路。
func newChatDSCompatHarness(t *testing.T, ups ...*upstream) *harness {
	t.Helper()
	return newHarnessFor(t, harnessConfig{
		requestPath: "/v1/chat/completions",
		kind:        degrade.ProviderDashScopeCompatible,
		newHandler:  NewChatHandler,
		limits:      config.Default().Limits,
		factory:     dashScopeCompatFactory,
	}, ups...)
}

// newChatDSNativeHarness 是 Chat -> DashScope Native 异构路径的 harness。
//
// door 决定候选目标的门：纯文本用例用 text-generation，含媒体的用
// multimodal-generation。门由调用点显式声明而不是从请求内容推断——那正是
// 生产侧 config.Validate 守着的同一条规矩，harness 不该另立一套。
func newChatDSNativeHarness(t *testing.T, door string, ups ...*upstream) *harness {
	t.Helper()
	return newHarnessFor(t, harnessConfig{
		requestPath:    string(degrade.EndpointOpenAIChat),
		kind:           degrade.ProviderDashScopeNative,
		newHandler:     NewChatHandler,
		limits:         config.Default().Limits,
		factory:        dashScopeNativeFactory,
		nativeEndpoint: door,
	}, ups...)
}

// newDashScopeNativeHarness 是 DashScope Native 入站的 harness。
//
// 尽管同源直通（passthrough）路由时只看 Inbound.Endpoint 而忽略 Target.NativeEndpoint，
// 但网关在分派前会统一对 DashScope Native 候选执行媒体过滤。如果这里不给一个合法的门
// （如 multimodal-generation），带媒体的 Native fixture 测试就会在过滤阶段被拦截并返回 422。
// 纯文本请求则不受影响，会保留所有门。
func newDashScopeNativeHarness(t *testing.T, implemented bool, ups ...*upstream) *harness {
	t.Helper()
	kind := degrade.ProviderDashScopeNative
	if !implemented {
		kind = degrade.ProviderDashScopeCompatible
	}
	return newHarnessFor(t, harnessConfig{
		requestPath:    dashscopenative.TextGenerationPath,
		kind:           kind,
		newHandler:     NewDashScopeNativeHandler,
		limits:         config.Default().Limits,
		factory:        passthroughFactory,
		nativeEndpoint: "multimodal-generation",
	}, ups...)
}

// newDashScopeNativeHarnessWithLimits 与 newDashScopeNativeHarness 相同，
// 但注入自定义 Limits——内联闸门先于矩阵裁决生效的性质要用一个小到会被
// 击穿的上限来证明。不走 hs.h.deps.Limits 事后改写：那是绕过构造契约的后门，
// 而这里要证的恰恰是「按配置构造出来的网关」在闸门顺序上的行为。
//
// 同样需要注入合法的 nativeEndpoint 以通过网关的媒体过滤。
func newDashScopeNativeHarnessWithLimits(t *testing.T, limits config.Limits, ups ...*upstream) *harness {
	t.Helper()
	return newHarnessFor(t, harnessConfig{
		requestPath:    dashscopenative.TextGenerationPath,
		kind:           degrade.ProviderDashScopeNative,
		newHandler:     NewDashScopeNativeHandler,
		limits:         limits,
		factory:        passthroughFactory,
		nativeEndpoint: "multimodal-generation",
	}, ups...)
}

func newHarnessFor(t *testing.T, cfg harnessConfig, ups ...*upstream) *harness {
	t.Helper()

	m, err := degrade.Phase1()
	if err != nil {
		t.Fatal(err)
	}

	timeouts := config.Timeouts{
		Connect:   200 * time.Millisecond,
		FirstByte: 1 * time.Second,
		Total:     10 * time.Second,
		Idle:      500 * time.Millisecond,
	}
	client := httpx.New(timeouts, nil)

	var targets []router.Target
	pools := map[string]*credential.Pool{}
	provs := map[string]provider.Provider{}

	for i, u := range ups {
		name := endpointName(i)
		targets = append(targets, router.Target{
			Kind:           cfg.kind,
			Endpoint:       name,
			BaseURL:        u.srv.URL,
			UpstreamModel:  "upstream-model",
			CredentialPool: name,
			NativeEndpoint: cfg.nativeEndpoint,
		})
		pool, err := credential.NewPool(name,
			[]credential.Credential{{ID: "k1", Secret: "sk-" + name}},
			credential.DefaultPolicy(), nil)
		if err != nil {
			t.Fatal(err)
		}
		pools[name] = pool
		provs[name] = cfg.factory(cfg.kind, client)
	}

	rt, err := router.New([]router.Rule{{Match: "*", Targets: targets}})
	if err != nil {
		t.Fatal(err)
	}

	metrics := obs.NewMetrics(prometheus.NewRegistry())
	return &harness{
		matrix:  m,
		metrics: metrics,
		path:    cfg.requestPath,
		h: cfg.newHandler(Deps{
			Matrix:    m,
			Router:    rt,
			Auth:      NewAuthenticator([]config.AuthKey{{ID: "tester", Key: testKey}}),
			Limits:    cfg.limits,
			Metrics:   metrics,
			Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			Pools:     pools,
			Providers: provs,
		}),
	}
}

func endpointName(i int) string { return string(rune('a' + i)) }

func (hs *harness) do(t *testing.T, body string, withAuth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, hs.path, strings.NewReader(body))
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+testKey)
	}
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	return rec
}

// doPath 按指定端点路径直接打 Handler。
//
// 走 Handler 而不是 Build 出来的 Mux 是刻意的：Mux 的 /api/v1/ 兜底会把任何
// 未注册端点一律答成 501，那条 501 出自路由表，与矩阵裁决无关。要证的是矩阵
// 按 (端点, 能力) 作答，就必须绕开兜底，让请求真的走完 serve 的闸门。
func doPath(t *testing.T, hs *harness, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	return rec
}

func jsonUpstream(t *testing.T, body string) *upstream {
	return newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
}

// callThroughHarness 拿 harness 自己装出来的路由、凭据池与适配器打一次上游。
//
// 走这三样而不是现搭一个 Target，是本测试的全部意义所在：要证的正是
// newHarnessFor 装配出来的东西对不对，自己造一份 Target 就等于把被测对象
// 换成了测试代码里的副本，怎么写都绿。
//
// 绕开 Handler.serve 则是另一回事：矩阵此刻还没兑现 openai.chat →
// dashscope.native（那是任务 18 的事），走完整链路只会停在 501 端点闸门，
// 测不到装配。等转正再验，harness 已经被三个任务当地基用了。
func callThroughHarness(t *testing.T, hs *harness, body string) (*httpx.Response, error) {
	t.Helper()

	targets, err := hs.h.deps.Router.Resolve("m")
	if err != nil {
		t.Fatalf("harness 的路由解不出模型: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("期望 1 个候选，实际 %d", len(targets))
	}
	target := targets[0]

	prov, ok := hs.h.deps.Providers[target.Endpoint]
	if !ok {
		t.Fatalf("harness 没为 endpoint %q 装配适配器", target.Endpoint)
	}
	pool, ok := hs.h.deps.Pools[target.CredentialPool]
	if !ok {
		t.Fatalf("harness 没为 %q 装配凭据池", target.CredentialPool)
	}
	lease, err := pool.Acquire(nil)
	if err != nil {
		t.Fatalf("取凭据失败: %v", err)
	}
	defer lease.Succeed()

	decoded, err := openaichat.Decode([]byte(body))
	if err != nil {
		t.Fatalf("请求体解码失败: %v", err)
	}

	return prov.Call(t.Context(), provider.Request{
		Target:     target,
		Credential: lease.Credential,
		Raw:        []byte(body),
		Canonical:  &decoded.Request,
		Stream:     decoded.Request.Stream,
		Inbound:    degrade.Inbound{Protocol: degrade.ProtoOpenAIChat, Endpoint: degrade.EndpointOpenAIChat},
		Header:     http.Header{},
	})
}

// TestChatDSNativeHarnessCarriesDoor 钉死 harness 真的把门送进了 router.Target。
//
// 这个 harness 是任务 15/16/19 的测试基建，先落地、后被用。而「能编译」证明不了
// 它可用：门忘了填进 Target，Composite 拿到空门，那三个任务的测试会一起红在一个
// 与它们无关的地方；更糟的是填成固定值——多模态用例被打到文本生成端点却照样 200，
// 图像块在上游被当成读不懂的内容丢掉，响应里看不出任何痕迹。
//
// 所以两扇门各走一遍真实 Composite：假上游逐字对账 URL 路径与出站信封，
// 响应还得是重编码后的 chat.completion——那是「装的确实是 Composite 而不是直通」
// 的唯一硬证据。
func TestChatDSNativeHarnessCarriesDoor(t *testing.T) {
	cases := []struct {
		door string
		path string
		body string
	}{
		{
			door: "text-generation",
			path: dashscopenative.TextGenerationPath,
			body: `{"model":"m","messages":[{"role":"user","content":"你好"}]}`,
		},
		{
			door: "multimodal-generation",
			path: dashscopenative.MultimodalGenerationPath,
			body: `{"model":"m","messages":[{"role":"user","content":[` +
				`{"type":"text","text":"看图"},` +
				`{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.door, func(t *testing.T) {
			var (
				mu      sync.Mutex
				gotPath string
				gotBody []byte
			)
			up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				gotPath, gotBody = r.URL.Path, body
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"output":{"choices":[{"finish_reason":"stop",`+
					`"message":{"role":"assistant","content":"好"}}]},"request_id":"req-h"}`)
			})

			hs := newChatDSNativeHarness(t, tc.door, up)
			resp, err := callThroughHarness(t, hs, tc.body)
			if err != nil {
				t.Fatalf("Composite 调用失败（门没接线时这里就是空门 500）: %v", err)
			}
			defer resp.Body.Close()

			mu.Lock()
			path, sent := gotPath, append([]byte(nil), gotBody...)
			mu.Unlock()

			if path != tc.path {
				t.Errorf("上游路径 = %q，期望 %q——harness 送进 Target 的门不对", path, tc.path)
			}
			if !strings.Contains(string(sent), `"input"`) ||
				!strings.Contains(string(sent), "result_format") {
				t.Errorf("上游收到的不是 Native 出站信封: %s", sent)
			}

			out, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("读响应失败: %v", err)
			}
			if !strings.Contains(string(out), `"chat.completion"`) {
				t.Errorf("响应不是重编码后的 chat.completion，装的可能不是 Composite: %s", out)
			}
		})
	}
}
