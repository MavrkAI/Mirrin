package tray

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

func TestPendingTitle(t *testing.T) {
	if got := pendingTitle(0); got != "No approvals waiting" {
		t.Fatal(got)
	}
	if got := pendingTitle(2); got != "2 approval(s) waiting — open the screen" {
		t.Fatal(got)
	}
}

type fakeProvider struct {
	err      error
	accounts string
}

func (f fakeProvider) SetProvider(string) error { return f.err }
func (f fakeProvider) AccountsURL() string      { return f.accounts }

// Regression: choosing a provider without its key only said so; now it
// opens the page where the key goes.
func TestSwitchingToAProviderWithoutAKeyOpensTheModelPage(t *testing.T) {
	var opened, told []string
	open := func(u string) { opened = append(opened, u) }
	tell := func(m string) { told = append(told, m) }
	switchProvider(fakeProvider{err: &llm.KeyMissingError{Provider: "openai", Env: "OPENAI_API_KEY"}, accounts: "http://127.0.0.1:7742/accounts?t=x"}, "openai", open, tell)
	if len(opened) != 1 || opened[0] != "http://127.0.0.1:7742/accounts?t=x#model" || len(told) != 0 {
		t.Fatalf("opened %v, told %v", opened, told)
	}
	opened, told = nil, nil
	switchProvider(fakeProvider{err: errors.New("disk full"), accounts: "http://x/accounts"}, "openai", open, tell)
	if len(opened) != 0 || len(told) != 1 || !strings.Contains(told[0], "disk full") {
		t.Fatalf("opened %v, told %v", opened, told)
	}
	opened, told = nil, nil
	switchProvider(fakeProvider{}, "openai", open, tell)
	if len(opened)+len(told) != 0 {
		t.Fatal("a switch that worked said something")
	}
}

func TestVoiceSetupOpensItsPage(t *testing.T) {
	prev := pageServed
	served := true
	pageServed = func(string) bool { return served }
	t.Cleanup(func() { pageServed = prev })
	var opened, ran []string
	openVoiceSetup(fakePages{}, func(u string) { opened = append(opened, u) }, func(a ...string) { ran = append(ran, strings.Join(a, " ")) })
	if len(opened) != 1 || !strings.HasSuffix(opened[0], "/voice/setup") || len(ran) != 0 {
		t.Fatalf("opened %v, ran %v", opened, ran)
	}
	opened = nil
	openVoiceSetup(nil, func(u string) { opened = append(opened, u) }, func(a ...string) { ran = append(ran, strings.Join(a, " ")) })
	if len(opened) != 0 || len(ran) != 1 || ran[0] != "voice setup" {
		t.Fatalf("without the page: opened %v, ran %v", opened, ran)
	}
	// Regression: with the API on but no such page served yet, the menu
	// opened a dead link where the terminal setup used to run.
	served, ran = false, nil
	openVoiceSetup(fakePages{}, func(u string) { opened = append(opened, u) }, func(a ...string) { ran = append(ran, strings.Join(a, " ")) })
	if len(opened) != 0 || len(ran) != 1 || ran[0] != "voice setup" {
		t.Fatalf("page not served: opened %v, ran %v", opened, ran)
	}
}

func TestPageServedAsksTheAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/voice/setup" {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if !pageServed(srv.URL + "/voice/setup?token=k") {
		t.Fatal("a served page counted as missing")
	}
	if pageServed(srv.URL + "/nope?token=k") {
		t.Fatal("a missing page counted as served")
	}
	srv.Close()
	if pageServed(srv.URL + "/voice/setup") {
		t.Fatal("an API that is down counted as serving")
	}
}

type fakeConfig struct{ cfg config.Config }

func (f *fakeConfig) Config() config.Config { return f.cfg }
func (f *fakeConfig) UpdateConfig(mutate func(*config.Config)) error {
	mutate(&f.cfg)
	return nil
}

func TestTimeZonePinGoesThroughTheBackend(t *testing.T) {
	b := &fakeConfig{cfg: config.Config{User: config.User{Timezone: "Local"}}}
	if got := zoneTitle(b.cfg.User.Timezone, "Europe/Lisbon"); got != "Pin time zone to Europe/Lisbon" {
		t.Fatal(got)
	}
	if err := toggleZone(b, "Europe/Lisbon"); err != nil || b.cfg.User.Timezone != "Europe/Lisbon" {
		t.Fatalf("pin: %v %q", err, b.cfg.User.Timezone)
	}
	if got := zoneTitle(b.cfg.User.Timezone, "Europe/Lisbon"); !strings.Contains(got, "Europe/Lisbon") || !strings.Contains(got, "follow") {
		t.Fatal(got)
	}
	if err := toggleZone(b, "Asia/Tokyo"); err != nil || !config.FollowsSystem(b.cfg.User.Timezone) {
		t.Fatalf("unpin: %v %q", err, b.cfg.User.Timezone)
	}
	b.cfg.User.Timezone = ""
	if err := toggleZone(b, ""); err == nil {
		t.Fatal("pinned an unknown zone")
	}
}

func TestPauseEnds(t *testing.T) {
	loc := time.FixedZone("here", 10*3600)
	now := time.Date(2026, 9, 29, 22, 30, 0, 0, loc)
	if got := pauseEnd("hour", now); !got.Equal(now.Add(time.Hour)) {
		t.Fatal(got)
	}
	if got := pauseEnd("tomorrow", now); !got.Equal(time.Date(2026, 9, 30, 8, 0, 0, 0, loc)) {
		t.Fatal(got)
	}
	// Regression: clicked just after midnight it lasted until the morning
	// after the coming one, about 31 hours.
	late := time.Date(2026, 9, 30, 0, 30, 0, 0, loc)
	if got := pauseEnd("tomorrow", late); !got.Equal(time.Date(2026, 9, 30, 8, 0, 0, 0, loc)) {
		t.Fatal(got)
	}
	if got := pausedState(time.Time{}, now); got != "paused" {
		t.Fatal(got)
	}
	if got := pausedState(now.Add(time.Hour), now); got != "paused until Tue 11:30 PM" && got != "paused until 11:30 PM" {
		t.Fatal(got)
	}
	if got := pausedState(now.Add(10*time.Minute), now); got != "paused until 10:40 PM" {
		t.Fatal(got)
	}
	if got := pausedState(time.Date(2026, 9, 30, 8, 0, 0, 0, loc), now); got != "paused until Wed 8:00 AM" {
		t.Fatal(got)
	}
}

// The menu calls the twin by the persona's name, which a switch changes
// without a restart: no "Talk to Pickoo" left over after picking Nyra.
func TestTheMenuFollowsThePersonasName(t *testing.T) {
	b := &fakeConfig{cfg: config.Config{Name: "Pickoo"}}
	if got := liveName(b, "Mirrin"); got != "Pickoo" {
		t.Fatalf("named %q", got)
	}
	_ = b.UpdateConfig(func(c *config.Config) { c.Name = "Nyra" })
	if got := liveName(b, "Mirrin"); got != "Nyra" {
		t.Fatalf("after the switch, named %q", got)
	}
	_ = b.UpdateConfig(func(c *config.Config) { c.Name = "  " })
	if got := liveName(b, "Mirrin"); got != "Mirrin" {
		t.Fatalf("with no name, named %q", got)
	}
}
