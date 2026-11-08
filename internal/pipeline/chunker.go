package pipeline

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/yurin-kami/KahoAgent/internal/rag"
	"github.com/yurin-kami/KahoAgent/internal/rag/chunker"
)

// ExtChunker 按文档扩展名选择分块策略。
//
// 为什么需要它：上传接口收到的文档类型不止一种，而 Markdown 的
// 「按标题层级切分」策略对纯文本完全不适用 —— 没有标题时它会退化成
// 一整块，或者切出大量空块。PlainChunker 的固定长度切分反而是更稳的兜底。
//
// 为什么不用把类型塞进 IngestTask：docID 本身已经带了扩展名
// （上传接口用文件名做 docID，见 internal/handler/upload.go），
// 再增加一个字段就要维护「docID 与 type 不一致」的新一类 bug。
// 扩展名是 docID 的一部分，两者天然同步。
type ExtChunker struct {
	markdown rag.Chunker
	plain    rag.Chunker
}

// 编译期断言：确保实现了 rag.Chunker 接口。
var _ rag.Chunker = (*ExtChunker)(nil)

// NewExtChunker 组装按扩展名分发的分块器。
//
// 两个参数都允许为 nil，缺省时自动补 PlainChunker —— 分块器在启动时
// 构造，为了一个可选依赖让整个 worker 起不来不划算。
func NewExtChunker(markdown, plain rag.Chunker) *ExtChunker {
	if markdown == nil {
		markdown = chunker.NewMarkdownChunker(0)
	}
	if plain == nil {
		plain = chunker.NewPlainChunker(0, 0)
	}
	return &ExtChunker{markdown: markdown, plain: plain}
}

func (c *ExtChunker) Name() string { return "ext" }

// Chunk 按扩展名分发。
//
// 只认 .md / .markdown 两种：其他所有类型走 PlainChunker。
// 这是有意的保守选择 —— 把 .txt 误判成 Markdown，最坏结果是按段落切，
// 损失可控；反过来把 Markdown 误判成纯文本，标题层级信息就永久丢了，
// 而 heading_path 是检索结果能给出「出处」的基础。
func (c *ExtChunker) Chunk(ctx context.Context, docID string, raw []byte) ([]rag.Chunk, error) {
	if isMarkdownDoc(docID) {
		return c.markdown.Chunk(ctx, docID, raw)
	}
	return c.plain.Chunk(ctx, docID, raw)
}

// isMarkdownDoc 判断 docID 是否指向 Markdown 文档。
//
// 用 filepath.Ext 而不是 strings.HasSuffix：前者对 "a.md/../b" 这类
// 畸形输入也能取到正确的最后一段扩展名，不会因为中间出现 ".md" 误判。
// 大小写不敏感，Windows 上上传 "README.MD" 是常见操作。
func isMarkdownDoc(docID string) bool {
	switch strings.ToLower(filepath.Ext(docID)) {
	case ".md", ".markdown":
		return true
	default:
		return false
	}
}
