# KahoAgent 详细实现规范

> 本文档是**执行契约**，不是参考资料。每个 Phase 的「不做什么」和「验收命令」是硬约束。
> 所有接口签名已对照 `eino@v0.9.14` / `go-sdk@v1.7.0` 源码验证，可直接照抄。

---

## 🚫 全局防漂移铁律

| # | 规则 | 违反的后果 |
|---|------|-----------|
| 1 | **一个 Phase 未通过验收，不准开下一个** | 底层没跑通，上层全是幻觉代码 |
| 2 | **禁止提前引入未列在当前 Phase 的依赖** | 依赖爆炸，编译一天修不好 |
| 3 | **每个 Phase 结束必须能 `go build ./... && go test ./...` 通过** | 留着编译错误进下一阶段 = 技术债雪球 |
| 4 | **接口先定义，实现后写** | 没有接口约束，实现会长成一坨 |
| 5 | **禁止在 Phase 1-2 碰 LLM 对话逻辑** | RAG 检索没调好就上 Agent，永远不知道是谁的锅 |
| 6 | **每个 Phase 一个 git 分支，合并前打 tag** | 出问题能回滚到上一个可用状态 |
| 7 | **指标必须实测，不准写"预计"** | 简历上的数字被追问就崩 |
| 8 | **不准为了"更完整"擅自加功能** | 范围蔓延是学生项目烂尾的第一死因 |

### 每日收工检查（30 秒）

```bash
go build ./... && go vet ./... && go test ./... && git status --short
```

四条都干净才准关电脑。

---

## Phase 0：环境验证 + 最小可运行样例

**目标**：证明每个外部依赖都通了。不写业务代码。

**分支**：`phase0-spike`

### 0.1 依赖服务启动

创建 `docker-compose.yml`（本 Phase 唯一要写的"正式"文件）：

服务清单（**只要这些，不准多**）：
- `milvus-standalone` (含 etcd + minio 依赖)
- `postgres:16`
- `redis:7-alpine`
- `kafka` (KRaft 模式，不用 Zookeeper)

验收：
```bash
docker compose up -d
docker compose ps          # 4 个服务全 healthy
```

### 0.2 四个连通性 Spike（写在 `scripts/spike/` 下，验收后删除）

| Spike | 文件 | 证明什么 | 成功标志 |
|-------|------|---------|---------|
| S1 | `spike/llm/main.go` | LLM API 通 | 打印出模型回复文本 |
| S2 | `spike/embed/main.go` | Embedding API 通 | 打印出向量维度（如 1536） |
| S3 | `spike/milvus/main.go` | Milvus 读写通 | 插 3 条向量，搜出最近的 1 条 |
| S4 | `spike/mcp/main.go` | MCP 协议通 | Claude Desktop 能调用 `hello` tool |

**S4 参考代码（已验证签名，`go-sdk@v1.7.0`）**：

```go
package main

import (
	"context"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	server := mcp.NewServer(&mcp.Implementation{Name: "kaho-spike"}, nil)

	type args struct {
		Name string `json:"name" jsonschema:"the person to greet"`
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "hello",
		Description: "say hi",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in args) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Hi " + in.Name}},
		}, nil, nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
```

### ✅ Phase 0 验收

- [ ] `docker compose ps` 四服务 healthy
- [ ] S1-S4 全部手动跑通，截图存 `docs/spike-evidence/`
- [ ] `git tag phase0-done`

### 🚫 Phase 0 不做

- 不写 chunker / retriever / agent 任何实现
- 不设计数据库表结构
- 不碰 Kafka（S1-S4 里没有它，Phase 1 才用）

---

## Phase 1：文档处理 Pipeline

**目标**：文件 → 分块 → 向量化 → 存入 Milvus。**同步执行，先不上 Kafka。**

**分支**：`phase1-pipeline`

### 1.1 接口先行（第一天只写这个文件）

`internal/rag/types.go`：

```go
package rag

import "context"

// Chunk 是分块后的最小检索单元
type Chunk struct {
	ID       string            // 稳定 ID: sha256(docID + index)
	DocID    string            // 源文档 ID
	Index    int               // 在文档中的序号
	Content  string            // 文本内容
	Metadata map[string]string // 来源、标题路径、文件类型等
}

// Chunker 将文档切分为 Chunk
type Chunker interface {
	Name() string
	Chunk(ctx context.Context, docID string, raw []byte) ([]Chunk, error)
}

// Embedder 文本转向量
// 注意：eino 的 embedding.Embedder 返回 [][]float64，
// 但 Milvus SDK 要 []float32，这里统一用 float32，在适配层转换
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dim() int
}

// VectorStore 向量存储
type VectorStore interface {
	Upsert(ctx context.Context, chunks []Chunk, vectors [][]float32) error
	Search(ctx context.Context, vector []float32, topK int) ([]ScoredChunk, error)
	DeleteByDoc(ctx context.Context, docID string) error
}

type ScoredChunk struct {
	Chunk Chunk
	Score float32
}
```

**这个文件定稿后，Phase 1-2 不准改**（真要改必须在 commit message 写明原因）。

### 1.2 实现顺序（严格按序，每步一个 commit）

| 步 | 文件 | 内容 | 单测验收 |
|----|------|------|---------|
| 1 | `internal/rag/chunker/plain.go` | 固定长度 + overlap 分块（**baseline，必须先有它**） | 给定 1000 字文本 + size=300/overlap=50，断言块数和边界 |
| 2 | `internal/rag/chunker/markdown.go` | 按 `#`/`##` 标题层级切，metadata 存标题路径 | 给定嵌套标题 MD，断言每块的 `heading_path` 正确 |
| 3 | `pkg/llm/embedder.go` | OpenAI 兼容 Embedding 客户端，带批量 + 重试 | 集成测试（打 tag `//go:build integration`） |
| 4 | `pkg/vectorstore/milvus.go` | Collection 建表 + Upsert + Search | 集成测试：插 10 条搜 top3 |
| 5 | `internal/pipeline/ingest.go` | 串起来：raw → chunk → embed → upsert | 端到端集成测试 |

### 1.3 Milvus Schema（定死，不准改）

```
Collection: kaho_documents
Fields:
  id          VarChar(64)   PRIMARY KEY
  doc_id      VarChar(64)               # 支持按文档删除
  chunk_index Int64
  content     VarChar(8192)
  metadata    JSON
  vector      FloatVector(1536)         # text-embedding-3-small

Index: HNSW on vector
  metric_type: COSINE
  M: 16
  efConstruction: 200
```

### 1.4 Kafka 异步化（**本 Phase 最后一步，同步链路跑通才做**）

- `internal/pipeline/producer.go` — HTTP 上传后发消息
- `internal/pipeline/consumer.go` — 消费后调用 `ingest.go` 的同名逻辑

**关键**：consumer 只是 `ingest.Run()` 的一层壳，不准在 consumer 里写业务逻辑。

### ✅ Phase 1 验收

```bash
go test ./internal/rag/... -v                          # 单测全绿
go test -tags=integration ./... -v                     # 集成测试全绿
go run scripts/eval/ingest_check.go docs/sample.md     # 手动验证
```

- [ ] 上传一个 Markdown → Milvus 里能查到对应 chunk（`doc_id` 匹配）
- [ ] 同一文件重复上传，chunk 数不翻倍（幂等，靠稳定 ID）
- [ ] 删除文档 → Milvus 里对应 chunk 全消失
- [ ] Kafka 停掉后重启，未处理消息不丢
- [ ] `git tag phase1-done`

### 🚫 Phase 1 不做

- 不写检索排序逻辑（Phase 2）
- 不接 LLM 对话（Phase 3）
- 不做 PDF 解析（先只支持 md/txt/go，PDF 是 Phase 6 可选项）
- 不做前端

---

## Phase 2：混合检索 + ReRank

**目标**：Query → 双路召回 → RRF 融合 → ReRank → Top-K。**有可量化的评估。**

**分支**：`phase2-retrieval`

### 2.1 评估集先行（**第一天必须做完，否则后面全是玄学**）

`scripts/eval/golden_set.json`：

```json
[
  {
    "query": "Redis 缓存穿透怎么解决",
    "relevant_chunk_ids": ["doc1-c3", "doc1-c4", "doc7-c0"],
    "note": "布隆过滤器 + 空值缓存"
  }
]
```

规模：**至少 50 条**（自己文档库里挑，1-2 小时能标完）。

`scripts/eval/eval.go` 输出：

```
Method              Recall@5   MRR      P99(ms)
------------------------------------------------
vector-only         0.41       0.48     28
bm25-only           0.35       0.39     15
hybrid-rrf          0.58       0.64     42
hybrid-rrf+rerank   0.63       0.72     180
```

**这张表就是简历数据的来源。没有它不准写简历。**

### 2.2 实现顺序

| 步 | 文件 | 内容 | 验收 |
|----|------|------|------|
| 1 | `scripts/eval/eval.go` | 评估框架（Recall@K / MRR） | 用假数据验证公式对 |
| 2 | `internal/rag/retriever/vector.go` | Milvus 向量召回 | eval 跑出 baseline |
| 3 | `internal/rag/retriever/bm25.go` | PG `ts_vector` 召回（中文用 `pg_jieba` 或退化到 `simple`） | eval 跑出 bm25 行 |
| 4 | `internal/rag/retriever/hybrid.go` | RRF 融合 | eval 分数高于两条单路 |
| 5 | `pkg/cache/query.go` | Redis 缓存 Query 向量 + 结果 | 二次请求 <10ms |
| 6 | `internal/rag/retriever/rerank.go` | LLM pointwise 打分精排 | eval MRR 再提升 |

### 2.3 RRF 公式（定死）

```go
// k=60 是论文推荐值，不准随便调
const rrfK = 60

func rrf(rankings [][]string) map[string]float64 {
    scores := make(map[string]float64)
    for _, ranking := range rankings {
        for rank, id := range ranking {
            scores[id] += 1.0 / float64(rrfK+rank+1)
        }
    }
    return scores
}
```

### 2.4 Retriever 接口（对齐 eino，方便 Phase 3 直接接）

eino 的接口是：
```go
// components/retriever/interface.go
type Retriever interface {
    Retrieve(ctx context.Context, query string, opts ...Option) ([]*schema.Document, error)
}
```

我们自己的 `HybridRetriever` 要实现这个接口，Phase 3 才能塞进 Eino Graph。

### ✅ Phase 2 验收

```bash
go run scripts/eval/eval.go --golden scripts/eval/golden_set.json
```

- [ ] 输出对比表，`hybrid-rrf` 的 MRR **明显高于** `vector-only`（低于就是实现有 bug）
- [ ] `HybridRetriever` 实现了 `retriever.Retriever` 接口（编译期断言 `var _ retriever.Retriever = (*HybridRetriever)(nil)`）
- [ ] Redis 缓存命中率 >60%（重复 query 场景）
- [ ] 评估结果存 `docs/eval-results.md`，带日期
- [ ] `git tag phase2-done`

### 🚫 Phase 2 不做

- 不训练/微调 Embedding 模型（性价比极低）
- 不做 Query 改写和 HyDE（**Phase 6 可选**，先把基础链路打穿）
- 不做多轮对话上下文（Phase 3）

---

## Phase 3：Agent 编排

**目标**：Eino ReAct Agent + Tool 调用 + SSE 流式输出。

**分支**：`phase3-agent`

### 3.1 已验证的 Eino API（`eino@v0.9.14`）

```go
import (
    "github.com/cloudwego/eino/adk"
    "github.com/cloudwego/eino/compose"
    "github.com/cloudwego/eino/components/tool"
)

// 1. 创建 Agent —— 注意 ChatModelAgentConfig 是
//    TypedChatModelAgentConfig[*schema.Message] 的别名
agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
    Name:        "KahoAgent",
    Description: "云盘智能文件助手",
    Instruction: systemPrompt,          // 支持 {Key} 占位符注入 session 值
    Model:       chatModel,             // model.BaseModel[*schema.Message]
    ToolsConfig: adk.ToolsConfig{
        ToolsNodeConfig: compose.ToolsNodeConfig{
            Tools: []tool.BaseTool{ /* ... */ },
        },
    },
    MaxIterations: 15,                  // 默认 20，防死循环
})

// 2. 用 Runner 跑（流式）
runner := adk.NewRunner(ctx, adk.RunnerConfig{
    Agent:           agent,
    EnableStreaming: true,
})

iter, err := runner.Query(ctx, "帮我找关于 Redis 的文档")
// iter 是 *adk.AsyncIterator[*adk.AgentEvent]，循环消费事件
```

### 3.2 Tool 定义（实现 `tool.InvokableTool`）

```go
// eino 的 tool 接口（已验证）
type BaseTool interface {
    Info(ctx context.Context) (*schema.ToolInfo, error)
}
type InvokableTool interface {
    BaseTool
    InvokableRun(ctx context.Context, argumentsInJSON string, opts ...Option) (string, error)
}
```

### 3.3 Tool 清单（**只做这 4 个，不准加**）

| Tool | 输入 | 输出 | 依赖 |
|------|------|------|------|
| `rag_query` | `{query, top_k}` | 检索到的 chunk + 引用来源 | Phase 2 的 HybridRetriever |
| `file_search` | `{keyword, limit}` | 文件列表（名称/大小/时间） | CloudKaho API |
| `file_metadata` | `{file_id}` | 单文件详情 | CloudKaho API |
| `file_share` | `{file_id, expire_hours}` | 预签名 URL | CloudKaho API |

**`file_upload` 不做成 Tool** —— 上传是大流量操作，走 HTTP 接口，不适合塞进 Agent 循环。

### 3.4 实现顺序

| 步 | 文件 | 验收 |
|----|------|------|
| 1 | `pkg/llm/chatmodel.go` | Eino ChatModel 适配（OpenAI 兼容） |
| 2 | `internal/agent/tools/rag_query.go` | 单独调用能返回检索结果 |
| 3 | `internal/agent/agent.go` | Agent 能调用 rag_query 并回答 |
| 4 | `internal/agent/memory.go` | Redis 存对话历史，滑动窗口截断 |
| 5 | `internal/agent/tools/file_*.go` | 剩下 3 个 Tool |
| 6 | `internal/handler/chat.go` | SSE 流式 HTTP 接口 |

### 3.5 上下文管理规则（定死）

```
最大历史轮数：10 轮
超出后：保留最近 6 轮 + 前面的摘要（用 LLM 压缩一次）
单次请求 Token 上限：8000（超出直接截断最早的历史）
```

### ✅ Phase 3 验收

手动测试脚本 `scripts/e2e/chat_test.sh`：

- [ ] "帮我找关于 Redis 缓存的文档" → 调用 `rag_query` → 返回答案 + 引用文件名
- [ ] "第二个文件多大" → 调用 `file_metadata`（**证明多轮上下文生效**）
- [ ] "给我一个分享链接" → 调用 `file_share` → 返回 URL
- [ ] 连续 12 轮对话不 OOM、不丢上下文（截断策略生效）
- [ ] SSE 逐 token 输出，首字延迟 <1.5s
- [ ] `git tag phase3-done`

### 🚫 Phase 3 不做

- 不做 Multi-Agent（单 Agent 够用，多 Agent 是过度设计）
- 不做前端页面（用 curl / Postman 验证）
- 不接 MCP（Phase 4）

---

## Phase 4：MCP 协议暴露

**目标**：Phase 3 的 Tool 通过 MCP 协议给 Claude Desktop / Cursor 用。

**分支**：`phase4-mcp`

### 4.1 已验证的 MCP API（`go-sdk@v1.7.0`）

```go
server := mcp.NewServer(&mcp.Implementation{
    Name:    "kahoagent",
    Version: "0.1.0",
}, nil)

type ragQueryArgs struct {
    Query string `json:"query" jsonschema:"搜索关键词"`
    TopK  int    `json:"top_k,omitempty" jsonschema:"返回结果数，默认5"`
}

mcp.AddTool(server, &mcp.Tool{
    Name:        "rag_query",
    Description: "语义搜索云盘中的文档内容",
}, func(ctx context.Context, req *mcp.CallToolRequest, in ragQueryArgs) (*mcp.CallToolResult, any, error) {
    // 复用 Phase 3 的 tool 逻辑，不准重写一遍
    result, err := ragTool.Run(ctx, in.Query, in.TopK)
    if err != nil {
        return nil, nil, err
    }
    return &mcp.CallToolResult{
        Content: []mcp.Content{&mcp.TextContent{Text: result}},
    }, nil, nil
})

// stdio 模式（Claude Desktop）
server.Run(ctx, &mcp.StdioTransport{})

// 或 HTTP 模式（远程）
handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
http.ListenAndServe(":3000", handler)
```

### 4.2 关键约束

**MCP 层是薄适配层，不准包含业务逻辑。**

```
internal/mcp/tools/rag_query.go  →  只做参数转换，调用 internal/agent/tools/rag_query.go
```

如果发现在 MCP 层写了 if/else 业务判断，说明设计错了，回去改 Phase 3 的 tool。

### 4.3 安全（简历亮点，必做）

| 项 | 实现 | 位置 |
|----|------|------|
| 并发限制 | `semaphore.NewWeighted(20)`，超出排队 | `internal/mcp/server/limiter.go` |
| 超时控制 | 每个 tool call `context.WithTimeout(30s)` | 同上 |
| 审计日志 | 记录 tool 名 + 参数摘要 + 耗时 + 调用方 | `internal/mcp/server/audit.go` |
| 参数校验 | JSON Schema 自动校验（SDK 提供）+ 业务范围校验（如 top_k ≤ 50） | 各 tool 内 |

### ✅ Phase 4 验收

- [ ] Claude Desktop 配置后，`list_tools` 能看到 4 个 Tool
- [ ] 在 Claude Desktop 里说"搜索我云盘里 Kafka 相关的文档"→ 成功调用并返回
- [ ] 并发 50 个 tool call：不 panic、不 OOM、超出 20 的排队而非拒绝
- [ ] 审计日志能看到完整调用记录
- [ ] 录一个 30 秒 GIF 存 `docs/demo.gif`（**简历/GitHub 用**）
- [ ] `git tag phase4-done`

### 🚫 Phase 4 不做

- 不实现 MCP Resources / Prompts 原语（Tools 够了）
- 不做 OAuth（API Key 足够，OAuth 是企业场景）
- 不做 MCP Client

---

## Phase 5：可观测性 + 压测出数

**目标**：产出简历上每一个数字。

**分支**：`phase5-observability`

### 5.1 Prometheus 指标（**只埋这些**）

```go
// internal/metrics/metrics.go
var (
    RetrievalDuration = prometheus.NewHistogramVec(...)  // labels: method(vector/bm25/hybrid/rerank)
    LLMTokens         = prometheus.NewCounterVec(...)    // labels: type(prompt/completion), model
    CacheHits         = prometheus.NewCounterVec(...)    // labels: cache(query_vec/result), result(hit/miss)
    ToolCalls         = prometheus.NewHistogramVec(...)  // labels: tool, status
    ActiveSessions    = prometheus.NewGauge(...)
)
```

### 5.2 压测清单（每项都要有输出文件）

| 测什么 | 工具 | 命令 | 输出 |
|--------|------|------|------|
| 检索延迟 | `go test -bench` | `go test -bench=Retrieve -benchmem ./internal/rag/...` | `docs/bench/retrieval.txt` |
| HTTP QPS | `vegeta` | `vegeta attack -rate=100 -duration=60s` | `docs/bench/http.txt` |
| MCP tool 并发 | 自写脚本 | `go run scripts/bench/mcp_load.go -c 50` | `docs/bench/mcp.txt` |
| 内存曲线 | pprof | `go tool pprof -http=:8081 heap.pprof` | `docs/bench/heap.svg` |

### 5.3 必须找到并修复至少 1 个性能问题

**这是简历上"pprof 定位到 X，优化后提升 Y"那一条的来源。**

常见可挖的点：
- Embedding 批量调用没做批处理（一条一条发）
- Chunk 结构体值传递导致大量拷贝
- Redis 序列化用 `encoding/json` 而非 `sonic`
- Milvus 连接没复用

修复前后各跑一次 bench，**数字记录在 `docs/optimization.md`**。

### ✅ Phase 5 验收

- [ ] `/metrics` 端点能看到全部指标
- [ ] `docs/bench/` 下有 4 个压测结果文件
- [ ] `docs/optimization.md` 记录至少 1 个优化的前后对比
- [ ] **填表**（这张表直接抄进简历）：

```markdown
| 指标 | 实测值 | 测量方式 |
|------|--------|---------|
| Recall@5 (hybrid) | ___ | 50 条 golden set |
| MRR (hybrid+rerank) | ___ | 同上 |
| 向量检索 P99 | ___ ms | go bench |
| 端到端首字延迟 | ___ ms | SSE 实测 |
| QPS (带缓存) | ___ | vegeta 60s |
| 缓存命中率 | ___ % | Prometheus |
| Tool 调用 P99 | ___ ms | MCP 压测 |
| 常驻内存 | ___ MB | pprof heap |
```

- [ ] `git tag phase5-done`

---

## Phase 6：部署收尾（可选增强）

**分支**：`phase6-polish`

### 必做
- [ ] `Dockerfile`（多阶段构建，最终镜像 <30MB）
- [ ] `.github/workflows/ci.yml`（lint + build + unit test）
- [ ] README 加架构图 + demo.gif
- [ ] 简历数字用 Phase 5 实测值替换

### 可选（时间够才做，按优先级）
1. Query 改写 / HyDE（提升检索质量）
2. PDF 解析支持
3. 简单 Web UI（一个 HTML 页面就行）
4. 文档更新增量索引

---

## 📅 时间线（每天 2-3h）

| 周 | Phase | 关键产出 |
|----|-------|---------|
| W1 前半 | Phase 0 | 4 个 spike 跑通 |
| W1 后半 ~ W2 | Phase 1 | 文档入库链路 |
| W3 | Phase 2 | **评估表出数** |
| W4 | Phase 3 | Agent 能对话 |
| W5 前半 | Phase 4 | Claude Desktop 能用 |
| W5 后半 | Phase 5 | 压测数据 |
| W6 | Phase 6 | 部署 + 简历定稿 |

**如果进度落后**：砍 Phase 6 可选项 → 砍 Phase 4（MCP）→ 但 **Phase 1/2/3 一个都不能砍**，那是项目的骨架。

---

## 🔍 卡住时的自查清单

| 症状 | 大概率原因 | 怎么办 |
|------|-----------|--------|
| 检索结果很烂 | 分块策略问题，不是模型问题 | 打印出实际的 chunk 看内容是否被切碎 |
| Agent 不调 Tool | Tool 的 Description 写得太模糊 | 用祈使句写清楚"什么时候该用这个工具" |
| Agent 死循环 | MaxIterations 太大 + Tool 返回值让模型困惑 | 降到 10，Tool 返回结构化文本而非 JSON 串 |
| MCP 连不上 | stdio 模式下往 stdout 打了日志 | **日志必须走 stderr**，stdout 是协议通道 |
| 编译一堆错 | 提前引入了下个 Phase 的依赖 | 回滚到上个 tag，按序来 |
| Milvus 搜不到 | 没 flush 或没 load collection | Insert 后要 `Flush()`，搜索前要 `LoadCollection()` |
