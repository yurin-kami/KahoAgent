# 本地 embedding 服务

一个零依赖（除 onnxruntime）的 OpenAI 兼容 `/v1/embeddings` 服务，
用来补上 DeepSeek 没有 embedding 接口的缺口。

## 为什么需要它

RAG 的检索链路必须有向量化，而 DeepSeek 只提供 chat。
两种接法：

1. 用另一家云服务（DashScope / SiliconFlow）——需要第二个 key；
2. **本地跑一个小模型**——这个目录就是方案 2。

模型默认 `BAAI/bge-small-zh-v1.5`（中文，512 维，ONNX 量化后约 23MB）。

## 用法

```bash
bash scripts/local-embed/run.sh
# 首次会：uv 创建 python3.12 venv → 安装 onnxruntime/tokenizers
#         → 从 HuggingFace 下载 ONNX 权重（约 23MB）

curl -s http://127.0.0.1:11435/healthz
# {"status": "ok", "model_dir": "models/bge-small-zh-v1.5"}
```

WSL 里没有 `uv` 时先装：

```bash
curl -LsSf https://astral.sh/uv/install.sh | sh
```

## 接到项目里

`config.toml`：

```toml
[milvus]
collection = "kaho_documents_bge512"   # 维度变了必须换 collection

[llm]
api_key   = "sk-..."                   # DeepSeek（仅 chat）
base_url  = "https://api.deepseek.com"
model     = "deepseek-chat"

embedding_base_url = "http://127.0.0.1:11435/v1"
embedding_api_key  = "local"
embedding_model    = "bge-small-zh-v1.5"
embedding_dim      = 512
```

> 维度必须与模型实际输出一致（bge-small-zh 是 512）。
> 换模型时 Milvus collection 也要换：同一 collection 不能混用不同维度。

## 设计取舍

| 选择 | 原因 |
|---|---|
| onnxruntime 而不是 PyTorch | 省 2GB+ 依赖，冷启动快 |
| 直接用 tokenizers + CLS pooling | fastembed 对部分模型会回退到 Google Storage 的预转换包，国内不可达 |
| `model_quantized.onnx` | 23MB vs 90MB，检索质量差异很小 |
| 单进程 + 一把锁 | 本地单用户；避免 tokenizer 的线程安全问题 |
| CLS pooling + L2 归一化 | BGE 系列的标准做法，与 COSINE 距离配合 |

## 限制

- 只有 CPU 推理，单条 512 token 约几毫秒；批量入库时是吞吐瓶颈之一
- 没有鉴权（只监听 127.0.0.1）
- 500 多行文本会截断到 512 token（`EMBED_MAX_LEN` 可调）
