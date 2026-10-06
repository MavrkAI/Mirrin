"""Score the trained wake model on held-out synthetic phrases (macOS voices at rates not used in training)."""
import subprocess, sys, numpy as np, soundfile as sf, glob, os
from openwakeword.model import Model
model_path = sys.argv[1] if len(sys.argv) > 1 else "hey_maverick.onnx"
m = Model(wakeword_models=[model_path], inference_framework="onnx")
name = list(m.models.keys())[0]

def score_wav(path):
    audio, sr = sf.read(path, dtype="int16")
    if audio.ndim > 1: audio = audio[:, 0]
    m.reset(); best = 0.0
    for i in range(0, len(audio) - 1280, 1280):
        best = max(best, float(m.predict(audio[i:i+1280])[name]))
    return best

def say(text, voice, rate):
    subprocess.run(["say", "-v", voice, "-r", str(rate), "-o", "/tmp/ev.aiff", text], check=True)
    subprocess.run(["sox", "-q", "/tmp/ev.aiff", "-r", "16000", "-c", "1", "-b", "16", "/tmp/ev.wav"], check=True)
    return score_wav("/tmp/ev.wav")

voices = ["Samantha", "Daniel", "Karen", "Moira", "Rishi", "Fred", "Alex"]
pos_texts = ["Hey Maverick", "Hey Maverick, what's on this afternoon?", "hey maverick remind me to call mum", "Hey, Maverick."]
neg_texts = ["Hey everybody, what's on this afternoon?", "Maverick was a good film", "hey mavis, remind me to call mum",
             "have a rick and morty marathon", "hey there, what's the weather", "the maverick pilot flew fast", "hey Siri",
             "hey Jarvis, lights", "I'm heading out for a bit", "can you email Priya the deck", "hey", "nothing to see here at all"]
pos, neg = [], []
for v in voices:
    for t in pos_texts:
        for r in [165, 205]:
            try: pos.append(say(t, v, r))
            except Exception: pass
    for t in neg_texts:
        try: neg.append(say(t, v, 185))
        except Exception: pass
pos, neg = np.array(pos), np.array(neg)
for th in [0.3, 0.5, 0.7]:
    print(f"threshold {th}: recall {np.mean(pos>=th):.2f} ({int(np.sum(pos>=th))}/{len(pos)}), false accepts {int(np.sum(neg>=th))}/{len(neg)}")
print("positive scores: median %.2f min %.2f" % (np.median(pos), pos.min()))
print("negative scores: max %.2f" % neg.max())
# false positives on background/babble clips
fp = [score_wav(p) for p in glob.glob("hey_maverick/background/*.wav")]
print("background max score: %.2f" % max(fp))
