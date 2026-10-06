# Alerts

Exactly one alert pages. Everything else is an email, read in working
hours (`docs/cloud-design.md` §15). Status: planned; this describes how
the service will be watched once it runs.

The reason is the design: daemons are dual-homed across two relays,
entitlements last 35 days, certificates renew straight from Let's
Encrypt, and backups keep working against any bucket. The only failure a
paying user notices within minutes is "my twin's address doesn't answer",
and the canary measures exactly that.

## How it is watched

- **The canary twin** (`mirrin-canary twin`, on the control-plane VM) is a
  linked machine like a customer's: its own device key, entitlement,
  Let's Encrypt account and certificate (pinned by its handle's CAA
  record), and a tunnel to both relays. It answers `/healthz` with the id
  of the relay that carried the request.
- **Three probes** (`mirrin-canary probe`), one per region (a: the
  control-plane VM; b and c: small VMs at other providers), each dial r1
  and r2 directly every minute, over IPv4 and IPv6, and do a real HTTPS
  request to the canary's name: the certificate must verify for the name,
  and the answer must name the relay dialled. A relay counts as up from a
  region when any of its addresses answered.
- **The watcher** (`mirrin-canary watch`, on the control-plane VM) reads
  every probe's latest round each minute, and runs the email-only checks
  itself. A probe that can't be read, whose last round is more than
  about two minutes old, or whose round names another region, counts as
  unknown, never as failing, so the watcher's own network trouble can't
  page. A probe round the watcher has already counted isn't counted
  again, so "3 consecutive minutes" is 3 distinct probe rounds, and
  `check-config` refuses two regions with the same probe URL.
- **External uptime checks** (a free tier with several check locations,
  such as UptimeRobot or Better Stack) watch the watcher and the public
  names from outside our providers, by email only:

  | Check | URL | Interval |
  |---|---|---|
  | The watcher is alive (dead-man) | `https://watch.ops.OZ/healthz` | 5 min |
  | Control plane | `https://cloud.OZ/v1/version` | 5 min |
  | r1 control name | `https://r1.relay.OZ/healthz` | 5 min |
  | r2 control name | `https://r2.relay.OZ/healthz` | 5 min |
  | The canary by its name (either relay) | `https://CANARY.TZ/healthz` | 5 min |
  | Deny-list mirror | `https://denylist.OZ/denylist.paseto` | 15 min |

- **Provider emails:** budget alerts at every provider (AWS Budgets on the
  Route 53 account, Cloudflare R2 usage notifications, each VM provider's
  billing alert, set at twice the expected month), Route 53's DNSSEC
  CloudWatch alarms, Paddle's notification-destination failure emails, and
  Let's Encrypt's expiry emails to the ops address.

Test the pager and the mail path on the day they are set up, and after
any change to either:

```
sudo systemd-run --pty -p DynamicUser=yes -p LoadCredentialEncrypted=PAGERDUTY_ROUTING_KEY:/etc/credstore.encrypted/PAGERDUTY_ROUTING_KEY \
  -p LoadCredentialEncrypted=SMTP_USERNAME:/etc/credstore.encrypted/SMTP_USERNAME \
  -p LoadCredentialEncrypted=SMTP_PASSWORD:/etc/credstore.encrypted/SMTP_PASSWORD \
  /usr/local/lib/mirrin/with-credentials /usr/local/bin/mirrin-canary watch --config /etc/mirrin-canary/canary.yaml --test-page
```

The status of every alarm is on the control-plane VM:
`curl -s 127.0.0.1:9103/v1/status`, and the twin's own view at
`curl -s 127.0.0.1:9101/v1/status`.

## Page: canary unreachable through every relay

**Condition:** the canary fails end-to-end HTTPS through every relay, as
seen from at least 2 regions, for 3 consecutive minutes. It resolves after
3 good minutes in a row, and pages once per incident however much it
flaps. This rule is set in `canary.yaml` (`page_regions: 2`,
`page_after: 3`); `mirrin-canary check-config` refuses fewer than two
regions. Don't loosen it; add an email check instead.

**What it means:** paid users can't reach their twins from outside, right
now. Their twins still work at home and over any free route.

**Do:**
1. Acknowledge the page. Open an incident note (time, what you see).
2. `curl -s 127.0.0.1:9103/v1/status | jq` on the control-plane VM: which
   regions, which relays, what error per address (`connect:`, `tls:`,
   `http:`, `carried by relay …`).
3. Tell the two cases apart:
   - **Both relays down** (connect errors from every region): the relay
     hosts or their providers. `ssh r1 systemctl status mirrin-relay`,
     the providers' status pages. Restart what is down; if a provider is
     down for long, the relay IP migration runbook moves that relay.
   - **Relays up, canary not answering** (`tls:` or `http:` errors, or
     their `/healthz` is fine): the canary twin or something every twin
     shares. `curl -s 127.0.0.1:9101/v1/status`: tunnels refused
     (`denied`, `entitlement_expired`, `superseded`)? A deny-list or
     entitlement key problem affects every customer: check
     `https://cloud.OZ/v1/keys` against the relays' `issuer_keys`, and the
     key rotation runbook. A certificate error: the Let's Encrypt runbook.
     DNS answering wrongly: the DNS outage runbook.
4. When it resolves, the incident comms runbook: a post on the status
   page, and a short note in the incident log.

## One relay down

**Condition:** one relay fails from at least 2 regions for 3 minutes.
Email. **Means:** daemons are carrying everything over the other relay;
nothing is broken for users. **Do:** look within the working day.
`ssh rN journalctl -u mirrin-relay -n 200`, the provider's status page. If
the provider has lost the machine or the address, the relay IP migration
runbook. An upgrade in progress sends this email on purpose; wait for "carries
the canary again" before upgrading the other relay.

## A probe went quiet

**Condition:** a region's probe has had no fresh round for 10 minutes.
Email. **Means:** that region can't count towards a page. With one of three
probes quiet, paging still works; with two, it can't. **Do:** fix it the same
day: `ssh probe-b systemctl status mirrin-canary@probe caddy`.

## Canary certificate not renewing

**Condition:** the canary's certificate, as the probes see it, has less
than 20 days left. Email. **Means:** renewals through the relays are
failing; customers' certificates renew the same way, so theirs will follow.
**Do:** `journalctl -u mirrin-canary@twin | grep -i certificate` and the
Let's Encrypt runbook.

## A check failed

Each check in `canary.yaml` under `watch.checks` emails after its `after`
rounds of failing, and again when it passes:

| Check | Means | Runbook |
|---|---|---|
| control plane | `cloud.OZ/v1/version` isn't answering. New links, refreshes and backups wait; nothing running breaks for 35 days. | `systemctl status mirrin-cloud caddy`; restore from Litestream if the VM is lost |
| deny-list mirror | The mirror's list is over 15 minutes old or doesn't verify. Relays still poll the control plane first. | `journalctl -u mirrin-canary@mirror`; R2 status |
| relay rN control name | That relay's own `/healthz` fails (its process or its certificate), whatever the canary says. | One relay down |
| canary twin tunnels | The twin has a tunnel down or no certificate. | The twin's `/v1/status` |
| litestream | Litestream's metrics endpoint isn't answering: the process is down, so `cloud.db` isn't being replicated. A liveness check only: Litestream running with a revoked R2 token, or R2 refusing writes, still passes. The weekly restore drill (`RUNBOOK.md`, Routine) is what shows the replica is current. | `systemctl status litestream`; fix before anything else changes |

Other emails come from outside the watcher: the dead-man check (the
watcher itself is down: nothing can page until it is back; fix it first),
provider budgets (the cost review in `RUNBOOK.md`), DNSSEC alarms (the DNS
outage runbook), Paddle webhook failures (`journalctl -u mirrin-cloud`,
then Paddle's notification log; Paddle retries for days).

## Game day

Once in staging before launch, and once a year after:

1. Stop r1 (`systemctl stop mirrin-relay`). Within 60 s the probes still
   see the canary through r2; after 3 minutes the "Relay r1" email
   arrives; nobody is paged. Start r1; the "carries the canary again"
   email follows.
2. Stop both relays for 6 minutes. Exactly one page arrives, at about
   minute 3. Start both; the page resolves after 3 good minutes.
3. Note the times in the incident log. The same sequence runs on every
   commit in `cloud/cmd/mirrin-canary/canary_test.go`, in-process and on
   a simulated clock: the control plane, two relays and the canary twin,
   two regions' probes served over HTTP, and the watcher delivering to a
   stand-in pager and mail server.
