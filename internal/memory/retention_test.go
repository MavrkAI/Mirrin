package memory

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

func ago(days int) string { return time.Now().AddDate(0, 0, -days).UTC().Format(time.RFC3339) }

func TestTidyDeletesOldAuditAndTrimsDetail(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	long := "remember " + strings.Repeat("my sister's birthday is in March; ", 10)
	for _, row := range []struct {
		days   int
		detail string
	}{{120, long}, {45, long}, {2, long}, {45, "short"}} {
		if _, err := s.db.Exec(`INSERT INTO audit(ts, kind, chat_key, detail) VALUES(?,?,?,?)`, ago(row.days), "tool.ok", "c", row.detail); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.Tidy(ctx, Retention{AuditDays: 90, TrimAfterDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if res.AuditDeleted != 1 || res.AuditTrimmed != 1 {
		t.Fatalf("result %+v", res)
	}
	es, _ := s.RecentAudit(ctx, 10)
	if len(es) != 3 {
		t.Fatalf("want 3 entries left, got %d", len(es))
	}
	for _, e := range es {
		age := time.Since(e.TS)
		switch {
		case e.Detail == "short":
		case age > 30*24*time.Hour:
			if len([]rune(e.Detail)) != trimAuditTo+1 || !strings.HasSuffix(e.Detail, "…") {
				t.Fatalf("an old entry wasn't trimmed: %q", e.Detail)
			}
		default:
			if e.Detail != long {
				t.Fatalf("a recent entry was changed: %q", e.Detail)
			}
		}
	}
	// Running again changes nothing.
	if res, _ := s.Tidy(ctx, Retention{AuditDays: 90, TrimAfterDays: 30}); res != (TidyResult{}) {
		t.Fatalf("second run: %+v", res)
	}
	// Zero keeps everything.
	if res, _ := s.Tidy(ctx, Retention{}); res != (TidyResult{}) {
		t.Fatalf("zero policy: %+v", res)
	}
}

func TestTidyTrimsOnlyOldLongToolResults(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	page := strings.Repeat("é page text ", 400) // multi-byte, to check the cut
	msg := func(days int, blocks []llm.Block) {
		t.Helper()
		b, _ := json.Marshal(blocks)
		if _, err := s.db.Exec(`INSERT INTO messages(chat_key, role, blocks, created_at) VALUES(?,?,?,?)`, "telegram:1", "user", string(b), ago(days)); err != nil {
			t.Fatal(err)
		}
	}
	msg(40, []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "t1", Text: page}, {Type: llm.BlockToolResult, ToolUseID: "t2", Text: "small"}})
	msg(40, []llm.Block{{Type: llm.BlockText, Text: page}}) // what someone said is never cut
	msg(3, []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "t3", Text: page}})

	res, err := s.Tidy(ctx, Retention{TrimAfterDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if res.ResultsTrimmed != 1 {
		t.Fatalf("result %+v", res)
	}
	h, err := s.History(ctx, "telegram:1", 10)
	if err != nil {
		t.Fatalf("trimmed history must still load: %v", err)
	}
	old := h[0].Blocks[0].Text
	if len(old) > trimResultTo+100 || !strings.Contains(old, "[trimmed") || !strings.HasPrefix(page, strings.SplitN(old, "\n[trimmed", 2)[0]) {
		t.Fatalf("old tool result: %q", old)
	}
	if h[0].Blocks[1].Text != "small" || h[1].Blocks[0].Text != page || h[2].Blocks[0].Text != page {
		t.Fatal("only old, long tool results may be cut")
	}
	if !json.Valid([]byte(mustBlocks(t, s))) {
		t.Fatal("stored blocks are no longer JSON")
	}
	if res, _ := s.Tidy(ctx, Retention{TrimAfterDays: 30}); res.ResultsTrimmed != 0 {
		t.Fatalf("second run trimmed again: %+v", res)
	}
}

func mustBlocks(t *testing.T, s *Store) string {
	t.Helper()
	var b string
	if err := s.db.QueryRow(`SELECT blocks FROM messages ORDER BY id LIMIT 1`).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

// Each run picks up where the last left off, whether or not it found
// anything to cut, and never looks at messages still inside the period.
func TestTidyRemembersHowFarItGot(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	add := func(days int, text string) int64 {
		t.Helper()
		b, _ := json.Marshal([]llm.Block{{Type: llm.BlockToolResult, ToolUseID: "t", Text: text}})
		r, err := s.db.Exec(`INSERT INTO messages(chat_key, role, blocks, created_at) VALUES(?,?,?,?)`, "c", "user", string(b), ago(days))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := r.LastInsertId()
		return id
	}
	for i := 0; i < 250; i++ {
		add(40, "short")
	}
	last := add(40, "short")
	young := add(5, strings.Repeat("x", 5000))
	if res, err := s.Tidy(ctx, Retention{TrimAfterDays: 30}); err != nil || res.ResultsTrimmed != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if v, _ := s.Get(ctx, trimResultsKey); v != strconv.FormatInt(last, 10) {
		t.Fatalf("watermark %q, want %d (the newest aged message)", v, last)
	}
	// The young message comes of age later and is cut then.
	if _, err := s.db.Exec(`UPDATE messages SET created_at=? WHERE id=?`, ago(31), young); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Tidy(ctx, Retention{TrimAfterDays: 30}); err != nil || res.ResultsTrimmed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}
