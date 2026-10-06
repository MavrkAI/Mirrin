// Package google is one Google account for the twin: Calendar, Gmail and
// Drive share a single OAuth token, connected from the Accounts page. The
// calendar skill keeps its own package; this one adds mail and files and
// owns the sign-in: the browser flow, the one token every Google call
// shares (refreshed once, saved once), and noticing when Google has signed
// the twin out so the owner hears about it instead of an empty calendar.
package google

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	googleoauth "golang.org/x/oauth2/google"
	"google.golang.org/api/option"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// Scopes the twin asks for. Calendar full, mail read/send/label, files read.
var Scopes = []string{
	"https://www.googleapis.com/auth/calendar",
	"https://www.googleapis.com/auth/gmail.modify",
	"https://www.googleapis.com/auth/drive.readonly",
	"https://www.googleapis.com/auth/userinfo.email",
}

// APIs are the Google features, in the order people name them.
var APIs = []string{"calendar", "gmail", "drive"}

// scopeFor is the scope each feature needs.
var scopeFor = map[string]string{"calendar": Scopes[0], "gmail": Scopes[1], "drive": Scopes[2]}

// Auth holds the OAuth client and token files. One token file has one Auth
// in the process (see NewAuth), so the calendar, Gmail, Drive and the
// Accounts page share one token: refreshed once, and one opinion on whether
// Google still accepts it.
type Auth struct {
	CredentialsFile string
	TokenFile       string
	// Transport reaches Google; nil means the default. Tests point it at a
	// local fake so nothing leaves the machine.
	Transport http.RoundTripper

	mu    sync.Mutex
	flows map[string]flow // sign-ins in progress, by state

	tmu      sync.Mutex
	ts       oauth2.TokenSource // for the token file as last read
	stamp    fileStamp          // which version of the token file ts came from
	tokID    string             // which sign-in the token file holds
	stale    bool               // the access token was refused: renew it on next use
	out      *SignOut           // Google refused this sign-in, and why
	onOut    func(SignOut)
	cbs      sync.WaitGroup    // onOut calls under way (Settle)
	email    string            // the account's address, once known
	granted  map[string]bool   // features ticked at the last sign-in (nil: not known)
	problems map[string]string // feature → what's wrong with it (API off, box unticked)
	gw       *GmailWatch
}

// flow is one browser sign-in that has started and not yet come back.
type flow struct {
	redirect string
	verifier string // PKCE: only this process can redeem the code Google sends
	expires  time.Time
	bound    bool     // must come back to the browser that started it
	binding  [32]byte // sha256 of that browser's secret
}

type fileStamp struct {
	mod  time.Time
	size int64
}

// flowTTL is how long a sign-in may take before it has to start again.
const flowTTL = 10 * time.Minute

var (
	sharedMu sync.Mutex
	shared   = map[[2]string]*Auth{}
)

// NewAuth returns the Auth for the calendar skill's file settings (the
// Google files). Every caller with the same files gets the same Auth.
func NewAuth(cfg config.Calendar) *Auth {
	key := [2]string{filepath.Clean(cfg.CredentialsFile), filepath.Clean(cfg.TokenFile)}
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if a := shared[key]; a != nil {
		return a
	}
	a := &Auth{CredentialsFile: cfg.CredentialsFile, TokenFile: cfg.TokenFile}
	shared[key] = a
	return a
}

// HasCredentials reports whether an OAuth client (credentials.json) is present.
func (a *Auth) HasCredentials() bool {
	_, err := os.Stat(a.CredentialsFile)
	return err == nil
}

// Connected reports whether a token is stored. It says nothing about whether
// Google still accepts it: see Check and SignedOut.
func (a *Auth) Connected() bool {
	_, err := os.Stat(a.TokenFile)
	return err == nil
}

func (a *Auth) transport() http.RoundTripper {
	if a.Transport != nil {
		return a.Transport
	}
	return http.DefaultTransport
}

// oauthCtx carries the HTTP client the oauth2 package uses to talk to
// Google's token endpoint, with a timeout so a hung network can't hang a turn.
func (a *Auth) oauthCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: a.transport(), Timeout: 30 * time.Second})
}

func (a *Auth) config() (*oauth2.Config, error) {
	b, err := os.ReadFile(a.CredentialsFile)
	if err != nil {
		return nil, errors.New("there's no Google OAuth client yet; add one on the Accounts page (step 5)")
	}
	conf, err := googleoauth.ConfigFromJSON(b, Scopes...)
	if err != nil {
		return nil, fmt.Errorf("the saved Google OAuth client can't be read (%v); paste it again on the Accounts page", err)
	}
	// Google takes the client in the form, as its own endpoint says; left to
	// guess, the oauth2 package asks twice whenever a renewal is refused.
	conf.Endpoint.AuthStyle = oauth2.AuthStyleInParams
	return conf, nil
}

// Begin starts a browser sign-in bound to the browser that asked for it:
// binding is a secret for that browser to keep (a short-lived cookie) and
// hand back with the callback. A callback URL that leaks, from history or a
// shoulder, is useless anywhere else.
func (a *Auth) Begin(redirect string) (authURL, binding string, err error) {
	return a.begin(redirect, true)
}

// BeginURL starts a browser sign-in that isn't bound to a browser (the
// terminal's `mirrin calendar login`, which listens on a one-off port).
// redirect is where Google sends the code. Each call is its own flow, so two
// tabs don't undo each other.
func (a *Auth) BeginURL(redirect string) (string, error) {
	u, _, err := a.begin(redirect, false)
	return u, err
}

func (a *Auth) begin(redirect string, bind bool) (string, string, error) {
	conf, err := a.config()
	if err != nil {
		return "", "", err
	}
	if err := a.checkRedirect(redirect); err != nil {
		return "", "", err
	}
	state, err := randomHex(32)
	if err != nil {
		return "", "", err
	}
	f := flow{redirect: redirect, verifier: oauth2.GenerateVerifier(), expires: time.Now().Add(flowTTL), bound: bind}
	binding := ""
	if bind {
		if binding, err = randomHex(32); err != nil {
			return "", "", err
		}
		f.binding = sha256.Sum256([]byte(binding))
	}
	now := time.Now()
	a.mu.Lock()
	if a.flows == nil {
		a.flows = map[string]flow{}
	}
	for s, old := range a.flows {
		if now.After(old.expires) {
			delete(a.flows, s)
		}
	}
	a.flows[state] = f
	a.mu.Unlock()
	conf.RedirectURL = redirect
	return conf.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce, oauth2.S256ChallengeOption(f.verifier)), binding, nil
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// take removes and returns the flow a callback's state belongs to. A wrong
// state, or the right state from the wrong browser, leaves every real flow
// alone, so a forged or stolen callback can't cancel the owner's sign-in.
func (a *Auth) take(state, binding string, presented bool) (flow, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for s, f := range a.flows {
		if subtle.ConstantTimeCompare([]byte(s), []byte(state)) != 1 {
			continue
		}
		if f.bound {
			sum := sha256.Sum256([]byte(binding))
			if !presented || subtle.ConstantTimeCompare(sum[:], f.binding[:]) != 1 {
				return flow{}, errWrongBrowser
			}
		}
		delete(a.flows, s)
		if time.Now().After(f.expires) {
			return flow{}, errFlowExpired
		}
		return f, nil
	}
	return flow{}, errFlowExpired
}

var (
	errFlowExpired  = errors.New("this sign-in link has expired or was already used; press Connect Google on the Accounts page to start again")
	errWrongBrowser = errors.New("this sign-in was started in a different browser; press Connect Google on the Accounts page in the browser you want to use")
)

// Finish exchanges the code Google sent back and stores the token, for a
// flow started with BeginURL.
func (a *Auth) Finish(ctx context.Context, state, code string) error {
	return a.finish(ctx, state, code, "", false)
}

// FinishBound is Finish for a flow started with Begin: binding is the
// secret the starting browser kept.
func (a *Auth) FinishBound(ctx context.Context, state, code, binding string) error {
	return a.finish(ctx, state, code, binding, true)
}

func (a *Auth) finish(ctx context.Context, state, code, binding string, presented bool) error {
	if state == "" {
		return errFlowExpired
	}
	f, err := a.take(state, binding, presented)
	if err != nil {
		return err
	}
	conf, err := a.config()
	if err != nil {
		return err
	}
	conf.RedirectURL = f.redirect
	tok, err := conf.Exchange(a.oauthCtx(ctx), code, oauth2.VerifierOption(f.verifier))
	if err != nil {
		return a.exchangeError(err)
	}
	if tok.RefreshToken == "" {
		return errors.New("Google didn't hand over a lasting sign-in. Press Connect Google again; if it keeps happening, remove Mirrin at myaccount.google.com/permissions first")
	}
	b, _ := json.Marshal(tok)
	// Written under tmu, with the new sign-in's id, so a renewal of the old
	// sign-in that finishes now can't write the old token back over it.
	a.tmu.Lock()
	if err := os.WriteFile(a.TokenFile, b, 0o600); err != nil {
		a.tmu.Unlock()
		return err
	}
	a.ts, a.out, a.email, a.problems, a.stale = nil, nil, "", nil, false
	a.tokID = tokenID(tok)
	a.granted = grantedFeatures(tok)
	for _, api := range APIs {
		if a.granted != nil && !a.granted[api] {
			a.setProblemLocked(api, fmt.Sprintf("%s wasn't ticked on Google's screen. To add it, press Disconnect, then Connect Google and tick every box.", label(api)))
		}
	}
	a.tmu.Unlock()
	return nil
}

// grantedFeatures reads which features the owner ticked (Google lets people
// untick boxes on the consent screen). nil means Google didn't say.
func grantedFeatures(tok *oauth2.Token) map[string]bool {
	raw, _ := tok.Extra("scope").(string)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	got := strings.Fields(raw)
	out := map[string]bool{}
	for api, scope := range scopeFor {
		out[api] = slices.Contains(got, scope)
	}
	return out
}

// Granted reports which features were ticked at the last sign-in in this
// process; nil when not known (a sign-in from before this run).
func (a *Auth) Granted() map[string]bool {
	a.tmu.Lock()
	defer a.tmu.Unlock()
	if a.granted == nil {
		return nil
	}
	out := make(map[string]bool, len(a.granted))
	for k, v := range a.granted {
		out[k] = v
	}
	return out
}

// Disconnect forgets the token. A renewal still under way finds the sign-in
// gone and doesn't save what it gets.
func (a *Auth) Disconnect() error {
	a.tmu.Lock()
	err := os.Remove(a.TokenFile)
	a.ts, a.out, a.email, a.problems, a.granted, a.stale, a.tokID = nil, nil, "", nil, nil, false, ""
	a.tmu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// OnSignedOut is called once when Google refuses the stored sign-in (and
// again only after a new sign-in is refused too). It runs on its own
// goroutine.
func (a *Auth) OnSignedOut(f func(SignOut)) {
	a.tmu.Lock()
	a.onOut = f
	a.tmu.Unlock()
}

// SignedOut reports whether Google has refused the stored sign-in.
func (a *Auth) SignedOut() (SignOut, bool) {
	a.tmu.Lock()
	defer a.tmu.Unlock()
	if a.out == nil {
		return SignOut{}, false
	}
	return *a.out, true
}

// source is the one token source for the token file as it is now. It is
// built once and reused, so a refreshed token is used by every caller until
// it expires (and saved for the next run); a token file written by someone
// else (`mirrin calendar login`) is picked up.
func (a *Auth) source() (oauth2.TokenSource, error) {
	st, err := os.Stat(a.TokenFile)
	if err != nil {
		return nil, ErrNotConnected
	}
	now := fileStamp{st.ModTime(), st.Size()}
	a.tmu.Lock()
	defer a.tmu.Unlock()
	if a.ts != nil && a.stamp == now && !a.stale {
		return a.ts, nil
	}
	conf, err := a.config()
	if err != nil {
		return nil, err
	}
	tb, err := os.ReadFile(a.TokenFile)
	if err != nil {
		return nil, ErrNotConnected
	}
	var tok oauth2.Token
	if err := json.Unmarshal(tb, &tok); err != nil {
		return nil, errors.New("the saved Google sign-in is damaged; open Accounts and press Connect Google")
	}
	id := tokenID(&tok)
	if id != a.tokID {
		a.tokID, a.out, a.email, a.problems = id, nil, "", nil
	}
	if a.stale {
		tok.Expiry = time.Now().Add(-time.Minute) // Google refused the access token: renew it
		a.stale = false
	}
	inner := conf.TokenSource(a.oauthCtx(context.Background()), &tok)
	a.ts = oauth2.ReuseTokenSource(&tok, &guardSource{a: a, id: id, src: inner, last: tok.AccessToken})
	a.stamp = now
	return a.ts, nil
}

// tokenID names a sign-in without keeping any of it.
func tokenID(t *oauth2.Token) string {
	key := t.RefreshToken
	if key == "" {
		key = t.AccessToken
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// guardSource renews the token, saves what it gets, and notices when Google
// has signed the twin out, after which it stops asking until there is a new
// sign-in.
type guardSource struct {
	a    *Auth
	id   string
	src  oauth2.TokenSource
	mu   sync.Mutex
	last string
}

func (g *guardSource) Token() (*oauth2.Token, error) {
	if so, ok := g.a.signedOutAs(g.id); ok {
		return nil, &signedOutError{so}
	}
	t, err := g.src.Token()
	if err != nil {
		if so, ok := classify(err); ok {
			so.Token = g.id
			so.At = time.Now()
			g.a.markOut(so)
			return nil, &signedOutError{so}
		}
		return nil, &offlineError{err}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if t.AccessToken != g.last {
		g.last = t.AccessToken
		g.save(t)
	}
	return t, nil
}

// save keeps a renewed token for the next run, only while its sign-in is
// still the current one: a renewal that finishes after Disconnect, or after
// a new sign-in, must not bring the old sign-in back.
func (g *guardSource) save(t *oauth2.Token) {
	b, err := json.Marshal(t)
	if err != nil {
		return
	}
	a := g.a
	a.tmu.Lock()
	defer a.tmu.Unlock()
	if a.tokID != g.id {
		return
	}
	if os.WriteFile(a.TokenFile, b, 0o600) != nil {
		return
	}
	if st, err := os.Stat(a.TokenFile); err == nil {
		a.stamp = fileStamp{st.ModTime(), st.Size()}
	}
}

func (a *Auth) signedOutAs(id string) (SignOut, bool) {
	a.tmu.Lock()
	defer a.tmu.Unlock()
	if a.out != nil && a.out.Token == id {
		return *a.out, true
	}
	return SignOut{}, false
}

func (a *Auth) markOut(so SignOut) {
	a.tmu.Lock()
	if a.tokID != so.Token || a.out != nil {
		a.tmu.Unlock()
		return
	}
	a.out = &so
	cb := a.onOut
	a.tmu.Unlock()
	if so.Client {
		a.setAside() // so the Accounts page asks for a new client
	}
	if cb != nil {
		a.cbs.Add(1)
		go func() {
			defer a.cbs.Done()
			cb(so)
		}()
	}
}

// Settle waits for the OnSignedOut calls under way to return, so a test can
// look at what they did.
func (a *Auth) Settle() { a.cbs.Wait() }

// expireAccess makes the next call renew the access token: Google refused
// it (revoked from the Google account page, a password change), and
// renewing is how a refused sign-in is noticed.
func (a *Auth) expireAccess() {
	a.tmu.Lock()
	a.stale = true
	a.tmu.Unlock()
}

// HTTPClient is an HTTP client authorised as the connected account.
func (a *Auth) HTTPClient() (*http.Client, error) {
	ts, err := a.source()
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: &oauth2.Transport{Source: ts, Base: a.transport()}}, nil
}

// ClientOptions are what a google.golang.org/api service needs to act as
// the connected account.
func (a *Auth) ClientOptions() ([]option.ClientOption, error) {
	hc, err := a.HTTPClient()
	if err != nil {
		return nil, err
	}
	return []option.ClientOption{option.WithHTTPClient(hc)}, nil
}

// Check makes sure Google still accepts the stored sign-in, renewing it if
// it is due. It returns ErrNotConnected, a signed-out error (see
// IsSignedOut), or an error reaching Google.
func (a *Auth) Check(ctx context.Context) error {
	ts, err := a.source()
	if err != nil {
		return err
	}
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := ts.Token()
		done <- result{err}
	}()
	select {
	case r := <-done:
		return r.err
	case <-ctx.Done():
		return &offlineError{ctx.Err()}
	}
}

// knownEmail is the account's address if it is already known, without
// asking Google.
func (a *Auth) knownEmail() string {
	a.tmu.Lock()
	defer a.tmu.Unlock()
	return a.email
}

// Email returns the connected account's address, or "". It asks Google once
// per sign-in.
func (a *Auth) Email(ctx context.Context) string {
	a.tmu.Lock()
	cached := a.email
	a.tmu.Unlock()
	if cached != "" {
		return cached
	}
	hc, err := a.HTTPClient()
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.googleapis.com/oauth2/v2/userinfo", nil)
	resp, err := hc.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var info struct {
		Email string `json:"email"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&info)
	if info.Email != "" {
		a.tmu.Lock()
		a.email = info.Email
		a.tmu.Unlock()
	}
	return info.Email
}
