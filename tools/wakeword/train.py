"""Preflight and driver for the WP-24 wake-word pipeline.

  train.py --check --data DIR [--local]    provenance and checksum preflight only
  train.py --data DIR --local [--from STAGE] [--only STAGE] [--jobs N] [--dry-run]

Preflight verifies every inventoried input (DATA-LICENCES.md) against its recorded
SHA-256 before anything runs. The container route needs a training image pinned by
digest in recipe.yaml; `--local` instead uses a virtualenv built from
pipeline/requirements.lock (hash-locked). Nothing is downloaded by any stage:
fetching reviewed inputs is a separate, explicit step (pipeline/fetch_inputs.sh).
Stages are idempotent and write to WAKE_WORK and WAKE_OUT; long ones take hours on
a laptop CPU, so run this under nohup and follow the log.
"""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys

from lint_licences import HERE, validate

PIPE = HERE / "pipeline"


def stages(recipe, data, jobs):
    n = str(jobs)
    out = [
        ("voices", ["prepare_data.py", "voices"]),
        ("extract", ["prepare_data.py", "extract"]),
        ("split", ["prepare_data.py", "split"]),
        ("verify-frontend", ["verify_frontend.py", str(data)]),
        ("synth-plan", ["synth.py", "plan"]),
        ("synth", ["synth.py", "run", n]),
        ("clip-features", ["features.py", "clips", n]),
        ("stream-train", ["features.py", "stream", "train", n]),
        ("stream-cal", ["features.py", "stream", "cal", n]),
    ]
    for m in recipe.get("models", []):
        out.append((f"train-{m['name']}", ["train_model.py", m["name"], "--seed", str(recipe.get("seed", 7)),
                                                *[str(x) for x in m.get("train_args", [])]]))
    out.append(("evaluate", ["evaluate.py", n] + [m["name"] for m in recipe.get("models", [])]))
    return out


def preflight(recipe, document, data, *, local=False):
    if not isinstance(recipe, dict) or recipe.get("schema_version") != 1:
        raise ValueError("Use recipe schema version 1.")
    image = recipe.get("image")
    if not local and (not isinstance(image, str) or not re.fullmatch(r"[^\s@]+@sha256:[0-9a-f]{64}", image)):
        raise ValueError("Record a verified training image digest in recipe.yaml first "
                         "(or use --local with the hash-locked virtualenv).")
    return validate(document, recipe, data)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--recipe", type=Path, default=HERE / "recipe.yaml")
    parser.add_argument("--licences", type=Path, default=HERE / "DATA-LICENCES.md")
    parser.add_argument("--data", type=Path, required=True)
    parser.add_argument("--check", action="store_true", help="only check provenance and local checksums")
    parser.add_argument("--local", action="store_true", help="run in this virtualenv instead of the pinned image")
    parser.add_argument("--from", dest="start", help="resume at this stage")
    parser.add_argument("--only", help="run just this stage")
    parser.add_argument("--jobs", type=int, default=max(1, (os.cpu_count() or 2) - 2))
    parser.add_argument("--dry-run", action="store_true", help="print the stages without running them")
    args = parser.parse_args()
    try:
        recipe = json.loads(args.recipe.read_text())
        preflight(recipe, args.licences.read_text(), args.data, local=args.local)
    except (ValueError, OSError, TypeError) as exc:
        print(f"Wake training: {exc}", file=sys.stderr)
        return 1
    if args.check:
        print("Input checks passed. Training and model evaluation still need to run.")
        return 0
    plan = stages(recipe, args.data.resolve(), args.jobs)
    names = [s for s, _ in plan]
    for want in (args.start, args.only):
        if want and want not in names:
            print(f"Wake training: unknown stage {want}; stages are {', '.join(names)}", file=sys.stderr)
            return 1
    if args.only:
        plan = [p for p in plan if p[0] == args.only]
    elif args.start:
        plan = plan[names.index(args.start):]
    env = dict(os.environ, WAKE_DATA=str(args.data.resolve()), PYTHONDONTWRITEBYTECODE="1")
    for name, cmd in plan:
        line = [sys.executable, "-B", str(PIPE / cmd[0]), *cmd[1:]]
        print(f"== {name}: {' '.join(line[2:])}", flush=True)
        if args.dry_run:
            continue
        if subprocess.run(line, cwd=PIPE, env=env).returncode != 0:
            print(f"Wake training: stage {name} failed; fix it and rerun with --from {name}", file=sys.stderr)
            return 1
    print("Pipeline finished." if not args.dry_run else "Dry run: nothing was run.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
