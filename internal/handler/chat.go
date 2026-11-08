// Package handler 提供 HTTP 接口。
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/yurin-kami/KahoAgent/internal/agent"
	"github.com/yurin-kami/KahoAgent/internal/metrics"
	"github.com/yurin-kami/KahoAgent/internal/reqstats"
)

// Memory 是 chat handler 依赖的会话存储。
// 抽成接口便于测试注入，也允许将来换实现（如内存版）。
type Memory interface {
	Load(ctx context.Context, sessionID string) ([]*schema.Message, error)
	Append(ctx context.Context, sessionID string, msgs ...*schema.Message) error
}

// chatTimeout 限制单次对话的最长耗时。
// ReAct 可能要调好几轮工具，每轮都要等模型，180 秒是留了余量的上限；
// 没有上限的话一个卡住的请求会永久占着连接。
const chatTimeout = 180 * time.Second

// ChatHandler 处理 SSE 流式对话。
//
// 为什么用 SSE 而不是 WebSocket：
// LLM 输出是**单向**的（服务端 → 客户端），SSE 基于普通 HTTP，
// 不需要协议升级，浏览器还自带断线重连。WebSocket 的双向能力
// 在这里用不上，徒增复杂度。
type ChatHandler struct {
	agent  *agent.Agent
	memory Memory
	logger *slog.Logger
	// maxHistoryChars 是送入模型的历史字符预算。
	// 会话记忆本身有摘要压缩，但压缩是「按轮次」的，长文档粘贴
	// 仍可能让单轮历史爆炸 —— 这是最后一道闸。
	maxHistoryChars int
}

// defaultHistoryChars 是未配置时的历史预算。
// 中文约 1 字 ≈ 1 token，12000 字对应一次请求约 12k token 的输入。
const defaultHistoryChars = 12000

// ChatHandlerOption 是可选配置。
type ChatHandlerOption func(*ChatHandler)

// WithHistoryBudget 设置历史字符预算（<=0 表示不限制）。
func WithHistoryBudget(chars int) ChatHandlerOption {
	return func(h *ChatHandler) {
		if chars > 0 {
			h.maxHistoryChars = chars
		}
	}
}

func NewChatHandler(a *agent.Agent, m Memory, logger *slog.Logger, opts ...ChatHandlerOption) (*ChatHandler, error) {
	if a == nil {
		return nil, fmt.Errorf("agent must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	// memory 可为 nil —— 此时退化成无状态单轮对话
	h := &ChatHandler{
		agent:           a,
		memory:          m,
		logger:          logger,
		maxHistoryChars: defaultHistoryChars,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

type chatRequest struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

// sseEvent 是推给客户端的事件负载。
//
// 用 JSON 而非裸文本：文本里可能含换行，而 SSE 用 "\n\n" 分隔事件 ——
// 裸推会把一条消息拆成多个事件，客户端看到的内容就断了。
// JSON 编码后换行变成 \n 转义，天然安全。
type sseEvent struct {
	// Type 取值：delta（增量文本）/ tool（工具调用中）/ sources（本次检索的来源）
	// / done / error
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Tool string `json:"tool,omitempty"`
	// Sources 只在 Type=sources 时出现，是去重后的来源列表。
	Sources []string `json:"sources,omitempty"`

	// 以下字段只在 Type=stats 时出现，是本次请求的用量统计。
	// 取 0 时省略：前端据此判断「有没有统计」，
	// 而不是把 0 token 当成真实值展示。
	PromptTokens     int  `json:"prompt_tokens,omitempty"`
	CompletionTokens int  `json:"completion_tokens,omitempty"`
	CacheHit         bool `json:"cache_hit,omitempty"`
}

// sourceLineRE 匹配 rag_query 工具输出里的来源行。
//
// 为什么不给工具输出加一个 JSON 信封、让前端直接解析结构化数据：
// 工具输出是给**模型**读的，塞进 JSON 会占掉宝贵的上下文、还会干扰
// 模型对内容的理解。让模型读人话、让前端解析固定格式，各取所需。
//
// 这个格式与 internal/agent/tools/rag_query.go 的 formatDocs 是耦合的，
// 改那边必须同步改这里 —— 所以有单测盯着（chat_sources_test.go）。
var sourceLineRE = regexp.MustCompile(`【片段\s*\d+】来源:\s*(.+)`)

// maxSources 限制前端展示的来源条数。
// 检索返回 top-5，但模型可能多次调用工具，全部展示会把侧栏刷爆。
const maxSources = 8

// parseSources 从工具输出里提取来源，按出现顺序去重。
func parseSources(toolOutput string) []string {
	if toolOutput == "" {
		return nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0, maxSources)
	for _, line := range strings.Split(toolOutput, "\n") {
		m := sourceLineRE.FindStringSubmatch(strings.TrimSpace(line))
		if len(m) != 2 {
			continue
		}
		src := strings.TrimSpace(m[1])
		if src == "" || seen[src] {
			continue
		}
		seen[src] = true
		out = append(out, src)
		if len(out) >= maxSources {
			break
		}
	}
	return out
}

func (h *ChatHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		http.Error(w, "message must not be empty", http.StatusBadRequest)
		return
	}

	// ⚠️ Flusher 检查必须在写任何响应之前。
	// 不支持 Flush 的话数据会积在 buffer 里，等整段生成完才一次性吐出，
	// 流式就完全失去意义了 —— 而且用户看不出区别，只觉得「慢」。
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported by this connection", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// 禁用 nginx 等反代的缓冲，否则它们会攒够一批再转发，
	// 首字延迟又回到了非流式的水平
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // 立刻把响应头推出去，客户端才知道连接已建立

	// 请求 context 会在客户端断开时取消，用它做超时的父 context ——
	// 这样断开时正在进行的模型调用会一并取消，不会白烧 token
	ctx, cancel := context.WithTimeout(r.Context(), chatTimeout)
	defer cancel()

	// ⚠️ Inc 和 Dec 必须成对，且 Dec 要用 defer。
	// 中途 return 漏掉 Dec 的话，Gauge 只增不减，最后显示几万个
	// "活跃会话" —— 而这种指标一旦失真就再也没人信它了。
	metrics.ActiveSessions.Inc()
	defer metrics.ActiveSessions.Dec()

	h.stream(ctx, w, flusher, req)
}

func (h *ChatHandler) stream(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, req chatRequest) {
	send := func(ev sseEvent) error {
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			// 写失败基本等同客户端断开，返回错误让上层停止消费
			return fmt.Errorf("write to client: %w", err)
		}
		flusher.Flush() // ⚠️ 每条都要 Flush，否则还是攒批
		return nil
	}

	history, err := h.loadHistory(ctx, req.SessionID)
	if err != nil {
		h.logger.Warn("could not load history; continuing without it",
			"session", req.SessionID, "err", err)
		// 历史加载失败降级为单轮对话，而不是让用户没法聊 ——
		// 丢上下文比服务不可用好
	}
	history = trimHistory(history, h.maxHistoryChars)

	userMsg := schema.UserMessage(req.Message)
	messages := append(history, userMsg)

	// 请求级统计（token 用量、缓存命中）。
	// 放进 ctx 让 LLM 客户端与检索器埋点，结束时一次性推给前端 ——
	// 比让前端去解析日志或查 Prometheus 现实得多。
	stats := reqstats.New()
	ctx = reqstats.With(ctx, stats)

	var answer strings.Builder
	consumeErr := agent.Consume(ctx, h.agent.Run(ctx, messages), func(c agent.Chunk) error {
		switch {
		case c.Err != nil:
			return send(sseEvent{Type: "error", Text: userFacingError(c.Err)})
		case c.IsToolResult():
			// 工具结果不推给用户（那是给模型看的中间产物），
			// 但推一个提示事件，让前端能显示「正在检索…」
			if err := send(sseEvent{Type: "tool", Tool: c.ToolName}); err != nil {
				return err
			}
			// 再推一条来源事件：前端据此渲染「引用」列表。
			// 来源解析失败（格式变了）时静默不发，不影响主流程。
			if srcs := parseSources(c.ToolResult); len(srcs) > 0 {
				return send(sseEvent{Type: "sources", Sources: srcs})
			}
			return nil
		case c.Text != "":
			answer.WriteString(c.Text)
			return send(sseEvent{Type: "delta", Text: c.Text})
		}
		return nil
	})

	if consumeErr != nil {
		h.logger.Warn("chat stream ended early", "session", req.SessionID, "err", consumeErr)
		// 客户端已断开时再写也没用，但错误事件本身是幂等的，写失败就算了
		_ = send(sseEvent{Type: "error", Text: userFacingError(consumeErr)})
		return
	}

	// ⚠️ 只有成功完成才写入历史。
	// 中途失败仍然写的话，下一轮会带上一段残缺的回答，
	// 模型会以为自己说过那些话，越聊越偏。
	h.saveHistory(ctx, req.SessionID, userMsg, answer.String())

	// 先推统计再推 done：前端在收到 done 时就能确定所有数字已到齐，
	// 不需要再等一个可能永远不会来的事件。
	snap := stats.Snapshot()
	_ = send(sseEvent{
		Type:             "stats",
		PromptTokens:     snap.PromptTokens,
		CompletionTokens: snap.CompletionTokens,
		CacheHit:         snap.CacheHit,
	})
	_ = send(sseEvent{Type: "done"})
}

func (h *ChatHandler) loadHistory(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	if h.memory == nil || sessionID == "" {
		return nil, nil
	}
	return h.memory.Load(ctx, sessionID)
}

func (h *ChatHandler) saveHistory(ctx context.Context, sessionID string, userMsg *schema.Message, answer string) {
	if h.memory == nil || sessionID == "" || answer == "" {
		return
	}
	// ⚠️ 用 WithoutCancel：此刻请求的 context 可能已因客户端断开而取消，
	// 但对话已经成功完成了，历史必须存下来 ——
	// 否则用户刷新页面会发现刚才那轮消失了。
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := h.memory.Append(saveCtx, sessionID, userMsg,
		schema.AssistantMessage(answer, nil)); err != nil {
		h.logger.Warn("could not persist the conversation",
			"session", sessionID, "err", err)
	}
}

// trimHistory 从最旧的消息开始丢弃，直到总字符数落在预算内。
//
// 为什么是「丢最旧的」而不是「报错」或「截断中间」：
// 对话的语义依赖最近几轮，最旧的内容对当前问题贡献最小。
// 报错则等于惩罚聊得久的用户 —— 而他们恰恰是最活跃的那批。
//
// ⚠️ 字符数只是 token 的近似（中文约 1:1，英文约 4:1），
// 用它而不是真实 tokenizer 是为了不把分词器引入请求路径：
// 预估偏差由预算留出余量来吸收，而这里的目标是「不炸上下文」，
// 不是精确计费。
func trimHistory(history []*schema.Message, budget int) []*schema.Message {
	if budget <= 0 || len(history) == 0 {
		return history
	}

	total := 0
	cut := len(history)
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m == nil {
			cut = i
			continue
		}
		l := len(m.Content)
		// 最后一条无论如何保留：宁可超预算，也不能把用户刚说的话丢掉
		if total+l > budget && cut != len(history) {
			break
		}
		total += l
		cut = i
	}
	if cut == 0 {
		return history
	}

	trimmed := history[cut:]
	// 切口如果落在一轮中间（开头是 assistant），继续丢到下一个 user ——
	// 半轮上下文会让模型误以为上一句是它自己抛出的问题。
	for len(trimmed) > 1 && trimmed[0] != nil && trimmed[0].Role != schema.User {
		trimmed = trimmed[1:]
	}
	return trimmed
}

// userFacingError 把内部错误转成能给用户看的说明。
//
// 不直接暴露原始错误：里面可能有连接串、内网地址等信息，
// 而且「dial tcp 127.0.0.1:19530: connection refused」对用户毫无意义。
func userFacingError(err error) string {
	switch {
	case err == nil:
		return ""
	case strings.Contains(err.Error(), context.DeadlineExceeded.Error()):
		return "处理超时，请稍后重试或换个更具体的问题。"
	case strings.Contains(err.Error(), context.Canceled.Error()):
		return "请求已取消。"
	case strings.Contains(err.Error(), "401"),
		strings.Contains(err.Error(), "Authentication"),
		strings.Contains(err.Error(), "invalid api key"):
		// 鉴权失败是最常见的部署问题，给出可操作的提示而不是「处理出错」。
		// 注意不要回显 key 本身 —— 错误信息可能被日志或前端看到。
		return "模型服务鉴权失败：API Key 无效或已过期，请检查 config.toml 或环境变量。"
	case strings.Contains(err.Error(), "429"),
		strings.Contains(err.Error(), "Throttling"),
		strings.Contains(err.Error(), "rate limit"):
		return "模型服务限流（429），请稍后重试；若配置了备用模型会自动切换。"
	default:
		return "处理请求时出错，请稍后重试。"
	}
}
