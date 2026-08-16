# KahoAgent

> 基于 CloudWeGo Eino 的智能文件助手 — RAG 语义检索 + ReAct Agent + MCP 协议暴露

用户可通过自然语言搜索/管理 [CloudKaho](https://github.com/yurin-kami/CloudKaho) 云盘中的文件，同时以 MCP 协议标准化暴露 Tools，支持 Claude Desktop / Cursor 等 AI 客户端直接调用。

## 架构

```
┌─────────────────────────────────────────────────────────┐
│                     KahoAgent                            │
├─────────────┬─────────────┬─────────────┬───────────────┤
│  HTTP API   │  MCP Server │  Kafka      │  Agent Loop   │
│  (Gin/SSE)  │  (stdio/SSE)│  Consumer   │  (Eino ADK)   │
├─────────────┴──────┬──────┴─────────────┴───────────────┤
│                    │                                     │
│  ┌──── RAG Pipeline ────┐    ┌──── Tool Registry ────┐  │
│  │ Chunker → Embedding  │    │ file_search           │  │
│  │ → Milvus + PG BM25   │    │ file_upload           │  │
│  │ → RRF → ReRank       │    │ file_share            │  │
│  └───────────────────────┘    │ metadata_query        │  │
│                               │ rag_query             │  │
│                               └───────────────────────┘  │
├──────────────────────────────────────────────────────────┤
│  Milvus │ PostgreSQL │ Redis │ Kafka │ LLM API          │
└──────────────────────────────────────────────────────────┘
```

## 技术栈

- **语言**: Go 1.22+
- **AI 框架**: [CloudWeGo Eino](https://github.com/cloudwego/eino) (Agent 编排 + RAG)
- **MCP**: [modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk) (官方 Go SDK)
- **向量数据库**: Milvus (HNSW 索引)
- **全文检索**: PostgreSQL ts_vector (BM25)
- **消息队列**: Kafka (文档异步处理管道)
- **缓存**: Redis (Query 向量缓存 + 结果缓存)
- **Web**: Gin + SSE 流式响应
- **可观测性**: Prometheus + Zap 结构化日志

## 目录结构

```
KahoAgent/
├── cmd/
│   ├── server/        # HTTP API + Agent 服务入口
│   └── mcp/           # MCP Server 独立入口
├── internal/
│   ├── agent/         # Eino Agent 编排 (ReAct 循环)
│   ├── rag/
│   │   ├── chunker/   # 文档分块 (语义感知)
│   │   ├── embedding/ # 向量化
│   │   └── retriever/ # 混合检索 + ReRank
│   ├── mcp/
│   │   ├── server/    # MCP 协议服务
│   │   └── tools/     # MCP Tool 定义
│   ├── pipeline/      # Kafka 文档处理管道
│   ├── config/        # 配置加载
│   ├── handler/       # HTTP handlers
│   └── middleware/    # Gin 中间件
├── pkg/
│   ├── llm/           # LLM 客户端封装
│   ├── vectorstore/   # Milvus 操作封装
│   └── cache/         # Redis 缓存封装
├── docs/
├── scripts/
├── config.example.toml
└── go.mod
```

## Quick Start

```bash
# 1. 克隆
git clone https://github.com/yurin-kami/KahoAgent.git
cd KahoAgent

# 2. 配置
cp config.example.toml config.toml
# 编辑 config.toml 填入 API Key 和数据库地址

# 3. 启动依赖 (Docker Compose)
docker compose up -d  # Milvus + PostgreSQL + Redis + Kafka

# 4. 运行 HTTP 服务
go run cmd/server/main.go

# 5. 运行 MCP 服务 (Claude Desktop 集成)
go run cmd/mcp/main.go
```

## MCP 集成 (Claude Desktop)

在 `claude_desktop_config.json` 中添加：

```json
{
  "mcpServers": {
    "kahoagent": {
      "command": "go",
      "args": ["run", "cmd/mcp/main.go"],
      "cwd": "/path/to/KahoAgent"
    }
  }
}
```

## License

MIT
