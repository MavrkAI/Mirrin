package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/tools"
	"github.com/chromedp/chromedp"
)

// A field that keeps its own copy of its value, as React's do: keystrokes
// add to that copy, so clearing the element alone came back on the next key
// ("AkshayAkshayAkshay"). Typing must replace what's there, retries too,
// and named keys and shortcuts must never be typed out.
func TestTypingReplacesWhatAFieldHolds(t *testing.T) {
	needChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body><input id="first" value="Akshay">
<script>const el = document.getElementById('first'); let state = el.value;
el.addEventListener('input', e => { if (e.inputType === 'insertText') state += e.data; else state = el.value; el.value = state; });</script></body></html>`))
	}))
	defer srv.Close()
	s := newTestSession(t, "127.0.0.1")
	reg := tools.NewRegistry()
	reg.Register(s.Tools()...)
	act := func(steps string) {
		t.Helper()
		in, _ := json.Marshal(map[string]string{"steps": steps})
		if _, err := reg.Run(context.Background(), "browser_act", tools.Call{Input: in}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reg.Run(context.Background(), "browse_page", tools.Call{Input: json.RawMessage(`{"url":"` + srv.URL + `"}`)}); err != nil {
		t.Fatal(err)
	}
	value := func() string {
		tab, _ := s.tab(false)
		var v string
		if err := chromedp.Run(tab, chromedp.Value("#first", &v, chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
		return v
	}
	act(`[{"type":"type","selector":"#first","text":"Akshay"}]`)
	act(`[{"type":"type","selector":"#first","text":"Akshay"}]`) // a retry
	if v := value(); v != "Akshay" {
		t.Fatalf("after typing twice the field holds %q", v)
	}
	act(`[{"type":"press","key":"ArrowDown"},{"type":"press","key":"Meta+a"},{"type":"press","key":"Control+a"}]`)
	if v := value(); v != "Akshay" {
		t.Fatalf("keys were typed out: %q", v)
	}
}
