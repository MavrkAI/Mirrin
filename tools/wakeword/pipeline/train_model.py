"""Train one wake-word classifier on the shared embeddings and export ONNX.

  train_model.py MODEL [--steps N] [--seed S] [--hard-pool N] [--near-batch K]

Inputs (all derived from inventoried data, see features.py):
  WORK/feat/clips_train.npy   TTS clips, augmented with train-split noise/RIRs
  WORK/feat/stream_train.npy  LibriSpeech train-clean-100 + MUSAN train, clean and augmented
  WORK/feat/clips_cal.npy, stream_cal.npy   calibration only (checkpoint and threshold)
Outputs: OUT/MODEL.onnx and OUT/MODEL.train.json (curve, chosen checkpoint, threshold).

The classifier sees 16 embedding frames (1.28 s) and outputs one probability, the
shape openWakeWord's Model expects ([1, 16, 96] -> [1, 1]).

Hard negatives (off by default; the recipe turns them on per model): with
--hard-pool N, every --mine-every steps the network being trained scores every
training-stream window and keeps the N it scores highest; each step then adds
--hard-batch of them, weighted --hard-weight. With --near-batch K, each step also
adds K of this model's own training near-miss clips at the same weight, and the
adversarial clips are mined in-batch like the stream. Only the training split is
ever mined: calibration data still only picks the checkpoint and threshold, and
the held-out test split is never read here.
"""
import argparse
import json
import time

import numpy as np
import torch
from torch import nn

from common import OUT, WORK

FEAT = WORK / "feat"
COOLDOWN_FRAMES = 19          # 1.52 s: the runtime's 1.5 s cooldown (wake_helper.py)
GRID = [round(t, 2) for t in np.arange(0.05, 1.0, 0.05)]
TARGET_FAH = 0.25             # calibration target: half the release limit, for margin


class Net(nn.Module):
    def __init__(self, width=128):
        super().__init__()
        self.f = nn.Sequential(
            nn.Flatten(), nn.Linear(16 * 96, width), nn.LayerNorm(width), nn.ReLU(),
            nn.Linear(width, width), nn.LayerNorm(width), nn.ReLU(), nn.Linear(width, 1))

    def forward(self, x):
        return self.f(x)


class Export(nn.Module):
    def __init__(self, net):
        super().__init__()
        self.net = net

    def forward(self, x):
        return torch.sigmoid(self.net(x))


def windows(arr, starts):
    idx = starts[:, None] + np.arange(16)[None]
    return arr[idx]


def stream_starts(index):
    """All valid window starts that do not cross a file boundary."""
    return np.concatenate([np.arange(o, o + n - 15) for o, n, *_ in index["files"]])


def score_all(net, arr, starts, bs=65536):
    out = []
    with torch.no_grad():
        for i in range(0, len(starts), bs):
            x = torch.from_numpy(windows(arr, starts[i:i + bs]).astype(np.float32))
            out.append(torch.sigmoid(net(x)).numpy().ravel())
    return np.concatenate(out) if out else np.zeros(0)


def events(scores, th, cooldown=COOLDOWN_FRAMES):
    hits = np.where(scores >= th)[0]
    n, last = 0, -10**9
    for h in hits:
        if h - last > cooldown:
            n, last = n + 1, h
    return n


def stream_fah(net, arr, index, th_grid):
    """False-accept events per hour over the calibration stream, file by file."""
    counts = np.zeros(len(th_grid))
    for o, n, *_ in index["files"]:
        s = score_all(net, arr, np.arange(o, o + n - 15))
        counts += [events(s, t) for t in th_grid]
    return counts / index["hours"]


def clip_max(net, arr, offsets):
    starts, owner = [], []
    for i in range(len(offsets) - 1):
        a, b = offsets[i], offsets[i + 1]
        if b - a >= 16:
            starts.append(np.arange(a, b - 15))
            owner.append(np.full(b - a - 15, i))
    s = score_all(net, arr, np.concatenate(starts))
    owner = np.concatenate(owner)
    best = np.zeros(len(offsets) - 1)
    np.maximum.at(best, owner, s)
    return best


def mine(net, stream, starts, n):
    """Training-stream window starts the network currently scores highest."""
    net.eval()
    s = score_all(net, stream, starts)
    keep = np.argpartition(-s, n)[:n] if n < len(s) else np.arange(len(s))
    print(f"mined {len(keep)} hard windows, scores >= {s[keep].min():.3f}", flush=True)
    return starts[keep]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("model")
    ap.add_argument("--steps", type=int, default=40000)
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--width", type=int, default=128)
    ap.add_argument("--max-neg-weight", type=float, default=20.0)
    ap.add_argument("--hard-pool", type=int, default=0, help="mined training-stream windows kept")
    ap.add_argument("--hard-batch", type=int, default=512)
    ap.add_argument("--hard-weight", type=float, default=2.0)
    ap.add_argument("--mine-every", type=int, default=4000)
    ap.add_argument("--near-batch", type=int, default=0, help="own near-miss windows per step")
    args = ap.parse_args()
    torch.manual_seed(args.seed)
    rng = np.random.default_rng(args.seed)
    torch.set_num_threads(8)

    jobs = {}
    for line in (WORK / "clips/manifest.jsonl").read_text().splitlines():
        j = json.loads(line)
        jobs[j["id"]] = j
    clips = np.load(FEAT / "clips_train.npy", mmap_mode="r")
    ids = json.loads((FEAT / "clips_train_ids.json").read_text())
    kinds = [jobs[i]["kind"] for i in ids]
    models = [jobs[i]["model"] for i in ids]
    pos_idx = np.array([n for n, (k, m) in enumerate(zip(kinds, models)) if k in ("pos", "cont") and m == args.model])
    adv_idx = np.array([n for n, (k, m) in enumerate(zip(kinds, models)) if not (k in ("pos", "cont") and m == args.model)])
    near_idx = np.array([n for n, (k, m) in enumerate(zip(kinds, models)) if k == "near" and m == args.model])
    clips = np.asarray(clips)
    stream = np.load(FEAT / "stream_train.npy", mmap_mode="r")
    s_index = json.loads((FEAT / "stream_train_index.json").read_text())
    s_starts = stream_starts(s_index)
    print(f"{args.model}: {len(pos_idx)} positive windows, {len(adv_idx)} adversarial, "
          f"{len(s_starts)} stream windows ({s_index['hours']:.1f} h)", flush=True)

    cal = np.load(FEAT / "clips_cal.npy")
    cal_meta = json.loads((FEAT / "clips_cal_ids.json").read_text())
    cal_jobs = [jobs[i] for i in cal_meta["ids"]]
    cal_pos = np.array([j["kind"] == "pos" and j["model"] == args.model for j in cal_jobs])
    cal_near = np.array([j["kind"] == "near" and j["model"] == args.model for j in cal_jobs])
    cal_stream = np.load(FEAT / "stream_cal.npy")
    c_index = json.loads((FEAT / "stream_cal_index.json").read_text())

    net = Net(args.width)
    opt = torch.optim.Adam(net.parameters(), lr=1e-3)
    sched = torch.optim.lr_scheduler.OneCycleLR(opt, max_lr=1e-3, total_steps=args.steps, pct_start=0.1)
    bce = nn.BCEWithLogitsLoss(reduction="none")
    history, best = [], None
    hard = np.zeros(0, dtype=np.int64)
    t0 = time.time()
    for step in range(1, args.steps + 1):
        if args.hard_pool and step % args.mine_every == 0:
            hard = mine(net, stream, s_starts, args.hard_pool)
        net.train()
        p = torch.from_numpy(clips[rng.choice(pos_idx, 256)].astype(np.float32))
        a = torch.from_numpy(clips[rng.choice(adv_idx, 2048 if args.near_batch else 512)].astype(np.float32))
        g = torch.from_numpy(windows(stream, rng.choice(s_starts, 4096)).astype(np.float32))
        # hard-negative mining on the general stream: keep the 1024 most confusable windows
        with torch.no_grad():
            gl = net(g).squeeze(1)
            al = net(a).squeeze(1) if args.near_batch else None
        g = torch.cat([g[torch.topk(gl, 1024).indices], g[:256]])
        if args.near_batch:
            a = torch.cat([a[torch.topk(al, 384).indices], a[:128]])
        extra = []
        if len(hard):
            extra.append(torch.from_numpy(windows(stream, rng.choice(hard, args.hard_batch)).astype(np.float32)))
        if args.near_batch and len(near_idx):
            extra.append(torch.from_numpy(clips[rng.choice(near_idx, args.near_batch)].astype(np.float32)))
        h = torch.cat(extra) if extra else torch.zeros(0, 16, 96)
        x = torch.cat([p, a, g, h])
        y = torch.cat([torch.ones(len(p)), torch.zeros(len(a) + len(g) + len(h))])
        loss_each = bce(net(x).squeeze(1), y)
        w_neg = 1 + (args.max_neg_weight - 1) * min(1.0, step / (0.5 * args.steps))
        wn = torch.ones(len(x) - len(p))
        wn[len(a) + len(g):] = args.hard_weight
        loss = loss_each[: len(p)].mean() + w_neg * (wn * loss_each[len(p):]).sum() / wn.sum()
        opt.zero_grad()
        loss.backward()
        opt.step()
        sched.step()

        if step % 2000 == 0 or step == args.steps:
            net.eval()
            mx = clip_max(net, cal, cal_meta["offsets"])
            fah = stream_fah(net, cal_stream, c_index, GRID)
            ok = [i for i, f in enumerate(fah) if f <= TARGET_FAH]
            ti = ok[0] if ok else len(GRID) - 1
            rec = float(np.mean(mx[cal_pos] >= GRID[ti]))
            near = float(np.mean(mx[cal_near] >= GRID[ti]))
            row = {"step": step, "loss": float(loss), "threshold": GRID[ti], "recall": rec,
                   "fah": float(fah[ti]), "near_miss_accept": near,
                   "recall_at_0.5": float(np.mean(mx[cal_pos] >= 0.5)),
                   "fah_at_0.5": float(fah[GRID.index(0.5)])}
            history.append(row)
            print(json.dumps(row), f"{time.time() - t0:.0f}s", flush=True)
            # keep the checkpoint with the best calibration recall at <= TARGET_FAH,
            # breaking ties by fewer near-miss accepts
            key = (rec - 0.5 * near) if ok else -1
            if best is None or key >= best[0]:
                best = (key, row, {k: v.clone() for k, v in net.state_dict().items()})

    net.load_state_dict(best[2])
    net.eval()
    OUT.mkdir(parents=True, exist_ok=True)
    path = OUT / f"{args.model}.onnx"
    torch.onnx.export(Export(net), torch.zeros(1, 16, 96), str(path), input_names=["x"],
                      output_names=[args.model], opset_version=13, dynamo=False)
    (OUT / f"{args.model}.train.json").write_text(json.dumps(
        {"model": args.model, "args": vars(args), "chosen": best[1], "history": history,
         "positive_windows": int(len(pos_idx)), "adversarial_windows": int(len(adv_idx)),
         "own_near_miss_windows": int(len(near_idx)),
         "stream_hours": s_index["hours"], "calibration_stream_hours": c_index["hours"]}, indent=1))
    print("TRAIN_DONE", json.dumps(best[1]), flush=True)


if __name__ == "__main__":
    main()
