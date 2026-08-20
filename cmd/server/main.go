package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yurin-kami/KahoAgent/internal/app"
	"github.com/yurin-kami/KahoAgent/internal/config"
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
	cfg, err := config.Load("config.toml")
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

	mux := http.NewServeMux()
	mux.Handle("/api/chat", a.Handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Server.Port),
		Handler: mux,
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
