# Phase 2 验收证据

**日期**：2026-08-20 · **分支**：`phase2-retrieval`

---

## 验收项对照

| 验收项 | 状态 | 证据 |
|--------|------|------|
| 四行对比表，`hybrid-rrf` MRR 明显高于 `vector-only` | ✅ | 0.698 vs 0.656（+6.5%），见 `../eval-results.md` |
| `var _ einoretriever.Retriever = (*HybridRetriever)(nil)` 编译通过 | ✅ | `internal/rag/retriever/hybrid.go:18`，`go build ./...` 通过 |
| Redis 缓存命中后延迟 <10ms | ✅ | 端到端 P99 **1ms**（预算 10ms），加速 273× |
| **降级验证**：停掉 PG，检索仍能返回结果 | ✅ | 双向验证，见下 |
| 评估结果存 `docs/eval-results.md`，带日期和参数配置 | ✅ | 脚本生成，含日期与全部参数 |
| `git tag phase2-done` | ⏳ | 本文档提交后执行 |

---

## 1. 检索效果对比

```
$ go run -tags=eval ./scripts/eval/eval.go -rerank

Method               Recall@5    MRR      P50       P99
vector-only          0.304       0.656    135ms     277ms
bm25-only            0.357       0.720    4ms       32ms
hybrid-rrf           0.348       0.698    129ms     385ms
hybrid-rrf+rerank    0.441       0.808    1.431s    1.893s

=== 验收判定 ===
hybrid-rrf MRR 0.698 vs vector-only MRR 0.656  ✅ 提升 6.5%

⚠️  但 bm25-only MRR 0.720 > hybrid-rrf 0.698
   精排后 0.808 已反超 bm25-only（+12.2%）✅
```

验收判据（hybrid > vector）通过，但**`bm25-only` 高于 `hybrid-rrf`**。
这一条不在验收清单里，但不能不提 —— 只报告有利的对比是用挑选过的
基线抬高结论。完整排查记录见 `phase2-pitfalls.md` 坑 1，
结论是 RRF 的一致性加分在两路质量悬殊时会反噬，
解法是精排而非调权重（加权扫描证明三类查询的最优权重互相矛盾）。

现在验收脚本会自动打印这条警告，跑一次就会自己提醒。

---

## 2. Golden set 的独立性

77 条查询 / 637 条标注，由 `build_golden.go` 从**内容谓词**生成：

```go
{
    Query: "热点key突然失效导致数据库压力暴增怎么办",
    AllOf: []string{"击穿"},
    AnyOf: []string{"互斥锁", "逻辑过期", "热点", "SETNX"},
}
```

用 SQL 扫**全量** 1307 个 chunk，**检索器完全不参与标注**。

这一点是整套评估可信度的根基：若拿检索器 top-N 当候选再人工挑，
检索器**找不到**的相关块永远进不了 golden set，Recall 会系统性虚高，
而且漏召回根本测不出来 —— 因为漏掉的东西不在答案里。

已知局限也写进了报告：谓词基于关键词，会漏掉纯同义表达的块。
这让所有策略的 Recall 一致偏低，但不影响**相对**比较。

---

## 3. 降级验证

规划要求「手动停掉 PG」。实际改用**把 pgx 指向无人监听的端口**，
制造同样的 ECONNREFUSED —— 对被测对象 `HybridRetriever` 而言两者等价
（它只能看到 recaller 返回的 error），但这样验收可以自动重跑。

```
$ go run -tags=degrade ./scripts/eval/degrade.go

=== 阶段 1：PG 正常 ===
返回 5 条，耗时 190ms

=== 阶段 2：BM25 不可达（连接 59999 端口）===
已确认 BM25 数据源不可达：dial tcp 127.0.0.1:59999: connectex: ...
WARN msg="recaller failed, degrading" recaller=bm25
返回 5 条，耗时 128ms

=== 阶段 3：反向 —— 向量路不可达 ===
WARN msg="recaller failed, degrading" recaller=vector
返回 5 条，耗时 117ms

✅ 降级验证通过：任一路故障都不影响服务可用性，且延迟在 15s 预算内
```

三点值得注意：

1. **双向都测**。只测一个方向的话，可能恰好是「向量路碰巧总是先返回」
   而非真的降级。
2. **降级后反而更快**（128ms < 190ms）。说明 pgx 是快速失败而非重试等待。
   这才是真正要验证的东西 —— 若驱动卡在重连上不返回，降级逻辑写得再对，
   整个请求也会被一起拖死。脚本因此设了 15s 预算并断言。
3. **脚本自己会验证故障注入是否生效**（先 Ping 死端口，连得上就报错退出）。
   否则「降级成功」可能只是因为根本没坏。

实现上 `recallAll` 刻意**不用 `errgroup`** —— 它在首个错误时会取消 context，
导致一路失败连累另一路被取消，与降级要求正好相反。

---

## 4. Redis 缓存

```
$ go run -tags=cachebench ./scripts/eval/cache_bench.go

轮次         P50          P99          平均
冷启动        145ms        196ms        149ms
命中         530µs        1ms          470µs
命中复测       510µs        890µs        380µs

P50 加速 273×（145ms → 530µs）
✅ 缓存命中 P99 1ms < 预算 10ms
```

三个设计要点：

- **测端到端而非组件**。缓存组件本身的读写在单测里约 0.3ms，
  但那绕过了缓存键计算和结果反序列化。验收要求针对的是用户感知的延迟。
- **冷热用同一批查询**。否则差异里会混进查询本身的难度差异（召回多少不同）。
- **用 P99 而非 P50 判定**。中位数达标而尾部超标的系统在线上依然会被投诉。

缓存键包含 `recallN`/`topK`/是否精排 —— 只用 query 做键的话，
改了参数仍会命中旧结果，评估时会得到莫名其妙的数字。

---

## 5. Phase 1 遗留案例回访

Phase 1 记录了一条失败：查询「怎么防止缓存被恶意请求打穿」
Top1 返回「缓存击穿」而非「缓存穿透」，当时归因为
「纯向量检索分不开近义概念」，并作为引入 BM25 + RRF 的实证依据。

```
$ go run -tags=backlogcheck ./scripts/eval/backlog_check.go

策略               Top1
vector-only      ✅ 正确
bm25-only        ❌ 无关
hybrid-rrf       ✅ 正确
hybrid+rerank    ✅ 正确
```

**当时的归因是错的**。纯向量检索现在能答对 —— 变的不是算法而是语料：
Phase 1 时库里只有 `docs/sample.md`，没有真正讲穿透解法的段落，
向量检索只能在现有内容里挑最接近的。那是**召回覆盖问题**，
被误读成了**排序能力问题**。已在 `phase1.md` 末尾更正。

这不影响 Phase 2 的技术决策（近义混淆在完整 golden set 上确有其事，
`confusable` 类 vector-only MRR 仅 0.250），但那个具体案例
不构成该结论的证据。

教训：**语料不完整时观察到的检索错误，不能归因于检索算法。**
要先确认「正确答案在库里」，再谈「排没排出来」——
这正是本 Phase 用 recall@50 先做分层定位的原因。

---

## 6. 交付清单

| 步骤 | 文件 | 测试 |
|------|------|------|
| 1 | `internal/eval/metrics.go` | 25 断言（Recall@K / MRR / Percentile） |
| 2 | `internal/rag/retriever/rrf.go` | RRF 融合，含并列确定性 |
| 3 | `internal/rag/tokenizer/jieba.go` | gojieba 单例 + 查询分词 |
| 3 | `internal/rag/retriever/bm25.go` | PG ts_rank 召回 + 写入 |
| 4 | `internal/rag/retriever/hybrid.go` | 融合编排 + 降级 + eino 接口 |
| 5 | `pkg/cache/query.go` | 集成测试（真 Redis） |
| 6 | `internal/rag/retriever/rerank.go` | LLM pointwise 精排 |
| — | `scripts/eval/*.go` | 评估 / 权重扫描 / 降级 / 缓存 / 回访 五个脚本 |

评估工具（build tag 隔离，不进生产构建）：

| 脚本 | tag | 用途 |
|---|---|---|
| `eval.go` | `eval` | 四策略对比，生成 `eval-results.md` |
| `build_golden.go` | `buildgolden` | 从内容谓词生成 golden set |
| `diagnose.go` | `diagnose` | 分层定位召回 vs 排序 |
| `weight_sweep.go` | `weightsweep` | 加权 RRF 权重扫描 |
| `degrade.go` | `degrade` | 双向降级验证 |
| `cache_bench.go` | `cachebench` | 端到端缓存延迟 |
| `backlog_check.go` | `backlogcheck` | Phase 1 遗留案例回访 |

---

## 7. 本阶段解决的三个非平凡问题

详见 `phase2-pitfalls.md`。与 Phase 1 不同，这三个**都不报错、不挂测试**：

1. **RRF 一致性加分在两路质量悬殊时反噬** —— 单路强命中（1/61=0.0164）
   输给双路平庸（2/70=0.0286）。加权扫描证明三类查询最优权重互相矛盾，
   静态权重是死路，正解是精排。
2. **兜底路径与主路径量纲不一致** —— 打分成功归一化到 0-1，
   失败却保留 0.016 量级的 RRF 分数，导致「兜底」精确地做到了
   它注释里说要避免的事（把可能相关的块踢到最后）。
   原测试断言「等于原值」，忠实地固化了错误行为。
3. **`temperature=0` 不等于可复现** —— 三次运行 MRR 0.800/0.810/0.802，
   而不含 LLM 的三行逐位相同。已在报告中披露分辨率，
   避免把噪声当成优化效果。

---

## 8. 诚实记录的局限

- **`confusable` 仅 8 条样本**，分辨率 0.125。该列适合看趋势，
  不能比较小于该值的差异。报告已自动按分辨率选择措辞
  （「显著超过」/「基本持平」），避免我最初写的
  「精排 0.629 追平/超过 bm25 0.625」这种把 0.004 噪声当结论的表述。
- **精排 P99 约 2s**，比不精排慢一个数量级。这是 LLM 调用的固有成本，
  当前靠并发 5 + 只精排 top-20 控制。生产环境需要配合缓存
  （命中后 1ms）才可用。
- **golden set 谓词基于关键词**，会漏掉纯同义表达的相关块，
  所有策略 Recall 一致偏低。不影响相对比较。
- **PG 中有 346 行历史孤儿数据**（旧博客主题文档），
  经核查不含 Redis/Kafka/MySQL 等技术内容，不影响本次评估的 809 个技术块。
  清理脚本 `cleanup_docs.go -stale` 已就绪，待执行。
