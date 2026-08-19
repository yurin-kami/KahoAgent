// Package cache 提供检索结果的缓存。
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

// DefaultTTL 是缓存条目的存活时间。
//
// 1 小时是可用性与新鲜度的折中：文档入库后最多 1 小时才会被检索到，
// 对知识库场景完全可接受；而热门查询在这 1 小时里能省下大量
// embedding 调用和两次数据库往返。
const DefaultTTL = time.Hour

// keyPrefix 加版本号：缓存结构变更时只需 bump 版本，
// 旧数据自然失效，不必手动清库。
const keyPrefix = "kaho:retrieval:v1:"

// QueryCache 用 Redis 缓存检索结果。
//
// 为什么值得缓存：一次未命中的混合检索要做 1 次 embedding API 调用
// （网络往返上百毫秒）+ Milvus 向量搜索 + PG 全文检索。
// 命中缓存则只有一次 Redis GET，延迟差两个数量级。
type QueryCache struct {
	rdb    *redis.Client
	ttl    time.Duration
	logger *slog.Logger
}

type Option func(*QueryCache)

func WithTTL(d time.Duration) Option {
	return func(c *QueryCache) {
		if d > 0 {
			c.ttl = d
		}
	}
}

func WithLogger(l *slog.Logger) Option {
	return func(c *QueryCache) {
		if l != nil {
			c.logger = l
		}
	}
}

// New 创建缓存客户端。
func New(addr, password string, db int, opts ...Option) *QueryCache {
	c := &QueryCache{
		rdb: redis.NewClient(&redis.Options{
			Addr:     addr,
			Password: password,
			DB:       db,
			// 超时设得短：缓存是优化手段而非必需品，
			// 与其等一个慢 Redis，不如直接走后端。
			DialTimeout:  2 * time.Second,
			ReadTimeout:  500 * time.Millisecond,
			WriteTimeout: 500 * time.Millisecond,
		}),
		ttl:    DefaultTTL,
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// NewWithClient 用已有的 Redis 客户端创建缓存，便于测试注入。
func NewWithClient(rdb *redis.Client, opts ...Option) *QueryCache {
	c := &QueryCache{rdb: rdb, ttl: DefaultTTL, logger: slog.Default()}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *QueryCache) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

func (c *QueryCache) Close() error { return c.rdb.Close() }

// Get 读取缓存。
//
// ⚠️ 任何错误都返回 miss 而非上报 —— 缓存不可用时应静默降级到
// 后端检索，而不是让整个请求失败。缓存是加速器，不是依赖项。
func (c *QueryCache) Get(ctx context.Context, key string) ([]rag.ScoredChunk, bool) {
	raw, err := c.rdb.Get(ctx, hashKey(key)).Bytes()
	if err != nil {
		// redis.Nil 是正常的未命中，不值得记日志
		if !errors.Is(err, redis.Nil) {
			c.logger.Warn("cache get failed, falling back to backend", "err", err)
		}
		return nil, false
	}

	var chunks []rag.ScoredChunk
	if err := json.Unmarshal(raw, &chunks); err != nil {
		// 反序列化失败说明缓存内容已损坏或结构变更，
		// 当作未命中并顺手清掉，避免反复踩同一条脏数据
		c.logger.Warn("corrupt cache entry, discarding", "err", err)
		c.rdb.Del(ctx, hashKey(key))
		return nil, false
	}
	return chunks, true
}

// Set 写入缓存。错误只记日志不返回 —— 写缓存失败不该影响本次请求的结果。
func (c *QueryCache) Set(ctx context.Context, key string, chunks []rag.ScoredChunk) {
	// 不缓存空结果：查不到往往是文档还没入库，
	// 缓存 1 小时会让刚上传的文档一直查不到，体验很差。
	if len(chunks) == 0 {
		return
	}

	raw, err := json.Marshal(chunks)
	if err != nil {
		c.logger.Warn("cache marshal failed", "err", err)
		return
	}
	if err := c.rdb.Set(ctx, hashKey(key), raw, c.ttl).Err(); err != nil {
		c.logger.Warn("cache set failed", "err", err)
	}
}

// Invalidate 清空全部检索缓存。
//
// 文档入库或删除后调用：底层数据变了，缓存里的结果就不再准确。
// 用 SCAN 而非 KEYS —— KEYS 会阻塞 Redis 直到遍历完，
// 在生产库上执行足以造成明显卡顿。
func (c *QueryCache) Invalidate(ctx context.Context) error {
	var (
		cursor  uint64
		deleted int
	)
	for {
		keys, next, err := c.rdb.Scan(ctx, cursor, keyPrefix+"*", 100).Result()
		if err != nil {
			return fmt.Errorf("scan cache keys: %w", err)
		}
		if len(keys) > 0 {
			if err := c.rdb.Del(ctx, keys...).Err(); err != nil {
				return fmt.Errorf("delete cache keys: %w", err)
			}
			deleted += len(keys)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	c.logger.Info("retrieval cache invalidated", "keys", deleted)
	return nil
}

// hashKey 把可能很长的查询 key 压成定长哈希。
//
// 用户查询可以任意长，直接做 Redis key 既浪费内存也不便观察。
// 哈希后定长 32 字符，且天然规避了 key 里出现空格、换行等字符的问题。
func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return keyPrefix + hex.EncodeToString(sum[:])[:32]
}
