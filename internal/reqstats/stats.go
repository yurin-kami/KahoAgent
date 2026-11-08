// Package reqstats 在单次请求内收集 token 用量与缓存命中，供前端展示。
//
// 为什么不直接从 Prometheus 读：指标是**聚合**的（按模型、按结果），
// 而「这一次请求用了多少 token」是请求级数据。用指标反推需要
// 时间窗口对齐，在并发下根本对不上。
//
// 实现方式是把一个 *Stats 放进 context：handler 创建、下游埋点、
// handler 结束时读取。相比全局变量，它天然按请求隔离；
// 相比改一堆函数签名，它对现有代码的侵入最小。
package reqstats

import (
	"context"
	"sync"
)

// Stats 是一次请求的统计。
//
// 零值可用（nil 指针除外）：没有调用方时不会 panic，
// 因为所有方法都对 nil 接收者安全 —— 这样下游埋点可以无脑调用，
// 不必到处判空。
type Stats struct {
	mu         sync.Mutex
	prompt     int
	completion int
	cacheHit   bool
}

// Snapshot 是某一时刻的只读快照。
type Snapshot struct {
	PromptTokens     int
	CompletionTokens int
	CacheHit         bool
}

// New 创建一个统计收集器。
func New() *Stats { return &Stats{} }

type ctxKey struct{}

// With 把收集器放进 context。
func With(ctx context.Context, s *Stats) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, s)
}

// From 取出当前请求的收集器；没有则返回 nil。
func From(ctx context.Context) *Stats {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(ctxKey{}).(*Stats)
	return s
}

// AddTokens 累加 token 用量（nil 安全）。
func (s *Stats) AddTokens(prompt, completion int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompt += prompt
	s.completion += completion
}

// MarkCacheHit 标记这次请求命中了检索缓存（nil 安全）。
func (s *Stats) MarkCacheHit() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cacheHit = true
}

// Snapshot 返回当前统计。
func (s *Stats) Snapshot() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{PromptTokens: s.prompt, CompletionTokens: s.completion, CacheHit: s.cacheHit}
}
