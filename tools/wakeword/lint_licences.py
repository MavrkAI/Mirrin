"""Offline provenance gate for the planned WP-24 pipeline (standard library only)."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import sys


HERE = Path(__file__).resolve().parent
ALLOWED = {"MIT", "Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "CC0-1.0", "CC-BY-4.0"}
KINDS = {"dataset", "voice", "tts-model", "feature-model", "augmentation"}


def inventory(document, *, allow_empty=False):
    blocks = re.findall(r"^```json\s*\n(.*?)^```\s*$", document, re.M | re.S)
    if len(blocks) != 1:
        raise ValueError("Keep exactly one JSON inventory in DATA-LICENCES.md.")
    entries = json.loads(blocks[0])
    if not isinstance(entries, list):
        raise ValueError("The data inventory must be a list.")
    if not entries and not allow_empty:
        raise ValueError("No training inputs are approved yet. Add reviewed licence records first.")
    seen = set()
    for entry in entries:
        required = {"id", "kind", "licence", "source", "licence_source", "sha256", "path", "attribution"}
        if not isinstance(entry, dict) or set(entry) != required:
            raise ValueError("Each input needs all eight inventory fields; see README.md.")
        if any(not isinstance(v, str) or not v.strip() for v in entry.values()):
            raise ValueError("Fill in every inventory field before training.")
        if not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", entry["id"]) or entry["id"] in seen:
            raise ValueError("Give every input a unique, lower-case id.")
        seen.add(entry["id"])
        if entry["licence"] not in ALLOWED or entry["kind"] not in KINDS:
            raise ValueError(f"Review the licence and kind for {entry['id']}.")
        if not re.fullmatch(r"[0-9a-f]{64}", entry["sha256"]):
            raise ValueError(f"Record the SHA-256 for {entry['id']}.")
        for field in ("source", "licence_source"):
            if not re.fullmatch(r"https://[^/\s]+/\S+", entry[field]):
                raise ValueError(f"Record an HTTPS source for {entry['id']}.")
        path = entry["path"]
        if path.startswith("/") or "\\" in path or any(p in ("", ".", "..") for p in path.split("/")):
            raise ValueError("Keep input paths relative to the data directory.")
    if len({e["path"] for e in entries}) != len(entries):
        raise ValueError("Use a separate path for each inventory entry.")
    return entries


def validate(document, recipe, data=None, *, allow_empty_planned=False):
    if not isinstance(recipe, dict):
        raise ValueError("Use a recipe object with an inputs list.")
    entries = inventory(document, allow_empty=(
        allow_empty_planned and recipe.get("status") == "planned"
    ))
    ids = [e["id"] for e in entries]
    inputs = recipe.get("inputs")
    if not isinstance(inputs, list) or any(not isinstance(i, str) for i in inputs):
        raise ValueError("List the recipe's input ids.")
    if sorted(inputs) != sorted(ids):
        raise ValueError("The recipe and licence inventory must list exactly the same inputs.")
    if data is not None:
        root = Path(data).resolve(strict=True)
        for entry in entries:
            path = (root / entry["path"]).resolve(strict=True)
            if not path.is_relative_to(root) or not path.is_file():
                raise ValueError("Keep every input file inside the data directory.")
            digest = hashlib.sha256()
            with path.open("rb") as stream:
                for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                    digest.update(chunk)
            if digest.hexdigest() != entry["sha256"]:
                raise ValueError(f"The input {entry['id']} changed. Restore the reviewed file.")
    return entries


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--licences", type=Path, default=HERE / "DATA-LICENCES.md")
    parser.add_argument("--recipe", type=Path, default=HERE / "recipe.yaml")
    parser.add_argument("--data", type=Path, help="also verify local input checksums")
    parser.add_argument("--allow-empty-planned", action="store_true",
                        help="allow an empty inventory only for a planned recipe (CI scaffolding)")
    args = parser.parse_args()
    try:
        entries = validate(args.licences.read_text(), json.loads(args.recipe.read_text()), args.data,
                           allow_empty_planned=args.allow_empty_planned)
    except (ValueError, OSError, TypeError) as exc:
        print(f"Wake data check: {exc}", file=sys.stderr)
        return 1
    if not entries:
        print("Notice: planned recipe has no approved inputs. Training and release remain blocked.")
    else:
        print(f"Checked {len(entries)} licence records. Human provenance review is still required.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
