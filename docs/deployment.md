# 容器化与部署

> 实测环境：podman 5.x / Windows 11 + WSL2，构建于 2026-08-20。

## 镜像体积：110MB → 约 150MB，没达到规划的 30MB

> **2026-11 更新**：P1 接通异步入库后，镜像里新增了第三个二进制
> `kaho-worker`。它与 server/mcp 共用绝大部分依赖，按前两个各约 41MB
> 估算，镜像从 110MB 涨到**约 150MB**。该数字是估算而非实测 ——
> 本轮改动所在环境没有可用的容器运行时，下次真正构建时请用
> `podman history` 复核本表。取舍没变：三个角色共用一个镜像，
> 换来的是依赖版本永远一致。

`docs/IMPLEMENTATION.md` 的 Phase 6 验收项写的是「多阶段构建，最终镜像 <30MB」。
实测 **110MB**（2026-08，两个二进制）。这一节说明差距从哪来、为什么不打算硬压。

分层实测（`podman history`，2026-08）：

| 层 | 体积 | 内容 |
|---|---|---|
| alpine:3.21 | 8.1MB | 基础镜像 |
| apk add | 4.6MB | ca-certificates + tzdata + libstdc++ |
| dict | 13.8MB | gojieba 中文词典 |
| kaho-mcp | 42.4MB | MCP Server 二进制 |
| kaho-server | 41.6MB | HTTP 服务二进制 |
| kaho-worker | ≈41MB | 入库 worker 二进制（估算，未实测）|
| config.example.toml | 4.1kB | |
| **合计** | **≈151MB** | |

差距的根源是 **cgo**，而 cgo 来自 gojieba：

1. **基础镜像不能用 scratch/distroless。**
   cgo 编译出的二进制动态链接 musl 与 libstdc++，scratch 里没有动态链接器，
   启动会报 `no such file or directory` —— 看起来像二进制不存在，
   实际是 loader 找不到。这一条就锁死了 8.1MB 的地板。

2. **二进制压不下去。**
   已经用了 `-trimpath -ldflags="-s -w"`，40MB 是 cgo + Eino + Milvus SDK +
   Prometheus 客户端的合计结果。纯 Go 的等价服务通常在 15–20MB，
   但纯 Go 就意味着放弃 gojieba。

3. **词典 13.8MB 是检索质量的一部分。**
   删掉词典，BM25 那一路直接失效 —— 实测 `bm25-only` 的 MRR 是 0.720，
   `hybrid+rerank` 是 0.809；砍掉它换 13.8MB，检索质量的损失远大于收益。

**结论**：30MB 这个目标是在选定 gojieba 之前定的，属于规划阶段的估算失误。
真要达到，唯一的路是换纯 Go 分词器并重新跑一遍评估 —— 那是一次检索策略变更，
不是打包优化，不应该在 Phase 6 顺手做掉。

可以做但没做的两件事，以及不做的理由：

- **只打包一个二进制**（省 ~42MB）。HTTP 与 MCP 两种形态共用同一份镜像，
  部署时靠 entrypoint 切换；拆成两个镜像会让两者的依赖版本可能漂移，
  而这正是当初把装配逻辑收进 `internal/app` 要避免的问题。
- **UPX 压缩**（可省 50%+）。会拖慢启动、干扰 pprof 符号解析，
  而这个服务对镜像体积并不敏感（不是 Serverless 冷启动场景）。

## 构建时踩到的三个坑

都是 `go build` 完全发现不了、只有真正跑起来才暴露的问题。

### 1. 缺 g++（构建期失败）

```
github.com/yanyiwu/gojieba: exec: "g++": executable file not found in $PATH
```

gojieba 底层是 cppjieba，**C++ 而非 C**。只装 `gcc musl-dev` 不够，
必须加 `g++`。报错发生在 cgo 阶段，看起来像环境问题而不是缺依赖。

### 2. 缺 libstdc++（启动期失败）

```
Error loading shared library libstdc++.so.6: No such file or directory
Error relocating /app/kaho-mcp: _ZNSo9_M_insertImEERSoT_: symbol not found
... （几十行 C++ 修饰符号名）
```

alpine 基础镜像不带 C++ 运行时。构建阶段有 g++ 所以编译通过，
运行阶段没有 libstdc++ 于是一启动就刷屏。
**构建成功不等于跑得起来** —— 这也是 CI 里加「启动一次」这一步的原因。

### 3. 词典路径指向编译机（启动期失败）

```
panic: Dictionary file does not exist:
  github.com/yanyiwu/gojieba@v1.4.7/deps/cppjieba/dict/jieba.dict.utf8
```

gojieba 的词典路径是在 `init()` 里用 `runtime.Caller` 算出来的
**编译机源码路径**（`$GOMODCACHE/github.com/yanyiwu/gojieba@v1.4.7/deps/...`）。
运行镜像里没有 Go module 缓存，一调用 `NewJieba()` 就 panic。

加了 `-trimpath` 之后这个路径还会变成相对路径，报错看起来像
「词典文件缺失」，而真实原因是**路径基准整个错了**——极具误导性。

解法两步：

1. 构建阶段把词典从 module 缓存拷出来。用 `go list -m` 取路径而非写死版本号，
   否则依赖一升级，构建照样成功、容器照样启动失败：

   ```dockerfile
   RUN cp -r "$(go list -m -f '{{.Dir}}' github.com/yanyiwu/gojieba)/deps/cppjieba/dict" /out/dict
   ```

2. 运行阶段用 `KAHO_JIEBA_DICT_DIR` 告诉分词器去哪找
   （见 `internal/rag/tokenizer/jieba.go`）。未设置时行为完全不变，
   本地开发不需要任何额外配置。

这个开关有回归测试守着（`TestNewSegmenter_UsesCustomDictDir` 与
`TestNewSegmenter_HonorsTheEnvVar`）。后者是反证：指向不存在的目录必须失败，
否则无法区分「环境变量生效了」和「环境变量被忽略、走的还是默认路径」——
两种情况下第一个测试的结果完全一样。

## 三个角色，一个镜像

镜像里现在有三个二进制，用 `--entrypoint` 决定容器扮演什么角色：

| 角色 | 入口 | 端口 | 说明 |
|---|---|---|---|
| HTTP 服务 | `/app/kaho-server`（默认） | 8080 | 对话 + 上传 |
| MCP Server | `/app/kaho-mcp` | stdio | 给 Claude Desktop / Cursor |
| 入库 worker | `/app/kaho-worker` | 9191（指标） | 消费 Kafka，双写 Milvus/PG |

```bash
# worker：同一个镜像，换 entrypoint
docker run --entrypoint /app/kaho-worker \
  -v ./config.toml:/etc/kaho/config.toml:ro \
  -e KAHO_CONFIG=/etc/kaho/config.toml \
  -e DASHSCOPE_API_KEY=sk-xxx \
  -p 9191:9191 \
  kahoagent
```

为什么不拆成三个镜像：拆开之后，三个镜像各自的 `go.mod` 解析结果
可能在某个时间点漂移（比如重建镜像时上游发了新版本），
而 server 和 worker 对「同一条 chunk 的 ID 规则」必须完全一致 ——
这种漂移只会在入库数据上表现为极难排查的重复/缺失。
一个镜像、三个入口，版本天然锁定。

代价是镜像多了约 41MB（见上表），对非 Serverless 场景可以接受。

## 配置

`config.toml` 含 API Key，**绝不打进镜像**，由 `.dockerignore` 排除。
两种注入方式：

```bash
# 挂载配置文件
docker run -p 8080:8080 \
  -v ./config.toml:/etc/kaho/config.toml:ro \
  -e KAHO_CONFIG=/etc/kaho/config.toml \
  -e DASHSCOPE_API_KEY=sk-xxx \
  kahoagent
```

`KAHO_CONFIG` 是必要的：镜像里 `/app` 除了二进制什么都没有，
而 Kubernetes 的 ConfigMap/Secret 一般挂在 `/etc/...` 下，
路径写死就只剩「挂到工作目录」这一种用法。

密钥类配置一律优先读环境变量：`DASHSCOPE_API_KEY`、`CLOUDKAHO_TOKEN`。
`CLOUDKAHO_BASE_URL` 则是**覆盖**配置文件的值（而非留空才回退）——
指向哪个 CloudKaho 是部署环境的属性，容器里、CI 里、本地各不相同，
而 config.toml 是同一份。

## 网络受限环境

构建阶段的模块代理做成了构建参数，默认官方源：

```bash
docker build --build-arg GOPROXY=https://goproxy.cn,direct -t kahoagent .
```

不写死镜像源，是为了不让 CI 依赖某个第三方站点的可用性。
注意**容器不继承宿主机的 GOPROXY** —— 宿主机能 `go build`
不代表镜像能构建，这一点很容易想当然。

## 已验证

容器内实测（挂载真实配置 + host 网络连本地 Milvus/PG/Redis）：

| 项 | 结果 |
|---|---|
| 服务启动 | ✅ `KahoAgent listening port=8080` |
| `/healthz` | ✅ `ok` |
| `/metrics` | ✅ 指标有数据 |
| `POST /api/chat` 完整 RAG | ✅ 触发 `rag_query`，返回带出处的答案 |
| 中文分词（词典加载） | ✅ 检索链路正常，说明 BM25 那一路可用 |
| 未配 CloudKaho 时降级 | ✅ 告警后禁用文件工具，`rag_query` 仍可用 |
| `KAHO_CONFIG` 生效 | ✅ 指向不存在的路径时报该路径而非 `config.toml` |

指标片段（一次对话之后）：

```
kaho_cache_ops_total{cache="retrieval",result="hit"} 2
kaho_llm_tokens_total{model="qwen-plus",type="completion"} 533
kaho_llm_tokens_total{model="qwen-plus",type="prompt"} 2802
kaho_retrieval_duration_seconds_count{method="hybrid"} 2
```

## 备注：podman 与 HEALTHCHECK

podman 以默认的 OCI 格式构建时会忽略 `HEALTHCHECK` 并给出告警——
OCI 镜像规范里没有这个字段，它是 Docker 的扩展。
`docker build` 与 `podman build --format docker` 会正常保留。
Dockerfile 里保留这行，是因为多数部署环境用得上，而被忽略时也无害。
