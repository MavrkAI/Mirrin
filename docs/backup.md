# Back up your twin

Your twin's backups are end-to-end encrypted on your machine, under a key made
from 12 words that only you hold. Wherever they're kept (a USB disk, iCloud
Drive, an S3 bucket, or later Mirrin Cloud), only ciphertext arrives there,
and nobody but you can open it. This page is how to use them; the exact
format, with golden vectors, is [backup-format.md](backup-format.md), and the
design is `docs/cloud-design.md` §9.

**Status.** Folder, iCloud Drive and S3 backups are in the code and ship in
the next release. Keeping backups with Mirrin Cloud is built too, but Cloud
is planned and not for sale ([cloud.md](cloud.md)).

## Turn it on

```sh
mirrin backup init                            # macOS: iCloud Drive
mirrin backup init --folder /Volumes/Backup   # any folder outside ~/.mirrin: a USB disk, a NAS, a synced folder
mirrin backup init --s3 s3://my-bucket/mirrin # any S3-compatible bucket (options below)
```

Or use menu bar → **Backup…**, which does the same and shows the Recovery
Kit on screen.

`init` shows your **12 words** once, in a Recovery Kit you can print, then
asks you to type one of them back. Write them down and keep them somewhere
safe away from the computer. Mirrin keeps only the public keys the words
make; the words themselves are never written to disk, so **if you lose them,
nobody can open your backups, us included**. `mirrin backup init --new`
makes new words (backups then go to a new folder, and the old words keep
opening the old ones).

## What happens then

- A backup runs every night at 03:30 while Mirrin is running, and about 10
  minutes after a device is paired or revoked, a passkey is set up, the
  reach route changes or a fact is forgotten.
- Each run is checked after it is written: the stored copy is read back and
  its checksum compared.
- Old backups are pruned: the newest of each of the last 7 days, then 4
  weeks, then 6 months are kept, and the newest good one never goes.
- The Health page warns 48 hours after the last good backup and fails after
  7 days.

```sh
mirrin backup status     # where they go, the last one, the next one
mirrin backup now        # one now
mirrin backup list       # what's there
mirrin backup verify     # type your words; checks the newest backup opens and is whole
mirrin backup prune      # prune now instead of after the next run
```

## What's in a backup

Your settings with their keys, your memory, personas, protocols and custom
tools, wake-word models (yours, and a "Hey Maverick" model from an earlier
release, which no release reinstalls now), your paired devices and their
passkeys, push keys, your certificates and their account key (so
certificate pins survive a move), and the Google sign-in.

Left out on purpose: the browser profile (its cookies are sealed to this
computer's keychain; sign in to sites again), the WhatsApp link unless you
ask (`mirrin backup now --with-sessions`, or `backup.sessions: true`: two
computers on one WhatsApp link fight), Signal's session, voice models, logs,
screenshots, this computer's own API key and Cloud key, and the words.

## Where backups can go

| Place | Costs | Set it up |
|---|---|---|
| A folder | Free | `mirrin backup init --folder <path>` or `mirrin backup target folder <path>` |
| iCloud Drive | Free (your iCloud storage); the macOS default | `mirrin backup init --icloud` or `mirrin backup target icloud` |
| An S3-compatible bucket: AWS S3, Cloudflare R2, Backblaze B2, MinIO, Wasabi | Your storage provider's price | `mirrin backup init --s3 s3://<bucket>/<folder>` or `mirrin backup target s3 s3://<bucket>/<folder>` |
| Mirrin Cloud | Planned, not for sale: 20 GB | `mirrin backup init --cloud` or `mirrin backup target cloud`, on a linked machine |

A folder must be outside Mirrin's own folder, and is made once: if its disk
isn't connected at backup time, the run fails and says so rather than filling
the disk the backups are meant to protect.

### S3 options

```sh
mirrin backup target s3 s3://my-bucket/mirrin \
  --endpoint https://<account>.r2.cloudflarestorage.com \
  --access-key-env R2_KEY_ID --secret-key-env R2_SECRET
```

`--endpoint` (empty means AWS in `--region`), `--region`, `--path-style` or
`--virtual-host`, and `--access-key-env` / `--secret-key-env`, which name the
variables that hold the key pair (default `MIRRIN_S3_ACCESS_KEY_ID` and
`MIRRIN_S3_SECRET_ACCESS_KEY`, read from your environment or
`~/.mirrin/secrets.env`). The key pair is never written to `config.yaml`, and
it is checked before your 12 words are shown. Plain `http://` is refused for
a store beyond this computer or your local network. Large backups upload in
parts, and a broken transfer resumes.

## Moving backups

```sh
mirrin backup migrate --to /Volumes/Backup            # from where they go now
mirrin backup migrate --from icloud --to s3://my-bucket/mirrin
```

Every backup is copied as it is, still encrypted, byte for byte, and each copy
is checked. Running it again copies nothing twice. It doesn't change where new
backups go: `mirrin backup target …` does that.

## Restore, or move to a new computer

```sh
mirrin restore                               # from where this config says (on a Mac with none, iCloud Drive)
mirrin restore --from /Volumes/Backup
mirrin restore --from icloud
mirrin restore --from s3://my-bucket/mirrin  # plus the S3 options above
mirrin restore --from cloud                  # needs only your 12 words
```

On a new computer, the Welcome window offers **I already have a twin** and
does the same. Then your twin welcomes you back with what came along (how
many things it remembers, its routines and reminders) and what is still to
do on this Mac, the device review first. `restore`:

1. refuses while a twin runs from this home (`--force` overrides);
2. asks for your 12 words;
3. lists the backups it can open, newest first (`--snapshot <name>` picks
   another);
4. checks every file before it changes anything, and stops at the first
   damaged byte;
5. moves the twin that was here to `~/.mirrin.before-restore-<time>` (never
   deleted) and puts the restored one in its place;
6. shows a **device review**: revoke any device you don't recognise, since a
   backup taken before a revoke brings the device back. Menu bar → **Review
   devices after restore…** (or `mirrin devices review`) opens it again;
7. lists what to redo: the WhatsApp QR, website sign-ins, voice models.

`--with-sessions` also restores a WhatsApp link the backup holds. `--yes`
skips the confirmation.

### The old computer stands by

Two copies of one twin would answer your messages twice. After a restore, the
old computer finds a signed marker in the backup folder and **stands by**:
paused, chat apps stopped, no more backups, and its Health page says where
the twin moved. With Cloud, the address moves instead, and the old machine
stands by once the service confirms it. To make an old computer the twin
again:

```sh
mirrin backup resume
```

## Open a backup without Mirrin

The format is stock [age](https://age-encryption.org) 1.3:

```sh
mirrin backup key --age > key.txt     # asks for your 12 words
age -d -i key.txt snap-….age | tar -tzv
```

Keep `key.txt` as safe as the words, and delete it when you're done.

## Forgetting and backups

Forgetting a fact scrubs your live memory and its daily copies. Encrypted
backups already made can't be edited: the fact stays in them until they're
pruned (about six months at most). A backup runs soon after you forget
something; prune older ones yourself if they must go sooner.

## Settings

| Key | Meaning |
|---|---|
| `backup.target` | `icloud`, `folder`, `s3` or `cloud` |
| `backup.path` | the folder, for `folder` |
| `backup.s3.endpoint`, `.region`, `.bucket`, `.prefix`, `.path_style` | the bucket, for `s3` |
| `backup.s3.access_key_env`, `.secret_key_env` | the variables holding the bucket's key pair |
| `backup.sessions` | also back up the WhatsApp link (off) |
| `backup.recipient`, `backup.recovery_pub` | the public keys your words make; don't edit them |
