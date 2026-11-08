//go:build goldendraft

// draft_golden 为 golden set 起草候选标注。
//
// 做法：对每个查询跑一次混合检索，把 top-N 结果连同内容摘要输出，
// 由人工勾选哪些确实回答了问题。
//
//	go run -tags=goldendraft ./scripts/eval/draft_golden.go > /tmp/draft.md
//
// ⚠️ 为什么不让程序自动判定相关性：那等于用检索结果当标准答案来
// 评估检索本身，是循环论证 —— 评出来的分数必然虚高。
// 程序只负责把候选摆出来，「相关与否」必须人来定。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yurin-kami/KahoAgent/internal/config"
	"github.com/yurin-kami/KahoAgent/internal/rag/retriever"
	"github.com/yurin-kami/KahoAgent/pkg/llm"
	"github.com/yurin-kami/KahoAgent/pkg/vectorstore"
)

// 候选查询。覆盖三类，确保评估能区分出不同检索策略的强弱：
//
//	A. 语义类 —— 用口语提问，与原文用词不同。向量检索应占优。
//	B. 关键词类 —— 含专有名词、API 名。BM25 应占优。
//	C. 近义混淆类 —— 概念高度相似但答案不同。**两路都容易错，
//	   最能体现 RRF + ReRank 的价值**（Phase 1 实测栽在这里）。
var queries = []struct {
	Query string
	Kind  string
	Note  string
}{
	// --- C 类：近义混淆（最重要，先列） ---
	{"怎么防止缓存被恶意请求打穿", "confusable", "应命中缓存穿透而非击穿 —— Phase 1 实测失败的用例"},
	{"热点key突然失效导致数据库压力暴增怎么办", "confusable", "应命中缓存击穿而非穿透"},
	{"大量缓存同时过期怎么处理", "confusable", "应命中缓存雪崩"},
	{"MySQL 里读到别的事务没提交的数据是什么问题", "confusable", "脏读，非幻读/不可重复读"},
	{"同一个查询两次结果不一样是什么隔离级别问题", "confusable", "不可重复读"},
	{"事务中两次范围查询返回的行数变了", "confusable", "幻读"},
	{"Go 里协程和线程有什么区别", "confusable", "goroutine vs thread"},
	{"进程和线程的区别", "confusable", "应命中 OS 相关而非 goroutine"},

	// --- A 类：语义（口语化提问） ---
	{"数据库查询太慢了怎么优化", "semantic", "索引优化相关"},
	{"怎么保证消息不丢", "semantic", "Kafka 可靠性"},
	{"服务挂了怎么自动重连", "semantic", "重连机制"},
	{"多台机器怎么保证同一时间只有一个在执行", "semantic", "分布式锁"},
	{"怎么生成全局唯一的ID", "semantic", "雪花算法"},
	{"用户登录状态怎么保存", "semantic", "JWT / Session"},
	{"程序内存一直涨是什么原因", "semantic", "GC / 内存泄漏"},
	{"怎么让接口响应更快", "semantic", "缓存 / 性能优化"},
	{"两个服务之间怎么通信比较好", "semantic", "RPC / 协议选择"},
	{"大文件上传怎么做", "semantic", "分片上传"},
	{"密码应该怎么存到数据库", "semantic", "bcrypt"},
	{"怎么知道文件有没有重复上传过", "semantic", "秒传 / 哈希"},
	{"并发写同一条记录会有什么问题", "semantic", "锁 / 死锁"},
	{"怎么做限流", "semantic", "限流算法"},
	{"前端点了按钮怎么立刻有反馈", "semantic", "乐观 UI"},
	{"网络断了消息怎么补发", "semantic", "消息投递"},

	// --- B 类：关键词/专有名词 ---
	{"MVCC", "keyword", "MySQL 多版本并发控制"},
	{"GMP 调度模型", "keyword", "Go 调度器"},
	{"B+树索引", "keyword", "MySQL 索引结构"},
	{"sync.Pool", "keyword", "Go 标准库"},
	{"context.WithCancel", "keyword", "Go context"},
	{"Redis AOF RDB", "keyword", "持久化"},
	{"Redis Cluster 槽位", "keyword", "集群分片"},
	{"ZSet 跳表", "keyword", "Redis 数据结构"},
	{"Kafka ISR", "keyword", "副本同步"},
	{"Kafka 零拷贝 sendfile", "keyword", "高性能原理"},
	{"TCP 三次握手", "keyword", "网络协议"},
	{"HTTP/2 多路复用", "keyword", "协议演进"},
	{"bcrypt cost", "keyword", "密码哈希参数"},
	{"S3 multipart upload", "keyword", "分片上传 API"},
	{"gRPC 双向流", "keyword", "流式 RPC"},
	{"select case channel", "keyword", "Go 并发原语"},
	{"defer recover panic", "keyword", "Go 错误处理"},
	{"slice 扩容", "keyword", "Go 底层"},
	{"map 并发安全", "keyword", "Go map"},
	{"两阶段提交 2PC", "keyword", "分布式事务"},
	{"TCC 补偿事务", "keyword", "分布式事务"},
	{"雪花算法时钟回拨", "keyword", "ID 生成边界情况"},
	{"Redlock", "keyword", "Redis 分布式锁"},
	{"间隙锁 next-key lock", "keyword", "MySQL 锁"},
	{"WezTerm tab 关闭", "keyword", "工具类问题"},
	{"ImGui 破坏性变更", "keyword", "OptiScaler 项目"},
	{"UTF-8 编译错误", "keyword", "字符编码问题"},
	{"CJK 字体渲染", "keyword", "中文字体"},
	{"pgx 连接池", "keyword", "Go PostgreSQL 驱动"},
	{"微服务拆分粒度", "keyword", "架构设计"},
	{"设计模式 Go 实现", "keyword", "设计模式"},
}

const draftTopN = 8

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cfg, err := config.Load("config.toml")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	embedder := llm.NewEmbedder(cfg.LLM.EmbeddingKey(), cfg.LLM.EmbeddingURL(),
		cfg.LLM.EmbeddingModel, cfg.LLM.EmbeddingDim)

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

	vec, err := retriever.NewVectorRetriever(embedder, store)
	if err != nil {
		return err
	}
	bm25, err := retriever.NewBM25Retriever(pool)
	if err != nil {
		return err
	}
	h, err := retriever.NewHybridRetriever(
		[]retriever.Recaller{vec, bm25},
		retriever.WithTopK(draftTopN),
	)
	if err != nil {
		return err
	}

	fmt.Printf("# Golden Set 候选标注草稿\n\n")
	fmt.Printf("生成时间：%s\n\n", time.Now().Format("2006-01-02 15:04"))
	fmt.Printf("**用法**：逐条检查候选，把确实回答了问题的 chunk ID 填进 `relevant_chunk_ids`。\n")
	fmt.Printf("不相关的删掉。宁缺毋滥 —— 标错比标少更伤评估可信度。\n\n")
	fmt.Printf("---\n\n")

	entries := make([]goldenEntry, 0, len(queries))

	for i, q := range queries {
		hits, err := h.Search(ctx, q.Query)
		if err != nil {
			return fmt.Errorf("search %q: %w", q.Query, err)
		}

		fmt.Printf("## %d. %s\n\n", i+1, q.Query)
		fmt.Printf("- 类型：`%s`　预期：%s\n\n", q.Kind, q.Note)

		if len(hits) == 0 {
			fmt.Printf("⚠️ **无召回** —— 语料里可能没有相关内容，考虑换个查询\n\n")
			continue
		}

		ids := make([]string, 0, len(hits))
		for rank, hit := range hits {
			ids = append(ids, hit.Chunk.ID)
			fmt.Printf("%d. `%s`　_%s_　(score %.4f)\n",
				rank+1, hit.Chunk.ID,
				hit.Chunk.Metadata["heading_path"], hit.Score)
			fmt.Printf("   > %s\n\n", preview(hit.Chunk.Content, 110))
		}

		entries = append(entries, goldenEntry{
			Query:            q.Query,
			Kind:             q.Kind,
			RelevantChunkIDs: ids, // 待人工筛选
			Note:             q.Note,
		})
		fmt.Printf("---\n\n")
	}

	// 同时写一份 JSON 骨架，人工筛完直接改这个文件
	out, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal draft: %w", err)
	}
	if err := os.WriteFile("scripts/eval/golden_set.draft.json", out, 0o644); err != nil {
		return fmt.Errorf("write draft: %w", err)
	}
	fmt.Fprintf(os.Stderr, "\n草稿已写入 scripts/eval/golden_set.draft.json（%d 条查询）\n", len(entries))
	return nil
}

type goldenEntry struct {
	Query            string   `json:"query"`
	Kind             string   `json:"kind"`
	RelevantChunkIDs []string `json:"relevant_chunk_ids"`
	Note             string   `json:"note"`
}

func preview(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
