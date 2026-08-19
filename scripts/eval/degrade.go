//go:build degrade

// degrade 验证 Phase 2 的降级要求：**BM25 一路挂掉时检索仍能返回结果**。
//
//	go run -tags=degrade ./scripts/eval/degrade.go
//
// 故障注入方式：把 pgx 指向一个**没有监听的端口**，
// 制造真实的 ECONNREFUSED —— 这与容器被停掉时驱动看到的错误是同一个。
//
// 为什么不用「停容器」：停容器需要人工介入（或额外权限），
// 会让这项验收无法自动重跑；而对被测对象 HybridRetriever 而言，
// 两者完全等价 —— 它只能看到 recaller 返回的 error。
//
// 为什么不用假 recaller 做单测代替：
// 单测能证明「recaller 返回 error 时会降级」，但证明不了
// **pgx 在连不上时返回的是 error 而不是永久阻塞**。
// 后者才是线上真正会出事的地方 —— 若驱动卡在重连上不返回，
// 降级逻辑写得再对也没用，整个请求会一起挂死。
//
// 所以这里额外检查**耗时**：降级必须在预算内完成，
// 不能因为一路挂了就让整体延迟飙升。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yurin-kami/KahoAgent/internal/config"
	"github.com/yurin-kami/KahoAgent/internal/rag"
	"github.com/yurin-kami/KahoAgent/internal/rag/retriever"
	"github.com/yurin-kami/KahoAgent/pkg/llm"
	"github.com/yurin-kami/KahoAgent/pkg/vectorstore"
)

// degradeBudget 是降级必须在多久内完成。
// 取 15s：向量路 P99 约 300ms，留出充裕余量给 pgx 的连接超时，
// 但仍能捕捉「无限重试」这类真正的故障。
const degradeBudget = 15 * time.Second

// deadPort 选一个几乎不会有人监听的高位端口。
// 连它必然 ECONNREFUSED —— 这正是 PG 进程消失时的表现。
const deadPort = "59999"

const probeQuery = "缓存击穿怎么解决"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n❌ %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfg, err := config.Load("config.toml")
	if err != nil {
		return err
	}

	embedder := llm.NewEmbedder(cfg.LLM.APIKey, cfg.LLM.BaseURL,
		cfg.LLM.EmbeddingModel, cfg.LLM.EmbeddingDim)
	store, err := vectorstore.NewMilvusStore(ctx, cfg.Milvus.Addr,
		cfg.Milvus.Collection, cfg.LLM.EmbeddingDim)
	if err != nil {
		return fmt.Errorf("connect milvus: %w", err)
	}
	defer store.Close()

	vec, err := retriever.NewVectorRetriever(embedder, store)
	if err != nil {
		return err
	}

	// 降级警告要看得见 —— 这正是本次验证要观察的行为
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// ---- 阶段 1：两路都正常，建立基线 ----
	fmt.Println("=== 阶段 1：PG 正常 ===")

	healthyPool, err := pgxpool.New(ctx, cfg.DB.DSN)
	if err != nil {
		return fmt.Errorf("create pg pool: %w", err)
	}
	defer healthyPool.Close()

	// 先确认 PG 此刻真的通，否则「阶段 2 成功」可能只是因为它一直没通
	if err := healthyPool.Ping(ctx); err != nil {
		return fmt.Errorf("PG 在阶段 1 就不可达，本次验证无意义: %w", err)
	}

	healthyBM25, err := retriever.NewBM25Retriever(healthyPool)
	if err != nil {
		return err
	}
	healthyHybrid, err := retriever.NewHybridRetriever(
		[]retriever.Recaller{vec, healthyBM25},
		retriever.WithLogger(logger), retriever.WithRecallN(50), retriever.WithTopK(5))
	if err != nil {
		return err
	}

	healthy, err := probe(ctx, healthyHybrid)
	if err != nil {
		return fmt.Errorf("baseline probe failed: %w", err)
	}
	if healthy.n == 0 {
		return fmt.Errorf("baseline 返回 0 条 —— 语料或索引有问题，先修这个")
	}
	fmt.Printf("返回 %d 条，耗时 %v\n\n", healthy.n, healthy.dur.Round(time.Millisecond))

	// ---- 故障注入：把 BM25 指向一个没人监听的端口 ----
	deadDSN := redirectToDeadPort(cfg.DB.DSN)
	fmt.Printf("=== 阶段 2：BM25 不可达（连接 %s 端口，模拟 PG 进程消失）===\n", deadPort)

	deadPool, err := pgxpool.New(ctx, deadDSN)
	if err != nil {
		// pgxpool.New 只解析 DSN 不建连接，正常不会到这
		return fmt.Errorf("create dead pool: %w", err)
	}
	defer deadPool.Close()

	// 确认故障注入真的生效了 —— 否则「降级成功」可能只是因为根本没坏
	pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
	pingErr := deadPool.Ping(pingCtx)
	pingCancel()
	if pingErr == nil {
		return fmt.Errorf("故障注入失败：%s 端口居然连得上，本次验证无意义", deadPort)
	}
	fmt.Printf("已确认 BM25 数据源不可达：%v\n", truncateErr(pingErr))

	deadBM25, err := retriever.NewBM25Retriever(deadPool)
	if err != nil {
		return err
	}
	degradedHybrid, err := retriever.NewHybridRetriever(
		[]retriever.Recaller{vec, deadBM25},
		retriever.WithLogger(logger), retriever.WithRecallN(50), retriever.WithTopK(5))
	if err != nil {
		return err
	}

	degraded, err := probe(ctx, degradedHybrid)
	if err != nil {
		return fmt.Errorf("降级验证失败：BM25 挂掉后检索直接报错，"+
			"未能降级到向量路: %w", err)
	}
	if degraded.n == 0 {
		return fmt.Errorf("降级验证失败：BM25 挂掉后返回 0 条结果。" +
			"向量路本应独立工作 —— 检查 recallAll 是否被一路的失败连累")
	}
	fmt.Printf("返回 %d 条，耗时 %v\n", degraded.n, degraded.dur.Round(time.Millisecond))

	if degraded.dur > degradeBudget {
		return fmt.Errorf("降级验证失败：耗时 %v 超过预算 %v。"+
			"这说明 pgx 在重试而非快速失败 —— 线上会表现为一路挂掉拖垮整体延迟",
			degraded.dur.Round(time.Millisecond), degradeBudget)
	}

	// ---- 反向验证：向量路挂掉时，BM25 也要能独撑 ----
	// 只测一个方向的话，可能恰好是「向量路碰巧总是先返回」而非真的降级。
	fmt.Printf("\n=== 阶段 3：反向 —— 向量路不可达 ===\n")
	deadVec, err := retriever.NewVectorRetriever(embedder, unreachableStore{})
	if err != nil {
		return err
	}
	reverseHybrid, err := retriever.NewHybridRetriever(
		[]retriever.Recaller{deadVec, healthyBM25},
		retriever.WithLogger(logger), retriever.WithRecallN(50), retriever.WithTopK(5))
	if err != nil {
		return err
	}
	reverse, err := probe(ctx, reverseHybrid)
	if err != nil {
		return fmt.Errorf("反向降级失败：向量路挂掉后检索报错: %w", err)
	}
	if reverse.n == 0 {
		return fmt.Errorf("反向降级失败：向量路挂掉后返回 0 条")
	}
	fmt.Printf("返回 %d 条，耗时 %v\n", reverse.n, reverse.dur.Round(time.Millisecond))

	fmt.Printf("\n=== 结论 ===\n")
	fmt.Printf("两路正常  ：%d 条 / %v\n", healthy.n, healthy.dur.Round(time.Millisecond))
	fmt.Printf("BM25 挂掉 ：%d 条 / %v\n", degraded.n, degraded.dur.Round(time.Millisecond))
	fmt.Printf("向量挂掉  ：%d 条 / %v\n", reverse.n, reverse.dur.Round(time.Millisecond))
	fmt.Printf("✅ 降级验证通过：任一路故障都不影响服务可用性，且延迟在 %v 预算内\n", degradeBudget)
	return nil
}

type probeResult struct {
	n   int
	dur time.Duration
}

func probe(ctx context.Context, h *retriever.HybridRetriever) (probeResult, error) {
	start := time.Now()
	hits, err := h.Search(ctx, probeQuery)
	return probeResult{n: len(hits), dur: time.Since(start)}, err
}

// redirectToDeadPort 把 DSN 里的端口换成一个没人监听的端口。
// 其余部分（用户名、库名）保持不变，让失败原因干净地落在「连不上」，
// 而不是混入认证失败之类的其他错误。
//
// 两种 DSN 形式都要支持：
//   - 关键字形式：host=127.0.0.1 port=5432 user=kaho ...
//   - URL 形式：  postgres://user:pass@host:5432/db
//
// 本项目用的是前者，但两种都处理 —— 只认一种的话，换个写法就会
// 静默地不生效，而「注入没生效」和「降级成功」在输出上长得一模一样。
// （所以调用处还有一道 Ping 断言兜底。）
func redirectToDeadPort(dsn string) string {
	if strings.Contains(dsn, "://") {
		return redirectURLPort(dsn)
	}
	return redirectKeywordPort(dsn)
}

func redirectKeywordPort(dsn string) string {
	fields := strings.Fields(dsn)
	found := false
	for i, f := range fields {
		if strings.HasPrefix(f, "port=") {
			fields[i] = "port=" + deadPort
			found = true
		}
	}
	if !found {
		// 没写 port 时默认 5432，补一个指向死端口的
		fields = append(fields, "port="+deadPort)
	}
	// connect_timeout 防止验证卡死：没有它，连不上时 pgx 可能
	// 一直等到系统级 TCP 超时（Windows 上可达 20s+），
	// 会让「快速失败」的判定失真。
	if !strings.Contains(dsn, "connect_timeout") {
		fields = append(fields, "connect_timeout=3")
	}
	return strings.Join(fields, " ")
}

func redirectURLPort(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn
	}
	hostPart := dsn[at+1:]
	end := strings.IndexAny(hostPart, "/?")
	if end < 0 {
		end = len(hostPart)
	}
	hostPort := hostPart[:end]
	host := hostPort
	if c := strings.LastIndex(hostPort, ":"); c >= 0 {
		host = hostPort[:c]
	}
	return dsn[:at+1] + host + ":" + deadPort + hostPart[end:]
}

// unreachableStore 是必定失败的 VectorStore，用于验证反向降级。
type unreachableStore struct{}

func (unreachableStore) Search(context.Context, []float32, int) ([]rag.ScoredChunk, error) {
	return nil, fmt.Errorf("simulated milvus outage")
}
func (unreachableStore) Upsert(context.Context, []rag.Chunk, [][]float32) error {
	return fmt.Errorf("simulated milvus outage")
}
func (unreachableStore) DeleteByDoc(context.Context, string) error {
	return fmt.Errorf("simulated milvus outage")
}

func truncateErr(err error) string {
	s := err.Error()
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}
