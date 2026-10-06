"""Score the owner's real-voice recordings (record_holdout.sh) with the trained models.

  evaluate_holdout.py HOLDOUT_DIR [THRESHOLD]

Streams every WAV through openwakeword.model.Model in 1,280-sample frames (state reset
per recording) and prints, per model: positives detected, near-misses accepted, and
false-accept events per hour on the long negative recordings (1.5 s cooldown).
"""
import sys
from pathlib import Path

import numpy as np

from common import SR, read_wav
from evaluate import MODELS, THRESHOLDS, stream_scores
from train_model import COOLDOWN_FRAMES, events


def main():
    root = Path(sys.argv[1]).expanduser()
    thresholds = [float(sys.argv[2])] if len(sys.argv) > 2 else THRESHOLDS
    pad = np.zeros(int(1.6 * SR), dtype=np.float32)
    near = [(p, stream_scores(np.concatenate([pad, read_wav(p), pad[:8000]]), MODELS))
            for p in sorted((root / "near_miss").glob("*.wav"))]
    longs = [(p, stream_scores(read_wav(p), MODELS)) for p in sorted((root / "negative_long").glob("*.wav"))]
    hours = sum(len(s[MODELS[0]]) for _, s in longs) * 0.08 / 3600
    for m in MODELS:
        pos = [stream_scores(np.concatenate([pad, read_wav(p), pad[:8000]]), MODELS)[m].max()
               for p in sorted((root / m / "positive").glob("*.wav"))]
        print(f"\n{m}: {len(pos)} positives, {len(near)} near-misses, {hours * 60:.1f} min of negative talk")
        for t in thresholds:
            hit = sum(s >= t for s in pos)
            fa_near = [p.name for p, s in near if s[m].max() >= t]
            ev = sum(events(s[m], t, COOLDOWN_FRAMES) for _, s in longs)
            rate = f"{ev / hours:.2f}/h" if hours else "n/a"
            print(f"  t={t}: detected {hit}/{len(pos)}, near-misses accepted {len(fa_near)} "
                  f"{fa_near[:5]}, false accepts {ev} ({rate})")
        if pos:
            print(f"  positive scores: min {min(pos):.2f} median {np.median(pos):.2f}")


if __name__ == "__main__":
    main()
