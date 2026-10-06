package browser

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/tools"
)

// The approval names the element and the site, from what checkAct saw.
func TestActApprovalNamesTheElement(t *testing.T) {
	s := &Session{}
	in := json.RawMessage(`{"steps":"[{\"type\":\"type\",\"ref\":3,\"text\":\"4\"},{\"type\":\"click\",\"ref\":12}]"}`)
	call := tools.Call{ChatKey: "chat", Input: in}
	s.remember(askKey("chat", in), askedPage{url: "https://shop.example/cart#pay", at: time.Now(), targets: []askedTarget{
		{sel: `[data-oh-ref="3"]`, ref: 3, label: "Quantity", print: "x"},
		{sel: `[data-oh-ref="12"]`, ref: 12, label: "Pay now $49", print: "y"},
	}})
	got := checked{summary: s.actSummary}.ApprovalSummary(call)
	if want := `Type "4" into "Quantity", then click "Pay now $49" on shop.example`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Nothing was seen (no page yet): still readable, still the whole call.
	got = s.actSummary(tools.Call{ChatKey: "other", Input: in})
	if want := `Type "4" into element 3, then click element 12`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	got = s.actSummary(tools.Call{ChatKey: "chat", Input: json.RawMessage(`{"url":"https://news.example/a","steps":"[{\"type\":\"click\",\"selector\":\"#more\"}]"}`)})
	if want := `Open https://news.example/a, then click "#more" on news.example`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// On a real page, the approval text the owner sees after the check names
// the button and the host.
func TestActApprovalOnAFakePage(t *testing.T) {
	needChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<html><body><button id="pay">Pay now $49</button></body></html>`)
	}))
	defer srv.Close()
	s := newTestSession(t, "127.0.0.1")
	reg := tools.NewRegistry()
	reg.Register(s.Tools()...)
	if _, err := run(t, reg, "browser_inspect", map[string]string{"url": srv.URL}); err != nil {
		t.Fatal(err)
	}
	tool, _ := reg.Get("browser_act")
	act := tool.(interface {
		tools.Checker
		tools.Summarizer
	})
	call := tools.Call{ChatKey: "whatsapp:owner", Input: json.RawMessage(`{"steps":"[{\"type\":\"click\",\"ref\":1}]"}`)}
	if err := act.Check(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	got := act.ApprovalSummary(call)
	if !strings.Contains(got, `Click "Pay now $49"`) || !strings.Contains(got, "on 127.0.0.1") {
		t.Fatalf("approval text %q should name the button and the host", got)
	}
}
