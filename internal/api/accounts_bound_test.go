package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

const testGoogleBinding = "browser-secret-independent-of-state"

func (f *fake) BeginGoogleBound(ctx context.Context, redirect string) (string, string, error) {
	u, err := f.BeginGoogle(ctx, redirect)
	return u, testGoogleBinding, err
}

func (f *fake) FinishGoogleBound(ctx context.Context, state, code, binding string) error {
	if binding != testGoogleBinding || state != "st4te" {
		return errors.New("different browser")
	}
	return f.FinishGoogle(ctx, state, code)
}

func TestGoogleStateHashCannotReplaceBrowserSecret(t *testing.T) {
	e := newEnv(t)
	w := e.do(onLoopback, req{method: "POST", path: "/accounts/google/connect", header: bearer(master)})
	flow := cookieFrom(w, oauthCookie)
	if flow == nil || flow.Value != testGoogleBinding || flow.SameSite != http.SameSiteLaxMode || flow.MaxAge != 600 {
		t.Fatalf("flow: %+v", flow)
	}
	for _, binding := range []string{"st4te", devices.HashToken("st4te"), "another-browser"} {
		w = e.do(onLoopback, req{path: "/oauth/google?state=st4te&code=abc", cookies: []*http.Cookie{{Name: oauthCookie, Value: binding}}})
		if w.Code != 400 || e.f.finished {
			t.Fatalf("forged cookie accepted: %d", w.Code)
		}
	}
	w = e.do(onLoopback, req{path: "/oauth/google?state=st4te&code=abc", cookies: []*http.Cookie{flow}})
	if w.Code != http.StatusFound || !e.f.finished {
		t.Fatalf("bound callback: %d %s", w.Code, w.Body)
	}
	if cleared := cookieFrom(w, oauthCookie); cleared == nil || cleared.MaxAge != -1 {
		t.Fatal("flow cookie not cleared")
	}
}

type callbackErrorBackend struct{ *fake }

func (b callbackErrorBackend) FinishGoogleBound(context.Context, string, string, string) error {
	return errors.New(`<script>alert("callback")</script>`)
}

func TestGoogleCallbackEscapesBackendErrors(t *testing.T) {
	e := newEnv(t)
	e.s.WithAccounts(callbackErrorBackend{e.f})
	w := e.do(onLoopback, req{path: "/oauth/google?state=st4te&code=abc", header: map[string]string{"Accept": "text/html"}, cookies: []*http.Cookie{{Name: oauthCookie, Value: testGoogleBinding}}})
	if w.Code != 400 || strings.Contains(w.Body.String(), "<script>") || !strings.Contains(w.Body.String(), "&lt;script&gt;") {
		t.Fatalf("callback: %d %s", w.Code, w.Body)
	}
}
