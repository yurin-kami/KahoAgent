package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yurin-kami/KahoAgent/internal/pipeline"
)

// fakePublisher 记录收到的任务，或按需返回错误。
//
// 上传 handler 只依赖 TaskPublisher 这一个方法，所以整组测试
// 不需要真 Kafka —— 这正是把依赖抽成接口的直接收益。
type fakePublisher struct {
	err   error
	tasks []pipeline.IngestTask
}

func (f *fakePublisher) Publish(_ context.Context, task pipeline.IngestTask) error {
	if f.err != nil {
		return f.err
	}
	f.tasks = append(f.tasks, task)
	return nil
}

func newUploadHandler(t *testing.T, pub TaskPublisher) *UploadHandler {
	t.Helper()
	h, err := NewUploadHandler(pub, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewUploadHandler: %v", err)
	}
	return h
}

func TestUploadHandler_MethodNotAllowed(t *testing.T) {
	h := newUploadHandler(t, &fakePublisher{})
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/upload", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", got)
	}
}

func TestUploadHandler_UnsupportedContentType(t *testing.T) {
	h := newUploadHandler(t, &fakePublisher{})
	req := httptest.NewRequest(http.MethodPost, "/api/upload", strings.NewReader("x"))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
}

// TestUploadHandler_JSON 走 JSON 入口，覆盖正常路径与默认 docID。
func TestUploadHandler_JSON(t *testing.T) {
	t.Run("保留显式 doc_id", func(t *testing.T) {
		pub := &fakePublisher{}
		h := newUploadHandler(t, pub)
		body := `{"doc_id":"notes.md","content":"# 标题\n正文"}`

		req := httptest.NewRequest(http.MethodPost, "/api/upload", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
		}
		if len(pub.tasks) != 1 {
			t.Fatalf("published %d tasks, want 1", len(pub.tasks))
		}
		if pub.tasks[0].DocID != "notes.md" {
			t.Fatalf("doc_id = %q, want notes.md", pub.tasks[0].DocID)
		}
		if string(pub.tasks[0].Raw) != "# 标题\n正文" {
			t.Fatalf("raw mismatch: %q", pub.tasks[0].Raw)
		}
	})

	t.Run("缺省 doc_id 用内容哈希兜底", func(t *testing.T) {
		pub := &fakePublisher{}
		h := newUploadHandler(t, pub)

		req := httptest.NewRequest(http.MethodPost, "/api/upload",
			strings.NewReader(`{"content":"同样内容"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", rec.Code)
		}
		id := pub.tasks[0].DocID
		if !strings.HasPrefix(id, "upload-") || !strings.HasSuffix(id, ".txt") {
			t.Fatalf("derived doc_id = %q, want upload-*.txt", id)
		}

		// 同样内容必须得到同样的 docID —— 这是重复上传幂等的前提
		pub2 := &fakePublisher{}
		h2 := newUploadHandler(t, pub2)
		req2 := httptest.NewRequest(http.MethodPost, "/api/upload",
			strings.NewReader(`{"content":"同样内容"}`))
		req2.Header.Set("Content-Type", "application/json")
		h2.ServeHTTP(httptest.NewRecorder(), req2)
		if pub2.tasks[0].DocID != id {
			t.Fatalf("same content produced different doc_id: %q vs %q", id, pub2.tasks[0].DocID)
		}
	})
}

// TestUploadHandler_Rejects 覆盖所有拒绝路径。
//
// 每个用例同时断言状态码与「有没有真的投递出去」——
// 只断言状态码的话，一个「先 publish 再校验」的实现也能过测试，
// 但它会把非法文档推进索引。
func TestUploadHandler_Rejects(t *testing.T) {
	big := strings.Repeat("a", pipeline.MaxRawBytes+1)

	tests := []struct {
		name    string
		body    string
		want    int
		wantPub bool
	}{
		{
			name: "空内容",
			body: `{"doc_id":"a.md","content":""}`,
			want: http.StatusBadRequest,
		},
		{
			name: "doc_id 带路径",
			body: `{"doc_id":"../etc/passwd","content":"x"}`,
			want: http.StatusBadRequest,
		},
		{
			name: "doc_id 为空字符",
			// 用反引号：要让 JSON 文本里出现 \u0000 转义序列，
			// 而不是在 Go 字符串里就展开成真正的 NUL（那会让 JSON 本身非法）。
			body: `{"doc_id":"a\u0000b.md","content":"x"}`,
			want: http.StatusBadRequest,
		},
		{
			name: "二进制内容",
			body: `{"doc_id":"a.md","content":"\u0000\u0001\u0002"}`,
			want: http.StatusUnsupportedMediaType,
		},
		{
			name: "超过原文上限",
			body: `{"doc_id":"a.md","content":"` + big + `"}`,
			want: http.StatusRequestEntityTooLarge,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pub := &fakePublisher{}
			h := newUploadHandler(t, pub)
			req := httptest.NewRequest(http.MethodPost, "/api/upload", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if len(pub.tasks) != 0 {
				t.Fatalf("rejected request still published %d tasks", len(pub.tasks))
			}
		})
	}
}

// TestUploadHandler_PublishError 验证队列不可用时返回 503 而不是 500。
//
// 503 表示「暂时不可用，可以重试」；500 会让客户端以为是代码 bug。
// 这个区分直接影响调用方要不要重试。
func TestUploadHandler_PublishError(t *testing.T) {
	pub := &fakePublisher{err: errors.New("kafka down")}
	h := newUploadHandler(t, pub)

	req := httptest.NewRequest(http.MethodPost, "/api/upload",
		strings.NewReader(`{"doc_id":"a.md","content":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	// 内部拓扑（broker 地址、topic）不能泄露给客户端
	if strings.Contains(rec.Body.String(), "kafka") {
		t.Fatalf("response leaks internal error: %s", rec.Body.String())
	}
}

// TestUploadHandler_Multipart 走表单入口。
func TestUploadHandler_Multipart(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("doc_id", "guide.md"); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("file", "ignored-name.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("# 指南\n正文")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	pub := &fakePublisher{}
	h := newUploadHandler(t, pub)
	req := httptest.NewRequest(http.MethodPost, "/api/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if len(pub.tasks) != 1 || pub.tasks[0].DocID != "guide.md" {
		t.Fatalf("unexpected tasks: %+v", pub.tasks)
	}
}

// TestUploadHandler_MultipartMissingFile 验证缺 file 字段的报错。
func TestUploadHandler_MultipartMissingFile(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("doc_id", "a.md")
	_ = mw.Close()

	h := newUploadHandler(t, &fakePublisher{})
	req := httptest.NewRequest(http.MethodPost, "/api/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestNormalizeDocID 单测 ID 规则本身。
func TestNormalizeDocID(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"a.md", false},
		{"  a.md  ", false},
		{"a/b.md", true},
		{`a\b.md`, true},
		{"..", true},
		{".", true},
		{strings.Repeat("x", 201), true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			_, err := normalizeDocID(tc.in, []byte("raw"))
			if (err != nil) != tc.wantErr {
				t.Fatalf("normalizeDocID(%q) err = %v, wantErr = %v", tc.in, err, tc.wantErr)
			}
		})
	}
}
