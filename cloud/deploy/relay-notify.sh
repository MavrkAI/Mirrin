#!/bin/sh
# relay-notify.sh HANDLE NETWORKS
#
# abuse.notify_command for the hosted relays: the relay has suspended
# HANDLE for review because it was reached from NETWORKS distinct client
# networks in one day. This mails the abuse inbox, which is the start of
# the takedown runbook (cloud/ops/ABUSE.md). It runs with no shell from
# the relay, as the relay's user, with the relay's credentials directory:
# SMTP_USERNAME and SMTP_PASSWORD are systemd credentials there.
#
# Installed as /usr/local/lib/mirrin/relay-notify.
set -eu
handle=$1
networks=$2
case $handle in
'' | *[!a-z0-9-]*) echo "relay-notify: bad handle" >&2; exit 2 ;;
esac
case $networks in
'' | *[!0-9]*) echo "relay-notify: bad count" >&2; exit 2 ;;
esac
creds=${CREDENTIALS_DIRECTORY:?no credentials directory}
relay=@@RELAY_ID@@
now=$(date -u '+%a, %d %b %Y %H:%M:%S +0000')
msg=$(mktemp)
trap 'rm -f "$msg"' EXIT
cat >"$msg" <<MAIL
From: $relay <@@OPS_EMAIL@@>
To: @@ABUSE_EMAIL@@
Subject: [mirrin abuse] $relay suspended $handle.@@TENANT_ZONE@@ for review
Date: $now
Content-Type: text/plain; charset=utf-8

Relay $relay suspended $handle.@@TENANT_ZONE@@: it was reached from $networks
distinct client networks today, over the review threshold.

The suspension lasts until it expires or you lift it. Follow
cloud/ops/ABUSE.md: look before deciding, and write down what you did.

  ssh $relay 'curl -s http://127.0.0.1:9100/debug/connlog?handle=$handle'
  mirrin-cloud admin show $handle --config /etc/mirrin-cloud/cloud.yaml
MAIL
curl --silent --show-error --max-time 25 --ssl-reqd \
	--url "smtp://@@SMTP_ADDR@@" \
	--user "$(cat "$creds/SMTP_USERNAME"):$(cat "$creds/SMTP_PASSWORD")" \
	--mail-from "@@OPS_EMAIL@@" --mail-rcpt "@@ABUSE_EMAIL@@" \
	--upload-file "$msg"
