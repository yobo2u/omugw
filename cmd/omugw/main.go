// Command omugw 是 Universal AI Gateway 的数据面进程。
//
// M0 阶段只启动配置、日志、指标与健康检查——协议处理在 M1 接入。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/gateway"
	"github.com/yobo2u/omugw/internal/obs"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "omugw: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	log := obs.NewLogger(cfg.Log, os.Stdout)

	// 降级矩阵在启动时构建。不完整的矩阵在这里就会失败，而不是等到某个
	// 请求打进来才发现某条路径没人声明过——启动失败远比运行时静默丢字段便宜。
	matrix, err := degrade.Phase1()
	if err != nil {
		return fmt.Errorf("降级矩阵不完整: %w", err)
	}
	var implemented int
	for _, r := range matrix.Routes() {
		p := r.Preservation(matrix.Availability())
		if r.Implemented {
			implemented++
		}
		attrs := []any{
			"inbound", string(r.In),
			"outbound", string(r.Out),
			"implemented", r.Implemented,
			"fast_path", r.Homogeneous,
			"design_score", p.DesignScore(),
		}
		// 未实现的路径不打印当前可用分数，与矩阵文档里那一列的 "—" 保持一致。
		// 同一个数字在两处含义不同，正是 ADR-0002 要消灭的毛病。
		if r.Implemented {
			attrs = append(attrs, "available_score", p.AvailableScore())
		}
		log.Info("已注册转换路径", attrs...)
	}
	if implemented == 0 {
		// 一个一条路都走不通的网关必须自己说出来，而不是等第一个请求
		// 撞上 501 才让人发现。
		log.Warn("当前没有任何已实现的转换路径，网关只能响应健康检查",
			"planned", len(matrix.Routes()))
	}
	if len(cfg.Models) == 0 {
		log.Warn("未配置模型路由，网关只提供健康检查",
			"hint", "配齐 auth.keys / credentials / providers / models 后重启")
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	metrics := obs.NewMetrics(reg)

	built, err := gateway.Build(cfg, matrix, metrics, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	gwSrv := &http.Server{
		Addr:    cfg.Server.Addr,
		Handler: built.Mux,
		// 只设读头超时。整体超时由 internal/transport 的四层超时管理——
		// 在这里设 WriteTimeout 会把长流式响应直接掐断。
		ReadHeaderTimeout: cfg.Timeouts.Connect,
	}
	metricsSrv := &http.Server{
		Addr:              cfg.Server.MetricsAddr,
		Handler:           metricsMux(reg),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 2)
	go serve(gwSrv, "gateway", log, errCh)
	go serve(metricsSrv, "metrics", log, errCh)

	log.Info("omugw 已启动",
		"addr", cfg.Server.Addr,
		"metrics_addr", cfg.Server.MetricsAddr,
		"routes_registered", built.Registered,
		"routes_implemented", built.Implemented,
		"models", len(cfg.Models),
		"convstore_enabled", cfg.ConvStore.Enabled,
	)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("收到退出信号，开始优雅关闭")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_ = metricsSrv.Shutdown(shutdownCtx)
	return gwSrv.Shutdown(shutdownCtx)
}

func serve(s *http.Server, name string, log interface{ Error(string, ...any) }, errCh chan<- error) {
	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("监听失败", "server", name, "error", err.Error())
		errCh <- fmt.Errorf("%s 监听失败: %w", name, err)
	}
}

func metricsMux(reg *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	return mux
}
