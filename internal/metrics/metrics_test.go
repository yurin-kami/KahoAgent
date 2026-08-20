package metrics

import (
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestObserveRetrieval_RecordsPerMethod(t *testing.T) {
	RetrievalDuration.Reset()

	ObserveRetrieval("vector", 0.135)
	ObserveRetrieval("bm25", 0.004)
	ObserveRetrieval("bm25", 0.006)

	if got := testutil.CollectAndCount(RetrievalDuration); got != 2 {
		t.Errorf("expected 2 series (one per method), got %d", got)
	}
}

func TestRecordCache_SeparatesHitAndMiss(t *testing.T) {
	CacheOps.Reset()

	RecordCache("retrieval", true)
	RecordCache("retrieval", true)
	RecordCache("retrieval", false)

	// 命中率靠 hit/(hit+miss) 现算，所以两个方向都必须被记下来
	hits := testutil.ToFloat64(CacheOps.WithLabelValues("retrieval", ResultHit))
	misses := testutil.ToFloat64(CacheOps.WithLabelValues("retrieval", ResultMiss))

	if hits != 2 {
		t.Errorf("expected 2 hits, got %v", hits)
	}
	if misses != 1 {
		t.Errorf("expected 1 miss, got %v", misses)
	}
}

func TestObserveToolCall_SeparatesStatus(t *testing.T) {
	// 失败的调用通常快得多（参数校验就返回了）。
	// 混进成功的一起算 P99，会让延迟看起来虚低。
	ToolCallDuration.Reset()

	ObserveToolCall("rag_query", 1.4, nil)
	ObserveToolCall("rag_query", 0.001, errors.New("boom"))

	if got := testutil.CollectAndCount(ToolCallDuration); got != 2 {
		t.Errorf("expected separate ok/error series, got %d", got)
	}
}

func TestRecordTokens_SkipsZeros(t *testing.T) {
	LLMTokens.Reset()

	RecordTokens("qwen-plus", 100, 50)
	RecordTokens("qwen-plus", 0, 0) // 不该产生新的时间序列

	prompt := testutil.ToFloat64(LLMTokens.WithLabelValues(TokenPrompt, "qwen-plus"))
	completion := testutil.ToFloat64(LLMTokens.WithLabelValues(TokenCompletion, "qwen-plus"))

	if prompt != 100 || completion != 50 {
		t.Errorf("expected 100/50 tokens, got %v/%v", prompt, completion)
	}
}

func TestLatencyBuckets_CoverRerankLatency(t *testing.T) {
	// ⚠️ Phase 2 实测精排 P99 是 1.9s。若桶只铺到 1s，
	// 精排的延迟全落进 +Inf 桶，P99 就只能算出"大于最大边界"——
	// 等于没测。这个断言防止将来有人"顺手"把桶改小。
	var max float64
	for _, b := range latencyBuckets {
		if b > max {
			max = b
		}
	}
	if max < 2.0 {
		t.Errorf("the largest bucket is %v, which cannot resolve the measured "+
			"rerank P99 of ~1.9s", max)
	}
}

func TestMetrics_AreRegisteredAndExportable(t *testing.T) {
	// 指标必须能被 Prometheus 采集器抓出来 ——
	// promauto 注册失败会 panic，但名字写错不会
	expected := []string{
		"kaho_retrieval_duration_seconds",
		"kaho_llm_tokens_total",
		"kaho_cache_ops_total",
		"kaho_tool_call_duration_seconds",
		"kaho_active_sessions",
	}

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	got := map[string]bool{}
	for _, f := range families {
		got[f.GetName()] = true
	}

	// 先产生一些数据，否则没有样本的指标不会出现在 Gather 结果里
	ObserveRetrieval("vector", 0.1)
	RecordTokens("m", 1, 1)
	RecordCache("retrieval", true)
	ObserveToolCall("t", 0.1, nil)
	ActiveSessions.Set(0)

	families, err = prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	got = map[string]bool{}
	for _, f := range families {
		got[f.GetName()] = true
	}

	for _, name := range expected {
		if !got[name] {
			t.Errorf("metric %q is not exported", name)
		}
	}
}

func TestMetricNames_UseTheKahoPrefix(t *testing.T) {
	// 统一前缀便于在 Prometheus 里一次筛出本项目的全部指标
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		name := f.GetName()
		if strings.HasPrefix(name, "kaho") && !strings.HasPrefix(name, "kaho_") {
			t.Errorf("metric %q should use the kaho_ prefix", name)
		}
	}
}
