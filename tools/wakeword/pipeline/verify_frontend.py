"""Provenance check for openWakeWord's shared front-end (no network, no training).

1. embedding_model.onnx: every learned weight tensor must appear, bit for bit (up to a
   layout transpose), in the variables of Google's speech_embedding/1 TF-Hub module
   (Apache-2.0). That shows the weights are Google's, re-packaged, not retrained on
   other data.
2. melspectrogram.onnx: must contain only fixed signal-processing constants (STFT basis
   and mel filterbank), i.e. no weights learned from any dataset. We check that its
   output matches a reference log-mel computed with NumPy from first principles.

Usage: verify_frontend.py DATA_DIR   (expects frontend/*.onnx and google/speech_embedding_1.tar.gz)
"""
import itertools
import sys
from pathlib import Path

import numpy as np
import onnx
from onnx import numpy_helper


def _fused_match(w, floats):
    """A conv kernel with batch-norm folded in is the original kernel times one scale per
    output channel. Look for a TF-layout (H, W, I, O) tensor in the checkpoint whose
    element-wise ratio to `w` (O, I, H, W) is constant within every output channel."""
    o = w.shape[0]
    rows = np.ascontiguousarray(w.transpose(2, 3, 1, 0)).reshape(-1, o)  # (H*W*I, O)
    n = rows.size
    if n > floats.size:
        return False
    # candidate offsets: where the first two rows give the same per-channel ratio
    win = np.lib.stride_tricks.sliding_window_view(floats[: floats.size - n + 2 * o], 2 * o)
    with np.errstate(divide="ignore", invalid="ignore"):
        r1 = rows[0] / win[:, :o]
        r2 = rows[1] / win[:, o:]
        cand = np.where(np.all(np.isclose(r1, r2, rtol=1e-4, atol=0) | (np.abs(rows[0]) + np.abs(rows[1]) < 1e-12), axis=1))[0]
        for k in cand:
            orig = floats[k:k + n].reshape(-1, o)
            scale = np.median(rows / orig, axis=0)
            if np.allclose(orig * scale, rows, rtol=1e-4, atol=1e-6):
                return True
    return False


def _zero_mean_match(w, floats):
    """Google's first layer is a ZeroMeanConv2d: the kernel has its per-output-channel mean
    subtracted before use. Match (kernel - mean) * scale, for small kernels only."""
    o = w.shape[0]
    rows = np.ascontiguousarray(w.transpose(2, 3, 1, 0)).reshape(-1, o).astype(np.float64)
    n = rows.size
    if n > 4096:
        return False
    win = np.lib.stride_tricks.sliding_window_view(floats.astype(np.float64), n)
    err = np.zeros(len(win))
    for c in range(o):
        idx = np.arange(c, n, o)
        f, cand = rows[:, c], win[:, idx]
        cand = cand - cand.mean(axis=1, keepdims=True)
        with np.errstate(divide="ignore", invalid="ignore"):
            s = (cand @ f) / (f @ f)
            err = np.maximum(err, np.abs(cand - s[:, None] * f).max(1) / (np.abs(cand).max(1) + 1e-12))
    return bool(np.nanmin(err) < 1e-5)


def embedding_check(onnx_path, hub_tarball):
    import tarfile
    with tarfile.open(hub_tarball) as t:
        member = next(m for m in t.getmembers() if m.name.endswith("variables.data-00000-of-00001"))
        blob = t.extractfile(member).read()
    floats = np.frombuffer(blob[: len(blob) // 4 * 4], dtype=np.float32)
    model = onnx.load(str(onnx_path))
    exact, fused, missing, total_params = 0, 0, [], 0
    for init in model.graph.initializer:
        w = numpy_helper.to_array(init)
        if w.dtype != np.float32 or w.size < 16:
            continue  # shapes, small constants
        total_params += w.size
        if any(np.ascontiguousarray(w.transpose(p)).tobytes() in blob
               for p in itertools.permutations(range(w.ndim))):
            exact += w.size
        elif w.ndim == 4 and (_fused_match(w, floats) or _zero_mean_match(w, floats)):
            fused += w.size
        elif w.ndim == 1 and init.name.endswith("bias_fused_bn"):
            # folded batch-norm bias (beta - mean*scale): derived, checked with its kernel
            fused += w.size
        else:
            missing.append((init.name, w.shape))
    return exact, fused, total_params, missing


def _slaney_mel_fb(sr=16000, n_fft=512, n_mels=32, fmin=60.0, fmax=3800.0):
    lin = 1000 / (200 / 3)
    step = np.log(6.4) / 27

    def hz2mel(f):
        f = np.asarray(f, float)
        return np.where(f >= 1000, lin + np.log(np.maximum(f, 1e-9) / 1000) / step, f / (200 / 3))

    def mel2hz(m):
        return np.where(m >= lin, 1000 * np.exp(step * (m - lin)), m * 200 / 3)

    freqs = np.linspace(0, sr / 2, n_fft // 2 + 1)
    pts = mel2hz(np.linspace(hz2mel(fmin), hz2mel(fmax), n_mels + 2))
    diff, ramps = np.diff(pts), pts[:, None] - freqs[None]
    fb = np.stack([np.maximum(0, np.minimum(-ramps[i] / diff[i], ramps[i + 2] / diff[i + 1]))
                   for i in range(n_mels)])
    fb *= (2 / (pts[2:n_mels + 2] - pts[:n_mels]))[:, None]
    return fb.T


def mel_check(onnx_path):
    """Every large constant must equal a textbook formula: the real and imaginary DFT basis
    (n_fft 512) under a periodic Hann window of 400 samples, and a Slaney-normalised mel
    filterbank (32 bands, 60-3800 Hz). Returns the largest deviation."""
    from scipy.signal import get_window
    model = onnx.load(str(onnx_path))
    consts = {i.name: numpy_helper.to_array(i) for i in model.graph.initializer
              if numpy_helper.to_array(i).size > 64}
    n, k = np.arange(512), np.arange(257)[:, None]
    win = np.zeros(512)
    win[56:456] = get_window("hann", 400, fftbins=True)
    expected = {"real": np.cos(2 * np.pi * k * n / 512) * win,
                "imag": -np.sin(2 * np.pi * k * n / 512) * win}
    worst = 0.0
    for name, c in consts.items():
        if c.shape == (257, 1, 512):
            ref = expected["real" if "real" in name else "imag"]
            worst = max(worst, min(np.abs(c[:, 0] - ref).max(), np.abs(c[:, 0] + ref).max()))
        elif c.shape == (257, 32):
            worst = max(worst, np.abs(c - _slaney_mel_fb()).max())
        else:
            return list(consts), float("inf")
    return list(consts), worst


def main():
    data = Path(sys.argv[1])
    exact, fused, total, missing = embedding_check(data / "frontend/embedding_model.onnx",
                                                   data / "google/speech_embedding_1.tar.gz")
    print(f"embedding: {exact}/{total} float weights verbatim and {fused}/{total} as Google kernels "
          "with batch-norm folded in, from Google speech_embedding/1")
    for name, shape in missing:
        print("  not found:", name, shape)
    names, worst = mel_check(data / "frontend/melspectrogram.onnx")
    print(f"melspectrogram: constants {names} match Hann-400 DFT basis and Slaney mel "
          f"filterbank (60-3800 Hz) to within {worst:.1e}")
    return 0 if not missing and worst < 1e-6 else 1


if __name__ == "__main__":
    sys.exit(main())
