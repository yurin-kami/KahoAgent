//go:build backlogcheck

// backlog_check 验证 Phase 1 遗留的那个具体失败案例是否被 Phase 2 解决。
//
//	go run -tags=backlogcheck ./scripts/eval/backlog_check.go
//
// Phase 1 的端到端检查里，查询「怎么防止缓存被恶意请求打穿」的 Top1
// 返回了**缓存击穿**（score 0.7096），而正确答案是**缓存穿透**。
// 当时把它记进 BACKLOG，作为「纯向量检索分不开近义概念」的实证，
// 也是引入 BM25 + RRF + 精排的直接依据。
//
// 现在四种策略都实现了，就该回来验证这个**具体的**案例 ——
// 而不是只看聚合指标涨了就宣布问题解决。聚合指标会平均掉个例，
// 一个总分上涨的系统完全可能在原来那道题上仍然错。
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yurin-kami/KahoAgent/internal/config"
	"github.com/yurin-kami/KahoAgent/internal/rag"
	"github.com/yurin-kami/KahoAgent/internal/rag/retriever"
	"github.com/yurin-kami/KahoAgent/pkg/llm"
	"github.com/yurin-kami/KahoAgent/pkg/vectorstore"
)

// 就是 Phase 1 里失败的那条查询
const backlogQuery = "怎么防止缓存被恶意请求打穿"

// 正确答案必须讲「穿透」及其解法；返回「击穿」就是 Phase 1 的错误重现
const (
	wantConcept  = "穿透"
	wrongConcept = "击穿"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n❌ %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cfg, err := config.Load("config.toml")
	if err != nil {
		return err
	}
	embedder := llm.NewEmbedder(cfg.LLM.APIKey, cfg.LLM.BaseURL,
		cfg.LLM.EmbeddingModel, cfg.LLM.EmbeddingDim)
	store, err := vectorstore.NewMilvusStore(ctx, cfg.Milvus.Addr,
		cfg.Milvus.Collection, cfg.LLM.EmbeddingDim)
	if err != nil {
		return err
	}
	defer store.Close()
	pool, err := pgxpool.New(ctx, cfg.DB.DSN)
	if err != nil {
		return err
	}
	defer pool.Close()

	vec, err := retriever.NewVectorRetriever(embedder, store)
	if err != nil {
		return err
	}
	bm25, err := retriever.NewBM25Retriever(pool)
	if err != nil {
		return err
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	mk := func(rs []retriever.Recaller, opts ...retriever.HybridOption) *retriever.HybridRetriever {
		base := []retriever.HybridOption{
			retriever.WithLogger(quiet), retriever.WithRecallN(50), retriever.WithTopK(5),
		}
		h, _ := retriever.NewHybridRetriever(rs, append(base, opts...)...)
		return h
	}

	chat := llm.NewChatModel(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model)
	rr, err := retriever.NewLLMReranker(chat, quiet)
	if err != nil {
		return err
	}

	strategies := []struct {
		name string
		h    *retriever.HybridRetriever
	}{
		{"vector-only", mk([]retriever.Recaller{vec})},
		{"bm25-only", mk([]retriever.Recaller{bm25})},
		{"hybrid-rrf", mk([]retriever.Recaller{vec, bm25})},
		{"hybrid+rerank", mk([]retriever.Recaller{vec, bm25}, retriever.WithRerank(rr))},
	}

	fmt.Printf("查询：%s\n", backlogQuery)
	fmt.Printf("正确答案应讲「缓存%s」，Phase 1 时返回的是「缓存%s」\n\n",
		wantConcept, wrongConcept)

	fmt.Printf("%-16s %-8s %s\n", "策略", "Top1", "Top1 内容摘要")
	fmt.Println(strings.Repeat("-", 84))

	var rerankOK bool
	for _, s := range strategies {
		hits, err := s.h.Search(ctx, backlogQuery)
		if err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		if len(hits) == 0 {
			fmt.Printf("%-16s %-8s （无结果）\n", s.name, "—")
			continue
		}

		verdict, ok := judge(hits[0])
		fmt.Printf("%-16s %-8s %s\n", s.name, verdict, summarize(hits[0]))

		if s.name == "hybrid+rerank" {
			rerankOK = ok
		}
	}

	fmt.Println()
	if !rerankOK {
		return fmt.Errorf("完整链路（hybrid+rerank）在 Phase 1 的遗留案例上**仍然失败**。\n" +
			"聚合指标上涨不代表这个具体问题被解决 —— 需要继续排查")
	}
	fmt.Printf("✅ Phase 1 遗留案例已解决：完整链路 Top1 正确讲「缓存%s」\n", wantConcept)
	return nil
}

// judge 判断 Top1 是对是错。
//
// ⚠️ 两个都踩过的坑：
//
//  1. **只看 content 会漏判**。正确的块标题是
//     「二、缓存穿透：布隆过滤器 + 缓存空值」，正文只写「方案一：缓存空值……」
//     —— 概念名在标题里，正文不重复。只匹配正文会把最理想的命中判成「无关」。
//
//  2. **看整条 heading_path 会失去区分度**。这篇文章的标题本身是
//     「缓存穿透、击穿、雪崩怎么区分和解决」，三个词全在里面 ——
//     于是任何一个块都同时匹配「穿透」和「击穿」，所有策略都得「混合」，
//     判定等于没做。
//
// 所以取 heading_path 的**最后一段**（最具体的那级小标题）加正文：
// 小标题才是标识这一节讲什么概念的地方。
func judge(c rag.ScoredChunk) (string, bool) {
	text := leafHeading(c.Chunk.Metadata["heading_path"]) + "\n" + c.Chunk.Content

	has := strings.Contains(text, wantConcept)
	hasWrong := strings.Contains(text, wrongConcept)

	switch {
	case has && !hasWrong:
		return "✅ 正确", true
	case has && hasWrong:
		// 同时出现，多半是「三者对比」型段落。
		// 不算错（确实讲了穿透），但也不是干净命中，标出来人工看。
		return "⚠️ 混合", true
	case hasWrong:
		return "❌ 击穿", false // Phase 1 的错误重现
	default:
		return "❌ 无关", false
	}
}

// leafHeading 取 "A > B > C" 里的 C —— 最具体的那级标题。
func leafHeading(path string) string {
	if i := strings.LastIndex(path, ">"); i >= 0 {
		return strings.TrimSpace(path[i+1:])
	}
	return path
}

func summarize(c rag.ScoredChunk) string {
	path := c.Chunk.Metadata["heading_path"]
	body := strings.ReplaceAll(c.Chunk.Content, "\n", " ")
	r := []rune(body)
	if len(r) > 40 {
		body = string(r[:40]) + "…"
	}
	if path != "" {
		return fmt.Sprintf("[%s] %s", path, body)
	}
	return body
}
