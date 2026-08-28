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
	"github.com/yobo2u/omugw/internal/protocol/openaichat"
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

// Call 按入站协议分派：Native 同源直通，Chat 走完整重编码，其余 fail-closed。
func (p *Provider) Call(ctx context.Context, req provider.Request) (*httpx.Response, error) {
	switch req.Inbound.Protocol {
	case degrade.ProtoDashScopeNative:
		// 同源直通：入站门即出站端点，字节原样转发。
		return p.passthrough.Call(ctx, req)
	case degrade.ProtoOpenAIChat:
		// 投影先行：Canonical 承载不了的采样选项只有它解得出来，
		// 而 422 判定要靠它区分「未提交」与「显式提交了落不了地的值」。
		proj, err := openaichat.Project(req.Raw)
		if err != nil {
			return nil, err
		}
		// 无落点字段一律在出门前拒绝：发出去等于让客户端以为它生效了。
		if err := rejectUnmappable(proj, req.Canonical); err != nil {
			return nil, err
		}
		if req.Stream {
			return p.translateStream(ctx, req, proj)
		}
		return p.translateNonStream(ctx, req, proj)
	default:
		// fail-closed：未登记的入站坐标一律拒绝，不猜实现。
		return nil, canonical.Newf(canonical.ClassUnsupported,
			"dashscope.native 出站不支持入站协议 %q", req.Inbound.Protocol)
	}
}
