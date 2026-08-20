# KahoAgent

> 基于 CloudWeGo Eino 的智能文件助手 — RAG 混合检索 + ReAct Agent + MCP 协议暴露

用自然语言检索 [CloudKaho](https://github.com/yurin-kami/CloudKaho) 云盘中的文档内容，
同时以 MCP 协议把能力暴露给 Claude Desktop / Cursor 等客户端直接调用。

**核心指标**（全部实测，方法见 [docs/spike-evidence/phase5.md](docs/spike-evidence/phase5.md)）：

| 指标 | 实测值 |
|---|---|
| Recall@5（hybrid + rerank） | 0.444 |
| MRR（hybrid + rerank） | 0.798 |
| 端到端首字延迟 P50 / P99 | 409ms / 1388ms |
| 检索缓存命中率 | 94.8% |
| 常驻内存 | 23.9 MB |
| MCP 工具调用吞吐 | 69.7 call/s |

## 架构

```
                    ┌──────────────┐        ┌──────────────┐
   HTTP + SSE ─────▶│              │        │  Claude      │
   /api/chat        │  internal/   │        │  Desktop     │
                    │  app         │        └──────┬───────┘
                    │  (统一装配)   │               │ stdio
                    └──────┬───────┘        ┌──────▼───────┐
                           │                │  cmd/mcp     │
                           │                │  MCP Server  │
                           ▼                └──────┬───────┘
                    ┌─────────────┐                │
                    │ Eino ADK    │◀───────────────┘
                    │ ReAct Agent │   两侧复用同一份 Tools
                    └──────┬──────┘
                           │
        ┌──────────────────┼──────────────────┐
        ▼                  ▼                  ▼
   ┌─────────┐      ┌────────────┐     ┌──────────────┐
   │rag_query│      │file_search │     │file_metadata │
   └────┬────┘      │file_share  │     └──────────────┘
        │           └─────┬──────┘
        │                 └──────▶ CloudKaho API
        ▼
   ┌────────────────── RAG 检索链路 ──────────────────┐
   │  Query                                          │
   │    ├─▶ Embedding ─▶ Milvus (HNSW/COSINE)  ─┐    │
   │    │                                       ├─▶ RRF ─▶ LLM 精排 ─▶ TopK
   │    └─▶ gojieba 分词 ─▶ PostgreSQL ts_rank ─┘    │
   │                                                 │
   │  Redis 缓存查询向量与结果（命中率 94.8%）          │
   └─────────────────────────────────────────────────┘

   文档入库：Kafka ─▶ Chunker ─▶ Embedding ─▶ Milvus + PostgreSQL
```

## 技术栈

- **语言**：Go 1.25
- **AI 框架**：[CloudWeGo Eino](https://github.com/cloudwego/eino) v0.9.14（ADK ReAct Agent）
- **MCP**：[modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk) v1.7.0（官方 Go SDK）
- **向量数据库**：Milvus（HNSW，M=16 / efConstruction=200 / COSINE）
- **全文检索**：PostgreSQL `ts_rank` + [gojieba](https://github.com/yanyiwu/gojieba) 中文分词
- **消息队列**：Kafka（文档异步处理管道）
- **缓存**：Redis（查询向量 + 检索结果）
- **LLM**：DashScope qwen-plus / text-embedding-v3（OpenAI 兼容协议）
- **HTTP**：标准库 `net/http` + SSE
- **可观测性**：Prometheus + 标准库 `log/slog` + pprof

> HTTP 与日志都用标准库，没有引入 Gin / Zap：
> 本项目只有一个 POST 端点和一个健康检查，`http.ServeMux` 足够；
> 少一层框架，SSE 的刷写时机和 context 取消行为就少一层不确定性。

## 目录结构

```
KahoAgent/
├── cmd/
│   ├── server/          # HTTP API 入口
│   └── mcp/             # MCP Server 入口（stdio / http）
├── internal/
│   ├── app/             # 依赖装配（HTTP 与 MCP 共用，可被测试 import）
│   ├── agent/           # Eino ReAct 编排、会话记忆与压缩
│   │   └── tools/       # 四个业务工具的实现
│   ├── rag/
│   │   ├── chunker/     # 按标题层级分块
│   │   ├── retriever/   # 混合召回 + RRF 融合 + LLM 精排
│   │   └── tokenizer/   # gojieba 分词（供 BM25 用）
│   ├── mcp/server/      # Tools → MCP 的通用桥接 + 限流/超时/审计
│   ├── pipeline/        # Kafka 文档处理管道
│   ├── handler/         # HTTP handler（SSE 流式响应）
│   ├── metrics/         # Prometheus 指标
│   ├── eval/            # 检索评估指标计算
│   └── config/          # 配置加载
├── pkg/
│   ├── llm/             # LLM 客户端（Chat / Embedding / 流式）
│   ├── vectorstore/     # Milvus 封装
│   ├── cache/           # Redis 封装
│   └── cloudkaho/       # CloudKaho API 客户端
├── migrations/          # PostgreSQL 建表与索引
├── scripts/
│   ├── eval/            # 检索评估（-tags=eval）
│   ├── bench/           # 开环压测（-tags=loadtest）
│   └── spike/           # 技术验证脚本
└── docs/
    ├── IMPLEMENTATION.md   # 分阶段实施方案
    ├── eval-results.md     # 检索策略评估结果
    ├── optimization.md     # pprof 定位与优化前后对比
    ├── deployment.md       # 容器化与部署
    └── spike-evidence/     # 各阶段验收证据
```

## Quick Start

```bash
git clone https://github.com/yurin-kami/KahoAgent.git
cd KahoAgent

# 1. 配置
cp config.example.toml config.toml
# 编辑 config.toml；API Key 建议走环境变量而非写进文件：
export DASHSCOPE_API_KEY=sk-xxx

# 2. 启动依赖
docker compose up -d          # Milvus + PostgreSQL + Redis + Kafka
psql -f migrations/001_chunks.sql

# 3. 运行
go run ./cmd/server           # HTTP 服务，默认 :8080
go run ./cmd/mcp              # MCP 服务（stdio）
```

试一下：

```bash
curl -N -X POST http://127.0.0.1:8080/api/chat \
  -H "Content-Type: application/json" \
  -d '{"session_id":"demo","message":"缓存穿透怎么解决？"}'
```

会以 SSE 逐字返回，中间可以看到 `{"type":"tool","tool":"rag_query"}` 这样的
工具调用事件。

> ⚠️ gojieba 基于 cgo，构建需要 C++ 工具链（`g++`）。
> Windows 下需装 MinGW-w64 或在 WSL 里构建。

## 端点

| 路径 | 说明 |
|---|---|
| `POST /api/chat` | 对话（SSE 流式） |
| `GET /healthz` | 健康检查 |
| `GET /metrics` | Prometheus 指标 |
| `GET /debug/pprof/` | 性能剖析 |

## 工具

| 工具 | 用途 |
|---|---|
| `rag_query` | 语义检索文档内容，返回带出处的片段 |
| `file_search` | 按文件名/类型搜索网盘文件 |
| `file_metadata` | 查询单个文件的大小、时间等元信息 |
| `file_share` | 生成文件分享链接 |

后三个需要 CloudKaho 凭证（`CLOUDKAHO_TOKEN`）；未配置时会自动禁用，
`rag_query` 仍可正常使用。

## MCP 集成（Claude Desktop）

在 `claude_desktop_config.json` 中添加：

```json
{
  "mcpServers": {
    "kahoagent": {
      "command": "/path/to/KahoAgent/kaho-mcp",
      "env": {
        "KAHO_CONFIG": "/path/to/KahoAgent/config.toml",
        "DASHSCOPE_API_KEY": "sk-xxx"
      }
    }
  }
}
```

> 用编译好的二进制而不是 `go run`：`go run` 每次启动都要编译，
> 而 cgo 编译需要数秒，Claude Desktop 会在 MCP 握手超时前就放弃连接。
> 先 `go build -o kaho-mcp ./cmd/mcp`。

## 容器化

```bash
docker build -t kahoagent .

docker run -p 8080:8080 \
  -v ./config.toml:/etc/kaho/config.toml:ro \
  -e KAHO_CONFIG=/etc/kaho/config.toml \
  -e DASHSCOPE_API_KEY=sk-xxx \
  kahoagent
```

镜像 110MB，config.toml 不打进镜像（含密钥），以非 root 运行。
体积构成与取舍见 [docs/deployment.md](docs/deployment.md)。

## 测试

```bash
go test ./...                      # 单元测试，不需要任何外部服务
go test -race ./...                # 竞态检测（CI 跑这个）
go test -tags=integration ./...    # 需要 Milvus / PostgreSQL / Redis
go test -tags=e2e ./...            # 需要上述服务 + 真实 LLM API Key
```

分层的原因：单元测试必须能在任何机器上秒级跑完，否则没人会在提交前跑；
需要真实依赖的测试单独打标签，CI 只跑不带标签的那部分。

评估与压测：

```bash
go run -tags=eval ./scripts/eval/eval.go        # 检索策略对比 → docs/eval-results.md
go run -tags=loadtest ./scripts/bench/load.go   # 开环压测
go test -bench=. -benchmem ./internal/rag/retriever/
```

## 已知限制

- **HTTP 吞吐约 10 req/s**，瓶颈是 DashScope 账号并发配额而非本服务
  （三组独立证据见 [phase5.md](docs/spike-evidence/phase5.md)）。MCP 侧不经过
  生成环节，实测 69.7 call/s。
- **`confusable` 类查询样本仅 8 条**，该维度的指标只适合看趋势。
- **镜像 110MB**，达不到最初设定的 30MB —— cgo 二进制与 13.8MB 中文词典
  是主要构成，取舍说明见 [docs/deployment.md](docs/deployment.md)。

## License

MIT
