package retriever

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	einoretriever "github.com/cloudwego/eino/components/retriever"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

// fakeRecaller 让融合与降级逻辑无需 Milvus/PG 即可测试。
type fakeRecaller struct {
	name    string
	hits    []rag.ScoredChunk
	err     error
	delay   time.Duration
	calls   atomic.Int32
	gotTopN atomic.Int32
}

func (f *fakeRecaller) Name() string { return f.name }
func (f *fakeRecaller) Recall(ctx context.Context, _ string, topN int) ([]rag.ScoredChunk, error) {
	f.calls.Add(1)
	f.gotTopN.Store(int32(topN))

	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

func chunks(ids ...string) []rag.ScoredChunk {
	out := make([]rag.ScoredChunk, len(ids))
	for i, id := range ids {
		out[i] = rag.ScoredChunk{
			Chunk: rag.Chunk{
				ID: id, DocID: "d", Index: i, Content: "内容-" + id,
				Metadata: map[string]string{"heading_path": "章 > 节"},
			},
			Score: float32(len(ids) - i),
		}
	}
	return out
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func resultIDs(rs []rag.ScoredChunk) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Chunk.ID
	}
	return out
}

func TestHybrid_FusesBothPaths(t *testing.T) {
	vec := &fakeRecaller{name: "vector", hits: chunks("A", "B", "C")}
	bm := &fakeRecaller{name: "bm25", hits: chunks("C", "A", "D")}

	h, err := NewHybridRetriever([]Recaller{vec, bm}, WithLogger(quiet()), WithTopK(4))
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	got, err := h.Search(context.Background(), "查询")
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	// 与 RRF 算例一致：A > C > B > D
	want := []string{"A", "C", "B", "D"}
	gotIDs := resultIDs(got)
	for i := range want {
		if i >= len(gotIDs) || gotIDs[i] != want[i] {
			t.Fatalf("expected order %v, got %v", want, gotIDs)
		}
	}
}

func TestHybrid_RecallsConcurrently(t *testing.T) {
	// 两路互不依赖，必须并发 —— 串行会让延迟翻倍
	const delay = 300 * time.Millisecond
	vec := &fakeRecaller{name: "vector", hits: chunks("A"), delay: delay}
	bm := &fakeRecaller{name: "bm25", hits: chunks("B"), delay: delay}

	h, _ := NewHybridRetriever([]Recaller{vec, bm}, WithLogger(quiet()))

	start := time.Now()
	if _, err := h.Search(context.Background(), "查询"); err != nil {
		t.Fatalf("search: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > delay*2 {
		t.Errorf("recall appears serial: took %v for two %v calls", elapsed, delay)
	}
}

func TestHybrid_DegradesWhenOnePathFails(t *testing.T) {
	// 核心降级要求：BM25 挂了还有向量结果，反之亦然。
	// 这也是刻意不用 errgroup 的原因 —— 它会在首个错误时取消 context，
	// 把另一路也连累掉。
	t.Run("bm25 fails", func(t *testing.T) {
		vec := &fakeRecaller{name: "vector", hits: chunks("A", "B")}
		bm := &fakeRecaller{name: "bm25", err: errors.New("pg is down")}

		h, _ := NewHybridRetriever([]Recaller{vec, bm}, WithLogger(quiet()))
		got, err := h.Search(context.Background(), "查询")
		if err != nil {
			t.Fatalf("a single failing path must not fail the search: %v", err)
		}
		if len(got) != 2 || got[0].Chunk.ID != "A" {
			t.Errorf("expected the vector results to survive, got %v", resultIDs(got))
		}
	})

	t.Run("vector fails", func(t *testing.T) {
		vec := &fakeRecaller{name: "vector", err: errors.New("milvus is down")}
		bm := &fakeRecaller{name: "bm25", hits: chunks("X", "Y")}

		h, _ := NewHybridRetriever([]Recaller{vec, bm}, WithLogger(quiet()))
		got, err := h.Search(context.Background(), "查询")
		if err != nil {
			t.Fatalf("a single failing path must not fail the search: %v", err)
		}
		if len(got) != 2 || got[0].Chunk.ID != "X" {
			t.Errorf("expected the bm25 results to survive, got %v", resultIDs(got))
		}
	})

	t.Run("both fail", func(t *testing.T) {
		vec := &fakeRecaller{name: "vector", err: errors.New("down")}
		bm := &fakeRecaller{name: "bm25", err: errors.New("down")}

		h, _ := NewHybridRetriever([]Recaller{vec, bm}, WithLogger(quiet()))
		got, err := h.Search(context.Background(), "查询")
		// 全挂时返回空而非错误：对调用方而言就是「没查到」
		if err != nil {
			t.Errorf("expected no error, got %v", err)
		}
		if len(got) != 0 {
			t.Errorf("expected no results, got %v", resultIDs(got))
		}
	})
}

func TestHybrid_OnePathFailingDoesNotCancelTheOther(t *testing.T) {
	// 回归测试：若改用 errgroup，快速失败的一路会取消慢的那一路，
	// 这个测试会挂。
	fast := &fakeRecaller{name: "fast-fail", err: errors.New("boom")}
	slow := &fakeRecaller{name: "slow-ok", hits: chunks("survivor"), delay: 200 * time.Millisecond}

	h, _ := NewHybridRetriever([]Recaller{fast, slow}, WithLogger(quiet()))
	got, err := h.Search(context.Background(), "查询")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].Chunk.ID != "survivor" {
		t.Errorf("the slow path was cancelled by the fast failure: %v", resultIDs(got))
	}
}

func TestHybrid_AppliesTopK(t *testing.T) {
	vec := &fakeRecaller{name: "vector", hits: chunks("A", "B", "C", "D", "E")}

	h, _ := NewHybridRetriever([]Recaller{vec}, WithLogger(quiet()), WithTopK(2))
	got, err := h.Search(context.Background(), "查询")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 results, got %d: %v", len(got), resultIDs(got))
	}
}

func TestHybrid_PassesRecallNToRecallers(t *testing.T) {
	// 召回数应远大于 topK：RRF 需要足够的候选才有交叉印证的信号
	vec := &fakeRecaller{name: "vector", hits: chunks("A")}

	h, _ := NewHybridRetriever([]Recaller{vec}, WithLogger(quiet()),
		WithRecallN(50), WithTopK(5))
	if _, err := h.Search(context.Background(), "查询"); err != nil {
		t.Fatalf("search: %v", err)
	}

	if got := vec.gotTopN.Load(); got != 50 {
		t.Errorf("expected recallN=50 passed to the recaller, got %d", got)
	}
}

// --- 缓存 ---

type fakeCache struct {
	data     map[string][]rag.ScoredChunk
	getCalls atomic.Int32
	setCalls atomic.Int32
	lastKey  string
}

func newFakeCache() *fakeCache {
	return &fakeCache{data: map[string][]rag.ScoredChunk{}}
}

func (c *fakeCache) Get(_ context.Context, key string) ([]rag.ScoredChunk, bool) {
	c.getCalls.Add(1)
	c.lastKey = key
	v, ok := c.data[key]
	return v, ok
}

func (c *fakeCache) Set(_ context.Context, key string, chunks []rag.ScoredChunk) {
	c.setCalls.Add(1)
	c.lastKey = key
	c.data[key] = chunks
}

func TestHybrid_CacheHitSkipsRecall(t *testing.T) {
	vec := &fakeRecaller{name: "vector", hits: chunks("A", "B")}
	cache := newFakeCache()

	h, _ := NewHybridRetriever([]Recaller{vec}, WithLogger(quiet()), WithCache(cache))

	if _, err := h.Search(context.Background(), "查询"); err != nil {
		t.Fatalf("first search: %v", err)
	}
	if got := vec.calls.Load(); got != 1 {
		t.Fatalf("expected 1 recall on a cold cache, got %d", got)
	}

	if _, err := h.Search(context.Background(), "查询"); err != nil {
		t.Fatalf("second search: %v", err)
	}
	// 命中缓存后不应再打后端
	if got := vec.calls.Load(); got != 1 {
		t.Errorf("cache hit still triggered a recall: %d calls", got)
	}
}

func TestHybrid_CacheKeyIncludesParameters(t *testing.T) {
	// ⚠️ key 只用 query 的话，改了 topK 仍会命中旧缓存 ——
	// 评估时会得到莫名其妙的结果
	mk := func(topK int, rr Reranker) string {
		vec := &fakeRecaller{name: "vector"}
		opts := []HybridOption{WithLogger(quiet()), WithTopK(topK)}
		if rr != nil {
			opts = append(opts, WithRerank(rr))
		}
		h, _ := NewHybridRetriever([]Recaller{vec}, opts...)
		return h.cacheKey("同一个查询")
	}

	k5, k10 := mk(5, nil), mk(10, nil)
	if k5 == k10 {
		t.Errorf("cache key ignores topK: %q", k5)
	}

	kNoRerank, kRerank := mk(5, nil), mk(5, &fakeReranker{})
	if kNoRerank == kRerank {
		t.Errorf("cache key ignores the reranker: %q", kNoRerank)
	}
}

// --- 精排 ---

type fakeReranker struct {
	err    error
	result []rag.ScoredChunk
	gotN   atomic.Int32
}

func (r *fakeReranker) Rerank(_ context.Context, _ string, candidates []rag.ScoredChunk, topK int) ([]rag.ScoredChunk, error) {
	r.gotN.Store(int32(len(candidates)))
	if r.err != nil {
		return nil, r.err
	}
	if r.result != nil {
		return r.result, nil
	}
	// 默认反转顺序，便于断言精排确实生效
	out := make([]rag.ScoredChunk, len(candidates))
	for i, c := range candidates {
		out[len(candidates)-1-i] = c
	}
	if len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

func TestHybrid_RerankReordersResults(t *testing.T) {
	vec := &fakeRecaller{name: "vector", hits: chunks("A", "B", "C")}
	rr := &fakeReranker{}

	h, _ := NewHybridRetriever([]Recaller{vec}, WithLogger(quiet()),
		WithRerank(rr), WithTopK(3))

	got, err := h.Search(context.Background(), "查询")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// fakeReranker 会反转，故应为 C, B, A
	if ids := resultIDs(got); ids[0] != "C" {
		t.Errorf("rerank did not take effect: %v", ids)
	}
}

func TestHybrid_RerankFailureDegradesToRRFOrder(t *testing.T) {
	// 精排失败不该让整次检索失败 —— RRF 的结果已经可用
	vec := &fakeRecaller{name: "vector", hits: chunks("A", "B")}
	rr := &fakeReranker{err: errors.New("llm timeout")}

	h, _ := NewHybridRetriever([]Recaller{vec}, WithLogger(quiet()), WithRerank(rr))

	got, err := h.Search(context.Background(), "查询")
	if err != nil {
		t.Fatalf("rerank failure must not fail the search: %v", err)
	}
	if len(got) != 2 || got[0].Chunk.ID != "A" {
		t.Errorf("expected the RRF order to survive, got %v", resultIDs(got))
	}
}

func TestHybrid_RerankSeesLimitedCandidates(t *testing.T) {
	// 全部候选都精排太慢且没必要，RRF 已把不相关的排到后面
	ids := make([]string, 40)
	for i := range ids {
		ids[i] = string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	vec := &fakeRecaller{name: "vector", hits: chunks(ids...)}
	rr := &fakeReranker{}

	h, _ := NewHybridRetriever([]Recaller{vec}, WithLogger(quiet()), WithRerank(rr))
	if _, err := h.Search(context.Background(), "查询"); err != nil {
		t.Fatalf("search: %v", err)
	}

	if got := rr.gotN.Load(); got > rerankCandidates {
		t.Errorf("reranker received %d candidates, expected at most %d", got, rerankCandidates)
	}
}

// --- eino 接口适配 ---

func TestHybrid_RetrieveMapsToEinoDocuments(t *testing.T) {
	vec := &fakeRecaller{name: "vector", hits: chunks("A", "B")}
	h, _ := NewHybridRetriever([]Recaller{vec}, WithLogger(quiet()))

	docs, err := h.Retrieve(context.Background(), "查询")
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 documents, got %d", len(docs))
	}

	d := docs[0]
	if d.ID != "A" {
		t.Errorf("ID: expected A, got %q", d.ID)
	}
	if d.Content != "内容-A" {
		t.Errorf("Content: got %q", d.Content)
	}
	// Phase 3 的答案溯源依赖这些字段
	if d.MetaData["heading_path"] != "章 > 节" {
		t.Errorf("heading_path lost: %+v", d.MetaData)
	}
	if _, ok := d.MetaData["score"]; !ok {
		t.Errorf("score missing: %+v", d.MetaData)
	}
}

func TestHybrid_RetrieveHonorsTopKOption(t *testing.T) {
	vec := &fakeRecaller{name: "vector", hits: chunks("A", "B", "C", "D")}
	h, _ := NewHybridRetriever([]Recaller{vec}, WithLogger(quiet()), WithTopK(4))

	docs, err := h.Retrieve(context.Background(), "查询", einoretriever.WithTopK(2))
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(docs) != 2 {
		t.Errorf("eino TopK option ignored: got %d documents", len(docs))
	}
}

func TestHybrid_RetrieveTopKOptionDoesNotMutateShared(t *testing.T) {
	// 用选项改 topK 时必须复制，否则并发调用会互相污染
	vec := &fakeRecaller{name: "vector", hits: chunks("A", "B", "C", "D")}
	h, _ := NewHybridRetriever([]Recaller{vec}, WithLogger(quiet()), WithTopK(4))

	if _, err := h.Retrieve(context.Background(), "查询", einoretriever.WithTopK(1)); err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if h.topK != 4 {
		t.Errorf("shared retriever was mutated: topK is now %d, expected 4", h.topK)
	}
}

func TestHybrid_RetrieveAppliesScoreThreshold(t *testing.T) {
	vec := &fakeRecaller{name: "vector", hits: chunks("A", "B", "C")}
	h, _ := NewHybridRetriever([]Recaller{vec}, WithLogger(quiet()))

	// 阈值高于所有 RRF 分数（约 0.016），应全部过滤掉
	docs, err := h.Retrieve(context.Background(), "查询",
		einoretriever.WithScoreThreshold(1.0))
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("score threshold not applied: got %d documents", len(docs))
	}
}

func TestNewHybridRetriever_RequiresARecaller(t *testing.T) {
	if _, err := NewHybridRetriever(nil); err == nil {
		t.Error("expected an error when no recaller is provided")
	}
}

func TestHybrid_RejectsEmptyQuery(t *testing.T) {
	vec := &fakeRecaller{name: "vector"}
	h, _ := NewHybridRetriever([]Recaller{vec}, WithLogger(quiet()))

	if _, err := h.Search(context.Background(), ""); err == nil {
		t.Error("expected an error for an empty query")
	}
}
