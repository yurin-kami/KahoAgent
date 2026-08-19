//go:build ingestcheck

// ingest_check 是 Phase 1 的端到端人工验收脚本。
//
// 它是唯一一处让**真实**的 chunker → DashScope embedding → Milvus 串起来跑的
// 地方：集成测试用的是 mock embedder（省钱且快），因此「配置的维度是否与
// 真实模型输出一致」这类问题只有在这里才会暴露。
//
//	go run -tags=ingestcheck ./scripts/eval/ingest_check.go docs/sample.md
//
// 用 build tag 隔离，避免 main 函数与其他脚本冲突。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yurin-kami/KahoAgent/internal/config"
	"github.com/yurin-kami/KahoAgent/internal/pipeline"
	"github.com/yurin-kami/KahoAgent/internal/rag/chunker"
	"github.com/yurin-kami/KahoAgent/pkg/llm"
	"github.com/yurin-kami/KahoAgent/pkg/vectorstore"
)

// checkCollection 独立于生产 collection，避免验收脚本污染真实数据。
const checkCollection = "phase1_ingest_check"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ingest_check <file.md>")
		os.Exit(1)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "\n❌ FAILED: %v\n", err)
		os.Exit(1)
	}
}

func run(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfg, err := config.Load("config.toml")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	docID := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))

	// 组装真实管道
	embedder := llm.NewEmbedder(cfg.LLM.APIKey, cfg.LLM.BaseURL,
		cfg.LLM.EmbeddingModel, cfg.LLM.EmbeddingDim)

	store, err := vectorstore.NewMilvusStore(ctx, cfg.Milvus.Addr,
		checkCollection, cfg.LLM.EmbeddingDim)
	if err != nil {
		return fmt.Errorf("connect milvus: %w", err)
	}
	defer store.Close()

	if err := store.EnsureCollection(ctx); err != nil {
		return fmt.Errorf("ensure collection: %w", err)
	}
	// 每次从干净状态开始，否则上一次的残留会让计数断言失去意义
	if err := store.DeleteByDoc(ctx, docID); err != nil {
		return fmt.Errorf("pre-clean: %w", err)
	}
	if err := store.Flush(ctx); err != nil {
		return fmt.Errorf("pre-clean flush: %w", err)
	}

	ing, err := pipeline.NewIngester(
		chunker.NewMarkdownChunker(chunker.DefaultSize),
		embedder, store,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	)
	if err != nil {
		return fmt.Errorf("build ingester: %w", err)
	}

	fmt.Printf("=== Phase 1 端到端验收：%s ===\n\n", path)

	// ---- 检查 1：入库 ----
	n, err := ing.Run(ctx, docID, raw)
	if err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	if err := store.Flush(ctx); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	fmt.Printf("[1] 入库成功：%d 个 chunk\n", n)

	// ---- 检查 2：检索能命中，且 doc_id 匹配 ----
	qVec, err := embedder.Embed(ctx, []string{"怎么防止缓存被恶意请求打穿"})
	if err != nil {
		return fmt.Errorf("embed query: %w", err)
	}
	hits, err := store.Search(ctx, qVec[0], 3)
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}
	if len(hits) == 0 {
		return fmt.Errorf("检索无结果，入库可能未生效")
	}
	for _, h := range hits {
		if h.Chunk.DocID != docID {
			return fmt.Errorf("doc_id 不匹配：期望 %s，得到 %s", docID, h.Chunk.DocID)
		}
	}
	fmt.Printf("[2] 检索命中 %d 条，doc_id 全部匹配\n", len(hits))
	fmt.Printf("    Top1  score=%.4f  path=%q\n",
		hits[0].Score, hits[0].Chunk.Metadata[chunker.MetaHeadingPath])
	fmt.Printf("    内容：%s\n", preview(hits[0].Chunk.Content, 60))

	// 语义检索的真正价值：query 里没有「布隆过滤器」四个字，
	// 但应该能召回讲布隆过滤器的那一段
	if !strings.Contains(hits[0].Chunk.Content, "穿透") &&
		!strings.Contains(hits[0].Chunk.Content, "布隆") {
		fmt.Printf("    ⚠️  Top1 与查询语义关联较弱，建议人工确认\n")
	}

	// ---- 检查 3：中文无乱码 ----
	for _, h := range hits {
		if strings.ContainsRune(h.Chunk.Content, '�') {
			return fmt.Errorf("chunk %s 含替换字符，中文被切坏", h.Chunk.ID)
		}
	}
	fmt.Printf("[3] 中文验证：取回内容无乱码\n")

	// ---- 检查 4：heading_path 正确 ----
	var withPath int
	var sample string
	for _, h := range hits {
		if p := h.Chunk.Metadata[chunker.MetaHeadingPath]; p != "" {
			withPath++
			if strings.Contains(p, " > ") && sample == "" {
				sample = p
			}
		}
	}
	if withPath == 0 {
		return fmt.Errorf("所有 chunk 的 heading_path 都为空")
	}
	fmt.Printf("[4] heading_path：%d/%d 条有路径", withPath, len(hits))
	if sample != "" {
		fmt.Printf("，嵌套示例 %q", sample)
	}
	fmt.Println()

	// ---- 检查 5：幂等 ----
	n2, err := ing.Run(ctx, docID, raw)
	if err != nil {
		return fmt.Errorf("second ingest: %w", err)
	}
	if err := store.Flush(ctx); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	if n2 != n {
		return fmt.Errorf("幂等性破坏：两次入库块数不同（%d vs %d）", n, n2)
	}
	// 块数相同还不够 —— 要确认库里没有翻倍。用一个宽的 topK 数实际行数。
	all, err := store.Search(ctx, qVec[0], n*2+10)
	if err != nil {
		return fmt.Errorf("count search: %w", err)
	}
	if len(all) != n {
		return fmt.Errorf("幂等性破坏：重复入库后库中有 %d 条，期望 %d 条", len(all), n)
	}
	fmt.Printf("[5] 幂等验证：重复入库后仍为 %d 条，未堆积\n", n)

	// ---- 检查 6：删除 ----
	if err := ing.Delete(ctx, docID); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if err := store.Flush(ctx); err != nil {
		return fmt.Errorf("flush after delete: %w", err)
	}
	left, err := store.Search(ctx, qVec[0], 10)
	if err != nil {
		return fmt.Errorf("search after delete: %w", err)
	}
	for _, h := range left {
		if h.Chunk.DocID == docID {
			return fmt.Errorf("删除后仍能查到 %s 的 chunk", docID)
		}
	}
	fmt.Printf("[6] 删除验证：该文档 chunk 已全部清除\n")

	fmt.Printf("\n✅ Phase 1 端到端验收全部通过\n")
	return nil
}

func preview(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
