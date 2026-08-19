//go:build spike

// Spike S2：验证 Embedding API 连通性与维度。
//
// 目的：
//  1. 证明 embedding 接口可用
//  2. 确认实际输出维度，用于校准 config 里的 embedding_dim
//     —— 维度配错会导致 Phase 1 建 Milvus collection 时插入失败
//  3. 顺便验证「语义相近的文本向量距离也近」这一 RAG 基本假设
//
// 运行：go run -tags=spike scripts/spike/embed/main.go
// 成功标志：打印出维度，且「猫/狗」相似度明显高于「猫/汽车」。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/yurin-kami/KahoAgent/internal/config"
)

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
	} `json:"error"`
}

func main() {
	cfg, err := config.Load("config.toml")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	fmt.Printf("embedding_model = %s\nconfigured dim  = %d\n\n",
		cfg.LLM.EmbeddingModel, cfg.LLM.EmbeddingDim)

	texts := []string{"猫", "狗", "汽车"}

	body, _ := json.Marshal(embedRequest{
		Model: cfg.LLM.EmbeddingModel,
		Input: texts,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		cfg.LLM.BaseURL+"/embeddings", bytes.NewReader(body))
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

	var out embedResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Fatalf("decode: %v\nraw: %s", err, raw)
	}
	if out.Error != nil {
		log.Fatalf("API error: %s", out.Error.Message)
	}
	if len(out.Data) != len(texts) {
		log.Fatalf("expected %d embeddings, got %d", len(texts), len(out.Data))
	}

	dim := len(out.Data[0].Embedding)
	fmt.Printf("✅ Embedding 连通\n")
	fmt.Printf("实际维度: %d  (耗时 %v)\n", dim, time.Since(start).Round(time.Millisecond))

	if dim != cfg.LLM.EmbeddingDim {
		fmt.Printf("\n⚠️  配置的 embedding_dim=%d 与实际 %d 不符！\n", cfg.LLM.EmbeddingDim, dim)
		fmt.Printf("    请修改 config.toml 中的 embedding_dim = %d\n", dim)
		fmt.Printf("    否则 Phase 1 建 Milvus collection 时会插入失败。\n")
	}

	// 验证 RAG 的基本假设：语义相近 → 向量距离近（L0.2 学过）
	fmt.Printf("\n余弦相似度验证:\n")
	fmt.Printf("  猫 vs 狗   = %.4f  (都是宠物，应较高)\n",
		cosine(out.Data[0].Embedding, out.Data[1].Embedding))
	fmt.Printf("  猫 vs 汽车 = %.4f  (无关，应较低)\n",
		cosine(out.Data[0].Embedding, out.Data[2].Embedding))
}

// cosine 计算余弦相似度：dot(a,b) / (|a| * |b|)，值域 [-1, 1]。
func cosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
