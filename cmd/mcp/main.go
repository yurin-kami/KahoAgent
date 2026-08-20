// KahoAgent MCP Server：把 Phase 3 的工具暴露给 Claude Desktop / Cursor。
//
// stdio 模式（Claude Desktop 配置）：
//
//	{
//	  "mcpServers": {
//	    "kahoagent": {
//	      "command": "A:\\dev_pjs\\KahoAgent\\bin\\kaho-mcp.exe",
//	      "cwd": "A:\\dev_pjs\\KahoAgent"
//	    }
//	  }
//	}
//
// HTTP 模式（远程访问）：
//
//	kaho-mcp -transport=http -port=3000
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yurin-kami/KahoAgent/internal/app"
	"github.com/yurin-kami/KahoAgent/internal/config"
	mcpserver "github.com/yurin-kami/KahoAgent/internal/mcp/server"
)

func main() {
	if err := run(); err != nil {
		// ⚠️ 错误也必须走 stderr，理由同下面的 logger
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		transport = flag.String("transport", "", "stdio 或 http（留空则用配置文件的值）")
		port      = flag.Int("port", 0, "HTTP 模式端口（留空则用配置文件的值）")
	)
	flag.Parse()

	cfg, err := config.Load("config.toml")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if *transport != "" {
		cfg.MCP.Transport = *transport
	}
	if *port != 0 {
		cfg.MCP.Port = *port
	}

	// ⚠️ 日志必须走 stderr。
	//
	// stdio 模式下 stdout 是 JSON-RPC 协议通道，往里写任何非协议内容
	// 都会让客户端解析失败 —— 而且表现是"Claude Desktop 里工具莫名
	// 消失"，完全看不出是日志导致的。Spike S4 已经踩过一次。
	// HTTP 模式虽然没这个约束，但保持一致更省心。
	level := slog.LevelInfo
	if cfg.Debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	startCtx, cancelStart := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelStart()

	// 复用与主服务**完全相同**的装配。
	// 单独搭一套的话，两边的检索配置、降级策略迟早会分叉 ——
	// 而 MCP 路径平时没人盯着，分叉了也不会有人发现。
	a, err := app.New(startCtx, cfg, logger)
	if err != nil {
		return err
	}
	defer a.Close()

	srv, err := mcpserver.New(a.Tools, mcpserver.NewGuard(
		mcpserver.WithGuardLogger(logger),
	), logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cfg.MCP.Transport {
	case "http":
		return serveHTTP(ctx, srv, cfg.MCP.Port, logger)
	case "stdio", "":
		logger.Info("MCP server starting on stdio", "tools", len(a.Tools))
		if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil &&
			!errors.Is(err, context.Canceled) {
			return fmt.Errorf("mcp server failed: %w", err)
		}
		logger.Info("stopped cleanly")
		return nil
	default:
		return fmt.Errorf("unknown transport %q (want stdio or http)", cfg.MCP.Transport)
	}
}

func serveHTTP(ctx context.Context, srv *mcp.Server, port int, logger *slog.Logger) error {
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv }, nil)

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// 不设 WriteTimeout：streamable HTTP 是长连接，
		// 与主服务的 SSE 同理，设了会在传输中途被掐断
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("MCP server listening", "port", port, "transport", "http")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("mcp http server failed: %w", err)
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}
	logger.Info("stopped cleanly")
	return nil
}
