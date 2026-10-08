# Mirrin Cloud API: `v1`

This is the API between a Mirrin daemon and the Mirrin Cloud control plane.
`docs/cloud-design.md` §10 gives the design; this document is the
specification. It covers linking, the entitlement, the account, backup
storage and recovery.

Code:
- `internal/cloud`: the daemon's client, its link state, the daily refresh
  and the egress ledger. It is inert until the user runs
  `mirrin cloud link`.
- `internal/cloud/cloudtest`: `Fake`, an in-memory control plane that
  implements this document, and `Contract(t, baseURL)`, the suite that the
  fake and the real control plane (`cloud/`, in `--dev` mode) must both pass.
- `internal/entitle`: the entitlement and deny-list tokens.
- `internal/httpsig`: request signatures.

## 1. Principles

- **Nothing until link.** A machine that never ran `mirrin cloud link` has
  no `data/cloud` directory, holds no key, and sends nothing. The default
  config names no control plane. Two tests hold this:
  `internal/cloud/egress_test.go` runs a default twin through a busy
  morning and then two days of fake time (`testing/synctest`, so every
  timer fires), and fails on any request or name lookup for a Cloud or
  relay host; `internal/cloud/boundary_test.go` lets only `cmd/mirrin`,
  `internal/daemon` and `internal/reach` import `internal/cloud`.
- **Public keys and payment status only.** The daemon sends its device's
  public key, the handle it asks for and its ACME account URI. It never
  sends the owner's name, email, messages, memory or anything else about
  the twin. Email and card live only at the merchant of record.
- **Every request is on the record.** Each request the daemon sends is a
  line in `data/cloud/egress.jsonl`, and `mirrin cloud egress` prints them.
- **Nothing is gated here.** The entitlement says what the hosted servers
  (relays, backup storage) will serve. No feature on the user's machine
  checks it.

## 2. Transport

| Item | Value |
|---|---|
| Base | An origin, `https://cloud.mirrin.app` by default (nothing is deployed there yet). `cloud.api` in config, or `mirrin cloud link --api`, names another, such as a self-hosted control plane. A linked machine keeps using the origin it linked with. |
| TLS | Verified against the system roots. Plain `http` is accepted only for a loopback address (tests, dev). |
| Redirects | Never followed. A 3xx is an error. |
| Bodies | JSON, UTF-8, at most 64 KiB in a request. Answers are read up to 64 KiB (1 MiB for `/v1/me`); a longer answer is an error. |
| Timeout | 30 s per request, answer included. |

JSON is parsed strictly: one value and nothing after it, valid UTF-8, no
duplicate member names. The control plane refuses unknown members in a
request (400). The daemon ignores unknown members in an answer, so the
control plane may add fields without a version change.

Times are RFC 3339 in UTC, to the second: `"2026-09-27T03:30:00Z"`.

Keys are unpadded base64url of the 32 bytes (`entitle.EncodeKey`).

## 3. Request signatures

Every route marked *signed* carries an HTTP Message Signature (RFC 9421)
made with the daemon's Ed25519 device key:

| Item | Value |
|---|---|
| Covered | `("@method" "@target-uri" "content-digest")`, in that order |
| `content-digest` | RFC 9530, `sha-256` only, over the body (empty for none) |
| Parameters | `created`, `expires` (= `created` + 300), `nonce` (16 random bytes, base64url), `keyid`, `tag="mirrin-cloud-v1"` |
| `keyid` | `dev:` + base64url of the first 16 bytes of SHA-256 of the public key |
| Label | `sig1` |

The control plane:
- rebuilds `@target-uri` from its own origin (`httpsig.VerifyFor`), so a
  request signed for another server is refused;
- allows 300 s of clock skew either way;
- keeps each `(keyid, nonce)` until the signature's window has passed
  (10 minutes at most) and refuses it again with `409 replay`;
- looks keys up among registered devices and pending links. On
  `POST /v1/link/start` alone, the key may be new: it is the `device_pub`
  in the body, and the signature proves the daemon holds it.

## 4. Errors

Every answer outside 2xx is a JSON object:

```json
{"error":"<code>","message":"<a sentence for the user>"}
```

The daemon shows `message` after removing every character that is not
graphic (C0 and C1 controls, bidi overrides and other format characters,
line separators, private use) and cutting it to 300 bytes. Some errors add
members, listed with their route.

| Status | `error` | Meaning |
|---|---|---|
| 400 | `bad_request`, `bad_handle`, `bad_acme_account` | The request is malformed or breaks a rule |
| 401 | `unauthorized` | Unsigned, a bad or stale signature, an unknown key, or signed for another host |
| 402 | `lapsed` | Payment has lapsed (refresh, backup writes) |
| 403 | `revoked`, `deleted` | The key was revoked (unlinked, recovered from elsewhere, suspended), or the account is gone |
| 403 | `no_namespace`, `not_included` | Backups aren't set up for this account, or its plan has none |
| 404 | `not_found` | No such route or link, or not this device's link |
| 409 | `replay` | The signature's nonce was already used |
| 409 | `superseded`, `handle_taken`, `already_linked`, `not_deleting`, `payment_refused`, `exists`, `not_uploaded`, `size_mismatch`, `namespace_taken`, `namespace_bound` | See the route |
| 413 | `over_quota` | A backup would go over the account's space |
| 413 | `too_large` | The body is over 64 KiB |
| 429 | `rate_limited` | Slow down; `retry_after` (seconds) says for how long |
| 503 | `busy` | Try again shortly |

Because 409 carries several meanings, the daemon reads `error`, not only
the status. It does the same for 402 and 403: only `402 lapsed`, and only
`403 revoked` or `403 deleted`, say anything about this machine. Any other
402 or 403, such as a proxy's HTML page, is a passing failure: nothing is
recorded and the request is retried later.

## 5. Routes

| Route | Auth | Section |
|---|---|---|
| `GET /v1/version` | none | 5.1 |
| `GET /v1/keys` | none | 5.1 |
| `GET /v1/denylist` | none | 5.1 |
| `POST /v1/link/start` | signed by the key it names | 5.2 |
| `GET /v1/link/{id}` | signed by the link's key | 5.2 |
| `POST /v1/entitlement/refresh` | signed | 5.3 |
| `PUT /v1/acme-account` | signed | 5.4 |
| `GET /v1/me` | signed | 5.5 |
| `POST /v1/billing/portal` | signed | 5.5 |
| `DELETE /v1/device` | signed | 5.6 |
| `DELETE /v1/account` | signed | 5.6 |
| `POST /v1/account/restore` | signed | 5.6 |
| `POST /v1/backup/namespaces` | signed, and by the recovery key | 5.7 |
| `POST /v1/backup/presign` | signed | 5.7 |
| `GET /v1/backup/objects` | signed | 5.7 |
| `POST /v1/backup/commit` | signed | 5.7 |
| `POST /v1/recover` | signed by the key it names, and by the recovery key | 5.8 |
| `POST /v1/billing/webhook` | the merchant of record's signature | not for daemons |

A key on the deny list is refused everywhere with `403 revoked` (or
`403 deleted`), even while its device is otherwise registered. A device
whose handle another machine now holds at a later generation (see
`409 superseded` in 5.3) may still refresh, to hear that, and unlink
itself; `PUT /v1/acme-account`, `GET /v1/me`, `POST /v1/billing/portal`,
`DELETE /v1/account` and `POST /v1/account/restore` answer it with the
same `409 superseded`, as do the backup routes. A key a recovery put on the
deny list (`why` `recovered`) is the one exception to the first rule: while
its device row stands a generation behind its handle, the control plane
answers it as a superseded device (a refresh hears `409 superseded`), so
that machine stands by rather than carrying on unlinked. Relays refuse it
like any denied key.

### 5.1 Public

`GET /v1/version` → `200 {"api":"v1","server":"<name and version>"}`

`GET /v1/keys` → `200`:

```json
{"entitlement":{"ent-2026a":"<key>","ent-2027a":"<key>"},
 "denylist":{"dl-2026a":"<key>","dl-2027a":"<key>"}}
```

The current and next public key for each purpose. Kids start `ent-` and
`dl-`; no key appears under both. These are the keys compiled into
`internal/entitle/keys.go`; the daemon trusts the compiled set, not this
answer, which is for relays and people checking.

`GET /v1/denylist` → `200 text/plain`: one deny-list token
(`entitle.SignDenyList`, PASETO v4.public under a `dl-*` key) and a newline.
Relays poll it every 45 s. A key or handle is listed from the moment it is
revoked until every entitlement it could hold has expired (35 days).

### 5.2 Linking

```
POST /v1/link/start   {"device_pub":"<key>","handle":"ember-otter-42","acme_account":"https://…/acct/123"}
200                   {"id":"lk_…","checkout_url":"https://…"}
```

- `device_pub` is required and must be the key that signed the request.
  The signature is checked against it, which proves the daemon holds it.
- `handle` is optional; without it the control plane picks one at
  random. A handle is 3–32 characters of `a-z 0-9 -`, with no hyphen at
  either end and no `--`. Reserved and confusable names are refused.
  Handles are never reassigned, so a taken one is refused for good.
- `acme_account` is optional: the ACME account URI that the handle's CAA
  record will name. It is an `https` URL of at most 512 bytes with no user
  info, query or fragment, and none of `" ; \` or whitespace. Without it
  the handle's CAA forbids all issuance until `PUT /v1/acme-account`.
- `id` is 1–64 characters of `A-Z a-z 0-9 _ -`. It goes in a URL path
  unescaped.
- `checkout_url` is `https` (or `http` from a loopback control plane),
  at most 2048 bytes of printable ASCII (percent-encode anything else),
  with no user info. The daemon prints it, opens it in the user's browser,
  and never fetches it itself.
- A pending link holds its handle and lasts one hour.

| Error | When |
|---|---|
| 400 `bad_request` | The body is not `{device_pub, handle?, acme_account?}` |
| 401 `unauthorized` | `device_pub` is missing, malformed or not the key that signed: the signature is checked against it |
| 400 `bad_handle` | The handle breaks a rule or is reserved |
| 400 `bad_acme_account` | As it says |
| 409 `handle_taken` | Held by an account, or by another pending link |
| 409 `already_linked` | This key is already a linked device |

```
GET /v1/link/{id}
200 {"status":"pending"}
200 {"status":"expired"}
200 {"status":"active","entitlement":"v4.public.…"}
```

The daemon polls every 2 s while the user pays. Only the key that started
the link may poll it; any other gets `404`. Once active, every poll
answers with a freshly signed entitlement, or with the refresh errors of
5.3.

A payment the control plane will not attach to this machine answers
`409 payment_refused`, for good: the payer is a customer who already has
an account (the merchant of record matches customers by email, so
attaching would let anyone take an account's handle), or the key may not
link. The payment is cancelled and refunded at once, and `message` says so.

Before storing an entitlement the daemon verifies it against the keys
compiled into the build, checks its times, checks that `cnf` is its own
device key (constant time), and checks that `handle` keeps the handle
rules and each of `hosts` is a lowercase DNS name. A token that fails any
check is refused and nothing is stored. A build with no keys compiled in
refuses to start a checkout at all.

`mirrin cloud link` records the pending link in
`data/cloud/pending-link.json` and resumes it when run again, so a
checkout paid after the terminal closed is not paid twice.

### 5.3 The entitlement

The entitlement is `entitle.Claims`, as `docs/cloud-design.md` §10 lays
out, signed under an `ent-*` key. `exp` is the earlier of 35 days after
issue and 7 days after `paid_through`.

```
POST /v1/entitlement/refresh     (no body)
200 {"entitlement":"v4.public.…"}
402 {"error":"lapsed","message":"…","paid_through":"…"}
403 {"error":"revoked","message":"…"}
409 {"error":"superseded","message":"…","gen":4,"at":"…"}
```

- **200.** The daemon stores the new token if it passes the checks of 5.2,
  is for the same handle, is not at a lower generation than the one it
  holds, and was not issued (`iat`) before the token it holds. The
  control plane may answer with the token it last issued; it may not
  answer with an older one.
- **402 `lapsed`.** The stored token stays in use until it expires.
- **403 `revoked` or `deleted`.** The daemon records the revocation; it is
  Expired from then on and stops refreshing.
- **409 `superseded`.** Another machine holds the handle at generation
  `gen` since `at` (a recovery, WP-19). If `gen` is above the generation
  this machine holds, the daemon records it, stands by, and stops
  refreshing. Otherwise it is a passing failure.

A linked daemon refreshes about once a day: 20 to 28 hours (uniformly
random) after the control plane last answered a link or refresh with 200
or `402 lapsed`, timed by the daemon's own clock, never by the token's
`iat`, and never sooner than an hour after such an answer. A clock set
back never delays it past 28 hours. A failure is retried after a random
wait under a ceiling that doubles from 5 minutes to 6 hours. It is the
only request the daemon makes on its own, and only on a linked machine.
The control plane can be down for 35 days before any entitlement lapses.

### 5.4 ACME account

```
PUT /v1/acme-account   {"uri":"https://acme-v02.api.letsencrypt.org/acme/acct/123"}
204
```

Rewrites the handle's CAA record to name this ACME account, with
`validationmethods=tls-alpn-01`. Same rules as `acme_account` in 5.2;
`400 bad_acme_account` otherwise.

### 5.5 The account

`GET /v1/me` → `200`: every field the control plane stores that is linked
to this account, and the email, which is fetched from the merchant of
record for this answer and never stored. At least:

```json
{"account":{"id":"acct_…","billing_customer":"ctm_…","plan":"cloud","status":"active|lapsed|deleting",
            "paid_through":"…","created":"…","acme_account":"https://…"},
 "email":"…",
 "devices":[{"pub":"<key>","created":"…","last_seen_day":"2026-09-27","revoked":false}],
 "handles":[{"name":"ember-otter-42","gen":1,"byod":false,"created":"…","released":null}],
 "links":[…], "namespaces":[…], "events":[{"kind":"link","at":"…"}], "deletion":null}
```

The control plane's own test (WP-17) fails if a stored column is missing
here. There is no email column and no IP address anywhere.

`POST /v1/billing/portal` → `200 {"url":"https://…"}`: the merchant of
record's page for receipts, the card and cancelling. Same rules as
`checkout_url`.

### 5.6 Leaving

```
DELETE /v1/device          → 204
DELETE /v1/account         → 202 {"delete_at":"…"}
POST /v1/account/restore   → 204 | 409 not_deleting
```

- **`DELETE /v1/device`** revokes the calling key and puts it on the deny
  list. The account, the handle and the subscription are untouched. The
  daemon then deletes its key, entitlement and link record, and keeps the
  egress ledger. A `403 revoked` or `403 deleted` counts as done. A 401
  does not, since a clock more than 5 minutes out looks the same as an
  unknown key; the daemon says so and keeps the link.
  `mirrin cloud unlink --local` does the local half alone, for when the
  control plane cannot be reached or keeps refusing.
- **`DELETE /v1/account`** schedules deletion 7 days on. Until then
  everything keeps working, and `POST /v1/account/restore`, signed by a
  key of the account, cancels it. Then the control plane removes the
  account's rows, DNS records and backup objects, and deny-lists its keys;
  from then on every route answers `403 deleted`. The handle is never
  reassigned.

### 5.7 Backup storage

Backups go from the daemon straight to storage, at URLs the control plane
presigns for 15 minutes. They are ciphertext that only the owner's 12
words open (`docs/backup-format.md`); the control plane sees object names,
which carry a time and 8 random hex digits, and sizes. Names match
`^(snap|handover)-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}\.age$`, and an object is
always kept at `ns/<ns>/<name>`, whatever the request says.

The words make a recovery key (Ed25519), and `ns` is the first 26
characters of the lower-case base32 of its public key's SHA-256
(`entitle.Namespace`). Signatures by the recovery key are unpadded
base64url over these messages (`entitle.BindMessage`,
`entitle.RecoverMessage`), lines joined by `\n`:

```
antbot-backup-namespace-v1 / <ns> / <recovery_pub> / <device_pub>
antbot-recover-v1 / <ns> / <device_pub> / <acme_account> / <ts>
```

```
POST /v1/backup/namespaces  {"ns":"…","recovery_pub":"<key>","sig_recovery":"…"}
204
400 bad_request       ns isn't recovery_pub's
401 unauthorized      the signature isn't the recovery key's over this device
409 namespace_taken   another account holds these words' backups
409 namespace_bound   this account keeps backups made with other words

POST /v1/backup/presign     {"op":"put|get|delete","name":"snap-…age","size":12345}
200 {"url":"https://…","method":"PUT","headers":{"Content-Length":"12345"},"expires":"…"}
402 lapsed            put once payment has lapsed; get 90 days after that
403 no_namespace      no namespace is bound yet
404 not_found         get of an object that isn't stored
409 exists            put of a name stored, or presigned at another size
409 removed           put of a name removed before (names are never reused)
413 over_quota        put past the quota, with {"quota","bytes","held"}

GET /v1/backup/objects
200 {"ns":"…","objects":[{"name":"…","size":…,"created":"…"}],"bytes":…,"held":…,"quota":…}

POST /v1/backup/commit      {"name":"…","size":12345}
200 {"bytes":…,"pruned":["snap-…"]}
409 removed           the name was removed, or given up on
409 not_uploaded      storage holds nothing under that name, or it wasn't presigned
409 size_mismatch     not the size presigned; it has been removed and its name is not used again
```

- **Quota** is enforced at presign: a put holds its size from then until it
  is committed, deleted, or given up on by the sweep 6 hours after its URL
  ran out (its key is then deleted). `bytes` is what is committed, `held`
  what pending uploads and removed objects not yet gone from storage hold
  besides. Presigning the same put again holds it once. A put's URL signs
  its `Content-Length`, so storage takes no other size.
- **Commit** counts an object only after a HEAD finds exactly `size` bytes
  under its key. Committing again with the same size is a no-op. Then
  retention runs: the newest snapshot of each of the last 7 days, then of 4
  weeks, then of 6 months, the one just committed always, and never a
  handover marker. `pruned` names what went.
- **Delete** is done by the control plane itself, which holds the storage
  credentials, before it answers; the presigned DELETE it hands back finds
  nothing left. A committed object stops counting at once. An upload never
  committed holds its size until its URL could no longer be putting it
  back, when its key is deleted again. A name removed, by a delete,
  retention or a size mismatch, is never presigned for a put again.
- **Lapsed:** put and commit answer `402 lapsed`; presign get and the list
  keep working until 90 days after `paid_through`.
- Deleting the account removes every object under its namespaces.

The daemon records each storage request in the egress ledger too: method,
storage host, path and sizes, never the query, which holds the signature.

### 5.8 Recovery

```
POST /v1/recover  {"ns":"…","device_pub":"<key>","acme_account":"https://…"|"","ts":"…","sig_recovery":"…"}
200 {"entitlement":"v4.public.…","handle":"ember-otter-42","gen":3}
200 {"handle":"…","gen":3,"paid_through":"…"}    payment has lapsed: no entitlement
400 bad_acme_account
401 unauthorized      words that bound no namespace here, a signature they didn't make, ts more than 300 s out, or device_pub not the signing key
403 revoked|deleted   the new key is on the deny list, or the account is gone
409 already_linked    the new key is another account's, or an older device's
```

Signed like `link/start`, by the new machine's device key, which the body
names. In one transaction the control plane:
- raises the handle's generation, so every other device hears
  `409 superseded`;
- puts every other device key on the deny list (`why` `recovered`), so
  relays drop them within one poll;
- adds the new key as a device at the new generation;
- sets the account's ACME account to `acme_account`, and rewrites the
  handle's CAA record for it (an empty one lets no CA issue until the new
  machine names its account with `PUT /v1/acme-account`).

The same request again from the same key moves nothing and answers the
same way. `mirrin restore --from cloud` sends it before listing the
backups, with a new device key kept beside the home until the restored twin
is in place.

## 6. On the daemon

`data/cloud`, created by `mirrin cloud link` and by nothing else, mode
0700; every file 0600:

| File | Holds |
|---|---|
| `device.key` | The Ed25519 device key, PKCS #8 PEM. Made at link, never backed up, refused if others can read it |
| `link.json` | The control plane origin, handle, generation, when linked, when the control plane last answered (by the daemon's clock), and what it last said: a revocation, a supersede, a scheduled deletion |
| `entitlement.paseto` | The current entitlement |
| `pending-link.json` | A checkout started and not finished |
| `egress.jsonl` | The egress ledger |

A machine is linked when both `link.json` and `device.key` exist. A
`link.json` left without its key (an unlink in one process racing a
refresh in another) counts as no link, so `mirrin cloud link` can start
again.

States (`cloud.State.Current`):

| State | When | What works |
|---|---|---|
| None | Never linked | Everything local; nothing is sent |
| Active | Before `paid_through` | Relays and backup storage serve this machine |
| Grace | From `paid_through` until `exp` | The same; the tray shows one calm line |
| Expired | From `exp`, or once revoked | Relays refuse; backup reads work for 90 days |
| Superseded | Another machine holds the handle | This machine stands by |

`State.Has(now, feature)` is true only while Active or in Grace, and only
for features the entitlement grants.

### The egress ledger

One JSON object per line, per request:

```json
{"at":"2026-09-27T03:31:07Z","method":"POST","host":"cloud.mirrin.app","path":"/v1/entitlement/refresh","req_bytes":0,"resp_bytes":612,"status":200}
```

- `req_bytes` and `resp_bytes` are body sizes. `status` is 0 when no
  answer came back.
- There is no body, query, header, key or token in it.
- The ledger is opened before a request is sent, so a machine that cannot
  write its ledger sends nothing. At 1 MiB it moves to `egress.jsonl.1`.
- `mirrin cloud egress` prints it; `unlink` keeps it.

## 7. The contract suite

`cloudtest.Contract(t, baseURL)` checks a control plane from the outside,
the way the daemon sees it. The server must run in dev mode. Each run
makes new keys and new handles, so a long-lived dev server can be checked
again and again.

Dev mode adds routes that stand in for the merchant of record, a recovery
on another machine and an admin, so every answer the daemon acts on can be
produced. They take no signature, and a production control plane answers
404 to all of them.

| Route | Does |
|---|---|
| `GET <checkout_url>` | Pays for that link |
| `POST /dev/paid-through {"handle":"…","paid_through":"…"}` → 204 | Moves the end of the handle's paid period; a time past lapses it |
| `POST /dev/supersede {"handle":"…"}` → `200 {"gen":N}` | Raises the handle's generation, as a recovery elsewhere would |
| `POST /dev/revoke {"device_pub":"<key>"}` → 204 | Revokes the device key and puts it on the deny list |

An unknown handle or key gets `404 not_found`; a body that is not what the
route takes gets `400 bad_request`.

The suite checks:

- `/v1/version`, `/v1/keys` (kids by purpose, disjoint) and a deny list
  that verifies;
- every signed route refuses an unsigned request with 401 and an error
  body;
- a link: pending, invisible to another device (404), holding its handle
  (409 `handle_taken`), then active once paid, with an entitlement bound
  to the device key, for the handle, at generation 1 or more, lasting at
  most 35 days;
- a replayed request (409 `replay`), one signed 15 minutes ago (401), one
  sent to another path than signed (401), one whose body changed (401);
- `link/start` signed by a key other than `device_pub`, from a new key and
  from one with a pending link, or naming none (401); with a member it
  does not take (400 `bad_request`); from a linked device (409
  `already_linked`); a link that does not exist (404 `not_found`); a body
  over 64 KiB (413 `too_large`);
- a refresh; handle and ACME account rules (400);
- a second machine's refresh through a lapse (`402 lapsed` with
  `paid_through`), payment again (200), a supersede (`409 superseded`
  with `gen` and `at`), and a revocation (`403 revoked`, on refresh and on
  its link), after which the deny list names its key;
- `/v1/me` shows the device's key and handle; the billing portal answers;
- delete, restore, and restoring again (409 `not_deleting`);
- unlink: the key is refused afterwards and is on the deny list.

`internal/cloud` runs it against `Fake`; `cloud/` runs it against its own
`--dev` server.
