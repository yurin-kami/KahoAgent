# KahoAgent 实现路线图

> 总工期预估：4-6 周（每天 2-3 小时）
> 前置知识：Go 基础、HTTP/中间件、Redis/PostgreSQL 基本操作

---

## Phase 0：前置学习（3-5 天）

### 需要搞懂的概念

| 概念 | 你需要理解到什么程度 | 推荐资源 |
|------|---------------------|---------|
| RAG 原理 | 知道 "检索→增强→生成" 三步是什么、为什么不能只靠 LLM | [吴恩达 RAG 课](https://www.deeplearning.ai/short-courses/building-evaluating-advanced-rag/) (1.5h) |
| Embedding | 知道文本→向量的过程、余弦相似度、为什么需要分块 | 上面课程会覆盖 |
| Eino 框架 | 跑通官方 quickstart、理解 Graph/Chain/Tool/ChatModel 四个核心抽象 | [eino-examples/quickstart](https://github.com/cloudwego/eino-examples/tree/main/quickstart) |
| MCP 协议 | 知道 Tool/Resource/Prompt 三种原语、JSON-RPC 通信方式 | [MCP 官方文档](https://modelcontextprotocol.io/introduction) + [Go SDK Quick Start](https://go.sdk.modelcontextprotocol.io/quick_start/) |
| 向量数据库 | Milvus 的 Collection/Index/Search 基本概念 | [Milvus 快速入门](https://milvus.io/docs/quickstart.md) |

### 动手练习

1. 用 `go-openai` 或 DeepSeek API 发一次 Chat 请求（确认 API Key 通了）
2. 用 Eino 的 `ChatModel` 组件发一次对话
3. 用 `mcp-go` 或官方 SDK 写一个只有 1 个 Tool（如 `hello`）的 MCP Server，在 Claude Desktop 里调通
4. Docker 启动一个 Milvus standalone，用 SDK 插入 3 条向量、检索 1 次

---

## Phase 1：文档处理 Pipeline（5-7 天）

### 目标
文件上传 → Kafka → 解析/分块 → Embedding → 写入 Milvus

### 实现内容

```
internal/pipeline/producer.go   -- 接收文件上传，发 Kafka 消息
internal/pipeline/consumer.go   -- Kafka consumer，调用分块+向量化
internal/rag/chunker/
    ├── chunker.go              -- Chunker 接口定义
    ├── markdown.go             -- Markdown 按标题层级分块
    ├── plain.go                -- 纯文本按段落+滑动窗口分块
    └── code.go                 -- 代码文件按函数/类分块
internal/rag/embedding/
    └── openai.go               -- 调用 Embedding API（OpenAI/DeepSeek 兼容）
pkg/vectorstore/
    └── milvus.go               -- Milvus CRUD 封装（CreateCollection/Insert/Search）
```

### 学习要点

| 要点 | 说明 |
|------|------|
| 分块策略 | 固定长度 vs 语义分块 vs 结构化分块（按标题/函数），理解 overlap 的作用 |
| Kafka 生产消费 | `segmentio/kafka-go` 的 Writer/Reader 用法，理解 partition 和 offset |
| Embedding 模型选型 | `text-embedding-3-small` vs `bge-m3`，维度和成本的 tradeoff |
| Milvus Schema | 定义 Collection 字段（id/vector/content/metadata），HNSW 参数（M/efConstruction） |

### 验收标准
- [ ] 写一个测试：上传一个 Markdown 文件 → 最终在 Milvus 能搜到对应 chunk
- [ ] 语义分块和固定分块的对比测试（手动评估 5-10 个 query 的召回质量）

---

## Phase 2：混合检索 + ReRank（4-5 天）

### 目标
Query → 向量检索 + BM25 全文检索 → RRF 融合 → ReRank → 返回 Top-K 文档

### 实现内容

```
internal/rag/retriever/
    ├── retriever.go     -- Retriever 接口
    ├── milvus.go        -- 向量检索（Dense）
    ├── bm25.go          -- PostgreSQL ts_vector 全文检索（Sparse）
    ├── hybrid.go        -- RRF 融合排序
    └── rerank.go        -- Cross-Encoder / LLM rerank
pkg/cache/
    └── redis.go         -- Query 向量缓存 + 结果缓存
```

### 学习要点

| 要点 | 说明 |
|------|------|
| BM25 原理 | PostgreSQL 的 `to_tsvector` + `ts_rank`，中文需要 `pg_jieba` 分词 |
| RRF (Reciprocal Rank Fusion) | 公式：`score = Σ 1/(k+rank_i)`，理解 k 值的影响 |
| ReRank | 用 LLM 做 pointwise rerank（给每个 chunk 打相关性分），或 Cross-Encoder 模型 |
| 缓存策略 | 相同 Query 向量命中 Redis 缓存，TTL + LRU 淘汰 |

### 验收标准
- [ ] 构建 200 条 Golden Set（query + 预期相关文档）
- [ ] 对比指标：纯向量 MRR vs 混合检索 MRR vs 混合+ReRank MRR
- [ ] Redis 缓存命中后响应 <10ms

---

## Phase 3：Agent 编排（5-7 天）

### 目标
基于 Eino ADK 实现 ReAct Agent，支持多 Tool 调用、对话记忆

### 实现内容

```
internal/agent/
    ├── agent.go          -- Agent 初始化与运行（Eino ADK）
    ├── prompt.go         -- System Prompt 模板
    ├── tools.go          -- Tool 注册（file_search/upload/share/rag_query/...）
    └── memory.go         -- 对话记忆（Redis 存储上下文窗口）
internal/handler/
    ├── chat.go           -- POST /api/chat（SSE 流式响应）
    └── upload.go         -- POST /api/upload（触发文档处理管道）
```

### 学习要点

| 要点 | 说明 |
|------|------|
| ReAct 范式 | Reasoning → Action → Observation 循环，理解 MaxIterations 防死循环 |
| Eino ADK | `adk.NewChatModelAgent` 的配置、ToolsConfig、Graph 编排 |
| Tool 定义 | 符合 OpenAI Function Calling Schema，参数 JSON Schema 描述 |
| 流式输出 | SSE (Server-Sent Events) 的 Go 实现，逐 Token 推送 |
| Context Window 管理 | 滑动窗口 + 摘要压缩，控制 Token 消耗 |

### 验收标准
- [ ] 用户发 "帮我找关于 Redis 的文档" → Agent 调用 rag_query tool → 流式返回答案+引用
- [ ] 用户发 "分享刚才那个文件给 xxx" → Agent 调用 file_share tool
- [ ] 对话 5 轮以上不丢失上下文

---

## Phase 4：MCP 协议暴露（3-4 天）

### 目标
将 Agent 的 Tools 以 MCP 标准协议暴露，Claude Desktop/Cursor 可直接调用

### 实现内容

```
internal/mcp/server/
    └── server.go         -- MCP Server 初始化、传输层配置
internal/mcp/tools/
    ├── file_search.go    -- MCP Tool: 语义搜索文件
    ├── file_upload.go    -- MCP Tool: 上传文件到云盘
    ├── file_share.go     -- MCP Tool: 生成分享链接
    ├── rag_query.go      -- MCP Tool: 知识库问答
    └── metadata.go       -- MCP Tool: 查询文件元数据
```

### 学习要点

| 要点 | 说明 |
|------|------|
| MCP Tool 定义 | Name + Description + InputSchema (JSON Schema) + Handler |
| 传输层 | stdio（本地）vs SSE（远程），`go-sdk` 的 `mcp.NewServer` + Transport 配置 |
| 鉴权 | OAuth2 / API Key 验证，防止未授权调用 |
| 安全 | goroutine 池限制并发、敏感操作审计日志、参数校验防注入 |

### 验收标准
- [ ] Claude Desktop 中能 list 出所有 Tools
- [ ] 在 Claude Desktop 对话中说 "搜索我云盘里关于 Kafka 的文件" → 成功调用 tool 并返回结果
- [ ] 并发压测 50 个 tool call 不 panic、内存稳定

---

## Phase 5：性能优化 + 可观测性（3-4 天）

### 目标
加 Prometheus 指标、pprof 性能分析、压测出简历上的数据

### 实现内容

```
internal/middleware/
    ├── metrics.go        -- Prometheus 指标中间件
    ├── ratelimit.go      -- 令牌桶限流
    └── trace.go          -- TraceID 透传
scripts/
    ├── benchmark.sh      -- wrk/vegeta 压测脚本
    └── golden_set.json   -- RAG 评估数据集
docs/
    └── metrics.md        -- 指标说明
```

### 需要产出的量化指标（简历用）

| 指标 | 目标 | 怎么测 |
|------|------|--------|
| Recall@5 | ≥0.60 | Golden Set 评估 |
| MRR | ≥0.70 | Golden Set 评估 |
| 检索延迟 (Milvus) | <30ms P99 | wrk 压测 |
| 端到端首字延迟 | <800ms | 实测 SSE 第一个 token |
| QPS（带缓存） | >300 | vegeta 压测 |
| Tool 调用延迟 | <50ms P99 | MCP 压测 |
| 内存占用 | 稳定 <256MB | 长时间运行观察 |

### 学习要点

| 要点 | 说明 |
|------|------|
| Prometheus | `promhttp.Handler()`、Histogram/Counter/Gauge 使用 |
| pprof | `net/http/pprof`、火焰图生成、定位热点 |
| 压测工具 | `wrk` / `vegeta` / `go test -bench` |
| RAG 评估 | MRR、Recall@K、NDCG 的计算方法 |

### 验收标准
- [ ] Prometheus 能看到检索耗时、Token 消耗、缓存命中率面板
- [ ] pprof 找到至少一个优化点并修复（简历可写）
- [ ] 所有量化指标达标并记录

---

## Phase 6：收尾 + 部署（2-3 天）

### 内容
- [ ] Dockerfile + docker-compose.yml（一键启动所有依赖）
- [ ] GitHub Actions CI（lint + build + test）
- [ ] README 完善（GIF 演示、架构图）
- [ ] 简历 bullet points 最终定稿（用实测数据替换估计值）

---

## 📚 学习资源汇总

### Eino 框架
- [Eino 官方教程手册](https://github.com/wdx-123/go-eino-handbook) — 19 篇系统教程
- [eino-examples](https://github.com/cloudwego/eino-examples) — 官方示例代码
- [白泽 Eino 全解系列](https://www.cnblogs.com/YLTFY1998/p/18871451.html) — 含 RAG 实战
- [Eino + Redis RAG 实战](https://blog.csdn.net/weixin_58753619/article/details/155595263) — 保姆级教程

### MCP 协议
- [MCP 官方文档](https://modelcontextprotocol.io/introduction)
- [Go SDK 文档](https://go.sdk.modelcontextprotocol.io/)
- [Go MCP Server 实战 (CSDN)](https://blog.csdn.net/zy_dreamer/article/details/163626620) — 2026.08 最新
- [GitHub 官方 Go MCP 最佳实践](https://github.com/github/awesome-copilot/blob/main/instructions/go-mcp-server.instructions.md)

### RAG 工程
- [吴恩达 Building RAG](https://www.deeplearning.ai/short-courses/building-evaluating-advanced-rag/) — 免费短课
- [手把手 Go + Eino RAG (InfoQ)](https://xie.infoq.cn/article/ed3e1f316f6f5754334c9f323)
- [RAG 简历怎么写 (CSDN)](https://blog.csdn.net/2401_85375186/article/details/159115977) — 面试官视角

### 向量数据库
- [Milvus 官方文档](https://milvus.io/docs)
- [Milvus + Go SDK](https://milvus.io/docs/install-go.md)

### 简历写法
- [Agent 项目简历怎么写 (牛客)](https://www.nowcoder.com/discuss/832013460337672192)
- [AI Agent 简历模板 (牛客)](https://m.nowcoder.com/discuss/912309942026002432)

---

## ⏱️ 时间线总览

```
Week 1: Phase 0 (学习) + Phase 1 前半（分块器）
Week 2: Phase 1 后半（Kafka + Milvus 入库）+ Phase 2（检索）
Week 3: Phase 3（Agent 编排 + HTTP API）
Week 4: Phase 4（MCP）+ Phase 5 前半（指标埋点）
Week 5: Phase 5 后半（压测优化）+ Phase 6（部署收尾）
```

如果每天时间有限（1-2 小时），可以拉到 6 周。核心是 Phase 1-3，这三块做完简历就能写了，Phase 4-5 是锦上添花。
