"""Synthesise every TTS clip (positives, continuations, near-misses, generic speech)
for the train / cal / test voice splits. Voices never cross splits.

  synth.py plan          write WORK/synth_jobs.jsonl (deterministic, seed 7)
  synth.py run [NPROC]   render missing clips to WORK/clips/<split>/<kind>/<id>.wav

Output manifest: WORK/clips/manifest.jsonl (one line per rendered clip, with the voice,
speaker, text and synthesis settings: the clip's full ancestry).
"""
import json
import random
import sys
from multiprocessing import Pool
from pathlib import Path

import numpy as np

from common import (KOKORO_CAL, KOKORO_TEST, KOKORO_TRAIN, LIBRITTS, PIPER_CAL_SINGLE,
                    PIPER_TEST_SINGLE, PIPER_TRAIN_SINGLE, SR, TTS, WORK, libritts_split,
                    normalise, speech_bounds, write_wav)
from phrases import CONTINUATIONS, GENERIC, MODELS, eval_positive_text
from prepare_data import X

CLIPS = WORK / "clips"
JOBS = WORK / "synth_jobs.jsonl"


def librispeech_sentences(part, n, rng):
    """Short sentences from LibriSpeech transcripts (CC BY 4.0) of the given subset."""
    lines = []
    for f in sorted((X / "LibriSpeech" / part).rglob("*.trans.txt")):
        for line in f.read_text().splitlines():
            words = line.split()[1:]
            if 3 <= len(words):
                start = rng.randrange(0, max(1, len(words) - 10))
                lines.append(" ".join(words[start:start + rng.randint(3, 12)]).lower())
    rng.shuffle(lines)
    return lines[:n]


def plan():
    rng = random.Random(7)
    lt_train, lt_cal, lt_test = libritts_split()
    voices = {
        "train": (KOKORO_TRAIN, lt_train, PIPER_TRAIN_SINGLE),
        "cal": (KOKORO_CAL, lt_cal, PIPER_CAL_SINGLE),
        "test": (KOKORO_TEST, lt_test, PIPER_TEST_SINGLE),
    }
    jobs = []

    def add(split, kind, model, engine, voice, text, speaker=None, phonemes=False, cont=None, r=None):
        rng_ = r or rng
        if engine == "kokoro":
            params = {"speed": rng_.choice([0.8, 0.9, 1.0, 1.1, 1.25])}
        else:
            params = {"length_scale": round(rng_.uniform(0.8, 1.25), 2),
                      "noise_scale": round(rng_.uniform(0.45, 0.85), 2),
                      "noise_w": round(rng_.uniform(0.5, 1.0), 2)}
        jobs.append({"id": f"{split}-{len(jobs):06d}", "split": split, "kind": kind, "model": model,
                     "engine": engine, "voice": voice, "speaker": speaker, "text": text,
                     "phonemes": phonemes, "cont": cont, "level": round(rng_.uniform(0.3, 0.95), 2),
                     **params})

    # --- training split ---
    kv, lt, single = voices["train"]
    for model, cfg in MODELS.items():
        pos_text = cfg["positive_text"]
        for v in kv:
            for t in pos_text:
                for _ in range(2):
                    add("train", "pos", model, "kokoro", v, t)
            add("train", "pos", model, "kokoro", v, rng.choice(cfg["positive_rare"]))
            for p in cfg["positive_phonemes"]:
                for _ in range(2):
                    add("train", "pos", model, "kokoro", v, p, phonemes=True)
            for c in rng.sample(CONTINUATIONS, 4):
                add("train", "cont", model, "kokoro", v, f"Hey {cfg['say']}, {c}.", cont=f"Hey {cfg['say']}")
        for s in lt:
            for t in rng.sample(pos_text, 3):
                add("train", "pos", model, "piper", LIBRITTS, t, speaker=s)
            if rng.random() < 0.2:
                add("train", "pos", model, "piper", LIBRITTS, rng.choice(cfg["positive_rare"]), speaker=s)
            c = rng.choice(CONTINUATIONS)
            add("train", "cont", model, "piper", LIBRITTS, f"Hey {cfg['say']}, {c}.", speaker=s, cont=f"Hey {cfg['say']}")
        for v in single:
            for t in pos_text:
                for _ in range(3):
                    add("train", "pos", model, "piper", v, t)
            for c in rng.sample(CONTINUATIONS, 3):
                add("train", "cont", model, "piper", v, f"Hey {cfg['say']}, {c}.", cont=f"Hey {cfg['say']}")
        near = cfg["near_miss"] + [f"hey {n}" for n in cfg["hey_near"]] + \
            [f"hey {n}, {rng.choice(CONTINUATIONS)}." for n in cfg["hey_near"]]
        for t in near:
            for v in rng.sample(kv, 12):
                add("train", "near", model, "kokoro", v, t)
            for s in rng.sample(lt, 30):
                add("train", "near", model, "piper", LIBRITTS, t, speaker=s)
            add("train", "near", model, "piper", rng.choice(single), t)
    # --- calibration and test splits: held-out voices only ---
    for split in ("cal", "test"):
        kv, lt, single = voices[split]
        part = "dev-clean" if split == "cal" else "test-clean"
        for model, cfg in MODELS.items():
            ev = eval_positive_text(model)
            for v in kv:
                for t in ev:
                    add(split, "pos", model, "kokoro", v, t)
            for s in lt:
                for t in rng.sample(ev, 4):
                    add(split, "pos", model, "piper", LIBRITTS, t, speaker=s)
            for v in single:
                for t in ev:
                    for _ in range(2):
                        add(split, "pos", model, "piper", v, t)
            for t in cfg["eval_near_miss"]:
                for v in kv:
                    add(split, "near", model, "kokoro", v, t)
                for s in rng.sample(lt, 12):
                    add(split, "near", model, "piper", LIBRITTS, t, speaker=s)
        for t in librispeech_sentences(part, 300, rng):
            if rng.random() < 0.5:
                add(split, "generic", None, "kokoro", rng.choice(kv), t)
            else:
                add(split, "generic", None, "piper", LIBRITTS, t, speaker=rng.choice(lt))
    # --- generic training speech last, from its own generator, so that the jobs above
    # (and their ids) do not depend on whether train-clean-100 has been unpacked yet ---
    grng = random.Random(9)
    kv, lt, _ = voices["train"]
    train_text = librispeech_sentences("train-clean-100", 2400, grng)
    if not train_text:
        print("warning: train-clean-100 transcripts not unpacked; generic train speech is partial")
    for t in GENERIC * 3 + train_text:
        if grng.random() < 0.5:
            add("train", "generic", None, "kokoro", grng.choice(kv), t, r=grng)
        else:
            add("train", "generic", None, "piper", LIBRITTS, t, speaker=grng.choice(lt), r=grng)
    JOBS.write_text("".join(json.dumps(j) + "\n" for j in jobs))
    counts = {}
    for j in jobs:
        k = (j["split"], j["kind"], j["model"])
        counts[k] = counts.get(k, 0) + 1
    for k in sorted(counts, key=str):
        print(k, counts[k])


_tts = None


def render(job):
    global _tts
    if _tts is None:
        _tts = TTS()
    path = CLIPS / job["split"] / job["kind"] / f"{job['id']}.wav"
    if path.exists():
        return job | {"path": str(path.relative_to(CLIPS))}

    def say(text, phon=False):
        if job["engine"] == "kokoro":
            return _tts.kokoro(text, job["voice"], job["speed"], phonemes=phon)
        return _tts.piper(text, job["voice"], job["speaker"], job["length_scale"],
                          job["noise_scale"], job["noise_w"])

    try:
        audio = say(job["text"], job["phonemes"])
        if job["cont"]:
            # keep the phrase plus 0.10-0.35 s of what follows: what a streaming detector hears
            alone = say(job["cont"])
            a0, a1 = speech_bounds(alone)
            f0, _ = speech_bounds(audio)
            rng = random.Random(job["id"])
            n = min(len(audio), f0 + (a1 - a0) + int(SR * rng.uniform(0.10, 0.35)))
            audio = audio[:n].copy()
            fade = min(480, len(audio))
            audio[-fade:] *= np.linspace(1, 0, fade)
        write_wav(path, normalise(audio, job["level"]))
    except Exception as e:  # a voice that cannot say a text is skipped and logged
        return job | {"error": repr(e)}
    return job | {"path": str(path.relative_to(CLIPS))}


def run(nproc):
    jobs = [json.loads(l) for l in JOBS.read_text().splitlines()]
    done, errors = [], 0
    with Pool(nproc) as pool:
        for i, r in enumerate(pool.imap_unordered(render, jobs, chunksize=16)):
            if "error" in r:
                errors += 1
                print("skip", r["id"], r["error"], flush=True)
            else:
                done.append(r)
            if i % 1000 == 0:
                print(f"{i}/{len(jobs)} rendered", flush=True)
    done.sort(key=lambda r: r["id"])
    (CLIPS / "manifest.jsonl").write_text("".join(json.dumps(r) + "\n" for r in done))
    print(f"SYNTH_DONE {len(done)} clips, {errors} skipped", flush=True)


if __name__ == "__main__":
    if sys.argv[1] == "plan":
        plan()
    else:
        run(int(sys.argv[2]) if len(sys.argv) > 2 else 8)
