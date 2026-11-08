#!/usr/bin/env bash
# 下载 ONNX 版 embedding 模型到本地目录。
#
# 为什么不用 fastembed 自带的下载：
#   fastembed 对部分模型（如 BAAI/bge-small-zh-v1.5）会回退到
#   storage.googleapis.com 上的预转换包，而该域名在国内不可达。
#   Xenova 仓库直接提供 ONNX 权重，走 HuggingFace 即可。
#
# 用法：
#   bash scripts/local-embed/download_model.sh
#   MODEL_REPO=Xenova/multilingual-e5-small MODEL_DIR=models/e5-small \
#     bash scripts/local-embed/download_model.sh
set -euo pipefail

cd "$(dirname "$0")"

REPO="${MODEL_REPO:-Xenova/bge-small-zh-v1.5}"
DIR="${MODEL_DIR:-models/bge-small-zh-v1.5}"
BASE="${HF_BASE:-https://huggingface.co}/${REPO}/resolve/main"

mkdir -p "$DIR"

fetch() {
    local path="$1" out="$2"
    if [ -s "$out" ]; then
        echo "[model] exists: $out"
        return
    fi
    echo "[model] downloading $path"
    curl -fL --retry 3 --connect-timeout 15 -o "$out.part" "$BASE/$path"
    mv "$out.part" "$out"
}

# 量化版约 25MB，精度损失对检索影响很小；需要更高精度可换成 onnx/model.onnx
fetch "onnx/model_quantized.onnx" "$DIR/model.onnx"
fetch "tokenizer.json" "$DIR/tokenizer.json"
fetch "tokenizer_config.json" "$DIR/tokenizer_config.json"
fetch "config.json" "$DIR/config.json"

echo "[model] ready in $DIR"
ls -lh "$DIR"
