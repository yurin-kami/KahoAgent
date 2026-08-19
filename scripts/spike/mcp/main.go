//go:build spike

// Spike S4：验证 MCP 协议连通性。
//
// 目的：
//  1. 证明 MCP Go SDK 可用，能被 Claude Desktop 识别
//  2. 亲手体会 stdio 模式下 stdout 是协议通道这件事
//     —— 所有日志走 stderr，Phase 4 写正式 MCP Server 时不会再犯
//
// 运行方式一（手动 smoke test，看到 "starting" 即协议层就绪）：
//
//	go run -tags=spike scripts/spike/mcp/main.go
//
// 运行方式二（Claude Desktop 集成，claude_desktop_config.json）：
//
//	{
//	  "mcpServers": {
//	    "kaho-spike": {
//	      "command": "go",
//	      "args": ["run", "-tags=spike", "scripts/spike/mcp/main.go"],
//	      "cwd": "A:\\dev_pjs\\KahoAgent"
//	    }
//	  }
//	}
//
// 成功标志：Claude Desktop 中说「用 hello 工具跟 kami 打招呼」返回 "Hi, kami!"
package main

import (
	"context"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	// ⚠️ 关键：stdio 模式下 stdout 是 JSON-RPC 协议通道。
	//    往 stdout 打任何非协议内容都会让客户端解析失败。
	//    所有日志必须走 stderr。
	logger := log.New(os.Stderr, "[spike-mcp] ", log.LstdFlags)

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "kaho-spike",
		Version: "0.0.1",
	}, nil)

	// 参数结构体：SDK 用反射自动生成 JSON Schema。
	// jsonschema tag 的描述会传给模型，直接影响它能否正确调用。
	type args struct {
		Name string `json:"name" jsonschema:"要打招呼的人名"`
	}

	// 用泛型版 mcp.AddTool（而非 server.AddTool）：
	// 自动生成 input schema、自动校验入参、自动包装错误。
	mcp.AddTool(server, &mcp.Tool{
		Name:        "hello",
		Description: "向指定的人打招呼。当用户想要问候某人时使用。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in args) (*mcp.CallToolResult, any, error) {
		logger.Printf("tool called: name=%q", in.Name)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "Hi, " + in.Name + "!"},
			},
		}, nil, nil
	})

	logger.Println("MCP server starting on stdio (waiting for client)...")
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		logger.Fatalf("server failed: %v", err)
	}
}
