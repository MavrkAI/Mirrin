package pagetest

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/chromedp/chromedp"
)

// The memory page says how many facts there are and pages through them,
// past the thousand the page once stopped at.
func TestMemoryPageShowsHowManyAndPages(t *testing.T) {
	const total = 1500
	d := newDaemon(t)
	d.handle("GET /memory/audit", jsonH([]any{}))
	d.handle("GET /memory/twin", jsonH(map[string]any{"name": "Mirrin"}))
	d.handle("GET /memory/portrait", jsonH(map[string]any{"text": "", "updated_at": "0001-01-01T00:00:00Z"}))
	d.handle("GET /memory/facts", func(w http.ResponseWriter, r *http.Request) {
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		lim, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		facts := []any{}
		for i := off + 1; i <= min(off+lim, total); i++ {
			facts = append(facts, map[string]any{"id": i, "subject": "general", "content": fmt.Sprintf("fact number %d", i), "source": "test", "created_at": "2026-09-01T10:00:00Z"})
		}
		jsonH(map[string]any{"facts": facts, "total": total, "offset": off, "limit": lim})(w, r)
	})
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/memory")))
	waitFor(t, ctx, `document.querySelector('#factsCount').textContent.includes('of 1,500')`, "the count")
	if got := textOf(t, ctx, "#factsCount"); got != "Showing 1–200 of 1,500" {
		t.Fatalf("count reads %q", got)
	}
	if eval[bool](t, ctx, visible("#factsPrev")) || !eval[bool](t, ctx, visible("#factsNext")) {
		t.Fatal("on the first page only Later facts belongs")
	}
	// One page at a time: a click while the previous page is still loading
	// would land on the wrong page under load.
	for i := 1; i <= 7; i++ {
		run(t, ctx, chromedp.Click("#factsNext", chromedp.ByQuery))
		want := fmt.Sprintf("Showing %s–", commas(i*200+1))
		waitFor(t, ctx, fmt.Sprintf("document.querySelector('#factsCount').textContent.startsWith(%q)", want), "page "+strconv.Itoa(i+1))
	}
	waitFor(t, ctx, `document.querySelector('#factsCount').textContent === 'Showing 1,401–1,500 of 1,500'`, "the last page")
	if text := textOf(t, ctx, "#facts"); !strings.Contains(text, "fact number 1500") || eval[bool](t, ctx, visible("#factsNext")) {
		t.Fatalf("last page: %q", text[:min(len(text), 200)])
	}
}

// A fact added from the page shows: the page goes to the last page, where
// the newest fact is, not the first 200.
func TestMemoryPageShowsAFactJustAdded(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /memory/audit", jsonH([]any{}))
	d.handle("GET /memory/twin", jsonH(map[string]any{"name": "Mirrin"}))
	d.handle("GET /memory/portrait", jsonH(map[string]any{"text": "", "updated_at": "0001-01-01T00:00:00Z"}))
	var mu sync.Mutex
	total := 450
	d.handle("GET /memory/facts", func(w http.ResponseWriter, r *http.Request) {
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		lim, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		mu.Lock()
		n := total
		mu.Unlock()
		facts := []any{}
		for i := off + 1; i <= min(off+lim, n); i++ {
			c := fmt.Sprintf("fact number %d", i)
			if i == 451 {
				c = "likes green tea"
			}
			facts = append(facts, map[string]any{"id": i, "subject": "general", "content": c, "source": "test", "created_at": "2026-09-01T10:00:00Z"})
		}
		jsonH(map[string]any{"facts": facts, "total": n, "offset": off, "limit": lim})(w, r)
	})
	d.handle("POST /memory/facts", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		total++
		mu.Unlock()
		jsonH(map[string]any{"id": 451})(w, r)
	})
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/memory")))
	waitFor(t, ctx, `document.querySelector('#factsCount').textContent.includes('of 450')`, "the count")
	run(t, ctx, chromedp.SetValue(`#add [name=content]`, "likes green tea", chromedp.ByQuery), chromedp.Click(`#add button[type=submit]`, chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#factsCount').textContent === 'Showing 401–451 of 451'`, "the last page")
	if text := textOf(t, ctx, "#facts"); !strings.Contains(text, "likes green tea") {
		t.Fatalf("the new fact isn't shown: %q", text[:min(len(text), 200)])
	}
}

// commas writes n the way the page does: 1401 as 1,401.
func commas(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	return s[:len(s)-3] + "," + s[len(s)-3:]
}

// "Forget that I'm job hunting": the portrait that said so goes with the
// fact, and the page says why it's gone and what happens next, without a
// reload.
func TestMemoryPageSaysThePortraitWasSetAside(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /memory/audit", jsonH([]any{}))
	d.handle("GET /memory/twin", jsonH(map[string]any{"name": "Mirrin"}))
	var mu sync.Mutex
	forgot := false
	d.handle("GET /memory/portrait", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		aside := forgot
		mu.Unlock()
		if aside {
			jsonH(map[string]any{"text": "", "updated_at": "0001-01-01T00:00:00Z", "aside": true})(w, r)
			return
		}
		jsonH(map[string]any{"text": "You're quietly looking for a new role.", "updated_at": "2026-09-27T08:00:00Z", "aside": false})(w, r)
	})
	d.handle("GET /memory/facts", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gone := forgot
		mu.Unlock()
		facts := []any{}
		if !gone {
			facts = append(facts, map[string]any{"id": 7, "subject": "work", "content": "job hunting", "source": "test", "created_at": "2026-09-01T10:00:00Z"})
		}
		jsonH(map[string]any{"facts": facts, "total": len(facts), "offset": 0, "limit": 200})(w, r)
	})
	d.handle("DELETE /memory/facts/7", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		forgot = true
		mu.Unlock()
		jsonH(map[string]any{"ok": true})(w, r)
	})
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/memory")))
	waitFor(t, ctx, `document.querySelector('#portrait-text').textContent.includes('new role')`, "the portrait")
	waitFor(t, ctx, `!!document.querySelector('#facts .del')`, "the fact")
	run(t, ctx, chromedp.Evaluate(`window.confirm = () => true; true`, nil), chromedp.Click("#facts .del", chromedp.ByQuery))
	const aside = "I set my portrait aside after you asked me to forget something. I'll write a fresh one on Sunday, or now if you press Refresh."
	waitFor(t, ctx, `document.querySelector('#portrait-text').textContent === `+jsString(aside), "the aside line")
	if meta := textOf(t, ctx, "#portrait-meta"); meta != "" {
		t.Fatalf("the old portrait's date is still shown: %q", meta)
	}
}
