# What Mirrin Cloud can and can't see

Mirrin Cloud is the optional paid service: an address that reaches your twin
from any network, and space for encrypted backups. **It is planned and not
for sale.** Its code is in this repository (the relay in `cmd/mirrin-relay`,
the control plane in `cloud/`, the daemon's side in `internal/cloud`), and
this page describes what that code does. Nothing here applies to a twin that
was never linked: until you run `mirrin cloud link`, your machine sends
Mirrin Cloud nothing at all.

This page is the one place the list is kept. The pricing page on the website
and [docs/cloud.md](cloud.md) point here; if they ever disagree, this page
wins and they are wrong. The threat model that sits behind it is
[docs/threat-model.md](threat-model.md).

Every "can't" below comes with the reason it can't (the mechanism, not a
promise) and a command you can run to check it. Commands that start with
`mirrin` run on the computer your twin lives on.

<!-- visibility:start -->

## What we can see

| What | Held by | For how long | See it yourself |
|---|---|---|---|
| Your account number, plan, billing status and paid-through date, and the payment provider's customer number | The control plane | While you're subscribed; removed 7 days after `mirrin cloud delete-account` | `mirrin cloud me` |
| Your handle (your address) | The control plane, public DNS and public certificate logs | For good in the logs; the name is never given to anyone else | `mirrin cloud status` |
| Your machine's public keys: its device key, the recovery key your 12 words make, and the certificate account its address is pinned to | The control plane, public DNS (the CAA record) | While you're subscribed | `mirrin cloud me`, `mirrin reach fingerprint` |
| The day your machine last checked in, and when it refreshes its pass (about daily) | The control plane | While you're subscribed | `mirrin cloud me`, `mirrin cloud egress` |
| Your home IP address, and when your machine connects to the relays | The relays | While the tunnel is up; logs keep only the /24 or /48 network | `mirrin reach status` names the relays it connects to |
| For each connection to your address: the network it came from (/24 or /48), your address name, bytes and duration | The relays | 72 hours in memory, then hourly totals for 30 days | The relay's source: `internal/relay/server/connlog.go` |
| How many backups you keep, their sizes, and when each was made | The control plane and the storage provider | Until the backup is pruned or you delete the account | `mirrin backup list`, `mirrin cloud me` |
| Your email address, card and billing country | The payment provider (merchant of record), never us | Under the provider's own policy | `mirrin cloud billing` |

## What we can't see

| What | Why we can't | Check it with |
|---|---|---|
| Anything inside a connection to your twin: messages, the screen, approvals, cookies, memory, voice | TLS ends on your machine, with a key only your machine holds. The relay reads the name on the envelope (the SNI) and passes the still-encrypted bytes through; it has no code path that loads a certificate for your name, and a test proves it sees only ciphertext | `mirrin reach verify`, `mirrin reach fingerprint`, and the `openssl` check below |
| A certificate for your address, obtained quietly | Your address's CAA record names your machine's own Let's Encrypt account and the TLS-ALPN-01 method, so a relay, a stolen signing key or a hijacked route can't get one. We can rewrite that record, so on the shared zone a certificate we (or our DNS provider) caused would be detected, not prevented: every certificate lands in public logs, and your twin checks them every 6 hours | `mirrin reach verify`, `mirrin reach alarm`, and your address on crt.sh |
| What's in your backups, or the names of the files inside them | Each backup is encrypted on your machine with age, to a key made from your 12 words. The words are never stored, not even on your machine. Objects are named by time and a random tag only | `mirrin backup key --age`, then stock `age -d`; `mirrin backup verify` |
| Your 12 words | They are shown once, on your screen, for you to write down. Only public keys made from them are kept | `mirrin backup status` shows only a Kit ID; `mirrin backup verify` asks you to type the words, because they appear nowhere on disk (a test scans for them) |
| Your passwords, API keys and signed-in browser | They never leave your machine. Backups leave out the browser profile altogether, and the rest travels only inside the encrypted backup | Decrypt a backup with `mirrin backup key --age` and `age -d`, then list it with `tar -tz`: there is no browser profile. The Trust page lists everything that leaves |
| Your notifications | Your machine encrypts them and sends them straight to Apple's, Google's, Mozilla's or Microsoft's push service. No server of ours is in the path | `mirrin cloud egress` shows no notification ever sent to Cloud; the Trust page names the push service as their only destination |
| Your email address | The control plane has no email column. It fetches the address from the payment provider for one `mirrin cloud me` answer and doesn't keep it | `mirrin cloud me` (its schema-coverage test fails if a stored field is missing) |
| Your IP address, in our database | No table has an address column. Rate limits hold addresses in memory only | `mirrin cloud me` |
| What your twin does, or which model you use | None of it goes through us. The only requests your machine sends to Cloud are signed calls carrying public keys, your handle and backup sizes | `mirrin cloud egress` lists every request sent: method, path and size, never the body |
| Anything at all from a twin that never linked | The Cloud client is inert until `mirrin cloud link`. Only three packages may import it (a test enforces that), and a test runs a default twin for two days of simulated time and sees no request to a Cloud or relay host | `mirrin cloud status` ("sends nothing") and `mirrin cloud egress` (empty) |

<!-- visibility:end -->

## Checking the certificate from outside

From any computer, ask your address which key it presents. It should print
the "Current key" from `mirrin reach fingerprint` (or the "Next key", just
after a renewal):

```sh
h=your-address.example
openssl s_client -connect "$h:443" -servername "$h" </dev/null 2>/dev/null \
  | openssl x509 -pubkey -noout | openssl pkey -pubin -outform der \
  | openssl dgst -sha256 -binary | openssl base64 | tr '+/' '-_' | tr -d '='
```

The Trust page (menu bar → **Reach from anywhere…** → Trust) shows the same
fingerprint, the certificate's SHA-256 as a phone's browser shows it, and
what the certificate-log watch last found.

## The parts that aren't ours

- **Push services** (Apple, Google, Mozilla, Microsoft) see that a
  notification was sent, its size and its timing. Your machine sends it, with
  or without Cloud.
- **Let's Encrypt and the certificate logs** see your address and its public
  key. Every public certificate is logged; that is what lets your twin watch
  for one it didn't ask for.
- **The payment provider** (the merchant of record) sees your email and card,
  as any shop's payment provider does. We never receive the card.
- **The storage provider** holds the encrypted backup objects.
- **Your model provider, chat apps and Google** are between you and them.
  Cloud never touches them.

## What only you can do

- Read your backups. Nobody else holds the 12 words.
- Answer a dangerous request from a phone. It needs your passkey (Face ID, a
  fingerprint or the phone's PIN), made for that one request.
- Clear a certificate alarm: `mirrin reach alarm clear` on your computer, or
  `/alarm clear` in your own chat.

## If you'd rather trust nobody

Every paid part has a free route that needs no MavrkAI server:
[docs/reach.md](reach.md) (Tailscale, your own certificate, or your own
relay) and [docs/backup.md](backup.md) (a folder, iCloud Drive or any S3
bucket). With your own relay and your own domain, the CAA pin is yours too,
so a mis-issued certificate is prevented rather than detected.
