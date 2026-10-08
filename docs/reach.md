# Reach your twin from anywhere

Your twin lives on one computer. This page is how your phone, a wall screen
or another computer reaches it when you're not at home, free routes first.
The design behind it is `docs/cloud-design.md` §5 to §8; what each route lets
anyone see is in [docs/threat-model.md](threat-model.md).

**Status.** Everything here except Mirrin Cloud is in the code and ships in
the next release. Mirrin Cloud is planned and not for sale
([docs/cloud.md](cloud.md)).

## You may not need any of this

Text your twin on WhatsApp, Telegram, Signal, Slack or any of its other
messaging channels and it answers from anywhere, with no port opened and
nothing to set up beyond the channel. Approve with `yes 12` in your own chat.
The routes below are for the presence screen as an app on your phone,
notifications on your lock screen, Face ID for the risky approvals, and
`mirrin chat` from another computer.

## Pick a route

| Route | Costs | You need | Command |
|---|---|---|---|
| Tailscale | Free | Tailscale on this computer and your phone, with HTTPS Certificates on | `mirrin reach use tailscale` |
| Your own certificate | Free | A certificate and key for a name that points at this computer | `mirrin reach use files <cert.pem> <key.pem>` |
| Your own relay | Free, plus a small server | A VPS and a domain; see [relay-selfhost.md](relay-selfhost.md) | `mirrin reach use relay <wss-url> --hostname <name>` |
| Mirrin Cloud | Planned, not for sale | Nothing to run; an address on two relays | `mirrin reach use cloud` |
| Off | | | `mirrin reach use off` |

Every route ends TLS on this computer, with a key only this computer holds,
and takes device keys only: the master key in `data/api.token` never works
from another device. After `mirrin reach use …`, restart Mirrin.

The **Reach from anywhere…** page in the menu bar lists the same routes, in
this order, says which ones work right now and what to fix when one doesn't,
and rechecks every 15 seconds.

### Tailscale

1. Install Tailscale on this computer and on your phone, and sign in to both
   with the same account.
2. In the Tailscale admin console, open DNS and turn on **HTTPS
   Certificates**.
3. `mirrin reach use tailscale`, then restart Mirrin.

Your twin gets a certificate for this computer's `ts.net` name from
Tailscale, listens on its Tailscale address (port 443 on macOS and Windows;
7743 on Linux, unless Mirrin may bind low ports), and ends TLS itself. It
does not use `tailscale serve`, whose Host-based routing another device on
your tailnet could spoof. The `ts.net` name appears in public certificate
logs, like every public certificate.

### Your own certificate

```sh
mirrin reach use files /path/to/cert.pem /path/to/key.pem
```

The certificate must name this computer's hostname. Mirrin listens on port
7743 on every interface (set `reach.listen`, such as `192.168.1.20:443`, to
choose). For a planned key change, issue the next certificate using the key
at `<key.pem>.next`, which Mirrin creates, and install it with that key: phones
and terminals that pinned the old key already know the new one.

### Your own relay

A relay lets your phone reach your twin from any network while TLS still ends
on your machine. It is the same `mirrin-relay` MavrkAI would run for Cloud;
self-hosted, it checks an allow list you write and talks to no MavrkAI host.

```sh
mirrin reach use relay wss://relay.example.org/v1/tunnel --hostname twin.example.org
```

It prints the line to add to the relay's `relay.yaml`, and the DNS records to
publish for your name: A/AAAA (never a CNAME), a `_mirrin` TXT record, and a CAA record that
pins the name to this computer's own Let's Encrypt account and the
TLS-ALPN-01 method. With that CAA record, nobody else can get a
certificate for your name, whoever runs the relay. Options: `--acme
<directory-url>` for another ACME directory, `--relay-ca <file.pem>` when the
relay's own certificate comes from a private CA.

Your twin then gets and renews its own certificate through the tunnel. It
keeps the same key across renewals, announces the next key ahead of time and
rotates yearly. Setting up the relay itself is [relay-selfhost.md](relay-selfhost.md).

### Mirrin Cloud (planned)

An address that works from any network, on two relays at once, with the
certificate pinned to your machine. `mirrin reach use cloud [--handle NAME]`
links this machine (the checkout opens in your browser) and keeps your free
route as the fallback. This version trusts no Cloud signing key yet, so it
stops before any checkout and says why. What Cloud would see is in
[cloud-trust.md](cloud-trust.md).

### One address for good

Your phone's app, its sign-in, its notifications and its passkey all belong
to one address. Moving to another route gives your twin a new address, so
each phone needs the app added again, pairing, notifications and Face ID
again. Mirrin warns you before it switches.

## Add your phone

Menu bar → **Add your phone…** (or `mirrin devices add`) shows one QR code
for the best route that works now. Scan it with the phone's camera and watch
each step light up on the computer: paired, installed, notifications on,
Face ID set up, a test notification sent. The code works once, for 10
minutes, and holds none of this computer's keys.

- **iPhone:** open the link in Safari, then Share (or the "…" menu) → **Add
  to Home Screen**, and open the new icon. It finishes pairing by itself.
- **Android:** open the link in Chrome and choose **Install app**.

From a terminal instead: `mirrin pair --screen` prints the link and a QR
code; `mirrin pair --kiosk` makes a view-only link for a wall screen.

Every new device is announced in your own chat, with **That wasn't me** and
`/revoke` to cut it off.

## Notifications

A paired phone with notifications on gets approvals, questions and security
alarms on its lock screen, and a note when the twin hands you a page in its
browser (only devices that can approve get that one; take over on the
computer itself). Your machine encrypts each one and sends it
straight to Apple's, Google's, Mozilla's or Microsoft's push service; no
Mirrin server is involved, with or without Cloud. A notification has no
decision buttons: tap it to open the request, with its picture, and decide
there. When a request is decided anywhere, the others clear.

```yaml
push:
  preview: brief         # full (the whole summary) | brief (first 60 characters) | private ("Mirrin needs you")
  quiet_hours: "22:00-07:00"   # never holds back approvals, hand-overs or security alarms
  kinds: []              # empty means every kind
  enabled: true
```

## Face ID for the risky ones

Approving a dangerous request (a payment, a shell command, a call) from a
phone or any device other than this computer needs a passkey: Face ID, a
fingerprint or the device PIN. What the passkey signs is that one request,
its exact details and your decision, once, within two minutes. Without a
passkey the phone is refused and told to approve on the computer or reply
`yes 12` in your own chat, which work as before.

A phone can set up its passkey within 15 minutes of pairing, with a passkey
it already has, or after you send `/passkey <id>` in your own chat. Every
setup is announced. `reach.step_up` can ask for more, never less:

| `reach.step_up` | A passkey is needed for |
|---|---|
| `dangerous` (default) | dangerous requests |
| `write` | writes and dangerous requests |
| `all` | every decision, including a no |

## Other computers

On the computer your twin runs on, `mirrin pair` prints a single-use code. On
the other computer, `mirrin connect ab2.…` saves the connection; `mirrin
chat` and `mirrin voice` there talk to your twin, and `mirrin disconnect`
goes back. The code pins this computer's certificate key.

## Devices

`mirrin devices` lists every paired device; `mirrin devices rename <id>
<name>` and `mirrin devices revoke <id>` do what they say, even while the
twin is stopped. The **Devices…** page in the menu bar does the same, and
`/revoke` works in your own chat. Revoking is instant: the device's key, its
push subscription and its passkeys are deleted, and a backup follows.

## Stay reachable

```sh
mirrin reach stay-awake on    # keep this computer awake while it's on power
```

## Check it yourself

```sh
mirrin reach status        # the route, the address and the relays
mirrin reach verify        # from outside: the name reaches this computer's key, the certificate is valid, CAA is pinned
mirrin reach fingerprint   # the current and next key pins, and the CAA records to publish
```

`mirrin reach verify --json` gives the same as data, and it exits non-zero
when something doesn't match. The **Trust** page shows the fingerprint, the
certificate's SHA-256 as your phone's browser shows it, and an `openssl`
command to check it from anywhere ([cloud-trust.md](cloud-trust.md) has it
too).

## If a certificate turns up that you didn't ask for

With your own relay (and Cloud), your twin asks Cert Spotter and crt.sh every
6 hours for certificates issued for its name, and checks its CAA records over
DNS-over-HTTPS. If a certificate appears that this computer didn't ask for,
or CAA changes, it:

1. pauses approvals from other devices (including a yes typed on one);
2. signs out the devices that used the name since then, and drops passkeys
   set up since then;
3. clears the phone app's cached data on its next visit;
4. tells you by notification, on the presence screen, in your chat and on the
   Health page.

`mirrin reach alarm` shows it; `mirrin reach alarm clear` on this computer,
or `/alarm clear` in your own chat, clears it. Other devices can't.

## Twilio calls

With `phone.public_url` empty, two-way calls use your twin's public address
(your relay's name or your Cloud address). Twilio's requests travel over TLS
to this computer and are checked against their signature.

## The old way: plain HTTP on your tailnet

`api.remote: true` with `api.listen` on this computer's Tailscale address
(`100.x.y.z:7742`) still works for devices paired that way. It is plain HTTP:
use it only inside Tailscale or a network you trust, never forward the port,
and it can't take Face ID, so dangerous requests can't be approved over it.
`mirrin reach use tailscale` is the HTTPS replacement.

## Settings

| Key | Meaning |
|---|---|
| `reach.mode` | `off`, `tailscale`, `files`, `relay` or `cloud` |
| `reach.listen` | where the remote listener binds (defaults above) |
| `reach.cert_file`, `reach.key_file` | the `files` route's certificate and key |
| `reach.relay_url`, `reach.hostname` | the relay route's tunnel address and your public name |
| `reach.relay_ca_file` | extra roots for the relay's own certificate |
| `reach.acme_directory`, `reach.acme_email` | the ACME directory (default Let's Encrypt) and contact |
| `reach.fallback` | with `cloud`, the free route used while the paid address can't serve |
| `reach.step_up` | `dangerous`, `write` or `all` |
| `reach.stay_awake` | keep the computer awake on power |
| `reach.admin_remote` | let a device paired with `admin` open settings pages from afar (off) |
| `push.enabled`, `push.preview`, `push.quiet_hours`, `push.kinds` | notifications, above |
| `phone.public_url` | where Twilio reaches the twin, if not the reach address |
| `api.listen`, `api.remote` | the local listener, and the old plain-HTTP remote mode |
