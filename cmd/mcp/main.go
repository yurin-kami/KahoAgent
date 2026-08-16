package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"

	"github.com/yurin-kami/KahoAgent/internal/config"
)

// MCP Server 独立入口
// 通过 stdio 或 SSE 模式为 AI 客户端（Claude Desktop/Cursor）提供工具服务
func main() {
	cfg, err := config.Load("config.toml")
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	logger, _ := zap.NewProduction()
	if cfg.Debug {
		logger, _ = zap.NewDevelopment()
	}
	defer logger.Sync()
	sugar := logger.Sugar()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sugar.Info("KahoAgent MCP Server starting...")

	// TODO: 初始化 MCP Server
	// 1. 注册 Tools (file_search, file_upload, file_share, metadata_query, rag_query, ...)
	// 2. 启动 JSON-RPC over stdio/SSE 传输

	_ = ctx

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	sugar.Info("MCP Server shutting down...")
}
