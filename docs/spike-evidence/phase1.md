# Phase 1 验收证据

**日期**：2026-08-19 · **分支**：`phase1-pipeline`

---

## 验收命令与输出

### 1. 单元测试

```
$ go test ./internal/rag/... -v
ok  github.com/yurin-kami/KahoAgent/internal/rag/chunker   3.782s
```

- `PlainChunker` 8 个测试
- `MarkdownChunker` 10 个测试

### 2. 全量测试（含集成）

```
$ go test -tags=integration ./... -timeout 15m
?   github.com/yurin-kami/KahoAgent/cmd/mcp          [no test files]
?   github.com/yurin-kami/KahoAgent/cmd/server       [no test files]
?   github.com/yurin-kami/KahoAgent/internal/config  [no test files]
ok  github.com/yurin-kami/KahoAgent/internal/pipeline      15.120s
?   github.com/yurin-kami/KahoAgent/internal/rag     [no test files]
ok  github.com/yurin-kami/KahoAgent/internal/rag/chunker    3.782s
ok  github.com/yurin-kami/KahoAgent/pkg/llm                 7.217s
ok  github.com/yurin-kami/KahoAgent/pkg/vectorstore        30.149s
ok  github.com/yurin-kami/KahoAgent/scripts/spike/mcp       5.979s
```

### 3. 端到端人工验证

```
$ go run -tags=ingestcheck ./scripts/eval/ingest_check.go docs/sample.md

=== Phase 1 端到端验收：docs/sample.md ===

[1] 入库成功：7 个 chunk
[2] 检索命中 3 条，doc_id 全部匹配
    Top1  score=0.7096  path="Redis 缓存实战 > 缓存击穿"
    内容：缓存击穿是指某个热点 key 恰好过期的瞬间，大量并发请求同时打到数据库。 与穿透的区别在于：这个 key 是真实存在的...
[3] 中文验证：取回内容无乱码
[4] heading_path：3/3 条有路径，嵌套示例 "Redis 缓存实战 > 缓存击穿"
[5] 幂等验证：重复入库后仍为 7 条，未堆积
[6] 删除验证：该文档 chunk 已全部清除

✅ Phase 1 端到端验收全部通过
```

这是**唯一**让真实 chunker → DashScope embedding → Milvus 串起来跑的路径。
集成测试用 mock embedder（省钱且快），因此「配置的 1024 维是否与
`text-embedding-v3` 实际输出一致」这类问题只有在这里才会暴露。

---

## 验收项对照

| 验收项 | 状态 | 证据 |
|--------|------|------|
| 上传 Markdown → Milvus 能查到对应 chunk（`doc_id` 匹配） | ✅ | 端到端 [2]；`TestMilvus_UpsertAndSearch` |
| 幂等验证：同文件上传两次，chunk 总数不变 | ✅ | 端到端 [5]（7 条 → 7 条）；`TestMilvus_UpsertIsIdempotent` |
| 删除验证：`DeleteByDoc` 后该文档 chunk 全消失 | ✅ | 端到端 [6]；`TestMilvus_DeleteByDoc` |
| 中文验证：纯中文文档分块后无乱码 | ✅ | 端到端 [3]；`TestMilvus_LongContentIsTruncatedNotRejected` 断言无替换字符 |
| heading_path 验证：嵌套标题文档路径正确 | ✅ | 端到端 [4]，实测 `"Redis 缓存实战 > 缓存击穿"` |
| Kafka 可靠性：中途 kill consumer，重启后消息被重新消费 | ✅ | `TestKafka_FailedMessageIsRedeliveredAfterRestart` |

---

## 交付清单

| 步骤 | 文件 | 测试 |
|------|------|------|
| 1 | `internal/rag/types.go` | 接口契约（已冻结） |
| 2 | `internal/rag/chunker/plain.go` | 8 单测 |
| 3 | `internal/rag/chunker/markdown.go` | 10 单测 |
| 4 | `pkg/llm/embedder.go` | 10 单测 |
| 5 | `pkg/vectorstore/milvus.go` | 9 集成测试 |
| 6 | `internal/pipeline/ingest.go` | 10 单测（全 mock，不依赖外部服务） |
| 7 | `internal/pipeline/producer.go` + `consumer.go` | 4 集成测试 |

**测试总数**：51（单测 38 + 集成 13）

---

## 本阶段解决的三个非平凡问题

均记录于 `phase1-pitfalls.md`，都是只有连真实服务才会暴露的问题：

1. **Milvus VarChar 上限是字节不是字符** —— 8192 汉字 = 24576 字节被拒。
   纯 ASCII 场景永不触发，恰好在中文知识库这个主场景炸。
2. **Milvus SDK 零命中时伪造错误** —— 空结果不返回 fieldsData，
   SDK 误报 "extra output fields"。判空必须先于判错。
3. **kafka-go DefaultTransport 是包级共享缓存** —— 6 秒元数据 TTL 导致
   「单独跑过、一起跑挂」。验证必须走与写入相同的 transport。

---

## 遗留观察（已入 BACKLOG，不在本 Phase 处理）

端到端检查中，查询「怎么防止缓存被恶意请求打穿」的 Top1 返回了
**缓存击穿**（score 0.7096），而正确答案应是**缓存穿透**。

纯向量检索无法区分「穿透 / 击穿 / 雪崩」这类字面与语义都高度接近、
但答案完全不同的技术概念。这正是 Phase 2 引入 BM25 + RRF + ReRank 的
**实证依据**，也已记入 BACKLOG 作为 golden set 的难例。

按防漂移铁律，本 Phase 不做任何检索排序改进。

---

### 📌 Phase 2 回访：上面这个归因是错的

Phase 2 用 `scripts/eval/backlog_check.go` 复查了这条具体查询，
结果与当时的结论不符：

| 策略 | Top1 |
|---|---|
| vector-only | ✅ 正确（缓存穿透那一节） |
| bm25-only | ❌ 无关 |
| hybrid-rrf | ✅ 正确 |
| hybrid+rerank | ✅ 正确 |

**纯向量检索现在能答对**。变量不是检索算法 —— 是语料：
Phase 1 时库里只有 `docs/sample.md` 一篇示例文档，
里面并没有真正讲「缓存穿透」解法的段落，向量检索只能在
现有内容里挑最接近的，于是返回了「击穿」。

所以那是**召回覆盖问题**，被误读成了**排序能力问题**。
Phase 2 灌入 77 篇真实文章（1307 chunk）后，正确的块进了库，
向量路自己就能排对。

这不影响 Phase 2 引入 BM25 + RRF + 精排的决策 ——
近义概念混淆在更大的 golden set 上确有其事
（`confusable` 类 vector-only MRR 仅 0.250，见 `../eval-results.md`），
但**这个具体案例不构成那个结论的证据**，当时用它做论据是不严谨的。

教训：**在语料不完整时观察到的检索错误，不能归因于检索算法。**
要先确认「正确答案在库里」，再谈「排没排出来」——
这正是 Phase 2 用 recall@50 先做分层定位的原因。
