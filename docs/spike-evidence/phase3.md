# Phase 3 验收证据

运行方式：

```bash
go test -tags=e2e -v -timeout 25m ./internal/app/
```

跑的是与生产**同一份**装配（`internal/app.New`），接真实 Milvus / PostgreSQL /
Redis / DashScope。只有 CloudKaho 用 `httptest` 替身 —— 该服务当前起不来，
但替身实现的是核对过的真实 wire 格式（`code` 为字符串 `"0"`、
share 收绝对时间戳），因此验证的仍是真实的协议假设。

## 为什么不是 `scripts/e2e/chat_test.sh`

计划里写的是 shell 脚本。改用 Go test 的理由：

| | shell + curl + jq | Go test |
|---|---|---|
| 前置条件 | 手工起服务、手工起 CloudKaho 替身 | `httptest` 进程内拉起 |
| 「多轮上下文生效」 | 跨进程比对状态，难 | 直接读 `Memory.Load` 断言 |
| 「首字延迟 <1.5s」 | curl 难以精确计时到第一个 delta | 边收边解析，精确到 ms |
| CI | 依赖 jq / bash，Windows 上易碎 | `go test` 无差别执行 |

验收项一条没少，且**每条都是可重跑的自动断言**，而不是人工核对输出。

## 验收结果

全部 7 项通过（验收 6 项 + 补的 file_metadata 覆盖）（`ok github.com/yurin-kami/KahoAgent/internal/app 65.253s`）。

| 验收项 | 测试 | 实测结果 |
|---|---|---|
| rag_query → 答案 + 引用来源 | `TestE2E_RAGQueryWithCitations` | ✅ 调用 `rag_query`，回答带 `来源：...md` |
| 「第二个文件多大」→ 多轮上下文 | `TestE2E_MultiTurnContext` | ✅ 答 `kafka-guide.md，24.0 KB` |
| file_metadata 可被调用 | `TestE2E_FileMetadataIsReachable` | ✅ 无前序搜索时按 ID 直查，答 `8.0 KB` |
| 分享链接 → 返回 URL | `TestE2E_FileShareReturnsURL` | ✅ 返回 presigned URL |
| 12 轮不 OOM、不丢上下文 | `TestE2E_TwelveTurnsKeepContext` | ✅ 22 条压缩至 13 条，第 1 轮事实仍可追溯 |
| SSE 逐 token，首字 <1.5s | `TestE2E_FirstTokenLatency` | ✅ **502ms**，6 个 chunk |
| Tool 报错优雅降级 | `TestE2E_ToolFailureDegradesGracefully` | ✅ 如实报错，未编造链接 |

### 首字延迟

```
first token in 502ms, 6 chunks, 963ms total
```

用**不需要工具**的问题测。带工具的请求首字延迟里包含一整轮检索
（Phase 2 实测精排就要 1.4s），那测的是检索速度而非流式是否生效 —— 两回事。
带工具时实测首字仍在 338~927ms，因为「正在检索…」之前模型已开始输出。

### 多轮上下文（关键项）

第二轮的提问里**没有任何文件名或 ID**：

```
Q: 帮我列一下我的文件
A: - redis-cache.md（2.0 KB）
   - kafka-guide.md（24.0 KB）
   - milvus-hnsw.md（8.0 KB）
   (tools=[file_search])

Q: 第二个文件多大？
A: 第二个文件是 kafka-guide.md，大小为 24.0 KB。
   (tools=[])
```

模型只能从上一轮的 `file_search` 结果知道「第二个」指 `kafka-guide.md`。
测试同时断言它**没有**答成第一个文件的 2.0 KB —— 否则序数解析错了却看起来是对的。

### 工具异常降级

注入真实 HTTP 500：

```
Q: 给第一个文件生成一个分享链接
A: 创建分享链接失败（HTTP 500: s3 unavailable）。S3 服务暂时不可用，
   无法生成下载链接。建议稍后再试……
```

断言三件事：有回答（没崩）、没有 `error` 事件、**没有编造 URL**。

---

## 过程中发现并修复的真实缺陷

### 摘要丢失用户明确要求记住的事实

**12 轮验收第一次运行时失败。**

现象：第 1 轮说「项目代号是紫水晶」，第 12 轮问代号，模型答
"你没有在对话中提及任何项目代号"。

排查过程（两次假设都被证伪，值得记下来）：

1. **假设一：压缩机制没生效。** 证伪 —— 历史从 22 条压缩到 13 条，压缩跑了。
2. **假设二：摘要提示词有问题。** 写了 `TestSummarize_KeepsFacts...` 用真实
   LLM 验证 —— **通过了**，摘要里有「紫水晶」。
3. **假设三：落盘链路丢数据。** 写了 `TestMemoryCompaction_PreservesEarlyFacts`
   走真实 Redis —— **也通过了**，摘要正确落盘且排在首位。

三次都不对，于是**不再猜**，直接在 e2e 里把压缩后的历史打出来：

```
[0] system [对话摘要] 文件名：《Redis 八股总集》；查询主题：缓存设计、
    缓存穿透、缓存击穿、缓存雪崩；已确认结论：缓存设计需系统性考虑……
```

摘要**全是 RAG 内容**，用户说的代号被完全挤掉了。

**根因**：提示词只要求保留「文件名、主题、结论」。用户自己说的项目代号
三者都不沾边，模型于是名正言顺地省略了它。而单元测试之所以通过，是因为
填充内容是「缓存是用空间换时间」这类短句 —— 信噪比太高，模型有余量把
代号顺手带上。真实场景是 10 轮密集、带引用、几百字的检索回答。

**修复**（`internal/agent/memory.go`）：给保留内容排优先级，
判据是「能不能重新拿到」——

- RAG 检索到的知识随时可以再检索一次，丢了不可惜 → 排最后，明确允许精简
- 用户亲口说过的事实（代号、偏好、约束）一旦丢掉就再也找不回来 → 排第一，
  要求逐字保留

同时**不再截断用户消息**（原先一律截到 300 字）：用户消息通常很短，
且正是最该保的内容；会挤爆上下文的是助手/工具的检索结果，只截那些。

修复后第 12 轮：`项目代号是「紫水晶」。`

### 一个测不出 bug 的"回归测试"

修复后我想给这个缺陷补一道回归防线，于是**把提示词改回旧版**验证测试是否
真的会失败 —— 结果 **3/3 次都通过**。又试了 4 轮 / 6 轮 / 10 轮填充、
换成真实 RAG 回答做噪声，仍然通过。

摘要质量对输入长度和措辞极其敏感，脱离完整链路复现不出当时的信噪比。

**结论**：`TestSummarize_KeepsFactsTheUserAskedToRemember` 只是提示词的
**冒烟检查**，不是这个缺陷的回归防线；注释里已明确写清，避免给人虚假的
安全感。真正的防线是 `TestE2E_TwelveTurnsKeepContext` —— 它是唯一实际
抓住过这个缺陷、也实际验证过修复的测试。

> 一个测不出 bug 的回归测试比没有更糟：它让人以为这里已经有防护了。

---

## 顺带的结构调整

装配逻辑从 `cmd/server/main.go` 提到 `internal/app`。

原因：装配是**最容易出错却最难测**的部分 —— 依赖注错、顺序搞反、降级分支
写漏，编译期全都发现不了。而放在 `main` 包里无法被测试 import，端到端验证
只能照抄一份装配代码，于是会出现「e2e 全绿但线上起不来」这种最坏情况。

提出来之后，e2e 跑的是与生产同一份 `app.New`。
