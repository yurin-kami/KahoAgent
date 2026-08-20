// Package metrics 定义全项目的 Prometheus 指标。
//
// 只埋**能驱动决策**的指标。埋得太多有两个代价：一是每条时间序列都
// 要常驻内存，标签一旦带上高基数字段（query、session_id、file_id）
// 会直接把内存吃爆；二是指标多到没人看的时候，等于没有监控。
//
// 类型的选择（这决定了能问出什么问题）：
//
//	Counter   只增不减 —— token 消耗、缓存命中次数
//	Gauge     可增可减 —— 当前活跃会话数
//	Histogram 分布统计 —— 各类延迟
//
// ⚠️ 延迟必须用 Histogram 而不是 Gauge。
// 平均 50ms 可能是 99 次 10ms 加 1 次 4 秒 —— 而那 1 次正是用户投诉的
// 来源。只有分位数能暴露它，而分位数需要分桶数据才算得出来。
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// latencyBuckets 覆盖本项目实测的延迟范围。
//
// 边界不是随便取的，取自 Phase 2 的实测数据：
//
//	bm25-only       P50 4ms    P99 32ms
//	vector-only     P50 135ms  P99 277ms
//	hybrid+rerank   P50 1.43s  P99 1.9s
//
// 所以桶必须一路铺到 2.5s，否则精排的延迟全落进 +Inf 桶，
// P99 就只能算出个"大于最大边界"，等于没测。
var latencyBuckets = []float64{
	.005, .01, .025, .05, .1, .25, .5, 1, 1.5, 2, 2.5, 5,
}

var (
	// RetrievalDuration 按检索方式统计耗时。
	// method: vector / bm25 / hybrid / rerank
	RetrievalDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kaho_retrieval_duration_seconds",
		Help:    "检索耗时，按检索方式区分",
		Buckets: latencyBuckets,
	}, []string{"method"})

	// LLMTokens 累计 token 消耗。
	// ⚠️ 这是**花钱**的指标，也是唯一能事后核对账单的依据。
	// type: prompt / completion
	LLMTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kaho_llm_tokens_total",
		Help: "LLM token 消耗累计",
	}, []string{"type", "model"})

	// CacheOps 统计缓存命中与否。
	// 命中率 = hit / (hit + miss)，用 PromQL 现算，
	// 不单独埋一个"命中率"指标 —— 那样没法按时间窗口重新聚合。
	// result: hit / miss
	CacheOps = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kaho_cache_ops_total",
		Help: "缓存操作次数，按命中结果区分",
	}, []string{"cache", "result"})

	// ToolCallDuration 统计工具调用耗时。
	// status: ok / error —— 失败的调用通常快得多（参数校验就返回了），
	// 混在一起算 P99 会让延迟看起来虚低。
	ToolCallDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kaho_tool_call_duration_seconds",
		Help:    "工具调用耗时，按工具与结果区分",
		Buckets: latencyBuckets,
	}, []string{"tool", "status"})

	// ActiveSessions 是当前进行中的对话数。
	// 用 Gauge：它会随请求开始/结束增减。
	ActiveSessions = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "kaho_active_sessions",
		Help: "当前活跃的对话数",
	})
)

// 结果标签的取值，集中定义避免各处手写字符串拼错 ——
// 拼错了不会报错，只会安静地多出一条时间序列。
const (
	ResultHit  = "hit"
	ResultMiss = "miss"

	StatusOK    = "ok"
	StatusError = "error"

	TokenPrompt     = "prompt"
	TokenCompletion = "completion"
)

// ObserveRetrieval 记录一次检索耗时（秒）。
func ObserveRetrieval(method string, seconds float64) {
	RetrievalDuration.WithLabelValues(method).Observe(seconds)
}

// ObserveToolCall 记录一次工具调用。
func ObserveToolCall(tool string, seconds float64, err error) {
	status := StatusOK
	if err != nil {
		status = StatusError
	}
	ToolCallDuration.WithLabelValues(tool, status).Observe(seconds)
}

// RecordCache 记录一次缓存读取结果。
func RecordCache(cache string, hit bool) {
	result := ResultMiss
	if hit {
		result = ResultHit
	}
	CacheOps.WithLabelValues(cache, result).Inc()
}

// RecordTokens 累加 token 消耗。
func RecordTokens(model string, prompt, completion int) {
	if prompt > 0 {
		LLMTokens.WithLabelValues(TokenPrompt, model).Add(float64(prompt))
	}
	if completion > 0 {
		LLMTokens.WithLabelValues(TokenCompletion, model).Add(float64(completion))
	}
}
