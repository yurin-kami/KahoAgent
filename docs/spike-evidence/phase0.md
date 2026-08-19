# Phase 0 验收证据

日期：2026-08-19
分支：`phase0-spike`
环境：Windows 10 + Podman 5.8.2 (WSL2, 6GB)

---

## 依赖服务

```
$ podman ps --filter name=kaho- --format "{{.Names}}\t{{.Status}}"
kaho-etcd       Up 14 minutes (healthy)
kaho-minio      Up About a minute (healthy)
kaho-postgres   Up About a minute (healthy)
kaho-redis      Up About a minute (healthy)
kaho-kafka      Up About a minute (healthy)
kaho-milvus     Up About a minute (healthy)
```

✅ 6/6 healthy

### 遇到的问题：docker.io 直连失败

```
Error: pinging container registry registry-1.docker.io:
Get "https://registry-1.docker.io/v2/": EOF
```

**解法**：从 `docker.m.daocloud.io` 拉取后 tag 成标准名，compose 文件保持不变。
脚本化到 `scripts/pull-images.ps1`。

---

## S1 · LLM Chat API

```
$ go run -tags=spike scripts/spike/llm/main.go

provider = dashscope
base_url = https://dashscope.aliyuncs.com/compatible-mode/v1
model    = qwen-plus

✅ LLM 连通
回复: RAG（Retrieval-Augmented Generation，检索增强生成）是一种将外部知识检索
      与大语言模型生成能力相结合的技术：在生成回答前，先从可信知识库中检索相关
      文档片段，再将其作为上下文输入模型，从而提升回答的准确性、时效性和可追溯性。
耗时: 1.816s | tokens: prompt=15 completion=64
```

✅ 通过

---

## S2 · Embedding API

```
$ go run -tags=spike scripts/spike/embed/main.go

embedding_model = text-embedding-v3
configured dim  = 1024

✅ Embedding 连通
实际维度: 1024  (耗时 268ms)

余弦相似度验证:
  猫 vs 狗   = 0.7033  (都是宠物，应较高)
  猫 vs 汽车 = 0.6038  (无关，应较低)
```

✅ 通过 — 维度与配置一致，语义相似度符合预期

**记录**：`text-embedding-v3` 实测 **1024 维**，`config.toml` 的 `embedding_dim = 1024` 正确。
Phase 1 建 Milvus collection 时用这个值。

---

## S3 · Milvus 读写

```
$ go run -tags=spike scripts/spike/milvus/main.go

connecting to 127.0.0.1:19530 ...
✅ 连接成功
✅ Collection 创建成功
✅ 插入 3 条并 Flush
✅ HNSW 索引创建并加载

✅ 检索成功 (耗时 13.422ms)
命中 2 条:
  1. id=a  score=1.0000
  2. id=c  score=0.9939

预期: 1. id=a (1.0000)   2. id=c (~0.9939)
```

✅ 通过 — 排序与分数完全符合预期

**踩过的两个坑已验证**：
1. `Insert` 后必须 `Flush()`，否则数据在内存 buffer 搜不到
2. 搜索前必须 `LoadCollection()`，否则报错

---

## S4 · MCP 协议

```
$ go test -tags=spike ./scripts/spike/mcp/ -v -run TestMCPHandshake

=== RUN   TestMCPHandshakeAndToolCall
    smoke_test.go:48: ✅ tools/list: hello — 向指定的人打招呼。当用户想要问候某人时使用。
    smoke_test.go:73: ✅ tools/call: Hi, kami!
--- PASS: TestMCPHandshakeAndToolCall (4.08s)
PASS
ok  github.com/yurin-kami/KahoAgent/scripts/spike/mcp  6.176s
```

✅ 通过 — initialize 握手 / tools/list / tools/call 全部正常

**改进**：写了自动化 smoke test（`smoke_test.go`），不依赖 Claude Desktop 也能验证协议层。
比手工在 Claude Desktop 里点更可重复，且能进 CI。

---

## 学习自测

| 编号 | 问题 | 答案要点 |
|------|------|---------|
| L0.1 | 为什么不能把整个文档丢给 LLM？ | 上下文窗口有限、成本高、长文本中间信息会被忽略（lost-in-the-middle） |
| L0.2 | 为什么「把一整本书 embed 成一个向量」是错的？ | 语义被平均掉，检索时什么都匹配不上 |
| L0.3 | Milvus 两个必踩的坑？ | Insert 后不 Flush 搜不到；搜索前不 Load 报错 |
| L0.4 | MCP stdio 模式最致命的坑？ | stdout 是协议通道，日志必须走 stderr |
| L0.5 | 为什么用 Eino 而非自己撸？ | ReAct 循环的状态管理/流式/重试很繁琐；但检索器自己实现，保持掌控力 |

---

## 结论

Phase 0 全部验收通过，可以进入 Phase 1。

**关键产出给 Phase 1 用**：
- Embedding 维度确定为 **1024**（Milvus schema 要用）
- Milvus 完整操作流程已验证（vectorstore 实现可直接照抄 spike 代码）
- 6 个依赖服务全部可用
