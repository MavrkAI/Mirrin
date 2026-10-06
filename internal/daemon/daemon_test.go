package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

func TestUpdateConfigSavesAndApplies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	cfg := config.Default()
	cfg.Channels.WhatsApp.Enabled = false
	cfg.Skills.Browser.Enabled = false
	cfg.DataDir = filepath.Join(home, "data")
	cfg.ProtocolsDir = filepath.Join(home, "protocols")
	cfg.LLM.APIKey = "test-key"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	d, err := New(cfg, Options{Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.store.Close()

	err = d.UpdateConfig(func(c *config.Config) {
		c.LLM.Model = "claude-sonnet-5"
		c.Channels.Voice.Voice = "bm_lewis"
		c.Autonomy.Write = "auto"
	})
	if err != nil {
		t.Fatal(err)
	}
	got := d.Config()
	if got.LLM.Model != "claude-sonnet-5" || got.Channels.Voice.Voice != "bm_lewis" || got.Autonomy.Write != "auto" {
		t.Fatalf("live config not updated: %+v", got.LLM)
	}
	if d.agent.Config().LLM.Model != "claude-sonnet-5" {
		t.Fatal("the agent's next turn would still use the old settings")
	}
	b, _ := os.ReadFile(config.Path())
	if !strings.Contains(string(b), "claude-sonnet-5") || !strings.Contains(string(b), "bm_lewis") {
		t.Fatalf("config file not saved:\n%s", b)
	}
	// Invalid changes are rejected and leave the live config alone.
	if err := d.UpdateConfig(func(c *config.Config) { c.Autonomy.Read = "sometimes" }); err == nil {
		t.Fatal("expected validation error")
	}
	if d.Config().Autonomy.Read != "auto" {
		t.Fatal("invalid update leaked into live config")
	}
}
