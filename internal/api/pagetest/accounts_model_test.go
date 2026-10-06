package pagetest

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// The model key card on Accounts: Test asks the welcome check, Save saves,
// and the page never shows the key itself again.
func TestAccountsModelCardTestsAndSaves(t *testing.T) {
	const good = "sk-good-0123456789abcdef"
	var mu sync.Mutex
	state := map[string]any{"provider": "anthropic", "model": "claude-x", "key": "sk-a••••wxyz", "providers": []string{"anthropic", "openai"}}
	var saved []map[string]string
	d := newDaemon(t)
	d.handle("GET /accounts/list", jsonH([]any{}))
	d.handle("GET /accounts/model", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		jsonH(state)(w, r)
	})
	check := func(w http.ResponseWriter, r *http.Request) (map[string]string, bool) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["key"] != good {
			refusedH(400, "not_accepted", "That key wasn't accepted.", "Copy it again from your provider.")(w, r)
			return in, false
		}
		return in, true
	}
	d.handle("POST /accounts/model/check", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := check(w, r); ok {
			jsonH(map[string]bool{"ok": true})(w, r)
		}
	})
	d.handle("POST /accounts/model", func(w http.ResponseWriter, r *http.Request) {
		in, ok := check(w, r)
		if !ok {
			return
		}
		mu.Lock()
		saved = append(saved, in)
		state = map[string]any{"provider": in["provider"], "model": "gpt-x", "key": "sk-g••••cdef", "providers": []string{"anthropic", "openai"}}
		mu.Unlock()
		jsonH(state)(w, r)
	})
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/accounts#model")))
	waitFor(t, ctx, visible("#model")+` && document.querySelector('#modelNow').textContent.includes('Anthropic')`, "the model card")
	if ph := eval[string](t, ctx, `document.querySelector('#modelKey').placeholder`); !strings.Contains(ph, "sk-a••••wxyz") {
		t.Fatalf("placeholder %q", ph)
	}

	run(t, ctx, chromedp.SetValue("#modelProvider", "openai", chromedp.ByQuery), chromedp.Evaluate(`document.querySelector('#modelProvider').dispatchEvent(new Event('change')); true`, nil))
	run(t, ctx, chromedp.SendKeys("#modelKey", "sk-nope-0123456789abcdef", chromedp.ByQuery), chromedp.Click("#modelTest", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#modelMsg').textContent.includes("wasn't accepted")`, "Test says the bad key failed")
	run(t, ctx, chromedp.SetValue("#modelKey", good, chromedp.ByQuery), chromedp.Click("#modelTest", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#modelMsg').textContent.includes('That works')`, "Test says the good key works")
	mu.Lock()
	n := len(saved)
	mu.Unlock()
	if n != 0 {
		t.Fatal("Test saved the key")
	}
	run(t, ctx, chromedp.Click("#modelSave", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#modelMsg').textContent.includes('Saved') && document.querySelector('#modelNow').textContent.includes('OpenAI')`, "Save saved")
	mu.Lock()
	defer mu.Unlock()
	if len(saved) != 1 || saved[0]["provider"] != "openai" || saved[0]["key"] != good {
		t.Fatalf("saved %v", saved)
	}
	if html := eval[string](t, ctx, `document.body.innerHTML + document.querySelector('#modelKey').value`); strings.Contains(html, good) {
		t.Fatal("the page still shows the key")
	}
}

// A connected account's notices link to where to fix them, and "Use a
// different client" sets the client aside.
func TestAccountsNoticesAndDifferentClient(t *testing.T) {
	d := newDaemon(t)
	connected := map[string]any{"name": "google", "label": "Google", "blurb": "Connected, with something to fix: The Drive API is turned off.", "has_client": true, "connected": true,
		"features": []any{}, "steps": []any{}, "notices": []any{map[string]any{"text": "The Drive API is turned off for your Google Cloud project.", "url": "https://console.cloud.google.com/apis/library/drive.googleapis.com", "link": "Turn it on"}}}
	d.handle("GET /accounts/list", jsonH([]any{connected}))
	replaced := make(chan struct{}, 1)
	d.handle("POST /accounts/google/client/replace", func(w http.ResponseWriter, r *http.Request) {
		d.handle("GET /accounts/list", jsonH([]any{map[string]any{"name": "google", "label": "Google", "blurb": "Set aside.", "features": []any{}, "steps": []any{map[string]any{"text": "Make a client."}}}}))
		replaced <- struct{}{}
		jsonH(map[string]string{"replaced": "google"})(w, r)
	})
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/accounts")))
	waitFor(t, ctx, `!!document.querySelector('.notices a')`, "the notice")
	if href := eval[string](t, ctx, `document.querySelector('.notices a').href`); href != "https://console.cloud.google.com/apis/library/drive.googleapis.com" {
		t.Fatalf("notice links %q", href)
	}
	run(t, ctx, chromedp.Evaluate(`window.confirm = () => true; true`, nil), chromedp.Click(".replace", chromedp.ByQuery))
	select {
	case <-replaced:
	case <-time.After(5 * time.Second):
		t.Fatal("Use a different client sent nothing")
	}
	waitFor(t, ctx, `!!document.querySelector('[name=client_id]')`, "the page asks for a new client")
}
