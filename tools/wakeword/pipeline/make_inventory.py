"""Print the DATA-LICENCES.md JSON inventory for the files under DATA, hashing each one.

  make_inventory.py [DATA]  > inventory.json

The reviewed facts (licence, source, evidence, attribution) live in the table below;
this script only adds checksums. Changing a row is a provenance change: review it.
"""
import hashlib
import json
import sys
from pathlib import Path

from common import DATA, KOKORO_ALL

SLR = "https://www.openslr.org"
KOKORO_REV = "f3ff3571791e39611d31c381e3a41a3af07b4987"
KOKORO_CARD = f"https://huggingface.co/hexgrad/Kokoro-82M/blob/{KOKORO_REV}/README.md"
PIPER_REV = "c10ece1aade47bb51c153c893d14e5bf8e5b7117"
PIPER = f"https://huggingface.co/rhasspy/piper-voices/blob/{PIPER_REV}"
OWW_REV = "1eec2158c5c54150ac5f4c15065adacb1003b1e7"  # tag v0.5.1

LIBRISPEECH = ("LibriSpeech ASR corpus (V. Panayotov, G. Chen, D. Povey, S. Khudanpur, ICASSP 2015), "
               "OpenSLR SLR12, CC BY 4.0; audio from LibriVox public-domain audiobooks. Subset {}. "
               "Role: {}.")

ROWS = []
for part, role in [("train-clean-100", "training negatives; transcripts used as TTS text"),
                   ("dev-clean", "calibration negatives (threshold); transcripts as eval TTS text"),
                   ("dev-other", "calibration negatives (threshold)"),
                   ("test-clean", "held-out test negatives; transcripts as eval TTS text"),
                   ("test-other", "held-out test negatives")]:
    ROWS.append({"id": f"librispeech-{part}", "kind": "dataset", "licence": "CC-BY-4.0",
                 "source": f"{SLR}/resources/12/{part}.tar.gz", "licence_source": f"{SLR}/12/",
                 "path": f"raw/{part}.tar.gz", "attribution": LIBRISPEECH.format(part, role)})
ROWS.append({
    "id": "musan", "kind": "dataset", "licence": "CC-BY-4.0",
    "source": f"{SLR}/resources/17/musan.tar.gz", "licence_source": f"{SLR}/17/",
    "path": "raw/musan.tar.gz",
    "attribution": "MUSAN: A Music, Speech, and Noise Corpus (D. Snyder, G. Chen, D. Povey, 2015), "
                   "OpenSLR SLR17, CC BY 4.0. Only files whose own LICENSE/ANNOTATIONS entry is public "
                   "domain, CC0 or CC BY are used (prepare_data.py; exclusions listed in split.json). "
                   "Split by speaker/artist/file into train, calibration and test."})
ROWS.append({
    "id": "rirs-noises-simulated", "kind": "augmentation", "licence": "Apache-2.0",
    "source": f"{SLR}/resources/28/rirs_noises.zip", "licence_source": f"{SLR}/28/",
    "path": "raw/rirs_noises.zip",
    "attribution": "Room Impulse Response and Noise Database (T. Ko, V. Peddinti, D. Povey, M. Seltzer, "
                   "S. Khudanpur, ICASSP 2017), OpenSLR SLR28, Apache 2.0. Only simulated_rirs/ (also "
                   "published alone as SLR26, Apache 2.0) is used; real_rirs_isotropic_noises (RWCP, "
                   "REVERB, AIR terms) and pointsource_noises are not. Split by simulated room."})
ROWS.append({
    "id": "kokoro-v1-onnx", "kind": "tts-model", "licence": "Apache-2.0",
    "source": "https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/kokoro-v1.0.onnx",
    "licence_source": KOKORO_CARD, "path": "tts/kokoro/kokoro-v1.0.onnx",
    "attribution": "Kokoro-82M v1.0 by hexgrad, Apache 2.0 (trained on permissive/non-copyrighted audio per "
                   "its model card), ONNX export by thewh1teagle/kokoro-onnx (MIT); identical bytes to the "
                   "copy in ~/.mirrin/tts."})
ROWS.append({
    "id": "kokoro-v1-voice-pack", "kind": "voice", "licence": "Apache-2.0",
    "source": "https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/voices-v1.0.bin",
    "licence_source": KOKORO_CARD, "path": "tts/kokoro/voices-v1.0.bin",
    "attribution": "Kokoro-82M v1.0 voice packs by hexgrad, Apache 2.0, bundled by kokoro-onnx. Loaded "
                   "to initialise the engine; synthesis uses the per-voice files below."})
for v in KOKORO_ALL:
    ROWS.append({
        "id": f"kokoro-voice-{v.replace('_', '-')}", "kind": "voice", "licence": "Apache-2.0",
        "source": f"https://huggingface.co/hexgrad/Kokoro-82M/blob/{KOKORO_REV}/voices/{v}.pt",
        "licence_source": KOKORO_CARD, "path": f"tts/kokoro/voices/{v}.npy",
        "attribution": f"Kokoro-82M v1.0 voice {v} by hexgrad, Apache 2.0. Extracted from voices-v1.0.bin "
                       f"by prepare_data.py; numerically identical to voices/{v}.pt at the pinned revision."})

PIPER_VOICES = {
    "en_US-libritts-high": ("CC-BY-4.0", "en/en_US/libritts/high",
                            "Piper voice en_US-libritts-high (904 speakers), trained from scratch on LibriTTS "
                            "train-clean-360 (OpenSLR SLR60, CC BY 4.0) per its MODEL_CARD; weights published "
                            "in rhasspy/piper-voices (MIT). Speakers split train/cal/test by id."),
}
for name, path in [("en_US-ljspeech-high", "en/en_US/ljspeech/high"),
                   ("en_US-kristin-medium", "en/en_US/kristin/medium"),
                   ("en_US-john-medium", "en/en_US/john/medium"),
                   ("en_US-norman-medium", "en/en_US/norman/medium"),
                   ("en_GB-cori-high", "en/en_GB/cori/high"),
                   ("en_GB-cori-medium", "en/en_GB/cori/medium")]:
    data = "LJ Speech (public domain)" if "ljspeech" in name else "LibriVox public-domain recordings"
    lineage = "fine-tuned from en_US-kristin-medium (same lineage)" if "john" in name else "trained from scratch"
    PIPER_VOICES[name] = ("MIT", path,
                          f"Piper voice {name} by Bryce Beattie, {lineage} on {data} per its MODEL_CARD; "
                          "weights published in rhasspy/piper-voices under MIT.")
for name, (lic, path, attr) in PIPER_VOICES.items():
    card = f"{PIPER}/{path}/MODEL_CARD"
    evidence = card if lic == "CC-BY-4.0" else f"{PIPER}/README.md"
    attr_full = attr + f" Model card: {card}."
    ROWS.append({"id": f"piper-{name.lower().replace('_', '-')}", "kind": "voice", "licence": lic,
                 "source": f"{PIPER}/{path}/{name}.onnx", "licence_source": evidence,
                 "path": f"tts/piper/{name}.onnx", "attribution": attr_full})
    ROWS.append({"id": f"piper-{name.lower().replace('_', '-')}-config", "kind": "voice", "licence": lic,
                 "source": f"{PIPER}/{path}/{name}.onnx.json", "licence_source": evidence,
                 "path": f"tts/piper/{name}.onnx.json", "attribution": "Configuration for the voice above. " + attr})

ROWS.append({
    "id": "oww-melspectrogram", "kind": "feature-model", "licence": "Apache-2.0",
    "source": "https://github.com/dscripka/openWakeWord/releases/download/v0.5.1/melspectrogram.onnx",
    "licence_source": f"https://github.com/dscripka/openWakeWord/blob/{OWW_REV}/LICENSE",
    "path": "frontend/melspectrogram.onnx",
    "attribution": "openWakeWord v0.5.1 by David Scripka (Apache 2.0 code). Contains no learned weights: "
                   "verify_frontend.py checks every constant equals a Hann-400 DFT basis and a Slaney mel "
                   "filterbank. Upstream has not yet said in writing whether the README's CC BY-NC-SA note "
                   "on pre-trained models covers this file (dscripka/openWakeWord#348, open)."})
ROWS.append({
    "id": "oww-embedding", "kind": "feature-model", "licence": "Apache-2.0",
    "source": "https://github.com/dscripka/openWakeWord/releases/download/v0.5.1/embedding_model.onnx",
    "licence_source": "https://www.kaggle.com/models/google/speech-embedding",
    "path": "frontend/embedding_model.onnx",
    "attribution": "openWakeWord v0.5.1 re-implementation (D. Scripka) of Google's speech_embedding/1 "
                   "(J. Lin, K. Kilgour, D. Roblek, M. Sharifi, arXiv:2002.01322), Apache 2.0. "
                   "verify_frontend.py finds every learned weight in Google's published variables "
                   "(batch-norm folded). Upstream licence statement pending: dscripka/openWakeWord#348."})
ROWS.append({
    "id": "google-speech-embedding-1", "kind": "feature-model", "licence": "Apache-2.0",
    "source": "https://tfhub.dev/google/speech_embedding/1",
    "licence_source": "https://www.kaggle.com/models/google/speech-embedding",
    "path": "google/speech_embedding_1.tar.gz",
    "attribution": "Google speech_embedding/1 TF-Hub module, Apache 2.0. Used only as provenance evidence "
                   "for the openWakeWord embedding (verify_frontend.py); not used in training."})


def sha256(p):
    h = hashlib.sha256()
    with open(p, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def main():
    root = Path(sys.argv[1]) if len(sys.argv) > 1 else DATA
    out = []
    for r in ROWS:
        r = dict(r)
        r["sha256"] = sha256(root / r["path"])
        out.append({k: r[k] for k in ("id", "kind", "licence", "source", "licence_source", "sha256", "path", "attribution")})
    print(json.dumps(out, indent=1, ensure_ascii=False))


if __name__ == "__main__":
    main()
