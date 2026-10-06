package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "cloud.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// The deny list's seq rises with every change, an entry added or entries
// aging out, and the same seq always means the same entries.
func TestDenySeq(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	list := func() (int64, int) {
		var seq int64
		var n []DenyEntry
		s.View(ctx, func(tx *Tx) error {
			var err error
			seq, n, err = tx.DenyList()
			return err
		})
		return seq, len(n)
	}
	if seq, n := list(); seq != 1 || n != 0 {
		t.Fatalf("empty list: seq %d, %d entries", seq, n)
	}
	s.Update(ctx, func(tx *Tx) error {
		added, err := tx.Deny(DenyEntry{Kind: DenyHandle, Value: "a-b-c", Why: "abuse", Created: t0})
		if !added || err != nil {
			t.Errorf("Deny: %v %v", added, err)
		}
		added, err = tx.Deny(DenyEntry{Kind: DenyHandle, Value: "a-b-c", Why: "again", Created: t0})
		if added || err != nil {
			t.Errorf("Deny again: %v %v", added, err)
		}
		_, err = tx.Deny(DenyEntry{Kind: "user", Value: "x", Created: t0})
		if err == nil {
			t.Error("unknown kind accepted")
		}
		return nil
	})
	if seq, n := list(); seq != 2 || n != 1 {
		t.Fatalf("one entry: seq %d, %d entries", seq, n)
	}
	s.Update(ctx, func(tx *Tx) error { return tx.MoveDenyEdge(t0.Add(-time.Hour), t0.Add(-time.Hour)) })
	if seq, n := list(); seq != 2 || n != 1 {
		t.Errorf("edge before the entry: seq %d, %d entries", seq, n)
	}
	s.Update(ctx, func(tx *Tx) error { return tx.MoveDenyEdge(t0.Add(time.Hour), t0.Add(-time.Hour)) })
	if seq, n := list(); seq != 3 || n != 0 {
		t.Errorf("edge past the entry: seq %d, %d entries", seq, n)
	}
	s.Update(ctx, func(tx *Tx) error { return tx.MoveDenyEdge(t0, t0) }) // backwards: ignored
	if seq, n := list(); seq != 3 || n != 0 {
		t.Errorf("edge moved back: seq %d, %d entries", seq, n)
	}
	// Past the forget time the row goes too.
	s.Update(ctx, func(tx *Tx) error { return tx.MoveDenyEdge(t0.Add(2*time.Hour), t0.Add(2*time.Hour)) })
	s.View(ctx, func(tx *Tx) error {
		if _, err := tx.Denied(DenyHandle, "a-b-c"); !errors.Is(err, ErrNotFound) {
			t.Errorf("forgotten entry: %v", err)
		}
		return nil
	})
}

func TestBillingEventsAndDeletion(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	err := s.Update(ctx, func(tx *Tx) error {
		if seen, err := tx.SeenBillingEvent("evt_1", now); seen || err != nil {
			t.Errorf("first: %v %v", seen, err)
		}
		if seen, err := tx.SeenBillingEvent("evt_1", now); !seen || err != nil {
			t.Errorf("second: %v %v", seen, err)
		}
		a := Account{ID: "acct_1", BillingCustomer: "ctm_1", Plan: "cloud", BillingStatus: "active", PaidThrough: now, BillingAt: now, Created: now}
		if err := tx.InsertAccount(a); err != nil {
			return err
		}
		if err := tx.InsertHandle(Handle{Name: "a-b-c", Skeleton: "abc", Account: "acct_1", Gen: 1, GenAt: now, Created: now}); err != nil {
			return err
		}
		if err := tx.InsertHandle(Handle{Name: "a-bc", Skeleton: "abc", Gen: 1, Created: now}); err == nil {
			t.Error("two handles with one skeleton")
		}
		if err := tx.InsertDevice(Device{Pub: "p", KeyID: "k", Account: "acct_1", Gen: 1, Created: now, LastSeenDay: "2026-09-27"}); err != nil {
			return err
		}
		if err := tx.AddEvent("acct_1", "link", now); err != nil {
			return err
		}
		return tx.DeleteAccount("acct_1", now)
	})
	if err != nil {
		t.Fatal(err)
	}
	s.View(ctx, func(tx *Tx) error {
		if _, err := tx.Account("acct_1"); !errors.Is(err, ErrNotFound) {
			t.Error("account remains")
		}
		if _, err := tx.DeviceByKeyID("k"); !errors.Is(err, ErrNotFound) {
			t.Error("device remains")
		}
		h, err := tx.Handle("a-b-c")
		if err != nil || h.Account != "" || h.Released.IsZero() || !h.DNSPending {
			t.Errorf("handle %+v %v", h, err)
		}
		dump, _ := tx.Dump()
		if len(dump["events"]) != 0 {
			t.Error("events remain")
		}
		return nil
	})
}

func TestRowsRefusesNames(t *testing.T) {
	s := open(t)
	s.View(context.Background(), func(tx *Tx) error {
		if _, err := tx.Rows("accounts; DROP TABLE accounts", "id", "x"); err == nil {
			t.Error("a table name with SQL in it was used")
		}
		if _, err := tx.Rows("accounts", "id=id OR 1", "x"); err == nil {
			t.Error("a column name with SQL in it was used")
		}
		return nil
	})
}
