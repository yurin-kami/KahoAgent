#!/usr/bin/env bash
# 本地依赖栈启动脚本（podman 版，用于 WSL / podman machine）
#
# 为什么不用 docker-compose.yml：
#   compose 文件里每个服务都写了 mem_limit，而 podman 的 WSL 机器
#   没有启用 cgroup memory 控制器，启动时会报：
#     crun: open `memory.max` for writing: No such file or directory
#   这个脚本用 podman run 直接起容器，不设内存上限 —— 本地开发
#   本来也不需要硬限制。
#
# 用法：
#   bash scripts/local-stack.sh up      # 启动全部依赖
#   bash scripts/local-stack.sh down    # 停止并删除容器（保留数据卷）
#   bash scripts/local-stack.sh status  # 查看状态
#
# 依赖：podman 5.x；镜像已在本机（否则会走网络拉取）。
set -euo pipefail

# ⚠️ Git Bash（MSYS2）会把形如 /tmp/xxx 的参数自动转成 Windows 路径
# （/tmp/kraft-combined-logs → D:/Temps/kraft-combined-logs），
# 容器里就变成了一个不存在的盘符路径。关掉这个转换。
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL="*"

NET=kaho-net
PREFIX=kaho

# ⚠️ podman 默认把宿主机的 HTTP(S)_PROXY 注入容器。
# 本项目所在机器开着 127.0.0.1:7897 的代理，而容器里的 127.0.0.1
# 是容器自己 —— Milvus 的 gRPC 客户端会尝试走代理，报
# "dial tcp 127.0.0.1:7897: connect: connection refused"。
# 容器之间走的是 podman 网络，不需要代理，统一关掉。
RUN_OPTS=(--http-proxy=false)

log() { printf '\033[36m[stack]\033[0m %s\n' "$*"; }
die() { printf '\033[31m[stack] %s\033[0m\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"; }

wait_port() {
    local host=$1 port=$2 tries=${3:-60}
    for ((i = 0; i < tries; i++)); do
        if (echo >"/dev/tcp/$host/$port") >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

ensure_network() {
    podman network exists "$NET" 2>/dev/null || podman network create "$NET" >/dev/null
}

start_container() {
    local name=$1; shift
    podman rm -f "$name" >/dev/null 2>&1 || true
    podman run -d --name "$name" --network "$NET" "${RUN_OPTS[@]}" "$@" >/dev/null
    log "started $name"
}

up() {
    need podman
    ensure_network

    # ── Milvus 的两个依赖 ──────────────────────────────
    start_container "${PREFIX}-etcd" \
        -p 2379:2379 \
        quay.io/coreos/etcd:v3.5.5 \
        etcd -advertise-client-urls=http://127.0.0.1:2379 \
        -listen-client-urls http://0.0.0.0:2379 --data-dir /etcd

    start_container "${PREFIX}-minio" \
        -p 9000:9000 -p 9001:9001 \
        -e MINIO_ACCESS_KEY=minioadmin -e MINIO_SECRET_KEY=minioadmin \
        docker.io/minio/minio:RELEASE.2023-03-20T20-16-18Z \
        server /minio_data

    log "waiting for etcd / minio ..."
    wait_port 127.0.0.1 2379 30 || die "etcd 未就绪"
    # minio 的 9000 端口在容器内部监听，这里只等容器状态
    sleep 3

    # ── 向量库 ────────────────────────────────────────
    start_container "${PREFIX}-milvus" \
        -p 19530:19530 -p 9091:9091 \
        --security-opt seccomp=unconfined \
        -e ETCD_ENDPOINTS="${PREFIX}-etcd:2379" \
        -e MINIO_ADDRESS="${PREFIX}-minio:9000" \
        docker.io/milvusdb/milvus:v2.4.15 \
        milvus run standalone

    # ── 关系库（BM25 + 会话外的业务数据） ─────────────
    start_container "${PREFIX}-postgres" \
        -p 5432:5432 \
        -e POSTGRES_USER=kaho -e POSTGRES_PASSWORD=kaho_dev_pw \
        -e POSTGRES_DB=kahoagent \
        -v "${PREFIX}_pg_data:/var/lib/postgresql/data" \
        docker.io/library/postgres:16-alpine

    # ── 缓存 ──────────────────────────────────────────
    start_container "${PREFIX}-redis" \
        -p 6379:6379 \
        docker.io/library/redis:7-alpine \
        redis-server --save 60 1 --loglevel warning

    # ── 消息队列（KRaft，单节点） ─────────────────────
    start_container "${PREFIX}-kafka" \
        -p 9092:9092 \
        -e KAFKA_NODE_ID=1 \
        -e KAFKA_PROCESS_ROLES=broker,controller \
        -e KAFKA_LISTENERS=PLAINTEXT://:9092,CONTROLLER://:9093 \
        -e KAFKA_ADVERTISED_LISTENERS=PLAINTEXT://localhost:9092 \
        -e KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER \
        -e KAFKA_LISTENER_SECURITY_PROTOCOL_MAP=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT \
        -e KAFKA_CONTROLLER_QUORUM_VOTERS=1@localhost:9093 \
        -e KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR=1 \
        -e KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR=1 \
        -e KAFKA_TRANSACTION_STATE_LOG_MIN_ISR=1 \
        -e KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS=0 \
        -e KAFKA_NUM_PARTITIONS=3 \
        -e KAFKA_AUTO_CREATE_TOPICS_ENABLE=true \
        -e KAFKA_LOG_DIRS=/tmp/kraft-combined-logs \
        docker.io/apache/kafka:3.8.0

    log "waiting for ports ..."
    wait_port 127.0.0.1 5432 60 || die "postgres 未就绪"
    wait_port 127.0.0.1 6379 30 || die "redis 未就绪"
    wait_port 127.0.0.1 9092 60 || die "kafka 未就绪"
    # Milvus 冷启动要 20~40 秒
    wait_port 127.0.0.1 19530 90 || die "milvus 未就绪"

    # 建表幂等，可重复执行
    podman exec -i "${PREFIX}-postgres" \
        psql -U kaho -d kahoagent <"$(dirname "$0")/../migrations/001_chunks.sql" >/dev/null
    log "schema applied"

    log "ready: milvus :19530 | pg :5432 | redis :6379 | kafka :9092"
}

down() {
    for n in etcd minio milvus postgres redis kafka; do
        podman rm -f "${PREFIX}-${n}" >/dev/null 2>&1 || true
    done
    podman network rm "$NET" >/dev/null 2>&1 || true
    log "containers removed (volumes kept)"
}

status() {
    podman ps -a --filter "name=${PREFIX}-" --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"
}

case "${1:-up}" in
up) up ;;
down) down ;;
status) status ;;
*) die "用法: $0 {up|down|status}" ;;
esac
