package config

import (
	"path/filepath"
	"testing"
)

// tempUserHome points the user's home at a temporary folder and clears
// MIRRIN_HOME until the test ends. Nothing outside the temporary folder is
// looked at.
func tempUserHome(t *testing.T) string {
	t.Helper()
	user := t.TempDir()
	prev := userHomeDir
	userHomeDir = func() (string, error) { return user, nil }
	t.Setenv("MIRRIN_HOME", "")
	t.Cleanup(func() { userHomeDir = prev })
	return user
}

// MIRRIN_HOME names the home; with none, or one naming the default (as the
// setup program writes it), it is ~/.mirrin.
func TestHomePrecedence(t *testing.T) {
	user := tempUserHome(t)
	custom := t.TempDir()
	def := filepath.Join(user, ".mirrin")
	for _, c := range []struct{ name, env, want string }{
		{"named", custom, custom},
		{"the default named", def, def},
		{"none", "", def},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("MIRRIN_HOME", c.env)
			if got := Home(); got != c.want {
				t.Errorf("Home() = %q, want %q", got, c.want)
			}
		})
	}
	if got := HomeEnv(func(string) string { return def }); got != "" {
		t.Errorf("HomeEnv with the default home = %q, want \"\"", got)
	}
}

// A key is found in the environment first, then in the secrets file.
func TestSecretEnvironmentFirst(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	t.Setenv("MIRRIN_EMAIL_PASSWORD", "")
	if err := SaveSecrets(map[string]string{"MIRRIN_EMAIL_PASSWORD": "saved"}); err != nil {
		t.Fatal(err)
	}
	if got := Secret("MIRRIN_EMAIL_PASSWORD"); got != "saved" {
		t.Fatalf("from the file: %q", got)
	}
	if name, v := Exported("MIRRIN_EMAIL_PASSWORD"); name != "" || v != "" {
		t.Fatalf("Exported with nothing set = %q %q", name, v)
	}
	t.Setenv("MIRRIN_EMAIL_PASSWORD", "exported")
	if got := Secret("MIRRIN_EMAIL_PASSWORD"); got != "exported" {
		t.Fatalf("an exported key didn't win over the file: %q", got)
	}
	if name, v := Exported("MIRRIN_EMAIL_PASSWORD"); name != "MIRRIN_EMAIL_PASSWORD" || v != "exported" {
		t.Fatalf("Exported = %q %q", name, v)
	}
	if got := Secret("OPENAI_API_KEY_NOT_OURS"); got != "" {
		t.Fatalf("another program's variable: %q", got)
	}
}

// The default mailbox and bucket key names are read from the secrets file.
func TestS3AndEmailDefaultKeys(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	for _, k := range []string{"MIRRIN_EMAIL_PASSWORD", "MIRRIN_S3_ACCESS_KEY_ID", "MIRRIN_S3_SECRET_ACCESS_KEY"} {
		t.Setenv(k, "")
	}
	if err := SaveSecrets(map[string]string{"MIRRIN_EMAIL_PASSWORD": "mail", "MIRRIN_S3_ACCESS_KEY_ID": "AKIA", "MIRRIN_S3_SECRET_ACCESS_KEY": "shh"}); err != nil {
		t.Fatal(err)
	}
	c := Default()
	if c.Skills.Email.PasswordEnv != "MIRRIN_EMAIL_PASSWORD" || c.EmailPassword() != "mail" {
		t.Fatalf("email: %q %q", c.Skills.Email.PasswordEnv, c.EmailPassword())
	}
	access, secret := BackupS3{}.KeyEnvs()
	if access != "MIRRIN_S3_ACCESS_KEY_ID" || Secret(access) != "AKIA" || Secret(secret) != "shh" {
		t.Fatalf("s3: %q %q %q", access, Secret(access), Secret(secret))
	}
}

func TestServiceMarkers(t *testing.T) {
	for _, k := range []string{"MIRRIN_SERVICE", "XPC_SERVICE_NAME"} {
		t.Setenv(k, "")
	}
	if UnderServiceManager() {
		t.Fatal("nothing set")
	}
	for _, c := range [][2]string{{"MIRRIN_SERVICE", "1"}, {"XPC_SERVICE_NAME", "mirrin"}} {
		t.Setenv(c[0], c[1])
		if !UnderServiceManager() {
			t.Errorf("%s=%s isn't the service manager", c[0], c[1])
		}
		t.Setenv(c[0], "")
	}
	t.Setenv("XPC_SERVICE_NAME", "application.com.apple.Terminal.1")
	if UnderServiceManager() {
		t.Fatal("a terminal's label")
	}
	t.Setenv("MIRRIN_SERVICE", "1")
	if !ServiceMarked() {
		t.Fatal("the service marker")
	}
}
