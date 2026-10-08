#!/bin/sh
# Hosted twin: one container per owner. State lives under $MIRRIN_HOME
# (a persistent volume). First boot writes a headless config from env.
set -eu
: "${MIRRIN_HOME:=/data}"
export MIRRIN_HOME
mkdir -p "$MIRRIN_HOME/data" "$MIRRIN_HOME/protocols"
CFG="$MIRRIN_HOME/config.yaml"

BIN="${MIRRIN_BIN:-/usr/local/bin/mirrin}"

# The gateway gets a device key of its own, which the owner can revoke on
# its own (`mirrin devices revoke`), instead of the twin's master key. The
# twin's master key (data/api.token) is the daemon's alone.
if [ -n "${TWIN_API_TOKEN:-}" ]; then
  echo "entrypoint: TWIN_API_TOKEN is no longer used; the gateway's own key is in ${TWIN_GATEWAY_TOKEN_FILE:-$MIRRIN_HOME/gateway/token}" >&2
fi
GATEWAY_TOKEN="${TWIN_GATEWAY_TOKEN_FILE:-$MIRRIN_HOME/gateway/token}"
GATEWAY_DIR=$(dirname "$GATEWAY_TOKEN")
REVOKED="$GATEWAY_DIR/revoked"
BASE="http://127.0.0.1:${MIRRIN_PORT:-7742}"

# pair_gateway waits for the twin, then makes sure the gateway has a key:
# it keeps one that works, removes one that was revoked (and pairs again
# only when told to, with TWIN_REPAIR_GATEWAY=1), and pairs when there is
# none. It leaves one word in $GATEWAY_DIR/status: kept, paired, revoked or
# failed.
gw_status() { printf '%s\n' "$1" >"$GATEWAY_DIR/status"; }
pair_gateway() {
  [ "${TWIN_GATEWAY:-1}" = 0 ] && return 0
  n=0
  until wget -q -O /dev/null -T 2 "$BASE/healthz" 2>/dev/null; do
    n=$((n + 1))
    if [ "$n" -ge "${TWIN_START_WAIT:-120}" ]; then
      echo "entrypoint: the twin didn't answer on $BASE, so the gateway wasn't paired" >&2
      gw_status failed
      return 1
    fi
    sleep 1
  done
  if [ -s "$GATEWAY_TOKEN" ]; then
    rc=0
    resp=$(wget -S -O /dev/null -T 4 --header "Authorization: Bearer $(tr -d '[:space:]' <"$GATEWAY_TOKEN")" "$BASE/status" 2>&1) || rc=$?
    # Only a 401 means the key was refused. wget exits 8 on any HTTP error,
    # and a 500 or 503 (the twin busy, or can't save) says nothing about it.
    if [ "$rc" != 0 ] && printf '%s\n' "$resp" | grep -q '^ *HTTP/[0-9.]* 401'; then
      rc=revoked
    fi
    case $rc in
    0)
      gw_status kept
      return 0
      ;;
    revoked) # the twin answered, and refused it
      rm -f "$GATEWAY_TOKEN"
      : >"$REVOKED"
      echo "entrypoint: the gateway's key was revoked, so it was removed. To pair the gateway again, restart with TWIN_REPAIR_GATEWAY=1." >&2
      if [ "${TWIN_REPAIR_GATEWAY:-}" != 1 ]; then
        gw_status revoked
        return 0
      fi
      ;;
    *) # couldn't tell; keep it
      gw_status kept
      return 0
      ;;
    esac
  fi
  if [ -f "$REVOKED" ] && [ "${TWIN_REPAIR_GATEWAY:-}" != 1 ]; then
    gw_status revoked
    return 0
  fi
  out=$("$BIN" pair --scopes "${TWIN_GATEWAY_SCOPES:-view,chat,approve}" --host "$BASE" 2>&1) || {
    echo "entrypoint: couldn't make a pairing code for the gateway: $out" >&2
    gw_status failed
    return 1
  }
  # `pair` prints the line to run elsewhere: `mirrin connect <code>`.
  code=$(printf '%s\n' "$out" | sed -n -E 's/^ *mirrin connect ([^ ]*).*$/\1/p' | head -n 1)
  if [ -z "$code" ]; then
    echo "entrypoint: couldn't make a pairing code for the gateway: $out" >&2
    gw_status failed
    return 1
  fi
  # The gateway's own home, so its key isn't saved in the twin's home,
  # which would make the twin a client of itself.
  tmp=$(mktemp -d)
  if ! MIRRIN_HOME="$tmp" "$BIN" connect --name gateway "$code" >/dev/null 2>&1; then # rename:keep
    rm -rf "$tmp"
    echo "entrypoint: the gateway couldn't pair with the twin" >&2
    gw_status failed
    return 1
  fi
  tok=$(sed -n 's/^token: *//p' "$tmp/remote.yaml" | tr -d "\"' ")
  rm -rf "$tmp"
  if [ -z "$tok" ]; then
    gw_status failed
    return 1
  fi
  (umask 077 && printf '%s' "$tok" >"$GATEWAY_TOKEN.new") && mv "$GATEWAY_TOKEN.new" "$GATEWAY_TOKEN"
  rm -f "$REVOKED"
  echo "entrypoint: the gateway has a key of its own in $GATEWAY_TOKEN" >&2
  gw_status paired
}

# q prints a value as a YAML double-quoted string, so a colon, '#', quote or
# line break in a name or bio stays part of the text.
q() {
  printf '%s' "$1" | tr -d '\r' | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' |
    awk 'BEGIN { printf "\"" } { printf "%s%s", (NR > 1 ? "\\n" : ""), $0 } END { printf "\"" }'
}

if [ ! -f "$CFG" ]; then
  cat >"$CFG" <<YAML
name: $(q "${TWIN_NAME:-Mirrin}")
persona: $(q "${TWIN_PERSONA:-mirrin}")
user:
  name: $(q "${OWNER_NAME:-Owner}")
  honorific: $(q "${OWNER_HONORIFIC:-}")
  timezone: $(q "${OWNER_TIMEZONE:-UTC}")
  about: $(q "${OWNER_ABOUT:-}")
llm:
  provider: $(q "${LLM_PROVIDER:-openai}")
  model: $(q "${LLM_MODEL:-gpt-4.1}")
  providers:
    anthropic: {api_key_env: ANTHROPIC_API_KEY, model: claude-sonnet-5}
    openai:    {api_key_env: OPENAI_API_KEY, model: $(q "${LLM_MODEL:-gpt-4.1}")}
  max_tokens: 8000
  effort: medium
  history_turns: 40
channels:
  whatsapp: { enabled: false }
  cli: { enabled: false }
  voice: { enabled: false }
autonomy:
  read: auto
  write: ask
  dangerous: ask
skills:
  reminders: { enabled: true }
  web: { enabled: true }
  system: { enabled: false, allow_shell: false }
  browser: { enabled: false, headless: true }
  calendar: { enabled: false }
  email: { enabled: false }
api:
  listen: 0.0.0.0:7742
  remote: true
protocols_dir: $(q "$MIRRIN_HOME/protocols")
data_dir: $(q "$MIRRIN_HOME/data")
YAML
fi

if [ "${TWIN_GATEWAY:-1}" != 0 ]; then
  (umask 077 && mkdir -p "$GATEWAY_DIR")
  rm -f "$GATEWAY_DIR/status"
fi
"$BIN" run &
twin=$!
trap 'kill -TERM "$twin" 2>/dev/null' TERM INT HUP
pair_gateway &
gw=$!
status=0
wait "$twin" || status=$?
# A signal ends the first wait early; this one collects the twin's own exit.
if kill -0 "$twin" 2>/dev/null; then
  status=0
  wait "$twin" || status=$?
fi
kill "$gw" 2>/dev/null || true
exit "$status"
