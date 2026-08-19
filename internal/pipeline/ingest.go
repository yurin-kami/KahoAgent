// Package pipeline 编排文档入库的完整流程。
package pipeline

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

// Ingester 是文档入库的编排器。
//
// 设计意图：把 chunk → embed → upsert 三步串起来，但自己不实现任何一步 ——
// 全部依赖注入的接口。这样：
//   - 单测可以注入 mock，不依赖外部服务（本文件的测试就是这么跑的）
//   - Phase 2 换分块策略只需换注入的 Chunker，此处代码不动
//   - Kafka consumer 只是调用它的一层壳，不重复写逻辑
//
// 这是「依赖倒置」的实际收益：高层的编排逻辑不依赖低层的具体实现，
// 两者都依赖 internal/rag 里的接口。
type Ingester struct {
	chunker  rag.Chunker
	embedder rag.Embedder
	store    rag.VectorStore
	logger   *slog.Logger
}

// NewIngester 组装入库管道。三个依赖都是必需的，缺失即刻报错 ——
// 让 nil 依赖在启动时暴露，而不是等到第一次入库时空指针。
func NewIngester(chunker rag.Chunker, embedder rag.Embedder, store rag.VectorStore, logger *slog.Logger) (*Ingester, error) {
	if chunker == nil {
		return nil, fmt.Errorf("chunker must not be nil")
	}
	if embedder == nil {
		return nil, fmt.Errorf("embedder must not be nil")
	}
	if store == nil {
		return nil, fmt.Errorf("store must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Ingester{chunker: chunker, embedder: embedder, store: store, logger: logger}, nil
}

// Run 将一篇文档切块、向量化并写入向量库，返回写入的块数。
//
// 幂等性：同一 docID + 同一内容重复调用，结果完全一致 —— 由稳定的
// ChunkID（sha256(docID#index)）加 Milvus Upsert 共同保证。
// 这让 Kafka 的「至少一次」投递变得安全：重复消费不会产生重复数据。
func (i *Ingester) Run(ctx context.Context, docID string, raw []byte) (int, error) {
	if docID == "" {
		return 0, fmt.Errorf("docID must not be empty")
	}

	// 1. 分块
	chunks, err := i.chunker.Chunk(ctx, docID, raw)
	if err != nil {
		return 0, fmt.Errorf("chunk doc %s: %w", docID, err)
	}
	// 空文档返回 0 而非报错：上传一个空文件是合法操作，不是故障。
	// 若当成错误，Kafka 侧会不停重试这条永远不会成功的消息。
	if len(chunks) == 0 {
		i.logger.Info("document produced no chunks, skipping", "doc_id", docID)
		return 0, nil
	}

	// 2. 提取文本批量向量化。
	//    批量而非逐条：Embedder 内部会按 batchSize 分批，
	//    100 个 chunk 从 100 次 HTTP 往返降到 2 次。
	texts := make([]string, len(chunks))
	for idx, c := range chunks {
		texts[idx] = c.Content
	}
	vectors, err := i.embedder.Embed(ctx, texts)
	if err != nil {
		return 0, fmt.Errorf("embed %d chunks of doc %s: %w", len(chunks), docID, err)
	}

	// 3. 防御性校验：向量数必须等于块数。
	//    Embedder 已有同样的检查，这里再查一次是因为 Embedder 是可替换的接口 ——
	//    将来换实现时这道防线仍在。数量错位会导致「文本和向量对不上」，
	//    检索结果看着正常但内容全错，是最难排查的一类 bug。
	if len(vectors) != len(chunks) {
		return 0, fmt.Errorf("doc %s: got %d vectors for %d chunks", docID, len(vectors), len(chunks))
	}

	// 4. 写入
	if err := i.store.Upsert(ctx, chunks, vectors); err != nil {
		return 0, fmt.Errorf("upsert %d chunks of doc %s: %w", len(chunks), docID, err)
	}

	i.logger.Info("ingested document",
		"doc_id", docID, "chunks", len(chunks), "chunker", i.chunker.Name())
	return len(chunks), nil
}

// Delete 移除一篇文档的全部 chunk。
//
// 放在 Ingester 上而非让调用方直接碰 VectorStore：文档的生命周期
// 应由同一个组件掌管。Phase 2 加入 BM25 索引后，删除需要同时清理
// Milvus 和 PostgreSQL，届时只需改这里，调用方无感。
func (i *Ingester) Delete(ctx context.Context, docID string) error {
	if docID == "" {
		return fmt.Errorf("docID must not be empty")
	}
	if err := i.store.DeleteByDoc(ctx, docID); err != nil {
		return fmt.Errorf("delete doc %s: %w", docID, err)
	}
	i.logger.Info("deleted document", "doc_id", docID)
	return nil
}
