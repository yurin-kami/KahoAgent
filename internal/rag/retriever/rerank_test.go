package retriever

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

// fakeScorer 让精排逻辑无需真调 LLM 即可测试 ——
// 否则每跑一次测试都要花钱且要等数秒。
type fakeScorer struct {
	// byContent 按内容关键字返回分数，模拟「模型读懂了内容」
	byContent map[string]string
	fixed     string
	err       error
	errFor    map[string]bool // 对含特定关键字的内容返回错误
	delay     time.Duration

	calls    atomic.Int32
	inFlight atomic.Int32
	maxSeen  atomic.Int32
	prompts  []string
	mu       sync.Mutex
}

func (f *fakeScorer) Score(ctx context.Context, prompt string) (string, error) {
	f.calls.Add(1)

	// 记录并发峰值，用于验证信号量确实生效
	cur := f.inFlight.Add(1)
	for {
		maxV := f.maxSeen.Load()
		if cur <= maxV || f.maxSeen.CompareAndSwap(maxV, cur) {
			break
		}
	}
	defer f.inFlight.Add(-1)

	f.mu.Lock()
	f.prompts = append(f.prompts, prompt)
	f.mu.Unlock()

	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(f.delay):
		}
	}

	for kw := range f.errFor {
		if strings.Contains(prompt, kw) {
			return "", errors.New("scoring failed for " + kw)
		}
	}
	if f.err != nil {
		return "", f.err
	}
	for kw, score := range f.byContent {
		if strings.Contains(prompt, kw) {
			return score, nil
		}
	}
	if f.fixed != "" {
		return f.fixed, nil
	}
	return "5", nil
}

func rerankCandidatesFrom(contents ...string) []rag.ScoredChunk {
	out := make([]rag.ScoredChunk, len(contents))
	for i, c := range contents {
		out[i] = rag.ScoredChunk{
			Chunk: rag.Chunk{ID: c, Content: c},
			// RRF 量级的分数，且按输入顺序递减
			Score: float32(0.02 - float64(i)*0.001),
		}
	}
	return out
}

func TestRerank_ReordersByModelScore(t *testing.T) {
	// 精排的核心价值：RRF 排在后面但实际更相关的块应被提上来
	scorer := &fakeScorer{byContent: map[string]string{
		"不相关内容": "1",
		"部分相关":  "6",
		"完全命中":  "10",
	}}
	r, err := NewLLMReranker(scorer, quiet())
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	// 输入顺序刻意与正确顺序相反
	candidates := rerankCandidatesFrom("不相关内容", "部分相关", "完全命中")

	got, err := r.Rerank(context.Background(), "查询", candidates, 3)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}

	want := []string{"完全命中", "部分相关", "不相关内容"}
	for i, w := range want {
		if got[i].Chunk.ID != w {
			t.Errorf("position %d: expected %q, got %q (all: %v)",
				i, w, got[i].Chunk.ID, resultIDs(got))
		}
	}
}

func TestRerank_NormalisesScoresToUnitRange(t *testing.T) {
	// LLM 输出 0-10，RRF 是 0.016 量级。不归一化的话两者混在同一个
	// Score 字段里无从判断量纲 —— 而打分失败的块保留的正是 RRF 分数。
	scorer := &fakeScorer{fixed: "10"}
	r, _ := NewLLMReranker(scorer, quiet())

	got, err := r.Rerank(context.Background(), "查询", rerankCandidatesFrom("内容"), 1)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if got[0].Score != 1.0 {
		t.Errorf("expected a score of 1.0 for a 10/10 rating, got %v", got[0].Score)
	}
}

func TestRerank_ParsesMessyModelOutput(t *testing.T) {
	// 即便 prompt 强调「只输出数字」，模型仍可能加前缀或解释。
	// 与其指望模型完全听话，不如在解析侧宽容。
	cases := []struct {
		output string
		want   float32
	}{
		{"8", 0.8},
		{"8分", 0.8},
		{"Score: 8", 0.8},
		{"  9  ", 0.9},
		{"这个片段的相关性是 7 分，因为...", 0.7},
		{"10", 1.0},
		{"0", 0},
		{"7.5", 0.75},
		{"15", 1.0}, // 超出上限应夹紧而非拒绝
	}

	for _, tc := range cases {
		t.Run(tc.output, func(t *testing.T) {
			scorer := &fakeScorer{fixed: tc.output}
			r, _ := NewLLMReranker(scorer, quiet())

			got, err := r.Rerank(context.Background(), "查询",
				rerankCandidatesFrom("内容"), 1)
			if err != nil {
				t.Fatalf("rerank: %v", err)
			}
			if got[0].Score != tc.want {
				t.Errorf("output %q: expected %v, got %v", tc.output, tc.want, got[0].Score)
			}
		})
	}
}

func TestRerank_UnparseableOutputKeepsRRFScore(t *testing.T) {
	// 解析失败时保留原分数而非置 0 —— 置 0 会把一个可能相关的块
	// 直接踢到最后，比保持原状更糟
	scorer := &fakeScorer{fixed: "无法评分"}
	r, _ := NewLLMReranker(scorer, quiet())

	candidates := rerankCandidatesFrom("内容")
	original := candidates[0].Score

	got, err := r.Rerank(context.Background(), "查询", candidates, 1)
	if err != nil {
		t.Fatalf("unparseable output must not fail the rerank: %v", err)
	}
	if got[0].Score != original {
		t.Errorf("expected the RRF score %v to be kept, got %v", original, got[0].Score)
	}
}

func TestRerank_PartialFailureDoesNotFailEverything(t *testing.T) {
	// 一块打分失败不该毁掉整次精排
	scorer := &fakeScorer{
		byContent: map[string]string{"好内容": "9"},
		errFor:    map[string]bool{"坏内容": true},
	}
	r, _ := NewLLMReranker(scorer, quiet())

	got, err := r.Rerank(context.Background(), "查询",
		rerankCandidatesFrom("好内容", "坏内容"), 2)
	if err != nil {
		t.Fatalf("a partial failure must not fail the rerank: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	if got[0].Chunk.ID != "好内容" {
		t.Errorf("expected the successfully scored chunk first, got %v", resultIDs(got))
	}
}

func TestRerank_LimitsConcurrency(t *testing.T) {
	// 20 个候选开 20 个 goroutine 打 API 会触发限流，
	// 而限流后的重试反而更慢
	scorer := &fakeScorer{fixed: "5", delay: 50 * time.Millisecond}
	r, _ := NewLLMReranker(scorer, quiet())

	contents := make([]string, 20)
	for i := range contents {
		contents[i] = "内容" + string(rune('a'+i))
	}

	if _, err := r.Rerank(context.Background(), "查询",
		rerankCandidatesFrom(contents...), 5); err != nil {
		t.Fatalf("rerank: %v", err)
	}

	if peak := scorer.maxSeen.Load(); peak > rerankConcurrency {
		t.Errorf("concurrency peaked at %d, expected at most %d", peak, rerankConcurrency)
	}
}

func TestRerank_ScoresEveryCandidate(t *testing.T) {
	scorer := &fakeScorer{fixed: "5"}
	r, _ := NewLLMReranker(scorer, quiet())

	if _, err := r.Rerank(context.Background(), "查询",
		rerankCandidatesFrom("a", "b", "c", "d"), 2); err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if got := scorer.calls.Load(); got != 4 {
		t.Errorf("expected all 4 candidates scored, got %d calls", got)
	}
}

func TestRerank_AppliesTopK(t *testing.T) {
	scorer := &fakeScorer{fixed: "5"}
	r, _ := NewLLMReranker(scorer, quiet())

	got, err := r.Rerank(context.Background(), "查询",
		rerankCandidatesFrom("a", "b", "c", "d", "e"), 2)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 results, got %d", len(got))
	}
}

func TestRerank_TiesKeepRRFOrder(t *testing.T) {
	// LLM 打分粒度粗（0-10 整数），并列很常见。
	// 此时沿用 RRF 的次序比随机打乱更合理。
	scorer := &fakeScorer{fixed: "5"} // 全部同分
	r, _ := NewLLMReranker(scorer, quiet())

	candidates := rerankCandidatesFrom("first", "second", "third")
	got, err := r.Rerank(context.Background(), "查询", candidates, 3)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}

	for i, want := range []string{"first", "second", "third"} {
		if got[i].Chunk.ID != want {
			t.Errorf("tie-break changed the order: expected %v, got %v",
				[]string{"first", "second", "third"}, resultIDs(got))
			break
		}
		_ = i
	}
}

func TestRerank_PromptDemandsANumberOnly(t *testing.T) {
	// 「只输出数字」必须在 prompt 里强调，否则模型会写一段分析
	scorer := &fakeScorer{fixed: "5"}
	r, _ := NewLLMReranker(scorer, quiet())

	if _, err := r.Rerank(context.Background(), "我的查询",
		rerankCandidatesFrom("我的内容"), 1); err != nil {
		t.Fatalf("rerank: %v", err)
	}

	scorer.mu.Lock()
	prompt := scorer.prompts[0]
	scorer.mu.Unlock()

	for _, want := range []string{"只输出", "我的查询", "我的内容"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestRerank_TruncatesLongContent(t *testing.T) {
	// 判断相关性不需要读完整块，更短的 prompt 意味着更低的延迟和成本
	scorer := &fakeScorer{fixed: "5"}
	r, _ := NewLLMReranker(scorer, quiet())

	long := strings.Repeat("很长的内容", 500) // 2500 字符
	if _, err := r.Rerank(context.Background(), "查询",
		rerankCandidatesFrom(long), 1); err != nil {
		t.Fatalf("rerank: %v", err)
	}

	scorer.mu.Lock()
	prompt := scorer.prompts[0]
	scorer.mu.Unlock()

	if n := len([]rune(prompt)); n > contentLimit+400 {
		t.Errorf("prompt is %d runes, content does not appear truncated", n)
	}
}

func TestRerank_RespectsContextCancellation(t *testing.T) {
	scorer := &fakeScorer{fixed: "5", delay: 2 * time.Second}
	r, _ := NewLLMReranker(scorer, quiet())

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := r.Rerank(ctx, "查询", rerankCandidatesFrom("a", "b", "c"), 3)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("expected an error on cancellation")
	}
	if elapsed > time.Second {
		t.Errorf("cancellation was not honored: took %v", elapsed)
	}
}

func TestRerank_EmptyCandidates(t *testing.T) {
	scorer := &fakeScorer{fixed: "5"}
	r, _ := NewLLMReranker(scorer, quiet())

	got, err := r.Rerank(context.Background(), "查询", nil, 5)
	if err != nil {
		t.Fatalf("empty candidates should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no results, got %d", len(got))
	}
	if scorer.calls.Load() != 0 {
		t.Error("no candidates should mean no API calls")
	}
}

func TestNewLLMReranker_RequiresAModel(t *testing.T) {
	if _, err := NewLLMReranker(nil, quiet()); err == nil {
		t.Error("expected an error for a nil model")
	}
}
