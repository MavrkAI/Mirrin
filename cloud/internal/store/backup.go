package store

import (
	"database/sql"
	"errors"
	"time"
)

// Namespace is one account's backup namespace.
type Namespace struct {
	NS          string
	RecoveryPub string
	Account     string
	Bytes       int64
	Created     time.Time
}

const namespaceCols = "ns, recovery_pub, account, bytes, created"

func scanNamespace(row interface{ Scan(...any) error }) (Namespace, error) {
	var n Namespace
	var created int64
	err := row.Scan(&n.NS, &n.RecoveryPub, &n.Account, &n.Bytes, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return n, ErrNotFound
	}
	n.Created = fromUnix(created)
	return n, err
}

// Namespace returns namespace ns.
func (t *Tx) Namespace(ns string) (Namespace, error) {
	return scanNamespace(t.tx.QueryRow("SELECT "+namespaceCols+" FROM namespaces WHERE ns = ?", ns))
}

// AccountNamespace returns an account's namespace: an account has one.
func (t *Tx) AccountNamespace(account string) (Namespace, error) {
	return scanNamespace(t.tx.QueryRow("SELECT "+namespaceCols+" FROM namespaces WHERE account = ? ORDER BY created DESC, ns LIMIT 1", account))
}

// AccountNamespaces returns every namespace of an account.
func (t *Tx) AccountNamespaces(account string) ([]Namespace, error) {
	rows, err := t.tx.Query("SELECT "+namespaceCols+" FROM namespaces WHERE account = ? ORDER BY ns", account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Namespace
	for rows.Next() {
		n, err := scanNamespace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// InsertNamespace binds a namespace to an account.
func (t *Tx) InsertNamespace(n Namespace) error {
	_, err := t.tx.Exec("INSERT INTO namespaces ("+namespaceCols+") VALUES (?, ?, ?, ?, ?)",
		n.NS, n.RecoveryPub, n.Account, n.Bytes, unix(n.Created))
	return err
}

// DeleteNamespace removes an empty namespace's row.
func (t *Tx) DeleteNamespace(ns string) error {
	for _, q := range []string{"DELETE FROM objects WHERE ns = ?", "DELETE FROM uploads WHERE ns = ?", "DELETE FROM tombstones WHERE ns = ?"} {
		if _, err := t.tx.Exec(q, ns); err != nil {
			return err
		}
	}
	_, err := t.tx.Exec("DELETE FROM namespaces WHERE ns = ?", ns)
	return err
}

// StoredObject is one committed backup object.
type StoredObject struct {
	NS      string
	Name    string
	Size    int64
	Created time.Time
}

// Objects returns a namespace's objects, by name.
func (t *Tx) Objects(ns string) ([]StoredObject, error) {
	rows, err := t.tx.Query("SELECT ns, name, size, created FROM objects WHERE ns = ? ORDER BY name", ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredObject
	for rows.Next() {
		var o StoredObject
		var created int64
		if err := rows.Scan(&o.NS, &o.Name, &o.Size, &created); err != nil {
			return nil, err
		}
		o.Created = fromUnix(created)
		out = append(out, o)
	}
	return out, rows.Err()
}

// Object returns one object.
func (t *Tx) Object(ns, name string) (StoredObject, error) {
	var o StoredObject
	var created int64
	err := t.tx.QueryRow("SELECT ns, name, size, created FROM objects WHERE ns = ? AND name = ?", ns, name).Scan(&o.NS, &o.Name, &o.Size, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	o.Created = fromUnix(created)
	return o, err
}

// InsertObject records a committed object and adds its size to the
// namespace's bytes.
func (t *Tx) InsertObject(o StoredObject) error {
	if _, err := t.tx.Exec("INSERT INTO objects (ns, name, size, created) VALUES (?, ?, ?, ?)", o.NS, o.Name, o.Size, unix(o.Created)); err != nil {
		return err
	}
	return one(t.tx.Exec("UPDATE namespaces SET bytes = bytes + ? WHERE ns = ?", o.Size, o.NS))
}

// DeleteObject forgets an object, taking its size off the namespace's
// bytes, and reports whether there was one.
func (t *Tx) DeleteObject(ns, name string) (bool, error) {
	o, err := t.Object(ns, name)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := t.tx.Exec("DELETE FROM objects WHERE ns = ? AND name = ?", ns, name); err != nil {
		return false, err
	}
	return true, one(t.tx.Exec("UPDATE namespaces SET bytes = max(0, bytes - ?) WHERE ns = ?", o.Size, ns))
}

// Upload is a put presigned and not yet committed.
type Upload struct {
	NS      string
	Name    string
	Size    int64
	Expires time.Time
}

// Upload returns the pending upload of name, or ErrNotFound.
func (t *Tx) Upload(ns, name string) (Upload, error) {
	u := Upload{NS: ns, Name: name}
	var exp int64
	err := t.tx.QueryRow("SELECT size, expires FROM uploads WHERE ns = ? AND name = ?", ns, name).Scan(&u.Size, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	u.Expires = fromUnix(exp)
	return u, err
}

// PutUpload records a pending upload, or moves an existing one's expiry.
func (t *Tx) PutUpload(u Upload) error {
	_, err := t.tx.Exec("INSERT INTO uploads (ns, name, size, expires) VALUES (?, ?, ?, ?) ON CONFLICT (ns, name) DO UPDATE SET size = excluded.size, expires = excluded.expires",
		u.NS, u.Name, u.Size, unix(u.Expires))
	return err
}

// DeleteUpload forgets a pending upload and reports whether there was one.
func (t *Tx) DeleteUpload(ns, name string) (bool, error) {
	r, err := t.tx.Exec("DELETE FROM uploads WHERE ns = ? AND name = ?", ns, name)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

// UploadsExpiredBy returns the pending uploads whose URL ran out before t.
func (t *Tx) UploadsExpiredBy(at time.Time) ([]Upload, error) {
	rows, err := t.tx.Query("SELECT ns, name, size, expires FROM uploads WHERE expires < ? ORDER BY expires LIMIT 1000", unix(at))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Upload
	for rows.Next() {
		var u Upload
		var exp int64
		if err := rows.Scan(&u.NS, &u.Name, &u.Size, &exp); err != nil {
			return nil, err
		}
		u.Expires = fromUnix(exp)
		out = append(out, u)
	}
	return out, rows.Err()
}

// Tombstone is a name removed from a namespace, never to be used again.
// Size is held against the quota until storage is rid of the object; Due,
// when not zero, is when the key is deleted (again).
type Tombstone struct {
	NS      string
	Name    string
	Size    int64
	Due     time.Time
	Created time.Time
}

// HasTombstone reports whether name was removed from ns.
func (t *Tx) HasTombstone(ns, name string) (bool, error) {
	var n int
	err := t.tx.QueryRow("SELECT count(*) FROM tombstones WHERE ns = ? AND name = ?", ns, name).Scan(&n)
	return n > 0, err
}

// AddTombstone records a removed name. For a name already removed, the
// larger size and the later due are kept.
func (t *Tx) AddTombstone(tb Tombstone) error {
	var due any
	if !tb.Due.IsZero() {
		due = unix(tb.Due)
	}
	_, err := t.tx.Exec(`INSERT INTO tombstones (ns, name, size, due, created) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (ns, name) DO UPDATE SET size = max(size, excluded.size),
		due = CASE WHEN due IS NULL THEN excluded.due WHEN excluded.due IS NULL THEN due ELSE max(due, excluded.due) END`,
		tb.NS, tb.Name, tb.Size, due, unix(tb.Created))
	return err
}

// ReleaseTombstone stops holding a removed name's size against the quota:
// storage no longer has it. With done, nothing is left to delete either.
func (t *Tx) ReleaseTombstone(ns, name string, done bool) error {
	q := "UPDATE tombstones SET size = 0 WHERE ns = ? AND name = ?"
	if done {
		q = "UPDATE tombstones SET size = 0, due = NULL WHERE ns = ? AND name = ?"
	}
	_, err := t.tx.Exec(q, ns, name)
	return err
}

// TombstonesDueBy returns the removed names whose keys are to be deleted by
// t.
func (t *Tx) TombstonesDueBy(at time.Time) ([]Tombstone, error) {
	rows, err := t.tx.Query("SELECT ns, name, size, due, created FROM tombstones WHERE due IS NOT NULL AND due <= ? ORDER BY due LIMIT 1000", unix(at))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tombstone
	for rows.Next() {
		var tb Tombstone
		var due, created int64
		if err := rows.Scan(&tb.NS, &tb.Name, &tb.Size, &due, &created); err != nil {
			return nil, err
		}
		tb.Due, tb.Created = fromUnix(due), fromUnix(created)
		out = append(out, tb)
	}
	return out, rows.Err()
}

// Held is what a namespace's pending uploads and not yet deleted objects
// hold against its quota, beside its committed bytes.
func (t *Tx) Held(ns string) (int64, error) {
	var up, tomb int64
	if err := t.tx.QueryRow("SELECT coalesce(sum(size), 0) FROM uploads WHERE ns = ?", ns).Scan(&up); err != nil {
		return 0, err
	}
	if err := t.tx.QueryRow("SELECT coalesce(sum(size), 0) FROM tombstones WHERE ns = ?", ns).Scan(&tomb); err != nil {
		return 0, err
	}
	return up + tomb, nil
}
