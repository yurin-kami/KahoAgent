package retriever

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"sync"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

// Scorer 是打分用的最小 LLM 接口。
// 抽成接口而非直接依赖具体客户端，是为了让精排逻辑能用假模型测试 ——
// 否则每跑一次测试都要真调 API，既慢又花钱。
type Scorer interface {
	Score(ctx context.Context, prompt string) (string, error)
}

const (
	// rerankConcurrency 限制同时打到 LLM 的请求数。
	// 20 个候选不能开 20 个 goroutine 打 API —— 会触发限流，
	// 而限流后的重试反而更慢。
	rerankConcurrency = 5

	// maxScore 是打分上限，与 prompt 里的说明保持一致。
	maxScore = 10

	// contentLimit 截断送去打分的内容长度。
	// 判断相关性不需要读完整块 —— 开头几百字足以判断主题是否对得上，
	// 而更短的 prompt 意味着更低的延迟和成本。
	contentLimit = 500
)

// LLMReranker 用 LLM 对候选块做 pointwise 打分。
//
// 为什么用 pointwise 而非 listwise：
//   - pointwise：每块单独打分，可并发，prompt 短
//   - listwise：所有块一起排序，prompt 长易超限，
//     且模型对长列表的排序稳定性很差
//
// 为什么需要精排：向量检索和 BM25 都是「快而粗」的召回，
// 它们判断相关性靠的是表面特征（向量距离、词频）。
// LLM 能真正「读懂」问题和内容的关系，代价是慢两个数量级 ——
// 所以只对 RRF 筛出的前 20 条做，而非全部候选。
type LLMReranker struct {
	model  Scorer
	logger *slog.Logger
}

func NewLLMReranker(model Scorer, logger *slog.Logger) (*LLMReranker, error) {
	if model == nil {
		return nil, fmt.Errorf("model must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &LLMReranker{model: model, logger: logger}, nil
}

// scorePattern 从模型输出里抽第一个数字。
//
// 即便 prompt 强调「只输出数字」，模型仍可能回「8分」「Score: 8」
// 甚至带上一句解释。与其指望模型完全听话，不如在解析侧宽容一些。
var scorePattern = regexp.MustCompile(`\d+(?:\.\d+)?`)

// Rerank 对候选重新打分排序，返回前 topK 个。
func (r *LLMReranker) Rerank(ctx context.Context, query string, candidates []rag.ScoredChunk, topK int) ([]rag.ScoredChunk, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	if topK <= 0 {
		return nil, fmt.Errorf("topK must be > 0, got %d", topK)
	}

	scored := make([]rag.ScoredChunk, len(candidates))
	copy(scored, candidates)

	// ⚠️ 打分成功与失败的块必须落在**同一量纲**上。
	//
	// 成功的块会被归一化到 0-1，而 RRF 分数在 0.016 量级 ——
	// 若失败的块直接沿用 RRF 分数，它就等价于 0.016 分，
	// 会被排到所有打过分的块后面，与「让它仍能参与排序」的意图相反。
	// 一次 API 抖动不应该让一个可能高度相关的块被判死刑。
	//
	// 这里改为：失败的块取**成功块分数的中位数**，即「未知 ≈ 平均水平」。
	// 再由下面的 sort.SliceStable 在并列时保持 RRF 原序，
	// 于是失败块之间仍按 RRF 的偏好排列。
	ok := make([]bool, len(scored))

	// 用带缓冲的 channel 做信号量限制并发
	sem := make(chan struct{}, rerankConcurrency)
	var wg sync.WaitGroup

	for i := range scored {
		wg.Add(1)
		go func() {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			raw, err := r.model.Score(ctx, buildPrompt(query, scored[i].Chunk.Content))
			if err != nil {
				// 单块打分失败不让整次精排失败，分数留给下面统一兜底
				r.logger.Warn("rerank scoring failed, will fall back to the median score",
					"chunk", scored[i].Chunk.ID, "err", err)
				return
			}

			score, parsed := parseScore(raw)
			if !parsed {
				r.logger.Warn("could not parse a score, will fall back to the median score",
					"chunk", scored[i].Chunk.ID, "output", truncate(raw, 80))
				return
			}
			scored[i].Score = score
			ok[i] = true
		}()
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 全部失败时报错，让调用方降级回 RRF 顺序 ——
	// 此时没有任何新信息，强行排序只是把 RRF 的结果打乱。
	var succeeded []float64
	for i := range scored {
		if ok[i] {
			succeeded = append(succeeded, float64(scored[i].Score))
		}
	}
	if len(succeeded) == 0 {
		return nil, fmt.Errorf("all %d candidates failed to score", len(scored))
	}

	fallback := float32(median(succeeded))
	for i := range scored {
		if !ok[i] {
			scored[i].Score = fallback
		}
	}

	// 稳定排序：分数相同时保持 RRF 的相对顺序。
	// LLM 打分粒度粗（0-10 的整数），并列很常见，
	// 此时沿用 RRF 的次序比随机打乱更合理。
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	if len(scored) > topK {
		scored = scored[:topK]
	}
	return scored, nil
}

// buildPrompt 构造打分用的 prompt。
//
// 两个关键点：
//  1. 「只输出数字」必须强调 —— 否则模型会写一大段分析，
//     既浪费 token 又要额外解析
//  2. 给出评分档位的语义，否则不同调用之间的标准会漂移
func buildPrompt(query, content string) string {
	return fmt.Sprintf(`判断以下文档片段与问题的相关性，输出 0-%d 的整数分数。

评分标准：
- 9-10：直接回答了问题
- 6-8：包含相关信息但不完整
- 3-5：主题相关但没有回答问题
- 0-2：不相关

只输出一个数字，不要解释。

问题：%s

片段：%s`, maxScore, query, truncate(content, contentLimit))
}

// parseScore 从模型输出中解析分数并归一化到 0-1。
//
// 归一化的原因：RRF 分数在 0.016 量级，LLM 分数在 0-10 量级。
// 若不归一化，两者混在同一个 Score 字段里会让下游无从判断量纲 ——
// 而精排失败的块保留的正是 RRF 分数。统一到 0-1 后至少可比。
func parseScore(raw string) (float32, bool) {
	match := scorePattern.FindString(raw)
	if match == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(match, 32)
	if err != nil {
		return 0, false
	}
	// 模型偶尔会输出超出范围的数字，夹紧而非拒绝
	if v < 0 {
		v = 0
	}
	if v > maxScore {
		v = maxScore
	}
	return float32(v / maxScore), true
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// median 返回中位数。会就地排序，调用方传的是临时切片。
func median(v []float64) float64 {
	sort.Float64s(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}
