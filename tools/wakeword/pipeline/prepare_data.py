"""Unpack the reviewed archives and write the split manifest (before any synthesis,
augmentation or feature extraction).

  prepare_data.py voices    per-voice Kokoro style files from voices-v1.0.bin
  prepare_data.py extract   unpack LibriSpeech, MUSAN and the simulated RIRs
  prepare_data.py split     write WORK/split.json (speaker/recording-level splits)

Split policy (README "Evaluation protocol"):
  * LibriSpeech: train-clean-100 trains; dev-clean + dev-other calibrate the threshold;
    test-clean + test-other are the final held-out negatives. These subsets have
    disjoint speakers by construction (and are disjoint from LibriTTS train-clean-360,
    which the Piper LibriTTS voice was trained on).
  * MUSAN: only files whose own licence (per the corpus's LICENSE/ANNOTATIONS files) is
    public domain, CC0 or CC BY are used. Speech is split by speaker, music by artist,
    noise by file, with ~25% held out (half calibration, half test).
  * Simulated RIRs: split by room (small/medium/large room sets, 200 rooms each);
    room ids ending 0-6 train, 7 calibrate, 8-9 test.
"""
import hashlib
import json
import random
import re
import sys
import tarfile
import zipfile
from collections import defaultdict
from pathlib import Path

import numpy as np

from common import DATA, KOKORO_ALL, WORK

RAW = DATA / "raw"
X = DATA / "extracted"


def voices():
    pack = np.load(DATA / "tts/kokoro/voices-v1.0.bin")
    out = DATA / "tts/kokoro/voices"
    out.mkdir(parents=True, exist_ok=True)
    for v in KOKORO_ALL:
        np.save(out / f"{v}.npy", pack[v])
        print(v, hashlib.sha256((out / f"{v}.npy").read_bytes()).hexdigest())


def extract():
    X.mkdir(parents=True, exist_ok=True)
    for name in ["train-clean-100", "dev-clean", "dev-other", "test-clean", "test-other"]:
        if not (X / "LibriSpeech" / name).exists():
            with tarfile.open(RAW / f"{name}.tar.gz") as t:
                t.extractall(X, filter="data")
            print("extracted", name, flush=True)
    if not (X / "musan").exists():
        with tarfile.open(RAW / "musan.tar.gz") as t:
            t.extractall(X, filter="data")
        print("extracted musan", flush=True)
    if not (X / "RIRS_NOISES/simulated_rirs").exists():
        with zipfile.ZipFile(RAW / "rirs_noises.zip") as z:
            z.extractall(X, [n for n in z.namelist() if n.startswith("RIRS_NOISES/simulated_rirs/")])
        print("extracted simulated RIRs", flush=True)


LICENCE_LINE = re.compile(r"(public domain|cc0|creative commons|attribution|cc[- ]by|licen[cs]e[:d])", re.I)
RESTRICTED = re.compile(r"(share[- ]?alike|cc[- ]by[- ](nc|sa|nd)|non[- ]?commercial|no[- ]?deriv|"
                        r"attribution[- ](nc|sa|nd|non|share|no))", re.I)
PERMISSIVE = re.compile(r"(public domain|cc0|creative commons zero|attribution|cc[- ]by)", re.I)


def licence_ok(text):
    """Public domain, CC0 or CC BY (any version) only; anything with SA/NC/ND is out."""
    return bool(text) and bool(PERMISSIVE.search(text)) and not RESTRICTED.search(text)


def parse_licence_file(path):
    """MUSAN LICENSE files: either one licence for the whole directory (speech, free-sound),
    or blocks separated by '=====' lines, each listing file ids and a licence line. A block
    with ids but no licence line takes the licence of the id-less block before it (rfm puts
    one licence on top). Returns (directory licence or '', {file id: licence line})."""
    text = path.read_text(errors="ignore")
    blocks = re.split(r"^=+\s*$", text, flags=re.M)
    ids_seen = re.findall(r"^\s*((?:music|noise|speech)-[a-z-]+-\d+)\s*$", text, flags=re.M)
    if not ids_seen:
        return " ".join(text.split()), {}
    per_file, header = {}, ""
    for blk in blocks:
        ids = re.findall(r"^\s*((?:music|noise|speech)-[a-z-]+-\d+)\s*$", blk, flags=re.M)
        lic = [l.strip() for l in blk.splitlines()
               if LICENCE_LINE.search(l) and not l.strip().startswith("http") and not re.match(r"^\s*(music|noise|speech)-", l)]
        lic = " | ".join(lic)
        if ids:
            for i in ids:
                per_file[i] = lic or header
        elif lic:
            header = lic
    return "", per_file


def musan_licences():
    """Map every MUSAN WAV to (category, group, licence text). Licences come from the
    corpus's own per-directory LICENSE files; groups from ANNOTATIONS (music: artist).
    MUSAN gives no speaker ids for speech, so speech and noise are grouped by file."""
    root = X / "musan"
    rows = {}
    for lic_file in sorted(root.glob("*/*/LICENSE")):
        d = lic_file.parent
        cat = d.relative_to(root).parts[0]
        whole, per_file = parse_licence_file(lic_file)
        artist = {}
        if (d / "ANNOTATIONS").exists():
            for line in (d / "ANNOTATIONS").read_text(errors="ignore").splitlines():
                parts = line.split()
                if cat == "music" and len(parts) > 3:
                    artist[parts[0]] = parts[3]
        for wav in sorted(d.glob("*.wav")):
            fid = wav.stem
            group = artist.get(fid, fid) if cat == "music" else fid
            rows[str(wav.relative_to(root))] = (cat, group, whole or per_file.get(fid, ""))
    return rows


def split_key(name, n_holdout=8):
    """Stable bucket 0-31 from a group name."""
    return int(hashlib.sha256(name.encode()).hexdigest(), 16) % 32


def split():
    random.seed(7)
    out = {"librispeech": {}, "musan": {"train": [], "cal": [], "test": [], "excluded": []}, "rirs": {}}
    ls = X / "LibriSpeech"
    for part, role in [("train-clean-100", "train"), ("dev-clean", "cal"), ("dev-other", "cal"),
                       ("test-clean", "test"), ("test-other", "test")]:
        files = sorted(str(p.relative_to(X)) for p in (ls / part).rglob("*.flac"))
        out["librispeech"].setdefault(role, []).extend(files)
        out["librispeech"].setdefault(role + "_speakers", []).extend(
            sorted({p.name for p in (ls / part).iterdir() if p.is_dir()}))
    spk = {r: set(out["librispeech"][r + "_speakers"]) for r in ("train", "cal", "test")}
    assert not (spk["train"] & spk["cal"]) and not (spk["train"] & spk["test"]) and not (spk["cal"] & spk["test"])

    for rel, (cat, group, licence) in sorted(musan_licences().items()):
        entry = {"path": "musan/" + rel, "category": cat, "group": f"{cat}:{group}", "licence": licence[:160]}
        if not licence_ok(licence):
            out["musan"]["excluded"].append(entry)
            continue
        b = split_key(entry["group"])
        role = "train" if b < 24 else ("cal" if b < 28 else "test")
        out["musan"][role].append(entry)

    for size in ["smallroom", "mediumroom", "largeroom"]:
        for room in sorted((X / "RIRS_NOISES/simulated_rirs" / size).iterdir()):
            if not room.is_dir():
                continue
            n = int(re.sub(r"\D", "", room.name))
            role = "train" if n % 10 <= 6 else ("cal" if n % 10 == 7 else "test")
            out["rirs"].setdefault(role, []).extend(sorted(str(p.relative_to(X)) for p in room.glob("*.wav")))

    WORK.mkdir(parents=True, exist_ok=True)
    (WORK / "split.json").write_text(json.dumps(out, indent=1))
    summary = {
        "librispeech": {k: len(v) for k, v in out["librispeech"].items()},
        "musan": {k: len(v) for k, v in out["musan"].items()},
        "musan_excluded_licences": sorted({e["licence"] for e in out["musan"]["excluded"]}),
        "rirs": {k: len(v) for k, v in out["rirs"].items()},
    }
    print(json.dumps(summary, indent=1))


if __name__ == "__main__":
    {"voices": voices, "extract": extract, "split": split}[sys.argv[1]]()
