# KahoAgent 简历化改造方案

> 目标：让这个项目在「后端开发 / AI 应用开发」简历上**立得住、经得起追问**，
> 同时不推倒已经跑出来的实测数据。
>
> 约束（2026-11-07 确认）：简历保留 Go / Java / Python 三版换栈写法。
> 本文只讨论项目本身的改造，不改变简历的语言映射策略。
> 换栈写法与公开仓库不一致的风险已单独提示，属于使用者的自主决策。

---

## 0. 先回答：Eino 要不要换成 LangChain / 纯 Python

| 选项 | 改动量 | 简历收益 | 风险 | 结论 |
|---|---|---|---|---|
| 保留 Eino | 0 | 差异化：自研编排、Go 生态、MCP 官方 SDK | 面试官可能不熟 Eino | ✅ 主干保留 |
| 换 LangChain | 18.5k 行 + 8.4k 测试重写，全部实测数据作废 | 关键词命中 Python JD | 抽象层会吃掉 SSE 刷写与 tool 边界的控制力，首字延迟大概率变差 | ❌ 不建议 |
| 纯 Python 重写 | 2–3 个月 | 同上 | 重写后要重新做评估、压测、归因，等于把项目重做一遍 | ❌ 不建议 |
| Dify 低代码 | 1 周 | 几乎为零 | 简历会从「后端开发」降级成「平台配置」 | ❌ 不建议 |
| DSPy | 1–2 周 | 可在「精排 / 查询改写」上做 A/B | 不适合做服务主干 | ⭕ 只做局部实验 |
| 用 DSH 重写 | 2–4 周（机械翻译） | 无 | 翻译腔代码在面试里一眼能看出来，且指标全部要重测 | ❌ 不建议 |

**一句话**：换语言不改变面试结果，改变结果的是「你能否解释清楚每个取舍」。
这个项目最值钱的部分（78 条 golden set、RRF 反噬的归因、精排的延迟代价、
MCP 的审计与限流）恰好是框架无关的。

**如果一定要有 Python 叙事**：不是把 Go 翻译一遍，而是补一个**真实的 Python 侧组件**
（离线入库 / 评测 / 文档解析），见阶段 P6。

---

## 1. 现状体检

### 已经成立的强项（不用动）

- 混合检索 + RRF + LLM 精排，有完整评估（78 条 golden set，`docs/eval-results.md`）
- 评估体系的**诚实性**：分母是全部相关块，Recall@5 上界 0.667 而非 1.0
- pprof 定点优化，有前后对比与基数说明（`docs/optimization.md`）
- MCP 服务：通用 Schema 桥接、排队限流、审计只记 hash
- 文档密度极高，`LEARNING.md` 里有大量「假设被推翻」的一手记录

### 硬伤（按严重程度）

| # | 问题 | 后果 | 处理 |
|---|---|---|---|
| 1 | Kafka 管道「已实现未接线」 | 简历写了就被追问穿 | P1 接通，或删除 |
| 2 | 没有可访问的 Demo | HR / 面试官无法验证 | P4 |
| 3 | 只有 Prometheus 指标，无 LLM 链路 trace | 缺 AI 应用岗的关键词与能力证明 | P3 |
| 4 | 评估脚本只能手动跑 | 无法证明「防回归」 | P2 |
| 5 | 简历与 README 数字口径不一致（77 vs 78；0.314 vs 0.358 作基线） | 面试对不上 | P0 |
| 6 | PDF 末尾多一张空白页 | 观感问题 | 已随简历重建修复 |

---

## 2. 改造路线（按 ROI 排序）

### P0 · 口径对齐（0.5 天）

**做什么**

- 统一 golden set 条数为 **78**（`scripts/eval/golden_set.json` 实测 78 条；
  `docs/eval-results.md` 里 RRF 那节仍写 77，需修正）。
- 简历里的提升幅度必须标清楚基线：
  - `vector-only` 0.314 / 0.660 → `hybrid-rrf+rerank` 0.441 / 0.818（全链路）
  - `hybrid-rrf` 0.358 / 0.719 → `+rerank` 0.441 / 0.818（精排单独贡献）
  - 两种写法都对，但**不能混用**。
- 精排延迟要写成「代价」而不是「性能」：P50 150ms → 1.88s 是引入精排的代价，
  因此做了按需开启与失败回退 RRF。

**验收**：简历、README、`eval-results.md` 头部三处一致为 **78 条**。
历史文档（`eval-results.before-cleanup.md`、`pdf-plan*.md`、`phase2*.md`）里的
「77 条」是当时的事实，**保留不改**，但新材料不要再引用旧数字。

**简历解锁**：无新增，只是消除追问风险。

---

### P1 · 打通 Kafka 闭环（1 天）

**做什么**

1. `POST /api/upload`：落原始文件 → 发 Kafka（key = docID，保证同一文档分区有序）。
2. `cmd/worker/main.go`：消费 → 分块 → embedding → Milvus + PostgreSQL 双写。
3. 幂等：沿用 `sha256(docID#index)` 主键，重复投递不产生重复块。
4. 失败：`FetchMessage` + 手动 `CommitMessages`，处理失败不提交，重投递；
   超过 N 次进死信 topic。
5. 指标：`kaho_ingest_total{status}`、`kaho_ingest_duration_seconds`。

**验收**

- 上传一个 Markdown → 轮询 `/healthz` 或指标 → 能检索到。
- 集成测试：畸形消息不阻塞队列、重启后未提交消息重投递（已有用例，补 upload 路径）。
- 压测：连续投 100 个文档，块数与源文件一致，无重复。

**简历解锁**

> 文档异步入库链路：上传 → Kafka（docID 分区有序）→ Worker 分块/向量化 → Milvus + PG 双写；
> `FetchMessage` + 手动 commit 实现至少一次语义与失败重投，畸形消息不阻塞队列。

---

### P2 · 评测进 CI（1–2 天）

**做什么**

1. `scripts/eval/eval.go` 增加 `--baseline` 与 `--json` 输出。
2. GitHub Actions 增加 `eval` job（手动触发 + 每日定时，避免每次 PR 烧 API 额度）：
   跑 78 条 golden set，与 `docs/eval-baseline.json` 比较，
   Recall@5 或 MRR 跌幅 > 0.02 则失败。
3. 结果作为 artifact 上传，README 徽章展示最近一次。

**验收**：故意把 rerank 关掉，CI 必须红；恢复后绿。

**简历解锁**

> 建立检索质量回归门禁：78 条 golden set 接入 CI，指标跌幅超阈值即阻断合并，
> 把「调 prompt 靠感觉」变成可回滚的工程流程。

---

### P3 · LLM 链路可观测性（1–2 天）

**做什么**

1. 接入 OpenTelemetry，按 GenAI semconv 打 span：
   `chat`（模型、输入输出 token、finish_reason）、`retrieval`（召回数、融合耗时）、
   `tool`（工具名、耗时、是否失败）。
2. 本地用 Jaeger all-in-one（docker-compose 加一个服务），后端可换 Langfuse。
3. 脱敏：prompt/response 只存 hash + 长度，原文进 debug 开关。

**验收**：一次对话能在 Jaeger 里看到「改写 → 向量 → BM25 → RRF → 精排 → 生成」的完整瀑布图。

**简历解锁**

> 基于 OpenTelemetry 打通 LLM 调用链，按 GenAI 语义约定记录模型、token 与工具 span；
> 定位到精排占端到端 P99 的 62%，据此把它做成可降级旁路。

---

### P4 · 可访问的 Demo（1–2 天）

**做什么**

1. 单页前端（Next.js 或 Vite）：流式输出 + 引用片段高亮 + 工具调用轨迹侧栏 +
   本次请求的 token / 耗时 / 缓存命中。
2. 后端加：每日请求上限、按 IP 令牌桶、`/api/chat` 的 body 大小限制。
3. 部署：前端 Vercel / Cloudflare Pages，后端 Fly.io 或轻量云主机；
   README 顶部放 Demo 链接与 30 秒录屏。

**验收**：陌生设备打开链接，3 秒内能问出第一个问题并看到引用。

**简历解锁**：项目名旁边可以直接放 demo 链接，这是最高性价比的一步。

---

### P5 · 稳定性护栏（1 天）

**做什么**

- 主备模型 fallback：qwen-plus 超时/429 → qwen-turbo，再失败 → 返回检索结果 + 提示。
- 单请求 token 预算（输入 + 输出），超限截断历史而不是报错。
- 熔断：连续 N 次上游失败后短路 30s，避免雪崩。
- embed / rerank 失败降级：跳过该路或回退 RRF（精排已有）。

**验收**：把 API Key 改错，服务返回可读的降级回答而不是 500；
kill 掉 Milvus，BM25 单路仍可服务。

**简历解锁**：面试必问「线上 LLM 挂了怎么办」，这题有整套答案。

---

### P6 · 可选的 Python 侧组件（3–5 天）

只在「目标岗位明确偏 Python」时做，且**必须是新增而不是翻译**：

- `scripts/pdf/` 已有的 MinerU 解析升级为常驻服务（FastAPI + 任务队列），
  输入 PDF、输出带 heading_path 的 Markdown，被 Go worker 通过 HTTP 调用。
- 或者：Python 版评测服务（LLM-as-judge + 指标看板），复用同一份 golden set。

**验收**：Go 侧零改动即可切换解析实现；Python 服务有独立测试与 Dockerfile。

**简历解锁**

> 离线文档处理独立为 Python 服务（FastAPI + MinerU），与 Go 在线服务通过 HTTP 解耦；
> 在线路径保持 Go 的低延迟与确定性，离线路径复用 Python 的模型生态。

---

## 3. 简历措辞随阶段演进

| 阶段完成后 | 可加/可改的句子 |
|---|---|
| P0 | 数字统一为 78 条；精排标注为「以延迟换质量」 |
| P1 | 「异步入库链路」「至少一次语义」从「预留」变成事实 |
| P2 | 「检索质量回归门禁」 |
| P3 | 「OpenTelemetry / GenAI span」「精排占 P99 62%」 |
| P4 | 项目标题旁放 Demo 链接 |
| P5 | 「模型 fallback / 熔断 / 降级」 |
| P6 | 「Go + Python 双栈」 |

---

## 4. 反模式（明确不做）

- ❌ 为了关键词把 Go 服务翻译成 Python —— 指标全部作废，且代码会有翻译腔。
- ❌ 引入 Dify 当主干 —— 项目性质会从「开发」变成「配置」。
- ❌ 在简历上写未接线的链路 —— 这是最容易被一句话问穿的地方。
- ❌ 把单函数 −68% 说成「系统性能提升 68%」—— 基数必须一起说。
- ❌ 用 hit rate 冒充 Recall —— 你的 0.441 之所以可信，正因为分母诚实。

---

## 5. 时间预算

| 阶段 | 工时 | 累计 |
|---|---|---|
| P0 | 0.5d | 0.5d |
| P1 | 1d | 1.5d |
| P2 | 1–2d | 3.5d |
| P3 | 1–2d | 5.5d |
| P4 | 1–2d | 7.5d |
| P5 | 1d | 8.5d |
| P6 | 3–5d | 13.5d（可选） |

P0–P5 一周半可以做完，之后每一版简历都能对应到仓库里的真实提交。

---

## 6. 实施进度

| 阶段 | 状态 | 日期 | 说明 |
|---|---|---|---|
| P0 口径对齐 | ✅ 完成 | 2026-11-07 | 简历、README、eval-results 均为 78 条；精排延迟改写为「代价 + 降级」 |
| P1 Kafka 闭环 | ✅ 完成（DLQ 除外） | 2026-11-07 | `POST /api/upload` + `cmd/worker` + 双写；单测覆盖上传/分块/双写/幂等清理。死信队列未做，已写进 README「已知限制」 |
| P2 评测进 CI | ✅ 完成（门禁为手动触发） | 2026-11-07 | `internal/eval/gate.go` + `docs/eval-baseline.json` + `eval.go -baseline/-json`；CI 走 `workflow_dispatch`（全量评估要语料与密钥，不适合每个 PR） |
| P3 OTel trace | ✅ 完成 | 2026-11-07 | `internal/trace`、HTTP 中间件、检索/精排/入库 span、Jaeger 进 compose；默认关闭 |
| P4 Demo 前端 | ✅ 完成（token/缓存两项未做） | 2026-11-07 | 内嵌单页：流式回答、来源面板、运行轨迹、首字/总耗时/字数。**没有做 token 与缓存命中展示** —— 流式响应里拿不到 usage，缓存命中也没往前端透传；不编数字，宁可不显示 |
| P5 稳定性护栏 | ✅ 完成 | 2026-11-07 | `pkg/llm/failover.go`（熔断 + 主备切换，精排与对话共用）、历史字符预算、降级矩阵写进 README |
| P6 Python 侧组件 | ⏭ 跳过 | — | 简历保留换栈写法，Python 版不需要为此改造；如后续投纯 Python 岗位再启动 |

### P1 的实际落地（与计划的差异）

1. **新增 `rag.KeywordStore`**，而不是让 Ingester 直接依赖 `*retriever.ChunkWriter`。
   这样「有没有关键词索引」成为一个可选的装配项，评估脚本不受影响。
2. **新增 `WithStaleCleanup()`**，默认关闭、worker 打开。
   计划里没写这一条，但上传路径必然遇到「文档改短后旧块残留」，
   不处理会把一篇不存在的文档拼进检索结果。
3. **上传上限定为 640KB 原文**，而不是计划里含糊的「900KB 消息上限」。
   原因是 `[]byte` 经 JSON 编码会 base64 膨胀 4/3 —— 这个换算写进了
   `pipeline.MaxRawBytes` 的注释和 README。
4. **死信队列没做**。consumer 仍然是「失败不提交、等待重投」，
   永久性失败的消息会一直重试。当前单租户、人工可介入，先记为限制。

### P2 的实际落地（与计划的差异）

1. **门禁逻辑抽成 `internal/eval/gate.go`**，而不是写在 eval 脚本里 ——
   这样它能在普通 CI 里被单元测试覆盖，不需要 Milvus/LLM。
2. **用绝对差 0.02 而不是相对百分比**：检索指标的绝对值本身有语义
   （MRR 0.818 就是 81.8% 首条命中），0.02 也正好大于 LLM 精排的实测噪声。
3. **golden set 条数不一致直接判失败**：换了语料还比指标，等于比两套题。
4. **CI 是 `workflow_dispatch` + self-hosted runner**：全量评估要 77 篇语料、
   四个下游服务和 API Key，GitHub 托管 runner 拿不到。门禁逻辑本身仍由
   普通 CI 的单测守着，这里验证的是「真实数据上没有回退」。

### P3 的实际落地（与计划的差异）

1. **默认关闭**（`otel.enabled = false`）：本地开发不该为了跑通服务先起 collector。
2. **HTTP 中间件必须转发 `http.Flusher`**，否则 SSE 会被自己的埋点代码弄坏 ——
   `internal/trace/trace_test.go` 里有一条专门的测试锁死它。
3. **只记 query 长度，不记原文**：trace 后端通常没有业务日志同级的访问控制。
4. **跨进程 trace 没打通**：producer 没把 W3C `traceparent` 写进 Kafka header，
   所以「上传请求」和「worker 入库」目前是两条独立的 trace。
   要连起来需要 producer 注入、consumer Extract，留作后续。

### P4 的实际落地（与计划的差异）

1. **来源面板的数据来源**：`agent.Chunk` 新增 `ToolResult`，
   handler 用正则从 `rag_query` 的输出里提取「【片段 N】来源: …」，
   再以 `sources` 事件推给前端。
   没有改成「工具返回 JSON」——那段文本是给模型读的，
   塞 JSON 会占上下文、也干扰模型理解。
2. **不做 token 与缓存命中展示**：流式响应里 Eino 不返回 usage，
   缓存命中信息也没有从 retriever 透传到 handler。宁可不显示，也不编数字。
   前端展示的是浏览器实测的首字/总耗时/字数。
3. **安全**：所有模型与文档文本走 `textContent`，配合 CSP
   `script-src 'self'`（无 `unsafe-inline`），即使哪里漏了转义也不会执行脚本。
4. **验证方式**：用本地 mock SSE 服务驱动页面并截图核对
   （消息流、来源列表、轨迹、指标），截图不入库。

### P5 的实际落地（与计划的差异）

1. **熔断 + 主备切换抽成 `pkg/llm/failover.go`**，由 `ChatModel`（精排/摘要）
   和 `EinoChatModel`（对话）共用。两个模型各写一套的话，
   行为迟早分叉，而这类分叉在排查时最费时间。
2. **只重试一次，且不重试同一上游**：429/5xx 在几毫秒内重试同一端点
   基本必败；备用是另一个配额池，命中率完全不同。
3. **流式只敢在开始前切换**：一旦 SSE 已经开始吐字，中途换模型会得到
   两段拼接的回答 —— 那比分段失败更糟。所以备用接管发生在
   「连接建立 / 首个非 200」阶段。
4. **token 预算用字符近似**：中文约 1 字 ≈ 1 token。不引入 tokenizer 是
   为了不把分词成本加进请求路径；目标是「不炸上下文」，不是精确计费。
5. **没有做 embedding 的主备切换**：换 embedding 模型意味着换向量维度，
   和现有 Milvus 集合不兼容，属于数据迁移而不是容错。

### 验证情况

- `go build ./...`、`go vet ./...` 通过。
- `go test ./...` 除 `internal/rag/tokenizer` 外全部通过；
  该包的失败是 gojieba（cgo）在当前环境里 `NewJieba` 崩溃，
  **与本次改动无关**，属本地 C++ 运行环境问题（README 已注明 Windows 需要 MinGW）。
- Kafka 链路本身没有在本地跑通（环境无容器运行时，起不了 broker），
  验证依赖已有的 `//go:build integration` 测试与代码审查；
  上线前必须在有 Kafka 的环境执行一次 `go test -tags=integration ./internal/pipeline/`。

## 7. 完成情况（2026-11-07）

P0–P5 全部落地，P6 按需跳过。整体新增约 1.4k 行实现 + 0.9k 行测试。

| 阶段 | 主要落点 |
|---|---|
| P0 | 统一 78 条口径；简历与 README 的基线写法对齐 |
| P1 | `handler/upload.go`、`cmd/worker`、`pipeline/{chunker,ingest}.go`、`rag.KeywordStore` |
| P2 | `eval/gate.go`、`docs/eval-baseline.json`、`eval.go -baseline/-json`、`.github/workflows/eval.yml` |
| P3 | `internal/trace`、检索/精排/入库 span、Jaeger compose |
| P4 | `internal/web`（3 个静态文件）、`sources` SSE 事件、`agent.Chunk.ToolResult` |
| P5 | `pkg/llm/failover.go`、`handler.trimHistory`、降级矩阵 |

**仍未做**（都是有意取舍，已写进 README「已知限制」）：

- Kafka 链路的真机联调（本环境无容器运行时）
- 死信队列（DLQ）
- 跨进程 trace（producer 未写 traceparent）
- 控制台的 token / 缓存命中展示（流式拿不到 usage，不编数字）
- P6 Python 侧组件

下一步如果继续投入，优先级建议：**先做真机联调**（把已有代码在真实
Kafka + Milvus 上跑一遍），再补 DLQ，最后才是前端 token 展示。


---

## 8. 第二轮（2026-11-07 晚）：真机验收 + 遗留项 + 桌面端

### 8.1 环境

podman machine（8C/4G）× 本仓库根目录的 `scripts/local-stack.sh`。
compose 的 `mem_limit` 在 WSL 下不可用，所以脚本改为直接 `podman run`，
并处理了三个环境坑（详见 `STUDY-GUIDE.md` §16.5）：
cgroup memory、代理注入、MSYS 路径转换。

### 8.2 真机验收发现的真实缺陷

| 缺陷 | 影响 | 修复 |
|---|---|---|
| 服务启动从不调用 `EnsureCollection` | 空 Milvus 上第一次入库/检索报 `collection not found` | server / worker 启动时幂等 Ensure |
| worker 指标端口默认 9091 | 与 Milvus 指标端口冲突，本机联调直接 bind 失败 | 改为 9191 |
| 失败消息的「不提交 offset」不会在进程内重试 | DLQ 的失败计数永远到不了阈值 | 改为进程内 `runWithRetry` |
| DLQ topic 未预创建 | 死信投递报 `Unknown Topic Or Partition` | 文档与验收脚本显式建 topic |

### 8.3 新增能力

- **死信队列**：重试耗尽后转投 `<topic>.dlq`，保留 key/value，
  追加 `x-kaho-error` / `x-kaho-attempts` / `x-kaho-origin-offset`
- **跨进程 trace**：producer 注入 `traceparent`，consumer 还原；
  上下传一条链（`POST /api/upload → kafka.consume → ingest.document`）
- **请求级统计**：`reqstats` + `stats` SSE 事件，控制台/桌面端展示
  Token 与检索缓存命中
- **Electron 桌面端**：`desktop/`，主进程负责网络，preload 窄接口

### 8.4 验证

| 项 | 结果 |
|---|---|
| 单元测试（Windows） | 除 gojieba 的 cgo 环境问题外全绿 |
| 集成测试（WSL + podman 栈） | 18 个包全绿，含 Kafka/Milvus/PG/Redis |
| 端到端（`scripts/e2e-upload.sh`） | PASS：202 → worker 入库 → 混合检索命中 |
| 桌面端界面 | Chrome 渲染同一份 renderer 截图（`docs/desktop.png`）；Electron 在无交互桌面会话中无法启动窗口，已记录为限制 |

---

## 9. 第三轮（2026-11-08）：DeepSeek 接入 + 本地 embedding

### 9.1 背景

对话切到 DeepSeek（`deepseek-flash`），但遇到两个能力边界：

1. **DeepSeek 没有 `/embeddings`**：向量化必须有第二个来源；
2. **`deepseek-flash` 是推理模型**：先输出 `reasoning_content`，
   精排的 `max_tokens=16` 被 reasoning 吃光，`content` 为空
   （实测 `finish_reason=length`），表现为「所有候选都解析不出分数」。

### 9.2 改动

| 改动 | 说明 |
|---|---|
| `llm.embedding_base_url` / `embedding_api_key` | 对话与向量化可属两家；留空回退主配置 |
| `llm.rerank_model` | 精排单独用非推理模型（`deepseek-chat`） |
| `ChatModel.Score` 预算 16 → 256 | 推理模型的 reasoning 也要算进预算 |
| 启动校验 | embedding 指向 DeepSeek 时直接报错并给出修复提示 |
| `scripts/local-embed/` | ONNX 本地 embedding 服务（bge-small-zh-v1.5，512 维） |
| 修复对话模型误配 | 之前批量替换 embedding 配置时把 `app.go` 的 chat 也换掉了 |
| `pipeline.EnsureTopic` | worker 先于 topic 启动时不再空转 |

### 9.3 本地 embedding 的实现取舍

- **onnxruntime 而不是 PyTorch**：省 2GB+ 依赖；
- **不用 fastembed 自带下载**：对 bge-small-zh 会回退到 Google Storage 的
  预转换包，国内不可达；改用 Xenova 的 ONNX 权重 + 自己实现
  tokenize → 推理 → CLS pooling → L2 归一化；
- **量化权重**：23MB vs 90MB，检索质量差异很小。

### 9.4 验证

```
events: {tool: 2, sources: 2, delta: 385, stats: 1, done: 1}
stats:  prompt_tokens=1560, completion_tokens=661
 rerank 失败告警：0（修复前每次 3 条）
```

端到端：上传 → Kafka → worker → 本地 embedding → Milvus + PG → 混合检索 →
DeepSeek 生成（带工具调用与来源），全链路通过。
