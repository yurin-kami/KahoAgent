// Package retriever 实现检索召回与融合。
package retriever

import "sort"

// rrfK 是 RRF 的平滑常数。
//
// 60 是原论文（Cormack et al., 2009）的推荐值，社区广泛验证。
// ⚠️ 不准随意调整 —— 调了必须在评估集上验证确实更好，
// 并把对比数据记进 docs/eval-results.md。
const rrfK = 60

// Ranking 是单个检索器的召回结果，已按各自的相关性降序排列。
type Ranking struct {
	// Name 用于日志与评估时区分来源
	Name string
	// IDs 是 chunk ID 的有序列表，第 0 个最相关
	IDs []string
}

// FusedResult 是融合后的单条结果。
type FusedResult struct {
	ID    string
	Score float64
	// Sources 记录该 ID 出现在哪些检索器的结果里。
	// 排查时很有用：能一眼看出某条结果是双路印证还是单路独有。
	Sources []string
}

// FuseRRF 用 Reciprocal Rank Fusion 融合多路检索结果。
//
//	score(d) = Σ 1 / (k + rank_i(d))
//
// 核心思想：**只用排名，不用分数**。
// 向量检索返回余弦相似度（0~1），BM25 返回的分数可能是几十 ——
// 量纲完全不同，直接相加毫无意义。RRF 把两者都归约到排名，
// 从根本上绕开了归一化难题。
//
// 效果来源：
//   - 排名第 1 得 1/61 ≈ 0.0164，第 10 得 1/70 ≈ 0.0143，差距不大
//     → 容忍单个检索器的排序噪声
//   - 但在两路中都靠前的文档累加后明显更高
//     → 双重印证的结果被提升
//
// 返回按分数降序排列；分数相同时按 ID 升序，保证结果稳定可复现 ——
// 否则 map 遍历的随机性会让同样的输入产生不同的排序，评估数据就不可信了。
func FuseRRF(rankings []Ranking) []FusedResult {
	scores := make(map[string]float64)
	sources := make(map[string][]string)

	for _, r := range rankings {
		// 同一路内去重：某个 ID 重复出现时只按最靠前的排名计分，
		// 否则重复项会被累加多次，凭空拿到更高分数。
		seen := make(map[string]bool, len(r.IDs))
		for rank, id := range r.IDs {
			if seen[id] {
				continue
			}
			seen[id] = true

			// rank 从 0 开始，公式里排名从 1 开始，故 +1
			scores[id] += 1.0 / float64(rrfK+rank+1)
			sources[id] = append(sources[id], r.Name)
		}
	}

	out := make([]FusedResult, 0, len(scores))
	for id, score := range scores {
		out = append(out, FusedResult{ID: id, Score: score, Sources: sources[id]})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID // 分数相同时定序，保证可复现
	})
	return out
}
