package pagetest

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// On this computer a reply has a Why? that lists the facts it drew on,
// with the honest lead the API gives, and a Forget beside each that
// forgets it there and then. Tapping Why? again puts the list away.
func TestWhyListsWhatAReplyDrewOnWithForget(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	var asked string
	d.handle("POST /memory/why", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Reply string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		d.mu.Lock()
		asked = in.Reply
		d.mu.Unlock()
		jsonH(map[string]any{"known": true, "everything": true, "lead": "I had everything you've told me in mind; these matched most closely:",
			"facts": []map[string]any{{"id": 7, "subject": "family", "content": "Priya is Akshay's sister and lives in Pune.", "how": "matched"}, {"id": 8, "subject": "travel", "content": "Akshay is going to Pune in May.", "how": "matched"}}})(w, r)
	})
	d.handle("DELETE /memory/facts/7", jsonH(map[string]bool{"ok": true}))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded+` && es && es.readyState === 1`, "the screen loaded")
	voice := map[string]any{"channel": "voice"}
	d.push(map[string]any{"kind": "heard", "text": "Where does my sister live?", "data": voice})
	d.push(map[string]any{"kind": "said", "text": "Priya's in Pune.", "data": voice})
	line := `#transcript .line.said:last-child`
	waitFor(t, ctx, `!!document.querySelector(`+jsString(line+" .why")+`)`, "a Why? beside the reply")
	run(t, ctx, chromedp.Evaluate(`document.querySelector(`+jsString(line+" .why")+`).click(); true`, nil))
	d.waitCalled("POST /memory/why", 1)
	box := line + " .whybox"
	waitFor(t, ctx, `!!document.querySelector(`+jsString(box+" li")+`)`, "the facts it drew on")
	d.mu.Lock()
	got := asked
	d.mu.Unlock()
	if got != "Priya's in Pune." {
		t.Fatalf("asked why about %q", got)
	}
	if text := textOf(t, ctx, box); !strings.HasPrefix(text, "I had everything you've told me in mind; these matched most closely:") ||
		!strings.Contains(text, "Priya is Akshay's sister and lives in Pune. · Forget") || !strings.Contains(text, "Akshay is going to Pune in May. · Forget") {
		t.Fatalf("why box reads %q", text)
	}
	run(t, ctx, chromedp.Evaluate(`document.querySelector(`+jsString(box+" li .forget")+`).click(); true`, nil))
	d.waitCalled("DELETE /memory/facts/7", 1)
	waitFor(t, ctx, `document.querySelector(`+jsString(box+" li")+`).textContent === 'Forgotten.'`, "Forget says Forgotten.")
	if n := eval[int](t, ctx, `document.querySelectorAll(`+jsString(box+" .forget")+`).length`); n != 1 {
		t.Fatalf("%d Forget buttons left, want 1", n)
	}
	run(t, ctx, chromedp.Evaluate(`document.querySelector(`+jsString(line+" .why")+`).click(); true`, nil))
	waitFor(t, ctx, `!document.querySelector(`+jsString(box)+`)`, "Why? again puts the list away")
}

// Another device never offers Why?: what the twin knows about its owner
// opens on this computer only, as the memory page does.
func TestWhyOnlyOnThisComputer(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.remoteURL("/ui")))
	waitFor(t, ctx, loaded+` && es && es.readyState === 1`, "the screen loaded")
	d.push(map[string]any{"kind": "heard", "text": "Where does my sister live?", "data": map[string]any{"channel": "screen"}})
	d.push(map[string]any{"kind": "said", "text": "Priya's in Pune.", "data": map[string]any{"channel": "screen"}})
	waitFor(t, ctx, `[...document.querySelectorAll('#transcript .line.said')].some(l => l.textContent.includes("Priya's in Pune."))`, "the reply")
	if eval[bool](t, ctx, `!!document.querySelector('#transcript .why')`) {
		t.Fatal("another device offers Why?")
	}
}
