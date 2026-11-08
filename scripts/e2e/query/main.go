//go:build e2echeck

// query 直接调用检索链路，验证「入库之后能被检出来」。
//
// 不经过 /api/chat：对话需要真实 LLM（还要能驱动 ReAct 工具调用），
// 而本工具要验证的是检索本身 —— 上传、分块、双写、混合召回。
// 这条链路不通，再好的模型也答不出东西。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"

	"github.com/yurin-kami/KahoAgent/internal/config"
	"github.com/yurin-kami/KahoAgent/internal/rag/retriever"
	"github.com/yurin-kami/KahoAgent/pkg/llm"
	"github.com/yurin-kami/KahoAgent/pkg/vectorstore"
)

func main() {
	query := flag.String("query", "", "检索语句")
	expect := flag.String("expect", "", "期望命中的 doc_id")
	topK := flag.Int("topk", 5, "返回条数")
	createTopic := flag.String("create-topic", "", "只创建该 topic（单分区）后退出")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// 显式建 topic：broker 自动创建是 3 分区，而消费者在 topic
	// 出现之前可能已经缓存了「不存在」的元数据，表现为长时间空转。
	if *createTopic != "" {
		if err := ensureTopic(ctx, cfg.Kafka.Brokers, *createTopic); err != nil {
			log.Fatalf("create topic: %v", err)
		}
		fmt.Println("topic ready:", *createTopic)
		return
	}

	if *query == "" || *expect == "" {
		fmt.Fprintln(os.Stderr, "usage: query -query '...' -expect doc_id | -create-topic name")
		os.Exit(2)
	}

	embedder := llm.NewEmbedder(cfg.LLM.EmbeddingKey(), cfg.LLM.EmbeddingURL(),
		cfg.LLM.EmbeddingModel, cfg.LLM.EmbeddingDim)

	store, err := vectorstore.NewMilvusStore(ctx, cfg.Milvus.Addr,
		cfg.Milvus.Collection, cfg.LLM.EmbeddingDim)
	if err != nil {
		log.Fatalf("connect milvus: %v", err)
	}
	defer store.Close()

	pool, err := pgxpool.New(ctx, cfg.DB.DSN)
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()

	vec, err := retriever.NewVectorRetriever(embedder, store)
	if err != nil {
		log.Fatalf("vector retriever: %v", err)
	}
	bm25, err := retriever.NewBM25Retriever(pool)
	if err != nil {
		log.Fatalf("bm25 retriever: %v", err)
	}
	hybrid, err := retriever.NewHybridRetriever(
		[]retriever.Recaller{vec, bm25},
		retriever.WithTopK(*topK),
	)
	if err != nil {
		log.Fatalf("hybrid retriever: %v", err)
	}

	hits, err := hybrid.Search(ctx, *query)
	if err != nil {
		log.Fatalf("search: %v", err)
	}

	fmt.Printf("query=%q hits=%d\n", *query, len(hits))
	found := false
	for i, h := range hits {
		docID := h.Chunk.DocID
		fmt.Printf("  #%d doc=%s score=%.4f\n", i+1, docID, h.Score)
		if docID == *expect {
			found = true
		}
	}
	if !found {
		fmt.Fprintf(os.Stderr, "\n❌ 期望命中 %s，但 top-%d 里没有\n", *expect, *topK)
		os.Exit(1)
	}
	fmt.Printf("\n✅ 命中 %s\n", *expect)
}

// ensureTopic 建一个单分区 topic 并等 leader 就绪。
func ensureTopic(ctx context.Context, brokers []string, topic string) error {
	conn, err := kafka.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return fmt.Errorf("dial kafka: %w", err)
	}
	defer conn.Close()

	if err := conn.CreateTopics(kafka.TopicConfig{
		Topic: topic, NumPartitions: 1, ReplicationFactor: 1,
	}); err != nil {
		return err
	}
	// 建 topic 返回不代表可用：等 leader 就绪再退出。
	for i := 0; i < 30; i++ {
		partitions, err := conn.ReadPartitions(topic)
		if err == nil && len(partitions) > 0 && partitions[0].Leader.Host != "" {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("topic %s not ready", topic)
}
