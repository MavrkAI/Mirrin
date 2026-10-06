"""Overlapped-speech clips: the wake phrase spoken over the twin's own (Kokoro) voice, and other phrases over it."""
import os, random, subprocess, uuid, glob, numpy as np, soundfile as sf, sys
# The twin's home: MIRRIN_HOME when it is set, else ~/.mirrin.
HOME = os.environ.get("MIRRIN_HOME") or os.path.expanduser("~/.mirrin")
sys.path.insert(0, os.path.join(HOME, "tts"))
from kokoro_onnx import Kokoro
random.seed(31)
ROOT = os.path.join(HOME, "wake/hey_maverick")
k = Kokoro(os.path.join(HOME, "tts/kokoro-v1.0.onnx"), os.path.join(HOME, "tts/voices-v1.0.bin"))
kvoices = [v for v in k.get_voices() if v[:2] in ("am", "af", "bm", "bf")]
twin_voices = ["bf_lily", "bm_george", "bm_lewis", "af_bella", "af_heart", "am_michael", "bf_emma", "am_fenrir"]
speech = ["One. Two. Three. Four. Five. Six. Seven. Eight.", "Your three o'clock has moved to Thursday, and I've sent Priya the updated deck.",
          "Nothing on the books this afternoon, sir. Shall I run an inbox triage?", "Twenty past five on Thursday evening, sir.",
          "The forecast is twenty-one and clear. Traffic on the freeway is light.", "Reminder: call the accountant. Would you like me to dial?",
          "I've drafted the reply. It thanks them and proposes Tuesday at ten."]
def to16(s, r):
    tmp = "/tmp/mix24.wav"; sf.write(tmp, s.astype(np.float32), r)
    subprocess.run(["sox", "-q", tmp, "-r", "16000", "-c", "1", "-b", "16", "/tmp/mix16.wav"], check=True)
    a, _ = sf.read("/tmp/mix16.wav", dtype="float32"); return a
# a pool of the twin's speech at 16 kHz
pool = []
for v in twin_voices:
    for t in speech:
        s, r = k.create(t, voice=v, speed=1.0, lang="en-gb" if v.startswith("b") else "en-us")
        pool.append(to16(s, r))
print("pool", len(pool), flush=True)
def overlay(fg, ratio):
    bg = random.choice(pool)
    if len(bg) < len(fg) + 8000:
        bg = np.concatenate([bg, random.choice(pool)])
    start = random.randint(0, len(bg) - len(fg) - 1)
    seg = bg[start:start + len(fg)].copy()
    fg = fg / (np.max(np.abs(fg)) + 1e-6)
    seg = seg / (np.max(np.abs(seg)) + 1e-6)
    mix = seg + fg * ratio
    return (mix / (np.max(np.abs(mix)) + 1e-6) * random.uniform(0.5, 0.95)).astype(np.float32)
def out(kind): return os.path.join(ROOT, kind + ("_test" if random.random() < 0.15 else "_train"), uuid.uuid4().hex + ".wav")
pos_src = glob.glob(os.path.join(ROOT, "positive_train", "*.wav"))
neg_src = glob.glob(os.path.join(ROOT, "negative_train", "*.wav"))
random.shuffle(pos_src); random.shuffle(neg_src)
n = 0
for p in pos_src[:700]:
    a, _ = sf.read(p, dtype="float32")
    if len(a) < 4000: continue
    sf.write(out("positive"), overlay(a, random.choice([0.7, 1.0, 1.5, 2.0, 3.0])), 16000); n += 1
print("overlapped positives", n, flush=True)
m = 0
for p in neg_src[:500]:
    a, _ = sf.read(p, dtype="float32")
    if len(a) < 4000: continue
    sf.write(out("negative"), overlay(a, random.choice([0.7, 1.0, 1.5, 2.0])), 16000); m += 1
# the twin's speech alone as negatives (it must never wake itself)
for i in range(120):
    a = random.choice(pool); s = random.randint(0, max(0, len(a) - 32000)); seg = a[s:s + 32000]
    sf.write(out("negative"), (seg / (np.max(np.abs(seg)) + 1e-6) * random.uniform(0.3, 0.9)).astype(np.float32), 16000); m += 1
print("overlapped/self negatives", m, flush=True)
# longer babble backgrounds in the twin's voices for the augmenter
bgd = os.path.join(ROOT, "background")
for i in range(30):
    a = np.concatenate([random.choice(pool) for _ in range(3)])
    sf.write(os.path.join(bgd, f"twin{i}.wav"), (a / (np.max(np.abs(a)) + 1e-6) * 0.6).astype(np.float32), 16000)
print("MIX_DONE", flush=True)
