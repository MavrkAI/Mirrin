package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// keyCheckingWelcome is a WelcomeBackend that checks keys like fake.SaveModelKey.
type keyCheckingWelcome struct{}

func (keyCheckingWelcome) DetectBrains(context.Context) []Brain                   { return nil }
func (keyCheckingWelcome) UseBrain(context.Context, string, string, string) error { return nil }
func (keyCheckingWelcome) SetNames(context.Context, string, string, string, string) error {
	return nil
}
func (keyCheckingWelcome) Hello(context.Context, func(string), func(string)) (HelloReply, error) {
	return HelloReply{}, nil
}
func (keyCheckingWelcome) RestoreSources(context.Context) []Source { return nil }
func (keyCheckingWelcome) CheckBrain(_ context.Context, _, key, _ string) error {
	return fakeCheckKey(key)
}

// The model key card: Test says whether a key works without saving it, Save
// makes it the model, and no answer ever carries the key unmasked.
func TestModelKeyCardTestsAndSaves(t *testing.T) {
	e := newEnv(t)
	e.s.WithWelcome(keyCheckingWelcome{})
	const good, bad = "sk-good-0123456789abcdef", "sk-nope-0123456789abcdef"
	post := func(path, body string) (int, string) {
		w := e.do(onLoopback, req{method: "POST", path: path, body: body, header: bearer(master)})
		return w.Code, w.Body.String()
	}
	if code, body := post("/welcome/check", `{"provider":"openai","key":"`+good+`"}`); code != 200 || !strings.Contains(body, `"ok":true`) || strings.Contains(body, good) {
		t.Fatalf("test a good key: %d %s", code, body)
	}
	code, body := post("/welcome/check", `{"provider":"openai","key":"`+bad+`"}`)
	var he apiError
	if code != 400 || json.Unmarshal([]byte(body), &he) != nil || he.Error != "not_accepted" || he.Message != "That key wasn't accepted." || he.Fix == "" || strings.Contains(body, bad) {
		t.Fatalf("test a bad key: %d %s", code, body)
	}
	// The Accounts card tests on its own mount, beside Save.
	if code, body := post("/accounts/model/check", `{"provider":"openai","key":"`+good+`"}`); code != 200 || !strings.Contains(body, `"ok":true`) || strings.Contains(body, good) {
		t.Fatalf("card test of a good key: %d %s", code, body)
	}
	if code, body := post("/accounts/model/check", `{"provider":"openai","key":"`+bad+`"}`); code != 400 || !strings.Contains(body, "wasn't accepted") || strings.Contains(body, bad) {
		t.Fatalf("card test of a bad key: %d %s", code, body)
	}
	if e.f.key != "" {
		t.Fatal("Test saved the key")
	}

	if code, body := post("/accounts/model", `{"provider":"openai","key":"`+bad+`"}`); code != 400 || strings.Contains(body, bad) || !strings.Contains(body, "wasn't accepted") {
		t.Fatalf("save a bad key: %d %s", code, body)
	}
	code, body = post("/accounts/model", `{"provider":"openai","key":"`+good+`"}`)
	var st ModelKeyState
	if code != 200 || json.Unmarshal([]byte(body), &st) != nil || st.Provider != "openai" || st.Key != "sk-g••••cdef" || strings.Contains(body, good) {
		t.Fatalf("save a good key: %d %s", code, body)
	}
	if e.f.key != good {
		t.Fatalf("saved key %q", e.f.key)
	}
	if w := e.do(onLoopback, req{path: "/accounts/model", header: bearer(master)}); w.Code != 200 || strings.Contains(w.Body.String(), good) || !strings.Contains(w.Body.String(), "sk-g••••cdef") {
		t.Fatalf("state: %d %s", w.Code, w.Body)
	}
}

func TestMaskKey(t *testing.T) {
	for in, want := range map[string]string{"": "", "short": "••••", "sk-ant-api03-abcdefgh": "sk-a••••efgh"} {
		if got := MaskKey(in); got != want {
			t.Errorf("MaskKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// "Use a different client" whose client was set aside but whose sign-out
// failed answers that it worked, with a note, not a 500 saying it didn't.
func TestReplaceGoogleClientWhoseSignOutFailedSaysItWasSetAside(t *testing.T) {
	e := newEnv(t)
	e.f.replaceErr = fmt.Errorf("%w: disk full", ErrGoogleSignOutPending)
	w := e.do(onLoopback, req{method: "POST", path: "/accounts/google/client/replace", header: bearer(master)})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"replaced":"google"`) || !strings.Contains(w.Body.String(), "set aside") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	e.f.replaceErr = errors.New("read-only file system")
	if w := e.do(onLoopback, req{method: "POST", path: "/accounts/google/client/replace", header: bearer(master)}); w.Code != 500 {
		t.Fatalf("a failed rename: %d %s", w.Code, w.Body)
	}
}
