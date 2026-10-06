# Mirrin Cloud

**Mirrin is free. Cloud is our uptime.**

Mirrin Cloud is an optional paid service from MavrkAI that rents you two
things a server is genuinely good for: an address that reaches your twin from
any network, and off-site space for encrypted backups. It never holds your
twin. The brain, the memory, your passwords and the signed-in browser stay
whole on your machine.

## Status: planned, not for sale

- The code is in this repository: the relay (`cmd/mirrin-relay`), the
  control plane (`cloud/`) and the daemon's side (`internal/cloud`, with
  `mirrin cloud`, `mirrin reach use cloud` and `mirrin backup target cloud`).
- There is nothing to buy. This version of Mirrin trusts no Cloud signing
  key, so `mirrin cloud link` stops before any checkout and says why.
- Founding checkout is targeted for early 2027. The CHANGELOG will carry a
  notice at least 14 days before it opens, and the terms, acceptable use
  policy and privacy policy will be published before then (drafts are on the
  website, marked for legal review).

## Free first

Every paid convenience has a free, documented route, and the free route
ships first. Nothing Mirrin does for free today will ever need a
subscription, and your twin never mentions Cloud: it appears only where you
ask for the capability (the Reach page, the backup place picker, Add your
phone), listed last.

| Need | Free way | With Mirrin Cloud (planned) |
|---|---|---|
| Reach your twin from any network | Tailscale, your own certificate, or your own relay and domain ([reach.md](reach.md)) | An address on two relays, its certificate pinned to your machine |
| Off-site backup | A folder, iCloud Drive or any S3 bucket ([backup.md](backup.md)) | 20 GB with nothing to set up; 90 days to fetch after cancelling |
| Twilio calls to your twin | Any public address you run | Your Cloud address |
| Lock-screen approvals, Face ID, the phone app | Free, on every route | The same |

## What we'd see

In short: your handle, your machine's public keys, when it connects and
from which network, backup sizes and times, and billing status. Never
anything inside the connection, inside a backup, or your 12 words. The full
list, with the reason behind each "can't" and a command to check it, is
[cloud-trust.md](cloud-trust.md).

## Planned pricing

| | Free | Cloud | Supporter |
|---|---|---|---|
| Price | $0, no account | $6 a month or $60 a year; founding $48 a year for the first 500, kept while subscribed | $20 a month |
| Support | Community | Email, within 2 business days | Next business day, best effort |

No trial, a 30-day refund, and no lifetime deals. Fair use: about 100 GB
relayed a month (a warning first) and 20 Mbit/s per connection. Prices may
change before checkout opens.

## How it would work

There's no sign-up, login or password. When you link, your machine makes its
own device key and sends only public keys; the checkout opens in your
browser at the merchant of record, which is the only place your email and
card live. After payment your machine gets a signed pass that it checks
itself, valid for up to 35 days and refreshed about once a day, so a quiet
day at our end goes unnoticed.

```sh
mirrin reach use cloud            # link (the checkout opens in your browser) and reach through Cloud
mirrin backup target cloud        # keep backups with Cloud, on a linked machine
mirrin cloud status               # where this machine stands (sends nothing)
mirrin cloud me                   # every field Cloud stores about you
mirrin cloud egress               # every request this machine has sent Cloud (sends nothing)
mirrin cloud billing              # receipts, card, cancelling
mirrin cloud unlink [--local]     # unlink this machine (--local: without reaching the server)
mirrin cloud delete-account       # deleted after 7 days; --undo cancels
```

`mirrin cloud link [--handle NAME]` links without changing the reach route.

## If you stop paying

Your twin carries on as before. The pass runs for up to 7 days past the
paid-through date, with one calm line in the menu bar and never a push. After
that the address stops routing and your free route takes over (with one
notice), new backups aren't kept, and for 90 days you can still list and
fetch every backup there. Your address is held for 12 months and never given
to anyone else.

## Moving to a new computer

`mirrin restore --from cloud` on the new computer needs only your 12 words.
The address moves with the twin: the old computer stands by once the service
confirms the move, and phones keep working without being paired again.

## Leaving

You should be able to leave in an afternoon:

1. `mirrin backup migrate --from cloud --to <place>` copies every backup, byte
   for byte, to a folder, iCloud Drive or a bucket of yours.
2. Run your own relay ([relay-selfhost.md](relay-selfhost.md)); it is one
   container.
3. `mirrin reach use relay <wss-url> --hostname <name>` points your twin at
   it.
4. Pair your phone again. A new address means adding the app again, and
   setting up notifications and Face ID again.
5. `mirrin cloud billing` to cancel, then `mirrin cloud delete-account` if you
   want the record gone.

## If we ever shut Cloud down

90 days' notice, and your backups stay downloadable. Nothing is lost from
your twin: it keeps running on your machine, the backups are stock `age`
files your words open without us, and the relay and control plane are open
source, so anyone can run the same thing.

## Nothing free moves behind it: how that's kept

- Only `cmd/mirrin`, `internal/daemon` and `internal/reach` may import the
  Cloud client (a test enforces it).
- A test runs a default twin through two days of simulated time and checks it
  sends no request, and looks up no name, for a Cloud or relay host.
- A test keeps the product's name out of the twin's screen, personas,
  prompts and channels.
- `mirrin cloud egress` shows the local record of every request this machine
  sent to Cloud.

## Run your own

The control plane is `cloud/` (MIT, like the rest):
`go run ./cmd/mirrin-cloud serve --dev` inside it runs the whole paid
journey on a laptop, with fake payments and DNS ([cloud/README.md](../cloud/README.md)).
The wire contract is [cloud-api.md](cloud-api.md), and the relay protocol is
[relay-protocol.md](relay-protocol.md).
