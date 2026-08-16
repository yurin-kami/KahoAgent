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

func main() {
	// 加载配置
	cfg, err := config.Load("config.toml")
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// 初始化日志
	logger, _ := zap.NewProduction()
	if cfg.Debug {
		logger, _ = zap.NewDevelopment()
	}
	defer logger.Sync()
	sugar := logger.Sugar()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sugar.Info("KahoAgent starting...",
		"port", cfg.Server.Port,
		"debug", cfg.Debug,
	)

	// TODO: 初始化各组件
	// 1. 向量数据库连接 (Milvus)
	// 2. Redis 缓存
	// 3. PostgreSQL (BM25)
	// 4. Kafka consumer (文档处理管道)
	// 5. Eino Agent 编排
	// 6. MCP Server
	// 7. HTTP Server (Gin)

	_ = ctx

	// 优雅退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	sugar.Info("KahoAgent shutting down...")
}
