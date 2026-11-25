#!/usr/bin/env python3
"""Compare per-token logits of the Go pipeline (nereval -mode tags) with
Python onnxruntime + HF tokenizers on single-window texts.

Usage: python3 ort_compare.py <model_dir> <onnx file name> <nereval binary> <ort lib> [data_dir]
"""
import json
import os
import subprocess
import sys

import numpy as np
import onnxruntime as ort
from tokenizers import Tokenizer


def main():
    mdir, onnx, bin_, lib = sys.argv[1:5]
    data = sys.argv[5] if len(sys.argv) > 5 else os.path.join(os.path.dirname(__file__), "..", "data")
    tok = Tokenizer.from_file(os.path.join(mdir, "tokenizer.json"))
    sess = ort.InferenceSession(os.path.join(mdir, onnx), providers=["CPUExecutionProvider"])
    items = []
    for name in ("positives.jsonl", "negatives.jsonl"):
        for line in open(os.path.join(data, name), encoding="utf-8"):
            r = json.loads(line)
            if len(tok.encode(r["text"]).ids) <= 512:
                items.append({"id": r["id"], "text": r["text"]})
    items = items[::7]  # every 7th sample keeps the run short
    payload = "".join(json.dumps(x, ensure_ascii=False) + "\n" for x in items).encode()
    out = subprocess.run([bin_, "-ort", lib, "-model", mdir, "-mode", "tags"], input=payload, capture_output=True, check=True).stdout
    got = {d["id"]: np.array(d["logits"], dtype=np.float32) for d in map(json.loads, out.decode().splitlines())}
    maxdiff, argmax_diff, ntok = 0.0, 0, 0
    for x in items:
        ids = np.array([tok.encode(x["text"]).ids], dtype=np.int64)
        lg = sess.run(["logits"], {"input_ids": ids, "attention_mask": np.ones_like(ids), "token_type_ids": np.zeros_like(ids)})[0][0]
        g = got[x["id"]].reshape(lg.shape)
        maxdiff = max(maxdiff, float(np.abs(g - lg).max()))
        argmax_diff += int((g.argmax(-1) != lg.argmax(-1)).sum())
        ntok += lg.shape[0]
    print(f"texts={len(items)} tokens={ntok} max_abs_logit_diff={maxdiff:.5f} argmax_mismatch_tokens={argmax_diff} python_ort={ort.__version__}")


if __name__ == "__main__":
    main()
