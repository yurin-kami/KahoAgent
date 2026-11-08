//go:build integration

// Kafka 集成测试。需要真实的 Kafka：
//
//	podman-compose up -d
//	go test -tags=integration ./internal/pipeline/ -v
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/yurin-kami/KahoAgent/internal/rag"
)

func kafkaBrokers() []string {
	if v := os.Getenv("KAFKA_BROKERS"); v != "" {
		return []string{v}
	}
	return []string{"127.0.0.1:9092"}
}

// newTestTopic 创建独立 topic 并在测试结束时删除，避免测试间串消息。
func newTestTopic(t *testing.T) string {
	t.Helper()
	topic := fmt.Sprintf("test-%s-%d", t.Name(), time.Now().UnixNano()%1000000)
	createTopic(t, topic)
	return topic
}

// createTopic 建一个单分区 topic 并等 leader 就绪。
//
// 单分区是为了让测试可以按 Partition 0 精确读取，
// 避免消费组 rebalance 带来的不确定性。
func createTopic(t *testing.T, topic string) {
	t.Helper()

	conn, err := kafka.Dial("tcp", kafkaBrokers()[0])
	if err != nil {
		t.Fatalf("dial kafka: %v\n(是否已执行 bash scripts/local-stack.sh up ?)", err)
	}
	defer conn.Close()

	if err := conn.CreateTopics(kafka.TopicConfig{
		Topic: topic, NumPartitions: 1, ReplicationFactor: 1,
	}); err != nil {
		t.Fatalf("create topic %s: %v", topic, err)
	}

	// ⚠️ CreateTopics 返回 ≠ topic 可用。
	//    建 topic 后 broker 还要选举 partition leader 并传播元数据，
	//    这期间写入会收到 UnknownTopicOrPartition。必须等到 leader 就绪，
	//    否则测试会随机失败 —— 典型的分布式系统时序坑。
	waitForTopic(t, topic)
}

// waitForTopic 轮询直到 topic 对 Producer 实际走的那条路径可见。
//
// ⚠️ 这里必须用 kafka.Client + DefaultTransport，而不是 conn.ReadPartitions。
//
//	kafka-go 的 DefaultTransport 是**包级共享**的，带 6 秒元数据缓存
//	（transport.go:126、metadataTTL 默认 6s）。Producer 内部的 Writer 用的
//	正是它。用独立 Conn 查到 topic 已就绪，不代表共享缓存里也有 ——
//	实测表现为：第一个测试通过（缓存冷，会拉新数据），后续测试全部
//	收到 UnknownTopicOrPartition（缓存热但过期数据里没有新 topic）。
//	所以要从同一个 transport 查，查到了才说明 Producer 也能看到。
func waitForTopic(t *testing.T, topic string) {
	t.Helper()

	client := &kafka.Client{
		Addr:      kafka.TCP(kafkaBrokers()...),
		Transport: kafka.DefaultTransport,
	}
	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		resp, err := client.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{topic}})
		cancel()

		if err == nil {
			for _, tp := range resp.Topics {
				if tp.Name != topic || tp.Error != nil {
					continue
				}
				// Leader.Host 非空表示 partition 已选出 leader，此时才能安全写入
				if len(tp.Partitions) > 0 && tp.Partitions[0].Leader.Host != "" {
					return
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("topic %s did not become visible to the shared transport within 30s", topic)
}

// recordingIngester 记录每次 Run 的调用，并可按 docID 注入失败。
type recordingIngester struct {
	mu      sync.Mutex
	calls   []string
	failFor map[string]int // docID -> 还需失败几次
	done    chan string    // 每次成功处理后推送 docID
}

func newRecordingIngester(t *testing.T) (*Ingester, *recordingIngester) {
	t.Helper()
	rec := &recordingIngester{
		failFor: map[string]int{},
		done:    make(chan string, 64),
	}
	// 用 mock chunker 包住 recorder：Consumer 需要一个真的 *Ingester，
	// 而 Ingester 的行为由注入的接口决定 —— 在 chunker 层记录即可。
	ing, err := NewIngester(
		&recordingChunker{rec: rec},
		&mockEmbedder{dim: 4},
		&mockStore{},
		quietLogger(),
	)
	if err != nil {
		t.Fatalf("NewIngester: %v", err)
	}
	return ing, rec
}

type recordingChunker struct{ rec *recordingIngester }

func (c *recordingChunker) Name() string { return "recording" }
func (c *recordingChunker) Chunk(_ context.Context, docID string, raw []byte) ([]rag.Chunk, error) {
	c.rec.mu.Lock()
	c.rec.calls = append(c.rec.calls, docID)
	shouldFail := c.rec.failFor[docID] > 0
	if shouldFail {
		c.rec.failFor[docID]--
	}
	c.rec.mu.Unlock()

	if shouldFail {
		return nil, errors.New("injected failure")
	}

	select {
	case c.rec.done <- docID:
	default:
	}
	return []rag.Chunk{{ID: docID + "-0", Index: 0, Content: string(raw)}}, nil
}

func (r *recordingIngester) callCount(docID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c == docID {
			n++
		}
	}
	return n
}

// waitFor 等待某个 docID 被成功处理
func waitFor(t *testing.T, rec *recordingIngester, docID string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case got := <-rec.done:
			if got == docID {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s to be processed (calls: %v)", docID, rec.calls)
		}
	}
}

func TestKafka_ProduceAndConsume(t *testing.T) {
	topic := newTestTopic(t)
	ing, rec := newRecordingIngester(t)

	producer, err := NewProducer(kafkaBrokers(), topic)
	if err != nil {
		t.Fatalf("new producer: %v", err)
	}
	defer producer.Close()

	consumer, err := NewConsumer(kafkaBrokers(), topic, "test-group", ing, quietLogger())
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}
	defer consumer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	go func() { _ = consumer.Start(ctx) }()

	if err := producer.Publish(ctx, IngestTask{DocID: "doc1", Raw: []byte("文档内容")}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitFor(t, rec, "doc1", 45*time.Second)
	if n := rec.callCount("doc1"); n != 1 {
		t.Errorf("expected doc1 processed once, got %d", n)
	}
}

func TestKafka_MalformedMessageDoesNotBlockTheQueue(t *testing.T) {
	// 毒丸消息问题：脏消息若不提交 offset 会永远卡住后续所有消息
	topic := newTestTopic(t)
	ing, rec := newRecordingIngester(t)

	consumer, err := NewConsumer(kafkaBrokers(), topic, "test-group", ing, quietLogger())
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}
	defer consumer.Close()

	// 直接写一条非 JSON 消息，后面跟一条正常消息
	w := &kafka.Writer{Addr: kafka.TCP(kafkaBrokers()...), Topic: topic,
		RequiredAcks: kafka.RequireAll}
	defer w.Close()

	good, _ := json.Marshal(IngestTask{DocID: "good", Raw: []byte("正常内容")})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := w.WriteMessages(ctx,
		kafka.Message{Key: []byte("bad"), Value: []byte("{not json at all")},
		kafka.Message{Key: []byte("good"), Value: good},
	); err != nil {
		t.Fatalf("write: %v", err)
	}

	go func() { _ = consumer.Start(ctx) }()

	// 关键断言：脏消息被跳过，后面的正常消息仍被处理
	waitFor(t, rec, "good", 45*time.Second)
}

func TestKafka_FailedMessageIsRedeliveredAfterRestart(t *testing.T) {
	// 对应验收项「处理中途 kill consumer，重启后消息被重新消费」。
	// 这里用「处理失败 → 关闭 consumer → 重启」模拟崩溃：
	// 失败不提交 offset，新 consumer 应重新拿到同一条消息。
	topic := newTestTopic(t)
	const groupID = "test-group-restart"

	producer, err := NewProducer(kafkaBrokers(), topic)
	if err != nil {
		t.Fatalf("new producer: %v", err)
	}
	defer producer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := producer.Publish(ctx, IngestTask{DocID: "retry-me", Raw: []byte("内容")}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// 第一个 consumer：注入失败，因此不会提交 offset
	ing1, rec1 := newRecordingIngester(t)
	rec1.mu.Lock()
	rec1.failFor["retry-me"] = 100 // 始终失败
	rec1.mu.Unlock()

	consumer1, err := NewConsumer(kafkaBrokers(), topic, groupID, ing1, quietLogger())
	if err != nil {
		t.Fatalf("new consumer1: %v", err)
	}
	consumer1.retryDelay = 200 * time.Millisecond // 加快测试

	ctx1, cancel1 := context.WithCancel(ctx)
	go func() { _ = consumer1.Start(ctx1) }()

	// 等它至少尝试过一次
	deadline := time.Now().Add(45 * time.Second)
	for rec1.callCount("retry-me") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("consumer1 never attempted the message")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// 模拟崩溃：取消 ctx 并关闭 reader
	cancel1()
	if err := consumer1.Close(); err != nil {
		t.Logf("close consumer1: %v", err)
	}

	// 第二个 consumer：同一个 group，这次会成功
	ing2, rec2 := newRecordingIngester(t)
	consumer2, err := NewConsumer(kafkaBrokers(), topic, groupID, ing2, quietLogger())
	if err != nil {
		t.Fatalf("new consumer2: %v", err)
	}
	defer consumer2.Close()

	go func() { _ = consumer2.Start(ctx) }()

	// 关键断言：offset 未提交，重启后消息被重新投递
	waitFor(t, rec2, "retry-me", 45*time.Second)
}

func TestProducer_RejectsOversizedTask(t *testing.T) {
	// 超过 broker 上限的消息应在客户端就被拦截，
	// 而不是发出去等 broker 报一个难懂的错
	producer, err := NewProducer(kafkaBrokers(), "unused")
	if err != nil {
		t.Fatalf("new producer: %v", err)
	}
	defer producer.Close()

	// 用序列化上限直接构造：body 里的 []byte 会 base64 膨胀，
	// 900KB 原文 → 约 1.2MB 消息，必定超过 MaxTaskBytes。
	huge := make([]byte, MaxTaskBytes+1)
	err = producer.Publish(context.Background(), IngestTask{DocID: "big", Raw: huge})
	if err == nil {
		t.Fatal("expected an error for an oversized task")
	}
}
