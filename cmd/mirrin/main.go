// Command mirrin is Mirrin: an open-source personal AI that runs your life,
// installed on your own machine.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/channels/cli"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/daemon"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/persona"
	"github.com/MavrkAI/Mirrin/internal/protocols"
	"github.com/MavrkAI/Mirrin/internal/service"
	"github.com/MavrkAI/Mirrin/internal/tray"
)

var version = "dev"

const usage = `Mirrin — your personal AI that runs your life.

Usage:
  mirrin init                 Set up your twin, a few questions at a time
  mirrin run                  Run your twin with no window: your chat apps, routines and pages
  mirrin tray                 Run your twin with a menu bar / system tray icon
  mirrin chat                 Text with your twin in this terminal (joins the running twin if there is one)
  mirrin voice [--wake]       Talk to your twin out loud (push-to-talk, or always-on wake word)
  mirrin voice setup          Install offline hearing, a natural voice and the wake word
  mirrin whatsapp login       Pair with your WhatsApp account (QR code)
  mirrin calendar login       Authorise Google Calendar
  mirrin service <action>     install | uninstall | start | stop | restart | status   (keep your twin running in the background)
  mirrin identity export <file.tar.gz> [--with-conversations]   Your twin as a file you own (no secrets)
  mirrin identity import <file.tar.gz>                          Bring a twin to this machine
  mirrin backup <cmd>         init | now | status | list | verify | key --age | target | prune | resume   (encrypted backups)
  mirrin restore              Bring your twin back from a backup, here or on a new machine
  mirrin persona <cmd>        list | show <id> | use <id> | new <name>   (who your twin is; Mirrin is the default)
  mirrin protocols <cmd>      Routines: list | new <name> | check | lint [path] | add <git-url|path> | remove <pack> | update | search [term] | install <pack>
  mirrin models [provider]    List models the provider offers (anthropic, openai, gemini, ollama)
  mirrin pair [--screen|--kiosk]  Pair another computer (--screen: a phone or tablet; --kiosk: a wall screen that only looks)
  mirrin connect <code>       Use a twin running on another machine (chat/voice go there)
  mirrin devices [cmd]        list | revoke <id> | rename <id> <name> | add | page | review
  mirrin reach <cmd>          use tailscale|files|relay|off | status | verify | fingerprint | alarm [clear] | stay-awake on|off
  mirrin disconnect           Go back to the twin on this machine
  mirrin doctor               Run the self-checks: hearing, speaking, model, connections
  mirrin usage [prices]       What the model has cost today and this month (estimates)
  mirrin report               Write a problem report to share when asking for help (no secrets)
  mirrin memory <cmd>         check | backups | restore [--fresh]   (what your twin remembers)
  mirrin audit [n]            The last n things your twin did, from its own record
  mirrin update [--check]     Install the latest release, checked first (it never updates on its own)
  mirrin uninstall            Remove Mirrin from this machine (asks before touching your twin)
  mirrin licenses             The licences of Mirrin and of the code inside it
  mirrin version

Docs: https://github.com/MavrkAI/Mirrin
`

func main() {
	if len(os.Args) > 1 && os.Args[1] == "welcome-restore-worker" {
		os.Exit(welcomeRestoreWorker(os.Stdin))
	}
	if len(os.Args) < 2 {
		// Double-clicked Mirrin.app: run the menu bar app.
		if exe, err := os.Executable(); err == nil && strings.Contains(exe, ".app/Contents/MacOS/") {
			os.Args = append(os.Args, "tray")
		} else {
			fmt.Print(usage)
			os.Exit(2)
		}
	}
	// Asked only what this is: touch nothing. Installers, the release smoke
	// test and Homebrew run `mirrin version`, and the home (with one from
	// before the rename to move, and its log) must stay as it is until a
	// real command.
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println("mirrin", version)
		return
	case "help", "--help", "-h":
		fmt.Print(usage)
		return
	case "licenses", "licences":
		licensesCmd(os.Stdout)
		return
	}
	// A service from before the rename that runs an older program stops
	// before anything settles the home, so a home from before the rename
	// can move into place once that twin has quit. While one is still
	// installed (other commands leave it), the home doesn't move.
	config.HoldHomeMove = service.LegacyHold
	switch os.Args[1] {
	case "run", "tray", "chat", "voice", "init", "service":
		service.RetireLegacy(os.Stderr)
	}
	// Keys saved for the background twin work in every terminal too.
	if err := config.LoadSecrets(); err != nil {
		fmt.Fprintln(os.Stderr, "warning: can't read", config.SecretsPath()+":", err)
	}
	log := startLogging(os.Args[1]) // report.go
	slog.SetDefault(log)
	switch os.Args[1] {
	case "run", "tray", "chat", "voice", "init", "service":
		service.TidyUnit(os.Stderr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "init":
		err = runInit()
	case "run":
		err = withModel(true, func(cfg *config.Config) error {
			return service.RunUnderManager(func(ctx context.Context) error {
				d, err := daemon.New(cfg, daemon.Options{Headless: true, Version: version, Log: log})
				if err != nil {
					return err
				}
				return d.Run(ctx)
			})
		})
	case "tray":
		// The menu bar loop must own the main thread, so no service wrapper here.
		err = withModel(true, func(cfg *config.Config) error {
			d, err := daemon.New(cfg, daemon.Options{Headless: true, Tray: true, Version: version, Log: log})
			if err != nil {
				return err
			}
			if err := d.Claim(ctx); err != nil {
				return alreadyInMenuBar(cfg, err) // before a second icon appears
			}
			if modelProblem(cfg) != nil {
				go tray.Notify(cfg.Name, trayNeedsModel)
			}
			d.SetWelcomeRestore(startWelcomeRestore)
			return tray.Run(ctx, cfg.Name, trayBackend{d}, d.Run)
		})
	case "pair": // pair.go
		err = withConfig(func(cfg *config.Config) error { return pairCmd(ctx, cfg, os.Args[2:], os.Stdout) })
	case "connect":
		err = connectCmd(ctx, os.Args[2:], os.Stdout)
	case "devices":
		err = withConfig(func(cfg *config.Config) error { return devicesCmd(ctx, cfg, os.Args[2:], os.Stdout) })
	case "reach":
		err = configureReach(os.Args[2:])
	case "disconnect":
		err = disconnect(os.Stdout)
	case "chat":
		err = withConfig(func(cfg *config.Config) error {
			ch := cli.New(cfg.Name)
			if c := remoteClient(); c != nil {
				return runClient(ctx, ch, c)
			}
			if c := api.Connect(cfg.API.Listen, cfg.DataDir); c != nil {
				fmt.Println("(connected to the running " + cfg.Name + ")")
				// A twin started without a model picks up one chosen here on the next message.
				return withModel(true, func(*config.Config) error { return runClient(ctx, ch, c) })
			}
			return withModel(false, func(cfg *config.Config) error {
				d, err := daemon.New(cfg, daemon.Options{Interactive: true, Version: version, Log: log})
				if err != nil {
					return err
				}
				if err := d.Claim(ctx); err != nil {
					return cantJoin(err)
				}
				greet(ctx, d, cfg)
				return d.Run(ctx)
			})
		})
	case "voice":
		if len(os.Args) > 2 && os.Args[2] == "setup" {
			err = withConfig(func(cfg *config.Config) error { return voiceSetup(ctx, cfg) })
			break
		}
		wake := len(os.Args) > 2 && os.Args[2] == "--wake"
		err = withConfig(func(cfg *config.Config) error {
			if wake {
				cfg.Channels.Voice.Mode = "wake"
			}
			if c := remoteClient(); c != nil {
				return runClient(ctx, joinedVoice(cfg, c), c)
			}
			if c := api.Connect(cfg.API.Listen, cfg.DataDir); c != nil {
				fmt.Println("(connected to the running " + cfg.Name + ")")
				return withModel(true, func(*config.Config) error {
					return runClient(ctx, joinedVoice(cfg, c), c)
				})
			}
			return withModel(false, func(cfg *config.Config) error {
				if wake {
					cfg.Channels.Voice.Mode = "wake"
				}
				d, err := daemon.New(cfg, daemon.Options{Voice: true, Version: version, Log: log})
				if err != nil {
					return err
				}
				return cantJoin(d.Run(ctx))
			})
		})
	case "whatsapp":
		if len(os.Args) < 3 || os.Args[2] != "login" {
			err = fmt.Errorf("usage: mirrin whatsapp login")
			break
		}
		err = withConfig(func(cfg *config.Config) error { return daemon.WhatsAppLogin(ctx, cfg) })
	case "calendar":
		if len(os.Args) < 3 || os.Args[2] != "login" {
			err = fmt.Errorf("usage: mirrin calendar login")
			break
		}
		err = withConfig(func(cfg *config.Config) error { return daemon.CalendarLogin(ctx, cfg) })
	case "service":
		if len(os.Args) < 3 {
			err = fmt.Errorf("usage: mirrin service install|uninstall|start|stop|restart|status")
			break
		}
		action := os.Args[2]
		load := withConfig
		if action == "install" { // what it installs has to be able to think
			load = func(fn func(*config.Config) error) error { return withModel(false, fn) }
		}
		err = load(func(cfg *config.Config) error {
			// launchd runs LaunchAgents inside the GUI session, so the menu bar works there.
			// Windows services live in session 0 with no tray; start `mirrin tray` at login instead.
			service.Mode = serviceMode(runtime.GOOS, cfg.Tray, tray.Available())
			if action == "install" && runtime.GOOS == "darwin" && cfg.Tray && !tray.Available() {
				fmt.Println("This build has no menu bar (it was built without cgo), so the background twin runs without an icon.")
			}
			var up func() bool
			if cfg.API.Listen != "" {
				up = func() bool { return api.Connect(cfg.API.Listen, cfg.DataDir) != nil }
			}
			return service.Control(cfg, action, up, os.Stdout)
		})
	case "identity":
		err = identityCmd(os.Args[2:])
	case "backup", "restore":
		err = backupMain(ctx, os.Args[1], os.Args[2:])
	case "persona":
		err = withConfig(func(cfg *config.Config) error { return personaCmd(cfg, os.Args[2:]) })
	case "protocols":
		err = withConfig(func(cfg *config.Config) error { return protocolsCmd(ctx, cfg, os.Args[2:]) })
	case "models":
		err = withConfig(func(cfg *config.Config) error {
			provider := cfg.LLM.Provider
			if len(os.Args) > 2 {
				provider = os.Args[2]
			}
			p, err := llm.New(llm.ProviderSettings{Provider: provider, Model: cfg.ProviderModel(provider), APIKey: cfg.ProviderKey(provider), BaseURL: cfg.ProviderBaseURL(provider)})
			if err != nil {
				return err
			}
			models, err := llm.Models(ctx, p)
			if err != nil {
				return err
			}
			for _, m := range models {
				mark := "  "
				if provider == cfg.LLM.Provider && m == cfg.LLM.Model {
					mark = "* "
				}
				fmt.Println(mark + m)
			}
			return nil
		})
	case "doctor":
		err = withConfig(func(cfg *config.Config) error { return doctor(ctx, cfg) })
	case "report":
		err = reportCmd(ctx, os.Stdout, os.Args[2:])
	case "usage":
		err = withConfig(func(cfg *config.Config) error { return usageCmd(ctx, os.Stdout, cfg, os.Args[2:]) })
	case "memory":
		err = memoryCmd(os.Stdout, os.Args[2:])
	case "cloud":
		err = cloudCmd(ctx, os.Args[2:])
	case "audit":
		err = withConfig(func(cfg *config.Config) error {
			n := 30
			if len(os.Args) > 2 {
				fmt.Sscanf(os.Args[2], "%d", &n)
			}
			store, err := memory.Open(cfg.DataDir)
			if err != nil {
				return err
			}
			defer store.Close()
			es, err := store.RecentAudit(ctx, n)
			if err != nil {
				return err
			}
			for i := len(es) - 1; i >= 0; i-- {
				e := es[i]
				fmt.Printf("%s  %-22s %-28s %s\n", e.TS.Local().Format("2006-01-02 15:04:05"), e.Kind, e.ChatKey, e.Detail)
			}
			return nil
		})
	case "update":
		err = updateCmd(ctx, os.Args[2:])
	case "uninstall":
		err = uninstallCmd(os.Args[2:])
	default:
		unknownCommand(os.Stderr, os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		var code exitCode
		if errors.As(err, &code) {
			os.Exit(int(code))
		}
		msg := err.Error()
		if m, ok := llm.Describe(err); ok {
			msg = m
		}
		fmt.Fprintln(os.Stderr, "error:", msg)
		noteFatal(log, os.Args[1], logSafe(err, msg)) // the terminal had it whole; the log keeps no Recovery Kit word
		os.Exit(1)
	}
}

func printHealth(rep health.Report) {
	marks := map[health.State]string{health.OK: "✓", health.Warn: "!", health.Fail: "✗", health.Off: "-"}
	for _, r := range rep.Results {
		fmt.Printf(" %s %-22s %s\n", marks[r.State], r.Label, r.Detail)
		if r.Fix != "" && r.State != health.OK {
			fmt.Printf("   fix: %s\n", r.Fix)
		}
	}
	fmt.Println(rep.Summary())
}

// remoteClient connects to a paired twin on another machine, if one is saved.
func remoteClient() *api.Client { return pairedClient() } // pair.go

// personaCmd manages who the twin is.
func personaCmd(cfg *config.Config, args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	ps, skipped := persona.LoadAll(config.Home(), cfg.ProtocolsDir)
	for _, p := range skipped {
		fmt.Fprintf(os.Stderr, "skipped %s: %s\n", p.File, p.Message)
	}
	switch sub {
	case "list":
		cur, _ := persona.Pick(ps, cfg.Persona, "")
		for _, p := range ps {
			mark := "  "
			if p.ID == cur.ID {
				mark = "* "
			}
			src := "bundled"
			if p.Pack != "" {
				src = "pack:" + p.Pack
			} else if p.Source != "bundled" {
				src = "yours"
			}
			fmt.Printf("%s%-10s %-10s %-9s %s\n", mark, p.ID, p.Name, src, p.Tagline)
		}
		fmt.Printf("\nyour twin is called %q. Change the name in config (name:), the personality with `mirrin persona use <id>`.\n", cfg.Name)
		return nil
	case "show":
		if len(args) < 2 {
			return fmt.Errorf("usage: mirrin persona show <id>")
		}
		p, ok := persona.Find(ps, args[1])
		if !ok {
			return fmt.Errorf("no persona %q", args[1])
		}
		fmt.Printf("%s (%s)\n%s\n\n%s\n", p.Name, p.ID, p.Tagline, strings.TrimSpace(p.Character))
		if len(p.Style) > 0 {
			fmt.Println("\nstyle:")
			for _, st := range p.Style {
				fmt.Println("  -", st)
			}
		}
		if p.Voice != "" {
			fmt.Println("\nvoice:", p.Voice, " wake word:", p.WakeWord)
		}
		return nil
	case "use":
		if len(args) < 2 {
			return fmt.Errorf("usage: mirrin persona use <id>")
		}
		p, ok := persona.Find(ps, args[1])
		if !ok {
			return fmt.Errorf("no persona %q (see `mirrin persona list`)", args[1])
		}
		cfg.Persona = p.ID
		if len(args) > 2 {
			cfg.Name = strings.Join(args[2:], " ")
		} else if cfg.Name == "" || cfg.Name == "Mirrin" || persona.FormerDefault(cfg.Name) || persona.IsRetired(cfg.Name) {
			cfg.Name = p.Name
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Printf("your twin is now %s with the %q personality. Restart to apply: mirrin service restart\n", cfg.Name, p.ID)
		return nil
	case "new":
		if len(args) < 2 {
			return fmt.Errorf("usage: mirrin persona new <name>")
		}
		path, err := persona.Scaffold(config.Home(), strings.Join(args[1:], " "))
		if err != nil {
			return err
		}
		fmt.Println("wrote", path)
		fmt.Println("edit the character, then: mirrin persona use", strings.TrimSuffix(filepath.Base(path), ".yaml"))
		return nil
	}
	return fmt.Errorf("unknown persona command %q", sub)
}

// protocolsCmd manages the user's protocols and community packs.
func protocolsCmd(ctx context.Context, cfg *config.Config, args []string) error {
	dir := cfg.ProtocolsDir
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	reload := func() {
		if c := api.Connect(cfg.API.Listen, cfg.DataDir); c != nil {
			_, _ = c.Message(ctx, "cli", "terminal", "/reload")
		}
	}
	switch sub {
	case "list":
		ps, err := protocols.Load(dir)
		if err != nil {
			return err
		}
		if len(ps) == 0 {
			fmt.Println("no protocols. Try: mirrin protocols new <name>, or mirrin protocols search")
			return nil
		}
		for _, p := range ps {
			sched := p.Schedule
			if sched == "" {
				sched = "on demand"
			}
			src := "local"
			if p.Pack != "" {
				src = "pack:" + p.Pack
			}
			state := ""
			if !p.IsEnabled() {
				state = " (disabled)"
			}
			fmt.Printf("%-24s %-14s %-18s %s%s\n", p.Name, sched, src, p.Description, state)
		}
		packs, _ := protocols.InstalledPacks(dir)
		if len(packs) > 0 {
			fmt.Println("\ninstalled packs:")
			for _, pk := range packs {
				fmt.Printf("  %s  %s\n", pk.Name, pk.Description)
			}
		}
		return nil
	case "new":
		if len(args) < 2 {
			return fmt.Errorf("usage: mirrin protocols new <name>")
		}
		path, err := protocols.Scaffold(dir, strings.Join(args[1:], " "))
		if err != nil {
			return err
		}
		fmt.Println("wrote", path)
		fmt.Println("edit the prompt, then `mirrin protocols check`. To share it, put it in a pack (see docs/protocols/README.md).")
		reload()
		return nil
	case "lint":
		target := dir
		if len(args) > 1 {
			target = args[1]
			// A mistyped path would lint as an empty folder and pass.
			if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("there's no file or folder at %s", target)
			}
		}
		var known []string
		if d, err := daemon.New(cfg, daemon.Options{Headless: true, Version: version, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}); err == nil {
			known = d.Tools()
			d.Close()
		}
		var problems []protocols.Problem
		files := 0
		if st, err := os.Stat(target); err == nil && !st.IsDir() {
			problems, files = protocols.LintFile(target, known), 1
		} else {
			problems, files = protocols.LintDir(target, known)
		}
		for _, p := range problems {
			fmt.Println(" ", p)
		}
		fmt.Printf("%d file(s), %d finding(s)\n", files, len(problems))
		if protocols.Errors(problems) {
			os.Exit(1)
		}
		return nil
	case "check":
		ps, skipped := protocols.LoadAll(dir)
		for _, p := range skipped {
			fmt.Printf(" ✗ skipped %s: %s\n", p.File, p.Message)
		}
		d, err := daemon.New(cfg, daemon.Options{Headless: true, Version: version, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			return err
		}
		defer d.Close()
		have := d.Tools()
		problems := len(skipped)
		for _, p := range ps {
			miss, missingVars := p.Missing(have), p.Unset
			switch {
			case len(miss) > 0:
				problems++
				fmt.Printf(" ✗ %-24s needs: %s\n", p.Name, strings.Join(miss, ", "))
			case len(missingVars) > 0:
				problems++
				fmt.Printf(" ! %-24s set vars in %s: %s\n", p.Name, protocols.VarsPath(dir), strings.Join(missingVars, ", "))
			default:
				fmt.Printf(" ✓ %-24s ok\n", p.Name)
			}
		}
		if problems == 0 {
			fmt.Println("all protocols ready")
		}
		return nil
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: mirrin protocols add <git-url|path>")
		}
		name, err := protocols.AddPack(ctx, dir, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("installed pack %q\n", name)
		reload()
		return nil
	case "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: mirrin protocols remove <pack>")
		}
		if err := protocols.RemovePack(dir, args[1]); err != nil {
			return err
		}
		fmt.Println("removed", args[1])
		reload()
		return nil
	case "update":
		return updatePacks(ctx, dir, args[1:], reload)
	case "search", "install":
		reg, err := protocols.FetchRegistry(ctx, cfg.ProtocolRegistry)
		if err != nil {
			return fmt.Errorf("registry: %w", err)
		}
		if sub == "search" {
			term := strings.Join(args[1:], " ")
			hits := reg.Search(term)
			if len(hits) == 0 {
				fmt.Println("nothing matches")
				return nil
			}
			for _, p := range hits {
				fmt.Printf("%-20s %s\n%-20s %s  [%s]\n", p.Name, p.Description, "", p.Repo, strings.Join(p.Tags, ", "))
			}
			return nil
		}
		if len(args) < 2 {
			return fmt.Errorf("usage: mirrin protocols install <pack>")
		}
		pk, ok := reg.Find(args[1])
		if !ok {
			return fmt.Errorf("no pack %q in the registry (try `mirrin protocols search`)", args[1])
		}
		name, err := protocols.AddRegistryPack(ctx, dir, cfg.ProtocolRegistry, pk)
		if err != nil {
			return err
		}
		fmt.Printf("installed %q from %s\n", name, pk.Repo)
		reload()
		return nil
	}
	return fmt.Errorf("unknown protocols command %q", sub)
}

// trayBackend adapts the daemon to what the tray wants.
type trayBackend struct{ *daemon.Daemon }

func (t trayBackend) Protocols() []string {
	ps := t.Daemon.Protocols()
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

// runClient drives a local channel against an already-running daemon,
// streaming the reply when the channel can take it.
func runClient(ctx context.Context, ch channels.Channel, c *api.Client) error {
	err := ch.Start(ctx, func(ctx context.Context, in channels.Inbound) {
		if st, ok := ch.(channels.Streamer); ok {
			stream := st.OpenStream(ctx, in.ChatID)
			streamed := false
			var onNote func(string)
			if n, ok := stream.(channels.Noter); ok {
				onNote = n.Note
			}
			reply, err := c.MessageEvents(ctx, in.Channel, in.ChatID, in.Text, func(d string) { streamed = true; stream.Write(d) }, onNote)
			if err != nil {
				reply = clientErrorReply(err)
				streamed = false
			}
			if !streamed && reply != "" {
				stream.Write(reply)
			}
			stream.Close()
			return
		}
		reply, err := c.Message(ctx, in.Channel, in.ChatID, in.Text)
		if err != nil {
			reply = clientErrorReply(err)
		}
		if reply != "" {
			_ = ch.Send(ctx, in.ChatID, reply)
		}
	})
	if err == io.EOF {
		return nil
	}
	return err
}
