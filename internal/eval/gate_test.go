package eval

import "testing"

func report(golden int, strategies ...StrategyMetrics) Report {
	return Report{GoldenSet: golden, TopK: 5, Strategies: strategies}
}

// TestCompare_NoRegression 验证容差内的波动不会让门禁变红。
//
// 这条测试对应的是「门禁的可用性」：容差小于上游 LLM 的非确定性时，
// CI 会随机失败，几次之后所有人都会无视它。
func TestCompare_NoRegression(t *testing.T) {
	base := report(78,
		StrategyMetrics{Name: "vector-only", RecallAt5: 0.314, MRR: 0.660},
		StrategyMetrics{Name: "hybrid-rrf+rerank", RecallAt5: 0.441, MRR: 0.818},
	)
	cur := report(78,
		// 都在 ±0.02 内
		StrategyMetrics{Name: "vector-only", RecallAt5: 0.301, MRR: 0.645},
		StrategyMetrics{Name: "hybrid-rrf+rerank", RecallAt5: 0.452, MRR: 0.826},
	)

	if v := Compare(base, cur, 0.02); len(v) != 0 {
		t.Fatalf("unexpected violations: %v", v)
	}
}

// TestCompare_ReportsRecallAndMRRDrops 验证两类质量指标都被检查。
func TestCompare_ReportsRecallAndMRRDrops(t *testing.T) {
	base := report(78,
		StrategyMetrics{Name: "hybrid-rrf+rerank", RecallAt5: 0.441, MRR: 0.818},
	)
	cur := report(78,
		StrategyMetrics{Name: "hybrid-rrf+rerank", RecallAt5: 0.400, MRR: 0.700},
	)

	v := Compare(base, cur, 0.02)
	if len(v) != 2 {
		t.Fatalf("violations = %d (%v), want 2", len(v), v)
	}
	// 排序保证 Recall@5 在前（字符串序 "MRR" < "Recall@5"？）
	// 按 Strategy 相同、Metric 字典序：M < R，所以 MRR 在前。
	if v[0].Metric != "MRR" || v[1].Metric != "Recall@5" {
		t.Fatalf("unexpected order: %v", v)
	}
}

// TestCompare_MissingStrategy 验证策略消失也会触发失败。
func TestCompare_MissingStrategy(t *testing.T) {
	base := report(78,
		StrategyMetrics{Name: "hybrid-rrf", RecallAt5: 0.358, MRR: 0.719},
		StrategyMetrics{Name: "hybrid-rrf+rerank", RecallAt5: 0.441, MRR: 0.818},
	)
	cur := report(78,
		StrategyMetrics{Name: "hybrid-rrf", RecallAt5: 0.358, MRR: 0.719},
	)

	v := Compare(base, cur, 0.02)
	if len(v) != 1 || v[0].Strategy != "hybrid-rrf+rerank" || v[0].Metric != "缺失" {
		t.Fatalf("unexpected violations: %v", v)
	}
}

// TestCompare_GoldenSetChanged 验证查询条数不一致时直接判失败。
//
// 这是最容易发生也最容易被忽略的误判：换了语料导致 golden set 从
// 78 条变成 60 条，指标「上升」了，但比较的其实是两个不同的题目集。
func TestCompare_GoldenSetChanged(t *testing.T) {
	base := report(78, StrategyMetrics{Name: "hybrid-rrf", RecallAt5: 0.358, MRR: 0.719})
	cur := report(60, StrategyMetrics{Name: "hybrid-rrf", RecallAt5: 0.500, MRR: 0.900})

	v := Compare(base, cur, 0.02)
	if len(v) != 1 || v[0].Strategy != "golden_set" {
		t.Fatalf("unexpected violations: %v", v)
	}
}

// TestCompare_NewStrategyIsFine 验证新增策略不算回退。
func TestCompare_NewStrategyIsFine(t *testing.T) {
	base := report(78, StrategyMetrics{Name: "vector-only", RecallAt5: 0.314, MRR: 0.660})
	cur := report(78,
		StrategyMetrics{Name: "vector-only", RecallAt5: 0.314, MRR: 0.660},
		StrategyMetrics{Name: "bm25-only", RecallAt5: 0.363, MRR: 0.723},
	)

	if v := Compare(base, cur, 0.02); len(v) != 0 {
		t.Fatalf("new strategy should not fail the gate: %v", v)
	}
}
