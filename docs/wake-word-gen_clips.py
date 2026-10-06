"""Generate synthetic training clips for the "hey maverick" wake word.
Positives: Kokoro (all English voices) + macOS say voices, several phrasings and speeds.
Negatives: phoneme-adversarial phrases from openWakeWord + generic sentences.
Also synthesises background-noise clips and room impulse responses for augmentation.
"""
import os, random, subprocess, sys, uuid, math
import numpy as np, soundfile as sf
# The twin's home: MIRRIN_HOME when it is set, else ~/.mirrin.
HOME = os.environ.get("MIRRIN_HOME") or os.path.expanduser("~/.mirrin")
sys.path.insert(0, os.path.join(HOME, "tts"))
from kokoro_onnx import Kokoro
from openwakeword.data import generate_adversarial_texts

random.seed(7)
ROOT = os.path.join(HOME, "wake/hey_maverick")
DIRS = {k: os.path.join(ROOT, k) for k in ["positive_train", "positive_test", "negative_train", "negative_test"]}
for d in list(DIRS.values()) + [os.path.join(ROOT, "background"), os.path.join(ROOT, "rir")]:
    os.makedirs(d, exist_ok=True)

k = Kokoro(os.path.join(HOME, "tts/kokoro-v1.0.onnx"), os.path.join(HOME, "tts/voices-v1.0.bin"))
kvoices = [v for v in k.get_voices() if v[:2] in ("am", "af", "bm", "bf")]
say_voices = [l.split("  ")[0].strip() for l in subprocess.run(["say", "-v", "?"], capture_output=True, text=True).stdout.splitlines() if " en_" in l]
say_voices = [v for v in say_voices if "(" not in v][:12]

def kokoro_clip(text, voice, speed, path):
    lang = "en-gb" if voice.startswith("b") else "en-us"
    s, r = k.create(text, voice=voice, speed=speed, lang=lang)
    s = s / (np.max(np.abs(s)) + 1e-6) * random.uniform(0.5, 0.95)
    sf.write(path, s.astype(np.float32), r)

def say_clip(text, voice, rate, path):
    tmp = path + ".aiff"
    subprocess.run(["say", "-v", voice, "-r", str(rate), "-o", tmp, text], check=True)
    subprocess.run(["sox", "-q", tmp, "-r", "16000", "-c", "1", "-b", "16", path], check=True)
    os.remove(tmp)

def write_set(items, gen, label):
    n = 0
    for text, voice, speed, engine in items:
        path = os.path.join(gen, uuid.uuid4().hex + ".wav")
        try:
            if engine == "kokoro":
                kokoro_clip(text, voice, speed, path)
            else:
                say_clip(text, voice, speed, path)
            n += 1
        except Exception as e:
            print("skip", text, voice, e, flush=True)
        if n % 100 == 0:
            print(label, n, flush=True)
    print(label, "done", n, flush=True)

# ---- positives
phrasings = ["Hey Maverick", "Hey, Maverick.", "hey maverick", "Hey Maverick!", "Hey Mavrick"]
pos = []
for v in kvoices:
    for p in phrasings:
        for sp in [0.85, 0.95, 1.05, 1.2]:
            pos.append((p, v, sp, "kokoro"))
for v in say_voices:
    for p in phrasings[:4]:
        for rate in [150, 180, 210]:
            pos.append((p, v, rate, "say"))
random.shuffle(pos)
cut = int(len(pos) * 0.85)
print("positives", len(pos), flush=True)

# ---- negatives: adversarial + generic
adv = generate_adversarial_texts("hey maverick", N=400, include_partial_phrase=1.0, include_input_words=0.2)
generic = ["what time is it", "turn the lights off", "hey everyone, listen up", "maverick was a good film",
           "hey mavis, how are you", "have a rick and morty marathon", "hey, have a look at this", "hey there",
           "the weather is lovely today", "call the accountant at three", "email priya the deck", "hey mum",
           "a very quick brown fox", "hey siri", "ok google", "alexa, stop", "hey jarvis", "maverick", "hey",
           "never mind", "play some music", "how far is the airport", "he's a bit of a maverick, that one"]
neg_texts = list(adv) + generic * 4
neg = []
for t in neg_texts:
    if random.random() < 0.8:
        neg.append((t, random.choice(kvoices), random.choice([0.9, 1.0, 1.1]), "kokoro"))
    else:
        neg.append((t, random.choice(say_voices), random.choice([160, 190]), "say"))
random.shuffle(neg)
ncut = int(len(neg) * 0.85)
print("negatives", len(neg), flush=True)

write_set(pos[:cut], DIRS["positive_train"], "pos_train")
write_set(pos[cut:], DIRS["positive_test"], "pos_test")
write_set(neg[:ncut], DIRS["negative_train"], "neg_train")
write_set(neg[ncut:], DIRS["negative_test"], "neg_test")

# ---- background noise: coloured noise + babble made from spoken sentences
bg = os.path.join(ROOT, "background")
for i in range(12):
    n = 16000 * 10
    white = np.random.randn(n)
    beta = random.choice([0, 1, 2])  # white, pink-ish, brown-ish via cumulative filtering
    sig = white
    for _ in range(beta):
        sig = np.cumsum(sig); sig -= np.mean(sig); sig /= (np.max(np.abs(sig)) + 1e-6)
    sig = sig / (np.max(np.abs(sig)) + 1e-6) * 0.5
    sf.write(os.path.join(bg, f"noise{i}.wav"), sig.astype(np.float32), 16000)
babble_texts = ["the quarterly numbers look better than expected, though the second half is soft", "can someone grab the door, my hands are full",
                "we should leave by seven if we want to beat the traffic on the freeway", "i think the recipe needs more salt and a lot less time in the oven"]
for i in range(8):
    parts = []
    for _ in range(3):
        s, r = k.create(random.choice(babble_texts), voice=random.choice(kvoices), speed=1.0, lang="en-us")
        parts.append(s)
    mix = np.concatenate(parts)
    mix = mix / (np.max(np.abs(mix)) + 1e-6) * 0.4
    sf.write(os.path.join(bg, f"babble{i}.wav"), mix.astype(np.float32), 16000)

# ---- room impulse responses: exponentially decaying noise
rir = os.path.join(ROOT, "rir")
for i in range(10):
    rt60 = random.uniform(0.15, 0.9)
    n = int(16000 * rt60 * 1.5)
    t = np.arange(n) / 16000
    h = np.random.randn(n) * np.exp(-6.9 * t / rt60)
    h[0] = 1.0
    h = h / np.max(np.abs(h))
    sf.write(os.path.join(rir, f"rir{i}.wav"), h.astype(np.float32), 16000)
print("ALL_DONE", flush=True)
