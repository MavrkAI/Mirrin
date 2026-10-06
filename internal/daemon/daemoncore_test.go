package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/persona"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// The regression: a machine standing by after the twin moved answered
// "resume me from the menu bar", which the standby undoes at once.
func TestStandbyReplySaysHowToResume(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("unused") })
	if err := backup.UpdateState(td.Config().DataDir, func(s *backup.State) {
		s.Standby = &backup.Handover{HostLabel: "the Mac mini", At: time.Now()}
	}); err != nil {
		t.Fatal(err)
	}
	td.paused.Store(true)
	reply := td.owner(t, "are you there?")
	if !strings.Contains(reply, "mirrin backup resume") || !strings.Contains(reply, "the Mac mini") {
		t.Fatalf("reply %q", reply)
	}
	if len(td.llm.heard()) != 0 {
		t.Fatal("a paused twin doesn't ask the model")
	}
}

// The regression: while another program held the API's port, the menu's
// links opened that program.
func TestPageLinksAreEmptyWhileThePortIsTaken(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := holder.Addr().String()
	td.cmu.Lock()
	td.cfg.API.Listen = addr
	td.cmu.Unlock()
	if _, err := api.LoadOrCreateToken(td.Config().DataDir); err != nil { // the welcome link needs it
		t.Fatal(err)
	}
	base := "http://" + addr
	td.uiURL, td.memoryURL, td.healthURL, td.protocolsURL, td.channelsURL, td.accountsURL = base+"/ui", base+"/memory", base+"/health", base+"/protocols", base+"/channels", base+"/accounts"
	prevRetry := apiRetry
	apiRetry = 10 * time.Millisecond
	t.Cleanup(func() { apiRetry = prevRetry })
	links := func() []string {
		return []string{td.UIURL(), td.MemoryURL(), td.HealthURL(), td.ProtocolsURL(), td.ChannelsURL(), td.AccountsURL(), td.WelcomeURL()}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serving := make(chan struct{})
	go td.serveAPI(ctx, func(ctx context.Context) error {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		close(serving)
		<-ctx.Done()
		return ln.Close()
	})
	eventually(t, "the API to fail", func() bool { return td.apiErr.Load() != nil })
	for _, u := range links() {
		if u != "" {
			t.Fatalf("a link while the port is taken: %q", links())
		}
	}
	holder.Close()
	<-serving
	eventually(t, "the API to recover", func() bool { return td.apiErr.Load() == nil })
	for i, u := range links() {
		if !strings.HasPrefix(u, base) {
			t.Fatalf("link %d once the port is free: %q", i, u)
		}
	}
}

// reach.admin_remote was saved but never given to the API server, so the
// settings pages stayed closed to the admin's other devices.
func TestAdminRemoteOpensSettingsOnRemoteListeners(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprint("admin_remote ", admin), func(t *testing.T) {
			td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
			td.cmu.Lock()
			td.cfg.Reach.AdminRemote = admin
			td.cmu.Unlock()
			tok, err := api.LoadOrCreateToken(td.Config().DataDir)
			if err != nil {
				t.Fatal(err)
			}
			srv := td.newAPIServer(tok)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				_ = srv.ServeListener(ctx, ln, api.ListenerConfig{Exposure: api.Remote, Via: "test", Hostnames: []string{"twin.test"}})
			}()
			req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/channels/list", nil)
			req.Host = "twin.test"
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if notHere := strings.Contains(string(body), "not_here"); notHere == admin {
				t.Fatalf("admin_remote %v: %d %s", admin, resp.StatusCode, body)
			}
		})
	}
}

// retiredServer is an Anthropic endpoint where one model has been retired.
func retiredServer(t *testing.T, retired string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if body["model"] == retired {
			w.WriteHeader(404)
			fmt.Fprintf(w, `{"type":"error","error":{"type":"not_found_error","message":"model: %s"}}`, retired)
			return
		}
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"x","content":[{"type":"text","text":"still here"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
}

// The regression: a retired llm.voice_model failed every spoken reply, and
// nobody was told.
func TestRetiredVoiceModelFallsBack(t *testing.T) {
	const retired = "claude-3-opus-20240229"
	retiredServer(t, retired)
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("unused") })
	td.cmu.Lock()
	td.cfg.LLM.VoiceModel = retired
	cfg := *td.cfg
	td.cmu.Unlock()
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	p := td.applyVoiceProvider(&cfg)
	if _, ok := p.(*llm.Fallback); !ok {
		t.Fatalf("the voice model should fall back when retired, got %T", p)
	}
	resp, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{llm.Text(llm.RoleUser, "hi")}})
	if err != nil || resp.Message.PlainText() != "still here" {
		t.Fatalf("spoken replies go on with the default: %v %v", resp, err)
	}
	td.noticeRetired()
	notice := td.ch.next(t)
	if !strings.Contains(notice, "the model I was set to speak with") || !strings.Contains(notice, llm.DefaultModel("anthropic")) {
		t.Fatalf("notice %q", notice)
	}
	eventually(t, "the notice to be remembered", func() bool {
		v, _ := td.store.Get(context.Background(), retiredVoiceKey)
		return v == "anthropic/"+retired
	})
	td.noticeRetired()
	td.modelRetired(fieldVoiceModel, "anthropic", retired, llm.DefaultModel("anthropic")) // as after a restart
	select {
	case again := <-td.ch.out:
		t.Fatalf("told twice: %q", again)
	case <-time.After(200 * time.Millisecond):
	}
	if got := td.Config().LLM.Model; got != llm.DefaultModel("anthropic") {
		t.Fatalf("the main model is untouched, got %q", got)
	}
	aps, _ := td.store.AllPendingApprovals(context.Background())
	if len(aps) != 1 || !strings.HasPrefix(aps[0].Summary, "keep speaking with ") {
		t.Fatalf("the approval should say speaking: %+v", aps)
	}
	td.owner(t, "yes")
	if got := td.Config().LLM.VoiceModel; got != llm.DefaultModel("anthropic") {
		t.Fatalf("a yes keeps the stand-in as the voice model, got %q", got)
	}
}

// The regression: /status and the menu bar named the retired model while
// the twin was thinking with its stand-in.
func TestStatusShowsTheStandInModel(t *testing.T) {
	const retired = "claude-3-opus-20240229"
	retiredServer(t, retired)
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("unused") })
	td.cmu.Lock()
	td.cfg.LLM.Model = retired
	cfg := *td.cfg
	td.cmu.Unlock()
	p, err := buildModel(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	td.agent.SetProvider(p)
	if st := td.Status(context.Background()); st.Model != retired {
		t.Fatalf("before any fallback the configured model shows, got %q", st.Model)
	}
	if reply := td.owner(t, "hello?"); reply != "still here" {
		t.Fatalf("reply %q", reply)
	}
	want := llm.DefaultModel("anthropic")
	if got := td.owner(t, "/status"); !strings.Contains(got, "Model "+want+".") {
		t.Fatalf("/status %q", got)
	}
	if st := td.Status(context.Background()); st.Model != want {
		t.Fatalf("status model %q", st.Model)
	}
	if state, detail, _ := td.modelHealth(context.Background()); state != health.Warn || !strings.Contains(detail, want) || !strings.Contains(detail, retired) {
		t.Fatalf("health %s %q", state, detail)
	}
}

// keep_model was offered to the model with every request, inviting it to
// switch models on its own. Only the daemon's approval calls it now.
func TestKeepModelIsHiddenFromTheModel(t *testing.T) {
	var offered []string
	td := newTestDaemon(t, func(_ string, req llm.Request) llm.Response {
		for _, s := range req.Tools {
			offered = append(offered, s.Name)
		}
		return say("ok")
	})
	td.owner(t, "hello")
	if len(offered) == 0 || slices.Contains(offered, "keep_model") {
		t.Fatalf("offered %v", offered)
	}
	if _, ok := td.agent.Tools().Get("keep_model"); !ok {
		t.Fatal("approvals still find keep_model")
	}
}

// The regression: a change to the local API ran every self-check again,
// probing the model (a paid request) each time the port was taken or freed.
func TestAPIRecheckDoesNotProbeTheModel(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("pong") })
	td.health = td.newHealth()
	td.health.Run(context.Background())
	if n := len(td.llm.heard()); n != 1 {
		t.Fatalf("startup probes the model once, got %d", n)
	}
	td.apiFailed(fmt.Errorf("listen tcp: %w", syscall.EADDRINUSE))
	eventually(t, "the API check to fail", func() bool {
		for _, r := range td.Health().Results {
			if r.Name == "api" {
				return r.State == health.Fail
			}
		}
		return false
	})
	if n := len(td.llm.heard()); n != 1 {
		t.Fatalf("the model was probed %d times", n)
	}
}

// writeProtocol saves a protocol file and reloads.
func writeProtocol(t *testing.T, td *testDaemon, file, body string) {
	t.Helper()
	dir := td.Config().ProtocolsDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The regression: a protocol file that couldn't be read was only logged, so
// the owner never learned why a protocol they wrote didn't run.
func TestSkippedProtocolFileIsReportedOnce(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	writeProtocol(t, td, "broken.yaml", "name: [unclosed\n")
	_ = td.loadProtocols()
	notice := td.ch.next(t)
	if !strings.Contains(notice, "broken.yaml") || !strings.Contains(notice, "/reload") {
		t.Fatalf("notice %q", notice)
	}
	_ = td.loadProtocols()
	rep := td.RunHealth(context.Background())
	var files *health.Result
	for i := range rep.Results {
		if rep.Results[i].Name == "files" {
			files = &rep.Results[i]
		}
	}
	if files == nil || files.State != health.Warn || !strings.Contains(files.Detail, "broken.yaml") {
		t.Fatalf("health %+v", files)
	}
	select {
	case again := <-td.ch.out:
		t.Fatalf("told twice: %q", again)
	case <-time.After(300 * time.Millisecond):
	}
	// Fixed, then broken again: that is news again.
	writeProtocol(t, td, "broken.yaml", "name: fixed\nprompt: hi\n")
	_ = td.loadProtocols()
	eventually(t, "the fix to be noted", func() bool {
		v, _ := td.store.Get(context.Background(), problemFilesKey)
		return !strings.Contains(v, "broken.yaml")
	})
	writeProtocol(t, td, "broken.yaml", "name: [unclosed\n")
	_ = td.loadProtocols()
	if again := td.ch.next(t); !strings.Contains(again, "broken.yaml") {
		t.Fatalf("notice %q", again)
	}
	eventually(t, "the notice to be remembered", func() bool {
		v, _ := td.store.Get(context.Background(), problemFilesKey)
		return strings.Contains(v, "broken.yaml")
	})
}

func TestApprovalTTLFromConfig(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	ctx := context.Background()
	id, err := td.store.CreateApproval(ctx, ownerKey, "send", json.RawMessage(`{"to":"bob"}`), "send to bob")
	if err != nil {
		t.Fatal(err)
	}
	withClock(t, 2*time.Hour)
	td.expireApprovals(ctx)
	if ap, _ := td.store.GetApproval(ctx, id); ap.Status != "pending" {
		t.Fatalf("with the default 72h a 2h-old request waits, got %s", ap.Status)
	}
	td.cmu.Lock()
	td.cfg.Autonomy.ApprovalTTL = config.Duration(time.Hour)
	td.cmu.Unlock()
	td.expireApprovals(ctx)
	if ap, _ := td.store.GetApproval(ctx, id); ap.Status != "expired" {
		t.Fatalf("approval_ttl: 1h should expire a 2h-old request, got %s", ap.Status)
	}
	es, _ := td.store.RecentAuditOfKind(ctx, "approval.expired", 1)
	if len(es) != 1 || !strings.HasSuffix(es[0].Detail, "no answer in 1h") {
		t.Fatalf("audit %+v", es)
	}
}

func runProtocolTool(td *testDaemon, chatKey, name string) (string, error) {
	in, _ := json.Marshal(map[string]string{"name": name})
	return td.agent.Tools().Run(context.Background(), "run_protocol", tools.Call{ChatKey: chatKey, Input: in})
}

// The regression: run_protocol sent the model a prompt with {{city}} still
// in it, and it made up a city.
func TestRunProtocolRefusesUnsetVariables(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("sunny") })
	writeProtocol(t, td, "weather.yaml", "name: weather\nprompt: Check the weather in {{city}}.\nvars:\n  city:\n    description: The city you live in.\n")
	_ = td.loadProtocols()
	out, err := runProtocolTool(td, ownerKey, "weather")
	if err == nil || !strings.Contains(err.Error(), "city") {
		t.Fatalf("got %q %v", out, err)
	}
	if n := len(td.llm.heard()); n != 0 {
		t.Fatalf("the model was asked %d times", n)
	}
}

// The regression: an IRC room called #protocols looked like a protocol run,
// so protocols couldn't be run from it.
func TestProtocolsRunFromAnIRCRoomNamedLikeOne(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("sunny") })
	writeProtocol(t, td, "weather.yaml", "name: weather\nprompt: Check the weather in Sydney.\n")
	_ = td.loadProtocols()
	if out, err := runProtocolTool(td, "irc:#protocols|tony", "weather"); err != nil || out != "sunny" {
		t.Fatalf("got %q %v", out, err)
	}
	if _, err := runProtocolTool(td, "irc:#protocols|tony#protocol-20260101-120000.000", "weather"); err == nil || !strings.Contains(err.Error(), "already running a protocol") {
		t.Fatalf("a protocol run can't start another: %v", err)
	}
}

// The regression: the persona's voice picks were saved into config.yaml,
// so switching persona rewrote the voice settings, and a persona file's new
// voice never reached a twin that had saved the old one.
func TestPersonaVoiceIsNeverSaved(t *testing.T) {
	r := newVoiceRig(t, nil)
	r.restart()
	r.update(func(c *config.Config) {})
	before, _ := os.ReadFile(config.Path())
	r.update(choosePersona("nyra", "Nyra"))
	r.want("to Nyra", "nyra", "nyra", "", "af_nova")
	after, _ := os.ReadFile(config.Path())
	keep := func(b []byte) []string {
		var out []string
		for _, l := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(l, "persona:") && !strings.HasPrefix(l, "name:") {
				out = append(out, l)
			}
		}
		return out
	}
	if !slices.Equal(keep(before), keep(after)) {
		t.Fatalf("switching persona changed config.yaml beyond the persona:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if m := regexp.MustCompile(`(?m)^    (wake_word|wake_model|voice): \S.*$`).FindString(string(after)); m != "" {
		t.Fatalf("config.yaml holds the persona's %q:\n%s", strings.TrimSpace(m), after)
	}

	file := filepath.Join(persona.Dir(config.Home()), "ada.yaml")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("name: Ada\ncharacter: precise\nvoice: af_bella\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.update(choosePersona("ada", "Ada"))
	r.want("to Ada", "ada", "ada", "", "af_bella")
	saved, _ := os.ReadFile(config.Path())
	if err := os.WriteFile(file, []byte("name: Ada\ncharacter: precise\nvoice: af_nova\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.command(context.Background(), ownerKey, "/reload"); err != nil {
		t.Fatal(err)
	}
	r.want("after /reload", "ada", "ada", "", "af_nova")
	if now, _ := os.ReadFile(config.Path()); string(now) != string(saved) {
		t.Fatalf("/reload changed config.yaml:\n%s", now)
	}
	r.restart()
	r.want("after a restart", "ada", "ada", "", "af_nova")
}

func TestTruncateKeepsUTF8Valid(t *testing.T) {
	s := "héllo wörld — ✓✓ 日本語 🙂"
	for n := 0; n <= len(s)+1; n++ {
		got := truncate(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("truncate(%d) = %q is not valid UTF-8", n, got)
		}
		if n < len(s) && len(strings.TrimSuffix(got, "…")) > n {
			t.Fatalf("truncate(%d) = %q is longer than asked", n, got)
		}
	}
}

// The regression: on a host with no owner chat and no desktop, every health
// round tried the notice again and logged a warning.
func TestSkippedFilesWithNowhereToTellAreNotRetried(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	tries := 0
	var mu sync.Mutex
	prev := desktopNotify
	desktopNotify = func(_, _ string) error { mu.Lock(); tries++; mu.Unlock(); return errors.New("no desktop") }
	t.Cleanup(func() { desktopNotify = prev })
	td.chmu.Lock()
	delete(td.channels, "telegram") // no owner chat
	td.chmu.Unlock()
	td.noticeProblemFiles("protocol", []string{"broken.yaml (bad)"})
	td.noticeProblemFiles("protocol", []string{"broken.yaml (bad)"}) // the next health round
	mu.Lock()
	defer mu.Unlock()
	if tries != 1 {
		t.Fatalf("tried to tell %d times", tries)
	}
	v, _ := td.store.Get(context.Background(), problemFilesKey)
	if !strings.Contains(v, "broken.yaml") {
		t.Fatalf("the problem should be recorded as told, got %q", v)
	}
}
