package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/protocols"
	gauth "github.com/MavrkAI/Mirrin/internal/skills/google"
	"github.com/MavrkAI/Mirrin/internal/skills/google/googletest"
)

// googleDaemon is a test daemon whose Google is a local fake.
func googleDaemon(t *testing.T) (*testDaemon, *googletest.Fake) {
	t.Helper()
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("NOTHING_TO_REPORT") })
	fake := googletest.New(t)
	td.google.Transport = fake.Transport()
	t.Cleanup(func() { settleGoogle(td) }) // before the store closes and the home is removed
	return td, fake
}

// settleGoogle waits for the Google work the daemon left running in the
// background: the check after connecting, and telling the owner of a
// sign-out.
func settleGoogle(td *testDaemon) {
	td.googleWork.Wait()
	td.google.Settle()
}

// connectThroughThePage signs in the way the Accounts page does: paste the
// client, press Connect, come back from Google.
func connectThroughThePage(t *testing.T, td *testDaemon) {
	t.Helper()
	ctx := context.Background()
	if err := td.SaveGoogleClient(ctx, "123-abc.apps.googleusercontent.com", "GOCSPX-test", ""); err != nil {
		t.Fatal(err)
	}
	// A twin listening on every address still gets Google's answer on this computer.
	u, binding, err := td.BeginGoogleBound(ctx, "http://0.0.0.0:7742/oauth/google")
	if err != nil {
		t.Fatal(err)
	}
	q, _ := url.Parse(u)
	if got := q.Query().Get("redirect_uri"); got != "http://127.0.0.1:7742/oauth/google" {
		t.Fatalf("redirect %q", got)
	}
	if err := td.FinishGoogleBound(ctx, q.Query().Get("state"), "code", binding); err != nil {
		t.Fatal(err)
	}
	// The check it starts is done before the test changes what Google says.
	settleGoogle(td)
}

func auditHas(t *testing.T, td *testDaemon, kind, want string) bool {
	t.Helper()
	es, err := td.store.RecentAuditOfKind(context.Background(), kind, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if strings.Contains(e.Detail, want) {
			return true
		}
	}
	return false
}

// Connecting Google turns everything on and starts watching the calendar
// and the Gmail inbox at once, with no restart; new mail is then noticed.
func TestConnectingGoogleStartsWatchingGmailAndCalendarLive(t *testing.T) {
	td, fake := googleDaemon(t)
	if len(td.googleWatchSources()) != 0 {
		t.Fatal("watching Google before it was connected")
	}
	fake.AddMessage(googletest.Message{ID: "old", Labels: []string{"INBOX", "UNREAD"}, Payload: googletest.Headers(googletest.Part("text/plain", "text/plain", "", []byte("hi"), 0), "From", "Old <old@example.com>", "Subject", "Already there")})
	connectThroughThePage(t, td)

	cfg := td.Config()
	if !cfg.Skills.Calendar.Enabled || !cfg.Skills.Gmail.Enabled || !cfg.Skills.Drive.Enabled {
		t.Fatalf("features not on: %+v", cfg.Skills)
	}
	for _, name := range []string{"gmail_search", "gmail_read", "list_events", "drive_search"} {
		if !slices.Contains(td.agent.Tools().Names(), name) {
			t.Errorf("tool %s not registered", name)
		}
	}
	if got := td.googleWatchSources(); !slices.Contains(got, "gmail") || !slices.Contains(got, "calendar") {
		t.Fatalf("watching %v", got)
	}

	ctx := context.Background()
	td.watcher.Poll(ctx) // the starting point: mail already there isn't news
	if auditHas(t, td, "watch.change", "Already there") {
		t.Fatal("mail that was there before connecting was reported")
	}
	fake.AddMessage(googletest.Message{ID: "new", Labels: []string{"INBOX", "UNREAD"}, Payload: googletest.Headers(googletest.Part("text/plain", "text/plain", "", []byte("The lease is ready"), 0), "From", "Landlord <landlord@example.com>", "Subject", "About the lease")})
	td.watcher.Poll(ctx)
	if !auditHas(t, td, "watch.change", "gmail: NEW: unread from Landlord: About the lease") {
		t.Fatal("new Gmail mail not noticed")
	}

	if err := td.DisconnectGoogle(ctx); err != nil {
		t.Fatal(err)
	}
	if len(td.googleWatchSources()) != 0 || slices.Contains(td.agent.Tools().Names(), "gmail_search") {
		t.Fatal("still watching or offering Google after disconnecting")
	}
}

// Boxes left unticked on Google's screen stay off, and the page says so.
func TestUntickedFeaturesStayOffAndThePageSaysWhy(t *testing.T) {
	td, fake := googleDaemon(t)
	fake.Untick("gmail")
	connectThroughThePage(t, td)
	cfg := td.Config()
	if cfg.Skills.Gmail.Enabled || !cfg.Skills.Calendar.Enabled || !cfg.Skills.Drive.Enabled {
		t.Fatalf("features: %+v", cfg.Skills)
	}
	st := td.AccountStates(context.Background())[0]
	if !st.Connected || !strings.Contains(st.Blurb, "Gmail wasn't ticked") {
		t.Fatalf("page: %+v", st)
	}
}

// Google ending the sign-in (7 days into Testing mode) is caught by the
// self-check, shown on the Accounts page as needing a reconnect, and told
// to the owner once, with the exact fix and the page to do it on.
func TestGoogleSignOutIsCaughtAndToldOnceWithTheFix(t *testing.T) {
	td, fake := googleDaemon(t)
	connectThroughThePage(t, td)
	ctx := context.Background()
	// Connected a week ago; the access token has since expired.
	_ = td.store.Set(ctx, googleConnectedKey, time.Now().Add(-7*24*time.Hour).UTC().Format(time.RFC3339))
	fake.WriteFiles(t, td.google.CredentialsFile, td.google.TokenFile, time.Now().Add(-time.Hour))
	fake.SignOut()

	state, detail, fix := td.googleHealth(ctx)
	if state != health.Fail || !strings.Contains(detail, "signed Mirrin out") || !strings.Contains(fix, "Connect Google") || !strings.Contains(fix, "Publish app") {
		t.Fatalf("health: %s %q %q", state, detail, fix)
	}
	msg := td.ch.next(t)
	for _, want := range []string{"Google signed me out", "7 days", "Testing", "Publish app", "Connect Google", "http://127.0.0.1:7742/accounts"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "token=") {
		t.Fatalf("the page key went into a chat: %s", msg)
	}

	st := td.AccountStates(ctx)[0]
	if st.Connected || !st.HasClient || !strings.Contains(st.Blurb, "signed your twin out") || len(st.Steps) != 2 || !strings.Contains(st.Steps[0].Text, "Publish app") {
		t.Fatalf("page: %+v", st)
	}

	// However often it's noticed, and after a restart, the owner is told once.
	so, _ := td.google.SignedOut()
	td.googleSignedOut(so)
	td.googleHealth(ctx)
	settleGoogle(td)
	for _, m := range td.ch.messages() {
		if m != "owner: "+msg && strings.Contains(m, "signed me out") {
			t.Fatalf("told twice: %q", td.ch.messages())
		}
	}
	if n := strings.Count(strings.Join(td.ch.messages(), "\n"), "Google signed me out"); n != 1 {
		t.Fatalf("told %d times", n)
	}

	// Reconnecting clears it.
	connectThroughThePage(t, td)
	if st := td.AccountStates(ctx)[0]; !st.Connected {
		t.Fatalf("after reconnecting: %+v", st)
	}
	if state, _, _ := td.googleHealth(ctx); state != health.OK {
		t.Fatalf("health after reconnecting: %s", state)
	}
}

// A sign-out that isn't Testing's 7 days (access removed from the Google
// account, a password change) isn't blamed on Testing, but the owner still
// learns how to stop that from happening too.
func TestSignedOutMessageOnlyBlamesTestingWhenItFits(t *testing.T) {
	td, _ := googleDaemon(t)
	ctx := context.Background()
	_ = td.store.Set(ctx, googleConnectedKey, time.Now().Add(-2*24*time.Hour).UTC().Format(time.RFC3339))
	msg := td.signedOutMessage(ctx, gauth.SignOut{At: time.Now()})
	if strings.Contains(msg, "it's been 7 days") || !strings.Contains(msg, "expired or was removed") || !strings.Contains(msg, "Connect Google") || !strings.Contains(msg, "http://127.0.0.1:7742/accounts") {
		t.Fatalf("message: %s", msg)
	}
}

// A deleted OAuth client (or a changed secret) needs a new client, not just
// a new sign-in: the page asks for one and the owner is told so.
func TestDeletedClientAsksForANewOne(t *testing.T) {
	td, fake := googleDaemon(t)
	connectThroughThePage(t, td)
	ctx := context.Background()
	fake.WriteFiles(t, td.google.CredentialsFile, td.google.TokenFile, time.Now().Add(-time.Hour))
	fake.DeleteClient()
	state, detail, fix := td.googleHealth(ctx)
	if state != health.Fail || !strings.Contains(detail, "OAuth client") || !strings.Contains(fix, "Desktop app") {
		t.Fatalf("health: %s %q %q", state, detail, fix)
	}
	if msg := td.ch.next(t); !strings.Contains(msg, "new client") || !strings.Contains(msg, "Accounts") {
		t.Fatalf("message: %s", msg)
	}
	st := td.AccountStates(ctx)[0]
	if st.Connected || st.HasClient || len(st.Steps) != 6 || !strings.Contains(st.Blurb, "no longer accepts") {
		t.Fatalf("page: %+v", st)
	}
	// Pasting a new client and connecting again puts it right.
	fake.NewClient()
	connectThroughThePage(t, td)
	if st := td.AccountStates(ctx)[0]; !st.Connected {
		t.Fatalf("after a new client: %+v", st)
	}
}

func TestAccountsPageWalksThroughSetup(t *testing.T) {
	td, fake := googleDaemon(t)
	ctx := context.Background()
	st := td.AccountStates(ctx)[0]
	if st.HasClient || st.Connected || len(st.Steps) != 6 {
		t.Fatalf("fresh: %+v", st)
	}
	joined := ""
	for _, s := range st.Steps {
		joined += s.Text + " " + s.URL + "\n"
	}
	for _, want := range []string{"Desktop app", "Publish app", "7 days", "Go to Mirrin", "flows/enableapi", "auth/audience", "auth/clients"} {
		if !strings.Contains(joined, want) {
			t.Errorf("steps lack %q", want)
		}
	}
	if state, _, _ := td.googleHealth(ctx); state != health.Off {
		t.Fatalf("health before connecting: %s", state)
	}

	if err := td.SaveGoogleClient(ctx, "AIzaSyNotAClient", "x", ""); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("api key accepted: %v", err)
	}
	if err := td.SaveGoogleClient(ctx, "123-abc.apps.googleusercontent.com", "GOCSPX-test", ""); err != nil {
		t.Fatal(err)
	}
	if st := td.AccountStates(ctx)[0]; !st.HasClient || len(st.Steps) != 2 || !strings.Contains(st.Steps[0].Text, "Publish app") || !strings.Contains(st.Steps[0].Text, "7 days") {
		// Everyone upgrading from an earlier Mirrin already has a client:
		// publishing must come before Connect, or they are signed out in a week.
		t.Fatalf("client saved: %+v", st)
	}

	connectThroughThePage(t, td)
	fake.Disable("drive")
	if _, err := td.google.Probe(ctx, "calendar", "gmail", "drive"); err != nil {
		t.Fatal(err)
	}
	st = td.AccountStates(ctx)[0]
	if !st.Connected || st.Email != "sam@example.com" || !strings.Contains(st.Blurb, "Drive API is turned off") {
		t.Fatalf("connected with a problem: %+v", st)
	}
	state, detail, fix := td.googleHealth(ctx)
	if state != health.Fail || !strings.Contains(detail, "Drive") || !strings.Contains(fix, "drive.googleapis.com") {
		t.Fatalf("health: %s %q %q", state, detail, fix)
	}
	fake.Enable("drive")
	if state, detail, _ := td.googleHealth(ctx); state != health.OK || !strings.Contains(detail, "sam@example.com") {
		t.Fatalf("health: %s %q", state, detail)
	}
}

// When telling the owner fails (no channel up yet, no desktop to show a
// notification), the hourly self-check tells them once it can.
func TestSignOutNoticeIsRetriedUntilItIsHeard(t *testing.T) {
	td, fake := googleDaemon(t)
	connectThroughThePage(t, td)
	ctx := context.Background()
	td.chmu.Lock()
	delete(td.channels, "telegram") // nothing reaches the owner
	td.chmu.Unlock()
	fake.WriteFiles(t, td.google.CredentialsFile, td.google.TokenFile, time.Now().Add(-time.Hour))
	fake.SignOut()
	if state, _, _ := td.googleHealth(ctx); state != health.Fail {
		t.Fatalf("health: %s", state)
	}
	settleGoogle(td) // the notice tried, and failed
	if told, _ := td.store.Get(ctx, googleToldKey); told != "" {
		t.Fatalf("a notice nobody heard was taken as told (%q)", told)
	}

	td.chmu.Lock()
	td.channels["telegram"] = td.ch // the channel comes up
	td.chmu.Unlock()
	td.googleHealth(ctx) // the next hourly check
	if msg := td.ch.next(t); !strings.Contains(msg, "Google signed me out") || !strings.Contains(msg, "Connect Google") {
		t.Fatalf("message: %s", msg)
	}
}

// A connected account whose client file is gone isn't said to be refused by
// Google unless Google did refuse it.
func TestMissingClientIsNotBlamedOnGoogle(t *testing.T) {
	td, _ := googleDaemon(t)
	connectThroughThePage(t, td)
	ctx := context.Background()
	if err := os.Remove(td.google.CredentialsFile); err != nil {
		t.Fatal(err)
	}
	st := td.AccountStates(ctx)[0]
	if st.Connected || st.HasClient || strings.Contains(st.Blurb, "no longer accepts") || !strings.Contains(st.Blurb, "missing") {
		t.Fatalf("page: %+v", st)
	}
	// Set aside because Google refused it: then it says so.
	if err := os.WriteFile(td.google.CredentialsFile+".rejected", []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := td.AccountStates(ctx)[0]; !strings.Contains(st.Blurb, "no longer accepts") {
		t.Fatalf("page: %+v", st)
	}
}

// Regression (google merged with devices): a twin listening on a Tailscale
// address was refused Connect Google up front ("Google only returns a
// sign-in to 127.0.0.1"), and told to listen on 0.0.0.0, though the API
// always serves 127.0.0.1 on the same port too (devices' listeners) and the
// page is opened there. The sign-in goes ahead on loopback, and the
// sign-out notice links the Accounts page where it opens: on loopback.
func TestConnectingWithATailscaleListenUsesLoopback(t *testing.T) {
	td, _ := googleDaemon(t)
	ctx := context.Background()
	if err := td.SaveGoogleClient(ctx, "123-abc.apps.googleusercontent.com", "GOCSPX-test", ""); err != nil {
		t.Fatal(err)
	}
	setListen := func(addr string) {
		td.cmu.Lock()
		td.cfg.API.Listen = addr
		td.cfg.API.Remote = true
		td.cmu.Unlock()
	}
	setListen("100.64.1.2:7742")
	if got := td.accountsPage(); got != "http://127.0.0.1:7742/accounts" {
		t.Errorf("page link: %s", got)
	}
	u, err := td.BeginGoogle(ctx, "http://127.0.0.1:7742/oauth/google")
	if err != nil {
		t.Fatalf("a Tailscale listen was refused: %v", err)
	}
	parsed, _ := url.Parse(u)
	if got := parsed.Query().Get("redirect_uri"); got != "http://127.0.0.1:7742/oauth/google" {
		t.Fatalf("redirect_uri %q", got)
	}
	// A page opened on the Tailscale address can't finish a sign-in: the
	// google package says so in words.
	if _, err := td.BeginGoogle(ctx, "http://100.64.1.2:7742/oauth/google"); err == nil || !strings.Contains(err.Error(), "computer it started on") {
		t.Fatalf("a non-loopback redirect: %v", err)
	}
	for _, addr := range []string{"0.0.0.0:7742", "[::]:7742", "127.0.0.1:7742", "localhost:7742"} {
		setListen(addr)
		if got := td.accountsPage(); got != "http://127.0.0.1:7742/accounts" {
			t.Errorf("%s: page link %s", addr, got)
		}
		if _, _, err := td.BeginGoogleBound(ctx, "http://"+addr+"/oauth/google"); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
}

func TestLoopbackRedirect(t *testing.T) {
	for in, want := range map[string]string{
		"http://0.0.0.0:7742/oauth/google":    "http://127.0.0.1:7742/oauth/google",
		"http://[::]:7742/oauth/google":       "http://127.0.0.1:7742/oauth/google",
		"http://127.0.0.1:7742/oauth/google":  "http://127.0.0.1:7742/oauth/google",
		"http://localhost:7742/oauth/google":  "http://localhost:7742/oauth/google",
		"http://100.64.0.2:7742/oauth/google": "http://100.64.0.2:7742/oauth/google", // left as it is: the google package refuses it in words ("computer it started on")
	} {
		if got := loopbackRedirect(in); got != want {
			t.Errorf("%s: got %s", in, got)
		}
	}
	if s := (&Daemon{}).ExplainGoogleError("access_denied"); !strings.Contains(s, "Test users") {
		t.Fatal(s)
	}
}

// The bundled email chores work for someone who connected Gmail and never
// set up IMAP: none of them says email is missing, and every Gmail tool
// their prompts name is really there.
func TestBundledChoresWorkWithGmailAlone(t *testing.T) {
	td, _ := googleDaemon(t)
	if err := protocols.WriteExamples(td.Config().ProtocolsDir); err != nil {
		t.Fatal(err)
	}
	_ = td.ReloadProtocols()
	before := map[string][]string{}
	for _, p := range td.InstalledProtocols(context.Background()) {
		before[p.Name] = p.Missing
	}
	connectThroughThePage(t, td)
	if td.Config().Skills.Email.Enabled {
		t.Fatal("test assumes no IMAP")
	}
	_ = td.ReloadProtocols()
	have := td.agent.Tools().Names()
	reTool := regexp.MustCompile(`\bgmail_[a-z_]+`)
	checked := 0
	for _, p := range td.InstalledProtocols(context.Background()) {
		if !slices.Contains(p.Requires, "email") {
			continue
		}
		checked++
		if !slices.Contains(before[p.Name], "email") {
			t.Errorf("%s: didn't need email before connecting? %v", p.Name, before[p.Name])
		}
		if len(p.Missing) > 0 {
			t.Errorf("%s still needs %v with Gmail connected", p.Name, p.Missing)
		}
		for _, proto := range td.Protocols() {
			if proto.Name != p.Name {
				continue
			}
			for _, tool := range reTool.FindAllString(proto.Prompt, -1) {
				if !slices.Contains(have, tool) {
					t.Errorf("%s names %s, which Gmail doesn't offer", p.Name, tool)
				}
			}
		}
	}
	if checked < 3 {
		t.Fatalf("only %d bundled chores need email", checked)
	}
}

var _ api.AccountsBackend = (*Daemon)(nil)

// Wiring the screen's calendar_error (ui and google both left the daemon
// side undone): after Google signs the twin out, the presence screen says
// why the day is empty instead of "Nothing on the board.". The screen's
// calendar client is read by /screen while connecting or disconnecting
// swaps it, so it is swapped atomically (run under -race).
func TestTheScreenSaysWhyTheCalendarIsEmpty(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("NOTHING_TO_REPORT") })
	fake := googletest.NewInProcess()
	td.google.Transport = fake.Transport()
	t.Cleanup(func() { settleGoogle(td) })
	// Set up the screen directly: connecting through OAuth starts a background
	// probe that could refresh the token while this test expires it.
	fake.WriteFiles(t, td.google.CredentialsFile, td.google.TokenFile, time.Now().Add(-time.Hour))
	if err := td.UpdateConfig(func(c *config.Config) { c.Skills.Calendar.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	td.applyGoogle()
	ctx := context.Background()
	if td.calendar.Load() == nil {
		t.Fatal("setup: connecting didn't give the screen a calendar")
	}
	fake.WriteFiles(t, td.google.CredentialsFile, td.google.TokenFile, time.Now().Add(-time.Hour))
	fake.SignOut()
	sd := td.screenData(ctx)
	if !strings.Contains(sd.CalendarError, "signed me out") || len(sd.Events) != 0 {
		t.Fatalf("screen after a sign-out: %q %v", sd.CalendarError, sd.Events)
	}
	b, _ := json.Marshal(sd)
	if !strings.Contains(string(b), `"calendar_error":`) {
		t.Fatalf("the page reads calendar_error: %s", b)
	}
	if calendarProblem(errors.New("boom")) != "Google Calendar didn't answer." {
		t.Fatal("other failures")
	}
	// A working calendar reports nothing.
	fake.WriteFiles(t, td.google.CredentialsFile, td.google.TokenFile, time.Now().Add(time.Hour))
	td.applyGoogle() // a fresh client, as reconnecting makes
	if sd := td.screenData(ctx); sd.CalendarError != "" {
		t.Fatalf("a working calendar reported %q", sd.CalendarError)
	}
	// /screen reading while the calendar is swapped.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 20 {
			td.applyGoogle()
		}
	}()
	for range 20 {
		_ = td.screenData(ctx)
	}
	<-done
}
