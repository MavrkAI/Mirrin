package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/daemon"
	"github.com/MavrkAI/Mirrin/internal/identity"
)

const identityUsage = "usage: mirrin identity export [--with-conversations] <file.tar.gz>\n       mirrin identity import <file.tar.gz>"

// identityCmd runs `mirrin identity export|import`.
func identityCmd(args []string) error {
	if len(args) == 0 {
		return errors.New(identityUsage)
	}
	file, withConv, err := parseIdentityArgs(args[0], args[1:])
	if err != nil {
		return err
	}
	home := config.Home()
	switch args[0] {
	case "export":
		m, err := identity.Export(home, file, withConv)
		if err != nil {
			return err
		}
		printExport(os.Stdout, m, file)
	case "import":
		return importIdentity(context.Background(), os.Stdout, home, file)
	}
	return nil
}

// identityImport is identity.Import; tests watch it run.
var identityImport = identity.Import

// errTwinRunning is what an import says when a twin it can't stop holds the home.
var errTwinRunning = errors.New("your twin is running here. Quit it from the menu bar, then import again")

// importIdentity brings in the archive at file. A running twin would
// overwrite the import with its next save, so the background service is
// stopped for the moment and started again after, and the home's claim is
// held throughout, so no twin can start in between. A twin running outside
// the service (the menu bar app started by hand) is asked to quit.
func importIdentity(ctx context.Context, out io.Writer, home, file string) error {
	listen, dataDir := identity.LocalAPI(home)
	cfg, _ := config.Load()
	if cfg == nil {
		cfg = config.Default()
	}
	svc := memoryDeps.serviceInstalled()
	if svc {
		fmt.Fprintln(out, "Pausing the background service while your twin is imported…")
		_ = memoryDeps.stopService(cfg) // it may not have been running; the claim below still keeps a twin out
	}
	restart := func() {
		if !svc {
			return
		}
		if c, _ := config.Load(); c != nil {
			cfg = c // the imported settings
		}
		if err := memoryDeps.startService(cfg); err != nil {
			fmt.Fprintln(out, "The background service didn't start again. Start it with: mirrin service start")
			return
		}
		fmt.Fprintln(out, "Your twin is running again in the background.")
	}
	// Only a service being stopped is waited for; a twin outside it (the
	// menu bar app, or one started by hand) is refused straight away.
	var wait time.Duration
	if svc {
		wait = memoryDeps.lockWait
	}
	release, err := daemon.LockHome(ctx, dataDir, wait)
	if err != nil {
		restart()
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			return errTwinRunning
		}
		return err
	}
	if listen != "" && apiAnswers(listen, dataDir) {
		// Something answers as this twin without holding its claim.
		release()
		restart()
		return errTwinRunning
	}
	r, err := identityImport(home, file)
	release()
	if err != nil {
		restart()
		return err
	}
	printImport(out, r)
	switch {
	case !svc:
		fmt.Fprintln(out, "Start your twin again when you're ready.")
	case len(importWarnings(r)) > 0:
		// Something above needs a look before the twin runs with it.
		fmt.Fprintln(out, "The background service stays paused so you can check the above first. Then start it with: mirrin service start")
	default:
		restart()
	}
	return nil
}

// apiAnswers reports whether a twin answers on listen; tests replace it.
var apiAnswers = func(listen, dataDir string) bool { return api.Connect(listen, dataDir) != nil }

// parseIdentityArgs takes the file and flags in any order.
func parseIdentityArgs(sub string, args []string) (file string, withConv bool, err error) {
	if sub != "export" && sub != "import" {
		return "", false, errors.New(identityUsage)
	}
	for _, a := range args {
		switch {
		case a == "--with-conversations" && sub == "export":
			withConv = true
		case strings.HasPrefix(a, "-") && a != "-":
			return "", false, fmt.Errorf("unknown option %s\n%s", a, identityUsage)
		case file != "":
			return "", false, fmt.Errorf("one archive at a time\n%s", identityUsage)
		default:
			file = a
		}
	}
	if file == "" {
		return "", false, errors.New(identityUsage)
	}
	return file, withConv, nil
}

// twinRunning reports whether a twin is running from this home: it answers
// on this machine's API, or holds the home's claim (a twin with the API off,
// or whose address another program took). An import under a running twin
// would be overwritten by its next save. The address and data folder come
// straight from config.yaml, so a config that no longer loads (or one with a
// custom data_dir) still finds its twin.
func twinRunning() bool {
	listen, dataDir := identity.LocalAPI(config.Home())
	return listen != "" && api.Connect(listen, dataDir) != nil || daemon.HomeInUse(dataDir)
}

func printExport(w io.Writer, m identity.Manifest, file string) {
	var personas, protocols int
	memory := ""
	for _, f := range m.Files {
		switch {
		case strings.HasPrefix(f, "personas/"):
			personas++
		case strings.HasPrefix(f, "protocols/"):
			protocols++
		case f == "memory.yaml":
			memory = "memory and portrait"
		case f == "data/memory.db":
			memory = "memory and every conversation"
		}
	}
	parts := []string{"settings"}
	if memory != "" {
		parts = append(parts, memory)
	}
	if personas > 0 {
		parts = append(parts, plural(personas, "persona"))
	}
	if protocols > 0 {
		parts = append(parts, plural(protocols, "protocol file"))
	}
	fmt.Fprintf(w, "Exported %s to %s: %s.\n", twinLabel(m), file, strings.Join(parts, ", "))
	var secrets, env int
	for _, s := range m.Secrets {
		if _, _, ok := identity.MCPEnvLabel(s); ok {
			env++
		} else {
			secrets++
		}
	}
	switch secrets {
	case 0:
		fmt.Fprintln(w, "No keys, tokens or passwords are in it.")
	case 1:
		fmt.Fprintln(w, "1 key, token or password was left out; you'll set it on the new machine.")
	default:
		fmt.Fprintf(w, "%d keys, tokens and passwords were left out; you'll set them on the new machine.\n", secrets)
	}
	switch env {
	case 0:
	case 1:
		fmt.Fprintln(w, "1 add-on tool (MCP server) setting was left out too, in case it's secret.")
	default:
		fmt.Fprintf(w, "%d add-on tool (MCP server) settings were left out too, in case they're secret.\n", env)
	}
	if m.Conversations {
		fmt.Fprintln(w, "It holds every conversation, so keep it somewhere private.")
	}
	fmt.Fprintf(w, "On the other machine: mirrin identity import %s\n", filepath.Base(file))
}

func printImport(w io.Writer, r identity.Result) {
	fmt.Fprintf(w, "Imported %s.\n", twinLabel(r.Manifest))
	if r.Backup != "" {
		fmt.Fprintf(w, "What it replaced is saved in %s.\n", r.Backup)
	}
	if len(r.Reconnected) > 0 {
		fmt.Fprintf(w, "Downloaded %s again from where it came from, so it keeps updating: %s.\n", plural(len(r.Reconnected), "pack"), clean(strings.Join(r.Reconnected, ", ")))
	}
	warnings := importWarnings(r)
	if len(warnings) > 0 {
		fmt.Fprintln(w, "Check this before you start your twin:")
		for _, s := range warnings {
			fmt.Fprintln(w, "  - "+clean(s))
		}
	}
	var todo []string
	if len(r.Missing) > 0 {
		todo = append(todo, "add the keys and passwords that stayed on the old computer, in the settings file (config.yaml): "+clean(strings.Join(r.Missing, ", ")))
	}
	if len(r.MCPEnv) > 0 {
		servers := make([]string, 0, len(r.MCPEnv))
		for s := range r.MCPEnv {
			servers = append(servers, s)
		}
		sort.Strings(servers)
		var parts []string
		for _, s := range servers {
			parts = append(parts, s+": "+strings.Join(r.MCPEnv[s], ", "))
		}
		todo = append(todo, "fill in the add-on tool (MCP server) settings that stayed on the old computer, in the settings file (config.yaml): "+clean(strings.Join(parts, "; ")))
	}
	for _, s := range r.Relink {
		todo = append(todo, clean(s))
	}
	for _, p := range r.Packs {
		todo = append(todo, fmt.Sprintf("pack %s came as a copy, and its source couldn't be reached just now; `mirrin protocols update` connects it again once it can", clean(p.Dir)))
	}
	if len(todo) > 0 {
		fmt.Fprintln(w, "Still to do here:")
		for _, t := range todo {
			fmt.Fprintln(w, "  - "+t)
		}
	}
	if len(r.Skipped) > 0 {
		names := r.Skipped
		if len(names) > 5 {
			names = append(names[:5:5], fmt.Sprintf("and %d more", len(r.Skipped)-5))
		}
		fmt.Fprintf(w, "Left out %s this version doesn't import: %s\n", plural(len(r.Skipped), "file"), clean(strings.Join(names, ", ")))
	}
}

// importWarnings are the things to check before the imported twin starts.
func importWarnings(r identity.Result) []string {
	warnings := r.Warnings
	if r.Manifest.Conversations {
		// The database came whole: its reminders, background tasks and
		// pending approvals carry on here once the twin starts.
		warnings = append(warnings, "its reminders and background tasks came too, and carry on here when your twin starts: quit the twin on the old machine first, or they'll happen twice")
	}
	return warnings
}

func twinLabel(m identity.Manifest) string {
	name := clean(m.Twin)
	if name == "" {
		name = "your twin"
	}
	if m.Persona != "" {
		name += " (persona " + clean(m.Persona) + ")"
	}
	return name
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// clean drops control characters from text that came out of an archive, so a
// crafted one cannot rewrite the terminal.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
