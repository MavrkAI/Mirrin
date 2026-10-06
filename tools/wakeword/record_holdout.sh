#!/bin/sh
# Record your own voice for the final, real-voice held-out test of the wake models.
#
#   tools/wakeword/record_holdout.sh [OUTDIR]        (default ~/mirrin-wakeword-clean/holdout)
#
# For each phrase you are prompted, press Enter, say it once, and recording stops by
# itself after 3 seconds. Vary distance (close, across the room), loudness and speed;
# a few takes with the TV or music on help. Then record a few minutes of ordinary
# talk with no wake phrase in it, for false accepts. Recordings stay on this computer
# and are never used for training. Score them with:
#
#   ~/mirrin-wakeword-clean/venv/bin/python tools/wakeword/pipeline/evaluate_holdout.py OUTDIR
#
# Needs sox (`rec`), which `mirrin voice setup` already uses.
set -eu
OUT=${1:-$HOME/mirrin-wakeword-clean/holdout}
TAKES=${TAKES:-5}
command -v rec >/dev/null || { echo "Install sox first (brew install sox)."; exit 1; }
mkdir -p "$OUT"
n=0

take() { # take <dir> <prompt> <seconds>
  dir="$OUT/$1"; mkdir -p "$dir"
  i=$(ls "$dir" 2>/dev/null | wc -l | tr -d ' ')
  printf '\n  Say: "%s"   [Enter to record, s to skip] ' "$2"
  read -r ans </dev/tty
  [ "$ans" = "s" ] && return 0
  rec -q -r 16000 -c 1 -b 16 "$dir/$(printf %03d "$i").wav" trim 0 "$3"
  n=$((n + 1))
}

echo "Positives: say each phrase the way you naturally would."
for name in Mirrin Nyra Pickoo; do
  model=hey_$(echo "$name" | tr 'A-Z' 'a-z')
  t=1
  while [ "$t" -le "$TAKES" ]; do
    take "$model/positive" "Hey $name" 3
    t=$((t + 1))
  done
  take "$model/positive" "Hey $name, what's the weather like?" 4
  take "$model/positive" "Hey $name, remind me to call mum." 4
  take "$model/positive" "(from across the room) Hey $name" 3
  take "$model/positive" "(quickly, quietly) hey $name" 3
done

echo
echo "Near-misses: these must NOT wake anything."
for p in "hey mirror" "hey Miriam" "hey Myron" "pass the mirin please" "add mirin to the list" \
         "hey Merlin" "hey Marion" "hey Erin" "hey Karen" "hey Darren" \
         "I told Mirrin about it" "Mirrin is busy right now" \
         "hey Myra" "hey Kyra" "hey Tyra" "hey Ira" "hey Nina" "Nairobi" "I told Nyra about it" \
         "I'll pick you up" "pick up the phone" "pikachu" "peekaboo" "hey picky" "the piccolo" \
         "hey Vicky" "I told Pickoo about it" \
         "hey Siri" "hey everyone" "hey there"; do
  take near_miss "$p" 3
done

echo
printf '\nNow talk normally for 3 minutes (read something aloud, chat, or leave the TV on).\n'
printf 'Do not say any wake phrase. [Enter to start] '
read -r _ </dev/tty
mkdir -p "$OUT/negative_long"
rec -q -r 16000 -c 1 -b 16 "$OUT/negative_long/$(date +%Y%m%d-%H%M%S).wav" trim 0 180
n=$((n + 1))
echo
echo "Recorded $n files in $OUT. Score them with tools/wakeword/pipeline/evaluate_holdout.py."
