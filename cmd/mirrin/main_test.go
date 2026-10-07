package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/daemon"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/persona"
)

func home(t *testing.T) {
	t.Helper()
	t.Setenv("MIRRIN_HOME", t.TempDir())
	// Logging hands logs/crash.log to the runtime, which keeps it open for
	// good; Windows then can't delete the test's folder. Let go of it first
	// (cleanups run last-registered first, so this runs before the delete).
	t.Cleanup(func() {
		_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
		closeLogs() // and the log files the commands opened
	})
	t.Setenv("ANTHROPIC_CONFIG_DIR", t.TempDir()) // no `ant auth login` profile
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		t.Setenv(k, "")
	}
}

// fakeCheck accepts only the key "good".
func fakeCheck(s llm.ProviderSettings) error {
	if s.APIKey == "good" || s.Provider == "ollama" {
		return nil
	}
	return errors.New(s.Provider + " 401: Incorrect API key provided")
}

func TestSetupChecksTheKeyAndPhoneNumber(t *testing.T) {
	home(t)
	answers := strings.Join([]string{
		"Sam", "Juniper", "2", "", // who
		"c", "", "sk-bad", "good", // OpenAI: skip hint, a rejected key, a working one
		"0400 000 000", "+61 400 000 000", // a local number is refused, then fixed
	}, "\n") + "\n"
	var out strings.Builder
	p := newPrompter(strings.NewReader(answers), &out)
	cfg := firstRunConfig()
	if !setup(p, cfg, false, nil, fakeCheck) {
		t.Fatalf("a working key should count as a model:\n%s", out.String())
	}
	if cfg.User.Name != "Sam" || cfg.Name != "Juniper" || cfg.LLM.Provider != "openai" {
		t.Fatalf("answers not applied: %+v %+v", cfg.User, cfg.LLM)
	}
	if got := cfg.LLM.Providers["openai"].APIKey; got != "good" {
		t.Fatalf("stored key %q, want the one that worked", got)
	}
	if !cfg.Channels.WhatsApp.Enabled || cfg.Channels.WhatsApp.Owner != "+61400000000" {
		t.Fatalf("whatsapp = %+v", cfg.Channels.WhatsApp)
	}
	for _, want := range []string{"platform.openai.com/api-keys", "OpenAI didn't accept the API key", "country code"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSetupWithoutAKeyDoesNotPretend(t *testing.T) {
	home(t)
	p := newPrompter(strings.NewReader("\n\n\n\na\n\n\n\n"), &strings.Builder{})
	cfg := firstRunConfig()
	if setup(p, cfg, false, nil, fakeCheck) {
		t.Fatal("no key entered, yet setup reported a working model")
	}
	if cfg.APIKey() != "" {
		t.Fatal("an empty key was stored")
	}
}

func TestInitResumesAfterZeroConfig(t *testing.T) {
	home(t)
	cfg := firstRunConfig()
	cfg.LLM.Provider, cfg.LLM.Model = "ollama", "qwen3"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	got, existing, err := initialConfig(newPrompter(strings.NewReader(""), &out))
	if err != nil || got == nil || !existing {
		t.Fatalf("init refused an existing config: %v\n%s", err, out.String())
	}
	if got.LLM.Model != "qwen3" {
		t.Fatal("init should start from the current answers")
	}

	// A config that no longer parses can be set aside.
	if err := os.WriteFile(config.Path(), []byte("llm: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, existing, err = initialConfig(newPrompter(strings.NewReader("y\n"), &out))
	if err != nil || got == nil || existing {
		t.Fatalf("start over: %v %v", got, err)
	}
	if _, err := os.Stat(config.Path() + ".bak"); err != nil {
		t.Fatal("the broken config should be kept as .bak")
	}
}

func TestWithConfigWritesNothing(t *testing.T) {
	home(t)
	ran := false
	if err := withConfig(func(cfg *config.Config) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("a command that doesn't think needed a model: %v", err)
	}
	if _, err := os.Stat(config.Path()); err == nil {
		t.Fatal("browsing wrote a config, which would lock `mirrin init` out")
	}
}

func TestFirstRunPicksTheModelFromTheEnvironment(t *testing.T) {
	home(t)
	t.Setenv("OPENAI_API_KEY", "sk-x")
	cfg := firstRunConfig()
	firstRunDefaults(cfg)
	if cfg.LLM.Provider != "openai" || modelProblem(cfg) != nil {
		t.Fatalf("provider %s, problem %v", cfg.LLM.Provider, modelProblem(cfg))
	}
	cfg.LLM.Provider = "gemini"
	var km *llm.KeyMissingError
	if !errors.As(modelProblem(cfg), &km) {
		t.Fatal("gemini without a key should be a problem")
	}
}

func TestServiceMode(t *testing.T) {
	cases := []struct {
		goos           string
		wantTray, have bool
		mode           string
	}{
		{"darwin", true, true, "tray"},
		{"darwin", true, false, "run"}, // a build without cgo has no menu bar to run
		{"darwin", false, true, "run"},
		{"linux", true, true, "run"},
	}
	for _, c := range cases {
		if got := serviceMode(c.goos, c.wantTray, c.have); got != c.mode {
			t.Errorf("serviceMode(%s, %v, %v) = %s, want %s", c.goos, c.wantTray, c.have, got, c.mode)
		}
	}
}

func TestUnknownCommandSuggests(t *testing.T) {
	cases := map[string]string{"doctr": "doctor", "chta": "chat", "prot": "protocols", "bogus": ""}
	for in, want := range cases {
		if got := closest(in, commands); got != want {
			t.Errorf("closest(%q) = %q, want %q", in, got, want)
		}
	}
	var out strings.Builder
	unknownCommand(&out, "doctr")
	if !strings.Contains(out.String(), `"doctr" isn't a command`) || !strings.Contains(out.String(), "mirrin doctor") {
		t.Fatalf("got %q", out.String())
	}
}

func TestDisconnectWithoutAPairing(t *testing.T) {
	home(t)
	var out strings.Builder
	if err := disconnect(&out); err != nil {
		t.Fatalf("raw error instead of a plain answer: %v", err)
	}
	if !strings.Contains(out.String(), "Not connected") {
		t.Fatalf("got %q", out.String())
	}
	if err := config.SaveRemote(config.Remote{Address: "127.0.0.1:1", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := disconnect(&out); err != nil || !strings.Contains(out.String(), "Disconnected") {
		t.Fatalf("err %v, out %q", err, out.String())
	}
}

func TestClientErrorReply(t *testing.T) {
	if got := clientErrorReply(errors.New(`daemon: anthropic 401: {"type":"error"}`)); !strings.HasPrefix(got, "Anthropic didn't accept") {
		t.Fatalf("raw provider error: %q", got)
	}
	if got := clientErrorReply(errors.New("daemon: no such approval")); got != "Something went wrong: no such approval" {
		t.Fatalf("got %q", got)
	}
}

func TestSavingAFreshConfigKeepsTheDetectedModel(t *testing.T) {
	// `mirrin persona use` (or `mirrin voice setup`) as the very first command.
	home(t)
	t.Setenv("OPENAI_API_KEY", "sk-x")
	id := persona.Bundled()[0].ID
	if err := withConfig(func(cfg *config.Config) error { return personaCmd(cfg, []string{"use", id}) }); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.LLM.Provider != "openai" || saved.Persona != id {
		t.Fatalf("saved provider %q persona %q; the twin would start without the model it could use", saved.LLM.Provider, saved.Persona)
	}
	if entries, _ := os.ReadDir(saved.ProtocolsDir); len(entries) == 0 {
		t.Fatal("the first-run example protocols weren't written")
	}
}

func TestSetupKeepsAWhatsAppNumberSavedWithoutPlus(t *testing.T) {
	// whatsapp.go keeps digits only, so an owner can be saved as 61400000000.
	home(t)
	cfg := firstRunConfig()
	cfg.LLM.Provider = "ollama"
	cfg.Channels.WhatsApp.Enabled, cfg.Channels.WhatsApp.Owner = true, "61400000000"
	answers := strings.Repeat("\n", 7) // Enter keeps every answer: four about who, the model, the number
	var out strings.Builder
	setup(newPrompter(strings.NewReader(answers), &out), cfg, true, []string{"qwen3"}, fakeCheck)
	if cfg.Channels.WhatsApp.Owner != "+61400000000" || !cfg.Channels.WhatsApp.Enabled {
		t.Fatalf("owner = %q:\n%s", cfg.Channels.WhatsApp.Owner, out.String())
	}
	if strings.Contains(out.String(), "country code at the front") {
		t.Fatalf("Enter on the saved number was refused:\n%s", out.String())
	}
}

func TestSetupKeepsAPackPersona(t *testing.T) {
	// Re-running `mirrin init` and pressing Enter everywhere changes nothing,
	// a persona from a pack (or the user's own) included.
	home(t)
	cfg := firstRunConfig()
	cfg.LLM.Provider = "ollama"
	cfg.Persona = "starter/butler"
	answers := strings.Repeat("\n", 7)
	var out strings.Builder
	setup(newPrompter(strings.NewReader(answers), &out), cfg, true, []string{"qwen3"}, fakeCheck)
	if cfg.Persona != "starter/butler" {
		t.Fatalf("Enter switched the persona to %q:\n%s", cfg.Persona, out.String())
	}
	// Picking a bundled one still works.
	cfg.Persona = "starter/butler"
	answers = "\n\n1\n" + strings.Repeat("\n", 4)
	setup(newPrompter(strings.NewReader(answers), &out), cfg, true, []string{"qwen3"}, fakeCheck)
	if cfg.Persona != persona.Bundled()[0].ID {
		t.Fatalf("choosing 1 gave %q", cfg.Persona)
	}
}

func TestWakePhrase(t *testing.T) {
	cases := map[string]string{"mirrin": "Hey Mirrin", "": "Hey Mirrin", "hey juniper": "Hey Juniper", "Hey Ava": "Hey Ava", "émile": "Hey Émile"}
	for in, want := range cases {
		if got := wakePhrase(in); got != want {
			t.Errorf("wakePhrase(%q) = %q, want %q", in, got, want)
		}
	}
}

// An import refuses to run under a twin that is running with its local API
// off: the twin's next save would undo it.
func TestImportRefusedWhileATwinRunsWithTheAPIOff(t *testing.T) {
	home(t)
	cfg := firstRunConfig()
	cfg.LLM.Provider = "ollama"
	cfg.Channels.WhatsApp.Enabled = false
	cfg.API.Listen = ""
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if twinRunning() {
		t.Fatal("nothing is running yet")
	}
	d, err := daemon.New(cfg, daemon.Options{Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	err = identityCmd([]string{"import", filepath.Join(t.TempDir(), "twin.tar.gz")})
	if err == nil || !strings.Contains(err.Error(), "your twin is running here") {
		t.Fatalf("import under a running twin: %v", err)
	}
}

// `mirrin version` (which installers, the release smoke test and Homebrew
// run) touches nothing: no home is created, moved or logged to.
func TestVersionTouchesNothing(t *testing.T) {
	if os.Getenv("MIRRIN_TEST_MAIN") != "" {
		os.Args = []string{"mirrin", os.Getenv("MIRRIN_TEST_MAIN")}
		main()
		return
	}
	for _, arg := range []string{"version", "--version", "help"} {
		home := filepath.Join(t.TempDir(), "not-yet")
		cmd := exec.Command(os.Args[0], "-test.run=^TestVersionTouchesNothing$")
		cmd.Env = append(os.Environ(), "MIRRIN_HOME="+home, "MIRRIN_TEST_MAIN="+arg)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", arg, err, out)
		}
		if _, err := os.Stat(home); err == nil {
			t.Fatalf("`mirrin %s` created %s", arg, home)
		}
		if arg != "help" && !strings.Contains(string(out), "mirrin "+version) {
			t.Fatalf("%s printed %q", arg, out)
		}
	}
}
