// Package agent 组装 ReAct Agent。
//
// ReAct = Reasoning + Acting：模型在「思考下一步该干什么」和
// 「调用工具拿结果」之间循环，直到信息足够回答为止。
//
//	Thought: 我需要先搜索 Redis 相关文档
//	Action:  rag_query(query="Redis")
//	Observation: 找到 3 个文件……
//	Thought: 信息够了
//	Answer:  ……
//
// 本包只负责**组装**：模型、工具、系统提示词、迭代上限。
// 循环本身由 eino 的 adk 实现，业务逻辑在各个 Tool 里。
package agent

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// maxIterations 限制 ReAct 循环的轮数。
//
// ⚠️ 必须设置。模型可能陷入死循环 —— 最常见的是反复调用同一个工具：
// 工具返回「没找到」，模型换个说法再搜，还是没找到，如此往复。
// 没有上限的话这会一直烧 token 直到请求超时。
//
// 取 15（eino 默认 20）：正常对话 2-3 轮工具调用就够，
// 超过 15 轮基本可以断定是循环而非真的需要那么多信息。
const maxIterations = 15

// systemPrompt 定义 Agent 的行为准则。
//
// 几条规则的用意：
//   - 「先检索再回答」防止模型凭训练数据里的记忆瞎答，
//     那正是 RAG 要解决的问题
//   - 「注明来源」让答案可溯源，也是 rag_query 返回来源的意义所在
//   - 「不知道就说不知道」比编一个像样的答案有用得多
const systemPrompt = `你是 KahoAgent，一个云盘文件智能助手。

工作准则：

1. 涉及用户文档内容的问题，必须先用 rag_query 检索，再基于检索结果回答。
   不要凭自己的知识直接回答 —— 用户问的是**他自己的**文档。

2. 回答时注明信息来源（片段给出的来源路径），方便用户核对原文。

3. 检索不到相关内容时，直接说明没有找到，并建议用户换个说法或确认文档已上传。
   **不要编造内容**，也不要用你的通用知识冒充文档内容。

4. 需要文件属性（大小、修改时间）或分享链接时，使用对应的文件工具。

5. 工具报错时，向用户说明遇到了什么问题，不要假装操作成功。

回答用中文，简洁清晰，避免无谓的客套。`

// Config 组装 Agent 所需的依赖。
type Config struct {
	// Model 必须支持 model.WithTools 调用选项 ——
	// adk 就是通过它把工具信息传给模型的。
	Model model.BaseChatModel

	// Tools 是 Agent 可用的工具集。为空时 Agent 退化成纯对话，
	// 不报错（便于只测对话链路）。
	Tools []tool.BaseTool

	// Instruction 覆盖默认系统提示词。留空用 systemPrompt。
	Instruction string

	// MaxIterations 覆盖默认迭代上限。<=0 时用 maxIterations。
	MaxIterations int
}

// Agent 封装 eino 的 ChatModelAgent 与 Runner。
//
// 为什么包一层而不直接暴露 adk 类型：
// adk 的 API 用了大量泛型别名（AgentEvent = TypedAgentEvent[*schema.Message]），
// 直接透出会让 handler 层被迫理解这些类型参数。这里收敛成
// 「输入消息 → 输出事件流」的最小接口。
type Agent struct {
	runner *adk.Runner
}

func New(ctx context.Context, cfg Config) (*Agent, error) {
	if cfg.Model == nil {
		return nil, fmt.Errorf("model must not be nil")
	}

	instruction := cfg.Instruction
	if instruction == "" {
		instruction = systemPrompt
	}
	iterations := cfg.MaxIterations
	if iterations <= 0 {
		iterations = maxIterations
	}

	chatAgent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        "KahoAgent",
		Description: "云盘智能文件助手",
		Instruction: instruction,
		Model:       cfg.Model,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: cfg.Tools,
			},
		},
		MaxIterations: iterations,
	})
	if err != nil {
		return nil, fmt.Errorf("create chat model agent: %w", err)
	}

	// ⚠️ NewRunner 收的是**值**不是指针（与 NewChatModelAgent 相反）。
	// EnableStreaming 开启后事件里带 MessageStream，可逐 token 消费。
	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           chatAgent,
		EnableStreaming: true,
	})

	return &Agent{runner: runner}, nil
}

// Run 执行一轮对话，返回事件迭代器。
//
// messages 应包含完整的对话历史（由 Memory 负责裁剪），
// 最后一条是本轮用户输入。
//
// ⚠️ 返回的迭代器**没有** error ——  eino 把执行错误放在事件的 Err 字段里。
// 调用方必须检查每个 event.Err，否则失败会被静默吞掉。
func (a *Agent) Run(ctx context.Context, messages []*schema.Message) *adk.AsyncIterator[*adk.AgentEvent] {
	return a.runner.Run(ctx, messages)
}

// Query 是单轮提问的便捷入口，主要供测试和 CLI 使用。
func (a *Agent) Query(ctx context.Context, query string) *adk.AsyncIterator[*adk.AgentEvent] {
	return a.runner.Query(ctx, query)
}
