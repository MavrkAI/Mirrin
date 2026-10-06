package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/channels/whatsapp"
)

// Every copy of mirrin carries its licences, however it was installed.
func TestLicenses(t *testing.T) {
	saved := version
	t.Cleanup(func() { version = saved })
	for v, source := range map[string]string{
		"v0.3.0": "https://github.com/MavrkAI/Mirrin/releases/tag/v0.3.0 (Mirrin-v0.3.0-source.tar.gz)",
		"dev":    "The complete source: https://github.com/MavrkAI/Mirrin\n",
	} {
		version = v
		var out bytes.Buffer
		licensesCmd(&out)
		for _, want := range []string{"Mirrin " + v + "\n", source, "under the MIT licence", buildLicence(), "==== LICENSE ====", "MIT License", "==== THIRD_PARTY_NOTICES ====", "Go standard library and runtime (BSD-3-Clause)", "github.com/hashicorp/yamux (MPL-2.0)"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s: no %q in the output", v, want)
			}
		}
	}
}

// The program says plainly which build it is: GPL-3.0 as a whole with
// WhatsApp, MIT without it (CI runs this with -tags nowhatsapp too).
func TestBuildLicence(t *testing.T) {
	want := "distributed under MIT"
	if whatsapp.Built {
		want = "distributed as a whole under GPL-3.0"
	}
	if got := buildLicence(); !strings.Contains(got, want) {
		t.Fatalf("buildLicence() = %q, want %q in it", got, want)
	}
}
