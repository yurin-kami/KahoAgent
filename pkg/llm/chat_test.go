package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeChatServer(t *testing.T, handler func(req chatRequest) (int, any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		status, body := handler(req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func okChat(content string) map[string]any {
	return map[string]any{
		"choices": []map[string]any{
			{"message": map[string]string{"role": "assistant", "content": content}},
		},
	}
}

func TestChatModel_Score(t *testing.T) {
	srv := fakeChatServer(t, func(req chatRequest) (int, any) {
		if len(req.Messages) != 1 || req.Messages[0].Content != "打分请求" {
			t.Errorf("unexpected messages: %+v", req.Messages)
		}
		return http.StatusOK, okChat("8")
	})

	c := NewChatModel("k", srv.URL, "m")
	got, err := c.Score(context.Background(), "打分请求")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got != "8" {
		t.Errorf("expected %q, got %q", "8", got)
	}
}

func TestChatModel_UsesZeroTemperatureByDefault(t *testing.T) {
	// 精排必须可复现：同样输入必须给出同样分数，
	// 否则每次评估结果都不同，无法判断改动是否真的带来提升
	var seen float32 = -1
	srv := fakeChatServer(t, func(req chatRequest) (int, any) {
		seen = req.Temperature
		return http.StatusOK, okChat("5")
	})

	c := NewChatModel("k", srv.URL, "m")
	if _, err := c.Score(context.Background(), "x"); err != nil {
		t.Fatalf("score: %v", err)
	}
	if seen != 0 {
		t.Errorf("expected temperature 0, got %v", seen)
	}
}

func TestChatModel_ScoreLimitsOutputLength(t *testing.T) {
	// 精排只需要一个数字，限制长度可防止模型跑偏写长文
	var maxTokens int
	srv := fakeChatServer(t, func(req chatRequest) (int, any) {
		maxTokens = req.MaxTokens
		return http.StatusOK, okChat("7")
	})

	c := NewChatModel("k", srv.URL, "m")
	if _, err := c.Score(context.Background(), "x"); err != nil {
		t.Fatalf("score: %v", err)
	}
	if maxTokens == 0 || maxTokens > 64 {
		t.Errorf("expected a small max_tokens for scoring, got %d", maxTokens)
	}
}

func TestChatModel_CompleteDoesNotLimitLength(t *testing.T) {
	var maxTokens = -1
	srv := fakeChatServer(t, func(req chatRequest) (int, any) {
		maxTokens = req.MaxTokens
		return http.StatusOK, okChat("一段较长的回答")
	})

	c := NewChatModel("k", srv.URL, "m")
	got, err := c.Complete(context.Background(), "x")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got != "一段较长的回答" {
		t.Errorf("unexpected content: %q", got)
	}
	if maxTokens != 0 {
		t.Errorf("Complete should not cap max_tokens, got %d", maxTokens)
	}
}

func TestChatModel_SurfacesAPIError(t *testing.T) {
	srv := fakeChatServer(t, func(chatRequest) (int, any) {
		return http.StatusOK, map[string]any{
			"error": map[string]string{"message": "model not found"},
		}
	})

	c := NewChatModel("k", srv.URL, "nope")
	_, err := c.Score(context.Background(), "x")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Errorf("expected the API message to surface, got: %v", err)
	}
}

func TestChatModel_SurfacesHTTPError(t *testing.T) {
	srv := fakeChatServer(t, func(chatRequest) (int, any) {
		return http.StatusUnauthorized, map[string]string{"msg": "bad key"}
	})

	c := NewChatModel("bad", srv.URL, "m")
	if _, err := c.Score(context.Background(), "x"); err == nil {
		t.Fatal("expected an error for HTTP 401")
	}
}

func TestChatModel_RejectsEmptyChoices(t *testing.T) {
	// 空 choices 不能静默返回空串 —— 那会被精排解析成「无法评分」，
	// 掩盖掉真正的问题
	srv := fakeChatServer(t, func(chatRequest) (int, any) {
		return http.StatusOK, map[string]any{"choices": []any{}}
	})

	c := NewChatModel("k", srv.URL, "m")
	if _, err := c.Score(context.Background(), "x"); err == nil {
		t.Fatal("expected an error when the response has no choices")
	}
}

func TestChatModel_RespectsContextCancellation(t *testing.T) {
	srv := fakeChatServer(t, func(chatRequest) (int, any) {
		return http.StatusOK, okChat("5")
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := NewChatModel("k", srv.URL, "m")
	if _, err := c.Score(ctx, "x"); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}
