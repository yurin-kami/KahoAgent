// Package llm 封装对大模型服务的调用。
//
// 只依赖 OpenAI 兼容协议，因此同一份代码可对接 DashScope、DeepSeek、
// OpenAI 等服务，切换只需改 base_url 与 model。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

const (
	// defaultBatchSize 是单次请求携带的文本条数。
	//
	// ⚠️ 10 是 DashScope 的硬性上限，不是保守估计。超出会收到：
	//   HTTP 400 InternalError.Algo.InvalidParameter:
	//   "batch size is invalid, it should not be larger than 10"
	//
	// 这个限制在 Phase 1 没暴露 —— sample.md 只切出 7 个 chunk，
	// 一批就发完了。直到 Phase 2 入库 1300+ chunk 的真实语料才炸。
	// 教训：跟外部 API 的批量上限，必须用真实规模的数据验证。
	//
	// 若换成 OpenAI（上限 2048），可用 WithBatchSize 调大。
	defaultBatchSize = 10

	// maxAttempts 含首次尝试，即最多重试 2 次。
	maxAttempts = 3

	defaultTimeout = 60 * time.Second
)

// Embedder 调用 OpenAI 兼容的 embeddings 接口。
//
// 三个必须做对的工程点：
//  1. 批量：一条一条发会让 100 个 chunk 变成 100 次 HTTP 往返
//  2. 重试：网络抖动不应让整个入库任务失败
//  3. 顺序：返回的向量必须与输入文本一一对应，错位会导致
//     检索时文本和向量对不上，且这种 bug 极难排查
type Embedder struct {
	apiKey    string
	baseURL   string
	model     string
	dim       int
	batchSize int
	client    *http.Client
}

var _ rag.Embedder = (*Embedder)(nil)

// EmbedderOption 用于覆盖默认配置。
type EmbedderOption func(*Embedder)

// WithBatchSize 设置单次请求的文本条数。
func WithBatchSize(n int) EmbedderOption {
	return func(e *Embedder) {
		if n > 0 {
			e.batchSize = n
		}
	}
}

// WithHTTPClient 注入自定义 client，测试时用来指向 httptest server。
func WithHTTPClient(c *http.Client) EmbedderOption {
	return func(e *Embedder) {
		if c != nil {
			e.client = c
		}
	}
}

// NewEmbedder 创建 Embedding 客户端。
// dim 必须与 model 的实际输出维度一致，否则 Milvus 插入会因维度不符失败。
func NewEmbedder(apiKey, baseURL, model string, dim int, opts ...EmbedderOption) *Embedder {
	e := &Embedder{
		apiKey:    apiKey,
		baseURL:   baseURL,
		model:     model,
		dim:       dim,
		batchSize: defaultBatchSize,
		client:    &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

func (e *Embedder) Dim() int { return e.dim }

// Embed 批量向量化，返回顺序与 texts 一致。
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	// 预分配整个结果切片并按绝对下标写入。
	// 这样即便将来改成并发分批，顺序也不会错乱 ——
	// 用 append 拼接才是错位 bug 的温床。
	out := make([][]float32, len(texts))

	for start := 0; start < len(texts); start += e.batchSize {
		end := min(start+e.batchSize, len(texts))

		vecs, err := e.embedBatchWithRetry(ctx, texts[start:end])
		if err != nil {
			return nil, fmt.Errorf("embed batch [%d,%d): %w", start, end, err)
		}
		if len(vecs) != end-start {
			return nil, fmt.Errorf("embed batch [%d,%d): expected %d vectors, got %d",
				start, end, end-start, len(vecs))
		}
		copy(out[start:end], vecs)
	}
	return out, nil
}

// embedBatchWithRetry 以指数退避重试单个批次。
func (e *Embedder) embedBatchWithRetry(ctx context.Context, batch []string) ([][]float32, error) {
	var lastErr error

	for attempt := range maxAttempts {
		if attempt > 0 {
			// 退避 1s、2s。用 timer 而非 Sleep 以便及时响应 ctx 取消。
			delay := time.Duration(1<<(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		vecs, err := e.embedBatch(ctx, batch)
		if err == nil {
			return vecs, nil
		}
		lastErr = err

		// 4xx（除 429）是请求本身有问题，重试同样会失败，直接返回。
		var re *retryableError
		if !errors.As(err, &re) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// retryableError 标记值得重试的错误：网络故障、429、5xx。
type retryableError struct{ err error }

func (r *retryableError) Error() string { return r.err.Error() }
func (r *retryableError) Unwrap() error { return r.err }

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (e *Embedder) embedBatch(ctx context.Context, batch []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: e.model, Input: batch})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		e.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		// 网络层错误通常是瞬时的，值得重试
		return nil, &retryableError{err: fmt.Errorf("request failed: %w", err)}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &retryableError{err: fmt.Errorf("read body: %w", err)}
	}

	if resp.StatusCode != http.StatusOK {
		httpErr := fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return nil, &retryableError{err: httpErr}
		}
		return nil, httpErr
	}

	var parsed embedResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w (raw: %s)", err, truncate(string(raw), 300))
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("api error: %s", parsed.Error.Message)
	}
	if len(parsed.Data) != len(batch) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(batch), len(parsed.Data))
	}

	vecs := make([][]float32, len(batch))
	for _, d := range parsed.Data {
		// ⚠️ 依据 Index 字段回填而非按数组顺序。
		//    协议不保证 data 数组与输入同序，按顺序取是隐藏的错位风险。
		if d.Index < 0 || d.Index >= len(batch) {
			return nil, fmt.Errorf("response index %d out of range [0,%d)", d.Index, len(batch))
		}
		if len(d.Embedding) != e.dim {
			return nil, fmt.Errorf("dimension mismatch: model returned %d, configured %d "+
				"(check llm.embedding_dim in config.toml)", len(d.Embedding), e.dim)
		}
		// float64 -> float32：Milvus 只接受 float32，且精度损失对
		// 向量检索无实质影响，内存却能省一半。
		v := make([]float32, len(d.Embedding))
		for i, f := range d.Embedding {
			v[i] = float32(f)
		}
		vecs[d.Index] = v
	}

	// 校验没有空洞：若响应缺少某个 index，对应位置会是 nil。
	for i, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("missing embedding for input index %d", i)
		}
	}
	return vecs, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
