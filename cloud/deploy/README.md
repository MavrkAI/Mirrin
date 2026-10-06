# Deploying Mirrin Cloud

Everything needed to run the paid service's servers, as files in this
directory: two relays at two providers, one control-plane VM, the probes,
the DNS records, and the secrets each service needs. Nothing here is
deployed yet (status: planned). `docs/cloud-design.md` §15 is the design;
`cloud/ops/` holds the runbooks and the alert guide.

No file here holds a secret. Host files hold public values (addresses,
names, key ids, public keys, bucket names); secrets are systemd encrypted
credentials made on each machine.

## What runs where

| Machine | Provider (suggested) | Runs | Public names |
|---|---|---|---|
| r1 | provider A, region 1, reserved IPv4 + IPv6 | `mirrin-relay` | `r1.relay.OZ` |
| r2 | provider B, region 2, reserved IPv4 + IPv6 | `mirrin-relay` | `r2.relay.OZ` |
| cp | provider A or C | `mirrin-cloud`, Litestream, Caddy, `mirrin-canary@{twin,probe,watch,mirror}` | `cloud.OZ`, `watch.ops.OZ` |
| probe-b | a third provider or region | Caddy, `mirrin-canary@probe` | `probe-b.ops.OZ` |
| probe-c | a fourth provider or region | Caddy, `mirrin-canary@probe` | `probe-c.ops.OZ` |

OZ is the operator zone (`mirrin.app` in the examples) and TZ the tenant
zone (`mirrin.link`), where every handle lives. The relays are the only
machines users' traffic touches; a relay sees ciphertext and connection
metadata only. The control plane never sees user traffic.

Each machine is the smallest VM its provider sells (1 vCPU, 1–2 GB) on
Debian 13 or Ubuntu 24.04 with systemd 255 or later.

## Files

| File | Becomes | On |
|---|---|---|
| `hosts/*.env` | the values for one machine; `common.env` is shared | – |
| `render.sh` | fills the templates for one machine | your laptop |
| `install.sh` | installs and (re)starts one machine's services | each machine |
| `relay.example.yaml` | `/etc/mirrin-relay/relay.yaml` | r1, r2 |
| `mirrin-relay.service` | the relay's hardened unit | r1, r2 |
| `relay-notify.sh` | mails the abuse inbox when a relay suspends a handle | r1, r2 |
| `cloud.example.yaml` | `/etc/mirrin-cloud/cloud.yaml` | cp |
| `mirrin-cloud.service` | the control plane's unit, with its signing keys as encrypted credentials | cp |
| `litestream.yml`, `litestream.service` | continuous replication of `cloud.db` to R2 | cp |
| `restore-drill.sh` | the quarterly restore drill, and the real restore | cp |
| `canary.example.yaml`, `canary-probe.example.yaml` | `/etc/mirrin-canary/canary.yaml` | cp, probes |
| `mirrin-canary@.service`, `canary-credentials-*.conf` | one unit per canary role, and each role's credentials | cp, probes |
| `with-credentials.sh` | turns credentials into the environment variables the configs name | every machine |
| `Caddyfile.cp`, `Caddyfile.probe` | TLS in front of the control plane and the probes; no access log | cp, probes |
| `dns/` | the tenant zone (Route 53, DNSSEC), its IAM policy, the operator zone | – |

`deploy_test.go` renders every machine's files and checks them as the
services will: each config through its own loader, every unit for
hardening and for credentials that match what the configs read, the DNS
files for the zone rules, and every script for syntax. Run it with
`cd cloud && go test ./deploy`.

## Render and install

On your laptop, with the host files filled in:

```
make relay-release                  # dist/mirrin-relay-linux-amd64 (and arm64)
cd cloud && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o ../dist/ ./cmd/mirrin-cloud ./cmd/mirrin-canary && cd ..
cloud/deploy/render.sh out/r1 cloud/deploy/hosts/common.env cloud/deploy/hosts/r1.env
cp dist/mirrin-relay-linux-amd64 out/r1/mirrin-relay
rsync -a out/r1/ root@r1:/root/mirrin-deploy/
ssh root@r1 /root/mirrin-deploy/install.sh
```

The same for `r2`, `cp` (with `mirrin-cloud` and `mirrin-canary`),
`probe-b` and `probe-c` (with `mirrin-canary`). `install.sh` checks every
config before it restarts anything, refuses to start while a credential is
missing, and is safe to run again: it is also how an upgrade is deployed.
Upgrade one relay at a time and wait for the canary's "relay carries the
canary again" email before the other.

`render.sh` writes nothing to the output directory unless every template
renders: an unset key or a value still saying `REPLACE` fails the whole
host. A template line starting `@@?KEY@@` is kept only while `KEY` is set;
the second signing kid (`ENT_KID2`, `DL_KID2`) uses it during a key
rotation (`../ops/RUNBOOK.md`, "Key rotation").

## Secrets

Each secret is made on the machine that uses it, never copied as plain
text, with `systemd-creds` (systemd 250 or later). It is sealed to that
machine's host key, and to its TPM when it has one:

```
install -d -m 0700 /etc/credstore.encrypted
systemd-creds encrypt --name=PADDLE_API_KEY - /etc/credstore.encrypted/PADDLE_API_KEY
# paste the value, then Ctrl-D
```

The signing keys are made on the control-plane VM itself and encrypted
there; the plain files never leave it:

```
umask 077; d=$(mktemp -d)
mirrin-cloud keys generate --dir "$d"
for f in "$d"/*.pem; do
  systemd-creds encrypt --name="$(basename "$f")" "$f" "/etc/credstore.encrypted/$(basename "$f")"
done
mirrin-cloud keys public --dir "$d"    # into internal/entitle/keys.go and hosts/common.env
```

Keep an offline backup of the plain key files (an encrypted USB stick in a
safe, and the password in the maintainer's password manager), then shred
`$d`. Losing the keys means every daemon needs a release with new public
keys; leaking them means the key rotation runbook at once.

| Credential | Machine | Used by | What it is |
|---|---|---|---|
| `ent-YYYYa.pem`, `dl-YYYYa.pem` | cp | mirrin-cloud | Entitlement and deny-list signing keys |
| `PADDLE_API_KEY`, `PADDLE_WEBHOOK_SECRET` | cp | mirrin-cloud | Paddle API key; notification destination secret |
| `ROUTE53_ACCESS_KEY_ID`, `ROUTE53_SECRET_ACCESS_KEY` | cp | mirrin-cloud | IAM user with `dns/route53-policy.json` only |
| `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY` | cp | mirrin-cloud | R2 token: read and write on the backups bucket only |
| `LITESTREAM_ACCESS_KEY_ID`, `LITESTREAM_SECRET_ACCESS_KEY` | cp | Litestream | R2 token: read and write on the Litestream bucket only |
| `MIRROR_ACCESS_KEY_ID`, `MIRROR_SECRET_ACCESS_KEY` | cp | mirrin-canary@mirror | R2 token: write on the deny-list bucket only |
| `PAGERDUTY_ROUTING_KEY` | cp | mirrin-canary@watch | The pager's integration key |
| `SMTP_USERNAME`, `SMTP_PASSWORD` | cp, r1, r2 | watch, relay-notify | The alert mailbox's SMTP login |
| `CANARY_PROBE_TOKEN` | cp, probe-b, probe-c | probe, watch | A random token: `openssl rand -hex 32` |

A stolen control-plane VM must not be able to erase its own history. R2
tokens can't write without deleting, so put an R2 bucket lock on the
Litestream bucket (a 7-day retention rule, matching Litestream's
`retention: 168h`): nothing younger than a week can be deleted or
overwritten, whoever holds the token (RUNBOOK.md, "First deploy").
