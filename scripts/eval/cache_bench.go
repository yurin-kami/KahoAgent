//go:build cachebench

// cache_bench 测量 Redis 缓存在**完整检索链路**上的效果。
//
//	go run -tags=cachebench ./scripts/eval/cache_bench.go
//
// 与 pkg/cache 的单元测试的区别：那里测的是缓存组件本身的读写耗时
// （约 0.3ms），这里测的是**接入 HybridRetriever 之后端到端的延迟**。
// 两者差别很大 —— 端到端还包含缓存键计算、结果反序列化，
// 而这些在单测里都被绕过了。验收要求针对的是后者。
//
// 关键设计：冷热必须用**同一批查询**。
// 若冷用 A 组、热用 B 组，测出的差异里混进了查询本身的难度差异
// （有的查询召回多、有的少），无法归因于缓存。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/yurin-kami/KahoAgent/internal/config"
	"github.com/yurin-kami/KahoAgent/internal/eval"
	"github.com/yurin-kami/KahoAgent/internal/rag/retriever"
	"github.com/yurin-kami/KahoAgent/pkg/cache"
	"github.com/yurin-kami/KahoAgent/pkg/llm"
	"github.com/yurin-kami/KahoAgent/pkg/vectorstore"
)

// cacheBudget 是验收要求：缓存命中必须在 10ms 内返回。
const cacheBudget = 10 * time.Millisecond

// sampleSize 取多少条查询。不用全部 77 条是因为冷启动那轮
// 每条都要真调 embedding API，耗时且花钱；20 条足以得到稳定的分位数。
const sampleSize = 20

type goldenEntry struct {
	Query string `json:"query"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n❌ %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	raw, err := os.ReadFile("scripts/eval/golden_set.json")
	if err != nil {
		return err
	}
	var golden []goldenEntry
	if err := json.Unmarshal(raw, &golden); err != nil {
		return err
	}
	if len(golden) > sampleSize {
		golden = golden[:sampleSize]
	}

	cfg, err := config.Load("config.toml")
	if err != nil {
		return err
	}

	embedder := llm.NewEmbedder(cfg.LLM.EmbeddingKey(), cfg.LLM.EmbeddingURL(),
		cfg.LLM.EmbeddingModel, cfg.LLM.EmbeddingDim)
	store, err := vectorstore.NewMilvusStore(ctx, cfg.Milvus.Addr,
		cfg.Milvus.Collection, cfg.LLM.EmbeddingDim)
	if err != nil {
		return err
	}
	defer store.Close()
	pool, err := pgxpool.New(ctx, cfg.DB.DSN)
	if err != nil {
		return err
	}
	defer pool.Close()

	rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
	// 不在这里 defer Close：下面的 QueryCache 接管了这个客户端，
	// 它的 Close 会关掉同一个连接池，重复关闭没有意义。
	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("connect redis: %w", err)
	}

	vec, err := retriever.NewVectorRetriever(embedder, store)
	if err != nil {
		return err
	}
	bm25, err := retriever.NewBM25Retriever(pool)
	if err != nil {
		return err
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	qc := cache.NewWithClient(rdb,
		cache.WithTTL(10*time.Minute), cache.WithLogger(quiet))
	defer qc.Close()

	h, err := retriever.NewHybridRetriever([]retriever.Recaller{vec, bm25},
		retriever.WithLogger(quiet), retriever.WithRecallN(50),
		retriever.WithTopK(5), retriever.WithCache(qc))
	if err != nil {
		return err
	}

	// 清掉本次涉及的缓存，确保第一轮真的是冷的。
	// 不清的话，上一次运行留下的键会让「冷启动」测出热的数字。
	if err := qc.Invalidate(ctx); err != nil {
		return fmt.Errorf("invalidate cache: %w", err)
	}

	fmt.Printf("样本 %d 条查询，缓存已清空\n\n", len(golden))

	// ---- 第一轮：冷（必然回源）----
	fmt.Print("第 1 轮（冷启动，全部回源）...")
	cold, err := measure(ctx, h, golden)
	if err != nil {
		return err
	}
	fmt.Printf(" P50=%v P99=%v\n", rnd(cold.p50), rnd(cold.p99))

	// ---- 第二轮：热（应全部命中）----
	fmt.Print("第 2 轮（应全部命中缓存）...")
	warm, err := measure(ctx, h, golden)
	if err != nil {
		return err
	}
	fmt.Printf(" P50=%v P99=%v\n", rnd(warm.p50), rnd(warm.p99))

	// ---- 第三轮：再热一次，确认不是偶然 ----
	fmt.Print("第 3 轮（复测，排除偶然）...")
	warm2, err := measure(ctx, h, golden)
	if err != nil {
		return err
	}
	fmt.Printf(" P50=%v P99=%v\n", rnd(warm2.p50), rnd(warm2.p99))

	fmt.Printf("\n=== 结果 ===\n")
	fmt.Printf("%-10s %-12s %-12s %-12s\n", "轮次", "P50", "P99", "平均")
	fmt.Println("--------------------------------------------------")
	fmt.Printf("%-10s %-12v %-12v %-12v\n", "冷启动", rnd(cold.p50), rnd(cold.p99), rnd(cold.mean))
	fmt.Printf("%-10s %-12v %-12v %-12v\n", "命中", rnd(warm.p50), rnd(warm.p99), rnd(warm.mean))
	fmt.Printf("%-10s %-12v %-12v %-12v\n", "命中复测", rnd(warm2.p50), rnd(warm2.p99), rnd(warm2.mean))

	speedup := float64(cold.p50) / float64(warm.p50)
	fmt.Printf("\nP50 加速 %.0f×（%v → %v）\n", speedup, rnd(cold.p50), rnd(warm.p50))

	// 用 P99 而非 P50 判定：验收关心的是**最坏情况**是否可接受，
	// 中位数达标而尾部超标的系统在线上依然会被投诉。
	if warm.p99 > cacheBudget {
		return fmt.Errorf("缓存命中 P99 %v 超过预算 %v", rnd(warm.p99), cacheBudget)
	}
	fmt.Printf("✅ 缓存命中 P99 %v < 预算 %v\n", rnd(warm.p99), cacheBudget)
	return nil
}

type stats struct{ p50, p99, mean time.Duration }

func measure(ctx context.Context, h *retriever.HybridRetriever, golden []goldenEntry) (stats, error) {
	var us []float64
	for _, g := range golden {
		start := time.Now()
		hits, err := h.Search(ctx, g.Query)
		elapsed := time.Since(start)
		if err != nil {
			return stats{}, fmt.Errorf("search %q: %w", g.Query, err)
		}
		if len(hits) == 0 {
			// 空结果不进缓存（见 pkg/cache），会让下一轮仍是冷的，
			// 混进来会污染「命中」轮的数字
			continue
		}
		us = append(us, float64(elapsed.Microseconds()))
	}
	if len(us) == 0 {
		return stats{}, fmt.Errorf("没有任何查询返回结果")
	}
	return stats{
		p50:  time.Duration(eval.Percentile(us, 50)) * time.Microsecond,
		p99:  time.Duration(eval.Percentile(us, 99)) * time.Microsecond,
		mean: time.Duration(eval.Mean(us)) * time.Microsecond,
	}, nil
}

func rnd(d time.Duration) time.Duration {
	if d < time.Millisecond {
		return d.Round(10 * time.Microsecond)
	}
	return d.Round(time.Millisecond)
}
