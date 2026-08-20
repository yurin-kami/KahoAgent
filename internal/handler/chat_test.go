package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/yurin-kami/KahoAgent/internal/agent"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// scriptedModel 逐字返回预设文本，驱动真实的 Agent 而非 mock 掉它。
// 这样测的是「handler + agent + 流式」这条完整链路。
type scriptedModel struct {
	reply string
	calls int32
}

func (m *scriptedModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	atomic.AddInt32(&m.calls, 1)
	return schema.AssistantMessage(m.reply, nil), nil
}

func (m *scriptedModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	atomic.AddInt32(&m.calls, 1)
	sr, sw := schema.Pipe[*schema.Message](8)
	go func() {
		defer sw.Close()
		for _, r := range m.reply {
			sw.Send(schema.AssistantMessage(string(r), nil), nil)
		}
	}()
	return sr, nil
}

// fakeMemory 记录读写，便于断言历史行为。
type fakeMemory struct {
	history  []*schema.Message
	loadErr  error
	appended []*schema.Message
	appendN  int32
}

func (m *fakeMemory) Load(context.Context, string) ([]*schema.Message, error) {
	return m.history, m.loadErr
}

func (m *fakeMemory) Append(_ context.Context, _ string, msgs ...*schema.Message) error {
	atomic.AddInt32(&m.appendN, 1)
	m.appended = append(m.appended, msgs...)
	return nil
}

func newHandler(t *testing.T, reply string, mem Memory) *ChatHandler {
	t.Helper()
	a, err := agent.New(context.Background(), agent.Config{Model: &scriptedModel{reply: reply}})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	h, err := NewChatHandler(a, mem, quietLogger())
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return h
}

// parseSSE 解析响应体里的所有事件。
func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("malformed SSE payload %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestChat_StreamsDeltasThenDone(t *testing.T) {
	h := newHandler(t, "缓存穿透", nil)
	rec := post(t, h, `{"message":"讲讲缓存"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	events := parseSSE(t, rec.Body.String())
	if len(events) < 2 {
		t.Fatalf("expected several events, got %d: %s", len(events), rec.Body.String())
	}

	var text strings.Builder
	for _, ev := range events {
		if ev.Type == "delta" {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != "缓存穿透" {
		t.Errorf("expected the concatenated answer, got %q", text.String())
	}
	// 必须以 done 收尾，否则客户端不知道什么时候结束
	if last := events[len(events)-1]; last.Type != "done" {
		t.Errorf("the stream must end with a done event, got %q", last.Type)
	}
}

func TestChat_SetsSSEHeaders(t *testing.T) {
	h := newHandler(t, "答案", nil)
	rec := post(t, h, `{"message":"x"}`)

	want := map[string]string{
		"Content-Type":  "text/event-stream",
		"Cache-Control": "no-cache",
		// ⚠️ 没有这个头的话，nginx 等反代会攒够一批再转发，
		// 首字延迟又回到非流式水平 —— 而且本地测不出来
		"X-Accel-Buffering": "no",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
}

func TestChat_EventPayloadIsJSONEncoded(t *testing.T) {
	// ⚠️ SSE 用 "\n\n" 分隔事件。裸推含换行的文本会把一条消息
	// 拆成多个事件，客户端看到的内容就断了。JSON 编码后换行
	// 变成 \n 转义，天然安全。
	h := newHandler(t, "第一行\n\n第二行", nil)
	rec := post(t, h, `{"message":"x"}`)

	events := parseSSE(t, rec.Body.String())
	var text strings.Builder
	for _, ev := range events {
		if ev.Type == "delta" {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != "第一行\n\n第二行" {
		t.Errorf("newlines must survive the SSE encoding, got %q", text.String())
	}
}

func TestChat_RejectsBadRequests(t *testing.T) {
	h := newHandler(t, "答案", nil)

	cases := []struct {
		name, body string
		want       int
	}{
		{"malformed JSON", `{broken`, http.StatusBadRequest},
		{"empty message", `{"message":""}`, http.StatusBadRequest},
		{"whitespace only", `{"message":"   "}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := post(t, h, tc.body); rec.Code != tc.want {
				t.Errorf("expected %d, got %d", tc.want, rec.Code)
			}
		})
	}
}

func TestChat_RejectsNonPost(t *testing.T) {
	h := newHandler(t, "答案", nil)
	req := httptest.NewRequest(http.MethodGet, "/api/chat", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestChat_LoadsAndSavesHistory(t *testing.T) {
	mem := &fakeMemory{history: []*schema.Message{
		schema.UserMessage("之前的问题"),
		schema.AssistantMessage("之前的回答", nil),
	}}
	h := newHandler(t, "新回答", mem)

	rec := post(t, h, `{"session_id":"s1","message":"新问题"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	if atomic.LoadInt32(&mem.appendN) != 1 {
		t.Fatalf("expected exactly one append, got %d", mem.appendN)
	}
	if len(mem.appended) != 2 {
		t.Fatalf("expected the user message and the answer to be saved, got %d", len(mem.appended))
	}
	if mem.appended[0].Content != "新问题" || mem.appended[1].Content != "新回答" {
		t.Errorf("unexpected saved messages: %+v", mem.appended)
	}
}

func TestChat_HistoryFailureDegradesInsteadOfFailing(t *testing.T) {
	// 丢上下文比服务不可用好 —— Redis 抖动不该让用户没法聊天
	mem := &fakeMemory{loadErr: errors.New("redis down")}
	h := newHandler(t, "答案", mem)

	rec := post(t, h, `{"session_id":"s1","message":"x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a history failure must not fail the request, got %d", rec.Code)
	}

	events := parseSSE(t, rec.Body.String())
	if len(events) == 0 || events[len(events)-1].Type != "done" {
		t.Errorf("expected a normal completion, got %+v", events)
	}
}

func TestChat_SkipsHistoryWithoutSessionID(t *testing.T) {
	// 没有 session_id 就是无状态单轮对话，不该写库
	mem := &fakeMemory{}
	h := newHandler(t, "答案", mem)

	post(t, h, `{"message":"x"}`)

	if n := atomic.LoadInt32(&mem.appendN); n != 0 {
		t.Errorf("expected no history writes without a session id, got %d", n)
	}
}

func TestNewChatHandler_RejectsNilAgent(t *testing.T) {
	if _, err := NewChatHandler(nil, nil, nil); err == nil {
		t.Fatal("expected an error for a nil agent")
	}
}

func TestUserFacingError_HidesInternals(t *testing.T) {
	// 原始错误可能含连接串、内网地址，且对用户毫无意义
	got := userFacingError(errors.New("dial tcp 127.0.0.1:19530: connection refused"))

	if strings.Contains(got, "127.0.0.1") || strings.Contains(got, "19530") {
		t.Errorf("internal details must not leak to the user: %q", got)
	}
	if got == "" {
		t.Error("expected some user-facing message")
	}
}

func TestUserFacingError_DistinguishesTimeout(t *testing.T) {
	// 超时和一般错误的用户处置不同：前者可以「换个问法重试」
	timeout := userFacingError(fmt.Errorf("wrapped: %w", context.DeadlineExceeded))
	if !strings.Contains(timeout, "超时") {
		t.Errorf("expected a timeout-specific message, got %q", timeout)
	}

	generic := userFacingError(errors.New("something else"))
	if generic == timeout {
		t.Error("a generic error should read differently from a timeout")
	}
}

// nonFlusher 模拟不支持流式的 ResponseWriter
type nonFlusher struct{ http.ResponseWriter }

func TestChat_RequiresFlusher(t *testing.T) {
	// ⚠️ 不支持 Flush 时数据会积在 buffer 里，等整段生成完才吐出。
	// 用户看不出区别，只觉得「慢」—— 所以要在写响应前就明确拒绝。
	h := newHandler(t, "答案", nil)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"message":"x"}`))
	rec := httptest.NewRecorder()

	h.ServeHTTP(nonFlusher{rec}, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 when streaming is unsupported, got %d", rec.Code)
	}
}
