package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/approvals"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/skills/calendar"
	gauth "github.com/MavrkAI/Mirrin/internal/skills/google"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// The regression: pause lived in memory only, so a restart (launchd, a
// crash, the menu's Restart) lifted it without a word.
func TestPauseSurvivesARestart(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("hello") })
	td.SetPaused(true)
	cfg := td.Config()

	again := func() *Daemon {
		d, err := New(&cfg, Options{Headless: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.store.Close() })
		return d
	}
	d2 := again()
	if !d2.paused.Load() || !d2.Status(context.Background()).Paused {
		t.Fatal("a restarted twin should still be paused")
	}
	reply, err := d2.message(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "owner", Sender: "owner", Text: "hi", IsOwner: true}, agent.Events{})
	if err != nil || reply != d2.pausedReply(true) {
		t.Fatalf("a paused twin answers with the paused reply, got %q %v", reply, err)
	}
	d2.SetPaused(false)
	if d3 := again(); d3.paused.Load() {
		t.Fatal("resumed means resumed after the next restart too")
	}
}

// The regression: "Local" was Go's zone as of process start, so a twin that
// had flown kept telling the model the time at home.
func TestTheModelIsToldTheZoneTheOwnerIsIn(t *testing.T) {
	t.Setenv("TZ", "Europe/Paris")
	var system string
	var mu sync.Mutex
	td := newTestDaemon(t, func(_ string, req llm.Request) llm.Response {
		mu.Lock()
		system = req.SystemVolatile
		mu.Unlock()
		return say("ok")
	})
	td.owner(t, "what time is it?")
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(system, "(Europe/Paris)") {
		t.Fatalf("want the time in Europe/Paris, got %q", system)
	}
}

func TestRemindersFollowTheZoneTheOwnerMovesTo(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	td.zoneMoved(tokyo)
	in, _ := json.Marshal(map[string]string{"when": "2030-10-01 09:00", "text": "call mum"})
	out, err := td.agent.Tools().Run(context.Background(), "set_reminder", tools.Call{ChatKey: ownerKey, Input: in})
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := td.store.PendingReminders(context.Background(), ownerKey)
	if len(rs) != 1 || !rs[0].DueAt.Equal(time.Date(2030, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("\"09:00\" in Tokyo is 00:00 UTC; got %v (%s)", rs, out)
	}
}

// The regression: the ambient screen had no weather unless coordinates were
// typed into YAML.
func TestWeatherComesFromTheTimeZonesCity(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Query().Get("latitude")+","+r.URL.Query().Get("longitude"))
		mu.Unlock()
		fmt.Fprint(w, `{"current":{"temperature_2m":14.2,"weather_code":0}}`)
	}))
	defer srv.Close()
	old := weatherURL
	weatherURL = srv.URL
	defer func() { weatherURL = old }()
	fresh := func() {
		weatherCache.Lock()
		weatherCache.w = nil
		weatherCache.Unlock()
	}
	t.Cleanup(fresh)

	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	set := func(f func(c *config.Config)) {
		td.cmu.Lock()
		f(td.cfg)
		td.cmu.Unlock()
		fresh()
	}
	set(func(c *config.Config) { c.User.Timezone = "Europe/London" })
	w := td.weather(context.Background())
	if w == nil || w.Summary != "clear in London" || asked[len(asked)-1] != "51.5100,-0.1300" {
		t.Fatalf("want London's weather, got %+v from %v", w, asked)
	}
	set(func(c *config.Config) { c.User.Timezone = "US/Pacific" })
	if w := td.weather(context.Background()); w == nil || w.Summary != "clear in Los Angeles" {
		t.Fatalf("an old zone name should still find its city, got %+v", w)
	}
	set(func(c *config.Config) { c.UI.Latitude, c.UI.Longitude = -37.8, 145 })
	if w := td.weather(context.Background()); w == nil || w.Summary != "clear" || asked[len(asked)-1] != "-37.8000,145.0000" {
		t.Fatalf("coordinates in the settings win, got %+v from %v", w, asked)
	}
	off := false
	set(func(c *config.Config) { c.UI.Weather = &off })
	n := len(asked)
	if w := td.weather(context.Background()); w != nil || len(asked) != n {
		t.Fatalf("weather turned off asks nobody, got %+v", w)
	}
	set(func(c *config.Config) {
		c.UI.Weather, c.UI.Latitude, c.UI.Longitude, c.User.Timezone = nil, 0, 0, "Etc/GMT+3"
	})
	if w := td.weather(context.Background()); w != nil {
		t.Fatalf("a zone that isn't a place has no weather, got %+v", w)
	}
}

// The regression: a model id the provider retired failed every turn. The
// twin now thinks with the provider's default, asks once, and saves the
// change only with the owner's yes.
func TestRetiredModelIsReplacedForNowAndKeptOnYes(t *testing.T) {
	const retired = "claude-3-opus-20240229"
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
	defer srv.Close()
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)

	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("unused") })
	td.cmu.Lock()
	td.cfg.LLM.Provider, td.cfg.LLM.Model = "anthropic", retired
	cfg := *td.cfg
	td.cmu.Unlock()
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	p, err := buildModel(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	td.agent.SetProvider(p)

	if reply := td.owner(t, "hello?"); reply != "still here" {
		t.Fatalf("the twin should answer on the default model, got %q", reply)
	}
	notice := td.ch.next(t)
	want := fmt.Sprintf("Anthropic has retired %s, the model I was set to think with, so I'm using %s for now. Shall I keep %s? Reply \"yes ", retired, llm.DefaultModel("anthropic"), llm.DefaultModel("anthropic"))
	if !strings.HasPrefix(notice, want) {
		t.Fatalf("want %q…, got %q", want, notice)
	}
	if disk, _ := config.Load(); disk.LLM.Model != retired {
		t.Fatalf("nothing is saved before the owner says yes, got %q", disk.LLM.Model)
	}
	td.owner(t, "hello again")
	select {
	case again := <-td.ch.out:
		t.Fatalf("asked twice: %q", again)
	case <-time.After(200 * time.Millisecond):
	}

	td.owner(t, "yes")
	disk, err := config.Load()
	if err != nil || disk.LLM.Model != llm.DefaultModel("anthropic") {
		t.Fatalf("a yes saves the stand-in, got %q (%v)", disk.LLM.Model, err)
	}
	if got := td.Config().LLM.Model; got != llm.DefaultModel("anthropic") {
		t.Fatalf("the live settings follow, got %q", got)
	}
}

func TestKeepModelChangesNothingOnceTheSettingsMovedOn(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	in, _ := json.Marshal(map[string]string{"provider": "anthropic", "from": "claude-2.1", "to": "claude-opus-5"})
	out, err := td.agent.Tools().Run(context.Background(), "keep_model", tools.Call{ChatKey: ownerKey, Input: in})
	if err != nil || !strings.HasPrefix(out, "Nothing to change") {
		t.Fatalf("got %q %v", out, err)
	}
}

// The regression: keep_model saved any model it was given, so a prompt
// injection (a web page, an email) or a slip of the model could switch the
// twin to an arbitrary, perhaps expensive, model with no retirement at all.
func TestKeepModelOnlyReplacesARetiredModelWithTheDefault(t *testing.T) {
	const old = "claude-3-opus-20240229"
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	td.cmu.Lock()
	td.cfg.LLM.Provider, td.cfg.LLM.Model = "anthropic", old
	td.cmu.Unlock()
	keep := func(to string) (string, error) {
		in, _ := json.Marshal(map[string]string{"provider": "anthropic", "from": old, "to": to})
		return td.agent.Tools().Run(context.Background(), "keep_model", tools.Call{ChatKey: ownerKey, Input: in})
	}
	if out, err := keep("gpt-x"); err == nil {
		t.Fatalf("keep_model must refuse a model that isn't the provider's default, got %q", out)
	}
	if out, err := keep(llm.DefaultModel("anthropic")); err == nil {
		t.Fatalf("with nothing retired there's nothing to replace, got %q", out)
	}
	if got := td.Config().LLM.Model; got != old {
		t.Fatalf("nothing should have changed, the settings say %q", got)
	}
	tool, _ := td.agent.Tools().Get("keep_model")
	loose := approvals.New(config.Autonomy{Read: "auto", Write: "auto", Dangerous: "ask"})
	if loose.Decide(tool) != approvals.Ask {
		t.Fatal("even with writes on auto, changing the model asks the owner")
	}

	_ = td.store.Set(context.Background(), retiredKey, "anthropic/"+old) // the owner was told it was retired
	if out, err := keep(llm.DefaultModel("anthropic")); err != nil || !strings.HasPrefix(out, "Saved") {
		t.Fatalf("the retired model's replacement is saved, got %q %v", out, err)
	}
}

// The regression: after a move to Tokyo, reminders followed but the calendar
// still read "09:00" in the zone the twin started in, and invited people to
// the wrong hour.
func TestCalendarFollowsTheZoneTheOwnerMovesTo(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	dir := t.TempDir()
	creds, token := filepath.Join(dir, "creds.json"), filepath.Join(dir, "token.json")
	_ = os.WriteFile(creds, []byte(`{"installed":{"client_id":"id","client_secret":"secret","redirect_uris":["http://localhost"],"auth_uri":"https://accounts.example/auth","token_uri":"https://accounts.example/token"}}`), 0o600)
	_ = os.WriteFile(token, []byte(`{"access_token":"tok","token_type":"Bearer","expiry":"2099-01-01T00:00:00Z"}`), 0o600)
	td.cmu.Lock()
	td.cfg.Skills.Calendar.Enabled, td.cfg.Skills.Calendar.CredentialsFile, td.cfg.Skills.Calendar.TokenFile = true, creds, token
	cal := td.cfg.Skills.Calendar
	td.cmu.Unlock()
	melbourne, _ := time.LoadLocation("Australia/Melbourne")
	td.agent.Tools().Register(calendar.New(cal, melbourne).Tools()...) // as at start, at home
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	td.zoneMoved(tokyo)

	var mu sync.Mutex
	var start string
	google := roundTrip(func(r *http.Request) (*http.Response, error) {
		var ev struct {
			Start struct{ DateTime string } `json:"start"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &ev)
		mu.Lock()
		start = ev.Start.DateTime
		mu.Unlock()
		body := `{"id":"e1","summary":"dentist","start":{"dateTime":"2030-10-01T09:00:00+09:00"}}`
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	gauth.NewAuth(cal).Transport = google // the sign-in the calendar shares (internal/skills/google): nothing leaves the test
	ctx := context.Background()
	in, _ := json.Marshal(map[string]string{"title": "dentist", "start": "2030-10-01 09:00"})
	if out, err := td.agent.Tools().Run(ctx, "create_event", tools.Call{ChatKey: ownerKey, Input: in}); err != nil {
		t.Fatalf("create_event: %v (%s)", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	got, err := time.Parse(time.RFC3339, start)
	if err != nil || !got.Equal(time.Date(2030, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("\"09:00\" in Tokyo is 00:00 UTC; the event starts %q", start)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The regression: the zone code took "auto" and "local" for the system's
// zone, but the daemon wouldn't start with them: "unknown time zone auto".
func TestEverySpellingOfTheSystemsZoneStarts(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	for _, tz := range []string{"auto", "local", "LOCAL", " Local ", ""} {
		cfg := td.Config()
		cfg.User.Timezone = tz
		d, err := New(&cfg, Options{Headless: true})
		if err != nil {
			t.Fatalf("timezone %q: %v", tz, err)
		}
		d.store.Close()
	}
}

// Everything that reads "today" or a clock time goes by the zone the twin
// is in now (d.location, which follows the system as a laptop travels),
// not the one the process started in: the nightly backup's 03:30, what the
// model has cost today, and the times /audit shows. Before, removing any of
// that wiring broke no test.
func TestTheZoneTheTwinIsInReachesBackupsSpendAndTheAuditLog(t *testing.T) {
	far := "Pacific/Kiritimati" // UTC+14: a different date from most of the world
	if time.Now().In(mustZone(t, far)).Format("2006-01-02") == time.Now().Format("2006-01-02") {
		far = "Etc/GMT+12" // UTC-12
	}
	t.Setenv("TZ", far)
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	here := td.location()
	if here.String() != far {
		t.Fatalf("the twin keeps %s, want %s", here, far)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	td.startBackup(ctx)
	if z := td.backups.Zone(); z == nil || z.String() != far {
		t.Fatalf("the nightly backup keeps time in %v", z)
	}
	today := time.Now().In(here).Format(memory.DayFormat)
	if err := td.store.RecordUsage(ctx, today, "claude-opus-5", "chat", llm.Tokens{Input: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if sp, err := td.Spend(ctx); err != nil || sp.Today <= 0 {
		t.Fatalf("today's spend read in another zone: %+v %v", sp, err)
	}
	td.store.Audit(ctx, "zone.test", "", "marker")
	out, err := td.command(ctx, ownerKey, "/audit")
	now := time.Now().In(here)
	if err != nil || !(strings.Contains(out, now.Format("02 Jan 15:04")) || strings.Contains(out, now.Add(-time.Minute).Format("02 Jan 15:04"))) {
		t.Fatalf("/audit times aren't in %s: %q", far, out)
	}
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
