//go:build (darwin && cgo) || linux || windows

// Package tray puts Mirrin in the menu bar (macOS), system tray (Windows) or
// app indicator (Linux): status at a glance, one click to talk, pause, quit.
package tray

import (
	"context"
	_ "embed"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/systray"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/channels/voice"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/orb"
	"github.com/MavrkAI/Mirrin/internal/persona"
)

//go:embed assets/icon.png
var iconPNG []byte

//go:embed assets/icon.ico
var iconICO []byte

// Backend is what the tray needs from the daemon.
type Backend interface {
	api.Backend
	Spend(context.Context) (memory.Spend, error)
	Protocols() []string
	ConfigPath() string
	Config() config.Config
	UpdateConfig(mutate func(c *config.Config)) error
	PreviewVoice(ctx context.Context)
	Restart() error
	MemoryURL() string
	HealthURL() string
	UIURL() string
	ProtocolsURL() string
	ChannelsURL() string
	AccountsURL() string
	SpendingURL() string
	Events() *events.Bus
	Health() health.Report
	RunHealth(ctx context.Context) health.Report
	Models(ctx context.Context, provider string) ([]string, error)
	SetProvider(provider string) error
	// Interrupt stops whatever the twin is doing, in any chat, as "stop"
	// does in the owner's own chat, and says what that was.
	Interrupt(ctx context.Context) (string, bool)
}

// choice is one option in a radio group.
type choice struct {
	label string
	value string
}

// radio builds a group of checkbox items under parent where exactly one is
// checked. get reads the current value; set applies a new one.
func radio(ctx context.Context, parent *systray.MenuItem, choices []choice, get func(config.Config) string, set func(*config.Config, string), backend Backend, name string) func() {
	items := make([]*systray.MenuItem, len(choices))
	refresh := func() {
		cur := get(backend.Config())
		for i, ch := range choices {
			if ch.value == cur {
				items[i].Check()
			} else {
				items[i].Uncheck()
			}
		}
	}
	for i, ch := range choices {
		ch := ch
		items[i] = parent.AddSubMenuItemCheckbox(ch.label, "", false)
		go func(item *systray.MenuItem) {
			for {
				select {
				case <-ctx.Done():
					return
				case <-item.ClickedCh:
					if err := backend.UpdateConfig(func(c *config.Config) { set(c, ch.value) }); err != nil {
						notify(name, "Could not save: "+err.Error())
					}
					refresh()
				}
			}
		}(items[i])
	}
	refresh()
	return refresh
}

// Run shows the tray and blocks until Quit. runDaemon is started in the
// background; when it returns, or the user quits, the tray closes.
func Run(ctx context.Context, name string, backend Backend, runDaemon func(ctx context.Context) error) error {
	stopLogs := maintainServiceLogs(ctx)
	defer stopLogs()
	if runtime.GOOS == "linux" && !desktopPresent() {
		return runDaemon(ctx)
	}
	ctx, cancel := context.WithCancel(ctx)
	var daemonErr error
	done := make(chan struct{})
	go func() {
		daemonErr = runDaemon(ctx)
		close(done)
		systray.Quit()
	}()

	onReady := func() {
		go openWelcome(ctx, backend)
		if runtime.GOOS == "windows" {
			systray.SetIcon(iconICO)
		} else {
			systray.SetTemplateIcon(iconPNG, iconPNG)
		}
		systray.SetTooltip(name)

		shownName := name // what the menu calls the twin now (liveName)
		status := systray.AddMenuItem(name+" · starting", "")
		status.Disable()
		pending := systray.AddMenuItem(pendingTitle(0), "Open the screen to decide")
		pending.Disable()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-pending.ClickedCh:
					if u := backend.UIURL(); u != "" {
						openURL(u)
					}
				}
			}
		}()
		refreshExtras := buildExtras(ctx, name, backend)
		systray.AddSeparator()
		talk := systray.AddMenuItem("Talk to "+name, "Open a voice session in a terminal")
		chat := systray.AddMenuItem("Chat in terminal", "Open a text session in a terminal")
		screenItem := systray.AddMenuItem("Open screen…", "Your twin's own screen: chat, what needs you and your day. Put it on a wall or a second display")
		orbOn := orb.Available() && (backend.Config().UI.Orb == nil || *backend.Config().UI.Orb)
		orbItem := systray.AddMenuItemCheckbox("Floating orb", "A small orb in the corner of the screen, like Siri", orbOn)
		if !orb.Available() {
			orbItem.Disable()
		}
		var orbEnabled atomic.Bool
		orbEnabled.Store(orbOn)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-orbItem.ClickedCh:
					if orbItem.Checked() {
						orbItem.Uncheck()
						orbEnabled.Store(false)
						orb.Hide()
						_ = backend.UpdateConfig(func(c *config.Config) { f := false; c.UI.Orb = &f })
					} else {
						orbItem.Check()
						orbEnabled.Store(true)
						_ = backend.UpdateConfig(func(c *config.Config) { t := true; c.UI.Orb = &t })
					}
				}
			}
		}()
		if orb.Available() {
			go orbDriver(ctx, backend, &orbEnabled)
		}
		memoryItem := systray.AddMenuItem("Your twin…", "Who it is and what it remembers about you")
		storeItem := systray.AddMenuItem("Routines…", "Your routines, and packs from the community")
		channelsItem := systray.AddMenuItem("Channels…", "Connect Telegram, Discord, Slack, Signal and more")
		accountsItem := systray.AddMenuItem("Accounts…", "Sign your twin into Google: calendar, mail and files")
		spendingItem := systray.AddMenuItem("Spending…", "What it may pay for you, and what it has paid")
		healthItem := systray.AddMenuItem("Health: checking…", "Hearing, speaking, thinking and every connection, checked every hour")
		healthOpen := healthItem.AddSubMenuItem("Open health page…", "")
		healthRun := healthItem.AddSubMenuItem("Check now", "")
		var problemItems []*systray.MenuItem
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-healthOpen.ClickedCh:
					if u := backend.HealthURL(); u != "" {
						openURL(u)
					}
				case <-healthRun.ClickedCh:
					go backend.RunHealth(ctx)
				}
			}
		}()
		refreshHealth := func() {
			rep := backend.Health()
			if len(rep.Results) == 0 {
				return
			}
			healthItem.SetTitle("Health: " + rep.Summary())
			probs := rep.Problems()
			for i, p := range probs {
				title := fmt.Sprintf("%s: %s", p.Label, p.Detail)
				if i < len(problemItems) {
					problemItems[i].SetTitle(title)
					problemItems[i].Show()
				} else {
					it := healthItem.AddSubMenuItem(title, p.Fix)
					it.Disable()
					problemItems = append(problemItems, it)
				}
			}
			for i := len(probs); i < len(problemItems); i++ {
				problemItems[i].Hide()
			}
		}
		systray.AddSeparator()
		protos := systray.AddMenuItem("Run a routine", "")
		names := backend.Protocols()
		if len(names) == 0 {
			protos.Disable()
		}
		for _, n := range names {
			n := n
			item := protos.AddSubMenuItem(n, "")
			go func() {
				for range item.ClickedCh {
					if err := backend.RunProtocol(context.Background(), n); err != nil {
						notify(name, "That routine couldn't start: "+err.Error())
					}
				}
			}()
		}
		systray.AddSeparator()
		buildSettings(ctx, name, backend)
		systray.AddSeparator()
		stopItem := systray.AddMenuItem("Stop what it's doing", "Cut short whatever is running now, in any chat; anything already done stays done")
		pause := systray.AddMenuItemCheckbox("Pause", "Stop acting until resumed", false)
		timed, _ := any(backend).(timedPauser)
		pauseHour := systray.AddMenuItem("Pause for 1 hour", "Resumes by itself in an hour")
		pauseTomorrow := systray.AddMenuItem("Pause until tomorrow", "Resumes by itself at 8 tomorrow morning")
		if timed == nil {
			pauseHour.Hide()
			pauseTomorrow.Hide()
		} else {
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case <-pauseHour.ClickedCh:
						timed.PauseUntil(pauseEnd("hour", time.Now()))
						pause.Check()
					case <-pauseTomorrow.ClickedCh:
						timed.PauseUntil(pauseEnd("tomorrow", time.Now()))
						pause.Check()
					}
				}
			}()
		}
		config := systray.AddMenuItem("Open settings file", "Every setting, as text. For the few that have no page yet")
		restart := systray.AddMenuItem("Restart "+name, "Needed after granting microphone access")
		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit "+name, "")

		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-talk.ClickedCh:
					openTerminal("voice")
				case <-chat.ClickedCh:
					openTerminal("chat")
				case <-screenItem.ClickedCh:
					if u := backend.UIURL(); u != "" {
						openURL(u)
					}
				case <-storeItem.ClickedCh:
					if u := backend.ProtocolsURL(); u != "" {
						openURL(u)
					}
				case <-channelsItem.ClickedCh:
					if u := backend.ChannelsURL(); u != "" {
						openURL(u)
					}
				case <-accountsItem.ClickedCh:
					if u := backend.AccountsURL(); u != "" {
						openURL(u)
					}
				case <-spendingItem.ClickedCh:
					if u := backend.SpendingURL(); u != "" {
						openURL(u)
					}
				case <-memoryItem.ClickedCh:
					if u := backend.MemoryURL(); u != "" {
						openURL(u)
					} else {
						notify(name, "Settings pages are switched off for this twin, so this page can't open. In the settings file, set api.listen to 127.0.0.1:7742 and restart to turn them back on.")
					}
				case <-stopItem.ClickedCh:
					msg, _ := backend.Interrupt(ctx)
					notify(name, msg)
				case <-pause.ClickedCh:
					if pause.Checked() {
						pause.Uncheck()
						backend.SetPaused(false)
					} else {
						pause.Check()
						backend.SetPaused(true)
					}
				case <-config.ClickedCh:
					openFile(backend.ConfigPath())
				case <-restart.ClickedCh:
					if err := backend.Restart(); err != nil {
						notify(name, "Couldn't restart: "+err.Error())
					}
				case <-quit.ClickedCh:
					cancel()
					systray.Quit()
					return
				}
			}
		}()

		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				st := backend.Status(ctx)
				state := "online"
				for _, ch := range st.Channels {
					if ch == "voice" {
						state = "listening"
					}
				}
				if st.Paused {
					state = "paused"
				}
				if st.Paused != pause.Checked() { // a pause kept over a restart shows ticked
					if st.Paused {
						pause.Check()
					} else {
						pause.Uncheck()
					}
				}
				pending.SetTitle(pendingTitle(st.Pending))
				if st.Pending == 0 {
					pending.Disable()
				} else {
					pending.Enable()
				}
				if timed != nil {
					if st.Paused {
						state = pausedState(timed.PausedUntil(), time.Now())
						pauseHour.Hide()
						pauseTomorrow.Hide()
					} else {
						pauseHour.Show()
						pauseTomorrow.Show()
					}
				}
				// The twin's name follows the persona (switched in the menu, no
				// restart): every line that names it is renamed with it.
				if now := liveName(backend, name); now != shownName {
					shownName = now
					systray.SetTooltip(now)
					talk.SetTitle("Talk to " + now)
					restart.SetTitle("Restart " + now)
					quit.SetTitle("Quit " + now)
				}
				status.SetTitle(fmt.Sprintf("%s · %s · %s", shownName, state, st.Model))
				refreshHealth()
				refreshExtras()
				// Showing the orb makes it clickable, and hiding it gives the
				// clicks back to the desktop (orb.Show and orb.Hide).
				if orb.Available() && orbEnabled.Load() && st.Pending > 0 {
					orb.Show(backend.UIURL()+"&mode=orb", 380, 300, 12)
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}

	systray.Run(onReady, func() { cancel() })
	<-done
	return daemonErr
}

// orbBackend is what the orb needs from the twin.
type orbBackend interface {
	UIURL() string
	Events() *events.Bus
	Status(ctx context.Context) api.Status
}

// The native orb, replaced in tests.
var (
	orbStartDelay = 2 * time.Second
	orbPreload    = orb.Preload
	orbShow       = orb.Show
	orbHide       = orb.Hide
)

// orbDriver shows the floating orb only while Mirrin is active: from the first
// sign of listening until a few seconds after it goes quiet, or while something
// needs a decision.
func orbDriver(ctx context.Context, backend orbBackend, enabled *atomic.Bool) {
	time.Sleep(orbStartDelay) // let the API come up
	url := backend.UIURL()
	if url == "" {
		return
	}
	url += "&mode=orb"
	orbPreload(url, 380, 300, 12)
	ch, stop := backend.Events().Subscribe()
	defer stop()
	var hide *time.Timer
	schedHide := func(after time.Duration) {
		if hide != nil {
			hide.Stop()
		}
		hide = time.AfterFunc(after, func() {
			if backend.Status(ctx).Pending == 0 {
				orbHide()
			}
		})
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			if !enabled.Load() {
				continue
			}
			switch ev.Kind {
			case "state":
				if ev.Text == "idle" {
					schedHide(7 * time.Second)
				} else {
					if hide != nil {
						hide.Stop()
					}
					orbShow(url, 380, 300, 12)
				}
			case "message":
				// Something it did on its own (a briefing, a reminder) pops
				// the orb for a while. A system notice ("Telegram is
				// reconnecting") doesn't: it shows on the screen.
				orbShow(url, 380, 300, 12)
				schedHide(30 * time.Second)
			case "approval":
				// A new request shows beside the orb at once; a decided one
				// lets it go quiet again.
				if backend.Status(ctx).Pending > 0 {
					if hide != nil {
						hide.Stop()
					}
					orbShow(url, 380, 300, 12)
				} else {
					schedHide(3 * time.Second)
				}
			case "browser":
				// Mirrin handed a page over: the orb says so, and a click on
				// it opens the screen at the browser.
				if ev.Text == "handover" {
					orbShow(url, 380, 300, 12)
					schedHide(2 * time.Minute)
				}
			}
		}
	}
}

// itemTitles maps dynamic model items to their current title.
var itemTitles = map[*systray.MenuItem]string{}

// buildSettings adds the Voice, Autonomy and Model menus.
func buildSettings(ctx context.Context, name string, backend Backend) {
	cfg := backend.Config()

	// ---- Voice ----
	vm := systray.AddMenuItem("Voice", "")
	preview := vm.AddSubMenuItem("Preview current voice", "Speak a sample line")
	setup := vm.AddSubMenuItem("Set up voice…", "The natural offline voice, the microphone and the wake word")
	pages, _ := any(backend).(pagesBackend)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-preview.ClickedCh:
				go backend.PreviewVoice(ctx)
			case <-setup.ClickedCh:
				openVoiceSetup(pages, openURL, openTerminal)
			}
		}
	}()
	vm.AddSubMenuItem("", "").Disable() // separator substitute inside submenus

	// Voices grouped by engine; choosing one also selects its engine.
	voicesMenu := vm.AddSubMenuItem("Voice", "")
	options := voice.Catalog(cfg.Channels.Voice)
	var refreshers []func()
	lastGroup := ""
	var groupItems []*systray.MenuItem
	var groupOpts []voice.Option
	flush := func() {
		if len(groupOpts) == 0 {
			return
		}
		opts := groupOpts
		items := groupItems
		refresh := func() {
			c := backend.Config()
			eng := c.Channels.Voice.Engine
			if eng == "" || eng == "auto" {
				eng = effectiveEngine(c.Channels.Voice)
			}
			for i, o := range opts {
				if o.Engine == eng && o.ID == c.Channels.Voice.Voice {
					items[i].Check()
				} else {
					items[i].Uncheck()
				}
			}
		}
		for i := range opts {
			o := opts[i]
			go func(item *systray.MenuItem) {
				for {
					select {
					case <-ctx.Done():
						return
					case <-item.ClickedCh:
						err := backend.UpdateConfig(func(c *config.Config) {
							c.Channels.Voice.Engine = o.Engine
							c.Channels.Voice.Voice = o.ID
						})
						if err != nil {
							notify(name, "Could not save: "+err.Error())
						}
						for _, r := range refreshers {
							r()
						}
						go backend.PreviewVoice(ctx)
					}
				}
			}(items[i])
		}
		refreshers = append(refreshers, refresh)
		groupItems, groupOpts = nil, nil
	}
	for _, o := range options {
		if o.Group != lastGroup {
			flush()
			hdr := voicesMenu.AddSubMenuItem("— "+o.Group+" —", "")
			hdr.Disable()
			lastGroup = o.Group
		}
		groupItems = append(groupItems, voicesMenu.AddSubMenuItemCheckbox(o.Label, "", false))
		groupOpts = append(groupOpts, o)
	}
	flush()
	for _, r := range refreshers {
		r()
	}

	engineMenu := vm.AddSubMenuItem("Engine", "")
	engines := []choice{{"Auto (best available)", "auto"}, {"Natural, offline (Kokoro)", "kokoro"}, {"System voice", "system"}, {"ElevenLabs (needs API key)", "elevenlabs"}}
	refreshers = append(refreshers, radio(ctx, engineMenu, engines,
		func(c config.Config) string {
			if c.Channels.Voice.Engine == "" {
				return "auto"
			}
			return c.Channels.Voice.Engine
		},
		func(c *config.Config, v string) { c.Channels.Voice.Engine = v }, backend, name))

	modeMenu := vm.AddSubMenuItem("Listening", "")
	radio(ctx, modeMenu, []choice{
		{"Always on here (\"" + voice.WakePhrase(cfg.Channels.Voice.WakeWord) + "\")", "wake"},
		{"Only in a Talk session (push to talk)", "push"},
	},
		func(c config.Config) string {
			if c.Channels.Voice.Enabled && c.Channels.Voice.Mode == "wake" {
				return "wake"
			}
			return "push"
		},
		func(c *config.Config, v string) {
			c.Channels.Voice.Mode = v
			c.Channels.Voice.Enabled = v == "wake"
		}, backend, name)

	wakeMenu := vm.AddSubMenuItem("Wake word detection", "How \""+voice.WakePhrase(cfg.Channels.Voice.WakeWord)+"\" is recognised")
	radio(ctx, wakeMenu, []choice{
		{"Listen for the name only, on this computer", "openwakeword"},
		{"Understand everything said, then look for the name", "transcribe"},
		{"Auto", "auto"},
	},
		func(c config.Config) string {
			if c.Channels.Voice.WakeEngine == "" {
				return "auto"
			}
			return c.Channels.Voice.WakeEngine
		},
		func(c *config.Config, v string) { c.Channels.Voice.WakeEngine = v }, backend, name)

	talkMenu := vm.AddSubMenuItem("Interrupt by talking over it", "Saying its name always interrupts; this is about plain talking")
	radio(ctx, talkMenu, []choice{{"Off (name only)", "off"}, {"Only when clearly louder", "low"}, {"Normal", "normal"}, {"Eager (close microphone)", "high"}},
		func(c config.Config) string {
			if c.Channels.Voice.TalkOver == "" {
				return "normal"
			}
			return c.Channels.Voice.TalkOver
		},
		func(c *config.Config, v string) { c.Channels.Voice.TalkOver = v }, backend, name)

	followMenu := vm.AddSubMenuItem("Follow-up window", "How long it keeps listening after replying")
	radio(ctx, followMenu, []choice{{"Off", "0"}, {"4 seconds", "4"}, {"6 seconds", "6"}, {"8 seconds", "8"}, {"10 seconds", "10"}},
		func(c config.Config) string { return fmt.Sprint(c.Channels.Voice.FollowupSeconds) },
		func(c *config.Config, v string) { fmt.Sscan(v, &c.Channels.Voice.FollowupSeconds) }, backend, name)

	speedMenu := vm.AddSubMenuItem("Speed", "")
	radio(ctx, speedMenu, []choice{{"Slower (0.9×)", "0.9"}, {"Normal (1.0×)", "1"}, {"Brisk (1.1×)", "1.1"}, {"Fast (1.25×)", "1.25"}},
		func(c config.Config) string {
			sp := c.Channels.Voice.Speed
			if sp == 0 {
				sp = 1
			}
			return fmt.Sprint(sp)
		},
		func(c *config.Config, v string) { fmt.Sscan(v, &c.Channels.Voice.Speed) }, backend, name)

	// ---- Autonomy ----
	am := systray.AddMenuItem("Autonomy", "What it may do without asking")
	levels := []choice{{"Just do it", "auto"}, {"Ask me first", "ask"}, {"Never", "never"}}
	radio(ctx, am.AddSubMenuItem("Looking things up (read)", ""), levels[:2],
		func(c config.Config) string { return c.Autonomy.Read }, func(c *config.Config, v string) { c.Autonomy.Read = v }, backend, name)
	radio(ctx, am.AddSubMenuItem("Sending & creating (write)", ""), levels,
		func(c config.Config) string { return c.Autonomy.Write }, func(c *config.Config, v string) { c.Autonomy.Write = v }, backend, name)
	radio(ctx, am.AddSubMenuItem("Running commands & deleting (risky)", ""), levels,
		func(c config.Config) string { return c.Autonomy.Dangerous }, func(c *config.Config, v string) { c.Autonomy.Dangerous = v }, backend, name)

	// ---- Persona ----
	pm := systray.AddMenuItem("Persona", "Who your twin is")
	if ps, err := persona.Load(config.Home(), backend.Config().ProtocolsDir); err == nil {
		var choices []choice
		for _, p := range ps {
			choices = append(choices, choice{p.Name + " — " + p.Tagline, p.ID})
		}
		radio(ctx, pm, choices,
			func(c config.Config) string { return c.Persona },
			func(c *config.Config, v string) {
				// Another persona brings its own voice, wake word and name.
				if c.Persona != v {
					c.Channels.Voice.Voice, c.Channels.Voice.WakeWord, c.Channels.Voice.WakeModel = "", "", ""
				}
				c.Persona = v
				if p, ok := persona.Find(ps, v); ok && (c.Name == "" || personaName(ps, c.Name)) {
					c.Name = p.Name
				}
			}, backend, name)
	}

	// ---- Time zone ----
	zoneItem := systray.AddMenuItem(zoneTitle(backend.Config().User.Timezone, config.LocalTimezone()), "Pin the time zone reminders and routines keep, or follow this computer as it travels")
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-zoneItem.ClickedCh:
				if err := toggleZone(backend, config.LocalTimezone()); err != nil {
					notify(name, "Could not save: "+err.Error())
				}
				zoneItem.SetTitle(zoneTitle(backend.Config().User.Timezone, config.LocalTimezone()))
			}
		}
	}()

	// ---- Model ----
	mm := systray.AddMenuItem("Model", "")
	modelsMenu := mm.AddSubMenuItem("Model", "Models the current provider offers")
	var modelItems []*systray.MenuItem
	var modelMu sync.Mutex
	refreshModels := func() {
		c := backend.Config()
		lctx, lcancel := context.WithTimeout(ctx, 10*time.Second)
		models, err := backend.Models(lctx, "")
		lcancel()
		if err != nil || len(models) == 0 {
			models = []string{c.LLM.Model}
		}
		if len(models) > 40 {
			models = models[:40]
		}
		modelMu.Lock()
		defer modelMu.Unlock()
		for i, m := range models {
			if i >= len(modelItems) {
				item := modelsMenu.AddSubMenuItemCheckbox(m, "", false)
				modelItems = append(modelItems, item)
				go func(item *systray.MenuItem) {
					for {
						select {
						case <-ctx.Done():
							return
						case <-item.ClickedCh:
							modelMu.Lock()
							title := itemTitles[item]
							modelMu.Unlock()
							if err := backend.UpdateConfig(func(c *config.Config) { c.LLM.Model = title }); err != nil {
								notify(name, "Could not save: "+err.Error())
							}
							modelMu.Lock()
							for it, t := range itemTitles {
								if t == title {
									it.Check()
								} else {
									it.Uncheck()
								}
							}
							modelMu.Unlock()
						}
					}
				}(item)
			}
			modelItems[i].SetTitle(m)
			itemTitles[modelItems[i]] = m
			modelItems[i].Show()
			if m == c.LLM.Model {
				modelItems[i].Check()
			} else {
				modelItems[i].Uncheck()
			}
		}
		for i := len(models); i < len(modelItems); i++ {
			modelItems[i].Hide()
			delete(itemTitles, modelItems[i])
		}
	}
	go refreshModels()

	providerMenu := mm.AddSubMenuItem("Provider", "")
	providers := []choice{{"Anthropic (Claude)", "anthropic"}, {"OpenAI", "openai"}, {"Google Gemini", "gemini"}, {"Ollama (local, private)", "ollama"}}
	provItems := make([]*systray.MenuItem, len(providers))
	refreshProviders := func() {
		cur := backend.Config().LLM.Provider
		for i, p := range providers {
			if p.value == cur {
				provItems[i].Check()
			} else {
				provItems[i].Uncheck()
			}
		}
	}
	for i, p := range providers {
		p := p
		provItems[i] = providerMenu.AddSubMenuItemCheckbox(p.label, "", false)
		go func(item *systray.MenuItem) {
			for {
				select {
				case <-ctx.Done():
					return
				case <-item.ClickedCh:
					switchProvider(backend, p.value, openURL, func(msg string) { notify(name, msg) })
					refreshProviders()
					go refreshModels()
				}
			}
		}(provItems[i])
	}
	refreshProviders()
	radio(ctx, mm.AddSubMenuItem("Effort", "How hard it thinks"), []choice{{"Low", "low"}, {"Medium", "medium"}, {"High", "high"}, {"Extra high", "xhigh"}, {"Max", "max"}},
		func(c config.Config) string { return c.LLM.Effort }, func(c *config.Config, v string) { c.LLM.Effort = v }, backend, name)
}

// effectiveEngine mirrors the channel's auto selection for display.
func effectiveEngine(v config.Voice) string {
	switch {
	case v.TTSCommand != "":
		return "command"
	case voice.ElevenLabsKey(v) != "": // the environment or the saved secrets
		return "elevenlabs"
	case voice.KokoroInstalled(v):
		return "kokoro"
	default:
		return "system"
	}
}

// Available reports whether this build can show a menu bar icon.
func Available() bool { return true }

// openTerminal runs this mirrin binary with args in a new terminal window.
func openTerminal(args ...string) {
	exe, argv := selfCommand(args...)
	line := terminalLine(exe, argv)
	switch runtime.GOOS {
	case "darwin":
		startDesktop(exec.Command("osascript", "-e", terminalScript(line), "-e", `tell application "Terminal" to activate`))
	case "windows":
		startDesktop(exec.Command("cmd", windowsTerminal(exe, argv)...))
	default:
		for _, t := range [][]string{{"x-terminal-emulator", "-e"}, {"gnome-terminal", "--"}, {"konsole", "-e"}, {"xterm", "-e"}} {
			if _, err := exec.LookPath(t[0]); err == nil {
				startDesktop(exec.Command(t[0], append(t[1:], "sh", "-c", line+"; printf '\nPress Enter to close.'; read reply")...))
				return
			}
		}
	}
}

func openURL(u string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("open", u).Start()
	case "windows":
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	default:
		_ = exec.Command("xdg-open", u).Start()
	}
}

func openFile(path string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("open", "-t", path).Start()
	case "windows":
		_ = exec.Command("cmd", "/C", "start", "", path).Start()
	default:
		_ = exec.Command("xdg-open", path).Start()
	}
}

// Notify shows a desktop notification.
func Notify(title, body string) { notify(title, body) }

func notify(title, body string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("osascript", "-e", fmt.Sprintf(`display notification %q with title %q`, body, title)).Run()
	case "linux":
		_ = exec.Command("notify-send", title, body).Run()
	}
}

// personaName reports whether name is one of the personas' own names (so the
// owner hasn't given the twin a name of their own).
func personaName(ps []persona.Persona, name string) bool {
	for _, p := range ps {
		if strings.EqualFold(p.Name, name) {
			return true
		}
	}
	return false
}

// liveName is what the twin is called now: the configured name, which a
// persona switch changes, or the name it started with.
func liveName(b interface{ Config() config.Config }, started string) string {
	if n := strings.TrimSpace(b.Config().Name); n != "" {
		return n
	}
	return started
}
