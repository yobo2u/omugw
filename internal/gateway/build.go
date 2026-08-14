package gateway

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/protocol/dashscopewire"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/provider/passthrough"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// Built 是从配置组装出来的网关。
type Built struct {
	Mux *http.ServeMux

	// Routes 是已注册的转换路径数与已实现数，供启动日志使用。
	Registered  int
	Implemented int
}

// Build 从配置组装网关。
//
// 配置里的每一处引用在 config.Validate 时就校验过了，这里再遇到「找不到」
// 只可能是代码写错，因此一律返回错误让启动失败——一个跑起来才发现路由指向
// 空气的网关，比一个起不来的网关难查得多。
func Build(cfg config.Config, m *degrade.Matrix, metrics *obs.Metrics, log *slog.Logger) (*Built, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	built := &Built{Mux: mux}
	for _, r := range m.Routes() {
		built.Registered++
		if r.Implemented() {
			built.Implemented++
		}
	}

	// 网关部分没配 = 只提供健康检查。这是合法形态，不是失败。
	if len(cfg.Models) == 0 {
		return built, nil
	}

	client := httpx.New(cfg.Timeouts, nil)

	pools := make(map[string]*credential.Pool, len(cfg.Credentials))
	for name, specs := range cfg.Credentials {
		creds := make([]credential.Credential, 0, len(specs))
		for _, s := range specs {
			creds = append(creds, credential.Credential{
				ID: s.ID, Secret: s.Secret, Weight: s.Weight, Priority: s.Priority,
			})
		}
		pool, err := credential.NewPool(name, creds, credential.DefaultPolicy(), nil)
		if err != nil {
			return nil, err
		}
		pools[name] = pool
	}

	endpoints := make(map[string]config.ProviderSpec, len(cfg.Providers))
	provs := make(map[string]provider.Provider, len(cfg.Providers))
	for _, p := range cfg.Providers {
		endpoints[p.Endpoint] = p

		kind := degrade.Provider(p.Kind)
		switch kind {
		case degrade.ProviderOpenAICompat:
			provs[p.Endpoint] = passthrough.New(kind, "/v1/responses", client, nil)
		case degrade.ProviderDashScopeNative:
			// 直通路径随请求走（handler 会注入实际路径），这里只是兜底默认值。
			provs[p.Endpoint] = passthrough.New(kind, dashscopenative.TextGenerationPath, client, nil)
		default:
			// 未实现的协议族在这里就拒绝，而不是等请求打进来才发现没有适配器。
			return nil, fmt.Errorf(
				"gateway: provider %q 的协议族 %q 尚无出站适配器（已实现 %s 与 %s）",
				p.Endpoint, p.Kind, degrade.ProviderOpenAICompat, degrade.ProviderDashScopeNative)
		}
	}

	rules := make([]router.Rule, 0, len(cfg.Models))
	for _, mspec := range cfg.Models {
		targets := make([]router.Target, 0, len(mspec.Targets))
		for _, t := range mspec.Targets {
			ep := endpoints[t.Endpoint]
			targets = append(targets, router.Target{
				Kind:           degrade.Provider(ep.Kind),
				Endpoint:       ep.Endpoint,
				BaseURL:        ep.BaseURL,
				UpstreamModel:  t.UpstreamModel,
				CredentialPool: ep.CredentialPool,
			})
		}
		rules = append(rules, router.Rule{Match: mspec.Match, Targets: targets})
	}

	rt, err := router.New(rules)
	if err != nil {
		return nil, err
	}

	deps := Deps{
		Matrix:    m,
		Router:    rt,
		Auth:      NewAuthenticator(cfg.Auth.Keys),
		Limits:    cfg.Limits,
		Metrics:   metrics,
		Log:       log,
		Pools:     pools,
		Providers: provs,
	}
	native := NewDashScopeNativeHandler(deps) // handler 无状态，多扇门复用同一实例

	// 注册清单是端点与处理器的唯一事实来源：mux.Handle 与下面的启动期对账
	// 都从它推导。分成两处写就会各说各话——删掉一行 mux.Handle 而对账名单
	// 照旧，对账依然全绿，那扇门的请求却已经落进 501 兜底。
	doors := []struct {
		endpoint degrade.Endpoint
		handler  http.Handler
	}{
		{degrade.EndpointOpenAIResponses, NewResponsesHandler(deps)},
		{degrade.EndpointOpenAIChat, NewChatHandler(deps)},
		{degrade.EndpointDashScopeTextGeneration, native},
	}
	registered := make(map[degrade.Endpoint]bool, len(doors))
	for _, d := range doors {
		mux.Handle("POST "+string(d.endpoint), d.handler)
		registered[d.endpoint] = true
	}

	// 命名空间的方法兜底：不带方法的模式最不具体，只有既没命中精确端点、
	// 也没命中下面那条 POST 兜底的请求才会落到这里。
	//
	// 它防的是「兜底把 404 变成 405」：只注册 POST 兜底时，ServeMux 见到同路径
	// 的 GET 会答 405，等于告诉客户端「这个端点在，换个方法就能用」——而这些端点
	// 根本不存在。落回框架 404 才是实话。
	mux.HandleFunc(dashscopenative.NamespacePrefix, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	// DashScope Native 命名空间兜底：未投放端点返回协议化 501。
	// 依赖 net/http.ServeMux 的最长前缀匹配机制，精确注册的 TextGenerationPath 会优先命中。
	mux.HandleFunc("POST "+dashscopenative.NamespacePrefix, func(w http.ResponseWriter, r *http.Request) {
		metrics.ObserveNotImplemented(string(degrade.ProtoDashScopeNative), "planned")
		status, body, headers := dashscopewire.EncodeError(canonical.Newf(canonical.ClassNotImplemented, "DashScope Native 端点 %s 尚未实现", r.URL.Path))
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})

	// 启动期双向对账：矩阵兑现过的门必须注册了处理器，注册了处理器的门
	// 必须有路径兑现。防两种漂移：兑现过的门忘了注册，请求落进 501 兜底；
	// 注册了的门忘了在矩阵兑现，变成一处永远返回 501 的空头承诺。
	//
	// registered 直接来自上面那份注册清单，不另写第二份名单。
	opened := map[degrade.Endpoint]bool{}
	for _, r := range m.Routes() {
		if !r.Implemented() {
			continue
		}
		for _, ep := range r.Endpoints() {
			opened[ep] = true
			if !registered[ep] {
				return nil, fmt.Errorf("gateway: 端点 %s 已在矩阵兑现，但没有注册处理器", ep)
			}
		}
	}
	for ep := range registered {
		if !opened[ep] {
			return nil, fmt.Errorf("gateway: 端点 %s 注册了处理器，但没有任何路径兑现它", ep)
		}
	}

	return built, nil
}
