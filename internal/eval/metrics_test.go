package eval

import (
	"math"
	"testing"
)

// 用假数据验证公式 —— 指标算错会让所有优化判断失效，
// 「改完好像好点了」若源于公式 bug，整个 Phase 2 的结论都是错的。

func set(ids ...string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s: expected %.6f, got %.6f", name, want, got)
	}
}

func TestRecallAtK(t *testing.T) {
	cases := []struct {
		name      string
		retrieved []string
		relevant  map[string]bool
		k         int
		want      float64
	}{
		{
			name:      "全部命中",
			retrieved: []string{"a", "b", "c"},
			relevant:  set("a", "b"),
			k:         3,
			want:      1.0,
		},
		{
			name:      "命中一半",
			retrieved: []string{"a", "x", "y"},
			relevant:  set("a", "b"),
			k:         3,
			want:      0.5,
		},
		{
			name:      "K 截断掉了后面的命中",
			retrieved: []string{"x", "y", "a"},
			relevant:  set("a"),
			k:         2, // a 排第 3，被截断
			want:      0,
		},
		{
			name:      "相关文档数超过 K 时不可能达到 1.0",
			retrieved: []string{"a", "b", "c", "d"},
			relevant:  set("a", "b", "c", "d"),
			k:         2,
			want:      0.5, // 前 2 个命中 2 个，共 4 个相关
		},
		{
			name:      "全部未命中",
			retrieved: []string{"x", "y"},
			relevant:  set("a"),
			k:         5,
			want:      0,
		},
		{
			name:      "空结果",
			retrieved: nil,
			relevant:  set("a"),
			k:         5,
			want:      0,
		},
		{
			name:      "无相关文档时返回 0 而非除零",
			retrieved: []string{"a"},
			relevant:  set(),
			k:         5,
			want:      0,
		},
		{
			name:      "k 为 0",
			retrieved: []string{"a"},
			relevant:  set("a"),
			k:         0,
			want:      0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			approx(t, "Recall@K", RecallAtK(tc.retrieved, tc.relevant, tc.k), tc.want)
		})
	}
}

func TestRecallAtK_IgnoresDuplicates(t *testing.T) {
	// 检索器返回重复项不应能刷高召回率
	got := RecallAtK([]string{"a", "a", "a"}, set("a", "b"), 3)
	approx(t, "Recall@K with duplicates", got, 0.5) // 只算命中 1 个，共 2 个相关
}

func TestReciprocalRank(t *testing.T) {
	cases := []struct {
		name      string
		retrieved []string
		relevant  map[string]bool
		want      float64
	}{
		{"排第 1", []string{"a", "b"}, set("a"), 1.0},
		{"排第 2", []string{"x", "a"}, set("a"), 0.5},
		{"排第 3", []string{"x", "y", "a"}, set("a"), 1.0 / 3.0},
		{"未命中", []string{"x", "y"}, set("a"), 0},
		{"空结果", nil, set("a"), 0},
		{"只看第一个命中", []string{"x", "a", "b"}, set("a", "b"), 0.5},
		{"无相关文档", []string{"a"}, set(), 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			approx(t, "RR", ReciprocalRank(tc.retrieved, tc.relevant), tc.want)
		})
	}
}

func TestMRR_MatchesTheWorkedExample(t *testing.T) {
	// 直接复现 IMPLEMENTATION.md L2.4 里的例子：
	//   Query1: 正确答案排第 1 → RR = 1.0
	//   Query2: 正确答案排第 3 → RR = 0.33
	//   Query3: 前 10 都没有   → RR = 0
	//   MRR = (1.0 + 0.33 + 0) / 3 = 0.44
	rrs := []float64{
		ReciprocalRank([]string{"a", "x", "y"}, set("a")),
		ReciprocalRank([]string{"x", "y", "a"}, set("a")),
		ReciprocalRank([]string{"x", "y", "z"}, set("a")),
	}

	approx(t, "RR1", rrs[0], 1.0)
	approx(t, "RR2", rrs[1], 1.0/3.0)
	approx(t, "RR3", rrs[2], 0)

	got := MRR(rrs)
	want := (1.0 + 1.0/3.0 + 0) / 3.0
	approx(t, "MRR", got, want)

	// 文档里写的是约 0.44
	if math.Abs(got-0.444444) > 0.001 {
		t.Errorf("MRR should be ~0.444, got %.6f", got)
	}
}

func TestMRR_EmptyInput(t *testing.T) {
	approx(t, "MRR of nothing", MRR(nil), 0)
}

func TestPercentile(t *testing.T) {
	// 1..100，用最近秩法：P99 = 第 ceil(0.99*100)=99 个 = 99
	samples := make([]float64, 100)
	for i := range samples {
		samples[i] = float64(i + 1)
	}

	cases := []struct {
		p    float64
		want float64
	}{
		{50, 50},
		{95, 95},
		{99, 99},
		{100, 100},
		{0, 1},
	}
	for _, tc := range cases {
		if got := Percentile(samples, tc.p); got != tc.want {
			t.Errorf("P%.0f: expected %.0f, got %.0f", tc.p, tc.want, got)
		}
	}
}

func TestPercentile_ReturnsAnObservedValue(t *testing.T) {
	// 最近秩法保证返回值是真实观测过的样本，不是插值出来的虚构值。
	// 报告 P99=180ms 时，应当确实有过一次 180ms 的请求。
	samples := []float64{10, 20, 500}
	got := Percentile(samples, 99)

	observed := false
	for _, s := range samples {
		if s == got {
			observed = true
		}
	}
	if !observed {
		t.Errorf("P99 returned %.1f, which is not one of the observed samples %v", got, samples)
	}
}

func TestPercentile_DoesNotMutateInput(t *testing.T) {
	// 调用方通常还要用同一份样本算别的百分位
	samples := []float64{30, 10, 20}
	Percentile(samples, 50)

	if samples[0] != 30 || samples[1] != 10 || samples[2] != 20 {
		t.Errorf("input was sorted in place: %v", samples)
	}
}

func TestPercentile_EdgeCases(t *testing.T) {
	if got := Percentile(nil, 99); got != 0 {
		t.Errorf("empty samples: expected 0, got %v", got)
	}
	if got := Percentile([]float64{42}, 99); got != 42 {
		t.Errorf("single sample: expected 42, got %v", got)
	}
	// 越界的 p 应被夹紧而非 panic
	if got := Percentile([]float64{1, 2, 3}, 150); got != 3 {
		t.Errorf("p>100 should clamp to max, got %v", got)
	}
	if got := Percentile([]float64{1, 2, 3}, -5); got != 1 {
		t.Errorf("p<0 should clamp to min, got %v", got)
	}
}

func TestMean(t *testing.T) {
	approx(t, "mean", Mean([]float64{1, 2, 3, 4}), 2.5)
	approx(t, "mean of nothing", Mean(nil), 0)
}
