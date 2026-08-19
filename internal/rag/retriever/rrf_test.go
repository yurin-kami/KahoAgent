package retriever

import (
	"math"
	"testing"
)

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s: expected %.6f, got %.6f", name, want, got)
	}
}

func TestFuseRRF_MatchesTheWorkedExample(t *testing.T) {
	// 直接复现 IMPLEMENTATION.md L2.3 的例子：
	//   向量检索: [A, B, C]     BM25: [C, A, D]
	//   A: 1/(60+1) + 1/(60+2) = 0.0325  ← 两边都靠前，最高
	//   C: 1/(60+3) + 1/(60+1) = 0.0323
	//   B: 1/(60+2)            = 0.0161
	//   D: 1/(60+3)            = 0.0159
	got := FuseRRF([]Ranking{
		{Name: "vector", IDs: []string{"A", "B", "C"}},
		{Name: "bm25", IDs: []string{"C", "A", "D"}},
	})

	if len(got) != 4 {
		t.Fatalf("expected 4 fused results, got %d", len(got))
	}

	// 顺序断言：A > C > B > D
	wantOrder := []string{"A", "C", "B", "D"}
	for i, want := range wantOrder {
		if got[i].ID != want {
			t.Errorf("position %d: expected %s, got %s (full: %+v)", i, want, got[i].ID, got)
		}
	}

	approx(t, "A", got[0].Score, 1.0/61+1.0/62)
	approx(t, "C", got[1].Score, 1.0/63+1.0/61)
	approx(t, "B", got[2].Score, 1.0/62)
	approx(t, "D", got[3].Score, 1.0/63)

	// A 和 C 的差距很小，但顺序必须确定
	if got[0].Score <= got[1].Score {
		t.Errorf("A should outrank C: %.6f vs %.6f", got[0].Score, got[1].Score)
	}
}

func TestFuseRRF_RewardsAgreementBetweenRetrievers(t *testing.T) {
	// RRF 的核心价值：两路都认可的结果应该胜过单路的第一名。
	// vector 把 X 排第 1，bm25 把 X 排第 5；Y 只在 vector 里排第 2。
	// X 虽然在 bm25 里靠后，但双路印证应让它稳居第一。
	got := FuseRRF([]Ranking{
		{Name: "vector", IDs: []string{"X", "Y", "a", "b", "c"}},
		{Name: "bm25", IDs: []string{"p", "q", "r", "s", "X"}},
	})

	if got[0].ID != "X" {
		t.Errorf("expected X first (confirmed by both retrievers), got %s: %+v", got[0].ID, got)
	}
	if len(got[0].Sources) != 2 {
		t.Errorf("expected X to cite 2 sources, got %v", got[0].Sources)
	}
}

func TestFuseRRF_ToleratesRankingNoise(t *testing.T) {
	// 排名相近时分数差距很小 —— 这正是 RRF 容忍单路排序噪声的原因。
	// 第 1 名 1/61≈0.01639，第 10 名 1/70≈0.01429，差距不到 15%。
	got := FuseRRF([]Ranking{
		{Name: "only", IDs: []string{"1st", "2nd", "3rd", "4th", "5th", "6th", "7th", "8th", "9th", "10th"}},
	})

	ratio := got[9].Score / got[0].Score
	if ratio < 0.8 {
		t.Errorf("rank 10 should score within 20%% of rank 1, ratio was %.3f", ratio)
	}
}

func TestFuseRRF_DeduplicatesWithinOneRanking(t *testing.T) {
	// 单路内重复的 ID 只按最靠前的排名计分，
	// 否则检索器返回重复项就能刷高分数
	got := FuseRRF([]Ranking{
		{Name: "buggy", IDs: []string{"A", "A", "A"}},
	})

	if len(got) != 1 {
		t.Fatalf("expected 1 unique result, got %d: %+v", len(got), got)
	}
	approx(t, "A scored once", got[0].Score, 1.0/61)
	if len(got[0].Sources) != 1 {
		t.Errorf("expected 1 source, got %v", got[0].Sources)
	}
}

func TestFuseRRF_IsDeterministic(t *testing.T) {
	// map 遍历顺序随机，若不显式定序，同样输入会产生不同排序，
	// 评估数据就不可复现了。这里用完全并列的分数逼出问题。
	rankings := []Ranking{
		{Name: "a", IDs: []string{"x", "y", "z"}},
		{Name: "b", IDs: []string{"z", "y", "x"}},
	}

	first := FuseRRF(rankings)
	for i := range 20 {
		got := FuseRRF(rankings)
		for j := range got {
			if got[j].ID != first[j].ID {
				t.Fatalf("run %d differs at position %d: %s vs %s", i, j, got[j].ID, first[j].ID)
			}
		}
	}
}

func TestFuseRRF_SingleRankingPreservesOrder(t *testing.T) {
	// 只有一路时，融合结果应与原顺序一致
	got := FuseRRF([]Ranking{
		{Name: "vector", IDs: []string{"a", "b", "c", "d"}},
	})

	for i, want := range []string{"a", "b", "c", "d"} {
		if got[i].ID != want {
			t.Errorf("position %d: expected %s, got %s", i, want, got[i].ID)
		}
	}
}

func TestFuseRRF_HandlesEmptyInput(t *testing.T) {
	if got := FuseRRF(nil); len(got) != 0 {
		t.Errorf("expected no results, got %+v", got)
	}
	// 一路为空（如 BM25 降级）不应影响另一路
	got := FuseRRF([]Ranking{
		{Name: "vector", IDs: []string{"a", "b"}},
		{Name: "bm25", IDs: nil},
	})
	if len(got) != 2 || got[0].ID != "a" {
		t.Errorf("expected the vector results to survive, got %+v", got)
	}
}

func TestFuseRRF_ThreeWayFusion(t *testing.T) {
	// 公式对任意路数都成立，不写死两路
	got := FuseRRF([]Ranking{
		{Name: "a", IDs: []string{"common", "x"}},
		{Name: "b", IDs: []string{"common", "y"}},
		{Name: "c", IDs: []string{"common", "z"}},
	})

	if got[0].ID != "common" {
		t.Errorf("expected the thrice-confirmed result first, got %s", got[0].ID)
	}
	approx(t, "common", got[0].Score, 3*(1.0/61))
	if len(got[0].Sources) != 3 {
		t.Errorf("expected 3 sources, got %v", got[0].Sources)
	}
}
