# Run your own mirrin-relay

mirrin-relay lets your phone reach your twin from any network while TLS
still ends on your own machine. It is the same program MavrkAI runs for
Mirrin Cloud. Self-hosted, it talks to no MavrkAI host at all: it checks a
short allow list you write, and nothing else.

```
 phone ──TLS for twin.example.org──▶ your VPS: mirrin-relay ──tunnel──▶ your Mac: Mirrin
                                        │ reads only the SNI in the ClientHello,
                                        │ then passes the encrypted bytes through
                                        └ terminates TLS only for relay.example.org
```

- **The relay never holds your twin's key.** Your twin's certificate and key
  live on your machine. The relay has no code path that could load a
  certificate for any name but its own control name; a test enforces that.
- **What the relay sees:** connection times, sizes, your twin's hostname
  (the SNI) and each client's address. It never sees anything inside TLS:
  messages, screens, approvals, cookies.
- **What it keeps:** in memory only, the last 72 hours of connections, with
  each client shown only as its /24 (IPv4) or /48 (IPv6) network, then
  hourly totals for 30 days. Its logs never hold a full client address.

`docs/relay-protocol.md` is the protocol between the relay and your twin.

## 1. What you need

- A small Linux VPS with a public IPv4 address (IPv6 too, if you have it).
  One vCPU and 512 MB is plenty for a household: an idle tunnel costs the
  relay about 75 KiB, and 10,000 idle tunnels stay under 1 GB.
- Ports 80 and 443 free on it, and open in its firewall.
- A domain you control, for two names:
  - the relay's **control name**, such as `relay.example.org`;
  - your **twin's name**, such as `twin.example.org`.
- Mirrin on your machine, with reach support (`mirrin reach use relay`).

Do not put a CDN or TLS-terminating proxy (such as a proxied Cloudflare
record) in front of either name. The relay must receive the raw TLS
connection: it routes by the SNI, and the tunnel's signature is bound to the
TLS session between your twin and the relay.

## 2. DNS

Point both names at the VPS with A (and AAAA) records. Use A/AAAA, never a
CNAME, for the twin's name.

```
relay.example.org.  300 A     203.0.113.10
twin.example.org.   300 A     203.0.113.10
```

Add the IPv6 address as AAAA records if the VPS has one.

Both certificates come from Let's Encrypt. If your domain has CAA records,
they must allow `letsencrypt.org`. Your twin can go further and pin its own
name to its own ACME account; `mirrin reach use relay` prints the CAA record
that does it. With that record in place, nobody else, including whoever runs
the relay, can get a certificate for your twin's name.

## 3. Install the relay

Download `mirrin-relay-linux-amd64` (or `-arm64`) and
`SHA256SUMS` from the same
[release](https://github.com/MavrkAI/Mirrin/releases), check the binary
against it, and install it:

```sh
sha256sum --check --ignore-missing SHA256SUMS
sudo install -m 0755 mirrin-relay-linux-amd64 /usr/local/bin/mirrin-relay
mirrin-relay version
```

To build it yourself instead: `make relay` (or `make relay-release` for both
Linux architectures) in a checkout.

## 4. Configure

On your twin's machine, choose the relay and your twin's name:

```sh
mirrin reach use relay wss://relay.example.org/v1/tunnel --hostname twin.example.org
```

It prints the line for the relay's allow list: your twin's name and its
device key, a public key that proves the tunnel is your twin. It also
prints the DNS records from section 2.

On the VPS, start from
[`packaging/relay/relay.example.yaml`](../packaging/relay/relay.example.yaml):

```sh
sudo mkdir -p /etc/mirrin-relay
sudo cp relay.example.yaml /etc/mirrin-relay/relay.yaml
sudoedit /etc/mirrin-relay/relay.yaml
```

Set at least:

```yaml
id: r1
control_hostname: relay.example.org
state_dir: /var/lib/mirrin-relay
allow:
  - hostname: twin.example.org
    key: <the key mirrin reach use relay printed>
```

relay.yaml holds only public keys, so it needs no special permissions.
Check it:

```sh
mirrin-relay check-config --config /etc/mirrin-relay/relay.yaml
```

It says what the relay will do, including that it contacts no MavrkAI host.

The control name's certificate comes from Let's Encrypt by TLS-ALPN-01 on
port 443, for that name only, and is cached in `state_dir`. To use your own
certificate instead, set `cert_file` and `key_file` and restart the relay
after renewing them.

## 5. Run it

### With systemd

```sh
sudo install -m 0644 mirrin-relay.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now mirrin-relay
journalctl -u mirrin-relay -f
```

[`packaging/relay/mirrin-relay.service`](../packaging/relay/mirrin-relay.service)
runs the relay as a throwaway user that may bind ports 80 and 443 and write
only `/var/lib/mirrin-relay`.

### With Docker

```sh
make relay-release
docker build --platform linux/amd64 -f packaging/relay/Dockerfile -t mirrin-relay .
docker run -d --name mirrin-relay --restart unless-stopped --network host --stop-timeout 40 \
  -v /etc/mirrin-relay:/etc/mirrin-relay:ro -v mirrin-relay:/var/lib/mirrin-relay mirrin-relay
```

Use `--network host`. Behind Docker's port publishing (`-p 443:443`) every
client appears to come from Docker's bridge address, so the per-address
limits would count all of them together. The image runs as an unprivileged
user that may bind ports 80 and 443. `--stop-timeout 40` gives the drain its
30 seconds on `docker stop` and `docker restart`; Docker's default is 10.

### A relay set up before the rename

A relay installed before the rename to Mirrin runs as `antbot-relay`. Move
it over, keeping its settings and its state (the ACME account, the control
name's certificate and any suspensions):

```sh
sudo systemctl disable --now antbot-relay
sudo mv /etc/antbot-relay /etc/mirrin-relay
sudo mv /var/lib/private/antbot-relay /var/lib/private/mirrin-relay
sudo rm -f /var/lib/antbot-relay /etc/systemd/system/antbot-relay.service /usr/local/bin/antbot-relay
```

Set `state_dir` in `/etc/mirrin-relay/relay.yaml` to `/var/lib/mirrin-relay`
(the unit lets the relay write nowhere else), then install `mirrin-relay`
and its unit as above.

Under Docker, move the config folder and set `state_dir` the same way, then
`docker rm -f antbot-relay` and start the new container with the old
volume: the `docker run` above with `-v antbot-relay:/var/lib/mirrin-relay`.

Its metrics are now named `mirrin_relay_*` (they were `antbot_relay_*`):
update dashboards and alert rules, such as those on the deny list's age and
the control certificate's expiry, or they stop matching without a word.

## 6. Connect your twin

On your twin's machine, restart Mirrin (or run `mirrin reach use relay …`
again if you skipped it). The twin dials the relay, proves its device key,
and gets its own certificate for `twin.example.org` through the tunnel.

```sh
mirrin reach status   # the relay tunnel is online
mirrin reach verify   # the name resolves to the relay, and the certificate served is this machine's
```

Then pair your phone from the "Add your phone" page. It opens
`https://twin.example.org/`.

## 7. Check it yourself

```sh
# The relay's own name answers:
curl https://relay.example.org/healthz
curl https://relay.example.org/v1/version

# Your twin's name is served by your twin's key, not the relay's:
openssl s_client -connect twin.example.org:443 -servername twin.example.org </dev/null 2>/dev/null \
  | openssl x509 -noout -pubkey | openssl pkey -pubin -outform der | openssl dgst -sha256
mirrin reach fingerprint   # the same SHA-256

# Any other name is closed without a byte:
openssl s_client -connect relay.example.org:443 -servername nobody.example.org </dev/null
```

The relay also answers `GET https://relay.example.org/v1/status/twin.example.org?k=<status key>`
with whether your twin is online, or when it was last seen. Only your
twin's status key gets an answer; any other key gets a 404. The phone app
uses it to say "asleep since 14:02".

## 8. Operating it

- **Logs** go to the journal as JSON. They name hostnames (they are public
  in Certificate Transparency anyway) and show the addresses of clients
  and twins only as /24 or /48 networks, at every level. Set
  `MIRRIN_RELAY_DEBUG=1` for more.
- **The control name's certificate:** if the relay cannot get it (DNS not
  pointed at the VPS yet, a CAA record that does not allow Let's Encrypt,
  port 443 not reachable from outside, a rate limit), the journal says so
  in a `relay: control certificate` line with the reason, at most once a
  minute, and again when it is fixed. The metrics
  `mirrin_relay_control_cert_errors_total` and
  `mirrin_relay_control_cert_expiry_timestamp_seconds` show the same.
- **Metrics** are served on `metrics_listen` (default `127.0.0.1:9100`):
  `/metrics` in the Prometheus format, with no per-handle labels, and
  `/healthz`.
- **The connection log** is on the same listener while it is on loopback:
  `curl '127.0.0.1:9100/debug/connlog?handle=twin.example.org'` over SSH.
- **Restarts and upgrades** drain: on SIGTERM the relay tells each twin it
  is going away (the twin waits 15 s before redialling) and gives spliced
  connections up to 30 s. Replace the binary and `systemctl restart
  mirrin-relay`.
- **Moving to a new machine:** list the new machine's key next to the old
  one for the same hostname. The tunnel that connected last holds the name:
  when the new machine connects, the old one is told another machine holds
  it, and stands by. Remove the old key once the move is done.
- **Limits** (`limits:`) default to 64 connections and 20 Mbit/s per
  tunnel, 10 tunnel attempts per address per minute, and a 10-minute idle
  cut. They protect the relay; raise them if your twin needs more.
- **The abuse heuristic** (`abuse:`) suspends a name reached from more than
  200 client networks in a day, which a personal twin never is. Set
  `distinct_nets_per_day: 0` to turn it off on a relay only you use. To
  hear about a suspension, set `notify_command` to a script; the relay runs
  it as `<script> <name> <networks>` (no shell, 30 s at most), for example
  to send you a mail. Under the systemd unit it runs in the relay's
  sandbox: it can use the network but write only to `state_dir`, so a
  script that posts to a mail or chat API works best. Suspensions are kept
  in `state_dir/suspensions.json`, so a restart does not lift them.

## 9. Troubleshooting

| The twin says | Meaning |
|---|---|
| `hostname_not_allowed` | This device key is not in `allow`. Check the key, and restart the relay after editing relay.yaml. |
| `bad_signature` | The hello did not verify. The signature is bound to the TLS session between the twin and the relay, so something in between is terminating TLS: a CDN or proxied DNS record, or a TLS-inspecting firewall or VPN on the twin's network. |
| `rate_limited` | More than `hello_per_ip_per_min` attempts from one address. The twin waits and retries. |
| `denied` | This name was suspended by the abuse heuristic. It lapses after `abuse.suspend_for`. To lift it now, remove the name from `/var/lib/mirrin-relay/suspensions.json` (under Docker, in the volume) and restart the relay. |
| The tunnel never comes up | Is the control name's certificate valid (`curl https://relay.example.org/healthz`)? Look for `relay: control certificate` in the journal: Let's Encrypt must reach port 443 on the VPS directly, and CAA must allow it. |

## 10. Removing it

```sh
sudo systemctl disable --now mirrin-relay
sudo rm /etc/systemd/system/mirrin-relay.service /usr/local/bin/mirrin-relay
sudo rm -r /etc/mirrin-relay /var/lib/private/mirrin-relay
```

On your twin, `mirrin reach use off`, or pick another mode.
