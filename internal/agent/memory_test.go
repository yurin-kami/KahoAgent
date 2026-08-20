package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// 这些测试只覆盖纯函数部分（裁剪、摘要切分、token 估算）。
// 需要 Redis 的读写路径在 memory_integration_test.go 里测。

func user(s string) *schema.Message      { return schema.UserMessage(s) }
func assistant(s string) *schema.Message { return schema.AssistantMessage(s, nil) }

// conversation 生成 n 轮「用户问 + 助手答」。
func conversation(n int) []*schema.Message {
	msgs := make([]*schema.Message, 0, n*2)
	for i := 1; i <= n; i++ {
		msgs = append(msgs, user(fmt.Sprintf("问题%d", i)), assistant(fmt.Sprintf("回答%d", i)))
	}
	return msgs
}

func TestCountTurns(t *testing.T) {
	// 轮数以用户消息计 —— 助手可能因为工具调用产生多条消息，
	// 按消息数算会把一轮算成好几轮
	msgs := []*schema.Message{
		user("一"), assistant("答"),
		schema.ToolMessage("工具结果", "call_1"), assistant("再答"),
		user("二"),
	}
	if got := countTurns(msgs); got != 2 {
		t.Errorf("expected 2 turns, got %d", got)
	}
}

func TestSplitAtTurn(t *testing.T) {
	msgs := conversation(4) // 8 条消息，用户消息在下标 0/2/4/6

	cases := []struct {
		n    int
		want int
	}{
		{0, 0},  // 不切
		{-1, 0}, // 非法值等同不切
		{1, 2},  // 切在第 2 轮用户消息处
		{2, 4},
		{3, 6},
		{4, 8}, // 全切
		{99, 8},
	}
	for _, tc := range cases {
		if got := splitAtTurn(msgs, tc.n); got != tc.want {
			t.Errorf("splitAtTurn(n=%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}

func TestEstimateTokens_CJKvsASCII(t *testing.T) {
	// 中文按字算，英文按 4 字符算 —— 同样的字符数，
	// 中文的 token 数应显著更高
	cjk := estimateStringTokens(strings.Repeat("字", 100))
	ascii := estimateStringTokens(strings.Repeat("a", 100))

	if cjk <= ascii {
		t.Errorf("CJK should estimate higher than ASCII: cjk=%d ascii=%d", cjk, ascii)
	}
	if cjk != 100 {
		t.Errorf("expected ~1 token per CJK char, got %d", cjk)
	}
	if ascii != 25 {
		t.Errorf("expected ~4 chars per token for ASCII, got %d", ascii)
	}
}

func TestCapTokens_UnderLimitIsUnchanged(t *testing.T) {
	msgs := conversation(3)
	got := capTokens(msgs, maxTokens)
	if len(got) != len(msgs) {
		t.Errorf("expected no trimming, got %d of %d", len(got), len(msgs))
	}
}

func TestCapTokens_DropsOldestFirst(t *testing.T) {
	// 超限时丢最早的 —— 最近的对话与当前问题最相关
	msgs := []*schema.Message{
		user(strings.Repeat("旧", 3000)),
		user(strings.Repeat("新", 100)),
	}
	got := capTokens(msgs, 1000)

	if len(got) != 1 {
		t.Fatalf("expected 1 message to survive, got %d", len(got))
	}
	if !strings.Contains(got[0].Content, "新") {
		t.Errorf("the most recent message must survive, got %q", truncateRunes(got[0].Content, 20))
	}
}

func TestCapTokens_KeepsSummaryEvenWhenOldest(t *testing.T) {
	// ⚠️ 摘要浓缩了最多信息，丢掉它等于白压缩一场。
	// 即便它排在最前面也要保住。
	msgs := []*schema.Message{
		schema.SystemMessage(summaryPrefix + "用户在找 Redis 相关文档"),
		user(strings.Repeat("很长的问题", 1000)),
		user("最新问题"),
	}
	got := capTokens(msgs, 500)

	var hasSummary bool
	for _, m := range got {
		if strings.HasPrefix(m.Content, summaryPrefix) {
			hasSummary = true
		}
	}
	if !hasSummary {
		t.Error("the summary must be preserved when trimming")
	}
}

func TestCapTokens_DoesNotMutateInput(t *testing.T) {
	// ⚠️ 切片里存的是指针。早期实现直接改 msg.Content 做截断，
	// 改动会顺着 Load 的返回值传播到调用方持有的同一批对象上 ——
	// 表现为「读一次历史，存量数据就被截短一次」。
	original := strings.Repeat("字", 5000)
	msgs := []*schema.Message{user(original)}

	_ = capTokens(msgs, 100)

	if msgs[0].Content != original {
		t.Errorf("capTokens must not mutate the caller's messages: length went %d → %d",
			len([]rune(original)), len([]rune(msgs[0].Content)))
	}
}

func TestCapTokens_TruncatesWhenOnlySummaryRemains(t *testing.T) {
	// 能丢的都丢完仍超限时要截断，而不是返回超限内容 ——
	// 后者会让上游 API 直接报错
	msgs := []*schema.Message{
		schema.SystemMessage(summaryPrefix + strings.Repeat("摘", 5000)),
	}
	got := capTokens(msgs, 100)

	if len(got) != 1 {
		t.Fatalf("expected the summary to survive, got %d messages", len(got))
	}
	if estimateTokens(got) > 200 {
		t.Errorf("the oversized summary should have been truncated, still %d tokens", estimateTokens(got))
	}
}

func TestCapTokens_TerminatesOnAllSummaries(t *testing.T) {
	// 全是摘要且都超限时不能死循环 —— 跳过逻辑必须推进下标
	msgs := []*schema.Message{
		schema.SystemMessage(summaryPrefix + strings.Repeat("甲", 2000)),
		schema.SystemMessage(summaryPrefix + strings.Repeat("乙", 2000)),
	}

	done := make(chan []*schema.Message, 1)
	go func() { done <- capTokens(msgs, 50) }()

	select {
	case got := <-done:
		if len(got) == 0 {
			t.Error("expected the summaries to be kept (truncated), got none")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("capTokens did not terminate — the skip branch must advance the index")
	}
}

// ---- compact 的降级路径 ----

type stubSummarizer struct {
	out    string
	err    error
	called int
	gotIn  string
}

func (s *stubSummarizer) Complete(_ context.Context, prompt string) (string, error) {
	s.called++
	s.gotIn = prompt
	return s.out, s.err
}

func TestSummarize_IncludesEarlyContent(t *testing.T) {
	s := &stubSummarizer{out: "用户询问了 Redis 和 Kafka"}
	m := &SessionMemory{summarizer: s, logger: quietLogger(), maxTurns: maxTurns, keepTurns: keepTurns}

	got, err := m.summarize(context.Background(), []*schema.Message{
		user("Redis 怎么用"), assistant("Redis 是内存数据库"),
	})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if got != "用户询问了 Redis 和 Kafka" {
		t.Errorf("unexpected summary: %q", got)
	}
	// prompt 里要带上原始对话，否则模型没东西可压缩
	if !strings.Contains(s.gotIn, "Redis 怎么用") {
		t.Errorf("the prompt must contain the conversation, got: %s", s.gotIn)
	}
}

func TestSummarize_RejectsEmptyOutput(t *testing.T) {
	// 空摘要比没有摘要更糟：它占着位置却不含信息，
	// 还会让调用方以为压缩成功了
	s := &stubSummarizer{out: "   "}
	m := &SessionMemory{summarizer: s, logger: quietLogger()}

	if _, err := m.summarize(context.Background(), conversation(2)); err == nil {
		t.Fatal("expected an error for an empty summary")
	}
}
