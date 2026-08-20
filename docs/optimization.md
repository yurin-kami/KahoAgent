# 性能优化记录

## 优化 1：FuseRRF 的分配量

### 怎么发现的

先跑基准，再采 profile —— 不靠猜：

```bash
go test -bench=BenchmarkHybridSearch/recall=200 -benchmem -run=NONE \
  -memprofile=docs/bench/mem.pprof ./internal/rag/retriever/
go tool pprof -top -nodecount=15 docs/bench/mem.pprof
```

```
Showing nodes accounting for 693.98MB, 98.11% of 707.35MB total
      flat  flat%   sum%        cum   cum%
  305.61MB 43.20% 43.20%   306.11MB 43.28%  retriever.FuseRRF          ← 43%
  288.82MB 40.83% 84.04%   288.82MB 40.83%  retriever.recallAll.func1
   99.55MB 14.07% 98.11%   408.66MB 57.77%  retriever.Search
```

`recallAll.func1` 那 40% 是**假召回器造的测试数据**，不是真实开销 ——
真实场景里那部分数据来自 Milvus/PG 的响应解析。所以唯一值得动的是 `FuseRRF`。

### 根因

原实现每次调用建 **3 个 map**，外加每个 ID 一个 `Sources` 切片：

```go
scores := make(map[string]float64)      // 1
sources := make(map[string][]string)    // 2
for _, r := range rankings {
    seen := make(map[string]bool, len(r.IDs))   // 3（每路一个）
    ...
    sources[id] = append(sources[id], r.Name)   // 每个 ID 一次扩容
}
out := make([]FusedResult, 0, len(scores))
for id, score := range scores { ... }           // 再遍历一遍 map
```

map 的每次插入都可能触发桶分配与再散列，而这里的数据规模（几十到几百条）
根本不需要三个 map。

### 改法

只保留**一个** map 用于 ID→下标定位，其余数据直接落在预分配的切片里：

```go
total := 0
for _, r := range rankings { total += len(r.IDs) }   // 上界，一次分配到位

out := make([]FusedResult, 0, total)
index := make(map[string]int, total)
lastSeen := make([]int, 0, total)   // 代替「每路一个 seen map」
```

`lastSeen[i]` 记录 `out[i]` 上次被哪一路计过分，同路重复即跳过。
`Sources` 的容量直接给路数（一个 ID 最多被每路各命中一次），避免扩容。

### 效果

`recallN=50`（**生产默认值**）：

| | 优化前 | 优化后 | 变化 |
|---|---|---|---|
| FuseRRF 耗时 | 27.6–31.6 µs | 8.5–10.3 µs | **−68%** |
| FuseRRF 分配 | 16,984 B / 124 次 | 10,856 B / 55 次 | **−36% / −56%** |
| HybridSearch 耗时 | 87.8–95.1 µs | 46.7–52.5 µs | **−46%** |
| HybridSearch 分配 | 59,161 B / 154 次 | 44,341 B / 131 次 | **−25% / −15%** |

`recallN=200`：FuseRRF 耗时 111–126 µs → 39–44 µs（**−65%**），分配 432 → 205 次。

原始数据：`docs/bench/retrieval-before.txt` 与 `docs/bench/retrieval.txt`
（各 6 次采样）。

### ⚠️ 这个数字在端到端里意味着什么

**几乎可以忽略。**

省下的约 45 µs，放在整条检索链路里是这个量级：

| 环节 | 实测耗时 |
|---|---|
| 向量召回（Milvus + 向量化） | ~135 ms |
| BM25 召回（PostgreSQL） | ~4 ms |
| **RRF 融合** | **~0.03 ms** |
| LLM 精排 | ~1,400 ms |

即端到端 P50 从约 1,430 ms 变成约 1,430 ms —— 用户完全感知不到。

写清楚这一点，是因为「优化了 68%」这种说法脱离基数就是误导。
它真正的价值在两处：

1. **分配次数减半**降低 GC 压力，高并发时才体现（50 QPS 下每秒少 5 万次分配）
2. 它证明了瓶颈**不在**我们自己的代码里 —— 真正的耗时是外部调用与 LLM，
   这个结论本身比省下的 45 µs 有用得多：它说明继续优化 Go 代码是浪费时间，
   要提速就得从缓存命中率和精排策略下手（Phase 2 的缓存实测已带来 273 倍差距）。

### 正确性

改动动了去重逻辑，所以先确认原有 8 个 RRF 测试全过，再补了
`TestFuseRRF_DeduplicatesAcrossInterleavedRankings` ——
覆盖「同一 ID 既在单路内重复、又跨路出现」这个组合，
原有测试要么只测单路重复、要么只测跨路，碰不到它。

新实现的正确性**依赖遍历顺序**（`lastSeen` 单调前进），
而顺序依赖最容易在后续重构（改并发融合、按 ID 分组）里被无声破坏，
所以值得单独钉一个测试。

---

## 没有做的优化，以及为什么

### sonic 替换 encoding/json

计划里提到过。**没做**，因为 profile 里 JSON 序列化根本没进前 15 ——
本项目的 JSON 负载是几 KB 级别的检索结果，不是热点。
为一个测不出差异的地方引入依赖，属于负收益。

### 缓存键的字符串拼接

`BenchmarkCacheKey` 显示 ~330 ns / 185 B / 6 次分配。
相对单次检索的 135 ms，占比 0.0002%。不值得动。
