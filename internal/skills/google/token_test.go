package google

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/skills/google/googletest"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// connected is an Auth signed in to a fake Google whose access token has
// already expired, so the first call must renew it.
func connected(t *testing.T) (*Auth, *googletest.Fake) {
	t.Helper()
	fake := googletest.New(t)
	dir := t.TempDir()
	a := NewAuth(config.Calendar{CredentialsFile: filepath.Join(dir, "c.json"), TokenFile: filepath.Join(dir, "t.json")})
	a.Transport = fake.Transport()
	fake.WriteFiles(t, a.CredentialsFile, a.TokenFile, time.Now().Add(-time.Hour))
	return a, fake
}

func runTool(t *testing.T, ts []tools.Tool, name string, input any) (string, error) {
	t.Helper()
	for _, tl := range ts {
		if tl.Spec().Name == name {
			raw, _ := json.Marshal(input)
			return tl.Run(context.Background(), tools.Call{Input: raw})
		}
	}
	t.Fatalf("no tool %s", name)
	return "", nil
}

func TestNewAuthIsSharedPerTokenFile(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Calendar{CredentialsFile: filepath.Join(dir, "c.json"), TokenFile: filepath.Join(dir, "t.json")}
	a1, a2 := NewAuth(cfg), NewAuth(cfg)
	if a1 != a2 {
		t.Fatal("calendar, Gmail and the Accounts page must share one sign-in")
	}
	other := config.Calendar{CredentialsFile: filepath.Join(dir, "c.json"), TokenFile: filepath.Join(dir, "other.json")}
	if NewAuth(cfg) == NewAuth(other) {
		t.Fatal("different token files shared an Auth")
	}
}

// The token is renewed once and saved, however many calls use it: before,
// every calendar call renewed it again, because the renewed token was never
// kept.
func TestTokenIsRenewedOnceAndSaved(t *testing.T) {
	a, fake := connected(t)
	ctx := context.Background()
	for range 5 {
		if err := a.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runTool(t, a.GmailTools(), "gmail_search", map[string]any{"query": "is:unread"}); err != nil {
		t.Fatal(err)
	}
	if got := fake.Refreshes(); got != 1 {
		t.Fatalf("renewed %d times, want once", got)
	}
	var tok oauth2.Token
	b, _ := os.ReadFile(a.TokenFile)
	_ = json.Unmarshal(b, &tok)
	if !tok.Expiry.After(time.Now()) || tok.RefreshToken == "" {
		t.Fatalf("renewed token not saved: %+v", tok)
	}
	// The next run starts from the saved token and doesn't renew again.
	again := &Auth{CredentialsFile: a.CredentialsFile, TokenFile: a.TokenFile, Transport: fake.Transport()}
	if err := again.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := fake.Refreshes(); got != 1 {
		t.Fatalf("a restart renewed again (%d)", got)
	}
}

// Google ending the sign-in (7 days after connecting while the project is
// in Testing) is noticed once, explained in words the owner can act on, and
// stops costing a call to Google each time.
func TestSignOutIsNoticedOnceAndExplained(t *testing.T) {
	a, fake := connected(t)
	fake.SignOut()
	var mu sync.Mutex
	var told []SignOut
	a.OnSignedOut(func(so SignOut) { mu.Lock(); told = append(told, so); mu.Unlock() })

	for range 3 {
		_, err := runTool(t, a.GmailTools(), "gmail_search", map[string]any{"query": "x"})
		if err == nil || !strings.Contains(err.Error(), "signed Mirrin out") || !strings.Contains(err.Error(), "Connect Google") || !strings.Contains(err.Error(), "Publish app") {
			t.Fatalf("not explained: %v", err)
		}
	}
	err := a.Check(context.Background())
	so, out := IsSignedOut(err)
	if !out || so.Client || !strings.Contains(so.Detail, "expired or revoked") {
		t.Fatalf("check: %v", err)
	}
	if _, ok := a.SignedOut(); !ok {
		t.Fatal("state not kept")
	}
	if got := fake.Refreshes(); got != 1 {
		t.Fatalf("asked Google %d times after it said no", got)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(told) == 1 })
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	if len(told) != 1 {
		t.Fatalf("told %d times", len(told))
	}
	mu.Unlock()

	// Connecting again clears it.
	fake.WriteFiles(t, a.CredentialsFile, a.TokenFile, time.Now().Add(-time.Hour))
	if err := a.Check(context.Background()); err != nil {
		t.Fatalf("after reconnecting: %v", err)
	}
	if _, ok := a.SignedOut(); ok {
		t.Fatal("still signed out after reconnecting")
	}
}

func TestDeletedClientSaysANewClientIsNeeded(t *testing.T) {
	a, fake := connected(t)
	fake.DeleteClient()
	err := a.Check(context.Background())
	so, out := IsSignedOut(err)
	if !out || !so.Client || !strings.Contains(err.Error(), "new Desktop app client") || !strings.Contains(err.Error(), "Connect Google") {
		t.Fatalf("got %v", err)
	}
	// The page shows no Disconnect button in this state: don't send the
	// owner looking for one.
	if strings.Contains(err.Error(), "Disconnect") {
		t.Fatalf("points at a button that isn't there: %v", err)
	}
}

// unauthorized_client on a renewal means the sign-in was made with another
// client: a new sign-in fixes it, and the client that works is kept.
func TestSignInFromAnotherClientOnlyNeedsANewSignIn(t *testing.T) {
	a, fake := connected(t)
	fake.SignOutWith("unauthorized_client")
	err := a.Check(context.Background())
	so, out := IsSignedOut(err)
	if !out || so.Client || !strings.Contains(err.Error(), "Connect Google") {
		t.Fatalf("got %v", err)
	}
	if !a.HasCredentials() {
		t.Fatal("a working client was set aside")
	}
}

// A Web application client that can't take this return address is set
// aside only when nothing is connected with it: a working sign-in made
// with it keeps working (renewing needs no return address).
func TestWebClientIsKeptWhileItsSignInWorks(t *testing.T) {
	a, _ := connected(t)
	if err := a.ImportCredentials([]byte(`{"web":{"client_id":"9-z.apps.googleusercontent.com","client_secret":"s","redirect_uris":["https://example.com/cb"]}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.BeginURL("http://127.0.0.1:7742/oauth/google"); err == nil || !strings.Contains(err.Error(), "Desktop app") {
		t.Fatalf("web client: %v", err)
	}
	if !a.HasCredentials() {
		t.Fatal("the client of a working sign-in was set aside")
	}
	if _, err := os.Stat(a.CredentialsFile + ".rejected"); err == nil {
		t.Fatal("set aside while connected")
	}
}

// A renewal that was under way when the owner pressed Disconnect, or
// connected again, must not write the old sign-in back.
func TestLateRenewalDoesNotUndoDisconnect(t *testing.T) {
	a, fake := connected(t)
	started, release := fake.HoldRenewals()
	done := make(chan error, 1)
	go func() { done <- a.Check(context.Background()) }()
	<-started
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	release()
	<-done
	if a.Connected() {
		t.Fatal("a renewal that finished after Disconnect brought the sign-in back")
	}
}

func TestLateRenewalDoesNotUndoANewSignIn(t *testing.T) {
	a, fake := connected(t)
	started, release := fake.HoldRenewals()
	done := make(chan error, 1)
	go func() { done <- a.Check(context.Background()) }()
	<-started
	u, err := a.BeginURL("http://127.0.0.1:7742/oauth/google")
	if err != nil {
		t.Fatal(err)
	}
	q, _ := url.Parse(u)
	if err := a.Finish(context.Background(), q.Query().Get("state"), "new-code"); err != nil {
		t.Fatal(err)
	}
	fresh, _ := os.ReadFile(a.TokenFile)
	release()
	<-done
	if now, _ := os.ReadFile(a.TokenFile); string(now) != string(fresh) {
		t.Fatalf("the old sign-in was written over the new one:\n%s\nwas\n%s", now, fresh)
	}
}

func TestAPIProblemsAreExplainedAndRemembered(t *testing.T) {
	a, fake := connected(t)
	fake.Disable("gmail")
	_, err := runTool(t, a.GmailTools(), "gmail_search", map[string]any{"query": "x"})
	if err == nil || !strings.Contains(err.Error(), "Gmail API is turned off") || !strings.Contains(err.Error(), "https://console.developers.google.com/apis/api/gmail.googleapis.com/overview?project=123") {
		t.Fatalf("got %v", err)
	}
	if p := a.Problems()["gmail"]; !strings.Contains(p, "turned off") {
		t.Fatalf("problem not kept: %q", p)
	}
	fake.Untick("drive")
	found, err := a.Probe(context.Background(), "calendar", "gmail", "drive")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(found["gmail"], "turned off") || !strings.Contains(found["drive"], "wasn't ticked") || found["calendar"] != "" {
		t.Fatalf("probe: %v", found)
	}
	fake.Enable("gmail")
	found, _ = a.Probe(context.Background(), "gmail")
	if len(found) != 0 || a.Problems()["gmail"] != "" {
		t.Fatalf("fixed API still reported: %v / %v", found, a.Problems())
	}
}

// Google lets people untick boxes; the twin notices which.
func TestUntickedBoxesAreNoticedAtSignIn(t *testing.T) {
	a, fake := connected(t)
	fake.Untick("gmail")
	u, binding, err := a.Begin("http://127.0.0.1:7742/oauth/google")
	if err != nil {
		t.Fatal(err)
	}
	q, _ := url.Parse(u)
	if err := a.FinishBound(context.Background(), q.Query().Get("state"), "code", binding); err != nil {
		t.Fatal(err)
	}
	g := a.Granted()
	if g == nil || g["gmail"] || !g["calendar"] || !g["drive"] {
		t.Fatalf("granted: %v", g)
	}
	if p := a.Problems()["gmail"]; !strings.Contains(p, "wasn't ticked") {
		t.Fatalf("problem: %q", p)
	}
}

// A sign-in started in one browser can't be finished from another: a
// callback URL that leaks is useless on its own.
func TestBoundSignInOnlyFinishesInTheBrowserThatStartedIt(t *testing.T) {
	a, _ := connected(t)
	u, binding, err := a.Begin("http://127.0.0.1:7742/oauth/google")
	if err != nil || len(binding) < 32 {
		t.Fatalf("begin: %v %q", err, binding)
	}
	state := mustState(t, u)
	ctx := context.Background()
	if err := a.FinishBound(ctx, state, "code", "stolen-url-no-cookie"); err == nil || !strings.Contains(err.Error(), "different browser") {
		t.Fatalf("wrong browser accepted: %v", err)
	}
	if err := a.Finish(ctx, state, "code"); err == nil {
		t.Fatal("unbound finish accepted for a bound sign-in")
	}
	// The owner's own browser still finishes: the attempts above didn't use it up.
	if err := a.FinishBound(ctx, state, "code", binding); err != nil {
		t.Fatalf("right browser refused: %v", err)
	}
	if err := a.FinishBound(ctx, state, "code", binding); err == nil {
		t.Fatal("a sign-in worked twice")
	}
}

func mustState(t *testing.T, u string) string {
	t.Helper()
	q, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	return q.Query().Get("state")
}

func TestPastedClientMistakesAreCaught(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{CredentialsFile: filepath.Join(dir, "c.json"), TokenFile: filepath.Join(dir, "t.json")}
	// Swapped fields, with quotes copied along: fixed quietly.
	if err := a.WriteClient(` "GOCSPX-abc" `, `"123-x.apps.googleusercontent.com"`); err != nil {
		t.Fatalf("swapped: %v", err)
	}
	b, _ := os.ReadFile(a.CredentialsFile)
	if !strings.Contains(string(b), `"client_id":"123-x.apps.googleusercontent.com"`) || !strings.Contains(string(b), `"client_secret":"GOCSPX-abc"`) {
		t.Fatalf("saved %s", b)
	}
	for _, c := range []struct{ id, secret, want string }{
		{"AIzaSyD-apikey", "s", "API key"},
		{"my-project", "s", "ends in .apps.googleusercontent.com"},
		{"123-x.apps.googleusercontent.com", "", "both"},
	} {
		if err := a.WriteClient(c.id, c.secret); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q/%q: %v", c.id, c.secret, err)
		}
	}
	// The whole JSON pasted into the ID box works too.
	os.Remove(a.CredentialsFile)
	if err := a.WriteClient(`{"installed":{"client_id":"9-z.apps.googleusercontent.com","client_secret":"GOCSPX-z"}}`, ""); err != nil || !a.HasCredentials() {
		t.Fatalf("json in id box: %v", err)
	}
	if err := a.ImportCredentials([]byte(`{"type":"service_account","project_id":"p","private_key":"k"}`)); err == nil || !strings.Contains(err.Error(), "service account") {
		t.Fatalf("service account: %v", err)
	}
	if err := a.ImportCredentials([]byte(`{"installed":{"client_id":"9-z.apps.googleusercontent.com"}}`)); err == nil || !strings.Contains(err.Error(), "no client secret") {
		t.Fatalf("no secret: %v", err)
	}

	// A Web client that doesn't list our address is set aside with the fix.
	if err := a.ImportCredentials([]byte(`{"web":{"client_id":"9-z.apps.googleusercontent.com","client_secret":"s","redirect_uris":["https://example.com/cb"]}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.BeginURL("http://127.0.0.1:7742/oauth/google"); err == nil || !strings.Contains(err.Error(), "Desktop app") || !strings.Contains(err.Error(), "http://127.0.0.1:7742/oauth/google") {
		t.Fatalf("web client: %v", err)
	}
	if a.HasCredentials() {
		t.Fatal("unusable client kept, so the page can't ask for a new one")
	}
	// One that lists it is fine.
	if err := a.ImportCredentials([]byte(`{"web":{"client_id":"9-z.apps.googleusercontent.com","client_secret":"s","redirect_uris":["http://127.0.0.1:7742/oauth/google"]}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.BeginURL("http://127.0.0.1:7742/oauth/google"); err != nil {
		t.Fatal(err)
	}
	// Google won't send a sign-in to another machine's address.
	_ = a.WriteClient("123-x.apps.googleusercontent.com", "GOCSPX-abc")
	if _, err := a.BeginURL("http://100.64.1.2:7742/oauth/google"); err == nil || !strings.Contains(err.Error(), "computer running Mirrin") {
		t.Fatalf("remote redirect: %v", err)
	}
}

func TestCallbackErrorsInPlainWords(t *testing.T) {
	if s := ExplainCallback("access_denied"); !strings.Contains(s, "Test users") || !strings.Contains(s, "Publish app") {
		t.Fatal(s)
	}
	if s := ExplainCallback("something_new"); !strings.Contains(s, "something_new") || !strings.Contains(s, "Connect Google") {
		t.Fatal(s)
	}
	// The callback address is anyone's to write: only a plain error code is
	// ever repeated back.
	for _, evil := range []string{"<script>alert(1)</script>", `x" onload="y`, "access_denied\nSet-Cookie: a=b", strings.Repeat("a", 65)} {
		if s := ExplainCallback(evil); strings.ContainsAny(s, "<>\"\n") || strings.Contains(s, "script") || strings.Contains(s, "aaaa") || !strings.Contains(s, "didn't name") {
			t.Errorf("%q echoed: %s", evil, s)
		}
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
