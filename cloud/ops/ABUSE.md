# Abuse and takedowns

How Mirrin Cloud handles reports that a handle is being used for harm,
under the acceptable use policy (the AUP, `docs/site/aup.md`, planned in
WP-21). Status: planned. Nothing here runs yet.

## What we can and can't see

The relays pass TLS through without ending it: we never see a page, a
message or a file. What we have is the name (the handle, which is public in
Certificate Transparency anyway), connection metadata (client /24 or /48,
bytes, duration: 72 hours raw, then hourly totals per handle for 30 days,
on each relay only), and the control plane's rows (public keys, handle,
billing status; no email, no IP address). The customer's email is at the
merchant of record.

So a report is judged on what the reporter shows us (screenshots, URLs,
headers) plus what we can check ourselves from outside, like any visitor
would: visit the handle's address in a clean browser profile. We never ask a
customer for their content and have no way to take it.

## The AUP in one paragraph

A handle is for reaching your own twin. It must not host phishing or
malware, impersonate someone, send spam, serve content that is illegal where
we operate (including child sexual abuse material), or be used to attack
other systems. Heavy use is fine up to 100 GB a month; beyond that we ask
first. The full text is `docs/site/aup.md` (WP-21).

## Where reports arrive

- `abuse@OZ` (also listed in the RDAP/WHOIS abuse contact of the relay
  addresses at each provider, and in `security.txt`).
- The relays' automatic suspension: a handle reached from more than 200
  distinct client networks in one UTC day is suspended "under review" for
  7 days and `relay-notify` mails `abuse@OZ`. Passthrough means this is the
  only automatic signal there is: a personal twin is visited by its owner's
  few devices, a phishing page by thousands.
- Provider abuse desks forwarding complaints about a relay address. Answer
  them within their deadline (usually 24–48 h) with what you did: the relay
  is a passthrough, the handle named is the customer, and the action taken.
- CA or CT reports to `security@OZ` (the CAA iodef).

Every report gets an entry in the abuse log (a private notes file or
tracker, never in this repository): date, handle, reporter, evidence seen,
decision, action, time taken. The weekly review reads it.

## Triage

| Report | Act within | Action |
|---|---|---|
| CSAM | at once | Deny-list the handle and the key. Don't open, download or forward the material. Report to the national body (Australia: eSafety Commissioner; US-linked: NCMEC CyberTipline) with the URL and time only. Keep the metadata; take legal advice. |
| Phishing or malware, verified | 1 hour | Deny-list the handle. Report the URL to Google Safe Browsing and the impersonated brand if there is one. |
| Attacks from a twin (it scans, floods or relays spam) | 4 hours | Deny-list the handle; tell the customer by the merchant of record's email. |
| Relay auto-suspension | 1 working day | Look (below); lift the suspension, or deny-list. |
| Copyright or trademark notice | 2 working days | Forward the notice to the customer via the merchant of record; act only on a valid notice for content we can verify is served. |
| Law enforcement request | as the law requires | We hold no content and no IP addresses beyond the relays' 72 h of /24s; answer with what exists. Legal advice first; log it. |
| Anything unclear | 1 working day | Ask the reporter for evidence. Don't act on an unverified claim alone. |

## Taking a handle down (the deny list)

On the control-plane VM:

```
sudo -u mirrin-cloud mirrin-cloud admin show HANDLE --config /etc/mirrin-cloud/cloud.yaml
sudo -u mirrin-cloud mirrin-cloud admin deny handle HANDLE --why abuse --config /etc/mirrin-cloud/cloud.yaml
# and, when the machine itself is the problem (attacks, spam):
sudo -u mirrin-cloud mirrin-cloud admin deny key DEVICE_KEY --why abuse --config /etc/mirrin-cloud/cloud.yaml
```

The deny list is re-signed at once. Relays poll it every minute and cut the
handle's tunnel and refuse new ones; the mirror carries the new list within
a minute. Check it took:

```
curl -s https://cloud.OZ/v1/denylist            # the signed list; its seq went up
curl -s https://denylist.OZ/denylist.paseto     # the mirror, within a minute or two
curl -sv https://HANDLE.TZ/ 2>&1 | tail -3        # the relay closes the connection
```

`--why` is one line of at most 64 bytes and shows in `/v1/me` for the
customer: write the category (`abuse`, `phishing`, `csam`, `attack`,
`legal`), never the reporter's details.

Then cancel the subscription at the merchant of record if the AUP breach
ends the service (Paddle: cancel immediately, refund at your discretion).
The handle is never reassigned, so a phishing name can't come back under
someone else.

## Looking at a suspension

```
ssh r1 'curl -s "http://127.0.0.1:9100/debug/connlog?handle=HANDLE"' | jq
ssh r2 'curl -s "http://127.0.0.1:9100/debug/connlog?handle=HANDLE"' | jq
```

Many networks, short connections and small byte counts, arriving in bursts,
look like a link sent round by email or chat: open the address in a clean
browser profile and see what it serves. A few networks with long connections
is a real person with an unusual day (a trip, a mobile carrier's NAT). If it
is legitimate, lift the suspension on each relay, one relay at a time (the
other carries the traffic meanwhile): `systemctl stop mirrin-relay`, remove
the handle from `/var/lib/private/mirrin-relay/suspensions.json` (the relay
runs as a dynamic user; `/var/lib/mirrin-relay` is a symlink to it), then
`systemctl start mirrin-relay`. Edit only while the relay is stopped: a
running relay rewrites the whole file from memory at its next suspension,
which would put the handle back. Then tell the customer nothing happened
that needs them.

## Reinstating

There is no `admin undeny` yet (a gap; see the maintainer notes in the
WP-20 hand-off). Until there is, reinstating a wrongly denied handle is a
control-plane database edit, done only with a second person watching:

```
sudo -u mirrin-cloud sqlite3 /var/lib/mirrin-cloud/cloud.db <<'SQL'
BEGIN IMMEDIATE;
DELETE FROM deny WHERE kind = 'handle' AND value = 'HANDLE';
UPDATE meta SET v = v + 1 WHERE k = 'deny_seq';
COMMIT;
SQL
```

The seq must go up with the removal: relays ignore a list whose seq is not
higher than the one they hold. Check `https://cloud.OZ/v1/denylist` no
longer names the handle, and that the handle answers again within two
minutes. Log it, and apologise to the customer through the merchant of
record.
