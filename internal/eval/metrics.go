// Package eval 提供检索质量的评估指标。
//
// 独立成包而非写在 scripts/eval/eval.go 里，是为了能写单元测试：
// 指标算错了会让所有优化判断失效 —— 「改完好像好点了」若是因为公式有 bug，
// 那整个 Phase 2 的结论都是错的。所以公式必须先用假数据验证。
package eval

import "sort"

// RecallAtK 计算前 K 个结果中命中的相关文档比例。
//
//	Recall@K = 前 K 个结果中的相关文档数 / 相关文档总数
//
// 回答的是「该找的找到了多少」。
//
// 注意：当相关文档数 > K 时，Recall@K 不可能达到 1.0。这是指标的固有性质，
// 不是 bug —— 前 5 个位置装不下 8 个相关文档。
func RecallAtK(retrieved []string, relevant map[string]bool, k int) float64 {
	if len(relevant) == 0 || k <= 0 {
		return 0
	}

	// 用 set 去重：同一个 chunk 在结果里出现两次只能算一次命中，
	// 否则检索器返回重复项就能刷高分数。
	seen := make(map[string]bool, k)
	hits := 0
	for i, id := range retrieved {
		if i >= k {
			break
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if relevant[id] {
			hits++
		}
	}
	return float64(hits) / float64(len(relevant))
}

// ReciprocalRank 返回第一个命中结果的排名倒数。
//
//	RR = 1 / 第一个相关结果的排名（排名从 1 开始）
//
// 全部未命中返回 0。
func ReciprocalRank(retrieved []string, relevant map[string]bool) float64 {
	if len(relevant) == 0 {
		return 0
	}
	for i, id := range retrieved {
		if relevant[id] {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

// MRR 是所有查询 ReciprocalRank 的平均值。
//
// 为什么 RAG 场景 MRR 比 Recall 更重要：LLM 通常只看 top 3-5 个块，
// 排第 20 的正确答案等于没有 —— Recall 会把它算作命中，MRR 不会。
func MRR(rrs []float64) float64 {
	if len(rrs) == 0 {
		return 0
	}
	var sum float64
	for _, rr := range rrs {
		sum += rr
	}
	return sum / float64(len(rrs))
}

// Percentile 返回一组延迟样本的第 p 百分位（p 取 0-100）。
//
// 用最近秩法（nearest-rank）：P99 取的是「第 ceil(99% × N) 个样本」，
// 保证返回值一定是真实观测到的某个数，而非插值出来的虚构值。
// 报告延迟时这一点很重要 —— P99=180ms 应当意味着确实有过 180ms 的请求。
func Percentile(samples []float64, p float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	if p <= 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}

	// 复制后排序，避免修改调用方的切片 ——
	// 调用方通常还要用同一份样本算别的百分位。
	s := make([]float64, len(samples))
	copy(s, samples)
	sort.Float64s(s)

	// rank = ceil(p/100 × N)，转成 0-based 下标
	rank := int((p/100)*float64(len(s)) + 0.999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(s) {
		rank = len(s)
	}
	return s[rank-1]
}

// Mean 返回平均值，空切片返回 0。
func Mean(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, v := range samples {
		sum += v
	}
	return sum / float64(len(samples))
}
