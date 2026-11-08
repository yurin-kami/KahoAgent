package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

// mockKeywordStore 记录写入与删除，用来验证 Ingester 的双写行为。
type mockKeywordStore struct {
	upsertErr error
	deleteErr error
	gotChunks []rag.Chunk
	upserts   int
	deleted   []string
}

func (m *mockKeywordStore) Upsert(_ context.Context, chunks []rag.Chunk) error {
	m.upserts++
	if m.upsertErr != nil {
		return m.upsertErr
	}
	m.gotChunks = chunks
	return nil
}

func (m *mockKeywordStore) DeleteByDoc(_ context.Context, docID string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	m.deleted = append(m.deleted, docID)
	return nil
}

func newStore() *mockStore { return &mockStore{} }

func newChunks() []rag.Chunk {
	return []rag.Chunk{
		{ID: "a", Index: 0, Content: "第一块"},
		{ID: "b", Index: 1, Content: "第二块"},
	}
}

// TestIngester_WritesKeywordStore 验证「写 Milvus 之后也写 PG」。
//
// 这条链路缺了不会报错，只会让 BM25 少一路召回 —— 属于典型的
// 「测试不覆盖就永远发现不了」的静默劣化，所以必须有断言。
func TestIngester_WritesKeywordStore(t *testing.T) {
	store := newStore()
	kw := &mockKeywordStore{}

	ing, err := NewIngester(&mockChunker{chunks: newChunks()}, &mockEmbedder{dim: 4},
		store, quietLogger(), WithKeywordStore(kw))
	if err != nil {
		t.Fatalf("NewIngester: %v", err)
	}

	n, err := ing.Run(context.Background(), "doc1", []byte("内容"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 2 {
		t.Fatalf("chunks = %d, want 2", n)
	}
	if kw.upserts != 1 {
		t.Fatalf("keyword upserts = %d, want 1", kw.upserts)
	}
	// 两个存储必须拿到同一批 chunk（同一套 ID），否则 RRF 无法按 ID 融合
	if len(kw.gotChunks) != 2 || kw.gotChunks[0].ID != store.gotChunks[0].ID {
		t.Fatalf("keyword chunks mismatch: %+v vs %+v", kw.gotChunks, store.gotChunks)
	}
}

// TestIngester_KeywordStoreErrorPropagates 验证 PG 写失败会让整次入库失败。
//
// 这是 consumer 不提交 offset 的依据：返回 error → 重试 → 两条路都重写。
// 如果这里被吞掉，消息会被标记为「已处理」，BM25 那一路就永久缺数据了。
func TestIngester_KeywordStoreErrorPropagates(t *testing.T) {
	kw := &mockKeywordStore{upsertErr: errors.New("pg down")}

	ing, err := NewIngester(&mockChunker{chunks: newChunks()}, &mockEmbedder{dim: 4},
		newStore(), quietLogger(), WithKeywordStore(kw))
	if err != nil {
		t.Fatalf("NewIngester: %v", err)
	}

	if _, err := ing.Run(context.Background(), "doc1", []byte("内容")); err == nil {
		t.Fatal("expected error when keyword store fails, got nil")
	}
}

// TestIngester_StaleCleanup 验证「先删后写」只在显式开启时发生。
//
// 覆盖的是更新后文档变短的场景：旧版本 12 块、新版本 8 块时，
// 不删旧块会让第 9~12 块永远留在索引里，检索结果拼出一篇不存在的文档。
func TestIngester_StaleCleanup(t *testing.T) {
	t.Run("关闭时不删除", func(t *testing.T) {
		store := newStore()
		ing, err := NewIngester(&mockChunker{chunks: newChunks()}, &mockEmbedder{dim: 4},
			store, quietLogger())
		if err != nil {
			t.Fatalf("NewIngester: %v", err)
		}
		if _, err := ing.Run(context.Background(), "doc1", []byte("内容")); err != nil {
			t.Fatalf("run: %v", err)
		}
		if len(store.deleted) != 0 {
			t.Fatalf("unexpected deletes: %v", store.deleted)
		}
	})

	t.Run("开启时两个存储都清空", func(t *testing.T) {
		store := newStore()
		kw := &mockKeywordStore{}
		ing, err := NewIngester(&mockChunker{chunks: newChunks()}, &mockEmbedder{dim: 4},
			store, quietLogger(), WithKeywordStore(kw), WithStaleCleanup())
		if err != nil {
			t.Fatalf("NewIngester: %v", err)
		}
		if _, err := ing.Run(context.Background(), "doc1", []byte("内容")); err != nil {
			t.Fatalf("run: %v", err)
		}
		if len(store.deleted) != 1 || store.deleted[0] != "doc1" {
			t.Fatalf("vector store deletes = %v, want [doc1]", store.deleted)
		}
		if len(kw.deleted) != 1 || kw.deleted[0] != "doc1" {
			t.Fatalf("keyword store deletes = %v, want [doc1]", kw.deleted)
		}
	})
}

// TestIngester_DeleteRemovesBothStores 验证删除路径同样双写。
//
// 只删 Milvus 的话，文档会「搜不到但 BM25 还能命中」——
// 用户点开引用却发现原文已删除，这比彻底搜不到更困惑。
func TestIngester_DeleteRemovesBothStores(t *testing.T) {
	store := newStore()
	kw := &mockKeywordStore{}
	ing, err := NewIngester(&mockChunker{chunks: newChunks()}, &mockEmbedder{dim: 4},
		store, quietLogger(), WithKeywordStore(kw))
	if err != nil {
		t.Fatalf("NewIngester: %v", err)
	}

	if err := ing.Delete(context.Background(), "doc1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(store.deleted) != 1 || len(kw.deleted) != 1 {
		t.Fatalf("deletes: vector=%v keyword=%v, want one each", store.deleted, kw.deleted)
	}
}
