#!/usr/bin/env bash
# 端到端真机验收：上传 → Kafka → worker → Milvus + PostgreSQL → 混合检索
#
# 为什么不用真实 LLM：
#   这条链路要验证的是「文档进得去、检索得出来」，不是模型质量。
#   用本地 fakellm 桩（确定性 embedding）可以做到零外部依赖、可重复、免费；
#   对话生成那一层需要真实模型，不在本脚本的验收范围。
#
# 环境要求：
#   - Linux / WSL（gojieba 在 Windows 原生下会崩）
#   - 依赖栈已启动：bash scripts/local-stack.sh up
#
# 用法：
#   bash scripts/e2e-upload.sh
#   APP_PORT=18080 FAKE_PORT=18081 bash scripts/e2e-upload.sh
set -euo pipefail

APP_PORT="${APP_PORT:-18080}"
FAKE_PORT="${FAKE_PORT:-18081}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

TMP="$(mktemp -d)"
SERVER_LOG="$TMP/server.log"
WORKER_LOG="$TMP/worker.log"
FAKE_LOG="$TMP/fake.log"
CFG="$TMP/config.toml"
RUN_ID="$(date +%s)"
COLLECTION="kaho_e2e_${RUN_ID}"
TOPIC="kaho-e2e-${RUN_ID}"
DOC_ID="e2e-${RUN_ID}.md"
TOKEN="Kaho7749"

log() { printf '\033[36m[e2e]\033[0m %s\n' "$*"; }
fail() { printf '\033[31m[e2e] %s\033[0m\n' "$*" >&2; exit 1; }

cleanup() {
    [ -n "${SERVER_PID:-}" ] && kill "$SERVER_PID" 2>/dev/null || true
    [ -n "${WORKER_PID:-}" ] && kill "$WORKER_PID" 2>/dev/null || true
    [ -n "${FAKE_PID:-}" ] && kill "$FAKE_PID" 2>/dev/null || true
    wait 2>/dev/null || true
}
trap cleanup EXIT

cat >"$CFG" <<EOF
debug = false

[server]
port = ${APP_PORT}

[db]
dsn = "host=127.0.0.1 port=5432 user=kaho password=kaho_dev_pw dbname=kahoagent sslmode=disable"

[redis]
addr = "127.0.0.1:6379"
db = 0

[milvus]
addr = "127.0.0.1:19530"
collection = "${COLLECTION}"

[llm]
api_key = "e2e-placeholder"
base_url = "http://127.0.0.1:${FAKE_PORT}/v1"
model = "fake-chat"
embedding_model = "fake-embedding"
embedding_dim = 1024

[kafka]
brokers = ["127.0.0.1:9092"]
topic = "${TOPIC}"
group_id = "kaho-e2e-worker-${RUN_ID}"
dlq_topic = "${TOPIC}.dlq"
max_attempts = 3

[otel]
enabled = false
EOF
export KAHO_CONFIG="$CFG"
# Milvus 自身也监听 9091；worker 的指标端口必须避开
export KAHO_WORKER_METRICS_ADDR="127.0.0.1:${METRICS_PORT:-18092}"
export GOCACHE="${GOCACHE:-/tmp/kaho-e2e-gocache}"
export GOMODCACHE="${GOMODCACHE:-$ROOT/.gomodcache}"
export GOFLAGS="${GOFLAGS:--buildvcs=false}"
export GOPROXY="${GOPROXY:-off}"

log "building binaries"
mkdir -p "$TMP/bin"
go build -o "$TMP/bin/kaho-server" ./cmd/server
go build -o "$TMP/bin/kaho-worker" ./cmd/worker
go build -tags=e2echeck -o "$TMP/bin/fakellm" ./scripts/e2e/fakellm
go build -tags=e2echeck -o "$TMP/bin/query" ./scripts/e2e/query

# 先建 topic 再起 worker：broker 自动创建是 3 分区，而且消费者
# 可能在 topic 出现前就缓存了「不存在」的元数据，表现为长时间空转。
log "creating kafka topic ${TOPIC}"
"$TMP/bin/query" -create-topic "$TOPIC" || fail "create topic failed"
"$TMP/bin/query" -create-topic "${TOPIC}.dlq" || fail "create dlq topic failed"

log "starting fake embedding server on :${FAKE_PORT}"
"$TMP/bin/fakellm" -addr "127.0.0.1:${FAKE_PORT}" -dim 1024 >"$FAKE_LOG" 2>&1 &
FAKE_PID=$!
for _ in $(seq 1 30); do
    curl -fsS "http://127.0.0.1:${FAKE_PORT}/healthz" >/dev/null 2>&1 && break
    sleep 0.3
done
curl -fsS "http://127.0.0.1:${FAKE_PORT}/healthz" >/dev/null || fail "fakellm 未就绪"

log "starting server on :${APP_PORT} and worker"
"$TMP/bin/kaho-server" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
"$TMP/bin/kaho-worker" >"$WORKER_LOG" 2>&1 &
WORKER_PID=$!

for _ in $(seq 1 45); do
    curl -fsS "http://127.0.0.1:${APP_PORT}/healthz" >/dev/null 2>&1 && break
    sleep 1
done
curl -fsS "http://127.0.0.1:${APP_PORT}/healthz" >/dev/null || {
    tail -20 "$SERVER_LOG" >&2
    fail "server 未就绪"
}

DOC="$TMP/doc.md"
cat >"$DOC" <<EOF
# ${TOKEN} 验收文档

本项目的端到端验收代号是 ${TOKEN}。

缓存穿透的推荐解法是布隆过滤器加空值缓存；缓存击穿则用互斥锁重建。
EOF

log "uploading ${DOC_ID}"
UP="$(curl -fsS -X POST "http://127.0.0.1:${APP_PORT}/api/upload" \
    -F "doc_id=${DOC_ID}" -F "file=@${DOC}")" || fail "上传失败"
echo "$UP"
echo "$UP" | grep -q '"status":"accepted"' || fail "上传未被受理"

log "waiting for worker to ingest (最多 90s)"
INGESTED=0
for _ in $(seq 1 90); do
    if grep -q "message processed" "$WORKER_LOG" 2>/dev/null &&
        grep -q "$DOC_ID" "$WORKER_LOG" 2>/dev/null; then
        INGESTED=1
        break
    fi
    if grep -q "moved to dead-letter queue" "$WORKER_LOG" 2>/dev/null; then
        tail -30 "$WORKER_LOG" >&2
        fail "消息进了死信队列"
    fi
    sleep 1
done
[ "$INGESTED" = 1 ] || {
    tail -30 "$WORKER_LOG" >&2
    fail "worker 未在 90s 内完成入库"
}
log "ingested: $(grep "$DOC_ID" "$WORKER_LOG" | tail -1)"

# Milvus 默认 bounded consistency，写入后到可检索有短暂延迟
sleep 3

log "querying retrieval chain directly (绕过 LLM)"
"$TMP/bin/query" -query "$TOKEN" -expect "$DOC_ID" -topk 5

log "PASS: upload → kafka → worker → milvus+pg → hybrid retrieval"
