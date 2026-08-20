//go:build e2e

// 用真实 Redis + 真实 LLM 检验压缩后的历史到底长什么样。
//
//	go test -tags=e2e -v -run TestMemoryCompaction ./internal/agent/
//
// 这组测试是为定位 12 轮验收失败而写的：单元测试全绿、摘要提示词
// 也确实保住了关键事实，但端到端就是丢上下文 —— 说明问题出在
// 「压缩 → 落盘 → 重新加载」这条真实链路上，而它恰好是唯一
// 被 mock 掉的一段。
package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/redis/go-redis/v9"

	"github.com/yurin-kami/KahoAgent/internal/config"
	"github.com/yurin-kami/KahoAgent/pkg/llm"
)

func realMemory(t *testing.T) (*SessionMemory, func()) {
	t.Helper()
	cfg, err := config.Load(findConfig(t))
	if err != nil {
		t.Skipf("config.toml is unavailable: %v", err)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		t.Skipf("redis is unreachable: %v", err)
	}

	chat := llm.NewChatModel(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model)
	m, err := NewSessionMemory(rdb, chat, WithMemoryLogger(quietLogger()))
	if err != nil {
		t.Fatalf("new memory: %v", err)
	}
	return m, func() { _ = rdb.Close() }
}

// TestMemoryCompaction_PreservesEarlyFacts 走完整的落盘链路。
func TestMemoryCompaction_PreservesEarlyFacts(t *testing.T) {
	m, done := realMemory(t)
	defer done()

	sid := fmt.Sprintf("e2e-compact-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = m.Clear(context.Background(), sid) })

	// 第 1 轮埋事实
	if err := m.Append(ctx, sid,
		user("记住：我给这个项目起的代号是「紫水晶」。之后我会问你。"),
		assistant("已记住：项目代号为「紫水晶」。")); err != nil {
		t.Fatalf("append: %v", err)
	}
	// 再堆 10 轮无关对话，越过 maxTurns=10
	for i := 2; i <= 11; i++ {
		if err := m.Append(ctx, sid,
			user(fmt.Sprintf("第 %d 个问题：说说缓存。", i)),
			assistant("缓存是用空间换时间的权衡。")); err != nil {
			t.Fatalf("append turn %d: %v", i, err)
		}
	}

	loaded, err := m.Load(ctx, sid)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	t.Logf("loaded %d messages after compaction:", len(loaded))
	for i, msg := range loaded {
		t.Logf("  [%d] %-9s %s", i, msg.Role, truncateRunes(msg.Content, 120))
	}

	if len(loaded) >= 22 {
		t.Fatalf("compaction never ran: %d messages", len(loaded))
	}

	var all strings.Builder
	for _, msg := range loaded {
		all.WriteString(msg.Content)
		all.WriteString("\n")
	}
	if !strings.Contains(all.String(), "紫水晶") {
		t.Errorf("the fact from turn 1 did not survive compaction; "+
			"an ordinal/recall follow-up will fail here.\nLoaded:\n%s", all.String())
	}

	// 摘要必须以系统消息的形式排在最前 —— 排在中间的话
	// 模型会把它当成某一轮的内容，而不是全局背景
	if len(loaded) > 0 {
		first := loaded[0]
		if first.Role != schema.System || !strings.HasPrefix(first.Content, summaryPrefix) {
			t.Errorf("expected the summary to lead the history, got role=%s content=%q",
				first.Role, truncateRunes(first.Content, 80))
		}
	}
}

// TestMemoryCompaction_IsIdempotent 压缩结果必须被写回。
//
// 不写回的话每次 Load 都要重新调一次 LLM 压缩 ——
// 既慢又费钱，而且每次摘要措辞不同，上下文会来回抖动。
func TestMemoryCompaction_IsIdempotent(t *testing.T) {
	m, done := realMemory(t)
	defer done()

	sid := fmt.Sprintf("e2e-idem-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = m.Clear(context.Background(), sid) })

	for i := 1; i <= 11; i++ {
		if err := m.Append(ctx, sid,
			user(fmt.Sprintf("第 %d 个问题。", i)),
			assistant("好的。")); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	first, err := m.Load(ctx, sid)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}

	start := time.Now()
	second, err := m.Load(ctx, sid)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	elapsed := time.Since(start)

	if len(first) != len(second) {
		t.Errorf("the two loads disagree: %d vs %d messages", len(first), len(second))
	}
	// 第二次不该再调 LLM，应该快得多
	if elapsed > 500*time.Millisecond {
		t.Errorf("the second load took %v — the summary was probably not persisted", elapsed)
	}
	t.Logf("second load: %d messages in %v", len(second), elapsed.Round(time.Millisecond))
}
