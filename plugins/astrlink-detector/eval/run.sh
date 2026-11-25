#!/usr/bin/env bash
# Reproduce the AstrLink Guard evaluation (plugins/astrlink-detector/eval/REPORT.md).
#
#   ORT_LIB=/path/to/libonnxruntime.1.27.0.dylib \
#   ASTRLINK_DIR=/path/to/astrlink-guard/int8 \
#   [ASTRLINK_FP32_DIR=/path/to/astrlink-guard/fp32] \
#   plugins/astrlink-detector/eval/run.sh [all|samples|tokenizer|parity|eval|scan|perf]
#
# The end-to-end check through a real gatehouse is e2e/g19_astrlink_test.go.
#
# ORT_LIB: ONNX Runtime >= 1.26 (onnxruntime_go v1.31.0 uses C API 26).
# ASTRLINK_DIR: model_int8.onnx + model_int8.onnx_data + tokenizer.json + config.json
# from huggingface.co/QuantumNous/astrlink-guard (int8/). Model files and the
# runtime library are not part of the repository.
# tokenizer/parity need Python with `tokenizers` (and `onnxruntime`, `numpy`).
set -euo pipefail
cd "$(dirname "$0")"
: "${ORT_LIB:?set ORT_LIB}" "${ASTRLINK_DIR:?set ASTRLINK_DIR}"
BIN=${BIN:-$(mktemp -d)/bin}
PY=${PY:-python3}
mkdir -p "$BIN" results
step=${1:-all}

build() {
  go build -o "$BIN/tokdump" ./cmd/tokdump
  go build -o "$BIN/nereval" ./cmd/nereval
}

# Python scripts run from an empty directory so nothing next to the model
# files can shadow a module.
pyrun() { (cd "$(mktemp -d)" && "$PY" "$@"); }

build
here=$PWD
case $step in all|samples)
  "$PY" -I gen/gen_samples.py data
  git diff --stat -- data ;; esac
case $step in all|tokenizer)
  pyrun "$here/gen/tok_compare.py" "$ASTRLINK_DIR/tokenizer.json" "$BIN/tokdump" "$here/data" ;; esac
case $step in all|parity)
  pyrun "$here/gen/ort_compare.py" "$ASTRLINK_DIR" model_int8.onnx "$BIN/nereval" "$ORT_LIB" "$here/data" ;; esac
case $step in all|eval)
  "$BIN/nereval" -ort "$ORT_LIB" -model "$ASTRLINK_DIR" -mode eval -data data -out results/eval-int8-plain >/dev/null
  "$BIN/nereval" -ort "$ORT_LIB" -model "$ASTRLINK_DIR" -mode eval -wordvote -data data -out results/eval-int8-wordvote >/dev/null
  if [ -n "${ASTRLINK_FP32_DIR:-}" ]; then
    "$BIN/nereval" -ort "$ORT_LIB" -model "$ASTRLINK_FP32_DIR" -mode eval -wordvote -data data -out results/eval-fp32-wordvote >/dev/null
  fi ;; esac
case $step in all|scan)
  (cd ../../.. && git ls-files 'internal/*.go' 'cmd/*.go' 'plugin/*.go' 'docs/*.md' | grep -v '_test.go\|\.pb\.go' | sed "s#^#$PWD/#") \
    | xargs "$BIN/nereval" -ort "$ORT_LIB" -model "$ASTRLINK_DIR" -mode scan -wordvote -min 0.7 | tail -1 ;; esac
case $step in all|perf)
  for t in 0 1 2 4; do
    "$BIN/nereval" -ort "$ORT_LIB" -model "$ASTRLINK_DIR" -mode cold
    "$BIN/nereval" -ort "$ORT_LIB" -model "$ASTRLINK_DIR" -mode perf -wordvote -threads $t -data data > results/perf-int8-threads$t.json
  done ;; esac
