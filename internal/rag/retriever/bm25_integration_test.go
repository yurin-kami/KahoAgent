//go:build integration

// BM25 集成测试。需要真实的 PostgreSQL：
//
//	podman-compose up -d
//	go test -tags=integration ./internal/rag/retriever/ -v
package retriever

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

func pgDSN() string {
	if v := os.Getenv("PG_DSN"); v != "" {
		return v
	}
	return "host=127.0.0.1 port=5432 user=kaho password=kaho_dev_pw dbname=kahoagent sslmode=disable"
}

// newTestPG 建表并返回连接池，测试结束清空数据。
func newTestPG(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	pool, err := pgxpool.New(ctx, pgDSN())
	if err != nil {
		t.Fatalf("connect pg: %v\n(是否已执行 podman-compose up -d ?)", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping pg: %v\n(是否已执行 podman-compose up -d ?)", err)
	}

	// 应用 migration —— 幂等，可重复执行
	migration, err := os.ReadFile("../../../migrations/001_chunks.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}

	// 每个测试从干净状态开始
	if _, err := pool.Exec(ctx, "TRUNCATE chunks"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "TRUNCATE chunks")
		pool.Close()
	})
	return pool, ctx
}

func mkChunk(id, docID, content string) rag.Chunk {
	return rag.Chunk{
		ID: id, DocID: docID, Index: 0, Content: content,
		Metadata: map[string]string{"heading_path": "测试 > 小节"},
	}
}

func seed(t *testing.T, pool *pgxpool.Pool, ctx context.Context, chunks ...rag.Chunk) {
	t.Helper()
	w, err := NewChunkWriter(pool)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	if err := w.Upsert(ctx, chunks); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func ids(results []rag.ScoredChunk) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = r.Chunk.ID
	}
	return out
}

func TestBM25_FindsChineseKeywords(t *testing.T) {
	// 核心验证：PG 自带分词器不认识中文，必须靠 gojieba 预分词。
	// 若分词失效，查「缓存穿透」将完全匹配不到「Redis缓存穿透」。
	pool, ctx := newTestPG(t)
	seed(t, pool, ctx,
		mkChunk("c1", "d1", "Redis缓存穿透指查询数据库中不存在的数据，用布隆过滤器解决"),
		mkChunk("c2", "d1", "Kafka分区与消费者组的关系"),
		mkChunk("c3", "d2", "PostgreSQL全文检索使用tsvector"),
	)

	r, err := NewBM25Retriever(pool)
	if err != nil {
		t.Fatalf("new retriever: %v", err)
	}

	got, err := r.Recall(ctx, "缓存穿透", 10)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no results — Chinese tokenization is likely broken")
	}
	if got[0].Chunk.ID != "c1" {
		t.Errorf("expected c1 first, got %s (all: %v)", got[0].Chunk.ID, ids(got))
	}
	if got[0].Score <= 0 {
		t.Errorf("expected a positive ts_rank score, got %v", got[0].Score)
	}
}

func TestBM25_ExactTermBeatsSemanticNeighbours(t *testing.T) {
	// BM25 的看家本领：专有名词精确匹配。
	// 向量检索会把 RedisJSON 稀释成「Redis + JSON」的语义，
	// BM25 则能精准命中。
	pool, ctx := newTestPG(t)
	seed(t, pool, ctx,
		mkChunk("exact", "d1", "RedisJSON 模块提供原生的 JSON 数据类型支持"),
		mkChunk("noise1", "d1", "Redis 是一个高性能的键值存储系统"),
		mkChunk("noise2", "d1", "Redis 支持字符串、列表、集合等数据结构"),
	)

	r, _ := NewBM25Retriever(pool)
	got, err := r.Recall(ctx, "RedisJSON", 10)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no results for an exact term match")
	}
	if got[0].Chunk.ID != "exact" {
		t.Errorf("expected the exact match first, got %s (all: %v)", got[0].Chunk.ID, ids(got))
	}
}

func TestBM25_ReturnsFullChunkData(t *testing.T) {
	// 检索结果要能直接喂给 LLM，字段不能丢
	pool, ctx := newTestPG(t)
	seed(t, pool, ctx, mkChunk("c1", "doc-abc", "布隆过滤器是一个概率型数据结构"))

	r, _ := NewBM25Retriever(pool)
	got, err := r.Recall(ctx, "布隆过滤器", 5)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}

	c := got[0].Chunk
	if c.DocID != "doc-abc" {
		t.Errorf("doc_id: expected doc-abc, got %q", c.DocID)
	}
	if !strings.Contains(c.Content, "布隆过滤器") {
		t.Errorf("content lost: %q", c.Content)
	}
	if c.Metadata["heading_path"] != "测试 > 小节" {
		t.Errorf("metadata lost: %+v", c.Metadata)
	}
}

func TestBM25_MultiWordQueryUsesOR(t *testing.T) {
	// 用 AND 连接会让召回率趋近 0 —— 几乎没有文档含全部词。
	// 这里的查询词分散在不同文档中，OR 应该都能召回。
	pool, ctx := newTestPG(t)
	seed(t, pool, ctx,
		mkChunk("c1", "d1", "缓存穿透的解决方案"),
		mkChunk("c2", "d1", "消息队列的削峰填谷作用"),
	)

	r, _ := NewBM25Retriever(pool)
	got, err := r.Recall(ctx, "缓存穿透和消息队列", 10)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	// OR 语义下两条都该被召回
	if len(got) != 2 {
		t.Errorf("expected both chunks recalled via OR, got %d: %v", len(got), ids(got))
	}
}

func TestBM25_RanksMoreMatchesHigher(t *testing.T) {
	// OR 召回宽，但 ts_rank 要保证命中词更多的排更前
	pool, ctx := newTestPG(t)
	seed(t, pool, ctx,
		mkChunk("both", "d1", "缓存穿透和缓存雪崩都是常见的缓存问题"),
		mkChunk("one", "d1", "消息队列可以削峰填谷，缓存不是万能的"),
	)

	r, _ := NewBM25Retriever(pool)
	got, err := r.Recall(ctx, "缓存穿透 缓存雪崩", 10)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(got) < 1 {
		t.Fatal("no results")
	}
	if got[0].Chunk.ID != "both" {
		t.Errorf("expected the chunk matching more terms first, got %s: %v", got[0].Chunk.ID, ids(got))
	}
}

func TestBM25_NoMatchReturnsEmpty(t *testing.T) {
	pool, ctx := newTestPG(t)
	seed(t, pool, ctx, mkChunk("c1", "d1", "Redis 缓存"))

	r, _ := NewBM25Retriever(pool)
	got, err := r.Recall(ctx, "量子纠缠", 5)
	if err != nil {
		t.Fatalf("no-match should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no results, got %v", ids(got))
	}
}

func TestBM25_RespectsTopN(t *testing.T) {
	pool, ctx := newTestPG(t)
	chunks := make([]rag.Chunk, 10)
	for i := range chunks {
		chunks[i] = mkChunk(string(rune('a'+i)), "d1", "缓存相关的内容")
	}
	seed(t, pool, ctx, chunks...)

	r, _ := NewBM25Retriever(pool)
	got, err := r.Recall(ctx, "缓存", 3)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("expected exactly 3 results, got %d", len(got))
	}
}

func TestChunkWriter_UpsertIsIdempotent(t *testing.T) {
	// 与 Milvus 侧一致：重复入库不能堆积
	pool, ctx := newTestPG(t)
	w, _ := NewChunkWriter(pool)

	c := mkChunk("c1", "d1", "原始内容关于缓存")
	if err := w.Upsert(ctx, []rag.Chunk{c}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	c.Content = "更新后的内容关于缓存"
	if err := w.Upsert(ctx, []rag.Chunk{c}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM chunks").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("upsert created a duplicate: %d rows", count)
	}

	// tokens 也必须跟着更新，否则检索的是旧内容
	r, _ := NewBM25Retriever(pool)
	got, err := r.Recall(ctx, "更新", 5)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Chunk.Content, "更新后") {
		t.Errorf("tokens were not refreshed on upsert: %v", got)
	}
}

func TestChunkWriter_DeleteByDoc(t *testing.T) {
	pool, ctx := newTestPG(t)
	w, _ := NewChunkWriter(pool)

	if err := w.Upsert(ctx, []rag.Chunk{
		mkChunk("c1", "doc1", "文档一的缓存内容"),
		mkChunk("c2", "doc1", "文档一的另一段缓存内容"),
		mkChunk("c3", "doc2", "文档二的缓存内容"),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := w.DeleteByDoc(ctx, "doc1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	r, _ := NewBM25Retriever(pool)
	got, err := r.Recall(ctx, "缓存", 10)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	for _, sc := range got {
		if sc.Chunk.DocID == "doc1" {
			t.Errorf("doc1 chunk survived deletion: %s", sc.Chunk.ID)
		}
	}
	if len(got) != 1 || got[0].Chunk.DocID != "doc2" {
		t.Errorf("expected only doc2 to remain, got %v", ids(got))
	}
}

func TestBM25_RejectsInvalidArgs(t *testing.T) {
	pool, ctx := newTestPG(t)
	r, _ := NewBM25Retriever(pool)

	if _, err := r.Recall(ctx, "", 5); err == nil {
		t.Error("expected an error for an empty query")
	}
	if _, err := r.Recall(ctx, "缓存", 0); err == nil {
		t.Error("expected an error for topN=0")
	}
}

func TestBM25_TsqueryOperatorsDoNotBreakTheQuery(t *testing.T) {
	// 用户输入含 tsquery 语法字符时不能让查询崩掉
	pool, ctx := newTestPG(t)
	seed(t, pool, ctx, mkChunk("c1", "d1", "缓存穿透的解决方案"))

	r, _ := NewBM25Retriever(pool)
	for _, q := range []string{"缓存 & 穿透", "缓存 | 穿透", "缓存:*", "(缓存)", "!缓存", "缓存'"} {
		if _, err := r.Recall(ctx, q, 5); err != nil {
			t.Errorf("query %q should not error: %v", q, err)
		}
	}
}
