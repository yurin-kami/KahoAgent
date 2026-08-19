//go:build corpus

// ingest_corpus 把一个目录下的 Markdown 批量入库到 Milvus + PostgreSQL。
//
// 用于 Phase 2 评估：golden set 必须建立在**真实文档**上，
// 否则算出的 Recall/MRR 只是自娱自乐的数字。
//
//	go run -tags=corpus ./scripts/eval/ingest_corpus.go <dir>
//
// 两个存储都写：Milvus 存向量（语义召回），PG 存词元（关键词召回）。
// 混合检索的前提就是两条独立的召回路径。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yurin-kami/KahoAgent/internal/config"
	"github.com/yurin-kami/KahoAgent/internal/rag"
	"github.com/yurin-kami/KahoAgent/internal/rag/chunker"
	"github.com/yurin-kami/KahoAgent/internal/rag/retriever"
	"github.com/yurin-kami/KahoAgent/pkg/llm"
	"github.com/yurin-kami/KahoAgent/pkg/vectorstore"
)

func main() {
	var (
		dir     = flag.String("dir", "", "要入库的目录")
		reset   = flag.Bool("reset", false, "入库前清空两个存储")
		dryRun  = flag.Bool("dry-run", false, "只分块统计，不调用 embedding API")
		maxDocs = flag.Int("max-docs", 0, "最多处理几篇（0 = 不限）")
	)
	flag.Parse()

	if *dir == "" && flag.NArg() > 0 {
		*dir = flag.Arg(0)
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "usage: ingest_corpus [-reset] [-dry-run] <dir>")
		os.Exit(1)
	}

	if err := run(*dir, *reset, *dryRun, *maxDocs); err != nil {
		fmt.Fprintf(os.Stderr, "\n❌ %v\n", err)
		os.Exit(1)
	}
}

func run(dir string, reset, dryRun bool, maxDocs int) error {
	// 入库耗时随文档数线性增长，超时给足
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	cfg, err := config.Load("config.toml")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	files, err := findMarkdown(dir)
	if err != nil {
		return err
	}
	if maxDocs > 0 && len(files) > maxDocs {
		files = files[:maxDocs]
	}
	if len(files) == 0 {
		return fmt.Errorf("no .md files found under %s", dir)
	}
	fmt.Printf("发现 %d 篇文档\n\n", len(files))

	mdChunker := chunker.NewMarkdownChunker(chunker.DefaultSize)

	// ---- dry-run：只分块，不花钱 ----
	if dryRun {
		return reportChunking(ctx, mdChunker, files)
	}

	// ---- 组装两个存储 ----
	embedder := llm.NewEmbedder(cfg.LLM.APIKey, cfg.LLM.BaseURL,
		cfg.LLM.EmbeddingModel, cfg.LLM.EmbeddingDim)

	store, err := vectorstore.NewMilvusStore(ctx, cfg.Milvus.Addr,
		cfg.Milvus.Collection, cfg.LLM.EmbeddingDim)
	if err != nil {
		return fmt.Errorf("connect milvus: %w", err)
	}
	defer store.Close()
	if err := store.EnsureCollection(ctx); err != nil {
		return fmt.Errorf("ensure collection: %w", err)
	}

	pool, err := pgxpool.New(ctx, cfg.DB.DSN)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()
	if err := applyMigration(ctx, pool); err != nil {
		return err
	}

	writer, err := retriever.NewChunkWriter(pool)
	if err != nil {
		return fmt.Errorf("new chunk writer: %w", err)
	}

	if reset {
		fmt.Println("清空现有数据...")
		if _, err := pool.Exec(ctx, "TRUNCATE chunks"); err != nil {
			return fmt.Errorf("truncate pg: %w", err)
		}
		if err := store.DropCollection(ctx); err != nil {
			return fmt.Errorf("drop milvus collection: %w", err)
		}
		if err := store.EnsureCollection(ctx); err != nil {
			return fmt.Errorf("recreate collection: %w", err)
		}
		fmt.Println()
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelWarn}))
	_ = logger

	start := time.Now()
	var totalChunks int

	for i, path := range files {
		docID := docIDFor(dir, path)

		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		// 剥掉 frontmatter，但把 title 提升为一级标题 ——
		// 标题往往是文档最核心的语义信息，丢掉会明显降低召回质量
		body := stripFrontmatter(raw)

		chunks, err := mdChunker.Chunk(ctx, docID, body)
		if err != nil {
			return fmt.Errorf("chunk %s: %w", path, err)
		}
		if len(chunks) == 0 {
			fmt.Printf("[%2d/%d] %-55s 跳过（无内容）\n", i+1, len(files), truncate(docID, 55))
			continue
		}

		// 记录来源文件，供评估时人工核对
		for j := range chunks {
			chunks[j].Metadata["source"] = filepath.Base(path)
		}

		texts := make([]string, len(chunks))
		for j, c := range chunks {
			texts[j] = c.Content
		}
		vectors, err := embedder.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("embed %s: %w", path, err)
		}

		if err := store.Upsert(ctx, chunks, vectors); err != nil {
			return fmt.Errorf("milvus upsert %s: %w", path, err)
		}
		if err := writer.Upsert(ctx, chunks); err != nil {
			return fmt.Errorf("pg upsert %s: %w", path, err)
		}

		totalChunks += len(chunks)
		fmt.Printf("[%2d/%d] %-55s %3d chunks\n", i+1, len(files), truncate(docID, 55), len(chunks))
	}

	// 统一 Flush 一次，而非每篇都 Flush —— 后者会严重拖慢入库
	if err := store.Flush(ctx); err != nil {
		return fmt.Errorf("flush milvus: %w", err)
	}

	elapsed := time.Since(start)
	fmt.Printf("\n✅ 入库完成：%d 篇 / %d chunks / 耗时 %s\n",
		len(files), totalChunks, elapsed.Round(time.Second))

	// 校验两个存储的数量一致 —— 不一致意味着 RRF 融合时会有一路查不到
	var pgCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM chunks").Scan(&pgCount); err != nil {
		return fmt.Errorf("count pg: %w", err)
	}
	fmt.Printf("   PostgreSQL: %d 行\n", pgCount)
	switch {
	case pgCount < totalChunks:
		// 少于写入数说明有 chunk 被同 ID 覆盖了 —— 通常是 docID 冲突。
		// 这是数据丢失，不是残留，必须排查。
		fmt.Printf("   ⚠️  PG 行数比本次写入少 %d —— 存在 ID 冲突导致的覆盖，请检查 docID 生成逻辑\n",
			totalChunks-pgCount)
	case pgCount > totalChunks:
		fmt.Printf("   ℹ️  PG 行数比本次写入多 %d —— 有历史残留，需要全新语料请用 -reset\n",
			pgCount-totalChunks)
	}
	return nil
}

// reportChunking 只做分块统计，用于估算成本和检查分块质量。
func reportChunking(ctx context.Context, c rag.Chunker, files []string) error {
	var total, totalRunes int
	sizes := make([]int, 0, 512)

	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		chunks, err := c.Chunk(ctx, filepath.Base(path), stripFrontmatter(raw))
		if err != nil {
			return fmt.Errorf("chunk %s: %w", path, err)
		}
		total += len(chunks)
		for _, ch := range chunks {
			n := len([]rune(ch.Content))
			totalRunes += n
			sizes = append(sizes, n)
		}
	}

	fmt.Printf("dry-run 统计：\n")
	fmt.Printf("  文档数    : %d\n", len(files))
	fmt.Printf("  chunk 数  : %d\n", total)
	if total > 0 {
		fmt.Printf("  平均长度  : %d 字符\n", totalRunes/total)
	}
	// text-embedding-v3 按 token 计费，中文约 1 字符 ≈ 1 token
	fmt.Printf("  预估 token: ~%d（embedding 调用）\n", totalRunes)
	return nil
}

func findMarkdown(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// 跳过依赖和构建产物目录，它们不是知识内容
			switch d.Name() {
			case "node_modules", ".git", "dist", ".astro", ".cache":
				return filepath.SkipDir
			}
			return nil
		}
		if ext := strings.ToLower(filepath.Ext(path)); ext == ".md" || ext == ".mdx" {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

// docIDFor 由相对路径生成稳定且长度受限的 docID。
//
// ⚠️ 必须用相对路径而非纯文件名。语料里 en/ja 子目录下有同名文件
// （getting-started、hello-world 等各出现 3 次），只用 basename 会让
// 它们撞成同一个 docID，后写的静默覆盖先写的 —— 实测因此丢了 62 个 chunk。
//
// Milvus 的 doc_id 字段上限 64 字节，而带目录的中文路径极易超出，
// 故超长时改用「前缀 + 全路径哈希」保证既唯一又不超限。
func docIDFor(root, path string) string {
	const maxBytes = 60

	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = filepath.Base(path)
	}
	rel = strings.TrimSuffix(filepath.ToSlash(rel), filepath.Ext(rel))
	// 目录分隔符换成短横线，便于阅读
	name := strings.ReplaceAll(rel, "/", "-")

	if len(name) <= maxBytes {
		return name
	}

	// 超长：保留可读前缀 + 全路径哈希，避免截断导致的碰撞
	sum := sha256.Sum256([]byte(rel))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]

	cut := maxBytes - len(suffix)
	b := []byte(name)
	if cut > len(b) {
		cut = len(b)
	}
	// 回退到字符边界，避免切出非法 UTF-8
	for cut > 0 && b[cut]&0xC0 == 0x80 {
		cut--
	}
	return string(b[:cut]) + suffix
}

// stripFrontmatter 移除 YAML frontmatter，并把 title 提升为一级标题。
//
// 为什么保留 title：它常是全文最核心的语义概括
// （如「缓存穿透、击穿、雪崩怎么区分」），丢掉会让该文档的召回质量明显下降。
// 同时它会成为 MarkdownChunker 的 heading_path 根节点，
// 让每个 chunk 都带上来源主题。
func stripFrontmatter(raw []byte) []byte {
	const sep = "---"
	text := string(raw)
	if !strings.HasPrefix(text, sep) {
		return raw
	}

	// 找到第二个 --- 分隔符
	rest := text[len(sep):]
	end := strings.Index(rest, "\n"+sep)
	if end < 0 {
		return raw
	}
	front := rest[:end]
	body := rest[end+len("\n"+sep):]

	// 从 frontmatter 里抽 title
	var title string
	for _, line := range strings.Split(front, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "title:"); ok {
			title = strings.Trim(strings.TrimSpace(v), `"'`)
			break
		}
	}

	body = strings.TrimLeft(body, "\r\n")
	if title == "" {
		return []byte(body)
	}
	return []byte("# " + title + "\n\n" + body)
}

func applyMigration(ctx context.Context, pool *pgxpool.Pool) error {
	sql, err := os.ReadFile("migrations/001_chunks.sql")
	if err != nil {
		return fmt.Errorf("read migration: %w", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		return fmt.Errorf("apply migration: %w", err)
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
