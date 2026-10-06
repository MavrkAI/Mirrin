#!/usr/bin/env python
"""Mirrin ears: on-device wake word (openWakeWord) + utterance capture.

Streams the microphone (via sox) through the wake-word model. Nothing is
transcribed or stored until the wake word fires; then the following utterance
is captured to a WAV and its path is printed. Protocol on stdout, one line each:

  ready
  wake <score>
  wakeclip <path>         (the wake phrase alone, so the host can confirm it early)
  utterance <path> (wake 1.2s[ bare]) | (followup 0.8s)
                          bare: nothing was said after the wake phrase
  silence - (wake|followup)
                          (asked to listen but nobody spoke)
  near <score>            (a wake that didn't quite make it)
  interrupt               (someone is talking over the host)
  mute / unmute           (the microphone gives pure digital silence, e.g. no
                           permission; and when sound comes back)
  err <message>

Commands on stdin:
  listen                  capture the next utterance now, without a wake word
  pause / resume          ignore / honour the wake word
  speaking / idle         the host started / stopped talking
  reject / confirm        the last interruption was / wasn't the host's own voice
"""
import os
import queue
import subprocess
import sys
import threading
import time
from collections import deque

import numpy as np
import soundfile as sf
from openwakeword.model import Model

HERE = os.path.dirname(os.path.abspath(__file__))
RATE = 16000
CHUNK = 1280  # 80 ms, what openWakeWord expects
# No default: openWakeWord's stock models each wake to another name.
MODEL = os.environ.get("WAKE_MODEL") or ""
THRESHOLD = float(os.environ.get("WAKE_THRESHOLD") or "0.5")
# While the host is speaking, its own voice masks the wake word; be more eager
# (the host confirms the phrase with the transcript anyway).
THRESHOLD_SPEAKING = float(os.environ.get("WAKE_THRESHOLD_SPEAKING") or str(min(THRESHOLD, 0.1)))
OUT = os.environ.get("WAKE_OUT") or "/tmp/mirrin-utterance.wav"
CLIP = os.environ.get("WAKE_CLIP") or (os.path.splitext(OUT)[0] + "-wake.wav")
MAX_SECONDS = float(os.environ.get("MAX_SECONDS") or "30")
FOLLOWUP_SECONDS = float(os.environ.get("FOLLOWUP_SECONDS") or "6")
# Seconds of pure digital silence before the host is told the microphone gives nothing.
MUTE_SECONDS = float(os.environ.get("MUTE_SECONDS") or "30")
COOLDOWN = 1.5
# How much louder than our own playback a voice must be to count as talking over us
# (0 disables energy-based interruption; the wake word always works).
TALKOVER_RATIO = float(os.environ.get("TALKOVER_RATIO") or "1.6")

state = {"paused": False, "listen": False, "speaking": False}
own = {"peak": 0.0, "candidate": 0.0}  # how loud our own playback gets at the mic (learned)


def log(line):
    sys.stdout.write(line + "\n")
    sys.stdout.flush()


def stdin_loop():
    for line in sys.stdin:
        cmd = line.strip()
        if cmd == "listen":
            state["listen"] = True
        elif cmd == "pause":
            state["paused"] = True
        elif cmd == "resume":
            state["paused"] = False
        elif cmd == "speaking":
            state["speaking"] = True
            state["listen"] = False  # the host is talking; don't capture its own voice
        elif cmd == "idle":
            state["speaking"] = False
        elif cmd == "reject":
            # the last interruption was our own voice: remember how loud that gets
            own["peak"] = max(own["peak"], own["candidate"])
            sys.stderr.write(f"own playback peak now {own['peak']:.4f}\n"); sys.stderr.flush()
        elif cmd == "confirm":
            pass
    # stdin closed: the host is gone. Never outlive it holding the microphone.
    os._exit(0)


def read_exact(stream, n):
    """Pipes return short reads; assemble a full chunk or return b"" at EOF."""
    buf = b""
    while len(buf) < n:
        part = stream.read(n - len(buf))
        if not part:
            return b""
        buf += part
    return buf


class Mic:
    """The microphone stream, read on its own thread into a queue.

    Reading ahead on a thread lets drain() drop audio that piled up while
    nobody was listening (a follow-up capture must start from 'now', not from
    the host's own playback) the same way on every platform: Windows pipes
    have no non-blocking mode, and fcntl doesn't exist there."""

    def __init__(self, stream, chunk_bytes):
        self.stream = stream
        self.n = chunk_bytes
        self.q = queue.Queue()
        self.eof = False
        threading.Thread(target=self._pump, daemon=True).start()

    def _pump(self):
        while True:
            buf = read_exact(self.stream, self.n)
            self.q.put(buf)
            if not buf:
                return

    def read(self):
        """The next chunk, or b"" once the stream has ended."""
        if self.eof:
            return b""
        buf = self.q.get()
        if not buf:
            self.eof = True
        return buf

    def drain(self):
        """Drop everything read so far; returns how many bytes were dropped."""
        dropped = 0
        while not self.eof:
            try:
                buf = self.q.get_nowait()
            except queue.Empty:
                break
            if not buf:
                self.eof = True
                break
            dropped += len(buf)
        return dropped


def rms(chunk):
    return float(np.sqrt(np.mean(chunk.astype(np.float32) ** 2))) / 32768.0


def capture(mic, first_chunks, wait_for_speech, window=None):
    """Record until ~1 s of silence. Returns "speech" if speech was captured,
    "bare" if only the wake phrase (the pre-roll) was, else "".
    window overrides how long to wait for speech to start (follow-ups)."""
    noise = 0.004
    frames = list(first_chunks)
    speech_seen = False  # wait for the command after the wake phrase (or the follow-up)
    talking_frames = 0
    silence_run = 0.0
    waited = 0.0
    t0 = time.time()
    while time.time() - t0 < MAX_SECONDS:
        buf = mic.read()
        if not buf:
            break
        chunk = np.frombuffer(buf, dtype=np.int16)
        if wait_for_speech and state["speaking"]:
            sys.stderr.write("follow-up capture aborted: host is speaking\n"); sys.stderr.flush()
            return ""  # the host started talking; a follow-up capture would only hear it
        level = rms(chunk)
        # Adaptive: speech is well above the room's noise floor; the absolute floor is low
        # enough for a quiet microphone (input volume turned down) to still register.
        talking = level > max(noise * 2.5, 0.005)
        if not speech_seen:
            waited += CHUNK / RATE
            frames.append(chunk)
            if talking:
                speech_seen = True
            else:
                noise = 0.9 * noise + 0.1 * level
                limit = (window if window is not None else FOLLOWUP_SECONDS) if wait_for_speech else 1.2
                if waited > limit:
                    # Wake phrase alone ("Hey Mirrin" ... nothing): still hand it over if we have pre-roll.
                    return "bare" if len(first_chunks) > 0 and _write(frames) else ""
            continue
        frames.append(chunk)
        if talking:
            silence_run = 0.0
            talking_frames += 1
        else:
            silence_run += CHUNK / RATE
            # End of utterance. A few words in ("open the…"), a pause is usually
            # someone finding the next word, so it waits longer; once they've
            # said a sentence's worth, a shorter pause ends it and feels quick.
            said = talking_frames * CHUNK / RATE
            if silence_run >= (1.3 if said < 1.5 else 0.9):
                break
    if not speech_seen or not frames:
        return ""
    if not first_chunks and talking_frames * CHUNK / RATE < 0.3:
        # a click or a breath, not speech: keep the window open for what's left of it
        remaining = FOLLOWUP_SECONDS - (time.time() - t0)
        if wait_for_speech and remaining > 0.5:
            return capture(mic, [], wait_for_speech=True, window=remaining)
        return ""
    return "speech" if _write(frames) else ""


def _write(frames, path=None):
    audio = np.concatenate(frames)
    if len(audio) < RATE // 4:
        return False
    sf.write(path or OUT, audio, RATE)
    return True


def main():
    if not MODEL:
        raise RuntimeError("no wake-word model; run `mirrin voice setup`")
    model = Model(wakeword_models=[MODEL], inference_framework="onnx")
    name = list(model.models.keys())[0]
    source = os.environ.get("WAKE_INPUT")  # a file instead of the microphone, for tests
    # -D: no dither, so a microphone that gives nothing (no permission) reads as
    # zeros rather than as sox's own faint noise.
    sox = subprocess.Popen(
        ["sox", "-q", "-D", source or "-d", "-r", str(RATE), "-c", "1", "-b", "16", "-t", "raw", "-"],
        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, bufsize=0)
    mic = Mic(sox.stdout, CHUNK * 2)
    threading.Thread(target=stdin_loop, daemon=True).start()
    log("ready")
    last_fire = 0.0
    last_near = 0.0
    speak_base = None   # mic level of our own playback, learned while speaking
    speak_chunks = 0    # chunks since speaking began (playback reaches the mic after a beat)
    loud_run = 0        # consecutive chunks well above that level
    ambient = 0.003     # room noise while idle, so thresholds follow the microphone's gain
    zero_run = 0        # consecutive chunks of pure digital silence
    muted = False
    recent = deque(maxlen=20)  # ~1.6 s of audio: the wake phrase itself, so whisper can confirm it
    while True:
        buf = mic.read()
        if not buf:
            log("err microphone stream ended")
            return
        chunk = np.frombuffer(buf, dtype=np.int16)
        # A real microphone always hears a little noise; (next to) exact zeros for this
        # long mean it's blocked (no permission) or the input device is gone.
        if int(np.abs(chunk.astype(np.int32)).max()) <= 1:
            zero_run += 1
            if not muted and zero_run * CHUNK / RATE >= MUTE_SECONDS:
                muted = True
                log("mute")
        else:
            zero_run = 0
            if muted:
                muted = False
                log("unmute")
        recent.append(chunk)
        if state["listen"]:
            state["listen"] = False
            model.reset()
            dropped = mic.drain()
            t0 = time.time()
            ok = capture(mic, [], wait_for_speech=True)
            sys.stderr.write(f"follow-up window: {'captured' if ok else 'nothing'} after {time.time()-t0:.1f}s (dropped {dropped} bytes, speaking={state['speaking']})\n"); sys.stderr.flush()
            log(f"utterance {OUT} (followup {time.time()-t0:.1f}s)" if ok else "silence - (followup)")
            recent.clear()
            continue
        # Talking over it: while we speak, the mic hears our own playback at a fairly steady
        # level; a person speaking over that is markedly louder. Treat sustained loudness as
        # an interruption and capture it as the reply (the host confirms it's real speech).
        level = rms(chunk)
        if state["speaking"]:
            speak_chunks += 1
            # Baseline = the PEAK level of our own playback at the mic, decaying slowly, so a
            # pause between our words doesn't make the next word look like an interruption.
            if speak_base is None or speak_chunks <= 8:
                # first ~0.6 s: playback is arriving; learn its peak
                speak_base = level if speak_base is None else max(speak_base, level)
                loud_run = 0
            elif TALKOVER_RATIO > 0 and level > max(speak_base * TALKOVER_RATIO, own["peak"] * TALKOVER_RATIO, ambient * 5, 0.008):
                # candidate interruption: do NOT fold it into the baseline
                loud_run += 1
                own["candidate"] = level if loud_run == 1 else max(own["candidate"], level)
                if loud_run in (1, 5):
                    sys.stderr.write(f"talk-over? level {level:.4f} peak {speak_base:.4f} own {own['peak']:.4f} run {loud_run}\n"); sys.stderr.flush()
            else:
                speak_base = max(level, speak_base * 0.985)  # our own level this reply, decaying slowly
                own["peak"] = max(level, own["peak"] * 0.9995)  # our own level over time (all replies)
                loud_run = 0
            if loud_run >= 5 and time.time() - last_fire > COOLDOWN:
                last_fire = time.time()
                loud_run = 0
                log("interrupt")
                model.reset()
                t0 = time.time()
                ok = capture(mic, list(recent)[-8:], wait_for_speech=False)
                log(f"utterance {OUT} (followup {time.time()-t0:.1f}s)" if ok else "silence - (followup)")
                recent.clear()
                continue
        else:
            speak_base, loud_run, speak_chunks = None, 0, 0
            if level < ambient * 3:  # quiet chunk: refine the ambient estimate
                ambient = 0.97 * ambient + 0.03 * level
        score = model.predict(chunk)[name]
        threshold = THRESHOLD_SPEAKING if state["speaking"] else THRESHOLD
        if 0.08 <= score < threshold and time.time() - last_near > 1.0:
            last_near = time.time()
            log(f"near {score:.2f}")  # a wake that didn't quite make it; visible in the log for tuning
        if score >= threshold and not state["paused"] and time.time() - last_fire > COOLDOWN:
            last_fire = time.time()
            if state["listen"]:
                # The user is answering inside a follow-up window; their words just happened
                # to score on the wake model. Capture it as the follow-up, not as a wake.
                state["listen"] = False
                model.reset()
                t0 = time.time()
                ok = capture(mic, list(recent), wait_for_speech=False)
                log(f"utterance {OUT} (followup {time.time()-t0:.1f}s)" if ok else "silence - (followup)")
                recent.clear()
                continue
            log(f"wake {score:.2f}")
            # The wake phrase alone, so the host can confirm it while the user is still talking.
            if _write(list(recent), CLIP):
                log(f"wakeclip {CLIP}")
            model.reset()
            # Include the pre-roll so the transcript starts with the wake phrase; the host verifies it.
            t0 = time.time()
            ok = capture(mic, list(recent), wait_for_speech=False)
            bare = " bare" if ok == "bare" else ""
            log(f"utterance {OUT} (wake {time.time()-t0:.1f}s{bare})" if ok else "silence - (wake)")
            recent.clear()


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        pass
    except Exception as e:  # noqa: BLE001
        log("err " + str(e).replace("\n", " "))
