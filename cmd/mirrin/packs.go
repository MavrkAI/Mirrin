package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/protocols"
	"golang.org/x/term"
)

// updatePacks shows what an update would change in each installed pack and
// applies it on a yes (or with --yes), so pack prompts never change unseen. It
// fails when any pack couldn't be checked or updated, so scripts can tell.
func updatePacks(ctx context.Context, dir string, args []string, reload func()) error {
	yes := slices.Contains(args, "--yes") || slices.Contains(args, "-y")
	ups, err := protocols.CheckPackUpdates(ctx, dir)
	if err != nil {
		return err
	}
	if len(ups) == 0 {
		fmt.Println("no packs installed. Find some with `mirrin protocols search`.")
		return nil
	}
	failed := 0
	var pending []protocols.PackUpdate
	for _, u := range ups {
		switch {
		case u.Err != nil:
			failed++
			fmt.Printf("%s: couldn't check for updates: %v\n", u.Dir, u.Err)
		case u.Note != "":
			fmt.Printf("%s: %s\n", u.Dir, u.Note)
		case !u.Pending():
			fmt.Printf("%s: up to date (%s)\n", u.Dir, protocols.ShortCommit(u.From))
		case u.Reconnect:
			// A copy (from an identity import): there's no local history to diff.
			fmt.Printf("%s: a copy; cloning it again from %s at %s so it updates\n", u.Dir, u.Source, protocols.ShortCommit(u.To))
			pending = append(pending, u)
		default:
			fmt.Printf("%s: %s → %s\n", u.Dir, protocols.ShortCommit(u.From), protocols.ShortCommit(u.To))
			for _, c := range u.Changes {
				fmt.Println("   ", c)
			}
			fmt.Printf("    full diff: git -C %s diff %s %s\n", filepath.Join(protocols.PacksDir(dir), u.Dir), protocols.ShortCommit(u.From), protocols.ShortCommit(u.To))
			pending = append(pending, u)
		}
	}
	if len(pending) > 0 && confirmUpdate(yes) {
		applied := 0
		for _, u := range pending {
			if err := protocols.ApplyPackUpdate(ctx, dir, u); err != nil {
				failed++
				fmt.Println(err)
				continue
			}
			applied++
		}
		fmt.Printf("updated %d pack(s)\n", applied)
		if applied > 0 {
			reload()
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d pack(s) couldn't be updated; see above", failed)
	}
	return nil
}

// confirmUpdate asks before pack updates are applied, unless --yes was given.
// With nobody at the terminal to ask, nothing is applied.
func confirmUpdate(yes bool) bool {
	if yes {
		return true
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Println("\nnothing changed yet. Run `mirrin protocols update --yes` to apply.")
		return false
	}
	fmt.Print("\napply these updates? [y/N] ")
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "y") {
		fmt.Println("nothing changed.")
		return false
	}
	return true
}
