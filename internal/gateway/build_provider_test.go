package gateway

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
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
	for _, kind := range []string{"openai.compat", "dashscope.compatible", "dashscope.native"} {
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("错误应列出已实现的协议族 %s: %v", kind, err)
		}
	}
}
