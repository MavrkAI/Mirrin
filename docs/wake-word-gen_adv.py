"""Targeted negatives: 'maverick' inside other sentences, and 'hey <anything else>'."""
import os, random, subprocess, uuid, numpy as np, soundfile as sf, sys
# The twin's home: MIRRIN_HOME when it is set, else ~/.mirrin.
HOME = os.environ.get("MIRRIN_HOME") or os.path.expanduser("~/.mirrin")
sys.path.insert(0, os.path.join(HOME, "tts"))
from kokoro_onnx import Kokoro
random.seed(23)
ROOT = os.path.join(HOME, "wake/hey_maverick")
k = Kokoro(os.path.join(HOME, "tts/kokoro-v1.0.onnx"), os.path.join(HOME, "tts/voices-v1.0.bin"))
kvoices = [v for v in k.get_voices() if v[:2] in ("am", "af", "bm", "bf")]
say_voices = [l.split("  ")[0].strip() for l in subprocess.run(["say", "-v", "?"], capture_output=True, text=True).stdout.splitlines() if " en_" in l]
def out(): return os.path.join(ROOT, "negative_test" if random.random() < 0.15 else "negative_train", uuid.uuid4().hex + ".wav")
def clip(text):
    path = out()
    if random.random() < 0.7:
        v = random.choice(kvoices)
        s, r = k.create(text, voice=v, speed=random.choice([0.9, 1.0, 1.15]), lang="en-gb" if v.startswith("b") else "en-us")
        s = s / (np.max(np.abs(s)) + 1e-6) * random.uniform(0.4, 0.95)
        tmp = path + ".24k.wav"; sf.write(tmp, s.astype(np.float32), r)
        subprocess.run(["sox", "-q", tmp, "-r", "16000", "-c", "1", "-b", "16", path], check=True); os.remove(tmp)
    else:
        tmp = path + ".aiff"
        subprocess.run(["say", "-v", random.choice(say_voices), "-r", str(random.choice([160, 190, 220])), "-o", tmp, text], check=True)
        subprocess.run(["sox", "-q", tmp, "-r", "16000", "-c", "1", "-b", "16", path], check=True); os.remove(tmp)
names = ["everybody", "everyone", "anybody", "somebody", "mavis", "marvin", "mark", "matt", "matthew", "maddie", "maggie", "max", "maxine", "mabel",
         "mac", "mavric", "madison", "maria", "marie", "martin", "mavi", "avery", "harrison", "melanie", "mel", "mate", "man", "mum", "dad", "guys",
         "you", "there", "siri", "google", "alexa", "jarvis", "mycroft", "cortana", "bixby", "computer", "friday", "victor", "maverick's brother",
         "ma", "mav", "maverick's", "mavericks", "mabrick", "macbeth", "maverine", "magnus", "malcolm", "mason", "marcus"]
sentences = ["{} was a good film", "he's a bit of a {}", "the {} pilot flew fast", "top gun {} is on tonight", "she's such a {}", "{} is the word",
             "i met a {} at the pub", "that's a {} move", "call me {}", "a real {}", "{}, the film", "the {}s are playing tonight", "{}", "{}."]
phrases = []
for n in names:
    phrases += [f"hey {n}", f"hey {n}, what's on", f"hey, {n}"]
for s in sentences:
    phrases += [s.format("maverick"), s.format("Maverick")]
phrases += ["hey", "hey hey", "hey hey hey", "hey, hey", "okay", "hey okay", "hey wait", "hey look", "hey listen", "hey stop", "hey now", "hey what",
            "hey come on", "hey have a look at this", "hey did you see that", "may I", "hey may I", "a very rich man", "have a rick", "hey rick", "hey merrick",
            "hey derrick", "hey eric", "hey erica", "hey maverick's brother said", "may very quick", "make a wreck", "hey make a wreck"]
random.shuffle(phrases)
n = 0
for p in phrases:
    for _ in range(2):
        try: clip(p); n += 1
        except Exception as e: print("skip", p, e, flush=True)
    if n % 100 == 0: print("adv", n, flush=True)
# more pure noise / silence negatives
for i in range(60):
    dur = 16000 * 2
    kind = random.choice(["white", "pink", "brown", "silence", "hum"])
    if kind == "silence": sig = np.random.randn(dur) * random.uniform(0.0005, 0.003)
    elif kind == "hum": t = np.arange(dur) / 16000; sig = 0.3 * np.sin(2 * np.pi * random.choice([50, 60, 120]) * t) + np.random.randn(dur) * 0.02
    else:
        sig = np.random.randn(dur)
        for _ in range({"white": 0, "pink": 1, "brown": 2}[kind]): sig = np.cumsum(sig); sig -= np.mean(sig); sig /= (np.max(np.abs(sig)) + 1e-6)
        sig = sig / (np.max(np.abs(sig)) + 1e-6) * random.uniform(0.05, 0.9)
    sf.write(out(), np.asarray(sig, dtype=np.float32), 16000); n += 1
print("ADV_DONE", n, flush=True)
