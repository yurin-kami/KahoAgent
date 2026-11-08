#!/usr/bin/env python3
"""本地 OpenAI 兼容 embedding 服务。

为什么需要：
    DeepSeek 只提供 chat，没有 /embeddings。RAG 的向量化必须另找来源。
    这个服务在本地跑一个小模型（ONNX Runtime），对外暴露 OpenAI 的
    /v1/embeddings 协议 —— 项目代码零改动，只要把 config.toml 的
    embedding_base_url 指过来。

为什么直接用 onnxruntime + tokenizers，而不是 fastembed：
    fastembed 对 bge-small-zh-v1.5 这类模型会回退到 Google Storage 上的
    预转换包，国内不可达；而 Xenova 仓库本身就有 ONNX 权重。
    直接用 onnxruntime 只需要实现「tokenize → 推理 → CLS 池化 → 归一化」
    四步，反而少一层版本耦合。
"""

import json
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np
import onnxruntime as ort
from tokenizers import Tokenizer

MODEL_DIR = os.environ.get("MODEL_DIR", os.path.join(os.path.dirname(__file__), "models", "bge-small-zh-v1.5"))
HOST = os.environ.get("EMBED_HOST", "127.0.0.1")
PORT = int(os.environ.get("EMBED_PORT", "11435"))
MAX_LEN = int(os.environ.get("EMBED_MAX_LEN", "512"))
BATCH = int(os.environ.get("EMBED_BATCH", "16"))

_tok = Tokenizer.from_file(os.path.join(MODEL_DIR, "tokenizer.json"))
_tok.enable_truncation(max_length=MAX_LEN)
# BGE 系列的 pad token 是 id 0；用 tokenizer 里的 [PAD] 更稳妥
_tok.enable_padding(pad_id=0, pad_token="[PAD]")

_sess = ort.InferenceSession(
    os.path.join(MODEL_DIR, "model.onnx"),
    providers=["CPUExecutionProvider"],
)
_input_names = {i.name for i in _sess.get_inputs()}

# ONNX Runtime 的 session 可以并发，但 tokenizer 的 batch 操作不是线程安全的；
# 本地单用户场景用一把锁最省心。
_lock = threading.Lock()


def embed(texts):
    out = []
    with _lock:
        for start in range(0, len(texts), BATCH):
            batch = texts[start : start + BATCH]
            encs = _tok.encode_batch(batch)
            ids = np.array([e.ids for e in encs], dtype=np.int64)
            mask = np.array([e.attention_mask for e in encs], dtype=np.int64)

            feeds = {"input_ids": ids, "attention_mask": mask}
            if "token_type_ids" in _input_names:
                feeds["token_type_ids"] = np.zeros_like(ids)

            hidden = _sess.run(None, feeds)[0]
            # [B, T, H] → 取 CLS；已经是 [B, H] 就直接用
            vec = hidden[:, 0, :] if hidden.ndim == 3 else hidden
            # L2 归一化：后续用 COSINE 相似度，归一化后内积等价余弦
            norm = np.linalg.norm(vec, axis=1, keepdims=True)
            norm[norm == 0] = 1.0
            out.extend((vec / norm).astype(np.float32).tolist())
    return out


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _json(self, code, payload):
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/healthz":
            self._json(200, {"status": "ok", "model_dir": MODEL_DIR})
        else:
            self._json(404, {"error": {"message": "not found"}})

    def do_POST(self):
        if self.path != "/v1/embeddings":
            self._json(404, {"error": {"message": "not found"}})
            return

        length = int(self.headers.get("Content-Length", "0"))
        try:
            req = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError as e:
            self._json(400, {"error": {"message": f"invalid json: {e}"}})
            return

        inputs = req.get("input", [])
        if isinstance(inputs, str):
            inputs = [inputs]
        if not inputs:
            self._json(400, {"error": {"message": "input must not be empty"}})
            return

        try:
            vectors = embed(inputs)
        except Exception as e:  # pragma: no cover
            self._json(500, {"error": {"message": str(e)}})
            return

        self._json(
            200,
            {
                "object": "list",
                "model": req.get("model", "local"),
                "data": [
                    {"object": "embedding", "index": i, "embedding": v}
                    for i, v in enumerate(vectors)
                ],
                "usage": {"prompt_tokens": 0, "total_tokens": 0},
            },
        )

    def log_message(self, fmt, *args):
        print(f"[embed] {fmt % args}", flush=True)


def main():
    dim = len(embed(["dim probe"])[0])
    print(f"[embed] model_dir={MODEL_DIR} dim={dim} listen={HOST}:{PORT}", flush=True)
    ThreadingHTTPServer((HOST, PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
