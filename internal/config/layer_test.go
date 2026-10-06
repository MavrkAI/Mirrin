package config

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// home points the package at a fresh twin home.
func home(t *testing.T) {
	t.Helper()
	t.Setenv("MIRRIN_HOME", t.TempDir())
}

func saved(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// start is a config a person could have: defaults with WhatsApp off (it
// needs an owner to be valid).
func start() *Config {
	c := Default()
	c.Channels.WhatsApp.Enabled = false
	return c
}

// writeLegacy writes cfg the way releases before layering did: every field.
func writeLegacy(t *testing.T, cfg *Config) {
	t.Helper()
	b, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The regression: Save wrote every field, so defaults tuned in later
// releases (the wake threshold went 0.5 → 0.35 → 0.25) never reached anyone
// who had saved a setting.
func TestSaveKeepsOnlyWhatTheOwnerSet(t *testing.T) {
	home(t)
	c := start()
	c.Channels.Telegram.Enabled, c.Channels.Telegram.Owner = true, "123"
	c.LLM.Effort = "medium"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	s := saved(t)
	for _, want := range []string{"config_version: 2", "effort: medium", "owner: \"123\"", "provider: anthropic", "model: claude-opus-5", "name: Mirrin"} {
		if !strings.Contains(s, want) {
			t.Errorf("saved config should have %q:\n%s", want, s)
		}
	}
	for _, not := range []string{"wake_threshold", "whisper_model", "data_dir", "max_tokens", "history_turns", "ollama", "irc"} {
		if strings.Contains(s, not) {
			t.Errorf("saved config should leave %q to the defaults:\n%s", not, s)
		}
	}
	if !strings.HasPrefix(s, "# Mirrin settings.") {
		t.Errorf("want the header first:\n%s", s)
	}
}

func TestLayeredConfigLoadsAsItWasSaved(t *testing.T) {
	yes := false
	for name, change := range map[string]func(c *Config){
		"defaults":   func(*Config) {},
		"a provider": func(c *Config) { c.LLM.Provider, c.LLM.Model = "openai", "gpt-4o" },
		"remembered": func(c *Config) {
			c.LLM.Providers["openai"] = ProviderConfig{APIKeyEnv: "OPENAI_API_KEY", Model: "gpt-4o"}
		},
		"new provider": func(c *Config) { c.LLM.Providers["custom"] = ProviderConfig{BaseURL: "http://x:1/v1", Model: "m"} },
		"lists": func(c *Config) {
			c.Skills.System.AllowedDirs = []string{"/tmp", "~/work"}
			c.Autonomy.AlwaysAsk = []string{"send_email"}
		},
		"no dirs": func(c *Config) { c.Skills.System.AllowedDirs = []string{} },
		"voice": func(c *Config) {
			c.Channels.Voice.Enabled, c.Channels.Voice.Mode, c.Channels.Voice.WakeThreshold = true, "wake", 0.4
			c.Channels.Voice.Acknowledge = &yes
		},
		"mcp": func(c *Config) {
			c.MCP.Servers = []MCPServer{{Name: "fs", Command: "npx", Args: []string{"-y", "fs"}, Env: map[string]string{"A": "1"}}}
		},
		"weather":  func(c *Config) { c.UI.Latitude, c.UI.Longitude = -37.81, 144.96 },
		"timezone": func(c *Config) { c.User.Timezone = "Asia/Tokyo" },
		"paths":    func(c *Config) { c.DataDir = "/srv/mirrin-data" },
	} {
		t.Run(name, func(t *testing.T) {
			home(t)
			c := start()
			change(c)
			if err := c.Save(); err != nil {
				t.Fatal(err)
			}
			got, err := Load()
			if err != nil {
				t.Fatalf("load: %v\n%s", err, saved(t))
			}
			want := c.Clone()
			want.expand()
			g, _ := yaml.Marshal(got)
			w, _ := yaml.Marshal(want)
			if string(g) != string(w) {
				t.Fatalf("loaded config differs from the saved one\nsaved file:\n%s\ngot:\n%s\nwant:\n%s", saved(t), g, w)
			}
		})
	}
}

func TestOldFullDumpGetsImprovedDefaultsAndKeepsChoices(t *testing.T) {
	home(t)
	old := start()
	old.Channels.Voice.WakeThreshold = 0.5 // the default two releases ago
	old.LLM.Effort = "low"                 // the owner's choice
	old.Channels.Telegram.Enabled, old.Channels.Telegram.Owner = true, "42"
	old.LLM.Providers["openai"] = ProviderConfig{APIKeyEnv: "OPENAI_API_KEY", Model: "gpt-4o"}
	writeLegacy(t, old)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Channels.Voice.WakeThreshold != Default().Channels.Voice.WakeThreshold {
		t.Fatalf("a stale default should give way, got %v", c.Channels.Voice.WakeThreshold)
	}
	if c.LLM.Effort != "low" || c.Channels.Telegram.Owner != "42" || c.LLM.Providers["openai"].Model != "gpt-4o" {
		t.Fatalf("the owner's choices must survive: %+v", c.LLM)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	s := saved(t)
	if !strings.Contains(s, "config_version: 2") || strings.Contains(s, "wake_threshold") || !strings.Contains(s, "effort: low") ||
		!strings.Contains(s, "gpt-4o") || strings.Contains(s, "whisper_model") {
		t.Fatalf("the first save after an upgrade keeps only what was set:\n%s", s)
	}
	again, err := Load()
	if err != nil || again.LLM.Effort != "low" || again.Channels.Telegram.Owner != "42" {
		t.Fatalf("reload: %v %+v", err, again)
	}
}

// The default persona was MAVRK (said Maverick) before he was Mirrin. A full
// dump from then names him the old way, which was never the owner's choice.
func TestOldFullDumpFromBeforeMirrinGetsMirrin(t *testing.T) {
	home(t)
	old := start()
	old.Name, old.Persona, old.Channels.Voice.WakeWord = "MAVRK", "mavrk", "maverick"
	writeLegacy(t, old)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Mirrin" || c.Persona != "mirrin" || c.Channels.Voice.WakeWord != "mirrin" {
		t.Fatalf("name %q, persona %q, wake word %q", c.Name, c.Persona, c.Channels.Voice.WakeWord)
	}
}

func TestAWakeThresholdTheOwnerChoseIsKept(t *testing.T) {
	home(t)
	old := start()
	old.Channels.Voice.WakeThreshold = 0.42
	writeLegacy(t, old)
	c, _ := Load()
	if c.Channels.Voice.WakeThreshold != 0.42 {
		t.Fatalf("not a past default, so it's the owner's: got %v", c.Channels.Voice.WakeThreshold)
	}
}

func TestSettingsTheOwnerNamedStayNamed(t *testing.T) {
	home(t)
	if err := os.WriteFile(Path(), []byte("config_version: 2\nllm:\n  effort: high\n  model: claude-opus-5\nchannels:\n  whatsapp:\n    enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.Name = "Juniper"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if s := saved(t); !strings.Contains(s, "effort: high") || !strings.Contains(s, "name: Juniper") {
		t.Fatalf("a setting written in the file is the owner's, default or not:\n%s", s)
	}
}

func TestAChangeBackToTheDefaultIsKept(t *testing.T) {
	home(t)
	c := start()
	c.LLM.Effort = "medium"
	_ = c.Save()
	c, _ = Load()
	c.LLM.Effort = Default().LLM.Effort // chosen from the menu
	_ = c.Save()
	if s := saved(t); !strings.Contains(s, "effort: "+Default().LLM.Effort) {
		t.Fatalf("a choice made now is the owner's even if it matches today's default:\n%s", s)
	}
}

func TestChannelsTheOwnerTurnedOnStayOn(t *testing.T) {
	home(t)
	c := start()
	c.Channels.WhatsApp.Enabled, c.Channels.WhatsApp.Owner = true, "+61400000000"
	_ = c.Save()
	if s := saved(t); !strings.Contains(s, "whatsapp:\n    enabled: true") {
		t.Fatalf("a channel that's on stays on whatever the default becomes:\n%s", s)
	}
}

// The regression: setup wrote the install-time zone into the config, so the
// twin kept home time on every trip.
func TestInstallTimeZoneFollowsTheSystemAfterUpgrade(t *testing.T) {
	home(t)
	t.Setenv("TZ", "Europe/Berlin")
	old := start()
	old.User.Timezone = "Europe/Berlin"
	writeLegacy(t, old)
	if c, _ := Load(); c.User.Timezone != "Local" {
		t.Fatalf("the zone written at install is the system's, so it follows the system: got %q", c.User.Timezone)
	}

	old.User.Timezone = "Asia/Tokyo"
	writeLegacy(t, old)
	if c, _ := Load(); c.User.Timezone != "Asia/Tokyo" {
		t.Fatalf("a zone other than the system's may be pinned, so it stays: got %q", c.User.Timezone)
	}

	if err := os.WriteFile(Path(), []byte("config_version: 2\nuser:\n  timezone: Europe/Berlin\nllm:\n  model: claude-opus-5\nchannels:\n  whatsapp:\n    enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _ := Load(); c.User.Timezone != "Europe/Berlin" {
		t.Fatalf("a zone set in a layered config is the owner's pin: got %q", c.User.Timezone)
	}
}

// The regression: trust settings that matched the defaults of the day were
// left out, so a release that loosened one of those defaults (writes on
// auto, shell on, a higher spending limit) would have loosened it silently
// for everyone already running the twin.
func TestTrustSettingsAreSavedEvenAtTheirDefaults(t *testing.T) {
	home(t)
	if err := start().Save(); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Autonomy map[string]any `yaml:"autonomy"`
		Spending map[string]any `yaml:"spending"`
		API      map[string]any `yaml:"api"`
		Skills   struct {
			System map[string]any `yaml:"system"`
			Web    map[string]any `yaml:"web"`
		} `yaml:"skills"`
	}
	s := saved(t)
	if err := yaml.Unmarshal([]byte(s), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"read", "write", "dangerous", "always_allow", "always_ask"} {
		if _, ok := got.Autonomy[k]; !ok {
			t.Errorf("autonomy.%s should be saved:\n%s", k, s)
		}
	}
	for _, k := range []string{"per_action_limit", "monthly_limit"} {
		if _, ok := got.Spending[k]; !ok {
			t.Errorf("spending.%s should be saved:\n%s", k, s)
		}
	}
	if _, ok := got.Skills.System["allow_shell"]; !ok {
		t.Errorf("skills.system.allow_shell should be saved:\n%s", s)
	}
	if _, ok := got.Skills.System["allowed_dirs"]; !ok {
		t.Errorf("skills.system.allowed_dirs should be saved:\n%s", s)
	}
	if _, ok := got.Skills.Web["allow_hosts"]; !ok {
		t.Errorf("skills.web.allow_hosts should be saved, even empty:\n%s", s)
	}
	if _, ok := got.API["listen"]; !ok {
		t.Errorf("api.listen should be saved:\n%s", s)
	}
	c, err := Load()
	if err != nil || c.Autonomy.Write != "ask" || c.Spending.PerActionLimit != Default().Spending.PerActionLimit {
		t.Fatalf("they load as saved: %v %+v", err, c.Autonomy)
	}
}

// Settings other groups added that mark a trust boundary are guarded too:
// how long the activity log and its quotes are kept (a later default that
// kept them longer must not reach an owner silently), and whether the
// WhatsApp session leaves the machine in a backup.
func TestPrivacySettingsAddedLaterAreGuarded(t *testing.T) {
	home(t)
	c := start()
	c.Backup.Recipient = "age1example"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Retention map[string]any `yaml:"retention"`
		Backup    map[string]any `yaml:"backup"`
	}
	s := saved(t)
	if err := yaml.Unmarshal([]byte(s), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"audit_days", "trim_after_days"} {
		if v, ok := got.Retention[k]; !ok || v == 0 {
			t.Errorf("retention.%s should be saved at its default:\n%s", k, s)
		}
	}
	if v, ok := got.Backup["sessions"]; !ok || v != false {
		t.Errorf("backup.sessions should be saved, off:\n%s", s)
	}
	for _, p := range []string{"usage.monthly_budget", "skills.browser.keep_screenshots_days", "skills.browser.screenshots_max_mb"} {
		if isGuarded(p) {
			t.Errorf("%s only informs or means today's default at zero; it isn't a trust setting", p)
		}
	}
}

// The regression: readDisk deferred expanding the config it started with, so
// when the file on disk didn't fit the settings and it fell back to the
// defaults, those were compared unexpanded ("~" against a home path).
func TestAFileThatDoesntFitStillComparesExpanded(t *testing.T) {
	home(t)
	if err := os.WriteFile(Path(), []byte("config_version: 2\nllm: [not, a, map]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := readDisk()
	want := Default()
	want.expand()
	if len(d.cfg.Skills.System.AllowedDirs) == 0 || d.cfg.Skills.System.AllowedDirs[0] != want.Skills.System.AllowedDirs[0] {
		t.Fatalf("the fallback defaults should be expanded like any other: %v", d.cfg.Skills.System.AllowedDirs)
	}
}

// Every saved config points at config.example.yaml for the settings with
// notes. It loads as a config, and it has the ones added since the last
// release that decide what the twin may reach or keep (browser merged with
// security, observability, backup): the one allow_hosts list fetch_url and
// the browser share, how long screenshots and the activity log are kept,
// and the backup section.
func TestTheExampleConfigHasTheNewSettings(t *testing.T) {
	home(t)
	b, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatalf("config.example.yaml doesn't load: %v", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	namedPaths(doc.Content[0], "", have)
	for _, p := range []string{"skills.web.allow_hosts", "skills.browser.keep_screenshots_days", "skills.browser.screenshots_max_mb",
		"retention.audit_days", "retention.trim_after_days", "usage.monthly_budget", "api.remote", "user.timezone", "user.quiet_hours"} {
		if !have[p] {
			t.Errorf("config.example.yaml lacks %s", p)
		}
	}
	for _, key := range []string{"backup:", "recipient:", "sessions:"} {
		if !strings.Contains(string(b), key) {
			t.Errorf("config.example.yaml doesn't show the backup section's %s", key)
		}
	}
}
