# syntax=docker/dockerfile:1

# 多阶段构建。实测镜像 110MB —— 规划里写的目标是 <30MB，达不到，
# 原因不在构建方式而在依赖本身（实测分层数据见 docs/deployment.md）：
#
#   alpine 基础镜像      8.1MB
#   ca-certificates 等   4.6MB
#   jieba 词典          13.8MB   ← BM25 那一路的中文分词，删了检索直接失效
#   kaho-server         41.6MB
#   kaho-mcp            42.4MB
#   kaho-worker         ≈41MB（新增，与上面两个同源依赖，未单独实测）
#
# 光是「跑得起来」的最小地基就 26.5MB，cgo 二进制又各占 40MB+
# （cgo 二进制无法静态裁剪到纯 Go 的水平）。
# 三个二进制一起打包后镜像约 150MB —— 这是「一个镜像跑三种角色」的代价，
# 换来的是三者依赖版本永远一致（见 docs/deployment.md 的取舍说明）。
# <30MB 那个目标是在选定 gojieba 之前定的，属于规划阶段的估算失误，
# 而不是实现没做到位 —— 记在这里，不悄悄改掉验收标准。

# ---- 构建阶段 ----
#
# ⚠️ 必须用与 go.mod 一致的 Go 版本。用更旧的版本会因为本项目用了
# slices/cmp（1.21+）和泛型别名而编译失败；用更新的大版本则可能引入
# 未验证过的运行时行为差异。
FROM golang:1.25-alpine AS builder

# gojieba（中文分词）是 cgo 库，需要 C 工具链。
# 这也是最终镜像不能用 scratch 的原因 —— 见下面的说明。
#
# ⚠️ g++ 不能省。gojieba 底层是 cppjieba，是 **C++** 而非 C；
# 只装 gcc 会在编译期报 `exec: "g++": executable file not found`，
# 而这个错误发生在 cgo 阶段，看起来像是环境问题而不是缺依赖。
RUN apk add --no-cache gcc g++ musl-dev

WORKDIR /src

# 模块代理做成构建参数，而不是写死。
#
# ⚠️ 容器里不会继承宿主机的 GOPROXY —— 宿主机能 go build 不代表镜像能构建。
# 默认值保持官方源，保证任何人 clone 下来都能构建；
# 在访问不到 proxy.golang.org 的网络环境下用
#   podman build --build-arg GOPROXY=https://goproxy.cn,direct .
# 覆盖即可。写死镜像源会让 CI 依赖某个第三方站点的可用性。
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

# 先只拷依赖清单再下载：只要 go.mod/go.sum 没变，
# 这一层就能命中缓存，改业务代码时不必重新拉依赖。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# -trimpath   去掉编译机的绝对路径，否则二进制里会带 /src 甚至开发机路径
# -ldflags -s 去掉符号表，-w 去掉 DWARF 调试信息，能显著减小体积
#
# ⚠️ 不能用 CGO_ENABLED=0：gojieba 依赖 cgo。
# 关掉的话编译期就会报错，而不是运行时才发现。
RUN CGO_ENABLED=1 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/kaho-server ./cmd/server && \
    CGO_ENABLED=1 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/kaho-mcp ./cmd/mcp && \
    CGO_ENABLED=1 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/kaho-worker ./cmd/worker

# 把 gojieba 的词典从 module 缓存里捞出来。
#
# ⚠️ 必须显式拷贝。gojieba 的词典路径是 init() 里用 runtime.Caller 算的
# **编译机源码路径**，运行镜像里没有 module 缓存，一调用 NewJieba 就 panic
# （见 internal/rag/tokenizer/jieba.go 的说明）。
# 这个坑编译期毫无征兆，只有真正跑起来才会暴露。
#
# 用 `go list -m` 取路径而不是写死版本号：版本一升级，写死的路径
# 就会静默失配，构建照样成功、容器照样启动失败。
RUN cp -r "$(go list -m -f '{{.Dir}}' github.com/yanyiwu/gojieba)/deps/cppjieba/dict" /out/dict

# ---- 运行阶段 ----
#
# 用 alpine 而非 scratch/distroless：
# cgo 编译出的二进制动态链接了 musl，scratch 里没有动态链接器，
# 一跑就是 "no such file or directory" —— 而这个错误极具误导性，
# 看起来像二进制不存在，实际是 loader 找不到。
FROM alpine:3.21

# ca-certificates: 调用 DashScope 等 HTTPS API 必需，
#   缺了会报 "x509: certificate signed by unknown authority"
# tzdata: 日志时间戳需要，否则容器里永远是 UTC
# libstdc++: cppjieba 是 C++，二进制动态链接了 libstdc++.so.6 与 libgcc_s.so.1。
#   ⚠️ 这个是实测踩出来的：缺了不是编译失败，而是容器一启动就刷屏
#   "Error relocating: symbol not found"（几十行 C++ 修饰符号名），
#   完全看不出是缺一个包。alpine 基础镜像默认不带 C++ 运行时。
RUN apk add --no-cache ca-certificates tzdata libstdc++ && \
    adduser -D -u 10001 kaho

WORKDIR /app

COPY --from=builder /out/kaho-server /app/kaho-server
COPY --from=builder /out/kaho-mcp /app/kaho-mcp
COPY --from=builder /out/kaho-worker /app/kaho-worker
COPY --from=builder /out/dict /app/dict
COPY config.example.toml /app/config.example.toml

# 告诉分词器去哪找词典（默认路径指向编译机源码，容器里不存在）
ENV KAHO_JIEBA_DICT_DIR=/app/dict

# ⚠️ 以非 root 运行。容器逃逸时 root 权限会直接变成宿主机权限，
# 而本服务完全不需要特权 —— 没有理由不降权。
USER kaho

EXPOSE 8080

# 健康检查用 wget 而非 curl：alpine 自带 busybox wget，
# 装 curl 要多 3MB 且没有额外价值。
#
# 注：podman 以默认的 OCI 格式构建时会忽略 HEALTHCHECK 并给出告警
# （OCI 镜像规范里没有这个字段，它是 Docker 的扩展）。
# docker build 与 `podman build --format docker` 会正常保留。
# 保留这行是因为绝大多数部署环境用得上，而被忽略时也不会造成任何损害。
HEALTHCHECK --interval=30s --timeout=3s --start-period=40s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

# config.toml 通过挂载或环境变量提供 —— 绝不打进镜像。
# 里面有 API Key，打进去等于把密钥发布到镜像仓库。
#
#   podman run -v ./config.toml:/etc/kaho/config.toml:ro \
#     -e KAHO_CONFIG=/etc/kaho/config.toml -e DASHSCOPE_API_KEY=... \
#     -p 8080:8080 kahoagent
#
# 也可以完全不用文件，全靠环境变量（见 internal/config/config.go）。
ENTRYPOINT ["/app/kaho-server"]
