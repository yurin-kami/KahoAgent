package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestIsRetryable 锁住「什么错误值得切备用」的判定。
//
// 这条规则错了的代价是不对称的：
// 把不可重试的 4xx 当成可重试，会白白多打一次上游；
// 把 429 当成不可重试，则明明有备用却让用户看到失败。
func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"429", &HTTPError{Status: 429}, true},
		{"500", &HTTPError{Status: 500}, true},
		{"503", &HTTPError{Status: 503}, true},
		{"400", &HTTPError{Status: 400}, false},
		{"401", &HTTPError{Status: 401}, false},
		{"404", &HTTPError{Status: 404}, false},
		{"deadline", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, false},
		{"network", errors.New("dial tcp: connection refused"), true},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryable(tc.err); got != tc.want {
				t.Fatalf("isRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestBreaker_OpensAndRecovers 验证熔断阈值与冷却恢复。
func TestBreaker_OpensAndRecovers(t *testing.T) {
	b := NewBreaker(3, 10*time.Second)
	now := time.Now()
	b.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		b.Failure()
		if !b.Allow() {
			t.Fatalf("breaker opened too early at failure %d", i+1)
		}
	}
	b.Failure() // 第 3 次，达到阈值
	if b.Allow() {
		t.Fatal("breaker should be open after 3 failures")
	}

	// 冷却期内仍然拒绝
	now = now.Add(5 * time.Second)
	if b.Allow() {
		t.Fatal("breaker should stay open during cooldown")
	}

	// 冷却结束自动放行并复位
	now = now.Add(6 * time.Second)
	if !b.Allow() {
		t.Fatal("breaker should allow after cooldown")
	}
	if !b.Allow() {
		t.Fatal("breaker should stay closed after reset")
	}
}

// TestBreaker_SuccessResets 验证成功会清零失败计数。
func TestBreaker_SuccessResets(t *testing.T) {
	b := NewBreaker(2, time.Minute)
	b.Failure()
	b.Success()
	b.Failure()
	if !b.Allow() {
		t.Fatal("success should reset the failure counter")
	}
}

// okServer 返回一个固定的 chat completion。
func okServer(t *testing.T, content string, gotModel *atomic.Value) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if gotModel != nil {
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			if v, ok := m["model"].(string); ok {
				gotModel.Store(v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"`+content+`"}}]}`)
	}))
}

func failServer(status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", status)
	}))
}

// TestChatModel_FallsBackOn429 验证 429 时切到备用模型。
func TestChatModel_FallsBackOn429(t *testing.T) {
	primary := failServer(http.StatusTooManyRequests)
	defer primary.Close()

	var model atomic.Value
	fallback := okServer(t, "备用回答", &model)
	defer fallback.Close()

	m := NewChatModel("k", primary.URL, "qwen-plus",
		WithChatFallback(Endpoint{APIKey: "k", BaseURL: fallback.URL, Model: "qwen-turbo"}))

	out, err := m.Complete(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "备用回答" {
		t.Fatalf("content = %q, want 备用回答", out)
	}
	if got := model.Load(); got != "qwen-turbo" {
		t.Fatalf("fallback received model %v, want qwen-turbo", got)
	}
}

// TestChatModel_NoFallbackOn400 验证 4xx 不切备用。
//
// 400 通常是请求本身有问题（模型名拼错、参数不合法），
// 换一个上游只会再报一次同样的错，还多花一次配额。
func TestChatModel_NoFallbackOn400(t *testing.T) {
	primary := failServer(http.StatusBadRequest)
	defer primary.Close()

	var calls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer fallback.Close()

	m := NewChatModel("k", primary.URL, "qwen-plus",
		WithChatFallback(Endpoint{APIKey: "k", BaseURL: fallback.URL, Model: "qwen-turbo"}))

	if _, err := m.Complete(context.Background(), "hi"); err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 0 {
		t.Fatalf("fallback was called %d times, want 0", calls.Load())
	}
}

// TestChatModel_CircuitOpen 验证熔断打开后不再打上游。
func TestChatModel_CircuitOpen(t *testing.T) {
	var hits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "boom", 500)
	}))
	defer primary.Close()

	b := NewBreaker(1, time.Minute)
	m := NewChatModel("k", primary.URL, "qwen-plus",
		WithChatBreaker(b), WithChatHTTPClient(&http.Client{Timeout: 2 * time.Second}))

	// 第一次：失败并打开熔断
	if _, err := m.Complete(context.Background(), "hi"); err == nil {
		t.Fatal("first call should fail")
	}
	// 第二次：直接返回 ErrCircuitOpen，不再发请求
	_, err := m.Complete(context.Background(), "hi")
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hit %d times, want 1", hits.Load())
	}
}

// TestRewriteModel 验证请求体里的 model 被正确替换，其余字段保留。
func TestRewriteModel(t *testing.T) {
	in := []byte(`{"model":"a","temperature":0.7,"messages":[{"role":"user","content":"x"}]}`)
	out, err := rewriteModel(in, "b")
	if err != nil {
		t.Fatalf("rewriteModel: %v", err)
	}
	if !strings.Contains(string(out), `"model":"b"`) {
		t.Fatalf("model not rewritten: %s", out)
	}
	if !strings.Contains(string(out), `"messages"`) {
		t.Fatalf("other fields lost: %s", out)
	}
}
