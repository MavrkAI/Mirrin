// Package backuptarget keeps backups with Mirrin Cloud, the optional paid
// service: a backup.Target whose objects go straight to the service's
// storage at URLs its control plane presigns for this machine. Like every
// target, it only ever carries ciphertext the owner's 12 words open, under
// names that hold a time and nothing else.
//
// It lives under internal/cloud, so only the wiring (cmd/mirrin and
// internal/daemon) can reach it; internal/backup never imports it and sees
// it through backup.OpenCloud.
package backuptarget

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/cloud"
)

// Where is the target's name, as the owner sees it.
const Where = "Mirrin Cloud"

type target struct {
	c  *cloud.Client
	ns string // the namespace the backups must be kept under, or "" for any

	mu      sync.Mutex
	checked bool // the account's namespace was found to be ns
}

// New is the paid service's backup target for the linked machine c. Its
// namespace is the one bound to the account (Bind).
func New(c *cloud.Client) backup.Target { return &target{c: c} }

// NewFor is New for the backups of namespace ns: before it stores, lists
// or fetches anything, it checks that the account's bound namespace is ns,
// so backups encrypted to one set of words never go where another set's
// recovery would look.
func NewFor(c *cloud.Client, ns string) backup.Target { return &target{c: c, ns: ns} }

// check makes sure, once, that the account keeps its backups under t.ns.
// A list already fetched (l) saves a request.
func (t *target) check(ctx context.Context, l *cloud.BackupListing) error {
	if t.ns == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.checked {
		return nil
	}
	if l == nil {
		got, err := t.c.BackupList(ctx)
		if err != nil {
			return explain(err, false)
		}
		l = &got
	}
	if l.NS != t.ns {
		return errors.New("Mirrin Cloud keeps this account's backups under other words than this twin's; run `mirrin backup target cloud` again")
	}
	t.checked = true
	return nil
}

func (t *target) String() string { return Where }

// Keeps implements backup.Keeper: the control plane runs retention on each
// commit, whatever the daemon's own settings.
func (t *target) Keeps() string {
	return "the newest backup of each of the last 7 days, 4 weeks and 6 months"
}

// Put uploads exactly size bytes of r as name, then asks the control plane
// to count it, which it does only after checking the size arrived. A body
// shorter or longer than size stores nothing.
func (t *target) Put(ctx context.Context, name string, r io.Reader, size int64) error {
	if !backup.ValidName(name) || size < 0 {
		return fmt.Errorf("%q isn't a backup's name", name)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.check(ctx, nil); err != nil {
		return err
	}
	p, err := t.c.Presign(ctx, cloud.OpPut, name, size)
	if err != nil {
		return explain(err, true)
	}
	res, err := t.c.Transfer(ctx, p, io.LimitReader(r, size), size)
	if err != nil {
		t.discard(name)
		return err
	}
	io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	res.Body.Close()
	if res.StatusCode/100 != 2 {
		t.discard(name)
		return fmt.Errorf("storage answered %s", res.Status)
	}
	var one [1]byte
	if n, _ := io.ReadFull(r, one[:]); n > 0 {
		t.discard(name)
		return fmt.Errorf("%s is longer than %d bytes", name, size)
	}
	if err := t.c.Commit(ctx, name, size); err != nil {
		if !cloud.HasCode(err, cloud.CodeExists) && !cloud.HasCode(err, cloud.CodeRemoved) {
			t.discard(name)
		}
		return explain(err, true)
	}
	return nil
}

// discard removes what an upload that failed may have left, best effort:
// the control plane never counted it.
func (t *target) discard(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = t.Delete(ctx, name)
}

// Get opens a stored object.
func (t *target) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if !backup.ValidName(name) {
		return nil, fmt.Errorf("%q isn't a backup's name", name)
	}
	if err := t.check(ctx, nil); err != nil {
		return nil, err
	}
	p, err := t.c.Presign(ctx, cloud.OpGet, name, 0)
	if cloud.HasCode(err, cloud.CodeNotFound) {
		return nil, backup.ErrNotFound
	}
	if err != nil {
		return nil, explain(err, false)
	}
	res, err := t.c.Transfer(ctx, p, nil, 0)
	if err != nil {
		return nil, err
	}
	switch {
	case res.StatusCode == http.StatusNotFound:
		res.Body.Close()
		return nil, backup.ErrNotFound
	case res.StatusCode != http.StatusOK:
		res.Body.Close()
		return nil, fmt.Errorf("storage answered %s", res.Status)
	}
	return res.Body, nil
}

// List returns what the control plane counts as stored, oldest name first.
func (t *target) List(ctx context.Context) ([]backup.Object, error) {
	l, err := t.c.BackupList(ctx)
	if err != nil {
		return nil, explain(err, false)
	}
	if err := t.check(ctx, &l); err != nil {
		return nil, err
	}
	var out []backup.Object
	for _, o := range l.Objects {
		if backup.ValidName(o.Name) {
			out = append(out, backup.Object{Name: o.Name, Size: o.Size, Modified: o.Created})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := stamp(out[i].Name), stamp(out[j].Name)
		if a == b {
			return out[i].Name < out[j].Name
		}
		return a < b
	})
	return out, nil
}

// stamp is the time in a name, which sorts as text.
func stamp(name string) string {
	parts := strings.SplitN(name, "-", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// Delete removes an object; one that isn't there is not an error.
func (t *target) Delete(ctx context.Context, name string) error {
	if !backup.ValidName(name) {
		return fmt.Errorf("%q isn't a backup's name", name)
	}
	if err := t.check(ctx, nil); err != nil {
		return err
	}
	p, err := t.c.Presign(ctx, cloud.OpDelete, name, 0)
	if cloud.HasCode(err, cloud.CodeNotFound) {
		return nil
	}
	if err != nil {
		return explain(err, false)
	}
	res, err := t.c.Transfer(ctx, p, nil, 0)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode/100 != 2 && res.StatusCode != http.StatusNotFound {
		return fmt.Errorf("storage answered %s", res.Status)
	}
	return nil
}

// explain turns the control plane's refusals into what the owner can do.
func explain(err error, put bool) error {
	switch {
	case errors.Is(err, cloud.ErrNotLinked):
		return errors.New("this machine isn't linked to Mirrin Cloud any more; run `mirrin backup target` to choose where backups go")
	case cloud.HasCode(err, cloud.CodeLapsed) && put:
		return fmt.Errorf("%w: payment for Mirrin Cloud has lapsed. The backups kept there can still be restored for 90 days", backup.ErrWritesClosed)
	case cloud.HasCode(err, cloud.CodeLapsed):
		return errors.New("payment for Mirrin Cloud lapsed over 90 days ago, so the backups there can't be fetched any more")
	case cloud.HasCode(err, cloud.CodeOverQuota):
		return fmt.Errorf("%w: there isn't room for it in Mirrin Cloud. `mirrin backup prune` removes old ones", backup.ErrWritesClosed)
	case cloud.HasCode(err, cloud.CodeNoNamespace):
		return errors.New("Mirrin Cloud isn't set up for these backups yet: run `mirrin backup target cloud`, which asks for your 12 words once")
	case cloud.HasCode(err, cloud.CodeSuperseded):
		return errors.New("another machine took over this twin with its 12 words; this one stands by")
	case cloud.HasCode(err, cloud.CodeExists):
		return errors.New("a backup of that name is there already")
	case cloud.HasCode(err, cloud.CodeRemoved):
		return fmt.Errorf("%w: Mirrin Cloud removed a backup of that name before, and never takes one back", backup.ErrNotKept)
	}
	return err
}

// Bind sets the paid service up for the backups the words p make: the
// recovery key they give binds its namespace to this machine's account.
// The words are needed once, here; after that this machine's own key signs.
func Bind(ctx context.Context, c *cloud.Client, p backup.Phrase) error {
	err := c.BindNamespace(ctx, p.RecoveryKey())
	switch {
	case cloud.HasCode(err, cloud.CodeNamespaceBound):
		return errors.New("Mirrin Cloud already keeps backups made with other words for this account; delete them there first, or keep these backups somewhere else")
	case cloud.HasCode(err, "namespace_taken"):
		return errors.New("these words' backups belong to another Mirrin Cloud account")
	}
	return err
}

// Recover makes the machine c belongs to the twin's home, with the words
// p: the paid service's account moves here from the machine that had it,
// which stands by. acmeAccount is this machine's ACME account URI, or "".
func Recover(ctx context.Context, c *cloud.Client, p backup.Phrase, acmeAccount string) (cloud.Recovered, error) {
	r, err := c.Recover(ctx, p.RecoveryKey(), acmeAccount)
	if errors.Is(err, cloud.ErrUnauthorized) {
		return r, errors.New("Mirrin Cloud keeps no backups made with those words (or this computer's clock is more than five minutes out)")
	}
	return r, err
}
