package identity

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// twinHome makes a home the way first run does (config.Default() without
// WhatsApp, saved), with edit applied, and points MIRRIN_HOME at it.
func twinHome(t *testing.T, edit func(c *config.Config)) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	c := config.Default()
	c.Channels.WhatsApp.Enabled = false
	if edit != nil {
		edit(c)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	return home
}

// loadConfig loads home's config exactly as `mirrin` would.
func loadConfig(t *testing.T, home string) *config.Config {
	t.Helper()
	t.Setenv("MIRRIN_HOME", home)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config does not load after import: %v", err)
	}
	return cfg
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// readArchive returns every file in a tar.gz by name.
func readArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[hdr.Name] = b
	}
}

type tarFile struct {
	name, body string
	typ        byte
	size       int64 // declared size when it differs from the body (a cut-short archive)
}

// makeArchive writes a hand-built archive.
func makeArchive(t *testing.T, files []tarFile) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		hdr := &tar.Header{Name: f.name, Mode: 0o600, Size: int64(len(f.body)), Typeflag: tar.TypeReg}
		if f.typ != 0 {
			hdr.Typeflag = f.typ
			if f.typ == tar.TypeSymlink {
				hdr.Linkname, hdr.Size = f.body, 0
			}
		}
		if f.size != 0 {
			hdr.Size = f.size
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte(f.body))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	if err := os.WriteFile(out, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}

func manifestYAML(format int, extra string) string {
	return "format: " + string(rune('0'+format)) + "\ntwin: Jeeves\npersona: mavrk\n" + extra
}

// fillSecrets puts a recognisable value in every secret text field of v.
func fillSecrets(v reflect.Value, prefix string) {
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		name := strings.Split(f.Tag.Get("yaml"), ",")[0]
		p := strings.TrimPrefix(prefix+"."+name, ".")
		switch fv := v.Field(i); fv.Kind() {
		case reflect.Struct:
			fillSecrets(fv, p)
		case reflect.String:
			if isSecretKey(name) {
				fv.SetString("SECRET-" + p)
			}
		}
	}
}

func facts(t *testing.T, dataDir string) []string {
	t.Helper()
	st, err := memory.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fs, err := st.AllFacts(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range fs {
		out = append(out, f.Content)
	}
	sort.Strings(out)
	return out
}

func TestMoveTwinToAnotherMachine(t *testing.T) {
	user, _ := os.UserHomeDir()
	for _, env := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "TELEGRAM_BOT_TOKEN", "AWS_SECRET_ACCESS_KEY", "NOTION_SECRET", "STRIPE_SECRET_KEY", "GITHUB_PERSONAL_ACCESS_TOKEN"} {
		t.Setenv(env, "")
	}

	src := twinHome(t, func(c *config.Config) {
		c.Name, c.User.Name = "Jeeves", "Akshay"
		c.LLM.MaxTokens = 32000 // a number whose name looks secret; set, so it's saved
		fillSecrets(reflect.ValueOf(c).Elem(), "")
		for name, p := range c.LLM.Providers {
			p.APIKey = "SECRET-provider-" + name
			c.LLM.Providers[name] = p
		}
		c.LLM.BaseURL = "https://me:SECRET-in-url@llm.example.com/v1"
		c.MCP.Servers = []config.MCPServer{{
			Name:    "github",
			Command: "npx",
			Args:    []string{"-y", "server-github", "--api-key=SECRET-flag", "--token", "SECRET-flag2", "--header", "Authorization: Bearer SECRET-bearer"},
			Env: map[string]string{
				"GITHUB_PERSONAL_ACCESS_TOKEN": "SECRET-ghp",
				"AWS_SECRET_ACCESS_KEY":        "SECRET-aws",
				"NOTION_SECRET":                "SECRET-notion",
				"STRIPE_SECRET_KEY":            "SECRET-stripe",
			},
			// Tool names are the user's: these are risks, not passwords.
			ToolRisk: map[string]string{"get_password": "dangerous", "rotate_token": "dangerous"},
		}, {
			Name:    "db",
			Command: "npx",
			Args:    []string{"-y", "@modelcontextprotocol/server-postgres", "postgresql://admin:SECRET-pg@db/prod"},
		}, {
			Name:    "docker",
			Command: "docker",
			Args:    []string{"run", "-i", "--rm", "-e", "GITHUB_PERSONAL_ACCESS_TOKEN=SECRET-docker", "ghcr.io/github/github-mcp-server"},
		}, {
			Name:    "shell",
			Command: "sh",
			Args:    []string{"-c", "API_KEY=SECRET-sh exec notes-mcp --no-password /srv/notes"},
		}}
		c.API = config.API{Listen: "100.64.0.1:7742", Remote: true}
		c.Channels.Voice.ChimeSound = filepath.Join(config.Home(), "sounds", "blip.aiff")
		if user != "" {
			c.Skills.System.AllowedDirs = []string{filepath.Join(user, "Projects")}
		}
	})
	writeFile(t, filepath.Join(src, "personas", "me.yaml"), "name: Me\ncharacter: calm\n")
	writeFile(t, filepath.Join(src, "protocols", "x.yaml"), "name: x\nprompt: new\n")
	writeFile(t, filepath.Join(src, "protocols", "vars.yaml"), "x:\n  topic: tea\n")
	writeFile(t, filepath.Join(src, "tools", "evil", "tool.yaml"), "name: evil\nrisk: read\n")
	writeFile(t, filepath.Join(src, "remote.yaml"), "address: 1.2.3.4:7742\ntoken: SECRET-remote\n")
	writeFile(t, filepath.Join(src, "data", "api.token"), "SECRET-api-token")
	st, err := memory.Open(filepath.Join(src, "data"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.Remember(context.Background(), "user", "Likes tea.", "t")
	_ = st.SetPortrait(context.Background(), "A tea person.")
	st.Close()

	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	m, err := Export(src, out, false)
	if err != nil {
		t.Fatal(err)
	}
	if m.Twin != "Jeeves" || m.Persona != "mirrin" || m.User != "Akshay" || m.Format != Format {
		t.Fatalf("manifest %+v", m)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
			t.Fatalf("archive mode %v, want 0600", st.Mode().Perm())
		}
	}
	files := readArchive(t, out)
	for name, b := range files {
		if strings.Contains(string(b), "SECRET-") {
			t.Errorf("%s in the archive carries a secret:\n%s", name, b)
		}
		if !importable(name) {
			t.Errorf("archive holds %s, which is not part of a twin", name)
		}
	}
	cfgOut := string(files["config.yaml"])
	if strings.Contains(cfgOut, src) || !strings.Contains(cfgOut, "max_tokens: 32000") {
		t.Fatalf("exported config keeps machine paths or loses max_tokens:\n%s", cfgOut)
	}
	if !strings.Contains(cfgOut, "$MIRRIN_HOME/sounds/blip.aiff") || (user != "" && !strings.Contains(cfgOut, "~/Projects")) {
		t.Fatalf("exported paths are not portable:\n%s", cfgOut)
	}
	for _, want := range []string{"llm.providers.openai.api_key", "mcp.servers.github.env.AWS_SECRET_ACCESS_KEY", "mcp.servers.github.args[2]", "llm.base_url", "skills.email.password",
		"mcp.servers.db.args[2]", "mcp.servers.docker.args[4]", "mcp.servers.shell.args[1]"} {
		if !contains(m.Secrets, want) {
			t.Errorf("manifest secrets %v lack %s", m.Secrets, want)
		}
	}
	for _, not := range []string{"mcp.servers.github.tool_risk.get_password", "mcp.servers.github.tool_risk.rotate_token"} {
		if contains(m.Secrets, not) {
			t.Errorf("%s is a tool's risk, not a secret", not)
		}
	}

	// The destination has its own keys, a custom data folder and a protocol
	// the archive replaces.
	dst := twinHome(t, func(c *config.Config) {
		c.Name = "Old"
		p := c.LLM.Providers["anthropic"]
		p.APIKey = "LOCAL-anthropic"
		c.LLM.Providers["anthropic"] = p
		c.DataDir = filepath.Join(config.Home(), "elsewhere")
		c.MCP.Servers = []config.MCPServer{
			{Name: "github", Command: "npx", Args: []string{"-y", "server-github", "--api-key=LOCAL-flag", "--token", "LOCAL-flag2", "--header", "Authorization: Bearer LOCAL-bearer"}, Env: map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": "LOCAL-ghp"}},
			{Name: "db", Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-postgres", "postgresql://admin:LOCAL-pg@db/prod"}},
			{Name: "notes", Command: "notes-mcp", Env: map[string]string{"NOTES_TOKEN": "LOCAL-notes"}},
		}
	})
	writeFile(t, filepath.Join(dst, "protocols", "x.yaml"), "name: x\nprompt: old\n")
	here, err := memory.Open(filepath.Join(dst, "elsewhere"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = here.Remember(context.Background(), "user", "Likes coffee.", "t")
	_ = here.SetPortrait(context.Background(), "A coffee person.")
	here.Close()

	r, err := Import(dst, out)
	if err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(t, dst)
	if cfg.Name != "Jeeves" || cfg.LLM.MaxTokens != 32000 {
		t.Fatalf("imported twin %q, max_tokens %d", cfg.Name, cfg.LLM.MaxTokens)
	}
	if cfg.DataDir != filepath.Join(dst, "elsewhere") || cfg.ProtocolsDir != filepath.Join(dst, "protocols") {
		t.Fatalf("machine folders not kept: data %s, protocols %s", cfg.DataDir, cfg.ProtocolsDir)
	}
	if cfg.API.Listen != "127.0.0.1:7742" || cfg.API.Remote {
		t.Fatalf("API binding came from the old machine: %+v", cfg.API)
	}
	if cfg.Channels.Voice.ChimeSound != filepath.Join(dst, "sounds", "blip.aiff") {
		t.Fatalf("chime path not rewritten: %s", cfg.Channels.Voice.ChimeSound)
	}
	if user != "" && cfg.Skills.System.AllowedDirs[0] != filepath.Join(user, "Projects") {
		t.Fatalf("allowed dir %v", cfg.Skills.System.AllowedDirs)
	}
	if got := cfg.LLM.Providers["anthropic"].APIKey; got != "LOCAL-anthropic" {
		t.Fatalf("local key lost: %q", got)
	}
	servers := map[string]config.MCPServer{}
	var order []string
	for _, s := range cfg.MCP.Servers {
		servers[s.Name] = s
		order = append(order, s.Name)
	}
	if !reflect.DeepEqual(order, []string{"github", "db", "docker", "shell", "notes"}) {
		t.Fatalf("MCP servers %v", order)
	}
	gh := servers["github"]
	if gh.Env["GITHUB_PERSONAL_ACCESS_TOKEN"] != "LOCAL-ghp" || !reflect.DeepEqual(gh.Args, []string{"-y", "server-github", "--api-key=LOCAL-flag", "--token", "LOCAL-flag2", "--header", "Authorization: Bearer LOCAL-bearer"}) {
		t.Fatalf("local MCP secrets not kept: %+v", gh)
	}
	if _, blank := gh.Env["AWS_SECRET_ACCESS_KEY"]; blank {
		t.Fatalf("a blank env value would be passed to the server as an empty variable: %+v", gh.Env)
	}
	if gh.ToolRisk["get_password"] != "dangerous" || gh.ToolRisk["rotate_token"] != "dangerous" {
		t.Fatalf("tool risks lost, so the approvals gate would ask less: %v", gh.ToolRisk)
	}
	if got := servers["db"].Args[2]; got != "postgresql://admin:LOCAL-pg@db/prod" {
		t.Fatalf("this machine's database password was not kept: %s", got)
	}
	if got := servers["docker"].Args[4]; got != "GITHUB_PERSONAL_ACCESS_TOKEN=" {
		t.Fatalf("docker token arg %q", got)
	}
	if got := servers["shell"].Args[1]; got != "API_KEY= exec notes-mcp --no-password /srv/notes" {
		t.Fatalf("shell command %q", got)
	}
	if servers["notes"].Env["NOTES_TOKEN"] != "LOCAL-notes" {
		t.Fatalf("a server only this machine has was dropped: %+v", cfg.MCP.Servers)
	}
	// A URL whose password was cut out is missing too, not just blank keys.
	for _, want := range []string{"llm.providers.openai.api_key", "channels.telegram.token", "llm.base_url", "mcp.servers.docker.args[4]", "mcp.servers.shell.args[1]"} {
		if !contains(r.Missing, want) {
			t.Errorf("missing %v lacks %s", r.Missing, want)
		}
	}
	for _, not := range []string{"llm.providers.anthropic.api_key", "mcp.servers.github.env.GITHUB_PERSONAL_ACCESS_TOKEN", "mcp.servers.github.args[2]",
		"mcp.servers.db.args[2]", "mcp.servers.github.env.AWS_SECRET_ACCESS_KEY", "mcp.servers.github.tool_risk.get_password"} {
		if contains(r.Missing, not) {
			t.Errorf("%s is set here, or not a secret, but reported missing", not)
		}
	}
	if len(r.Warnings) != 1 || !strings.HasPrefix(r.Warnings[0], "llm.base_url is now https://llm.example.com/v1") {
		t.Errorf("the archive points this machine's key at a new endpoint, but warnings are %v", r.Warnings)
	}
	// MCP env values are reported on their own: some are not secret at all.
	if want := []string{"AWS_SECRET_ACCESS_KEY", "NOTION_SECRET", "STRIPE_SECRET_KEY"}; !reflect.DeepEqual(r.MCPEnv["github"], want) || len(r.MCPEnv) != 1 {
		t.Errorf("MCP env to fill in %v, want github: %v", r.MCPEnv, want)
	}
	if strings.Contains(readFile(t, filepath.Join(dst, "config.yaml")), "SECRET-") {
		t.Fatal("a source secret reached the destination")
	}
	if got := facts(t, cfg.DataDir); !reflect.DeepEqual(got, []string{"Likes coffee.", "Likes tea."}) {
		t.Fatalf("facts %v", got)
	}
	if !strings.Contains(readFile(t, filepath.Join(r.Backup, "memory.yaml")), "A coffee person.") {
		t.Fatal("the replaced portrait was not backed up")
	}
	if got := readFile(t, filepath.Join(dst, "protocols", "x.yaml")); !strings.Contains(got, "new") {
		t.Fatalf("protocol not imported: %s", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "personas", "me.yaml")); err != nil {
		t.Fatal("persona not imported")
	}
	for _, p := range []string{"tools", "remote.yaml", filepath.Join("data", "api.token")} {
		if _, err := os.Stat(filepath.Join(dst, p)); err == nil {
			t.Errorf("%s came across", p)
		}
	}
	if r.Backup == "" || !strings.Contains(readFile(t, filepath.Join(r.Backup, "protocols", "x.yaml")), "old") ||
		!strings.Contains(readFile(t, filepath.Join(r.Backup, "config.yaml")), "name: Old") {
		t.Fatalf("replaced files not backed up in %q", r.Backup)
	}

	// Importing the same archive again changes nothing further.
	before := readFile(t, filepath.Join(dst, "config.yaml"))
	if _, err := Import(dst, out); err != nil {
		t.Fatal(err)
	}
	if after := readFile(t, filepath.Join(dst, "config.yaml")); after != before {
		t.Fatalf("second import changed the config:\n%s\n---\n%s", before, after)
	}
	if got := facts(t, cfg.DataDir); len(got) != 2 {
		t.Fatalf("second import duplicated facts: %v", got)
	}
}

func TestImportOnFreshMachine(t *testing.T) {
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("VAULT_TOKEN", "")
	src := twinHome(t, func(c *config.Config) {
		c.Name = "Jeeves"
		c.MCP.Servers = []config.MCPServer{{
			Name: "vault", Command: "vault-mcp",
			Env:      map[string]string{"LOG_LEVEL": "debug", "VAULT_TOKEN": "SECRET-vault"},
			ToolRisk: map[string]string{"get_password": "dangerous", "list_credentials": "read"},
		}}
	})
	st, _ := memory.Open(filepath.Join(src, "data"))
	_, _ = st.Remember(context.Background(), "user", "Likes tea.", "t")
	st.Close()
	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	if _, err := Export(src, out, false); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "new-home")
	t.Setenv("MIRRIN_HOME", dst)
	t.Setenv("VAULT_TOKEN", "this-machines-token") // set here: passed on, not reported
	r, err := Import(dst, out)
	if err != nil {
		t.Fatal(err)
	}
	if r.Backup != "" {
		t.Fatalf("nothing was replaced, yet a backup was made: %s", r.Backup)
	}
	cfg := loadConfig(t, dst)
	if cfg.Name != "Jeeves" || cfg.DataDir != filepath.Join(dst, "data") || cfg.ProtocolsDir != filepath.Join(dst, "protocols") {
		t.Fatalf("fresh import: name %s data %s protocols %s", cfg.Name, cfg.DataDir, cfg.ProtocolsDir)
	}
	if got := facts(t, cfg.DataDir); len(got) != 1 {
		t.Fatalf("facts %v", got)
	}
	// A tool the user marked dangerous stays dangerous; blank would mean write.
	if v := cfg.MCP.Servers[0].ToolRisk; v["get_password"] != "dangerous" || v["list_credentials"] != "read" {
		t.Fatalf("tool risks %v", v)
	}
	if len(r.Missing) != 0 || !reflect.DeepEqual(r.MCPEnv, map[string][]string{"vault": {"LOG_LEVEL"}}) {
		t.Fatalf("to set: missing %v, MCP env %v", r.Missing, r.MCPEnv)
	}
	if _, blank := cfg.MCP.Servers[0].Env["LOG_LEVEL"]; blank {
		t.Fatal("a blank env value would be passed to the server as an empty variable")
	}
	// A server gets only its own env block (procenv), so a variable this
	// machine has is named for it, not left to an environment it won't see.
	if got := cfg.MCP.Servers[0].Env["VAULT_TOKEN"]; got != "$VAULT_TOKEN" {
		t.Fatalf("VAULT_TOKEN, set on this machine, is %q in the config, want $VAULT_TOKEN", got)
	}
	if entries, _ := os.ReadDir(dst); len(entries) == 0 {
		t.Fatal("nothing imported")
	} else {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".import-") {
				t.Fatalf("staging folder left behind: %s", e.Name())
			}
		}
	}
}

func TestConversationsTravelWhileTwinRuns(t *testing.T) {
	ctx := context.Background()
	src := twinHome(t, nil)
	// The running twin keeps the database open; recent writes sit in the WAL.
	live, err := memory.Open(filepath.Join(src, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	_, _ = live.Remember(ctx, "user", "Likes tea.", "t")
	_ = live.AppendMessage(ctx, "cli:me", llm.Text(llm.RoleUser, "remember the tea"))

	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	m, err := Export(src, out, true)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Conversations || !contains(m.Files, "data/memory.db") {
		t.Fatalf("manifest %+v", m)
	}

	dst := twinHome(t, nil)
	here, err := memory.Open(filepath.Join(dst, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer here.Close()
	_, _ = here.Remember(ctx, "user", "Likes coffee.", "t")
	_ = here.AppendMessage(ctx, "cli:me", llm.Text(llm.RoleUser, "coffee please"))

	r, err := Import(dst, out)
	if err != nil {
		t.Fatal(err)
	}
	// The open connection sees the restored memory, not a file swapped under it.
	fs, err := here.AllFacts(ctx, 10)
	if err != nil || len(fs) != 1 || fs[0].Content != "Likes tea." {
		t.Fatalf("facts after restore: %+v %v", fs, err)
	}
	h, _ := here.History(ctx, "cli:me", 10)
	if len(h) != 1 || !strings.Contains(h[0].PlainText(), "tea") {
		t.Fatalf("history after restore: %+v", h)
	}
	if err := checkDB(filepath.Join(dst, "data", "memory.db")); err != nil {
		t.Fatalf("database damaged: %v", err)
	}
	if got := facts(t, filepath.Join(r.Backup, "data")); !reflect.DeepEqual(got, []string{"Likes coffee."}) {
		t.Fatalf("backup of the replaced memory holds %v", got)
	}
}

func TestImportOnlyTakesTwinFiles(t *testing.T) {
	manifest := tarFile{name: "manifest.yaml", body: manifestYAML(2, "")}
	cases := []struct {
		name    string
		file    tarFile
		wantErr string
		skipped bool
		lands   string // where an accepted file ends up, relative to home
	}{
		{name: "persona", file: tarFile{name: "personas/me.yaml", body: "name: Me\ncharacter: c\n"}, lands: "personas/me.yaml"},
		{name: "pack protocol", file: tarFile{name: "protocols/packs/p/a.yaml", body: "name: a\nprompt: b\n"}, lands: "protocols/packs/p/a.yaml"},
		{name: "protocol variables", file: tarFile{name: "protocols/vars.yaml", body: "a:\n  x: y\n"}, lands: "protocols/vars.yaml"},
		{name: "pack metadata", file: tarFile{name: "protocols/packs/p/pack.yaml", body: "name: p\n"}, lands: "protocols/packs/p/pack.yaml"},
		{name: "pack licence", file: tarFile{name: "protocols/packs/p/LICENSE", body: "MIT"}, lands: "protocols/packs/p/LICENSE"},
		{name: "pack protocols folder", file: tarFile{name: "protocols/packs/p/protocols/a.yaml", body: "name: a\nprompt: b\n"}, lands: "protocols/packs/p/protocols/a.yaml"},
		{name: "pack persona", file: tarFile{name: "protocols/packs/p/personas/a.yml", body: "name: A\ncharacter: c\n"}, lands: "protocols/packs/p/personas/a.yml"},
		{name: "pack script", file: tarFile{name: "protocols/packs/p/scripts/run.sh", body: "#!/bin/sh\n"}, skipped: true},
		{name: "pack provenance", file: tarFile{name: "protocols/packs/p/.mirrin-pack.json", body: "{}"}, skipped: true},
		{name: "pack being installed", file: tarFile{name: "protocols/packs/.p.installing/a.yaml", body: "name: a\n"}, skipped: true},
		{name: "text file", file: tarFile{name: "protocols/notes.txt", body: "x"}, skipped: true},
		{name: "deep folder", file: tarFile{name: "protocols/sub/dir/a.yaml", body: "name: a\n"}, skipped: true},
		{name: "database under protocols", file: tarFile{name: "protocols/data/memory.db", body: "x"}, skipped: true},
		{name: "custom tool", file: tarFile{name: "tools/x/tool.yaml", body: "risk: read"}, skipped: true},
		{name: "remote", file: tarFile{name: "remote.yaml", body: "address: evil:7742"}, skipped: true},
		{name: "api token", file: tarFile{name: "data/api.token", body: "x"}, skipped: true},
		{name: "whatsapp session", file: tarFile{name: "data/whatsapp.db", body: "x"}, skipped: true},
		{name: "git hook", file: tarFile{name: "protocols/packs/p/.git/hooks/post-merge", body: "#!/bin/sh\nrm -rf ~"}, skipped: true},
		{name: "nested persona", file: tarFile{name: "personas/sub/x.yaml", body: "x"}, skipped: true},
		{name: "symlink", file: tarFile{name: "protocols/link.yaml", body: "/etc/passwd", typ: tar.TypeSymlink}, skipped: true},
		{name: "parent escape", file: tarFile{name: "../evil.yaml", body: "x"}, wantErr: "unsafe path"},
		{name: "hidden escape", file: tarFile{name: "protocols/../../evil.yaml", body: "x"}, wantErr: "unsafe path"},
		{name: "absolute", file: tarFile{name: "/tmp/evil.yaml", body: "x"}, wantErr: "unsafe path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("MIRRIN_HOME", home)
			r, err := Import(home, makeArchive(t, []tarFile{manifest, tc.file}))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err %v, want %q", err, tc.wantErr)
				}
				if entries, _ := os.ReadDir(home); len(entries) != 0 {
					t.Fatalf("a refused archive left files: %v", entries)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := contains(r.Skipped, tc.file.name); got != tc.skipped {
				t.Fatalf("skipped %v, want %v (%v)", got, tc.skipped, r.Skipped)
			}
			if tc.lands != "" {
				if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(tc.lands))); err != nil {
					t.Fatalf("%s not imported", tc.lands)
				}
			}
			if tc.skipped {
				if _, err := os.Lstat(filepath.Join(home, filepath.FromSlash(tc.file.name))); err == nil {
					t.Fatalf("%s was written", tc.file.name)
				}
			}
		})
	}
}

func TestImportRejectsBadArchives(t *testing.T) {
	good := tarFile{name: "manifest.yaml", body: manifestYAML(2, "")}
	cases := []struct {
		name    string
		files   []tarFile
		raw     string // a file that is not an archive at all
		limit   int64
		wantErr string
	}{
		{name: "not gzip", raw: "hello", wantErr: "isn't a Mirrin identity archive"},
		{name: "no manifest", files: []tarFile{{name: "config.yaml", body: "name: x\n"}}, wantErr: "isn't a Mirrin identity archive"},
		{name: "newer format", files: []tarFile{{name: "manifest.yaml", body: manifestYAML(9, "")}}, wantErr: "newer Mirrin"},
		{name: "cut short", files: []tarFile{good, {name: "memory.yaml", body: "facts: []\n", size: 4096}}, wantErr: "cut short"},
		{name: "too big", files: []tarFile{good, {name: "protocols/big.yaml", body: strings.Repeat("x", 2048)}}, limit: 1024, wantErr: "too big"},
		{name: "damaged memory", files: []tarFile{good, {name: "data/memory.db", body: "not a database at all, just text padding it out"}}, wantErr: "memory in this archive is damaged"},
		{name: "settings that won't load", files: []tarFile{good, {name: "config.yaml", body: "autonomy:\n  write: sometimes\n"}}, wantErr: "wouldn't load here"},
		{name: "settings of the wrong shape", files: []tarFile{good, {name: "config.yaml", body: "llm:\n  max_tokens: lots\n"}}, wantErr: "wouldn't load here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.limit != 0 {
				old := maxFileSize
				maxFileSize = tc.limit
				defer func() { maxFileSize = old }()
			}
			home := twinHome(t, func(c *config.Config) { c.Name = "Untouched" })
			before := readFile(t, filepath.Join(home, "config.yaml"))
			var in string
			if tc.raw != "" {
				in = filepath.Join(t.TempDir(), "x.tar.gz")
				writeFile(t, in, tc.raw)
			} else {
				in = makeArchive(t, tc.files)
			}
			_, err := Import(home, in)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err %v, want %q", err, tc.wantErr)
			}
			if after := readFile(t, filepath.Join(home, "config.yaml")); after != before {
				t.Fatal("a refused import changed the config")
			}
			if _, err := os.Stat(filepath.Join(home, "backups")); err == nil {
				t.Fatal("a refused import made a backup")
			}
		})
	}
}

// An archive from the previous exporter blanked max_tokens and carried the
// source machine's absolute paths.
func TestImportLegacyArchive(t *testing.T) {
	user, _ := os.UserHomeDir()
	old := "/Users/someone/.mirrin"
	legacy := "name: Jeeves\npersona: mavrk\nllm:\n  provider: anthropic\n  model: claude-opus-5\n  max_tokens: \"\"\n  api_key: \"\"\n" +
		"autonomy:\n  read: auto\n  write: ask\n  dangerous: ask\n" +
		"data_dir: " + old + "/data\nprotocols_dir: " + old + "/protocols\n" +
		"channels:\n  whatsapp:\n    enabled: false\n  voice:\n    chime_sound: " + old + "/sounds/x.aiff\n" +
		"skills:\n  system:\n    allowed_dirs: [/Users/someone/Projects]\n"
	in := makeArchive(t, []tarFile{
		{name: "config.yaml", body: legacy},
		{name: "protocols/x.yaml", body: "name: x\nprompt: y\n"},
		{name: "manifest.yaml", body: manifestYAML(1, "")},
	})
	dst := twinHome(t, func(c *config.Config) {
		p := c.LLM.Providers["anthropic"]
		p.APIKey = "LOCAL"
		c.LLM.Providers["anthropic"] = p
		c.LLM.APIKey = "LOCAL-legacy"
	})
	if _, err := Import(dst, in); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(t, dst)
	if cfg.LLM.MaxTokens != 16000 || cfg.LLM.APIKey != "LOCAL-legacy" || cfg.LLM.Providers["anthropic"].APIKey != "LOCAL" {
		t.Fatalf("llm %+v", cfg.LLM)
	}
	if cfg.DataDir != filepath.Join(dst, "data") || cfg.ProtocolsDir != filepath.Join(dst, "protocols") {
		t.Fatalf("folders %s %s", cfg.DataDir, cfg.ProtocolsDir)
	}
	if cfg.Channels.Voice.ChimeSound != filepath.Join(dst, "sounds", "x.aiff") {
		t.Fatalf("chime %s", cfg.Channels.Voice.ChimeSound)
	}
	if user != "" && cfg.Skills.System.AllowedDirs[0] != filepath.Join(user, "Projects") {
		t.Fatalf("allowed dirs %v", cfg.Skills.System.AllowedDirs)
	}
	if _, err := os.Stat(filepath.Join(dst, "protocols", "x.yaml")); err != nil {
		t.Fatal("protocol not imported")
	}
}

// An archive's paths in the home, as the token or (format 1) as the source
// machine's absolute paths, land in this home.
func TestImportReadsEveryHomeToken(t *testing.T) {
	for _, c := range []struct{ name, chime, dataDir string }{
		{"the token", "$MIRRIN_HOME/sounds/x.aiff", ""},
		{"format 1 from ~/.mirrin", "/Users/someone/.mirrin/sounds/x.aiff", "/Users/someone/.mirrin/data"},
	} {
		t.Run(c.name, func(t *testing.T) {
			format := 2
			body := "name: Jeeves\nchannels:\n  voice:\n    chime_sound: " + c.chime + "\n"
			if c.dataDir != "" {
				format = 1
				body += "data_dir: " + c.dataDir + "\nskills:\n  system:\n    allowed_dirs: [/Users/someone/Projects]\n"
			}
			in := makeArchive(t, []tarFile{
				{name: "config.yaml", body: body},
				{name: "manifest.yaml", body: manifestYAML(format, "")},
			})
			dst := twinHome(t, nil)
			if _, err := Import(dst, in); err != nil {
				t.Fatal(err)
			}
			cfg := loadConfig(t, dst)
			if cfg.Channels.Voice.ChimeSound != filepath.Join(dst, "sounds", "x.aiff") {
				t.Fatalf("chime %s", cfg.Channels.Voice.ChimeSound)
			}
			if user, _ := os.UserHomeDir(); c.dataDir != "" && user != "" && cfg.Skills.System.AllowedDirs[0] != filepath.Join(user, "Projects") {
				t.Fatalf("allowed dirs %v", cfg.Skills.System.AllowedDirs)
			}
		})
	}
}

// The previous importer left `max_tokens: ""` in configs, which then refused
// to load. Importing again repairs it, even from an old archive.
func TestImportRepairsConfigBrokenByOldImport(t *testing.T) {
	dst := t.TempDir()
	t.Setenv("MIRRIN_HOME", dst)
	broken := "name: Jeeves\nllm:\n  model: claude-opus-5\n  max_tokens: \"\"\n  api_key: LOCAL\nautonomy:\n  read: auto\n  write: ask\n  dangerous: ask\nchannels:\n  whatsapp:\n    enabled: false\n"
	writeFile(t, filepath.Join(dst, "config.yaml"), broken)
	// config.Load now reads a blank max_tokens as unset; the import still
	// writes a clean config.yaml without it.
	in := makeArchive(t, []tarFile{
		{name: "manifest.yaml", body: manifestYAML(1, "")},
		{name: "config.yaml", body: broken},
	})
	if _, err := Import(dst, in); err != nil {
		t.Fatal(err)
	}
	if cfg := loadConfig(t, dst); cfg.LLM.MaxTokens != 16000 || cfg.LLM.APIKey != "LOCAL" {
		t.Fatalf("llm %+v", cfg.LLM)
	}
}

func TestScrubSecrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // the value after export, as YAML
	}{
		{"max_tokens is a number, not a secret", "llm:\n  max_tokens: 16000\n", "llm:\n    max_tokens: 16000\n"},
		{"quoted max_tokens stays", "llm:\n  max_tokens: \"16000\"\n", "llm:\n    max_tokens: \"16000\"\n"},
		{"api key", "llm:\n  api_key: sk-1\n", "llm:\n    api_key: \"\"\n"},
		{"env name stays", "llm:\n  api_key_env: ANTHROPIC_API_KEY\n", "llm:\n    api_key_env: ANTHROPIC_API_KEY\n"},
		{"token file stays", "skills:\n  calendar:\n    token_file: /x/google-token.json\n", "skills:\n    calendar:\n        token_file: /x/google-token.json\n"},
		{"auth token", "phone:\n  auth_token: t\n", "phone:\n    auth_token: \"\"\n"},
		{"some future secret", "x:\n  client_secret: s\n", "x:\n    client_secret: \"\"\n"},
		{"MCP env, whatever its name", "mcp:\n  servers:\n    - name: a\n      env:\n        STRIPE: sk_live\n", "mcp:\n    servers:\n        - env:\n            STRIPE: \"\"\n          name: a\n"},
		{"MCP flags", "mcp:\n  servers:\n    - name: a\n      args: [--api-key=k, --token, t, --verbose, --max-tokens, \"5\"]\n", "mcp:\n    servers:\n        - args:\n            - --api-key=\n            - --token\n            - \"\"\n            - --verbose\n            - --max-tokens\n            - \"5\"\n          name: a\n"},
		{"URL credentials", "llm:\n  base_url: https://u:p@host/v1\n", "llm:\n    base_url: https://host/v1\n"},
		{"plain URL stays", "llm:\n  base_url: https://host/v1\n", "llm:\n    base_url: https://host/v1\n"},
		{"key in a URL query", "llm:\n  base_url: https://host/v1?api_key=k&x=1\n", "llm:\n    base_url: https://host/v1?api_key=&x=1\n"},
		{"database password", "x:\n  dsn: postgres://u:p@h/db\n", "x:\n    dsn: postgres://u@h/db\n"},
		{"key in a command", "channels:\n  voice:\n    tts_command: ELEVENLABS_API_KEY=k speak\n", "channels:\n    voice:\n        tts_command: ELEVENLABS_API_KEY= speak\n"},
		{"URL in a list", "x:\n  mirrors: [\"https://u:p@a/\", \"https://b/\"]\n", "x:\n    mirrors:\n        - https://a/\n        - https://b/\n"},
		{"tool risks are not secrets", "mcp:\n  servers:\n    - name: v\n      tool_risk:\n        get_password: dangerous\n", "mcp:\n    servers:\n        - name: v\n          tool_risk:\n            get_password: dangerous\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg map[string]any
			if err := yaml.Unmarshal([]byte(tc.in), &cfg); err != nil {
				t.Fatal(err)
			}
			scrubSecrets(cfg)
			var buf bytes.Buffer
			enc := yaml.NewEncoder(&buf)
			enc.SetIndent(4)
			_ = enc.Encode(cfg)
			if buf.String() != tc.want {
				t.Fatalf("got\n%s\nwant\n%s", buf.String(), tc.want)
			}
		})
	}
}

func TestScrubArgs(t *testing.T) {
	cases := []struct {
		name     string
		in, want []string
	}{
		{"secret flag with =", []string{"--api-key=k", "--verbose"}, []string{"--api-key=", "--verbose"}},
		{"secret flag and its value", []string{"--token", "t", "--bot-token", "b", "--secret-access-key", "s", "--key", "k"}, []string{"--token", "", "--bot-token", "", "--secret-access-key", "", "--key", ""}},
		{"number flags stay", []string{"--max-tokens", "5", "--max-tokens=5"}, []string{"--max-tokens", "5", "--max-tokens=5"}},
		{"postgres URL", []string{"-y", "@modelcontextprotocol/server-postgres", "postgresql://admin:PASS@db/prod"}, []string{"-y", "@modelcontextprotocol/server-postgres", "postgresql://admin@db/prod"}},
		{"URL after a flag", []string{"--db=postgresql://admin:PASS@db/prod"}, []string{"--db=postgresql://admin@db/prod"}},
		{"password with @ in it", []string{"mysql://root:p@ss@db/x"}, []string{"mysql://root@db/x"}},
		{"token as the user of an https URL", []string{"https://ghp_x@github.com/o/r.git"}, []string{"https://github.com/o/r.git"}},
		{"ssh user stays", []string{"ssh://git@github.com/o/r.git"}, []string{"ssh://git@github.com/o/r.git"}},
		{"docker env", []string{"run", "-e", "GITHUB_PERSONAL_ACCESS_TOKEN=ghp_x", "-e", "GITHUB_TOOLSETS=repos", "image"}, []string{"run", "-e", "GITHUB_PERSONAL_ACCESS_TOKEN=", "-e", "GITHUB_TOOLSETS=repos", "image"}},
		{"env passed through by name stays", []string{"-e", "GITHUB_PERSONAL_ACCESS_TOKEN"}, []string{"-e", "GITHUB_PERSONAL_ACCESS_TOKEN"}},
		{"shell command", []string{"-c", "API_KEY=k exec server"}, []string{"-c", "API_KEY= exec server"}},
		{"shell command, later assignment", []string{"-c", "cd /srv && export DB_PASSWORD='p w' OTHER=1; run"}, []string{"-c", "cd /srv && export DB_PASSWORD= OTHER=1; run"}},
		{"bearer header", []string{"--header", "Authorization: Bearer x"}, []string{"--header", ""}},
		{"secret header", []string{"-H", "X-API-Key: k"}, []string{"-H", "X-API-Key: "}},
		// A flag that only mentions a secret word takes no secret.
		{"negated flag keeps its path", []string{"--no-password", "/srv/notes"}, []string{"--no-password", "/srv/notes"}},
		{"tokenizer keeps its value", []string{"--tokenizer", "cl100k", "--tokenizer=cl100k"}, []string{"--tokenizer", "cl100k", "--tokenizer=cl100k"}},
		{"token file keeps its path", []string{"--token-file", "/x/t.json"}, []string{"--token-file", "/x/t.json"}},
		{"secret flag before another flag", []string{"--token", "--verbose"}, []string{"--token", "--verbose"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := make([]any, len(tc.in))
			for i, a := range tc.in {
				args[i] = a
			}
			hit := scrubArgs(args)
			var got []string
			var changed []int
			for i, a := range args {
				got = append(got, a.(string))
				if a.(string) != tc.in[i] {
					changed = append(changed, i)
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q\nwant %q", got, tc.want)
			}
			if !reflect.DeepEqual(hit, changed) {
				t.Fatalf("reported %v, changed %v", hit, changed)
			}
		})
	}
}

// Every secret field in the config is classified on purpose: adding one that
// the exporter would miss, or a number whose name looks secret (max_tokens),
// fails here.
func TestSecretFieldsAreKnown(t *testing.T) {
	want := []string{
		"channels.discord.token", "channels.irc.password", "channels.matrix.access_token", "channels.mattermost.token",
		"channels.slack.app_token", "channels.slack.bot_token", "channels.telegram.token", "channels.voice.elevenlabs_api_key",
		"channels.zulip.api_key", "jev.typesafe_api_key", "llm.api_key", "llm.providers.*.api_key", "phone.auth_token", "skills.email.password",
	}
	var got []string
	var walk func(tp reflect.Type, prefix string)
	walk = func(tp reflect.Type, prefix string) {
		for i := 0; i < tp.NumField(); i++ {
			f := tp.Field(i)
			name := strings.Split(f.Tag.Get("yaml"), ",")[0]
			p := strings.TrimPrefix(prefix+"."+name, ".")
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			switch ft.Kind() {
			case reflect.Struct:
				walk(ft, p)
				continue
			case reflect.Map, reflect.Slice:
				if ft.Elem().Kind() == reflect.Struct {
					walk(ft.Elem(), p+".*")
					continue
				}
			case reflect.String:
				if isSecretKey(name) {
					got = append(got, p)
				}
				continue
			}
			if isSecretKey(name) {
				t.Errorf("%s is not text but its name looks secret; add it to notSecret", p)
			}
		}
	}
	walk(reflect.TypeOf(config.Config{}), "")
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("secret fields changed:\n got %v\nwant %v\nUpdate the list once the export handles the new field.", got, want)
	}
}

func TestExportNeedsATwin(t *testing.T) {
	home := t.TempDir()
	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	_, err := Export(home, out, false)
	if err == nil || !strings.Contains(err.Error(), "mirrin init") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("an empty archive was written")
	}
}

func TestExportUsesConfiguredDirs(t *testing.T) {
	elsewhere := t.TempDir()
	src := twinHome(t, func(c *config.Config) {
		c.DataDir = filepath.Join(elsewhere, "data")
		c.ProtocolsDir = filepath.Join(elsewhere, "protocols")
	})
	writeFile(t, filepath.Join(elsewhere, "protocols", "x.yaml"), "name: x\nprompt: y\n")
	st, _ := memory.Open(filepath.Join(elsewhere, "data"))
	_, _ = st.Remember(context.Background(), "user", "Likes tea.", "t")
	st.Close()
	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	m, err := Export(src, out, false)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(m.Files, "memory.yaml") || !contains(m.Files, "protocols/x.yaml") {
		t.Fatalf("configured folders not exported: %v", m.Files)
	}
}

func TestExportFollowsLinkedFolders(t *testing.T) {
	elsewhere := t.TempDir()
	writeFile(t, filepath.Join(elsewhere, "x.yaml"), "name: x\nprompt: y\n")
	src := twinHome(t, func(c *config.Config) { c.ProtocolsDir = filepath.Join(config.Home(), "linked") })
	if err := os.Symlink(elsewhere, filepath.Join(src, "linked")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	m, err := Export(src, filepath.Join(t.TempDir(), "twin.tar.gz"), false)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(m.Files, "protocols/x.yaml") {
		t.Fatalf("linked protocols folder not exported: %v", m.Files)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestExportReportsWriteErrors(t *testing.T) {
	// The whole archive fits in gzip's buffer, so the failure only shows at the
	// final flush, which used to be ignored.
	if err := writeArchive(failingWriter{}, Manifest{Format: Format}, []entry{{name: "config.yaml", data: []byte("name: x\n")}}); err == nil {
		t.Fatal("a failed flush was reported as success")
	}
	src := twinHome(t, nil)
	out := filepath.Join(t.TempDir(), "missing", "twin.tar.gz")
	if _, err := Export(src, out, false); err == nil {
		t.Fatal("export into a missing folder succeeded")
	}
	if entries, _ := os.ReadDir(filepath.Dir(filepath.Dir(out))); len(entries) != 0 {
		t.Fatalf("a failed export left files: %v", entries)
	}
}

func TestPacksKeepTheirSource(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src := twinHome(t, nil)
	pack := filepath.Join(src, "protocols", "packs", "nightly")
	writeFile(t, filepath.Join(pack, "nightly.yaml"), "name: nightly\nprompt: y\n")
	gitRun := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitRun(pack, "init", "-q")
	gitRun(pack, "remote", "add", "origin", "https://user:SECRET-pat@example.com/packs/nightly.git")
	gitRun(pack, "add", ".")
	gitRun(pack, "commit", "-q", "-m", "x")

	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	m, err := Export(src, out, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Packs) != 1 || m.Packs[0].Repo != "https://example.com/packs/nightly.git" || !safeCommit.MatchString(m.Packs[0].Commit) {
		t.Fatalf("pack source %+v", m.Packs)
	}
	for name := range readArchive(t, out) {
		if strings.Contains(name, ".git/") || strings.Contains(name, provenanceFile) {
			t.Fatalf("archive carries %s", name)
		}
	}

	dst := twinHome(t, nil)
	r, err := Import(dst, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Packs) != 1 || r.Packs[0].Dir != "nightly" {
		t.Fatalf("packs %+v", r.Packs)
	}
	var pv provenance
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dst, "protocols", "packs", "nightly", provenanceFile))), &pv); err != nil ||
		pv.Source != "https://example.com/packs/nightly.git" || pv.Commit != m.Packs[0].Commit {
		t.Fatalf("provenance %+v %v", pv, err)
	}
	// Moving on again keeps the source.
	m2, err := Export(dst, filepath.Join(t.TempDir(), "again.tar.gz"), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Packs) != 1 || m2.Packs[0] != m.Packs[0] {
		t.Fatalf("pack source lost on the second move: %+v", m2.Packs)
	}

	// Where the pack is already installed from git, the updatable copy stays.
	dst2 := twinHome(t, nil)
	mine := filepath.Join(dst2, "protocols", "packs", "nightly")
	writeFile(t, filepath.Join(mine, "nightly.yaml"), "name: nightly\nprompt: mine\n")
	gitRun(mine, "init", "-q")
	r, err = Import(dst2, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Packs) != 0 || !strings.Contains(readFile(t, filepath.Join(mine, "nightly.yaml")), "mine") {
		t.Fatal("an installed git pack was overwritten by a copy")
	}
}

// A pack's provenance file is read for its source, with credentials removed,
// and never travels itself. A pack copied from a local folder has no source
// another machine could use.
func TestPackProvenance(t *testing.T) {
	src := twinHome(t, nil)
	packs := filepath.Join(src, "protocols", "packs")
	commit := strings.Repeat("ab", 20)
	writeFile(t, filepath.Join(packs, "news", "news.yaml"), "name: news\nprompt: y\n")
	writeFile(t, filepath.Join(packs, "news", provenanceFile), `{"source": "https://me:SECRET-pat@github.com/x/news.git", "ref": "v1.2.0", "commit": "`+commit+`", "installed": "2026-01-01T00:00:00Z"}`)
	writeFile(t, filepath.Join(packs, "local", "local.yaml"), "name: local\nprompt: y\n")
	writeFile(t, filepath.Join(packs, "local", provenanceFile), `{"source": "/Users/someone/SECRET-folder/local"}`)
	writeFile(t, filepath.Join(packs, "bad", "bad.yaml"), "name: bad\nprompt: y\n")
	writeFile(t, filepath.Join(packs, "bad", provenanceFile), `{"source": "https://x/y; rm -rf ~"}`)

	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	m, err := Export(src, out, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []PackSource{{Dir: "news", Repo: "https://github.com/x/news.git", Ref: "v1.2.0", Commit: commit}}
	if !reflect.DeepEqual(m.Packs, want) {
		t.Fatalf("packs %+v, want %+v", m.Packs, want)
	}
	for name, b := range readArchive(t, out) {
		if strings.Contains(name, provenanceFile) || strings.Contains(string(b), "SECRET-") {
			t.Fatalf("archive carries %s:\n%s", name, b)
		}
	}
	dst := twinHome(t, nil)
	if _, err := Import(dst, out); err != nil {
		t.Fatal(err)
	}
	var pv provenance
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dst, "protocols", "packs", "news", provenanceFile))), &pv); err != nil ||
		pv != (provenance{Source: "https://github.com/x/news.git", Ref: "v1.2.0", Commit: commit}) {
		t.Fatalf("provenance %+v %v", pv, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "protocols", "packs", "local", provenanceFile)); err == nil {
		t.Fatal("a local folder's path became a pack source")
	}
}

// protocols_dir's YAML files travel as protocols, so a protocols_dir that is,
// or holds, the twin's home or data folder would carry config.yaml (every
// key) and remote.yaml out as "protocols".
func TestProtocolsDirHoldingTheTwinIsRefused(t *testing.T) {
	cases := []struct {
		name string
		edit func(c *config.Config, elsewhere string)
	}{
		{"the home itself", func(c *config.Config, _ string) { c.ProtocolsDir = config.Home() }},
		{"above the home", func(c *config.Config, _ string) { c.ProtocolsDir = filepath.Dir(config.Home()) }},
		{"the data folder", func(c *config.Config, e string) {
			c.DataDir = filepath.Join(e, "data")
			c.ProtocolsDir = c.DataDir
		}},
		{"above the data folder", func(c *config.Config, e string) {
			c.DataDir = filepath.Join(e, "data")
			c.ProtocolsDir = e
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			elsewhere := t.TempDir()
			home := twinHome(t, func(c *config.Config) { tc.edit(c, elsewhere) })
			writeFile(t, filepath.Join(home, "remote.yaml"), "address: 1.2.3.4:7742\ntoken: SECRET-remote\n")
			out := filepath.Join(t.TempDir(), "twin.tar.gz")
			_, err := Export(home, out, false)
			if err == nil || !strings.Contains(err.Error(), "protocols_dir") {
				t.Fatalf("err %v", err)
			}
			if _, err := os.Stat(out); err == nil {
				t.Fatal("an archive was written")
			}

			// Importing protocols into such a folder would write over the twin's files.
			before := readFile(t, filepath.Join(home, "config.yaml"))
			in := makeArchive(t, []tarFile{{name: "manifest.yaml", body: manifestYAML(2, "")}, {name: "protocols/config.yaml", body: "name: evil\n"}})
			_, err = Import(home, in)
			if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("import err %v", err)
			}
			if readFile(t, filepath.Join(home, "config.yaml")) != before {
				t.Fatal("the import wrote over the twin's config")
			}
		})
	}
}

func TestExportChecksSizes(t *testing.T) {
	old := maxFileSize
	maxFileSize = 64 << 10
	defer func() { maxFileSize = old }()
	src := twinHome(t, nil)
	writeFile(t, filepath.Join(src, "protocols", "big.yaml"), "name: big\nprompt: "+strings.Repeat("x", 128<<10)+"\n")
	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	_, err := Export(src, out, false)
	if err == nil || !strings.Contains(err.Error(), "big.yaml") || !strings.Contains(err.Error(), "too big") {
		t.Fatalf("an archive import would refuse was made: %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("an archive was written")
	}
}

// data_dir may be a synced folder: the conversation snapshot must not be
// staged there. A data folder the export may not write to shows it.
func TestConversationsExportLeavesDataDirAlone(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs unix permissions")
	}
	src := twinHome(t, nil)
	dataDir := filepath.Join(src, "data")
	live, err := memory.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	_, _ = live.Remember(context.Background(), "user", "Likes tea.", "t")
	before, _ := os.ReadDir(dataDir)
	if err := os.Chmod(dataDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dataDir, 0o700)
	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	if _, err := Export(src, out, true); err != nil {
		t.Fatalf("export staged its snapshot in data_dir: %v", err)
	}
	if after, _ := os.ReadDir(dataDir); len(after) != len(before) {
		t.Fatalf("data_dir changed: %v", after)
	}
	if entries, _ := os.ReadDir(filepath.Dir(out)); len(entries) != 1 {
		t.Fatalf("export left files beside the archive: %v", entries)
	}
}

// A config or protocol kept in a dotfiles repo and linked into the home
// stays linked; the import writes through the link.
func TestImportWritesThroughLinks(t *testing.T) {
	dots := t.TempDir()
	dst := twinHome(t, nil)
	cfgPath := filepath.Join(dst, "config.yaml")
	if err := os.Rename(cfgPath, filepath.Join(dots, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dots, "x.yaml"), "name: x\nprompt: old\n")
	if err := os.MkdirAll(filepath.Join(dst, "protocols"), 0o700); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{cfgPath: "config.yaml", filepath.Join(dst, "protocols", "x.yaml"): "x.yaml"} {
		if err := os.Symlink(filepath.Join(dots, target), link); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
	}
	in := makeArchive(t, []tarFile{
		{name: "manifest.yaml", body: manifestYAML(2, "")},
		{name: "config.yaml", body: "name: Linked\n"},
		{name: "protocols/x.yaml", body: "name: x\nprompt: new\n"},
	})
	if _, err := Import(dst, in); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{cfgPath: "config.yaml", filepath.Join(dst, "protocols", "x.yaml"): "x.yaml"} {
		if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is no longer a link", link)
		}
		if got := readFile(t, filepath.Join(dots, target)); !strings.Contains(got, "Linked") && !strings.Contains(got, "new") {
			t.Fatalf("%s was not updated through the link:\n%s", target, got)
		}
	}
	if cfg := loadConfig(t, dst); cfg.Name != "Linked" {
		t.Fatalf("name %q", cfg.Name)
	}
}

// The memory restore is the first change an import makes, in one
// transaction, so when it fails nothing has changed and the user hears so.
func TestFailedMemoryRestoreChangesNothing(t *testing.T) {
	// A database that checks out but whose facts break this version's rules.
	bad := filepath.Join(t.TempDir(), "memory.db")
	db, err := sql.Open("sqlite", bad)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE facts (id INTEGER PRIMARY KEY, subject TEXT, content TEXT, source TEXT, created_at TEXT, updated_at TEXT)`,
		`INSERT INTO facts (subject, content, created_at, updated_at) VALUES ('user', NULL, 'x', 'x')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	b, err := os.ReadFile(bad)
	if err != nil {
		t.Fatal(err)
	}

	dst := twinHome(t, func(c *config.Config) { c.Name = "Untouched" })
	st, err := memory.Open(filepath.Join(dst, "data"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.Remember(context.Background(), "user", "Likes coffee.", "t")
	st.Close()
	before := readFile(t, filepath.Join(dst, "config.yaml"))

	in := makeArchive(t, []tarFile{
		{name: "manifest.yaml", body: manifestYAML(2, "conversations: true\n")},
		{name: "config.yaml", body: "name: Changed\n"},
		{name: "data/memory.db", body: string(b)},
	})
	_, err = Import(dst, in)
	if err == nil || !strings.Contains(err.Error(), "couldn't restore the memory") || !strings.Contains(err.Error(), "nothing was changed") || strings.Contains(err.Error(), "partway") {
		t.Fatalf("err %v", err)
	}
	if readFile(t, filepath.Join(dst, "config.yaml")) != before {
		t.Fatal("the config changed")
	}
	if got := facts(t, filepath.Join(dst, "data")); !reflect.DeepEqual(got, []string{"Likes coffee."}) {
		t.Fatalf("facts %v", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "backups")); err == nil {
		t.Fatal("a backup of nothing was left behind")
	}
}

func TestLocalAPIReadsBrokenConfig(t *testing.T) {
	user, _ := os.UserHomeDir()
	cases := []struct {
		name, config         string
		wantListen, wantData string // wantData relative to home unless absolute
	}{
		{"no config", "", "127.0.0.1:7742", "data"},
		// config.Load refuses this one; the twin running from it still counts.
		{"config that no longer loads", "llm:\n  max_tokens: \"\"\napi:\n  listen: 127.0.0.1:9999\ndata_dir: /srv/twin-data\n", "127.0.0.1:9999", "/srv/twin-data"},
		{"API off", "api:\n  listen: \"\"\n", "", "data"},
	}
	if user != "" {
		cases = append(cases, struct {
			name, config         string
			wantListen, wantData string
		}{"data under ~", "data_dir: ~/twin-data\n", "127.0.0.1:7742", filepath.Join(user, "twin-data")})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.config != "" {
				writeFile(t, filepath.Join(home, "config.yaml"), tc.config)
			}
			want := tc.wantData
			if !filepath.IsAbs(want) && !strings.HasPrefix(want, "/") {
				want = filepath.Join(home, want)
			}
			if listen, data := LocalAPI(home); listen != tc.wantListen || filepath.ToSlash(data) != filepath.ToSlash(want) {
				t.Fatalf("got %q %q, want %q %q", listen, data, tc.wantListen, want)
			}
		})
	}
}

// An archive that moves an endpoint while this machine keeps its own key for
// it would send the key somewhere new; the import says so.
func TestImportWarnsWhenKeyWouldGoElsewhere(t *testing.T) {
	cases := []struct {
		name, here, archive string
		want                []string // setting names warned about
	}{
		{"provider endpoint moves",
			"llm:\n  providers:\n    openai:\n      api_key: LOCAL\n",
			"llm:\n  providers:\n    openai:\n      api_key: \"\"\n      base_url: https://proxy.example/v1\n",
			[]string{"llm.providers.openai.base_url"}},
		{"shared endpoint moves, key under providers",
			"llm:\n  providers:\n    anthropic:\n      api_key: LOCAL\n",
			"llm:\n  base_url: https://proxy.example/v1\n  providers:\n    anthropic:\n      api_key: \"\"\n",
			[]string{"llm.base_url"}},
		{"matrix homeserver moves",
			"channels:\n  matrix:\n    homeserver: https://matrix.org\n    access_token: LOCAL\n",
			"channels:\n  matrix:\n    homeserver: https://evil.example\n    access_token: \"\"\n",
			[]string{"channels.matrix.homeserver"}},
		{"same endpoint",
			"llm:\n  providers:\n    openai:\n      api_key: LOCAL\n      base_url: https://api.openai.com/v1\n",
			"llm:\n  providers:\n    openai:\n      api_key: \"\"\n      base_url: https://api.openai.com/v1\n",
			nil},
		{"same endpoint, password cut out",
			"llm:\n  api_key: LOCAL\n  base_url: https://u:p@host/v1\n",
			"llm:\n  api_key: \"\"\n  base_url: https://host/v1\n",
			nil},
		{"no key here", "llm:\n  model: m\n", "llm:\n  base_url: https://proxy.example/v1\n", nil},
		{"the archive brings its own key",
			"llm:\n  api_key: LOCAL\n",
			"llm:\n  api_key: theirs\n  base_url: https://proxy.example/v1\n",
			nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cur, in map[string]any
			if err := yaml.Unmarshal([]byte(tc.here), &cur); err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal([]byte(tc.archive), &in); err != nil {
				t.Fatal(err)
			}
			var r Result
			mergeSettings(cur, in, Manifest{Format: Format}, t.TempDir(), &r)
			var got []string
			for _, w := range r.Warnings {
				got = append(got, strings.Fields(w)[0])
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("warned about %v, want %v (%v)", got, tc.want, r.Warnings)
			}
		})
	}
}

// The regression: config.yaml is saved in layers now (config_version: 2), so
// a full dump from an earlier release imported into a new home kept the
// version and skipped the upgrade: the old wake threshold and the zone frozen
// at install survived, and every default in the dump became the owner's.
func TestImportedFullDumpGetsTheUpgrade(t *testing.T) {
	t.Setenv("TZ", "Europe/Berlin")
	legacy := "name: Jeeves\npersona: mavrk\nuser:\n  timezone: Europe/Berlin\nllm:\n  provider: anthropic\n  model: claude-opus-5\n" +
		"autonomy:\n  read: auto\n  write: ask\n  dangerous: ask\n" +
		"channels:\n  whatsapp:\n    enabled: false\n  voice:\n    wake_threshold: 0.5\n"
	in := makeArchive(t, []tarFile{
		{name: "config.yaml", body: legacy},
		{name: "manifest.yaml", body: manifestYAML(Format, "")},
	})
	dst := twinHome(t, nil) // a new Mac: `mirrin init` saved it in layers
	if _, err := Import(dst, in); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(t, dst)
	if cfg.Channels.Voice.WakeThreshold != config.Default().Channels.Voice.WakeThreshold {
		t.Fatalf("the old default wake threshold should give way, got %v", cfg.Channels.Voice.WakeThreshold)
	}
	if cfg.User.Timezone != "Local" {
		t.Fatalf("the zone written at install follows the system, got %q", cfg.User.Timezone)
	}
	if cfg.Name != "Jeeves" {
		t.Fatalf("the archive's settings still win: %q", cfg.Name)
	}
}

func TestLayeredArchiveIntoAnOldHomeGetsTheUpgrade(t *testing.T) {
	t.Setenv("TZ", "Europe/Berlin")
	dst := t.TempDir()
	t.Setenv("MIRRIN_HOME", dst)
	old := config.Default()
	old.Channels.WhatsApp.Enabled = false
	old.Channels.Voice.WakeThreshold = 0.5
	b, _ := yaml.Marshal(old) // how releases before layering saved it
	writeFile(t, filepath.Join(dst, "config.yaml"), string(b))
	in := makeArchive(t, []tarFile{
		{name: "config.yaml", body: "config_version: 2\nname: Jeeves\nllm:\n  model: claude-opus-5\nchannels:\n  whatsapp:\n    enabled: false\n"},
		{name: "manifest.yaml", body: manifestYAML(Format, "")},
	})
	if _, err := Import(dst, in); err != nil {
		t.Fatal(err)
	}
	if cfg := loadConfig(t, dst); cfg.Channels.Voice.WakeThreshold != config.Default().Channels.Voice.WakeThreshold || cfg.Name != "Jeeves" {
		t.Fatalf("the old home's stale defaults should give way: wake %v, name %q", cfg.Channels.Voice.WakeThreshold, cfg.Name)
	}
}

// An archive's memory comes in table by table, but not its record of the
// migrations run on it (an archive from a newer build would mark migrations
// this build lacks as done, so after an upgrade they would never run), nor
// what the machine it came from kept for itself: its pause and when its
// scheduler last looked (runs due before the import aren't made up here).
func TestImportKeepsThisMemorysMigrationsAndMachineState(t *testing.T) {
	srcDir := t.TempDir()
	st, err := memory.Open(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, _ = st.Remember(ctx, "user", "Likes flat whites.", "t")
	old := time.Now().Add(-9 * time.Hour).UTC().Format(time.RFC3339Nano)
	_ = st.Set(ctx, memory.KeyAlive, old)
	_ = st.Set(ctx, memory.KeyPaused, old)
	st.Close()
	db, err := sql.Open("sqlite", filepath.Join(srcDir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version(version, name, applied_at) VALUES(99, 'from-a-newer-build', 'x')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	b, err := os.ReadFile(filepath.Join(srcDir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	dst := twinHome(t, nil)
	in := makeArchive(t, []tarFile{
		{name: "manifest.yaml", body: manifestYAML(2, "conversations: true\n")},
		{name: "config.yaml", body: "name: Moved\n"},
		{name: "data/memory.db", body: string(b)},
	})
	before := time.Now()
	if _, err := Import(dst, in); err != nil {
		t.Fatal(err)
	}
	if got := facts(t, filepath.Join(dst, "data")); !reflect.DeepEqual(got, []string{"Likes flat whites."}) {
		t.Fatalf("facts %v", got)
	}
	got := memory.MachineState(filepath.Join(dst, "data", "memory.db"))
	if got[memory.KeyPaused] != "" {
		t.Fatal("the import brought the other machine's pause")
	}
	if at, err := time.Parse(time.RFC3339Nano, got[memory.KeyAlive]); err != nil || at.Before(before.Add(-time.Second)) {
		t.Fatalf("the scheduler's last look came from the archive: %q", got[memory.KeyAlive])
	}
	ddb, err := sql.Open("sqlite", filepath.Join(dst, "data", "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ddb.Close()
	var n int
	if err := ddb.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version=99`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a migration this build lacks is marked done (%d, %v)", n, err)
	}
	if err := ddb.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("this memory's own migrations were lost (%d, %v)", n, err)
	}
}
