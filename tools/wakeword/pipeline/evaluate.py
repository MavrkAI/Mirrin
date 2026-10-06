"""Final held-out evaluation, run once after training, through openWakeWord itself.

  evaluate.py [NPROC] [MODEL ...]

Every recording is streamed as 16 kHz mono int16 in 1,280-sample frames through
openwakeword.model.Model loaded with the exported models and the inventoried
front-end, with the model state reset between independent recordings (as the README
protocol requires). Nothing here was seen in training or calibration:

  * positives: test-split voices only (8 Kokoro voices, 75 LibriTTS speakers, Piper
    "norman"), clean and mixed with test-split MUSAN noise / test-split room responses;
  * near-misses: the same held-out voices saying each model's near-miss phrases;
  * false accepts per hour: LibriSpeech test-clean + test-other and MUSAN test files.

Events: a detection is a frame at or above the threshold more than 1.5 s after the
previous detection (wake_helper.py's COOLDOWN). Per-frame peaks are never counted.
"""
import json
import random
import sys
import zlib
from multiprocessing import Pool

import numpy as np

from common import EMBEDDING, MELSPEC, OUT, SR, WORK, read_wav, speech_bounds
from features import augment, noise_pool
from prepare_data import X
from train_model import COOLDOWN_FRAMES, events

THRESHOLDS = [0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9]
MODELS = ["hey_mirrin", "hey_nyra", "hey_pickoo"]
_model = None


def oww(models):
    global _model
    if _model is None:
        from openwakeword.model import Model
        _model = Model(wakeword_models=[str(OUT / f"{m}.onnx") for m in models],
                       inference_framework="onnx", melspec_model_path=str(MELSPEC),
                       embedding_model_path=str(EMBEDDING))
    return _model


def stream_scores(x, models, key):
    """Per-frame scores for one recording. openWakeWord's reset() refills its feature
    buffer from unseeded np.random noise, so seed it from the recording's key: without
    that, the first frames of every recording (and the report) changed between runs."""
    m = oww(models)
    np.random.seed(zlib.crc32(key.encode()))
    m.reset()
    pcm = (np.clip(x, -1, 1) * 32767).astype(np.int16)
    n = len(pcm) // 1280 * 1280
    out = {k: [] for k in models}
    for i in range(0, n, 1280):
        p = m.predict(pcm[i:i + 1280])
        for k in models:
            out[k].append(float(p[k]))
    return {k: np.array(v) for k, v in out.items()}


def clip_job(args):
    row, noisy, models = args
    clip = read_wav(WORK / "clips" / row["path"])
    s0, s1 = speech_bounds(clip)
    clip = clip[s0:s1]
    rng = random.Random(row["id"] + str(noisy))
    pre, post = np.zeros(int(2.0 * SR)), np.zeros(int(0.6 * SR))
    x = np.concatenate([pre, clip, post]).astype(np.float32)
    if noisy:
        x = augment(x, noise_pool("test"), rng, p_rir=0.5, p_noise=1.0, snr=(5, 20))
    s = stream_scores(x, models, f"{row['id']}/{noisy}")
    return row["id"], noisy, {k: float(v.max()) for k, v in s.items()}


def neg_job(args):
    rel, models, thresholds = args
    x = read_wav(X / rel)
    s = stream_scores(x, models, rel)
    return rel, len(x) / SR, {k: [events(v, t, COOLDOWN_FRAMES) for t in thresholds] for k, v in s.items()}, \
        {k: float(v.max()) for k, v in s.items()}


def wilson(k, n, z=1.96):
    if n == 0:
        return (0.0, 0.0)
    p = k / n
    d = 1 + z * z / n
    c = (p + z * z / (2 * n)) / d
    h = z * np.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / d
    return (round(float(c - h), 3), round(float(c + h), 3))


def poisson_upper(k, hours):
    """95% upper bound on an event rate (chi-square / Poisson)."""
    from scipy.stats import chi2
    return float(chi2.ppf(0.975, 2 * (k + 1)) / 2 / hours)


def main():
    nproc = int(sys.argv[1]) if len(sys.argv) > 1 else 12
    models = sys.argv[2:] or MODELS
    rows = [json.loads(l) for l in (WORK / "clips/manifest.jsonl").read_text().splitlines()]
    test = [r for r in rows if r["split"] == "test"]
    split = json.loads((WORK / "split.json").read_text())
    neg_files = split["librispeech"]["test"] + [e["path"] for e in split["musan"]["test"]]
    # the fixed grid plus each model's threshold chosen on calibration data
    calibrated = {m: json.loads((OUT / f"{m}.train.json").read_text())["chosen"]["threshold"] for m in models}
    thresholds = sorted(set(THRESHOLDS) | set(calibrated.values()))
    with Pool(nproc) as p:
        clip_res = p.map(clip_job, [(r, noisy, models) for r in test for noisy in (False, True)], chunksize=8)
        neg_res = p.map(neg_job, [(f, models, thresholds) for f in neg_files], chunksize=8)
    by_id = {r["id"]: r for r in test}
    hours = {"librispeech": 0.0, "musan": 0.0}
    ev = {m: {"librispeech": np.zeros(len(thresholds)), "musan": np.zeros(len(thresholds))} for m in models}
    worst = {m: [] for m in models}
    for rel, secs, e, mx in neg_res:
        src = "musan" if rel.startswith("musan/") else "librispeech"
        hours[src] += secs / 3600
        for m in models:
            ev[m][src] += e[m]
            worst[m].append((mx[m], rel))
    report = {"thresholds": thresholds, "negative_hours": hours, "cooldown_s": COOLDOWN_FRAMES * 0.08,
              "models": {}}
    for m in models:
        res = {"calibrated_threshold": calibrated[m], "by_threshold": []}
        pos = [(by_id[i], n, s[m]) for i, n, s in clip_res if by_id[i]["kind"] == "pos" and by_id[i]["model"] == m]
        near = [(by_id[i], n, s[m]) for i, n, s in clip_res if by_id[i]["kind"] == "near" and by_id[i]["model"] == m]
        other_pos = [(by_id[i], n, s[m]) for i, n, s in clip_res if by_id[i]["kind"] == "pos" and by_id[i]["model"] != m]
        generic = [(by_id[i], n, s[m]) for i, n, s in clip_res if by_id[i]["kind"] == "generic"]
        tot_h = hours["librispeech"] + hours["musan"]
        for ti, t in enumerate(thresholds):
            row = {"threshold": t}
            for cond, flag in (("clean", False), ("noisy", True)):
                sc = [s for _, n, s in pos if n == flag]
                k = int(sum(s >= t for s in sc))
                row[f"recall_{cond}"] = round(k / max(1, len(sc)), 3)
                row[f"recall_{cond}_ci95"] = wilson(k, len(sc))
                row[f"n_pos_{cond}"] = len(sc)
            for engine in ("kokoro", "piper"):
                sc = [s for r, n, s in pos if not n and r["engine"] == engine]
                row[f"recall_clean_{engine}"] = round(float(np.mean(np.array(sc) >= t)), 3) if sc else None
            e_tot = int(ev[m]["librispeech"][ti] + ev[m]["musan"][ti])
            row["false_accepts"] = e_tot
            row["fa_per_hour"] = round(e_tot / tot_h, 3)
            row["fa_per_hour_upper95"] = round(poisson_upper(e_tot, tot_h), 3)
            row["fa_per_hour_librispeech"] = round(ev[m]["librispeech"][ti] / hours["librispeech"], 3)
            row["fa_per_hour_musan"] = round(ev[m]["musan"][ti] / max(1e-9, hours["musan"]), 3)
            row["near_miss_accept_rate"] = round(float(np.mean([s >= t for _, _, s in near])), 3)
            row["other_wake_phrase_accept_rate"] = round(float(np.mean([s >= t for _, _, s in other_pos])), 3)
            row["generic_tts_accept_rate"] = round(float(np.mean([s >= t for _, _, s in generic])), 3)
            row["meets_target"] = row["recall_clean"] >= 0.8 and row["fa_per_hour"] <= 0.5
            res["by_threshold"].append(row)
        near_sorted = sorted(((s, r["text"]) for r, n, s in near if not n), reverse=True)[:10]
        res["top_near_misses"] = [[round(s, 3), t] for s, t in near_sorted]
        res["top_negative_recordings"] = [[round(s, 3), r] for s, r in sorted(worst[m], reverse=True)[:10]]
        report["models"][m] = res
    (OUT / "eval_report.json").write_text(json.dumps(report, indent=1))
    for m, res in report["models"].items():
        print(f"\n{m} (calibrated threshold {res['calibrated_threshold']}), negatives "
              f"{hours['librispeech']:.1f} h LibriSpeech + {hours['musan']:.1f} h MUSAN")
        for r in res["by_threshold"]:
            print(f"  t={r['threshold']}: recall clean {r['recall_clean']} {r['recall_clean_ci95']} "
                  f"noisy {r['recall_noisy']}  FA/h {r['fa_per_hour']} (<= {r['fa_per_hour_upper95']}) "
                  f"near-miss {r['near_miss_accept_rate']} other-wake {r['other_wake_phrase_accept_rate']} "
                  f"{'MEETS' if r['meets_target'] else ''}")
    print("EVAL_DONE")


if __name__ == "__main__":
    main()
