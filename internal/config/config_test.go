package config

import (
	"os"
	"slices"
	"testing"
)

func TestSecretsRoundTripAndEnvironmentWins(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "from-shell")
	if err := SaveSecrets(map[string]string{"OPENAI_API_KEY": `sk-"quoted" key`, "GEMINI_API_KEY": "saved", "EMPTY": ""}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(SecretsPath())
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("secrets file must be 0600: %v %v", st, err)
	}
	if got := Secret("OPENAI_API_KEY"); got != `sk-"quoted" key` {
		t.Fatalf("OPENAI_API_KEY = %q", got)
	}
	if got := Secret("GEMINI_API_KEY"); got != "from-shell" {
		t.Fatalf("an exported key must win over a saved one, got %q", got)
	}
	vals, _ := ReadSecrets()
	if _, ok := vals["EMPTY"]; ok {
		t.Fatal("empty values should not be saved")
	}
	// Merging keeps what's there.
	if err := SaveSecrets(map[string]string{"ZULIP_API_KEY": "z"}); err != nil {
		t.Fatal(err)
	}
	if vals, _ := ReadSecrets(); vals["OPENAI_API_KEY"] == "" || vals["ZULIP_API_KEY"] != "z" {
		t.Fatalf("merge lost a secret: %v", vals)
	}
}

func TestSecretsStayOutOfTheEnvironment(t *testing.T) {
	// Programs mirrin starts (MCP servers, pack tools, the shell) inherit its
	// environment, so saved keys are looked up, not exported.
	t.Setenv("MIRRIN_HOME", t.TempDir())
	for _, k := range []string{"OPENAI_API_KEY", "TELEGRAM_BOT_TOKEN", "MIRRIN_EMAIL_PASSWORD", "ELEVENLABS_API_KEY"} {
		t.Setenv(k, "")
	}
	if err := SaveSecrets(map[string]string{"OPENAI_API_KEY": "sk-saved", "TELEGRAM_BOT_TOKEN": "1:tg", "MIRRIN_EMAIL_PASSWORD": "pw", "ELEVENLABS_API_KEY": "el"}); err != nil {
		t.Fatal(err)
	}
	if err := LoadSecrets(); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"OPENAI_API_KEY", "TELEGRAM_BOT_TOKEN", "MIRRIN_EMAIL_PASSWORD", "ELEVENLABS_API_KEY"} {
		if v := os.Getenv(k); v != "" {
			t.Errorf("%s was exported, so every child process gets it", k)
		}
	}
	if Secret("ELEVENLABS_API_KEY") != "el" {
		t.Error("the saved ElevenLabs key isn't found")
	}
	c := Default()
	c.LLM.Providers = map[string]ProviderConfig{} // an older config that names no variable
	c.Skills.Email.PasswordEnv = "MIRRIN_EMAIL_PASSWORD"
	if c.ProviderKey("openai") != "sk-saved" || c.TelegramToken() != "1:tg" || c.EmailPassword() != "pw" {
		t.Fatalf("saved secrets not found: %q %q %q", c.ProviderKey("openai"), c.TelegramToken(), c.EmailPassword())
	}
	if c.ProviderKey("ollama") != "" {
		t.Fatal("ollama has no key to find")
	}
}

func TestSecretEnvsCoversEveryConfiguredKey(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	c := Default()
	c.LLM.Providers["custom"] = ProviderConfig{APIKeyEnv: "MY_GATEWAY_KEY"}
	// An MCP server's "$NAME" (procenv.Expand) must reach the installed service too.
	c.MCP.Servers = []MCPServer{{Name: "gh", Command: "gh-mcp", Env: map[string]string{"GITHUB_TOKEN": "$GITHUB_TOKEN", "AWS": "${MY_AWS}", "LOG_LEVEL": "debug"}}}
	got := c.SecretEnvs()
	if slices.Contains(got, "debug") || slices.Contains(got, "LOG_LEVEL") {
		t.Errorf("a literal env value is not a variable to save: %v", got)
	}
	for _, want := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "MY_GATEWAY_KEY", "GITHUB_TOKEN", "MY_AWS",
		"TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "MATRIX_ACCESS_TOKEN",
		"MATTERMOST_TOKEN", "IRC_PASSWORD", "ZULIP_API_KEY", "MIRRIN_EMAIL_PASSWORD", "TWILIO_AUTH_TOKEN", "ELEVENLABS_API_KEY"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
}

func TestCloneIsDeep(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	c := Default()
	cp := c.Clone()
	cp.LLM.Providers["anthropic"] = ProviderConfig{Model: "changed"}
	cp.Skills.System.AllowedDirs[0] = "/elsewhere"
	if c.LLM.Providers["anthropic"].Model == "changed" || c.Skills.System.AllowedDirs[0] == "/elsewhere" {
		t.Fatal("changing the clone changed the original")
	}
	if cp.LLM.Model != c.LLM.Model || cp.DataDir != c.DataDir {
		t.Fatal("clone lost values")
	}
}

func TestRegionFrom(t *testing.T) {
	cases := []struct{ tz, lc, want string }{
		{"America/Los_Angeles", "en_US.UTF-8", "US"},
		{"Europe/London", "en_US.UTF-8", "GB"}, // the zone beats a default LANG
		{"Australia/Perth", "", "AU"},
		{"", "de_DE.UTF-8", "DE"},
		{"Etc/UTC", "C.UTF-8", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := regionFrom(c.tz, c.lc); got != c.want {
			t.Errorf("regionFrom(%q, %q) = %q, want %q", c.tz, c.lc, got, c.want)
		}
	}
	if l := localeFor("GB"); l.Currency != "GBP" || l.PhoneLang != "en-GB" {
		t.Fatalf("GB locale = %+v", l)
	}
	if l := localeFor(""); l.Currency != "USD" {
		t.Fatalf("unknown region should fall back to USD, got %+v", l)
	}
}

func TestNormalizePhone(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"+61 400 000 000", "+61400000000", true},
		{"+1 (415) 555-0123", "+14155550123", true},
		{"0044 7700 900123", "+447700900123", true},
		{"0400 000 000", "0400000000", false}, // no country code
		{"+61 4OO", "+61 4OO", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizePhone(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("NormalizePhone(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
