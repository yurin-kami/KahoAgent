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

	"github.com/cloudwego/eino/components/tool"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/yurin-kami/KahoAgent/internal/agent"
	"github.com/yurin-kami/KahoAgent/internal/agent/tools"
	"github.com/yurin-kami/KahoAgent/internal/config"
	"github.com/yurin-kami/KahoAgent/internal/handler"
	"github.com/yurin-kami/KahoAgent/internal/rag/retriever"
	"github.com/yurin-kami/KahoAgent/pkg/cache"
	"github.com/yurin-kami/KahoAgent/pkg/cloudkaho"
	"github.com/yurin-kami/KahoAgent/pkg/llm"
	"github.com/yurin-kami/KahoAgent/pkg/vectorstore"
)

const shutdownGrace = 10 * time.Second

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

	// ⚠️ 这个 ctx 只用于**启动期**的连接建立，不能传给 HTTP handler。
	// 传下去的话优雅退出时会立刻取消所有在途请求，
	// 而优雅退出的意义正是让在途请求跑完。
	startCtx, cancelStart := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelStart()

	deps, err := dial(startCtx, cfg, logger)
	if err != nil {
		return err
	}
	defer deps.close()

	h, err := buildHandler(startCtx, cfg, deps, logger)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/api/chat", h)
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

// deps 持有所有外部连接，便于统一关闭。
type deps struct {
	store  *vectorstore.MilvusStore
	pool   *pgxpool.Pool
	rdb    *redis.Client
	closed []func()
}

func (d *deps) close() {
	for i := len(d.closed) - 1; i >= 0; i-- {
		d.closed[i]()
	}
}

func dial(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*deps, error) {
	d := &deps{}

	store, err := vectorstore.NewMilvusStore(ctx, cfg.Milvus.Addr,
		cfg.Milvus.Collection, cfg.LLM.EmbeddingDim)
	if err != nil {
		return nil, fmt.Errorf("connect milvus: %w", err)
	}
	d.store = store
	d.closed = append(d.closed, func() { _ = store.Close() })

	pool, err := pgxpool.New(ctx, cfg.DB.DSN)
	if err != nil {
		d.close()
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	d.pool = pool
	d.closed = append(d.closed, pool.Close)

	// PG 连不上不致命：混合检索会降级为纯向量路（Phase 2 已验证）。
	// 启动期就 fail-fast 反而让一个可降级的故障变成全站不可用。
	if err := pool.Ping(ctx); err != nil {
		logger.Warn("postgres is unreachable; retrieval will degrade to vector-only", "err", err)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	d.rdb = rdb
	d.closed = append(d.closed, func() { _ = rdb.Close() })

	if err := rdb.Ping(ctx).Err(); err != nil {
		// Redis 挂了会丢缓存和会话历史，但单轮问答仍可用
		logger.Warn("redis is unreachable; caching and session memory are disabled", "err", err)
	}

	return d, nil
}

func buildHandler(ctx context.Context, cfg *config.Config, d *deps, logger *slog.Logger) (http.Handler, error) {
	embedder := llm.NewEmbedder(cfg.LLM.APIKey, cfg.LLM.BaseURL,
		cfg.LLM.EmbeddingModel, cfg.LLM.EmbeddingDim)

	vec, err := retriever.NewVectorRetriever(embedder, d.store)
	if err != nil {
		return nil, err
	}
	bm25, err := retriever.NewBM25Retriever(d.pool)
	if err != nil {
		return nil, err
	}

	chat := llm.NewChatModel(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model)
	rerank, err := retriever.NewLLMReranker(chat, logger)
	if err != nil {
		return nil, err
	}

	hybrid, err := retriever.NewHybridRetriever(
		[]retriever.Recaller{vec, bm25},
		retriever.WithLogger(logger),
		retriever.WithRerank(rerank),
		retriever.WithCache(cache.NewWithClient(d.rdb, cache.WithLogger(logger))),
	)
	if err != nil {
		return nil, err
	}

	agentTools, err := buildTools(cfg, hybrid, logger)
	if err != nil {
		return nil, err
	}

	a, err := agent.New(ctx, agent.Config{
		Model: llm.NewEinoChatModel(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model),
		Tools: agentTools,
	})
	if err != nil {
		return nil, err
	}

	// summarizer 用同一个 chat 客户端 —— 摘要不需要单独的模型
	memory, err := agent.NewSessionMemory(d.rdb, chat, agent.WithMemoryLogger(logger))
	if err != nil {
		return nil, err
	}

	return handler.NewChatHandler(a, memory, logger)
}

func buildTools(cfg *config.Config, r tools.Retriever, logger *slog.Logger) ([]tool.BaseTool, error) {
	ragTool, err := tools.NewRAGQueryTool(r, tools.WithLogger(logger))
	if err != nil {
		return nil, err
	}
	out := []tool.BaseTool{ragTool}

	// ⚠️ 没配 token 时**不注册**文件工具，而不是注册一个必然失败的。
	//
	// 注册了但没凭证的话，模型会兴冲冲地调用，拿到一堆认证失败，
	// 然后要么反复重试撞上 MaxIterations，要么向用户报告一个
	// 它无法解决的错误。不注册则模型根本不知道有这个能力，
	// 会直接说「我只能搜索文档内容」—— 这才是准确的表述。
	if cfg.CloudKaho.Token == "" {
		logger.Warn("CloudKaho token is not set; file tools are disabled " +
			"(set CLOUDKAHO_TOKEN or cloudkaho.token to enable them)")
		return out, nil
	}

	client, err := cloudkaho.New(cfg.CloudKaho.BaseURL, cfg.CloudKaho.Token)
	if err != nil {
		return nil, err
	}

	search, err := tools.NewFileSearchTool(client, logger)
	if err != nil {
		return nil, err
	}
	meta, err := tools.NewFileMetadataTool(client, logger)
	if err != nil {
		return nil, err
	}
	share, err := tools.NewFileShareTool(client, logger)
	if err != nil {
		return nil, err
	}

	logger.Info("file tools enabled", "cloudkaho", cfg.CloudKaho.BaseURL)
	return append(out, search, meta, share), nil
}
