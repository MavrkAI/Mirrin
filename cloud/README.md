# mirrin-cloud

The Mirrin Cloud control plane: the smallest server that turns a payment
into a handle, its DNS records and a signed entitlement. It is a nested Go
module (`github.com/MavrkAI/Mirrin/cloud`), MIT like the rest of Mirrin, so
anyone can read what the paid service does and run their own.

`docs/cloud-design.md` §10 is the design and `docs/cloud-api.md` the wire
contract. The daemon's side is `internal/cloud`, which stays inert until the
user runs `mirrin cloud link`.

## What it keeps

Public keys, handles and billing status. Nothing else.

- **No accounts or passwords.** A machine is known by its Ed25519 device key,
  and every request it sends is signed with it (RFC 9421, `internal/httpsig`).
- **No email.** The email and card live at the merchant of record (Paddle).
  `GET /v1/me` fetches the email for that one answer and never stores it.
- **No IP addresses.** No table has an address column, request logs name
  the route and status only, and net/http's own error lines are logged with
  addresses removed. Rate limits hold client addresses in memory only (see
  below).
- **Everything is visible.** `GET /v1/me` shows every column of every table
  that names the account, read from the schema itself, and
  `me_coverage_test.go` fails if one is missing.

The tables are in `internal/store/schema.sql`: accounts, devices, handles,
links (checkouts), the deny list, events (a kind and a time, never content)
and the ids of billing events already applied.

## Run it on a laptop

```
cd cloud
go run ./cmd/mirrin-cloud serve --dev
```

`--dev` runs on a fake merchant of record (its checkout page pays when you
open it, through the real webhook path), fake DNS, a SQLite file in a new
temporary directory (`--data DIR` keeps one between runs), and the public
development signing keys. It listens on a loopback address only, and
prints it. Link a daemon built with the development keys to it:

```
go build -tags mirrin_devkeys -o /tmp/mirrin ./cmd/mirrin
MIRRIN_HOME=$(mktemp -d) /tmp/mirrin cloud link --api http://127.0.0.1:8787
```

`make cloud-dev` does the first step from the repository root. The dev
server also answers the `/dev/` routes the contract suite uses to stand in
for billing webhooks, a recovery elsewhere and an admin
(`docs/cloud-api.md` §7). A production server has none of them.

Never expose a dev server: anyone can derive the development keys.

## Run it for real

`cloud.example.yaml` is a complete config. Secrets never go in it; it names
the environment variables that hold them.

- **Signing keys.** Entitlements and deny lists have separate keys (`ent-*`
  and `dl-*`), each a `<kid>.pem` file in `keys.dir`, readable only by the
  service (systemd encrypted credentials are the intent). Make them with
  `mirrin-cloud keys generate --dir DIR`, and the next one a year later with
  `mirrin-cloud keys rotate --dir DIR --purpose ent|dl`. Every key in the
  directory is published at `/v1/keys`; `keys public` prints the lines for
  `internal/entitle/keys.go`, where a key must ship a release before it
  signs.
- **Billing.** Paddle: an API key, the notification secret, and a price per
  plan. Point Paddle's notification destination at `/v1/billing/webhook`.
  Webhooks are checked with Paddle's signature scheme (tested against
  Paddle's own SDK vector), a repeated event id is a no-op, and an event
  older than the last one applied changes nothing.
- **Behind a proxy.** `listen` serves plain HTTP unless `cert_file` and
  `key_file` are set. Behind a TLS proxy, name it in `trusted_proxies`: the
  client's address is then read from the proxy's `X-Forwarded-For`, and
  only from it. Without that every client would share one rate limit, so
  a production config serving plain HTTP must set it.
- **DNS.** Route 53, signed with `internal/sigv4`. Per handle it writes A and
  AAAA for the relays and a CAA set that lets only the handle's own Let's
  Encrypt account issue, by TLS-ALPN-01, and never a wildcard. The IAM
  policy needs only `route53:ChangeResourceRecordSets` and
  `route53:ListResourceRecordSets` on the tenant zone.

Admin is this CLI over SSH, working on the same database:

```
mirrin-cloud admin show HANDLE --config cloud.yaml
mirrin-cloud admin deny handle HANDLE --why abuse --config cloud.yaml
mirrin-cloud admin deny key DEVICE_KEY --why lost --config cloud.yaml
```

## Rules it keeps

- A handle is 3 to 32 of `a-z 0-9 -`, with no hyphen at either end and no
  `--`. Reserved names, names that look like them (`adm1n`), names holding a
  brand however spelled (`g00gle-fan`), and names that look like an existing
  handle are refused. A handle is never reassigned, even after its account
  is deleted.
- An entitlement expires at the earlier of 35 days after issue and 7 days
  after the paid period ends, so the control plane can be down for a month
  without anyone noticing.
- `DELETE /v1/account` waits seven days. Then the subscription is cancelled
  at the merchant of record, the device keys and handle go on the deny list,
  the DNS records are removed and the rows are deleted; the handle's name
  stays taken.
- A customer who already has an account cannot attach another machine by
  paying again: the merchant of record matches customers by email, so that
  would let anyone who can check out under an owner's email take the
  handle. That payment, and any other that attaches to nothing (a link
  forgotten after 30 days, a denied key, a machine already linked, a late
  payment past the weekly cap), is cancelled and refunded at once, and the
  machine's link poll answers `409 payment_refused`.
- A device key on the deny list can do nothing, and a machine superseded
  at its handle by a later generation cannot read or change the account.
- Limits, all in memory: `link/start` per client (an IPv4 address or an
  IPv6 /48) once its signature checks out, and from everyone together; the
  link routes more loosely before signatures are checked; the signed
  device routes per device key. Keys that are not a device's keep their
  nonces apart, so strangers minting keys cannot crowd out linked machines.
  New handles are capped per week, counting checkouts still open.

## Tests

```
cd cloud && go test ./...
```

`internal/server/contract_test.go` and `cmd/mirrin-cloud/main_test.go` run
`cloudtest.Contract`, the daemon's own contract suite, against this server
in `--dev` mode with a real SQLite store. No test contacts Paddle, AWS, Let's
Encrypt or any MavrkAI host.

## Backup storage and recovery

`/v1/backup/*` and `/v1/recover` (docs/cloud-api.md §5.7, §5.8) keep
backups in `storage.Provider`: R2 in production (`storage.r2` in
cloud.yaml, presigned with `internal/sigv4`), and in `--dev` a folder under
the data directory that the server serves itself at `/storage/`, with
HMAC-signed URLs that also hold each PUT to the size allowed. Objects are
ciphertext under `ns/<namespace>/`; the server sees only names and sizes,
checks each upload with a HEAD before counting it, enforces the quota at
presign, and prunes to 7 daily, 4 weekly and 6 monthly snapshots, never the
one just stored. `internal/server/backup_test.go` covers binding, presign,
quota, lapse, retention, recovery and deletion;
`internal/cloud/backuptarget` in the main module runs the backup target
conformance suite and a whole recovery against `serve --dev`.
