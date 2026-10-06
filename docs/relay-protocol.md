# Relay tunnel protocol: `antbot.tunnel.v1`

This is the protocol between a Mirrin daemon and a mirrin-relay. It is how
a phone on any network reaches the twin on the user's machine while TLS still
ends on that machine. `docs/cloud-design.md` §6.3–6.4 gives the design; this
document is the specification.

Code:
- `internal/relay/wire`: messages, signatures, the PROXY v2 header, parsing
  and limits. Both ends use it, so both check every message the same way. It
  uses the standard library only.
- `internal/relay`: the daemon's tunnel client and `net.Listener`.
- `internal/relay/server`: the relay (WP-14).

## 1. Shape

```
 browser ──TCP:443──▶ relay ──peek SNI──▶ tunnel for that name
                        │                     │ one yamux stream per browser connection:
                        │                     │ PROXY v2 header, then the raw TLS bytes
                        ▼                     ▼
               (its own control name)   daemon: relay.Listener ─▶ tls.Server ─▶ API
```

1. The daemon dials out to each relay: a WebSocket over TLS 1.3.
2. A three-message handshake proves the daemon holds its device key, bound
   to this relay, this challenge and this TLS session.
3. The WebSocket then carries a yamux session. The relay is the yamux
   client, and it opens one stream per browser connection.
4. Each stream starts with a PROXY v2 header naming the browser's address,
   followed by the browser's bytes from the ClientHello on. The relay never
   terminates tenant TLS, so it sees only ciphertext and metadata.

## 2. Transport

| Item | Value |
|---|---|
| URL | `wss://<relay control name>/v1/tunnel`, such as `wss://r1.relay.mirrin.app/v1/tunnel` |
| TLS | 1.3 only, verified against the system roots. The relay serves its control name with its own certificate. |
| HTTP | HTTP/1.1 Upgrade (ALPN `http/1.1`). No redirects are followed and no HTTP proxy is used. |
| WebSocket subprotocol | `antbot.tunnel.v1`. Either side closes if it is not the one negotiated. |
| WebSocket compression | none |

The daemon dials every relay listed in its entitlement (`relays[]`), or the
one self-hosted relay it is configured with. Each tunnel is independent.

## 3. Handshake

Three text frames, each one JSON object of at most 16 KiB.

```
relay  → {"t":"challenge","v":1,"relay":"r1","nonce":"<b64url 32 B>"}
daemon → {"t":"hello","v":1,"key":"<b64url Ed25519 public key>","sig":"<b64url 64 B>",
          "ent":"v4.public.…" | null,"status_key_hash":"<b64url 32 B>","client":"mirrin/0.4.0 darwin/arm64"}
relay  → {"t":"welcome","hostnames":["ember-otter-42.mirrin.link"],"gen":3,"keepalive":25,"max_streams":64}
       | {"t":"error","code":"…","message":"<sentence shown verbatim>","retry_after":3600}
```

### 3.1 Encoding rules

- JSON per RFC 8259, parsed with `encoding/json/v2`:
  - exactly one object per frame, with nothing after it;
  - valid UTF-8 only;
  - member names are case-sensitive, and a duplicate name rejects the frame;
  - unknown members are ignored, so a peer may add optional fields
    without a version change.
- `b64url` is unpadded base64url (RFC 4648 §5), and only its canonical form
  is accepted: no padding, no non-zero trailing bits, and an exact length
  for the size (43 characters for 32 bytes, 86 for 64).
- A frame that breaks a rule ends the attempt. A malformed refusal is never
  taken for a refusal.

### 3.2 challenge

| Field | Rule |
|---|---|
| `t` | `"challenge"` |
| `v` | `1`. Any other value is a version mismatch. |
| `relay` | The relay's id: 1–32 characters of `a-z 0-9 . _ -`, starting with a letter or digit. |
| `nonce` | 32 fresh random bytes, b64url. |

The daemon knows which relay it dialled (`RelayRef.ID`, from the
entitlement). If the challenge names another id, the daemon sends no hello:
a hello signed for `r1` must not be obtainable by `r9`. A self-hosted relay
configured without an id is taken at its word.

### 3.3 hello

| Field | Rule |
|---|---|
| `t`, `v` | `"hello"`, `1` |
| `key` | The daemon's Ed25519 device key, 32 bytes, b64url. With an entitlement it must equal the entitlement's `cnf`. |
| `sig` | Ed25519 over the signed input below, 64 bytes, b64url. |
| `ent` | The entitlement token (`v4.public.…`, at most 8 KiB), or `null` for a self-hosted relay. An empty string is rejected. |
| `status_key_hash` | SHA-256 of the daemon's 32-byte status key, b64url. The relay stores only this. |
| `client` | 1–128 printable ASCII characters: `mirrin/<version> <os>/<arch>`. |

The signed input is:

```
"antbot-relay-tunnel-v1" 0x00 relay 0x00 nonce 0x00 exporter
```

- `relay` is the relay id, as ASCII.
- `nonce` is the challenge's 32 raw bytes.
- `exporter` is `ExportKeyingMaterial("EXPORTER-antbot-tunnel", nil, 32)`
  on the daemon–relay TLS 1.3 session (RFC 8446 §7.5). Both ends compute it
  from their side of the same session.

A relay id cannot contain 0x00, and the nonce and exporter have fixed
sizes, so two different inputs never serialize to the same bytes. The
context string keeps these signatures apart from every other use of the
device key (such as the RFC 9421 signatures sent to the control plane).

**What the binding buys.** A hello proves possession of the device key for
one relay, one challenge and one TLS session.
- It cannot be replayed: the nonce is fresh.
- It cannot be used at another relay: the relay id differs.
- It cannot be forwarded by anyone in the middle. A party that terminates
  the daemon's TLS has a different session, and so a different exporter,
  from the one it holds with the real relay. This is channel binding in the
  manner of RFC 9266's `tls-exporter`.

**Relay checks, in order** (WP-14):
1. `VerifyHello` for its own id, its nonce and its exporter.
2. Hosted mode: the entitlement verifies under a pinned `ent-*` key; it has
   not expired; `cnf` equals `key`; the hostnames are within `hosts`;
   neither the handle nor the key is on the deny list.
   Self-host mode: `{hostname, key}` is in the static allow list.
   The hello names no hostnames. The tunnel carries the entitlement's
   `hosts`, each of which must be `<handle>.<zone>` for one of the relay's
   tenant zones, with `reach` in `feat`; or, self-hosted, every hostname
   the allow list gives the key.
3. Supersede ordering by `(gen, iat)` per hostname.

### 3.4 welcome

| Field | Rule |
|---|---|
| `t` | `"welcome"` |
| `hostnames` | 1–64 lower-case DNS names (LDH labels): the names this tunnel now carries |
| `gen` | ≥ 0: the handle generation the relay holds for them (0 in self-host mode) |
| `keepalive` | 5–300 s: the yamux ping interval both sides use |
| `max_streams` | 1–4096: concurrent data streams the relay will open. The daemon holds at most 256 (§5). |

### 3.5 error

| Field | Rule |
|---|---|
| `t` | `"error"` |
| `code` | 1–32 characters of `a-z _` |
| `message` | At most 512 bytes of printable UTF-8 (no control or formatting characters). It may be empty. The daemon shows it verbatim. |
| `retry_after` | 0–86400 s, optional. The daemon waits at least this long before redialling. |

After an error the relay closes the WebSocket.

| Code | Meaning | Daemon |
|---|---|---|
| `entitlement_expired` | `exp` has passed | retry (a refresh may have happened) |
| `bad_signature` | the hello did not verify | retry |
| `hostname_not_allowed` | a name outside the entitlement or allow list | retry |
| `denied` | the handle or key is on the deny list | retry, honouring `retry_after` |
| `superseded` | a higher `gen` holds the names | **stop this tunnel**; the daemon stands by |
| `superseded_retry` | same `gen`, newer `iat` | refresh the entitlement, then reconnect; never give up |
| `rate_limited` | too many hellos | retry after `retry_after` |
| `upgrade_required` | this client is too old | retry, honouring `retry_after` |
| anything else | a newer relay | retry |

Every refusal reaches `ClientConfig.OnRefused` before any reconnect.

## 4. The session

After `welcome`, both sides switch to binary WebSocket frames that carry a
[yamux](https://github.com/hashicorp/yamux/blob/master/spec.md) session.
Message boundaries mean nothing; the frames are one byte stream.

- The relay is the yamux **client**, so it opens streams (odd ids from 1).
  The daemon is the server and never opens a stream.
- Both sides ping every `keepalive` seconds. yamux closes a session whose
  ping goes unanswered.
- A hosted relay ends a tunnel within a minute of its entitlement's `exp`,
  with a `notice`. The daemon redials with the entitlement it holds then.
- The relay opens **stream 1** first, for control. Every later stream is
  one browser connection.

### 4.1 Control stream (stream 1)

Newline-terminated JSON, one message per line, each line at most 4 KiB
before its newline. Each message has a `t`; its other fields are optional.

| `t` | Fields | Daemon |
|---|---|---|
| `superseded` | `gen`, `message` | A `gen` above the welcome's means another machine holds the names now: the tunnel ends, `OnRefused` gets `superseded`, and it never reconnects. A `gen` at or below it (same generation, newer token) is handled as `superseded_retry`. |
| `drain` | `retry_after`, `message` | The relay is going away. It opens no new streams, and the daemon waits at least `retry_after` after the session ends before redialling. |
| `notice` | `message` | Passed to `OnControl` only. |
| `limits` | `max_streams`, `bps` | Updates `Status().MaxStreams` and the session's stream limit (§5). |
| anything else | | Ignored, after `OnControl`. |

Every message goes to `ClientConfig.OnControl`. A malformed line ends the
session, which then reconnects.

### 4.2 Data streams

Each data stream starts with a PROXY protocol version 2 header, as HAProxy
specifies it in
[proxy-protocol.txt §2.2](https://www.haproxy.org/download/3.0/doc/proxy-protocol.txt).
The raw bytes of the browser's connection follow, from the first byte of its
ClientHello, which the relay peeked to route by SNI.

```
0d0a0d0a000d0a515549540a   signature
21                         version 2, command PROXY
11 | 21                    TCP over IPv4 | TCP over IPv6
LLLL                       length of the rest (big-endian)
src addr, dst addr, src port, dst port    (12 or 36 bytes)
02 LLLL <SNI>              PP2_TYPE_AUTHORITY: the browser's server_name
e0 LLLL <relay id>         PP2_TYPE_MIN_CUSTOM: the relay's id
```

- The source is the browser's address and port. The destination is the
  relay address the browser connected to. A relay writes TCP over IPv4 only
  when both are IPv4; otherwise TCP over IPv6, with IPv4 mapped.
- The whole header is at most 1024 bytes; a relay's are under 400. The
  daemon allows 5 s for it to arrive.
- The daemon accepts a stream only when all of these hold:
  - the command is PROXY over TCP;
  - the relay-id TLV equals this tunnel's relay id;
  - the authority TLV matches, case-insensitively, a name from the
    welcome.

  Otherwise it closes the stream, and nothing reaches `Accept`.
- A `PP2_TYPE_CRC32C` TLV is verified when present; relays do not send one.
- Other TLVs are skipped. A duplicate authority, relay-id or CRC TLV
  rejects the header.
- Closing is yamux's: each side sends FIN when its half is done. When the
  daemon closes a stream, the relay closes the browser's socket and then
  its own half of the stream. If it has not done so within 10 s, the
  daemon resets the stream.

Example, as the tests pin it (`wire.goldenV4`): browser 203.0.113.9:51234
to relay 198.51.100.1:443, SNI `h.test`, relay `r1`.

```
0d0a0d0a000d0a515549540a 21 11 001a cb007109 c6336401 c822 01bb 02 0006 682e74657374 e0 0002 7231
```

## 5. The daemon's side

`relay.Listen(ctx, relay.ClientConfig{…})` returns a `*relay.Listener`: a
`net.Listener` that yields each browser connection as a `*relay.Conn`.

- `RemoteAddr` is the browser's address from the PROXY header, and
  `LocalAddr` is the relay's. `Relay()` and `ServerName()` give the relay id
  and the SNI.
- The remote listener serves it with `tls.Server`, exactly as it serves a
  socket (`api.ServeRemote`, WP-03).
- `Status()` reports each tunnel:
  - online and since when;
  - the hostnames, `gen` and `max_streams` from the welcome;
  - whether the relay is draining;
  - the last refusal, and whether the tunnel has stopped;
  - the last error.
- `Close`, or cancelling the context, ends every tunnel and with them every
  accepted connection. `Done()` closes once every tunnel goroutine has
  returned, and it also closes when every relay has superseded the daemon.

**Reconnecting.**
- The first dial is immediate.
- After a failure the daemon waits a random time in `[0, ceiling)` ("full
  jitter"). The ceiling starts at 1 s and doubles to 60 s.
- A tunnel that stayed up for a minute starts the ceiling over.
- A relay's `retry_after`, from an error or a drain, is a floor under that
  wait.

**Limits on the daemon's side.**

| Limit | Value |
|---|---|
| Whole attempt: dial, TLS, upgrade and handshake | 30 s |
| Data streams a session holds | `max_streams`, at most 256 |
| Streams yamux queues for the daemon to take | 256; yamux resets the rest |
| PROXY header arrival | 5 s |
| A closed stream the relay has not closed | reset after 10 s |
| Handshake frames | 16 KiB |
| Control lines | 4 KiB |

A yamux stream can hold 256 KiB of unread data before its first window
update. A daemon's `Close` only half-closes a stream: yamux keeps the stream
until the relay closes its half, resets it, or the 10 s pass. So a data
stream counts against the session's limit from when the daemon takes it
from yamux until yamux lets go of it, not just until `Conn.Close`. Streams
the daemon drops for a bad header count the same way.

At the limit the daemon takes no more streams. New ones wait in yamux's
queue, and they are neither dropped nor accepted until a place frees. A
relay that never has more than 256 streams open never fills the queue.

A relay, however hostile, can therefore make the daemon hold at most
1 + 256 + 256 streams per tunnel: control, the limit and the queue. Each
holds at most 256 KiB of unread data, so about 128 MiB in all. The
daemon's goroutines per tunnel are at most the limit plus a few. Opening
more streams only gets them reset. `TestStreamFloodBounded` floods 600
streams at a limit of 16 and checks that the daemon never holds more than
1 + 16 + 256.

## 6. What each side can and cannot do

**A relay can** refuse or drop tunnels and streams, delay traffic, see
connection metadata (times, sizes, SNI, browser addresses), and lie in PROXY
headers about where a connection came from. `RemoteAddr` is therefore the
relay's claim. It is fine for rate limits and the audit line ("relay r2,
203.0.113.9"), but it is never evidence of identity.

**A relay cannot** read or change anything inside the browser's TLS. TLS
ends in the daemon under a key only the daemon holds, and certificates for
the handle are CAA-pinned to the daemon's ACME account (`cloud-design.md`
§6.2). It also cannot reuse a hello elsewhere (§3.3), or learn the status
key.

**The daemon trusts** the relay's TLS certificate for its control name (the
system roots) and nothing else about the relay.

## 7. Versions

`v` in the challenge and hello, and the subprotocol name, carry the version.
- Adding optional fields, control types or error codes is compatible:
  unknown ones are ignored or retried.
- Anything else is `antbot.tunnel.v2`. A relay that cannot serve a client's
  version refuses it with `upgrade_required`.

## 8. Test vectors and fuzzing

- `TestHelloKnownAnswer` fixes a complete hello:
  - inputs: device key seed 32×`01`, relay `r1`, nonce 32×`11`, exporter
    32×`22`, status key 32×`33`;
  - it produces the frame below. Ed25519 is deterministic, so any correct
    implementation produces the same bytes.

  ```
  {"t":"hello","v":1,"key":"iojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w","sig":"KlBxrfYKXmCKM4VLmZbTMG-Y_EBkH1_5G8Tg7TSNxijTFGspTxH_mvoqEXcNI-sTQ88kmVv4qO54YPBSTWykDg","ent":null,"status_key_hash":"3rDjjO0eQd5vkucOgMQY0tNWr6qpnib1k528fT70dyo","client":"mirrin/0.4.0 darwin/arm64"}
  ```
- `TestProxyV2Golden` pins IPv4 and IPv6 headers assembled byte by byte from
  the HAProxy specification, which has no binary examples of its own. The
  CRC-32C used for `PP2_TYPE_CRC32C` is checked against the RFC 3720 B.4
  examples, stored verbatim in `internal/relay/wire/testdata`.
- `FuzzReadProxyV2`, `FuzzHello` and `FuzzRelayMessages` run for 60 s each
  nightly (`.github/workflows/fuzz.yml`). Each checks that anything accepted
  re-encodes to something that parses identically.
