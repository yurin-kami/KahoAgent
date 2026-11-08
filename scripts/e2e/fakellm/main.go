//go:build e2echeck

// fakellm 是一个本地 OpenAI 兼容桩服务，供端到端测试使用。
//
// 为什么需要它：真机验收要验证的是「上传 → Kafka → worker → 双写 → 检索」
// 这条链路，而不是模型质量。用真实 API Key 会让验收依赖外部配额、
// 变慢且有费用；用一个确定性的 embedding 桩，链路该验证的一点不少：
//
//	HTTP 调用、批量、重试、维度校验、Milvus 写入、PG 写入、混合检索
//
// 注意：它只实现 embeddings（本项目入库用的），chat 仅返回固定文本，
// 不足以驱动 ReAct —— 对话质量验证需要真实模型。
package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"log"
	"math"
	"net/http"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18081", "监听地址")
	dim := flag.Int("dim", 1024, "向量维度")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type item struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		}
		resp := struct {
			Object string `json:"object"`
			Data   []item `json:"data"`
			Model  string `json:"model"`
		}{Object: "list", Model: req.Model, Data: make([]item, 0, len(req.Input))}

		for i, text := range req.Input {
			resp.Data = append(resp.Data, item{
				Object: "embedding", Index: i, Embedding: vector(text, *dim),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"fake"}}]}`))
	})

	log.Printf("fake llm listening on %s (dim=%d)", *addr, *dim)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

// vector 由文本哈希生成确定性向量并归一化。
//
// 确定性很重要：同一个 chunk 重跑入库必须得到同样的向量，
// 否则重复消费（至少一次语义）会写入两个不同的向量。
func vector(text string, dim int) []float64 {
	sum := sha256.Sum256([]byte(text))
	v := make([]float64, dim)
	var norm float64
	for i := range v {
		// 用哈希字节铺满整个维度，再映射到 [-1, 1]
		b := sum[i%len(sum)]
		x := float64(b)/127.5 - 1
		v[i] = x
		norm += x * x
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return v
	}
	for i := range v {
		v[i] /= norm
	}
	return v
}
