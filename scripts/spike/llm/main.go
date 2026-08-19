//go:build spike

// Spike S1：验证 LLM Chat API 连通性。
//
// 目的：证明 API Key 有效、base_url 正确、模型名存在。
// 这一步单独验证，出问题时能精确定位是凭证问题还是代码问题。
//
// 运行：go run -tags=spike scripts/spike/llm/main.go
// 成功标志：打印出模型的回复文本。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/yurin-kami/KahoAgent/internal/config"
)

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func main() {
	cfg, err := config.Load("config.toml")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	fmt.Printf("provider = %s\nbase_url = %s\nmodel    = %s\n\n",
		cfg.LLM.Provider, cfg.LLM.BaseURL, cfg.LLM.Model)

	body, _ := json.Marshal(chatRequest{
		Model: cfg.LLM.Model,
		Messages: []chatMessage{
			{Role: "user", Content: "用一句话说明什么是 RAG。"},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		cfg.LLM.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		log.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.LLM.APIKey)

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Fatalf("decode response: %v\nraw: %s", err, raw)
	}
	if out.Error != nil {
		log.Fatalf("API error: %s (%s)", out.Error.Message, out.Error.Type)
	}
	if len(out.Choices) == 0 {
		log.Fatalf("no choices in response: %s", raw)
	}

	fmt.Printf("✅ LLM 连通\n")
	fmt.Printf("回复: %s\n", out.Choices[0].Message.Content)
	fmt.Printf("耗时: %v | tokens: prompt=%d completion=%d\n",
		time.Since(start).Round(time.Millisecond),
		out.Usage.PromptTokens, out.Usage.CompletionTokens)
}
