// Package store keeps the control plane's state in SQLite (schema.sql):
// accounts, device keys, handles, checkouts, the deny list and a few
// counters. It holds public keys, handles and billing status only: no email,
// no IP address, no content.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// ErrNotFound means no row matched.
var ErrNotFound = errors.New("store: not found")

// Store is the database. All access goes through one connection, so every
// Update is serialized and no transaction waits on another.
type Store struct {
	db *sql.DB
}

// Open opens (creating if need be) the database at path, a file only its
// owner can read.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600); err != nil {
		return nil, err
	} else {
		f.Close()
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Update runs fn in a write transaction, committed if fn returns nil.
func (s *Store) Update(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(&Tx{tx: tx}); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// View runs fn in a transaction that is always rolled back.
func (s *Store) View(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(&Tx{tx: tx})
}

// Tx is one transaction.
type Tx struct {
	tx *sql.Tx
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Account is one paying customer.
type Account struct {
	ID              string
	BillingCustomer string
	Plan            string
	BillingStatus   string // the subscription as the merchant of record last reported it
	PaidThrough     time.Time
	BillingAt       time.Time // when the latest billing event applied happened
	ACMEAccount     string
	DeleteAt        time.Time
	Created         time.Time
}

const accountCols = "id, billing_customer, plan, billing_status, paid_through, billing_at, acme_account, delete_at, created"

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var paid, billing, del, created int64
	err := row.Scan(&a.ID, &a.BillingCustomer, &a.Plan, &a.BillingStatus, &paid, &billing, &a.ACMEAccount, &del, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	a.PaidThrough, a.BillingAt, a.DeleteAt, a.Created = fromUnix(paid), fromUnix(billing), fromUnix(del), fromUnix(created)
	return a, err
}

// Account returns account id.
func (t *Tx) Account(id string) (Account, error) {
	return scanAccount(t.tx.QueryRow("SELECT "+accountCols+" FROM accounts WHERE id = ?", id))
}

// AccountByCustomer returns the account of a merchant-of-record customer.
func (t *Tx) AccountByCustomer(customer string) (Account, error) {
	return scanAccount(t.tx.QueryRow("SELECT "+accountCols+" FROM accounts WHERE billing_customer = ?", customer))
}

// AccountsDeletingBy returns every account whose deletion is due at or
// before now.
func (t *Tx) AccountsDeletingBy(now time.Time) ([]Account, error) {
	rows, err := t.tx.Query("SELECT "+accountCols+" FROM accounts WHERE delete_at != 0 AND delete_at <= ?", unix(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// InsertAccount adds a new account.
func (t *Tx) InsertAccount(a Account) error {
	_, err := t.tx.Exec("INSERT INTO accounts ("+accountCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		a.ID, a.BillingCustomer, a.Plan, a.BillingStatus, unix(a.PaidThrough), unix(a.BillingAt), a.ACMEAccount, unix(a.DeleteAt), unix(a.Created))
	return err
}

// UpdateAccount writes every field of a but its id and creation time.
func (t *Tx) UpdateAccount(a Account) error {
	return one(t.tx.Exec(`UPDATE accounts SET billing_customer = ?, plan = ?, billing_status = ?, paid_through = ?, billing_at = ?,
		acme_account = ?, delete_at = ? WHERE id = ?`,
		a.BillingCustomer, a.Plan, a.BillingStatus, unix(a.PaidThrough), unix(a.BillingAt), a.ACMEAccount, unix(a.DeleteAt), a.ID))
}

// one insists that an UPDATE changed exactly one row.
func one(r sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

// Device is one device key of an account.
type Device struct {
	Pub         string
	KeyID       string
	Account     string
	Gen         int64
	Created     time.Time
	LastSeenDay string
	Revoked     bool
}

const deviceCols = "pub, key_id, account, gen, created, last_seen_day, revoked"

func scanDevice(row interface{ Scan(...any) error }) (Device, error) {
	var d Device
	var created int64
	var revoked int
	err := row.Scan(&d.Pub, &d.KeyID, &d.Account, &d.Gen, &created, &d.LastSeenDay, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	d.Created, d.Revoked = fromUnix(created), revoked != 0
	return d, err
}

// DeviceByKeyID returns the device whose key has this httpsig key id.
func (t *Tx) DeviceByKeyID(keyID string) (Device, error) {
	return scanDevice(t.tx.QueryRow("SELECT "+deviceCols+" FROM devices WHERE key_id = ?", keyID))
}

// Devices returns an account's devices.
func (t *Tx) Devices(account string) ([]Device, error) {
	rows, err := t.tx.Query("SELECT "+deviceCols+" FROM devices WHERE account = ? ORDER BY pub", account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// InsertDevice adds a device key.
func (t *Tx) InsertDevice(d Device) error {
	_, err := t.tx.Exec("INSERT INTO devices ("+deviceCols+") VALUES (?, ?, ?, ?, ?, ?, ?)",
		d.Pub, d.KeyID, d.Account, d.Gen, unix(d.Created), d.LastSeenDay, b2i(d.Revoked))
	return err
}

// UpdateDevice writes a device's generation, last day seen and revocation.
func (t *Tx) UpdateDevice(d Device) error {
	return one(t.tx.Exec("UPDATE devices SET gen = ?, last_seen_day = ?, revoked = ? WHERE pub = ?", d.Gen, d.LastSeenDay, b2i(d.Revoked), d.Pub))
}

// Handle is a name under the tenant zone.
type Handle struct {
	Name       string
	Skeleton   string
	Account    string // "" once released
	Gen        int64
	GenAt      time.Time
	BYOD       bool
	Created    time.Time
	Released   time.Time
	DNSPending bool
}

const handleCols = "name, skeleton, account, gen, gen_at, byod, created, released, dns_pending"

func scanHandle(row interface{ Scan(...any) error }) (Handle, error) {
	var h Handle
	var account sql.NullString
	var genAt, created, released int64
	var byod, pending int
	err := row.Scan(&h.Name, &h.Skeleton, &account, &h.Gen, &genAt, &byod, &created, &released, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	h.Account, h.GenAt, h.BYOD, h.Created, h.Released, h.DNSPending = account.String, fromUnix(genAt), byod != 0, fromUnix(created), fromUnix(released), pending != 0
	return h, err
}

// Handle returns handle name.
func (t *Tx) Handle(name string) (Handle, error) {
	return scanHandle(t.tx.QueryRow("SELECT "+handleCols+" FROM handles WHERE name = ?", name))
}

// HandleBySkeleton returns the handle that folds to skeleton.
func (t *Tx) HandleBySkeleton(skeleton string) (Handle, error) {
	return scanHandle(t.tx.QueryRow("SELECT "+handleCols+" FROM handles WHERE skeleton = ?", skeleton))
}

// AccountHandle returns an account's handle.
func (t *Tx) AccountHandle(account string) (Handle, error) {
	return scanHandle(t.tx.QueryRow("SELECT "+handleCols+" FROM handles WHERE account = ? ORDER BY created LIMIT 1", account))
}

// HandlesPendingDNS returns the handles whose records wait to be written.
func (t *Tx) HandlesPendingDNS() ([]Handle, error) {
	rows, err := t.tx.Query("SELECT " + handleCols + " FROM handles WHERE dns_pending != 0 ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Handle
	for rows.Next() {
		h, err := scanHandle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// CountHandlesSince counts the handles created at or after t.
func (t *Tx) CountHandlesSince(since time.Time) (int, error) {
	var n int
	err := t.tx.QueryRow("SELECT COUNT(*) FROM handles WHERE created >= ?", unix(since)).Scan(&n)
	return n, err
}

// InsertHandle adds a handle.
func (t *Tx) InsertHandle(h Handle) error {
	_, err := t.tx.Exec("INSERT INTO handles ("+handleCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		h.Name, h.Skeleton, nullable(h.Account), h.Gen, unix(h.GenAt), b2i(h.BYOD), unix(h.Created), unix(h.Released), b2i(h.DNSPending))
	return err
}

// UpdateHandle writes every field of h but its name, skeleton and creation.
func (t *Tx) UpdateHandle(h Handle) error {
	return one(t.tx.Exec("UPDATE handles SET account = ?, gen = ?, gen_at = ?, byod = ?, released = ?, dns_pending = ? WHERE name = ?",
		nullable(h.Account), h.Gen, unix(h.GenAt), b2i(h.BYOD), unix(h.Released), b2i(h.DNSPending), h.Name))
}

// Link is one checkout.
type Link struct {
	ID          string
	DevicePub   string
	KeyID       string
	Handle      string
	ACMEAccount string
	Status      string
	Account     string // "" until paid
	Expires     time.Time
	Created     time.Time
}

const linkCols = "id, device_pub, key_id, handle, acme_account, status, account, expires, created"

func scanLink(row interface{ Scan(...any) error }) (Link, error) {
	var l Link
	var account sql.NullString
	var expires, created int64
	err := row.Scan(&l.ID, &l.DevicePub, &l.KeyID, &l.Handle, &l.ACMEAccount, &l.Status, &account, &expires, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	l.Account, l.Expires, l.Created = account.String, fromUnix(expires), fromUnix(created)
	return l, err
}

// Link returns link id.
func (t *Tx) Link(id string) (Link, error) {
	return scanLink(t.tx.QueryRow("SELECT "+linkCols+" FROM links WHERE id = ?", id))
}

// LinksByKeyID returns every link started by a key.
func (t *Tx) LinksByKeyID(keyID string) ([]Link, error) {
	return t.links("SELECT "+linkCols+" FROM links WHERE key_id = ? ORDER BY created", keyID)
}

// PendingLinks returns the links still waiting for payment at now.
func (t *Tx) PendingLinks(now time.Time) ([]Link, error) {
	return t.links("SELECT "+linkCols+" FROM links WHERE status = 'pending' AND expires > ? ORDER BY created", unix(now))
}

func (t *Tx) links(q string, args ...any) ([]Link, error) {
	rows, err := t.tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Link
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// InsertLink adds a checkout.
func (t *Tx) InsertLink(l Link) error {
	_, err := t.tx.Exec("INSERT INTO links ("+linkCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		l.ID, l.DevicePub, l.KeyID, l.Handle, l.ACMEAccount, l.Status, nullable(l.Account), unix(l.Expires), unix(l.Created))
	return err
}

// UpdateLink writes a link's status and account.
func (t *Tx) UpdateLink(l Link) error {
	return one(t.tx.Exec("UPDATE links SET status = ?, account = ? WHERE id = ?", l.Status, nullable(l.Account), l.ID))
}

// DeleteLink removes a checkout.
func (t *Tx) DeleteLink(id string) error {
	_, err := t.tx.Exec("DELETE FROM links WHERE id = ?", id)
	return err
}

// CountPendingLinks counts the links still waiting for payment at now.
func (t *Tx) CountPendingLinks(now time.Time) (int, error) {
	var n int
	err := t.tx.QueryRow("SELECT COUNT(*) FROM links WHERE status = 'pending' AND expires > ?", unix(now)).Scan(&n)
	return n, err
}

// ExpireLinks marks the unpaid links past their hour as expired, and drops
// the expired and refused links of no account that expired before forget
// (they name a key that never linked).
func (t *Tx) ExpireLinks(now, forget time.Time) error {
	if _, err := t.tx.Exec("UPDATE links SET status = 'expired' WHERE status = 'pending' AND expires <= ?", unix(now)); err != nil {
		return err
	}
	_, err := t.tx.Exec("DELETE FROM links WHERE status IN ('expired', 'refused') AND account IS NULL AND expires <= ?", unix(forget))
	return err
}

// AddEvent records that something happened to an account.
func (t *Tx) AddEvent(account, kind string, at time.Time) error {
	_, err := t.tx.Exec("INSERT INTO events (account, kind, at) VALUES (?, ?, ?)", account, kind, unix(at))
	return err
}

// Deny kinds.
const (
	DenyHandle = "handle"
	DenyKey    = "key"
)

// DenyEntry is one row of the deny list.
type DenyEntry struct {
	Kind    string
	Value   string
	KeyID   string
	Why     string
	Seq     int64
	Created time.Time
}

const denyCols = "kind, value, key_id, why, seq, created"

func scanDeny(row interface{ Scan(...any) error }) (DenyEntry, error) {
	var e DenyEntry
	var created int64
	err := row.Scan(&e.Kind, &e.Value, &e.KeyID, &e.Why, &e.Seq, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	e.Created = fromUnix(created)
	return e, err
}

// Deny puts a handle or key on the deny list, and reports whether it was
// not there already. The list's seq goes up with every entry.
func (t *Tx) Deny(e DenyEntry) (bool, error) {
	if e.Kind != DenyHandle && e.Kind != DenyKey {
		return false, fmt.Errorf("store: deny kind %q", e.Kind)
	}
	var n int
	if err := t.tx.QueryRow("SELECT COUNT(*) FROM deny WHERE kind = ? AND value = ?", e.Kind, e.Value).Scan(&n); err != nil || n > 0 {
		return false, err
	}
	seq, err := t.bump("deny_seq")
	if err != nil {
		return false, err
	}
	// The first list to carry the entry has seq one more than the count of
	// changes (see DenyList).
	_, err = t.tx.Exec("INSERT INTO deny ("+denyCols+") VALUES (?, ?, ?, ?, ?, ?)", e.Kind, e.Value, e.KeyID, e.Why, seq+1, unix(e.Created))
	return err == nil, err
}

// DeniedKey returns the deny entry for a key id.
func (t *Tx) DeniedKey(keyID string) (DenyEntry, error) {
	return scanDeny(t.tx.QueryRow("SELECT "+denyCols+" FROM deny WHERE kind = 'key' AND key_id = ?", keyID))
}

// Denied returns the entry for a handle or key value.
func (t *Tx) Denied(kind, value string) (DenyEntry, error) {
	return scanDeny(t.tx.QueryRow("SELECT "+denyCols+" FROM deny WHERE kind = ? AND value = ?", kind, value))
}

// DenyList returns the list as published: its seq, one more than the number
// of changes so far (an entry added, or entries aging out), and the entries
// made after the window's edge, which MoveDenyEdge moves. The same seq
// always means the same entries.
func (t *Tx) DenyList() (int64, []DenyEntry, error) {
	seq, err := t.meta("deny_seq")
	if err != nil {
		return 0, nil, err
	}
	edge, err := t.meta("deny_edge")
	if err != nil {
		return 0, nil, err
	}
	rows, err := t.tx.Query("SELECT "+denyCols+" FROM deny WHERE created > ? ORDER BY kind, value", edge)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var out []DenyEntry
	for rows.Next() {
		e, err := scanDeny(rows)
		if err != nil {
			return 0, nil, err
		}
		out = append(out, e)
	}
	return seq + 1, out, rows.Err()
}

// MoveDenyEdge drops the entries made at or before edge from the published
// list, raising its seq if any went, and forgets entries made before
// forget.
func (t *Tx) MoveDenyEdge(edge, forget time.Time) error {
	old, err := t.meta("deny_edge")
	if err != nil {
		return err
	}
	if unix(edge) <= old {
		return nil
	}
	var n int
	if err := t.tx.QueryRow("SELECT COUNT(*) FROM deny WHERE created > ? AND created <= ?", old, unix(edge)).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		if _, err := t.bump("deny_seq"); err != nil {
			return err
		}
	}
	if err := t.setMeta("deny_edge", unix(edge)); err != nil {
		return err
	}
	_, err = t.tx.Exec("DELETE FROM deny WHERE created < ? AND created <= ?", unix(forget), unix(edge))
	return err
}

func (t *Tx) meta(k string) (int64, error) {
	var v int64
	err := t.tx.QueryRow("SELECT v FROM meta WHERE k = ?", k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

func (t *Tx) setMeta(k string, v int64) error {
	_, err := t.tx.Exec("INSERT INTO meta (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v", k, v)
	return err
}

func (t *Tx) bump(k string) (int64, error) {
	v, err := t.meta(k)
	if err != nil {
		return 0, err
	}
	return v + 1, t.setMeta(k, v+1)
}

// SeenBillingEvent records a merchant-of-record event id, and reports
// whether it was recorded before.
func (t *Tx) SeenBillingEvent(id string, occurred time.Time) (bool, error) {
	r, err := t.tx.Exec("INSERT INTO billing_events (event_id, occurred) VALUES (?, ?) ON CONFLICT (event_id) DO NOTHING", id, unix(occurred))
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 0, err
}

// ForgetBillingEvents drops event ids that occurred before t.
func (t *Tx) ForgetBillingEvents(before time.Time) error {
	_, err := t.tx.Exec("DELETE FROM billing_events WHERE occurred < ?", unix(before))
	return err
}

// DeleteAccount removes an account's rows and releases its handle, which
// keeps its row so the name is never given to anyone else.
func (t *Tx) DeleteAccount(id string, now time.Time) error {
	if _, err := t.tx.Exec("UPDATE handles SET account = NULL, released = ?, dns_pending = 1 WHERE account = ?", unix(now), id); err != nil {
		return err
	}
	for _, q := range []string{
		"DELETE FROM objects WHERE ns IN (SELECT ns FROM namespaces WHERE account = ?)",
		"DELETE FROM uploads WHERE ns IN (SELECT ns FROM namespaces WHERE account = ?)",
		"DELETE FROM tombstones WHERE ns IN (SELECT ns FROM namespaces WHERE account = ?)",
		"DELETE FROM namespaces WHERE account = ?",
		"DELETE FROM devices WHERE account = ?",
		"DELETE FROM links WHERE account = ?",
		"DELETE FROM events WHERE account = ?",
		"DELETE FROM accounts WHERE id = ?",
	} {
		if _, err := t.tx.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}

// Row is one row as column → value, for /v1/me and admin show.
type Row map[string]any

// Rows returns every row of table where column equals value, all columns.
// Table and column must be names from the schema.
func (t *Tx) Rows(table, column string, value any) ([]Row, error) {
	if !isName(table) || !isName(column) {
		return nil, fmt.Errorf("store: bad name %q.%q", table, column)
	}
	rows, err := t.tx.Query("SELECT * FROM "+table+" WHERE "+column+" = ? ORDER BY rowid", value)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}

func scanRows(rows *sql.Rows) ([]Row, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []Row
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := Row{}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				vals[i] = string(b)
			}
			r[c] = vals[i]
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func isName(s string) bool {
	return s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyz_") == ""
}

// Table is a table's name and columns, as SQLite reports them.
type Table struct {
	Name    string
	Columns []string
}

// Schema lists every table and its columns.
func (t *Tx) Schema() ([]Table, error) {
	rows, err := t.tx.Query("SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	var out []Table
	for _, n := range names {
		if !isName(n) {
			return nil, fmt.Errorf("store: unexpected table %q", n)
		}
		cr, err := t.tx.Query("SELECT name FROM pragma_table_info(?) ORDER BY cid", n)
		if err != nil {
			return nil, err
		}
		tb := Table{Name: n}
		for cr.Next() {
			var c string
			if err := cr.Scan(&c); err != nil {
				cr.Close()
				return nil, err
			}
			tb.Columns = append(tb.Columns, c)
		}
		cr.Close()
		out = append(out, tb)
	}
	return out, nil
}

// Dump returns every row of every table, for tests that look for what must
// never be stored.
func (t *Tx) Dump() (map[string][]Row, error) {
	tables, err := t.Schema()
	if err != nil {
		return nil, err
	}
	out := map[string][]Row{}
	for _, tb := range tables {
		rows, err := t.tx.Query("SELECT * FROM " + tb.Name)
		if err != nil {
			return nil, err
		}
		r, err := scanRows(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		out[tb.Name] = r
	}
	return out, nil
}

// AccountLinked reports whether a table holds rows about one account: the
// accounts table, and every table with an account column.
func AccountLinked(tb Table) bool {
	return tb.Name == "accounts" || slices.Contains(tb.Columns, "account")
}
