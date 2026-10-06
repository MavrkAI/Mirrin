"""Extra clips: every macOS English voice at more rates, plus noise/babble/silence as negatives."""
import os, random, subprocess, uuid, numpy as np, soundfile as sf, sys
# The twin's home: MIRRIN_HOME when it is set, else ~/.mirrin.
HOME = os.environ.get("MIRRIN_HOME") or os.path.expanduser("~/.mirrin")
sys.path.insert(0, os.path.join(HOME, "tts"))
from kokoro_onnx import Kokoro
random.seed(11)
ROOT = os.path.join(HOME, "wake/hey_maverick")
k = Kokoro(os.path.join(HOME, "tts/kokoro-v1.0.onnx"), os.path.join(HOME, "tts/voices-v1.0.bin"))
kvoices = [v for v in k.get_voices() if v[:2] in ("am", "af", "bm", "bf")]
say_voices = [l.split("  ")[0].strip() for l in subprocess.run(["say", "-v", "?"], capture_output=True, text=True).stdout.splitlines() if " en_" in l]
print("say voices", len(say_voices), flush=True)

def say_clip(text, voice, rate, path):
    tmp = path + ".aiff"
    subprocess.run(["say", "-v", voice, "-r", str(rate), "-o", tmp, text], check=True)
    subprocess.run(["sox", "-q", tmp, "-r", "16000", "-c", "1", "-b", "16", path], check=True)
    os.remove(tmp)

def kokoro_clip(text, voice, speed, path):
    s, r = k.create(text, voice=voice, speed=speed, lang="en-gb" if voice.startswith("b") else "en-us")
    s = s / (np.max(np.abs(s)) + 1e-6) * random.uniform(0.4, 0.95)
    tmp = path + ".24k.wav"; sf.write(tmp, s.astype(np.float32), r)
    subprocess.run(["sox", "-q", tmp, "-r", "16000", "-c", "1", "-b", "16", path], check=True); os.remove(tmp)

def out(split): return os.path.join(ROOT, split, uuid.uuid4().hex + ".wav")

# positives: all say voices × phrasings × rates; kokoro at extra speeds
n = 0
for v in say_voices:
    for t in ["Hey Maverick", "Hey, Maverick.", "hey maverick", "Hey Maverick!"]:
        for r in [140, 165, 195, 230]:
            try: say_clip(t, v, r, out("positive_test" if random.random() < 0.15 else "positive_train")); n += 1
            except Exception as e: print("skip", v, e, flush=True)
for v in kvoices:
    for t in ["Hey Maverick", "hey maverick"]:
        for sp in [0.75, 1.0, 1.35]:
            kokoro_clip(t, v, sp, out("positive_test" if random.random() < 0.15 else "positive_train")); n += 1
print("extra positives", n, flush=True)

# negatives: noise, babble, silence, near-misses
texts = ["hey everybody", "hey mav", "maverick", "hey ma", "hey marvin", "hey mark", "hey have a look", "hey", "have a good one",
         "hey maddie", "hey mavis", "hey matthew", "everything is fine", "hey there mate", "hey mum", "hey man"]
m = 0
for t in texts:
    for _ in range(6):
        if random.random() < 0.7: kokoro_clip(t, random.choice(kvoices), random.choice([0.9, 1.0, 1.15]), out("negative_test" if random.random() < 0.15 else "negative_train"))
        else: say_clip(t, random.choice(say_voices), random.choice([160, 200]), out("negative_test" if random.random() < 0.15 else "negative_train"))
        m += 1
for i in range(80):
    kind = random.choice(["white", "pink", "brown", "silence", "babble"])
    dur = 16000 * 2
    if kind == "silence":
        sig = np.random.randn(dur) * random.uniform(0.0005, 0.003)
    elif kind == "babble":
        parts = [k.create(random.choice(["we should leave by seven", "the numbers look better than expected", "can someone grab the door", "i think it needs more salt"]), voice=random.choice(kvoices), speed=1.0, lang="en-us")[0] for _ in range(2)]
        sig = np.concatenate(parts); sig = sig / (np.max(np.abs(sig)) + 1e-6) * random.uniform(0.2, 0.7)
        tmp = "/tmp/babble24.wav"; sf.write(tmp, sig.astype(np.float32), 24000); subprocess.run(["sox", "-q", tmp, "-r", "16000", "/tmp/babble16.wav"], check=True); sig, _ = sf.read("/tmp/babble16.wav"); sig = sig[:dur]
    else:
        sig = np.random.randn(dur)
        for _ in range({"white": 0, "pink": 1, "brown": 2}[kind]):
            sig = np.cumsum(sig); sig -= np.mean(sig); sig /= (np.max(np.abs(sig)) + 1e-6)
        sig = sig / (np.max(np.abs(sig)) + 1e-6) * random.uniform(0.05, 0.8)
    sf.write(out("negative_test" if random.random() < 0.15 else "negative_train"), np.asarray(sig, dtype=np.float32), 16000); m += 1
print("extra negatives", m, flush=True)
print("EXTRA_DONE", flush=True)
