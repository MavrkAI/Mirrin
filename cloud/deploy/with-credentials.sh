#!/bin/sh
# with-credentials.sh COMMAND [ARG...]
#
# Exports each systemd credential whose name looks like an environment
# variable (PADDLE_API_KEY, SMTP_PASSWORD, ...) as that variable, then runs
# COMMAND in its place. Other credentials, such as the ent-*.pem and
# dl-*.pem signing keys, stay files in $CREDENTIALS_DIRECTORY, where the
# service reads them. Decrypted secrets exist only in the service's own
# credentials directory, in memory, for as long as it runs.
#
# Installed as /usr/local/lib/mirrin/with-credentials.
set -eu
if [ -n "${CREDENTIALS_DIRECTORY:-}" ] && [ -d "$CREDENTIALS_DIRECTORY" ]; then
	for f in "$CREDENTIALS_DIRECTORY"/*; do
		[ -f "$f" ] || continue
		name=${f##*/}
		case $name in
		'' | [0-9]* | *[!A-Z0-9_]*) continue ;;
		esac
		value=$(cat "$f")
		export "$name=$value"
	done
fi
exec "$@"
