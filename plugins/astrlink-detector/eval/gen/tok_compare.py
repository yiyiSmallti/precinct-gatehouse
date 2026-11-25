#!/usr/bin/env python3
"""Compare the Go tokenizer (cmd/tokdump) with HF tokenizers, token by token.

Inputs: every text of data/*.jsonl (request segments included) plus the
edge cases below. Reports the number of texts whose ids or offsets differ and
prints the first few differences.

Usage: python3 tok_compare.py <tokenizer.json> <tokdump-binary> [data_dir]
Needs: pip install tokenizers (tested with 0.22.2).
"""
import json
import os
import subprocess
import sys

from tokenizers import Tokenizer

EDGE = [
    "", " ", "  leading spaces", "trailing   ", "a\tb\nc", "1\n  x", "12  \n\n 34", "x\r\ny", "1\r\n2",
    "Ｈｅｌｌｏ　世界①②", "™ ﬁ ½ ㎏ Ⅻ", "é café", "zero​width", "​start", "nbsp here",
    "emoji 👍🏽 family 👨‍👩‍👧 flag 🇨🇳", "<s>x</s> <mask> y <pad><unk>", "tab\t\tafter 9\t\tdigits",
    "中文标点，。！？「」『』…—", "ＡＢＣ１２３", "Ǆ ǅ ǆ ﬀ ﬃ", "　　全角空格开头", "x y z\u0085w",
    "日本語のテキストとカタカナ", "한국어 텍스트", "Ελληνικά και русский", "عربى", "ab" * 300, "1 2 3 4 5 6 7 8 9 0",
    "▁already▁meta", "a\u0000b", "﻿bom", "\x7f\x01ctrl",
]


def main():
    tj, bin_ = sys.argv[1], sys.argv[2]
    data = sys.argv[3] if len(sys.argv) > 3 else os.path.join(os.path.dirname(__file__), "..", "data")
    items = [{"id": f"edge-{i}", "text": t} for i, t in enumerate(EDGE)]
    for name in ("positives.jsonl", "negatives.jsonl", "requests.jsonl"):
        for line in open(os.path.join(data, name), encoding="utf-8"):
            r = json.loads(line)
            if "segments" in r:
                items += [{"id": f"{r['id']}/{k}", "text": s["text"]} for k, s in enumerate(r["segments"])]
            else:
                items.append({"id": r["id"], "text": r["text"]})
    tok = Tokenizer.from_file(tj)
    payload = "".join(json.dumps(x, ensure_ascii=False) + "\n" for x in items).encode()
    got = subprocess.run([bin_, "-tokenizer", tj], input=payload, capture_output=True, check=True).stdout.decode()
    got = {d["id"]: d for d in map(json.loads, got.splitlines())}
    bad, ntok = 0, 0
    for x in items:
        e = tok.encode(x["text"])
        ids, offs = e.ids[1:-1], [list(o) for o in e.offsets[1:-1]]
        ntok += len(ids)
        g = got[x["id"]]
        if g["ids"] != ids or g["offsets"] != offs:
            bad += 1
            if bad <= 8:
                print("DIFF", x["id"], repr(x["text"][:80]))
                for k in range(max(len(ids), len(g["ids"]))):
                    a = (ids[k], offs[k], e.tokens[k + 1]) if k < len(ids) else None
                    b = (g["ids"][k], g["offsets"][k]) if k < len(g["ids"]) else None
                    if a is None or b is None or a[:2] != b:
                        print("   at", k, "hf", a, "go", b)
                        break
    print(f"texts={len(items)} tokens={ntok} mismatched_texts={bad}")
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
