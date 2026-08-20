package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestGuard_PassesThroughResult(t *testing.T) {
	g := NewGuard(WithGuardLogger(quietLogger()))

	var gotArgs string
	wrapped := g.Wrap("demo", func(_ context.Context, args string) (string, error) {
		gotArgs = args
		return "结果", nil
	})

	out, err := wrapped(context.Background(), `{"query":"redis"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "结果" {
		t.Errorf("expected the result to pass through, got %q", out)
	}
	if gotArgs != `{"query":"redis"}` {
		t.Errorf("expected the arguments to pass through unchanged, got %q", gotArgs)
	}
}

func TestGuard_LimitsConcurrency(t *testing.T) {
	// 验收项：并发 50 个调用，超出上限的排队而非报错
	const (
		limit = 5
		calls = 50
	)
	g := NewGuard(WithMaxConcurrent(limit), WithGuardLogger(quietLogger()))

	var inFlight, peak int64
	var mu sync.Mutex

	wrapped := g.Wrap("demo", func(ctx context.Context, _ string) (string, error) {
		n := atomic.AddInt64(&inFlight, 1)
		mu.Lock()
		if n > peak {
			peak = n
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
		return "ok", nil
	})

	var wg sync.WaitGroup
	errs := make([]error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = wrapped(context.Background(), "{}")
		}(i)
	}
	wg.Wait()

	// 关键断言：全部成功。排队 ≠ 拒绝 ——
	// 直接返回"服务繁忙"的话，AI 客户端多半会立刻重试，反而雪上加霜
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d failed; queueing must not turn into rejection: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if peak > limit {
		t.Errorf("peak concurrency %d exceeded the limit of %d", peak, limit)
	}
	t.Logf("%d calls completed, peak concurrency %d (limit %d)", calls, peak, limit)
}

func TestGuard_TimesOutSlowCalls(t *testing.T) {
	g := NewGuard(WithTimeout(50*time.Millisecond), WithGuardLogger(quietLogger()))

	wrapped := g.Wrap("slow", func(ctx context.Context, _ string) (string, error) {
		select {
		case <-time.After(5 * time.Second):
			return "太慢了", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})

	start := time.Now()
	_, err := wrapped(context.Background(), "{}")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed > time.Second {
		t.Errorf("the timeout did not fire promptly: took %v", elapsed)
	}
}

func TestGuard_TimeoutExcludesQueueTime(t *testing.T) {
	// ⚠️ 超时必须在**拿到令牌之后**才开始计时。
	// 若把排队时间算进超时预算，负载高时所有调用都会超时 ——
	// 明明只是排队慢，却表现为工具全线故障，排查方向会完全跑偏。
	g := NewGuard(WithMaxConcurrent(1), WithTimeout(200*time.Millisecond),
		WithGuardLogger(quietLogger()))

	release := make(chan struct{})
	blocker := g.Wrap("blocker", func(ctx context.Context, _ string) (string, error) {
		<-release
		return "ok", nil
	})
	victim := g.Wrap("victim", func(ctx context.Context, _ string) (string, error) {
		// 很快就返回；只要计时是从拿到令牌开始，它就不该超时
		return "ok", ctx.Err()
	})

	go func() { _, _ = blocker(context.Background(), "{}") }()
	time.Sleep(30 * time.Millisecond) // 确保 blocker 已占住令牌

	done := make(chan error, 1)
	go func() {
		_, err := victim(context.Background(), "{}")
		done <- err
	}()

	// 排队时间（300ms）远超单次超时预算（200ms）
	time.Sleep(300 * time.Millisecond)
	close(release)

	if err := <-done; err != nil {
		t.Errorf("a call that waited in the queue must not inherit the queue time "+
			"as elapsed timeout: %v", err)
	}
}

func TestGuard_RespectsCallerCancellation(t *testing.T) {
	// 客户端断开时，排队中的调用应立刻放弃而不是继续等令牌
	g := NewGuard(WithMaxConcurrent(1), WithGuardLogger(quietLogger()))

	release := make(chan struct{})
	defer close(release)

	blocker := g.Wrap("blocker", func(context.Context, string) (string, error) {
		<-release
		return "ok", nil
	})
	go func() { _, _ = blocker(context.Background(), "{}") }()
	time.Sleep(30 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	var called atomic.Bool
	victim := g.Wrap("victim", func(context.Context, string) (string, error) {
		called.Store(true)
		return "ok", nil
	})

	done := make(chan error, 1)
	go func() { _, err := victim(ctx, "{}"); done <- err }()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected an error after the caller cancelled")
		}
		if called.Load() {
			t.Error("the tool must not run after the caller gave up")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not unblock the queued call")
	}
}

func TestGuard_AuditLogsToTheProvidedWriter(t *testing.T) {
	// ⚠️ 审计必须写到注入的 logger（生产里指向 stderr）。
	// stdio 模式下 stdout 是 JSON-RPC 协议通道，
	// 污染它会让客户端解析失败 —— Spike S4 已经踩过一次。
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	g := NewGuard(WithGuardLogger(logger))

	wrapped := g.Wrap("rag_query", func(context.Context, string) (string, error) {
		return "ok", nil
	})
	if _, err := wrapped(context.Background(), `{"query":"缓存穿透"}`); err != nil {
		t.Fatalf("run: %v", err)
	}

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("audit log is not valid JSON: %v\n%s", err, buf.String())
	}

	if entry["msg"] != "tool_call" {
		t.Errorf("expected a tool_call entry, got %v", entry["msg"])
	}
	if entry["tool"] != "rag_query" {
		t.Errorf("expected the tool name to be recorded, got %v", entry["tool"])
	}
	for _, field := range []string{"duration_ms", "status", "args_hash"} {
		if _, ok := entry[field]; !ok {
			t.Errorf("audit entry is missing %q: %s", field, buf.String())
		}
	}
}

func TestGuard_AuditDoesNotLeakArguments(t *testing.T) {
	// 查询词可能含个人信息、内部文件名甚至误粘贴的密码。
	// 审计日志保留久、访问面广，不该成为泄露源。
	var buf bytes.Buffer
	g := NewGuard(WithGuardLogger(slog.New(slog.NewJSONHandler(&buf, nil))))

	const secret = "hunter2-super-secret"
	wrapped := g.Wrap("demo", func(context.Context, string) (string, error) {
		return "ok", nil
	})
	if _, err := wrapped(context.Background(), `{"query":"`+secret+`"}`); err != nil {
		t.Fatalf("run: %v", err)
	}

	if strings.Contains(buf.String(), secret) {
		t.Errorf("the raw arguments leaked into the audit log:\n%s", buf.String())
	}
}

func TestGuard_AuditRecordsFailures(t *testing.T) {
	var buf bytes.Buffer
	g := NewGuard(WithGuardLogger(slog.New(slog.NewJSONHandler(&buf, nil))))

	wrapped := g.Wrap("broken", func(context.Context, string) (string, error) {
		return "", errors.New("milvus 连接失败")
	})
	if _, err := wrapped(context.Background(), "{}"); err == nil {
		t.Fatal("expected the error to propagate")
	}

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("audit log is not valid JSON: %v", err)
	}
	if entry["status"] != "error" {
		t.Errorf("expected status=error, got %v", entry["status"])
	}
	// 出了事要能查到原因，否则审计日志的价值大打折扣
	if !strings.Contains(buf.String(), "milvus") {
		t.Errorf("the failure reason should be recorded: %s", buf.String())
	}
}

func TestHashArgs_IsStableAndDistinct(t *testing.T) {
	a := hashArgs(`{"query":"redis"}`)
	if a != hashArgs(`{"query":"redis"}`) {
		t.Error("the same arguments must hash to the same value")
	}
	if a == hashArgs(`{"query":"kafka"}`) {
		t.Error("different arguments must hash differently")
	}
}
