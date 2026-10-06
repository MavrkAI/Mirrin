-- The control plane's whole memory. Times are Unix seconds (0 for none).
-- There is no email column and no IP column: the email lives at the
-- merchant of record, and addresses are never stored. GET /v1/me shows every
-- column of every table that names an account (me_coverage_test.go).

-- One per paying customer.
CREATE TABLE IF NOT EXISTS accounts (
	id               TEXT PRIMARY KEY,           -- acct_…
	billing_customer TEXT NOT NULL UNIQUE,       -- the merchant of record's customer id
	plan             TEXT NOT NULL,
	billing_status   TEXT NOT NULL,              -- the subscription as the merchant of record last reported it
	paid_through     INTEGER NOT NULL,           -- end of the paid period
	billing_at       INTEGER NOT NULL,           -- when the latest billing event applied happened
	acme_account     TEXT NOT NULL DEFAULT '',   -- the ACME account URI the handle's CAA names
	delete_at        INTEGER NOT NULL DEFAULT 0, -- deletion scheduled for this time
	created          INTEGER NOT NULL
);

-- Device keys (Ed25519, unpadded base64url) that sign for an account.
CREATE TABLE IF NOT EXISTS devices (
	pub           TEXT PRIMARY KEY,
	key_id        TEXT NOT NULL UNIQUE,          -- httpsig.KeyID(pub), for lookups
	account       TEXT NOT NULL REFERENCES accounts(id),
	gen           INTEGER NOT NULL,              -- the handle generation this device holds
	created       INTEGER NOT NULL,
	last_seen_day TEXT NOT NULL,                 -- YYYY-MM-DD, the only activity kept
	revoked       INTEGER NOT NULL DEFAULT 0     -- 1 once unlinked or revoked
);

-- Handles are never reassigned: a row stays after its account is deleted,
-- with released set and no account.
CREATE TABLE IF NOT EXISTS handles (
	name        TEXT PRIMARY KEY,
	skeleton    TEXT NOT NULL UNIQUE,            -- the name with look-alike characters folded
	account     TEXT REFERENCES accounts(id),
	gen         INTEGER NOT NULL,
	gen_at      INTEGER NOT NULL,
	byod        INTEGER NOT NULL DEFAULT 0,
	created     INTEGER NOT NULL,
	released    INTEGER NOT NULL DEFAULT 0,
	dns_pending INTEGER NOT NULL DEFAULT 0       -- 1 while the DNS records wait to be written
);

-- Checkouts. A pending link holds its handle for an hour. An unpaid link is
-- kept for 30 days after it expires, so a late payment still finds it.
CREATE TABLE IF NOT EXISTS links (
	id           TEXT PRIMARY KEY,               -- lk_…
	device_pub   TEXT NOT NULL,
	key_id       TEXT NOT NULL,
	handle       TEXT NOT NULL DEFAULT '',       -- asked for; '' picks one at random
	acme_account TEXT NOT NULL DEFAULT '',
	status       TEXT NOT NULL,                  -- pending, active, expired, refused (paid, but cancelled and refunded)
	account      TEXT REFERENCES accounts(id),   -- set once paid
	expires      INTEGER NOT NULL,
	created      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS links_key_id ON links(key_id);

-- The deny list: handles and device keys refused before their entitlements
-- expire. seq is the list's sequence number when the entry went in.
CREATE TABLE IF NOT EXISTS deny (
	kind    TEXT NOT NULL,                       -- handle or key
	value   TEXT NOT NULL,
	key_id  TEXT NOT NULL DEFAULT '',            -- for keys, httpsig.KeyID(value)
	why     TEXT NOT NULL,
	seq     INTEGER NOT NULL,
	created INTEGER NOT NULL,
	PRIMARY KEY (kind, value)
);
CREATE INDEX IF NOT EXISTS deny_key_id ON deny(key_id);

-- What happened to an account, never with content.
CREATE TABLE IF NOT EXISTS events (
	id      INTEGER PRIMARY KEY,
	account TEXT NOT NULL REFERENCES accounts(id),
	kind    TEXT NOT NULL,
	at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS events_account ON events(account);

-- Merchant-of-record events already applied, so a retry is a no-op.
CREATE TABLE IF NOT EXISTS billing_events (
	event_id TEXT PRIMARY KEY,
	occurred INTEGER NOT NULL
);

-- Counters: the deny list's seq and the edge of its window.
CREATE TABLE IF NOT EXISTS meta (
	k TEXT PRIMARY KEY,
	v INTEGER NOT NULL
);

-- Backup namespaces (docs/cloud-design.md §9): an account's backups are
-- kept in storage under ns/<ns>/, bound once with a signature by the
-- recovery key derived from the owner's 12 words. bytes is what its
-- committed objects add up to, for the quota.
CREATE TABLE IF NOT EXISTS namespaces (
	ns           TEXT PRIMARY KEY,               -- base32 of the recovery key's SHA-256, 26 characters
	recovery_pub TEXT NOT NULL,                  -- the recovery public key, unpadded base64url
	account      TEXT NOT NULL REFERENCES accounts(id),
	bytes        INTEGER NOT NULL DEFAULT 0,
	created      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS namespaces_account ON namespaces(account);

-- Backup objects the control plane checked with a HEAD after upload. Only
-- names and sizes: the contents are ciphertext it never reads.
CREATE TABLE IF NOT EXISTS objects (
	ns      TEXT NOT NULL REFERENCES namespaces(ns),
	name    TEXT NOT NULL,                       -- snap-… or handover-…
	size    INTEGER NOT NULL,
	created INTEGER NOT NULL,
	PRIMARY KEY (ns, name)
);

-- Uploads presigned and not yet committed. Each holds its size against the
-- namespace's quota from the presign until it is committed, deleted, or
-- swept a day after its URL ran out (docs/cloud-design.md §9: quota is
-- enforced at presign).
CREATE TABLE IF NOT EXISTS uploads (
	ns      TEXT NOT NULL REFERENCES namespaces(ns),
	name    TEXT NOT NULL,
	size    INTEGER NOT NULL,
	expires INTEGER NOT NULL,                    -- when the PUT URL runs out
	PRIMARY KEY (ns, name)
);

-- Names removed from a namespace: never presigned again. size is held
-- against the quota until storage is known to be rid of the object; due is
-- when the sweep deletes the key (again), in case a URL handed out earlier
-- put something back. Both are cleared once that is done.
CREATE TABLE IF NOT EXISTS tombstones (
	ns      TEXT NOT NULL REFERENCES namespaces(ns),
	name    TEXT NOT NULL,
	size    INTEGER NOT NULL DEFAULT 0,
	due     INTEGER,                             -- NULL once nothing is left to delete
	created INTEGER NOT NULL,
	PRIMARY KEY (ns, name)
);
CREATE INDEX IF NOT EXISTS tombstones_due ON tombstones(due) WHERE due IS NOT NULL;
