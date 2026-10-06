"""Shared helpers: paths, voices, TTS engines, audio utilities.

Every file read here is listed in tools/wakeword/DATA-LICENCES.md. Nothing is
downloaded: openWakeWord's front-end is loaded from the inventoried copies, never
from the package's auto-download path.
"""
import json
import os
import re
from pathlib import Path

import numpy as np

SR = 16000
DATA = Path(os.environ.get("WAKE_DATA", Path.home() / "mirrin-wakeword-clean/data"))
WORK = Path(os.environ.get("WAKE_WORK", Path.home() / "mirrin-wakeword-clean/work"))
OUT = Path(os.environ.get("WAKE_OUT", Path.home() / "mirrin-wakeword-clean/out"))
MELSPEC = DATA / "frontend/melspectrogram.onnx"
EMBEDDING = DATA / "frontend/embedding_model.onnx"

# --- Voices -------------------------------------------------------------------------
# Kokoro voices (all from voices-v1.0.bin, Apache-2.0). Split by voice identity: a voice
# is either training or evaluation, never both. Calibration voices tune the threshold;
# test voices are touched once, for the final report.
KOKORO_TRAIN = [
    "af_alloy", "af_bella", "af_heart", "af_jessica", "af_kore", "af_nicole", "af_nova",
    "af_sarah", "af_sky", "am_adam", "am_eric", "am_fenrir", "am_liam", "am_michael",
    "am_onyx", "am_santa", "bf_emma", "bf_isabella", "bf_lily", "bm_daniel", "bm_george",
    "bm_lewis", "ff_siwis", "hf_beta", "hm_psi", "im_nicola", "em_santa", "pf_dora",
]
KOKORO_CAL = ["af_aoede", "am_echo", "bm_fable", "hm_omega"]
KOKORO_TEST = ["af_river", "am_puck", "bf_alice", "hf_alpha", "ef_dora", "pm_alex", "if_sara", "em_alex"]
KOKORO_ALL = KOKORO_TRAIN + KOKORO_CAL + KOKORO_TEST

# Piper voices with a permissive lineage (see DATA-LICENCES.md). LibriTTS speakers are
# split by speaker id; single-speaker voices are whole units.
PIPER_TRAIN_SINGLE = ["en_US-kristin-medium", "en_US-john-medium", "en_GB-cori-high", "en_GB-cori-medium"]
PIPER_CAL_SINGLE = ["en_US-ljspeech-high"]
PIPER_TEST_SINGLE = ["en_US-norman-medium"]
LIBRITTS = "en_US-libritts-high"


def libritts_split():
    """Deterministic speaker split of the 904 LibriTTS speakers: 1 in 6 to evaluation
    (alternating calibration / test), the rest to training."""
    cfg = json.loads((DATA / f"tts/piper/{LIBRITTS}.onnx.json").read_text())
    ids = sorted(cfg["speaker_id_map"].items(), key=lambda kv: kv[1])
    train, cal, test = [], [], []
    for n, (spk, idx) in enumerate(ids):
        if n % 6 == 5:
            (cal if (n // 6) % 2 == 0 else test).append(idx)
        else:
            train.append(idx)
    return train, cal, test


def kokoro_lang(voice):
    return "en-gb" if voice[0] == "b" else "en-us"


def ort_session(path, threads=1):
    """One-thread onnxruntime session: the pipeline runs one process per core, and
    default per-session thread pools oversubscribe the machine many times over."""
    import onnxruntime as ort
    opts = ort.SessionOptions()
    opts.intra_op_num_threads = threads
    opts.inter_op_num_threads = 1
    return ort.InferenceSession(str(path), sess_options=opts, providers=["CPUExecutionProvider"])


class TTS:
    """Kokoro and Piper synthesis returning 16 kHz float32 mono."""

    def __init__(self):
        self._kokoro = None
        self._piper = {}

    def kokoro(self, text, voice, speed=1.0, phonemes=False):
        if self._kokoro is None:
            from kokoro_onnx import Kokoro
            self._kokoro = Kokoro.from_session(ort_session(DATA / "tts/kokoro/kokoro-v1.0.onnx"),
                                               str(DATA / "tts/kokoro/voices-v1.0.bin"))
        style = np.load(DATA / f"tts/kokoro/voices/{voice}.npy")
        if "[[" in text and not phonemes:
            # Piper-style inline phonemes ("Hey [[mˈɪɹɪn]], ..."): phonemise the plain parts
            # with Kokoro's own G2P and splice the given phonemes in between.
            parts = [p.strip() for p in re.split(r"(\[\[.*?\]\])", text)]
            text = " ".join(p[2:-2].strip() if p.startswith("[[") else
                            self._kokoro.tokenizer.phonemize(p, kokoro_lang(voice)) for p in parts if p)
            text, phonemes = re.sub(r" ([,.!?])", r"\1", text), True
        audio, sr = self._kokoro.create(text, voice=style, speed=speed, lang=kokoro_lang(voice),
                                        is_phonemes=phonemes)
        return resample(audio, sr)

    def piper(self, text, voice, speaker=None, length_scale=1.0, noise_scale=0.667, noise_w=0.8):
        from piper import PiperVoice, SynthesisConfig
        if voice not in self._piper:
            self._piper[voice] = PiperVoice.load(str(DATA / f"tts/piper/{voice}.onnx"),
                                                 str(DATA / f"tts/piper/{voice}.onnx.json"))
            self._piper[voice].session = ort_session(DATA / f"tts/piper/{voice}.onnx")
        v = self._piper[voice]
        cfg = SynthesisConfig(speaker_id=speaker, length_scale=length_scale,
                              noise_scale=noise_scale, noise_w_scale=noise_w)
        chunks = list(v.synthesize(text, syn_config=cfg))
        audio = np.concatenate([c.audio_float_array for c in chunks])
        return resample(audio, v.config.sample_rate)


def resample(x, sr):
    x = np.asarray(x, dtype=np.float32)
    if sr == SR:
        return x
    from scipy.signal import resample_poly
    g = np.gcd(int(sr), SR)
    return resample_poly(x, SR // g, int(sr) // g).astype(np.float32)


def speech_bounds(x, thr=0.02):
    """First and last sample above thr * peak (simple energy trim)."""
    peak = np.max(np.abs(x)) + 1e-9
    idx = np.where(np.abs(x) > thr * peak)[0]
    return (int(idx[0]), int(idx[-1]) + 1) if len(idx) else (0, len(x))


def normalise(x, level):
    return (x / (np.max(np.abs(x)) + 1e-9) * level).astype(np.float32)


def write_wav(path, x):
    import soundfile as sf
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    sf.write(str(path), np.clip(x, -1, 1), SR, subtype="PCM_16")


def read_wav(path):
    import soundfile as sf
    x, sr = sf.read(str(path), dtype="float32", always_2d=False)
    if x.ndim > 1:
        x = x.mean(axis=1)
    return resample(x, sr)


def features(ncpu=1):
    from openwakeword.utils import AudioFeatures
    return AudioFeatures(melspec_model_path=str(MELSPEC), embedding_model_path=str(EMBEDDING),
                         inference_framework="onnx", ncpu=ncpu)
