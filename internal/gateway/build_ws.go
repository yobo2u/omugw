package gateway

import (
	"context"
	"fmt"

	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 每个 Built 共用一个预算/registry，不能按 provider 分摊后绕过进程总量。
func (b *Built) initWebSockets(limits config.WebSocket) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	budget, err := ws.NewBufferBudget(limits.MaxBufferedBytes)
	if err != nil {
		return err
	}
	b.wsBudget, b.wsRegistry = budget, newWSRegistry(limits.MaxSessions)
	return nil
}

// ShutdownWebSockets 包含 pending 与 Hijacked 连接，不能以 HTTP Shutdown 替代。
func (b *Built) ShutdownWebSockets(ctx context.Context) error {
	if b.wsRegistry == nil {
		return nil
	}
	return b.wsRegistry.Shutdown(ctx)
}

// 整门批准在注册与每次 Dial 前各检查一次，测试注册也没有豁免入口。
func checkWSDoor(m *degrade.Matrix, h *WSHandler) error {
	in := h.inbound()
	if _, err := m.Check(in, h.profile.outbound, degrade.ExpressibleSet(in.Protocol)); err != nil {
		return fmt.Errorf("gateway: Realtime 整门未批准: %w", err)
	}
	return nil
}
