package memory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

func TestForgetUnknownFactSaysSo(t *testing.T) {
	s := openTest(t)
	err := s.Forget(context.Background(), 999)
	if !errors.Is(err, ErrNoFact) || !strings.Contains(err.Error(), "#999") {
		t.Fatalf("want ErrNoFact naming #999, got %v", err)
	}
}

func TestForgetRemovesEveryCopy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	secret := "Tony was diagnosed with type 2 diabetes in March & takes metformin <daily>."
	input, _ := json.Marshal(map[string]string{"subject": "health", "fact": secret})

	// The trail a remember_sensitive call leaves: the tool call and its result
	// in the conversation, the approval, the audit lines, then the fact itself.
	_ = s.AppendMessage(ctx, "c", llm.Text(llm.RoleUser, "keep this private"))
	_ = s.AppendMessage(ctx, "c", llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: "t1", ToolName: "remember_sensitive", Input: input}}})
	_ = s.AppendMessage(ctx, "c", llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "t1", Text: "#1 [health] " + secret}}})
	aid, _ := s.CreateApproval(ctx, "c", "remember_sensitive", input, "remember_sensitive(fact="+secret[:80-len("remember_sensitive(fact=")]+"…[truncated], subject=health)")
	s.Audit(ctx, "tool.succeeded", "c", "remember_sensitive "+string(input)[:60]+"…[truncated]")
	s.Audit(ctx, "tool.ok", "c", "recall (1ms) "+string(input))
	s.Audit(ctx, "message.out", "c", "Noted.")
	id, err := s.Remember(ctx, "health", secret, "c")
	if err != nil {
		t.Fatal(err)
	}
	keep, _ := s.Remember(ctx, "home", "Tony lives in Sydney.", "c")
	if _, err := s.Backup(ctx, 3); err != nil {
		t.Fatal(err)
	}

	f, err := s.ForgetFact(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if f.Content != secret {
		t.Fatalf("should report what was removed, got %+v", f)
	}
	if facts, _ := s.AllFacts(ctx, 10); len(facts) != 1 || facts[0].ID != keep {
		t.Fatalf("only the other fact should remain: %+v", facts)
	}
	leaks := func(where, text string) {
		t.Helper()
		for _, piece := range []string{"diabetes", "metformin"} {
			if strings.Contains(text, piece) {
				t.Errorf("%s still holds %q: %s", where, piece, text)
			}
		}
	}
	es, _ := s.RecentAudit(ctx, 10)
	for _, e := range es {
		leaks("audit", e.Detail)
		if e.Kind == "message.out" && e.Detail != "Noted." {
			t.Errorf("unrelated audit changed: %q", e.Detail)
		}
	}
	ap, _ := s.GetApproval(ctx, aid)
	leaks("approval input", string(ap.Input))
	leaks("approval summary", ap.Summary)
	if !json.Valid(ap.Input) || !strings.Contains(string(ap.Input), "health") {
		t.Errorf("approval input should stay valid JSON with the rest intact: %s", ap.Input)
	}
	h, _ := s.History(ctx, "c", 10)
	for _, m := range h {
		b, _ := json.Marshal(m)
		leaks("conversation", string(b))
	}
	if len(h) != 3 || h[0].PlainText() != "keep this private" {
		t.Errorf("conversation shape changed: %+v", h)
	}

	// Nothing left on disk either: not in free pages, not in the WAL, not in a backup.
	s.Close()
	files, _ := filepath.Glob(filepath.Join(dir, "memory.db*"))
	backups, _ := filepath.Glob(filepath.Join(dir, "backups", "*"))
	if len(backups) != 1 {
		t.Fatalf("backup should survive being scrubbed: %v", backups)
	}
	for _, p := range append(files, backups...) {
		b, _ := os.ReadFile(p)
		if bytes.Contains(b, []byte("metformin")) {
			t.Errorf("%s still holds the forgotten fact", filepath.Base(p))
		}
	}
	if b, _ := os.ReadFile(backups[0]); !bytes.Contains(b, []byte("Tony lives in Sydney.")) {
		t.Error("backup lost the facts that weren't forgotten")
	}
}

// The portrait may say a forgotten thing in other words ("you're quietly
// looking for a new role"), so a forget takes it out of the daily backups
// and the recorded copies as well: restoring one doesn't bring it back.
func TestForgetDropsThePortraitFromEveryCopy(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	id, err := s.Remember(ctx, "work", "Akshay is looking for a new job at another company.", "c")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(ctx, "home", "Tony lives in Sydney.", "c"); err != nil {
		t.Fatal(err)
	}
	words := "You're quietly looking for a new role."
	if err := s.SetPortrait(ctx, words); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"portrait.prev", "portrait.new"} {
		if err := s.Set(ctx, k, words); err != nil {
			t.Fatal(err)
		}
	}
	backup, err := s.Backup(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	// What an import replaced: a database and an exported memory.yaml.
	dir := filepath.Join(t.TempDir(), "backups", "20261003-080000")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	copyDB := filepath.Join(dir, "memory.db")
	b, _ := os.ReadFile(backup)
	if err := os.WriteFile(copyDB, b, 0o600); err != nil {
		t.Fatal(err)
	}
	copyYAML := filepath.Join(dir, "memory.yaml")
	if err := os.WriteFile(copyYAML, []byte("portrait: "+words+"\nfacts:\n  - subject: home\n    content: Tony lives in Sydney.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{copyDB, copyYAML} {
		if err := s.NoteCopy(p); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.ForgetFact(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{backup, copyDB} {
		db, err := sql.Open("sqlite", fileURI(p))
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		rows, err := db.QueryContext(ctx, `SELECT key FROM kv WHERE key LIKE 'portrait%'`)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		for rows.Next() {
			var k string
			_ = rows.Scan(&k)
			keys = append(keys, k)
		}
		rows.Close()
		var kept int
		_ = db.QueryRowContext(ctx, `SELECT count(*) FROM facts WHERE content='Tony lives in Sydney.'`).Scan(&kept)
		db.Close()
		if len(keys) != 0 {
			t.Errorf("%s still keeps %q", filepath.Base(p), keys)
		}
		if kept != 1 {
			t.Errorf("%s lost the facts that weren't forgotten", filepath.Base(p))
		}
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("quietly looking")) {
			t.Errorf("%s still holds the portrait's words on disk", filepath.Base(p))
		}
	}
	y, _ := os.ReadFile(copyYAML)
	if bytes.Contains(y, []byte("quietly looking")) || !bytes.Contains(y, []byte("Tony lives in Sydney.")) {
		t.Errorf("memory.yaml after the forget:\n%s", y)
	}
}

func TestForgettingAShortFactLeavesOtherTextAlone(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	remember := json.RawMessage(`{"subject":"preferences","fact":"blue"}`)
	other := json.RawMessage(`{"subject":"home","fact":"The front door is blue."}`)
	msgs := []llm.Message{
		llm.Text(llm.RoleUser, "my favourite colour is blue. is bluetooth on?"),
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: "t1", ToolName: "remember", Input: remember}}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "t1", Text: "remembered (#1)"}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: "t2", ToolName: "remember", Input: other}}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "t3", Text: "#1 [preferences] blue\n#2 [home] The front door is blue.\n"}}},
		llm.Text(llm.RoleAssistant, "Blue it is. Bluetooth is on, and the sky is blue today."),
	}
	for _, m := range msgs {
		_ = s.AppendMessage(ctx, "c", m)
	}
	s.Audit(ctx, "tool.ok", "c", "remember (2ms) "+string(remember))
	s.Audit(ctx, "tool.ok", "c", "remember (2ms) "+string(other))
	s.Audit(ctx, "message.out", "c", "blue sky, bluetooth, blueberries")
	aid, _ := s.CreateApproval(ctx, "c", "remember", remember, "remember(fact=blue, subject=preferences)")
	id, _ := s.Remember(ctx, "preferences", "blue", "c")
	door, _ := s.Remember(ctx, "home", "The front door is blue.", "c")

	if err := s.Forget(ctx, id); err != nil {
		t.Fatal(err)
	}
	h, _ := s.History(ctx, "c", 10)
	got := []string{
		h[0].PlainText(),
		string(h[1].Blocks[0].Input),
		string(h[3].Blocks[0].Input),
		h[4].Blocks[0].Text,
		h[5].PlainText(),
	}
	want := []string{
		"my favourite colour is blue. is bluetooth on?", // the user's own words
		`{"fact":"[forgotten]","subject":"preferences"}`,
		string(other),
		"#1 [preferences] [forgotten]\n#2 [home] The front door is blue.\n",
		"Blue it is. Bluetooth is on, and the sky is blue today.", // too short to tell apart from other uses
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
	es, _ := s.RecentAudit(ctx, 10)
	details := []string{}
	for _, e := range es {
		details = append(details, e.Detail)
	}
	wantAudit := []string{
		"blue sky, bluetooth, blueberries",
		"remember (2ms) " + string(other),
		`remember (2ms) {"subject":"preferences","fact":"[forgotten]"}`,
	}
	if strings.Join(details, "\n") != strings.Join(wantAudit, "\n") {
		t.Errorf("audit:\n got %q\nwant %q", details, wantAudit)
	}
	ap, _ := s.GetApproval(ctx, aid)
	if ap.Summary != "remember(fact=[forgotten], subject=preferences)" || strings.Contains(string(ap.Input), "blue") {
		t.Errorf("approval should drop the fact: %s %s", ap.Summary, ap.Input)
	}
	if fs, _ := s.AllFacts(ctx, 10); len(fs) != 1 || fs[0].ID != door {
		t.Errorf("only the other fact should remain: %+v", fs)
	}
}

func TestForgetRedactsWordForWordCopiesOfALongFact(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	fact := "Tony's PIN for the side gate is 4417."
	_ = s.AppendMessage(ctx, "c", llm.Text(llm.RoleUser, "remember that Tony's PIN for the side gate is 4417."))
	_ = s.AppendMessage(ctx, "c", llm.Text(llm.RoleAssistant, "Noted: Tony's PIN for the side gate is 4417. I won't share it."))
	_ = s.AppendMessage(ctx, "c", llm.Text(llm.RoleAssistant, "Not the same: Tony's PIN for the side gate is 44170."))
	s.Audit(ctx, "message.out", "c", "Noted: Tony's PIN for the side gate is 4417. I won't share it.")
	id, _ := s.Remember(ctx, "secrets", fact, "c")
	if err := s.Forget(ctx, id); err != nil {
		t.Fatal(err)
	}
	h, _ := s.History(ctx, "c", 10)
	if got := h[0].PlainText(); got != "remember that Tony's PIN for the side gate is 4417." {
		t.Errorf("the user's own words should stay: %q", got)
	}
	if got := h[1].PlainText(); got != "Noted: [forgotten] I won't share it." {
		t.Errorf("the twin's word-for-word copy should go: %q", got)
	}
	if got := h[2].PlainText(); !strings.Contains(got, "44170") {
		t.Errorf("a longer number isn't the fact: %q", got)
	}
	if es, _ := s.RecentAuditOfKind(ctx, "message.out", 1); len(es) != 1 || es[0].Detail != "Noted: [forgotten] I won't share it." {
		t.Errorf("audit copy should go: %+v", es)
	}
}
