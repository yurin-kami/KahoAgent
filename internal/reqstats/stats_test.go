package reqstats

import (
	"context"
	"sync"
	"testing"
)

// TestNilSafety 验证 nil 接收者不会 panic。
//
// 这条很重要：下游埋点（LLM 客户端、检索器）拿到的 ctx 可能
// 根本不来自 handler（评估脚本、精排调用），From 会返回 nil。
// 如果方法不判空，这些路径会在打印日志时才炸。
func TestNilSafety(t *testing.T) {
	var s *Stats
	s.AddTokens(10, 20)
	s.MarkCacheHit()
	if got := s.Snapshot(); got != (Snapshot{}) {
		t.Fatalf("nil stats snapshot = %+v, want zero", got)
	}
	if From(context.Background()) != nil {
		t.Fatal("From on empty context should be nil")
	}
}

// TestWithFrom 验证 context 往返。
func TestWithFrom(t *testing.T) {
	s := New()
	ctx := With(context.Background(), s)
	if From(ctx) != s {
		t.Fatal("From did not return the injected stats")
	}
	if From(nil) != nil {
		t.Fatal("From(nil) should be nil")
	}
}

// TestSnapshot 验证累加与缓存标记。
func TestSnapshot(t *testing.T) {
	s := New()
	s.AddTokens(100, 50)
	s.AddTokens(10, 5)
	s.MarkCacheHit()

	got := s.Snapshot()
	if got.PromptTokens != 110 || got.CompletionTokens != 55 || !got.CacheHit {
		t.Fatalf("snapshot = %+v, want {110 55 true}", got)
	}
}

// TestConcurrent 验证并发累加不丢数据。
//
// 一次对话里可能有多个并发请求（复述、工具调用、精排），
// 它们都会往同一个 Stats 里写。
func TestConcurrent(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.AddTokens(1, 2)
			s.MarkCacheHit()
		}()
	}
	wg.Wait()

	got := s.Snapshot()
	if got.PromptTokens != 100 || got.CompletionTokens != 200 || !got.CacheHit {
		t.Fatalf("snapshot = %+v, want {100 200 true}", got)
	}
}
