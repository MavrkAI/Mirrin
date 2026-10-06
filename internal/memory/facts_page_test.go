package memory

import (
	"context"
	"fmt"
	"testing"
)

// Past 1,000 facts the memory page pages through them: page 2 holds the
// newest remainder, and the total counts them all.
func TestFactsPagePastAThousand(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 1500; i++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO facts(subject, content, source, created_at, updated_at) VALUES(?,?,?,?,?)`,
			"general", fmt.Sprintf("fact number %d", i), "test", now(), now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	first, total, err := s.FactsPage(ctx, 0, 1000, "")
	if err != nil || total != 1500 || len(first) != 1000 || first[0].Content != "fact number 1" {
		t.Fatalf("page 1: %d facts of %d, %v", len(first), total, err)
	}
	second, total, err := s.FactsPage(ctx, 1000, 1000, "")
	if err != nil || total != 1500 || len(second) != 500 || second[0].Content != "fact number 1001" || second[499].Content != "fact number 1500" {
		t.Fatalf("page 2: %d facts of %d, %v", len(second), total, err)
	}
	found, total, err := s.FactsPage(ctx, 1, 10, "NUMBER 150")
	if err != nil || total != 2 || len(found) != 1 || found[0].Content != "fact number 1500" { // 150 and 1500, from the second
		t.Fatalf("search: %d facts of %d, %v", len(found), total, err)
	}
	if none, total, err := s.FactsPage(ctx, 0, 10, "100%"); err != nil || total != 0 || len(none) != 0 {
		t.Fatalf("a %% in the search is a wildcard: %d of %d, %v", len(none), total, err)
	}
}
