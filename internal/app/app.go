// Package app 负责把各个组件装配成一个可服务的应用。
//
// 为什么不直接写在 cmd/server/main.go 里：
// 装配逻辑是**最容易出错却最难测**的部分 —— 依赖注错、顺序搞反、
// 降级分支写漏，编译期全都发现不了。放在 main 包里则完全无法被
// 测试代码 import，端到端验证只能照抄一份装配代码，
// 于是就出现「e2e 全绿但线上起不来」这种最坏情况。
// 提到这里之后，e2e 跑的是与生产**同一份**装配。
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

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

// App 持有装配好的 HTTP 处理器与其依赖的外部连接。
type App struct {
	Handler http.Handler

	// Tools 是注册给 Agent 的工具集，MCP Server 直接复用同一份 ——
	// 各自装配一套的话，两边的检索配置与降级策略迟早分叉，
	// 而 MCP 路径平时没人盯着，分叉了也不会有人发现。
	Tools []tool.BaseTool

	// Memory 暴露出来供测试清理会话；生产代码不需要碰它。
	Memory *agent.SessionMemory

	store *vectorstore.MilvusStore
	pool  *pgxpool.Pool
	rdb   *redis.Client

	closers []func()
}

// New 建立所有外部连接并装配出 HTTP 处理器。
//
// ctx 只用于**启动期**的连接建立，不会被存下来传给请求处理 ——
// 传下去的话优雅退出时会立刻取消所有在途请求，
// 而优雅退出的意义正是让在途请求跑完。
func New(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*App, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}

	a := &App{}
	if err := a.dial(ctx, cfg, logger); err != nil {
		return nil, err
	}
	if err := a.build(ctx, cfg, logger); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

// Close 逆序释放所有连接。
func (a *App) Close() {
	for i := len(a.closers) - 1; i >= 0; i-- {
		a.closers[i]()
	}
	a.closers = nil
}

func (a *App) dial(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	store, err := vectorstore.NewMilvusStore(ctx, cfg.Milvus.Addr,
		cfg.Milvus.Collection, cfg.LLM.EmbeddingDim)
	if err != nil {
		return fmt.Errorf("connect milvus: %w", err)
	}
	a.store = store
	a.closers = append(a.closers, func() { _ = store.Close() })

	pool, err := pgxpool.New(ctx, cfg.DB.DSN)
	if err != nil {
		a.Close()
		return fmt.Errorf("connect postgres: %w", err)
	}
	a.pool = pool
	a.closers = append(a.closers, pool.Close)

	// PG 连不上不致命：混合检索会降级为纯向量路（Phase 2 已实测验证）。
	// 启动期就 fail-fast 反而把一个可降级的故障变成了全站不可用。
	if err := pool.Ping(ctx); err != nil {
		logger.Warn("postgres is unreachable; retrieval will degrade to vector-only", "err", err)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	a.rdb = rdb
	a.closers = append(a.closers, func() { _ = rdb.Close() })

	if err := rdb.Ping(ctx).Err(); err != nil {
		// Redis 挂了会丢缓存和会话历史，但单轮问答仍然可用
		logger.Warn("redis is unreachable; caching and session memory are disabled", "err", err)
	}
	return nil
}

func (a *App) build(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	embedder := llm.NewEmbedder(cfg.LLM.APIKey, cfg.LLM.BaseURL,
		cfg.LLM.EmbeddingModel, cfg.LLM.EmbeddingDim)

	vec, err := retriever.NewVectorRetriever(embedder, a.store)
	if err != nil {
		return err
	}
	bm25, err := retriever.NewBM25Retriever(a.pool)
	if err != nil {
		return err
	}

	chat := llm.NewChatModel(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model)
	rerank, err := retriever.NewLLMReranker(chat, logger)
	if err != nil {
		return err
	}

	hybrid, err := retriever.NewHybridRetriever(
		[]retriever.Recaller{vec, bm25},
		retriever.WithLogger(logger),
		retriever.WithRerank(rerank),
		retriever.WithCache(cache.NewWithClient(a.rdb, cache.WithLogger(logger))),
	)
	if err != nil {
		return err
	}

	agentTools, err := buildTools(cfg, hybrid, logger)
	if err != nil {
		return err
	}
	a.Tools = agentTools

	ag, err := agent.New(ctx, agent.Config{
		Model: llm.NewEinoChatModel(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model),
		Tools: agentTools,
	})
	if err != nil {
		return err
	}

	// 摘要复用同一个 chat 客户端 —— 压缩历史不需要另配模型
	mem, err := agent.NewSessionMemory(a.rdb, chat, agent.WithMemoryLogger(logger))
	if err != nil {
		return err
	}
	a.Memory = mem

	h, err := handler.NewChatHandler(ag, mem, logger)
	if err != nil {
		return err
	}
	a.Handler = h
	return nil
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
