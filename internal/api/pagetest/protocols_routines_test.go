package pagetest

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/chromedp/chromedp"
)

// A routine reads when it runs in words, with the screen's clock, and has
// an on/off switch, Skip next (which then says so) and a time field.
func TestProtocolsSwitchSkipAndTime(t *testing.T) {
	brief := map[string]any{"name": "morning briefing", "description": "The day at a glance", "schedule": "0 7 * * *", "when": "every day at 7:00", "enabled": true, "pack": "news"}
	with := func(kv ...any) map[string]any {
		m := map[string]any{}
		for k, v := range brief {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	var mu sync.Mutex
	bodies := map[string]string{}
	answer := func(key string, v map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies[key] = string(b)
			mu.Unlock()
			jsonH(v)(w, r)
		}
	}
	got := func(key string) string {
		mu.Lock()
		defer mu.Unlock()
		return bodies[key]
	}
	d := newDaemon(t)
	d.handle("GET /protocols/installed", jsonH(map[string]any{"protocols": []any{brief}, "packs": []any{}}))
	d.handle("POST /protocols/morning briefing/skip", answer("skip", with("skipping", true)))
	d.handle("POST /protocols/morning briefing/schedule", answer("schedule", with("schedule", "30 8 * * 1-5", "when", "weekdays at 8:30", "pack", "", "skipping", true)))
	d.handle("POST /protocols/morning briefing/enabled", answer("enabled", with("schedule", "30 8 * * 1-5", "when", "weekdays at 8:30", "pack", "", "enabled", false)))
	ctx := tab(t)
	run(t, ctx, desktop(), utc(), chromedp.Navigate(d.url("/protocols"))) // a 12-hour clock
	waitFor(t, ctx, `!!document.querySelector('#installed .when')`, "the routine")
	if text := textOf(t, ctx, "#installed .when"); text != "Every day at 7:00 AM" {
		t.Fatalf("the schedule should read in words, with the screen's clock: %q", text)
	}
	if text := textOf(t, ctx, "#installed"); strings.Contains(text, "0 7 * * *") {
		t.Fatalf("the page shows cron: %q", text)
	}
	if !eval[bool](t, ctx, `document.querySelector('#installed [role=switch]').checked && document.querySelector('#installed form.sched input').value === '07:00'`) {
		t.Fatal("the switch should be on and the time field at 07:00")
	}

	run(t, ctx, chromedp.Click("#installed .skip", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#installed .skip').disabled && document.querySelector('#installed .skip').textContent.startsWith('Skipping the next one')`, "Skipping the next one")

	run(t, ctx, chromedp.SetValue("#installed form.sched input", "08:30", chromedp.ByQuery),
		chromedp.SetValue("#installed form.sched select", "1-5", chromedp.ByQuery),
		chromedp.Click("#installed form.sched button", chromedp.ByQuery))
	d.waitCalled("POST /protocols/morning briefing/schedule", 1)
	waitFor(t, ctx, `document.querySelector('#installed .when').textContent === 'Weekdays at 8:30 AM'`, "the new time in words")
	if b := got("schedule"); !strings.Contains(b, `"schedule":"30 8 * * 1-5"`) {
		t.Fatalf("the time field sent %s", b)
	}
	if text := textOf(t, ctx, "#installed .msg"); !strings.Contains(text, "your own copy now") || !strings.Contains(text, "“news” pack") {
		t.Fatalf("moving a pack's routine should say it is the owner's copy now: %q", text)
	}

	run(t, ctx, chromedp.Click("#installed [role=switch]", chromedp.ByQuery))
	d.waitCalled("POST /protocols/morning briefing/enabled", 1)
	waitFor(t, ctx, `!document.querySelector('#installed [role=switch]').checked && document.querySelector('#installed .swt').textContent === 'Off' && document.querySelector('#installed .skip').hidden`, "the switch off")
	if b := got("enabled"); !strings.Contains(b, `"enabled":false`) {
		t.Fatalf("the switch sent %s", b)
	}
	if text := textOf(t, ctx, "#installed .msg"); !strings.Contains(text, "Off.") {
		t.Fatalf("the switch should say what it did: %q", text)
	}
}
