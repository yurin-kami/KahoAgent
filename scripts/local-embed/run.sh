#!/usr/bin/env bash
# 启动本地 embedding 服务（OpenAI 兼容 /v1/embeddings）。
#
# 首次运行会：创建 Python 3.12 venv → 安装 onnxruntime/tokenizers →
# 下载 ONNX 模型（约 25MB）。
#
#   bash scripts/local-embed/run.sh
#   EMBED_PORT=11435 MODEL_DIR=models/bge-small-zh-v1.5 bash scripts/local-embed/run.sh
set -euo pipefail

cd "$(dirname "$0")"

DIR="${MODEL_DIR:-models/bge-small-zh-v1.5}"
export MODEL_DIR="$DIR"

# 国内网络建议走镜像；海外环境可以删掉
export HF_ENDPOINT="${HF_ENDPOINT:-https://hf-mirror.com}"

if [ ! -x .venv/bin/python ]; then
    echo "[embed] creating venv (python 3.12) ..."
    uv venv --python 3.12 .venv
    echo "[embed] installing dependencies ..."
    uv pip install --python .venv/bin/python -r requirements.txt
fi

if [ ! -s "$DIR/model.onnx" ]; then
    bash ./download_model.sh
fi

exec .venv/bin/python serve.py
