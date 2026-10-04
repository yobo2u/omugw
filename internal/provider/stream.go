package provider

import (
	"context"
	"net/http"

	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// StreamProvider 与 HTTP Call 平行，防止 HTTP total 超时和响应体语义侵入双向会话。
// Dial 只建立上游连接；下游 101 承诺与 failover 仍由网关统一持有。
type StreamProvider interface {
	Kind() degrade.Provider
	Dial(context.Context, Request) (*ws.Conn, *http.Response, error)
}
