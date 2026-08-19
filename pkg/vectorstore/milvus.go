// Package vectorstore 提供向量存储的具体实现。
package vectorstore

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

// Schema 字段名。定义为常量避免散落的字符串字面量拼错。
const (
	FieldID         = "id"
	FieldDocID      = "doc_id"
	FieldChunkIndex = "chunk_index"
	FieldContent    = "content"
	FieldMetadata   = "metadata"
	FieldVector     = "vector"
)

// 索引与检索参数。HNSW 的 M / efConstruction 采用社区推荐值；
// 改动它们需要重建索引并重跑全量数据，因此固定在此。
const (
	hnswM              = 16
	hnswEfConstruction = 200
	hnswSearchEf       = 64 // 必须 >= topK

	// ⚠️ VarChar 的 max_length 单位是「字节」而非「字符」。
	// 一个汉字占 3 字节，按字符数估算上限会导致中文内容被服务端拒绝。
	// 常量名带 Bytes 后缀就是为了让这一点无法被忽略。
	maxIDBytes = 64

	// 32KB 对 chunk 而言极为宽裕（默认 500 字符 ≈ 1.5KB），
	// 留出的余量足以覆盖调大 chunk size 的情况，实际几乎不会触发截断。
	maxContentBytes = 32768
)

// MilvusStore 基于 Milvus 实现向量存储。
type MilvusStore struct {
	cli      client.Client
	collName string
	dim      int
}

// 编译期断言：接口没实现全会在此处报错，而不是等到运行时。
var _ rag.VectorStore = (*MilvusStore)(nil)

// NewMilvusStore 连接 Milvus 并返回存储实例。
// 调用方需负责 Close。
func NewMilvusStore(ctx context.Context, addr, collName string, dim int) (*MilvusStore, error) {
	if dim <= 0 {
		return nil, fmt.Errorf("vector dim must be > 0, got %d", dim)
	}
	cli, err := client.NewClient(ctx, client.Config{Address: addr})
	if err != nil {
		return nil, fmt.Errorf("connect milvus at %s: %w", addr, err)
	}
	return &MilvusStore{cli: cli, collName: collName, dim: dim}, nil
}

func (s *MilvusStore) Close() error { return s.cli.Close() }

// EnsureCollection 幂等地建表、建索引并加载。
//
// 设计为可重复调用，服务启动时无脑执行即可，不需要单独的初始化脚本。
func (s *MilvusStore) EnsureCollection(ctx context.Context) error {
	has, err := s.cli.HasCollection(ctx, s.collName)
	if err != nil {
		return fmt.Errorf("check collection: %w", err)
	}

	if !has {
		schema := entity.NewSchema().
			WithName(s.collName).
			WithDescription("KahoAgent document chunks").
			WithField(entity.NewField().
				WithName(FieldID).
				WithDataType(entity.FieldTypeVarChar).
				WithIsPrimaryKey(true).
				WithMaxLength(maxIDBytes)).
			WithField(entity.NewField().
				WithName(FieldDocID).
				WithDataType(entity.FieldTypeVarChar).
				WithMaxLength(maxIDBytes)).
			WithField(entity.NewField().
				WithName(FieldChunkIndex).
				WithDataType(entity.FieldTypeInt64)).
			WithField(entity.NewField().
				WithName(FieldContent).
				WithDataType(entity.FieldTypeVarChar).
				WithMaxLength(maxContentBytes)).
			WithField(entity.NewField().
				WithName(FieldMetadata).
				WithDataType(entity.FieldTypeJSON)).
			WithField(entity.NewField().
				WithName(FieldVector).
				WithDataType(entity.FieldTypeFloatVector).
				WithDim(int64(s.dim)))

		if err := s.cli.CreateCollection(ctx, schema, 1); err != nil {
			return fmt.Errorf("create collection: %w", err)
		}
	}

	// 索引可能因上次中断而缺失，故独立判断而非跟随 has。
	indexes, err := s.cli.DescribeIndex(ctx, s.collName, FieldVector)
	if err != nil || len(indexes) == 0 {
		// COSINE：文本向量的方向才携带语义，长度无意义。
		idx, err := entity.NewIndexHNSW(entity.COSINE, hnswM, hnswEfConstruction)
		if err != nil {
			return fmt.Errorf("build index config: %w", err)
		}
		if err := s.cli.CreateIndex(ctx, s.collName, FieldVector, idx, false); err != nil {
			return fmt.Errorf("create index: %w", err)
		}
	}

	// ⚠️ 不 Load 就搜索会直接报错。重复 Load 是安全的。
	if err := s.cli.LoadCollection(ctx, s.collName, false); err != nil {
		return fmt.Errorf("load collection: %w", err)
	}
	return nil
}

// Upsert 写入或覆盖 chunks。vectors 顺序须与 chunks 一致。
func (s *MilvusStore) Upsert(ctx context.Context, chunks []rag.Chunk, vectors [][]float32) error {
	if len(chunks) == 0 {
		return nil
	}
	// 前置校验：数量不等说明上游有 bug。放任写入会产生错位数据，
	// 而错位数据在检索阶段表现为「结果看起来对但内容不对」，极难排查。
	if len(chunks) != len(vectors) {
		return fmt.Errorf("chunk/vector count mismatch: %d chunks, %d vectors",
			len(chunks), len(vectors))
	}

	// Milvus 是列式写入：需要把 []Chunk 转置成一组列。
	n := len(chunks)
	ids := make([]string, n)
	docIDs := make([]string, n)
	indices := make([]int64, n)
	contents := make([]string, n)
	metas := make([][]byte, n)

	for i, ch := range chunks {
		if len(vectors[i]) != s.dim {
			return fmt.Errorf("chunk %s: vector dim %d, expected %d",
				ch.ID, len(vectors[i]), s.dim)
		}
		// ID 只校验不截断。内容超长截断是可接受的降级，但 ID 截断会让
		// 两个不同 chunk 撞成同一主键，静默丢数据 —— 必须直接报错。
		if len(ch.ID) > maxIDBytes {
			return fmt.Errorf("chunk ID %q is %d bytes, exceeds limit %d",
				ch.ID, len(ch.ID), maxIDBytes)
		}
		if len(ch.DocID) > maxIDBytes {
			return fmt.Errorf("chunk %s: doc_id %q is %d bytes, exceeds limit %d",
				ch.ID, ch.DocID, len(ch.DocID), maxIDBytes)
		}
		meta, err := json.Marshal(ch.Metadata)
		if err != nil {
			return fmt.Errorf("chunk %s: marshal metadata: %w", ch.ID, err)
		}

		ids[i] = ch.ID
		docIDs[i] = ch.DocID
		indices[i] = int64(ch.Index)
		contents[i] = truncateBytes(ch.Content, maxContentBytes)
		metas[i] = meta
	}

	// 用 Upsert 而非 Insert —— 配合稳定的 ChunkID，
	// 重复入库会覆盖而非堆积，这是管道幂等性的另一半。
	if _, err := s.cli.Upsert(ctx, s.collName, "",
		entity.NewColumnVarChar(FieldID, ids),
		entity.NewColumnVarChar(FieldDocID, docIDs),
		entity.NewColumnInt64(FieldChunkIndex, indices),
		entity.NewColumnVarChar(FieldContent, contents),
		entity.NewColumnJSONBytes(FieldMetadata, metas),
		entity.NewColumnFloatVector(FieldVector, s.dim, vectors),
	); err != nil {
		return fmt.Errorf("upsert %d chunks: %w", n, err)
	}
	return nil
}

// Flush 将内存 buffer 落盘使数据可检索。
//
// 单独暴露而非在 Upsert 内部调用：批量入库时每批都 Flush 会严重拖慢速度，
// 由调用方在全部写完后统一 Flush 一次。
func (s *MilvusStore) Flush(ctx context.Context) error {
	if err := s.cli.Flush(ctx, s.collName, false); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	return nil
}

// Search 返回与 vector 最相似的 topK 个 Chunk，按相似度降序。
func (s *MilvusStore) Search(ctx context.Context, vector []float32, topK int) ([]rag.ScoredChunk, error) {
	if len(vector) != s.dim {
		return nil, fmt.Errorf("query vector dim %d, expected %d", len(vector), s.dim)
	}
	if topK <= 0 {
		return nil, fmt.Errorf("topK must be > 0, got %d", topK)
	}

	ef := max(hnswSearchEf, topK) // ef 小于 topK 会被 Milvus 拒绝
	sp, err := entity.NewIndexHNSWSearchParam(ef)
	if err != nil {
		return nil, fmt.Errorf("build search param: %w", err)
	}

	results, err := s.cli.Search(ctx, s.collName,
		nil, // partitions
		"",  // expr：标量过滤，Phase 2 的混合检索会用到
		[]string{FieldID, FieldDocID, FieldChunkIndex, FieldContent, FieldMetadata},
		[]entity.Vector{entity.FloatVector(vector)},
		FieldVector,
		entity.COSINE, // 必须与建索引时一致
		topK,
		sp,
	)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	if len(results) == 0 {
		return nil, nil
	}

	return parseSearchResult(results[0])
}

func parseSearchResult(r client.SearchResult) ([]rag.ScoredChunk, error) {
	// ⚠️ 必须先判空再判错，顺序不能反。
	// 命中为 0 时服务端不返回任何 fieldsData，SDK 却仍期待 output_fields
	// 对应的列存在，于是伪造出一个 "extra output fields ... found" 错误。
	// 空集合、过滤后无结果都会走到这里，属于正常情况而非故障。
	if r.ResultCount == 0 {
		return nil, nil
	}
	if r.Err != nil {
		return nil, fmt.Errorf("search result: %w", r.Err)
	}

	// 用 comma-ok 断言：类型不符时返回错误而非 panic。
	idCol, ok := r.Fields.GetColumn(FieldID).(*entity.ColumnVarChar)
	if !ok {
		return nil, fmt.Errorf("unexpected column type for %s", FieldID)
	}
	docCol, ok := r.Fields.GetColumn(FieldDocID).(*entity.ColumnVarChar)
	if !ok {
		return nil, fmt.Errorf("unexpected column type for %s", FieldDocID)
	}
	idxCol, ok := r.Fields.GetColumn(FieldChunkIndex).(*entity.ColumnInt64)
	if !ok {
		return nil, fmt.Errorf("unexpected column type for %s", FieldChunkIndex)
	}
	contentCol, ok := r.Fields.GetColumn(FieldContent).(*entity.ColumnVarChar)
	if !ok {
		return nil, fmt.Errorf("unexpected column type for %s", FieldContent)
	}
	metaCol, ok := r.Fields.GetColumn(FieldMetadata).(*entity.ColumnJSONBytes)
	if !ok {
		return nil, fmt.Errorf("unexpected column type for %s", FieldMetadata)
	}

	out := make([]rag.ScoredChunk, 0, r.ResultCount)
	for i := range r.ResultCount {
		id, err := idCol.ValueByIdx(i)
		if err != nil {
			return nil, fmt.Errorf("read id at %d: %w", i, err)
		}
		docID, err := docCol.ValueByIdx(i)
		if err != nil {
			return nil, fmt.Errorf("read doc_id at %d: %w", i, err)
		}
		chunkIdx, err := idxCol.ValueByIdx(i)
		if err != nil {
			return nil, fmt.Errorf("read chunk_index at %d: %w", i, err)
		}
		content, err := contentCol.ValueByIdx(i)
		if err != nil {
			return nil, fmt.Errorf("read content at %d: %w", i, err)
		}

		meta := map[string]string{}
		if rawMeta, err := metaCol.ValueByIdx(i); err == nil && len(rawMeta) > 0 {
			// metadata 损坏不应让整次检索失败 —— 内容才是主体，
			// 丢失 heading_path 只会降级溯源能力。
			_ = json.Unmarshal(rawMeta, &meta)
		}

		out = append(out, rag.ScoredChunk{
			Chunk: rag.Chunk{
				ID:       id,
				DocID:    docID,
				Index:    int(chunkIdx),
				Content:  content,
				Metadata: meta,
			},
			Score: r.Scores[i],
		})
	}
	return out, nil
}

// DeleteByDoc 删除某文档的全部 Chunk。
func (s *MilvusStore) DeleteByDoc(ctx context.Context, docID string) error {
	if docID == "" {
		return fmt.Errorf("docID must not be empty")
	}
	// 表达式中的字符串需转义，否则含引号的 docID 会破坏语法。
	expr := fmt.Sprintf("%s == %q", FieldDocID, docID)
	if err := s.cli.Delete(ctx, s.collName, "", expr); err != nil {
		return fmt.Errorf("delete doc %s: %w", docID, err)
	}
	return nil
}

// DropCollection 删除整个 collection，仅供测试清理使用。
func (s *MilvusStore) DropCollection(ctx context.Context) error {
	return s.cli.DropCollection(ctx, s.collName)
}

// truncateBytes 将 s 截断到至多 maxBytes 字节，且不切坏多字节字符。
//
// Milvus 的 VarChar max_length 以字节计，而 Go 的 len(string) 恰好也是字节数，
// 两者单位一致；但直接按字节切片会把一个汉字劈成两半产生非法 UTF-8，
// 因此要回退到最近的字符边界。
func truncateBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// 从 maxBytes 处向前回退，直到落在字符起始字节上。
	// UTF-8 续接字节形如 10xxxxxx，最多回退 3 次。
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
