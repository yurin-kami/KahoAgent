package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
)

// IngestTask 是投递到 Kafka 的入库任务。
//
// 直接携带文档原文（Raw）而非只传一个存储路径，是为了让 consumer 不依赖
// 外部存储就能独立工作。代价是消息体较大，因此在 Producer 侧限制了大小 ——
// 超过限制的文档属于 Phase 6 的 PDF 场景，不在当前范围。
type IngestTask struct {
	DocID string `json:"doc_id"`
	Raw   []byte `json:"raw"`
}

// MaxTaskBytes 限制单条 Kafka 消息的大小。
//
// Kafka broker 默认 message.max.bytes 约为 1MB，超出会被 broker 拒绝。
// 在 Producer 侧提前拦截，好过让消息发出去才失败 —— 后者的错误信息
// 来自 broker，排查起来远不如这里直白。
//
// ⚠️ 这是**序列化后**的大小，不是文档原文大小。IngestTask.Raw 是 []byte，
// encoding/json 会把它编码成 base64，体积膨胀约 4/3。因此上传接口
// 校验的原文上限应该是 MaxRawBytes，而不是这个值 ——
// 拿 900KB 去卡原文，会导致 700KB 的文档在 Publish 阶段才失败。
const MaxTaskBytes = 900 * 1024

// MaxRawBytes 是单篇文档原文的安全上限（base64 膨胀后的余量）。
//
// 推导：raw * 4/3 + JSON 字段开销 ≤ MaxTaskBytes
// → raw ≤ 900KB * 3/4 = 675KB。再留一点余量，取 640KB。
const MaxRawBytes = 640 * 1024

// Producer 把入库任务投递到 Kafka。
//
// 存在的意义：上传接口收到文件后立即返回，不必等 30 秒的向量化完成。
// HTTP 层只负责「收下并确认」，真正的处理交给后台 worker。
type Producer struct {
	writer *kafka.Writer
}

// NewProducer 创建投递端。
//
// RequiredAcks 设为 RequireAll：必须等所有副本确认才算成功。
// 这是可靠性与延迟的取舍 —— 入库任务丢了用户的文档就永远不会被处理，
// 而多等几毫秒对异步流程毫无影响，所以这里毫不犹豫选可靠性。
func NewProducer(brokers []string, topic string) (*Producer, error) {
	if len(brokers) == 0 {
		return nil, fmt.Errorf("at least one broker is required")
	}
	if topic == "" {
		return nil, fmt.Errorf("topic must not be empty")
	}
	return &Producer{
		writer: &kafka.Writer{
			Addr:         kafka.TCP(brokers...),
			Topic:        topic,
			Balancer:     &kafka.Hash{}, // 按 Key 哈希：同一文档固定落到同一 partition
			RequiredAcks: kafka.RequireAll,
			MaxAttempts:  3,
			WriteTimeout: 10 * time.Second,
			// Async 保持 false（默认）：异步写会让 WriteMessages 立即返回，
			// 错误只能从 Logger 里看到，调用方无从得知投递是否成功。
			// 上传接口需要「确认已收下」的语义，必须同步。
		},
	}, nil
}

// Publish 投递一个入库任务。
func (p *Producer) Publish(ctx context.Context, task IngestTask) error {
	if task.DocID == "" {
		return fmt.Errorf("task doc_id must not be empty")
	}

	payload, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("marshal task %s: %w", task.DocID, err)
	}
	if len(payload) > MaxTaskBytes {
		return fmt.Errorf("task %s is %d bytes, exceeds the %d limit",
			task.DocID, len(payload), MaxTaskBytes)
	}

	// ⚠️ Key 设为 DocID：Kafka 只保证 partition 内有序。
	//    同一文档的多次更新必须落到同一 partition，否则「先删后增」
	//    可能被乱序消费成「先增后删」，文档凭空消失。
	msg := kafka.Message{
		Key:   []byte(task.DocID),
		Value: payload,
		// 把 W3C trace 上下文写进消息头：这样「上传请求」与
		// 「worker 入库」在 Jaeger 里是同一条 trace，而不是两条孤立的。
		Headers: InjectTraceHeaders(ctx),
	}
	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		return fmt.Errorf("publish task %s: %w", task.DocID, err)
	}
	return nil
}

func (p *Producer) Close() error { return p.writer.Close() }

// PublishMessage 投递一条原始 Kafka 消息（死信队列用）。
//
// 与 Publish 分开：死信要保留原始 key / value / headers，
// 而 Publish 只接受结构化的 IngestTask。
func (p *Producer) PublishMessage(ctx context.Context, msg kafka.Message) error {
	if len(msg.Value) > MaxTaskBytes {
		return fmt.Errorf("dead-letter message is %d bytes, exceeds the %d limit",
			len(msg.Value), MaxTaskBytes)
	}
	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		return fmt.Errorf("publish raw message: %w", err)
	}
	return nil
}
