package pipeline

import (
	"context"
	"testing"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

// namedChunker 记录自己被调用时收到的 docID，用来断言分球分发。
type namedChunker struct {
	name string
	got  []string
}

func (c *namedChunker) Name() string { return c.name }

func (c *namedChunker) Chunk(_ context.Context, docID string, _ []byte) ([]rag.Chunk, error) {
	c.got = append(c.got, docID)
	return []rag.Chunk{{ID: "x", DocID: docID, Content: "x"}}, nil
}

// TestExtChunker_DispatchesByExtension 锁住「按扩展名选分块器」的规则。
//
// 为什么这条规则重要：Markdown 分块器要读标题层级来生成 heading_path，
// 纯文本没有标题，硬用它会退化成一整块或大量空块。反过来把 Markdown
// 当纯文本切，标题信息永久丢失，检索结果就无法给出「来自哪一节」。
func TestExtChunker_DispatchesByExtension(t *testing.T) {
	tests := []struct {
		docID string
		want  string
	}{
		{"notes.md", "markdown"},
		{"README.MD", "markdown"},  // 大小写不敏感
		{"a.markdown", "markdown"}, // 官方长扩展名
		{"a.txt", "plain"},         // 纯文本走兜底
		{"noext", "plain"},         // 没有扩展名
		{"a.md.txt", "plain"},      // 取最后一段扩展名，不能被中间 .md 骗到
		{"docs/a.md", "markdown"},  // 带目录时只看 basename 的扩展名
	}

	for _, tc := range tests {
		t.Run(tc.docID, func(t *testing.T) {
			md := &namedChunker{name: "markdown"}
			plain := &namedChunker{name: "plain"}
			c := NewExtChunker(md, plain)

			if _, err := c.Chunk(context.Background(), tc.docID, []byte("正文")); err != nil {
				t.Fatalf("chunk: %v", err)
			}

			var hit *namedChunker
			if len(md.got) > 0 {
				hit = md
			} else if len(plain.got) > 0 {
				hit = plain
			}
			if hit == nil {
				t.Fatal("neither chunker was called")
			}
			if hit.name != tc.want {
				t.Fatalf("docID %q dispatched to %q, want %q", tc.docID, hit.name, tc.want)
			}
		})
	}
}

// TestNewExtChunker_FillsDefaults 验证 nil 依赖会被补成默认实现。
//
// 构造期兜底而不是 panic：分块器在 worker 启动时创建，
// 为了一个可选依赖让整个消费进程起不来，代价远大于收益。
func TestNewExtChunker_FillsDefaults(t *testing.T) {
	c := NewExtChunker(nil, nil)
	if c.markdown == nil || c.plain == nil {
		t.Fatal("defaults should be filled when nil is passed")
	}
	if _, err := c.Chunk(context.Background(), "a.md", []byte("# 标题\n正文")); err != nil {
		t.Fatalf("chunk with defaults: %v", err)
	}
}
