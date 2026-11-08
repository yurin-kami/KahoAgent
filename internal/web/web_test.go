package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newHandler(t *testing.T) http.Handler {
	t.Helper()
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	return h
}

// TestHandler_ServesIndex 验证根路径返回控制台页面。
func TestHandler_ServesIndex(t *testing.T) {
	rec := httptest.NewRecorder()
	newHandler(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "KahoAgent") {
		t.Fatal("index page does not contain the product name")
	}
}

// TestHandler_ServesStatic 验证静态资源可访问且类型正确。
func TestHandler_ServesStatic(t *testing.T) {
	for _, tc := range []struct{ path, wantType string }{
		{"/static/app.css", "text/css"},
		{"/static/app.js", "javascript"},
	} {
		rec := httptest.NewRecorder()
		newHandler(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", tc.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tc.wantType) {
			t.Fatalf("%s: content-type = %q, want contains %q", tc.path, ct, tc.wantType)
		}
	}
}

// TestHandler_NotFound 验证未知路径返回 404，而不是偷偷 fallback 到 index。
//
// 不做 SPA fallback 是有意的：这个控制台只有一页，
// 把 /api/typo 也渲染成 HTML 只会让调试更困惑。
func TestHandler_NotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	newHandler(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestHandler_SecurityHeaders 验证 CSP 等响应头存在。
//
// CSP 是「前端漏了转义」时的最后一道防线：
// 即使模型输出里混进了 <script>，没有 'unsafe-inline' 也执行不了。
func TestHandler_SecurityHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	newHandler(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("CSP = %q, want script-src 'self'", csp)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
}
