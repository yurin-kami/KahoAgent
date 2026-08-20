# Phase 5 验收证据

## 📊 实测数据表（简历用）

| 指标 | 实测值 | 测量方式 |
|------|--------|---------|
| Recall@5 (hybrid) | **0.346** | 77 条 golden set，`scripts/eval/eval.go` |
| Recall@5 (hybrid+rerank) | **0.444** | 同上 |
| MRR (hybrid+rerank) | **0.798** | 同上 |
| 向量检索 P50 / P99 | **151ms / 267ms** | 同上，nearest-rank |
| RRF 融合耗时 (recallN=50) | **8.5–10.3 µs** | `go test -bench`，6 次采样 |
| 端到端首字延迟 P50 | **409ms** | SSE 边收边计时，300 请求 |
| 端到端首字延迟 P99 | **1388ms** | 同上 |
| HTTP 吞吐 | **~10 req/s**（受上游配额限制，见下） | 20 req/s 开环压测 |
| MCP 工具调用吞吐 | **69.7 call/s** | 并发 50，300 次调用 |
| MCP 调用 P50 / P99 | **9ms / 4100ms** | 同上（P50 是缓存命中） |
| 缓存命中率 | **94.8%**（1137/1200） | Prometheus `kaho_cache_ops_total` |
| 常驻内存 | **23.9 MB** | pprof heap，990 请求之后 |

⚠️ **检索类指标于 2026-08-20 清理 346 行孤儿数据后重测**。
清理前的旧值（Recall@5 0.442 / MRR 0.809 等）保留在
`docs/eval-results.before-cleanup.md` 以便对照。
除 `confusable` 一列外，各项变动均在 ±0.011 以内，属噪声量级；
`confusable` 从 0.629 降到 0.546，但该列仅 8 条样本、且精排走 LLM 不确定输出，
单条查询即可造成 0.125 的摆动 —— 这是采样噪声，不是清理导致的退化。

⚠️ **每个数字的适用范围见下文**。脱离条件的单个数字没有意义。

---

## 验收项

| 验收项 | 状态 | 证据 |
|---|---|---|
| `/metrics` 能看到全部指标 | ✅ | 5 类指标均有数据，实测输出见下 |
| `docs/bench/` 下有压测结果文件 | ✅ | 6 个文件（含 pprof 二进制） |
| `docs/optimization.md` 有前后对比 | ✅ | FuseRRF −68%，含基数说明 |
| 填表 | ✅ | 上表，全部实测 |

### /metrics 实测输出

压测后抓取，节选：

```
kaho_active_sessions 0
kaho_cache_ops_total{cache="retrieval",result="hit"} 1137
kaho_cache_ops_total{cache="retrieval",result="miss"} 63
kaho_llm_tokens_total{model="qwen-plus",type="prompt"} 2105257
kaho_llm_tokens_total{model="qwen-plus",type="completion"} 490492
kaho_retrieval_duration_seconds_count{method="vector"} 63
kaho_retrieval_duration_seconds_sum{method="vector"} 9.047
kaho_retrieval_duration_seconds_count{method="bm25"} 63
kaho_retrieval_duration_seconds_sum{method="bm25"} 1.726
kaho_retrieval_duration_seconds_count{method="rerank"} 63
kaho_retrieval_duration_seconds_sum{method="rerank"} 113.486
kaho_retrieval_duration_seconds_count{method="hybrid"} 1200
```

三个可以直接读出来的结论：

1. **`active_sessions` 归零** —— 990 次请求（含 187 次失败）之后没有泄漏。
   Gauge 的 Inc/Dec 配平了，这是 `TestActiveSessions_ReturnsToZeroOnEveryPath`
   在真实负载下的印证。
2. **vector 平均 143ms、bm25 平均 27ms**（sum/count），与 Phase 2 的独立
   评估（139ms / 4ms）同量级 —— bm25 这次偏高是因为并发下 PG 有竞争。
3. **rerank 平均 1.80s**，占 hybrid 未命中路径的绝大部分。这直接说明
   优化方向是缓存和精排策略，不是 Go 代码。

---

## ⚠️ HTTP 吞吐：瓶颈不在本项目

三档速率连续测得：

| 目标速率 | 成功/失败 | 实际吞吐 | 首字 P50 | 完成 P50 |
|---------|----------|---------|---------|---------|
| 3 req/s  | 90 / 0    | 2.2 req/s  | 445ms | 7026ms |
| 10 req/s | 300 / 0   | 7.0 req/s  | 409ms | 7290ms |
| 20 req/s | 413 / 187 | 10.0 req/s | 429ms | 7242ms |

**结论：上限来自 DashScope 账号的并发配额，不是 KahoAgent 的架构容量。**

三条依据缺一不可：

1. 三档速率下**完成延迟几乎不变**（7.0 / 7.3 / 7.2 秒）。若是本服务过载，
   排队会让延迟随速率显著上升 —— 它没有。
2. 20 req/s 时吞吐**卡在 10 不再增长**，多出来的全部失败。
3. 服务端日志里 187 次失败**全部**是同一个错误，且是上游返回的：

   ```
   HTTP 429: {"error":{"message":"Too many concurrent requests.",
              "type":"Throttling.Concurrency","code":"Throttling.Concurrency"}}
   ```

换个配额更高的账号或自建模型，这个数字就会变。所以简历上若要写吞吐，
必须带上「受限于 LLM 服务商配额」这个前提，否则是在虚报架构能力。

**完成延迟 P50 7 秒也不是本服务慢** —— 那是模型生成几百字所需的时间。
真正反映服务响应能力的是首字延迟 P50 约 410ms，且它在三档速率下都稳定，
说明流式链路本身没被压力影响。

### 过载时的行为

187 次失败全部以 SSE `error` 事件返回给客户端：服务未 panic、未拒绝新连接、
未泄漏 goroutine（`active_sessions` 归零）。这是 Phase 3「上游报错时优雅降级」
在真实过载下的验证 —— 比构造的故障注入更有说服力。

---

## MCP 吞吐比 HTTP 高一个数量级的原因

| | HTTP /api/chat | MCP rag_query |
|---|---|---|
| 吞吐 | ~10 req/s | 69.7 call/s |
| 瓶颈 | 上游 LLM 生成 + 配额 | 未触及 |

MCP 的 `rag_query` **只做检索，不生成** —— 那 7 秒生成和 DashScope 的
并发配额都不在这条路径上。这本身就是把工具经 MCP 暴露出去的价值：
客户端（Claude Desktop）自己承担生成，本服务只提供检索能力。

**MCP 的 P50 9ms 不代表检索只要 9ms**：压测脚本循环使用 5 个查询，
绝大多数命中缓存。未命中的真实延迟看 P95/P99（2.5s / 4.1s）。

---

## 为什么不用 vegeta

计划里写的是 vegeta，改为自写 `scripts/bench/load.go`：

1. `/api/chat` 是 **SSE 长连接**。vegeta 测的是「请求到响应结束」的总时长，
   而 SSE 要等整段生成完才结束 —— 测出来的是**生成耗时**而非服务吞吐。
2. 真正需要分位数的是**首字延迟**，那要求边收边计时，vegeta 给不了。

自写的版本按固定速率**开环**发压（不等上一个请求返回）。开环很关键：
闭环压测在服务变慢时会自动降低发压速率，把过载现象掩盖掉 ——
而线上真实流量不会「体谅」你。正是开环才让 20 req/s 下的 429 暴露出来。

---

## 优化

见 `docs/optimization.md`。摘要：pprof 定位到 `FuseRRF` 占检索链路 43%
的分配量（每次调用建 3 个 map），改为单 map + 预分配切片后：

- recallN=50（生产默认）：耗时 −68%，分配次数 −56%
- recallN=200：耗时 −65%

**但文档里同时写清了这在端到端几乎不可感知**：省下的约 45µs 放在
139ms 向量召回 + 1.8s 精排面前微不足道。真正价值一是降低 GC 压力，
二是它证明了瓶颈不在自己的代码里 —— 这个结论比省下的 45µs 有用得多。

---

## 产物清单

```
docs/bench/
  retrieval-before.txt   优化前基准（6 次采样）
  retrieval.txt          优化后基准（6 次采样）
  http.txt               HTTP 压测（三档速率 + 归因）
  mcp.txt                MCP 压测（含读数说明）
  heap.txt               堆内存 top（23.9MB 常驻）
  heap.pprof             堆 profile 原始文件
  cpu.pprof / mem.pprof  基准采集的 profile
docs/optimization.md     优化前后对比与基数说明
```
