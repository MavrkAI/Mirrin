# Mirrin Cloud: design

Status: accepted for build, 2026-09-27.
Scope: the free availability features that ship first, and the paid availability layer ("Mirrin Cloud").
Names, chosen 2026-10-04 (nothing is deployed on them yet):
- tenant zone `mirrin.link` (TZ), to be registered before Cloud opens
- operator zone `mirrin.app` (OZ), also the website's domain. The egress guard counts the zone's apex as Cloud, so nothing the free twin fetches may be served from `mirrin.app` or a name under it.
- product name "Mirrin Cloud"

## 1. The decision

**Mirrin Cloud sells availability, never custody.** The twin stays whole and local on the user's hardware: brain, memory, credentials and signed-in browser. Cloud rents only what needs a server:

1. **Reach.** An address that works from any network. TLS terminates on the user's machine, with a certificate only that machine holds.
2. **Off-site backup.** Storage for snapshots encrypted before they leave, under a key derived from 12 words we never see.
3. **Later, a GPU.** Wake-word training from phrase text only, once the training data is licence-clean.

Every paid convenience has a free, documented route that ships first. Using Mirrin never requires an account, and nothing the OSS does today moves behind a paywall. We do not host twins, multi-tenant or otherwise.

**Spine: the trust-maximalist design.** Its core pieces:
- SNI-routed TLS passthrough.
- Daemon-held Let's Encrypt certificates obtained through the relay (TLS-ALPN-01).
- Stock age backups.
- Offline PASETO entitlements.
- All servers open source and self-hostable.

**Grafted from the ship-first design:**
- One plan, sold through a merchant of record.
- A signed deny list and generation-ordered supersede.
- A CHANGELOG notice before checkout.
- A free "Pocket" release first.
- One pageable alert.
- A dated plan with cut lines.

**Grafted from the magic-first design:**
- The Welcome window.
- One-QR "Add your phone" with live steps.
- iOS install tickets.
- A relay status endpoint.
- "Resolved" pushes.
- A focused approval page.
- A handover marker.
- A 12-word Recovery Kit.
- An import allowlist test.

**Rejected:**
- DNS-01 via cloud TXT writes.
- Phone key-slot restore: the operator or anyone holding the account email could exfiltrate the backup key.
- Managed Twilio numbers.
- CNAME BYOD.
- Trials.
- Backing up Chrome Safe Storage.
- Global pairing lockouts.
- Classifying `tailscale serve` traffic by Host.
- Stripe without a merchant of record.
- Anycast autocert.
- A GTS fallback without EAB.
- Selling wake training on non-commercial data.

## 2. Code facts this design rests on (verified 2026-09-27)

| Fact | Where | Consequence |
|---|---|---|
| `mirrin pair` prints the master `api.token` inside every code (base64url of `addr\|token\|name`, no prefix) | `cmd/mirrin/main.go` showPairing/connectRemote | Pairing v2 uses single-use offers. Legacy codes are detected by decoding and still work on plain-HTTP `api.remote`, with a warning. |
| The page cookie *is* the master token (SameSite Lax on `/ui`, Strict on `/memory` and `/health`). Auth compares with `!=` | `internal/api/memory.go`, `api.go` | Any page on another `127.0.0.1`/`localhost` port is same-site, so it can POST `/approvals/{id}/approve`. `/message/stream` decodes a `text/plain` body. Loopback gets Host, Origin and Sec-Fetch-Site checks plus constant-time compares. DNS rebinding cannot obtain the cookie, so the Host check is defence in depth. Fix `docs/threat-model.md` ("derived from"). |
| The Docker `HEALTHCHECK` calls `/health` without a token and always gets 401 | `packaging/docker/Dockerfile` | Add a public `/healthz`. |
| The `approvals` table has no risk column; risk is computed at execute time | `internal/memory/store.go`, `agent.execute` | WP-02 adds a persisted risk. |
| `events.Bus.Publish` drops events for slow subscribers | `internal/events/events.go` | Push and security alarms use a direct hook. |
| `ui.html` approves with a bare `fetch(url,{method:'POST'})` and has no step-up handling | `internal/api/ui.html` | A `pwa.js` shim handles 428, so `ui.html` is untouched until the maintainer's work lands. |
| `x/crypto/acme` v0.57 has no ARI; `autocert` makes a new key on every renewal | module cache | Use our own ACME loop with key reuse and a small ARI GET. |
| Go ≥1.26 `ecdh.GenerateKey` ignores `rand`; `testing/cryptotest.SetGlobalRandom` exists | `go doc` | The RFC 8291 test uses an `encryptWith(ephemeral, salt)` seam. |
| Notarization pipeline exists (`scripts/release-macos.sh`) but secrets are unset; `install.sh` strips quarantine and verifies nothing | `release.yml`, `install.sh` | WP-11. |
| Wake training uses `openwakeword_features_ACAV100M` (CC BY-NC-SA 4.0) and macOS `say` voices | `docs/wake-word-config.yaml`, `docs/wake-word-gen_clips.py` | No paid training until WP-24. The old model is not shipped (since 2026-10-04); WP-24 trains a licence-clean one. |
| The twin's Chrome uses the real macOS keychain | `internal/skills/browser/browser.go` | Never back up Chrome Safe Storage; exclude `chrome-profile`. |
| `identity export` already moves a twin without secrets | `internal/identity` | Backup is the encrypted superset. The two share a Manifest format. |
| Uncommitted: `internal/api/api.go`, `ui.html`, `internal/channels/voice/*`, README, CHANGELOG, `docs/skills.md`, whatsapp, browser | git status | Only WP-00, WP-01, WP-21, WP-24 and WP-26 touch them. Land that work first. |

## 3. Tiers

| | Free (open source) | Mirrin Cloud | Supporter |
|---|---|---|---|
| Price | $0, no account | $6/mo or $60/yr. Founding: $48/yr for the first 500, kept while subscribed. 30-day refund, no trial | $20/mo |
| The whole twin | ✓ forever | ✓ | ✓ |
| Installable phone app, per-device keys, QR pairing | ✓ | ✓ | ✓ |
| Lock-screen approvals (Web Push sent directly from your machine) | ✓ | ✓ | ✓ |
| Face ID step-up for dangerous approvals | ✓ | ✓ | ✓ |
| Reach from anywhere | Tailscale, your cert files, or your own mirrin-relay + domain | `handle.TZ` on two relays, CAA-pinned to your machine | ✓ |
| Encrypted backup | Folder, iCloud Drive, any S3 | + 20 GB zero-config, 90 days' access after cancelling | ✓ |
| Twilio webhooks | Any public URL | Your handle | ✓ |
| Support | Community | Email, 2 business days | Next business day, best effort |
| Later | local wake training | BYOD on managed relays (v1.1); 3 wake trainings/yr once licence-clean | |

A household plan and one-off wake training may come after launch, driven by demand.

## 4. Architecture

```
 phone / tablet / wall / laptop (PWA, CLI)           Apple · Google · Mozilla · Microsoft push
          │ HTTPS; TLS ends on the daemon                           ▲ RFC 8291 ciphertext
          ▼                                                          │
 ┌─ free routes ─────────────────┐  ┌─ paid route ─────────────────────┐
 │ Tailscale (ts.net cert, own)  │  │ r1 / r2 mirrin-relay (SNI splice)│
 │ your own mirrin-relay + domain│  │   ▲ outbound WSS + yamux tunnels │
 └──────────────┬────────────────┘  └───┼──────────────────────────────┘
                ▼                       │
 ┌──────────────────────────── user's machine ─────────────────────────────────┐
 │ remote TLS listener ─ remote policy ─ device auth ─┐                         │
 │ loopback HTTP listener (tray, local CLI, master) ──┴─ api mux ─ daemon      │
 │ agent · memory · browser · 13 channels · approvals · voice                   │
 │ push sender · step-up · backup engine (age) · ACME · CT/CAA watch · ledger   │
 └──────┬─────────────────────────────────────┬───────────────────────────────┘
        │ age ciphertext                      │ RFC 9421-signed metadata calls (only when linked)
        ▼                                     ▼
 folder / iCloud / S3 / R2 (presigned)   mirrin-cloud control plane ── Route 53 (per-handle A/AAAA/CAA)
                                               └── merchant of record (email and card live here)
```

**Trust boundaries:**
- **User's machine:** all plaintext and every private key.
- **Paired devices:** each holds its own scoped, revocable credential.
- **Relays:** ciphertext and connection metadata.
- **Control plane:** public keys, handles, billing status, backup sizes.
- **Vendors:** push services (encrypted payloads), CAs and CT (hostname and public key), R2 (ciphertext), merchant of record (email and card), Twilio (the user's own contract).

**Package map (new):**

| Package | Role |
|---|---|
| `internal/devices` | Device registry, offers, install tickets, passkeys, tombstones |
| `internal/api` | `auth.go`, `mount.go`, `remote.go`, `pwa.go`, `push.go`, `stepup.go`, `approve.go`, loopback pages |
| `internal/tlsmgr` | Certificate sources: files, tailscale, acme |
| `internal/tailscale` | Tailscale CLI wrapper |
| `internal/reach` | Mode orchestration, verify, alarm playbook, BYOD records, keep-awake |
| `internal/push` | VAPID, RFC 8291, dispatcher |
| `internal/stepup` | WebAuthn step-up (go-webauthn) |
| `internal/backup`, `internal/sigv4` | Backup engine; S3/R2/Route 53 signing |
| `internal/relay/wire`, `internal/relay`, `internal/relay/server` | Protocol, client, server |
| `internal/entitle`, `internal/httpsig` | Tokens, deny lists, request signatures |
| `internal/certwatch` | CT and CAA watch |
| `internal/cloud` (+`/cloudtest`, `/backuptarget`) | Cloud client, inert until linked |
| `cmd/mirrin-relay` | Relay binary |
| `cloud/` | Nested MIT module: control plane, canary, deploy, ops |

## 5. Local API: listeners, devices, pairing

### 5.1 Listeners and classification
Trust comes from **which listener** a request arrived on, never from the Host header or the source address.

**Loopback listener** (plain HTTP, `api.listen`, default `127.0.0.1:7742`):
- The master token and the `?token=` bootstrap work here only.
- Host must be `127.0.0.1:p`, `localhost:p` or `[::1]:p`, else 421.
- Unsafe methods need `Origin` equal to the request origin (or absent when a bearer token is used), and `Sec-Fetch-Site` ∈ {same-origin, none}.
- Requests carrying `Forwarded`, `X-Forwarded-*`, `Via` or `Tailscale-*` get 421.
- Loopback-only pages: Welcome, Reach, Devices, Backup, Trust, Channels, Accounts, Memory.

**Remote listeners** (TLS: Tailscale, files, relay):
- Host must be in the listener's hostnames, else 421.
- Headers: HSTS 1y, `CSP default-src 'self'` (connect-src adds the relay status origins), `frame-ancestors 'none'`, CORP same-origin, nosniff, `Referrer-Policy no-referrer`.
- Per-IP token buckets.
- Unsafe methods need `Origin == https://host` and `Sec-Fetch-Site: same-origin`.
- Route allowlist: `/ui /screen /screen/shot /events /status /message /message/stream /approvals/* /approve/* /push/* /stepup/* /pair /pair/claim /pair/ticket /start /manifest.webmanifest /sw.js /pwa/* /icons/* /characters.js /offline /healthz /phone/*` (Twilio-signed). Everything else returns 404 unless `reach.admin_remote` is set and the device has `admin`.
- Device credentials only.

The Tailscale mode binds the tailnet IP (`:443` on macOS and Windows; `:7743` on Linux unless `CAP_NET_BIND_SERVICE`). The daemon terminates TLS with a `tailscale cert`. We do not use `tailscale serve` (its Host-based classification is spoofable by tailnet peers). Tailscale Funnel or a user's reverse proxy may forward **raw TCP** to a remote listener bound on loopback, and the listener's exposure still applies.

### 5.2 Devices and scopes
Token format: `abt1_<id16hex>_<b64url 32B>`. `data/devices.json` (0600) stores:
- the SHA-256 of each token
- scopes
- passkeys
- last seen and last IP
- revocation tombstones

Comparisons are constant-time.

| Scope | Grants |
|---|---|
| `view` | Screen, events, shots |
| `chat` | `/message*` |
| `approve` | Approvals |
| `admin` | Devices, channels, accounts, memory writes, reach config. Loopback only by default. |

Defaults: PWA view+chat+approve, kiosk view, CLI view+chat+approve.

Browsers get `__Host-mirrin` (Secure, HttpOnly, SameSite=Lax, Path=/, 180 days sliding). CLIs store the token in `remote.yaml` and pin the SPKI set.

Revocation is instant and local. It deletes push subscriptions and passkeys and schedules a backup.

### 5.3 Pairing v2
Owner action: the "Add your phone" page or `mirrin pair`.
1. The daemon mints an offer: `of_<10 base32>` with a 32-byte secret, 10-minute TTL, single use, carrying kind and scopes.
2. Browsers get the QR `https://<host>/pair#v=2&o=<id>&s=<secret>&n=<name>`. The fragment never hits a log.
3. CLIs get `ab2.<b64url {"u":[urls],"o":"of_…","s":"…","n":"Mirrin","fp":["<spki cur>","<spki next>"]}>`.

```
POST /pair/claim {"o":"of_…","s":"…","name":"Akshay's iPhone","kind":"pwa|cli|kiosk"}
200 {"device":{"id":"…","name":"…","scopes":["view","chat","approve"]},"ticket":"it_…"}   (pwa)
200 {"device":{…},"token":"abt1_…"}                                                  (cli, kiosk)
410 {"error":"offer_used|offer_expired"}   404 (unknown offer: same as any unknown route)
```

- After 5 bad secrets the offer is burned.
- Claims are limited to 10 per minute per IP. There is **no global lockout**: handles are public in CT, so a global lockout would be a free DoS.
- Every new device is announced on the owner's channels and on the Mac page, with a "That wasn't me" revoke.

**Install tickets (iOS Home Screen storage is separate from Safari's):**
1. After the claim, the pair page switches to `<link rel=manifest href="/manifest.webmanifest?t=it_…">`, whose `start_url` is `/start#t=it_…`.
2. `/start` is static. It POSTs `/pair/ticket {t}` only when `display-mode: standalone` and no cookie is present. Tickets are single use and expire after 30 minutes.
3. Fallback: in-app "Finish pairing" with an 8-character link code from the Mac (2 minutes, 5 attempts).

Legacy codes (base64url `addr|token|name`) are detected by decoding and accepted only against plain-HTTP `api.remote`, with a deprecation warning.

## 6. Reach

### 6.1 Modes
`reach.mode`:

| Mode | How | Cost |
|---|---|---|
| `off` | — | — |
| `tailscale` | ts.net certificate | free |
| `files` | Your own certificate files | free |
| `relay` | Your own mirrin-relay | free |
| `cloud` | The paid handle | paid |

CLI: `mirrin reach use …`, `mirrin reach status`, `mirrin reach verify`, `mirrin reach fingerprint`.

**Origin continuity.** One origin per twin for life. The PWA icon, cookie, service worker, push subscription and passkey RP ID are all bound to the origin. Switching modes shows a warning: "your phone will need a new app, a re-pair, notifications and Face ID again". Handles are never reassigned, and are held for 12 months after cancelling.

### 6.2 DNS and certificates

**Tenant zone TZ** (Route 53, DNSSEC signed). Submitted to the PSL on tenant-isolation grounds, and registered for ≥ 2 years.
- No wildcard records, and no HTTPS/ECH records, which would break SNI routing. Unassigned names are NXDOMAIN.
- Apex: `CAA 0 issue ";"` and `CAA 0 issuewild ";"`.
- Per handle, written by the control plane only at link, relink and ACME-account change (never on the renewal path):

```
ember-otter-42.TZ.  300 A     <r1 v4> <r2 v4>
ember-otter-42.TZ.  300 AAAA  <r1 v6> <r2 v6>
ember-otter-42.TZ.  300 CAA   0 issue "letsencrypt.org; accounturi=https://acme-v02.api.letsencrypt.org/acme/acct/<id>; validationmethods=tls-alpn-01"
ember-otter-42.TZ.  300 CAA   0 issuewild ";"
ember-otter-42.TZ.  300 CAA   0 iodef "mailto:security@OZ"
```

**Issuance.** The daemon runs its own `x/crypto/acme` loop (not autocert) using TLS-ALPN-01 through the passthrough. Let's Encrypt's multi-perspective validators simply arrive as relay streams carrying ALPN `acme-tls/1`.
- The ECDSA P-256 key is reused across renewals.
- A next key is pre-generated and announced in `/trust`, in pairing codes and in `verify`. Keys rotate yearly.
- Renewal happens at ⅓ of lifetime remaining, or inside the ARI window when the directory offers `renewalInfo`. This is ready for 45-day certificates.
- An alarm fires at 10 days left.
- Files: `data/tls/{acme-account.key, key-current.pem, key-next.pem, cert.pem, issued.json}`. All of them are in backups.

**What the CAA pin buys.** An attacker who compromises a relay, steals the entitlement key or BGP-hijacks relay IPs can steer routing, but cannot obtain a certificate. The operator, or whoever holds the DNS account or registrar, can rewrite CAA. On the default zone that is **detected, not prevented** (§6.5). **BYOD makes it impossible:**
- A/AAAA to the relay IPs, never a CNAME.
- `_mirrin TXT "v=mirrin1; acct=<id>"`.
- The user's own CAA accounturi.

BYOD is free today with a self-hosted relay, and on the managed relays in v1.1.

Let's Encrypt allows 50 new certificates per registered domain per week (renewals are exempt). Until an adjustment is granted, new activations are capped at 40 a week. A second CA with per-account EAB comes later (WP-28). No CA is listed in CAA without its accounturi.

### 6.3 Tunnel protocol (`mirrin.tunnel.v1`)
The daemon dials `wss://r1.relay.OZ/v1/tunnel` and `wss://r2.relay.OZ/v1/tunnel`: every relay listed in its entitlement, or the one configured self-hosted relay. Each relay terminates only its own control name, on its own IP, with a local autocert certificate. There is no anycast.

```
relay → {"t":"challenge","v":1,"relay":"r1","nonce":"<b64url 32B>"}
daemon→ {"t":"hello","v":1,"key":"<b64url ed25519 pub>","sig":"<b64url>","ent":"v4.public.…|null",
         "status_key_hash":"<b64url sha256(k)>","client":"mirrin/0.4.0 darwin/arm64"}
        sig = Ed25519(key, "mirrin-relay-tunnel-v1" 0x00 relay 0x00 nonce 0x00 exporter)
        exporter = tls.ConnectionState.ExportKeyingMaterial("EXPORTER-mirrin-tunnel", nil, 32)
relay → {"t":"welcome","hostnames":["ember-otter-42.TZ"],"gen":3,"keepalive":25,"max_streams":64}
      | {"t":"error","code":"entitlement_expired|bad_signature|hostname_not_allowed|denied|superseded|superseded_retry|rate_limited|upgrade_required",
         "message":"<sentence shown verbatim>","retry_after":3600}
```

After `welcome`, binary WebSocket frames carry a `hashicorp/yamux` session with the relay as the yamux client.
- **Stream 1** is relay control, as NDJSON: `{"t":"superseded"|"drain"|"notice"|"limits",…}`.
- **Every other stream** is one browser TCP connection: a PROXY v2 header (source address, TLV `0x02` authority = SNI, TLV `0xE0` = relay id), followed by the raw bytes from the peeked ClientHello onward.

The daemon's relay listener yields these as `net.Conn`s whose `RemoteAddr` is the PROXY source. `ServeRemote` wraps them in `tls.Server`.

### 6.4 Relay behaviour

**Peek.** The relay reads at most 16 KiB of ClientHello within a 5 s deadline, handling hellos fragmented across TCP segments and large X25519MLKEM768 key shares.
- Control name: terminate locally.
- Live tunnel for the SNI: splice.
- Anything else: close with zero bytes.

`GetCertificate` refuses every tenant name, and a unit test enforces that.

**Auth.**
- Hosted mode: verify the entitlement against pinned `ent-*` kids, then `exp`, then `cnf == key`, then that the hostnames are a subset of `hosts`, then that neither the handle nor the key is on the deny list.
- Self-host mode: a static `allow: [{hostname, key}]`, with no MavrkAI involvement.

**Supersede.** Per hostname, the higher `(gen, iat)` wins.
- A lower `gen` gets `superseded`. That daemon enters standby and raises an alarm.
- The same `gen` with a lower `iat` gets `superseded_retry`. The daemon refreshes its entitlement and reconnects; it never gives up.
- `gen` increments on every recover or relink.

**Deny list.** PASETO v4.public signed with a `dl-*` key (separate from entitlements): `{seq, iat, handles:[{h,why}], keys:[{k,why}]}`.
- Polled every 60 s from `https://cloud.OZ/v1/denylist`, with a public R2 mirror as a fallback.
- The last good list is kept, and a lower `seq` is ignored.
- Recover deny-lists the old device keys.
- A list token may be up to 256 KiB; entitlements stay within 8 KiB, which would hold only about 90 list entries (WP-13; awaiting sign-off, §19).

**Limits and abuse:**
- 64 streams per handle.
- 20 Mbit/s per tunnel.
- 10 hellos per minute per IP.
- 100 GB/month fair use (warning, then deny list).
- More than 200 distinct client /24 or /48 networks per handle per day triggers auto-suspend ("under review"), and the abuse inbox is emailed. Passthrough means connection metadata is the only abuse signal there is.

**Status.** `GET https://rN.relay.OZ/v1/status/<handle>?k=<b64url 32B>` returns `{"online":true,"since":…}` or `{"online":false,"last_seen":…}`.
- The relay stores only `sha256(k)` from the hello.
- A wrong `k` returns 404. CORS is `*`, and the endpoint is rate-limited.
- The PWA's offline shell asks both relays so it can say "asleep since 14:02".

**Logs.** A 72 h in-memory ring of `{time, handle, client /24|/48, bytes, duration}`, then hourly per-handle totals for 30 days. Payload is never logged. SNI is logged as-is: handles are public in CT, so hashing them would be theatre.

### 6.5 CT/CAA watch and alarm playbook

**Polling.** `certwatch` runs every 6 h and after each issuance:
- **Cert Spotter** `issuances?domain=<host>&match_wildcards=true`, which covers wildcards.
- **crt.sh** for `<host>` and `*.<parent>`.
- **CAA over DoH** (1.1.1.1, 8.8.8.8, 9.9.9.9), expecting our accounturi.

**What counts as unknown.** An issuance whose SPKI is not in {current, next, restored history}, and whose notBefore is after the last-known checkpoint. That second condition is what prevents false alarms after a restore.

**Outages.** One source down produces a warning. Both down produce a health Warn, never a silent pass.

**Playbook.** It runs automatically on an unknown issuance, a CAA mismatch, or a supersede this machine didn't cause:
1. Pause remote approvals (423) until the owner clears the alarm on loopback or the owner channel.
2. Rotate the credentials of devices that authenticated via the public hostname since the rogue notBefore. Those devices see a re-pair page.
3. Revoke passkeys enrolled in that window.
4. On each device's next contact, send `Clear-Site-Data: "cache", "storage"` and bump the service worker version. The service worker registers with `updateViaCache: 'none'` and checks SHA-256 hashes of its shell assets.
5. Raise a security push, a presence-screen banner, an owner-channel message and a health Fail.

**Limit, stated plainly:** browsers can't pin keys, so a same-origin impostor with a valid certificate defeats in-browser defences while it lasts. CT detection, the playbook and BYOD are the answers.

### 6.6 Keep awake
"Stay reachable while on power" is available in every mode:
- macOS: `caffeinate -s -w <pid>`, on AC power only.
- Linux: `systemd-inhibit`.
- Windows: `SetThreadExecutionState`.

## 7. PWA and Web Push

**PWA.**
- `injectHead` adds the manifest link, Apple meta tags and `/pwa/pwa.js` to `ui.html` at serve time. The file itself is never edited.
- `sw.js` caches the shell (`mirrin-shell-<version>`) and is network-only for `/events /screen /message* /approvals* /push* /pair*`. Navigations fall back to `/offline`.
- `pwa.js` provides:
  - the install coach (iOS 26/27 wording: "…" menu → Add to Home Screen)
  - the `#approval=<id>` deep link
  - a 428 step-up fetch shim
- A PWA offline outbox comes later (WP-26). It will carry original timestamps, and stale requests will become questions.

**Push (no Mirrin server).**
- VAPID P-256 key: `data/vapid.pem`, included in backups so subscriptions survive a restore.
- JWT `sub` is `https://github.com/MavrkAI/Mirrin`, never the owner's email (the JWT is signed, not encrypted).

**Endpoint allowlist**, checked at subscribe and at send:
- https on port 443.
- Hosts `web.push.apple.com`, `fcm.googleapis.com`, `*.push.services.mozilla.com`, `*.notify.windows.com`.
- Resolved IPs must be public.

**Encoding and headers.**
- RFC 8291 `aes128gcm` (rs 4096) with RFC 8292 VAPID (ES256, `aud` = the endpoint origin, `exp` 12 h).
- `TTL`: 24 h for approvals, 1 h otherwise.
- `Urgency: high` for approvals, questions and security alarms.
- `Topic: approval-<id>`.
- 404/410 delete the subscription; 429 honours Retry-After.

**Payload:**

```json
{"v":1,"k":"approval","id":12,"t":"Mirrin needs a yes","b":"Book the 5:10 cab, £42.10","u":"/approve/12","tag":"approval-12","badge":1,"ts":1790300000}
```

- `k` is one of `approval | resolved | question | task | security | test`.
- `push.preview` controls the body:
  - `full`: the summary.
  - `brief` (default): the first 60 characters.
  - `private`: "<Name> needs you".
- When an approval is decided anywhere, every other device gets a `resolved` push with the same Topic and tag, and `setAppBadge` shows the count still pending.
- Quiet hours never suppress approval or security pushes.
- **Notifications have no decision buttons.** A tap opens `/approve/<id>`. Android lock-screen actions can run without unlocking the phone, and iOS shows no action buttons anyway.
- On Safari 18.4+ the payload also carries a declarative `web_push` block.

**Delivery.** Dispatch comes from `agent.OnApproval` (WP-02) plus an in-memory retry queue, never from the lossy events bus.

## 8. Approvals from afar: step-up

**When step-up is required.** A dangerous approval (payments, shell, calls) from a remote device needs a WebAuthn assertion with user verification (go-webauthn; RP ID = the listener's host).
- If the device has no passkey, the answer is `403 {"error":"passkey_required","hint":"Approve on the Mac or reply yes 12"}`.
- There is no "optional until enrolled" state.
- Write-risk approvals from the page need only the `approve` scope, unless `reach.step_up: write|all`.
- Loopback decisions and owner-channel `yes N` are unchanged.

**Flow:**

```
POST /approvals/12/approve            → 428 {"stepup":<PublicKeyCredentialRequestOptions>,"session":"su_…"}
challenge = SHA-256("mirrin-approval-v1" 0x00 id(8B BE) "approve" SHA-256(stored input) nonce(32B))
POST /approvals/12/approve  Mirrin-Stepup: su_…  {assertion}  → 200 {"reply":…} | 409 {"decided_by":…,"at":…}
```

Sessions are single use and expire after 2 minutes.

**Enrolment is gated.** A device can register a passkey only:
- within 15 minutes of its pairing claim (once), or
- with an assertion from an existing passkey on that device, or
- after the owner confirms on loopback or the owner channel.

Every enrolment is announced.

**Audit.** Every decision is recorded with the device, the method and the path, e.g. `approval.granted #12 by Akshay's iPhone (passkey, relay r2, 203.0.113.9)`. The focused `/approve/{id}` page shows the screenshot (reusing screenData's lookup), the what and why, two large buttons and the live outcome.

## 9. Backup and restore

**Keys.** The phrase is 12 BIP-39 words (128-bit K), shown once in a Recovery Kit: printable, with a "type word 7" check and a yearly "still have your words?" nudge. K is **never stored**.
- `HKDF-SHA256(K, salt "antbot-backup-v1")` with:
  - info `age-x25519`: the age identity. If the pinned filippo.io/age release supports the hybrid ML-KEM-768+X25519 recipient, info `age-pq-seed` gives that identity instead.
  - info `recovery-ed25519`: the recovery key.
- `ns = base32lower(sha256(recovery_pub))[:26]`.
- Config keeps only `backup.recipient` (`age1…`) and `backup.recovery_pub`. Golden vectors are published in `docs/backup-format.md`.

**Snapshot.**
- **Schedule:** nightly at 03:30, plus 10 minutes (debounced) after a pairing, revoke, passkey enrolment or reach change.
- **Process:** `VACUUM INTO` for each database, then tar.gz, then age.
- **Name:** `snap-20260927T033000Z-1a2b3c4d.age`, with no host or twin name.
- **First tar entry:** `manifest.json`, the same Manifest type as identity export, now `Format 2`:

```json
{"format":2,"kind":"backup","secrets":true,"created":"…","seq":412,"twin":"Mirrin","host_label":"Akshay's MacBook Pro","version":"0.4.0",
 "files":[{"path":"data/memory.db","size":…,"sha256":"…"}],"excluded":["data/chrome-profile","data/whatsapp.db"]}
```

**Contents.**

Included:
- `config.yaml` (with secrets)
- `memory.db`
- personas, protocols, custom tools
- user-trained wake models
- `devices.json`, `push.json`, `vapid.pem`
- OAuth token files
- `data/tls/*` (the ACME account key, so a BYOD CAA pin survives a restore; the current and next TLS keys, so CLI pins survive; `issued.json`)

Excluded:
- `chrome-profile`, whose cookies are Keychain-bound. The Chrome Safe Storage secret is **never** included.
- WhatsApp and Signal sessions: opt-in only (`--with-sessions`), because two machines on one linked identity conflict.
- Models, logs and screenshots.
- `api.token` and the cloud device key.
- The phrase, which never exists on disk.

**Targets.** Each implements `Put / Get / List / Delete`:

| Target | Cost | Notes |
|---|---|---|
| Folder | free | |
| iCloud Drive | free | Default on macOS |
| S3-compatible via SigV4 | free | AWS, R2, B2, MinIO, Wasabi |
| Mirrin Cloud | paid | See below |

Mirrin Cloud mechanics:
- A namespace is bound once, with a recovery-key signature. After that, the device key signs presigns.
- Quota is enforced at presign, and objects are HEAD-verified on commit.
- R2 holds ciphertext under `ns/<ns>/`.
- Retention is 7 daily, 4 weekly and 6 monthly, and the newest verified snapshot is never pruned.
- A monthly restore drill feeds the health check: warn after 48 h without a good snapshot, fail after 7 days.

**Restore** (`mirrin restore` or the Welcome window):
1. Refuse if a daemon is running (unless `--force`).
2. Derive the keys from the phrase.
3. List snapshots by decrypting only the first 64 KiB of each one to read its manifest. The date and `seq` are authenticated, so the operator can withhold snapshots but cannot re-date one.
4. Decrypt to staging and verify every sha256. Reject paths containing `..` or absolute paths.
5. Move the old home to `~/.mirrin.before-restore-<ts>` and swap in the staging copy.
6. Mint a new `api.token` and a new cloud device key.
7. Show a **device review** ("revoke any you don't recognise"): a snapshot taken before a revoke would otherwise resurrect the device. Revokes also trigger a snapshot.
8. Show a checklist: WhatsApp QR, browser sign-ins, voice models.

**Handover.**
- Free targets get an encrypted `handover-<ts>-<rand>.age` marker. The old machine's next backup tick sees it and stands by: paused, channels stopped, tray reads "moved to X".
- With Cloud, the higher handle `gen` supersedes the old machine.

## 10. Mirrin Cloud control plane

**Identity.** There are no accounts or passwords.
- The daemon's Ed25519 **device key** is created at link time and never backed up.
- The **recovery key** comes from the phrase.
- Email and card exist only at the merchant of record. `/v1/me` fetches the email live and never stores it.

**Request signing.** RFC 9421:
- alg `ed25519`
- covered components `@method @target-uri content-digest` (RFC 9530 sha-256)
- params `created`, `expires` (+300 s), `nonce` (16 B), `keyid` (`dev:<b64url sha256(pub)[:16]>`), `tag` (`mirrin-cloud-v1`)

The server allows 300 s of clock skew and keeps a 10-minute nonce cache.

| Endpoint | Purpose |
|---|---|
| `POST /v1/link/start {device_pub, handle?, acme_account}` → `{id, checkout_url}` | The daemon opens the checkout itself |
| `GET /v1/link/{id}` → `{status, entitlement?}` | Poll |
| `POST /v1/entitlement/refresh` → 200 · 402 lapsed · 403 revoked · 409 superseded `{gen, at}` | Daily, jittered |
| `PUT /v1/acme-account {uri}` | Rewrites the handle CAA |
| `POST /v1/backup/namespaces {ns, recovery_pub, sig_recovery}` | Bind the backup namespace |
| `POST /v1/backup/presign {op, name, size}` → `{url, method, headers, expires}` · 413 · 402 | 15-minute URLs |
| `GET /v1/backup/objects`, `POST /v1/backup/commit` | List, verify |
| `POST /v1/recover {ns, device_pub, acme_account, ts, sig_recovery}` (device-signed) | `gen++`, deny-list old keys, rewrite CAA, new entitlement |
| `GET /v1/me` | Every stored field (schema-coverage tested) |
| `POST /v1/billing/portal`, `POST /v1/billing/webhook` | Merchant of record |
| `DELETE /v1/account` | 7-day undo, then rows, DNS records and the R2 prefix are removed; the handle is never reassigned |
| `GET /v1/denylist`, `/v1/keys`, `/v1/version` | Public |

**Entitlement** (PASETO v4.public, footer `{"kid":"ent-2026a"}`):

```json
{"iss":"cloud.OZ","sub":"acct_…","aud":"mirrin","iat":…,"nbf":…,"exp":…,"gen":3,"plan":"cloud","feat":["reach","backup"],
 "handle":"ember-otter-42","hosts":["ember-otter-42.TZ"],"cnf":"<b64url device pub>",
 "relays":[{"id":"r1","url":"wss://r1.relay.OZ/v1/tunnel","ips":["…"]}],"bq":21474836480,"wk":0,"paid_through":…}
```

- `exp = min(now+35d, paid_through+7d)`. Tokens are cached at `data/cloud/entitlement.paseto`.
- Current and next public keys are compiled into `internal/entitle/keys.go`. Until launch the sets are empty, so a default build trusts nothing; dev builds add the public dev keys with `-tags mirrin_devkeys`, which nothing that ships sets.
- **States:**
  - None: never linked; the default, with zero requests.
  - Active.
  - Grace: one calm tray line.
  - Expired: relays refuse, put returns 402, get and list keep working for 90 days.
  - Superseded: standby.
- The control plane can be down for 35 days without anyone noticing.
- Signing keys are separate by purpose: `ent-*` (entitlements), `dl-*` (deny lists), `wk-*` (wake manifests, later). They live in systemd encrypted credentials and rotate yearly, with the next kids shipped one release ahead.

**No crippling, provably:**
- `internal/cloud` may be imported only by `cmd/mirrin`, `internal/daemon`, `internal/reach` and `internal/cloud/...` (an allowlist test).
- A zero-egress test covers the default config.
- `mirrin cloud egress` prints the local ledger of every request sent to MavrkAI (method, path, sizes; never bodies).
- A grep invariant keeps the words "Mirrin Cloud" out of `ui.html`, personas, prompts and channels.

**Data held** (SQLite + Litestream to R2):
- `accounts(id, billing_customer, plan, status, paid_through, created)`
- `devices(pub, account, created, last_seen_day, revoked)`
- `handles(name, account, gen, byod, created, released)`
- `links`
- `namespaces(ns, recovery_pub, account, bytes)`
- `objects(ns, name, size, created)`
- `deny`
- `events` (no content)
- `billing_events`

There is no email column and no IP column.

**Adapters.** Every vendor sits behind an interface with a fake, so v1 builds and tests without credentials:
- `billing.Provider` (Paddle + fake)
- `dns.Provider` (Route 53 via `internal/sigv4` + fake)
- `storage.Provider` (R2 presign + disk fake)

`mirrin-cloud serve --dev` runs the control plane on those fakes. `make cloud-dev` adds two relays and Pebble, so the whole paid journey runs on a laptop.

## 11. Phone
We don't resell numbers. A MavrkAI Twilio parent account could read subaccount SMS and recordings.

Users bring their own Twilio account. When `phone.public_url` is empty, it falls back to the twin's public URL (Cloud handle, BYOD, self-hosted relay, or a Funnel forwarding raw TCP to the remote listener). Twilio's webhooks travel over TLS end to end, and the existing `X-Twilio-Signature` check runs against that URL.

Later (WP-23): an inbound SMS channel and inbound calls. Caller ID is never trusted for approvals.

## 12. Wake words
Later. The current training data is non-commercial (ACAV100M features) and uses the macOS `say` voices, whose SLA bars commercial use.
- **WP-24** rebuilds the pipeline as a pinned container from permissively licensed data, with `DATA-LICENCES.md` checked by a lint, and retrains the bundled model.
- **WP-25** adds cloud GPU jobs:
  - input is phrase text only
  - the output manifest is signed with a `wk-*` key and verified by sha256
  - the model installs with rollback
  - phrases and artifacts are deleted after 7 days
  - failed jobs refund their credit

Local training stays free and remains the default.

## 13. Threat model (summary; `docs/threat-model.md` is updated in WP-21)

**Operator can see:**
- account id and merchant customer id
- handles
- device, recovery and ACME public keys
- tunnel connect times and home IP
- per-connection client /24, SNI, bytes and duration (raw for 72 h in memory, then hourly totals)
- backup counts, sizes and times
- refresh times

**Operator cannot see:**
- anything inside TLS: messages, screens, approvals, cookies, memory, voice
- backup contents or file names
- the phrase, credentials, push payloads

| Threat | Control | Residual |
|---|---|---|
| Relay reads traffic | TLS ends in the daemon. No tenant-cert code path (tested). Splice-tap test. SPKI verify | none |
| Relay compromise, entitlement-key theft or BGP hijack → certificate | Per-handle CAA accounturi + tls-alpn-01, DNSSEC | DoS only; the superseded daemon alarms |
| Operator, DNS account or registrar → certificate | CT watch covering wildcards and apex; CAA check; playbook | Detected, not prevented, on the default zone. BYOD prevents it |
| MITM window harvests cookies, plants passkeys, poisons the SW | Playbook: pause, rotate, revoke, Clear-Site-Data; SW asset hashes | A same-origin impostor wins while it lasts |
| Stolen phone or copied cookie | Per-device revoke; passkey for remote dangerous approvals; gated enrolment; no notification decisions | Write-risk approvals from an unlocked stolen phone |
| Local CSRF from other localhost ports (exists today) | Loopback Host, Origin and Sec-Fetch-Site checks; master token only on loopback; constant-time | none |
| Old or stolen machine takes the handle | gen-ordered supersede; deny list within 60 s; alarm | none after recover |
| Backup read or rollback | age AEAD; authenticated manifest date and seq; staleness warning; device review | Withholding is detectable only by staleness |
| Entitlements used to cripple free features | Import allowlist, zero-egress test, string invariant | none |
| Push SSRF or email leak | Endpoint allowlist, public-IP check, VAPID sub = project URL | Push services see timing and size |
| Pairing brute force or DoS | 256-bit secret in the fragment, per-offer burn, per-IP limit, no global lockout | none |
| Sibling tenant | PSL, `__Host-` cookies, Origin checks, per-host certificates (no HTTP/2 coalescing) | Until the PSL ships, Origin checks carry it |
| Phishing on the shared zone | Payment-gated handles, distinct-client auto-suspend, deny list, AUP | Reputational |
| Malicious release | Checksums + attestations verified by install.sh, notarization, no auto-update | Server binaries can't be attested to users; confidentiality doesn't depend on them |
| Legal compulsion | Minimal metadata; compelled mis-issuance shows up in CT | What the operator can see (above) can be compelled |

**How to verify:**
- `mirrin reach verify | fingerprint`
- `openssl s_client -servername <h> -connect <h>:443 | openssl x509 -pubkey …`
- crt.sh
- `mirrin backup key --age` + stock `age -d`
- `mirrin cloud me`, `mirrin cloud egress`
- the relay source (~1.5k lines)

## 14. Journeys (condensed; see also the structured journeys)

1. **First run.** Notarized DMG, then the Welcome window: brain auto-detected, a live key check, names, a spoken first reply in under 2 minutes. No account, and an empty egress ledger.
2. **Phone from 5G.**
   - Once: "Add your phone", one QR (best route), claim, install coach, install ticket, notifications, Face ID, test push. Each step lights up on the Mac.
   - Later: tap the icon, the cached shell paints, the relay splices by SNI, TLS completes with the Mac's own key.
   - Mac asleep: "asleep since 14:02" from the status endpoint.
3. **Lock-screen approval.** Approval #12 is stored with risk and input; a direct hook sends the push; tap, then Face ID; `/approve/12`; 428 step-up bound to the stored input; the input runs; resolved pushes go everywhere; the audit names the phone.
4. **New Mac.** Welcome, then "Moving in?", then source and 12 words. Authenticated manifests are listed, decrypted, verified and swapped in. `recover` bumps gen and deny-lists the old keys; the same ACME account keeps the CAA valid; the same TLS key means no false CT alarm. Device review and checklist follow. The phone never notices, and the old Mac stands by.
5. **Upgrade.** Three equal cards; `link/start` with public keys only; merchant-of-record checkout (first email); webhook, random handle, DNS records, `gen=1`; tunnels, TLS-ALPN-01 in about 20 s; verify panel green; QR.
6. **Lost phone.** Disconnect on the Mac: the token is tombstoned, push and passkeys are deleted, a backup is triggered, and the phone sees "disconnected".
7. **Leave.** `backup migrate` (byte for byte); `docker run mirrin-relay`; `reach use relay`; re-pair (new origin); cancel.

## 15. Operations

**What runs:**
- 2 relays at 2 providers with reserved IPs.
- 1 control-plane VM (SQLite + Litestream to R2), which also runs the canary.
- Route 53 tenant zone (DNSSEC), and an operator zone that is DNS-only.
- R2 for backups, the Litestream replica and the deny-list mirror.
- Uptime checks on a free tier.
- There is no admin web UI: admin is a CLI over SSH.

**Paging.** Exactly one alert pages: the canary twin fails end-to-end HTTPS through both relays, from at least 2 regions, for 3 consecutive minutes. Everything else is email:
- one relay down (daemons are dual-homed)
- control plane down (entitlements last 35 days; certificates renew directly from Let's Encrypt)
- R2 down
- merchant-of-record webhook retries
- deny-list mirror stale
- cost anomalies

**Toil:**
- A weekly 30-minute review: abuse queue, costs, dependencies.
- A quarterly Litestream restore drill.
- Yearly key rotation.
- Runbooks for relay IP migration, takedown, key rotation, DNS outage and hitting the LE rate limit.

**Shutdown pledge:** 90 days' notice, and backups stay downloadable.

Costs and business planning are kept with the maintainers.

## 16. Launch
The free build ships first, as an open-source release. Founding checkout is targeted for early 2027, with a notice in the CHANGELOG at least 14 days before it opens, and new activations are paced at 40 a week at first (§6.2). Step-up is never cut to make a date: remote dangerous approvals stay refused until it ships.

The dated launch plan is kept with the maintainers.

## 17. Presentation
- **Headline:** "Mirrin is free. Cloud is our uptime."
- **README:**
  - Everything above the fold stays as it is.
  - "Multi-device" becomes "Reach it from anywhere, keep it safe": a table of *Need · Free way · With Mirrin Cloud*.
  - Then a Cloud paragraph of ≤ 6 lines linking `docs/cloud-trust.md`.
  - The roadmap item "TLS or SSH for the multi-device API outside Tailscale" is marked done.
  - The governance section names MavrkAI's role.
- **Pricing page:** Free is the longest column. It carries the "can see / can't / command" table (single source: `internal/api/trust_visibility.md`), a "Leaving" section, and the shutdown pledge. No trial, no countdowns.
- **In-app,** Cloud appears only where the user asked for the capability:
  - the Reach cards (Cloud last)
  - the backup destination picker (Cloud last)
  - the "Add your phone" empty state (Tailscale first)
- **Lapse** is one calm line and never a push.
- **The twin never pitches Cloud.**

## 18. Work packages
Order, sizes and dependencies (full specs are in the structured build list). **Touches uncommitted** (U) means: sequence after the maintainer's in-flight work lands.

| ID | Title | Kind | Depends | U | Size |
|---|---|---|---|---|---|
| WP-00 | [v1] Governance notice and pre-launch applications | docs | — | U | S |
| WP-01 | [v1] Device registry, pairing v2, listener-scoped auth | oss | — | U | L |
| WP-02 | [v1] Approvals persist risk; direct hook | oss | — | | S |
| WP-03 | [v1] HTTPS remote listener, tlsmgr, Tailscale | oss | 01 | | L |
| WP-04 | [v1] Installable PWA, install tickets | oss | 01 | | M |
| WP-05 | [v1] Web Push from the daemon | oss | 01,02,04 | | M |
| WP-06 | [v1] Passkey step-up, focused approval page | oss | 01,02,03,04 | | L |
| WP-07 | [v1] Backup engine, local targets, restore, handover | oss | — | | L |
| WP-08 | [v1] S3 target, shared SigV4 | oss | 07 | | M |
| WP-09 | [v1] Add your phone, Devices, Backup, Trust pages; tray | oss | 01,03,04,05,06,07 | | L |
| WP-10 | [v1] Welcome window | oss | 07 | | M |
| WP-11 | [v1] Release integrity, notarized DMG | oss | — | | M |
| WP-12 | [v1] Relay wire protocol, tunnel client | oss | — | | M |
| WP-13 | [v1] Entitlements, deny lists, httpsig | client | — | | M |
| WP-14 | [v1] mirrin-relay | server | 12,13 | | L |
| WP-15 | [v1] Own certs via TLS-ALPN-01, CT/CAA, playbook, verify | oss | 01,03,05,06,12,14 | | L |
| WP-16 | [v1] Cloud client, fake, contract suite, boundary tests | client | 13 | | M |
| WP-17 | [v1] Control plane core | server | 13,16 | | L |
| WP-18 | [v1] Cloud Reach in the daemon, Reach page | client | 09,15,16,17 | | M |
| WP-19 | [v1] Cloud backup and recovery relink | client | 07,08,16,17,18 | | M |
| WP-20 | [v1] Production, canary, runbooks, abuse | server | 14,17,18,19 | | M |
| WP-21 | [v1] Docs, threat model, pricing, legal | docs | 00,15,18,19 | U | M |
| WP-22 | [later] BYOD on managed relays | client | 17,18 | | S |
| WP-23 | [later] Inbound calls and SMS (BYO Twilio) | oss | 02,03,05,15 | | M |
| WP-24 | [later] Licence-clean wake pipeline, retrained model | oss | — | U | L |
| WP-25 | [later] Cloud wake training | server | 16,17,24 | | M |
| WP-26 | [later] PWA outbox with timestamps | oss | 04,18 | U | S |
| WP-27 | [later] Reproducible builds, trust-claim CI | docs | 11,20 | | M |
| WP-28 | [later] Second CA via EAB | client | 15,17 | | S |

**Dependencies added:**

| Dependency | Licence | Used by |
|---|---|---|
| `github.com/hashicorp/yamux` | MPL-2.0, pure Go | relay |
| `filippo.io/age` | BSD-3 | backup |
| `github.com/go-webauthn/webauthn` | BSD-3 | step-up |
| `rsc.io/qr` | already indirect | QR codes |
| `golang.org/x/crypto/acme` | already indirect | certificates |

## 19. Open questions for the maintainer
Each is listed with a recommendation in the structured output:
- control-plane licence (MIT recommended)
- name and zones
- tenant DNS provider (Route 53 + DNSSEC)
- relay providers and regions
- wake-word data licensing
- notarization secrets
- 12-word phrase
- the deny-list token cap: 256 KiB rather than the 8 KiB token limit (256 KiB recommended)

Questions about prices, payments, legal advice, budget and dates are kept with the maintainers.

## 20. Not building
- Hosting anyone's twin, whether multi-tenant or managed single-tenant. Revisit only with confidential VMs plus attestation.
- Managed phone numbers or a Twilio parent account.
- A TLS-terminating relay, wildcard certificates, relay error pages for tenant names, or any operator-held tenant key.
- DNS-01 TXT writing on the renewal path.
- Phone key slots that unlock restores.
- CNAME BYOD.
- Trials or unpaid handles.
- A push relay.
- Cloud sync of memory. There is one twin with many front ends, and backup is not sync.
- Custom crypto in page JavaScript.
- Accounts, passwords, magic links or an admin web UI.
- Telemetry.
- Auto-updates.
- Notification decision buttons.
- Backing up Chrome Safe Storage or the browser profile.
- Kubernetes.
- Lifetime deals.
- Stored email, or IP logs beyond 72 h.
- Paid wake training before WP-24.

## 21. Review must-fixes and where they're resolved

| Must-fix | Resolution |
|---|---|
| Passkey enrolment bypass | §8 gating (WP-06) |
| Loopback CSRF | §5.1 (WP-01) |
| Phone key-slot restore | Dropped |
| CNAME BYOD | A/AAAA + user CAA (WP-15, WP-22) |
| Operator-written CAA oversold | §6.2 wording; detection plus BYOD |
| CT wildcard gap | §6.5 (WP-15) |
| Managed numbers | Not built |
| Browser, Chrome Safe Storage and session backup | Excluded or opt-in (WP-07) |
| VAPID sub email | Project URL (WP-05) |
| Entitlement-key theft → MITM | Per-handle CAA (WP-17) |
| Offline tokens without revocation | Deny list + gen (WP-13, WP-14) |
| Step-up deferred | v1; remote dangerous refused without a passkey (WP-06) |
| 40-bit pairing codes and global lockout | 256-bit per-offer burn (WP-01) |
| `tailscale serve` Host spoofing | Own listener + tailscale cert (WP-03) |
| Push SSRF | Allowlist (WP-05) |
| RFC 8291 test with Go ≥1.26 | `encryptWith` seam (WP-05) |
| Approvals risk column | WP-02 |
| Lossy bus | Direct hook (WP-02, WP-05) |
| Restore resurrects revoked devices | Snapshot on revoke + device review (WP-07, WP-09) |
| PSL framing | Isolation grounds + separate LE request (WP-00) |
| Notarization, checksums | WP-11 |
| Home-grown WebAuthn | go-webauthn |
| Replay-less signatures | RFC 9421 with nonce (WP-13) |
| ACME account key not backed up | Included (WP-07) |
| Anycast autocert | Per-node control names (WP-14) |
| GTS without EAB | Later with EAB (WP-28) |
| Cloudflare record quotas | Route 53 (WP-17) |
| CHANGELOG governance | WP-00 |
| Wake data licences | WP-24 |
| iOS cookie jar | Install tickets (WP-04) |
| `/start` GET redemption | POST-only in standalone (WP-04) |
| ui.html 401 break | pwa.js 428 shim (WP-04, WP-06) |
| Legacy code prefix | Detect by decoding (WP-01) |
| One-tap origin switch | Origin-continuity warning (§6.1) |
| iOS 18 target | iOS 26/27 matrix (WP-04) |
| Identity vs backup | Shared manifest (WP-07) |
| Outbox timestamps | WP-26 |
| Relay false CT alarms after restore | TLS keys + history + checkpoint (WP-07, WP-15) |

Must-fixes about payments and costs are kept with the maintainers.
