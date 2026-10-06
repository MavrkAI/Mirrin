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
