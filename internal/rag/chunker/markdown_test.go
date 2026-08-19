package chunker

import (
	"context"
	"strings"
	"testing"
)

func chunkMD(t *testing.T, md string, maxSize int) []struct {
	Path    string
	Content string
} {
	t.Helper()
	c := NewMarkdownChunker(maxSize)
	chunks, err := c.Chunk(context.Background(), "doc", []byte(md))
	if err != nil {
		t.Fatalf("chunk: %v", err)
	}
	out := make([]struct {
		Path    string
		Content string
	}, len(chunks))
	for i, ch := range chunks {
		out[i].Path = ch.Metadata[MetaHeadingPath]
		out[i].Content = ch.Content
	}
	return out
}

func TestMarkdownChunker_FlatHeadings(t *testing.T) {
	md := `# 缓存

缓存的正文内容。

# 消息队列

消息队列的正文内容。`

	got := chunkMD(t, md, 500)
	if len(got) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %+v", len(got), got)
	}
	if got[0].Path != "缓存" {
		t.Errorf("chunk[0] path: expected %q, got %q", "缓存", got[0].Path)
	}
	if got[1].Path != "消息队列" {
		t.Errorf("chunk[1] path: expected %q, got %q", "消息队列", got[1].Path)
	}
	if !strings.Contains(got[0].Content, "缓存的正文") {
		t.Errorf("chunk[0] lost its body: %q", got[0].Content)
	}
}

func TestMarkdownChunker_NestedHeadings(t *testing.T) {
	md := `# Redis

## 主从复制

主从复制的内容。

### 全量同步

全量同步的细节。`

	got := chunkMD(t, md, 500)
	if len(got) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %+v", len(got), got)
	}

	// 「# Redis」下无正文，不产生块；路径应逐层累积
	if want := "Redis > 主从复制"; got[0].Path != want {
		t.Errorf("chunk[0] path: expected %q, got %q", want, got[0].Path)
	}
	if want := "Redis > 主从复制 > 全量同步"; got[1].Path != want {
		t.Errorf("chunk[1] path: expected %q, got %q", want, got[1].Path)
	}
}

func TestMarkdownChunker_LevelRollback(t *testing.T) {
	// 从 ## 回到 # 时，路径必须重置而非继续累积
	md := `# A

## A1

A1 的内容。

# B

B 的内容。`

	got := chunkMD(t, md, 500)
	if len(got) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %+v", len(got), got)
	}
	if want := "A > A1"; got[0].Path != want {
		t.Errorf("chunk[0] path: expected %q, got %q", want, got[0].Path)
	}
	// 关键断言：B 的路径是 "B"，不是 "A > A1 > B"
	if got[1].Path != "B" {
		t.Errorf("level rollback failed: expected %q, got %q", "B", got[1].Path)
	}
}

func TestMarkdownChunker_IgnoresHashInsideCodeFence(t *testing.T) {
	// 代码块里的 # 是注释，不能当标题，否则会把一段代码切得粉碎
	md := "# 配置说明\n" +
		"\n" +
		"下面是配置示例：\n" +
		"\n" +
		"```bash\n" +
		"# 这是注释不是标题\n" +
		"export KEY=value\n" +
		"## 这也是注释\n" +
		"```\n" +
		"\n" +
		"配置完成后重启服务。"

	got := chunkMD(t, md, 2000)
	if len(got) != 1 {
		t.Fatalf("code fence was split into %d chunks, expected 1: %+v", len(got), got)
	}
	if got[0].Path != "配置说明" {
		t.Errorf("path: expected %q, got %q", "配置说明", got[0].Path)
	}
	for _, want := range []string{"这是注释不是标题", "export KEY=value", "配置完成后重启服务"} {
		if !strings.Contains(got[0].Content, want) {
			t.Errorf("content missing %q:\n%s", want, got[0].Content)
		}
	}
}

func TestMarkdownChunker_TildeFenceNotClosedByBacktick(t *testing.T) {
	// ~~~ 围栏不能被 ``` 关闭，否则围栏状态会错乱
	md := "# T\n~~~\n# inside tilde\n```\n# still inside\n~~~\n\n正文。"

	got := chunkMD(t, md, 2000)
	if len(got) != 1 {
		t.Fatalf("expected 1 chunk, got %d: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Content, "# inside tilde") {
		t.Errorf("tilde fence content was treated as heading:\n%s", got[0].Content)
	}
}

func TestMarkdownChunker_OversizedSectionFallsBack(t *testing.T) {
	// 单节内容超过 maxSize 时二次切分，但所有子块必须保留同一 heading_path
	body := strings.Repeat("很长的内容", 300) // 1500 字符
	md := "# 长文\n\n" + body

	got := chunkMD(t, md, 400)
	if len(got) < 2 {
		t.Fatalf("expected fallback to produce multiple chunks, got %d", len(got))
	}
	for i, g := range got {
		if g.Path != "长文" {
			t.Errorf("chunk[%d]: heading_path lost during fallback, got %q", i, g.Path)
		}
	}
}

func TestMarkdownChunker_GlobalIndexStaysSequential(t *testing.T) {
	// 触发二次切分后，Index 仍必须全局连续 —— 否则 ChunkID 会碰撞
	md := "# A\n\n" + strings.Repeat("x", 900) + "\n\n# B\n\n短内容。"

	c := NewMarkdownChunker(300)
	chunks, err := c.Chunk(context.Background(), "doc", []byte(md))
	if err != nil {
		t.Fatalf("chunk: %v", err)
	}
	if len(chunks) < 3 {
		t.Fatalf("expected fallback split, got %d chunks", len(chunks))
	}

	seen := make(map[string]bool, len(chunks))
	for i, ch := range chunks {
		if ch.Index != i {
			t.Errorf("chunk[%d]: Index is %d, expected %d", i, ch.Index, i)
		}
		if seen[ch.ID] {
			t.Errorf("duplicate chunk ID %q at index %d", ch.ID, i)
		}
		seen[ch.ID] = true
	}
}

func TestMarkdownChunker_SkipsEmptySections(t *testing.T) {
	// 只有标题没有正文的节不该入库 —— 存进去也检索不出有用内容
	md := `# 只有标题

## 也只有标题

# 有正文的

这里有内容。`

	got := chunkMD(t, md, 500)
	if len(got) != 1 {
		t.Fatalf("expected 1 chunk, got %d: %+v", len(got), got)
	}
	if got[0].Path != "有正文的" {
		t.Errorf("path: expected %q, got %q", "有正文的", got[0].Path)
	}
}

func TestMarkdownChunker_ContentBeforeFirstHeading(t *testing.T) {
	// 首个标题之前的前言应保留，路径为空字符串
	md := `这是前言部分。

# 第一章

第一章内容。`

	got := chunkMD(t, md, 500)
	if len(got) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %+v", len(got), got)
	}
	if got[0].Path != "" {
		t.Errorf("preamble path: expected empty, got %q", got[0].Path)
	}
	if !strings.Contains(got[0].Content, "前言") {
		t.Errorf("preamble lost: %q", got[0].Content)
	}
}

func TestMarkdownChunker_Idempotent(t *testing.T) {
	md := "# A\n\n内容一。\n\n## A1\n\n内容二。"
	c := NewMarkdownChunker(500)

	first, _ := c.Chunk(context.Background(), "doc", []byte(md))
	second, _ := c.Chunk(context.Background(), "doc", []byte(md))

	if len(first) != len(second) {
		t.Fatalf("chunk count differs: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Errorf("chunk[%d] ID differs across runs", i)
		}
	}
}
