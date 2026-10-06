package config

import (
	"fmt"
	"os"
	"testing"
)

func TestLoadWhatsAppOptIn(t *testing.T) {
	for _, tt := range []struct {
		name    string
		yaml    string
		enabled bool
		owner   string
		wantErr string
	}{
		{name: "partial config", yaml: "api:\n  listen: 127.0.0.1:7752\n"},
		{name: "enabled with owner", yaml: "channels:\n  whatsapp:\n    enabled: true\n    owner: '+61400000000'\n", enabled: true, owner: "+61400000000"},
		{name: "enabled without owner", yaml: "channels:\n  whatsapp:\n    enabled: true\n", wantErr: "channels.whatsapp.owner is required when WhatsApp is enabled"},
		{name: "no file"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home(t)
			wantErr := tt.wantErr
			if tt.yaml == "" {
				wantErr = fmt.Sprintf("no config at %s (run `mirrin init`)", Path())
				if Default().Channels.WhatsApp.Enabled {
					t.Error("WhatsApp must be off before setup")
				}
			} else if err := os.WriteFile(Path(), []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load()
			if wantErr != "" {
				if err == nil || err.Error() != wantErr {
					t.Fatalf("Load error = %v, want %q", err, wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			check := func(c *Config) {
				t.Helper()
				if got := c.Channels.WhatsApp; got.Enabled != tt.enabled || got.Owner != tt.owner {
					t.Errorf("WhatsApp = %+v, want enabled=%v owner=%q", got, tt.enabled, tt.owner)
				}
			}
			check(cfg)
			if tt.name == "partial config" && cfg.API.Listen != "127.0.0.1:7752" {
				t.Errorf("listen = %q", cfg.API.Listen)
			}
			// Saving upgrades unversioned files to layers; the choice must survive.
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}
			again, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			check(again)
		})
	}
}
