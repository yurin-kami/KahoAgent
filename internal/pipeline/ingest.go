// Package pipeline 编排文档入库的完整流程。
package pipeline

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"

	"github.com/yurin-kami/KahoAgent/internal/rag"
	"github.com/yurin-kami/KahoAgent/internal/trace"
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
	// keyword 可为 nil：纯向量场景（评估脚本、单测）不需要 PostgreSQL。
	// 一旦装配，Run 会在写完向量后把同一批 chunk 写进关键词索引，
	// 两条召回路径的数据才能在同一个版本上对齐。
	keyword rag.KeywordStore
	// replace 为 true 时，写入前先按 docID 清空旧块。
	//
	// 解决的是「更新后变短」的场景：chunk ID 是 sha256(docID#index)，
	// 新版本只有 8 块而旧版本有 12 块时，编号 8~11 的旧块不会被覆盖，
	// 会永远留在索引里 —— 检索结果里就出现了同一篇文档的两段历史内容，
	// 拼接起来是一篇根本不存在的文档。
	replace bool
	logger  *slog.Logger
}

// IngesterOption 是可选装配项。
//
// 用 Option 而不是把 keyword 加进 NewIngester 的必填参数，是为了不破坏
// 现有调用方（评估脚本、集成测试、单元测试）—— 它们只关心向量那条路。
type IngesterOption func(*Ingester)

// WithKeywordStore 装配关键词索引写入端（PostgreSQL）。
//
// ⚠️ 装配顺序有讲究：Run 先写 Milvus 再写 PG。若 PG 写失败，Run 返回 error，
// consumer 不提交 offset，消息会被重新消费 —— 而 Milvus 的写入是
// 按 Chunk.ID 的 upsert，重复执行不会产生重复数据。反过来先写 PG 也一样，
// 关键是**两个存储的写入都必须是幂等的**，否则重试会制造脏数据。
func WithKeywordStore(k rag.KeywordStore) IngesterOption {
	return func(i *Ingester) { i.keyword = k }
}

// WithStaleCleanup 让入库在写入前先清空该文档的旧块。
//
// ⚠️ 为什么默认关闭：评估脚本（scripts/eval/ingest_corpus.go）是
// 一次性全量重建，清空再写入会让「删了一半时 embedding 失败」的代价
// 从「部分未更新」变成「整篇消失」。而在线上传路径必须打开 ——
// 用户改短文档是常态，残留的尾部块会被检索出来，比整篇消失更难发现。
func WithStaleCleanup() IngesterOption {
	return func(i *Ingester) { i.replace = true }
}

// NewIngester 组装入库管道。三个核心依赖都是必需的，缺失即刻报错 ——
// 让 nil 依赖在启动时暴露，而不是等到第一次入库时空指针。
func NewIngester(chunker rag.Chunker, embedder rag.Embedder, store rag.VectorStore, logger *slog.Logger, opts ...IngesterOption) (*Ingester, error) {
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
	ing := &Ingester{chunker: chunker, embedder: embedder, store: store, logger: logger}
	for _, opt := range opts {
		opt(ing)
	}
	return ing, nil
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

	// 入库的 trace：worker 侧每个文档一条，记录耗时与块数。
	// doc_id 作为属性而不是 span 名 —— span 名要可聚合，
	// 把高基数的 ID 放进名字会让 Jaeger 里出现几十万个不同操作名。
	ctx, span := trace.Tracer().Start(ctx, "ingest.document")
	defer span.End()
	span.SetAttributes(attribute.String("ingest.doc_id", docID))

	// 1. 分块
	chunks, err := i.chunker.Chunk(ctx, docID, raw)
	if err != nil {
		return 0, fmt.Errorf("chunk doc %s: %w", docID, err)
	}
	span.SetAttributes(attribute.Int("ingest.chunks", len(chunks)))
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

	// 3.5 清理旧块（按需）。
	//
	// 放在 embedding 之后而不是最前面：向量化失败时旧版本仍然可用，
	// 不会出现「索引里什么都没有」的窗口。代价是这一步之后到 upsert
	// 完成前有几毫秒的空窗，但 Kafka 的同一 partition 有序消费保证了
	// 同一文档不会与其他写操作并发。
	if i.replace {
		if err := i.store.DeleteByDoc(ctx, docID); err != nil {
			return 0, fmt.Errorf("clear old chunks of doc %s: %w", docID, err)
		}
		if i.keyword != nil {
			if err := i.keyword.DeleteByDoc(ctx, docID); err != nil {
				return 0, fmt.Errorf("clear old chunks of doc %s from keyword store: %w", docID, err)
			}
		}
	}

	// 4. 写入向量库
	if err := i.store.Upsert(ctx, chunks, vectors); err != nil {
		return 0, fmt.Errorf("upsert %d chunks of doc %s: %w", len(chunks), docID, err)
	}

	// 5. 写入关键词索引。
	//
	// 这一步是「混合检索在异步链路里也成立」的前提：只写 Milvus 的话，
	// 上传的文档能被子句/向量召回，但永远进不了 BM25 —— 而混合检索
	// 的另一半会安静地少一路召回，指标不会报错，只会变差。
	//
	// 失败时不回滚 Milvus：Milvus 是幂等 upsert，consumer 重试会重写一遍；
	// 分布式事务在这里既昂贵又没必要。
	if i.keyword != nil {
		if err := i.keyword.Upsert(ctx, chunks); err != nil {
			return 0, fmt.Errorf("index %d chunks of doc %s into keyword store: %w",
				len(chunks), docID, err)
		}
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
	if i.keyword != nil {
		if err := i.keyword.DeleteByDoc(ctx, docID); err != nil {
			return fmt.Errorf("delete doc %s from keyword store: %w", docID, err)
		}
	}
	i.logger.Info("deleted document", "doc_id", docID)
	return nil
}
