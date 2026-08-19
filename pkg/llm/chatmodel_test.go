package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// fakeAgentServer 捕获请求并按脚本回复。
func fakeAgentServer(t *testing.T, handler func(req agentRequest) (int, string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		status, body := handler(req)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func jsonReply(msg map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": msg}},
	})
	return string(b)
}

// sseReply 把若干 delta 拼成一个 SSE 响应体。
func sseReply(deltas ...map[string]any) string {
	var sb strings.Builder
	for _, d := range deltas {
		b, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"delta": d}},
		})
		fmt.Fprintf(&sb, "data: %s\n\n", b)
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

func TestEinoChatModel_Generate(t *testing.T) {
	srv := fakeAgentServer(t, func(req agentRequest) (int, string) {
		if len(req.Messages) != 1 || req.Messages[0].Content != "你好" {
			t.Errorf("unexpected messages: %+v", req.Messages)
		}
		if req.Stream {
			t.Error("Generate must not request streaming")
		}
		return http.StatusOK, jsonReply(map[string]any{
			"role": "assistant", "content": "你好呀",
		})
	})

	c := NewEinoChatModel("k", srv.URL, "m")
	got, err := c.Generate(context.Background(), []*schema.Message{schema.UserMessage("你好")})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got.Content != "你好呀" {
		t.Errorf("expected %q, got %q", "你好呀", got.Content)
	}
	if got.Role != schema.Assistant {
		t.Errorf("expected the assistant role, got %q", got.Role)
	}
}

func TestEinoChatModel_GenerateParsesToolCalls(t *testing.T) {
	// Agent 靠 tool_calls 决定调什么工具 —— 解析错了整个 ReAct 循环就断了
	srv := fakeAgentServer(t, func(agentRequest) (int, string) {
		return http.StatusOK, jsonReply(map[string]any{
			"role": "assistant", "content": "",
			"tool_calls": []map[string]any{{
				"id": "call_1", "type": "function",
				"function": map[string]any{
					"name": "rag_query", "arguments": `{"query":"Redis"}`,
				},
			}},
		})
	})

	c := NewEinoChatModel("k", srv.URL, "m")
	got, err := c.Generate(context.Background(), []*schema.Message{schema.UserMessage("找 Redis 文档")})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(got.ToolCalls))
	}
	tc := got.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "rag_query" {
		t.Errorf("unexpected tool call: %+v", tc)
	}
	if tc.Function.Arguments != `{"query":"Redis"}` {
		t.Errorf("arguments were mangled: %q", tc.Function.Arguments)
	}
}

func TestEinoChatModel_CallOptionToolsOverrideBoundTools(t *testing.T) {
	// ⚠️ 这是最容易搞反的地方。
	// Agent 每轮通过 model.WithTools 传工具；若预绑定的工具优先，
	// Agent 传来的工具会被旧列表盖掉，表现为模型突然「忘记」自己有工具。
	var seen []string
	srv := fakeAgentServer(t, func(req agentRequest) (int, string) {
		seen = nil
		for _, tl := range req.Tools {
			seen = append(seen, tl.Function.Name)
		}
		return http.StatusOK, jsonReply(map[string]any{"role": "assistant", "content": "ok"})
	})

	base := NewEinoChatModel("k", srv.URL, "m")
	bound, err := base.WithTools([]*schema.ToolInfo{{Name: "old_tool", Desc: "旧的"}})
	if err != nil {
		t.Fatalf("with tools: %v", err)
	}

	_, err = bound.Generate(context.Background(),
		[]*schema.Message{schema.UserMessage("x")},
		model.WithTools([]*schema.ToolInfo{{Name: "new_tool", Desc: "新的"}}))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if len(seen) != 1 || seen[0] != "new_tool" {
		t.Errorf("call-option tools must win, got %v", seen)
	}
}

func TestEinoChatModel_WithToolsDoesNotMutateReceiver(t *testing.T) {
	// eino 的语义是「返回一个新模型」。若原地修改，
	// 多个 Agent 共享同一实例时会互相覆盖工具列表。
	srv := fakeAgentServer(t, func(agentRequest) (int, string) {
		return http.StatusOK, jsonReply(map[string]any{"role": "assistant", "content": "ok"})
	})

	base := NewEinoChatModel("k", srv.URL, "m")
	if _, err := base.WithTools([]*schema.ToolInfo{{Name: "t", Desc: "d"}}); err != nil {
		t.Fatalf("with tools: %v", err)
	}
	if len(base.tools) != 0 {
		t.Errorf("WithTools must not mutate the receiver, got %d tools", len(base.tools))
	}
}

func TestEinoChatModel_Stream(t *testing.T) {
	srv := fakeAgentServer(t, func(req agentRequest) (int, string) {
		if !req.Stream {
			t.Error("Stream must request streaming")
		}
		return http.StatusOK, sseReply(
			map[string]any{"role": "assistant", "content": "缓存"},
			map[string]any{"content": "穿透"},
			map[string]any{"content": "的解法"},
		)
	})

	c := NewEinoChatModel("k", srv.URL, "m")
	sr, err := c.Stream(context.Background(), []*schema.Message{schema.UserMessage("讲讲")})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer sr.Close()

	var sb strings.Builder
	var chunks int
	for {
		msg, err := sr.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		chunks++
		sb.WriteString(msg.Content)
	}

	if got := sb.String(); got != "缓存穿透的解法" {
		t.Errorf("expected the concatenated text, got %q", got)
	}
	// 必须是逐片到达而非一次性返回 —— 否则流式没有意义
	if chunks != 3 {
		t.Errorf("expected 3 incremental chunks, got %d", chunks)
	}
}

func TestEinoChatModel_StreamSkipsEmptyHeartbeats(t *testing.T) {
	// 心跳分片（无内容无工具调用）不该作为消息发给下游，
	// 否则 SSE handler 会往客户端推一堆空 data 帧
	srv := fakeAgentServer(t, func(agentRequest) (int, string) {
		return http.StatusOK, sseReply(
			map[string]any{"role": "assistant", "content": ""},
			map[string]any{"content": "答案"},
			map[string]any{},
		)
	})

	c := NewEinoChatModel("k", srv.URL, "m")
	sr, err := c.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer sr.Close()

	var chunks int
	for {
		if _, err := sr.Recv(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("recv: %v", err)
		}
		chunks++
	}
	if chunks != 1 {
		t.Errorf("expected only the 1 non-empty chunk, got %d", chunks)
	}
}

func TestEinoChatModel_StreamSurfacesAPIError(t *testing.T) {
	// 流已经开始后出现的错误不能静默丢弃 ——
	// 那会让用户看到一段截断的回答却以为是完整的
	srv := fakeAgentServer(t, func(agentRequest) (int, string) {
		body, _ := json.Marshal(map[string]any{
			"error": map[string]string{"message": "rate limited"},
		})
		return http.StatusOK, fmt.Sprintf("data: %s\n\n", body)
	})

	c := NewEinoChatModel("k", srv.URL, "m")
	sr, err := c.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer sr.Close()

	var gotErr error
	for {
		_, err := sr.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "rate limited") {
		t.Errorf("expected the API error to surface, got %v", gotErr)
	}
}

func TestEinoChatModel_StreamParsesToolCallDeltas(t *testing.T) {
	// 工具调用的参数会分多片到达，index 是归并依据 ——
	// 丢了 index 就无法把分片拼回同一个调用
	srv := fakeAgentServer(t, func(agentRequest) (int, string) {
		return http.StatusOK, sseReply(
			map[string]any{"role": "assistant", "tool_calls": []map[string]any{{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": "rag_query", "arguments": `{"que`},
			}}},
			map[string]any{"tool_calls": []map[string]any{{
				"index":    0,
				"function": map[string]any{"arguments": `ry":"Redis"}`},
			}}},
		)
	})

	c := NewEinoChatModel("k", srv.URL, "m")
	sr, err := c.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer sr.Close()

	var args strings.Builder
	var sawIndex bool
	for {
		msg, err := sr.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		for _, tc := range msg.ToolCalls {
			if tc.Index != nil && *tc.Index == 0 {
				sawIndex = true
			}
			args.WriteString(tc.Function.Arguments)
		}
	}

	if !sawIndex {
		t.Error("the tool-call index must be preserved for chunk merging")
	}
	if got := args.String(); got != `{"query":"Redis"}` {
		t.Errorf("expected the reassembled arguments, got %q", got)
	}
}

func TestEinoChatModel_SurfacesHTTPError(t *testing.T) {
	srv := fakeAgentServer(t, func(agentRequest) (int, string) {
		return http.StatusUnauthorized, `{"msg":"bad key"}`
	})

	c := NewEinoChatModel("bad", srv.URL, "m")
	if _, err := c.Generate(context.Background(), []*schema.Message{schema.UserMessage("x")}); err == nil {
		t.Fatal("expected an error for HTTP 401")
	}
	if _, err := c.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")}); err == nil {
		t.Fatal("expected a stream error for HTTP 401")
	}
}

func TestEinoChatModel_RejectsEmptyInput(t *testing.T) {
	c := NewEinoChatModel("k", "http://unused", "m")
	if _, err := c.Generate(context.Background(), nil); err == nil {
		t.Error("expected an error for empty input")
	}
	if _, err := c.Generate(context.Background(), []*schema.Message{nil}); err == nil {
		t.Error("expected an error when input holds only nil messages")
	}
}

func TestEinoChatModel_SendsToolSchema(t *testing.T) {
	// 模型靠 parameters 里的 JSON Schema 决定怎么填参数。
	// 漏掉它模型会瞎传，且错误难定位。
	var params any
	srv := fakeAgentServer(t, func(req agentRequest) (int, string) {
		if len(req.Tools) == 1 {
			params = req.Tools[0].Function.Parameters
		}
		return http.StatusOK, jsonReply(map[string]any{"role": "assistant", "content": "ok"})
	})

	c := NewEinoChatModel("k", srv.URL, "m")
	_, err := c.Generate(context.Background(),
		[]*schema.Message{schema.UserMessage("x")},
		model.WithTools([]*schema.ToolInfo{{
			Name: "rag_query", Desc: "搜索",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"query": {Type: schema.String, Desc: "查询", Required: true},
			}),
		}}))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	encoded, _ := json.Marshal(params)
	if !strings.Contains(string(encoded), "query") {
		t.Errorf("the tool parameter schema must reach the wire, got %s", encoded)
	}
}

func TestEinoChatModel_ToolMessageRoundTrip(t *testing.T) {
	// 工具执行结果要带 tool_call_id 回传，模型据此把结果对上请求。
	// 丢了它模型会认为工具没执行，从而重复调用 —— 正是 MaxIterations 想防的死循环。
	var got agentMessage
	srv := fakeAgentServer(t, func(req agentRequest) (int, string) {
		got = req.Messages[len(req.Messages)-1]
		return http.StatusOK, jsonReply(map[string]any{"role": "assistant", "content": "好的"})
	})

	c := NewEinoChatModel("k", srv.URL, "m")
	_, err := c.Generate(context.Background(), []*schema.Message{
		schema.UserMessage("找文档"),
		schema.ToolMessage("找到 3 篇", "call_1", schema.WithToolName("rag_query")),
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if got.ToolCallID != "call_1" {
		t.Errorf("tool_call_id must survive the round trip, got %q", got.ToolCallID)
	}
	if got.Role != string(schema.Tool) {
		t.Errorf("expected the tool role, got %q", got.Role)
	}
}
