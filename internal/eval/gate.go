package eval

import (
	"fmt"
	"math"
	"sort"
)

// StrategyMetrics 是某一种检索策略在一轮评估中的结果。
//
// 字段名与 docs/eval-baseline.json 一一对应，改这里必须同步改 JSON，
// 否则基线会静默地读成零值 —— 而零值基线会让一切检查都通过。
type StrategyMetrics struct {
	Name      string  `json:"name"`
	RecallAt5 float64 `json:"recall_at_5"`
	MRR       float64 `json:"mrr"`
	P50Ms     float64 `json:"p50_ms"`
	P99Ms     float64 `json:"p99_ms"`
}

// Report 是一轮评估的完整快照。
type Report struct {
	// GoldenSet 记录评估用了多少条查询。
	// 条数不一致时指标不可比 —— 78 条和 20 条的 MRR 放在一起没有意义，
	// 所以 Compare 会把它当成一条独立的违规。
	GoldenSet   int               `json:"golden_set"`
	TopK        int               `json:"top_k"`
	GeneratedAt string            `json:"generated_at,omitempty"`
	Strategies  []StrategyMetrics `json:"strategies"`
}

// Violation 描述一处质量回退。
type Violation struct {
	Strategy string
	Metric   string
	Baseline float64
	Current  float64
}

func (v Violation) String() string {
	return fmt.Sprintf("%s 的 %s 从 %.3f 掉到 %.3f（Δ%+.3f）",
		v.Strategy, v.Metric, v.Baseline, v.Current, v.Current-v.Baseline)
}

// Compare 把当前结果与基线比较，返回所有超过容差的回退。
//
// 为什么用**绝对差**而不是相对百分比：
// MRR 从 0.80 掉到 0.76 是 5% 的相对跌幅，但从 0.82 掉到 0.78 也是 5% ——
// 前者意味着「排第一的答案掉到第二」，后者在 Recall 上几乎无感。
// 检索指标的绝对值本身有语义（0.818 就是 81.8% 的查询首条命中），
// 用绝对阈值更贴近「用户能感知到多少」。
//
// 为什么容差默认 0.02：LLM 精排带 temperature=0 仍有非确定性
// （上游批处理、模型版本微调），实测同一配置重跑会在 ±0.01 内波动。
// 容差必须大于这个噪声，否则门禁会因为随机波动天天误报，
// 最后所有人都学会「红了就重跑一次」—— 那样的门禁等于没有。
func Compare(baseline, current Report, tolerance float64) []Violation {
	var out []Violation

	// 查询集合变了，指标就不可比。这条必须最先报，否则后面的
	// 数字比较都是在错误的前提下进行的。
	if baseline.GoldenSet != 0 && current.GoldenSet != 0 &&
		baseline.GoldenSet != current.GoldenSet {
		out = append(out, Violation{
			Strategy: "golden_set",
			Metric:   "查询条数",
			Baseline: float64(baseline.GoldenSet),
			Current:  float64(current.GoldenSet),
		})
	}

	cur := make(map[string]StrategyMetrics, len(current.Strategies))
	for _, s := range current.Strategies {
		cur[s.Name] = s
	}

	for _, b := range baseline.Strategies {
		c, ok := cur[b.Name]
		if !ok {
			// 基线里的策略消失了，本身就是一种回退：
			// 要么评估脚本删了它，要么实现坏了跑不出来。
			out = append(out, Violation{
				Strategy: b.Name, Metric: "缺失",
				Baseline: 1, Current: 0,
			})
			continue
		}
		if c.RecallAt5 < b.RecallAt5-tolerance {
			out = append(out, Violation{
				Strategy: b.Name, Metric: "Recall@5",
				Baseline: b.RecallAt5, Current: c.RecallAt5,
			})
		}
		if c.MRR < b.MRR-tolerance {
			out = append(out, Violation{
				Strategy: b.Name, Metric: "MRR",
				Baseline: b.MRR, Current: c.MRR,
			})
		}
	}

	// 固定顺序输出：map 遍历是随机的，日志/CI 输出每次不一样会让人怀疑
	// 是不是又跑了不同的东西。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Strategy != out[j].Strategy {
			return out[i].Strategy < out[j].Strategy
		}
		return out[i].Metric < out[j].Metric
	})
	return out
}

// Round 把浮点数截到 3 位，用于日志与报告。
//
// 不用 fmt.Sprintf("%.3f") 是因为 0.4414999 会被截成 0.441，
// 而 0.4415001 变成 0.442 —— 展示层不需要这种精度差异，统一四舍五入。
func Round(v float64) float64 {
	return math.Round(v*1000) / 1000
}
