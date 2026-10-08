package config

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// Jev is off unless switched on with a key: a key in the environment or
// secrets.env alone never turns it on, and switching it on without a key
// doesn't either.
func TestJevNeedsBothTheSwitchAndAKey(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	t.Setenv("TYPESAFE_API_KEY", "")
	c := Default()
	if c.JevOn() || c.JevKeyEnv() != "TYPESAFE_API_KEY" || c.JevModel() != "jev-latest" {
		t.Fatalf("default: on %v env %s model %s", c.JevOn(), c.JevKeyEnv(), c.JevModel())
	}
	t.Setenv("TYPESAFE_API_KEY", "ts-exported")
	if c.JevOn() {
		t.Fatal("an exported key turned Jev on by itself")
	}
	c.Jev.Enabled = true
	if !c.JevOn() || c.JevKey() != "ts-exported" {
		t.Fatalf("on with an exported key: %v %q", c.JevOn(), c.JevKey())
	}
	t.Setenv("TYPESAFE_API_KEY", "")
	if c.JevOn() {
		t.Fatal("on without a key")
	}
	if err := SaveSecrets(map[string]string{"TYPESAFE_API_KEY": "ts-saved"}); err != nil {
		t.Fatal(err)
	}
	if !c.JevOn() || c.JevKey() != "ts-saved" {
		t.Fatalf("a key saved while running isn't found: %q", c.JevKey())
	}
	c.Jev.APIKeyEnv, c.Jev.Model = "MY_TS_KEY", "jev-1"
	t.Setenv("MY_TS_KEY", "ts-mine")
	if c.JevKey() != "ts-mine" || c.JevModel() != "jev-1" {
		t.Fatalf("named variable: %q %s", c.JevKey(), c.JevModel())
	}
	c.Jev.TypeSafeAPIKey = "ts-literal"
	if c.JevKey() != "ts-literal" {
		t.Fatalf("literal key: %q", c.JevKey())
	}
}

// An installed service gets the TypeSafe key, under either name.
func TestSecretEnvsCarryTheJevKey(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	c := Default()
	c.Jev.APIKeyEnv = "MY_TS_KEY"
	got := c.SecretEnvs()
	for _, want := range []string{"TYPESAFE_API_KEY", "MY_TS_KEY"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
}

// An existing config has no jev section, and saving the default adds none.
func TestJevStaysOutOfASavedConfig(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	c := Default()
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(Path()); strings.Contains(string(b), "jev") {
		t.Fatalf("a default config mentions jev:\n%s", b)
	}
	c.Jev.Enabled = true
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Jev.Enabled || got.Jev.TypeSafeAPIKey != "" {
		t.Fatalf("loaded %+v", got.Jev)
	}
}

func TestForgetSecrets(t *testing.T) {
	home := t.TempDir()
	if err := ForgetSecretsIn(home, "TYPESAFE_API_KEY"); err != nil {
		t.Fatalf("no file: %v", err)
	}
	if err := SaveSecretsIn(home, map[string]string{"TYPESAFE_API_KEY": "ts", "OPENAI_API_KEY": "sk"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(SecretsPathIn(home), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("# a note of mine\nexport TYPESAFE_API_KEY=old\n")
	f.Close()
	if err := ForgetSecretsIn(home, "TYPESAFE_API_KEY", ""); err != nil {
		t.Fatal(err)
	}
	vals, err := ReadSecretsIn(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vals["TYPESAFE_API_KEY"]; ok || vals["OPENAI_API_KEY"] != "sk" {
		t.Fatalf("after forgetting: %v", vals)
	}
	b, _ := os.ReadFile(SecretsPathIn(home))
	if !strings.Contains(string(b), "# a note of mine") {
		t.Fatalf("other lines lost:\n%s", b)
	}
	if st, _ := os.Stat(SecretsPathIn(home)); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if _, err := os.Stat(SecretsPathIn(home) + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("a temporary file was left behind")
	}
	// Nothing to forget leaves the file alone.
	before, _ := os.Stat(SecretsPathIn(home))
	if err := ForgetSecretsIn(home, "NOT_THERE"); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(SecretsPathIn(home)); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("rewrote the file with nothing to forget")
	}
}
