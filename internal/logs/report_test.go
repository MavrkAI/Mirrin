package logs

import (
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
)

func TestConfigTextHidesSecretsAndPersonalDetails(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := config.Default()
	cfg.User.Name = "Noor Castellano"
	cfg.User.About = "Lives in Bondi, allergic to strawberries."
	cfg.LLM.APIKey = "sk-ant-inline-key-123456"
	cfg.LLM.Providers["openai"] = config.ProviderConfig{APIKey: "plain-openai-key", Model: "gpt-4.1", APIKeyEnv: "OPENAI_API_KEY"}
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.Token = "123456:telegram-token-value"
	cfg.Channels.Telegram.Owner = "987654321"
	cfg.Channels.WhatsApp.Owner = "61400111222"
	cfg.UI.Latitude, cfg.UI.Longitude = -33.86, 151.2
	cfg.User.Honorific = "Ms Castellano"
	cfg.Channels.Voice.Vocabulary = []string{"Priya Nair", "Dana Okafor"}
	cfg.MCP.Servers = []config.MCPServer{
		{Name: "github", Command: "npx", Args: []string{"server-github"}, Env: map[string]string{"GITHUB_TOKEN": "ghp_literal"}},
		{Name: "notes", Command: "notes-mcp", Args: []string{"--token", "abcdef0123456789abcdef", "--api-key=plainvalue42", "-e", "NOTES_SECRET=s3cr3tvalue", "--port", "8080", "--password"}},
	}
	txt, err := ConfigText(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"Noor", "Bondi", "sk-ant-inline", "plain-openai-key", "telegram-token-value", "987654321", "61400111222", "-33.86", "151.2", "ghp_literal",
		"Castellano", "Priya", "Okafor", "abcdef0123456789abcdef", "plainvalue42", "s3cr3tvalue"} {
		if strings.Contains(txt, leak) {
			t.Errorf("config text leaks %q:\n%s", leak, txt)
		}
	}
	for _, keep := range []string{"provider: anthropic", "model: claude-opus-5", "api_key_env: OPENAI_API_KEY", "max_tokens: 16000", "GITHUB_TOKEN: '[hidden]'", "name: github", "monthly_budget: 25",
		"- --token\n", "- --api-key=[hidden]", "- NOTES_SECRET=[hidden]", "- --port\n", "- \"8080\"", "- --password\n"} {
		if !strings.Contains(txt, keep) {
			t.Errorf("config text lacks %q:\n%s", keep, txt)
		}
	}
}

// Regression (backup merged with observability): the problem report, text
// meant to leave the machine, printed backup.recipient (whoever holds it can
// plant snapshots, which is why identity export leaves it out) and the
// recovery key that names the backup's folder.
func TestConfigTextHidesTheBackupKeys(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := config.Default()
	cfg.Backup = config.Backup{Recipient: "age1pq1probeprobeprobe", RecoveryPub: "recpubprobe", Target: "folder", Path: "/Volumes/NAS/backups"}
	txt, err := ConfigText(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"age1pq1probe", "recpubprobe"} {
		if strings.Contains(txt, leak) {
			t.Errorf("config text leaks %q:\n%s", leak, txt)
		}
	}
	if !strings.Contains(txt, "target: folder") {
		t.Errorf("where backups go is still useful:\n%s", txt)
	}
}

func TestReportTextScrubsWhateverTheSectionsHold(t *testing.T) {
	r := &Report{Made: time.Date(2026, 9, 27, 18, 30, 0, 0, time.UTC), Version: "1.2.3"}
	r.Add("Self-check", " ✗ Email  noor@castellano.example: login failed for sk-ant-api03-abcdefghijklmnop", " ✓ Channels  WhatsApp (whatsapp:61400111222)")
	txt := r.Text(NewRedactorWith())
	for _, leak := range []string{"noor@", "sk-ant-api03", "61400111222"} {
		if strings.Contains(txt, leak) {
			t.Errorf("report leaks %q:\n%s", leak, txt)
		}
	}
	for _, keep := range []string{"mirrin 1.2.3", "== Self-check", "n•••@castellano.example", "•••••22", "Nothing in this file has been sent anywhere"} {
		if !strings.Contains(txt, keep) {
			t.Errorf("report lacks %q:\n%s", keep, txt)
		}
	}
}
