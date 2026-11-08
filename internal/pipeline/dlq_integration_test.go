//go:build integration

package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

// TestDLQ_PoisonMessageIsMovedAfterMaxAttempts 验证「重试到上限 → 转死信」。
//
// 覆盖的是没有 DLQ 时最痛的一种故障：一条永久失败的消息永远不提交
// offset，把整个分区堵死，而监控上只看到「消费延迟上涨」。
func TestDLQ_PoisonMessageIsMovedAfterMaxAttempts(t *testing.T) {
	topic := newTestTopic(t)
	dlqTopic := topic + "-dlq"
	// 显式建 DLQ topic（单分区），而不是依赖 broker 自动创建：
	// 自动创建是 3 分区，测试读哪一分区不确定。
	createTopic(t, dlqTopic)

	// 让入库必然失败：embedding 直接报错。不需要任何外部服务。
	ing, err := NewIngester(
		&mockChunker{chunks: []rag.Chunk{{ID: "c0", Index: 0, Content: "x"}}},
		&mockEmbedder{dim: 4, err: errors.New("embedding upstream down")},
		&mockStore{},
		quietLogger(),
	)
	if err != nil {
		t.Fatalf("new ingester: %v", err)
	}

	prod, err := NewProducer(kafkaBrokers(), topic)
	if err != nil {
		t.Fatalf("new producer: %v", err)
	}
	defer prod.Close()

	dlqProd, err := NewProducer(kafkaBrokers(), dlqTopic)
	if err != nil {
		t.Fatalf("new dlq producer: %v", err)
	}
	defer dlqProd.Close()

	cons, err := NewConsumer(kafkaBrokers(), topic, "dlq-test-"+topic, ing, quietLogger(),
		WithDLQ(dlqProd),
		WithMaxAttempts(2),
		WithRetryDelay(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cons.Start(ctx) }()
	defer func() { _ = cons.Close() }()

	if err := prod.Publish(ctx, IngestTask{DocID: "poison.md", Raw: []byte("# 坏文档")}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// DLQ topic 由投递端自动创建，第一次读可能拿不到元数据 —— 重试。
	var msg kafka.Message
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		msg, err = readOneFrom(t, dlqTopic, 3*time.Second)
		if err == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("no dead-letter message within 30s: %v", err)
	}

	var task IngestTask
	if err := json.Unmarshal(msg.Value, &task); err != nil {
		t.Fatalf("decode dead-letter value: %v", err)
	}
	if task.DocID != "poison.md" {
		t.Fatalf("dead-letter doc_id = %q, want poison.md", task.DocID)
	}

	headers := make(map[string]string, len(msg.Headers))
	for _, h := range msg.Headers {
		headers[h.Key] = string(h.Value)
	}
	if got := headers["x-kaho-attempts"]; got != "2" {
		t.Fatalf("x-kaho-attempts = %q, want 2", got)
	}
	if headers["x-kaho-error"] == "" {
		t.Fatal("x-kaho-error header is missing — 死信里没有失败原因，排查会很困难")
	}
}

// readOneFrom 从 Partition 0 读一条消息。
//
// 不用消费组：单分区 + 指定 offset 更确定，也不用等 rebalance。
// 不提交 offset，所以可以重复读。
func readOneFrom(t *testing.T, topic string, timeout time.Duration) (kafka.Message, error) {
	t.Helper()
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:     kafkaBrokers(),
		Topic:       topic,
		Partition:   0,
		StartOffset: kafka.FirstOffset,
		MinBytes:    1,
		// kafka-go 要求 MaxBytes > MinBytes；不设会 panic。
		MaxBytes: 10 << 20,
		MaxWait:  200 * time.Millisecond,
	})
	defer r.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return r.ReadMessage(ctx)
}
