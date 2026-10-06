# If Mirrin Cloud ever shuts down

Mirrin Cloud is planned, not running. This is the pledge it will run
under, in full.

**90 days' notice, and your backups stay downloadable.**

## What that means

1. **You hear first.** On the site, in the CHANGELOG, on the status page
   and by email to every customer, at least 90 days before anything
   stops. No new sign-ups from that day.
2. **Nothing changes for 90 days.** Your address keeps working. Nobody is
   charged for time past the end, and time already paid past it is
   refunded.
3. **You can move without us.** Your twin already runs on your machine.
   For an address from anywhere, switch to Tailscale or to your own relay
   and domain (`mirrin reach use`); the relay is the same open-source
   program we run. Your phone needs to be paired again at the new address.
4. **Your backups outlive the service.** For at least 90 more days after
   the address stops, you can list and download every backup. They are
   standard `age` files, and your 12 words decrypt them with the
   open-source `mirrin` program (any version, no account), or with any
   tool that follows the key derivation published in
   `docs/backup-format.md`. A download is all you need.
5. **Then everything is deleted,** and we say so publicly: the backups,
   the control-plane database and its replica, the signing keys. The
   handles' domain stays registered until its registration runs out, with
   every name empty, so nobody can take over an address you gave out.

## Why we can promise it

Cloud only ever sells availability, so closing it takes nothing away from
your twin. Your machine checks its Cloud pass itself and holds every key
that matters; the relays can't read your traffic; your backups are
encrypted before they leave. The operator's side of the steps is in
`cloud/ops/RUNBOOK.md`, "Shutdown".
