package google

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
)

func TestSignInFlowsAreSeparateSingleUseAndBound(t *testing.T) {
	var mu sync.Mutex
	challenges := map[string]string{} // code → PKCE challenge from the auth URL
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		mu.Lock()
		want := challenges[r.Form.Get("code")]
		mu.Unlock()
		if want == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != want {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","token_type":"Bearer","refresh_token":"rt","expires_in":3600}`))
	}))
	defer tokenSrv.Close()

	dir := t.TempDir()
	a := NewAuth(config.Calendar{CredentialsFile: filepath.Join(dir, "c.json"), TokenFile: filepath.Join(dir, "t.json")})
	creds := `{"installed":{"client_id":"id.apps.googleusercontent.com","client_secret":"s","auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"` + tokenSrv.URL + `","redirect_uris":["http://localhost"]}}`
	if err := a.ImportCredentials([]byte(creds)); err != nil {
		t.Fatal(err)
	}
	begin := func(code string) string {
		u, err := a.BeginURL("http://127.0.0.1:7742/oauth/google")
		if err != nil {
			t.Fatal(err)
		}
		q, _ := url.Parse(u)
		if q.Query().Get("code_challenge_method") != "S256" || len(q.Query().Get("state")) < 64 {
			t.Fatalf("auth url lacks PKCE or a strong state: %s", u)
		}
		mu.Lock()
		challenges[code] = q.Query().Get("code_challenge")
		mu.Unlock()
		return q.Query().Get("state")
	}
	ctx := context.Background()
	first, second := begin("code-1"), begin("code-2")

	// A stray or forged callback doesn't cancel the real sign-ins.
	if err := a.Finish(ctx, "forged", "code-1"); err == nil {
		t.Fatal("forged state accepted")
	}
	// The code must be redeemed by the flow that asked for it (PKCE).
	if err := a.Finish(ctx, second, "code-1"); err == nil {
		t.Fatal("a code from another flow was accepted")
	}
	if err := a.Finish(ctx, first, "code-1"); err != nil {
		t.Fatalf("first sign-in: %v", err)
	}
	if _, err := os.Stat(a.TokenFile); err != nil {
		t.Fatal("token not stored")
	}
	if err := a.Finish(ctx, first, "code-1"); err == nil {
		t.Fatal("a state worked twice")
	}

	// An old link has expired.
	stale := begin("code-3")
	a.mu.Lock()
	f := a.flows[stale]
	f.expires = time.Now().Add(-time.Second)
	a.flows[stale] = f
	a.mu.Unlock()
	if err := a.Finish(ctx, stale, "code-3"); err == nil {
		t.Fatal("expired sign-in accepted")
	}
}
