"""Augment clips and compute openWakeWord embeddings with the inventoried front-end.

  features.py clips [NPROC]     TTS clips -> WORK/feat/clips_<split>.npz
        train: each clip mixed R times with train-split noise and room responses
        cal/test: one clean copy (and features of noisy copies for calibration only)
  features.py stream SPLIT [NPROC]   long recordings -> WORK/feat/stream_<split>.npy (+ _index.json)
        Files are read and embedded CHUNK samples at a time and written in resumable
        shards (WORK/feat/stream_<split>_shards/), then merged on disk, so memory stays
        bounded however long a recording is; a rerun keeps every finished shard.
        train: LibriSpeech train-clean-100 and MUSAN train files, clean and augmented
        cal:   LibriSpeech dev-clean/dev-other and MUSAN cal files (threshold calibration)

Augmentation only ever draws noise and room responses from the same split as the clip.
"""
import json
import random
import sys
from multiprocessing import Pool

import numpy as np
from scipy.signal import fftconvolve

from common import SR, WORK, features, normalise, read_wav, speech_bounds
from prepare_data import X

WIN = 32000            # 2.0 s -> exactly 16 embedding frames (1.28 s of decisions)
FEAT = WORK / "feat"
CLIPS = WORK / "clips"
REPEATS = {"pos": 3, "cont": 3, "near": 2, "generic": 1}

_split = None
_fe = None


def split_manifest():
    global _split
    if _split is None:
        _split = json.loads((WORK / "split.json").read_text())
    return _split


def noise_pool(role):
    s = split_manifest()
    return {
        "noise": [e["path"] for e in s["musan"][role] if e["category"] == "noise"],
        "music": [e["path"] for e in s["musan"][role] if e["category"] == "music"],
        "speech": [e["path"] for e in s["musan"][role] if e["category"] == "speech"],
        "rir": s["rirs"][role],
    }


_cache = {}
_cache_samples = 0
CACHE_SAMPLES = 16 * 10**6     # ~64 MB of float32 per worker, whatever the file lengths
CHUNK = 30 * SR                # long recordings are embedded 30 s at a time (~0.6 GB peak RSS)
SHARD = 500                    # stream files per resumable shard


def load(rel):
    """Small files (room responses, short noises), cached within a fixed memory budget."""
    global _cache_samples
    if rel not in _cache:
        x = read_wav(X / rel)
        if _cache_samples + len(x) > CACHE_SAMPLES:
            _cache.clear()
            _cache_samples = 0
        _cache[rel] = x
        _cache_samples += len(x)
    return _cache[rel]


def read_span(path, start=0, n=None):
    """Read n samples (16 kHz mono float32) from start without decoding the whole file."""
    import soundfile as sf
    info = sf.info(str(path))
    if info.samplerate != SR:
        x = read_wav(path)
        return x[start:None if n is None else start + n]
    x, _ = sf.read(str(path), start=start, frames=-1 if n is None else n, dtype="float32", always_2d=False)
    return x.mean(axis=1).astype(np.float32) if x.ndim > 1 else x


def length(path):
    import soundfile as sf
    info = sf.info(str(path))
    return int(info.frames * SR // info.samplerate)


def random_noise(pool, n, rng):
    kind = rng.choices(["noise", "music", "speech"], weights=[0.5, 0.3, 0.2])[0]
    rel = rng.choice(pool[kind])
    total = length(X / rel)
    if total < n:
        src = read_span(X / rel)
        src = np.tile(src, n // max(1, len(src)) + 1)
        start = rng.randrange(0, len(src) - n + 1)   # same draws as the whole-file version
        return src[start:start + n]
    start = rng.randrange(0, total - n + 1)
    return read_span(X / rel, start, n)


def reverb(x, pool, rng):
    rir = load(rng.choice(pool["rir"]))
    y = fftconvolve(x, rir)[: len(x)]
    return y / (np.max(np.abs(y)) + 1e-9) * np.max(np.abs(x))


def augment(x, pool, rng, p_rir=0.5, p_noise=0.8, snr=(0, 25)):
    if rng.random() < p_rir:
        x = reverb(x, pool, rng)
    if rng.random() < p_noise:
        nz = random_noise(pool, len(x), rng)
        ps, pn = np.mean(x ** 2) + 1e-9, np.mean(nz ** 2) + 1e-9
        x = x + nz * np.sqrt(ps / pn / 10 ** (rng.uniform(*snr) / 10))
    return normalise(x, rng.uniform(0.05, 0.9))


def place(clip, rng, kind):
    """Put the clip in a 2 s window. Wake positives end 0-0.3 s before the window's end,
    as the detector sees them in a stream; negatives go anywhere."""
    s0, s1 = speech_bounds(clip)
    clip = clip[s0:s1]
    out = np.zeros(WIN, dtype=np.float32)
    if kind in ("pos", "cont"):
        end = WIN - int(SR * rng.uniform(0.0, 0.3))
        start = end - len(clip)
        if start < 0:
            clip, start = clip[-start:], 0
        out[start:end] = clip
    else:
        if len(clip) >= WIN:
            o = rng.randrange(0, len(clip) - WIN + 1)
            out[:] = clip[o:o + WIN]
        else:
            o = rng.randrange(0, WIN - len(clip) + 1)
            out[o:o + len(clip)] = clip
    return out


def pcm16(x):
    """float [-1, 1] -> int16 PCM, the only input openWakeWord's front-end accepts."""
    return (np.clip(x, -1, 1) * 32767).astype(np.int16)


def fe():
    global _fe
    if _fe is None:
        _fe = features()
    return _fe


def clip_worker(args):
    rows, role, aug = args
    if not aug:
        # held-out clips: the whole utterance in a short silent stream, every window scored
        embs, meta = [], []
        for r in rows:
            clip = read_wav(CLIPS / r["path"])
            s0, s1 = speech_bounds(clip)
            x = np.concatenate([np.zeros(int(1.6 * SR)), clip[s0:s1], np.zeros(int(0.5 * SR))])
            embs.append(fe()._get_embeddings(pcm16(x)).astype(np.float16))
            meta.append(r["id"])
        return embs, meta
    pool = noise_pool(role)
    xs, meta = [], []
    for r in rows:
        clip = read_wav(CLIPS / r["path"])
        rng = random.Random(r["id"])
        reps = REPEATS[r["kind"]] if aug else 1
        for k in range(reps):
            x = place(clip, rng, r["kind"])
            if aug:
                x = augment(x, pool, rng)
            xs.append(x)
            meta.append(r["id"])
    emb = fe().embed_clips(pcm16(np.stack(xs)), batch_size=64)  # 16-bit PCM, like a stream
    return emb.astype(np.float16), meta


def clips(nproc):
    rows = [json.loads(l) for l in (CLIPS / "manifest.jsonl").read_text().splitlines()]
    FEAT.mkdir(parents=True, exist_ok=True)
    for split in ("train", "cal"):
        sel = [r for r in rows if r["split"] == split]
        chunks = [sel[i:i + 64] for i in range(0, len(sel), 64)]
        with Pool(nproc) as p:
            res = p.map(clip_worker, [(c, split, split == "train") for c in chunks])
        ids = [m for _, ms in res for m in ms]
        if split == "train":
            emb = np.concatenate([e for e, _ in res])
            np.save(FEAT / f"clips_{split}.npy", emb)
            (FEAT / f"clips_{split}_ids.json").write_text(json.dumps(ids))
        else:
            seqs = [e for es, _ in res for e in es]
            emb = np.concatenate(seqs)
            offs = np.cumsum([0] + [len(e) for e in seqs]).tolist()
            np.save(FEAT / f"clips_{split}.npy", emb)
            (FEAT / f"clips_{split}_ids.json").write_text(json.dumps({"ids": ids, "offsets": offs}))
        print(split, emb.shape, flush=True)
    print("CLIP_FEATURES_DONE", flush=True)


def stream_worker(args):
    """Embed one recording, CHUNK samples at a time, so memory stays bounded however long
    the file is. Returns one embedding sequence per chunk (windows never cross chunks)."""
    rel, role, aug, seed = args
    total = length(X / rel)
    out = []
    for k, start in enumerate(range(0, total, CHUNK)):
        x = read_span(X / rel, start, CHUNK)
        if len(x) < WIN:
            continue
        rng = random.Random(seed * 1000 + k)
        if aug:
            x = augment(x, noise_pool(role), rng, p_rir=0.6, p_noise=0.9, snr=(-5, 20))
        else:
            x = normalise(x, rng.uniform(0.1, 0.9))
        emb = fe()._get_embeddings(pcm16(x))
        if len(emb) >= 16:
            out.append(emb.astype(np.float16))
    return out


def stream_jobs(which):
    s = split_manifest()
    if which == "train":
        ls = s["librispeech"]["train"]
        mus = [e["path"] for e in s["musan"]["train"]]
        jobs = [(f, "train", False, i) for i, f in enumerate(ls + mus)]
        # an augmented copy of half the speech and all MUSAN, with train-split noise/RIRs
        jobs += [(f, "train", True, 10**6 + i) for i, f in enumerate(ls[::2] + mus)]
        return jobs
    files = s["librispeech"]["cal"] + [e["path"] for e in s["musan"]["cal"]]
    return [(f, "cal", False, i) for i, f in enumerate(files)]


def shard_done(path, jobs):
    """A shard is reusable only if both files exist and it was made from exactly these jobs."""
    meta = path.with_suffix(".json")
    if not (path.exists() and meta.exists()):
        return False
    try:
        m = json.loads(meta.read_text())
        arr = np.load(path, mmap_mode="r")
    except (ValueError, OSError):
        return False
    return m.get("jobs") == [list(j) for j in jobs] and arr.shape == (m["frames"], 96)


def stream(nproc, which):
    jobs = stream_jobs(which)
    shards = FEAT / f"stream_{which}_shards"
    shards.mkdir(parents=True, exist_ok=True)
    groups = [jobs[i:i + SHARD] for i in range(0, len(jobs), SHARD)]
    with Pool(nproc, maxtasksperchild=50) as p:
        for g, group in enumerate(groups):
            path = shards / f"{g:04d}.npy"
            if shard_done(path, group):
                continue
            seqs, index, total = [], [], 0
            for job, embs in zip(group, p.imap(stream_worker, group, chunksize=2)):
                for e in embs:
                    index.append([total, len(e), job[0], job[2]])
                    seqs.append(e)
                    total += len(e)
            arr = np.concatenate(seqs) if seqs else np.zeros((0, 96), np.float16)
            tmp = shards / f"{g:04d}.tmp.npy"
            np.save(tmp, arr)
            tmp.replace(path)
            path.with_suffix(".json").write_text(json.dumps(
                {"jobs": [list(j) for j in group], "frames": int(len(arr)), "files": index}))
            print(f"{which}: {(g + 1) * SHARD if g + 1 < len(groups) else len(jobs)}/{len(jobs)} files", flush=True)
    # merge the shards on disk (never all in memory) into one array and index
    metas = [json.loads((shards / f"{g:04d}.json").read_text()) for g in range(len(groups))]
    frames = sum(m["frames"] for m in metas)
    final = FEAT / f"stream_{which}.npy"
    tmp = FEAT / f"stream_{which}.tmp.npy"
    arr = np.lib.format.open_memmap(tmp, mode="w+", dtype=np.float16, shape=(frames, 96))
    index, off = [], 0
    for g, m in enumerate(metas):
        part = np.load(shards / f"{g:04d}.npy", mmap_mode="r")
        arr[off:off + len(part)] = part
        index += [[off + o, n, rel, aug] for o, n, rel, aug in m["files"]]
        off += len(part)
    arr.flush()
    del arr
    tmp.replace(final)
    hours = frames * 0.08 / 3600
    (FEAT / f"stream_{which}_index.json").write_text(json.dumps({"hours": hours, "files": index}))
    print(f"STREAM_{which.upper()}_DONE ({frames}, 96) {hours:.1f} h", flush=True)


if __name__ == "__main__":
    n = int(sys.argv[3]) if len(sys.argv) > 3 else 12
    if sys.argv[1] == "clips":
        clips(int(sys.argv[2]) if len(sys.argv) > 2 else 12)
    else:
        stream(n, sys.argv[2])
