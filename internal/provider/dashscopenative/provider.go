// Package dashscopenative 是 dashscope.native 的 Composite 出站适配器。
//
// 同一个 provider.Provider 接口后隐藏两种实现：Native 入站复用既有 passthrough
// （同源直通，保住字节与 TTFT）；OpenAI Chat 入站进入 translator（完整重编码）。
// 其他入站坐标 fail-closed——Gateway 不承担协议转换，这里也不猜。
package dashscopenative

import (
	"context"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/degrade"
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/provider/passthrough"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// Provider 是 dashscope.native 的 Composite 适配器。
type Provider struct {
	passthrough *passthrough.Provider
	client      *httpx.Client
	now         func() time.Time
}

// New 构造 Composite 适配器。内部自带一个 Native 同源 passthrough。
func New(c *httpx.Client, now func() time.Time) *Provider {
	if now == nil {
		now = time.Now
	}
	return &Provider{
		passthrough: passthrough.New(degrade.ProviderDashScopeNative, nativewire.TextGenerationPath, c, now),
		client:      c,
		now:         now,
	}
}

// Kind 返回协议族。
func (p *Provider) Kind() degrade.Provider { return degrade.ProviderDashScopeNative }

// Call 先钉死已完整实现的 Native 同源分支；Chat 分支要等非流式与流式
// translator 都落地后再由任务 13 原子接入，避免提交临时 501 桩。
func (p *Provider) Call(ctx context.Context, req provider.Request) (*httpx.Response, error) {
	switch req.Inbound.Protocol {
	case degrade.ProtoDashScopeNative:
		// 同源直通：入站门即出站端点，字节原样转发。
		return p.passthrough.Call(ctx, req)
	default:
		// fail-closed：未登记的入站坐标一律拒绝，不猜实现。
		return nil, canonical.Newf(canonical.ClassUnsupported,
			"dashscope.native 出站不支持入站协议 %q", req.Inbound.Protocol)
	}
}
