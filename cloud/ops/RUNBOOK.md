# Mirrin Cloud runbook

How to run the paid service: the first deploy, the routine, and what to do
when something specific happens. `ALERTS.md` says what each alert means;
`ABUSE.md` covers reports and takedowns; `../deploy/README.md` is the
deploy config. Status: planned. Every section below is to be executed once
in staging before launch and the date written in the log at the end.

Names used: OZ is the operator zone (`mirrin.app`), TZ the tenant zone
(`mirrin.link`), cp the control-plane VM, r1 and r2 the relays.
Commands run as root unless shown with `sudo -u`.

Contents: [First deploy](#first-deploy) ·
[Routine](#routine) ·
[Upgrades](#upgrades) ·
[Relay IP migration](#relay-ip-migration) ·
[Takedown](#takedown) ·
[Key rotation](#key-rotation) ·
[DNS outage](#dns-outage) ·
[Let's Encrypt rate limit](#lets-encrypt-rate-limit) ·
[Litestream restore drill](#litestream-restore-drill) ·
[Control-plane VM lost](#control-plane-vm-lost) ·
[Incident comms](#incident-comms) ·
[Shutdown](#shutdown) ·
[Log](#log)

## First deploy

Accounts first (the maintainer's list in the WP-20 hand-off names every
one), then in this order:

1. **DNS.** The tenant zone at Route 53 with DNSSEC, its apex CAA and the
   control plane's IAM user (`../deploy/dns/tenant-zone.md`); the operator
   zone at Cloudflare.
2. **R2.** Three buckets: `mirrin-backups` (private), `mirrin-litestream`
   (private, with a bucket lock: 7-day retention), `mirrin-denylist`
   (public through the custom domain `denylist.OZ`, nothing else in it).
   Three API tokens, each for one bucket (`../deploy/README.md`,
   "Secrets").
3. **Machines.** r1 and r2 at two providers, each with a reserved IPv4 and
   IPv6 address; cp; probe-b and probe-c at two more places. Debian 13 or
   Ubuntu 24.04, SSH keys only, unattended security upgrades on, the
   provider's firewall allowing 22 (from the maintainer's addresses), 80
   and 443 only (the probes: 22 and 443). Fill in `hosts/common.env`.
4. **Signing keys** on cp (`../deploy/README.md`, "Secrets"). Their public
   halves go into `internal/entitle/keys.go` in a release, and into
   `hosts/common.env`.
5. **Deploy** r1, r2, then cp, then the probes: render, copy, `install.sh`
   (`../deploy/README.md`). Check: `curl https://r1.relay.OZ/healthz`,
   `curl https://cloud.OZ/v1/version`, `curl https://cloud.OZ/v1/keys`.
6. **The canary.** On cp:
   `sudo systemd-run --pty -p DynamicUser=yes -p StateDirectory=mirrin-canary-twin mirrin-canary link --config /etc/mirrin-canary/canary.yaml`
   (the same dynamic user and state directory the twin's unit uses). Open
   the printed checkout link and pay with the operator's 100% discount
   code. Then `systemctl enable --now mirrin-canary@twin`. Within a few
   minutes: `curl -s 127.0.0.1:9101/v1/status` shows both tunnels up and
   a certificate; the probes' rounds turn green.
7. **Alerts.** The external uptime checks and provider budget alerts
   (`ALERTS.md`); the test page (`ALERTS.md`); then the game day.
8. **Paddle.** Point the notification destination at
   `https://cloud.OZ/v1/billing/webhook`; send a test event from Paddle's
   dashboard and see it in `journalctl -u mirrin-cloud`.
9. **Checks from outside:** `testssl.sh https://CANARY.TZ/` shows the
   canary's own Let's Encrypt certificate and nothing else;
   `dig nobody-here.TZ` is NXDOMAIN; `dig TZ CAA` shows the apex records.

## Routine

**Weekly (30 minutes):** `/usr/local/sbin/mirrin-restore-drill` (a few
minutes; it restores the latest replica from R2 into a scratch directory
and prints its row counts beside the live database's). This is the check
that replication works: the watcher's `litestream` check only sees that
Litestream is running, not that R2 accepts its writes, so a revoked token
or a bucket refusing writes shows up here. Row counts far apart mean the
replica is behind: `journalctl -u litestream` and R2's status, at once.
Then the abuse log and any open suspension
(`ABUSE.md`); costs at each provider against the table in
`docs/cloud-design.md` §15; `curl -s 127.0.0.1:9103/v1/status` on cp for
anything still alarming; dependency and security advisories
(`govulncheck ./...` in both modules); the Let's Encrypt new-certificate
count for the week (`sudo -u mirrin-cloud sqlite3 /var/lib/mirrin-cloud/cloud.db "SELECT count(*) FROM handles WHERE created > strftime('%s','now','-7 days')"`,
kept under 40).

**Quarterly:** the Litestream restore drill, timed and logged below; read
this runbook and fix what has drifted.

**Yearly:** key rotation; the game day (`ALERTS.md`); renew the domains
(both at least two years ahead); review who has access to each account.

## Upgrades

Build the Linux binaries from a tagged release, render if configs changed,
and `install.sh` one machine at a time:

1. r1. Wait for "Relay r1 carries the canary again" (or check
   `curl -s 127.0.0.1:9103/v1/status` on cp: no `relay-r1` alarm).
2. r2, the same way. Never both relays at once.
3. cp. The control plane restarts in a second or two; nothing depends on
   it minute to minute.
4. The probes.

A relay restart drains: daemons are told to move to the other relay, and
spliced connections get 30 seconds. If an upgrade misbehaves, install the
previous binary the same way.

## Relay IP migration

When a relay must move to new addresses: the provider is retiring them, a
new provider, or the old addresses are unusable (blocked, attacked, the
machine lost). The other relay carries everyone throughout; the trick is
never to publish an address that accepts connections without the
daemons' tunnels behind it. A stopped relay refuses connections, and
browsers then try the next address, which is the other relay.

1. **Bring up the new machine** with the new reserved addresses, as rN
   (same id, same control name). Put the new addresses in
   `hosts/common.env` (`RN_IPV4`, `RN_IPV6`), render rN, cp, probe-b and
   probe-c. Install rN's files on the new machine with
   `install.sh --no-start`, so its relay stays stopped: started now it
   would try Let's Encrypt for `rN.relay.OZ` while that name still points
   at the old machine, and the failed validations count against Let's
   Encrypt's limits.
2. **Move the control name:** in Cloudflare, point `rN.relay.OZ` A/AAAA at
   the new addresses (TTL 300).
3. **Switch over:** start the new relay
   (`systemctl enable --now mirrin-relay`; it gets its control
   certificate from Let's Encrypt within a minute), then stop the old one
   (`systemctl stop mirrin-relay` on the old machine, and leave it
   stopped). Daemons are told to drain and reconnect to `rN.relay.OZ`,
   which now resolves to the new machine.
4. **Wait for the tunnels:** on the new machine,
   `curl -s 127.0.0.1:9100/metrics | grep '^mirrin_relay_tunnels'` climbs
   to about what the old one had (10 to 15 minutes, as resolvers' caches
   expire). The canary twin's tunnel is among them:
   `curl -s 127.0.0.1:9101/v1/status` on cp.
5. **Move every handle's records:** install cp's rendered files
   (`install.sh`; `cloud.yaml` now names the new addresses), then mark
   every live handle for rewriting:
   ```
   sudo -u mirrin-cloud sqlite3 /var/lib/mirrin-cloud/cloud.db \
     "UPDATE handles SET dns_pending = 1 WHERE account IS NOT NULL AND released = 0;"
   ```
   The control plane rewrites them in its minutely sweep. Watch it reach
   zero: `... "SELECT count(*) FROM handles WHERE dns_pending = 1;"`. Spot
   check: `dig +short CANARY.TZ A` shows the new rN address and r2's.
6. **Probes:** install probe-b's and probe-c's rendered files (the new
   addresses in `canary.yaml`). cp's own probe came with step 5.
7. **After a day**, release the old addresses at the provider and delete
   the old machine.

The one-relay-down email for rN during steps 3–6 is expected. If the new
machine fails at step 3, start the old relay again and point
`rN.relay.OZ` back.

## Takedown

`ABUSE.md`: triage, then `mirrin-cloud admin deny handle|key`, then check
the relays refuse it.

## Key rotation

Three signing purposes, each with its own key: `ent-*` (entitlements,
checked by daemons and relays), `dl-*` (deny lists, checked by relays and
the watcher), and `wk-*` (wake manifests, planned; not built yet, so
nothing to rotate). Keys rotate yearly, and the next kid always ships one
release before it signs.

The deploy templates carry a second kid per purpose for exactly this:
`ENT_KID2`/`ENT_PUB2` and `DL_KID2`/`DL_PUB2` in `hosts/common.env`,
empty the rest of the year. When set, render adds the second kid to
mirrin-cloud's credentials (so `/v1/keys` lists it), to both relays'
`issuer_keys`, to `canary.yaml`'s trusted keys and to install.sh's
credential check. `ENT_KID`/`DL_KID` is always the kid that signs; the
second kid is the next one before the switch and the old one after it.
No `systemctl edit` drop-ins are used. The steps below are for `ent`; do
the same for `dl` (with `DL_*`), in the same release or the next.

**A release ahead (month 0):**

1. On cp: `umask 077; d=$(mktemp -d)`, restore the current plain keys
   from the offline backup into `$d`, then
   `mirrin-cloud keys rotate --dir "$d" --purpose ent`. It writes the next
   kid, such as `ent-2027a.pem`, and prints its public line.
2. Encrypt the new file as a credential:
   `systemd-creds encrypt --name=ent-2027a.pem "$d/ent-2027a.pem" /etc/credstore.encrypted/ent-2027a.pem`.
3. In `hosts/common.env` set `ENT_KID2=ent-2027a` and `ENT_PUB2=` its
   public key; leave `ENT_KID` as it is. Render and install r1, then r2
   (Upgrades), then cp. Check: `curl https://cloud.OZ/v1/keys` lists both
   kids, and the current kid still signs (`keys:` in `cloud.yaml` is
   unchanged).
4. Add the public line to `internal/entitle/keys.go` beside the current
   key and ship a release.
5. Back up the new plain key offline; shred `$d`.

**The switch (month 1 or later, once most daemons run the release):**

6. In `hosts/common.env` swap the two: `ENT_KID=ent-2027a`,
   `ENT_PUB=` the new public key, and `ENT_KID2`/`ENT_PUB2` now the old
   kid. Render and install r1, r2, cp. Both kids stay loaded, trusted by
   the relays and listed at `/v1/keys`; only the signer changed. New
   entitlements are signed with the new kid; entitlements already issued
   under the old one stay valid until they expire (35 days at most), which
   is why the old kid stays published and trusted until retirement.
   Daemons on releases older than step 4 reject the new entitlement at
   their next daily refresh and keep the old one until it expires: their
   tray says to update.

**Retiring the old kid (35 days after the switch, plus a week):**

7. In `hosts/common.env` empty `ENT_KID2` and `ENT_PUB2`. Render and
   install r1, r2, cp: the old kid leaves the relays, the canary and
   `/v1/keys`. Remove it from `internal/entitle/keys.go` in the next
   release. Delete `/etc/credstore.encrypted/<old kid>.pem` and destroy
   the offline copy.

**If a key leaks:** an `ent-*` key lets whoever holds it mint
entitlements, which steer traffic for any handle but can't get a
certificate for one (the CAA pin); a `dl-*` key lets them deny-list
anyone. Rotate at once, compressing the steps: make the next key, ship an
emergency release with it, switch the control plane, and remove the
leaked kid from the relays straight away. Daemons that haven't updated
lose Cloud reach until they do (their free routes keep working); say so
plainly in the incident post.

## DNS outage

**Route 53 down or refusing writes:** existing handle records keep
resolving (resolvers cache for 300 s; Route 53 serves from its anycast
edge even when its API is down). What waits: new links, recoveries and
ACME-account changes, whose DNS writes the control plane retries every
minute (`dns_pending`). Nothing to do but watch
`SELECT count(*) FROM handles WHERE dns_pending = 1` fall after it
recovers. If the tenant zone stops resolving at all, the canary pages:
check `dig TZ SOA @8.8.8.8` and dnsviz.net (an expired DS or a KSK
problem fails validation everywhere; the CloudWatch DNSSEC alarms say
which). A broken DNSSEC chain is fixed at Route 53 (reactivate the KSK) or,
as a last resort, by removing the DS at the registrar, which takes the
parent TTL (up to a day) to clear.

**Cloudflare (the operator zone) down:** daemons already connected keep
their tunnels; new tunnel connections resolve `rN.relay.OZ` from caches.
The control plane's name is also there; nothing needs it minute to
minute. If it lasts hours, move the operator zone's records to Route 53
(a second hosted zone, the rendered `operator-zone.zone` imported) and
change the NS at the registrar.

**The registrar:** a hijacked registrar account can rewrite everything,
including CAA. Registry lock and 2FA are the prevention; every twin's
CT/CAA watch is the detection.

## Let's Encrypt rate limit

Let's Encrypt allows 50 new certificates per registered domain (the tenant
zone, until it is on the PSL) per week; renewals don't count. The control
plane caps new handles at `activations_per_week: 40`, counting open
checkouts.

- **A daemon reports `rateLimited`** (its logs; the "canary certificate
  not renewing" email for the canary): check the week's new handles
  (Routine) and https://letsencrypt.org/docs/rate-limits/. Renewals are
  exempt, so an existing customer only hits this if their renewal became
  a new order (key rotation counts as a renewal only while the name set is
  unchanged).
- **The cap is reached:** new links are turned away with the control
  plane's plain message until the week rolls on; nothing to do but wait,
  or lower `activations_per_week` further if Let's Encrypt's count and
  ours disagree.
- **To grow:** the PSL entry (per-handle limits then apply instead), and
  Let's Encrypt's rate limit adjustment form for the tenant zone (submitted
  in W0). A second CA with per-account EAB comes later (WP-28).
- **Let's Encrypt down:** certificates already issued last 90 days and
  renew with a third left, so there are weeks of slack. New handles wait.

## Litestream restore drill

Quarterly, on cp (it reads R2 and changes nothing):

```
/usr/local/sbin/mirrin-restore-drill
```

It restores the latest replica into a scratch directory, checks its
integrity and schema, prints its row counts beside the live database's (a
difference of a few rows is the last seconds of writes), serves the copy
with a throwaway `mirrin-cloud serve --dev` on loopback, and prints how
long it took. Write the date, the time taken and the counts in the log
below. It fails if it took over 15 minutes.

Once a year, run the full drill on a fresh VM instead (the next section,
against a staging bucket copy or with the fresh VM's mirrin-cloud kept off
the public name), timing from VM creation to `/v1/version` answering. The
target is under 15 minutes.

## Control-plane VM lost

Nothing user-facing breaks at once: entitlements last 35 days, relays keep
the last deny list (and read the mirror), certificates renew from Let's
Encrypt. Restore calmly, from the replica:

1. Create a new VM; point `cloud.OZ` and `watch.ops.OZ` at it (TTL 300).
2. Make its credentials again on it (`../deploy/README.md`, "Secrets"): the
   signing keys come from the offline backup; the other secrets from their
   services (Paddle, AWS, R2: make new tokens and revoke the old ones,
   since the old machine is gone with them in its credential store).
3. Render cp, copy the files and binaries, and run
   `install.sh --no-start`: everything is installed and checked, and
   nothing starts. Starting mirrin-cloud now would make an empty database,
   and Litestream would replicate it as the newest generation.
4. `/usr/local/sbin/mirrin-restore-drill --production`. It restores
   `cloud.db` from R2, checks it and starts Litestream and mirrin-cloud.
5. `install.sh` again to start Caddy and the canary roles. The canary
   twin's state was on the lost machine: link it again (First deploy,
   step 6; the old canary handle stays taken, so use a new
   `CANARY_HANDLE`, or recover the old one with its recovery words if you
   kept them).
6. Check `https://cloud.OZ/v1/version`, a daemon's `mirrin cloud status`,
   and that the deny list's seq is not lower than the relays' (their
   `mirrin_relay_denylist_seq` metric) or the mirror's. Writes after the
   last replicated second are lost: at most a second of them, which can
   include a `deny_seq` bump. If `/v1/denylist`'s seq is lower, relays
   and the mirror ignore every list until it passes theirs (the
   "deny-list mirror" email says "lower than the N already mirrored").
   Bump it past the highest seq seen, with a second person watching:
   ```
   sudo -u mirrin-cloud sqlite3 /var/lib/mirrin-cloud/cloud.db \
     "BEGIN IMMEDIATE; UPDATE meta SET v = MAX(v, HIGHEST) + 1 WHERE k = 'deny_seq'; COMMIT;"
   ```
   (HIGHEST is the largest `mirrin_relay_denylist_seq` or mirrored seq),
   then check `/v1/denylist`'s seq is above it and the mirror email
   clears.

## Incident comms

For any incident a user could notice (the page, a lost control plane, a
takedown error, a key leak):

1. **Within 30 minutes of the page:** a short post on the status page (or
   the pinned GitHub issue, until there is one), following
   `docs/ops/incident-comms.md`: what is affected, what isn't (twins keep
   working at home and over free routes), and when the next update comes.
2. **Every hour** while it lasts, even if nothing changed.
3. **When resolved:** what happened and when it ended.
4. **Within 5 working days:** a written review in `docs/ops/incidents/`
   (what happened, the timeline, why, and what changes), blameless and
   plain. Security incidents follow `SECURITY.md` first.

Never promise a time you don't control; never blame a provider without
their confirmation.

## Shutdown

The pledge (`docs/ops/shutdown-pledge.md`): 90 days' notice, and backups
stay downloadable. If Mirrin Cloud ever closes:

1. **Day 0:** announce on the site, the status page, the CHANGELOG and by
   email to every customer (through the merchant of record). Stop new
   checkouts by archiving the price at Paddle, so no checkout can be
   opened for it. Leave `activations_per_week` as it is: the control
   plane reads 0 as "no cap", not "closed", so setting it to 0 would lift
   the Let's Encrypt safety cap instead. (A real "closed to new links"
   switch in the control plane is a follow-up.)
   Explain the free routes (Tailscale, a self-hosted relay with its own
   domain) and how to move to them with `mirrin reach use`.
2. **Day 0 to 90:** everything keeps running. Cancel renewals at the
   merchant of record so nobody is charged past the end; refund any period
   paid past day 90. Keep answering support.
3. **Day 90:** the relays stop carrying Cloud handles. Backups stay
   downloadable: the control plane keeps serving `get` and `list` presigns
   for at least 90 more days, and customers can restore with their 12
   words from the downloaded `age` files, without us.
4. **Day 180:** delete the backups bucket, the Litestream replica, the
   control-plane database and the signing keys; release the relays'
   addresses. Keep the tenant zone registered (and every handle NXDOMAIN)
   until the registration runs out, so no handle can be taken over.
   Publish a final note saying all of this was done.

## Log

Each runbook section executed in staging, then each drill. Add a row per
run.

| Date | Section | Who | Result / time taken |
|---|---|---|---|
| | First deploy | | |
| | Upgrades | | |
| | Relay IP migration | | |
| | Takedown | | |
| | Key rotation | | |
| | DNS outage | | |
| | Let's Encrypt rate limit | | |
| | Litestream restore drill | | |
| | Control-plane VM lost | | |
| | Incident comms | | |
| | Game day (ALERTS.md) | | |
