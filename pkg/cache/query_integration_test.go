//go:build integration

// Redis 缓存集成测试。需要真实 Redis：
//
//	podman-compose up -d
//	go test -tags=integration ./pkg/cache/ -v
//
// 用真实 Redis 而非 mock：要验证的正是序列化往返、TTL、SCAN 这些
// 与真实客户端行为强相关的部分，mock 只会验证我对 API 的想象。
package cache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

func redisAddr() string {
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:6379"
}

func newTestCache(t *testing.T, opts ...Option) (*QueryCache, context.Context) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	// DB 15 专用于测试，避免污染开发数据
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr(), DB: 15})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("connect redis: %v\n(是否已执行 podman-compose up -d ?)", err)
	}

	c := NewWithClient(rdb, opts...)
	if err := c.Invalidate(ctx); err != nil {
		t.Fatalf("clean start: %v", err)
	}

	t.Cleanup(func() {
		_ = c.Invalidate(context.Background())
		_ = c.Close()
	})
	return c, ctx
}

func sample() []rag.ScoredChunk {
	return []rag.ScoredChunk{
		{
			Chunk: rag.Chunk{
				ID: "c1", DocID: "d1", Index: 0,
				Content:  "缓存穿透指查询不存在的数据",
				Metadata: map[string]string{"heading_path": "Redis > 缓存穿透"},
			},
			Score: 0.0325,
		},
		{
			Chunk: rag.Chunk{
				ID: "c2", DocID: "d1", Index: 1,
				Content:  "布隆过滤器可以拦截不存在的 key",
				Metadata: map[string]string{"heading_path": "Redis > 布隆过滤器"},
			},
			Score: 0.0161,
		},
	}
}

func TestCache_RoundTripPreservesEverything(t *testing.T) {
	c, ctx := newTestCache(t)
	want := sample()

	c.Set(ctx, "查询", want)

	got, ok := c.Get(ctx, "查询")
	if !ok {
		t.Fatal("expected a cache hit")
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d chunks, got %d", len(want), len(got))
	}

	for i := range want {
		w, g := want[i], got[i]
		if g.Chunk.ID != w.Chunk.ID {
			t.Errorf("[%d] ID: expected %q, got %q", i, w.Chunk.ID, g.Chunk.ID)
		}
		if g.Chunk.Content != w.Chunk.Content {
			t.Errorf("[%d] Content: expected %q, got %q", i, w.Chunk.Content, g.Chunk.Content)
		}
		if g.Chunk.DocID != w.Chunk.DocID {
			t.Errorf("[%d] DocID mismatch", i)
		}
		if g.Chunk.Index != w.Chunk.Index {
			t.Errorf("[%d] Index mismatch", i)
		}
		// Phase 3 的答案溯源依赖 heading_path
		if g.Chunk.Metadata["heading_path"] != w.Chunk.Metadata["heading_path"] {
			t.Errorf("[%d] heading_path lost: %+v", i, g.Chunk.Metadata)
		}
		if g.Score != w.Score {
			t.Errorf("[%d] Score: expected %v, got %v", i, w.Score, g.Score)
		}
	}
}

func TestCache_HitIsFasterThan10ms(t *testing.T) {
	// 对应 Phase 2 验收项：「Redis 缓存命中后延迟 <10ms」
	c, ctx := newTestCache(t)
	c.Set(ctx, "查询", sample())

	// 预热一次，排除首次连接建立的开销
	if _, ok := c.Get(ctx, "查询"); !ok {
		t.Fatal("expected a cache hit")
	}

	const runs = 20
	var total time.Duration
	worst := time.Duration(0)

	for range runs {
		start := time.Now()
		if _, ok := c.Get(ctx, "查询"); !ok {
			t.Fatal("expected a cache hit")
		}
		d := time.Since(start)
		total += d
		if d > worst {
			worst = d
		}
	}

	avg := total / runs
	t.Logf("cache hit latency: avg %v, worst %v over %d runs", avg, worst, runs)

	if avg >= 10*time.Millisecond {
		t.Errorf("average cache hit took %v, expected <10ms", avg)
	}
}

func TestCache_MissOnUnknownKey(t *testing.T) {
	c, ctx := newTestCache(t)

	if _, ok := c.Get(ctx, "从没存过的查询"); ok {
		t.Error("expected a miss for an unknown key")
	}
}

func TestCache_DifferentKeysDoNotCollide(t *testing.T) {
	c, ctx := newTestCache(t)

	c.Set(ctx, "查询A", []rag.ScoredChunk{{Chunk: rag.Chunk{ID: "a"}, Score: 1}})
	c.Set(ctx, "查询B", []rag.ScoredChunk{{Chunk: rag.Chunk{ID: "b"}, Score: 1}})

	a, okA := c.Get(ctx, "查询A")
	b, okB := c.Get(ctx, "查询B")

	if !okA || !okB {
		t.Fatal("expected both keys to hit")
	}
	if a[0].Chunk.ID != "a" || b[0].Chunk.ID != "b" {
		t.Errorf("keys collided: A=%s B=%s", a[0].Chunk.ID, b[0].Chunk.ID)
	}
}

func TestCache_EmptyResultsAreNotCached(t *testing.T) {
	// 查不到往往是文档还没入库。缓存 1 小时会让刚上传的文档
	// 一直查不到，体验很差。
	c, ctx := newTestCache(t)

	c.Set(ctx, "无结果查询", nil)
	if _, ok := c.Get(ctx, "无结果查询"); ok {
		t.Error("empty results should not be cached")
	}

	c.Set(ctx, "空切片查询", []rag.ScoredChunk{})
	if _, ok := c.Get(ctx, "空切片查询"); ok {
		t.Error("an empty slice should not be cached")
	}
}

func TestCache_RespectsTTL(t *testing.T) {
	c, ctx := newTestCache(t, WithTTL(time.Second))

	c.Set(ctx, "短命查询", sample())
	if _, ok := c.Get(ctx, "短命查询"); !ok {
		t.Fatal("expected a hit immediately after Set")
	}

	// 等过期
	time.Sleep(1500 * time.Millisecond)

	if _, ok := c.Get(ctx, "短命查询"); ok {
		t.Error("entry should have expired")
	}
}

func TestCache_Invalidate(t *testing.T) {
	// 文档入库或删除后，缓存里的结果不再准确，必须能整体清空
	c, ctx := newTestCache(t)

	for _, q := range []string{"查询1", "查询2", "查询3"} {
		c.Set(ctx, q, sample())
	}
	if _, ok := c.Get(ctx, "查询2"); !ok {
		t.Fatal("setup failed: expected a hit")
	}

	if err := c.Invalidate(ctx); err != nil {
		t.Fatalf("invalidate: %v", err)
	}

	for _, q := range []string{"查询1", "查询2", "查询3"} {
		if _, ok := c.Get(ctx, q); ok {
			t.Errorf("%s survived invalidation", q)
		}
	}
}

func TestCache_InvalidateLeavesOtherKeysAlone(t *testing.T) {
	// Invalidate 用前缀匹配，不能误删同一个 Redis 里的其他数据
	c, ctx := newTestCache(t)

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr(), DB: 15})
	defer rdb.Close()

	const outsider = "some:other:app:key"
	if err := rdb.Set(ctx, outsider, "别动我", time.Minute).Err(); err != nil {
		t.Fatalf("seed outsider key: %v", err)
	}
	t.Cleanup(func() { rdb.Del(context.Background(), outsider) })

	c.Set(ctx, "查询", sample())
	if err := c.Invalidate(ctx); err != nil {
		t.Fatalf("invalidate: %v", err)
	}

	if err := rdb.Get(ctx, outsider).Err(); err != nil {
		t.Errorf("invalidation deleted an unrelated key: %v", err)
	}
}

func TestCache_CorruptEntryIsTreatedAsMiss(t *testing.T) {
	// 缓存内容损坏或结构变更时应静默降级，并顺手清掉脏数据
	c, ctx := newTestCache(t)

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr(), DB: 15})
	defer rdb.Close()

	key := hashKey("损坏的查询")
	if err := rdb.Set(ctx, key, "{这不是合法的 JSON", time.Minute).Err(); err != nil {
		t.Fatalf("seed corrupt entry: %v", err)
	}

	if _, ok := c.Get(ctx, "损坏的查询"); ok {
		t.Error("a corrupt entry should be reported as a miss")
	}
	// 脏数据应被清掉，避免反复踩
	if err := rdb.Get(ctx, key).Err(); err == nil {
		t.Error("the corrupt entry should have been deleted")
	}
}

func TestCache_UnreachableRedisDegradesGracefully(t *testing.T) {
	// 缓存是加速器不是依赖项：Redis 挂了应静默降级到后端检索，
	// 而不是让整个请求失败
	rdb := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:16380", // 无人监听
		DialTimeout: 200 * time.Millisecond,
	})
	defer rdb.Close()

	c := NewWithClient(rdb)
	ctx := context.Background()

	if _, ok := c.Get(ctx, "查询"); ok {
		t.Error("expected a miss when Redis is unreachable")
	}
	// Set 不应 panic 或阻塞
	c.Set(ctx, "查询", sample())
}

func TestCache_LongKeysAreHashed(t *testing.T) {
	// 用户查询可以任意长，直接做 Redis key 既浪费内存也不便观察
	c, ctx := newTestCache(t)

	long := ""
	for range 500 {
		long += "很长的查询内容"
	}

	c.Set(ctx, long, sample())
	got, ok := c.Get(ctx, long)
	if !ok {
		t.Fatal("expected a hit for a long key")
	}
	if len(got) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(got))
	}

	// key 长度应固定，与查询长度无关
	if n := len(hashKey(long)); n != len(keyPrefix)+32 {
		t.Errorf("hashed key length is %d, expected %d", n, len(keyPrefix)+32)
	}
}
