package retriever

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yurin-kami/KahoAgent/internal/rag"
	"github.com/yurin-kami/KahoAgent/internal/rag/tokenizer"
)

// BM25Retriever 走 PostgreSQL 全文检索做稀疏召回。
//
// 擅长关键词精确匹配：专有名词、代码符号、版本号。
// 查「RedisJSON」能精准命中含该词的文档，而向量检索会把它
// 稀释成「Redis + JSON」的语义，返回一堆无关内容。
//
// 不擅长同义词：查「登录」匹配不到只写了「认证」的文档。
// 这正是需要向量检索补位的原因 —— 两者互补，所以要混合。
//
// PG 的 ts_rank 用的是类 BM25 的算法：词频有饱和上限、
// 罕见词权重更高、长文档会被惩罚。
type BM25Retriever struct {
	pool *pgxpool.Pool
	tok  *tokenizer.Jieba
}

func NewBM25Retriever(pool *pgxpool.Pool) (*BM25Retriever, error) {
	if pool == nil {
		return nil, fmt.Errorf("pool must not be nil")
	}
	return &BM25Retriever{pool: pool, tok: tokenizer.Shared()}, nil
}

func (r *BM25Retriever) Name() string { return "bm25" }

// Recall 返回 topN 个候选，按 ts_rank 降序。
func (r *BM25Retriever) Recall(ctx context.Context, query string, topN int) ([]rag.ScoredChunk, error) {
	if query == "" {
		return nil, fmt.Errorf("query must not be empty")
	}
	if topN <= 0 {
		return nil, fmt.Errorf("topN must be > 0, got %d", topN)
	}

	tsquery := r.tok.TokenizeQuery(query)
	if tsquery == "" {
		// 查询里全是停用词或空白，没有可检索的词元。
		// 返回空而非报错 —— 这是合法输入，只是无从检索。
		return nil, nil
	}

	// 用参数化查询而非拼接字符串：tsquery 内容来自用户输入，
	// 拼接会有注入风险。to_tsquery 解析失败时 PG 会报错，
	// 由下面的错误处理兜住。
	const q = `
		SELECT id, doc_id, chunk_index, content, metadata,
		       ts_rank(content_tsv, query) AS score
		FROM chunks, to_tsquery('simple', $1) query
		WHERE content_tsv @@ query
		ORDER BY score DESC
		LIMIT $2`

	rows, err := r.pool.Query(ctx, q, tsquery, topN)
	if err != nil {
		return nil, fmt.Errorf("bm25 query: %w", err)
	}
	defer rows.Close()

	var out []rag.ScoredChunk
	for rows.Next() {
		var (
			c       rag.Chunk
			score   float32
			rawMeta []byte
		)
		if err := rows.Scan(&c.ID, &c.DocID, &c.Index, &c.Content, &rawMeta, &score); err != nil {
			return nil, fmt.Errorf("scan bm25 row: %w", err)
		}

		c.Metadata = map[string]string{}
		if len(rawMeta) > 0 {
			// metadata 损坏不应让整次检索失败 —— 内容才是主体
			_ = json.Unmarshal(rawMeta, &c.Metadata)
		}
		out = append(out, rag.ScoredChunk{Chunk: c, Score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate bm25 rows: %w", err)
	}
	return out, nil
}

// ChunkWriter 把 chunk 写入 PG 供 BM25 检索。
//
// 独立于 Recall 是因为写入发生在入库管道里，而检索发生在查询路径 ——
// 两者的调用方完全不同。
type ChunkWriter struct {
	pool *pgxpool.Pool
	tok  *tokenizer.Jieba
}

func NewChunkWriter(pool *pgxpool.Pool) (*ChunkWriter, error) {
	if pool == nil {
		return nil, fmt.Errorf("pool must not be nil")
	}
	return &ChunkWriter{pool: pool, tok: tokenizer.Shared()}, nil
}

// Upsert 写入或覆盖 chunks。
//
// 与 Milvus 侧共用同一套 ChunkID，因此两个存储天然对齐，
// RRF 才能按 ID 融合两路结果。
func (w *ChunkWriter) Upsert(ctx context.Context, chunks []rag.Chunk) error {
	if len(chunks) == 0 {
		return nil
	}

	// 用 Batch 而非逐条 Exec：一次往返写完整批，
	// 100 个 chunk 从 100 次网络往返降到 1 次。
	batch := &pgx.Batch{}
	const q = `
		INSERT INTO chunks (id, doc_id, chunk_index, content, tokens, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
			doc_id      = EXCLUDED.doc_id,
			chunk_index = EXCLUDED.chunk_index,
			content     = EXCLUDED.content,
			tokens      = EXCLUDED.tokens,
			metadata    = EXCLUDED.metadata`

	for _, c := range chunks {
		meta, err := json.Marshal(c.Metadata)
		if err != nil {
			return fmt.Errorf("chunk %s: marshal metadata: %w", c.ID, err)
		}
		// 分词在写入时做一次，检索时直接用 —— 避免每次查询都重新分词
		batch.Queue(q, c.ID, c.DocID, c.Index, c.Content, w.tok.Tokenize(c.Content), meta)
	}

	br := w.pool.SendBatch(ctx, batch)
	defer br.Close()

	for i := range chunks {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("upsert chunk %s: %w", chunks[i].ID, err)
		}
	}
	return nil
}

// DeleteByDoc 删除某文档的全部 chunk。
func (w *ChunkWriter) DeleteByDoc(ctx context.Context, docID string) error {
	if docID == "" {
		return fmt.Errorf("docID must not be empty")
	}
	if _, err := w.pool.Exec(ctx, `DELETE FROM chunks WHERE doc_id = $1`, docID); err != nil {
		return fmt.Errorf("delete doc %s: %w", docID, err)
	}
	return nil
}
