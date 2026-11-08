//go:build milvusprobe

// 探针：确认 Milvus 里到底有没有数据、能不能搜出来。
//
//	go run -tags=milvusprobe ./scripts/spike/milvusprobe/main.go
//
// 起因：重灌语料后 ingest_corpus 报告「77 篇 / 1307 chunks」且无报错，
// PG 侧也确实是 1307 行，但评估里 vector-only 的 Recall@5 是 0.000。
// 「入库成功」与「检索为空」两者必有一个在说谎 —— 直接问 Milvus 最快。
//
// 为什么不用现成的 eval.go 去查：那条链路经过 RRF、缓存、精排，
// 任何一环都可能吃掉结果。探针只做「embed 一个查询 → Search」两步，
// 排除一切中间环节。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yurin-kami/KahoAgent/internal/config"
	"github.com/yurin-kami/KahoAgent/pkg/llm"
	"github.com/yurin-kami/KahoAgent/pkg/vectorstore"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n❌ %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 与 ingest_corpus / eval 一致用 DefaultPath：
	//   KAHO_CONFIG=config.pdf.toml go run -tags=milvusprobe ...
	// 探针的意义就是核对「刚灌进去的那个库」，写死 config.toml
	// 会让它永远只验主语料 —— 而新灌的库恰恰是最需要验的那个。
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	fmt.Printf("collection = %q   dim = %d\n\n", cfg.Milvus.Collection, cfg.LLM.EmbeddingDim)

	store, err := vectorstore.NewMilvusStore(ctx, cfg.Milvus.Addr,
		cfg.Milvus.Collection, cfg.LLM.EmbeddingDim)
	if err != nil {
		return fmt.Errorf("connect milvus: %w", err)
	}
	defer store.Close()

	emb := llm.NewEmbedder(cfg.LLM.EmbeddingKey(), cfg.LLM.EmbeddingURL(),
		cfg.LLM.EmbeddingModel, cfg.LLM.EmbeddingDim)

	// ---- 双存储一致性核对 ----
	//
	// 这正是 ingest_corpus 缺的那道校验：它只查了 PG 行数，
	// 没查 Milvus。欠费期间 embedding 全部失败，脚本却照样报告
	// 「✅ 77 篇 / 1307 chunks / PostgreSQL: 1307 行」—— 双写场景
	// 只验证一路，等于没验证。
	pool, err := pgxpool.New(ctx, cfg.DB.DSN)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	var pgCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM chunks").Scan(&pgCount); err != nil {
		return fmt.Errorf("count pg: %w", err)
	}

	// Milvus 侧没有 Count API，改用抽样：取 PG 里的若干 chunk，
	// 用它们的内容反查 Milvus，看能不能命中自己。
	// 命中率低说明 Milvus 缺数据。
	rows, err := pool.Query(ctx,
		`SELECT id, content FROM chunks ORDER BY id LIMIT 20`)
	if err != nil {
		return fmt.Errorf("sample pg: %w", err)
	}
	type sample struct{ id, content string }
	var samples []sample
	for rows.Next() {
		var s sample
		if err := rows.Scan(&s.id, &s.content); err != nil {
			rows.Close()
			return err
		}
		samples = append(samples, s)
	}
	rows.Close()

	texts := make([]string, len(samples))
	for i, s := range samples {
		texts[i] = s.content
	}
	vecs, err := emb.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("embed samples: %w", err)
	}

	var hit int
	for i, v := range vecs {
		got, err := store.Search(ctx, v, 3)
		if err != nil {
			return fmt.Errorf("search sample %d: %w", i, err)
		}
		// 用自己的内容检索，自己必须排在前列
		for _, g := range got {
			if g.Chunk.ID == samples[i].id {
				hit++
				break
			}
		}
	}

	fmt.Printf("PostgreSQL 行数        : %d\n", pgCount)
	fmt.Printf("抽样自检（%d 条）      : %d 条能在 Milvus 中检索到自己\n",
		len(samples), hit)

	switch {
	case hit == len(samples):
		fmt.Println("✅ 两个存储一致")
	case hit == 0:
		fmt.Println("❌ Milvus 完全没有数据 —— 需要重新入库")
	default:
		fmt.Printf("⚠️  Milvus 数据不完整（%d/%d）—— 需要重新入库\n", hit, len(samples))
	}

	// ---- 语义检索连通性 ----
	qv, err := emb.Embed(ctx, []string{"缓存穿透怎么解决"})
	if err != nil {
		return fmt.Errorf("embed query: %w", err)
	}
	got, err := store.Search(ctx, qv[0], 5)
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}
	fmt.Printf("\n查询「缓存穿透怎么解决」命中 %d 条：\n", len(got))
	for _, g := range got {
		fmt.Printf("  %.4f  %-45s  %.36s\n", g.Score, g.Chunk.DocID, g.Chunk.Content)
	}
	return nil
}
