package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/llm"
	memskill "github.com/MavrkAI/Mirrin/internal/skills/memory"
)

// The screen's "Remember this page" keeps the page once, shows it as
// noted with Undo, and with no browser open says there's nothing to keep.
func TestRememberThisPageFromTheBrowserSheet(t *testing.T) {
	td := newTestDaemon(t, butler)
	ctx := context.Background()
	if _, err := td.BrowserRemember(ctx); !errors.Is(err, api.ErrNoWebPage) {
		t.Fatalf("no browser: %v", err)
	}
	got := listen(t, td.bus)
	p := memskill.Page{URL: "https://www.bbcgoodfood.com/recipes/chicken-traybake", Title: "Chicken traybake"}
	if already, err := td.rememberPage(ctx, p); err != nil || already {
		t.Fatalf("first save: %v %v", already, err)
	}
	if already, err := td.rememberPage(ctx, p); err != nil || !already {
		t.Fatalf("second save: %v %v", already, err)
	}
	fs, _ := td.store.Recall(ctx, "chicken", 5)
	if len(fs) != 1 || fs[0].Subject != "recipes" {
		t.Fatalf("kept: %+v", fs)
	}
	evs := noted(got())
	if len(evs) != 1 || !strings.Contains(evs[0].Text, "Chicken traybake") {
		t.Fatalf("noted: %+v", evs)
	}
	if err := td.UndoFact(ctx, fs[0].ID); err != nil {
		t.Fatalf("undo: %v", err)
	}
}

// Said in a chat, "remember this page" keeps it through remember_page, and
// a week later recall brings it back for the twin to offer.
func TestRememberThisPageInAChatComesBackLater(t *testing.T) {
	td := newTestDaemon(t, func(last string, _ llm.Request) llm.Response {
		switch {
		case strings.Contains(last, "remember this page"):
			return call("p1", "remember_page", `{"note":"A one-tray roast chicken dinner"}`)
		case strings.Contains(last, "remembered (#"):
			return say("Saved.")
		case strings.Contains(last, "what's for dinner"):
			return call("r1", "recall", `{"query":"chicken recipe"}`)
		case strings.Contains(last, "#") && strings.Contains(last, "[recipes]"):
			return say("You saved a chicken traybake from BBC Good Food. Want it?")
		}
		return say("Heard: " + last)
	})
	page := memskill.Page{URL: "https://www.bbcgoodfood.com/recipes/chicken-traybake", Title: "Chicken traybake recipe"}
	td.agent.Tools().Register(memskill.PageTool(td.store, func(context.Context) memskill.Page { return page }))
	if r := td.owner(t, "remember this page"); r != "Saved." {
		t.Fatalf("reply %q", r)
	}
	if r := td.owner(t, "what's for dinner?"); !strings.Contains(r, "BBC Good Food") {
		t.Fatalf("reply %q", r)
	}
}
