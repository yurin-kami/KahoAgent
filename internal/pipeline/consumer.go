package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/segmentio/kafka-go"
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

	// retryDelay 是处理失败后的等待时长。
	// 失败不提交 offset 意味着下次 Fetch 还会拿到同一条消息；
	// 没有这个间隔就会变成 CPU 打满的忙轮询。
	retryDelay time.Duration
}

const defaultRetryDelay = 2 * time.Second

// NewConsumer 创建消费端。
//
// CommitInterval 保持 0（默认）：kafka-go 在此配置下不自动提交，
// 完全由我们手动 CommitMessages 控制。这是实现「至少一次」的前提 ——
// 一旦设成非 0，offset 会按时间自动提交，处理失败的消息也就丢了。
func NewConsumer(brokers []string, topic, groupID string, ingester *Ingester, logger *slog.Logger) (*Consumer, error) {
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

	return &Consumer{
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
		}),
		ingester:   ingester,
		logger:     logger,
		retryDelay: defaultRetryDelay,
	}, nil
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
// 三种结局：
//   - 脏消息 → 提交（跳过，重试无意义）
//   - 处理失败 → 不提交（重启后重新消费）
//   - 成功 → 提交
func (c *Consumer) handle(ctx context.Context, msg kafka.Message) {
	var task IngestTask
	if err := json.Unmarshal(msg.Value, &task); err != nil {
		// 反序列化失败是脏消息，重试也没用 —— 记日志后跳过并提交 offset，
		// 否则会卡在这条消息上无限重试（毒丸消息问题），
		// 后面所有正常消息都被它堵死。
		c.logger.Error("skipping malformed message",
			"err", err, "offset", msg.Offset, "partition", msg.Partition)
		c.commit(ctx, msg)
		return
	}
	// DocID 为空同样是脏消息：Ingester 会拒绝它，重试多少次都一样
	if task.DocID == "" {
		c.logger.Error("skipping message with empty doc_id", "offset", msg.Offset)
		c.commit(ctx, msg)
		return
	}

	n, err := c.ingester.Run(ctx, task.DocID, task.Raw)
	if err != nil {
		// 不提交 offset → 重启后会重新消费。
		// 这里假定失败是暂时的（网络抖动、Milvus 重启）；
		// 若是永久性失败，消息会反复重试 —— 死信队列是 Phase 5 的事，
		// 当前规模下人工介入即可。
		c.logger.Error("ingest failed, offset not committed",
			"doc_id", task.DocID, "offset", msg.Offset, "err", err)

		// 退避，避免对同一条失败消息忙轮询打满 CPU
		select {
		case <-ctx.Done():
		case <-time.After(c.retryDelay):
		}
		return
	}

	// 处理成功才提交
	c.commit(ctx, msg)
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

func (c *Consumer) Close() error { return c.reader.Close() }
