package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	"github.com/redis/go-redis/v9"
)

// 上下文管理策略（规划定死，不要随意改）：
//
//	最大历史轮数：10
//	超出后：保留最近 6 轮完整对话 + 更早的压缩成一段摘要
//	单次请求 Token 上限：8000（硬截断兜底）
//
// 为什么不全量保留：对话越长历史越多，token 消耗线性增长，
// 最终超出模型上下文限制而报错。
// 为什么不只用滑动窗口：那会丢掉早期信息，用户问「刚才第一个文件」时就傻了。
// 窗口 + 摘要是复杂度与效果的平衡点。
const (
	maxTurns  = 10
	keepTurns = 6
	// maxTokens 是硬兜底。摘要本身也可能很长，或者单条消息巨大
	// （比如用户粘贴了一整篇日志），此时仍需截断。
	maxTokens = 8000
	// sessionTTL 决定会话保留多久。24 小时足够覆盖「今天聊过的」，
	// 又不至于让 Redis 无限增长。
	sessionTTL = 24 * time.Hour
)

// summaryPrefix 标记一条消息是摘要而非原始对话。
// 存回 Redis 时靠它识别，避免把摘要再当成普通历史反复压缩。
const summaryPrefix = "[对话摘要] "

// Summarizer 把早期对话压缩成一段话。
type Summarizer interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// SessionMemory 管理单个会话的对话历史。
//
// 存储：Redis List，key = "kaho:session:{sessionID}"，TTL 24h。
// 用 List 而非单个 JSON blob：追加消息是 O(1) 的 RPUSH，
// 不必每次读出整个历史再写回。
type SessionMemory struct {
	rdb        *redis.Client
	summarizer Summarizer
	logger     *slog.Logger

	maxTurns  int
	keepTurns int
	ttl       time.Duration
}

type MemoryOption func(*SessionMemory)

func WithMemoryLogger(l *slog.Logger) MemoryOption {
	return func(m *SessionMemory) {
		if l != nil {
			m.logger = l
		}
	}
}

func WithTurnLimits(max, keep int) MemoryOption {
	return func(m *SessionMemory) {
		if max > 0 && keep > 0 && keep < max {
			m.maxTurns, m.keepTurns = max, keep
		}
	}
}

func WithSessionTTL(d time.Duration) MemoryOption {
	return func(m *SessionMemory) {
		if d > 0 {
			m.ttl = d
		}
	}
}

// NewSessionMemory 创建会话存储。
//
// summarizer 可为 nil —— 此时超限直接丢弃早期对话而不压缩。
// 这是刻意的降级路径：摘要要调 LLM，失败不该让对话不可用。
func NewSessionMemory(rdb *redis.Client, summarizer Summarizer, opts ...MemoryOption) (*SessionMemory, error) {
	if rdb == nil {
		return nil, fmt.Errorf("redis client must not be nil")
	}
	m := &SessionMemory{
		rdb:        rdb,
		summarizer: summarizer,
		logger:     slog.Default(),
		maxTurns:   maxTurns,
		keepTurns:  keepTurns,
		ttl:        sessionTTL,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

func sessionKey(id string) string { return "kaho:session:" + id }

// storedMessage 是落盘格式。
//
// 不直接序列化 schema.Message：它带 ToolCalls、ResponseMeta 等
// 大量运行期字段，存下来既浪费空间，反序列化时还可能因为
// eino 升级改结构而炸。只存对话真正需要的三个字段。
type storedMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolName 仅用于工具消息的可读性，重放时不参与逻辑
	ToolName string `json:"tool_name,omitempty"`
}

// Append 追加若干条消息到会话历史。
func (m *SessionMemory) Append(ctx context.Context, sessionID string, msgs ...*schema.Message) error {
	if sessionID == "" {
		return fmt.Errorf("sessionID must not be empty")
	}
	if len(msgs) == 0 {
		return nil
	}

	payloads := make([]any, 0, len(msgs))
	for _, msg := range msgs {
		if msg == nil || msg.Content == "" {
			// 空消息没有存储价值，且会干扰轮数统计
			continue
		}
		b, err := json.Marshal(storedMessage{
			Role:     string(msg.Role),
			Content:  msg.Content,
			ToolName: msg.ToolName,
		})
		if err != nil {
			return fmt.Errorf("marshal message: %w", err)
		}
		payloads = append(payloads, b)
	}
	if len(payloads) == 0 {
		return nil
	}

	key := sessionKey(sessionID)
	pipe := m.rdb.TxPipeline()
	pipe.RPush(ctx, key, payloads...)
	// 每次写入都刷新 TTL —— 活跃会话不该因为创建时间久而过期
	pipe.Expire(ctx, key, m.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("append to session: %w", err)
	}
	return nil
}

// Load 读取会话历史，按策略裁剪后返回。
//
// 返回的消息可直接喂给 Agent。裁剪逻辑：
//  1. 轮数未超限 → 原样返回
//  2. 超限 → 早期对话压缩成摘要 + 保留最近 keepTurns 轮
//  3. 仍超 token 上限 → 从最早的开始丢
func (m *SessionMemory) Load(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("sessionID must not be empty")
	}

	raw, err := m.rdb.LRange(ctx, sessionKey(sessionID), 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	if len(raw) == 0 {
		return nil, nil
	}

	msgs := make([]*schema.Message, 0, len(raw))
	for _, item := range raw {
		var s storedMessage
		if err := json.Unmarshal([]byte(item), &s); err != nil {
			// 单条坏数据不该让整个会话不可用 —— 跳过并记日志
			m.logger.Warn("skipping a corrupt history entry",
				"session", sessionID, "err", err)
			continue
		}
		msgs = append(msgs, &schema.Message{
			Role:     schema.RoleType(s.Role),
			Content:  s.Content,
			ToolName: s.ToolName,
		})
	}

	msgs = m.compact(ctx, sessionID, msgs)
	return capTokens(msgs, maxTokens), nil
}

// compact 在超限时把早期对话压缩成摘要。
func (m *SessionMemory) compact(ctx context.Context, sessionID string, msgs []*schema.Message) []*schema.Message {
	// 以「用户消息数」为轮数 —— 一轮 = 用户问一次
	if countTurns(msgs) <= m.maxTurns {
		return msgs
	}

	cut := splitAtTurn(msgs, countTurns(msgs)-m.keepTurns)
	if cut <= 0 || cut >= len(msgs) {
		return msgs
	}
	older, recent := msgs[:cut], msgs[cut:]

	// 没有 summarizer 时直接丢弃早期对话。
	// 丢信息不理想，但比让整个请求失败好。
	if m.summarizer == nil {
		m.logger.Warn("no summarizer configured; dropping early turns",
			"session", sessionID, "dropped", len(older))
		return recent
	}

	summary, err := m.summarize(ctx, older)
	if err != nil {
		// 摘要失败同样降级为丢弃 —— 不能因为 LLM 抖动就让用户没法聊天
		m.logger.Warn("summarization failed; dropping early turns",
			"session", sessionID, "err", err)
		return recent
	}

	// ⚠️ 把摘要写回 Redis，替换掉被压缩的那段。
	// 不写回的话每次请求都要重新压缩一遍，既慢又费钱。
	if err := m.persistSummary(ctx, sessionID, summary, recent); err != nil {
		m.logger.Warn("could not persist the summary; it will be recomputed next time",
			"session", sessionID, "err", err)
	}

	return append([]*schema.Message{
		schema.SystemMessage(summaryPrefix + summary),
	}, recent...)
}

// summarize 调 LLM 压缩对话。
func (m *SessionMemory) summarize(ctx context.Context, msgs []*schema.Message) (string, error) {
	var sb strings.Builder
	sb.WriteString("请把下面的对话压缩成一段简短摘要，保留：" +
		"用户提到的具体文件名、查询过的主题、已确认的结论。" +
		"省略寒暄与重复内容。直接输出摘要，不要解释。\n\n")

	for _, msg := range msgs {
		role := "用户"
		switch msg.Role {
		case schema.Assistant:
			role = "助手"
		case schema.Tool:
			role = "工具结果"
		case schema.System:
			role = "系统"
		}
		fmt.Fprintf(&sb, "%s：%s\n", role, truncateRunes(msg.Content, 300))
	}

	summary, err := m.summarizer.Complete(ctx, sb.String())
	if err != nil {
		return "", err
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "", fmt.Errorf("summarizer returned empty text")
	}
	return summary, nil
}

// persistSummary 用「摘要 + 最近若干轮」重写整个会话。
func (m *SessionMemory) persistSummary(ctx context.Context, sessionID, summary string, recent []*schema.Message) error {
	payloads := make([]any, 0, len(recent)+1)

	b, err := json.Marshal(storedMessage{
		Role:    string(schema.System),
		Content: summaryPrefix + summary,
	})
	if err != nil {
		return err
	}
	payloads = append(payloads, b)

	for _, msg := range recent {
		b, err := json.Marshal(storedMessage{
			Role: string(msg.Role), Content: msg.Content, ToolName: msg.ToolName,
		})
		if err != nil {
			return err
		}
		payloads = append(payloads, b)
	}

	key := sessionKey(sessionID)
	// 用事务保证「删除 + 重建」的原子性 ——
	// 中间崩溃会让会话只剩一半，比不压缩糟糕得多
	pipe := m.rdb.TxPipeline()
	pipe.Del(ctx, key)
	pipe.RPush(ctx, key, payloads...)
	pipe.Expire(ctx, key, m.ttl)
	_, err = pipe.Exec(ctx)
	return err
}

// Clear 删除整个会话。
func (m *SessionMemory) Clear(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("sessionID must not be empty")
	}
	return m.rdb.Del(ctx, sessionKey(sessionID)).Err()
}

// ---- 纯函数部分（无 IO，便于单测） ----

// countTurns 以用户消息数作为轮数。
func countTurns(msgs []*schema.Message) int {
	var n int
	for _, msg := range msgs {
		if msg.Role == schema.User {
			n++
		}
	}
	return n
}

// splitAtTurn 返回第 n 轮用户消息的下标，用于切分历史。
// n <= 0 时返回 0（不切）。
func splitAtTurn(msgs []*schema.Message, n int) int {
	if n <= 0 {
		return 0
	}
	var seen int
	for i, msg := range msgs {
		if msg.Role != schema.User {
			continue
		}
		seen++
		if seen > n {
			return i
		}
	}
	return len(msgs)
}

// capTokens 在估算 token 超限时从最早的消息开始丢弃。
//
// ⚠️ 摘要（system 角色）优先保留：它浓缩了最多信息，
// 丢掉它等于白压缩一场。
//
// ⚠️ 本函数**不修改入参指向的消息**。早期实现直接改 msg.Content
// 做截断，但切片里存的是指针，改动会顺着 Load 的返回值传播到
// 调用方持有的同一批对象上 —— 表现为「读一次历史，存量数据就被截短一次」。
// 需要截断时一律复制新对象。
func capTokens(msgs []*schema.Message, limit int) []*schema.Message {
	if estimateTokens(msgs) <= limit {
		return msgs
	}

	kept := make([]*schema.Message, len(msgs))
	copy(kept, msgs)

	// 从最早的开始丢，但跳过摘要
	i := 0
	for i < len(kept) && estimateTokens(kept) > limit {
		if strings.HasPrefix(kept[i].Content, summaryPrefix) {
			i++ // 保留摘要，看下一条
			continue
		}
		kept = append(kept[:i], kept[i+1:]...)
		// 不推进 i：删除后当前下标已经是下一条
	}

	// 极端情况：能丢的都丢完了仍超限（只剩摘要，或摘要本身过长）。
	// 此时截断内容 —— 返回超限内容会让上游 API 直接报错，
	// 给个短的至少还能对话。
	if estimateTokens(kept) > limit {
		for j, msg := range kept {
			clone := *msg // 复制后再改，绝不碰调用方的对象
			clone.Content = truncateRunes(clone.Content, limit)
			kept[j] = &clone
		}
	}
	return kept
}

// estimateTokens 粗估 token 数。
//
// 用「中文字符 ≈ 1 token，英文 ≈ 4 字符 1 token」的经验规则。
// 不追求精确 —— 这只是硬截断的兜底，真正的限制由服务端执行。
// 精确计数需要引 tokenizer 库，为一个兜底逻辑不值得。
func estimateTokens(msgs []*schema.Message) int {
	var total int
	for _, msg := range msgs {
		total += estimateStringTokens(msg.Content)
		total += 4 // 每条消息的角色标记等固定开销
	}
	return total
}

func estimateStringTokens(s string) int {
	var cjk, other int
	for _, r := range s {
		if r > 0x2E80 { // CJK 及以上的码点
			cjk++
		} else {
			other++
		}
	}
	return cjk + other/4
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}
