//go:build weightsweep

// weight_sweep 验证「RRF 的一致性加分在两路质量不对称时会反噬」这一假设，
// 并寻找最优权重。
//
//	go run -tags=weightsweep ./scripts/eval/weight_sweep.go
//
// 背景：诊断显示两路 recall@50 接近（0.707 / 0.740），说明都能**找到**
// 相关块，问题出在**排序**。具体机制：
//
//	某块被 bm25 排第 1、但不在 vector 的 50 条里 → 得分 1/61 = 0.0164
//	某块被两路都排第 10                      → 得分 2/70 = 0.0286  ← 反而更高
//
// 于是「单路强命中」输给「双路平庸」。加权 RRF 可缓解：
//
//	score(d) = Σ wᵢ / (k + rankᵢ(d))
//
// 本脚本扫描权重组合，用**同一个 golden set** 对比，
// 结果记入 docs/eval-results.md。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
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
	_ = quiet

	// 预先缓存每条查询的两路召回结果，避免每种权重都重跑一次检索 ——
	// 77 条查询 × 8 种权重 = 616 次 embedding 调用，缓存后只需 77 次。
	fmt.Printf("预取 %d 条查询的两路召回...\n", len(golden))
	type recalled struct {
		vecIDs, bmIDs []string
		relevant      map[string]bool
		kind          string
	}
	cached := make([]recalled, 0, len(golden))

	for i, g := range golden {
		vHits, err := vec.Recall(ctx, g.Query, 50)
		if err != nil {
			return fmt.Errorf("vector recall %q: %w", g.Query, err)
		}
		bHits, err := bm25.Recall(ctx, g.Query, 50)
		if err != nil {
			return fmt.Errorf("bm25 recall %q: %w", g.Query, err)
		}

		rel := make(map[string]bool, len(g.RelevantChunkIDs))
		for _, id := range g.RelevantChunkIDs {
			rel[id] = true
		}
		cached = append(cached, recalled{
			vecIDs: idsOf(vHits), bmIDs: idsOf(bHits),
			relevant: rel, kind: g.Kind,
		})
		if (i+1)%20 == 0 {
			fmt.Printf("  %d/%d\n", i+1, len(golden))
		}
	}
	fmt.Println()

	// 权重组合。vector:bm25，含两个单路极端作为参照。
	weights := []struct{ v, b float64 }{
		{0, 1},     // 纯 bm25
		{0.3, 1.0}, //
		{0.5, 1.0}, //
		{0.7, 1.0}, //
		{1.0, 1.0}, // 等权（当前实现）
		{1.0, 0.7}, //
		{1.0, 0.5}, //
		{1.0, 0.3}, //
		{1, 0},     // 纯 vector
	}

	fmt.Printf("%-14s %-9s %-8s %-12s %-10s %-10s\n",
		"vector:bm25", "Recall@5", "MRR", "confusable", "semantic", "keyword")
	fmt.Println("--------------------------------------------------------------------------")

	type row struct {
		label       string
		recall, mrr float64
		byKind      map[string]float64
	}
	var rows []row

	for _, w := range weights {
		var recalls, rrs []float64
		byKind := map[string][]float64{}

		for _, c := range cached {
			fused := fuseWeighted(c.vecIDs, c.bmIDs, w.v, w.b)
			if len(fused) > 5 {
				fused = fused[:5]
			}
			recalls = append(recalls, eval.RecallAtK(fused, c.relevant, 5))
			rr := eval.ReciprocalRank(fused, c.relevant)
			rrs = append(rrs, rr)
			byKind[c.kind] = append(byKind[c.kind], rr)
		}

		bk := map[string]float64{}
		for k, v := range byKind {
			bk[k] = eval.MRR(v)
		}

		label := fmt.Sprintf("%.1f:%.1f", w.v, w.b)
		rows = append(rows, row{label, eval.Mean(recalls), eval.MRR(rrs), bk})

		fmt.Printf("%-14s %-9.3f %-8.3f %-12.3f %-10.3f %-10.3f\n",
			label, eval.Mean(recalls), eval.MRR(rrs),
			bk["confusable"], bk["semantic"], bk["keyword"])
	}

	// 找出 MRR 最高的组合
	best := rows[0]
	for _, r := range rows[1:] {
		if r.mrr > best.mrr {
			best = r
		}
	}
	fmt.Printf("\n最优权重：%s（MRR %.3f）\n", best.label, best.mrr)
	return nil
}

// fuseWeighted 是加权 RRF：score(d) = Σ wᵢ/(k + rankᵢ(d))
func fuseWeighted(vecIDs, bmIDs []string, wVec, wBM float64) []string {
	const k = 60
	scores := map[string]float64{}

	accumulate := func(ids []string, w float64) {
		if w == 0 {
			return
		}
		seen := map[string]bool{}
		for rank, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			scores[id] += w / float64(k+rank+1)
		}
	}
	accumulate(vecIDs, wVec)
	accumulate(bmIDs, wBM)

	type kv struct {
		id string
		s  float64
	}
	arr := make([]kv, 0, len(scores))
	for id, s := range scores {
		arr = append(arr, kv{id, s})
	}
	// 分数降序；并列按 ID 升序保证可复现
	for i := 1; i < len(arr); i++ {
		for j := i; j > 0; j-- {
			if arr[j].s > arr[j-1].s || (arr[j].s == arr[j-1].s && arr[j].id < arr[j-1].id) {
				arr[j], arr[j-1] = arr[j-1], arr[j]
			} else {
				break
			}
		}
	}
	out := make([]string, len(arr))
	for i, e := range arr {
		out[i] = e.id
	}
	return out
}

func idsOf(hits []rag.ScoredChunk) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Chunk.ID
	}
	return out
}
