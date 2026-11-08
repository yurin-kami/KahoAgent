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

	// UploadTotal 统计上传接口的结果。
	//
	// result 的取值刻意分成 rejected / accepted 两类而不是 ok / error：
	// 用户传了个 10MB 的文件被拒，是**正常业务**而不是故障，
	// 混进 error 会让告警噪音掩盖真正的投递失败。
	UploadTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kaho_upload_total",
		Help: "文档上传结果，按拒绝原因区分",
	}, []string{"result"})

	// IngestTotal 统计 worker 侧的入库结果。
	//
	// status: ok / error / skipped（空文档、脏消息等不值得重试的情况）。
	// 与 UploadTotal 分开：上传成功不代表入库成功，两者中间隔着消息队列，
	// 只有分别看才能判断积压是「上游传不进来」还是「下游处理不掉」。
	IngestTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kaho_ingest_total",
		Help: "文档入库结果",
	}, []string{"status"})

	// IngestDuration 统计单篇文档从消费到写库完成的耗时。
	// 桶上限铺到 60s：一篇长文档要调 Embedding API，比检索慢两个数量级。
	IngestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kaho_ingest_duration_seconds",
		Help:    "单篇文档入库耗时",
		Buckets: []float64{.1, .25, .5, 1, 2.5, 5, 10, 30, 60},
	}, []string{"status"})
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

// kaho_upload_total 的 result 取值。
//
// 每个取值都对应一个**可执行的排查方向**：
//
//	accepted    正常
//	invalid     客户端参数错误 —— 不用管
//	too_large   客户端该分片或换格式 —— 产品决策
//	binary      可能有人在传 PDF/图片 —— 该扩展支持了
//	unavailable 投递失败 —— 需要告警，这是真正的故障
const (
	UploadAccepted    = "accepted"
	UploadInvalid     = "invalid"
	UploadTooLarge    = "too_large"
	UploadBinary      = "binary"
	UploadUnavailable = "unavailable"

	IngestOK      = "ok"
	IngestError   = "error"
	IngestSkipped = "skipped"
	// IngestDeadLetter 表示消息重试到上限后转投死信队列。
	// 单列一个状态而不是算进 error：error 会随网络抖动频繁波动，
	// 而死信只会在「真的处理不了」时增长，更适合做告警。
	IngestDeadLetter = "dead_letter"
)

// RecordUpload 记录一次上传结果。
func RecordUpload(result string) { UploadTotal.WithLabelValues(result).Inc() }

// RecordIngest 记录一次入库结果与耗时。
func RecordIngest(status string, seconds float64) {
	IngestTotal.WithLabelValues(status).Inc()
	IngestDuration.WithLabelValues(status).Observe(seconds)
}
