package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/channels/voice"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/daemon"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/persona"
	"github.com/MavrkAI/Mirrin/internal/protocols"
)

// loadConfig reads config.yaml, or builds first-run defaults in memory when
// there isn't one yet (fresh), with the model this machine can offer. Nothing
// is written.
func loadConfig() (cfg *config.Config, fresh bool, err error) {
	if _, err := os.Stat(config.Path()); err != nil {
		_ = os.MkdirAll(config.Home(), 0o700) // logins and pairing write beside the config
		cfg := firstRunConfig()
		firstRunDefaults(cfg)
		return cfg, true, nil
	}
	cfg, err = config.Load()
	if err != nil {
		return nil, false, fmt.Errorf("%w\nRun `mirrin init` to repair it", err)
	}
	return cfg, false, nil
}

// withConfig runs fn with the config, or with first-run defaults when there is
// none yet, and writes nothing itself. Commands that don't think work before a
// model is set up, and `mirrin init` stays open. A command that saves a fresh
// config (`mirrin voice setup`, `mirrin persona use`) saves the detected model
// with it, and gets the rest of the first-run setup here.
func withConfig(fn func(cfg *config.Config) error) error {
	cfg, fresh, err := loadConfig()
	if err != nil {
		return err
	}
	err = fn(cfg)
	if _, statErr := os.Stat(config.Path()); fresh && statErr == nil {
		if e := os.MkdirAll(cfg.DataDir, 0o700); e == nil {
			_ = protocols.WriteExamples(cfg.ProtocolsDir)
		}
	}
	return err
}

// withModel is withConfig for commands that think. The first time, it picks a
// model from the environment or a running Ollama and writes the config; with
// no model and a terminal, it asks. With degrade (the menu bar and the
// service), a missing model is not fatal: the twin starts and says what's
// missing, rather than exiting where nobody sees why.
func withModel(degrade bool, fn func(cfg *config.Config) error) error {
	cfg, fresh, err := loadConfig()
	if err != nil {
		return err
	}
	problem, answered := modelProblem(cfg), false
	if problem != nil && isTerminal() {
		p := newPrompter(os.Stdin, os.Stdout)
		fmt.Fprintf(p.out, "%s needs a model to think with.\n", cfg.Name)
		if askBrain(p, cfg, detectOllama(cfg), checkModel) {
			problem, answered = nil, true
		}
		fmt.Fprintln(p.out)
	}
	if problem != nil && !degrade {
		if fresh {
			return errNoModel
		}
		return problem
	}
	switch {
	case fresh:
		if err := writeFirstRun(cfg); err != nil {
			return err
		}
	case answered:
		if err := cfg.Save(); err != nil {
			return err
		}
	}
	return fn(cfg)
}

var errNoModel = errors.New("Mirrin needs a model to think with. Get an Anthropic API key at " + llm.KeyURL("anthropic") +
	" and run `mirrin init`, or install Ollama from https://ollama.com and run `ollama pull " + llm.DefaultModel("ollama") + "` to think locally for free.")

// firstRunConfig is Default with what this machine can tell us. The time
// zone stays "Local": the twin follows the system's as the laptop travels,
// where writing today's zone in would pin it to home.
func firstRunConfig() *config.Config {
	cfg := config.Default()
	cfg.Channels.WhatsApp.Enabled = false
	return cfg
}

// firstRunDefaults picks a model without asking: Claude if ANTHROPIC_API_KEY
// is set, else OpenAI or Gemini from their keys, else a running Ollama with a
// model that can chat. Otherwise it leaves Claude, and modelProblem says so.
func firstRunDefaults(cfg *config.Config) {
	switch {
	case config.Secret("ANTHROPIC_API_KEY") != "":
		cfg.LLM.Provider, cfg.LLM.Model = "anthropic", "claude-opus-5"
	case config.Secret("OPENAI_API_KEY") != "":
		cfg.LLM.Provider, cfg.LLM.Model = "openai", "gpt-4.1"
	case config.Secret("GEMINI_API_KEY") != "":
		cfg.LLM.Provider, cfg.LLM.Model = "gemini", "gemini-2.5-pro"
	case llm.AnthropicSignedIn():
		cfg.LLM.Provider, cfg.LLM.Model = "anthropic", "claude-opus-5"
	default:
		if models := detectOllama(cfg); len(models) > 0 {
			cfg.LLM.Provider, cfg.LLM.Model = "ollama", models[0]
		}
	}
}

// writeFirstRun saves a first-run config and the example protocols.
func writeFirstRun(cfg *config.Config) error {
	if err := cfg.Save(); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	_ = protocols.WriteExamples(cfg.ProtocolsDir)
	if problem := modelProblem(cfg); problem != nil {
		fmt.Fprintf(os.Stderr, "First run: saved %s. %s\n", config.Path(), llm.Friendly(problem))
		return nil
	}
	fmt.Fprintf(os.Stderr, "First run: thinking with %s/%s. `mirrin init` asks a few questions to make it yours; the menu bar changes the rest.\n", cfg.LLM.Provider, cfg.LLM.Model)
	return nil
}

// modelProblem says why the configured model can't answer, without calling it.
func modelProblem(cfg *config.Config) error {
	s := providerSettings(cfg, cfg.LLM.Provider)
	if err := llm.MissingKey(s); err != nil {
		return err
	}
	_, err := llm.New(s)
	return err
}

func providerSettings(cfg *config.Config, provider string) llm.ProviderSettings {
	return llm.ProviderSettings{Provider: provider, Model: cfg.ProviderModel(provider), APIKey: cfg.ProviderKey(provider), BaseURL: cfg.ProviderBaseURL(provider)}
}

// modelChecker proves a model answers with these settings.
type modelChecker func(llm.ProviderSettings) error

func checkModel(s llm.ProviderSettings) error {
	p, err := llm.New(s)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return llm.Check(ctx, p)
}

// detectOllama lists up to four local models that can hold a conversation.
func detectOllama(cfg *config.Config) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	models, err := llm.OllamaChatModels(ctx, cfg.ProviderBaseURL("ollama"))
	if err != nil {
		return nil
	}
	if len(models) > 4 {
		models = models[:4]
	}
	return models
}

func isTerminal() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// prompter asks questions on a terminal.
type prompter struct {
	rd  *bufio.Reader
	out io.Writer
}

func newPrompter(in io.Reader, out io.Writer) *prompter {
	return &prompter{rd: bufio.NewReader(in), out: out}
}

// line prints prompt and reads one trimmed line.
func (p *prompter) line(prompt string) string {
	fmt.Fprint(p.out, prompt)
	s, _ := p.rd.ReadString('\n')
	return strings.TrimSpace(s)
}

// ask shows def in brackets and returns it for an empty answer.
func (p *prompter) ask(prompt, def string) string {
	if def != "" {
		prompt = fmt.Sprintf("%s [%s]", prompt, def)
	}
	if s := p.line(prompt + ": "); s != "" {
		return s
	}
	return def
}

func (p *prompter) yes(prompt, def string) bool {
	return strings.HasPrefix(strings.ToLower(p.ask(prompt, def)), "y")
}

var providerChoices = map[string]string{"a": "anthropic", "b": "ollama", "c": "openai", "d": "gemini"}

// askBrain asks which model to think with and for its key, and checks the
// key really works before keeping it. It reports whether cfg now has a model
// that answered.
func askBrain(p *prompter, cfg *config.Config, ollama []string, check modelChecker) bool {
	cur := "a"
	for k, v := range providerChoices {
		if v == cfg.LLM.Provider {
			cur = k
		}
	}
	fmt.Fprintln(p.out, "     a) Claude (best; needs an Anthropic API key)")
	if len(ollama) > 0 {
		fmt.Fprintf(p.out, "     b) A local model via Ollama (private and free; found: %s)\n", strings.Join(ollama, ", "))
	} else {
		fmt.Fprintln(p.out, "     b) A local model via Ollama (private and free; install it from https://ollama.com first)")
	}
	fmt.Fprintln(p.out, "     c) OpenAI    d) Google Gemini")
	custom := cfg.LLM.Provider == "openai-compatible" || cfg.LLM.Provider == "custom"
	if custom {
		fmt.Fprintf(p.out, "     e) Keep %s at %s\n", cfg.LLM.Model, cfg.ProviderBaseURL(cfg.LLM.Provider))
		cur = "e"
	}
	answer := strings.ToLower(p.ask("     Choice", cur))
	if custom && answer == "e" {
		if err := check(providerSettings(cfg, cfg.LLM.Provider)); err != nil {
			fmt.Fprintln(p.out, "     "+llm.Friendly(err))
			return false
		}
		return true
	}
	provider, ok := providerChoices[answer]
	if !ok {
		provider = "anthropic"
	}
	if provider != cfg.LLM.Provider {
		model := cfg.ProviderModel(provider)
		if model == "" {
			model = llm.DefaultModel(provider)
		}
		cfg.LLM.Provider, cfg.LLM.Model = provider, model
	}
	if provider == "ollama" {
		return askOllama(p, cfg, ollama, check)
	}
	return askKey(p, cfg, provider, check)
}

func askOllama(p *prompter, cfg *config.Config, ollama []string, check modelChecker) bool {
	def := cfg.LLM.Model
	if len(ollama) > 0 && !containsModel(ollama, def) {
		def = ollama[0]
	}
	cfg.LLM.Model = p.ask("     Model", def)
	if len(ollama) == 0 {
		fmt.Fprintf(p.out, "     Once Ollama is running, get the model with `ollama pull %s`.\n", cfg.LLM.Model)
		return false
	}
	if err := check(providerSettings(cfg, "ollama")); err != nil {
		fmt.Fprintln(p.out, "     "+llm.Friendly(err))
		return false
	}
	return true
}

func containsModel(list []string, m string) bool {
	for _, x := range list {
		if x == m {
			return true
		}
	}
	return false
}

// askKey asks for a provider's API key until one works, or the person skips.
func askKey(p *prompter, cfg *config.Config, provider string, check modelChecker) bool {
	env := llm.DefaultKeyEnv(provider)
	have := cfg.ProviderKey(provider)
	for tries := 0; tries < 3; tries++ {
		prompt := fmt.Sprintf("     %s API key (from %s)", keyLabel(provider), llm.KeyURL(provider))
		if have != "" {
			prompt += " [Enter keeps the current one]"
		}
		key := p.line(prompt + ": ")
		if key == "" && have != "" {
			key, have = have, "" // if it fails, Enter means skip next time
		}
		if key == "" {
			if tries > 0 {
				return false
			}
			fmt.Fprintf(p.out, "     No key yet? Create one at %s (it takes a minute) and paste it here, or press Enter to skip for now.\n", llm.KeyURL(provider))
			continue
		}
		s := providerSettings(cfg, provider)
		s.APIKey = key
		err := check(s)
		if err == nil || offline(err) {
			if key != config.Secret(env) { // a key from the environment or secrets.env stays there
				if cfg.LLM.Providers == nil {
					cfg.LLM.Providers = map[string]config.ProviderConfig{}
				}
				pc := cfg.LLM.Providers[provider]
				pc.APIKey = key
				cfg.LLM.Providers[provider] = pc
			}
			if err == nil {
				fmt.Fprintln(p.out, "     That key works.")
			} else {
				fmt.Fprintln(p.out, "     I couldn't check it just now (offline?), so I've kept it as it is.")
			}
			return true
		}
		fmt.Fprintln(p.out, "     "+llm.Friendly(err))
	}
	return false
}

func keyLabel(provider string) string {
	switch provider {
	case "openai":
		return "OpenAI"
	case "gemini":
		return "Gemini"
	}
	return "Anthropic"
}

// offline reports a failure to reach the provider at all, as opposed to an
// answer that says no.
func offline(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded)
}

// runInit asks three questions and makes the twin yours. Run again, it starts
// from the current answers, so it also repairs and changes an existing setup.
func runInit() error {
	p := newPrompter(os.Stdin, os.Stdout)
	cfg, existing, err := initialConfig(p)
	if err != nil || cfg == nil {
		return err
	}
	wasOwner := cfg.Channels.WhatsApp.Owner
	thinks := setup(p, cfg, existing, detectOllama(cfg), checkModel)
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	if err := protocols.WriteExamples(cfg.ProtocolsDir); err != nil {
		return err
	}
	fmt.Printf("\nSaved %s\n", config.Path())
	if api.Connect(cfg.API.Listen, cfg.DataDir) != nil {
		fmt.Printf("%s is running now. If it was waiting for a model key, it uses the new one from its next message; for your other answers, restart it (Restart in its menu, or `mirrin service restart`).\n", cfg.Name)
	}

	// WhatsApp pairing, right here.
	if cfg.Channels.WhatsApp.Enabled && cfg.Channels.WhatsApp.Owner != wasOwner && p.yes("Pair WhatsApp now with a QR code? (y/n)", "y") {
		if err := daemon.WhatsAppLogin(context.Background(), cfg); err != nil {
			fmt.Println("WhatsApp pairing skipped:", err, "(run `mirrin whatsapp login` later)")
		}
	}

	if !thinks {
		fmt.Println()
		fmt.Println("Your twin is set up, but it needs a working model before it can talk.")
		fmt.Println("Run `mirrin init` again when you have a key, or once Ollama has a model.")
		return nil
	}
	// First look: the twin introduces itself with something true.
	intro, err := firstLook(cfg)
	switch {
	case err != nil:
		fmt.Println("(first look skipped: " + llm.Friendly(err) + ")")
	case intro != "":
		fmt.Printf("\n%s: %s\n\n", cfg.Name, intro)
		if v := voice.New(cfg.Channels.Voice, cfg.Name, cfg.DataDir); v != nil {
			v.Speak(context.Background(), intro)
		}
	}
	fmt.Println("Next:")
	fmt.Println("  mirrin chat              # text with it")
	fmt.Println("  mirrin voice setup       # hearing and a voice, all offline")
	fmt.Println("  mirrin service install   # keep it running in the menu bar")
	if cfg.Channels.WhatsApp.Enabled {
		fmt.Println("It will check in once a day this week to show you something it can do. Say \"stop nudging\" to end that early.")
	}
	fmt.Printf("Spending limits are in %s (%s per payment, %s a month); change them on the Spending page (menu bar → Spending…).\n",
		cfg.Spending.Currency, money(cfg.Spending.PerActionLimit), money(cfg.Spending.MonthlyLimit))
	return nil
}

func money(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// initialConfig is what init starts from: the current config when there is
// one, else first-run defaults. A config that no longer parses can be set
// aside and started over; nil means the person chose to stop.
func initialConfig(p *prompter) (*config.Config, bool, error) {
	if _, err := os.Stat(config.Path()); err != nil {
		return firstRunConfig(), false, nil
	}
	cfg, err := config.Load()
	if cfg != nil { // loaded; any validation problem gets fixed by the questions
		fmt.Fprintln(p.out, "You already have a twin. Press Enter to keep an answer as it is.")
		fmt.Fprintln(p.out)
		return cfg, true, nil
	}
	fmt.Fprintf(p.out, "%v\n", err)
	if !p.yes("Start over? Your old settings are kept beside the new ones, as config.yaml.bak (y/n)", "n") {
		return nil, false, nil
	}
	if err := os.Rename(config.Path(), config.Path()+".bak"); err != nil {
		return nil, false, err
	}
	return firstRunConfig(), false, nil
}

// setup asks the three questions, starting from cfg's current answers. It
// reports whether the chosen model answered.
func setup(p *prompter, cfg *config.Config, existing bool, ollama []string, check modelChecker) bool {
	if existing {
		fmt.Fprintln(p.out, "Three questions; your twin is already yours.")
	} else {
		fmt.Fprintln(p.out, "Three questions and your twin is yours.")
	}
	fmt.Fprintln(p.out)
	// 1. who
	cfg.User.Name = p.ask("1/3  Your first name", cfg.User.Name)
	cfg.Name = p.ask("     What will you call your twin", cfg.Name)
	personas := persona.Bundled()
	fmt.Fprintln(p.out, "     Personality:")
	cur := "1"
	for i, ps := range personas {
		fmt.Fprintf(p.out, "       %d) %-6s %s\n", i+1, ps.Name, ps.Tagline)
		if ps.ID == cfg.Persona {
			cur = strconv.Itoa(i + 1)
		}
	}
	// A persona of the user's own or from a pack stays an answer, so Enter
	// keeps it rather than switching to the first bundled one.
	_, bundled := persona.Find(personas, cfg.Persona)
	keep := existing && cfg.Persona != "" && !bundled
	if keep {
		cur = strconv.Itoa(len(personas) + 1)
		fmt.Fprintf(p.out, "       %s) %-6s the one you have now\n", cur, cfg.Persona)
	}
	prevPersona := cfg.Persona
	if i, err := strconv.Atoi(p.ask("     Choice", cur)); err == nil && i >= 1 && i <= len(personas) {
		cfg.Persona = personas[i-1].ID
	} else if keep && err == nil && i == len(personas)+1 {
		// kept as it is
	} else if len(personas) > 0 && !existing {
		cfg.Persona = personas[0].ID
	}
	address := cfg.User.Honorific
	if chosen, ok := persona.Find(personas, cfg.Persona); ok && (address == "" || !existing || cfg.Persona != prevPersona) {
		address = chosen.Address // empty is the persona's own way: offer it
	}
	cfg.User.Honorific = p.ask("     How should it address you (sir, ma'am, or your name)", address)
	// 2. brain
	fmt.Fprintln(p.out)
	fmt.Fprintln(p.out, "2/3  Which model should it think with?")
	thinks := askBrain(p, cfg, ollama, check)
	// 3. where to reach you
	fmt.Fprintln(p.out)
	fmt.Fprintln(p.out, "3/3  Where should it reach you? (you can add more later)")
	hint := "Enter to skip"
	if cfg.Channels.WhatsApp.Owner != "" {
		hint = "type none to turn WhatsApp off"
	}
	for tries := 1; ; tries++ {
		wa := p.ask(fmt.Sprintf("     WhatsApp number with country code, like %s (%s)", config.PhoneExample(), hint), cfg.Channels.WhatsApp.Owner)
		if wa == "" || strings.EqualFold(wa, "none") {
			cfg.Channels.WhatsApp.Enabled, cfg.Channels.WhatsApp.Owner = false, ""
			break
		}
		if n, ok := config.NormalizePhone(wa); ok {
			cfg.Channels.WhatsApp.Enabled, cfg.Channels.WhatsApp.Owner = true, n
			break
		}
		if n, ok := config.NormalizePhone("+" + wa); ok && wa == cfg.Channels.WhatsApp.Owner {
			// Kept as it was saved: digits only, with the country code.
			cfg.Channels.WhatsApp.Enabled, cfg.Channels.WhatsApp.Owner = true, n
			break
		}
		if tries == 3 { // and not forever when the answers run out
			fmt.Fprintln(p.out, "     Leaving WhatsApp as it was; run `mirrin init` again to change it.")
			break
		}
		fmt.Fprintf(p.out, "     That needs your country code at the front, like %s.\n", config.PhoneExample())
	}
	return thinks
}

// firstLook runs a one-off in-process daemon so the twin can say something
// real. It returns "" when the twin has already introduced itself.
func firstLook(cfg *config.Config) (string, error) {
	d, err := daemon.New(cfg, daemon.Options{Headless: true, Version: version, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		return "", err
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if !d.NeedsFirstLook(ctx) {
		return "", nil
	}
	fmt.Println()
	fmt.Printf("One moment, %s is taking a first look…\n", cfg.Name)
	return d.FirstLook(ctx)
}

// greet has a brand-new twin introduce itself at the start of its first
// conversation, as init would have.
func greet(ctx context.Context, d *daemon.Daemon, cfg *config.Config) {
	if !d.NeedsFirstLook(ctx) {
		return
	}
	fmt.Printf("One moment, %s is taking a first look…\n", cfg.Name)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	intro, err := d.FirstLook(ctx)
	if err != nil {
		fmt.Println(llm.Friendly(err))
		return
	}
	fmt.Printf("\n%s: %s\n\n", cfg.Name, intro)
}
