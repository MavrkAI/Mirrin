#!/bin/sh
# restore-drill.sh [--production] [--config /etc/litestream.yml]
#
# The quarterly Litestream restore drill, and the real restore
# (cloud/ops/RUNBOOK.md, "Litestream restore drill").
#
# Drill (the default): restores the latest replica from R2 into a scratch
# directory, checks it (integrity, schema, row counts next to the live
# database's), starts a throwaway mirrin-cloud --dev on loopback against
# the copy to prove it serves, and prints how long it all took. It changes
# nothing in production and reads the live database read-only.
#
# --production, on a fresh VM after `install.sh --no-start` has put
# everything in place, before mirrin-cloud or Litestream has ever started:
# restores the replica to /var/lib/mirrin-cloud/cloud.db and starts both.
# It refuses to overwrite an existing database.
#
# Target: under 15 minutes from a fresh VM to serving. Installed as
# /usr/local/sbin/mirrin-restore-drill. Run as root.
set -eu

mode=drill
config=/etc/litestream.yml
while [ $# -gt 0 ]; do
	case $1 in
	--production) mode=production ;;
	--config) config=$2; shift ;;
	*) echo "usage: restore-drill.sh [--production] [--config FILE]" >&2; exit 2 ;;
	esac
	shift
done
db=/var/lib/mirrin-cloud/cloud.db
start=$(date +%s)
say() { printf '[%4ss] %s\n' "$(($(date +%s) - start))" "$*"; }

# Litestream reads the R2 token from the environment, as the service does.
export_creds() {
	LITESTREAM_ACCESS_KEY_ID=$(systemd-creds decrypt --name=LITESTREAM_ACCESS_KEY_ID /etc/credstore.encrypted/LITESTREAM_ACCESS_KEY_ID -)
	LITESTREAM_SECRET_ACCESS_KEY=$(systemd-creds decrypt --name=LITESTREAM_SECRET_ACCESS_KEY /etc/credstore.encrypted/LITESTREAM_SECRET_ACCESS_KEY -)
	export LITESTREAM_ACCESS_KEY_ID LITESTREAM_SECRET_ACCESS_KEY
}

check_db() {
	ok=$(sqlite3 "file:$1?mode=ro" 'PRAGMA integrity_check;')
	[ "$ok" = ok ] || { echo "restore-drill: integrity_check says: $ok" >&2; exit 1; }
	for t in accounts devices handles links deny events; do
		sqlite3 "file:$1?mode=ro" "SELECT 1 FROM $t LIMIT 1;" >/dev/null ||
			{ echo "restore-drill: table $t is missing" >&2; exit 1; }
	done
}

counts() {
	sqlite3 "file:$1?mode=ro" \
		"SELECT 'accounts', count(*) FROM accounts UNION ALL
		 SELECT 'handles', count(*) FROM handles UNION ALL
		 SELECT 'devices', count(*) FROM devices UNION ALL
		 SELECT 'deny', count(*) FROM deny;"
}

export_creds
: "${LITESTREAM_ACCESS_KEY_ID:?}" "${LITESTREAM_SECRET_ACCESS_KEY:?}"

if [ "$mode" = production ]; then
	[ ! -e "$db" ] || { echo "restore-drill: $db exists; this is not a fresh VM. Move it aside first." >&2; exit 1; }
	systemctl stop mirrin-cloud.service litestream.service 2>/dev/null || true
	say "restoring the latest replica into $db"
	install -d -o mirrin-cloud -g mirrin-cloud -m 0750 /var/lib/mirrin-cloud
	litestream restore -config "$config" -o "$db" "$db"
	chown mirrin-cloud:mirrin-cloud "$db"
	check_db "$db"
	counts "$db"
	say "starting Litestream (replicating on from here) and mirrin-cloud"
	systemctl start litestream.service mirrin-cloud.service
	for _ in $(seq 1 30); do
		if curl -fsS http://127.0.0.1:8787/v1/version >/dev/null 2>&1; then
			say "serving. Now: DNS for the control plane's name (RUNBOOK.md), then mirrin-canary@* and caddy."
			exit 0
		fi
		sleep 1
	done
	echo "restore-drill: mirrin-cloud isn't answering; journalctl -u mirrin-cloud" >&2
	exit 1
fi

scratch=$(mktemp -d /var/tmp/mirrin-restore-drill.XXXXXX)
trap 'rm -rf "$scratch"; [ -n "${pid:-}" ] && kill "$pid" 2>/dev/null || true' EXIT
say "restoring the latest replica into $scratch"
litestream restore -config "$config" -o "$scratch/cloud.db" "$db"
check_db "$scratch/cloud.db"
say "restored copy:"
counts "$scratch/cloud.db"
if [ -e "$db" ]; then
	say "live database (read-only), for comparison; a few rows' difference is the last seconds of writes:"
	counts "$db"
fi

say "serving the copy with mirrin-cloud --dev on loopback"
port=$(( 20000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 20000 ))
/usr/local/bin/mirrin-cloud serve --dev --listen "127.0.0.1:$port" --data "$scratch" >"$scratch/serve.log" 2>&1 &
pid=$!
served=no
for _ in $(seq 1 30); do
	if curl -fsS "http://127.0.0.1:$port/v1/version" >/dev/null 2>&1; then
		served=yes
		break
	fi
	sleep 1
done
[ "$served" = yes ] || { echo "restore-drill: the copy didn't serve:" >&2; cat "$scratch/serve.log" >&2; exit 1; }
elapsed=$(($(date +%s) - start))
say "the restored copy serves. Drill took ${elapsed}s (target: under 900s on a fresh VM)."
echo "Record it in cloud/ops/RUNBOOK.md's drill log: date, ${elapsed}s, the counts above."
[ "$elapsed" -lt 900 ]
