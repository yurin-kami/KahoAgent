package pipeline

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TestTraceHeaders_RoundTrip 验证 trace 上下文能跨 Kafka 消息传递。
//
// 这条链路是「上传请求」与「worker 入库」在 Jaeger 里连成一条 trace 的关键；
// 断了不会影响业务，但会让排查退化成靠时间戳手工拼。
func TestTraceHeaders_RoundTrip(t *testing.T) {
	// 用一个真实的 SDK provider：noop provider 不会产生 traceparent。
	prev := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	ctx, span := tp.Tracer("test").Start(context.Background(), "upload")
	defer span.End()

	headers := InjectTraceHeaders(ctx)
	if len(headers) == 0 {
		t.Fatal("no trace headers injected — traceparent missing")
	}
	// 只注入标准头，不塞业务字段
	for _, h := range headers {
		if h.Key != "traceparent" && h.Key != "tracestate" && h.Key != "baggage" {
			t.Fatalf("unexpected header %q", h.Key)
		}
	}

	got := ExtractTrace(context.Background(), headers)
	sc := trace.SpanContextFromContext(got)
	if !sc.IsValid() {
		t.Fatal("extracted span context is invalid")
	}
	if sc.TraceID() != span.SpanContext().TraceID() {
		t.Fatalf("trace id mismatch: got %s want %s", sc.TraceID(), span.SpanContext().TraceID())
	}
}

// TestTraceHeaders_NoSpan 验证没有活动 span 时不产生垃圾 header。
func TestTraceHeaders_NoSpan(t *testing.T) {
	if hs := InjectTraceHeaders(context.Background()); len(hs) != 0 {
		t.Fatalf("expected no headers without an active span, got %v", hs)
	}
}
