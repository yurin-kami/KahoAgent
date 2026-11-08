package trace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHTTPMiddleware_PreservesFlusher 是本包最重要的测试。
//
// 中间件包装 ResponseWriter 后如果丢了 http.Flusher，
// ChatHandler 的 `w.(http.Flusher)` 断言会失败，SSE 直接不可用。
// 这个 bug 的表现是「接口返回 500 + streaming is not supported」，
// 与 tracing 毫无关联，靠日志几乎不可能定位 —— 所以用测试锁死。
func TestHTTPMiddleware_PreservesFlusher(t *testing.T) {
	var (
		hadFlusher bool
		flushed    bool
	)
	h := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		hadFlusher = ok
		if ok {
			w.WriteHeader(http.StatusOK)
			f.Flush()
			flushed = true
		}
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/chat", nil))

	if !hadFlusher {
		t.Fatal("wrapped ResponseWriter lost http.Flusher — SSE would break")
	}
	if !flushed {
		t.Fatal("Flush was not forwarded to the underlying writer")
	}
}

// TestStatusRecorder_DefaultsTo200 验证 handler 不写状态码时的语义。
func TestStatusRecorder_DefaultsTo200(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	if rec.status != 0 {
		t.Fatalf("initial status = %d, want 0 (unset)", rec.status)
	}
	_, _ = rec.Write([]byte("ok"))
	if rec.status != http.StatusOK {
		t.Fatalf("status after Write = %d, want 200", rec.status)
	}
}

// TestSkipPath 锁住「高频端点不埋点」的规则。
func TestSkipPath(t *testing.T) {
	for _, p := range []string{"/healthz", "/metrics", "/debug/pprof/", "/debug/pprof/heap"} {
		if !skipPath(p) {
			t.Fatalf("skipPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"/api/chat", "/api/upload", "/"} {
		if skipPath(p) {
			t.Fatalf("skipPath(%q) = true, want false", p)
		}
	}
}

// TestSetup_DisabledIsNoop 验证关闭时不会去连 collector。
//
// 关闭态必须绝对安全：本地开发、单元测试都不该因为
// 「没有 Jaeger」而报错。
func TestSetup_DisabledIsNoop(t *testing.T) {
	shutdown, err := Setup(context.Background(), Config{Enabled: false})
	if err != nil {
		t.Fatalf("Setup(disabled): %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown must not be nil")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// noop provider 下 Tracer 也必须可用
	_, span := Tracer().Start(context.Background(), "test")
	span.End()
}
