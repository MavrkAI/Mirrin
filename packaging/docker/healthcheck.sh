#!/bin/sh
# Docker HEALTHCHECK: healthy when the twin answers /healthz, the one route
# that needs no key (so the check never holds or reads one).
set -eu
exec wget -q -O /dev/null -T 4 "http://127.0.0.1:${MIRRIN_PORT:-${ANTBOT_PORT:-7742}}/healthz" # rename:keep
