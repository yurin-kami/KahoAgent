package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yurin-kami/KahoAgent/internal/metrics"
)

// ⚠️ Gauge 的 Inc/Dec 一旦配不齐，指标会永久失真 —— 而且不会报错，
// 只是数字越来越大。等到有人发现"活跃会话 3 万"时，这个指标早就
// 没人信了。所以每条会提前 return 的路径都要单独验一次。
func TestActiveSessions_ReturnsToZeroOnEveryPath(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"正常完成", func(t *testing.T) {
			h := newHandler(t, "答案", nil)
			post(t, h, `{"message":"x"}`)
		}},
		{"请求体非法", func(t *testing.T) {
			h := newHandler(t, "答案", nil)
			post(t, h, `{broken`)
		}},
		{"消息为空", func(t *testing.T) {
			h := newHandler(t, "答案", nil)
			post(t, h, `{"message":""}`)
		}},
		{"方法不对", func(t *testing.T) {
			h := newHandler(t, "答案", nil)
			req := httptest.NewRequest(http.MethodGet, "/api/chat", nil)
			h.ServeHTTP(httptest.NewRecorder(), req)
		}},
		{"不支持流式", func(t *testing.T) {
			h := newHandler(t, "答案", nil)
			req := httptest.NewRequest(http.MethodPost, "/api/chat",
				strings.NewReader(`{"message":"x"}`))
			h.ServeHTTP(nonFlusher{httptest.NewRecorder()}, req)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metrics.ActiveSessions.Set(0)
			tc.run(t)

			if got := testutil.ToFloat64(metrics.ActiveSessions); got != 0 {
				t.Errorf("active sessions leaked: expected 0 after the request, got %v", got)
			}
		})
	}
}
