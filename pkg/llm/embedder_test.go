package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEmbedServer 构造一个可控的 embeddings 端点。
// handler 收到解析好的请求，返回 (状态码, 响应体)。
func fakeEmbedServer(t *testing.T, handler func(req embedRequest, callNum int) (int, any)) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)

		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		status, body := handler(req, int(n))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// okResponse 生成一个正常响应，每条文本的向量填充为其在批内的下标值，
// 便于断言顺序是否正确。
func okResponse(req embedRequest, dim int) embedResponse {
	var resp embedResponse
	for i, text := range req.Input {
		emb := make([]float64, dim)
		for j := range emb {
			emb[j] = float64(len(text)) // 用文本长度做指纹
		}
		resp.Data = append(resp.Data, struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		}{Index: i, Embedding: emb})
	}
	return resp
}

func TestEmbedder_BatchesAndPreservesOrder(t *testing.T) {
	const dim = 4
	srv, calls := fakeEmbedServer(t, func(req embedRequest, _ int) (int, any) {
		return http.StatusOK, okResponse(req, dim)
	})

	e := NewEmbedder("k", srv.URL, "m", dim, WithBatchSize(3))

	// 7 条文本，batchSize=3 → 应发 3 次请求（3+3+1）
	texts := []string{"a", "bb", "ccc", "dddd", "eeeee", "ffffff", "ggggggg"}
	vecs, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}

	if got := atomic.LoadInt32(calls); got != 3 {
		t.Errorf("expected 3 batched requests, got %d", got)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("expected %d vectors, got %d", len(texts), len(vecs))
	}

	// 每个向量的指纹应等于对应文本的长度 —— 验证跨批次顺序没错位
	for i, text := range texts {
		if len(vecs[i]) != dim {
			t.Fatalf("vec[%d]: expected dim %d, got %d", i, dim, len(vecs[i]))
		}
		if want := float32(len(text)); vecs[i][0] != want {
			t.Errorf("vec[%d] misaligned: expected fingerprint %v, got %v", i, want, vecs[i][0])
		}
	}
}

func TestEmbedder_HonorsOutOfOrderResponseIndex(t *testing.T) {
	// 协议不保证 data 数组与输入同序，必须按 Index 字段回填
	const dim = 2
	srv, _ := fakeEmbedServer(t, func(req embedRequest, _ int) (int, any) {
		resp := okResponse(req, dim)
		// 反转 data 数组，Index 字段保持正确
		for i, j := 0, len(resp.Data)-1; i < j; i, j = i+1, j-1 {
			resp.Data[i], resp.Data[j] = resp.Data[j], resp.Data[i]
		}
		return http.StatusOK, resp
	})

	e := NewEmbedder("k", srv.URL, "m", dim)
	texts := []string{"a", "bb", "ccc"}

	vecs, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	for i, text := range texts {
		if want := float32(len(text)); vecs[i][0] != want {
			t.Errorf("vec[%d]: expected %v, got %v — response index was ignored", i, want, vecs[i][0])
		}
	}
}

func TestEmbedder_RetriesOn5xxThenSucceeds(t *testing.T) {
	const dim = 2
	srv, calls := fakeEmbedServer(t, func(req embedRequest, n int) (int, any) {
		if n < 3 {
			return http.StatusInternalServerError, map[string]string{"msg": "boom"}
		}
		return http.StatusOK, okResponse(req, dim)
	})

	e := NewEmbedder("k", srv.URL, "m", dim)

	start := time.Now()
	vecs, err := e.Embed(context.Background(), []string{"x"})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if len(vecs) != 1 {
		t.Fatalf("expected 1 vector, got %d", len(vecs))
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Errorf("expected 3 attempts, got %d", got)
	}
	// 退避应为 1s + 2s
	if elapsed < 3*time.Second {
		t.Errorf("backoff too short: %v (expected >= 3s)", elapsed)
	}
}

func TestEmbedder_DoesNotRetryOn4xx(t *testing.T) {
	// 401/400 重试没有意义，应立即失败以免浪费时间和配额
	srv, calls := fakeEmbedServer(t, func(_ embedRequest, _ int) (int, any) {
		return http.StatusUnauthorized, map[string]string{"msg": "bad key"}
	})

	e := NewEmbedder("bad", srv.URL, "m", 2)
	_, err := e.Embed(context.Background(), []string{"x"})

	if err == nil {
		t.Fatal("expected an error for 401")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("4xx should not be retried, but got %d attempts", got)
	}
}

func TestEmbedder_RetriesOn429(t *testing.T) {
	const dim = 2
	srv, calls := fakeEmbedServer(t, func(req embedRequest, n int) (int, any) {
		if n == 1 {
			return http.StatusTooManyRequests, map[string]string{"msg": "slow down"}
		}
		return http.StatusOK, okResponse(req, dim)
	})

	e := NewEmbedder("k", srv.URL, "m", dim)
	if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("expected retry on 429 to succeed: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("expected 2 attempts, got %d", got)
	}
}

func TestEmbedder_RejectsDimensionMismatch(t *testing.T) {
	// 维度不符要早失败并给出可操作的提示，否则会在 Milvus 插入时
	// 才炸，而那时错误信息已经离根因很远了
	srv, _ := fakeEmbedServer(t, func(req embedRequest, _ int) (int, any) {
		return http.StatusOK, okResponse(req, 8) // 服务端返回 8 维
	})

	e := NewEmbedder("k", srv.URL, "m", 4) // 配置声明 4 维
	_, err := e.Embed(context.Background(), []string{"x"})

	if err == nil {
		t.Fatal("expected a dimension mismatch error")
	}
	if !contains(err.Error(), "embedding_dim") {
		t.Errorf("error should point at the config key, got: %v", err)
	}
}

func TestEmbedder_DetectsMissingEmbedding(t *testing.T) {
	// 响应缺少某个 index 时不能静默返回 nil 向量
	srv, _ := fakeEmbedServer(t, func(req embedRequest, _ int) (int, any) {
		resp := okResponse(req, 2)
		resp.Data = resp.Data[:len(resp.Data)-1] // 少返回一条
		return http.StatusOK, resp
	})

	e := NewEmbedder("k", srv.URL, "m", 2)
	_, err := e.Embed(context.Background(), []string{"a", "b"})
	if err == nil {
		t.Fatal("expected an error when the response is short one embedding")
	}
}

func TestEmbedder_SurfacesAPIError(t *testing.T) {
	srv, _ := fakeEmbedServer(t, func(_ embedRequest, _ int) (int, any) {
		return http.StatusOK, map[string]any{
			"error": map[string]string{"message": "model not found", "type": "invalid_request"},
		}
	})

	e := NewEmbedder("k", srv.URL, "nope", 2)
	_, err := e.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("expected an error for an API-level error payload")
	}
	if !contains(err.Error(), "model not found") {
		t.Errorf("expected the API message to surface, got: %v", err)
	}
}

func TestEmbedder_EmptyInput(t *testing.T) {
	srv, calls := fakeEmbedServer(t, func(req embedRequest, _ int) (int, any) {
		return http.StatusOK, okResponse(req, 2)
	})

	e := NewEmbedder("k", srv.URL, "m", 2)
	vecs, err := e.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("empty input should not error: %v", err)
	}
	if len(vecs) != 0 {
		t.Errorf("expected no vectors, got %d", len(vecs))
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Errorf("empty input should not hit the API, but made %d calls", got)
	}
}

func TestEmbedder_RespectsContextCancellation(t *testing.T) {
	srv, _ := fakeEmbedServer(t, func(_ embedRequest, _ int) (int, any) {
		return http.StatusInternalServerError, map[string]string{"msg": "boom"}
	})

	e := NewEmbedder("k", srv.URL, "m", 2)

	// 超时短于第一次退避，应在退避期间就返回而非跑满重试
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := e.Embed(ctx, []string{"x"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error on cancellation")
	}
	if elapsed > 2*time.Second {
		t.Errorf("cancellation was not honored during backoff: took %v", elapsed)
	}
}

func TestEmbedder_DefaultBatchSizeRespectsDashScopeLimit(t *testing.T) {
	// DashScope 硬性上限为 10，超出直接 400。
	// 这个限制在 Phase 1 没暴露（sample.md 只有 7 个 chunk，一批发完），
	// 直到 Phase 2 入库 1300+ chunk 的真实语料才炸 ——
	// 故用测试钉死，避免有人「顺手调大点更快」。
	const dashscopeMax = 10
	if defaultBatchSize > dashscopeMax {
		t.Fatalf("defaultBatchSize is %d, DashScope rejects anything over %d",
			defaultBatchSize, dashscopeMax)
	}

	// 行为验证：22 条文本不能有任何一批超过上限
	srv, _ := fakeEmbedServer(t, func(req embedRequest, _ int) (int, any) {
		if len(req.Input) > dashscopeMax {
			t.Errorf("sent a batch of %d, exceeds the limit of %d", len(req.Input), dashscopeMax)
		}
		return http.StatusOK, okResponse(req, 2)
	})

	texts := make([]string, 22)
	for i := range texts {
		texts[i] = "文本"
	}

	e := NewEmbedder("k", srv.URL, "m", 2)
	if _, err := e.Embed(context.Background(), texts); err != nil {
		t.Fatalf("embed: %v", err)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
