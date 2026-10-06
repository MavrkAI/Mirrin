#!/bin/sh
# render.sh OUTDIR ENVFILE...
#
# Renders one host's files from the templates in this directory into
# OUTDIR. Each ENVFILE is KEY=VALUE lines (read as data, never run); later
# files override earlier ones, so a host is rendered as
#
#   cloud/deploy/render.sh out/r1 cloud/deploy/hosts/common.env cloud/deploy/hosts/r1.env
#
# Templates name values as @@KEY@@. Rendering fails if a template names a
# key no file sets, or if a value still says REPLACE (set
# ALLOW_PLACEHOLDERS=1 to render an example anyway). A line that starts
# with @@?KEY@@ is optional: it is kept, without that marker, only when KEY
# is set and not empty (the second signing kid during a key rotation).
# ROLE picks the files: relay, cp, probe, or dns (the DNS files, for no
# machine). Nothing is written to OUTDIR unless every file renders.
set -eu

if [ $# -lt 2 ]; then
	echo "usage: render.sh OUTDIR ENVFILE..." >&2
	exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
out=$1
shift

# The last ROLE= wins, as with every other key.
role=$(awk -F= '$1 == "ROLE" { r = substr($0, 6) } END { print r }' "$@")
case $role in
relay)
	files="relay.example.yaml:relay.yaml mirrin-relay.service relay-notify.sh with-credentials.sh install.sh"
	;;
cp)
	files="cloud.example.yaml:cloud.yaml mirrin-cloud.service litestream.yml litestream.service
	       canary.example.yaml:canary.yaml mirrin-canary@.service with-credentials.sh
	       canary-credentials-probe.conf canary-credentials-watch.conf canary-credentials-mirror.conf
	       Caddyfile.cp:Caddyfile restore-drill.sh install.sh"
	;;
probe)
	files="canary-probe.example.yaml:canary.yaml mirrin-canary@.service with-credentials.sh
	       canary-credentials-probe.conf Caddyfile.probe:Caddyfile install.sh"
	;;
dns)
	files="dns/tenant-apex.json:tenant-apex.json dns/route53-policy.json:route53-policy.json
	       dns/route53-dnssec.sh:route53-dnssec.sh dns/operator-zone.zone:operator-zone.zone"
	;;
*)
	echo "render.sh: ROLE must be relay, cp, probe or dns, not '$role'" >&2
	exit 2
	;;
esac

# Render into a scratch directory and move the files into OUTDIR only when
# every one rendered: a half-rendered OUTDIR could be copied to a host.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
for f in $files; do
	src=${f%%:*}
	dst=${f##*:}
	awk -v allow="${ALLOW_PLACEHOLDERS:-0}" -v tmpl="$here/$src" '
		# Every file but the template is an env file.
		FILENAME != tmpl && /^[A-Za-z_][A-Za-z0-9_]*=/ {
			i = index($0, "=")
			vals[substr($0, 1, i - 1)] = substr($0, i + 1)
			next
		}
		FILENAME != tmpl { next }
		{
			line = $0
			if (match(line, /^@@[?][A-Za-z_][A-Za-z0-9_]*@@/)) {
				k = substr(line, 4, RLENGTH - 5)
				line = substr(line, RLENGTH + 1)
				if (!(k in vals) || vals[k] == "") next
			}
			done = ""
			while (match(line, /@@[A-Za-z_][A-Za-z0-9_]*@@/)) {
				k = substr(line, RSTART + 2, RLENGTH - 4)
				if (!(k in vals)) {
					printf "render.sh: %s names @@%s@@, which no env file sets\n", tmpl, k > "/dev/stderr"
					bad = 1
				} else if (allow != "1" && vals[k] ~ /REPLACE/) {
					printf "render.sh: %s is still a placeholder (%s)\n", k, vals[k] > "/dev/stderr"
					bad = 1
				}
				done = done substr(line, 1, RSTART - 1) vals[k]
				line = substr(line, RSTART + RLENGTH)
			}
			print done line
		}
		END { exit bad }
	' "$@" "$here/$src" >"$tmp/$dst"
	case $dst in
	*.sh) chmod 0755 "$tmp/$dst" ;;
	*) chmod 0644 "$tmp/$dst" ;;
	esac
done
mkdir -p "$out"
for f in $files; do
	mv -f "$tmp/${f##*:}" "$out/"
done
echo "rendered the $role files into $out"
