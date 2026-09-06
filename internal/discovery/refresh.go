package discovery

import (
	"context"
	"log/slog"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// Endpoint 是一个待探测的上游。
type Endpoint struct {
	Name    string
	Kind    degrade.Provider
	BaseURL string
	Pool    *credential.Pool
}

// Refresher 按固定周期刷新全部上游的模型清单。
type Refresher struct {
	registry  *Registry
	endpoints []Endpoint
	probers   map[degrade.Provider]Prober
	client    *httpx.Client
	cfg       config.Discovery
	log       *slog.Logger
}

// NewRefresher 构造刷新器。
func NewRefresher(
	reg *Registry,
	endpoints []Endpoint,
	probers map[degrade.Provider]Prober,
	client *httpx.Client,
	cfg config.Discovery,
	log *slog.Logger,
) *Refresher {
	return &Refresher{
		registry:  reg,
		endpoints: endpoints,
		probers:   probers,
		client:    client,
		cfg:       cfg,
		log:       log,
	}
}

// Run 先立即刷一轮，随后按周期刷新，直到 ctx 结束。
//
// 启动时先刷一轮而不是等第一个周期：否则网关刚起来的那半小时里，
// /v1/models 只有配置模型，而运维会以为是发现坏了。
func (r *Refresher) Run(ctx context.Context) {
	r.RefreshOnce(ctx)

	t := time.NewTicker(r.cfg.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.RefreshOnce(ctx)
		}
	}
}

// RefreshOnce 刷新一轮全部上游。
//
// 单个 endpoint 失败不影响其余：一把过期的 DashScope 凭据不该让 OpenAI 那边
// 的清单也跟着消失。
func (r *Refresher) RefreshOnce(ctx context.Context) {
	for _, ep := range r.endpoints {
		r.refreshEndpoint(ctx, ep)
	}
}

func (r *Refresher) refreshEndpoint(ctx context.Context, ep Endpoint) {
	prober, ok := r.probers[ep.Kind]
	if !ok {
		// 不是错误。dashscope.compatible 就没有官方列表接口，
		// 报错会让日志里长期躺着一条永远修不好的 ERROR。
		r.log.Info("该协议族没有官方模型列表接口，跳过发现",
			"endpoint", ep.Name, "kind", string(ep.Kind))
		return
	}

	lease, err := ep.Pool.Acquire(nil)
	if err != nil {
		r.log.Warn("发现模型清单时无可用凭据",
			"endpoint", ep.Name, "error", canonical.AsError(err).Message)
		return
	}

	// 目录查询自带一层更短的超时：httpx 的四层超时是按生成请求的尺度定的，
	// 让一次目录 GET 等那么久，只会让刷新周期彼此追尾。
	callCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	models, err := prober.Probe(callCtx, r.client, ep.BaseURL, lease.Credential.Secret)
	if err != nil {
		lease.Fail(err)
		// 失败时**保留上一次成功的快照**：清空会让一次上游抖动直接表现为
		// 「客户端看到的模型清单突然少了一半」。
		r.log.Warn("发现模型清单失败，保留上一次快照",
			"endpoint", ep.Name, "error", canonical.AsError(err).Message)
		return
	}
	lease.Succeed()

	for i := range models {
		models[i].Endpoint = ep.Name
	}
	r.registry.Put(ep.Name, models)
	r.log.Info("已刷新模型清单", "endpoint", ep.Name, "models", len(models))
}
