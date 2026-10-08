package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// jevAccounts is the fake accounts backend with the quick judgments card:
// a key that isn't "ts-good…" is refused, as TypeSafe would.
type jevAccounts struct {
	*fake
	mu  sync.Mutex
	key string
	on  bool
}

func (j *jevAccounts) JevState(context.Context) JevState {
	j.mu.Lock()
	defer j.mu.Unlock()
	return JevState{On: j.on && j.key != "", HasKey: j.key != "", Masked: MaskKey(j.key), Env: "TYPESAFE_API_KEY"}
}

func (j *jevAccounts) CheckJev(_ context.Context, key string) error {
	if !strings.HasPrefix(key, "ts-good") {
		return &HumanError{Sentence: "That key wasn't accepted.", Fix: "Check it at typesafe.ai and paste it again."}
	}
	return nil
}

func (j *jevAccounts) SaveJev(ctx context.Context, key string, on, forget bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if forget {
		j.key, j.on = "", false
		return nil
	}
	if key != "" {
		if err := j.CheckJev(ctx, key); err != nil {
			return err
		}
		j.key = key
	}
	if on && j.key == "" {
		return &HumanError{Sentence: "Add your TypeSafe key first.", Fix: "Paste it above, then press Check and save."}
	}
	j.on = on
	return nil
}

// The card checks a key without saving it, saves only one that works, and
// never answers with the key unmasked.
func TestJevCardChecksAndSaves(t *testing.T) {
	e := newEnv(t)
	j := &jevAccounts{fake: e.f}
	e.s.WithAccounts(j)
	const good, bad = "ts-good-0123456789abcdef", "ts-nope-0123456789abcdef"
	post := func(path, body string) (int, string) {
		w := e.do(onLoopback, req{method: "POST", path: path, body: body, header: bearer(master)})
		return w.Code, w.Body.String()
	}
	if code, body := post("/accounts/jev/check", `{"key":"`+good+`"}`); code != 200 || !strings.Contains(body, `"ok":true`) || strings.Contains(body, good) {
		t.Fatalf("check a good key: %d %s", code, body)
	}
	var ae apiError
	code, body := post("/accounts/jev/check", `{"key":"`+bad+`"}`)
	if code != 400 || json.Unmarshal([]byte(body), &ae) != nil || ae.Error != "not_accepted" || ae.Message != "That key wasn't accepted." || !strings.Contains(ae.Fix, "typesafe.ai") || strings.Contains(body, bad) {
		t.Fatalf("check a bad key: %d %s", code, body)
	}
	if j.key != "" {
		t.Fatal("Check saved the key")
	}
	if code, body := post("/accounts/jev", `{"on":true}`); code != 400 || !strings.Contains(body, "key first") {
		t.Fatalf("on without a key: %d %s", code, body)
	}
	if code, body := post("/accounts/jev", `{"key":"`+bad+`","on":true}`); code != 400 || !strings.Contains(body, "wasn't accepted") || strings.Contains(body, bad) || j.key != "" {
		t.Fatalf("save a bad key: %d %s", code, body)
	}
	code, body = post("/accounts/jev", `{"key":"`+good+`","on":true}`)
	var st JevState
	if code != 200 || json.Unmarshal([]byte(body), &st) != nil || !st.On || !st.HasKey || st.Masked != "ts-g••••cdef" || strings.Contains(body, good) {
		t.Fatalf("save a good key: %d %s", code, body)
	}
	w := e.do(onLoopback, req{path: "/accounts/jev", header: bearer(master)})
	if w.Code != 200 || strings.Contains(w.Body.String(), good) || !strings.Contains(w.Body.String(), `"masked":"ts-g••••cdef"`) {
		t.Fatalf("state: %d %s", w.Code, w.Body)
	}
	if code, body := post("/accounts/jev", `{"forget":true}`); code != 200 || !strings.Contains(body, `"has_key":false`) || j.key != "" {
		t.Fatalf("forget: %d %s", code, body)
	}
	if code, _ := post("/accounts/jev", `not json`); code != 400 {
		t.Fatalf("a body that isn't JSON: %d", code)
	}
}

// Settings: admin on this computer only.
func TestJevCardIsAdminOnly(t *testing.T) {
	e := newEnv(t)
	e.s.WithAccounts(&jevAccounts{fake: e.f})
	_, chat, err := e.store.Add("phone", devices.KindPWA, []devices.Scope{devices.Chat, devices.Approve}, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	_, admin, err := e.store.Add("laptop", devices.KindPWA, []devices.Scope{devices.Admin}, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, rt := range []struct{ method, path, body string }{
		{"GET", "/accounts/jev", ""},
		{"POST", "/accounts/jev", `{"key":"ts-good-0123456789abcdef","on":true}`},
		{"POST", "/accounts/jev/check", `{"key":"ts-good-0123456789abcdef"}`},
	} {
		for _, tc := range []struct {
			at   where
			tok  string
			want int
		}{
			{onLoopback, "", http.StatusUnauthorized},
			{onLoopback, chat, http.StatusForbidden},
			{onLoopback, admin, http.StatusOK},
			{onLoopback, master, http.StatusOK},
			{onRemote, admin, http.StatusNotFound},
		} {
			q := req{method: rt.method, path: rt.path, body: rt.body}
			if tc.tok != "" {
				q.header = bearer(tc.tok)
			}
			if w := e.do(tc.at, q); w.Code != tc.want {
				t.Errorf("%s %s on %v: %d, want %d", rt.method, rt.path, tc.at, w.Code, tc.want)
			}
		}
	}
}

// A twin without the card answers 404, which hides it.
func TestJevCardHiddenWithoutABackend(t *testing.T) {
	e := newEnv(t)
	for _, q := range []req{{path: "/accounts/jev"}, {method: "POST", path: "/accounts/jev", body: `{}`}, {method: "POST", path: "/accounts/jev/check", body: `{}`}} {
		q.header = bearer(master)
		if w := e.do(onLoopback, q); w.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d", q.method, q.path, w.Code)
		}
	}
	if !strings.Contains(string(accountsHTML), "e.status===404){$('#jev').hidden=true") {
		t.Fatal("the page doesn't hide the card on a 404")
	}
}
