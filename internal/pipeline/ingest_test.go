package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

// 三个 mock 实现 —— Ingester 依赖接口而非具体类型，
// 所以这些测试完全不需要 Milvus 或 DashScope。

type mockChunker struct {
	chunks []rag.Chunk
	err    error
	calls  int
}

func (m *mockChunker) Name() string { return "mock" }
func (m *mockChunker) Chunk(_ context.Context, docID string, _ []byte) ([]rag.Chunk, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	// 回填 docID，模拟真实 chunker 的行为
	out := make([]rag.Chunk, len(m.chunks))
	copy(out, m.chunks)
	for i := range out {
		out[i].DocID = docID
	}
	return out, nil
}

type mockEmbedder struct {
	dim     int
	err     error
	vectors [][]float32 // 非 nil 时直接返回，用于构造数量不匹配的场景
	gotText []string
}

func (m *mockEmbedder) Dim() int { return m.dim }
func (m *mockEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	m.gotText = texts
	if m.err != nil {
		return nil, m.err
	}
	if m.vectors != nil {
		return m.vectors, nil
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, m.dim)
		v[0] = float32(len(texts[i])) // 指纹：便于断言顺序
		out[i] = v
	}
	return out, nil
}

type mockStore struct {
	err         error
	gotChunks   []rag.Chunk
	gotVectors  [][]float32
	upsertCalls int
	deleted     []string
	deleteErr   error
}

func (m *mockStore) Upsert(_ context.Context, chunks []rag.Chunk, vectors [][]float32) error {
	m.upsertCalls++
	if m.err != nil {
		return m.err
	}
	m.gotChunks = chunks
	m.gotVectors = vectors
	return nil
}

func (m *mockStore) Search(context.Context, []float32, int) ([]rag.ScoredChunk, error) {
	return nil, nil
}

func (m *mockStore) DeleteByDoc(_ context.Context, docID string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	m.deleted = append(m.deleted, docID)
	return nil
}

// quietLogger 丢弃日志输出，避免测试刷屏
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestIngester(t *testing.T, c rag.Chunker, e rag.Embedder, s rag.VectorStore) *Ingester {
	t.Helper()
	ing, err := NewIngester(c, e, s, quietLogger())
	if err != nil {
		t.Fatalf("NewIngester: %v", err)
	}
	return ing
}

func TestIngester_HappyPath(t *testing.T) {
	chunker := &mockChunker{chunks: []rag.Chunk{
		{ID: "a", Index: 0, Content: "第一块"},
		{ID: "b", Index: 1, Content: "第二块内容"},
	}}
	embedder := &mockEmbedder{dim: 4}
	store := &mockStore{}

	n, err := newTestIngester(t, chunker, embedder, store).
		Run(context.Background(), "doc1", []byte("原始内容"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 chunks ingested, got %d", n)
	}

	// 传给 Embedder 的应是 chunk 内容，顺序一致
	want := []string{"第一块", "第二块内容"}
	for i, w := range want {
		if embedder.gotText[i] != w {
			t.Errorf("embed text[%d]: expected %q, got %q", i, w, embedder.gotText[i])
		}
	}

	// 写入的 chunk 与向量必须对齐 —— 用指纹验证没有错位
	if len(store.gotChunks) != 2 || len(store.gotVectors) != 2 {
		t.Fatalf("store got %d chunks / %d vectors", len(store.gotChunks), len(store.gotVectors))
	}
	for i, ch := range store.gotChunks {
		if fp := store.gotVectors[i][0]; fp != float32(len(ch.Content)) {
			t.Errorf("chunk[%d] %q paired with the wrong vector (fingerprint %v)", i, ch.Content, fp)
		}
		if ch.DocID != "doc1" {
			t.Errorf("chunk[%d]: doc_id is %q, expected doc1", i, ch.DocID)
		}
	}
}

func TestIngester_EmptyDocumentIsNotAnError(t *testing.T) {
	// 空文件是合法输入。若当成错误，Kafka 会无限重试这条永远不成功的消息。
	chunker := &mockChunker{chunks: nil}
	embedder := &mockEmbedder{dim: 4}
	store := &mockStore{}

	n, err := newTestIngester(t, chunker, embedder, store).
		Run(context.Background(), "empty", nil)
	if err != nil {
		t.Fatalf("empty document should not error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 chunks, got %d", n)
	}
	// 没有内容就不该浪费一次 embedding 调用和一次写入
	if embedder.gotText != nil {
		t.Error("embedder was called for an empty document")
	}
	if store.upsertCalls != 0 {
		t.Errorf("store was written to for an empty document (%d calls)", store.upsertCalls)
	}
}

func TestIngester_PropagatesChunkerError(t *testing.T) {
	sentinel := errors.New("chunker exploded")
	chunker := &mockChunker{err: sentinel}
	embedder := &mockEmbedder{dim: 4}
	store := &mockStore{}

	_, err := newTestIngester(t, chunker, embedder, store).
		Run(context.Background(), "doc1", []byte("x"))
	if !errors.Is(err, sentinel) {
		t.Errorf("expected the chunker error to be wrapped, got: %v", err)
	}
	if store.upsertCalls != 0 {
		t.Error("nothing should be written when chunking fails")
	}
}

func TestIngester_PropagatesEmbedderError(t *testing.T) {
	sentinel := errors.New("api down")
	chunker := &mockChunker{chunks: []rag.Chunk{{ID: "a", Content: "内容"}}}
	embedder := &mockEmbedder{dim: 4, err: sentinel}
	store := &mockStore{}

	_, err := newTestIngester(t, chunker, embedder, store).
		Run(context.Background(), "doc1", []byte("x"))
	if !errors.Is(err, sentinel) {
		t.Errorf("expected the embedder error to be wrapped, got: %v", err)
	}
	// 关键：向量化失败绝不能写入半成品数据
	if store.upsertCalls != 0 {
		t.Error("nothing should be written when embedding fails")
	}
}

func TestIngester_PropagatesStoreError(t *testing.T) {
	sentinel := errors.New("milvus unreachable")
	chunker := &mockChunker{chunks: []rag.Chunk{{ID: "a", Content: "内容"}}}
	embedder := &mockEmbedder{dim: 4}
	store := &mockStore{err: sentinel}

	n, err := newTestIngester(t, chunker, embedder, store).
		Run(context.Background(), "doc1", []byte("x"))
	if !errors.Is(err, sentinel) {
		t.Errorf("expected the store error to be wrapped, got: %v", err)
	}
	// 失败时必须返回 0，否则 Kafka 侧会误以为成功而提交 offset
	if n != 0 {
		t.Errorf("expected 0 on failure, got %d", n)
	}
}

func TestIngester_DetectsVectorCountMismatch(t *testing.T) {
	// Embedder 返回的向量数与块数不符时必须拒绝写入。
	// 放任写入会产生文本与向量错位的数据 —— 检索结果看着正常但内容全错。
	chunker := &mockChunker{chunks: []rag.Chunk{
		{ID: "a", Content: "一"},
		{ID: "b", Content: "二"},
	}}
	embedder := &mockEmbedder{dim: 4, vectors: [][]float32{{1, 0, 0, 0}}} // 只给 1 个
	store := &mockStore{}

	_, err := newTestIngester(t, chunker, embedder, store).
		Run(context.Background(), "doc1", []byte("x"))
	if err == nil {
		t.Fatal("expected an error when vector count != chunk count")
	}
	if store.upsertCalls != 0 {
		t.Error("misaligned data must never reach the store")
	}
}

func TestIngester_RejectsEmptyDocID(t *testing.T) {
	// docID 为空会让 DeleteByDoc 无法定位文档，必须在入口挡住
	chunker := &mockChunker{chunks: []rag.Chunk{{ID: "a", Content: "内容"}}}
	ing := newTestIngester(t, chunker, &mockEmbedder{dim: 4}, &mockStore{})

	if _, err := ing.Run(context.Background(), "", []byte("x")); err == nil {
		t.Error("expected an error for an empty docID")
	}
	if err := ing.Delete(context.Background(), ""); err == nil {
		t.Error("Delete should reject an empty docID")
	}
}

func TestIngester_Delete(t *testing.T) {
	store := &mockStore{}
	ing := newTestIngester(t, &mockChunker{}, &mockEmbedder{dim: 4}, store)

	if err := ing.Delete(context.Background(), "doc1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "doc1" {
		t.Errorf("expected doc1 to be deleted, got %v", store.deleted)
	}
}

func TestIngester_DeletePropagatesError(t *testing.T) {
	sentinel := errors.New("delete failed")
	ing := newTestIngester(t, &mockChunker{}, &mockEmbedder{dim: 4},
		&mockStore{deleteErr: sentinel})

	if err := ing.Delete(context.Background(), "doc1"); !errors.Is(err, sentinel) {
		t.Errorf("expected the store error to be wrapped, got: %v", err)
	}
}

func TestNewIngester_RejectsNilDependencies(t *testing.T) {
	// nil 依赖应在启动时暴露，而不是等到第一次入库时空指针
	c, e, s := &mockChunker{}, &mockEmbedder{dim: 4}, &mockStore{}

	cases := []struct {
		name     string
		chunker  rag.Chunker
		embedder rag.Embedder
		store    rag.VectorStore
	}{
		{"nil chunker", nil, e, s},
		{"nil embedder", c, nil, s},
		{"nil store", c, e, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewIngester(tc.chunker, tc.embedder, tc.store, nil); err == nil {
				t.Errorf("expected an error for %s", tc.name)
			}
		})
	}

	// logger 为 nil 是允许的，回退到默认 logger
	if _, err := NewIngester(c, e, s, nil); err != nil {
		t.Errorf("nil logger should be allowed: %v", err)
	}
}
