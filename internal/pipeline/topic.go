package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
)

// EnsureTopic 幂等创建 topic，并等到 leader 就绪再返回。
//
// 为什么 worker 启动时要显式建 topic：
//
//	kafka-go 的消费组在 topic 尚不存在时也能「加入」，但它随后不会
//	自动发现这个 topic 后来被创建出来 —— 实测表现为：先起 worker、
//	再上传文档，worker 一直空转，直到重启才消费。
//
// 单分区是有意的：本项目按 docID 做 key，单分区即可保证同一文档有序；
// 需要更高吞吐时再按业务维度扩分区。
func EnsureTopic(ctx context.Context, brokers []string, topic string, partitions int) error {
	if topic == "" {
		return fmt.Errorf("topic must not be empty")
	}
	if len(brokers) == 0 {
		return fmt.Errorf("at least one broker is required")
	}
	if partitions <= 0 {
		partitions = 1
	}

	conn, err := kafka.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return fmt.Errorf("dial kafka: %w", err)
	}
	defer conn.Close()

	err = conn.CreateTopics(kafka.TopicConfig{
		Topic:             topic,
		NumPartitions:     partitions,
		ReplicationFactor: 1,
	})
	// 已存在是正常情况（可重复执行），其他错误才返回。
	if err != nil && !errors.Is(err, kafka.TopicAlreadyExists) {
		return fmt.Errorf("create topic %s: %w", topic, err)
	}

	// CreateTopics 返回不代表可用：还要等 leader 选举与元数据传播。
	deadline := time.Now().Add(15 * time.Second)
	for {
		parts, perr := conn.ReadPartitions(topic)
		if perr == nil && len(parts) > 0 {
			ready := true
			for _, p := range parts {
				if p.Leader.Host == "" {
					ready = false
					break
				}
			}
			if ready {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("topic %s did not become ready in time", topic)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
