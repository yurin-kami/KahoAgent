package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeTool 是最小的 eino 工具实现。
type fakeTool struct {
	name   string
	desc   string
	params map[string]*schema.ParameterInfo
	result string
	err    error

	calls   atomic.Int32
	gotArgs atomic.Value // string
	delay   time.Duration
}

func (f *fakeTool) Info(context.Context) (*schema.ToolInfo, error) {
	info := &schema.ToolInfo{Name: f.name, Desc: f.desc}
	if f.params != nil {
		info.ParamsOneOf = schema.NewParamsOneOfByParams(f.params)
	}
	return info, nil
}

func (f *fakeTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	f.calls.Add(1)
	f.gotArgs.Store(argsJSON)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return f.result, f.err
}

func ragLikeTool() *fakeTool {
	return &fakeTool{
		name: "rag_query",
		desc: "语义搜索云盘中的文档内容",
		params: map[string]*schema.ParameterInfo{
			"query": {Type: schema.String, Desc: "搜索关键词，使用自然语言", Required: true},
			"top_k": {Type: schema.Integer, Desc: "返回结果数，默认 5，最大 20"},
		},
		result: "找到 3 篇关于 Redis 的文档",
	}
}

// connect 用真实的 MCP 客户端接上服务端。
//
// 为什么不直接调 handler：那样测的是我对协议的**假设**。
// 走真实客户端才能验证 schema 生成、参数编码、错误语义确实符合
// 协议 —— Claude Desktop 用的就是同一套客户端实现。
func connect(t *testing.T, tools ...tool.BaseTool) *mcp.ClientSession {
	t.Helper()

	srv, err := New(tools, NewGuard(WithGuardLogger(quietLogger())), quietLogger())
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()

	serverSession, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestServer_ListsTools(t *testing.T) {
	// 验收项：客户端能 list 出注册的工具
	names := []string{"rag_query", "file_search", "file_metadata", "file_share"}
	tools := make([]tool.BaseTool, 0, len(names))
	for _, n := range names {
		tools = append(tools, &fakeTool{name: n, desc: n + " 的说明", result: "ok"})
	}

	cs := connect(t, tools...)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
	}
	for _, want := range names {
		if !got[want] {
			t.Errorf("tool %q was not listed", want)
		}
	}
	if len(res.Tools) != len(names) {
		t.Errorf("expected %d tools, got %d", len(names), len(res.Tools))
	}
}

func TestServer_ExposesSchemaDerivedFromTheTool(t *testing.T) {
	// ⚠️ schema 必须由 eino 的 ToolInfo **现场生成**，不能在 MCP 侧
	// 手写一遍。手写的话两边迟早不一致，表现为模型传了合法参数却被
	// 拒绝 —— 而这种错误极难排查（协议层看着完全正常）。
	cs := connect(t, ragLikeTool())

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(res.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(res.Tools))
	}

	raw, err := json.Marshal(res.Tools[0].InputSchema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	got := string(raw)

	for _, want := range []string{"query", "top_k", "搜索关键词"} {
		if !strings.Contains(got, want) {
			t.Errorf("the schema should carry %q from the eino ToolInfo: %s", want, got)
		}
	}
	// required 必须传递过去，否则模型可能漏传必填参数
	if !strings.Contains(got, "required") {
		t.Errorf("the schema lost the required-field information: %s", got)
	}
}

func TestServer_CallsToolAndReturnsText(t *testing.T) {
	f := ragLikeTool()
	cs := connect(t, f)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "rag_query",
		Arguments: map[string]any{"query": "Redis 缓存", "top_k": 3},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res.Content)
	}

	if len(res.Content) == 0 {
		t.Fatal("expected content")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected text content, got %T", res.Content[0])
	}
	if text.Text != "找到 3 篇关于 Redis 的文档" {
		t.Errorf("unexpected result text: %q", text.Text)
	}

	// 参数必须原样透传给 eino 工具
	args, _ := f.gotArgs.Load().(string)
	if !strings.Contains(args, "Redis 缓存") || !strings.Contains(args, "top_k") {
		t.Errorf("arguments were not forwarded intact: %s", args)
	}
}

func TestServer_ToolFailureIsAToolErrorNotAProtocolError(t *testing.T) {
	// ⚠️ 工具失败要返回 IsError 结果，而不是 JSON-RPC 协议错误。
	//
	// 协议错误会让客户端直接中断整轮对话；IsError 只是"这个工具这次
	// 失败了"，模型能看到原因，自行决定改参数重试还是如实告诉用户。
	// 与 Phase 3 里工具"从不返回 error"是同一个道理。
	f := &fakeTool{name: "broken", desc: "总是失败", err: fmt.Errorf("milvus 连接失败")}
	cs := connect(t, f)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "broken",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("a tool failure must not surface as a protocol error: %v", err)
	}
	if !res.IsError {
		t.Error("expected IsError to be set")
	}
	if len(res.Content) == 0 {
		t.Fatal("the failure reason should be reported to the model")
	}
	if text, ok := res.Content[0].(*mcp.TextContent); ok {
		if !strings.Contains(text.Text, "milvus") {
			t.Errorf("expected the reason in the result, got %q", text.Text)
		}
	}
}

func TestServer_NoArgumentsBecomesEmptyObject(t *testing.T) {
	// 空串会让工具的 json.Unmarshal 报 "unexpected end of JSON input"，
	// 那对模型来说是个毫无意义的错误信息
	f := &fakeTool{name: "noargs", desc: "不需要参数", result: "ok"}
	cs := connect(t, f)

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "noargs"}); err != nil {
		t.Fatalf("call tool: %v", err)
	}

	args, _ := f.gotArgs.Load().(string)
	if args != "{}" {
		t.Errorf("expected an empty JSON object, got %q", args)
	}
}

func TestServer_GuardAppliesToMCPCalls(t *testing.T) {
	// Guard 必须真的挂在 MCP 路径上 —— 只在单测里验证 Guard 本身
	// 不能说明它被接进去了
	f := &fakeTool{name: "slow", desc: "很慢", delay: 5 * time.Second, result: "ok"}

	srv, err := New([]tool.BaseTool{f},
		NewGuard(WithTimeout(50*time.Millisecond), WithGuardLogger(quietLogger())),
		quietLogger())
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	start := time.Now()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "slow", Arguments: map[string]any{}})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected a tool-level error, got a protocol error: %v", err)
	}
	if !res.IsError {
		t.Error("expected the timeout to surface as a tool error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("the guard timeout was not applied on the MCP path: took %v", elapsed)
	}
}

func TestServer_ConcurrentCallsAreSerialisedByTheGuard(t *testing.T) {
	// 验收项：并发 50 个 tool call，不 panic、不 OOM、排队而非报错
	const (
		limit = 5
		calls = 50
	)
	var inFlight, peak int64
	var mu sync.Mutex

	f := &fakeTool{name: "counted", desc: "计数", result: "ok"}
	counting := &countingTool{fakeTool: f, onCall: func() {
		n := atomic.AddInt64(&inFlight, 1)
		mu.Lock()
		if n > peak {
			peak = n
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
	}}

	srv, err := New([]tool.BaseTool{counting},
		NewGuard(WithMaxConcurrent(limit), WithGuardLogger(quietLogger())), quietLogger())
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	var wg sync.WaitGroup
	failures := make([]error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{
				Name:      "counted",
				Arguments: map[string]any{},
			})
			switch {
			case err != nil:
				failures[i] = err
			case res.IsError:
				failures[i] = fmt.Errorf("tool reported an error: %+v", res.Content)
			}
		}(i)
	}
	wg.Wait()

	for i, err := range failures {
		if err != nil {
			t.Fatalf("call %d failed; queueing must not become rejection: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if peak > limit {
		t.Errorf("peak concurrency %d exceeded the limit of %d", peak, limit)
	}
	t.Logf("%d concurrent MCP calls completed, peak concurrency %d (limit %d)",
		calls, peak, limit)
}

// countingTool 在真实调用前后记录并发数。
type countingTool struct {
	*fakeTool
	onCall func()
}

func (c *countingTool) InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	c.onCall()
	return c.fakeTool.InvokableRun(ctx, argsJSON, opts...)
}

func TestNew_Validates(t *testing.T) {
	g := NewGuard(WithGuardLogger(quietLogger()))

	if _, err := New(nil, g, quietLogger()); err == nil {
		t.Error("expected an error when no tools are provided")
	}
	if _, err := New([]tool.BaseTool{&fakeTool{name: "x"}}, nil, quietLogger()); err == nil {
		t.Error("expected an error for a nil guard")
	}
}
