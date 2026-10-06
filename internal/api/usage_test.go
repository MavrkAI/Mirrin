package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// GET /usage is the spend the screen shows: every field is there, even with
// nothing spent, and it is labelled an estimate.
func TestUsageHasEveryField(t *testing.T) {
	e := newEnv(t)
	w := e.do(onLoopback, req{path: "/usage", header: bearer(master)})
	if w.Code != 200 {
		t.Fatalf("GET /usage: %d %s", w.Code, w.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"day", "month", "today", "month_to_date", "budget", "used", "today_calls", "month_calls", "today_tokens", "month_tokens", "unpriced", "currency", "estimate"} {
		if _, ok := got[k]; !ok {
			t.Errorf("no %q in %s", k, w.Body)
		}
	}
	if got["estimate"] != true || got["unpriced"] == nil {
		t.Fatalf("usage %s", w.Body)
	}
}

type brokenUsage struct{}

func (brokenUsage) Spend(context.Context) (memory.Spend, error) {
	return memory.Spend{}, errors.New("database is locked")
}

// When spending can't be added up, the answer is a refusal in words, in the
// {error, message, fix} shape pages read, not a bare line of text.
func TestUsageFailureIsInWords(t *testing.T) {
	e := newEnv(t)
	e.s.WithUsage(brokenUsage{})
	w := e.do(onLoopback, req{path: "/usage", header: bearer(master)})
	var got struct{ Error, Message, Fix string }
	if w.Code != 500 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Error != "usage_failed" || !strings.Contains(got.Message, "database is locked") || got.Fix == "" {
		t.Fatalf("GET /usage failing: %d %s", w.Code, w.Body)
	}
}

// The screen says what the device asking may do, so a wall screen paired
// to look shows no text box (every send would be refused) and a device that
// may not decide shows no Approve/Deny.
func TestScreenSaysWhatThisDeviceMayDo(t *testing.T) {
	e := newEnv(t)
	_, kiosk, _ := e.store.Add("Wall", devices.KindKiosk, nil, "", "")
	_, phone, _ := e.store.Add("Phone", devices.KindPWA, []devices.Scope{devices.View, devices.Chat}, "", "")
	for _, c := range []struct {
		name string
		at   where
		tok  string
		want string
	}{
		{"a wall screen", onRemote, kiosk, `["view"]`},
		{"a phone paired to talk", onRemote, phone, `["view","chat"]`},
		{"this computer", onLoopback, master, `["view","chat","approve","admin"]`},
	} {
		w := e.do(c.at, req{path: "/screen", header: bearer(c.tok)})
		var got struct{ Can json.RawMessage }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || string(got.Can) != c.want {
			t.Errorf("%s: %d can=%s, want %s", c.name, w.Code, got.Can, c.want)
		}
	}
}

// The screen sends its own id with a message ("client"): a paired phone's
// cookie, from the twin's own page, is answered (the field isn't refused),
// and the id reaches the daemon, which marks that turn's lines with it for
// every other screen. An id that isn't a page's plain one is dropped.
func TestTheScreensOwnIDReachesTheTwin(t *testing.T) {
	e := newEnv(t)
	_, tok, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	ck := &http.Cookie{Name: cookieSecure, Value: tok} // a phone's cookie on the remote (TLS) listener
	hdr := map[string]string{"Origin": "https://twin.example.ts.net", "Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"}
	for _, c := range []struct{ client, want string }{{"abc123", "abc123"}, {"<img src=x>", ""}} {
		body, _ := json.Marshal(map[string]string{"channel": "screen", "chat_id": "local", "text": "hi", "client": c.client})
		w := e.do(onRemote, req{method: "POST", path: "/message/stream", body: string(body), header: hdr, cookies: []*http.Cookie{ck}})
		if w.Code != 200 || !strings.Contains(w.Body.String(), "event: done") {
			t.Fatalf("%q: %d %s", c.client, w.Code, w.Body)
		}
		e.f.mu.Lock()
		got := e.f.clients[len(e.f.clients)-1]
		e.f.mu.Unlock()
		if got != c.want {
			t.Errorf("client %q reached the twin as %q", c.client, got)
		}
	}
}

// A browser on this computer that lost its key is told the way back that
// fits the page: a settings page or Health is opened again from its own
// menu item (a screen paired with --screen couldn't open it anyway), the
// presence screen from "Open screen…".
func TestNotPairedNamesTheWayBackToThePage(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ path, want, not string }{
		{"/channels/list", "Channels…", "Open screen"},
		{"/health", "Health", "Open screen"},
		{"/screen", "Open screen", "Channels…"},
	} {
		w := e.do(onLoopback, req{path: c.path, cookies: []*http.Cookie{{Name: cookieDev, Value: "abt1_0000000000000000_nope-nope-nope-nope-nope"}}})
		var got struct{ Error, Fix string }
		if w.Code != 401 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Error != "not_paired" || !strings.Contains(got.Fix, c.want) || strings.Contains(got.Fix, c.not) {
			t.Errorf("%s: %d %s", c.path, w.Code, w.Body)
		}
	}
}
