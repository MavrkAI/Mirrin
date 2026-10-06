package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

// buildModel builds cfg's model, refusing a hosted one with no key at all
// (llm.New would build an Anthropic client that fails every turn).
func buildModel(cfg *config.Config) (llm.Provider, error) {
	s := providerSettings(cfg)
	if err := llm.MissingKey(s); err != nil {
		return nil, err
	}
	return llm.NewWithFallback(s) // a retired model id falls back to the provider's default (retired.go)
}

// startingModel is the model a new daemon thinks with. Without a usable one
// it starts anyway with a stand-in that says what's missing: the menu bar and
// health page show it, and the service doesn't crash-loop where nobody can
// see why. healModel swaps in the real one once a key turns up.
func startingModel(cfg *config.Config, log *slog.Logger) llm.Provider {
	p, err := buildModel(cfg)
	if err != nil {
		log.Warn("model unavailable", "err", err)
		return llm.Unavailable(err)
	}
	return p
}

// healModel rebuilds a model that had no usable key from the config on disk
// and the saved secrets, so a key added with `mirrin init` or `mirrin chat`
// works on the next message without a restart. While the model works it does
// nothing.
func (d *Daemon) healModel() {
	if !d.modelDown.Load() && !llm.IsUnavailable(d.agent.Provider()) {
		return
	}
	d.cmu.Lock()
	defer d.cmu.Unlock()
	next, err := d.configBase()
	if err != nil {
		return
	}
	p, err := buildModel(next)
	if err != nil {
		return // still nothing to think with; the reply says what's missing
	}
	d.modelDown.Store(false)
	d.cfg.LLM = next.LLM
	d.agent.SetConfig(*d.cfg) // the agent keeps its own copy (max_tokens, effort, history)
	d.agent.SetProvider(p)
	d.applyVoiceProvider(d.cfg)
	d.log.Info("model reloaded", "provider", next.LLM.Provider, "model", next.LLM.Model)
}

// runTask is agent.RunTask for background work (protocols, watchers,
// tasks), which has no message to heal the model first: a key added since
// the model failed works here too, and a failure is put in plain words.
func (d *Daemon) runTask(ctx context.Context, chatKey, task string) (string, error) {
	d.healModel()
	out, err := d.agent.RunTask(ctx, chatKey, task)
	return out, d.explain(err)
}

// explain turns a model error into the plain explanation, and notes a
// missing or rejected key so the next turn reloads the model first.
func (d *Daemon) explain(err error) error {
	d.noticeRetired() // the model fell back from a retired one: ask the owner once (retired.go)
	if llm.CredentialError(err) {
		d.modelDown.Store(true)
	}
	return llm.Explain(err)
}

// modelHealth makes a real, free request to the model provider, so a bad key
// or a model Ollama hasn't pulled shows up here rather than mid-conversation.
func (d *Daemon) modelHealth(ctx context.Context) (health.State, string, string) {
	d.healModel()
	c := d.Config()
	if err := llm.MissingKey(providerSettings(&c)); err != nil {
		return health.Fail, "no API key for " + c.LLM.Provider + " yet",
			"get one at " + llm.KeyURL(c.LLM.Provider) + " and paste it under Accounts → Model, or use a free model on this computer with Ollama"
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := llm.Check(ctx, d.agent.Provider()); err != nil {
		return health.Fail, llm.Friendly(d.explain(err)), ""
	}
	if configured, using, fellBack := d.fellBack(); fellBack { // retired.go
		return health.Warn, c.LLM.Provider + "/" + using + " (standing in for the retired " + configured + ")",
			"say yes when I ask to keep it, or pick a model from my Model menu"
	}
	return health.OK, c.LLM.Provider + "/" + c.LLM.Model, ""
}

// apiRetry is how long the local API waits before trying a taken address again.
var apiRetry = 30 * time.Second

// serveAPI runs the local API until ctx ends. If another program holds the
// address, the rest of the twin keeps running, the health check says why, and
// it tries again every apiRetry until the address is free.
func (d *Daemon) serveAPI(ctx context.Context, start func(context.Context) error) {
	addr := d.Config().API.Listen
	for {
		err := start(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		d.apiFailed(err)
		if !addrInUse(err) {
			return // a setting to fix, not a program to wait for
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(apiRetry):
			}
			if ln, err := net.Listen("tcp", addr); err == nil {
				ln.Close()
				break
			}
		}
		d.apiErr.Store(nil)
		d.log.Info("local API address is free again", "addr", addr)
		d.recheck()
	}
}

// apiFailed records that the local API couldn't listen. Everything else keeps
// running; the health check says why and what to do.
func (d *Daemon) apiFailed(err error) {
	msg := err.Error()
	if addrInUse(err) {
		msg = "another program is using it"
	}
	d.apiErr.Store(&msg)
	d.log.Warn("local API unavailable; everything else keeps running", "addr", d.Config().API.Listen, "err", err)
	d.recheck()
}

// recheck runs the local API's self-check again so the menu bar and health
// page catch up with a change to it. Only that check: the model's is a
// request to its provider, and nothing about it changed. Runs are
// serialised, so this one lands last.
func (d *Daemon) recheck() {
	if d.health != nil && d.runCtx != nil {
		go d.health.Recheck(d.runCtx, "api")
	}
}

// apiURL is a page link, or "" while the local API can't listen: a link to
// an address another program holds would open that program instead.
func (d *Daemon) apiURL(u string) string {
	if d.apiErr.Load() != nil {
		return ""
	}
	return u
}

// newAPIServer builds the local API with everything the twin serves on it.
// Settings pages answer on other devices' connections only with
// reach.admin_remote (and an admin device).
func (d *Daemon) newAPIServer(tok string) *api.Server {
	c := d.Config()
	srv := api.New(c.API.Listen, tok, d).WithMemory(memoryAdapter{d.store, d}).WithHealth(d)
	if c.API.Remote {
		srv.AllowRemote()
	}
	if c.Reach.AdminRemote {
		srv.AllowAdminRemote()
	}
	srv.WithScreen(d).WithProtocols(d).WithChannels(d).WithAccounts(d)
	srv.WithWelcome(d)
	srv.WithUsage(d) // usage.go: GET /usage for the screen
	return srv
}

// addrInUse reports whether a listen failed because the address is taken.
func addrInUse(err error) bool {
	const wsaeaddrinuse = syscall.Errno(10048) // Windows
	var errno syscall.Errno
	if errors.Is(err, syscall.EADDRINUSE) || errors.As(err, &errno) && errno == wsaeaddrinuse {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "address already in use") || strings.Contains(msg, "Only one usage of each socket address")
}

// apiHealth reports whether the local API is really serving. In a one-off
// check with no twin running (mirrin doctor) it looks at who holds the port.
func (d *Daemon) apiHealth(context.Context) (health.State, string, string) {
	addr := d.Config().API.Listen
	if addr == "" {
		return health.Off, "switched off in the settings file (api.listen is empty)", ""
	}
	taken := "quit the other program using " + addr + ", or in the settings file change api.listen (the address these pages use) to a free one, then restart"
	if e := d.apiErr.Load(); e != nil {
		if *e == "another program is using it" {
			return health.Fail, "can't open " + addr + ": " + *e, taken + " (I try again every 30 seconds)"
		}
		return health.Fail, "can't open " + addr + ": " + *e, "in the settings file, check api.listen (the address these pages use) and api.remote (how other devices reach it), then restart"
	}
	if d.runCtx == nil {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return health.Off, "not running", ""
		}
		conn.Close()
		return health.Fail, "another program is using " + addr, taken
	}
	return health.OK, addr, ""
}

// channelsIdle describes the channels when the twin isn't running, so a
// one-off check doesn't claim they're connected.
func channelsIdle(cfg config.Config) (health.State, string, string) {
	var names []string
	for _, k := range cfg.Connectors() {
		if k.Enabled {
			names = append(names, k.Label)
		}
	}
	if len(names) == 0 {
		return health.Off, "none connected", "open Channels from the menu to add one"
	}
	return health.Off, strings.Join(names, ", ") + " (they connect when the twin runs)", ""
}

// configBase is what a settings change starts from: the file on disk, so
// `mirrin voice setup` and hand edits made while the twin runs aren't undone
// by saving the copy in memory. With no file it is a deep copy of the live
// config, so a rejected change can't reach the live maps.
func (d *Daemon) configBase() (*config.Config, error) {
	if _, err := os.Stat(config.Path()); err != nil {
		c := d.cfg.Clone()
		d.voiceOwn.saved(&c.Channels.Voice) // as a file would hold it: the owner's choices, not the persona's (persona.go)
		return c, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("the config file has a problem, so I haven't changed anything (%w); fix it or run `mirrin init`", err)
	}
	return cfg, nil
}

// stampInstall starts the first-week tour's clock the first time the twin
// runs, however it was set up. A twin that was already talking to people
// before this was recorded (an upgrade) isn't new: its clock starts at its
// first conversation, and it has introduced itself long since.
func (d *Daemon) stampInstall(ctx context.Context) {
	if v, _ := d.store.Get(ctx, "installed_at"); v != "" {
		return
	}
	at := time.Now()
	if first, ok := d.store.FirstConversationAt(ctx); ok {
		if first.Before(at) {
			at = first
		}
		_ = d.store.Set(ctx, "first_look_at", at.Format(time.RFC3339))
	}
	_ = d.store.Set(ctx, "installed_at", at.Format(time.RFC3339))
}

// NeedsFirstLook reports whether the twin has yet to introduce itself.
// Installs more than a day old count as introduced.
func (d *Daemon) NeedsFirstLook(ctx context.Context) bool {
	d.stampInstall(ctx)
	if v, _ := d.store.Get(ctx, "first_look_at"); v != "" {
		return false
	}
	v, _ := d.store.Get(ctx, "installed_at")
	t, err := time.Parse(time.RFC3339, v)
	return err == nil && time.Since(t) < 24*time.Hour
}
