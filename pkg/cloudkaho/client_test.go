package cloudkaho

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fakeServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestClient_Search(t *testing.T) {
	srv := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/file/search" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("keyword"); got != "redis" {
			t.Errorf("expected the keyword to be forwarded, got %q", got)
		}
		if got := r.URL.Query().Get("limit"); got != "5" {
			t.Errorf("expected limit=5, got %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "0", "total": 1,
			"files": []map[string]any{{
				"file_id": 100, "file_name": "redis.md", "file_size": 2048,
			}},
		})
	})

	c, _ := New(srv.URL, "tok")
	got, err := c.Search(context.Background(), "redis", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].FileName != "redis.md" || got[0].FileSize != 2048 {
		t.Errorf("unexpected result: %+v", got)
	}
}

func TestClient_SendsBearerToken(t *testing.T) {
	// ⚠️ 服务端用 strings.HasPrefix(bearer, "Bearer ") 检查，
	// 大小写敏感且必须恰好一个空格
	var got string
	srv := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "files": []any{}})
	})

	c, _ := New(srv.URL, "my-token")
	if _, err := c.Search(context.Background(), "", 0); err != nil {
		t.Fatalf("search: %v", err)
	}
	if got != "Bearer my-token" {
		t.Errorf("expected %q, got %q", "Bearer my-token", got)
	}
}

func TestClient_WithTokenDoesNotMutateReceiver(t *testing.T) {
	// 多个会话可能并发用同一个 Client。原地改 token 会让
	// A 用户的请求带上 B 用户的身份 —— 越权，且极难复现。
	c, _ := New("http://x", "original")
	_ = c.WithToken("other")
	if c.token != "original" {
		t.Errorf("WithToken must not mutate the receiver, got %q", c.token)
	}
}

func TestClient_Metadata(t *testing.T) {
	srv := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/file/metadata/100" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "0",
			"file": map[string]any{"file_id": 100, "file_name": "a.md", "file_size": 24576},
		})
	})

	c, _ := New(srv.URL, "tok")
	got, err := c.Metadata(context.Background(), 100)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if got.FileSize != 24576 {
		t.Errorf("expected size 24576, got %d", got.FileSize)
	}
}

func TestClient_ShareSendsAbsoluteTimestamp(t *testing.T) {
	// ⚠️ 这是 CloudKaho 的一个坑：expire_at 的注释写「单位为秒」，
	// 但实现是 time.Unix(expireAt, 0) —— 当绝对时间戳用。
	// 传相对秒数会算出负数有效期，得到一个已过期的 URL。
	var body shareRequest
	srv := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "0", "share_url": "https://s3.example.com/presigned",
		})
	})

	c, _ := New(srv.URL, "tok")
	url, err := c.Share(context.Background(), 100, 2)
	if err != nil {
		t.Fatalf("share: %v", err)
	}
	if url != "https://s3.example.com/presigned" {
		t.Errorf("unexpected URL: %q", url)
	}

	// 必须是未来的绝对时间戳，且大致等于 now + 2h
	wantAround := time.Now().Add(2 * time.Hour).Unix()
	if diff := body.ExpireAt - wantAround; diff < -60 || diff > 60 {
		t.Errorf("expire_at must be an absolute future timestamp (~now+2h): got %d, want ~%d",
			body.ExpireAt, wantAround)
	}
	if body.ShareType != shareTypePublic {
		t.Errorf("expected a public share type, got %d", body.ShareType)
	}
}

func TestClient_ShareClampsExpiry(t *testing.T) {
	// AWS 预签名上限 7 天，超过会被 SDK 拒绝
	var body shareRequest
	srv := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "share_url": "u"})
	})

	c, _ := New(srv.URL, "tok")
	if _, err := c.Share(context.Background(), 100, 99999); err != nil {
		t.Fatalf("share: %v", err)
	}

	maxTS := time.Now().Add(maxShareHours * time.Hour).Unix()
	if body.ExpireAt > maxTS+60 {
		t.Errorf("expiry must be clamped to 7 days, got %d (max ~%d)", body.ExpireAt, maxTS)
	}
}

func TestClient_BusinessCodeFailureIsAnError(t *testing.T) {
	// ⚠️ HTTP 200 + code="1" 是可能的。只看状态码会把失败当成功。
	srv := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "1", "error": "something"})
	})

	c, _ := New(srv.URL, "tok")
	if _, err := c.Search(context.Background(), "x", 0); err == nil {
		t.Fatal("expected an error when the business code is not \"0\"")
	}
}

func TestClient_UnauthorizedIsExplicit(t *testing.T) {
	// 401 几乎总是 token 过期或没配，与「文件不存在」是
	// 完全不同的处理方式，错误信息要说清楚
	srv := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "1", "error": "invalid token",
		})
	})

	c, _ := New(srv.URL, "bad")
	_, err := c.Search(context.Background(), "x", 0)
	if err == nil {
		t.Fatal("expected an error for HTTP 401")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("the error should point at the token, got: %v", err)
	}
}

func TestClient_SurfacesServerErrorDetails(t *testing.T) {
	srv := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "1", "error": "file not found",
		})
	})

	c, _ := New(srv.URL, "tok")
	_, err := c.Metadata(context.Background(), 999)
	if err == nil {
		t.Fatal("expected an error for HTTP 404")
	}
	if !strings.Contains(err.Error(), "file not found") {
		t.Errorf("the server message must surface, got: %v", err)
	}
}

func TestClient_ShareRejectsEmptyURL(t *testing.T) {
	// 成功码但没有 URL 是服务端 bug，不能静默返回空串 ——
	// 那会让 Agent 把空链接发给用户
	srv := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "share_url": ""})
	})

	c, _ := New(srv.URL, "tok")
	if _, err := c.Share(context.Background(), 100, 1); err == nil {
		t.Fatal("expected an error when no URL is returned")
	}
}

func TestNew_RejectsEmptyBaseURL(t *testing.T) {
	if _, err := New("", "tok"); err == nil {
		t.Fatal("expected an error for an empty base URL")
	}
}

func TestClient_RespectsContextCancellation(t *testing.T) {
	srv := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "files": []any{}})
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c, _ := New(srv.URL, "tok")
	if _, err := c.Search(ctx, "x", 0); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}
