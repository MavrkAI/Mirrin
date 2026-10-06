package pagetest

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/chromedp/chromedp"
)

// The Spending page shows the month against its limit and the payments, and
// saves new limits.
func TestSpendingPageShowsAndSavesLimits(t *testing.T) {
	d := newDaemon(t)
	var mu sync.Mutex
	info := map[string]any{"currency": "AUD", "per_action_limit": 100.0, "monthly_limit": 500.0, "month_total": 420.0,
		"payments": []any{map[string]any{"at": "2026-09-30T10:00:00Z", "amount": 420.0, "currency": "AUD", "merchant": "Jetstar", "purpose": "MEL-DPS 2 Oct"}}}
	var saved map[string]float64
	d.handle("GET /spending/info", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		jsonH(info)(w, r)
	})
	d.handle("POST /spending/limits", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewDecoder(r.Body).Decode(&saved)
		info["per_action_limit"], info["monthly_limit"] = saved["per_action_limit"], saved["monthly_limit"]
		jsonH(info)(w, r)
	})
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1200, 900), chromedp.Navigate(d.url("/spending")))
	waitFor(t, ctx, `document.querySelector('#per').value === '100' && document.querySelector('#pays').textContent.includes('Jetstar')`, "the limits and payments")
	if got := textOf(t, ctx, "#total"); !strings.Contains(got, "420") || !strings.Contains(got, "500") {
		t.Fatalf("total %q", got)
	}
	if !eval[bool](t, ctx, `document.querySelector('#meter').classList.contains('near')`) {
		t.Fatal("84% of the month didn't show as near the limit")
	}
	run(t, ctx, chromedp.SetValue("#per", "1500", chromedp.ByQuery), chromedp.SetValue("#month", "2000", chromedp.ByQuery), chromedp.Click("#save", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#saveMsg').textContent.startsWith('Saved')`, "saved")
	mu.Lock()
	defer mu.Unlock()
	if saved["per_action_limit"] != 1500 || saved["monthly_limit"] != 2000 {
		t.Fatalf("saved %v", saved)
	}
}
