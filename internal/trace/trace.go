// Package trace 初始化 OpenTelemetry，并给 HTTP / 检索 / 入库三条链路提供埋点。
//
// 为什么用 OTel 而不是直接接某个厂商 SDK：埋点应该与后端解耦 ——
// 本地用 Jaeger，线上换 Tempo 或云厂商，业务代码一行都不用改。
//
// 设计上是「可关闭」的：默认 Enabled=false，此时全局 provider 保持
// OTel 的 noop 实现，所有 Start/End 都是空操作，几乎没有开销。
// 这样本地开发和单元测试完全不需要跑 collector。
package trace

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// scopeName 是 tracer 的 instrumentation scope。
//
// 用模块路径而不是 "kahoagent"：一个进程里可能同时有多个库的埋点，
// scope 是排查时区分「这个 span 是谁打的」的唯一依据。
const scopeName = "github.com/yurin-kami/KahoAgent"

// Config 是 OTel 的启动配置。
type Config struct {
	// Enabled 为 false 时 Setup 直接返回 noop，不建立任何连接。
	Enabled bool

	// Endpoint 是 OTLP/HTTP 接收端，形如 "127.0.0.1:4318"（不带 scheme）。
	// ⚠️ 带 http:// 前缀会导致 exporter 报 "invalid endpoint"，
	// 这是 OTLP exporter 最常见的配置错误。
	Endpoint string

	// Insecure 表示用明文 HTTP。本地 Jaeger 必须开；线上应关掉并用 TLS。
	Insecure bool

	ServiceName string

	// SampleRatio 是采样比例（0~1）。
	//
	// ⚠️ 不要在生产直接设 1.0：SSE 对话可能持续几十秒，
	// 全采样时 span 量与请求量线性相关，collector 很容易被打满。
	// 排查阶段临时调高即可。
	SampleRatio float64
}

// Shutdown 冲刷并关闭 provider。
//
// 必须调用：batcher 是异步的，不 Shutdown 会丢掉最后一批 span ——
// 而进程退出前的那几条往往正是排查崩溃时要看的。
type Shutdown func(context.Context) error

// Setup 初始化全局 TracerProvider 与传播器。
//
// 返回 error 的只有「创建 exporter 失败」这一种情况；
// 真正的连接失败是异步的（batcher 在后台重试），不会在这里暴露 ——
// 所以调用方不应该因为 Setup 成功就认为 collector 是通的。
func Setup(ctx context.Context, cfg Config) (Shutdown, error) {
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "kahoagent"
	}
	// 采样率非法时退回 1.0 而不是 0：配错采样率的意图通常是「想开」，
	// 静默变成全关会让人以为埋点没生效，排查方向直接跑偏。
	if cfg.SampleRatio <= 0 || cfg.SampleRatio > 1 {
		cfg.SampleRatio = 1.0
	}

	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, err
	}

	// 用 Schemaless 而不是 semconv 资源：避免绑定某个 semconv 版本，
	// 那些包在小版本升级时经常改常量名。
	res := resource.NewSchemaless(
		attribute.String("service.name", cfg.ServiceName),
		attribute.String("service.version", "dev"),
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			// 默认 5 秒一批。SSE 请求结束后如果进程恰好在这 5 秒内退出，
			// 这批 span 就丢了；2 秒是延迟与吞吐的折中。
			sdktrace.WithBatchTimeout(2*time.Second),
		),
		sdktrace.WithResource(res),
		// ParentBased：有上游 trace 时沿用上游的采样决定，
		// 否则按比例采 —— 这样跨服务调用不会出现「上游采了、下游没采」。
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tp.Shutdown, nil
}

// Tracer 返回本项目的 tracer。
//
// 取全局 provider：这样「是否开启」完全由 Setup 决定，
// 业务代码不需要把 tracer 一路传下去，也不会出现「某条路径忘了注入」。
func Tracer() trace.Tracer { return otel.Tracer(scopeName) }
