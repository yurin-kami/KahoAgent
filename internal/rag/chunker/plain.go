// Package chunker 提供文档分块实现。
//
// 分块是 RAG 效果的第一决定因素：切得太碎会丢失上下文，
// 切得太大会混入无关信息稀释语义。本包提供两种策略：
//
//   - PlainChunker：固定长度 + overlap，作为评估基线
//   - MarkdownChunker：按标题层级切分，保留语义边界（Phase 1 步骤 2）
package chunker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

// 默认参数。size 取 500 是中文技术文档的经验值：
// 约等于 2-3 个自然段，能容纳一个完整的小知识点。
const (
	DefaultSize    = 500
	DefaultOverlap = 80
)

// PlainChunker 按固定字符数切分，相邻块保留 overlap 重叠。
//
// 存在意义是作为 Phase 2 评估的对照组。我们需要用数据证明
// 「结构化分块优于固定分块」，没有基线就没有对比，
// 简历上的「提升 X%」就是空话。
//
// overlap 的作用：若边界正好切在「布隆过滤器可以解决缓存穿透」中间，
// 前块结尾是「布隆过滤器可以」，后块开头是「解决缓存穿透」，
// 两块单独看都不完整。overlap 让后块包含前块结尾，保证语义连续。
type PlainChunker struct {
	size    int
	overlap int
}

// 编译期断言：确保实现了 rag.Chunker 接口。
var _ rag.Chunker = (*PlainChunker)(nil)

// NewPlainChunker 创建固定长度分块器。
//
// 非法参数会被修正为合理值而非报错，因为分块器通常在启动时构造，
// 这里 panic 或返回 error 会让调用方代码变得啰嗦。
func NewPlainChunker(size, overlap int) *PlainChunker {
	if size <= 0 {
		size = DefaultSize
	}
	// overlap >= size 会导致 step <= 0，进而死循环。
	if overlap < 0 || overlap >= size {
		overlap = size / 6
	}
	return &PlainChunker{size: size, overlap: overlap}
}

func (c *PlainChunker) Name() string { return "plain" }

func (c *PlainChunker) Chunk(_ context.Context, docID string, raw []byte) ([]rag.Chunk, error) {
	// ⚠️ 用 []rune 而非 []byte 切分。中文一个字占 3 字节，
	//    按 byte 切会把字符劈开产生乱码 —— 这是中文场景最容易踩的坑。
	runes := []rune(string(raw))
	if len(runes) == 0 {
		return nil, nil
	}

	step := c.size - c.overlap
	// 预估容量，避免 append 反复扩容。
	chunks := make([]rag.Chunk, 0, (len(runes)+step-1)/step)

	for start, idx := 0, 0; start < len(runes); start, idx = start+step, idx+1 {
		end := min(start+c.size, len(runes))

		chunks = append(chunks, rag.Chunk{
			ID:      ChunkID(docID, idx),
			DocID:   docID,
			Index:   idx,
			Content: string(runes[start:end]),
			Metadata: map[string]string{
				"chunker": c.Name(),
			},
		})

		// 已覆盖到末尾就停，否则最后会多切出一个纯 overlap 的冗余块。
		if end == len(runes) {
			break
		}
	}
	return chunks, nil
}

// ChunkID 生成稳定的块标识。
//
// 相同的 (docID, index) 永远产生相同 ID，配合 Milvus 的 Upsert
// 使得重复入库覆盖而非新增 —— 这就是整条管道幂等性的实现基础。
// 不能用自增 ID 或 UUID：那样每次上传都是新记录，
// 重复数据会污染检索结果。
//
// 取 sha256 前 32 个十六进制字符（128 bit），碰撞概率可忽略，
// 同时把 Milvus 主键字段从 64 字符压到 32 字符。
func ChunkID(docID string, index int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", docID, index)))
	return hex.EncodeToString(h[:])[:32]
}
