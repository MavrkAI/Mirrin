package config

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// SecretsPath is the 0600 file of KEY=value lines that keeps API keys and
// tokens given as environment variables, so the background service, a fresh
// terminal and the menu bar app all see them without copying them into
// launchd or systemd files.
func SecretsPath() string { return SecretsPathIn(Home()) }

// SecretsPathIn is the secrets file of the twin in home.
func SecretsPathIn(home string) string { return filepath.Join(home, "secrets.env") }

// Secret is the value of a secret named by its environment variable: the
// environment first, so an exported key overrides a saved one, then the
// secrets file. It is read each time, so a key saved while the twin runs is
// found without a restart.
func Secret(name string) string {
	if name == "" {
		return ""
	}
	if _, v := Exported(name); v != "" {
		return v
	}
	vals, _ := ReadSecrets()
	return vals[name]
}

// Exported is name and its value when it is set in this process's
// environment, or "", "" when it isn't.
func Exported(name string) (string, string) {
	if v := os.Getenv(name); v != "" {
		return name, v
	}
	return "", ""
}

// exported are the saved secrets still read straight from the environment
// by code that doesn't use Secret yet: none now (the voice reads
// ELEVENLABS_API_KEY with Secret). Secrets stay out of the environment, so
// programs mirrin starts (MCP servers, protocol-pack tools, the shell)
// don't inherit the owner's keys.
var exported []string

// LoadSecrets puts the few saved secrets that are read from the environment
// into this process's environment; the rest are looked up with Secret. A
// variable that is already set wins. A missing file is not an error.
func LoadSecrets() error {
	vals, err := ReadSecrets()
	if err != nil {
		return err
	}
	for _, k := range exported {
		if v := vals[k]; v != "" && os.Getenv(k) == "" {
			if err := os.Setenv(k, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReadSecrets returns the saved secrets.
func ReadSecrets() (map[string]string, error) { return ReadSecretsIn(Home()) }

// ReadSecretsIn returns the secrets saved for the twin in home.
func ReadSecretsIn(home string) (map[string]string, error) {
	b, err := os.ReadFile(SecretsPathIn(home))
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok || strings.TrimSpace(k) == "" {
			continue
		}
		v = strings.TrimSpace(v)
		if u, err := strconv.Unquote(v); err == nil {
			v = u
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, sc.Err()
}

// SaveSecrets merges vals into the secrets file (0600). Empty values are
// ignored rather than erasing a saved secret.
func SaveSecrets(vals map[string]string) error { return SaveSecretsIn(Home(), vals) }

// SaveSecretsIn is SaveSecrets for the twin in home.
func SaveSecretsIn(home string, vals map[string]string) error {
	cur, err := ReadSecretsIn(home)
	if err != nil {
		return err
	}
	changed := false
	for k, v := range vals {
		if v != "" && cur[k] != v {
			cur[k] = v
			changed = true
		}
	}
	if !changed {
		return nil
	}
	keys := make([]string, 0, len(cur))
	for k := range cur {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# API keys and tokens for Mirrin. Only you can read this file.\n")
	b.WriteString("# The environment wins over these; delete a line to forget a secret.\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, strconv.Quote(cur[k]))
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	path := SecretsPathIn(home)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SecretEnvs lists the environment variables this config reads secrets from:
// model keys, channel tokens, the mailbox password, speech keys and the
// backup bucket's key pair.
func (c *Config) SecretEnvs() []string {
	names := []string{
		// Read directly by the model gateway and the voice, whatever the config says.
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "ELEVENLABS_API_KEY",
		DefaultJevKeyEnv, c.Jev.APIKeyEnv,
		c.LLM.APIKeyEnv,
		c.Channels.Telegram.TokenEnv, c.Channels.Discord.TokenEnv,
		c.Channels.Slack.BotTokenEnv, c.Channels.Slack.AppTokenEnv,
		c.Channels.Matrix.AccessTokenEnv, c.Channels.Mattermost.TokenEnv,
		c.Channels.IRC.PasswordEnv, c.Channels.Zulip.APIKeyEnv,
		c.Skills.Email.PasswordEnv, c.Phone.AuthTokenEnv,
	}
	for _, p := range c.LLM.Providers {
		names = append(names, p.APIKeyEnv)
	}
	if c.Backup.Target == "s3" {
		access, secret := c.Backup.S3.KeyEnvs()
		names = append(names, access, secret)
	}
	// An MCP server's "$NAME" is this process's NAME (procenv.Expand), so an
	// installed service needs it saved too.
	for _, srv := range c.MCP.Servers {
		for _, v := range srv.Env {
			if m := reEnvRef.FindStringSubmatch(strings.TrimSpace(v)); m != nil {
				names = append(names, m[1])
			}
		}
	}
	// A custom tool sees the variables its tool.yaml names (procenv.With),
	// so its API key must reach the installed service too.
	names = append(names, toolEnvs(c.ToolsDir)...)
	seen := map[string]bool{}
	out := names[:0]
	for _, n := range names {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// toolEnvs are the env: names of the custom tools in dir (each folder's
// tool.yaml; config can't import the custom package). A tool that can't be
// read is skipped: the loader reports it.
func toolEnvs(dir string) []string {
	if dir == "" {
		dir = filepath.Join(Home(), "tools")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*", "tool.yaml"))
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var m struct {
			Env []string `yaml:"env"`
		}
		if yaml.Unmarshal(b, &m) != nil {
			continue
		}
		for _, n := range m.Env {
			if n = strings.TrimPrefix(strings.TrimSpace(n), "$"); reEnvName.MatchString(n) {
				out = append(out, n)
			}
		}
	}
	return out
}

var reEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reEnvRef is a value naming a variable, "$NAME" or "${NAME}" (as procenv
// reads it; config can't import procenv).
var reEnvRef = regexp.MustCompile(`^\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?$`)

// Clone returns a deep copy, so a change can be tried and thrown away without
// touching the original's maps and slices.
func (c *Config) Clone() *Config {
	out := *c
	if b, err := yaml.Marshal(c); err == nil {
		var cp Config
		if yaml.Unmarshal(b, &cp) == nil {
			return &cp
		}
	}
	return &out
}
