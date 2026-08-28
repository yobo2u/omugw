// Package provider 定义出站适配器接口。
package provider

import (
	"context"
	"net/http"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// Request 是一次上游调用的输入。
//
// 同时带上原始字节与 Canonical 表示，是刻意的：同源直通用前者（保住 TTFT，
// 也保住我们没实现的字段），异构转换用后者。让适配器自己挑，比在调用方那里
// 分叉出两条几乎一样的路径要干净。
type Request struct {
	Target     router.Target
	Credential credential.Credential

	// Raw 是客户端原始请求体。
	Raw []byte

	// Canonical 是解码后的统一表示。
	Canonical *canonical.Request

	// Stream 表示客户端要的是流式响应。
	Stream bool

	// Inbound 钉住这次上游调用对应的入站坐标：从哪个协议进来、敲的哪扇门。
	//
	// 矩阵与 Provider 消费同一份入站坐标，避免把裸路径暗当协议标签。同源直通
	// 用它取上游端点路径（入站门即出站端点）；异构适配器用它分派实现。
	// 门留零值时适配器退回自身默认路径。
	Inbound degrade.Inbound

	// Header 是客户端原始请求头。同源直通要把协议相关的头（如 DashScope 的
	// X-DashScope-WorkSpace 租户头）原样带走，否则请求会落到错误的租户。
	// 适配器只转发白名单内的头，Authorization 永远由网关自己的凭据覆盖。
	Header http.Header

	// OnDashScopeUsage 是 DashScope Native 专用的请求级用量回调。
	//
	// 只有 ProviderDashScopeNative 分支注入，其他 Provider 必须为 nil。Native
	// 每帧携带累计 usage，回调被同步调用、后值覆盖前值。这是已接受的 Provider
	// 专用耦合，刻意不推广成通用 usage seam。
	OnDashScopeUsage func(canonical.Usage)
}

// Provider 是一个出站适配器。
type Provider interface {
	// Kind 返回协议族，用于查降级矩阵。
	Kind() degrade.Provider

	// Call 发起一次上游调用。
	//
	// 返回原始 HTTP 响应而不是解好的结构：同源直通要把它整个转发给客户端，
	// 提前解码再重新编码既浪费又会丢掉我们没建模的字段。异构适配器自己在
	// 内部解码，对调用方仍然只暴露这一个形态。
	Call(ctx context.Context, req Request) (*httpx.Response, error)
}
