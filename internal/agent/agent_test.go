package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// scriptedModel 按脚本逐轮返回消息，用于驱动 ReAct 循环。
//
// 用假模型而非真 API：ReAct 的循环逻辑、工具结果回传、
// 迭代上限都能在毫秒内确定性地测出来，不花钱也不看网络脸色。
type scriptedModel struct {
	turns []*schema.Message
	calls int32

	// gotTools 记录最后一次收到的工具列表，
	// 用于验证 adk 确实通过调用选项传了工具
	gotTools []string
	// gotMessages 记录最后一次收到的消息，用于验证工具结果回传
	gotMessages []*schema.Message
}

func (m *scriptedModel) record(input []*schema.Message, opts []model.Option) {
	o := model.GetCommonOptions(&model.Options{}, opts...)
	m.gotTools = nil
	for _, t := range o.Tools {
		m.gotTools = append(m.gotTools, t.Name)
	}
	m.gotMessages = input
}

func (m *scriptedModel) Generate(_ context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	m.record(input, opts)
	n := int(atomic.AddInt32(&m.calls, 1)) - 1
	if n >= len(m.turns) {
		return schema.AssistantMessage("（脚本已用尽）", nil), nil
	}
	return m.turns[n], nil
}

func (m *scriptedModel) Stream(_ context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.record(input, opts)
	n := int(atomic.AddInt32(&m.calls, 1)) - 1

	var out *schema.Message
	if n >= len(m.turns) {
		out = schema.AssistantMessage("（脚本已用尽）", nil)
	} else {
		out = m.turns[n]
	}

	sr, sw := schema.Pipe[*schema.Message](8)
	go func() {
		defer sw.Close()
		// 带工具调用的消息不拆分：工具调用参数拆开后需要下游按 index
		// 归并，那是 chatmodel 层的职责，这里只验证 agent 编排
		if len(out.ToolCalls) > 0 {
			sw.Send(out, nil)
			return
		}
		// 纯文本按字符拆成多片，模拟真实流式
		for _, r := range out.Content {
			sw.Send(schema.AssistantMessage(string(r), nil), nil)
		}
	}()
	return sr, nil
}

// echoTool 是一个可控的假工具。
type echoTool struct {
	name   string
	result string
	err    error
	calls  int32
	gotArg string
}

func (t *echoTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: t.name,
		Desc: "测试工具",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"query": {Type: schema.String, Desc: "查询", Required: true},
		}),
	}, nil
}

func (t *echoTool) InvokableRun(_ context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	atomic.AddInt32(&t.calls, 1)
	t.gotArg = argsJSON
	if t.err != nil {
		return "", t.err
	}
	return t.result, nil
}

func toolCallMsg(id, name, args string) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID: id, Type: "function",
		Function: schema.FunctionCall{Name: name, Arguments: args},
	}})
}

func newTestAgent(t *testing.T, m model.BaseChatModel, tools ...tool.BaseTool) *Agent {
	t.Helper()
	a, err := New(context.Background(), Config{Model: m, Tools: tools})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a
}

func TestAgent_AnswersWithoutTools(t *testing.T) {
	m := &scriptedModel{turns: []*schema.Message{
		schema.AssistantMessage("你好呀", nil),
	}}
	a := newTestAgent(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := Collect(ctx, a.Query(ctx, "你好"))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got != "你好呀" {
		t.Errorf("expected %q, got %q", "你好呀", got)
	}
}

func TestAgent_CallsToolThenAnswers(t *testing.T) {
	// 这是 ReAct 的核心路径：模型要求调工具 → 工具执行 →
	// 结果回传给模型 → 模型基于结果作答
	tl := &echoTool{name: "rag_query", result: "找到 3 篇关于 Redis 的文档"}
	m := &scriptedModel{turns: []*schema.Message{
		toolCallMsg("call_1", "rag_query", `{"query":"Redis"}`),
		schema.AssistantMessage("根据文档，Redis 是内存数据库", nil),
	}}
	a := newTestAgent(t, m, tl)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := Collect(ctx, a.Query(ctx, "介绍 Redis"))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	if n := atomic.LoadInt32(&tl.calls); n != 1 {
		t.Errorf("expected the tool to be called once, got %d", n)
	}
	if !strings.Contains(tl.gotArg, "Redis") {
		t.Errorf("the tool arguments must reach the tool, got %q", tl.gotArg)
	}
	if !strings.Contains(got, "内存数据库") {
		t.Errorf("expected the final answer, got %q", got)
	}
	// 工具结果是给模型看的中间产物，不该混进给用户的答案
	if strings.Contains(got, "找到 3 篇") {
		t.Errorf("the tool result must not leak into the user-facing answer: %q", got)
	}
}

func TestAgent_ToolResultIsFedBackToModel(t *testing.T) {
	// 工具结果必须回传给模型，否则模型没有依据作答 ——
	// 这正是 RAG 中「R」到「G」的传递
	tl := &echoTool{name: "rag_query", result: "布隆过滤器可以拦截不存在的 key"}
	m := &scriptedModel{turns: []*schema.Message{
		toolCallMsg("call_1", "rag_query", `{"query":"缓存穿透"}`),
		schema.AssistantMessage("答案", nil),
	}}
	a := newTestAgent(t, m, tl)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Collect(ctx, a.Query(ctx, "缓存穿透怎么办")); err != nil {
		t.Fatalf("collect: %v", err)
	}

	var sawToolResult bool
	for _, msg := range m.gotMessages {
		if msg.Role == schema.Tool && strings.Contains(msg.Content, "布隆过滤器") {
			sawToolResult = true
		}
	}
	if !sawToolResult {
		t.Errorf("the tool result must be fed back to the model, got %+v", m.gotMessages)
	}
}

func TestAgent_PassesToolsToModel(t *testing.T) {
	// adk 通过 model.WithTools 调用选项传工具（而非 WithTools 方法）。
	// 传不到的话模型不知道自己有工具，永远不会调用。
	tl := &echoTool{name: "rag_query", result: "ok"}
	m := &scriptedModel{turns: []*schema.Message{schema.AssistantMessage("答案", nil)}}
	a := newTestAgent(t, m, tl)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Collect(ctx, a.Query(ctx, "x")); err != nil {
		t.Fatalf("collect: %v", err)
	}

	if len(m.gotTools) != 1 || m.gotTools[0] != "rag_query" {
		t.Errorf("expected the tool to be advertised to the model, got %v", m.gotTools)
	}
}

func TestAgent_ToolErrorDoesNotCrash(t *testing.T) {
	// 验收要求：故意让 Tool 报错，Agent 能优雅回复而非崩溃。
	//
	// ⚠️ 这里用的是「工具把失败作为观察结果返回」的写法，
	// 因为 eino 对工具返回的 error 会中止整个节点
	// （compose/tool_node.go:1203）。见 TestAgent_ToolReturningErrorAbortsRun
	// —— 那条测试记录了返回 error 时的真实行为，两者对照说明
	// 为什么 tools 包里所有工具都不返回 error。
	tl := &echoTool{name: "rag_query", result: "检索服务暂时不可用（milvus 连接失败）。请如实告知用户。"}
	m := &scriptedModel{turns: []*schema.Message{
		toolCallMsg("call_1", "rag_query", `{"query":"x"}`),
		schema.AssistantMessage("抱歉，检索服务暂时不可用", nil),
	}}
	a := newTestAgent(t, m, tl)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := Collect(ctx, a.Query(ctx, "找文档"))
	if err != nil {
		t.Fatalf("a degraded tool must not fail the whole run: %v", err)
	}
	if !strings.Contains(got, "抱歉") {
		t.Errorf("expected a graceful reply, got %q", got)
	}
}

func TestAgent_ToolReturningErrorAbortsRun(t *testing.T) {
	// 这条测试固化 eino 的真实行为，而不是我们希望的行为：
	// 工具返回 error → 整个运行中止。
	//
	// 记录它的价值在于：如果哪天 eino 改成「错误也作为观察结果」，
	// 这条测试会失败，提醒我们 tools 包里那些「不返回 error」的
	// 变通写法可以简化掉。没有这条测试，那些变通会永远留着，
	// 而且没人知道当初为什么这么写。
	tl := &echoTool{name: "rag_query", err: fmt.Errorf("milvus 连接失败")}
	m := &scriptedModel{turns: []*schema.Message{
		toolCallMsg("call_1", "rag_query", `{"query":"x"}`),
		schema.AssistantMessage("不会走到这里", nil),
	}}
	a := newTestAgent(t, m, tl)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := Collect(ctx, a.Query(ctx, "找文档"))
	if err == nil {
		t.Fatal("eino 的当前行为是中止运行；若此处不再报错，" +
			"说明 eino 已改变行为，tools 包里的变通写法可以简化")
	}
	if !strings.Contains(err.Error(), "milvus 连接失败") {
		t.Errorf("the underlying tool error should surface, got: %v", err)
	}
}

func TestAgent_SystemPromptIsApplied(t *testing.T) {
	// 系统提示词决定 Agent 的行为准则（先检索再回答、注明来源等）
	m := &scriptedModel{turns: []*schema.Message{schema.AssistantMessage("ok", nil)}}
	a := newTestAgent(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Collect(ctx, a.Query(ctx, "x")); err != nil {
		t.Fatalf("collect: %v", err)
	}

	if len(m.gotMessages) == 0 {
		t.Fatal("the model received no messages")
	}
	first := m.gotMessages[0]
	if first.Role != schema.System {
		t.Fatalf("expected a system message first, got role %q", first.Role)
	}
	if !strings.Contains(first.Content, "rag_query") {
		t.Errorf("the system prompt should mention the retrieval rule: %q", first.Content)
	}
}

func TestAgent_CustomInstructionOverridesDefault(t *testing.T) {
	m := &scriptedModel{turns: []*schema.Message{schema.AssistantMessage("ok", nil)}}
	a, err := New(context.Background(), Config{Model: m, Instruction: "你是一只猫"})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Collect(ctx, a.Query(ctx, "x")); err != nil {
		t.Fatalf("collect: %v", err)
	}

	if !strings.Contains(m.gotMessages[0].Content, "你是一只猫") {
		t.Errorf("the custom instruction must be used, got %q", m.gotMessages[0].Content)
	}
}

func TestAgent_StopsAtMaxIterations(t *testing.T) {
	// ⚠️ 死循环防护。模型反复要求调同一个工具时必须能停下来，
	// 否则会一直烧 token 直到超时。
	tl := &echoTool{name: "rag_query", result: "没找到"}
	// 脚本永远返回工具调用 —— 模拟模型陷入循环
	turns := make([]*schema.Message, 30)
	for i := range turns {
		turns[i] = toolCallMsg(fmt.Sprintf("call_%d", i), "rag_query", `{"query":"x"}`)
	}
	m := &scriptedModel{turns: turns}

	a, err := New(context.Background(), Config{
		Model: m, Tools: []tool.BaseTool{tl}, MaxIterations: 3,
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 不关心返回内容，只要求它**能停下来**
	_, _ = Collect(ctx, a.Query(ctx, "x"))

	if ctx.Err() != nil {
		t.Fatal("the agent did not stop before the context deadline")
	}
	if n := atomic.LoadInt32(&tl.calls); n > 5 {
		t.Errorf("MaxIterations=3 should bound tool calls, got %d", n)
	}
}

func TestNew_RejectsNilModel(t *testing.T) {
	if _, err := New(context.Background(), Config{}); err == nil {
		t.Fatal("expected an error for a nil model")
	}
}

func TestAgent_StreamsIncrementally(t *testing.T) {
	// 流式的意义在于逐片到达。一次性返回的话首字延迟等于整段生成时间。
	m := &scriptedModel{turns: []*schema.Message{
		schema.AssistantMessage("缓存穿透", nil),
	}}
	a := newTestAgent(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var chunks int
	var sb strings.Builder
	err := Consume(ctx, a.Query(ctx, "x"), func(c Chunk) error {
		if c.Err == nil && !c.IsToolResult() && c.Text != "" {
			chunks++
			sb.WriteString(c.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("consume: %v", err)
	}

	if sb.String() != "缓存穿透" {
		t.Errorf("expected the concatenated text, got %q", sb.String())
	}
	if chunks < 2 {
		t.Errorf("expected incremental delivery, got %d chunk(s)", chunks)
	}
}

func TestConsume_StopsWhenCallbackErrors(t *testing.T) {
	// 客户端断开时 handler 返回 error，消费应立刻停止
	m := &scriptedModel{turns: []*schema.Message{
		schema.AssistantMessage(strings.Repeat("字", 100), nil),
	}}
	a := newTestAgent(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stopErr := fmt.Errorf("client gone")
	var seen int
	err := Consume(ctx, a.Query(ctx, "x"), func(c Chunk) error {
		seen++
		return stopErr
	})

	if err != stopErr {
		t.Errorf("expected the callback error to propagate, got %v", err)
	}
	if seen != 1 {
		t.Errorf("expected consumption to stop after the first chunk, got %d", seen)
	}
}

func TestConsume_RejectsNilIterator(t *testing.T) {
	if err := Consume(context.Background(), nil, func(Chunk) error { return nil }); err == nil {
		t.Fatal("expected an error for a nil iterator")
	}
}

func TestConsume_RespectsContextCancellation(t *testing.T) {
	m := &scriptedModel{turns: []*schema.Message{
		schema.AssistantMessage(strings.Repeat("字", 500), nil),
	}}
	a := newTestAgent(t, m)

	ctx, cancel := context.WithCancel(context.Background())
	iter := a.Query(ctx, "x")
	cancel()

	if err := Consume(ctx, iter, func(Chunk) error { return nil }); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}

// 保证 JSON 工具参数在测试里是合法的，避免脚本写错却测不出来
func TestToolCallArgumentsAreValidJSON(t *testing.T) {
	msg := toolCallMsg("id", "name", `{"query":"x"}`)
	var v map[string]any
	if err := json.Unmarshal([]byte(msg.ToolCalls[0].Function.Arguments), &v); err != nil {
		t.Fatalf("test fixture produced invalid JSON: %v", err)
	}
}
