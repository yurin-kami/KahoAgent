// Package handler 提供 HTTP 接口。
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/yurin-kami/KahoAgent/internal/agent"
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
}

func NewChatHandler(a *agent.Agent, m Memory, logger *slog.Logger) (*ChatHandler, error) {
	if a == nil {
		return nil, fmt.Errorf("agent must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	// memory 可为 nil —— 此时退化成无状态单轮对话
	return &ChatHandler{agent: a, memory: m, logger: logger}, nil
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
	// Type 取值：delta（增量文本）/ tool（工具调用中）/ done / error
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Tool string `json:"tool,omitempty"`
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

	userMsg := schema.UserMessage(req.Message)
	messages := append(history, userMsg)

	var answer strings.Builder
	consumeErr := agent.Consume(ctx, h.agent.Run(ctx, messages), func(c agent.Chunk) error {
		switch {
		case c.Err != nil:
			return send(sseEvent{Type: "error", Text: userFacingError(c.Err)})
		case c.IsToolResult():
			// 工具结果不推给用户（那是给模型看的中间产物），
			// 但推一个提示事件，让前端能显示「正在检索…」
			return send(sseEvent{Type: "tool", Tool: c.ToolName})
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
	default:
		return "处理请求时出错，请稍后重试。"
	}
}
