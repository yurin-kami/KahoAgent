// smoke_test.go 验证 MCP spike 的协议握手。
//
// 目的：不依赖 Claude Desktop 也能确认协议层工作正常。
// 手工构造 JSON-RPC 请求走 stdio，断言：
//  1. initialize 握手成功
//  2. tools/list 能列出 hello
//  3. tools/call 能正确执行
//
// 运行：go test -tags=spike ./scripts/spike/mcp/ -v
package main

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPHandshakeAndToolCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 用 SDK 自带的 client 连接本 spike server 的进程
	client := mcp.NewClient(&mcp.Implementation{Name: "smoke-test", Version: "0.0.1"}, nil)

	cmd := exec.Command("go", "run", "-tags=spike", ".")
	transport := &mcp.CommandTransport{Command: cmd}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	defer session.Close()

	// ── 1. tools/list ────────────────────────────────────
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools.Tools))
	}
	if got := tools.Tools[0].Name; got != "hello" {
		t.Fatalf("expected tool 'hello', got %q", got)
	}
	t.Logf("✅ tools/list: %s — %s", tools.Tools[0].Name, tools.Tools[0].Description)

	// ── 2. tools/call ────────────────────────────────────
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "hello",
		Arguments: map[string]any{"name": "kami"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %+v", res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatal("empty content")
	}

	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	const want = "Hi, kami!"
	if text.Text != want {
		t.Fatalf("expected %q, got %q", want, text.Text)
	}
	t.Logf("✅ tools/call: %s", text.Text)
}
