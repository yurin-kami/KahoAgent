package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yurin-kami/KahoAgent/internal/app"
	"github.com/yurin-kami/KahoAgent/internal/config"
	"github.com/yurin-kami/KahoAgent/internal/trace"
	"github.com/yurin-kami/KahoAgent/internal/web"
)

const (
	shutdownGrace = 10 * time.Second
	startupGrace  = 60 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	level := slog.LevelInfo
	if cfg.Debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	// 这个 ctx 只覆盖启动期的连接建立
	startCtx, cancelStart := context.WithTimeout(context.Background(), startupGrace)
	defer cancelStart()

	a, err := app.New(startCtx, cfg, logger)
	if err != nil {
		return err
	}
	defer a.Close()

	// 链路追踪。失败只告警不退出：collector 没起来不该让整个服务不可用，
	// 而这正是「可观测性组件不能成为单点」的含义。
	traceShutdown, err := trace.Setup(startCtx, trace.Config{
		Enabled:     cfg.OTel.Enabled,
		Endpoint:    cfg.OTel.Endpoint,
		Insecure:    cfg.OTel.Insecure,
		ServiceName: cfg.OTel.ServiceName,
		SampleRatio: cfg.OTel.SampleRatio,
	})
	if err != nil {
		logger.Warn("tracing is disabled: setup failed", "err", err)
	} else {
		defer func() {
			// 用独立的 context：主 context 可能已经因信号取消，
			// 而 Shutdown 需要时间冲刷最后一批 span。
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := traceShutdown(flushCtx); err != nil {
				logger.Warn("tracing shutdown failed", "err", err)
			}
		}()
	}

	mux := http.NewServeMux()
	// 内嵌控制台：/ 是一个零依赖的单页，直接调 /api/chat。
	// 与 API 同源部署，省掉 CORS，也不会出现「前端指向了测试环境」的问题。
	webHandler, err := web.Handler()
	if err != nil {
		return fmt.Errorf("build web console: %w", err)
	}
	mux.Handle("/", webHandler)

	mux.Handle("/api/chat", a.Handler)
	// 上传只负责「收下并确认」，真正的分块/向量化在 cmd/worker 里异步完成。
	// 两个进程分开部署：worker 是 CPU/IO 密集型（embedding + 双写），
	// 和面向用户的 SSE 服务混在一起会互相抢资源。
	mux.Handle("/api/upload", a.UploadHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Prometheus 抓取端点
	mux.Handle("/metrics", promhttp.Handler())

	// ⚠️ pprof 手动注册到本 mux，而不是 import _ net/http/pprof。
	//
	// 那个包会在 init() 里往 http.DefaultServeMux 注册 —— 而任何
	// 第三方库若也用了 DefaultServeMux，profile 端点就会跟着
	// 业务端口一起暴露出去。pprof 能拉取堆内存、goroutine 栈，
	// 里面可能有密钥和用户数据，绝不能无意中公开。
	// 显式注册则完全可控，将来要加鉴权也只改这一处。
	registerPprof(mux)

	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.Server.Port),
		// 埋点包在最外层：这样 handler 内部的检索/工具 span 都能挂到
		// 同一个 trace 下。⚠️ 中间件必须正确转发 Flusher，
		// 否则 SSE 会被自己的可观测性代码弄坏（见 internal/trace 的测试）。
		Handler: trace.HTTPMiddleware(mux),
		// ⚠️ 不设 WriteTimeout：SSE 是长连接，一个回答可能生成几十秒，
		// 设了会在写到一半时被掐断。超时由 handler 内部的
		// context 控制，粒度更合适。
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("KahoAgent listening", "port", cfg.Server.Port, "debug", cfg.Debug)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return fmt.Errorf("server failed: %w", err)
	case sig := <-quit:
		logger.Info("shutting down", "signal", sig.String())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}
	logger.Info("stopped cleanly")
	return nil
}

// registerPprof 把 profile 端点挂到指定 mux。
//
// 采集方式：
//
//	go tool pprof http://localhost:8080/debug/pprof/profile?seconds=30
//	go tool pprof http://localhost:8080/debug/pprof/heap
func registerPprof(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}
