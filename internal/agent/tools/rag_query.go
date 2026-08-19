// Package tools 实现 Agent 可调用的工具。
//
// 每个工具都实现 eino 的 tool.InvokableTool。设计上有两条硬规则：
//
//  1. **Description 决定模型会不会调用它**。模型看不到实现，
//     只能看到 Name 和 Desc。Desc 要回答「什么情况下该用我」，
//     而不只是「我是干什么的」。Agent 不调某个工具时，
//     绝大多数情况是描述写得不够明确，而不是模型笨。
//
//  2. **返回给模型的是结构化文本，不是原始 JSON**。
//     模型读文本比读 JSON 准，且 JSON 的括号引号很占 token。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	einoretriever "github.com/cloudwego/eino/components/retriever"
)

// Retriever 是 rag_query 依赖的检索能力。
//
// 抽成接口而非直接依赖 *retriever.HybridRetriever，是为了让工具
// 能用假检索器测试 —— 否则每跑一次测试都要连 Milvus 和 PG。
// 签名与 eino 的 Retriever 一致，HybridRetriever 天然满足。
type Retriever interface {
	Retrieve(ctx context.Context, query string, opts ...einoretriever.Option) ([]*schema.Document, error)
}

const (
	// defaultTopK 与 Phase 2 的检索默认值保持一致
	defaultTopK = 5
	// maxTopK 限制模型能要求的最大条数。
	// 模型偶尔会传 100 甚至 1000 —— 不设上限会撑爆上下文。
	maxTopK = 20
	// snippetLimit 单个片段的字数上限。
	// 5 个片段 × 500 字 = 2500 字，约 1700 token，
	// 给 8000 的预算留足空间放对话历史。
	snippetLimit = 500
)

// RAGQueryTool 让 Agent 能语义搜索文档。
//
// 这是本项目最核心的 Tool —— 它把 Phase 2 的检索能力暴露给 LLM，
// 是 RAG 中「R」与「G」的连接点。
//
// ⚠️ 本工具**从不返回 error**，所有失败都作为文本观察结果返回。
// 这不是偷懒：eino 在 compose/tool_node.go:1203 对任何非中断类的
// 工具错误都会中止整个节点，于是一次 Milvus 抖动会让整个请求崩掉。
// 把失败变成模型能读到的文本，模型才有机会纠错或如实告知用户。
type RAGQueryTool struct {
	retriever Retriever
	logger    *slog.Logger
}

var _ tool.InvokableTool = (*RAGQueryTool)(nil)

type RAGOption func(*RAGQueryTool)

func WithLogger(l *slog.Logger) RAGOption {
	return func(t *RAGQueryTool) {
		if l != nil {
			t.logger = l
		}
	}
}

func NewRAGQueryTool(r Retriever, opts ...RAGOption) (*RAGQueryTool, error) {
	if r == nil {
		return nil, fmt.Errorf("retriever must not be nil")
	}
	t := &RAGQueryTool{retriever: r, logger: slog.Default()}
	for _, opt := range opts {
		opt(t)
	}
	return t, nil
}

func (t *RAGQueryTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "rag_query",
		// ⚠️ 这段描述决定模型会不会调用它。
		// 写清「什么时候用」，并明确产出可用于引用 ——
		// 后者会引导模型在回答里带上来源，这是答案溯源的基础。
		Desc: "在用户的云盘文档中进行语义搜索。当用户询问技术问题、" +
			"查找资料、或需要基于已有文档内容回答时使用此工具。" +
			"返回最相关的文档片段及来源文件名，可用于引用。",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"query": {
				Type:     schema.String,
				Desc:     "搜索查询，使用自然语言描述要查找的内容",
				Required: true,
			},
			"top_k": {
				Type: schema.Integer,
				Desc: fmt.Sprintf("返回结果数量，默认 %d，最大 %d", defaultTopK, maxTopK),
			},
		}),
	}, nil
}

type ragQueryArgs struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
}

func (t *RAGQueryTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	var args ragQueryArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		// 同样不返回 error（理由见下方检索失败处）：
		// 把「参数错了」作为观察结果交给模型，它下一轮就能修正重试。
		// 返回 error 则整个请求当场结束，模型没有纠错机会。
		t.logger.Warn("could not parse tool arguments", "raw", truncate(argsJSON, 200), "err", err)
		return fmt.Sprintf("参数解析失败：%s。请检查 JSON 格式后重试，query 为必填的字符串。",
			truncate(err.Error(), 150)), nil
	}

	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		// 明确说明缺什么，模型下一轮可以补上。
		// 若返回「未找到相关文档」，模型会以为搜过了，从而不再重试。
		return "参数 query 不能为空。请提供要搜索的内容后重试。", nil
	}

	// 参数兜底与范围限制：模型传离谱的值时收敛到合法区间，
	// 而不是报错 —— 能干活就别打断 ReAct 循环。
	topK := args.TopK
	if topK <= 0 {
		topK = defaultTopK
	}
	if topK > maxTopK {
		topK = maxTopK
	}

	docs, err := t.retriever.Retrieve(ctx, args.Query, einoretriever.WithTopK(topK))
	if err != nil {
		// ⚠️ 检索失败**不返回 error**，而是把失败作为「观察结果」交给模型。
		//
		// 原因在 eino 的实现里：compose/tool_node.go:1203 对任何非中断类
		// 的工具错误都会 `return nil, fmt.Errorf(...)`，直接中止整个节点 ——
		// 于是一次 Milvus 抖动会让整个请求崩掉，用户看到的是连接断开
		// 而不是一句人话。这与验收要求「Tool 报错时 Agent 能优雅回复」相悖。
		//
		// 返回文本则让模型看到「检索失败了」，由它决定怎么告诉用户
		// （系统提示词第 5 条要求它如实说明而非假装成功）。
		//
		// 日志仍要记 —— 降级不等于掩盖，运维需要知道检索挂了。
		t.logger.Warn("retrieval failed, reporting to the model as an observation",
			"query", args.Query, "err", err)
		return fmt.Sprintf("检索服务暂时不可用（%s）。请如实告知用户当前无法搜索文档，不要编造内容。",
			truncate(err.Error(), 200)), nil
	}

	return formatDocs(docs), nil
}

// formatDocs 把检索结果转成模型易读的文本。
func formatDocs(docs []*schema.Document) string {
	if len(docs) == 0 {
		// ⚠️ 必须明确说「没找到」，不能返回空串。
		// 空串会让模型以为工具坏了，进而重复调用直到撞上 MaxIterations。
		return "未找到相关文档。"
	}

	var sb strings.Builder
	for i, d := range docs {
		if d == nil {
			continue
		}
		fmt.Fprintf(&sb, "【片段 %d】来源: %s\n%s\n\n",
			i+1, sourceOf(d), truncate(d.Content, snippetLimit))
	}
	if sb.Len() == 0 {
		return "未找到相关文档。"
	}
	return strings.TrimRight(sb.String(), "\n")
}

// sourceOf 组装人类可读的来源标识。
//
// 优先用 heading_path（如「Redis 缓存实战 > 缓存击穿」），
// 它比文件名更能说明这段话在讲什么；缺失时退回文件名。
func sourceOf(d *schema.Document) string {
	heading, _ := d.MetaData["heading_path"].(string)
	source, _ := d.MetaData["source"].(string)

	switch {
	case heading != "" && source != "":
		return fmt.Sprintf("%s（%s）", heading, source)
	case heading != "":
		return heading
	case source != "":
		return source
	default:
		// 兜底用 doc_id，总比「未知」有用 —— 至少能定位到文档
		if id, ok := d.MetaData["doc_id"].(string); ok && id != "" {
			return id
		}
		return "未知来源"
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
