package gateway

import (
	"context"
	"fmt"

	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 每个 Built 共用一个预算/registry，不能按 provider 分摊后绕过进程总量。
func (b *Built) initWebSockets(limits config.WebSocket, timeouts config.Timeouts) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	if wsCloseBudget(timeouts) <= 0 {
		return fmt.Errorf("gateway: WebSocket 关闭预算必须为正数")
	}
	budget, err := ws.NewBufferBudget(limits.MaxBufferedBytes)
	if err != nil {
		return err
	}
	b.wsBudget, b.wsRegistry = budget, newWSRegistry(limits.MaxSessions, wsCloseBudget(timeouts))
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

// 仅固定协议构造器能把守已知门；任意 profile 注册会绕过握手与计量契约。
func buildWSDoors(d WSDeps, endpoints []degrade.Endpoint) ([]*WSHandler, error) {
	handlers := make([]*WSHandler, 0, len(endpoints))
	seen := make(map[degrade.Endpoint]bool, len(endpoints))
	for _, ep := range endpoints {
		if seen[ep] {
			return nil, fmt.Errorf("gateway: WebSocket 端点 %q 重复注册", ep)
		}
		seen[ep] = true
		var h *WSHandler
		switch ep {
		case degrade.EndpointDashScopeRealtime:
			h = NewDashScopeRealtimeHandler(d)
		case degrade.EndpointOpenAIRealtime:
			h = NewOpenAIRealtimeHandler(d)
		default:
			return nil, fmt.Errorf("gateway: 未知 WebSocket 端点 %q", ep)
		}
		if err := checkWSDoor(d.Matrix, h); err != nil {
			return nil, err
		}
		handlers = append(handlers, h)
	}
	return handlers, nil
}
