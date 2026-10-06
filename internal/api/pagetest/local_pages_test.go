package pagetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"github.com/MavrkAI/Mirrin/internal/qr"
)

// The device and safety pages: Add your phone, Devices, Backup, Trust and
// the device review after a restore. They share the settings pages' helper,
// so the not-paired, error and empty-state tests cover them too.
func init() {
	settingsPages = append(settingsPages,
		settingsPage{path: "/devices/page", reads: []string{"GET /devices"},
			empty: map[string]any{"GET /devices": map[string]any{"devices": []any{}, "notifications": []any{}}}, says: "Nothing is paired yet"},
		settingsPage{path: "/backup", reads: []string{"GET /backup/status"},
			empty: map[string]any{"GET /backup/status": map[string]any{"on": false, "health": "off", "icloud": true}}, says: "Backups are off"},
		settingsPage{path: "/trust", reads: []string{"GET /trust/info"},
			empty: map[string]any{"GET /trust/info": map[string]any{"outbound": []any{}, "certs": []any{}}}, says: "Only on this computer"},
		settingsPage{path: "/restore/review", reads: []string{"GET /restore/review/state", "GET /devices"},
			empty: map[string]any{"GET /restore/review/state": map[string]any{"pending": false}, "GET /devices": map[string]any{"devices": []any{}}}, says: "Nothing to review"},
	)
}

const pairLink = "https://mac.tail1234.ts.net/pair#v=2&o=of_abcdefghij&s=Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MGFiY2RlZmdoaWpr&n=Mirrin"

func offerEvent(t *testing.T) map[string]any {
	t.Helper()
	svg, err := qr.SVG(pairLink, 280)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"step": "offer", "twin": "Mirrin", "qr": string(svg), "link": pairLink, "expires": time.Now().Add(10 * time.Minute).Format(time.RFC3339),
		"route":  map[string]any{"kind": "tailscale", "base_url": "https://mac.tail1234.ts.net", "ready": true},
		"routes": []any{map[string]any{"kind": "tailscale", "base_url": "https://mac.tail1234.ts.net", "ready": true}, map[string]any{"kind": "relay", "problem": "No relay of your own is set up."}},
		"peers":  []any{map[string]any{"name": "akshays-iphone", "os": "iOS", "online": true}}}
}

var tailscaleFixes = []any{
	map[string]any{"kind": "tailscale", "problem": "Tailscale is running, but HTTPS certificates are off for your tailnet.", "fix": "In the Tailscale admin console, open DNS and turn on HTTPS Certificates.", "fix_url": "https://login.tailscale.com/admin/dns"},
	map[string]any{"kind": "relay", "problem": "No relay of your own is set up.", "fix": "If you run mirrin-relay on a server of yours, run `mirrin reach use relay` on this computer."},
}

// sseH is an event stream that sends first, then whatever arrives on more.
func sseH(d *daemon, first []map[string]any, more <-chan map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		send := func(ev map[string]any) {
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
		for _, ev := range first {
			send(ev)
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case <-d.done:
				return
			case ev := <-more:
				send(ev)
			}
		}
	}
}

var phoneDevice = map[string]any{"id": "0123456789abcdef", "name": "Akshay's iPhone", "kind": "pwa", "scopes": []string{"view", "chat", "approve"}}

func devicesList() map[string]any {
	now := time.Now()
	return map[string]any{"devices": []any{
		map[string]any{"id": "0123456789abcdef", "name": "Akshay's iPhone", "kind": "pwa", "scopes": []string{"view", "chat", "approve"}, "via": "tailscale",
			"created": now.Add(-40 * 24 * time.Hour), "last_seen": now.Add(-3 * time.Minute), "last_ip": "100.101.12.4", "passkey_count": 1},
		map[string]any{"id": "fedcba9876543210", "name": "Kitchen screen", "kind": "kiosk", "scopes": []string{"view"}, "via": "relay",
			"created": now.Add(-100 * 24 * time.Hour), "last_seen": now.Add(-26 * time.Hour), "last_ip": "203.0.113.20"},
		map[string]any{"id": "00112233aabbccdd", "name": "Work laptop terminal with a really long name that wraps", "kind": "cli", "scopes": []string{"view", "chat", "approve"}, "via": "tailscale",
			"created": now.Add(-5 * 24 * time.Hour), "last_seen": now.Add(-2 * time.Hour), "last_ip": "100.64.0.9"},
		map[string]any{"id": "99887766554433aa", "name": "Old iPad", "kind": "pwa", "scopes": []string{"view", "chat", "approve"}, "created": now.Add(-300 * 24 * time.Hour), "revoked_at": now.Add(-60 * 24 * time.Hour)},
		map[string]any{"id": "aaaaaaaaaaaaaaaa", "name": "Safari on this Mac", "kind": "local", "scopes": []string{"view", "chat", "approve", "admin"}},
	}, "notifications": []string{"0123456789abcdef"}}
}

func backupOn() map[string]any {
	return map[string]any{"on": true, "target": "icloud", "where": "iCloud Drive › Mirrin Backups", "kit_id": "k3q9-x2mz", "health": "ok",
		"detail": "Last backup 3 hours ago (iCloud Drive › Mirrin Backups)", "last_good": time.Now().Add(-3 * time.Hour), "seq": 412, "size": 48213000, "icloud": true}
}

func trustInfo() map[string]any {
	out := []any{}
	for _, o := range []struct {
		id, what, when, data, where string
		on                          bool
	}{
		{"model", "Your model provider", "Every reply, routine and background task, and an hourly check", "Your messages, the memories relevant to them and what tools found.", "Anthropic", true},
		{"weather", "Open-Meteo, for the weather", "The presence screen, every 15 minutes", "Rough coordinates for your city.", "api.open-meteo.com", true},
		{"channels", "Your messaging apps", "While each channel is connected", "The messages you exchange with your twin there.", "Telegram, WhatsApp", true},
		{"push", "Apple, Google, Mozilla or Microsoft push services", "When a paired phone has notifications on", "Encrypted notifications.", "", true},
		{"relay", "Your own relay", "While other devices reach your twin through it", "Encrypted connections.", "relay.example.com", true},
		{"google", "Google", "Every few minutes for mail and calendar", "Requests to your own Calendar, Gmail and Drive.", "", false},
		{"updates", "GitHub", "Only when you run mirrin update", "A request for the latest release.", "", false},
	} {
		out = append(out, map[string]any{"id": o.id, "what": o.what, "when": o.when, "data": o.data, "where": o.where, "on": o.on})
	}
	return map[string]any{"outbound": out,
		"certs": []any{map[string]any{"route": "relay", "host": "twin.example.com", "current": "q1Wz3e1XgFz2wqk3qGSG7m1b2b6yx8gFf8W1Q2x4yJ0", "next": "Zp7fQv0l2kz9x1yH3m4n5b6v7c8x9z0a1s2d3f4g5h6",
			"sha256": "3A:7F:12:9C:44:0B:DE:21:77:E1:5A:90:C3:3D:88:0F:61:BA:47:2E:9D:10:5C:FE:08:43:6B:D2:11:A0:7C:E9", "issuer": "R11", "not_after": time.Now().Add(60 * 24 * time.Hour)}},
		"ct": map[string]any{"state": "ok", "detail": "No stray certificates", "checked": time.Now().Add(-2 * time.Hour), "sources_up": 2}}
}

// localDaemon is the stand-in with realistic answers for every page.
func localDaemon(t *testing.T, addFirst []map[string]any, more <-chan map[string]any) *daemon {
	d := newDaemon(t)
	d.handle("GET /devices", jsonH(devicesList()))
	d.handle("GET /devices/add/state", sseH(d, addFirst, more))
	d.handle("GET /devices/routes", jsonH(map[string]any{"routes": tailscaleFixes, "problems": tailscaleFixes, "peers": []any{}}))
	d.handle("GET /backup/status", jsonH(backupOn()))
	d.handle("GET /trust/info", jsonH(trustInfo()))
	d.handle("GET /restore/review/state", jsonH(map[string]any{"pending": true, "restored_at": time.Now().Add(-10 * time.Minute)}))
	d.handle("GET /reach/info", jsonH(reachInfo(false)))
	return d
}

// "Add your phone" shows the code, lights each step as the phone takes it,
// and "That wasn't me" asks the twin to cut the phone off.
func TestAddPageLightsStepsAndRevokes(t *testing.T) {
	more := make(chan map[string]any, 8)
	d := localDaemon(t, []map[string]any{offerEvent(t)}, more)
	d.handle("POST /devices/0123456789abcdef/revoke", jsonH(map[string]any{"device": phoneDevice}))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/devices/add")))
	waitFor(t, ctx, visible("#offer")+` && document.querySelector('#qrImg').naturalWidth > 0`, "the code")
	if s := textOf(t, ctx, "#routeText"); !strings.Contains(s, "mac.tail1234.ts.net") || !strings.Contains(s, "Tailscale app") {
		t.Fatalf("route: %q", s)
	}
	if s := textOf(t, ctx, "#peerHint"); !strings.Contains(s, "akshays-iphone is on your tailnet") {
		t.Fatalf("peer hint: %q", s)
	}
	for _, step := range []string{"scanned", "claimed", "installed", "notifications", "passkey", "test_received"} {
		ev := map[string]any{"step": step}
		if step == "claimed" {
			ev["device"] = phoneDevice
		}
		more <- ev
		waitFor(t, ctx, fmt.Sprintf(`document.querySelector('.steps li[data-step=%q]').classList.contains('lit')`, step), step+" lit")
		if step == "claimed" && !eval[bool](t, ctx, visible("#who")+` && !`+visible("#offer")) {
			t.Fatal("the pairing card didn't replace the code")
		}
		if step == "notifications" && !eval[bool](t, ctx, visible("#sendTest")) {
			t.Fatal("no Send a test notification once notifications are on")
		}
	}
	waitFor(t, ctx, visible("#allSet"), "All set")
	if n := eval[int](t, ctx, `document.querySelectorAll('.steps li.lit').length`); n != 6 {
		t.Fatalf("%d steps lit", n)
	}
	if s := textOf(t, ctx, "#live"); s == "" {
		t.Fatal("the steps aren't announced")
	}
	run(t, ctx, chromedp.Click("#notMe", chromedp.ByQuery))
	d.waitCalled("POST /devices/0123456789abcdef/revoke", 1)
	waitFor(t, ctx, `document.querySelector('#whoMsg').textContent.includes("Akshay's iPhone can't reach")`, "the revoke said so")
}

// With no working route the page says what to set up, Tailscale first,
// with the exact fix; the menu's "Reach from anywhere" shows the same.
func TestAddPageEmptyStateNamesTailscaleFirst(t *testing.T) {
	d := localDaemon(t, []map[string]any{{"step": "no_route", "twin": "Mirrin", "routes": tailscaleFixes}}, nil)
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/devices/add")))
	waitFor(t, ctx, visible("#empty"), "the empty state")
	first := textOf(t, ctx, "#fixes li:first-child")
	if !strings.HasPrefix(first, "Tailscale") || !strings.Contains(first, "turn on HTTPS Certificates") {
		t.Fatalf("first fix: %q", first)
	}
	if eval[bool](t, ctx, visible("#stepsBox")) || eval[bool](t, ctx, visible("#offer")) {
		t.Fatal("steps or a code with nothing to scan")
	}
	if code := textOf(t, ctx, "#fixes li:nth-child(2) code"); code != "mirrin reach use relay" {
		t.Fatalf("the relay fix's command: %q", code)
	}
	run(t, ctx, chromedp.Navigate(d.url("/devices/add?view=reach")))
	waitFor(t, ctx, visible("#empty")+` && document.querySelector('#fixes').children.length === 2`, "the reach view")
	if title := eval[string](t, ctx, `document.title`); !strings.Contains(title, "Reach from anywhere") {
		t.Fatalf("title %q", title)
	}
	if d.count("GET /devices/add/state") != 1 {
		t.Fatal("the reach view made a pairing code")
	}
}

// Devices lists, renames and removes, from the keyboard; local browsers
// aren't listed, and removed ones fold away.
func TestDevicesPageRenamesAndRemoves(t *testing.T) {
	d := localDaemon(t, nil, nil)
	d.handle("POST /devices/fedcba9876543210/rename", jsonH(map[string]any{"device": map[string]any{"id": "fedcba9876543210", "name": "Hall screen"}}))
	d.handle("POST /devices/fedcba9876543210/revoke", jsonH(map[string]any{"device": map[string]any{"id": "fedcba9876543210"}}))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/devices/page")))
	waitFor(t, ctx, `document.querySelectorAll('#list .dev').length === 3`, "three devices")
	if eval[bool](t, ctx, `document.body.innerText.includes('Safari on this Mac')`) {
		t.Fatal("a browser on this computer is listed")
	}
	if s := textOf(t, ctx, "#goneSum"); s != "Removed (1)" {
		t.Fatalf("removed: %q", s)
	}
	card := `#list li[data-id="fedcba9876543210"]`
	run(t, ctx, chromedp.Focus(card+" .ren", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	waitFor(t, ctx, `document.activeElement === document.querySelector('`+card+` .rename input')`, "focus in the name field")
	run(t, ctx, chromedp.SetValue(card+" .rename input", "Hall screen", chromedp.ByQuery), chromedp.Evaluate(`document.querySelector('`+card+` .rename').requestSubmit(); true`, nil))
	d.waitCalled("POST /devices/fedcba9876543210/rename", 1)
	waitFor(t, ctx, `document.querySelector('`+card+` .msg').textContent.includes('Renamed to Hall screen')`, "renamed")
	run(t, ctx, chromedp.Click(card+" .rm", chromedp.ByQuery))
	waitFor(t, ctx, visible(card+" .confirm")+` && document.activeElement === document.querySelector('`+card+` .no')`, "the confirmation, on Keep it")
	run(t, ctx, chromedp.Click(card+" .yes", chromedp.ByQuery))
	d.waitCalled("POST /devices/fedcba9876543210/revoke", 1)
	waitFor(t, ctx, `document.querySelector('#live').textContent.includes("can't reach")`, "the removal announced")
}

// The Recovery Kit: words shown once, printable alone, and backups turn on
// only when word 7 is typed back.
func TestBackupKitNeedsWordSeven(t *testing.T) {
	d := localDaemon(t, nil, nil)
	d.handle("GET /backup/status", jsonH(map[string]any{"on": false, "health": "off", "icloud": true}))
	words := []string{"abandon", "ability", "able", "about", "above", "absent", "absorb", "abstract", "absurd", "abuse", "access", "accident"}
	d.handle("POST /backup/kit", jsonH(map[string]any{"kit": "kit_1", "words": words, "kit_id": "k3q9-x2mz", "check": 7, "where": "iCloud Drive › Mirrin Backups", "twin": "Mirrin", "made": "29 September 2026"}))
	var typed string
	d.handle("POST /backup/kit/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		typed = body["word"]
		if body["word"] != "absorb" {
			refusedH(400, "wrong_word", "That isn't word 7.", "Look at word 7 on your paper and type it again.")(w, r)
			return
		}
		jsonH(backupOn())(w, r)
	})
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/backup")))
	waitFor(t, ctx, `document.querySelector('#headline').textContent === 'Backups are off' && document.querySelector('#tIcloud').checked`, "the page, iCloud chosen")
	run(t, ctx, chromedp.Click("#destGo", chromedp.ByQuery))
	waitFor(t, ctx, visible("#sheet")+` && document.querySelectorAll('#words li').length === 12`, "the words")
	if !eval[bool](t, ctx, `!`+visible("#dest")) {
		t.Fatal("the destination form stays while the words show")
	}
	// Printing shows the kit alone.
	run(t, ctx, emulation.SetEmulatedMedia().WithMedia("print"))
	onlyKit := eval[bool](t, ctx, visible("#sheet")+` && !`+visible("h1")+` && !`+visible("nav")+` && !`+visible("#wrote"))
	run(t, ctx, emulation.SetEmulatedMedia().WithMedia(""))
	if !onlyKit {
		t.Fatal("printing shows more than the kit")
	}
	run(t, ctx, chromedp.Click("#wrote", chromedp.ByQuery))
	waitFor(t, ctx, visible("#check")+` && document.activeElement === document.querySelector('#word') && !`+visible("#sheet"), "the check, words hidden")
	if l := textOf(t, ctx, "#wordLabel"); l != "To check, type word 7" {
		t.Fatalf("label %q", l)
	}
	run(t, ctx, chromedp.SetValue("#word", "abuse", chromedp.ByQuery), chromedp.Click("#finish", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#checkMsg').textContent.includes("That isn't word 7")`, "the wrong word refused")
	if eval[bool](t, ctx, `document.querySelector('#headline').textContent === 'Backups are on'`) {
		t.Fatal("backups on with a wrong word")
	}
	run(t, ctx, chromedp.SetValue("#word", "absorb", chromedp.ByQuery), chromedp.Click("#finish", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#headline').textContent === 'Backups are on' && !`+visible("#check")+` && document.querySelectorAll('#words li').length === 0`, "backups on, words gone from the page")
	if typed != "absorb" {
		t.Fatalf("finished with %q", typed)
	}
}

// Every one of these pages fits a 360px phone without sideways scrolling,
// has its landmarks and a heading, and its text fields don't zoom iOS.
func TestLocalPagesFitAPhoneAndHaveLandmarks(t *testing.T) {
	more := make(chan map[string]any)
	d := localDaemon(t, []map[string]any{offerEvent(t)}, more)
	ctx := tab(t)
	for _, c := range []struct{ path, ready string }{
		{"/devices/add", visible("#offer")},
		{"/devices/add?view=reach", visible("#empty")},
		{"/devices/page", `document.querySelectorAll('#list .dev').length === 3`},
		{"/backup", visible("#status")},
		{"/trust", `document.querySelectorAll('#out li').length > 3`},
		{"/restore/review", `document.querySelectorAll('#list input').length === 3`},
		{"/reach", `document.querySelectorAll('#cards .card').length === 3`},
	} {
		run(t, ctx, chromedp.EmulateViewport(360, 740, chromedp.EmulateScale(2), chromedp.EmulateMobile, chromedp.EmulateTouch), chromedp.Navigate(d.url(c.path)))
		waitFor(t, ctx, c.ready, c.path+" loaded")
		if w := eval[int](t, ctx, `document.documentElement.scrollWidth`); w > 360 {
			t.Errorf("%s is %dpx wide on a 360px phone", c.path, w)
		}
		if !eval[bool](t, ctx, `!!document.querySelector('main') && !!document.querySelector('nav[aria-label]') && [...document.querySelectorAll('h1')].filter(h => h.offsetParent).length === 1`) {
			t.Errorf("%s: no main, labelled nav or single h1", c.path)
		}
		small := eval[[]string](t, ctx, `[...document.querySelectorAll('input:not([type=checkbox]):not([type=radio])')].map(e => [e.id, parseFloat(getComputedStyle(e).fontSize)]).filter(([, px]) => px < 16).map(([n]) => n)`)
		if len(small) > 0 {
			t.Errorf("%s: fields under 16px: %v", c.path, small)
		}
		unnamed := eval[[]string](t, ctx, `[...document.querySelectorAll('button, a[href], input')].filter(e => e.offsetParent && !(e.innerText || e.getAttribute('aria-label') || (e.labels && e.labels.length) || e.value)).map(e => e.outerHTML.slice(0, 80))`)
		if len(unnamed) > 0 {
			t.Errorf("%s: controls without a name: %v", c.path, unnamed)
		}
	}
}

// Trust shows what is in use first, the certificate's fingerprints, and how
// to check them, with the name filled in.
func TestTrustPageShowsCertificateAndChecks(t *testing.T) {
	d := localDaemon(t, nil, nil)
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/trust")))
	waitFor(t, ctx, `document.querySelectorAll('#out li').length === 7 && !!document.querySelector('#certs .fp')`, "the trust page")
	if first := textOf(t, ctx, "#out li:first-child .badge"); first != "In use" {
		t.Fatalf("first line: %q", first)
	}
	body := eval[string](t, ctx, `document.querySelector('main').innerText`)
	for _, want := range []string{"3A:7F:12", "q1Wz3e1XgFz2", "mirrin reach fingerprint", "openssl s_client -connect twin.example.com:443", "mirrin reach verify", "Nothing unexpected"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page doesn't say %q", want)
		}
	}
	if href := eval[string](t, ctx, `document.querySelector('#verify a[href*="crt.sh"]').href`); href != "https://crt.sh/?q=twin.example.com" {
		t.Fatalf("crt.sh link %q", href)
	}
}

// MIRRIN_SCREENSHOTS=<dir> writes each page at 360 and 1280 pixels wide, in
// light and dark, for review.
func TestLocalPagesScreenshots(t *testing.T) {
	dir := os.Getenv("MIRRIN_SCREENSHOTS")
	if dir == "" {
		t.Skip("set MIRRIN_SCREENSHOTS to a folder to write the pages' screenshots")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	more := make(chan map[string]any, 8)
	d := localDaemon(t, []map[string]any{offerEvent(t), {"step": "scanned"}, {"step": "claimed", "device": phoneDevice}, {"step": "installed"}}, more)
	d.handle("POST /backup/kit", jsonH(map[string]any{"kit": "kit_1", "words": []string{"orbit", "velvet", "canyon", "lemon", "harbor", "tiger", "absorb", "quiet", "maple", "random", "shiver", "useful"}, "kit_id": "k3q9-x2mz", "check": 7, "where": "iCloud Drive › Mirrin Backups", "twin": "Mirrin", "made": "29 September 2026"}))
	ctx := tab(t)
	offering := localDaemon(t, []map[string]any{offerEvent(t), {"step": "scanned"}}, nil)
	shots := []struct {
		name, path, ready string
		then, after       string // run once ready, then wait for after
	}{
		{"add-offer", "/devices/add", visible("#offer") + ` && document.querySelector('#qrImg').naturalWidth > 0`, "", ""},
		{"add", "/devices/add", visible("#who"), "", ""},
		{"add-empty", "/devices/add?view=reach", visible("#empty"), "", ""},
		{"add-other", "/devices/add", visible("#who"), `handle({step:'claimed_other',device:{id:'dv_other',name:'Unknown iPhone'}})`, visible("#others")},
		{"devices", "/devices/page", `document.querySelectorAll('#list .dev').length === 3`, "", ""},
		{"backup", "/backup", visible("#status"), "", ""},
		{"backup-kit", "/backup", visible("#status"), `document.querySelector('#newYes').click()`, `document.querySelectorAll('#words li').length === 12`},
		{"trust", "/trust", `!!document.querySelector('#certs .fp')`, "", ""},
		{"restore-review", "/restore/review", `document.querySelectorAll('#list input').length === 3`, "", ""},
	}
	for _, theme := range []string{"light", "dark"} {
		for _, width := range []int64{360, 1280} {
			for _, s := range shots {
				on := d
				if s.name == "add-offer" {
					on = offering
				}
				view := chromedp.EmulateViewport(width, 900)
				if width == 360 {
					view = chromedp.EmulateViewport(width, 780, chromedp.EmulateScale(2), chromedp.EmulateMobile, chromedp.EmulateTouch)
				}
				run(t, ctx, view, emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: theme}}),
					chromedp.Navigate(on.url(s.path)))
				waitFor(t, ctx, s.ready, s.name)
				if s.then != "" {
					run(t, ctx, chromedp.Evaluate(s.then+`; true`, nil))
					waitFor(t, ctx, s.after, s.name+" after "+s.then)
				}
				time.Sleep(150 * time.Millisecond)
				var png []byte
				run(t, ctx, chromedp.FullScreenshot(&png, 100))
				name := filepath.Join(dir, fmt.Sprintf("%s-%d-%s.png", s.name, width, theme))
				if err := os.WriteFile(name, png, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}
