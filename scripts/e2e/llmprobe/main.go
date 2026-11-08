//go:build e2echeck

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/yurin-kami/KahoAgent/internal/config"
	"github.com/yurin-kami/KahoAgent/pkg/llm"
)

func main() {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		fmt.Println("config:", err)
		return
	}
	fmt.Println("base:", cfg.LLM.BaseURL, "| model:", cfg.LLM.Model)
	chat := llm.NewChatModel(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := chat.Complete(ctx, "hi")
	fmt.Printf("out=%q err=%v\n", out, err)
}
