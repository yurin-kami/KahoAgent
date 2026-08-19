// Package rag 定义了检索增强生成（RAG）链路的核心抽象。
//
// 本文件是 Phase 1-2 的接口契约，定稿后不应修改。
// 若确需变更，必须在 commit message 中说明原因与影响范围。
package rag

import "context"

// Chunk 是分块后的最小检索单元。
type Chunk struct {
	// ID 是稳定标识：sha256(DocID + Index)，保证重复入库幂等。
	ID string

	// DocID 标识来源文档，用于按文档批量删除。
	DocID string

	// Index 是该块在文档中的序号，从 0 开始。
	Index int

	// Content 是块的文本内容。
	Content string

	// Metadata 存储来源路径、标题层级、文件类型等附加信息。
	Metadata map[string]string
}

// ScoredChunk 是带相关性分数的检索结果。
type ScoredChunk struct {
	Chunk Chunk
	Score float32
}

// Chunker 将原始文档切分为 Chunk。
//
// 实现需保证：相同输入产生相同的 Chunk.ID，以支持幂等入库。
type Chunker interface {
	// Name 返回分块器名称，用于评估对比与日志。
	Name() string

	// Chunk 将 raw 切分为若干 Chunk。
	Chunk(ctx context.Context, docID string, raw []byte) ([]Chunk, error)
}

// Embedder 将文本转换为向量。
//
// 注意：eino 的 components/embedding.Embedder 返回 [][]float64，
// 而 Milvus SDK 需要 []float32。本接口统一使用 float32，
// 由 pkg/llm 中的适配层负责转换。
type Embedder interface {
	// Embed 批量向量化。返回的切片顺序与 texts 一致。
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Dim 返回向量维度，用于建表校验。
	Dim() int
}

// VectorStore 是向量存储的抽象。
type VectorStore interface {
	// Upsert 写入或覆盖 chunks。vectors 的顺序与 chunks 一致，长度必须相等。
	Upsert(ctx context.Context, chunks []Chunk, vectors [][]float32) error

	// Search 返回与 vector 最相似的 topK 个 Chunk，按分数降序。
	Search(ctx context.Context, vector []float32, topK int) ([]ScoredChunk, error)

	// DeleteByDoc 删除某文档的全部 Chunk。
	DeleteByDoc(ctx context.Context, docID string) error
}
