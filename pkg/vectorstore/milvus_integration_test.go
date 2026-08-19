//go:build integration

// Milvus 集成测试。需要真实的 Milvus 实例：
//
//	podman-compose up -d
//	go test -tags=integration ./pkg/vectorstore/ -v
//
// 单测用 mock 无法覆盖 Flush/Load 时序这类问题 —— 那正是最容易踩的坑，
// 所以这里坚持连真实服务。
package vectorstore

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

const testDim = 8

func milvusAddr() string {
	if v := os.Getenv("MILVUS_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:19530"
}

// newTestStore 创建一个测试专用 collection，测试结束自动清理。
func newTestStore(t *testing.T) (*MilvusStore, context.Context) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	// 每个测试独立 collection，避免相互干扰
	collName := fmt.Sprintf("test_%s_%d", t.Name(), time.Now().UnixNano()%100000)
	if len(collName) > 60 {
		collName = collName[:60]
	}

	store, err := NewMilvusStore(ctx, milvusAddr(), collName, testDim)
	if err != nil {
		t.Fatalf("connect milvus: %v\n(是否已执行 podman-compose up -d ?)", err)
	}

	if err := store.EnsureCollection(ctx); err != nil {
		t.Fatalf("ensure collection: %v", err)
	}

	t.Cleanup(func() {
		_ = store.DropCollection(context.Background())
		_ = store.Close()
	})
	return store, ctx
}

// mkVec 生成一个各分量相同的向量，便于构造可预测的相似度关系。
func mkVec(base float32) []float32 {
	v := make([]float32, testDim)
	v[0] = base
	for i := 1; i < testDim; i++ {
		v[i] = 1 - base
	}
	return v
}

func mkChunk(docID string, idx int, content string) rag.Chunk {
	return rag.Chunk{
		ID:      fmt.Sprintf("%s-c%d", docID, idx),
		DocID:   docID,
		Index:   idx,
		Content: content,
		Metadata: map[string]string{
			"chunker":      "test",
			"heading_path": "章节 > 小节",
		},
	}
}

func TestMilvus_UpsertAndSearch(t *testing.T) {
	store, ctx := newTestStore(t)

	chunks := []rag.Chunk{
		mkChunk("doc1", 0, "Redis 缓存穿透与布隆过滤器"),
		mkChunk("doc1", 1, "Kafka 分区与消费者组"),
		mkChunk("doc2", 0, "PostgreSQL 全文检索"),
	}
	vectors := [][]float32{
		mkVec(1.0), // 与查询向量最接近
		mkVec(0.0),
		mkVec(0.9),
	}

	if err := store.Upsert(ctx, chunks, vectors); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// ⚠️ 不 Flush 就搜索会返回空 —— 这正是 spike 阶段验证过的坑
	if err := store.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	got, err := store.Search(ctx, mkVec(1.0), 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}

	// 排序断言：mkVec(1.0) 应排第一，mkVec(0.9) 第二
	if got[0].Chunk.ID != "doc1-c0" {
		t.Errorf("expected doc1-c0 first, got %s", got[0].Chunk.ID)
	}
	if got[1].Chunk.ID != "doc2-c0" {
		t.Errorf("expected doc2-c0 second, got %s", got[1].Chunk.ID)
	}
	if got[0].Score < got[1].Score {
		t.Errorf("scores not descending: %.4f then %.4f", got[0].Score, got[1].Score)
	}

	// 字段完整性：内容和 metadata 必须能原样取回，
	// Phase 3 的答案溯源依赖 heading_path
	first := got[0].Chunk
	if first.Content != "Redis 缓存穿透与布隆过滤器" {
		t.Errorf("content mismatch: %q", first.Content)
	}
	if first.DocID != "doc1" {
		t.Errorf("doc_id mismatch: %q", first.DocID)
	}
	if first.Index != 0 {
		t.Errorf("chunk_index mismatch: %d", first.Index)
	}
	if first.Metadata["heading_path"] != "章节 > 小节" {
		t.Errorf("heading_path lost: %+v", first.Metadata)
	}
}

func TestMilvus_UpsertIsIdempotent(t *testing.T) {
	store, ctx := newTestStore(t)

	chunks := []rag.Chunk{mkChunk("doc1", 0, "原始内容")}
	vectors := [][]float32{mkVec(1.0)}

	// 第一次写入
	if err := store.Upsert(ctx, chunks, vectors); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// 同 ID 二次写入，内容变更
	chunks[0].Content = "更新后的内容"
	if err := store.Upsert(ctx, chunks, vectors); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if err := store.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	got, err := store.Search(ctx, mkVec(1.0), 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	// 关键断言：条数不翻倍，内容是最新的
	if len(got) != 1 {
		t.Fatalf("upsert created a duplicate: expected 1 row, got %d", len(got))
	}
	if got[0].Chunk.Content != "更新后的内容" {
		t.Errorf("upsert did not overwrite: %q", got[0].Chunk.Content)
	}
}

func TestMilvus_DeleteByDoc(t *testing.T) {
	store, ctx := newTestStore(t)

	chunks := []rag.Chunk{
		mkChunk("doc1", 0, "文档一第一块"),
		mkChunk("doc1", 1, "文档一第二块"),
		mkChunk("doc2", 0, "文档二内容"),
	}
	vectors := [][]float32{mkVec(1.0), mkVec(0.95), mkVec(0.9)}

	if err := store.Upsert(ctx, chunks, vectors); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if err := store.DeleteByDoc(ctx, "doc1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.Flush(ctx); err != nil {
		t.Fatalf("flush after delete: %v", err)
	}

	got, err := store.Search(ctx, mkVec(1.0), 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	// doc1 的两块应全部消失，doc2 保留
	for _, sc := range got {
		if sc.Chunk.DocID == "doc1" {
			t.Errorf("doc1 chunk survived deletion: %s", sc.Chunk.ID)
		}
	}
	if len(got) != 1 || got[0].Chunk.DocID != "doc2" {
		t.Errorf("expected only doc2 to remain, got %d rows", len(got))
	}
}

func TestMilvus_EnsureCollectionIsIdempotent(t *testing.T) {
	store, ctx := newTestStore(t)

	// 服务每次启动都会调用，必须能安全重复执行
	for i := range 3 {
		if err := store.EnsureCollection(ctx); err != nil {
			t.Fatalf("EnsureCollection call %d failed: %v", i+2, err)
		}
	}

	// 重复调用后仍可正常读写
	if err := store.Upsert(ctx,
		[]rag.Chunk{mkChunk("d", 0, "内容")},
		[][]float32{mkVec(1.0)},
	); err != nil {
		t.Fatalf("upsert after repeated ensure: %v", err)
	}
}

func TestMilvus_RejectsMismatchedCounts(t *testing.T) {
	store, ctx := newTestStore(t)

	err := store.Upsert(ctx,
		[]rag.Chunk{mkChunk("d", 0, "a"), mkChunk("d", 1, "b")},
		[][]float32{mkVec(1.0)}, // 只有 1 个向量对应 2 个 chunk
	)
	if err == nil {
		t.Fatal("expected an error when chunk and vector counts differ")
	}
}

func TestMilvus_RejectsWrongVectorDim(t *testing.T) {
	store, ctx := newTestStore(t)

	err := store.Upsert(ctx,
		[]rag.Chunk{mkChunk("d", 0, "a")},
		[][]float32{{1, 2, 3}}, // 3 维，实际需要 8 维
	)
	if err == nil {
		t.Fatal("expected an error for wrong vector dimension")
	}
}

func TestMilvus_SearchEmptyCollection(t *testing.T) {
	store, ctx := newTestStore(t)

	// 空集合检索应返回空结果而非报错
	got, err := store.Search(ctx, mkVec(1.0), 5)
	if err != nil {
		t.Fatalf("searching an empty collection should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no results, got %d", len(got))
	}
}

func TestMilvus_LongContentIsTruncatedNotRejected(t *testing.T) {
	store, ctx := newTestStore(t)

	// 超长内容应被截断，而不是让整批写入失败。
	// 用汉字构造：每字 3 字节，能同时验证「上限按字节算」和
	// 「截断不会切出非法 UTF-8」两件事。
	long := make([]rune, maxContentBytes) // 3 * maxContentBytes 字节，远超上限
	for i := range long {
		long[i] = '文'
	}

	if err := store.Upsert(ctx,
		[]rag.Chunk{mkChunk("d", 0, string(long))},
		[][]float32{mkVec(1.0)},
	); err != nil {
		t.Fatalf("long content should be truncated, not rejected: %v", err)
	}
	if err := store.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	got, err := store.Search(ctx, mkVec(1.0), 1)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}

	content := got[0].Chunk.Content
	if len(content) > maxContentBytes {
		t.Errorf("content is %d bytes, exceeds the %d limit", len(content), maxContentBytes)
	}
	// 截断必须落在字符边界上，否则取回的内容含替换字符
	if !utf8.ValidString(content) {
		t.Error("truncation produced invalid UTF-8")
	}
	if strings.ContainsRune(content, '�') {
		t.Error("truncation split a multi-byte character")
	}
}

func TestMilvus_RejectsOversizedID(t *testing.T) {
	store, ctx := newTestStore(t)

	// ID 超长必须报错而非截断 —— 截断会让不同 chunk 撞成同一主键，
	// 表现为静默丢数据，比直接失败危险得多
	ch := mkChunk("d", 0, "内容")
	ch.ID = strings.Repeat("x", maxIDBytes+1)

	if err := store.Upsert(ctx, []rag.Chunk{ch}, [][]float32{mkVec(1.0)}); err == nil {
		t.Fatal("expected an error for an oversized chunk ID")
	}
}
