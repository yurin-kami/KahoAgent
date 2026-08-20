package retriever

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	einoretriever "github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"

	"github.com/yurin-kami/KahoAgent/internal/metrics"
	"github.com/yurin-kami/KahoAgent/internal/rag"
)

// 编译期断言：确保实现了 eino 的接口。
// 这是 Phase 3 能把自研检索器塞进 Eino Graph 的前提 ——
// 接口对不上会在这里报错，而不是等到搭 Agent 时才发现。
var _ einoretriever.Retriever = (*HybridRetriever)(nil)

// 默认参数。
//
// recallN=50：每路召回 50 条送去融合。太少则 RRF 没有足够的
// 交叉印证信号，太多则 rerank 成本上升。
// topK=5：LLM 通常只看前 3-5 个块，再多只是浪费 token。
const (
	DefaultRecallN = 50
	DefaultTopK    = 5
	// rerankCandidates 是送去精排的候选数。
	// 融合后取前 20 精排 —— 全部 100 条都精排太慢且没必要，
	// RRF 已经把明显不相关的排到后面了。
	rerankCandidates = 20
)

// Recaller 是单路检索器的抽象。
// 抽成接口是为了让 HybridRetriever 能在测试中注入假检索器，
// 从而不依赖 Milvus 和 PG 就能验证融合与降级逻辑。
type Recaller interface {
	Name() string
	Recall(ctx context.Context, query string, topN int) ([]rag.ScoredChunk, error)
}

// Reranker 对候选做精排。可为 nil，nil 时跳过精排。
type Reranker interface {
	Rerank(ctx context.Context, query string, candidates []rag.ScoredChunk, topK int) ([]rag.ScoredChunk, error)
}

// QueryCache 缓存检索结果。可为 nil。
type QueryCache interface {
	Get(ctx context.Context, key string) ([]rag.ScoredChunk, bool)
	Set(ctx context.Context, key string, chunks []rag.ScoredChunk)
}

// HybridRetriever 双路召回 + RRF 融合 + 可选精排。
//
// 为什么需要混合（L2.1）：
//   - 向量检索擅长语义（「防止缓存被击穿」→「布隆过滤器」），
//     但对专有名词会稀释语义
//   - BM25 擅长关键词精确匹配，但同义词完全无能为力
//
// 两者的失败模式正交，所以融合后效果优于任何一路。
type HybridRetriever struct {
	recallers []Recaller
	reranker  Reranker
	cache     QueryCache
	logger    *slog.Logger

	recallN int
	topK    int
}

// HybridOption 用于覆盖默认配置。
type HybridOption func(*HybridRetriever)

func WithRerank(r Reranker) HybridOption {
	return func(h *HybridRetriever) { h.reranker = r }
}

func WithCache(c QueryCache) HybridOption {
	return func(h *HybridRetriever) { h.cache = c }
}

func WithRecallN(n int) HybridOption {
	return func(h *HybridRetriever) {
		if n > 0 {
			h.recallN = n
		}
	}
}

func WithTopK(k int) HybridOption {
	return func(h *HybridRetriever) {
		if k > 0 {
			h.topK = k
		}
	}
}

func WithLogger(l *slog.Logger) HybridOption {
	return func(h *HybridRetriever) {
		if l != nil {
			h.logger = l
		}
	}
}

func NewHybridRetriever(recallers []Recaller, opts ...HybridOption) (*HybridRetriever, error) {
	if len(recallers) == 0 {
		return nil, fmt.Errorf("at least one recaller is required")
	}
	h := &HybridRetriever{
		recallers: recallers,
		logger:    slog.Default(),
		recallN:   DefaultRecallN,
		topK:      DefaultTopK,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// Search 执行混合检索，返回 topK 个最相关的 Chunk。
//
// 这是核心实现；Retrieve 只是它到 eino 类型的适配层。
func (h *HybridRetriever) Search(ctx context.Context, query string) ([]rag.ScoredChunk, error) {
	if query == "" {
		return nil, fmt.Errorf("query must not be empty")
	}

	start := time.Now()

	// 1. 查缓存。key 必须包含策略参数 —— 否则改了 topK
	//    还会命中用旧参数算出的结果。
	key := h.cacheKey(query)
	if h.cache != nil {
		hit, ok := h.cache.Get(ctx, key)
		metrics.RecordCache("retrieval", ok)
		if ok {
			// ⚠️ 命中也要记 hybrid 耗时。
			// 只记未命中的话，P99 会虚高 —— 而用户感知到的是**所有**
			// 请求的分布，其中相当一部分是毫秒级的缓存命中。
			metrics.ObserveRetrieval("hybrid", time.Since(start).Seconds())
			return hit, nil
		}
	}

	// 2. 双路并发召回
	rankings, chunkByID := h.recallAll(ctx, query)
	if len(chunkByID) == 0 {
		// 所有路都失败或都没结果。区分这两种情况没有意义 ——
		// 对调用方而言都是「没查到」。
		return nil, nil
	}

	// 3. RRF 融合
	fused := FuseRRF(rankings)

	// 4. 还原成 Chunk。RRF 分数与余弦相似度不可比，
	//    但同一次检索内部可比，用于排序足够。
	results := make([]rag.ScoredChunk, 0, len(fused))
	for _, f := range fused {
		c, ok := chunkByID[f.ID]
		if !ok {
			continue // 理论上不会发生，防御性跳过
		}
		results = append(results, rag.ScoredChunk{Chunk: c, Score: float32(f.Score)})
	}

	// 5. 精排（可选）
	if h.reranker != nil {
		candidates := results
		if len(candidates) > rerankCandidates {
			candidates = candidates[:rerankCandidates]
		}
		rerankStart := time.Now()
		reranked, err := h.reranker.Rerank(ctx, query, candidates, h.topK)
		metrics.ObserveRetrieval("rerank", time.Since(rerankStart).Seconds())
		if err != nil {
			// 精排失败不应让整次检索失败 —— RRF 的结果已经可用了，
			// 降级返回它好过返回错误。
			h.logger.Warn("rerank failed, falling back to RRF order", "err", err)
		} else {
			results = reranked
		}
	}

	if len(results) > h.topK {
		results = results[:h.topK]
	}

	if h.cache != nil {
		h.cache.Set(ctx, key, results)
	}
	metrics.ObserveRetrieval("hybrid", time.Since(start).Seconds())
	return results, nil
}

// recallAll 并发执行所有召回路径，返回各路排名和 ID→Chunk 映射。
//
// ⚠️ 刻意不用 errgroup：它在首个错误时会取消 context，
// 导致一路失败连累另一路被取消 —— 这与降级要求正好相反。
// BM25 挂了还有向量结果，反之亦然，任何一路的失败都只记日志。
func (h *HybridRetriever) recallAll(ctx context.Context, query string) ([]Ranking, map[string]rag.Chunk) {
	var (
		mu        sync.Mutex
		wg        sync.WaitGroup
		rankings  = make([]Ranking, len(h.recallers))
		chunkByID = make(map[string]rag.Chunk)
	)

	for i, r := range h.recallers {
		wg.Add(1)
		go func() {
			defer wg.Done()

			recallStart := time.Now()
			hits, err := r.Recall(ctx, query, h.recallN)
			// 按路记耗时：vector 和 bm25 差着两个数量级
			// （Phase 2 实测 135ms vs 4ms），混在一起看不出是哪路慢
			metrics.ObserveRetrieval(r.Name(), time.Since(recallStart).Seconds())
			if err != nil {
				// 降级：记日志但不影响其他路
				h.logger.Warn("recaller failed, degrading",
					"recaller", r.Name(), "err", err)
				return
			}

			ids := make([]string, 0, len(hits))
			mu.Lock()
			for _, hit := range hits {
				ids = append(ids, hit.Chunk.ID)
				// 同一 chunk 可能被多路召回，存一次即可
				if _, seen := chunkByID[hit.Chunk.ID]; !seen {
					chunkByID[hit.Chunk.ID] = hit.Chunk
				}
			}
			rankings[i] = Ranking{Name: r.Name(), IDs: ids}
			mu.Unlock()
		}()
	}
	wg.Wait()

	// 过滤掉失败或无结果的路，避免空 Ranking 干扰融合
	out := make([]Ranking, 0, len(rankings))
	for _, r := range rankings {
		if len(r.IDs) > 0 {
			out = append(out, r)
		}
	}
	return out, chunkByID
}

// cacheKey 由查询和全部影响结果的参数生成。
//
// ⚠️ 必须包含参数：只用 query 做 key 的话，改了 topK 或开关了 rerank
// 仍会命中旧缓存，评估时会得到莫名其妙的结果。
func (h *HybridRetriever) cacheKey(query string) string {
	rerank := "norerank"
	if h.reranker != nil {
		rerank = "rerank"
	}
	names := ""
	for _, r := range h.recallers {
		names += r.Name() + ","
	}
	return fmt.Sprintf("hybrid:%s:n=%d:k=%d:%s:%s", names, h.recallN, h.topK, rerank, query)
}

// Retrieve 实现 eino 的 Retriever 接口。
//
// 只是 Search 到 eino 类型的适配层 —— 业务逻辑全在 Search 里，
// 这样评估脚本可以直接用 Search 拿 []rag.ScoredChunk，
// 不必绕经 eino 的类型。
func (h *HybridRetriever) Retrieve(ctx context.Context, query string, opts ...einoretriever.Option) ([]*schema.Document, error) {
	// eino 的 TopK 选项应当覆盖默认值
	o := einoretriever.GetCommonOptions(&einoretriever.Options{}, opts...)

	hr := h
	if o.TopK != nil && *o.TopK > 0 && *o.TopK != h.topK {
		// 复制一份改 topK，避免并发调用互相污染
		clone := *h
		clone.topK = *o.TopK
		hr = &clone
	}

	chunks, err := hr.Search(ctx, query)
	if err != nil {
		return nil, err
	}

	docs := make([]*schema.Document, 0, len(chunks))
	for _, c := range chunks {
		// ScoreThreshold 是过滤器而非排序依据：低于阈值的整条剔除
		if o.ScoreThreshold != nil && float64(c.Score) < *o.ScoreThreshold {
			continue
		}
		docs = append(docs, &schema.Document{
			ID:      c.Chunk.ID,
			Content: c.Chunk.Content,
			MetaData: map[string]any{
				"doc_id":       c.Chunk.DocID,
				"chunk_index":  c.Chunk.Index,
				"score":        c.Score,
				"heading_path": c.Chunk.Metadata["heading_path"],
				"source":       c.Chunk.Metadata["source"],
			},
		})
	}
	return docs, nil
}
