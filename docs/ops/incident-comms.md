# Telling people about an incident

For Mirrin Cloud (planned). When something breaks that a customer could
notice, we say so early, plainly and often. `cloud/ops/RUNBOOK.md`,
"Incident comms", says when; this page says how.

## Where

The status page (until there is one, a pinned issue in the GitHub
repository titled "Mirrin Cloud status"). Email to affected customers only
for an incident that changes something they must do (re-pair a phone,
update Mirrin), or that lasts more than four hours.

## Rules

- Say what users notice, not what broke inside: "your Mirrin address
  isn't answering from outside your home", not "r1 yamux sessions reset".
- Always say what still works: the twin itself, at home and over the free
  routes.
- Say when the next update comes, and keep to it, even if the update is
  "no change yet".
- No guesses about cause or duration. No blaming a provider unless they
  have confirmed it.
- Never ask anyone to send us their data, keys or 12 words. We would
  never need them.

## Templates

**Investigating** (within 30 minutes of the page):

> **Cloud addresses aren't answering from outside** · investigating
>
> Since 14:02 UTC, Mirrin Cloud addresses aren't reachable from outside
> your home network. Your twin itself is fine and keeps working at home
> and over Tailscale or your own relay. We're working on it and will
> update here by 15:00 UTC.

**Identified / monitoring:**

> We've found the cause (a configuration change we made reached both
> relays at once) and rolled it back. Addresses are answering
> again since 14:41 UTC. We're watching it and will close this by
> 16:00 UTC if it stays healthy.

**Resolved:**

> Resolved at 14:41 UTC. Between 14:02 and 14:41 UTC, Cloud addresses
> didn't answer from outside. Nothing was lost and there's nothing you
> need to do. A write-up follows within a week.

**Action needed** (only when it's true):

> To finish the fix, update Mirrin (`mirrin update`), then open the app on
> your phone once. If your phone asks to pair again, that's expected.

## The write-up

Within five working days, in `docs/ops/incidents/YYYY-MM-DD-short-name.md`:
what users saw and for how long, the timeline, why it happened, what we
are changing, and anything still open. Blameless and plain. Security
incidents go through `SECURITY.md` first, and the write-up waits until
it is safe to publish.
