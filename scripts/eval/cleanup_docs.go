//go:build cleanup

// cleanup_docs 按 docID 删除文档，同时清理 Milvus 与 PostgreSQL。
//
//	go run -tags=cleanup ./scripts/eval/cleanup_docs.go <docID> [docID...]
//	go run -tags=cleanup ./scripts/eval/cleanup_docs.go -stale   # 清理已知的失效 docID
//
// 为什么需要它：两个存储必须同步删除，否则 RRF 会融合到一条只存在于
// 单边的记录 —— 表现为检索结果里出现内容缺失或重复的条目。
// 走 DeleteByDoc 而非手写 SQL，保证与生产代码同一条路径。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yurin-kami/KahoAgent/internal/config"
	"github.com/yurin-kami/KahoAgent/internal/rag/retriever"
	"github.com/yurin-kami/KahoAgent/pkg/vectorstore"
)

// staleDocIDs 是 ingest_corpus 早期版本用纯 basename 生成的 docID。
//
// 那版实现让 en/ja 子目录下的同名文件互相覆盖，修复后改用相对路径
// （如 tools-getting-started），这些无前缀的旧 ID 便成了孤儿数据。
// 它们的内容已由带前缀的新记录覆盖，留着只会在检索时产生重复结果。
var staleDocIDs = []string{
	"astro-koharu-guide",
	"custom-keywords",
	"draft-example",
	"encrypted-post-demo",
	"getting-started",
	"hello-world",
	"infographic-guide",
	"markdown-features",
	"shoka-features",
	"theme-customization",
	"toc-no-numbering",
	"weekly-example-1",
}

func main() {
	stale := flag.Bool("stale", false, "清理早期 docID bug 遗留的孤儿文档")
	flag.Parse()

	docIDs := flag.Args()
	if *stale {
		docIDs = append(docIDs, staleDocIDs...)
	}
	if len(docIDs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: cleanup_docs [-stale] <docID>...")
		os.Exit(1)
	}

	if err := run(docIDs); err != nil {
		fmt.Fprintf(os.Stderr, "\n❌ %v\n", err)
		os.Exit(1)
	}
}

func run(docIDs []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cfg, err := config.Load("config.toml")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	store, err := vectorstore.NewMilvusStore(ctx, cfg.Milvus.Addr,
		cfg.Milvus.Collection, cfg.LLM.EmbeddingDim)
	if err != nil {
		return fmt.Errorf("connect milvus: %w", err)
	}
	defer store.Close()

	pool, err := pgxpool.New(ctx, cfg.DB.DSN)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	writer, err := retriever.NewChunkWriter(pool)
	if err != nil {
		return fmt.Errorf("new chunk writer: %w", err)
	}

	var before int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM chunks").Scan(&before); err != nil {
		return fmt.Errorf("count before: %w", err)
	}

	for _, docID := range docIDs {
		// 两个存储都删。任一失败即中止 —— 半删状态比不删更糟。
		if err := store.DeleteByDoc(ctx, docID); err != nil {
			return fmt.Errorf("milvus delete %s: %w", docID, err)
		}
		if err := writer.DeleteByDoc(ctx, docID); err != nil {
			return fmt.Errorf("pg delete %s: %w", docID, err)
		}
		fmt.Printf("已删除 %s\n", docID)
	}

	if err := store.Flush(ctx); err != nil {
		return fmt.Errorf("flush milvus: %w", err)
	}

	var after int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM chunks").Scan(&after); err != nil {
		return fmt.Errorf("count after: %w", err)
	}

	var docs int
	if err := pool.QueryRow(ctx, "SELECT count(DISTINCT doc_id) FROM chunks").Scan(&docs); err != nil {
		return fmt.Errorf("count docs: %w", err)
	}

	fmt.Printf("\n✅ 清理完成：删除 %d 行\n", before-after)
	fmt.Printf("   剩余 %d 篇文档 / %d chunks\n", docs, after)
	return nil
}
