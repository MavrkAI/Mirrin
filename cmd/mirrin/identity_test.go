package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/identity"
)

func TestParseIdentityArgs(t *testing.T) {
	cases := []struct {
		sub      string
		args     []string
		file     string
		withConv bool
		wantErr  string
	}{
		{sub: "export", args: []string{"twin.tar.gz"}, file: "twin.tar.gz"},
		{sub: "export", args: []string{"twin.tar.gz", "--with-conversations"}, file: "twin.tar.gz", withConv: true},
		// The flag used to work only after the file; before it, it became the file name.
		{sub: "export", args: []string{"--with-conversations", "twin.tar.gz"}, file: "twin.tar.gz", withConv: true},
		{sub: "import", args: []string{"twin.tar.gz"}, file: "twin.tar.gz"},
		{sub: "import", args: []string{"--with-conversations", "twin.tar.gz"}, wantErr: "unknown option"},
		{sub: "export", args: []string{"--conversations", "twin.tar.gz"}, wantErr: "unknown option"},
		{sub: "export", args: []string{"a.tar.gz", "b.tar.gz"}, wantErr: "one archive at a time"},
		{sub: "export", args: nil, wantErr: "usage"},
		{sub: "backup", args: []string{"twin.tar.gz"}, wantErr: "usage"},
	}
	for _, tc := range cases {
		t.Run(tc.sub+" "+strings.Join(tc.args, " "), func(t *testing.T) {
			file, withConv, err := parseIdentityArgs(tc.sub, tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || file != tc.file || withConv != tc.withConv {
				t.Fatalf("got %q %v %v", file, withConv, err)
			}
		})
	}
}

func TestCleanDropsTerminalControl(t *testing.T) {
	if got := clean("Jee\x1b[2Jves\n"); got != "Jee[2Jves" {
		t.Fatalf("got %q", got)
	}
}

// MCP env values are left out in case they're secret, but many are not
// (LOG_LEVEL), so they are counted and listed apart from keys and tokens.
func TestIdentityReportsMCPEnvApart(t *testing.T) {
	var out bytes.Buffer
	printExport(&out, identity.Manifest{Twin: "Jeeves", Secrets: []string{"mcp.servers.fs.env.LOG_LEVEL", "mcp.servers.fs.env.NODE_ENV"}}, "twin.tar.gz")
	for _, want := range []string{"No keys, tokens or passwords are in it.", "2 add-on tool (MCP server) settings were left out too"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("export says:\n%s\nwant %q", out.String(), want)
		}
	}

	out.Reset()
	printImport(&out, identity.Result{
		Manifest: identity.Manifest{Twin: "Jeeves"},
		Missing:  []string{"channels.telegram.token"},
		MCPEnv:   map[string][]string{"fs": {"LOG_LEVEL", "NODE_ENV"}, "db": {"PGPASSWORD"}},
		Warnings: []string{"llm.base_url is now https://proxy.example/v1, from the archive, and your key here will be sent there; if you don't recognise it, change it in config.yaml"},
	})
	got := out.String()
	for _, want := range []string{
		"Check this before you start your twin:\n  - llm.base_url is now https://proxy.example/v1",
		"add the keys and passwords that stayed on the old computer, in the settings file (config.yaml): channels.telegram.token\n",
		"fill in the add-on tool (MCP server) settings that stayed on the old computer, in the settings file (config.yaml): db: PGPASSWORD; fs: LOG_LEVEL, NODE_ENV\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("import says:\n%s\nwant %q", got, want)
		}
	}
}

// A twin imported with its conversations brings its reminders and running
// tasks: the owner is told to stop the old twin so nothing happens twice. A
// pack the import couldn't clone again stays a copy until protocols update
// reaches its source.
func TestImportSaysWhatCarriesOnAndHowToReconnectPacks(t *testing.T) {
	var out bytes.Buffer
	printImport(&out, identity.Result{
		Manifest: identity.Manifest{Twin: "Jeeves", Conversations: true},
		Packs: []identity.PackSource{
			{Dir: "news", Repo: "https://github.com/x/news.git", Ref: "v1.2.0"},
			{Dir: "starter", Repo: "https://github.com/x/starter.git", Registry: "starter"},
		},
	})
	got := out.String()
	for _, want := range []string{
		"quit the twin on the old machine first",
		"pack news came as a copy, and its source couldn't be reached just now; `mirrin protocols update` connects it again",
		"pack starter came as a copy",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("import says:\n%s\nwant %q", got, want)
		}
	}
	out.Reset()
	printImport(&out, identity.Result{Manifest: identity.Manifest{Twin: "Jeeves"}})
	if strings.Contains(out.String(), "old machine first") {
		t.Fatalf("a facts-only import brings no tasks: %s", out.String())
	}
}
