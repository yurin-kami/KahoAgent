# KahoAgent 详细实现规范

> 本文档是**执行契约**，不是参考资料。每个 Phase 的「不做什么」和「验收命令」是硬约束。
> 所有接口签名已对照 `eino@v0.9.14` / `go-sdk@v1.7.0` / `milvus-sdk-go@v2.4.2` / `kafka-go@v0.4.51` 源码验证，可直接照抄。
>
> **阅读方式**：做到哪个 Phase 看哪个 Phase。提前看后面的会让你想「顺手做一下」，那就是漂移的开始。

---

## 🚫 全局防漂移铁律

| # | 规则 | 违反的后果 |
|---|------|-----------|
| 1 | **一个 Phase 未通过验收，不准开下一个** | 底层没跑通，上层全是幻觉代码 |
| 2 | **禁止提前引入未列在当前 Phase 的依赖** | 依赖爆炸，编译一天修不好 |
| 3 | **每个 Phase 结束必须 `go build ./... && go test ./...` 通过** | 留着编译错误进下一阶段 = 技术债雪球 |
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

### 「我想加个功能」时的三问

1. 它在当前 Phase 的文件清单里吗？→ 不在就记进 `docs/BACKLOG.md`，不准现在做
2. 不做它，当前 Phase 的验收能过吗？→ 能过就不做
3. 它会改动已冻结的接口吗？→ 会就绝对不做

---

# Phase 0：环境验证 + 最小可运行样例

**目标**：证明每个外部依赖都通了。不写业务代码。
**分支**：`phase0-spike` · **预计**：3-4 天

---

## 📚 本阶段学习内容

### L0.1 RAG 到底在解决什么问题（必学，1 小时）

**学之前先理解痛点**：LLM 有两个硬伤 —— 知识截止日期、不知道你的私有数据。微调成本高且更新慢，所以业界的解法是：**把相关资料塞进 prompt 里让模型现读现答**。这就是 RAG。

```
用户问题 → [检索] 从知识库找出最相关的几段文字
         → [增强] 把这几段拼进 prompt
         → [生成] LLM 基于这些材料回答
```

**关键认知**：RAG 系统 90% 的效果取决于「检索」这一步。检索不准，再强的模型也是垃圾进垃圾出。所以本项目 Phase 1-2 全部在打磨检索，Phase 3 才碰生成。

- 资源：[吴恩达 Building & Evaluating Advanced RAG](https://www.deeplearning.ai/short-courses/building-evaluating-advanced-rag/)（免费，1.5h，看前 3 节即可）
- **自测**：能用一句话说清「为什么不能把整个文档丢给 LLM」（答案：上下文窗口有限 + 成本 + 长文本中间信息会被忽略，即 lost-in-the-middle）

### L0.2 Embedding 与向量检索（必学，1 小时）

**核心概念**：
- **Embedding**：把文本映射成一个高维向量（如 1536 维），语义相近的文本向量距离也近
- **余弦相似度**：衡量两个向量方向的接近程度，值域 [-1, 1]，越大越相似
- **为什么用余弦不用欧氏距离**：文本向量的「长度」不携带语义信息，只有方向重要

**动手理解**：
```
"猫" 的向量 · "狗" 的向量  → 相似度 0.7   （都是宠物）
"猫" 的向量 · "汽车" 的向量 → 相似度 0.1   （无关）
```

**必须理解的坑**：Embedding 模型有最大输入长度（通常 512-8192 token）。超长文本必须先分块 —— 这就是 Phase 1 的 Chunker 存在的原因。

- **自测**：解释为什么「把一整本书 embed 成一个向量」是错的（答案：语义被平均掉了，检索时什么都匹配不上）

### L0.3 Milvus 基本概念（必学，1 小时）

| 概念 | 类比关系型数据库 | 说明 |
|------|-----------------|------|
| Collection | Table | 一组同 schema 的向量 |
| Field | Column | 字段，其中必须有一个 VectorField |
| Index | Index | 向量索引，决定检索速度与精度 |
| Partition | 分区表 | 本项目不用，先忽略 |

**HNSW 索引**（本项目用的）：
- 全称 Hierarchical Navigable Small World，一种图索引
- 参数 `M`：每个节点的连接数，越大越准但内存越高（推荐 16）
- 参数 `efConstruction`：建索引时的搜索宽度，越大质量越好但建索引越慢（推荐 200）
- 参数 `ef`（搜索时）：越大越准越慢，必须 ≥ topK（推荐 64）

**两个必踩的坑**（现在记住，Phase 1 会遇到）：
1. `Insert` 之后不 `Flush()`，数据还在内存 buffer 里，搜不到
2. 搜索前必须 `LoadCollection()` 把索引加载进内存，否则报错

- 资源：[Milvus Quickstart](https://milvus.io/docs/quickstart.md)

### L0.4 MCP 协议（必学，1.5 小时）

**一句话**：MCP 是 AI 客户端（Claude Desktop / Cursor）调用外部工具的标准协议，相当于 AI 世界的 USB-C。

**三种原语**：
| 原语 | 作用 | 本项目用吗 |
|------|------|-----------|
| **Tools** | 模型可主动调用的函数 | ✅ 只用这个 |
| Resources | 模型可读取的数据源 | ❌ 不做 |
| Prompts | 预定义的提示词模板 | ❌ 不做 |

**通信方式**：JSON-RPC 2.0，两种传输层
- **stdio**：本地进程，通过标准输入输出通信（Claude Desktop 用这个）
- **Streamable HTTP**：远程服务

**⚠️ 最致命的坑**：stdio 模式下 **stdout 是协议通道**。你打一行 `fmt.Println("debug")` 到 stdout，整个协议就崩了。**所有日志必须走 stderr**。

- 资源：[MCP 官方介绍](https://modelcontextprotocol.io/introduction) + [Go SDK Quick Start](https://go.sdk.modelcontextprotocol.io/quick_start/)

### L0.5 Eino 框架的四个核心抽象（必学，1.5 小时）

| 抽象 | 是什么 | 对应本项目 |
|------|--------|-----------|
| **ChatModel** | 大模型对话接口 | Phase 3 用 |
| **Tool** | 模型可调用的工具 | Phase 3 用 |
| **Retriever** | 检索器 | Phase 2 我们自己实现这个接口 |
| **Graph / Chain** | 编排：把组件连成流程 | Phase 3 用 ADK 封装好的 ReAct |

**为什么选 Eino 而不是自己撸**：ReAct 循环（模型思考→调工具→看结果→再思考）的状态管理、流式处理、错误重试非常繁琐，Eino 的 `adk` 包封装好了。但我们**不会盲用** —— Phase 2 的检索器是自己实现的，只是让它满足 Eino 的接口，这样既有掌控力又能接进生态。

- 资源：[eino-examples/quickstart](https://github.com/cloudwego/eino-examples/tree/main/quickstart)

---

## 🔨 实现内容

### 0.1 docker-compose.yml

**目的**：一条命令拉起全部依赖，避免「在我机器上能跑」。

服务清单（**只要这些，不准多**）：

| 服务 | 镜像 | 端口 | 为什么需要 |
|------|------|------|-----------|
| milvus-standalone | `milvusdb/milvus:v2.4.x` | 19530 | 向量存储与检索 |
| etcd | `quay.io/coreos/etcd` | - | Milvus 的元数据依赖 |
| minio | `minio/minio` | - | Milvus 的对象存储依赖 |
| postgres | `postgres:16` | 5432 | Phase 2 的 BM25 全文检索 |
| redis | `redis:7-alpine` | 6379 | 缓存 |
| kafka | `bitnami/kafka`（KRaft 模式） | 9092 | Phase 1 的异步管道 |

**注意**：etcd 和 minio 是 Milvus 的内部依赖，不是我们直接用的。Kafka 用 KRaft 模式（不需要 Zookeeper），配置更简单。

验收：
```bash
docker compose up -d
docker compose ps          # 全部 healthy（Milvus 启动慢，等 60s）
```

### 0.2 四个连通性 Spike

**为什么要写 spike**：每个外部依赖单独验证一次，出问题时能精确定位。如果直接写业务代码，报错时你分不清是 API Key 错了、网络不通、还是代码逻辑有 bug。

写在 `scripts/spike/` 下，**Phase 0 验收后删除**（它们不是产品代码）。

| Spike | 文件 | 证明什么 | 成功标志 |
|-------|------|---------|---------|
| S1 | `spike/llm/main.go` | LLM API 通 | 打印出模型回复文本 |
| S2 | `spike/embed/main.go` | Embedding API 通 | 打印出向量维度（应为 1536） |
| S3 | `spike/milvus/main.go` | Milvus 读写通 | 插 3 条向量，搜出最近的 1 条 |
| S4 | `spike/mcp/main.go` | MCP 协议通 | Claude Desktop 能调用 `hello` tool |

---

### S3 参考代码（Milvus，已验证 `milvus-sdk-go@v2.4.2`）

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/milvus-io/milvus-sdk-go/v2/client"
    "github.com/milvus-io/milvus-sdk-go/v2/entity"
)

const (
    collName = "spike_test"
    dim      = 4 // spike 用小维度，真实项目是 1536
)

func main() {
    ctx := context.Background()

    // ── 1. 连接 ──────────────────────────────────────────
    // 目的：验证 Milvus 服务可达。连不上说明 docker compose 没起来。
    cli, err := client.NewClient(ctx, client.Config{Address: "127.0.0.1:19530"})
    if err != nil {
        log.Fatalf("connect failed: %v", err)
    }
    defer cli.Close()

    // ── 2. 建 Collection ─────────────────────────────────
    // 目的：理解 Schema 定义方式。生产环境的 schema 见 Phase 1.3。
    // 注意：PrimaryKey 字段必须显式标记，VarChar 必须指定 max_length。
    schema := entity.NewSchema().
        WithName(collName).
        WithField(entity.NewField().
            WithName("id").
            WithDataType(entity.FieldTypeVarChar).
            WithIsPrimaryKey(true).
            WithMaxLength(64)).
        WithField(entity.NewField().
            WithName("vector").
            WithDataType(entity.FieldTypeFloatVector).
            WithDim(dim))

    // shardsNum=1：spike 阶段单分片够用，生产可调
    if err := cli.CreateCollection(ctx, schema, 1); err != nil {
        log.Fatalf("create collection: %v", err)
    }

    // ── 3. 插入数据 ───────────────────────────────────────
    // 目的：理解 Milvus 的列式插入 —— 不是一行一行插，是一列一列插。
    ids := []string{"a", "b", "c"}
    vecs := [][]float32{
        {1, 0, 0, 0},
        {0, 1, 0, 0},
        {0.9, 0.1, 0, 0}, // 与 "a" 最接近
    }
    _, err = cli.Insert(ctx, collName, "",
        entity.NewColumnVarChar("id", ids),
        entity.NewColumnFloatVector("vector", dim, vecs),
    )
    if err != nil {
        log.Fatalf("insert: %v", err)
    }

    // ⚠️ 坑 1：不 Flush 数据还在内存 buffer，搜不到
    if err := cli.Flush(ctx, collName, false); err != nil {
        log.Fatalf("flush: %v", err)
    }

    // ── 4. 建索引 ─────────────────────────────────────────
    // 目的：理解 HNSW 参数含义。M=16/efConstruction=200 是社区推荐值。
    // COSINE：余弦相似度，适合文本向量（L0.2 学过为什么）
    idx, err := entity.NewIndexHNSW(entity.COSINE, 16, 200)
    if err != nil {
        log.Fatalf("build index config: %v", err)
    }
    if err := cli.CreateIndex(ctx, collName, "vector", idx, false); err != nil {
        log.Fatalf("create index: %v", err)
    }

    // ⚠️ 坑 2：搜索前必须 Load，把索引加载进内存
    if err := cli.LoadCollection(ctx, collName, false); err != nil {
        log.Fatalf("load: %v", err)
    }

    // ── 5. 检索 ───────────────────────────────────────────
    // 目的：验证检索能返回预期结果。查 {1,0,0,0} 应该命中 "a"，其次 "c"。
    // ef=64：搜索时的候选集大小，必须 ≥ topK
    sp, _ := entity.NewIndexHNSWSearchParam(64)
    results, err := cli.Search(ctx, collName,
        nil,                             // partitions：不分区
        "",                              // expr：标量过滤条件，Phase 2 会用到
        []string{"id"},                  // outputFields：要返回的字段
        []entity.Vector{entity.FloatVector([]float32{1, 0, 0, 0})},
        "vector",                        // vectorField
        entity.COSINE,                   // 必须与建索引时一致
        2,                               // topK
        sp,
    )
    if err != nil {
        log.Fatalf("search: %v", err)
    }

    for _, r := range results {
        fmt.Printf("命中 %d 条:\n", r.ResultCount)
        idCol := r.Fields.GetColumn("id").(*entity.ColumnVarChar)
        for i := 0; i < r.ResultCount; i++ {
            v, _ := idCol.ValueByIdx(i)
            fmt.Printf("  id=%s  score=%.4f\n", v, r.Scores[i])
        }
    }

    // 清理，避免影响后续
    _ = cli.DropCollection(ctx, collName)
}
```

**预期输出**：
```
命中 2 条:
  id=a  score=1.0000
  id=c  score=0.9939
```
如果顺序反了或分数不对，说明 metricType 配错了。

---

### S4 参考代码（MCP，已验证 `go-sdk@v1.7.0`）

```go
package main

import (
    "context"
    "log"
    "os"

    "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
    // ⚠️ 关键：stdio 模式下 stdout 是协议通道，日志必须走 stderr
    logger := log.New(os.Stderr, "[spike-mcp] ", log.LstdFlags)

    // ── 1. 创建 Server ────────────────────────────────────
    // Implementation 的 Name 会显示在客户端的工具列表里
    server := mcp.NewServer(&mcp.Implementation{
        Name:    "kaho-spike",
        Version: "0.0.1",
    }, nil)

    // ── 2. 定义参数结构体 ──────────────────────────────────
    // 目的：SDK 会用反射从这个结构体自动生成 JSON Schema。
    // `jsonschema` tag 里的描述会传给模型，直接影响模型能否正确调用 —— 要写清楚。
    type args struct {
        Name string `json:"name" jsonschema:"要打招呼的人名"`
    }

    // ── 3. 注册 Tool ──────────────────────────────────────
    // 用泛型版 mcp.AddTool（而非 server.AddTool），它会：
    //   - 自动生成 input schema
    //   - 自动校验入参
    //   - 自动处理错误包装
    mcp.AddTool(server, &mcp.Tool{
        Name:        "hello",
        Description: "向指定的人打招呼", // 模型据此判断何时调用
    }, func(ctx context.Context, req *mcp.CallToolRequest, in args) (*mcp.CallToolResult, any, error) {
        logger.Printf("tool called: name=%s", in.Name)

        return &mcp.CallToolResult{
            Content: []mcp.Content{
                &mcp.TextContent{Text: "Hi, " + in.Name + "!"},
            },
        }, nil, nil
    })

    // ── 4. 启动 ───────────────────────────────────────────
    // StdioTransport：通过 stdin/stdout 与 Claude Desktop 通信
    logger.Println("MCP server starting on stdio...")
    if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
        logger.Fatalf("server failed: %v", err)
    }
}
```

**Claude Desktop 配置**（`claude_desktop_config.json`）：
```json
{
  "mcpServers": {
    "kaho-spike": {
      "command": "go",
      "args": ["run", "scripts/spike/mcp/main.go"],
      "cwd": "A:\dev_pjs\KahoAgent"
    }
  }
}
```

**验证**：重启 Claude Desktop → 在对话框输入「用 hello 工具跟 kami 打招呼」→ 应返回 `Hi, kami!`

---

## ✅ Phase 0 验收

- [ ] `docker compose ps` 六个服务 healthy
- [ ] S1-S4 全部手动跑通，截图存 `docs/spike-evidence/`
- [ ] 能口头回答 L0.1-L0.5 的每个「自测」问题
- [ ] `git tag phase0-done`

## 🚫 Phase 0 不做

- 不写 chunker / retriever / agent 任何实现
- 不设计数据库表结构（Phase 1 才定）
- 不碰 Kafka（S1-S4 里没有它）
- 不优化 spike 代码质量（它们马上要删）

---

# Phase 1：文档处理 Pipeline

**目标**：文件 → 分块 → 向量化 → 存入 Milvus。**先同步跑通，最后才上 Kafka。**
**分支**：`phase1-pipeline` · **预计**：5-7 天

---

## 📚 本阶段学习内容

### L1.1 分块策略（核心，必须理解透）

**为什么分块是 RAG 效果的第一杀手**：块切得不好，检索到的内容要么缺上下文（太碎），要么混入无关信息（太大）。

| 策略 | 做法 | 优点 | 缺点 | 本项目 |
|------|------|------|------|--------|
| **固定长度** | 每 N 字符切一刀 | 简单、块大小均匀 | 可能从句子中间切断 | ✅ 做 baseline |
| **固定长度 + overlap** | 相邻块重叠 M 字符 | 缓解切断问题 | 存储冗余 | ✅ baseline 标配 |
| **递归字符分割** | 按 `\n\n` → `\n` → `。` 优先级递归切 | 尽量保持语义完整 | 块大小不均 | ⏸️ 可选增强 |
| **结构化分割** | 按 Markdown 标题 / 代码函数切 | 语义边界最准，可保留层级 | 仅适用于结构化文档 | ✅ 主力方案 |
| **语义分割** | 计算相邻句子 embedding 相似度，在语义转折处切 | 效果最好 | 成本高（每句都要 embed） | ❌ 不做 |

**overlap 的作用**：假设块边界正好切在「布隆过滤器可以解决缓存穿透」这句话中间，前一块结尾是「布隆过滤器可以」，后一块开头是「解决缓存穿透」，两块单独看都不完整。overlap 让后一块包含前一块的结尾部分，保证语义完整。

**本项目的选择理由**：
- 先做**固定长度 + overlap** —— 它是 baseline，Phase 2 评估时要用它做对照组，证明「结构化分割确实更好」
- 再做 **Markdown 标题分割** —— 我们的文档主要是技术文档，标题层级天然是语义边界

**自测**：给定一篇有 `# 一级标题 / ## 二级标题` 的文档，说出按标题切和按 500 字切，检索「Redis 主从复制怎么配置」时哪个更可能命中正确内容，为什么。

### L1.2 幂等性设计（工程细节，面试常问）

**问题**：同一个文件上传两次，会不会在 Milvus 里存两份？

**解法**：让 Chunk ID 可复现 —— `ID = sha256(DocID + "#" + Index)`。第二次上传时，同样的输入产生同样的 ID，Milvus 的 `Upsert` 会覆盖而非新增。

**为什么不用自增 ID 或 UUID**：那样每次上传都是新记录，重复数据会污染检索结果。

**面试可以聊的点**：这是「幂等」在数据管道中的应用。类比 HTTP 的 PUT（幂等）vs POST（非幂等）。

### L1.3 Embedding API 的工程问题（必学）

| 问题 | 后果 | 解法 |
|------|------|------|
| 一条一条调用 | 100 个 chunk = 100 次 HTTP，慢 100 倍 | **批量调用**，一次发 N 条（OpenAI 支持最多 2048 条） |
| 无重试 | 网络抖动就整个任务失败 | 指数退避重试（1s → 2s → 4s，最多 3 次） |
| 无限流 | 触发 API rate limit 被封 | 令牌桶限流 |
| 超长文本 | API 报错 | 入库前校验 token 数，超长的截断或再切 |

**本项目实现要求**：批量 + 重试必做，限流看情况（个人项目 QPS 低）。

### L1.4 Kafka 的作用与关键概念（必学）

**为什么要 Kafka**：文档向量化很慢（一个 10 页 PDF 可能要 30 秒）。如果同步处理，用户上传时 HTTP 请求会挂 30 秒。用 Kafka 解耦：上传接口收到文件立即返回，后台 worker 慢慢处理。

| 概念 | 说明 | 本项目 |
|------|------|--------|
| Topic | 消息分类 | `document-processing` |
| Partition | Topic 的并行单元，一个 partition 只能被同组内一个 consumer 消费 | 3 个（支持 3 个 worker 并行） |
| Consumer Group | 一组 consumer 协作消费一个 topic | `kahoagent-worker` |
| Offset | 消费位置 | 手动提交（保证处理完才确认） |

**⚠️ 关键坑**：`ReadMessage` 会自动提交 offset，如果处理失败消息就丢了。必须用 `FetchMessage` + 处理成功后 `CommitMessages`，这叫「至少一次」语义。

**自测**：解释为什么本项目「先同步跑通，最后才上 Kafka」（答案：异步会让调试变难 —— 出错时你不知道是业务逻辑错了还是消息没消费到。先证明业务逻辑对，再加异步层）。

---

## 🔨 实现内容

### 1.1 接口先行（第一天只写这个文件）

`internal/rag/types.go` —— **已完成并冻结**。核心约定：

```go
type Chunk struct {
    ID       string            // sha256(DocID + Index)，保证幂等
    DocID    string            // 支持按文档批量删除
    Index    int               // 块序号
    Content  string
    Metadata map[string]string // heading_path / file_type / source 等
}

type Chunker interface {
    Name() string  // 用于评估对比时区分策略
    Chunk(ctx context.Context, docID string, raw []byte) ([]Chunk, error)
}

type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
    Dim() int
}

type VectorStore interface {
    Upsert(ctx context.Context, chunks []Chunk, vectors [][]float32) error
    Search(ctx context.Context, vector []float32, topK int) ([]ScoredChunk, error)
    DeleteByDoc(ctx context.Context, docID string) error
}
```

**设计说明**：
- `Chunker.Name()` 存在的唯一目的是 Phase 2 评估时能在结果表里区分不同策略
- `Embedder` 用 `float32` 而非 eino 的 `float64` —— Milvus SDK 要 float32，转换在适配层做一次，避免全链路来回转
- `VectorStore` 不暴露 Milvus 细节，Phase 2 如果要换 Qdrant 只需换实现

---

### 1.2 实现顺序（严格按序，每步一个 commit）

| 步 | 文件 | 内容 | 单测验收 |
|----|------|------|---------|
| 1 | `internal/rag/chunker/plain.go` | 固定长度 + overlap（**baseline，必须先有**） | 给定 1000 字 + size=300/overlap=50，断言块数与边界 |
| 2 | `internal/rag/chunker/markdown.go` | 按标题层级切，metadata 记 heading_path | 嵌套标题 MD，断言每块的 heading_path 正确 |
| 3 | `pkg/llm/embedder.go` | OpenAI 兼容 Embedding，批量 + 重试 | 集成测试（`//go:build integration`） |
| 4 | `pkg/vectorstore/milvus.go` | 实现 VectorStore 接口 | 集成测试：插 10 条搜 top3 |
| 5 | `internal/pipeline/ingest.go` | 串联：raw → chunk → embed → upsert | 端到端集成测试 |
| 6 | `internal/pipeline/producer.go` + `consumer.go` | Kafka 异步化 | 停机重启不丢消息 |

---

### 步骤 1 代码骨架：`chunker/plain.go`

```go
package chunker

import (
    "context"
    "crypto/sha256"
    "encoding/hex"
    "fmt"

    "github.com/yurin-kami/KahoAgent/internal/rag"
)

// PlainChunker 按固定字符数切分，相邻块保留 overlap 重叠。
//
// 存在意义：这是 Phase 2 评估的对照组（baseline）。
// 我们需要用数据证明「结构化分块比固定分块效果好」，
// 没有 baseline 就没有对比，简历上的「提升 X%」就是空话。
type PlainChunker struct {
    size    int // 每块目标字符数
    overlap int // 相邻块重叠字符数
}

func NewPlainChunker(size, overlap int) *PlainChunker {
    // 防御：overlap 必须小于 size，否则会无限循环
    if overlap >= size {
        overlap = size / 4
    }
    return &PlainChunker{size: size, overlap: overlap}
}

func (c *PlainChunker) Name() string { return "plain" }

func (c *PlainChunker) Chunk(ctx context.Context, docID string, raw []byte) ([]rag.Chunk, error) {
    // ⚠️ 用 []rune 而非 []byte 切分 —— 中文一个字 3 字节，
    //    按 byte 切会切出乱码。这是中文场景最容易踩的坑。
    runes := []rune(string(raw))

    var chunks []rag.Chunk
    step := c.size - c.overlap // 每次前进的距离

    for start, idx := 0, 0; start < len(runes); start, idx = start+step, idx+1 {
        end := start + c.size
        if end > len(runes) {
            end = len(runes)
        }

        chunks = append(chunks, rag.Chunk{
            ID:      chunkID(docID, idx), // 幂等的关键
            DocID:   docID,
            Index:   idx,
            Content: string(runes[start:end]),
            Metadata: map[string]string{
                "chunker": c.Name(), // 评估时用来区分策略
            },
        })

        if end == len(runes) {
            break // 已到末尾，避免最后多切一个空块
        }
    }
    return chunks, nil
}

// chunkID 生成稳定 ID。
// 相同 (docID, index) 永远产生相同 ID，使得重复入库时
// Milvus 的 Upsert 会覆盖而非新增 —— 这就是幂等性的实现。
func chunkID(docID string, index int) string {
    h := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", docID, index)))
    return hex.EncodeToString(h[:])[:32] // 取前 32 位够用，节省存储
}
```

**单测要覆盖的 case**：
```go
// 1. 正常切分：1000 字，size=300，overlap=50 → step=250 → 应得 4 块
// 2. 中文不乱码：输入纯中文，断言每块 utf8.ValidString() == true
// 3. 短文本：输入 100 字，size=300 → 应得 1 块，内容完整
// 4. 幂等性：同样输入调用两次，断言所有 Chunk.ID 相同
// 5. 边界：overlap >= size 时不死循环
```

---

### 步骤 2 代码骨架：`chunker/markdown.go`

```go
package chunker

// MarkdownChunker 按标题层级切分。
//
// 设计目的：技术文档的标题天然是语义边界。
// 「## Redis 主从复制」下面的内容一定都在讲主从复制，
// 按标题切能保证块内语义高度聚焦，检索准确率显著高于固定切分。
//
// 关键增强：把标题路径写进 metadata。
// 检索到某块时，能告诉用户「这段来自 《Redis笔记》> 高可用 > 主从复制」，
// 这就是简历上「答案溯源」那一条的实现基础。
type MarkdownChunker struct {
    maxSize int // 单块上限：标题下内容过长时需二次切分
}

func (c *MarkdownChunker) Chunk(ctx context.Context, docID string, raw []byte) ([]rag.Chunk, error) {
    // 实现要点：
    //
    // 1. 逐行扫描，用正则识别 ATX 标题：^(#{1,6})\s+(.+)$
    //
    // 2. 维护一个标题栈 headingStack []string，遇到 N 级标题时：
    //    - 截断栈到 N-1 层
    //    - push 当前标题
    //    这样任何时刻 strings.Join(headingStack, " > ") 就是完整路径
    //
    // 3. 遇到新标题时，把累积的内容作为一个 Chunk 输出，
    //    metadata["heading_path"] = 当前路径
    //
    // 4. 若某标题下内容超过 maxSize，退化为调用 PlainChunker 二次切分，
    //    但保留 heading_path metadata（复用而非重写 —— 这是接口设计的价值）
    //
    // 5. ⚠️ 坑：代码块 ``` 内部的 # 不是标题，扫描时要跟踪代码块状态
}
```

**单测要覆盖的 case**：
```go
// 1. 单层标题：# A / 内容 / # B / 内容 → 2 块，heading_path 分别是 "A" "B"
// 2. 嵌套标题：# A / ## A1 / 内容 → heading_path == "A > A1"
// 3. 层级回退：# A / ## A1 / # B → B 的 path 是 "B" 而非 "A > A1 > B"
// 4. 代码块内的 # 不当标题：```\n# not a heading\n``` → 不切分
// 5. 超长内容降级：某标题下 10000 字 → 应切成多块，但都带同一 heading_path
```

---

### 步骤 3 代码骨架：`pkg/llm/embedder.go`

```go
package llm

// OpenAIEmbedder 调用 OpenAI 兼容的 Embedding API。
//
// 三个必须实现的工程点（L1.3 学过为什么）：
//   1. 批量：一次请求发多条文本，避免 N 次 HTTP 往返
//   2. 重试：指数退避，容忍网络抖动
//   3. 顺序保证：返回的向量顺序必须与输入 texts 一致
type OpenAIEmbedder struct {
    apiKey    string
    baseURL   string
    model     string
    dim       int
    batchSize int           // 单次请求最大条数，默认 64
    client    *http.Client
}

func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
    // 实现要点：
    //
    // 1. 按 batchSize 分批。注意：分批后要按原顺序拼回结果，
    //    不能用 goroutine 并发后随机 append —— 顺序错了向量就对不上文本了。
    //    若要并发，用 index 定位写入预分配的 result 切片。
    //
    // 2. 每批调用 embedBatch，失败时指数退避重试：
    //      for attempt := 0; attempt < 3; attempt++ {
    //          resp, err := e.embedBatch(ctx, batch)
    //          if err == nil { break }
    //          // 429 / 5xx 才重试，4xx 直接返回（重试也没用）
    //          time.Sleep(time.Duration(1<<attempt) * time.Second)
    //      }
    //
    // 3. API 返回的是 float64，转成 float32。
    //    ⚠️ 精度损失可忽略：向量检索对小数点后 7 位不敏感，
    //       但内存能省一半，Milvus 也只接受 float32。
    //
    // 4. 校验返回维度 == e.dim，不一致直接报错（防止模型配错）
}
```

---

### 1.3 Milvus Schema（**定死，不准改**）

改 schema 意味着重建索引 + 重跑全量数据，代价极高。现在定好。

| 字段 | 类型 | 约束 | 目的 |
|------|------|------|------|
| `id` | VarChar(64) | PrimaryKey | 幂等的载体，值是 sha256 前 32 位 |
| `doc_id` | VarChar(64) | - | 支持 `DeleteByDoc` 按文档删除 |
| `chunk_index` | Int64 | - | 排序、定位原文位置 |
| `content` | VarChar(8192) | - | 检索命中后直接返回，避免二次查库 |
| `metadata` | JSON | - | heading_path / chunker / file_type |
| `vector` | FloatVector(1536) | - | `text-embedding-3-small` 的维度 |

**索引**：
```
HNSW on vector
  metric_type:    COSINE     # 文本向量用余弦（L0.2 学过）
  M:              16         # 每节点连接数
  efConstruction: 200        # 建索引搜索宽度
搜索参数：
  ef:             64         # 必须 ≥ topK
```

**为什么 content 也存在 Milvus 里**（而非只存 ID 再去 MySQL 查）：
- 优点：检索一次就拿到全部数据，少一次 IO
- 代价：存储冗余
- 权衡：个人项目数据量小，用空间换延迟是对的。数据量上 TB 时才需要拆分。

---

### 步骤 4 代码骨架：`pkg/vectorstore/milvus.go`

```go
package vectorstore

// MilvusStore 实现 rag.VectorStore 接口。
//
// 编译期断言：如果接口没实现全，这行会编译报错。
// 这是 Go 里保证接口实现的标准做法，比运行时才发现好得多。
var _ rag.VectorStore = (*MilvusStore)(nil)

type MilvusStore struct {
    cli      client.Client
    collName string
    dim      int
}

// EnsureCollection 幂等地创建 collection 与索引。
//
// 目的：让服务启动时自动建表，不需要手动初始化脚本。
// 已存在时直接返回，可重复调用。
func (s *MilvusStore) EnsureCollection(ctx context.Context) error {
    // 1. HasCollection 检查是否已存在，存在则跳过创建
    // 2. 不存在则 CreateCollection（schema 见 1.3）
    // 3. CreateIndex：entity.NewIndexHNSW(entity.COSINE, 16, 200)
    // 4. LoadCollection：⚠️ 不 Load 无法搜索（Phase 0 踩过的坑）
}

func (s *MilvusStore) Upsert(ctx context.Context, chunks []rag.Chunk, vectors [][]float32) error {
    // 实现要点：
    //
    // 1. 前置校验：len(chunks) == len(vectors)，不等直接报错
    //    （这类 bug 不校验的话会写入错位数据，极难排查）
    //
    // 2. Milvus 是列式插入 —— 把 []Chunk 转置成各字段的列：
    //      ids := make([]string, len(chunks))
    //      docIDs := make([]string, len(chunks))
    //      ...
    //    然后用 entity.NewColumnVarChar("id", ids) 等构造列
    //
    // 3. metadata 是 JSON 字段，需要 json.Marshal 成 [][]byte，
    //    用 entity.NewColumnJSONBytes 构造
    //
    // 4. 调用 cli.Upsert(ctx, collName, "", columns...)
    //    用 Upsert 而非 Insert —— 这是幂等的另一半
    //    （ID 稳定 + Upsert 覆盖 = 重复上传不产生脏数据）
    //
    // 5. ⚠️ Flush：Upsert 后数据在内存 buffer，不 Flush 搜不到。
    //    但每次都 Flush 性能差 —— 折中方案：批量入库完成后统一 Flush 一次。
}

func (s *MilvusStore) Search(ctx context.Context, vector []float32, topK int) ([]rag.ScoredChunk, error) {
    // 实现要点：
    //
    // 1. sp, _ := entity.NewIndexHNSWSearchParam(64)  // ef=64
    //
    // 2. cli.Search 参数顺序（已验证 v2.4.2）：
    //      ctx, collName, partitions(nil), expr(""),
    //      outputFields, vectors, vectorField, metricType, topK, sp
    //
    // 3. outputFields 要包含所有需要的字段：
    //      []string{"id", "doc_id", "chunk_index", "content", "metadata"}
    //
    // 4. 解析结果：results[0].Fields.GetColumn("content").(*entity.ColumnVarChar)
    //    然后 ValueByIdx(i) 取值。⚠️ 类型断言失败会 panic，要用 comma-ok。
    //
    // 5. Scores 是 []float32，与结果一一对应。COSINE 下越大越相似。
}
```

---

### 步骤 5：`internal/pipeline/ingest.go`

```go
package pipeline

// Ingester 是文档入库的编排器。
//
// 设计意图：把 chunk → embed → upsert 三步串起来，
// 但自己不实现任何一步 —— 全部依赖注入的接口。
// 这样：
//   - 单测可以注入 mock，不依赖外部服务
//   - Phase 2 换分块策略只需换注入的 Chunker
//   - Kafka consumer 只是调用它的一层壳，不重复写逻辑
type Ingester struct {
    chunker  rag.Chunker
    embedder rag.Embedder
    store    rag.VectorStore
}

func (i *Ingester) Run(ctx context.Context, docID string, raw []byte) (int, error) {
    // 1. 分块
    chunks, err := i.chunker.Chunk(ctx, docID, raw)
    // 空文档直接返回 0，不算错误
    if len(chunks) == 0 { return 0, nil }

    // 2. 提取文本批量向量化
    texts := make([]string, len(chunks))
    for idx, c := range chunks { texts[idx] = c.Content }
    vectors, err := i.embedder.Embed(ctx, texts)

    // 3. 校验：向量数必须等于块数（防御 Embedder 实现有 bug）
    if len(vectors) != len(chunks) {
        return 0, fmt.Errorf("vector count %d != chunk count %d", len(vectors), len(chunks))
    }

    // 4. 写入
    if err := i.store.Upsert(ctx, chunks, vectors); err != nil { return 0, err }
    return len(chunks), nil
}
```

---

### 1.4 Kafka 异步化（**本 Phase 最后一步**）

**前置条件**：同步链路（步骤 1-5）必须全部跑通。

```go
// internal/pipeline/consumer.go

// Consumer 消费文档处理消息。
//
// ⚠️ 核心约束：consumer 只是 Ingester.Run 的一层壳。
//    不准在这里写任何业务逻辑 —— 一旦写了，同步和异步两条路径
//    的行为就会分叉，测试覆盖不到，线上必出诡异 bug。
func (c *Consumer) Start(ctx context.Context) error {
    for {
        // ⚠️ 用 FetchMessage 而非 ReadMessage
        //    ReadMessage 会自动提交 offset，处理失败消息就丢了。
        //    FetchMessage + 手动 Commit = 至少一次语义（L1.4 学过）
        msg, err := c.reader.FetchMessage(ctx)
        if err != nil { return err }

        var task IngestTask
        if err := json.Unmarshal(msg.Value, &task); err != nil {
            // 反序列化失败是脏消息，重试也没用 —— 记日志后跳过并提交 offset，
            // 否则会卡在这条消息上无限重试（毒丸消息问题）
            c.logger.Error("bad message", "err", err)
            _ = c.reader.CommitMessages(ctx, msg)
            continue
        }

        // 委托给 Ingester，不在这里写逻辑
        if _, err := c.ingester.Run(ctx, task.DocID, task.Raw); err != nil {
            c.logger.Error("ingest failed", "doc", task.DocID, "err", err)
            continue // 不提交 offset → 重启后会重新消费
        }

        // 处理成功才提交
        if err := c.reader.CommitMessages(ctx, msg); err != nil {
            c.logger.Error("commit failed", "err", err)
        }
    }
}
```

---

## ✅ Phase 1 验收

```bash
go test ./internal/rag/... -v                          # 单测全绿
go test -tags=integration ./... -v                     # 集成测试全绿
go run scripts/eval/ingest_check.go docs/sample.md     # 手动验证
```

- [ ] 上传 Markdown → Milvus 能查到对应 chunk（`doc_id` 匹配）
- [ ] **幂等验证**：同文件上传两次，chunk 总数不变
- [ ] **删除验证**：`DeleteByDoc` 后该文档 chunk 全消失
- [ ] **中文验证**：纯中文文档分块后无乱码
- [ ] **heading_path 验证**：嵌套标题文档，metadata 里路径正确
- [ ] **Kafka 可靠性**：处理中途 kill consumer，重启后消息被重新消费
- [ ] `git tag phase1-done`

## 🚫 Phase 1 不做

- 不写检索排序逻辑（Phase 2）
- 不接 LLM 对话（Phase 3）
- 不做 PDF 解析（只支持 md/txt/go，PDF 是 Phase 6 可选）
- 不做前端
- 不做递归分割、语义分割（有 plain + markdown 两个够对比了）

---

# Phase 2：混合检索 + ReRank

**目标**：Query → 双路召回 → RRF 融合 → ReRank → Top-K。**产出可量化的评估数据。**
**分支**：`phase2-retrieval` · **预计**：4-5 天

---

## 📚 本阶段学习内容

### L2.1 为什么单靠向量检索不够（核心认知）

| 检索方式 | 擅长 | 不擅长 |
|---------|------|--------|
| **向量检索（Dense）** | 语义相似：「怎么防止缓存被击穿」能匹配「布隆过滤器的应用」 | 精确匹配：搜「RedisJSON」可能返回一堆讲 Redis 的无关内容 |
| **BM25（Sparse）** | 关键词精确匹配：专有名词、代码符号、版本号 | 同义词：搜「登录」匹配不到「认证」 |

**结论**：两者互补，所以要混合。这是工业界 RAG 的标配做法。

**面试可以聊的点**：为什么不训练一个更好的 embedding 模型来同时解决两个问题？答：成本极高且收益不确定，混合检索是性价比最优解。

### L2.2 BM25 原理（够用即可）

BM25 是改进版的 TF-IDF，核心思想：
- **TF（词频）**：一个词在文档中出现越多，文档越相关 —— 但有饱和上限（出现 100 次不比 10 次相关 10 倍）
- **IDF（逆文档频率）**：一个词在整个语料中越罕见，它越有区分度（「的」没区分度，「布隆过滤器」有）
- **文档长度归一化**：长文档天然包含更多词，要惩罚

**PostgreSQL 实现**：
```sql
-- to_tsvector 把文本转成词元向量
-- ts_rank 计算相关性分数（PG 用的是类 BM25 的算法）
SELECT id, ts_rank(content_tsv, query) AS score
FROM chunks, to_tsquery('simple', 'redis & 缓存') query
WHERE content_tsv @@ query
ORDER BY score DESC LIMIT 20;
```

**⚠️ 中文的坑**：PG 默认分词器不支持中文，「Redis缓存穿透」会被当成一个词。两种解法：
1. 装 `pg_jieba` 扩展（效果好，但要编译安装）
2. 入库时在 Go 侧用 `gojieba` 分好词，存成空格分隔的字符串，PG 用 `simple` 分词器（**本项目用这个，避免 Docker 镜像折腾**）

### L2.3 RRF 融合算法（必须理解）

**问题**：向量检索返回一个排序，BM25 返回另一个排序，两者的分数量纲完全不同（余弦相似度 0~1，BM25 分数 0~几十），不能直接相加。

**RRF（Reciprocal Rank Fusion）的巧妙之处**：**只用排名，不用分数**。

```
score(d) = Σ  1 / (k + rank_i(d))
          i∈检索器
```

- `rank_i(d)`：文档 d 在第 i 个检索器结果中的排名（从 1 开始）
- `k`：平滑常数，论文推荐 60

**为什么有效**：
- 排名第 1 得 1/61 ≈ 0.0164，排名第 10 得 1/70 ≈ 0.0143，差距不大 → 容忍单个检索器的排序噪声
- 但在**两个检索器中都排前面**的文档，累加后分数明显更高 → 双重印证的结果被提升

**举例**：
```
向量检索: [A, B, C]     BM25: [C, A, D]
A: 1/(60+1) + 1/(60+2) = 0.0164 + 0.0161 = 0.0325  ← 两边都靠前，最高
C: 1/(60+3) + 1/(60+1) = 0.0159 + 0.0164 = 0.0323
B: 1/(60+2)            = 0.0161
D: 1/(60+3)            = 0.0159
```

### L2.4 评估指标（必学，简历数字的来源）

| 指标 | 公式 | 含义 | 什么时候用 |
|------|------|------|-----------|
| **Recall@K** | 前 K 个结果中相关文档数 / 全部相关文档数 | 「该找的找到了多少」 | 关心覆盖率 |
| **MRR** | 第一个正确结果排名的倒数，对所有 query 取平均 | 「正确答案排得多靠前」 | 关心首位准确性 |
| **NDCG@K** | 考虑排序位置的加权得分 | 最全面但计算复杂 | 本项目不做 |

**MRR 举例**：
```
Query1: 正确答案排第 1 → RR = 1/1 = 1.0
Query2: 正确答案排第 3 → RR = 1/3 = 0.33
Query3: 前 10 都没有   → RR = 0
MRR = (1.0 + 0.33 + 0) / 3 = 0.44
```

**为什么 RAG 场景 MRR 比 Recall 更重要**：LLM 通常只看 top 3-5 个块，排第 20 的正确答案等于没有。

### L2.5 ReRank 的作用

**流程**：召回 50 条（快但粗）→ 精排取前 5（慢但准）

**为什么要两阶段**：直接用精排模型算 10 万条太慢；只用向量检索取 top5 又不够准。先粗筛缩小范围，再精排。

**两种实现**：
| 方式 | 原理 | 成本 | 本项目 |
|------|------|------|--------|
| Cross-Encoder | 把 query 和 doc 拼在一起过模型，直接输出相关性分 | 需部署模型 | ❌ |
| **LLM pointwise** | 让 LLM 给每个候选打 0-10 分 | 一次 API 调用 | ✅ 简单可行 |

---

## 🔨 实现内容

### 2.1 评估集先行（**第一天必须做完**）

**为什么必须先做**：没有评估集，你所有的「优化」都是凭感觉。改了参数觉得「好像好点了」—— 这不是工程，是玄学。

`scripts/eval/golden_set.json`：
```json
[
  {
    "query": "Redis 缓存穿透怎么解决",
    "relevant_chunk_ids": ["<chunk_id_1>", "<chunk_id_2>"],
    "note": "布隆过滤器 + 空值缓存"
  }
]
```

**规模**：至少 50 条。**怎么标**：
1. 先把自己的技术笔记入库（Phase 1 的能力）
2. 针对文档内容写 50 个你会问的问题
3. 手工检查哪些 chunk 确实回答了这个问题，记下 ID

耗时约 1.5-2 小时，**这是整个项目最值的两小时**。

---

### 2.2 评估框架：`scripts/eval/eval.go`

```go
// 目的：对比不同检索策略的效果，产出简历上的数据。
//
// 输出格式（这张表直接进简历）：
//   Method              Recall@5   MRR      P99(ms)
//   ------------------------------------------------
//   vector-only         0.41       0.48     28
//   bm25-only           0.35       0.39     15
//   hybrid-rrf          0.58       0.64     42
//   hybrid-rrf+rerank   0.63       0.72     180

// recallAtK 计算前 K 个结果中命中的相关文档比例
func recallAtK(retrieved []string, relevant map[string]bool, k int) float64 {
    if len(relevant) == 0 { return 0 }
    hits := 0
    for i, id := range retrieved {
        if i >= k { break }
        if relevant[id] { hits++ }
    }
    return float64(hits) / float64(len(relevant))
}

// reciprocalRank 返回第一个命中的排名倒数
func reciprocalRank(retrieved []string, relevant map[string]bool) float64 {
    for i, id := range retrieved {
        if relevant[id] {
            return 1.0 / float64(i+1)
        }
    }
    return 0
}
```

**先用假数据验证公式正确性**，再接真实检索器 —— 否则指标算错了你还以为是检索差。

---

### 2.3 实现顺序

| 步 | 文件 | 内容 | 验收 |
|----|------|------|------|
| 1 | `scripts/eval/eval.go` | 评估框架 | 假数据验证公式对 |
| 2 | `internal/rag/retriever/vector.go` | Milvus 向量召回 | eval 跑出 baseline 行 |
| 3 | `internal/rag/retriever/bm25.go` | PG 全文检索 | eval 跑出 bm25 行 |
| 4 | `internal/rag/retriever/hybrid.go` | RRF 融合 | **分数高于两条单路**（低于说明有 bug） |
| 5 | `pkg/cache/query.go` | Redis 缓存 | 二次请求 <10ms |
| 6 | `internal/rag/retriever/rerank.go` | LLM 精排 | MRR 再提升 |

---

### RRF 实现（**参数定死**）

```go
package retriever

// rrfK 是 RRF 的平滑常数。
// 60 是原论文（Cormack et al., 2009）的推荐值，社区广泛验证。
// ⚠️ 不准随意调整 —— 调了必须在评估集上验证确实更好，
//    并把对比数据记进 docs/eval-results.md。
const rrfK = 60

// fuseRRF 融合多路检索结果。
//
// 输入：每个检索器返回的 chunk ID 有序列表（已按各自相关性排序）
// 输出：chunk ID → 融合分数
//
// 核心思想（L2.3 学过）：只用排名不用分数，
// 天然解决了不同检索器分数量纲不一致的问题。
func fuseRRF(rankings [][]string) map[string]float64 {
    scores := make(map[string]float64)
    for _, ranking := range rankings {
        for rank, id := range ranking {
            // rank 从 0 开始，公式里排名从 1 开始，故 +1
            scores[id] += 1.0 / float64(rrfK+rank+1)
        }
    }
    return scores
}
```

---

### HybridRetriever（对齐 eino 接口）

```go
package retriever

import (
    einoretriever "github.com/cloudwego/eino/components/retriever"
    "github.com/cloudwego/eino/schema"
)

// 编译期断言：确保实现了 eino 的接口。
// 这是 Phase 3 能把自研检索器塞进 Eino Graph 的前提。
//
// eino 接口签名（已验证 v0.9.14）：
//   Retrieve(ctx context.Context, query string, opts ...Option) ([]*schema.Document, error)
var _ einoretriever.Retriever = (*HybridRetriever)(nil)

type HybridRetriever struct {
    vector   *VectorRetriever
    bm25     *BM25Retriever
    reranker *Reranker    // 可为 nil，nil 时跳过精排
    cache    *cache.QueryCache
    recallN  int          // 每路召回数量，默认 50
    topK     int          // 最终返回数量，默认 5
}

func (r *HybridRetriever) Retrieve(ctx context.Context, query string, opts ...einoretriever.Option) ([]*schema.Document, error) {
    // 实现要点：
    //
    // 1. 查缓存：key = sha256(query + 策略参数)
    //    ⚠️ key 必须包含参数 —— 否则改了 topK 还命中旧缓存
    //
    // 2. 双路并发召回（用 errgroup，两路互不依赖）：
    //      g, gctx := errgroup.WithContext(ctx)
    //      var vecIDs, bm25IDs []string
    //      g.Go(func() error { vecIDs, err = r.vector.Recall(gctx, query, r.recallN); return err })
    //      g.Go(func() error { bm25IDs, err = r.bm25.Recall(gctx, query, r.recallN); return err })
    //      if err := g.Wait(); err != nil { ... }
    //
    //    ⚠️ 降级策略：某一路失败不应导致整体失败。
    //       BM25 挂了还有向量结果，反之亦然。用 errgroup 时要注意
    //       它会在首个错误时取消 context —— 这里更适合各自 recover 并记录。
    //
    // 3. RRF 融合 → 按分数排序
    //
    // 4. 若配置了 reranker，取前 20 送去精排，返回 topK
    //
    // 5. 转成 *schema.Document（eino 的类型）：
    //      &schema.Document{
    //          ID:       chunk.ID,
    //          Content:  chunk.Content,
    //          MetaData: map[string]any{"heading_path": ..., "score": ...},
    //      }
    //
    // 6. 写缓存，TTL 1 小时
}
```

---

### ReRank 实现要点

```go
// Reranker 用 LLM 对候选块做 pointwise 打分。
//
// 为什么用 pointwise 而非 listwise：
//   - pointwise：每块单独打分，可并发，prompt 短
//   - listwise：所有块一起排序，prompt 长易超限，且模型对长列表排序不稳定
type Reranker struct {
    model     llm.ChatModel
    batchSize int  // 单次 prompt 里放几个候选，默认 5
}

func (r *Reranker) Rerank(ctx context.Context, query string, candidates []rag.ScoredChunk, topK int) ([]rag.ScoredChunk, error) {
    // 实现要点：
    //
    // 1. Prompt 设计（关键）：
    //      "判断以下文档片段与问题的相关性，输出 0-10 的整数分数。
    //       只输出数字，不要解释。
    //       问题：{query}
    //       片段：{content}"
    //
    //    ⚠️ "只输出数字" 必须强调，否则模型会写一堆分析文字
    //
    // 2. 用 temperature=0 保证打分稳定（同样输入同样输出）
    //
    // 3. 解析失败的降级：模型输出了非数字 → 用原 RRF 分数，不要整体失败
    //
    // 4. 并发控制：候选 20 个不能开 20 个 goroutine 打 API，
    //    用 semaphore 限制并发 5
    //
    // 5. 成本意识：rerank 会让延迟从 40ms 涨到 150ms+，
    //    要在评估表里体现这个 trade-off
}
```

---

## ✅ Phase 2 验收

```bash
go run scripts/eval/eval.go --golden scripts/eval/golden_set.json
```

- [ ] 输出四行对比表，`hybrid-rrf` 的 MRR **明显高于** `vector-only`
      （若不高于，说明 RRF 实现有 bug 或 BM25 召回质量太差，必须排查）
- [ ] `var _ einoretriever.Retriever = (*HybridRetriever)(nil)` 编译通过
- [ ] Redis 缓存命中后延迟 <10ms
- [ ] **降级验证**：手动停掉 PG，检索仍能返回结果（只走向量路）
- [ ] 评估结果存 `docs/eval-results.md`，带日期和参数配置
- [ ] `git tag phase2-done`

## 🚫 Phase 2 不做

- 不训练/微调 Embedding 模型
- 不做 Query 改写和 HyDE（Phase 6 可选）
- 不做多轮对话上下文（Phase 3）
- 不做 NDCG 等复杂指标（Recall + MRR 够用）

---

# Phase 3：Agent 编排

**目标**：Eino ReAct Agent + Tool 调用 + SSE 流式输出。
**分支**：`phase3-agent` · **预计**：5-7 天

---

## 📚 本阶段学习内容

### L3.1 ReAct 范式（核心）

**ReAct = Reasoning + Acting**，让模型在「思考」和「行动」之间循环：

```
用户："帮我找关于 Redis 的文档，然后告诉我第一个文件多大"

Thought: 我需要先搜索 Redis 相关文档
Action:  rag_query(query="Redis")
Observation: 找到 3 个文件：redis-notes.md, cache.md, ...

Thought: 现在需要查第一个文件的大小
Action:  file_metadata(file_id="redis-notes.md")
Observation: {size: 24KB, modified: 2026-08-01}

Thought: 信息够了，可以回答
Answer:  找到了 redis-notes.md，大小 24KB...
```

**关键认知**：模型不是一次性回答，而是多轮「决定调什么工具 → 看结果 → 再决定」。这个循环的状态管理就是 Agent 框架的核心价值。

**必须设置 MaxIterations**：模型可能陷入死循环（反复调同一个工具）。本项目设 15。

### L3.2 Tool Description 的重要性（**最容易忽视的点**）

模型靠什么决定调哪个工具？**只有 Tool 的 Name 和 Description**。

```go
// ❌ 差的描述 —— 模型不知道什么时候该用
Description: "搜索文件"

// ✅ 好的描述 —— 说清场景、输入、输出
Description: "在用户的云盘中语义搜索文档内容。当用户询问某个技术主题、" +
             "想找相关资料、或需要基于文档内容回答问题时使用。" +
             "输入自然语言查询，返回最相关的文档片段及其来源文件名。"
```

**经验法则**：Description 要能回答「什么情况下该用我」，而不只是「我是干什么的」。

**调试技巧**：如果 Agent 不调用某个工具，99% 是 Description 写得不够明确，不是模型笨。

### L3.3 上下文窗口管理（工程必修）

**问题**：对话越长，历史消息越多，Token 消耗指数增长，最终超出模型上下文限制。

| 策略 | 做法 | 优缺点 |
|------|------|--------|
| 全量保留 | 什么都不做 | 简单，但很快爆炸 |
| **滑动窗口** | 只保留最近 N 轮 | 简单有效，但丢失早期信息 |
| **窗口 + 摘要** | 保留最近 N 轮 + 更早的用 LLM 压缩成摘要 | 平衡，本项目用这个 |
| 向量检索历史 | 把历史存向量库，按相关性召回 | 最优但复杂 |

**本项目规则（定死）**：
```
最大历史轮数：10 轮
超出后：保留最近 6 轮完整对话 + 前面的压缩成一段摘要
单次请求 Token 上限：8000（硬截断兜底）
```

### L3.4 SSE 流式输出（用户体验关键）

**为什么要流式**：LLM 生成 500 字要 10 秒。不流式的话用户盯着空白等 10 秒；流式的话第一个字 0.8 秒就出来了，感知延迟差 10 倍。

**SSE vs WebSocket**：
| | SSE | WebSocket |
|---|-----|-----------|
| 方向 | 单向（服务端→客户端） | 双向 |
| 协议 | 基于 HTTP，无需升级 | 需要协议升级 |
| 重连 | 浏览器自动重连 | 要自己实现 |
| 适用 | **LLM 流式输出** ✅ | 聊天室、协作编辑 |

**Go 实现要点**：
```go
w.Header().Set("Content-Type", "text/event-stream")
w.Header().Set("Cache-Control", "no-cache")
w.Header().Set("Connection", "keep-alive")

flusher, ok := w.(http.Flusher)  // ⚠️ 必须 Flush，否则数据积在 buffer 里
if !ok { /* 不支持流式 */ }

for token := range tokenChan {
    fmt.Fprintf(w, "data: %s\n\n", token)  // SSE 格式：data: xxx\n\n
    flusher.Flush()                         // 立即推给客户端
}
```

---

## 🔨 实现内容

### 3.1 已验证的 Eino API（`eino@v0.9.14`）

```go
import (
    "github.com/cloudwego/eino/adk"
    "github.com/cloudwego/eino/compose"
    "github.com/cloudwego/eino/components/tool"
)

// ChatModelAgentConfig 实为 TypedChatModelAgentConfig[*schema.Message] 的别名
agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
    Name:        "KahoAgent",
    Description: "云盘智能文件助手",
    Instruction: systemPrompt,   // 支持 {Key} 占位符注入 session 值
    Model:       chatModel,      // model.BaseModel[*schema.Message]
    ToolsConfig: adk.ToolsConfig{
        ToolsNodeConfig: compose.ToolsNodeConfig{
            Tools: []tool.BaseTool{ragTool, searchTool, metaTool, shareTool},
        },
    },
    MaxIterations: 15,           // 默认 20，防死循环（L3.1）
})

// Runner 负责执行与事件流
runner := adk.NewRunner(ctx, adk.RunnerConfig{
    Agent:           agent,
    EnableStreaming: true,       // 开启后可逐 token 消费
})

iter, err := runner.Query(ctx, "帮我找关于 Redis 的文档")
// iter 是 *adk.AsyncIterator[*adk.AgentEvent]
```

### 3.2 Tool 接口（已验证）

```go
// eino 的 tool 接口
type BaseTool interface {
    Info(ctx context.Context) (*schema.ToolInfo, error)
}

type InvokableTool interface {
    BaseTool
    InvokableRun(ctx context.Context, argumentsInJSON string, opts ...Option) (string, error)
}
```

**注意 `InvokableRun` 收的是 JSON 字符串**，需要自己反序列化到参数结构体。

### 3.3 Tool 清单（**只做这 4 个**）

| Tool | 输入 | 输出 | 依赖 |
|------|------|------|------|
| `rag_query` | `{query, top_k}` | 文档片段 + 来源文件名 | Phase 2 的 HybridRetriever |
| `file_search` | `{keyword, limit}` | 文件列表 | CloudKaho API |
| `file_metadata` | `{file_id}` | 文件详情 | CloudKaho API |
| `file_share` | `{file_id, expire_hours}` | 预签名 URL | CloudKaho API |

**`file_upload` 不做成 Tool** —— 上传是大流量二进制操作，走 HTTP 接口更合适，塞进 Agent 循环会让 prompt 爆炸。

---

### rag_query Tool 骨架

```go
package tools

// RAGQueryTool 让 Agent 能语义搜索文档。
//
// 这是本项目最核心的 Tool —— 它把 Phase 2 的检索能力
// 暴露给 LLM，是 RAG 中「R」与「G」的连接点。
type RAGQueryTool struct {
    retriever *retriever.HybridRetriever
}

var _ tool.InvokableTool = (*RAGQueryTool)(nil)

func (t *RAGQueryTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
    return &schema.ToolInfo{
        Name: "rag_query",
        // ⚠️ Description 决定模型会不会调用它（L3.2）
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
                Desc: "返回结果数量，默认 5，最大 20",
            },
        }),
    }, nil
}

func (t *RAGQueryTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
    var args struct {
        Query string `json:"query"`
        TopK  int    `json:"top_k"`
    }
    if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
        // 返回错误信息给模型，让它有机会修正参数
        return "", fmt.Errorf("参数解析失败: %w", err)
    }

    // 参数兜底与范围限制（防止模型传离谱的值）
    if args.TopK <= 0 { args.TopK = 5 }
    if args.TopK > 20 { args.TopK = 20 }

    docs, err := t.retriever.Retrieve(ctx, args.Query)
    if err != nil {
        return "", err
    }

    // ⚠️ 返回格式很重要：
    //   - 用结构化文本而非原始 JSON —— 模型读文本比读 JSON 准
    //   - 必须带来源文件名 —— 这是「答案溯源」功能的基础
    //   - 内容要截断 —— 每块限制 500 字，否则 5 个块就撑爆上下文
    var sb strings.Builder
    for i, d := range docs {
        source := d.MetaData["heading_path"]
        content := truncate(d.Content, 500)
        fmt.Fprintf(&sb, "【片段 %d】来源: %v\n%s\n\n", i+1, source, content)
    }
    if sb.Len() == 0 {
        return "未找到相关文档。", nil  // 明确告诉模型没找到，而非返回空串
    }
    return sb.String(), nil
}
```

---

### 3.4 实现顺序

| 步 | 文件 | 验收 |
|----|------|------|
| 1 | `pkg/llm/chatmodel.go` | Eino ChatModel 适配，能对话 |
| 2 | `internal/agent/tools/rag_query.go` | 单独调用能返回检索结果 |
| 3 | `internal/agent/agent.go` | Agent 能调用 rag_query 并回答 |
| 4 | `internal/agent/memory.go` | 多轮对话不丢上下文 |
| 5 | `internal/agent/tools/file_*.go` | 剩下 3 个 Tool |
| 6 | `internal/handler/chat.go` | SSE 流式接口 |

---

### Memory 实现要点

```go
// SessionMemory 管理单个会话的对话历史。
//
// 存储：Redis，key = "session:{sessionID}"，TTL 24h
// 结构：List，每个元素是一条 JSON 序列化的 message
type SessionMemory struct {
    rdb        *redis.Client
    maxTurns   int  // 10
    keepTurns  int  // 6：超限后保留的完整轮数
    summarizer llm.ChatModel
}

func (m *SessionMemory) Load(ctx context.Context, sessionID string) ([]*schema.Message, error) {
    // 1. LRANGE 取全部历史
    // 2. 若轮数 <= maxTurns，直接返回
    // 3. 若超限：
    //      - 取前 (总数 - keepTurns) 轮，调 LLM 压缩成一段摘要
    //      - 返回 [摘要消息] + 最近 keepTurns 轮
    //      - ⚠️ 把摘要写回 Redis，避免每次都重新压缩（省钱）
    //
    // 4. 兜底：估算总 token 数，若仍超 8000，从最早的开始丢
}
```

---

## ✅ Phase 3 验收

手动测试（`scripts/e2e/chat_test.sh`）：

- [ ] 「帮我找关于 Redis 缓存的文档」→ 调用 `rag_query` → 返回答案 + 引用来源
- [ ] 「第二个文件多大」→ 调用 `file_metadata`（**证明多轮上下文生效**）
- [ ] 「给我一个分享链接」→ 调用 `file_share` → 返回 URL
- [ ] 连续 12 轮对话不 OOM、不丢上下文（截断+摘要生效）
- [ ] SSE 逐 token 输出，首字延迟 <1.5s
- [ ] **异常处理**：故意让 Tool 报错，Agent 能优雅回复而非崩溃
- [ ] `git tag phase3-done`

## 🚫 Phase 3 不做

- 不做 Multi-Agent（单 Agent 够用，多 Agent 是过度设计）
- 不做前端页面（curl / Postman 验证即可）
- 不接 MCP（Phase 4）
- 不做 Plan-and-Execute 等高级范式（ReAct 够用）

---

# Phase 4：MCP 协议暴露

**目标**：Phase 3 的 Tool 通过 MCP 给 Claude Desktop / Cursor 用。
**分支**：`phase4-mcp` · **预计**：3-4 天

---

## 📚 本阶段学习内容

### L4.1 MCP 在架构中的位置

```
        ┌─────────────────┐
        │ Claude Desktop  │  ← 别人的 AI 客户端
        └────────┬────────┘
                 │ MCP (JSON-RPC)
        ┌────────▼────────┐
        │  MCP Server     │  ← 我们写的（薄适配层）
        ├─────────────────┤
        │  Tool 实现       │  ← Phase 3 已写好，复用
        ├─────────────────┤
        │  RAG / CloudKaho │  ← Phase 1-2 已写好
        └─────────────────┘
```

**核心认知**：MCP 层**不包含任何业务逻辑**，它只做协议转换。业务逻辑在 Phase 3 的 Tool 里。

**判断标准**：如果你在 MCP handler 里写了 `if`/`else` 业务判断，说明设计错了 —— 应该回去改 Phase 3 的 Tool。

### L4.2 MCP 的安全模型（简历亮点）

MCP Server 把能力暴露给外部 AI，安全边界必须自己守：

| 风险 | 场景 | 防护 |
|------|------|------|
| **资源耗尽** | 恶意 prompt 让模型疯狂调工具 | 并发信号量 + 单次超时 |
| **参数越界** | `top_k=1000000` | 业务范围校验 |
| **越权访问** | 访问别人的文件 | API Key 绑定用户身份 |
| **审计缺失** | 出事查不到谁调的 | 结构化审计日志 |

**面试可以聊的点**：这本质上是「把内部 API 暴露给不可信客户端」的经典问题，和写开放平台 API 是同一套方法论。

---

## 🔨 实现内容

### 4.1 已验证的 MCP API（`go-sdk@v1.7.0`）

```go
server := mcp.NewServer(&mcp.Implementation{
    Name:    "kahoagent",
    Version: "0.1.0",
}, nil)

type ragQueryArgs struct {
    Query string `json:"query" jsonschema:"搜索关键词，使用自然语言"`
    TopK  int    `json:"top_k,omitempty" jsonschema:"返回结果数，默认5，最大20"`
}

// 泛型版 AddTool 会自动从结构体生成 JSON Schema 并校验入参
mcp.AddTool(server, &mcp.Tool{
    Name:        "rag_query",
    Description: "语义搜索云盘中的文档内容",
}, func(ctx context.Context, req *mcp.CallToolRequest, in ragQueryArgs) (*mcp.CallToolResult, any, error) {
    // ⚠️ 只做转换与调用，不写业务逻辑（L4.1）
    result, err := ragTool.InvokableRun(ctx, mustJSON(in))
    if err != nil {
        return nil, nil, err
    }
    return &mcp.CallToolResult{
        Content: []mcp.Content{&mcp.TextContent{Text: result}},
    }, nil, nil
})

// stdio 模式（Claude Desktop）
server.Run(ctx, &mcp.StdioTransport{})

// 或 HTTP 模式（远程访问）
handler := mcp.NewStreamableHTTPHandler(
    func(*http.Request) *mcp.Server { return server }, nil)
http.ListenAndServe(":3000", handler)
```

### 4.2 安全实现（必做，简历亮点）

```go
// internal/mcp/server/guard.go

// Guard 为每个 tool 调用提供并发限制、超时与审计。
//
// 为什么需要：MCP Server 面向不可信的 AI 客户端。
// 模型可能因为 prompt 注入或自身错误而疯狂调用工具，
// 没有防护会打爆下游服务。
type Guard struct {
    sem    *semaphore.Weighted  // 全局并发上限 20
    timeout time.Duration       // 单次调用 30s
    logger  *slog.Logger
}

func (g *Guard) Wrap(name string, fn ToolFunc) ToolFunc {
    return func(ctx context.Context, args any) (any, error) {
        start := time.Now()

        // 1. 并发控制：拿不到令牌就排队（而非直接拒绝）
        if err := g.sem.Acquire(ctx, 1); err != nil {
            return nil, fmt.Errorf("服务繁忙，请稍后重试")
        }
        defer g.sem.Release(1)

        // 2. 超时：防止单个慢调用占住并发额度
        ctx, cancel := context.WithTimeout(ctx, g.timeout)
        defer cancel()

        result, err := fn(ctx, args)

        // 3. 审计日志：⚠️ 必须走 stderr（stdio 模式下 stdout 是协议通道）
        g.logger.Info("tool_call",
            "tool", name,
            "duration_ms", time.Since(start).Milliseconds(),
            "success", err == nil,
            // 参数只记摘要，避免日志里出现敏感内容
            "args_hash", hashArgs(args),
        )
        return result, err
    }
}
```

---

## ✅ Phase 4 验收

- [ ] Claude Desktop 配置后能 list 出 4 个 Tool
- [ ] 在 Claude Desktop 说「搜索我云盘里 Kafka 相关的文档」→ 成功调用并返回
- [ ] 并发 50 个 tool call：不 panic、不 OOM、超出 20 的排队而非报错
- [ ] 审计日志能看到完整调用记录（且不污染 stdout）
- [ ] 录 30 秒 GIF 存 `docs/demo.gif`（**简历/GitHub README 用**）
- [ ] `git tag phase4-done`

## 🚫 Phase 4 不做

- 不实现 Resources / Prompts 原语
- 不做 OAuth（API Key 足够）
- 不做 MCP Client

---

# Phase 5：可观测性 + 压测出数

**目标**：产出简历上每一个数字。
**分支**：`phase5-observability` · **预计**：3-4 天

---

## 📚 本阶段学习内容

### L5.1 Prometheus 三种指标类型

| 类型 | 特点 | 用在哪 |
|------|------|--------|
| **Counter** | 只增不减 | Token 消耗总量、缓存命中次数 |
| **Gauge** | 可增可减 | 当前活跃会话数 |
| **Histogram** | 分布统计，可算 P50/P95/P99 | 检索耗时、Tool 调用耗时 |

**为什么延迟必须用 Histogram 而非 Gauge**：平均值会掩盖问题。平均 50ms 可能是 99 次 10ms + 1 次 4 秒 —— 那 1 次就是用户投诉的来源。P99 才能暴露它。

### L5.2 pprof 使用

```bash
# 采集 30 秒 CPU profile
go tool pprof http://localhost:8080/debug/pprof/profile?seconds=30

# 采集堆内存
go tool pprof http://localhost:8080/debug/pprof/heap

# 可视化（需要 graphviz）
go tool pprof -http=:8081 heap.pprof
```

**看什么**：
- `top` 命令看 CPU/内存占用最高的函数
- 火焰图看调用链，宽度 = 耗时占比

**常见可优化点**（本项目大概率能挖到）：
- JSON 序列化用标准库 → 换 `sonic`（字节的高性能 JSON 库，Eino 已经依赖了）
- 大结构体值传递 → 改指针
- 循环里重复分配 → 预分配 slice 容量
- Embedding 没批量 → 改批量

---

## 🔨 实现内容

### 5.1 指标埋点（**只埋这些**）

```go
// internal/metrics/metrics.go

var (
    // Histogram：能算 P99（L5.1）
    RetrievalDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Name:    "kaho_retrieval_duration_seconds",
        Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1},
    }, []string{"method"})  // method: vector/bm25/hybrid/rerank

    // Counter：只增不减
    LLMTokens = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "kaho_llm_tokens_total",
    }, []string{"type", "model"})  // type: prompt/completion

    CacheOps = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "kaho_cache_ops_total",
    }, []string{"cache", "result"})  // result: hit/miss

    ToolCallDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Name: "kaho_tool_call_duration_seconds",
    }, []string{"tool", "status"})

    // Gauge：可增可减
    ActiveSessions = promauto.NewGauge(prometheus.GaugeOpts{
        Name: "kaho_active_sessions",
    })
)
```

### 5.2 压测清单（每项都要有输出文件）

| 测什么 | 工具 | 命令 | 输出 |
|--------|------|------|------|
| 检索延迟 | go bench | `go test -bench=Retrieve -benchmem ./internal/rag/...` | `docs/bench/retrieval.txt` |
| HTTP QPS | vegeta | `vegeta attack -rate=100 -duration=60s \| vegeta report` | `docs/bench/http.txt` |
| MCP 并发 | 自写脚本 | `go run scripts/bench/mcp_load.go -c 50` | `docs/bench/mcp.txt` |
| 内存 | pprof | `go tool pprof -http=:8081 heap.pprof` | `docs/bench/heap.svg` |

### 5.3 必须找到并修复至少 1 个性能问题

**这是简历上「pprof 定位到 X，优化后提升 Y」那条的来源。**

流程：
1. 压测 → 采 profile → 找热点
2. 修复
3. 再压测
4. **前后数据都记进 `docs/optimization.md`**

---

## ✅ Phase 5 验收

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

# Phase 6：部署收尾

**分支**：`phase6-polish`

## 必做
- [ ] `Dockerfile`（多阶段构建，最终镜像 <30MB）
- [ ] `.github/workflows/ci.yml`（lint + build + test）
- [ ] README 加架构图 + demo.gif
- [ ] **简历数字用 Phase 5 实测值替换**

## 可选（按优先级，时间够才做）
1. Query 改写 / HyDE
2. PDF 解析
3. 简单 Web UI
4. 增量索引更新

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

**进度落后时的砍法**：Phase 6 可选项 → Phase 4（MCP）→ **Phase 1/2/3 一个都不能砍**。

---

## 🔍 卡住时的自查清单

| 症状 | 大概率原因 | 怎么办 |
|------|-----------|--------|
| 检索结果很烂 | 分块策略问题，不是模型问题 | 打印实际 chunk 看内容是否被切碎 |
| 中文检索乱码 | 用 []byte 而非 []rune 切分 | 见 Phase 1 步骤 1 |
| Milvus 搜不到 | 没 Flush 或没 LoadCollection | Insert 后 Flush，搜索前 Load |
| Agent 不调 Tool | Description 太模糊 | 用祈使句写清「什么时候该用」（L3.2）|
| Agent 死循环 | MaxIterations 太大 + Tool 返回值让模型困惑 | 降到 10，Tool 返回结构化文本而非 JSON |
| MCP 连不上 | stdout 被日志污染 | **日志走 stderr**（L0.4）|
| 混合检索不如单路 | RRF 实现有 bug 或某路召回质量太差 | 分别打印两路结果人工看 |
| 编译一堆错 | 提前引入了下个 Phase 的依赖 | 回滚到上个 tag，按序来 |
| Token 消耗爆炸 | 没做上下文截断 | 检查 memory.go 的窗口逻辑（L3.3）|
