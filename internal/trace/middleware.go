package trace

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// HTTPMiddleware 为每个请求创建 server span。
//
// ⚠️ 这个中间件最危险的地方不是埋点，而是**包装 ResponseWriter**。
// 包装之后原类型丢了 `http.Flusher`，而 ChatHandler 在写响应前会
// 断言 `w.(http.Flusher)` —— 断言失败就直接返回 500「streaming is not
// supported」。也就是说：一个漏写 Flush 的中间件会让 SSE 整体不可用，
// 且错误信息指向的是「连接不支持流式」，跟 tracing 毫无关系，
// 排查成本极高。statusRecorder 因此显式实现了 Flush。
func HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 健康检查、指标抓取、pprof 不埋点：
		// 它们频率高、无业务价值，采进来只会把 span 总量撑大，
		// 把真正要看的对话/检索 span 挤出采样窗口。
		if skipPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		ctx, span := Tracer().Start(r.Context(),
			r.Method+" "+r.URL.Path,
			oteltrace.WithSpanKind(oteltrace.SpanKindServer),
		)
		defer span.End()

		span.SetAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("url.path", r.URL.Path),
		)

		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r.WithContext(ctx))

		status := rec.status
		if status == 0 {
			// handler 一个字节都没写就返回了，按 Go 的默认语义是 200
			status = http.StatusOK
		}
		span.SetAttributes(attribute.Int("http.response.status_code", status))
		if status >= 500 {
			// 4xx 不标 Error：那是客户端传错了参数，不是服务出了问题。
			// 把 4xx 标成 Error 会让错误率指标失去意义。
			span.SetStatus(codes.Error, http.StatusText(status))
		}
	})
}

func skipPath(p string) bool {
	switch p {
	case "/healthz", "/metrics":
		return true
	}
	return strings.HasPrefix(p, "/debug/pprof")
}

// statusRecorder 记录状态码，并转发 Flusher。
//
// 只实现 WriteHeader/Write/Flush 三个方法：多实现没用到的接口
// （Hijacker、Pusher）反而危险 —— 上游会据此误判连接能力。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Flush 转发到底层 writer。
//
// 底层不支持 Flush 时静默忽略，而不是 panic：
// 中间件不应该假设每个 ResponseWriter 都可流式。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController（Go 1.20+）能取到底层 writer。
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
