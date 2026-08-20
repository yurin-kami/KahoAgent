package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	serverName    = "kahoagent"
	serverVersion = "0.1.0"
)

// New 用 Phase 3 的 Tool 装配出一个 MCP Server。
//
// ⚠️ 这里刻意做成**对任意 eino 工具通用**，而不是给 4 个工具各写一段
// 适配代码。理由不是"少写代码"，而是：逐个适配意味着每个工具的
// 参数 schema 要在 MCP 侧再声明一遍，两边迟早不一致 ——
// 而不一致的表现是模型传了合法参数却被拒，极难排查。
// 从 ToolInfo 现场生成 schema 则不可能不一致。
func New(tools []tool.BaseTool, guard *Guard, logger *slog.Logger) (*mcp.Server, error) {
	if len(tools) == 0 {
		return nil, fmt.Errorf("at least one tool is required")
	}
	if guard == nil {
		return nil, fmt.Errorf("guard must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    serverName,
		Version: serverVersion,
	}, nil)

	for _, t := range tools {
		if err := register(srv, t, guard); err != nil {
			return nil, err
		}
	}
	return srv, nil
}

// register 把单个 eino 工具挂到 MCP Server 上。
func register(srv *mcp.Server, t tool.BaseTool, guard *Guard) error {
	ctx := context.Background()

	info, err := t.Info(ctx)
	if err != nil {
		return fmt.Errorf("read tool info: %w", err)
	}

	// eino 的工具必须是可调用的才有意义暴露出去
	invokable, ok := t.(tool.InvokableTool)
	if !ok {
		return fmt.Errorf("tool %q is not invokable", info.Name)
	}

	schema, err := inputSchema(info)
	if err != nil {
		return fmt.Errorf("tool %q: %w", info.Name, err)
	}

	// ⚠️ 用非泛型的 srv.AddTool 而不是 mcp.AddTool[In, Out]。
	// 泛型版要求编译期已知的参数结构体，那正好是我们要避免的
	// "每个工具再声明一遍 schema"。非泛型版接受运行期构造的 schema，
	// 代价是入参校验要自己做 —— 但 eino 的工具本来就自己解析 JSON，
	// 重复校验反而会让两边的错误信息不一致。
	guarded := guard.Wrap(info.Name, func(ctx context.Context, argsJSON string) (string, error) {
		return invokable.InvokableRun(ctx, argsJSON)
	})

	srv.AddTool(&mcp.Tool{
		Name:        info.Name,
		Description: info.Desc,
		InputSchema: schema,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := encodeArgs(req)
		if err != nil {
			return errorResult(fmt.Sprintf("参数格式错误：%v", err)), nil
		}

		out, err := guarded(ctx, args)
		if err != nil {
			// ⚠️ 返回 IsError 结果而不是 Go error。
			//
			// 返回 error 会变成 JSON-RPC 协议级错误，客户端通常直接
			// 中断整轮对话；而 IsError 只是"这个工具这次失败了"，
			// 模型能看到原因并自行决定改参数重试还是如实告诉用户。
			// 这与 Phase 3 里工具"从不返回 error"是同一个道理。
			return errorResult(err.Error()), nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: out}},
		}, nil
	})
	return nil
}

// inputSchema 把 eino 的参数描述转成 MCP 要的 JSON Schema。
func inputSchema(info *schema.ToolInfo) (json.RawMessage, error) {
	// 无参工具：MCP 要求 InputSchema 非空，给一个空 object
	if info.ParamsOneOf == nil {
		return json.RawMessage(`{"type":"object","properties":{}}`), nil
	}

	js, err := info.ParamsOneOf.ToJSONSchema()
	if err != nil {
		return nil, fmt.Errorf("convert params: %w", err)
	}

	raw, err := json.Marshal(js)
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	return raw, nil
}

// encodeArgs 把 MCP 的入参转成 eino 工具期望的 JSON 字符串。
func encodeArgs(req *mcp.CallToolRequest) (string, error) {
	if req == nil || req.Params == nil || len(req.Params.Arguments) == 0 {
		// 无参调用：给 "{}" 而不是空串。
		// 空串会让工具的 json.Unmarshal 报 "unexpected end of JSON input"，
		// 而那对模型来说是个毫无意义的错误信息。
		return "{}", nil
	}
	// SDK 已经把 arguments 保留为原始 JSON，直接透传即可 ——
	// 反序列化再序列化一遍只会引入精度丢失和字段顺序变化
	return string(req.Params.Arguments), nil
}

func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}
