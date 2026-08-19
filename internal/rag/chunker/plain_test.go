package chunker

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPlainChunker_BasicSplit(t *testing.T) {
	// 1000 字，size=300，overlap=50 → step=250
	// 起点 0,250,500,750 → 第 4 块 [750,1000) 触达末尾停止 → 4 块
	text := strings.Repeat("a", 1000)
	c := NewPlainChunker(300, 50)

	chunks, err := c.Chunk(context.Background(), "doc1", []byte(text))
	if err != nil {
		t.Fatalf("chunk: %v", err)
	}
	if len(chunks) != 4 {
		t.Fatalf("expected 4 chunks, got %d", len(chunks))
	}

	// 前三块应为满长度，最后一块是剩余部分
	for i, want := range []int{300, 300, 300, 250} {
		if got := len([]rune(chunks[i].Content)); got != want {
			t.Errorf("chunk[%d]: expected len %d, got %d", i, want, got)
		}
	}

	// Index 必须连续递增，Phase 2 排序和定位原文都依赖它
	for i, ch := range chunks {
		if ch.Index != i {
			t.Errorf("chunk[%d]: expected Index %d, got %d", i, i, ch.Index)
		}
		if ch.DocID != "doc1" {
			t.Errorf("chunk[%d]: expected DocID doc1, got %q", i, ch.DocID)
		}
	}
}

func TestPlainChunker_OverlapIsPreserved(t *testing.T) {
	// 用可区分的内容验证重叠区确实存在，而非只对长度
	text := ""
	for i := 0; i < 100; i++ {
		text += string(rune('A' + i%26))
	}
	c := NewPlainChunker(40, 10) // step=30

	chunks, _ := c.Chunk(context.Background(), "d", []byte(text))
	if len(chunks) < 2 {
		t.Fatalf("need at least 2 chunks to test overlap, got %d", len(chunks))
	}

	// chunk[0] 覆盖 [0,40)，chunk[1] 覆盖 [30,70)
	// 故 chunk[0] 的末 10 字符应等于 chunk[1] 的首 10 字符
	first := []rune(chunks[0].Content)
	second := []rune(chunks[1].Content)
	tail := string(first[len(first)-10:])
	head := string(second[:10])

	if tail != head {
		t.Errorf("overlap broken: chunk[0] tail %q != chunk[1] head %q", tail, head)
	}
}

func TestPlainChunker_ChineseNoMojibake(t *testing.T) {
	// 中文一个字 3 字节。若误用 []byte 切分会把字符劈开产生非法 UTF-8。
	text := strings.Repeat("缓存穿透与布隆过滤器", 50) // 500 个中文字符
	c := NewPlainChunker(37, 7)                       // 故意用非 3 倍数触发字节错位

	chunks, err := c.Chunk(context.Background(), "cn", []byte(text))
	if err != nil {
		t.Fatalf("chunk: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("no chunks produced")
	}

	for i, ch := range chunks {
		if !utf8.ValidString(ch.Content) {
			t.Errorf("chunk[%d] is not valid UTF-8 (mojibake): %q", i, ch.Content)
		}
		if strings.ContainsRune(ch.Content, utf8.RuneError) {
			t.Errorf("chunk[%d] contains replacement char", i)
		}
	}

	// 拼回去（去掉 overlap）应能还原原文，证明没有丢字符
	var sb strings.Builder
	step := 37 - 7
	for _, ch := range chunks {
		r := []rune(ch.Content)
		if len(r) > step {
			r = r[:step]
		}
		sb.WriteString(string(r))
	}
	if sb.String() != text {
		t.Errorf("reassembled text differs from original")
	}
}

func TestPlainChunker_ShortText(t *testing.T) {
	// 短于 size 的文本应产生单块且内容完整
	text := "很短的一段话"
	c := NewPlainChunker(300, 50)

	chunks, _ := c.Chunk(context.Background(), "short", []byte(text))
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Content != text {
		t.Errorf("expected %q, got %q", text, chunks[0].Content)
	}
}

func TestPlainChunker_EmptyInput(t *testing.T) {
	c := NewPlainChunker(300, 50)
	chunks, err := c.Chunk(context.Background(), "empty", []byte(""))
	if err != nil {
		t.Fatalf("empty input should not error: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for empty input, got %d", len(chunks))
	}
}

func TestPlainChunker_Idempotent(t *testing.T) {
	// 幂等性是整条管道的地基：同样输入必须产生同样的 ID，
	// 否则重复上传会在 Milvus 里堆积重复数据污染检索。
	text := strings.Repeat("测试内容", 200)
	c := NewPlainChunker(300, 50)

	first, _ := c.Chunk(context.Background(), "doc-x", []byte(text))
	second, _ := c.Chunk(context.Background(), "doc-x", []byte(text))

	if len(first) != len(second) {
		t.Fatalf("chunk count differs between runs: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Errorf("chunk[%d] ID differs: %q vs %q", i, first[i].ID, second[i].ID)
		}
	}

	// 不同 docID 必须产生不同 ID，否则跨文档会相互覆盖
	other, _ := c.Chunk(context.Background(), "doc-y", []byte(text))
	if first[0].ID == other[0].ID {
		t.Error("different docIDs produced the same chunk ID")
	}
}

func TestPlainChunker_GuardsAgainstInfiniteLoop(t *testing.T) {
	// overlap >= size 会让 step <= 0 导致死循环，构造函数必须修正
	for _, tc := range []struct{ size, overlap int }{
		{100, 100}, // 相等
		{100, 200}, // 超过
		{100, -5},  // 负数
		{0, 50},    // size 非法
	} {
		c := NewPlainChunker(tc.size, tc.overlap)
		if c.size-c.overlap <= 0 {
			t.Errorf("NewPlainChunker(%d,%d) yields non-positive step", tc.size, tc.overlap)
		}

		done := make(chan struct{})
		go func() {
			_, _ = c.Chunk(context.Background(), "d", []byte(strings.Repeat("x", 500)))
			close(done)
		}()
		select {
		case <-done:
		case <-timeoutAfterSeconds(5):
			t.Fatalf("NewPlainChunker(%d,%d) caused an infinite loop", tc.size, tc.overlap)
		}
	}
}

func TestChunkID_StableAndDistinct(t *testing.T) {
	a := ChunkID("doc", 0)
	if a != ChunkID("doc", 0) {
		t.Error("ChunkID is not deterministic")
	}
	if a == ChunkID("doc", 1) {
		t.Error("different indices produced the same ID")
	}
	if a == ChunkID("other", 0) {
		t.Error("different docIDs produced the same ID")
	}
	if len(a) != 32 {
		t.Errorf("expected 32-char ID, got %d", len(a))
	}
}
