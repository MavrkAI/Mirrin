# Backup format

Mirrin's backups are end-to-end encrypted snapshots of a twin. This page is
the whole format: with it and the 12 words, any [age](https://age-encryption.org)
1.3 or newer can open a backup without Mirrin. The code is `internal/backup`;
the design is `docs/cloud-design.md` §9.

## In one paragraph

`mirrin backup init` makes 12 words, shows them once in a Recovery Kit, and
keeps only the public keys they give. Every night at 03:30 while Mirrin is
running (and 10 minutes after a security change) the twin copies its
databases with `VACUUM INTO`,
tars them with its settings, personas, protocols, tools and keys, gzips the
tar and encrypts it with age to the words' key. The machine that writes a
backup can't read it, and neither can wherever it is kept. `mirrin restore`
takes the words, checks everything, and moves the twin in.

## The words

- 12 words from the official BIP-39 English wordlist: 128 bits of entropy
  (called **K**) and a 4-bit checksum, 11 bits a word, as in BIP-39.
- The wordlist is `internal/backup/bip39_english.txt`, copied unchanged from
  `github.com/bitcoin/bips` (`bip-0039/english.txt`, SHA-256
  `2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda`; a test
  checks it). BIP-39 is published under the MIT licence ("License: MIT" in
  `bip-0039.mediawiki`); the same list ships in the reference implementation,
  `trezor/python-mnemonic` (MIT, Copyright (c) 2013-2016 Pavol Rusnak).
- K is the entropy itself. Mirrin does **not** use the BIP-39 PBKDF2 seed.
- K is never written to disk. The words are shown once, then only typed back
  (`restore`, `verify`, `key --age`). A test takes a snapshot and scans the
  home, the data folder and the backup folder for the words, K and every key
  derived from them.
- Typing is forgiving: any case; spaces, commas or new lines; the kit's
  numbers ("1.", "7.") are read as positions, so its two columns can be
  pasted as they are; four letters of a word are enough. A word that isn't
  on the list is named by position with a suggestion; words that are all
  real but fail the checksum are reported as such.

## Keys

Every key is `HKDF-SHA256(ikm = K, salt = "antbot-backup-v1", info, length 32)`:

| info | Key | Encoding |
|---|---|---|
| `age-pq-seed` | the age identity backups are encrypted to: hybrid ML-KEM-768 + X25519 (age 1.3's `mlkem768x25519`, whose private key is this 32-byte seed) | identity `AGE-SECRET-KEY-PQ-1…`, recipient `age1pq1…` (Bech32, as age writes them) |
| `age-x25519` | a classic X25519 age identity from the same words; it opens a backup whose config names an `age1…` recipient | `AGE-SECRET-KEY-1…`, `age1…` |
| `recovery-ed25519` | the Ed25519 recovery key (the 32 bytes are its seed) | public key: base64url, no padding |

The pinned `filippo.io/age` v1.3.2 supports the hybrid recipient, so, as the
design asks, new backups use it: a backup made today stays private against a
future quantum computer. Opening one needs age 1.3 or newer.

- **Namespace:** `ns = base32(sha256(recovery public key, 32 raw bytes))`,
  RFC 4648 alphabet, lower case, no padding, first 26 characters. It names
  the backup's folder, so backups made with other words never mix.
- **Kit ID:** the first eight characters of `ns`, as `abcd-efgh`. It is
  printed on the kit and by `mirrin backup status`, so a kit can be matched to
  a twin without showing the words.

`config.yaml` keeps only the public halves and where backups go. The
`backup` section belongs to this machine: `mirrin identity export` leaves it
out and `mirrin identity import` keeps the importing machine's own, so two
machines never back up into one folder. Keep `backup.recipient` private
anyway (see the note under Manifest).

```yaml
backup:
    recipient: age1pq1…          # 1,959 characters
    recovery_pub: 6lOTY_zGUp_V34wsxNOkad6tlNsk3J24XUwfJg9zw5M
    target: icloud               # or: folder
    path: /Volumes/Backup        # for target: folder
    sessions: false              # true also backs up the WhatsApp session
```

### Golden vectors

These phrases are public BIP-39 test vectors. Never use them for a real twin.
`internal/backup/testdata/vectors.json` holds the same values, and a test
checks the code against them. They were cross-checked with an independent
Python HKDF and OpenSSL's Ed25519 and X25519.

**Phrase A:** `legal winner thank year wave sausage worth useful legal winner thank yellow`

| | |
|---|---|
| K | `7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f` |
| `age-pq-seed` | `ae310ed74c656492ef0cce0841ec1d2604b3b95663b8291eb130deb30dcb3247` |
| age identity | `AGE-SECRET-KEY-PQ-14CCSA46VV4JF9MCVECYYRMQAYCZT8W2KVWUZJ843XR0TXRWTXFRSV3JH62` |
| recipient | starts `age1pq18y5qf4g2wy0cwl0749gxmfutrd5gyqphd`, 1,959 characters, SHA-256 of the string `ede662cc8a2548a7c8ddcbba1333fbb4ece1904664989238e2f7511df00c54b1` |
| `age-x25519` | `f5d3ce2023615cd2b3e94adb385f013c8d33b093ee72b83d98c53f6ef9c30815` |
| X25519 identity | `AGE-SECRET-KEY-17HFUUGPRV9WD9VLFFTDNSHCP8JXN8VYNAEETS0VCC5LKA7WRPQ2SWSLYX9` |
| X25519 recipient | `age12he9vran3eweystsgzf7gf559ndn7t7xw3ztdzae3g8ckf45l3tsh99u5q` |
| `recovery-ed25519` | `8162d3a4339e38a65402aa2dce9685c7aea8c71e4219695f4b481c5b8efd4056` |
| recovery_pub | `6lOTY_zGUp_V34wsxNOkad6tlNsk3J24XUwfJg9zw5M` |
| ns | `xtutvadnogfdegisiijio6tcry` (Kit ID `xtut-vadn`) |

**Phrase B:** `abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about`

| | |
|---|---|
| K | `00000000000000000000000000000000` |
| `age-pq-seed` | `9ba67575e7ef38d9a53585b73cb3d538c3358c9b37b16a12c24b4fca0be2c9c2` |
| age identity | `AGE-SECRET-KEY-PQ-1NWN82A08AUUDNFF4SKMNEV748RPNTRYMX7CK5YKZFD8U5ZLZE8PQEFP2QK` |
| recipient | starts `age1pq1mmez46ltn5sv2t2cawfl85clv49f7sjjw`, 1,959 characters, SHA-256 of the string `fabf66522ba49372118bd6c20e5744b7d0046fadccf6f96f2c84173f09b28657` |
| `age-x25519` | `bbddd8f3d9ca7b5bc12a81293998c1f8a78601924785e0796293068fa1849f80` |
| X25519 identity | `AGE-SECRET-KEY-1H0WA3U7EEFA4HSF2SY5NNXXPLZNCVQVJG7Z7Q7TZJVRGLGVYN7QQPG4U5H` |
| X25519 recipient | `age1z022c458qt0j3duq2xved2r74qr2e5flcx97y73yj754w0ztjf7syceuvu` |
| `recovery-ed25519` | `e12cc74e29c94c6e5501c892f4b94b554f2a010b1226c54e6217e0cd2e727e91` |
| recovery_pub | `Tp4FBafn4kP0iNGUirAh8wmjUf5RMIEsdc8cUXftdIw` |
| ns | `yyqwbuqw4hdtq2lfmrtiqoj4hn` (Kit ID `yyqw-buqw`) |

## A snapshot

**Name:** `snap-20260927T033000Z-1a2b3c4d.age`: the UTC time and 4 random bytes,
nothing about the machine or the twin. The time in the name is only a hint
for sorting and pruning; the authenticated time is in the manifest.

**Bytes:** `age( gzip( tar ) )`, encrypted to `backup.recipient`, binary (not
armored). The tar is POSIX (PAX) with regular files only, no directories, links
or devices. Its entries:

1. `manifest.json`, always first.
2. Each file the manifest lists, in the manifest's order, at the path it
   gives (relative, slash-separated). Mode `0700` for files that were
   executable (custom tools' scripts), else `0600`.

**Manifest:** the same `Manifest` type as `mirrin identity export`
(`internal/identity`), format 2, kind `backup`:

```json
{
  "format": 2,
  "kind": "backup",
  "created": "2026-09-27T03:30:00Z",
  "seq": 412,
  "twin": "Mirrin",
  "host_label": "Akshay's MacBook Pro",
  "version": "0.4.0",
  "os": "darwin",
  "files": [
    {"path": "config.yaml", "size": 2211, "sha256": "…"},
    {"path": "data/memory.db", "size": 4718592, "sha256": "…"}
  ],
  "secrets": true,
  "excluded": ["data/api.token", "data/chrome-profile", "data/whatsapp.db", "logs", "models"],
  "home": "/Users/akshay/.mirrin",
  "user_home": "/Users/akshay",
  "zone": "Australia/Sydney",
  "handover_to": "age1pq1…"
}
```

- `created` and `seq` are inside the encryption, and age authenticates every
  64 KiB chunk, so nobody can change a snapshot's date, number or files
  without the words noticing. But age proves a snapshot was made *for* these
  words, not *who* made it: anyone who has `backup.recipient` and can write
  to the folder can add a snapshot the words open, with any date. So keep
  `backup.recipient` private (it is in `config.yaml`, never in an identity
  export), and a restore never picks a snapshot dated more than a day from
  now unless it is named with `--snapshot`. `seq` counts one twin's
  snapshots and carries on after a restore.
- `files` lists every file with its size and SHA-256; a restore checks each.
- `home` and `user_home` let a restore on another machine move the config's
  paths. `handover_to` is the writing machine's standby key (see Handover).
- `zone` is the writing machine's system time zone. A config saved before
  settings were layered has the install-time zone written in; if it matches
  `zone`, the restored config follows this machine's system instead.
- `secrets: true`: a backup holds the config's keys and passwords. An identity
  export never does (its manifest is `manifest.yaml`, without `kind`).

### What a snapshot holds

| Archive path | From |
|---|---|
| `config.yaml`, `secrets.env` | the home, byte for byte, secrets included |
| `data/memory.db` | the memory, through `VACUUM INTO` (a consistent copy while the twin runs) |
| `personas/…`, `protocols/…`, `tools/…` | those folders (wherever config puts them), minus `node_modules`, `.venv`, `venv`, `__pycache__` and links |
| `tts/<model>.onnx` | wake-word models, from the voice folder or wherever `channels.voice.wake_model` points (not Kokoro's model). A `hey_maverick.onnx` from an earlier release is kept too, since releases no longer carry it |
| `data/devices.json`, `data/push.json`, `data/vapid.pem` | paired devices and push keys, when they exist |
| `data/tls/…` | the ACME account and TLS keys, so certificate pins survive a restore |
| `google-token.json`, `google-credentials.json` | the Google sign-in files (`skills.calendar.*_file`) |
| `data/whatsapp.db` | only with `backup.sessions: true`, through `VACUUM INTO` |

**Names.** Every archive path must pass the check a restore applies:
relative, slash-separated, no `..`, no `:` or `\`, valid UTF-8, and no
Windows device name (`con`, `nul`, `com1`…). A file or folder whose name
fails it (Finder saves "Meeting 10/30" as `Meeting 10:30`) is left out, listed
in `excluded`, logged, and named by `mirrin backup now` and `mirrin backup
status` so the owner can rename it; the rest of the snapshot is saved as
usual. The writer runs the reader's own manifest check before it encrypts,
so Mirrin never saves a snapshot a restore would call damaged.

It works from that allowlist, and some things are refused wherever they turn
up: the browser profile (`data/chrome-profile`, whose cookies are sealed with
this Mac's keychain; Mirrin never reads the Chrome Safe Storage secret),
`api.token`, anything under `data/cloud` and any `device.key`, this machine's
standby key, `models`, `tts`, `logs`, `remote.yaml`, screenshots and voice
clips, photos from chat apps (`data/media`, kept 30 days; a restored
conversation that had one says the photo is no longer on this computer), the
daily memory copies in `data/backups`, and the words, which never exist on
disk. WhatsApp is opt-in because two machines on one linked device
conflict. Signal's session lives in signal-cli's own folder and is not backed
up. A test plants each of these in a twin and checks no snapshot holds it.

## Where snapshots go

A **target** stores objects by name: `Put`, `Get`, `List`, `Delete`. The
snapshots of one set of words live in their namespace folder:

| Target | Folder |
|---|---|
| iCloud Drive (the macOS default) | `~/Library/Mobile Documents/com~apple~CloudDocs/Mirrin Backups/<ns>/` |
| Folder | `<path>/<ns>/` (a USB disk, a NAS, any synced folder) |
| S3-compatible bucket (AWS S3, R2, B2, MinIO, Wasabi) | `<prefix>/<ns>/` in the bucket |
| Mirrin Cloud (planned, not for sale) | `ns/<ns>/` in the service's storage, bound once to the recovery key |

Backups set up before the rename went to `AntBot Backups` in iCloud Drive.
They keep going there while it exists and `Mirrin Backups` doesn't: the
Recovery Kits printed then name it, and a machine standing by watches it.
It is never moved.

The folder the owner chose (or iCloud Drive itself) is made once, by `mirrin
backup init` or `mirrin backup target`, and never again: if it isn't there
at backup time (a USB disk or network share that isn't connected), the run
fails with "the backup folder … isn't there (is its disk or network drive
connected?)" rather than filling a fresh folder on the disk the backups are
meant to protect. Only the `<ns>` folder inside it is made as needed. A
folder inside Mirrin's own folder (`~/.mirrin`, or the data folder) is
refused: it would share the twin's disk, and a restore moves it aside.

Objects are written to a `.partial-*` file, synced and renamed, `0600` in a
`0700` folder. After writing, the twin reads the object back and checks its
SHA-256: it can't decrypt its own backup (it has no words), but it can check
the target holds exactly what was written. Only then does the run count as a
good snapshot.

**Schedule.** Nightly at 03:30 in the owner's time zone, or as soon as the
machine is awake after that (the clock is looked at every 5 minutes), while
the twin is running (the background service or the menu bar app); `mirrin
backup status` says when it isn't. A failed
run is retried after an hour. `BackupSoon(reason)` takes one 10 minutes after
a pairing, a revoke, a passkey enrolment or a reach change; a burst of changes
makes one snapshot. A backup runs while the twin is paused, but never on a
machine standing by. `mirrin backup now` takes one at once. One run at a time
(`data/backup.lock`).

**Retention** (after each run, or `mirrin backup prune`): the newest snapshot
of each of the last 7 days, then of the 4 weeks before those, then of the 6
months before those (borg's rules; a daily history of 400 days keeps exactly
17). The newest snapshot this machine wrote and read back is never pruned.
Names claiming a time more than a day ahead are never counted or deleted.

**Health** (`mirrin doctor`, the menu, `/health`): off until set up; a
warning 48 hours after the last good snapshot and a failure after 7 days
(measured from set-up before the first one); on a machine standing by,
"Standing by: moved to <host>".

This machine's record is `data/backup-state.json` (sequence number, last
good run, last error, files left out for their names, standby, dismissed
markers, when a restore last made this machine the twin). It is not part of
a snapshot.

## Restore

`mirrin restore [--from <folder> | icloud | s3://<bucket>/<folder> | cloud] [--snapshot <name>] [--with-sessions] [--force] [--yes]`

1. Refuse while a twin runs from this home (it answers on the local API or
   holds `data/antbot.lock`), unless `--force`.
2. Take the 12 words; derive the keys.
3. List snapshots by decrypting only as much of each as its manifest needs
   (normally the first 64 KiB chunk), newest first by the authenticated date.
   A snapshot the words don't open is marked so, not guessed at. One dated
   more than a day after now is passed over (and named) unless `--snapshot`
   picks it.
4. Decrypt the chosen one (the newest, or `--snapshot`) into a staging folder
   beside the home, on the same disk. Every path is checked (relative, no
   `..`, no absolute or drive paths, no backslashes, only regular files, each
   listed once in the manifest), every size and SHA-256 matched, nothing
   missing, and the age and gzip streams read to their authenticated end;
   `data/memory.db` must pass SQLite's `quick_check`. Any failure is
   "snapshot damaged", the staging folder is removed and nothing else changes.
   One flipped ciphertext byte is enough to stop it (tested).
5. A WhatsApp session in the snapshot is left out unless `--with-sessions`.
6. The config's folders and sign-in files are pointed at the restored home,
   and paths under the old home or the old user's home are moved to this
   machine's (the file is edited in place: comments and order stay).
7. The twin that was here moves to `~/.mirrin.before-restore-<time>`, never
   deleted, and the staging folder takes its place. What belongs to this
   machine rather than the twin (voice models, logs, the browser profile, the
   WhatsApp link when the snapshot has none, `remote.yaml`, and the standby
   key) is moved over from it, and so are the handover markers the owner
   dismissed.
8. A new `data/api.token` is made: devices that held the old one pair again.
   A Cloud device key is never in a snapshot: `mirrin restore --from cloud`
   makes a new one on this machine.
9. A handover marker is left for the machine the snapshot came from (below),
   unless it came from this machine: its `handover_to` is this machine's
   standby key, or one a twin moved aside here held. The yearly "still have
   your words?" question counts from the restore, since the owner just typed
   them.
10. The report: the date, `seq` and machine of the snapshot, a warning if it is
    more than 48 hours old, the **device review** (the paired devices in the
    snapshot's `devices.json`: revoke any you don't recognise, since a
    snapshot taken before a revoke would bring the device back), and the
    **checklist** (WhatsApp QR, website sign-ins, Google, Signal, voice
    models, re-pairing other devices).

## Handover

Free targets can't move a twin's address, so a restore leaves
`handover-<time>-<rand>.age` where the old machine looks: the target in the
restored config (where that machine backs up), and also the folder the
restore read from when that is another one, such as a copy on a USB disk. If
the old machine's target can't be reached from here, the restore says so and
asks the owner to quit Mirrin there. The old machine stands by when it next
looks (every hour, and before each backup): paused, messaging
channels stopped, no more backups, and its self-check reads "Standing by:
moved to <host>". `mirrin backup resume` makes it the twin again and ignores
that marker from then on.

- Each machine has a standby key, `data/backup-standby.key` (an age hybrid
  identity, made on its first backup, never backed up). Its recipient is the
  manifest's `handover_to`.
- Restoring on the same machine keeps its standby key (it is carried over
  from the twin moved aside), so every snapshot this machine took still names
  a key it holds.
- The marker is age-encrypted to that recipient and to the words' recipient,
  so only the old machine (and the owner) can read where the twin went.
- A marker dated before this machine's last restore is ignored: the twin has
  moved back here since.
- Inside is `{"payload": {...}, "sig": "..."}`. The payload is
  `{"format":1,"kind":"handover","at":…,"host_label":…,"seq":…,"to":"age1pq1…"}`;
  `sig` is the Ed25519 signature, by the recovery key, over
  `"antbot-handover-v1\n"` followed by the payload bytes exactly as stored
  (base64url). The old machine checks it against its `backup.recovery_pub` and
  checks `to` is its own recipient, so a marker written by someone who can
  write to the folder but hasn't got the words is ignored (tested).

With Mirrin Cloud, the handle's generation number does this job instead:
`mirrin restore --from cloud` moves the account to the new machine, and the
old one stands by once the service confirms it.

## Check it yourself with stock age

```sh
mirrin backup key --age > key.txt      # asks for the 12 words; prints AGE-SECRET-KEY-PQ-1…
age -d -i key.txt "Mirrin Backups/<ns>/snap-….age" | tar -tzv
```

`manifest.json` is the first entry. The Go test
`TestStockAgeDecryptsASnapshot` does the same with the `age` on `PATH` (or
`AGE_BIN`), and with the age library from the printed key when there is none.
`scripts/verify-backup-with-age.sh` builds the stock `age` at the version
`go.mod` pins and runs that test with it; CI runs the script.

## Forgetting and backups

Forgetting a fact scrubs the live memory and its local copies. Encrypted
snapshots already written can't be edited: a forgotten fact stays in them
until retention removes them (about six months at most), and a restore brings
back a snapshot as it was. Take a new backup after forgetting something
important, and prune older ones if you need them gone sooner.

After `mirrin backup init --new`, backups go to the new words' folder and
nothing prunes the old words' folder any more: what it holds stays until you
delete it. `init --new` names that folder.

## Dependencies

- `filippo.io/age` v1.3.2, BSD-3-Clause.
- `filippo.io/hpke` v0.4.0 (age's ML-KEM hybrid), BSD-3-Clause.
- The BIP-39 English wordlist, MIT (above).
