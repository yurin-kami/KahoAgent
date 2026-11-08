package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const defaultChatTimeout = 60 * time.Second

// ChatModel 调用 OpenAI 兼容的 chat completions 接口。
//
// 与 Embedder 同样只依赖 OpenAI 协议，因此 DashScope、DeepSeek、
// OpenAI 都能用同一份代码，切换只需改 base_url 和 model。
type ChatModel struct {
	apiKey  string
	baseURL string
	model   string
	// temperature 影响输出随机性。精排场景固定为 0：
	// 同样的输入必须给出同样的分数，否则每次评估结果都不同，
	// 无法判断改动是否真的带来提升。
	temperature float32
	client      *http.Client

	// up 负责「主上游 + 备用 + 熔断」。不在 complete 里直接 Do，
	// 是为了让 ChatModel 与 EinoChatModel 共享同一套容错行为。
	up *upstream

	// fallback/breaker 只用于构造 upstream，运行期不直接使用。
	fallback *Endpoint
	breaker  *Breaker
}

type ChatOption func(*ChatModel)

func WithTemperature(t float32) ChatOption {
	return func(c *ChatModel) { c.temperature = t }
}

func WithChatHTTPClient(h *http.Client) ChatOption {
	return func(c *ChatModel) {
		if h != nil {
			c.client = h
		}
	}
}

// WithChatFallback 配置备用上游（通常在 429 / 5xx 时接管）。
//
// 只换模型不换供应商也能起作用：DashScope 的 qwen-turbo 与
// qwen-plus 是独立的并发配额。
func WithChatFallback(ep Endpoint) ChatOption {
	return func(c *ChatModel) { c.fallback = &ep }
}

// WithChatBreaker 注入自定义熔断器（测试用）。
func WithChatBreaker(b *Breaker) ChatOption {
	return func(c *ChatModel) {
		if b != nil {
			c.breaker = b
		}
	}
}

func NewChatModel(apiKey, baseURL, model string, opts ...ChatOption) *ChatModel {
	c := &ChatModel{
		apiKey:      apiKey,
		baseURL:     baseURL,
		model:       model,
		temperature: 0,
		client:      &http.Client{Timeout: defaultChatTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.up == nil {
		c.up = newUpstream(Endpoint{APIKey: c.apiKey, BaseURL: c.baseURL, Model: c.model},
			c.fallback, c.breaker, c.client)
	}
	return c
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float32       `json:"temperature"`
	// MaxTokens 限制输出长度。精排只需要一个数字，
	// 给 16 足够，同时防止模型跑偏写长文浪费时间和配额。
	MaxTokens int `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Score 实现 retriever.Scorer，供精排使用。
//
// 限制输出长度是刻意的：精排只需要一个 0-10 的数字。
//
// ⚠️ 但预算不能太小：推理模型（如 deepseek-flash）会先输出
// reasoning_content，16 个 token 会被它吃光，content 为空、
// finish_reason=length —— 表现为「所有候选都解析不出分数」。
// 256 对普通模型没有额外成本（它们输出一个数字就 stop），
// 对推理模型则够用。
func (c *ChatModel) Score(ctx context.Context, prompt string) (string, error) {
	return c.complete(ctx, prompt, 256)
}

// Complete 发起一次不限长度的补全，供其他场景使用。
func (c *ChatModel) Complete(ctx context.Context, prompt string) (string, error) {
	return c.complete(ctx, prompt, 0)
}

func (c *ChatModel) complete(ctx context.Context, prompt string, maxTokens int) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    []chatMessage{{Role: "user", Content: prompt}},
		Temperature: c.temperature,
		MaxTokens:   maxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	// 统一走 upstream：熔断、429/5xx 切备用都在里面完成。
	resp, err := c.up.post(ctx, body, false)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("decode response: %w (raw: %s)", err, truncate(string(raw), 300))
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("api error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("response contained no choices")
	}
	return parsed.Choices[0].Message.Content, nil
}
