//go:build diagnose

// diagnose 排查 hybrid 为何未超过 bm25-only。
//
//	go run -tags=diagnose ./scripts/eval/diagnose.go
//
// 假设：golden set 由**关键词谓词**生成，而 BM25 也按关键词匹配 ——
// 两者用了同一套机制，标注天然偏向 BM25。若成立，
// 则「bm25 > hybrid」不代表 hybrid 更差，而是评估基准有偏。
//
// 验证方法：统计每条查询的相关块中，有多少能被 BM25 的分词直接命中。
// 若比例极高，说明 golden set 确实是「BM25 友好」的。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yurin-kami/KahoAgent/internal/config"
	"github.com/yurin-kami/KahoAgent/internal/eval"
	"github.com/yurin-kami/KahoAgent/internal/rag"
	"github.com/yurin-kami/KahoAgent/internal/rag/retriever"
	"github.com/yurin-kami/KahoAgent/pkg/llm"
	"github.com/yurin-kami/KahoAgent/pkg/vectorstore"
)

type goldenEntry struct {
	Query            string   `json:"query"`
	Kind             string   `json:"kind"`
	RelevantChunkIDs []string `json:"relevant_chunk_ids"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	raw, err := os.ReadFile("scripts/eval/golden_set.json")
	if err != nil {
		return err
	}
	var golden []goldenEntry
	if err := json.Unmarshal(raw, &golden); err != nil {
		return err
	}

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

	vec, _ := retriever.NewVectorRetriever(embedder, store)
	bm25, _ := retriever.NewBM25Retriever(pool)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	mk := func(rs ...retriever.Recaller) *retriever.HybridRetriever {
		h, _ := retriever.NewHybridRetriever(rs,
			retriever.WithLogger(quiet), retriever.WithRecallN(50), retriever.WithTopK(5))
		return h
	}
	vOnly, bOnly, hyb := mk(vec), mk(bm25), mk(vec, bm25)

	// ---- 诊断 1：召回层面谁更强（不看排序，只看 50 条里有没有） ----
	fmt.Println("=== 诊断 1：召回率对比（recall@50，衡量「能不能找到」）===")
	var vRec, bRec []float64
	for _, g := range golden {
		relevant := toSet(g.RelevantChunkIDs)
		vHits, _ := vec.Recall(ctx, g.Query, 50)
		bHits, _ := bm25.Recall(ctx, g.Query, 50)
		vRec = append(vRec, eval.RecallAtK(idsOf(vHits), relevant, 50))
		bRec = append(bRec, eval.RecallAtK(idsOf(bHits), relevant, 50))
	}
	fmt.Printf("vector recall@50 = %.3f\n", eval.Mean(vRec))
	fmt.Printf("bm25   recall@50 = %.3f\n", eval.Mean(bRec))
	fmt.Println()

	// ---- 诊断 2：逐查询看 hybrid 在哪些 case 上输给了 bm25 ----
	fmt.Println("=== 诊断 2：hybrid 输给 bm25-only 的查询 ===")
	type diff struct {
		query       string
		kind        string
		bmRR, hybRR float64
		vecRR       float64
	}
	var worse []diff

	for _, g := range golden {
		relevant := toSet(g.RelevantChunkIDs)

		vr, _ := vOnly.Search(ctx, g.Query)
		br, _ := bOnly.Search(ctx, g.Query)
		hr, _ := hyb.Search(ctx, g.Query)

		vRR := eval.ReciprocalRank(idsOfScored(vr), relevant)
		bRR := eval.ReciprocalRank(idsOfScored(br), relevant)
		hRR := eval.ReciprocalRank(idsOfScored(hr), relevant)

		if hRR < bRR {
			worse = append(worse, diff{g.Query, g.Kind, bRR, hRR, vRR})
		}
	}

	sort.Slice(worse, func(i, j int) bool {
		return (worse[i].bmRR - worse[i].hybRR) > (worse[j].bmRR - worse[j].hybRR)
	})

	fmt.Printf("共 %d/%d 条查询上 hybrid 不如 bm25-only\n\n", len(worse), len(golden))
	fmt.Printf("%-38s %-11s %6s %6s %6s\n", "Query", "Kind", "bm25", "hybrid", "vector")
	fmt.Println("--------------------------------------------------------------------------")
	for i, d := range worse {
		if i >= 15 {
			fmt.Printf("... 其余 %d 条略\n", len(worse)-15)
			break
		}
		fmt.Printf("%-38s %-11s %6.3f %6.3f %6.3f\n",
			truncate(d.query, 36), d.kind, d.bmRR, d.hybRR, d.vecRR)
	}

	// ---- 诊断 3：这些 case 里向量路的表现 ----
	var vecDrag int
	for _, d := range worse {
		if d.vecRR < d.bmRR {
			vecDrag++
		}
	}
	fmt.Printf("\n其中 %d/%d 条（%.0f%%）是向量路 RR 低于 bm25 —— "+
		"即 hybrid 被较弱的向量路拉低\n", vecDrag, len(worse),
		float64(vecDrag)/float64(len(worse))*100)

	return nil
}

func toSet(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func idsOf(hits []rag.ScoredChunk) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Chunk.ID
	}
	return out
}

func idsOfScored(hits []rag.ScoredChunk) []string { return idsOf(hits) }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
