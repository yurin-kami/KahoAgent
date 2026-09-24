# 2026-09-04 重新入库与评估

> 为什么这天的数字与 2026-08-20 那版不同 —— 变更了什么、影响多大、结论是什么。
>
> 旧版数据保留在 `eval-results.before-cleanup.md`（2026-08-20 清理孤儿数据之前）
> 与 git 历史中。

## 起因

支持 PDF 入库（方案见 `pdf-plan-b.md`）需要放开 `ingest_corpus` 的扩展名限制。
评审时发现 `docIDFor` 会剥掉扩展名，`guide.md` 与 `guide.pdf` 会撞成同一个
docID —— 而 `ChunkID = sha256(docID#index)`，两个存储又都是 upsert，
后入库的会**静默覆盖**先入库的。

修这个 bug 会改变所有 docID，进而改变所有 chunk ID，
而 golden set 里存的正是 chunk ID —— 所以必须重新入库、重建 golden set、重跑评估。

## 本次变更了两件事

**必须分清，否则无法归因。**

| # | 变更 | 预期影响 |
|---|---|---|
| 1 | `docIDFor` 保留扩展名 | **应为零** —— 只改 ID 的生成方式，语料内容与分块完全不变 |
| 2 | `CAP 定理` 谓词收紧 | keyword 类**轻微下降**（去掉虚高） |

### 变更 1：docID 保留扩展名

```go
// 旧：rel = strings.TrimSuffix(filepath.ToSlash(rel), filepath.Ext(rel))
// 新：rel = filepath.ToSlash(rel)   // 扩展名参与 docID
```

回归测试见 `scripts/eval/ingest_corpus_test.go`（带 `corpus` build tag）。
**验证方式是反证**：把 `TrimSuffix` 放回去，两个测试必须变红 —— 已实测确认。

### 变更 2：CAP 谓词误标修复

这是重建 golden set 时**顺带查出的既有缺陷**，与变更 1 无关。

原谓词是 `AnyOf: {"CAP"}`，而 `match()` 用 `ILIKE`（大小写不敏感），
于是命中了语料里的：

- slice 容量 `cap`
- 逃逸分析 `Es**cap**e`
- C++ 的 `hex es**cap**e sequence`

实测 `ILIKE '%CAP%'` 命中 17 块，其中含大写 `CAP` 的只有 5 块 —— **误标率近 70%**。
这违反 `build_golden.go` 开头的方法论铁律「宁严勿松」：
多标的块会让所有策略的分数一起虚高，掩盖真实差距。

**为什么不把 `ILIKE` 改成 `LIKE`**：逐个核查了 13 个大写缩写谓词，
发现 `SAGA` 在语料中**全部写作 `Saga`**，大小写敏感匹配会让它从 11 条归零。
一刀切会制造新的漏标。故改用共现收紧：

```go
AllOf: {"CAP"}, AnyOf: {"一致性", "可用性", "分区容错"}
```

CAP 条目从 16 条误标收敛到 **2 条**，人工核对两条都确实在讲 CAP 三选二。
总标注 652 → 638。

### golden set 条数 77 → 78

新增的正是 `CAP 定理` 一条，**旧 77 条全部保留，无一消失**。

它上次没进 golden set，是因为当时库里还残留着 346 行孤儿数据时的状态；
本次在干净语料上重建，谓词匹配到了。分布 confusable 8 / semantic 29 /
keyword 40 → 41。

## 结果对比

| Method | Recall@5 旧→新 | MRR 旧→新 |
|---|---|---|
| `vector-only` | 0.298 → **0.314** (+0.016) | 0.656 → **0.660** (+0.004) |
| `bm25-only` | 0.357 → **0.363** (+0.006) | 0.714 → **0.723** (+0.009) |
| `hybrid-rrf` | 0.346 → **0.358** (+0.012) | 0.698 → **0.719** (+0.021) |
| `hybrid-rrf+rerank` | 0.444 → **0.441** (−0.003) | 0.798 → **0.818** (+0.020) |

**全部在噪声量级内**（最大 +0.021，且 Phase 2 已实测 LLM 精排的重复性波动
可达 0.083，见 `phase2-pitfalls.md` 坑 3）。

### 结论

**变更 1（docID）对检索质量无影响** —— 这正是需要证明的：
修复 bug 没有引入回归。语料内容、分块结果（1307 chunks）、
文档数（77 篇）与旧版逐位一致，只有 ID 的生成方式变了。

**核心结论全部不变**：

- `bm25-only` MRR 仍高于 `hybrid-rrf`（0.723 > 0.719）—— RRF 只用排名
  不用分数的缺陷依旧存在
- 精排后反超（0.818 > 0.723，+13.2%）
- `confusable` 类仍是最难的一类

## 语料一致性保证

为确保新旧可比，本次入库排除了两篇新增文章：

```bash
go run -tags=corpus ./scripts/eval/ingest_corpus.go -reset \
  -exclude "2026-08-20-rrf-hybrid-retrieval-pitfall.md,2026-8-20-kahoagent-cgo-docker-pitfalls.md" \
  <blog-dir>
```

`-exclude` 是本次新增的 flag。存在意义：语料目录会随时间新增文件，
而「重灌是为了验证某处改动无害」这类场景下，
**语料多一篇少一篇都会让新旧数字失去可比性 —— 变量必须一次只动一个**。

dry-run 验证：77 篇 / 1307 chunks，与数据库现状逐位一致
（连分块数都相同，说明分块逻辑也无变化）。

排除的两篇正是讲 KahoAgent 自身 cgo 与 RRF 问题的技术笔记，
其中 RRF 那篇大量出现「缓存穿透/击穿/雪崩」，若入库很可能成为
`confusable` 类的新干扰项 —— 那会让本次对比失去意义。

## ⚠️ 过程中暴露的一个真实缺陷

**`ingest_corpus` 对 Milvus 的失败是静默的。**

重灌后首次评估时 `vector-only` 全部归零，且三个策略数字逐位相同。
排查发现是 DashScope 账户欠费（`code: "Arrearage"`），embedding 调不通。

问题在于：入库脚本当时报告的是

```
✅ 入库完成：77 篇 / 1307 chunks
   PostgreSQL: 1307 行
```

**它只校验了 PG 行数，没有校验 Milvus。** 双写场景只验证一路，等于没验证。

（本次实际情况是入库时 API 尚可用、数据写入成功，欠费发生在之后的评估阶段 ——
但脚本无法区分这两种情形，这正是缺陷所在。）

新增的 `scripts/spike/milvusprobe`（`-tags=milvusprobe`）做双存储一致性核对：
抽 20 条 PG 里的 chunk，用其内容反查 Milvus，看能否命中自己。
**建议把这道校验并入 `ingest_corpus` 的收尾**，让这类失败当场暴露。
已记入 `BACKLOG.md`。

### 另一条教训

首次那轮故障评估的输出里带着 `✅ 提升 +Inf%` 的验收通过字样，
且已经把 `docs/eval-results.md` 覆盖了。

**「验收通过」的标记本身不能当作验收通过。**
真正的信号是 `vector-only` 归零、三个策略数字逐位相同 ——
这两个现象在数据正常时不可能同时出现。

## 后续：这次重跑是 PDF 工作的前置

当天晚些时候实施了 PDF 支持（`docs/pdf-plan-b.md`）。
本文档记录的语料锁定（`-exclude` 锁 77 篇）和重灌，
正是 PDF 入库前主语料数字可用的前提 ——
PDF 语料、对照组、主语料三套存储的隔离与验证见
[`eval-results-pdf.md`](eval-results-pdf.md)。
