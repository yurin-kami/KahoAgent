// Package retriever 实现检索召回与融合。
package retriever

import (
	"cmp"
	"slices"
)

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
//
// ⚠️ 实现上刻意避开「多个 map」的写法，理由见 docs/optimization.md：
// pprof 显示原实现（scores/sources/seen 三个 map + 每 ID 一个 slice）
// 占了整条检索链路 43% 的分配量。现在只用一个 map 定位下标，
// 其余数据都落在预分配的 slice 里。
func FuseRRF(rankings []Ranking) []FusedResult {
	// 上界：所有路的 ID 总数。实际会因去重而少一些，
	// 但预分配到上界可以完全避免 append 过程中的多次扩容拷贝。
	total := 0
	for _, r := range rankings {
		total += len(r.IDs)
	}
	if total == 0 {
		return nil
	}

	out := make([]FusedResult, 0, total)
	index := make(map[string]int, total)
	// lastSeen[i] 记录 out[i] 最后一次被哪一路计过分。
	//
	// 用它代替「每路一个 seen map」：同一路内重复出现的 ID 只按最靠前的
	// 排名计分，否则重复项会被累加多次，凭空拿到更高分数。
	// 初值 -1 表示还没被任何一路计过。
	lastSeen := make([]int, 0, total)

	for ri, r := range rankings {
		for rank, id := range r.IDs {
			// rank 从 0 开始，公式里排名从 1 开始，故 +1
			score := 1.0 / float64(rrfK+rank+1)

			i, ok := index[id]
			if !ok {
				index[id] = len(out)
				out = append(out, FusedResult{
					ID:    id,
					Score: score,
					// 容量给路数：一个 ID 最多被每路各命中一次
					Sources: append(make([]string, 0, len(rankings)), r.Name),
				})
				lastSeen = append(lastSeen, ri)
				continue
			}
			if lastSeen[i] == ri {
				continue // 同一路内重复，跳过
			}
			lastSeen[i] = ri
			out[i].Score += score
			out[i].Sources = append(out[i].Sources, r.Name)
		}
	}

	slices.SortFunc(out, func(a, b FusedResult) int {
		if a.Score != b.Score {
			// 降序：分数高的在前
			if a.Score > b.Score {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.ID, b.ID) // 分数相同时定序，保证可复现
	})
	return out
}
