package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/tools"
)

func TestReviseApprovalOnlyFromTheOutcomeGiven(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, _ := s.CreateApproval(ctx, "c", "send", nil, "send()")
	if err := s.ReviseApproval(ctx, id, "approved", "expired"); err == nil {
		t.Fatal("a pending approval was revised as if it had been approved")
	}
	if err := s.ResolveApproval(ctx, id, "approved"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReviseApproval(ctx, id, "approved", "expired"); err != nil {
		t.Fatal(err)
	}
	if ap, _ := s.GetApproval(ctx, id); ap.Status != "expired" {
		t.Fatalf("status %q", ap.Status)
	}
	if err := s.ReviseApproval(ctx, id, "approved", "denied"); err == nil {
		t.Fatal("revised twice")
	}
}

func TestUnset(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.Set(ctx, "k", "v")
	if err := s.Unset(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get(ctx, "k"); err != nil || v != "" {
		t.Fatalf("got %q %v", v, err)
	}
	if err := s.Unset(ctx, "never-set"); err != nil {
		t.Fatal(err)
	}
}

// An install from before risks were kept opens cleanly, as often as it is
// opened: what was waiting counts as dangerous and keeps the hash of what it
// holds, and nothing else about it changes.
func TestApprovalsMigrateAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	old, err := sql.Open("sqlite", fileURI(filepath.Join(dir, "memory.db")))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE approvals (id INTEGER PRIMARY KEY AUTOINCREMENT, chat_key TEXT NOT NULL, tool TEXT NOT NULL, input TEXT NOT NULL, summary TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', created_at TEXT NOT NULL, resolved_at TEXT)`,
		`INSERT INTO approvals(chat_key, tool, input, summary, created_at) VALUES('telegram:1', 'send', '{"to":"boss"}', 'send(to=boss)', '2026-09-20T10:00:00Z')`,
		`INSERT INTO approvals(chat_key, tool, input, summary, status, created_at, resolved_at) VALUES('telegram:1', 'send', '{}', 'send()', 'approved', '2026-09-20T10:00:00Z', '2026-09-20T10:01:00Z')`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()
	for i := 0; i < 2; i++ { // idempotent
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		ap, err := s.GetApproval(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if ap.Risk != tools.RiskDangerous || ap.Status != "pending" || ap.Summary != "send(to=boss)" || string(ap.Input) != `{"to":"boss"}` {
			t.Fatalf("legacy row read back as %+v", ap)
		}
		if ap.InputHash != HashInput([]byte(`{"to":"boss"}`)) || !ap.Intact() {
			t.Fatalf("legacy row hash %q", ap.InputHash)
		}
		if done, _ := s.GetApproval(ctx, 2); done.Status != "approved" || done.ResolvedAt.IsZero() {
			t.Fatalf("decided legacy row: %+v", done)
		}
		if ps, _ := s.AllPendingApprovals(ctx); len(ps) != 1 || ps[0].ID != 1 {
			t.Fatalf("pending: %+v", ps)
		}
		// It is one of the migrations (migrate.go), recorded like the rest.
		if applied, err := appliedMigrations(ctx, s.db); err != nil || !applied["approvals-risk"] {
			t.Fatalf("approvals-risk not recorded: %v %v", err, applied)
		}
		s.Close()
	}
}

func TestApprovalKeepsItsRiskHashAndDecider(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := s.CreateApproval(ctx, "c", "send_email", []byte(`{"to":"a@b.c"}`), "send_email(to=a@b.c)", tools.RiskWrite)
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := s.CreateApproval(ctx, "c", "pay", nil, "pay()") // no risk given
	ap, _ := s.GetApproval(ctx, id)
	if ap.Risk != tools.RiskWrite || ap.InputHash != HashInput([]byte(`{"to":"a@b.c"}`)) || !ap.Intact() || ap.DecidedBy != "" || !ap.ResolvedAt.IsZero() {
		t.Fatalf("stored as %+v", ap)
	}
	if lp, _ := s.GetApproval(ctx, legacy); lp.Risk != tools.RiskDangerous || string(lp.Input) != "{}" || lp.InputHash != HashInput([]byte("{}")) {
		t.Fatalf("a request without a risk: %+v", lp)
	}
	if err := s.ResolveApprovalBy(ctx, id, "approved", "Akshay's iPhone (passkey, relay r2, 203.0.113.9)"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveApprovalBy(ctx, id, "denied", "the screen"); err == nil {
		t.Fatal("an approval got two outcomes")
	}
	ap, _ = s.GetApproval(ctx, id)
	if ap.Status != "approved" || ap.DecidedBy != "Akshay's iPhone (passkey, relay r2, 203.0.113.9)" || ap.ResolvedAt.IsZero() {
		t.Fatalf("decided as %+v", ap)
	}
	// The input changed after it was asked (forget rewrites what quoted a fact).
	ap.Input = json.RawMessage(`{"to":"[forgotten]"}`)
	if ap.Intact() {
		t.Fatal("a changed input reads as the one the owner saw")
	}
}

func TestLapsedApprovalsAreTheOldPendingOnes(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old, _ := s.CreateApproval(ctx, "c", "send", nil, "send()")
	decided, _ := s.CreateApproval(ctx, "c", "send", nil, "send()")
	_ = s.ResolveApproval(ctx, decided, "denied")
	if _, err := s.db.Exec(`UPDATE approvals SET created_at=? WHERE id IN (?,?)`, time.Now().Add(-4*24*time.Hour).UTC().Format(time.RFC3339), old, decided); err != nil {
		t.Fatal(err)
	}
	fresh, _ := s.CreateApproval(ctx, "c", "send", nil, "send()")
	got, err := s.LapsedApprovals(ctx, time.Now().Add(-72*time.Hour))
	if err != nil || len(got) != 1 || got[0].ID != old {
		t.Fatalf("lapsed %+v %v (fresh is #%d)", got, err, fresh)
	}
}

// Migration 4 hashes every approval's input once, and is recorded as done;
// a row written afterwards without a hash (an older Mirrin after going back
// a version, or an identity import, which keeps this memory's own record of
// migrations) is hashed at the next open, so from then on an edit to what
// was asked is caught (observability's runner merged with approvals).
func TestApprovalsWrittenWithoutAHashGetOneAtTheNextOpen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO approvals(chat_key, tool, input, summary, status, created_at, input_hash) VALUES('telegram:1','send','{"to":"boss"}','send','pending',?, '')`, now()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	aps, err := s.AllPendingApprovals(ctx)
	if err != nil || len(aps) != 1 {
		t.Fatalf("%v %v", aps, err)
	}
	if aps[0].InputHash != HashInput([]byte(`{"to":"boss"}`)) {
		t.Fatalf("still unhashed: %q", aps[0].InputHash)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE approvals SET input='{"to":"attacker"}' WHERE id=?`, aps[0].ID); err != nil {
		t.Fatal(err)
	}
	if ap, _ := s.GetApproval(ctx, aps[0].ID); ap.Intact() {
		t.Fatal("an edit after the hash was taken went unnoticed")
	}
}
