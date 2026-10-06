#!/usr/bin/env python
"""Mirrin voice: Kokoro neural TTS, offline.

One-shot:  echo "text" | python kokoro_tts.py out.wav
Server:    python kokoro_tts.py --server   (reads JSON lines {"text":..,"out":..} on stdin,
           replies "ok <out>" per line; keeps the model loaded for low latency)

Voices are Kokoro names such as bf_emma or af_heart. A blend averages voices:
"bf_emma+af_heart" or weighted "bf_emma*0.7+af_heart*0.3". The language
follows the voice's first letter (a American, b British, e Spanish, f French,
h Hindi, i Italian, j Japanese, p Portuguese, z Chinese) unless LANG_CODE is
set, to one of those letters or to an espeak code such as fr-fr.

The daemon sends one sentence per request. The model's own silence is trimmed
(it can put a long pause at the start), and a breath-sized pause is put back
after the clip according to how the sentence ends, so sentences flow without
running into each other.
"""
import json
import os
import re
import sys

import numpy as np
import soundfile as sf
from kokoro_onnx import Kokoro

HERE = os.path.dirname(os.path.abspath(__file__))
VOICE = os.environ.get("VOICE") or "bm_george"
SPEED = float(os.environ.get("SPEED") or "1.0")
LANG = os.environ.get("LANG_CODE") or ""
SAMPLE_RATE = 24000

# Seconds of silence after a clip, by how its text ends.
PAUSE_SENTENCE = 0.32  # . ! ? …
PAUSE_CLAUSE = 0.16    # , ; : — –
PAUSE_OTHER = 0.10     # cut mid-phrase


def load():
    return Kokoro(os.path.join(HERE, "kokoro-v1.0.onnx"), os.path.join(HERE, "voices-v1.0.bin"))


# Kokoro's one-letter language codes, as espeak names them.
LANGS = {"a": "en-us", "b": "en-gb", "e": "es", "f": "fr-fr", "h": "hi",
         "i": "it", "j": "ja", "p": "pt-br", "z": "cmn"}


def lang_for(voice):
    if LANG:
        return LANGS.get(LANG.lower(), LANG)
    return LANGS.get(voice.strip()[:1].lower(), "en-us")


_blends = {}


def style_for(k, voice):
    """A voice name, or a blend of names averaged by weight."""
    if "+" not in voice and "*" not in voice:
        return voice
    if voice in _blends:
        return _blends[voice]
    total, acc = 0.0, None
    for part in voice.split("+"):
        name, _, w = part.strip().partition("*")
        weight = float(w) if w else 1.0
        style = k.get_voice_style(name.strip()) * weight
        acc = style if acc is None else acc + style
        total += weight
    _blends[voice] = acc / total
    return _blends[voice]


def pause_for(text):
    end = text.rstrip()[-1:] if text.rstrip() else ""
    if end in ".!?…":
        return PAUSE_SENTENCE
    if end in ",;:—–-":
        return PAUSE_CLAUSE
    return PAUSE_OTHER


def synth(k, text, out, voice=VOICE, speed=SPEED, pause=None):
    text = text.strip()
    samples, rate = k.create(text, voice=style_for(k, voice), speed=speed, lang=lang_for(voice))
    gap = pause_for(text) if pause is None else float(pause)
    if gap > 0:
        samples = np.concatenate([samples, np.zeros(int(gap * rate), dtype=samples.dtype)])
    sf.write(out, samples, rate)


def main():
    if "--server" in sys.argv:
        k = load()
        print("ready", flush=True)
        for line in sys.stdin:
            line = line.strip()
            if not line:
                continue
            try:
                req = json.loads(line)
                synth(k, req["text"], req["out"], req.get("voice") or VOICE, float(req.get("speed") or SPEED), req.get("pause"))
                print("ok " + req["out"], flush=True)
            except Exception as e:  # keep serving
                print("err " + str(e).replace("\n", " "), flush=True)
        os._exit(0)  # host gone
        return
    out = sys.argv[1] if len(sys.argv) > 1 else "out.wav"
    text = os.environ.get("TEXT") or sys.stdin.read()
    synth(load(), text.strip(), out)


if __name__ == "__main__":
    main()
