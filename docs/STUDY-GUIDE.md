# KahoAgent 源码学习指南

> 这份文档不是 README 的副本。README 回答「它是什么、怎么跑」，
> 这份文档回答「**为什么这样写、读代码时该看哪里、改坏了会怎样**」。
>
> 全篇中文，代码片段都带逐行注释。建议对照源码阅读，不要只读本文。

---

## 0. 先建立三个心智模型

在读任何一行代码之前，先接受三个事实。它们能解释这个仓库里 80% 的设计选择。

### 0.1 两条完全不同的路径

```
在线：  用户提问 → 检索 → 精排 → LLM 生成 → SSE 逐字返回        （延迟敏感）
离线：  上传文档 → Kafka → 分块 → 向量化 → 双写 Milvus/PG       （吞吐敏感，可失败重试）
```

两条路径的**资源画像、失败语义、扩展方式**都不一样，所以它们是两个进程
（`cmd/server` 与 `cmd/worker`），只在 `internal/app` 的装配层共享依赖。
把 worker 塞进 server 起个 goroutine 是最常见的错误做法：一次批量入库
就能把 SSE 的首字延迟拖垮。

### 0.2 三层抽象

```
接口层  internal/rag/types.go       Chunker / Embedder / VectorStore / KeywordStore
实现层  internal/rag/*, pkg/*        具体算法与客户端
装配层  internal/app/app.go          决定用哪个实现、缺了谁可以降级
```

为什么值得这么分层：**单元测试不需要 Milvus**。`pipeline` 的测试注入
假 Chunker/Embedder/Store，秒级跑完；而 e2e 测试复用同一份装配代码，
不会出现「测试绿、线上起不来」。

### 0.3 一条铁律：能降级就降级，降级必须可见

- PG 挂了 → 检索降级为纯向量，**打 WARN 日志**
- Redis 挂了 → 缓存与记忆禁用，**打 WARN**
- 精排失败 → 回退 RRF 顺序，**打 WARN**
- 上游 429 → 切备用模型，**熔断计数**
- worker 连不上 PG → **拒绝启动**（因为它的职责就是双写，半写等于静默劣化）

判断标准：**降级后的结果是否还能正确回答用户？** 能，就降级并告警；
不能，就快速失败。这条规则贯穿全项目。

---

## 1. 全景：目录职责与一次请求的完整路径

### 1.1 目录职责表

| 路径 | 职责 | 读代码优先级 |
|---|---|---|
| `cmd/server` | HTTP 入口：路由、pprof、优雅退出 | ★★ |
| `cmd/worker` | Kafka 消费进程 | ★ |
| `cmd/mcp` | MCP Server（stdio/http） | ★ |
| `internal/app` | **依赖装配**，HTTP/MCP 共用 | ★★★ |
| `internal/rag/types.go` | 四个核心接口，全项目的契约 | ★★★ |
| `internal/rag/chunker` | 分块策略（Markdown / 纯文本） | ★★ |
| `internal/rag/retriever` | 混合召回、RRF、精排、BM25 | ★★★ |
| `internal/agent` | Eino ReAct 编排、记忆压缩、流式事件 | ★★★ |
| `internal/agent/tools` | 4 个业务工具 | ★★ |
| `internal/pipeline` | Kafka 入库：producer/consumer/ingester | ★★ |
| `internal/handler` | SSE 对话、上传 | ★★★ |
| `internal/mcp/server` | Tools → MCP 的通用桥接 + 安全防护 | ★★ |
| `internal/metrics` | Prometheus 指标定义 | ★ |
| `internal/trace` | OTel 初始化 + HTTP 中间件 | ★ |
| `internal/eval` | 指标计算 + 回归门禁 | ★★ |
| `internal/web` | 内嵌控制台（3 个静态文件） | ★ |
| `pkg/llm` | LLM/Embedding 客户端、熔断与主备 | ★★★ |
| `pkg/vectorstore` | Milvus 封装 | ★ |
| `pkg/cache` | Redis 封装 | ★ |
| `scripts/eval` | 评估、压测、语料入库 | ★★ |

### 1.2 一次问答的调用链

对照这张表，每一步都能落到具体函数：

| # | 位置 | 发生了什么 |
|---|---|---|
| 1 | `cmd/server/main.go` → `mux` | `/api/chat` 交给 `ChatHandler` |
| 2 | `trace.HTTPMiddleware` | 建 server span，包一层 `statusRecorder`（**转发 Flusher**） |
| 3 | `handler.ChatHandler.ServeHTTP` | 校验方法/JSON/空消息，断言 `http.Flusher`，写 SSE 头并立刻 Flush |
| 4 | `handler.stream` | 读会话历史 → `trimHistory` 截到预算内 |
| 5 | `agent.Run` | Eino ADK 启动 ReAct 循环 |
| 6 | `EinoChatModel.Stream` | 调上游 chat/completions（含熔断、主备） |
| 7 | `rag_query` 工具 | 进入 `HybridRetriever.Search` |
| 8 | `hybrid.Search` | 查缓存 → 并发召回 vector/bm25 → RRF → 精排 → TopK |
| 9 | 工具结果回到模型 | 模型据此生成最终回答 |
| 10 | `agent.Consume` | 把事件流转成 `Chunk{Text/ToolResult}` |
| 11 | `handler.stream` | `delta` 逐片推给前端；工具结果解析成 `sources` 事件 |
| 12 | 结束后 | 成功才写历史；`done` 事件收尾 |

### 1.3 一次上传的调用链

| # | 位置 | 发生了什么 |
|---|---|---|
| 1 | `handler.UploadHandler` | 校验 docID / 大小 / 二进制 |
| 2 | `pipeline.Producer.Publish` | JSON 序列化，key=docID，RequireAll |
| 3 | 返回 202 | 接口不等入库 |
| 4 | `worker` → `Consumer.Start` | `FetchMessage` 拉取，处理成功才 Commit |
| 5 | `Ingester.Run` | 分块 → 批量 embedding → （可选清旧块）→ Milvus → PG |
| 6 | `ExtChunker` | 按扩展名选 Markdown / 纯文本分块器 |

---

## 2. 依赖装配：`internal/app/app.go`

这个文件只做两件事：**建立连接（dial）** 和 **组装对象（build）**。
它值得精读，因为「为什么某些依赖可以缺、某些必须报错」全在这里。

```go
// New 建立所有外部连接并装配出 HTTP 处理器。
//
// ctx 只用于启动期的连接建立，不会被存下来传给请求处理 ——
// 传下去的话优雅退出时会立刻取消所有在途请求，
// 而优雅退出的意义正是让在途请求跑完。
func New(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*App, error)
```

> 学习点：**启动期的 context 不等于请求期的 context**。
> 前者只活几秒，后者跟着一次 SSE 会话活几十秒。

### 2.1 三种依赖的失败策略

```go
// Milvus：连不上直接失败。它是向量召回的唯一来源，
// 没有它就没有「检索」这件事。
store, err := vectorstore.NewMilvusStore(...)
if err != nil {
    return fmt.Errorf("connect milvus: %w", err)
}

// PostgreSQL：连不上只告警。混合检索会降级为纯向量路，
// 用户仍然能问问题。
if err := pool.Ping(ctx); err != nil {
    logger.Warn("postgres is unreachable; retrieval will degrade to vector-only", "err", err)
}

// Redis：连不上只告警。丢的是缓存和会话历史，
// 单轮问答不受影响。
if err := rdb.Ping(ctx).Err(); err != nil {
    logger.Warn("redis is unreachable; caching and session memory are disabled", "err", err)
}
```

这三段是「降级矩阵」的代码化。注意它们都是 **Ping 失败不返回 error**，
而不是「等第一次用到再发现」—— 启动时就把状态写进日志，
排查时第一屏就能看到。

### 2.2 为什么 Tools 只装配一次

```go
// Tools 是注册给 Agent 的工具集，MCP Server 直接复用同一份 ——
// 各自装配一套的话，两边的检索配置与降级策略迟早分叉，
// 而 MCP 路径平时没人盯着，分叉了也不会有人发现。
Tools []tool.BaseTool
```

这是「单一事实来源」的典型应用：同一个能力，两个出口（HTTP / MCP），
但只有一份实现。

---

## 3. 检索链路：这个项目最值得读的部分

### 3.1 抽象先于实现：`internal/rag/types.go`

```go
type Chunker interface {
    Name() string
    Chunk(ctx context.Context, docID string, raw []byte) ([]Chunk, error)
}

type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)  // 批量
    Dim() int
}

type VectorStore interface {
    Upsert(ctx context.Context, chunks []Chunk, vectors [][]float32) error
    Search(ctx context.Context, vector []float32, topK int) ([]ScoredChunk, error)
    DeleteByDoc(ctx context.Context, docID string) error
}

type KeywordStore interface {          // 2026-11 新增
    Upsert(ctx context.Context, chunks []Chunk) error
    DeleteByDoc(ctx context.Context, docID string) error
}
```

> 为什么 `Embed` 是批量接口而不是单条：100 个 chunk 逐条调 API 是
> 100 次 HTTP 往返；批量后 Embedder 内部按 batchSize 分批，通常 2 次。

> 为什么 `KeywordStore` 独立于 `VectorStore`：两者存的是完全不同的东西
> （向量 vs 词元）。合并成一个接口会让「只想用向量库」的评估脚本
> 被迫依赖 PostgreSQL。

### 3.2 分块：`internal/rag/chunker`

两种策略，各有明确用途：

| 实现 | 依据 | 用途 |
|---|---|---|
| `MarkdownChunker` | 标题层级（`#`/`##`），保留 `heading_path` | 生产默认 |
| `PlainChunker` | 固定字数 + overlap | 评估对照组 / 纯文本兜底 |

`heading_path` 是后面「回答能给出处」的基础：

```
Redis 缓存实战 > 缓存击穿
```

比文件名更能说明「这段话在讲什么」。

**分块的三个坑**（都在代码注释里）：

1. **按字节切 vs 按字符切**：中文一个字符 3 字节，按字节切会把
   一个汉字劈成两半，产生非法 UTF-8。
2. **overlap 必须小于 size**：否则 step <= 0，死循环。
3. **chunk ID 必须稳定**：`sha256(docID#index)` 保证重复入库幂等。

### 3.3 向量路：`retriever/vector.go` + `pkg/vectorstore/milvus.go`

流程简单：query → embedding → Milvus HNSW 搜索 → `[]ScoredChunk`。

真正的知识点在封装里：

- Milvus 需要 `[]float32`，而 Eino 的 embedding 组件返回 `[][]float64`，
  适配层在 `pkg/llm/embedder.go` 完成转换。
- Collection 的维度在建表时固定，必须与 `llm.embedding_dim` 一致；
  不一致的报错发生在插入时，而不是建表时 —— 所以配置校验要做在前面。

### 3.4 关键词路：`retriever/bm25.go`

```go
// 写入时就把分词结果存进 tokens 列：
// ⚠️ 不能直接对 content 做 to_tsvector：PG 自带的分词器不认识中文，
//    会把「Redis缓存穿透」当成一个不可分割的词。
//    故在 Go 侧用 gojieba 切好，PG 用 'simple' 分词器按空格切。
tokens := w.tok.Tokenize(c.Content)
```

> 关键设计：**分词只做一次**，在入库时；查询时对 query 分词即可。
> 如果每次查询都重新对全表分词，延迟会爆炸。

为什么不用 PG 的 `pg_jieba` 扩展：部署依赖更重（要在数据库侧装扩展），
而项目已经因为 gojieba 引入了 cgo，不如复用同一个分词器保证
「入库与查询的口径一致」。

### 3.5 融合：`retriever/rrf.go`

RRF 公式：

```
score(d) = Σ 1/(k + rank_i(d))      k = 60
```

**为什么用排名而不是分数**：向量余弦相似度是 0~1，BM25 分数是几十，
量纲不可比。任何「加权求和」都要先归一化，而归一化本身就是个错误来源
（不同 query 的分数分布不同）。RRF 直接绕开这个问题。

**为什么 k=60**：来自原论文的经验值。k 越大，排名差异的影响越小。

代码里有一处值得注意的优化：

```go
// 优化前：三个 map 来回查，分配量占检索链路 43%
// 优化后：单 map 定位下标 + 预分配 slice
// 用 pprof 实测该函数耗时 −68%
```

> 学习点：这个优化**只对单函数有效**（微秒级），端到端延迟瓶颈在 LLM 生成。
> 面试时把基数说清楚，比吹「系统性能提升 68%」可信得多。

### 3.6 精排：`retriever/rerank.go`

把 RRF 的 top-20 交给 LLM，逐条打相关性分（pointwise）：

```go
// temperature = 0：
// 同样的输入必须给出同样的分数，否则每次评估结果都不同，
// 无法判断改动是否真的带来提升。
```

两个工程细节：

- **单块失败取成功块的中位数**：LLM 偶发返回非数字，不能让整次精排失败。
- **全失败回退 RRF**：返回一个稍差但可用的结果，而不是错误。

> 这是简历上「MRR 0.719 → 0.818」的来源，代价是 P50 从 150ms 涨到 1.88s。
> 所以它是「按需启用」的。

### 3.7 编排：`retriever/hybrid.go`

这是检索链路的总指挥，四个关键决策都在这里：

**① 缓存 key 必须包含策略参数**

```go
return fmt.Sprintf("hybrid:%s:n=%d:k=%d:%s:%s", names, h.recallN, h.topK, rerank, query)
//              召回器  召回数 topK  是否精排  原始 query
```

少了任何一项，改了配置还会命中旧结果 —— 而这类 bug 表现为
「结果莫名其妙不对」，极难排查。

**② 并发召回，但不能用 errgroup**

```go
// ⚠️ 刻意不用 errgroup：它在首个错误时会取消 context，
// 导致一路失败连累另一路被取消 —— 这与降级要求正好相反。
// BM25 挂了还有向量结果，反之亦然，任何一路的失败都只记日志。
```

这是全仓库最能体现「理解库的语义」的一处。

**③ 缓存命中也要记耗时**

```go
// 只记未命中的话，P99 会虚高 —— 而用户感知到的是**所有**请求的分布，
// 其中相当一部分是毫秒级的缓存命中。
```

**④ 精排失败不失败整个请求**

```go
if err != nil {
    h.logger.Warn("rerank failed, falling back to RRF order", "err", err)
} else {
    results = reranked
}
```

---

## 4. Agent 与工具

### 4.1 Eino 适配：`pkg/llm/chatmodel.go`

`EinoChatModel` 把 OpenAI 兼容接口适配成 Eino 的 `BaseChatModel`。
这个文件里有大量「只有读过源码才知道」的注释，是很好的学习材料。

**三个关键事实**（注释里都有）：

1. `model.BaseChatModel` 是类型别名 `BaseModel[*schema.Message]`，
   实现 `Generate` + `Stream` 即可。
2. 工具不通过 `WithTools` 绑定，而是 **Agent 每次调用时通过
   `model.WithTools` 选项传入** —— 所以 `buildRequest` 要优先读调用选项，
   读不到才退回预绑定的 `c.tools`。优先级搞反的表现是「模型突然忘记自己有工具」。
3. **流式响应默认不带 usage**，必须在请求里加
   `stream_options.include_usage=true`，且 usage 只出现在最后一个 chunk。
   不知道这点就会以为「流式统计不到 token」。

```go
// ⚠️ usage 只出现在**最后一个** chunk 里，而那个 chunk 的
// choices 通常是空的 —— 所以必须在 len(Choices)==0 提前 continue
// 之前处理，否则 token 统计永远是 0。
c.recordUsage(chunk.Usage)
```

这个「先处理 usage 再跳过空 choices」的顺序，是典型的**只有踩过才知道**的细节。

### 4.2 事件流翻译：`internal/agent/stream.go`

Eino 的事件流是泛型嵌套的 `AgentEvent`，handler 不该理解它。
`Chunk` 做了扁平化：

```go
type Chunk struct {
    Text       string  // 模型增量输出
    ToolName   string  // 非空表示这是工具结果
    ToolResult string  // 工具返回给模型的原文（供前端解析来源）
    Err        error
}
```

> 2026-11 新增 `ToolResult` 的原因：前端要展示「这次回答引用了哪些片段」，
> 而引用信息就藏在 `rag_query` 的输出里。让 handler 解析固定格式，
> 比让前端去猜模型输出里的引用标记可靠得多。

**为什么 Go 里能平铺处理文本流**：`drain` 里每个 chunk 都是**增量**，
直接 `WriteString` 即可；这与你熟悉的「每次收到全量」不同。

### 4.3 ReAct 循环：`internal/agent/agent.go`

用 Eino ADK 的 `ChatModelAgent`，配置里最关键的是 `MaxIterations`：

- 太小：复杂问题工具调用没完成就被截断
- 太大：模型陷入「调工具 → 结果不满意 → 再调」的死循环，烧配额

提示词里还有一条容易忽略的约束：工具返回「未找到」时必须让模型
如实说不知道，而不是编造 —— 这也是 `rag_query` 在空结果时返回
「未找到相关文档。」而不是空串的原因（空串会让模型以为工具坏了，反复重试）。

### 4.4 会话记忆：`internal/agent/memory.go`

记忆存储 Redis，超出窗口时触发**摘要压缩**。压缩的优先级排序是
实测调出来的：

```
用户亲口说的事实  >  文件/主题  >  检索到的资料
```

判断依据是「能不能重新拿到」：

- RAG 检索到的知识随时可以再检索，丢了不可惜；
- 用户亲口说的「我叫什么、我在做什么项目」一旦丢掉就找不回来。

早期版本按「文件名、主题、结论」排序，12 轮验收直接失败 ——
摘要 100% 是检索资料，用户陈述一条没留。

> 这一段是简历里「12 轮对话后仍能准确回忆第 1 轮」的出处。
> 面试追问「怎么测的」：构造 12 轮对话，第 1 轮埋一个用户事实，
> 第 12 轮问它。

---

## 5. HTTP 层：SSE 与上传

### 5.1 为什么是 SSE 不是 WebSocket

LLM 输出是**单向**的（服务端 → 客户端）。SSE 基于普通 HTTP，
不需要协议升级，浏览器自带断线重连。WebSocket 的双向能力在这里用不上。

### 5.2 SSE 事件协议（与前端 `app.js` 的契约）

| `type` | 字段 | 含义 |
|---|---|---|
| `delta` | `text` | 增量文本 |
| `tool` | `tool` | 正在调用某个工具（前端显示「检索中」） |
| `sources` | `sources[]` | 本轮检索的来源列表（去重，最多 8 条） |
| `done` | — | 正常结束 |
| `error` | `text` | 用户可读的错误 |

**每条事件都必须 Flush**：

```go
flusher.Flush() // ⚠️ 每条都要 Flush，否则还是攒批
```

少了这一行不会报错，只是「流式」退化成「等全部生成完再一次性返回」——
用户只会觉得慢，看不出是 bug。

### 5.3 三个容易写错的地方

**① Flusher 断言必须在写任何响应之前**

```go
flusher, ok := w.(http.Flusher)
if !ok {
    http.Error(w, "streaming is not supported by this connection", 500)
    return
}
```

写到一半才发现不支持 Flush，响应头已经发出去了，没法改状态码。

**② 只有成功完成才写历史**

```go
// 中途失败仍然写的话，下一轮会带上一段残缺的回答，
// 模型会以为自己说过那些话，越聊越偏。
```

**③ 保存历史用 WithoutCancel**

```go
saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
```

客户端断开时请求 ctx 已取消，但对话其实已经成功完成了，历史必须存下来。

### 5.4 历史预算：`trimHistory`

```go
// 从最旧的消息开始丢，直到总字符数落在预算内。
// 为什么丢最旧的：对话语义依赖最近几轮，最旧内容贡献最小。
// 报错则等于惩罚聊得久的用户 —— 而他们恰恰最活跃。
```

两个细节：

- 单条超预算时**仍然保留最后一条**：宁可让上游报 context 超限
  （用户看得懂），也不静默丢掉用户刚提的问题。
- 切口落在 assistant 上时继续前移，避免模型收到「半轮」上下文。

### 5.5 上传：`internal/handler/upload.go`

校验顺序（每一步都对应一类攻击面）：

1. Content-Type 只接受 multipart / JSON
2. docID：去空格、长度 ≤200、禁止 `/` `\` 和 NUL、禁止 `.`/`..`
3. 大小：≤ `pipeline.MaxRawBytes`（640KB）
4. 二进制嗅探：前 512 字节出现 NUL → 415
5. 投递失败 → 503；错误详情只进日志，不返回给客户端

**640KB 是怎么来的**（面试常问）：

```
Kafka 单条消息上限 ≈ 900KB
IngestTask.Raw 是 []byte，encoding/json 会编码成 base64
base64 膨胀 = 4/3
→ 原文上限 = 900KB × 3/4 = 675KB，再留余量取 640KB
```

拿 900KB 去卡原文，会导致 700KB 的文档在 `Publish` 阶段才失败。

---

## 6. 异步入库：`internal/pipeline`

### 6.1 幂等：为什么重复消费无害

```
chunk.ID = sha256(docID#index)
Milvus: upsert by ID
PG:     INSERT ... ON CONFLICT (id) DO UPDATE
```

Kafka 的「至少一次」意味着同一条消息可能被消费两次。
幂等不是锦上添花，而是**至少一次语义成立的前提**。

### 6.2 更新后文档变短：`WithStaleCleanup`

chunk ID 带下标，所以：

```
旧版本 12 块，新版本 8 块
→ 第 9~12 块的 ID 不变，但内容已过期
→ upsert 覆盖不到它们，索引里残留旧版本尾部
→ 检索结果拼出一篇根本不存在的文档
```

解法：写入前按 docID 清空旧块。**放在 embedding 之后**，
这样向量化失败时旧版本仍然可用——代价是几毫秒的空窗，
而 Kafka 的同一 partition 有序消费保证同一文档不会并发写。

> 为什么默认关闭、只在 worker 打开：评估脚本是「全量重建」，
> 先删后写会让「删了一半时 API 失败」的代价从「部分未更新」
> 变成「整篇消失」。

### 6.3 至少一次 vs 毒丸消息

```go
// ⚠️ 用 FetchMessage 而非 ReadMessage。
// ReadMessage 会自动提交 offset —— 消息一取出就算「已处理」，
// 处理失败就永久丢失。
msg, err := c.reader.FetchMessage(ctx)
```

三种结局：

| 情况 | 是否 commit | 原因 |
|---|---|---|
| 反序列化失败 / docID 为空 | ✅ commit | 重试多少次都一样，不提交会堵死整个分区（毒丸消息） |
| 处理失败 | ❌ 不 commit | 假定是暂时故障（网络、Milvus 重启），重启后重投 |
| 成功 | ✅ commit | 正常 |

**已知缺口**：没有死信队列。永久性失败的消息会一直重试，
靠人工介入。这一点写进了 README 的「已知限制」，不要假装没有。

### 6.4 worker 为什么要求 PG 必须可用

```go
// ⚠️ 与 server 的降级策略相反：worker 的职责就是双写，
// 缺了关键词索引写一半，结果是「检索得到但 BM25 永远缺一路」
// 的静默劣化。宁可起不来。
```

同一个依赖，在不同进程里的失败策略可以相反 —— 判断依据是
「这个进程的职责是否允许只做一半」。

---

## 7. MCP：把能力暴露给 Claude Desktop / Cursor

### 7.1 通用桥接

不给每个工具写一份 MCP 适配，而是读取工具自带的 JSON Schema：

```go
// 新增工具零改动：注册到 Agent 的 Tools 后，MCP 侧自动出现。
```

### 7.2 Guard：限流、超时、审计

三个细节都能在面试里展开：

**① 限流用「排队」而不是「拒绝」**

MCP 客户端（Claude Desktop）不会优雅处理 429，
它只会把错误当成「这个工具坏了」。排队则在客户端看来只是「慢一点」。

**② 超时计时必须在拿到令牌之后**

```go
// ⚠️ 放在前面的话，排队时间会算进超时预算，
// 负载高时所有调用都会超时 —— 而且看起来像「工具变慢了」。
```

**③ 审计日志只记 args_hash，且必须写 stderr**

- 查询可能含 PII 或误粘的密码，原文不能落审计日志；
- stdout 是 JSON-RPC 协议通道，日志写进去会破坏协议。

---

## 8. 可观测性

### 8.1 指标：`internal/metrics`

只埋「能驱动决策」的指标。三个设计选择：

- **延迟用 Histogram 不用 Gauge**：均值会掩盖长尾。
- **桶边界按实测铺**：精排 P99 是 1.9s，桶必须铺到 2.5s，
  否则全落进 `+Inf` 桶，等于没测。
- **标签不能有高基数**：不要带 query、session_id、file_id。

### 8.2 链路追踪：`internal/trace`

一次问答的 span 树：

```
POST /api/chat
└── retrieval.hybrid
    ├── retrieval.vector
    ├── retrieval.bm25
    └── rerank.llm
```

入库的 span 树：

```
kafka.consume
└── ingest.document
```

**全仓库最容易踩的坑**：HTTP 中间件包装 `ResponseWriter` 后丢了
`http.Flusher`，`ChatHandler` 的断言失败 → 直接返回
「streaming is not supported」。一个纯埋点的改动能把 SSE 整个弄坏，
错误信息还指向别处。`internal/trace/trace_test.go` 有一条测试专门锁死它。

> 同类问题：任何 `middleware` 只要包装 ResponseWriter，就要问一句
> 「它是否支持流式/ Hijack / 双向流」。这是 Go 后端的通用面试题。

### 8.3 稳定性：`pkg/llm/failover.go`

```
请求 → 熔断器允许？
        ├─ 否 → ErrCircuitOpen（立刻返回）
        └─ 是 → 主上游
                ├─ 成功 → 清零失败计数
                └─ 429/5xx → 备用上游（改写 model 字段）
                        ├─ 成功 → 清零
                        └─ 失败 → 记一次失败，累计到阈值则熔断 30s
```

**为什么不在同一上游重试**：429/5xx 在几毫秒内重试基本必败；
备用是另一套配额。

**为什么 400 不切备用**：请求本身有问题（模型名错、参数非法），
换谁都一样，只会多花一次配额并掩盖真正的 bug。

**为什么流式只在开始前切换**：SSE 一旦开始吐字，中途换模型会得到
两段拼接的回答 —— 比分段失败更糟。

---

## 9. 评测：让「检索变好了」成为可证伪的命题

### 9.1 指标定义

| 指标 | 分母 | 回答什么 |
|---|---|---|
| Recall@5 | 该查询的**全部相关块** | 该找的找到了多少 |
| MRR | 首个命中的**位次** | 第一个正确答案排得多靠前 |

**Recall@5 的理论上界不是 1.0，是 0.667**：
golden set 平均每条查询有 8.18 个相关块，前 5 个位置装不下。

```
理论上界 = mean(min(5, n) / n) = 0.667
实测 0.441 = 上界的 66%
```

> 这个数字低不是检索差，是**分母诚实**。
> 用 hit rate（top-5 命中 ≥1 就记 1）冒充 Recall，同样的结果能报到 0.9。

### 9.2 golden set 怎么构建

- 由内容谓词 SQL 全量扫描生成（`build_golden.go`），
  **检索器不参与标注** —— 否则就是自己出题自己考。
- 78 条查询，分三类：`confusable` 8 / `semantic` 29 / `keyword` 41。

### 9.3 RRF 反噬与精排

`hybrid-rrf` 的 MRR（0.719）反而低于 `bm25-only`（0.723）。
排查路径：

1. 看 recall@50：两路接近（0.707 / 0.740）→ 问题在排序不在召回
2. 定位到 RRF 机制：只用排名不用分数 → 单路强命中输给双路平庸
3. 试加权 RRF：9 组权重里三类查询的最优解互相矛盾 → 静态权重是死路
4. 精排引入新判据 → MRR 0.818

### 9.4 门禁：`internal/eval/gate.go`

```go
// 绝对差 0.02，而不是相对百分比：
// 检索指标的绝对值本身有语义（0.818 就是 81.8% 首条命中）。
// 0.02 也正好大于 LLM 精排的实测噪声，否则门禁会天天误报，
// 最后所有人都学会「红了就重跑一次」—— 那样的门禁等于没有。
func Compare(baseline, current Report, tolerance float64) []Violation
```

三条判定规则：

- 任一路的 Recall@5 / MRR 绝对跌幅 > 0.02 → 失败
- golden set 条数变化 → 失败（换了题目就不能比分数）
- 基线里的策略本轮缺失 → 失败（不带 `-rerank` 时先裁剪基线并打印跳过项）

---

## 10. 前端控制台：`internal/web`

三个静态文件（`index.html` / `app.css` / `app.js`），`go:embed` 进二进制。

### 10.1 为什么不用 EventSource

`EventSource` 只支持 GET，而 `/api/chat` 是 POST（要带 JSON body）。
所以用 `fetch` + `ReadableStream` 手动解析 SSE。

### 10.2 最小解析逻辑

```js
let buffer = "";
// SSE 事件以空行分隔；最后一段可能不完整，留在 buffer 里等下一块。
while ((idx = buffer.indexOf("\n\n")) >= 0) {
    const raw = buffer.slice(0, idx);
    buffer = buffer.slice(idx + 2);
    for (const line of raw.split("\n")) {
        const ev = parseEvent(line);   // 只认 "data:" 行
        if (ev) handleEvent(ev, body);
    }
}
```

这个「按空行切 + 保留残余」的模式是所有 SSE 客户端的通用写法。

### 10.3 安全

- 所有模型/文档文本走 `textContent`，不拼 `innerHTML`
- CSP：`script-src 'self'`，没有 `unsafe-inline`
- 即使哪里漏了转义，CSP 也不会执行注入脚本

---

## 11. 测试策略

### 11.1 分层

| 层级 | 命令 | 依赖 | 目的 |
|---|---|---|---|
| 单元 | `go test ./...` | 无 | 逻辑正确，秒级跑完 |
| 集成 | `go test -tags=integration ./...` | Milvus / PG / Redis | 真实协议与 SQL |
| 端到端 | `go test -tags=e2e ./...` | 上述 + LLM Key | 整条链路 |
| 评估 | `go run -tags=eval ./scripts/eval/eval.go` | 语料 + 下游 | 检索质量 |

分层的原因：单元测试必须能在任何机器上秒级跑完，否则没人会在提交前跑；
需要真实依赖的测试单独打标签，CI 只跑不带标签的那部分。

### 11.2 注入点在哪

所有外部依赖都通过接口注入，所以测试里可以替换：

```go
// pipeline 的测试注入三个假实现，不需要 Milvus
ing, _ := NewIngester(&mockChunker{...}, &mockEmbedder{...}, &mockStore{...}, quietLogger())
```

```go
// handler 的上传测试注入假 publisher，不需要 Kafka
h, _ := NewUploadHandler(&fakePublisher{}, logger)
```

**写测试时先问**：这个测试依赖了什么外部服务？如果是，能不能改成
接口注入 —— 否则它最终会因为「本地没跑起来」而被跳过。

### 11.3 反证测试

仓库里有几处「把缺陷放回去，测试必须失败」的测试：

- `TestNewSegmenter_HonorsTheEnvVar`：指向不存在的词典目录必须失败，
  否则无法区分「环境变量生效」和「被忽略」
- 上传的拒绝用例同时断言「没有投递出去」——只断言状态码的话，
  一个「先 publish 再校验」的实现也能过

> 一个测不出 bug 的回归测试比没有更糟，它给的是虚假的安全感。

---

## 12. 常见改动指南

| 想做什么 | 改哪里 | 注意 |
|---|---|---|
| 换分块策略 | `internal/rag/chunker` + `app.buildTools` | 必须保证 chunk ID 稳定，否则去重失效 |
| 加一个工具 | `internal/agent/tools/` + `app.buildTools` | MCP 侧自动出现（Schema 桥接）；描述要写清楚「什么时候用」 |
| 换 embedding 模型 | `config.llm.embedding_model` + `embedding_dim` | 维度变了要重建 Milvus collection，不能只改配置 |
| 换 LLM | `config.llm.model/base_url` | 精排依赖 temperature=0 与 JSON 输出稳定性 |
| 加指标 | `internal/metrics` | 标签不能有高基数 |
| 加 SSE 事件 | `handler.sseEvent` + `app.js` | 前端未识别的类型会被忽略，向后兼容 |
| 调门禁阈值 | `internal/eval/gate.go` 的 tolerance | 先确认波动来自上游还是代码 |
| 改上传限制 | `pipeline.MaxRawBytes` | 必须 ≤ 900KB × 3/4 |

---

## 13. 自测 30 问

> 先自己回答，再对照括号里的要点。答不出来说明那一章还没读透。

**检索**

1. 为什么 RRF 不用分数而用排名？（量纲不可比，归一化本身是错误来源）
2. RRF 的 k 起什么作用？（越大越削弱排名差异）
3. 为什么混合检索会「反噬」，MRR 低于纯 BM25？（单路强命中输给双路平庸）
4. 精排为什么能解决它？（引入新的判据：直接读内容）
5. 为什么精排温度必须为 0？（可复现，否则评估不可比）
6. 单块精排失败怎么办？（取成功块中位数；全失败回退 RRF）
7. Recall@5 的理论上界为什么不是 1？（分母是全部相关块）
8. MRR 和 Recall 能互相推上下界吗？（不能，分母不同）
9. 为什么缓存 key 要带 topK 和 rerank 开关？（否则改配置仍命中旧结果）
10. 为什么召回不能用 errgroup？（首错取消会破坏降级）

**入库**

11. chunk ID 怎么生成，为什么要稳定？（sha256(docID#index)，幂等的前提）
12. 文档改短后会发生什么？（旧尾部块残留，需先删后写）
13. 为什么 Kafka key 用 docID？（同文档分区有序，避免乱序覆盖）
14. 至少一次为什么必须配幂等？（重复消费）
15. 毒丸消息怎么处理？（提交跳过，不能堵分区）
16. 上传为什么限 640KB？（base64 膨胀 4/3）
17. worker 为什么不能降级 PG？（职责是双写，半写是静默劣化）

**Agent / 服务**

18. 为什么用 SSE 不用 WebSocket？（单向输出，无需协议升级）
19. 每条事件为什么要 Flush？（否则攒批，流式失效）
20. 什么时候写会话历史？（仅成功完成，否则模型会以为自己说过残缺的话）
21. 历史压缩的优先级依据是什么？（能不能重新拿到）
22. MCP 的超时为什么从拿到令牌后开始计时？（排队时间不能算进预算）
23. 审计日志为什么不记原文？（PII / 误粘密码）
24. 为什么日志写 stderr？（stdout 是 JSON-RPC 通道）

**可观测 / 稳定性**

25. 为什么延迟用 Histogram？（均值掩盖长尾）
26. 追踪中间件为什么必须转发 Flusher？（否则 SSE 断言失败）
27. 429 和 400 的处理有什么不同？（429 切备用，400 不切）
28. 流式为什么只在开始前切备用？（中途切会拼接两段回答）
29. 熔断解决什么问题？（避免把连接池耗在必然失败的请求上）
30. 门禁为什么用绝对差 0.02？（指标绝对值有语义；0.02 大于上游噪声）

---

## 14. 术语表

| 术语 | 含义 |
|---|---|
| RAG | 检索增强生成：先检索相关资料，再交给模型回答 |
| Chunk | 文档切分后的最小检索单元 |
| Embedding | 文本 → 向量，用于语义相似度 |
| BM25 | 基于词频的关键词检索算法 |
| RRF | Reciprocal Rank Fusion，按排名融合多路结果 |
| Rerank | 对召回结果做二次排序（本项目用 LLM 打分） |
| ReAct | Reasoning + Acting，模型在「思考 → 调工具 → 观察」间循环 |
| MCP | Model Context Protocol，把工具以标准协议暴露给客户端 |
| Golden Set | 人工标注的评估集（query + 相关块 ID） |
| TTFT | Time To First Token，首字延迟 |
| DLQ | 死信队列（本项目未实现） |

---

## 15. 三条阅读路线

### 路线 A：30 分钟速览（面试前）

1. 本文 §0、§1（心智模型与调用链）
2. `internal/rag/types.go`（接口）
3. `internal/rag/retriever/hybrid.go` 的 `Search` 函数
4. `docs/eval-results.md` 的「结论与已知问题」
5. 本文 §13 自测题

### 路线 B：半天精读（准备改代码）

1. §2 装配 → `internal/app/app.go`
2. §3 全章 + 对照 `retriever/` 目录
3. §4 Agent + `pkg/llm/chatmodel.go`
4. §5 handler
5. 跑一遍 `go test ./...`，再挑一个包读它的测试

### 路线 C：动手实验

| 实验 | 做法 | 观察什么 |
|---|---|---|
| 关掉精排 | 不配 reranker 跑一次对话 | 延迟降一个数量级，MRR 降 0.1 |
| 停掉 PG | `podman-compose stop postgres` 后提问 | 降级为纯向量，日志出现 WARN |
| 制造 429 | 把 API Key 改错或并发打满 | 熔断打开，请求快速失败 |
| 观察 SSE | 浏览器 Network 面板看 `text/event-stream` | 每条事件独立到达 |
| 看 trace | 起 jaeger，开 `otel.enabled` | 检索的并发瀑布图 |
| 破坏门禁 | 把 `eval-baseline.json` 的 MRR 改高 | 门禁报错并列出回退项 |

---

## 16. 2026-11 增量：死信、跨进程 trace、桌面端

这一章记录第一版文档之后补的三块能力。它们的共同点是：
**代码量不大，但每一个都对应一个容易想当然的错误认知。**

### 16.1 死信队列：为什么「不提交 offset」不等于「重试」

最初的实现是「处理失败就不提交 offset」，注释写着「重启后会重新消费」——
这句话没错，但它只对**重启/重平衡**成立。

```go
// ⚠️ kafka-go 的 FetchMessage 即使不提交 offset，
// 同一个 Reader 也会继续向后读。
// 未提交的消息只有在重启/重平衡时才会被重新投递。
msg, _ := reader.FetchMessage(ctx)
```

也就是说：一条坏消息在**运行期间不会被重试**，而是被跳过；
直到进程重启才回来。基于「失败计数」写 DLQ 永远不会触发。

正确做法是进程内显式重试：

```go
func (c *Consumer) runWithRetry(ctx, task) (int, error) {
    for attempt := 1; attempt <= c.maxAttempts; attempt++ {
        n, err := c.ingester.Run(...)
        if err == nil { return n, nil }
        // 退避后重试；耗尽则返回最后一次错误
    }
}
```

重试耗尽后：

1. 把原始消息（key/value/headers 原样保留）投到 `<topic>.dlq`；
2. 追加 `x-kaho-error` / `x-kaho-attempts` / `x-kaho-origin-offset`；
3. 投递成功才提交 offset —— 投递失败仍然不提交，宁可重来也不丢文档。

> 为什么诊断信息放 header 而不是改写 value：
> 回放工具可以原样把消息发回主 topic，不需要先剥掉我们加的字段。

### 16.2 跨进程 trace：noop 传播器的坑

上传在 server，入库在 worker，原本是两条互不相关的 trace。
桥接靠 W3C `traceparent` 写进 Kafka header：

```go
headers := InjectTraceHeaders(ctx)   // producer
ctx = ExtractTrace(ctx, msg.Headers) // consumer
```

**坑**：`otel.GetTextMapPropagator()` 默认是 noop。
`trace.Setup` 里才会把它设成 `TraceContext`，而追踪默认关闭时
`Setup` 直接返回 —— 于是「provider 有 span、propagator 却不写 header」。

解法是两侧都补一次 W3C 兜底：

```go
otel.GetTextMapPropagator().Inject(ctx, carrier)
if len(carrier) == 0 {
    propagation.TraceContext{}.Inject(ctx, carrier)  // 有 span 才写，没有就是空
}
```

修好之后 Jaeger 里是一条链：

```
POST /api/upload
└── kafka.consume
    └── ingest.document
```

### 16.3 请求级统计：为什么不能从 Prometheus 反推

指标是聚合的（按模型、按结果），而「这一次请求用了多少 token」
是请求级数据。并发下用时间窗口反推根本对不上。

做法是把一个 `*reqstats.Stats` 放进 context：

```go
stats := reqstats.New()
ctx = reqstats.With(ctx, stats)     // handler 注入
// llm 客户端与检索器各自往里面写
snap := stats.Snapshot()            // 结束时读一次
```

两个设计细节：

- **nil 安全**：`From(ctx)` 可能返回 nil（精排、评估脚本没有请求上下文），
  所有方法都对 nil 接收者安全，下游埋点不用到处判空。
- **stats 事件在 done 之前发**：前端收到 done 时就能确定所有数字到齐。

`cache_hit` 只表示「检索缓存命中」，命中时延迟会从 ~150ms 降到 ~500µs ——
这个数字在 Web 控制台和桌面端都会展示。

### 16.4 桌面端：把网络留在主进程

Electron 的 renderer 的 origin 是 `file://`，直接 `fetch` 服务端会被 CORS 拦。
常见做法是关掉 `webSecurity`，但那是把整个渲染进程的安全边界拆了。

本项目的做法：

```
renderer  --(IPC)-->  main.js  --(fetch/SSE)-->  server
```

- renderer 的 CSP 是 `connect-src 'none'`，它根本没有网络能力
- main.js 解析 SSE 后按事件转发，顺便统一超时/取消/错误翻译
- preload 只暴露窄接口，不暴露 `ipcRenderer`

> 一个可以直接复用的细节：renderer 在 `window.kaho` 不存在时会退化成
> mock bridge，所以同一份界面可以用 Chrome 直接打开做视觉验收 ——
> 不需要为了截图维护第二套页面。

### 16.5 真机验收：三个环境坑

`scripts/local-stack.sh` 用 `podman run` 而不是 compose，原因是三个
在 WSL 上真实踩到的问题：

| 现象 | 根因 | 处理 |
|---|---|---|
| `crun: open memory.max ... No such file` | WSL 机器没启用 cgroup memory 控制器，compose 的 `mem_limit` 触发 | 不用 `--memory`，直接 run |
| Milvus 连 etcd 时报 `dial tcp 127.0.0.1:7897` | podman 把宿主机的 `HTTP(S)_PROXY` 注入了容器 | `--http-proxy=false` |
| Kafka 日志目录变成 `D:/Temps/...` | Git Bash（MSYS）把 `/tmp/...` 参数自动转成 Windows 路径 | `MSYS_NO_PATHCONV=1` |

验收脚本（`scripts/e2e-upload.sh`）用假 embedding 服务代替真实模型：

- 要验证的是「进得去、检索得出来」，不是模型质量
- 假服务返回确定性向量，重跑结果一致、零费用
- 断言链：202 受理 → worker `message processed` → 直接用 `HybridRetriever`
  查回该 doc_id

> 这一步还暴露了一个真实 bug：服务启动时从不调用
> `MilvusStore.EnsureCollection`，空库上第一次入库直接报
> `collection not found`。只有真机跑才会发现 —— 单测里的 store 是 mock。

### 16.6 接 DeepSeek 时踩到的四个坑

1. **没有 embedding 接口**。DeepSeek 只有 chat，向量化必须另找来源：
   拆出 `embedding_base_url` / `embedding_api_key`，并在启动时校验
   「embedding 不能指向 DeepSeek」——否则要等到第一次入库才报 404。
2. **推理模型吃 token**。`deepseek-flash` 先输出 `reasoning_content`，
   精排的 `max_tokens=16` 全被它吃光，`content` 为空、`finish_reason=length`，
   表现为「所有候选都解析不出分数」。两个修正缺一不可：
   `rerank_model` 指到非推理模型（`deepseek-chat`），
   同时把 Score 的预算放宽到 256（普通模型不会用满）。
3. **key 格式**。官方 key 以 `sk-` 开头；填成 UUID 会得到 401。
   项目把 401/429 翻译成可读提示，而不是笼统的「处理请求时出错」。
4. **worker 先于 topic 启动**。kafka-go 的消费组不会发现「启动后新建的
   topic」，表现为 worker 空转、重启才消费。修正：启动时 `EnsureTopic`
   + `WatchPartitionChanges`。

> 这四条共同说明一件事：**外部依赖的能力边界要在启动期校验**，
> 而不是让它在第一次真实请求时以一个没有上下文的 404/空字符串出现。

## 附录 A：数字索引（全部可追溯）

| 数字 | 出处 |
|---|---|
| Recall@5 0.314 → 0.441、MRR 0.660 → 0.818 | `docs/eval-results.md` |
| 首字延迟 P50 409ms / P99 1388ms | `docs/spike-evidence/phase5.md` |
| 缓存命中率 94.8% | 同上 |
| 常驻内存 23.9MB | 同上 |
| MCP 69.7 call/s（并发 50） | 同上 |
| FuseRRF 耗时 −68% | `docs/optimization.md` |
| golden set 78 条 | `scripts/eval/golden_set.json` |

> 写进简历前请读 `docs/resume-material.md`：每个数字都标了基数和适用范围，
> 例如「−68% 是单函数、微秒级」，不能说成「系统性能提升 68%」。

## 附录 B：学习时最该带走的五条经验

1. **降级要成体系**：每个依赖都要回答「它挂了会怎样」，并让降级可见。
2. **幂等是异步链路的地基**：至少一次语义只有在幂等的前提下才安全。
3. **指标要能证伪**：分母诚实的 0.441，比刷出来的 0.9 有价值。
4. **中间件会改变能力**：包装 ResponseWriter 之前，先问它丢没丢 Flusher。
5. **取舍要写下来**：代码注释里那些「为什么不用 X」比「用了 Y」更值钱。
