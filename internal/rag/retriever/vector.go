package retriever

import (
	"context"
	"fmt"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

// VectorRetriever 走 Milvus 做稠密向量召回。
//
// 擅长语义相似：查「怎么防止缓存被击穿」能匹配到「布隆过滤器的应用」，
// 即便两者没有共同的关键词。
//
// 不擅长精确匹配：查「RedisJSON」可能返回一堆讲 Redis 的无关内容，
// 因为专有名词在向量空间里会被稀释成它的组成语义。
// 这正是需要 BM25 补位的原因。
type VectorRetriever struct {
	embedder rag.Embedder
	store    rag.VectorStore
}

func NewVectorRetriever(embedder rag.Embedder, store rag.VectorStore) (*VectorRetriever, error) {
	if embedder == nil {
		return nil, fmt.Errorf("embedder must not be nil")
	}
	if store == nil {
		return nil, fmt.Errorf("store must not be nil")
	}
	return &VectorRetriever{embedder: embedder, store: store}, nil
}

func (r *VectorRetriever) Name() string { return "vector" }

// Recall 返回 topN 个候选，按相似度降序。
func (r *VectorRetriever) Recall(ctx context.Context, query string, topN int) ([]rag.ScoredChunk, error) {
	if query == "" {
		return nil, fmt.Errorf("query must not be empty")
	}
	if topN <= 0 {
		return nil, fmt.Errorf("topN must be > 0, got %d", topN)
	}

	// ⚠️ 查询向量必须用与入库时相同的模型生成，否则两者不在同一向量空间，
	//    检索结果会是纯噪声 —— 而且不会报错，只是结果莫名其妙地差。
	vecs, err := r.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("expected 1 query vector, got %d", len(vecs))
	}

	hits, err := r.store.Search(ctx, vecs[0], topN)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}
	return hits, nil
}
