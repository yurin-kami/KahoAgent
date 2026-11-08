package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel/attribute"

	"github.com/yurin-kami/KahoAgent/internal/metrics"
	"github.com/yurin-kami/KahoAgent/internal/trace"
)

// Consumer 消费文档处理消息。
//
// ⚠️ 核心约束：consumer 只是 Ingester.Run 的一层壳。
// 不准在这里写任何业务逻辑 —— 一旦写了，同步和异步两条路径的行为就会分叉，
// 测试覆盖不到，线上必出诡异 bug。这里唯一该操心的是「消息该不该提交」。
type Consumer struct {
	reader   *kafka.Reader
	ingester *Ingester
	logger   *slog.Logger

	// dlq 是可选的死信投递端。为 nil 时退化为「无限重试」——
	// 这是旧行为，保留它是为了让没配 DLQ 的部署不用改配置。
	dlq         *Producer
	maxAttempts int

	// retryDelay 是两次尝试之间的等待时长。
	retryDelay time.Duration
}

const defaultRetryDelay = 2 * time.Second

// defaultMaxAttempts 是进死信前的最大尝试次数。
// 5 次足够扛过分钟级的网络抖动，又不至于让一条坏消息卡住分区太久。
const defaultMaxAttempts = 5

// ConsumerOption 是可选的消费端配置。
type ConsumerOption func(*Consumer)

// WithDLQ 配置死信投递端。
func WithDLQ(p *Producer) ConsumerOption {
	return func(c *Consumer) { c.dlq = p }
}

// WithMaxAttempts 设置进死信前的最大尝试次数（<=0 表示用默认值）。
func WithMaxAttempts(n int) ConsumerOption {
	return func(c *Consumer) {
		if n > 0 {
			c.maxAttempts = n
		}
	}
}

// WithRetryDelay 设置失败重试的退避时长（测试用）。
func WithRetryDelay(d time.Duration) ConsumerOption {
	return func(c *Consumer) {
		if d > 0 {
			c.retryDelay = d
		}
	}
}

// NewConsumer 创建消费端。
//
// CommitInterval 保持 0（默认）：kafka-go 在此配置下不自动提交，
// 完全由我们手动 CommitMessages 控制。这是实现「至少一次」的前提 ——
// 一旦设成非 0，offset 会按时间自动提交，处理失败的消息也就丢了。
func NewConsumer(brokers []string, topic, groupID string, ingester *Ingester, logger *slog.Logger, opts ...ConsumerOption) (*Consumer, error) {
	if len(brokers) == 0 {
		return nil, fmt.Errorf("at least one broker is required")
	}
	if topic == "" {
		return nil, fmt.Errorf("topic must not be empty")
	}
	if groupID == "" {
		// 没有 GroupID 就没有 offset 管理，重启会从头消费
		return nil, fmt.Errorf("group_id must not be empty")
	}
	if ingester == nil {
		return nil, fmt.Errorf("ingester must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}

	c := &Consumer{
		reader: kafka.NewReader(kafka.ReaderConfig{
			Brokers:  brokers,
			Topic:    topic,
			GroupID:  groupID,
			MinBytes: 1,
			MaxBytes: 10 << 20, // 10MB，需大于 producer 侧的单条上限
			MaxWait:  500 * time.Millisecond,
			// StartOffset 仅在消费组首次创建时生效。
			// FirstOffset：新组从最早的消息开始，避免漏掉启动前积压的任务。
			StartOffset: kafka.FirstOffset,
			// 让 Reader 感知分区变化：topic 扩容或延迟创建时，
			// 不必重启 worker 也能发现新分区。
			WatchPartitionChanges: true,
		}),
		ingester:    ingester,
		logger:      logger,
		retryDelay:  defaultRetryDelay,
		maxAttempts: defaultMaxAttempts,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Start 阻塞消费直到 ctx 取消或发生不可恢复的错误。
//
// 投递语义为**至少一次**：处理成功才提交 offset。
// 崩溃或中途退出时，未提交的消息会在重启后重新消费 ——
// 这正是 Ingester 必须幂等的原因（稳定 ChunkID + Upsert）。
func (c *Consumer) Start(ctx context.Context) error {
	c.logger.Info("consumer started", "topic", c.reader.Config().Topic,
		"group", c.reader.Config().GroupID)

	for {
		// ⚠️ 用 FetchMessage 而非 ReadMessage。
		//    ReadMessage 会自动提交 offset —— 消息一取出就算「已处理」，
		//    处理失败就永久丢失。FetchMessage + 手动 Commit 才是至少一次。
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			// ctx 取消是正常停机，不是故障
			if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
				c.logger.Info("consumer stopping")
				return nil
			}
			return fmt.Errorf("fetch message: %w", err)
		}

		c.handle(ctx, msg)

		// 每轮检查一次退出信号：处理耗时可能很长，
		// 期间收到的停机信号应尽快响应。
		if ctx.Err() != nil {
			c.logger.Info("consumer stopping")
			return nil
		}
	}
}

// handle 处理单条消息并决定是否提交 offset。
//
// 四种结局：
//   - 脏消息 → 提交（跳过，重试无意义）
//   - 重试后成功 → 提交
//   - 重试耗尽且配了 DLQ → 转死信后提交
//   - 重试耗尽且没配 DLQ → 不提交（重启/重平衡后重新消费）
func (c *Consumer) handle(ctx context.Context, msg kafka.Message) {
	// 先还原上游的 trace 上下文，再建 span —— 顺序反了的话
	// 这次消费就会成为一条新的 root trace，与上传请求断开。
	ctx = ExtractTrace(ctx, msg.Headers)

	// 每条消息一个 span（若无上游上下文则为 root）。
	ctx, span := trace.Tracer().Start(ctx, "kafka.consume")
	defer span.End()
	span.SetAttributes(
		attribute.Int("messaging.kafka.partition", msg.Partition),
		attribute.Int64("messaging.kafka.offset", msg.Offset),
	)

	var task IngestTask
	if err := json.Unmarshal(msg.Value, &task); err != nil {
		// 反序列化失败是脏消息，重试也没用 —— 记日志后跳过并提交 offset，
		// 否则会卡在这条消息上无限重试（毒丸消息问题），
		// 后面所有正常消息都被它堵死。
		c.logger.Error("skipping malformed message",
			"err", err, "offset", msg.Offset, "partition", msg.Partition)
		metrics.RecordIngest(metrics.IngestSkipped, 0)
		c.commit(ctx, msg)
		return
	}
	// DocID 为空同样是脏消息：Ingester 会拒绝它，重试多少次都一样
	if task.DocID == "" {
		c.logger.Error("skipping message with empty doc_id", "offset", msg.Offset)
		metrics.RecordIngest(metrics.IngestSkipped, 0)
		c.commit(ctx, msg)
		return
	}

	start := time.Now()
	n, err := c.runWithRetry(ctx, task)
	if err != nil {
		metrics.RecordIngest(metrics.IngestError, time.Since(start).Seconds())

		if c.dlq != nil {
			// 重试耗尽 → 转死信并提交 offset。
			//
			// 为什么需要 DLQ：没有它，一条永久失败的消息会永远不提交
			// offset，把整个分区堵死 —— 后面的正常文档全部积压，
			// 而报警只会显示「消费延迟上升」，根本看不出是单条消息造成的。
			if derr := c.publishDLQ(ctx, msg, err, c.maxAttempts); derr != nil {
				// 死信也投不进去（broker 挂了）：不提交，等重启后重投。
				// 宁可多试几次，也不能把用户的文档静默丢掉。
				c.logger.Error("dead-letter publish failed; offset not committed",
					"doc_id", task.DocID, "err", derr)
				return
			}
			c.logger.Error("message moved to dead-letter queue",
				"doc_id", task.DocID, "attempts", c.maxAttempts,
				"offset", msg.Offset, "err", err)
			metrics.RecordIngest(metrics.IngestDeadLetter, time.Since(start).Seconds())
			c.commit(ctx, msg)
			return
		}

		// 没有 DLQ：不提交 offset，等进程重启/重平衡后重投。
		// 这是旧行为，保留它是为了向后兼容。
		c.logger.Error("ingest failed, offset not committed; will redeliver after restart",
			"doc_id", task.DocID, "offset", msg.Offset, "err", err)
		return
	}

	// 处理成功才提交
	c.commit(ctx, msg)
	metrics.RecordIngest(metrics.IngestOK, time.Since(start).Seconds())
	c.logger.Info("message processed",
		"doc_id", task.DocID, "chunks", n, "offset", msg.Offset)
}

func (c *Consumer) commit(ctx context.Context, msg kafka.Message) {
	// 用 context.WithoutCancel：停机时已处理完的消息仍需提交，
	// 否则重启后会重复消费一条本已成功的消息。
	// 幂等性保证了重复消费无害，但能避免就避免。
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := c.reader.CommitMessages(commitCtx, msg); err != nil {
		c.logger.Error("commit failed", "offset", msg.Offset, "err", err)
	}
}

// runWithRetry 在进程内重试入库。
//
// ⚠️ 为什么必须显式重试：kafka-go 的 FetchMessage 即使不提交 offset，
// 同一个 Reader 也会继续向后读 —— 未提交的消息只有在重启/重平衡时
// 才会被重新投递。换言之，「不提交 offset」只保证**至少一次**，
// 不保证**立即重试**。所以重试必须在这里做。
//
// 这也让瞬时故障（Milvus 抖动、embedding 偶发 5xx）能在进程内自愈，
// 不必依赖一次进程重启。
func (c *Consumer) runWithRetry(ctx context.Context, task IngestTask) (int, error) {
	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		n, err := c.ingester.Run(ctx, task.DocID, task.Raw)
		if err == nil {
			return n, nil
		}
		lastErr = err
		if attempt == c.maxAttempts {
			break
		}
		c.logger.Warn("ingest failed, retrying",
			"doc_id", task.DocID, "attempt", attempt, "max", c.maxAttempts, "err", err)
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(c.retryDelay):
		}
	}
	return 0, lastErr
}

// publishDLQ 把失败消息原样转投死信 topic。
//
// 保留原始 key（同一文档的分区语义在 DLQ 里也成立）与原始 value，
// 追加一组诊断 header。诊断信息用 header 而不是改写 value：
// 下游排查工具可以原样重放这条消息，不需要先剥掉我们加的字段。
func (c *Consumer) publishDLQ(ctx context.Context, msg kafka.Message, cause error, attempts int) error {
	reason := cause.Error()
	if len(reason) > 200 {
		reason = reason[:200]
	}
	headers := make([]kafka.Header, 0, len(msg.Headers)+3)
	headers = append(headers,
		kafka.Header{Key: "x-kaho-error", Value: []byte(reason)},
		kafka.Header{Key: "x-kaho-attempts", Value: []byte(strconv.Itoa(attempts))},
		kafka.Header{Key: "x-kaho-origin-offset", Value: []byte(strconv.FormatInt(msg.Offset, 10))},
	)
	headers = append(headers, msg.Headers...)

	return c.dlq.PublishMessage(ctx, kafka.Message{
		Key:     msg.Key,
		Value:   msg.Value,
		Headers: headers,
	})
}

func (c *Consumer) Close() error { return c.reader.Close() }
