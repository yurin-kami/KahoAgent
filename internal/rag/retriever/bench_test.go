package retriever

import (
	"context"
	"fmt"
	"testing"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

// 本文件测的是**本项目自己的** CPU 与内存开销：RRF 融合、结果重组、
// 并发调度。召回路用假实现（零延迟），所以测不到 Milvus/PG 的网络耗时。
//
// 为什么这样切：网络延迟由外部系统决定，压测它只是在测机房；
// 而融合与重组是我们写的代码，是唯一优化得动的部分。
// 端到端延迟另有实测数据（Phase 2 的 eval + Phase 3 的 SSE 首字）。
//
//	go test -bench=. -benchmem ./internal/rag/retriever/

// benchChunks 造 n 个块，内容长度接近真实分块（约 500 字）。
// 用短内容会让拷贝开销失真 —— 而结果重组正是在拷贝这些块。
func benchChunks(prefix string, n int) []rag.ScoredChunk {
	body := make([]rune, 500)
	for i := range body {
		body[i] = '缓' + rune(i%20)
	}
	content := string(body)

	out := make([]rag.ScoredChunk, n)
	for i := range out {
		out[i] = rag.ScoredChunk{
			Chunk: rag.Chunk{
				ID:      fmt.Sprintf("%s-%d", prefix, i),
				DocID:   "doc",
				Index:   i,
				Content: content,
				Metadata: map[string]string{
					"heading_path": "Redis 八股总集 > 三、缓存穿透 > 1. 定义",
					"source":       "interview-redis.md",
				},
			},
			Score: float32(n - i),
		}
	}
	return out
}

// overlappingRankings 模拟真实的双路召回：两路有部分重合。
// 全不重合的话 RRF 退化成简单拼接，测不出融合的实际开销。
func overlappingRankings(n int) []Ranking {
	vec := make([]string, n)
	bm := make([]string, n)
	for i := 0; i < n; i++ {
		vec[i] = fmt.Sprintf("c-%d", i)
		// 后一半重合，前一半各有各的 —— 与实测的双路重合度相近
		bm[i] = fmt.Sprintf("c-%d", (i+n/2)%n)
	}
	return []Ranking{
		{Name: "vector", IDs: vec},
		{Name: "bm25", IDs: bm},
	}
}

func BenchmarkFuseRRF(b *testing.B) {
	for _, n := range []int{10, 50, 200} {
		b.Run(fmt.Sprintf("recall=%d", n), func(b *testing.B) {
			rankings := overlappingRankings(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = FuseRRF(rankings)
			}
		})
	}
}

// BenchmarkHybridSearch 测完整的 Search 路径（不含网络）。
func BenchmarkHybridSearch(b *testing.B) {
	for _, n := range []int{10, 50, 200} {
		b.Run(fmt.Sprintf("recall=%d", n), func(b *testing.B) {
			vec := &fakeRecaller{name: "vector", hits: benchChunks("v", n)}
			bm := &fakeRecaller{name: "bm25", hits: benchChunks("b", n)}

			h, err := NewHybridRetriever([]Recaller{vec, bm},
				WithLogger(quiet()), WithRecallN(n))
			if err != nil {
				b.Fatal(err)
			}

			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := h.Search(ctx, "缓存穿透怎么解决"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkHybridSearch_CacheHit 测缓存命中路径。
// 与未命中对比能看出缓存到底省了多少 —— 这是 Phase 2
// 「273 倍」那个数字在纯 CPU 维度上的对照。
func BenchmarkHybridSearch_CacheHit(b *testing.B) {
	const n = 50
	vec := &fakeRecaller{name: "vector", hits: benchChunks("v", n)}
	bm := &fakeRecaller{name: "bm25", hits: benchChunks("b", n)}

	cache := newFakeCache()
	h, err := NewHybridRetriever([]Recaller{vec, bm},
		WithLogger(quiet()), WithRecallN(n), WithCache(cache))
	if err != nil {
		b.Fatal(err)
	}

	ctx := context.Background()
	// 先跑一次把缓存填上
	if _, err := h.Search(ctx, "缓存穿透怎么解决"); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := h.Search(ctx, "缓存穿透怎么解决"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCacheKey(b *testing.B) {
	vec := &fakeRecaller{name: "vector"}
	bm := &fakeRecaller{name: "bm25"}
	h, err := NewHybridRetriever([]Recaller{vec, bm}, WithLogger(quiet()))
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = h.cacheKey("缓存穿透怎么解决，布隆过滤器是不是万能的")
	}
}
