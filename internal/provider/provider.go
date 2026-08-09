// Package provider 定义出站适配器接口。
package provider

import (
	"context"

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
