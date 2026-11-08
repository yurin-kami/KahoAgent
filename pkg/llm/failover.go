package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Endpoint 描述一个 OpenAI 兼容的上游。
type Endpoint struct {
	APIKey  string
	BaseURL string
	Model   string
}

// ErrCircuitOpen 表示熔断器处于打开状态，本次调用被直接拒绝。
//
// 单独定义一个可判定的错误，而不是靠字符串匹配：
// handler 需要据此决定给用户的提示（「服务暂时不可用」而不是「处理出错」）。
var ErrCircuitOpen = errors.New("upstream circuit is open")

// HTTPError 携带状态码。
//
// ⚠️ 为什么不用 fmt.Errorf("HTTP %d: ...")：判断「该不该切备用」
// 必须看状态码，而字符串匹配会在上游改措辞时静默失效。
// 结构化错误让 isRetryable 变成一次类型断言。
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// isRetryable 判断错误是否值得切到备用上游。
//
// 429：配额/并发打满 —— 换个模型往往立刻可用，是最该兜底的场景。
// 5xx：上游自己挂了。
// 网络错误/超时：连接问题，换域名可能有用。
// 4xx（除 429）：请求本身有问题（参数错、模型名不存在），换谁都一样，
// 重试只会浪费一次配额并掩盖真正的 bug。
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		// 用户主动取消，不该再打一次备用
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status == http.StatusTooManyRequests || he.Status >= 500
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// 其余（dial 失败、连接重置、EOF 等）都按网络问题处理
	return true
}

// Breaker 是简单的连续失败熔断器。
//
// 为什么需要它：上游 429 时，每个用户请求都会先等一次超时再切备用，
// 而备用也未必可用 —— 结果是一批请求全部卡在重试上，把 goroutine 和
// 连接池占满，连「快速失败」都做不到。熔断之后，连续失败 N 次就短路
// 一段时间，请求立刻返回可读的错误。
//
// 不做半开探测（half-open）：本项目只有一个上游 + 一个备用，
// 短路到期后直接放行一个请求即可，额外的探测状态机收益很低。
type Breaker struct {
	mu        sync.Mutex
	threshold int
	openFor   time.Duration
	now       func() time.Time

	fails     int
	openUntil time.Time
}

// NewBreaker 创建熔断器。threshold <= 0 时使用默认值 5；
// openFor <= 0 时使用默认值 30 秒。
func NewBreaker(threshold int, openFor time.Duration) *Breaker {
	if threshold <= 0 {
		threshold = 5
	}
	if openFor <= 0 {
		openFor = 30 * time.Second
	}
	return &Breaker{
		threshold: threshold,
		openFor:   openFor,
		now:       time.Now,
	}
}

// Allow 报告当前是否允许发起调用。
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true
	}
	if b.now().Before(b.openUntil) {
		return false
	}
	// 冷却期结束：复位计数，重新放行。
	b.openUntil = time.Time{}
	b.fails = 0
	return true
}

// Success 记录一次成功，清零失败计数。
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails = 0
	b.openUntil = time.Time{}
}

// Failure 记录一次失败，达到阈值时打开熔断。
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails++
	if b.fails >= b.threshold {
		b.openUntil = b.now().Add(b.openFor)
	}
}

// upstream 封装「主上游 + 可选备用 + 熔断」的调用逻辑。
//
// 两个模型（ChatModel 与 EinoChatModel）都用它，避免两边的
// 容错行为悄悄分叉 —— 那种分叉在排查时最费时间：
// 精排明明切了备用，对话却没有。
type upstream struct {
	primary  Endpoint
	fallback *Endpoint
	breaker  *Breaker
	client   *http.Client
}

// post 发起一次 chat/completions 请求，必要时切备用。
//
// 返回的 resp 状态码一定是 200，调用方负责关闭 Body。
// 失败时返回的错误可能是 *HTTPError、ErrCircuitOpen 或网络错误。
func (u *upstream) post(ctx context.Context, body []byte, acceptSSE bool) (*http.Response, error) {
	if !u.breaker.Allow() {
		return nil, ErrCircuitOpen
	}

	resp, err := u.do(ctx, u.primary, body, acceptSSE)
	if err == nil {
		u.breaker.Success()
		return resp, nil
	}

	// 只在「值得重试」且有备用时切换。
	// ⚠️ 不重试同一个上游：429 与 5xx 在几毫秒内重试基本必然再失败，
	// 徒增延迟。备用上游是另一个配额池，命中率完全不同。
	if u.fallback != nil && isRetryable(err) {
		fbBody, berr := rewriteModel(body, u.fallback.Model)
		if berr == nil {
			if resp2, err2 := u.do(ctx, *u.fallback, fbBody, acceptSSE); err2 == nil {
				u.breaker.Success()
				return resp2, nil
			} else {
				err = fmt.Errorf("%w; fallback %s also failed: %v", err, u.fallback.Model, err2)
			}
		}
	}

	u.breaker.Failure()
	return nil, err
}

// do 向指定端点发一次请求，非 200 一律转成 *HTTPError。
func (u *upstream) do(ctx context.Context, ep Endpoint, body []byte, acceptSSE bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ep.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ep.APIKey)
	if acceptSSE {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// 只读前 2KB：错误页可能是整页 HTML，全读进来没意义
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_ = resp.Body.Close()
		return nil, &HTTPError{Status: resp.StatusCode, Body: truncate(string(raw), 300)}
	}
	return resp, nil
}

// newUpstream 组装 upstream，nil 参数用安全默认值补齐。
func newUpstream(primary Endpoint, fallback *Endpoint, b *Breaker, client *http.Client) *upstream {
	if b == nil {
		b = NewBreaker(5, 30*time.Second)
	}
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &upstream{primary: primary, fallback: fallback, breaker: b, client: client}
}

// rewriteModel 把请求体里的 model 换成备用模型。
//
// 用 map 而不是重新序列化整个结构：两个模型各有自己的请求类型，
// 这里只需要改一个字段，泛化处理反而要维护两份逻辑。
func rewriteModel(body []byte, model string) ([]byte, error) {
	if model == "" {
		return body, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("rewrite model: %w", err)
	}
	raw, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	m["model"] = raw
	return json.Marshal(m)
}
