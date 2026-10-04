package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
)

// TestBuildAcceptsDashScopeCompatibleProvider 固化 dashscope.compatible 的装配：
// 配置了这个协议族的网关必须能启动——它已在降级矩阵里设计，适配器也已写好。
func TestBuildAcceptsDashScopeCompatibleProvider(t *testing.T) {
	m, err := degrade.Phase1()
	if err != nil {
		t.Fatal(err)
	}

	cfg := buildTestConfig("http://127.0.0.1:0")
	cfg.Providers[0].Kind = "dashscope.compatible"
	if _, err := Build(cfg, m, obs.NewMetrics(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("dashscope.compatible 应已装配: %v", err)
	}
}

func TestWSBuildGate(t *testing.T) {
	cfg := buildTestConfig("http://127.0.0.1:0")
	cfg.Providers[0].Kind = string(degrade.ProviderDashScopeWSRealtime)
	cfg.WebSocket = config.DefaultWebSocket()
	metrics := obs.NewMetrics(prometheus.NewRegistry())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, _ := degrade.Phase1()
	b, err := Build(cfg, m, metrics, log)
	if err != nil {
		t.Fatal("已实现的 WS provider 应可装配", err)
	}
	r := httptest.NewRequest("GET", "/api-ws/v1/realtime?model=m", nil)
	w := httptest.NewRecorder()
	b.Mux.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal("正式 Build 尚无真实证据却注册了 WS 门")
	}
	if err := b.ShutdownWebSockets(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := buildWithWS(cfg, m, metrics, log, degrade.EndpointDashScopeRealtime); err == nil {
		t.Fatal("测试注册绕过 PLANNED 矩阵")
	}
	if _, err := buildWithWS(cfg, wsHandlerMatrix(t, false), metrics, log, degrade.EndpointDashScopeRealtime); err == nil {
		t.Fatal("部分兑现通过整门检查")
	}
	if _, err := Build(cfg, wsHandlerMatrix(t, true), metrics, log); err == nil {
		t.Fatal("兑现却未注册通过反向对账")
	}
	b, err = buildWithWS(cfg, wsHandlerMatrix(t, true), metrics, log, degrade.EndpointDashScopeRealtime)
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	b.Mux.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("测试门未由自身身份的 WS handler 处理")
	}
	if err := b.ShutdownWebSockets(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestBuildRejectsUnimplementedKindListsImplemented 固化未实现协议族的启动错误
// 必须列全已实现的协议族——漏列会让运维以为某个已实现的族不存在。
func TestBuildRejectsUnimplementedKindListsImplemented(t *testing.T) {
	m := degrade.NewMatrix()

	cfg := buildTestConfig("http://127.0.0.1:0")
	cfg.Providers[0].Kind = "anthropic.messages"
	_, err := Build(cfg, m, obs.NewMetrics(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("未实现的协议族应当启动失败")
	}
	for _, kind := range []string{"openai.compat", "dashscope.compatible", "dashscope.native", "dashscope.ws.realtime", "openai.realtime"} {
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("错误应列出已实现的协议族 %s: %v", kind, err)
		}
	}
}
