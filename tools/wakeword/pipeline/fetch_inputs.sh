#!/bin/sh
# Fetch every reviewed input into DATA (default ~/mirrin-wakeword-clean/data) from its
# fixed-revision upstream. Only the files listed in DATA-LICENCES.md; nothing else.
# Verify afterwards with: python3 -B tools/wakeword/lint_licences.py --data "$DATA"
# (the derived per-voice Kokoro files and extracted subsets are made by prepare_data.py).
set -eu
DATA=${DATA:-$HOME/mirrin-wakeword-clean/data}
mkdir -p "$DATA/raw" "$DATA/tts/kokoro" "$DATA/tts/piper" "$DATA/frontend" "$DATA/google"
get() { [ -s "$2" ] || curl -sSL --retry 5 -C - -o "$2" "$1"; echo "ok $2"; }

# Speech, noise and room responses (OpenSLR static archives)
for f in 12/test-clean.tar.gz 12/test-other.tar.gz 12/dev-clean.tar.gz 12/dev-other.tar.gz \
         12/train-clean-100.tar.gz 17/musan.tar.gz 28/rirs_noises.zip; do
  get "https://www.openslr.org/resources/$f" "$DATA/raw/$(basename "$f")"
done

# Kokoro v1.0 (ONNX export of hexgrad/Kokoro-82M) and its voice pack
KR=https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0
get "$KR/kokoro-v1.0.onnx" "$DATA/tts/kokoro/kokoro-v1.0.onnx"
get "$KR/voices-v1.0.bin" "$DATA/tts/kokoro/voices-v1.0.bin"

# Piper voices whose lineage is permissive (see DATA-LICENCES.md), pinned to one revision
PR=https://huggingface.co/rhasspy/piper-voices/resolve/c10ece1aade47bb51c153c893d14e5bf8e5b7117/en
for v in en_US/libritts/high en_US/ljspeech/high en_US/kristin/medium en_US/john/medium \
         en_US/norman/medium en_GB/cori/high en_GB/cori/medium; do
  name=$(echo "$v" | awk -F/ '{print $1"-"$2"-"$3}')
  get "$PR/$v/$name.onnx" "$DATA/tts/piper/$name.onnx"
  get "$PR/$v/$name.onnx.json" "$DATA/tts/piper/$name.onnx.json"
done

# openWakeWord front-end (v0.5.1 release assets) and Google's speech_embedding/1 for the
# provenance check in verify_frontend.py
OW=https://github.com/dscripka/openWakeWord/releases/download/v0.5.1
get "$OW/melspectrogram.onnx" "$DATA/frontend/melspectrogram.onnx"
get "$OW/embedding_model.onnx" "$DATA/frontend/embedding_model.onnx"
get "https://tfhub.dev/google/speech_embedding/1?tf-hub-format=compressed" "$DATA/google/speech_embedding_1.tar.gz"
echo FETCH_DONE
