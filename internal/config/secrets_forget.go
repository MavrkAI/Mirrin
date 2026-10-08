package config

import (
	"bufio"
	"bytes"
	"os"
	"strings"
)

// ForgetSecrets removes the named secrets from secrets.env, keeping every
// other line (comments included) as it was. The file is replaced in one
// step, so a crash leaves the old one or the new one, never half of each.
// A missing file, or a name that isn't there, is not an error.
func ForgetSecrets(names ...string) error { return ForgetSecretsIn(Home(), names...) }

// ForgetSecretsIn is ForgetSecrets for the twin in home.
func ForgetSecretsIn(home string, names ...string) error {
	path := SecretsPathIn(home)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	drop := map[string]bool{}
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			drop[n] = true
		}
	}
	var out bytes.Buffer
	changed := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		t := strings.TrimSpace(line)
		if t != "" && !strings.HasPrefix(t, "#") {
			if k, _, ok := strings.Cut(strings.TrimPrefix(t, "export "), "="); ok && drop[strings.TrimSpace(k)] {
				changed = true
				continue
			}
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out.Bytes(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
