// Command worker 是文档异步入库的消费者进程。
//
// 为什么单独一个进程，而不是在 server 里起一个 goroutine：
//   - 资源画像完全不同：server 是 IO 等待型（等 LLM 吐 token），
//     worker 是 CPU + 外呼型（切块、调 Embedding API、写 Milvus/PG）。
//     混在一起时，一次批量入库就能把 SSE 的首字延迟拖垮。
//   - 生命周期不同：server 要常驻且随时可滚动更新；worker 允许
//     处理完当前消息再停，扩缩容策略也不同（按堆积量扩，而不是按 QPS）。
//   - 故障隔离：worker 崩溃不影响在线问答，反过来也一样。
//
// 启动顺序：本进程不依赖 server，但两者共享 config.toml 与下游存储。
// 建议先起 server（它会在 Kafka 不可用时降级），再起 worker。
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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yurin-kami/KahoAgent/internal/config"
	"github.com/yurin-kami/KahoAgent/internal/pipeline"
	"github.com/yurin-kami/KahoAgent/internal/rag/retriever"
	"github.com/yurin-kami/KahoAgent/internal/trace"
	"github.com/yurin-kami/KahoAgent/pkg/llm"
	"github.com/yurin-kami/KahoAgent/pkg/vectorstore"
)

const (
	startupGrace  = 60 * time.Second
	shutdownGrace = 15 * time.Second
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

	// signal.NotifyContext 把「收到 SIGINT/SIGTERM」变成一个 ctx.Done()。
	// 比手写 channel 简单，且所有下游组件都只认 ctx —— 不用各自注册信号。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startCtx, cancelStart := context.WithTimeout(ctx, startupGrace)
	defer cancelStart()

	// 追踪：失败只告警。worker 的核心职责是消费，不该因为
	// collector 没起来就停止入库。
	traceShutdown, terr := trace.Setup(startCtx, trace.Config{
		Enabled:     cfg.OTel.Enabled,
		Endpoint:    cfg.OTel.Endpoint,
		Insecure:    cfg.OTel.Insecure,
		ServiceName: cfg.OTel.ServiceName + "-worker",
		SampleRatio: cfg.OTel.SampleRatio,
	})
	if terr != nil {
		logger.Warn("tracing is disabled: setup failed", "err", terr)
	} else {
		defer func() {
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = traceShutdown(flushCtx)
		}()
	}

	// ── 依赖装配 ────────────────────────────────────────────────
	// 顺序有意为之：先连存储，最后连 Kafka。worker 如果连不上 Kafka
	// 也不该空转，早失败比在消费循环里反复报错更容易定位。
	store, err := vectorstore.NewMilvusStore(startCtx, cfg.Milvus.Addr,
		cfg.Milvus.Collection, cfg.LLM.EmbeddingDim)
	if err != nil {
		return fmt.Errorf("connect milvus: %w", err)
	}
	defer func() { _ = store.Close() }()

	// 幂等建表/建索引/加载。worker 是第一个写 Milvus 的角色，
	// 空库时必须由它把 collection 建出来，否则第一条消息就会失败。
	if err := store.EnsureCollection(startCtx); err != nil {
		return fmt.Errorf("ensure milvus collection: %w", err)
	}

	pool, err := pgxpool.New(startCtx, cfg.DB.DSN)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	// ⚠️ 与 server 的降级策略相反：server 连不上 PG 还能降级成纯向量检索，
	// worker 不行 —— 它的职责就是双写，缺了关键词索引写一半，
	// 结果是「检索得到但 BM25 永远缺一路」的静默劣化。宁可起不来。
	if err := pool.Ping(startCtx); err != nil {
		return fmt.Errorf("postgres is required by the worker: %w", err)
	}

	embedder := llm.NewEmbedder(cfg.LLM.EmbeddingKey(), cfg.LLM.EmbeddingURL(),
		cfg.LLM.EmbeddingModel, cfg.LLM.EmbeddingDim)

	keyword, err := retriever.NewChunkWriter(pool)
	if err != nil {
		return fmt.Errorf("build keyword writer: %w", err)
	}

	// ExtChunker 按 docID 的扩展名在 Markdown / 纯文本两种切法间分发。
	// 传 nil 表示用默认参数（Markdown 按标题层级，纯文本 500 字 + 80 重叠）。
	ingester, err := pipeline.NewIngester(
		pipeline.NewExtChunker(nil, nil),
		embedder,
		store,
		logger,
		pipeline.WithKeywordStore(keyword),
		// 线上路径必须开启：用户把文档改短后重新上传，
		// 不清理旧块就会残留上一版的尾部内容。
		pipeline.WithStaleCleanup(),
	)
	if err != nil {
		return fmt.Errorf("build ingester: %w", err)
	}

	// 先幂等建 topic 再起消费者。
	// ⚠️ 实测：worker 先于 topic 启动时，消费组会一直空转 ——
	// kafka-go 在加入组时 topic 还不存在，之后不会自动发现它被创建。
	if err := pipeline.EnsureTopic(startCtx, cfg.Kafka.Brokers, cfg.Kafka.Topic, 1); err != nil {
		return fmt.Errorf("ensure kafka topic: %w", err)
	}

	// 死信投递端：与主 topic 分开的 writer。
	// DLQTopic 设为 "-" 可显式关闭（退化为无限重试的旧行为）。
	consumerOpts := []pipeline.ConsumerOption{
		pipeline.WithMaxAttempts(cfg.Kafka.MaxAttempts),
	}
	if cfg.Kafka.DLQTopic != "" && cfg.Kafka.DLQTopic != "-" {
		dlq, err := pipeline.NewProducer(cfg.Kafka.Brokers, cfg.Kafka.DLQTopic)
		if err != nil {
			return fmt.Errorf("build dlq producer: %w", err)
		}
		defer func() { _ = dlq.Close() }()
		// 死信 topic 同样先建好，避免第一次转投时失败
		if err := pipeline.EnsureTopic(startCtx, cfg.Kafka.Brokers, cfg.Kafka.DLQTopic, 1); err != nil {
			return fmt.Errorf("ensure dlq topic: %w", err)
		}
		consumerOpts = append(consumerOpts, pipeline.WithDLQ(dlq))
		logger.Info("dead-letter queue enabled",
			"topic", cfg.Kafka.DLQTopic, "max_attempts", cfg.Kafka.MaxAttempts)
	}

	consumer, err := pipeline.NewConsumer(cfg.Kafka.Brokers, cfg.Kafka.Topic,
		cfg.Kafka.GroupID, ingester, logger, consumerOpts...)
	if err != nil {
		return fmt.Errorf("build consumer: %w", err)
	}
	defer func() { _ = consumer.Close() }()

	// ── 指标端点 ────────────────────────────────────────────────
	// worker 不起 Prometheus 的话，入库延迟/失败率就只能翻日志。
	metricsSrv := serveMetrics(logger)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = metricsSrv.Shutdown(shutdownCtx)
	}()

	logger.Info("worker started",
		"topic", cfg.Kafka.Topic, "group", cfg.Kafka.GroupID,
		"brokers", cfg.Kafka.Brokers, "metrics_addr", metricsSrv.Addr)

	// Start 阻塞到 ctx 取消。返回 nil 表示正常停机。
	if err := consumer.Start(ctx); err != nil {
		return fmt.Errorf("consume: %w", err)
	}
	logger.Info("worker stopped cleanly")
	return nil
}

// metricsAddr 是 worker 的指标端口。
//
// 默认 9191：worker 与 server 常常同机部署，不能和 server 的 8080 撞；
// 同时避开 Milvus 的 9091、Prometheus 的 9090、Kafka 的 9092 ——
// 本机联调时 9091 已经被 Milvus 占用，实测这个冲突会让 worker 指标起不来。
// 用环境变量覆盖是为了在 K8s 里能按 Pod 注入不同端口。
func metricsAddr() string {
	if v := os.Getenv("KAHO_WORKER_METRICS_ADDR"); v != "" {
		return v
	}
	return ":9191"
}

// serveMetrics 在后台起一个只暴露 /metrics 的 HTTP 服务。
//
// 单独一个监听端口而不是复用 server 的：worker 通常不对外暴露，
// 指标只给集群内的 Prometheus 抓取，暴露面越小越好。
func serveMetrics(logger *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:              metricsAddr(),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("worker metrics server failed", "err", err)
		}
	}()
	return srv
}
