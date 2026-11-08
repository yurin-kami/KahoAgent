package pipeline

import (
	"context"
	"sort"

	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// InjectTraceHeaders 把当前 span 的 W3C trace 上下文写进 Kafka 消息头。
//
// 为什么需要它：上传接口的 trace 与 worker 的入库 trace 本来是两条
// 互不相关的记录 —— 出了问题只能靠 doc_id 和时间戳手工拼。
// 注入 traceparent 之后，Jaeger 里看到的是：
//
//	POST /api/upload
//	└── kafka.consume
//	    └── ingest.document
//
// 一条完整的链路。
//
// 没有活动 span 时返回 nil：不往消息里塞空 header，
// 也避免为一条没有 trace 的请求伪造上下文。
func InjectTraceHeaders(ctx context.Context) []kafka.Header {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) == 0 {
		// 全局传播器可能仍是 noop（trace.Setup 没被调用，或本次没开追踪），
		// 但上下文里可能已经有 span（比如测试、或调用方自己建的）。
		// 用 W3C 标准实现兜底，避免「明明有 trace 却静默丢 header」。
		// 没有有效 span 时 TraceContext.Inject 什么也不写，不会产生垃圾头。
		propagation.TraceContext{}.Inject(ctx, carrier)
	}
	if len(carrier) == 0 {
		return nil
	}
	headers := make([]kafka.Header, 0, len(carrier))
	for k, v := range carrier {
		headers = append(headers, kafka.Header{Key: k, Value: []byte(v)})
	}
	// map 遍历顺序随机；排序让消息头稳定，便于测试与抓包对比。
	sort.Slice(headers, func(i, j int) bool { return headers[i].Key < headers[j].Key })
	return headers
}

// ExtractTrace 从 Kafka 消息头还原 trace 上下文。
//
// 还原失败（header 缺失/损坏）时返回原 ctx —— 埋点出问题不能影响业务。
func ExtractTrace(ctx context.Context, headers []kafka.Header) context.Context {
	if len(headers) == 0 {
		return ctx
	}
	carrier := propagation.MapCarrier{}
	for _, h := range headers {
		carrier[h.Key] = string(h.Value)
	}
	ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	if !trace.SpanContextFromContext(ctx).IsValid() {
		// 与 Inject 侧同理：全局传播器可能是 noop，
		// 这时用 W3C 标准实现兜底，避免「header 写了却读不出来」。
		ctx = propagation.TraceContext{}.Extract(ctx, carrier)
	}
	return ctx
}
