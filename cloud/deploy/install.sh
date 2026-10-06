#!/bin/sh
# install.sh [--no-start]: run as root on the host, from the directory
# render.sh wrote, with the Linux binaries this host needs copied next to
# it:
#   relay: mirrin-relay
#   cp:    mirrin-cloud, mirrin-canary (and the litestream, caddy and
#          sqlite3 packages installed; README.md)
#   probe: mirrin-canary (and caddy)
# It installs the binaries, configs and units, checks the configs, and
# (re)starts the services. Secrets must already be in
# /etc/credstore.encrypted (README.md, "Secrets"); it says which are
# missing and stops. Running it again upgrades in place.
#
# --no-start installs and checks everything but starts and restarts
# nothing: the first step of restoring a lost control plane, which must
# not start mirrin-cloud or Litestream on an empty database
# (cloud/ops/RUNBOOK.md, "Control-plane VM lost").
set -eu
role=@@ROLE@@
start=yes
case ${1:-} in
--no-start) start=no ;;
'') ;;
*) echo "usage: install.sh [--no-start]" >&2; exit 2 ;;
esac
here=$(cd "$(dirname "$0")" && pwd)
cd "$here"

[ "$(id -u)" = 0 ] || { echo "install.sh: run as root" >&2; exit 1; }
command -v systemd-creds >/dev/null || { echo "install.sh: needs systemd 250 or later (systemd-creds)" >&2; exit 1; }

need_creds() {
	missing=""
	for c in "$@"; do
		[ -f "/etc/credstore.encrypted/$c" ] || missing="$missing $c"
	done
	if [ -n "$missing" ]; then
		echo "install.sh: these credentials are missing from /etc/credstore.encrypted:$missing" >&2
		echo "  make each with: systemd-creds encrypt --name=NAME - /etc/credstore.encrypted/NAME" >&2
		exit 1
	fi
}
need_cmd() {
	command -v "$1" >/dev/null || { echo "install.sh: install $1 first" >&2; exit 1; }
}

bin() { install -m 0755 "$here/$1" "/usr/local/bin/$1"; }
lib() { install -D -m 0755 "$here/$1" "/usr/local/lib/mirrin/$2"; }
unit() { install -m 0644 "$here/$1" "/etc/systemd/system/$1"; }
dropin() { install -D -m 0644 "$here/canary-credentials-$1.conf" "/etc/systemd/system/mirrin-canary@$1.service.d/credentials.conf"; }
# svc runs systemctl, unless --no-start.
svc() {
	if [ "$start" = yes ]; then
		systemctl "$@"
	fi
}

lib with-credentials.sh with-credentials

case $role in
relay)
	need_creds SMTP_USERNAME SMTP_PASSWORD
	need_cmd curl
	bin mirrin-relay
	lib relay-notify.sh relay-notify
	install -D -m 0644 relay.yaml /etc/mirrin-relay/relay.yaml
	/usr/local/bin/mirrin-relay check-config --config /etc/mirrin-relay/relay.yaml
	unit mirrin-relay.service
	systemctl daemon-reload
	svc enable mirrin-relay.service
	# A restart drains: daemons move to the other relay first.
	svc restart mirrin-relay.service
	;;
cp)
	need_creds @@ENT_KID@@.pem @@DL_KID@@.pem PADDLE_API_KEY PADDLE_WEBHOOK_SECRET \
		ROUTE53_ACCESS_KEY_ID ROUTE53_SECRET_ACCESS_KEY R2_ACCESS_KEY_ID R2_SECRET_ACCESS_KEY \
		LITESTREAM_ACCESS_KEY_ID LITESTREAM_SECRET_ACCESS_KEY \
		CANARY_PROBE_TOKEN PAGERDUTY_ROUTING_KEY SMTP_USERNAME SMTP_PASSWORD \
		MIRROR_ACCESS_KEY_ID MIRROR_SECRET_ACCESS_KEY
@@?ENT_KID2@@	need_creds @@ENT_KID2@@.pem
@@?DL_KID2@@	need_creds @@DL_KID2@@.pem
	need_cmd litestream
	need_cmd caddy
	need_cmd sqlite3
	need_cmd curl
	getent passwd mirrin-cloud >/dev/null || useradd --system --home-dir /var/lib/mirrin-cloud --shell /usr/sbin/nologin mirrin-cloud
	bin mirrin-cloud
	bin mirrin-canary
	install -D -m 0644 cloud.yaml /etc/mirrin-cloud/cloud.yaml
	install -m 0644 litestream.yml /etc/litestream.yml
	install -D -m 0644 canary.yaml /etc/mirrin-canary/canary.yaml
	install -m 0644 Caddyfile /etc/caddy/Caddyfile
	install -D -m 0755 restore-drill.sh /usr/local/sbin/mirrin-restore-drill
	/usr/local/bin/mirrin-canary check-config --config /etc/mirrin-canary/canary.yaml --role twin,probe,watch,mirror
	caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
	unit mirrin-cloud.service
	unit litestream.service
	unit mirrin-canary@.service
	dropin probe
	dropin watch
	dropin mirror
	systemctl daemon-reload
	svc enable litestream.service mirrin-cloud.service caddy.service \
		mirrin-canary@probe.service mirrin-canary@watch.service mirrin-canary@mirror.service
	svc restart litestream.service mirrin-cloud.service
	svc reload-or-restart caddy.service
	svc restart mirrin-canary@probe.service mirrin-canary@watch.service mirrin-canary@mirror.service
	if [ -f /var/lib/private/mirrin-canary-twin/cloud/link.json ]; then
		svc enable mirrin-canary@twin.service
		svc restart mirrin-canary@twin.service
	else
		echo "The canary twin isn't linked yet: cloud/ops/RUNBOOK.md, \"First deploy\", step 6."
	fi
	;;
probe)
	need_creds CANARY_PROBE_TOKEN
	need_cmd caddy
	bin mirrin-canary
	install -D -m 0644 canary.yaml /etc/mirrin-canary/canary.yaml
	install -m 0644 Caddyfile /etc/caddy/Caddyfile
	/usr/local/bin/mirrin-canary check-config --config /etc/mirrin-canary/canary.yaml --role probe
	caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
	unit mirrin-canary@.service
	dropin probe
	systemctl daemon-reload
	svc enable caddy.service mirrin-canary@probe.service
	svc reload-or-restart caddy.service
	svc restart mirrin-canary@probe.service
	;;
*)
	echo "install.sh: unknown role $role" >&2
	exit 2
	;;
esac
if [ "$start" = yes ]; then
	systemctl --no-pager --failed
	echo "Installed and started the $role services."
else
	echo "Installed the $role services; nothing was started."
fi
