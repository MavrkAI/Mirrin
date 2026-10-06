package identity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/procenv"
)

// serverEnv is what mcp.Start hands a server configured with env: the basics
// plus its own env block, each value through procenv.Expand.
func serverEnv(env map[string]string) []string {
	out := procenv.Base()
	for k, v := range env {
		out = append(out, k+"="+procenv.Expand(v))
	}
	return out
}

// An MCP server no longer inherits the daemon's environment, so a token the
// export left out that this machine has (in its environment or saved with
// mirrin) must reach the server through its env block, or be reported.
func TestMCPEnvThisMachineHasIsPassedOn(t *testing.T) {
	for _, via := range []string{"environment", "secrets.env"} {
		t.Run(via, func(t *testing.T) {
			t.Setenv("PROBE_GH_TOKEN", "")
			src := twinHome(t, func(c *config.Config) {
				c.MCP.Servers = []config.MCPServer{{Name: "github", Command: "gh-mcp", Env: map[string]string{"PROBE_GH_TOKEN": "SECRET-x"}}}
			})
			out := filepath.Join(t.TempDir(), "twin.tar.gz")
			if _, err := Export(src, out, false); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(t.TempDir(), "new-home")
			t.Setenv("MIRRIN_HOME", dst)
			if via == "environment" {
				t.Setenv("PROBE_GH_TOKEN", "tok-here")
			} else if err := config.SaveSecrets(map[string]string{"PROBE_GH_TOKEN": "tok-here"}); err != nil {
				t.Fatal(err)
			}
			r, err := Import(dst, out)
			if err != nil {
				t.Fatal(err)
			}
			env := loadConfig(t, dst).MCP.Servers[0].Env
			if env["PROBE_GH_TOKEN"] != "$PROBE_GH_TOKEN" || len(r.MCPEnv) != 0 || len(r.Missing) != 0 {
				t.Fatalf("env in config %v, MCP env to fill in %v, missing %v", env, r.MCPEnv, r.Missing)
			}
			if !slices.Contains(serverEnv(env), "PROBE_GH_TOKEN=tok-here") {
				t.Fatalf("the server would start without this machine's token: %v", env)
			}
			if strings.Contains(readFile(t, filepath.Join(dst, "config.yaml")), "tok-here") {
				t.Fatal("the token was written into config.yaml instead of named")
			}
		})
	}
}

// "$NAME" in an MCP env block names a variable and holds no secret: it
// travels as written, and a destination's own reference survives an import
// whose archive left that value out.
func TestMCPEnvReferenceTravels(t *testing.T) {
	src := twinHome(t, func(c *config.Config) {
		c.MCP.Servers = []config.MCPServer{{Name: "github", Command: "gh-mcp", Env: map[string]string{"GITHUB_TOKEN": "$GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY": "SECRET-aws"}}}
	})
	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	m, err := Export(src, out, false)
	if err != nil {
		t.Fatal(err)
	}
	cfgYAML := string(readArchive(t, out)["config.yaml"])
	if !strings.Contains(cfgYAML, "$GITHUB_TOKEN") || strings.Contains(cfgYAML, "SECRET-") {
		t.Fatalf("export blanked the reference or kept the secret:\n%s", cfgYAML)
	}
	if contains(m.Secrets, "mcp.servers.github.env.GITHUB_TOKEN") || !contains(m.Secrets, "mcp.servers.github.env.AWS_SECRET_ACCESS_KEY") {
		t.Fatalf("declared secrets %v", m.Secrets)
	}

	// This machine names its own variable for a value the archive left out.
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("MY_AWS", "")
	dst := twinHome(t, func(c *config.Config) {
		c.MCP.Servers = []config.MCPServer{{Name: "github", Command: "gh-mcp", Env: map[string]string{"AWS_SECRET_ACCESS_KEY": "${MY_AWS}"}}}
	})
	r, err := Import(dst, out)
	if err != nil {
		t.Fatal(err)
	}
	env := loadConfig(t, dst).MCP.Servers[0].Env
	if env["GITHUB_TOKEN"] != "$GITHUB_TOKEN" || env["AWS_SECRET_ACCESS_KEY"] != "${MY_AWS}" {
		t.Fatalf("env after import %v", env)
	}
	if len(r.MCPEnv) != 0 {
		t.Fatalf("reported %v, but this machine already names what to pass", r.MCPEnv)
	}
}

// Keys kept only in secrets.env (mirrin init, service install) count as set
// on this machine, for settings with a variable name and for MCP env alike.
func TestSecretsSavedWithMirrinAreNotMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("SAVED_MCP_TOKEN", "")
	t.Setenv("UNSAVED_MCP_TOKEN", "")
	if err := config.SaveSecrets(map[string]string{"TELEGRAM_BOT_TOKEN": "saved", "SAVED_MCP_TOKEN": "saved"}); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"channels": map[string]any{"telegram": map[string]any{"token": "", "token_env": "TELEGRAM_BOT_TOKEN"}},
		"mcp":      map[string]any{"servers": []any{map[string]any{"name": "gh", "env": map[string]any{"SAVED_MCP_TOKEN": "", "UNSAVED_MCP_TOKEN": ""}}}},
	}
	missing, env := finishSecrets(cfg, []string{"channels.telegram.token", "mcp.servers.gh.env.SAVED_MCP_TOKEN", "mcp.servers.gh.env.UNSAVED_MCP_TOKEN"})
	if slices.Contains(missing, "channels.telegram.token") {
		t.Fatalf("a token saved in secrets.env is reported missing: %v", missing)
	}
	if want := []string{"UNSAVED_MCP_TOKEN"}; !slices.Equal(env["gh"], want) {
		t.Fatalf("MCP env to fill in %v, want %v", env, want)
	}
	srv := cfg["mcp"].(map[string]any)["servers"].([]any)[0].(map[string]any)["env"].(map[string]any)
	if srv["SAVED_MCP_TOKEN"] != "$SAVED_MCP_TOKEN" {
		t.Fatalf("saved MCP token not named for the server: %v", srv)
	}
	if _, kept := srv["UNSAVED_MCP_TOKEN"]; kept {
		t.Fatalf("a blank value is passed to the server: %v", srv)
	}
}

// A pack's links never travel: the loaders refuse them there, and a cloned
// pack could point one at secrets.env or any file on the machine.
func TestExportLeavesPackLinksBehind(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	src := twinHome(t, nil)
	pack := filepath.Join(src, "protocols", "packs", "evil")
	writeFile(t, filepath.Join(pack, "pack.yaml"), "name: evil\n")
	writeFile(t, filepath.Join(pack, "protocols", "ok.yaml"), "name: ok\nprompt: fine\n")
	writeFile(t, filepath.Join(src, "secrets.env"), "ANTHROPIC_API_KEY=\"SECRET-in-secrets-env\"\n")
	if err := os.Symlink("../../../../secrets.env", filepath.Join(pack, "protocols", "steal.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../secrets.env", filepath.Join(pack, "vars.yaml")); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	writeFile(t, filepath.Join(elsewhere, "p.yaml"), "name: P\ncharacter: SECRET-linked-folder\n")
	pack2 := filepath.Join(src, "protocols", "packs", "evil2")
	writeFile(t, filepath.Join(pack2, "pack.yaml"), "name: evil2\n")
	if err := os.Symlink(elsewhere, filepath.Join(pack2, "personas")); err != nil {
		t.Fatal(err)
	}
	// The user's own files still follow links, as their loaders do.
	writeFile(t, filepath.Join(elsewhere, "mine.yaml"), "name: mine\nprompt: y\n")
	if err := os.Symlink(filepath.Join(elsewhere, "mine.yaml"), filepath.Join(src, "protocols", "mine.yaml")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	m, err := Export(src, out, false)
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range readArchive(t, out) {
		if strings.Contains(string(b), "SECRET-") {
			t.Errorf("%s carries what a pack's link pointed at:\n%s", name, b)
		}
	}
	if !contains(m.Files, "protocols/packs/evil/protocols/ok.yaml") || !contains(m.Files, "protocols/mine.yaml") {
		t.Fatalf("files a loader reads were left out: %v", m.Files)
	}
}

// A model key kept under its provider's own variable (ANTHROPIC_API_KEY in
// secrets.env, as `mirrin init` and `service install` save it) is found, as
// config.ProviderKey finds it, so it isn't reported missing.
func TestProviderKeyUnderItsDefaultVariableIsNotMissing(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	src := twinHome(t, func(c *config.Config) {
		c.LLM.Provider = "anthropic"
		c.LLM.Providers = map[string]config.ProviderConfig{"anthropic": {APIKey: "SECRET-a"}}
	})
	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	if _, err := Export(src, out, false); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "new-home")
	t.Setenv("MIRRIN_HOME", dst)
	if err := config.SaveSecrets(map[string]string{"ANTHROPIC_API_KEY": "sk-here"}); err != nil {
		t.Fatal(err)
	}
	r, err := Import(dst, out)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := loadConfig(t, dst); cfg.ProviderKey("anthropic") != "sk-here" || slices.Contains(r.Missing, "llm.providers.anthropic.api_key") {
		t.Fatalf("key %q, missing %v", cfg.ProviderKey("anthropic"), r.Missing)
	}
}

// Hosts on the old machine's network that fetch_url may reach are pointed
// out: here they may be someone else's.
func TestImportPointsOutLocalHostsItAllows(t *testing.T) {
	src := twinHome(t, func(c *config.Config) { c.Skills.Web.AllowHosts = []string{"192.168.1.0/24", "nas.local"} })
	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	if _, err := Export(src, out, false); err != nil {
		t.Fatal(err)
	}
	r, err := Import(twinHome(t, nil), out)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "192.168.1.0/24, nas.local") {
		t.Fatalf("warnings %v", r.Warnings)
	}
	// Importing it again, where it is already allowed, says nothing new.
	dst := twinHome(t, func(c *config.Config) { c.Skills.Web.AllowHosts = []string{"192.168.1.0/24", "nas.local"} })
	if r, err := Import(dst, out); err != nil || len(r.Warnings) != 0 {
		t.Fatalf("warnings %v %v", r.Warnings, err)
	}
}

// Import rewrites config.yaml, so the record of what the persona asked of
// the voice settings (which describes the file it replaced, or the old
// machine's paths) is dropped: the next start goes by the persona files.
func TestImportDropsThePersonaVoiceRecord(t *testing.T) {
	for _, conv := range []bool{false, true} {
		t.Run(fmt.Sprint("with conversations ", conv), func(t *testing.T) {
			src := twinHome(t, nil)
			st, err := memory.Open(filepath.Join(src, "data"))
			if err != nil {
				t.Fatal(err)
			}
			_ = st.Set(context.Background(), memory.PersonaVoiceKey, `{"wake_model":"/Users/old/.mirrin/models/hey.onnx"}`)
			st.Close()
			out := filepath.Join(t.TempDir(), "twin.tar.gz")
			if _, err := Export(src, out, conv); err != nil {
				t.Fatal(err)
			}
			dst := twinHome(t, nil)
			here, err := memory.Open(filepath.Join(dst, "data"))
			if err != nil {
				t.Fatal(err)
			}
			_ = here.Set(context.Background(), memory.PersonaVoiceKey, `{"wake_word":"ava"}`)
			here.Close()
			if _, err := Import(dst, out); err != nil {
				t.Fatal(err)
			}
			here, _ = memory.Open(filepath.Join(dst, "data"))
			defer here.Close()
			if v, _ := here.Get(context.Background(), memory.PersonaVoiceKey); v != "" {
				t.Fatalf("the record survived the import: %s", v)
			}
		})
	}
}

// A home whose path has '#', '?' or '%' in it (which memory itself opens
// fine) moves too, conversations and all.
func TestConversationsMoveFromAnOddlyNamedHome(t *testing.T) {
	base := filepath.Join(t.TempDir(), "odd#name?100%")
	src := filepath.Join(base, "src")
	t.Setenv("MIRRIN_HOME", src)
	c := config.Default()
	c.Channels.WhatsApp.Enabled = false
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	st, err := memory.Open(filepath.Join(src, "data"))
	if err != nil {
		t.Fatalf("memory itself: %v", err)
	}
	_, _ = st.Remember(context.Background(), "user", "Likes tea.", "t")
	st.Close()
	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	if _, err := Export(src, out, true); err != nil {
		t.Fatalf("export with conversations: %v", err)
	}
	dst := filepath.Join(base, "dst")
	t.Setenv("MIRRIN_HOME", dst)
	st2, err := memory.Open(filepath.Join(dst, "data"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st2.Remember(context.Background(), "user", "Likes coffee.", "t")
	st2.Close()
	if _, err := Import(dst, out); err != nil {
		t.Fatalf("import with conversations: %v", err)
	}
	if got := facts(t, filepath.Join(dst, "data")); !slices.Equal(got, []string{"Likes tea."}) {
		t.Fatalf("facts %v", got)
	}
}

// What an import replaced is kept in home/backups, and forgetting a fact
// later reaches that copy too, as it reaches the daily backups.
func TestForgettingReachesWhatAnImportReplaced(t *testing.T) {
	for _, conv := range []bool{false, true} {
		t.Run(fmt.Sprint("with conversations ", conv), func(t *testing.T) {
			const secret = "My locker code is 4 8 15 16 23."
			src := twinHome(t, nil)
			st, _ := memory.Open(filepath.Join(src, "data"))
			_, _ = st.Remember(context.Background(), "user", "Likes tea.", "t")
			st.Close()
			out := filepath.Join(t.TempDir(), "twin.tar.gz")
			if _, err := Export(src, out, conv); err != nil {
				t.Fatal(err)
			}
			dst := twinHome(t, nil)
			here, _ := memory.Open(filepath.Join(dst, "data"))
			_, _ = here.Remember(context.Background(), "user", secret, "t")
			_, _ = here.Remember(context.Background(), "user", "Has two cats.", "t")
			_ = here.SetPortrait(context.Background(), "Keeps a locker.")
			here.Close()
			r, err := Import(dst, out)
			if err != nil {
				t.Fatal(err)
			}
			copyPath := filepath.Join(r.Backup, "memory.yaml")
			if conv {
				copyPath = filepath.Join(r.Backup, "data", "memory.db")
			}
			holds := func() bool {
				if !conv {
					return strings.Contains(readFile(t, copyPath), "locker code")
				}
				st, err := memory.Open(filepath.Dir(copyPath))
				if err != nil {
					t.Fatal(err)
				}
				defer st.Close()
				fs, _ := st.AllFacts(context.Background(), 100)
				for _, f := range fs {
					if f.Content == secret {
						return true
					}
				}
				return false
			}
			if !holds() {
				t.Fatal("the backup doesn't hold what it replaced")
			}
			// The user tells the twin to forget it (it came back with the
			// facts-only import, which only adds; or the owner re-adds it).
			here, _ = memory.Open(filepath.Join(dst, "data"))
			defer here.Close()
			id, _ := here.Remember(context.Background(), "user", secret, "t")
			if _, err := here.ForgetFact(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if holds() {
				t.Fatalf("the copy the import saved still holds the forgotten fact")
			}
			if !conv && !strings.Contains(readFile(t, copyPath), "Has two cats.") {
				t.Fatal("the rest of the backup was lost")
			}
			// The portrait may say it in other words, so it goes too.
			if !conv && strings.Contains(readFile(t, copyPath), "Keeps a locker.") {
				t.Fatal("the copy's portrait outlived the forget")
			}
		})
	}
}
