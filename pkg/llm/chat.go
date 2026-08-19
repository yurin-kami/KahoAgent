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
func (c *ChatModel) Score(ctx context.Context, prompt string) (string, error) {
	return c.complete(ctx, prompt, 16)
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
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
