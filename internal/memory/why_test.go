package memory

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

func countRows(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A reply's note lists what it drew on, recalled first, and is found by the
// reply's words however their lines were broken.
func TestWhyForFindsTheNewestNote(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	a, _ := s.Remember(ctx, "family", "Priya lives in Pune.", "c")
	b, _ := s.Remember(ctx, "work", "Tony works at Acme.", "c")
	if err := s.NoteWhy(ctx, WhyNote{ChatKey: "c", Reply: "Done.", Matched: []int64{b}}); err != nil {
		t.Fatal(err)
	}
	if err := s.NoteWhy(ctx, WhyNote{ChatKey: "c", Reply: "Done.", Everything: true, Recalled: []int64{a}, Matched: []int64{b, a}}); err != nil {
		t.Fatal(err)
	}
	w, ok, err := s.WhyFor(ctx, "  Done.\n")
	if err != nil || !ok || !w.Everything || len(w.Facts) != 2 || w.Facts[0].ID != a || w.Facts[0].How != WhyRecalled || w.Facts[1].ID != b || w.Facts[1].How != WhyMatched {
		t.Fatalf("why = %+v, %v, %v", w, ok, err)
	}
	if _, ok, _ := s.WhyFor(ctx, "Something never said."); ok {
		t.Fatal("found a note for a reply never noted")
	}
}

// Forgetting a fact takes it out of every note, in the live memory and in
// the backups, not only out of what the screen shows.
func TestForgetDropsTheFactFromEveryNote(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	gone, _ := s.Remember(ctx, "health", "Tony is allergic to penicillin and carries a card.", "c")
	kept, _ := s.Remember(ctx, "home", "Tony lives in Sydney.", "c")
	_ = s.NoteWhy(ctx, WhyNote{ChatKey: "c", Reply: "Noted.", Recalled: []int64{gone, kept}})
	path, err := s.Backup(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ForgetFact(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s.db, `SELECT count(*) FROM reply_facts WHERE fact_id=?`, gone); n != 0 {
		t.Fatalf("%d notes still point at the forgotten fact", n)
	}
	if w, _, _ := s.WhyFor(ctx, "Noted."); len(w.Facts) != 1 || w.Facts[0].ID != kept {
		t.Fatalf("why after forgetting = %+v", w)
	}
	bdb, err := sql.Open("sqlite", fileURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer bdb.Close()
	if n := countRows(t, bdb, `SELECT count(*) FROM reply_facts WHERE fact_id=?`, gone); n != 0 {
		t.Fatalf("the backup's notes still point at the forgotten fact (%d)", n)
	}
}

// A backup made before notes were kept has no table for them: forgetting
// still cleans it rather than deleting it as unscrubbable.
func TestForgetCleansABackupFromBeforeNotes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, _ := s.Remember(ctx, "home", "Tony keeps a spare key under the third flowerpot.", "c")
	path, err := s.Backup(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", fileURI(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`DROP TABLE reply_facts`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	if _, err := s.ForgetFact(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the old backup was deleted instead of cleaned: %v", err)
	}
	if paths, _ := s.Backups(); len(paths) != 1 || filepath.Base(paths[0]) != filepath.Base(path) {
		t.Fatalf("backups now %v", paths)
	}
}

// Notes are kept for the newest replies only, and each lists a dozen facts
// at most.
func TestNotesStaySmall(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	var many []int64
	for i := 0; i < 20; i++ {
		id, _ := s.Remember(ctx, "general", fmt.Sprintf("fact %d", i), "c")
		many = append(many, id)
	}
	_ = s.NoteWhy(ctx, WhyNote{ChatKey: "c", Reply: "Lots.", Matched: many})
	if w, _, _ := s.WhyFor(ctx, "Lots."); len(w.Facts) != whyMost {
		t.Fatalf("%d facts noted, want %d", len(w.Facts), whyMost)
	}
	if _, err := s.db.Exec(`UPDATE sqlite_sequence SET seq=? WHERE name='reply_why'`, whyKeep+10); err != nil {
		t.Fatal(err)
	}
	_ = s.NoteWhy(ctx, WhyNote{ChatKey: "c", Reply: "Newest.", Matched: many[:1]})
	if _, ok, _ := s.WhyFor(ctx, "Lots."); ok {
		t.Fatal("a note older than the newest replies was kept")
	}
	if n := countRows(t, s.db, `SELECT count(*) FROM reply_facts`); n != 1 {
		t.Fatalf("%d fact rows left, want the newest reply's 1", n)
	}
}

// When every fact fits, the closest few are still named as matches.
func TestPickFactsNamesTheClosestWhenEverythingFits(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	_, _ = s.Remember(ctx, "home", "Tony lives in Sydney.", "c")
	dentist, _ := s.Remember(ctx, "health", "Tony's dentist is Dr Okafor.", "c")
	p, err := s.PickFacts(ctx, "When is my dentist appointment?", 10000)
	if err != nil || !p.Everything || len(p.Facts) != 2 || len(p.Matched) != 1 || p.Matched[0] != dentist {
		t.Fatalf("picked %+v, %v", p, err)
	}
}

// The latest turn is what came after the message it answered, and its last
// reply's row.
func TestLastTurn(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	_ = s.AppendMessage(ctx, "c", llm.Text(llm.RoleUser, "earlier"))
	_ = s.AppendMessage(ctx, "c", llm.Text(llm.RoleAssistant, "earlier reply"))
	_ = s.AppendMessage(ctx, "c", llm.Text(llm.RoleUser, "now"))
	_ = s.AppendMessage(ctx, "c", llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: "t", ToolName: "recall"}}})
	_ = s.AppendMessage(ctx, "c", llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "t", Text: "#1 [x] y"}}})
	_ = s.AppendMessage(ctx, "c", llm.Text(llm.RoleAssistant, "the answer"))
	msgs, last, err := s.LastTurn(ctx, "c")
	if err != nil || len(msgs) != 3 || msgs[2].PlainText() != "the answer" || last != 6 {
		t.Fatalf("turn %d msgs, last %d, %v", len(msgs), last, err)
	}
}
