package main

// mirrin licenses prints the licences that go with this program, so every
// copy carries them, however it was installed: Mirrin's own (MIT), and the
// notices and full licence texts of the third-party code built into it.

import (
	"fmt"
	"io"

	"github.com/MavrkAI/Mirrin/internal/channels/whatsapp"
	"github.com/MavrkAI/Mirrin/internal/notices"
)

func init() { commands = append(commands, "licenses") }

func licensesCmd(out io.Writer) {
	ref, source := "main", "https://github.com/"+defaultRepo
	if isRelease(version) {
		ref = version
		source += "/releases/tag/" + version + " (Mirrin-" + version + "-source.tar.gz)"
	}
	fmt.Fprintf(out, "Mirrin %s\n\n", version)
	fmt.Fprintln(out, "Mirrin's own source code is under the MIT licence, below. This program also contains the Go standard library and third-party code under their own licences, which follow it with their full texts.")
	fmt.Fprintln(out, buildLicence())
	fmt.Fprintf(out, "What that means: https://github.com/%s/blob/%s/docs/licensing.md\n", defaultRepo, ref)
	fmt.Fprintf(out, "The complete source: %s\n\n", source)
	fmt.Fprintf(out, "==== LICENSE ====\n\n%s\n==== THIRD_PARTY_NOTICES ====\n\n%s", notices.License, notices.ThirdParty)
}

// buildLicence says in one line what this program is under: with WhatsApp it
// links GPL-3.0 code, so it is GPL-3.0 as a whole; without it, MIT. The
// notices that follow say which files keep a licence of their own.
func buildLicence() string {
	if whatsapp.Built {
		return "This build includes the WhatsApp channel, which links GPL-3.0 code, so it is distributed as a whole under GPL-3.0."
	}
	return "This build leaves out the WhatsApp channel, so the modules marked (WhatsApp) aren't in it, and it is distributed under MIT."
}
