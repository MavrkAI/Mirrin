# How Mirrin Cloud is run

Mirrin Cloud is the optional paid service: a public address for your
twin, carried by two relays, and off-site encrypted backup. It is
planned, not running. This page says how it will be operated, so anyone
can check the promises against the code and the config.

Everything free stays free and whole on your own machine. Cloud sells
availability only.

## What runs

- **Two relays** (`mirrin-relay`, the same program you can run yourself)
  at two different hosting providers. Your twin keeps a tunnel open to
  both, so either one alone is enough. They pass your encrypted traffic
  through without ending TLS: they can't read it.
- **One control-plane server** (`mirrin-cloud`) that turns a payment into
  your address, its DNS records and a signed pass your machine checks
  itself. It keeps public keys, handles, billing status and backup sizes;
  never your email (that stays with the payment provider) or your IP
  address. Its database is replicated continuously to object storage.
- **A canary** (`mirrin-canary`): a twin of our own, reached from three
  places every minute through each relay, exactly as your phone reaches
  yours. It is how we know within minutes when the service is down.

All of it is open source in this repository: `cloud/` (the control plane,
the canary and the deploy config) and `cmd/mirrin-relay`.

## What we promise

- **Your twin never depends on us to run.** A pass lasts up to 35 days,
  so even a long outage on our side leaves your address working for
  weeks, and your twin keeps working at home and over the free routes
  regardless.
- **One thing wakes someone up:** your address not answering through
  either relay. Everything else is handled in working hours, because the
  design gives it weeks of slack.
- **We tell you when something breaks** that you could notice, within 30
  minutes, and we write up what happened afterwards
  ([incident-comms.md](incident-comms.md)).
- **If Cloud ever closes:** 90 days' notice, and your backups stay
  downloadable ([shutdown-pledge.md](shutdown-pledge.md)).

## For operators

The runbooks are in `cloud/ops/`: `RUNBOOK.md` (deploying, upgrades,
relay moves, key rotation, DNS and certificate trouble, restores),
`ALERTS.md` (what pages, what emails, and what to do) and `ABUSE.md`
(reports and takedowns). The deploy config is `cloud/deploy/`.
