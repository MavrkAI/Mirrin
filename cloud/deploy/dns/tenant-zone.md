# The tenant zone

Every handle is a name in the tenant zone, TZ (`mirrin.link` in the
examples): `ember-otter-42.mirrin.link`. The zone lives at Route 53, is
signed with DNSSEC, and holds nothing but the apex and one small set of
records per handle (`docs/cloud-design.md` §6.2). Status: planned; nothing
here is live yet.

## What is in it

At the apex, besides the SOA and NS records Route 53 makes:

```
mirrin.link.  3600  CAA  0 issue ";"
mirrin.link.  3600  CAA  0 issuewild ";"
mirrin.link.  3600  CAA  0 iodef "mailto:security@mirrin.app"
```

No CA may issue for the zone itself or for any wildcard in it. There is no
wildcard record and no HTTPS/SVCB record (either would break SNI routing
at the relays), so a name no handle holds is NXDOMAIN.

Per handle, written by mirrin-cloud through `cloud/internal/dns`
(Route 53, signed with `internal/sigv4`) at link, relink, recovery and
ACME-account change, never on the renewal path, and removed when the
account is deleted:

```
ember-otter-42.mirrin.link.  300  A     <r1 v4> <r2 v4>
ember-otter-42.mirrin.link.  300  AAAA  <r1 v6> <r2 v6>
ember-otter-42.mirrin.link.  300  CAA   0 issue "letsencrypt.org; accounturi=https://acme-v02.api.letsencrypt.org/acme/acct/<id>; validationmethods=tls-alpn-01"
ember-otter-42.mirrin.link.  300  CAA   0 issuewild ";"
ember-otter-42.mirrin.link.  300  CAA   0 iodef "mailto:security@mirrin.app"
```

The handle's CAA record names the ACME account on the user's own machine,
so whoever controls a relay, the entitlement key or the relays' addresses
can steer traffic but can't get a certificate for the name. Whoever holds
this zone could rewrite CAA; that is detected by every twin's CT and CAA
watch, not prevented. BYOD (a user's own domain) is the answer for anyone
who needs prevention.

## Setting it up (once)

The files below are templates; render them with
`cloud/deploy/render.sh out/dns cloud/deploy/hosts/common.env cloud/deploy/hosts/dns.env`.

1. **Register the zone's domain for at least two years**, at a registrar
   that supports DNSSEC (DS records) and registry lock. Turn on registry
   lock or at least transfer lock, and 2FA on the registrar account.
2. **Create the hosted zone** in Route 53 (public). Put its four NS
   records at the registrar. Write its id into `hosts/common.env` as
   `ROUTE53_HOSTED_ZONE_ID`.
3. **Sign it:** run `route53-dnssec.sh` (rendered) with an AWS admin
   profile. It makes a KMS key in us-east-1 that only Route 53's DNSSEC
   service can use, creates the key-signing key, turns signing on and
   prints the DS record. Add the DS at the registrar. Check with
   `dig +dnssec mirrin.link SOA` (the `ad` flag) and dnsviz.net. Then add
   the CloudWatch alarms `DNSSECInternalFailure` and
   `DNSSECKeySigningKeysNeedingAction` for the zone, emailing ops (they
   are email alerts, `cloud/ops/ALERTS.md`).
4. **The apex CAA:**
   `aws route53 change-resource-record-sets --hosted-zone-id Z… --change-batch file://out/dns/tenant-apex.json`.
5. **The control plane's IAM user:** create a user with no console access
   and attach `route53-policy.json` as its only policy. It can list the
   zone and UPSERT or DELETE A, AAAA and CAA records below the apex, and
   nothing else: it can't touch the apex CAA, add a wildcard of another
   type, or change any other zone. Make one access key and store it on
   the control-plane VM as the `ROUTE53_ACCESS_KEY_ID` and
   `ROUTE53_SECRET_ACCESS_KEY` credentials (`../README.md`, "Secrets").
6. **The Public Suffix List:** submit the zone to the PSL on tenant
   isolation grounds (each name is a different customer's origin), with
   the DNS TXT proof the PSL asks for. It takes weeks; submit it in W0.
7. **Check an unbound name:** `dig nobody-has-this.mirrin.link` answers
   NXDOMAIN, and `dig mirrin.link CAA` shows the three apex records.

## The operator zone

`operator-zone.zone` is the operator zone, OZ (`mirrin.app`): the control
plane, each relay's control name, the probes and the watcher, plus a CAA
that allows only Let's Encrypt. It is at Cloudflare, DNS only (never
proxied: every service ends its own TLS), so the deny-list bucket can
have an R2 custom domain. Import the rendered file in Cloudflare's
dashboard, turn on Cloudflare DNSSEC and put its DS at the registrar.

## When DNS changes

- A relay's addresses: the relay IP migration runbook
  (`cloud/ops/RUNBOOK.md`). Both zones change: the relay's control name
  here, and every handle's A/AAAA through the control plane.
- Route 53 is down: the DNS outage runbook. Existing records keep
  answering from resolvers' caches and Route 53's anycast edges; only new
  handles and CAA changes wait.
