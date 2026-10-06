package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
	memtools "github.com/MavrkAI/Mirrin/internal/skills/memory"
)

func auditRows(t *testing.T, store *memory.Store) []memory.AuditEntry {
	t.Helper()
	rows, err := store.RecentAudit(context.Background(), 200)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestMemoryToolsKeepContentOutOfTheAudit(t *testing.T) {
	for _, write := range []string{"auto", "ask"} {
		t.Run("write="+write, func(t *testing.T) {
			fp := &fakeProvider{script: []llm.Response{
				toolUse("t1", "remember_sensitive", `{"subject":"secrets","fact":"secret X"}`),
				text("done"),
				toolUse("t2", "recall", `{"query":"secret X"}`),
				text("done"),
			}}
			a, store, _ := setup(t, fp, config.Autonomy{Read: "auto", Write: write, Dangerous: "ask"})
			a.tools.Register(memtools.Tools(store)...)
			ctx := context.Background()
			if _, err := a.Handle(ctx, "telegram:1", "keep secret X safe"); err != nil {
				t.Fatal(err)
			}
			okKind := "tool.ok"
			if write == "ask" {
				pending, _ := store.PendingApprovals(ctx, "telegram:1")
				if len(pending) != 1 {
					t.Fatalf("want one approval, got %+v", pending)
				}
				if !strings.Contains(pending[0].Summary, "secret X") {
					t.Fatalf("the owner must still see what they approve: %q", pending[0].Summary)
				}
				fp.script = append([]llm.Response{text("kept")}, fp.script...)
				if _, err := a.ResolveApproval(ctx, "telegram:1", pending[0].ID, true); err != nil {
					t.Fatal(err)
				}
				okKind = "tool.succeeded"
			}
			if _, err := a.Handle(ctx, "telegram:1", "what secrets do you know"); err != nil {
				t.Fatal(err)
			}
			facts, _ := store.Recall(ctx, "secret", 5)
			if len(facts) != 1 {
				t.Fatalf("fact not stored: %+v", facts)
			}
			id := fmt.Sprintf("#%d", facts[0].ID)
			var sawWrite, sawRecall bool
			for _, r := range auditRows(t, store) {
				if strings.Contains(r.Detail, "secret X") {
					t.Fatalf("audit row %s copies the fact: %q", r.Kind, r.Detail)
				}
				if r.Kind == okKind && strings.HasPrefix(r.Detail, "remember_sensitive") && strings.Contains(r.Detail, id) {
					sawWrite = true
				}
				if r.Kind == "tool.ok" && strings.HasPrefix(r.Detail, "recall") && strings.Contains(r.Detail, id) {
					sawRecall = true
				}
			}
			if !sawWrite || !sawRecall {
				t.Fatalf("want %s rows naming %s (write %v, recall %v): %+v", okKind, id, sawWrite, sawRecall, auditRows(t, store))
			}
		})
	}
}

func TestOtherToolsKeepAnExcerpt(t *testing.T) {
	if got := auditInput("send", []byte(`{"to":"priya"}`), "sent", 300); got != `{"to":"priya"}` {
		t.Fatalf("got %q", got)
	}
}

func TestTruncateIsRuneSafe(t *testing.T) {
	s := strings.Repeat("é", 10) // two bytes each
	for n := 0; n <= len(s); n++ {
		if got := truncate(s, n); !utf8.ValidString(got) {
			t.Fatalf("truncate(%d) is not valid UTF-8: %q", n, got)
		}
	}
	if got := truncate("日本語テキスト", 4); !utf8.ValidString(got) || !strings.HasPrefix(got, "日") {
		t.Fatalf("got %q", got)
	}
}

func TestAuditKeepsOnlyRealFactIDs(t *testing.T) {
	var recall strings.Builder
	recall.WriteString("#3 [home] The door code is #4521\n#7 [work] PIN #1234 for the badge\n")
	for i := 100; i < 125; i++ {
		fmt.Fprintf(&recall, "#%d [misc] fact %d\n", i, i)
	}
	got := auditInput("recall", []byte(`{"query":"code"}`), recall.String(), 300)
	for _, leak := range []string{"4521", "1234"} {
		if strings.Contains(got, leak) {
			t.Fatalf("a number from a fact's content reached the audit: %q", got)
		}
	}
	if !strings.Contains(got, "facts=#3,#7,#100,") || !strings.HasSuffix(got, ",#124") {
		t.Fatalf("want every listed id, got %q", got)
	}
	for tool, res := range map[string]string{
		"remember": "remembered (#9)",
		"forget":   `forgot #9 [home]: "Code is #4521…" (20 characters)`,
	} {
		if got := auditInput(tool, []byte(`{"id":9}`), res, 300); !strings.HasSuffix(got, "facts=#9") || strings.Contains(got, "4521") {
			t.Fatalf("%s: got %q", tool, got)
		}
	}
}
