package brand

import (
	"slices"
	"testing"
)

// The current name wins, then AntBot's, then openHuman's.
func TestEnvPrefersTheCurrentName(t *testing.T) {
	t.Setenv("MIRRIN_BRANDTEST", "")
	t.Setenv("ANTBOT_BRANDTEST", "")
	t.Setenv("OPENHUMAN_BRANDTEST", "")
	if got := Env("BRANDTEST"); got != "" {
		t.Fatalf("nothing set: %q", got)
	}
	t.Setenv("OPENHUMAN_BRANDTEST", "openhuman")
	if got := Env("BRANDTEST"); got != "openhuman" {
		t.Fatalf("only the oldest set: %q", got)
	}
	t.Setenv("ANTBOT_BRANDTEST", "antbot")
	if got := Env("BRANDTEST"); got != "antbot" {
		t.Fatalf("AntBot's over openHuman's: %q", got)
	}
	t.Setenv("MIRRIN_BRANDTEST", "mirrin")
	if got := Env("BRANDTEST"); got != "mirrin" {
		t.Fatalf("the current name over both: %q", got)
	}
	// Getenv starts from the name it is given.
	if got := Getenv("ANTBOT_BRANDTEST"); got != "antbot" {
		t.Fatalf("Getenv(ANTBOT_…) = %q", got)
	}
	t.Setenv("ANTBOT_BRANDTEST", "")
	if got := Getenv("ANTBOT_BRANDTEST"); got != "mirrin" {
		t.Fatalf("Getenv(ANTBOT_…) with only the others set = %q", got)
	}
}

func TestEnvAliases(t *testing.T) {
	cases := map[string][]string{
		"MIRRIN_EMAIL_PASSWORD":    {"MIRRIN_EMAIL_PASSWORD", "ANTBOT_EMAIL_PASSWORD", "OPENHUMAN_EMAIL_PASSWORD"},
		"ANTBOT_EMAIL_PASSWORD":    {"ANTBOT_EMAIL_PASSWORD", "MIRRIN_EMAIL_PASSWORD", "OPENHUMAN_EMAIL_PASSWORD"},
		"OPENHUMAN_EMAIL_PASSWORD": {"OPENHUMAN_EMAIL_PASSWORD", "MIRRIN_EMAIL_PASSWORD", "ANTBOT_EMAIL_PASSWORD"},
		"OPENAI_API_KEY":           {"OPENAI_API_KEY"},
		"MIRRIN_":                  {"MIRRIN_"},
		"":                         {""},
	}
	for name, want := range cases {
		if got := EnvAliases(name); !slices.Equal(got, want) {
			t.Errorf("EnvAliases(%q) = %v, want %v", name, got, want)
		}
	}
	for name, want := range map[string]string{
		"ANTBOT_S3_ACCESS_KEY_ID": "MIRRIN_S3_ACCESS_KEY_ID",
		"OPENHUMAN_X":             "MIRRIN_X",
		"MIRRIN_X":                "MIRRIN_X",
		"TELEGRAM_BOT_TOKEN":      "TELEGRAM_BOT_TOKEN",
	} {
		if got := CurrentEnv(name); got != want {
			t.Errorf("CurrentEnv(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestIsServiceLabel(t *testing.T) {
	for label, want := range map[string]bool{
		"mirrin": true, "antbot": true, "openhuman": true,
		"": false, "0": false, "application.com.apple.Terminal.123": false, "Mirrin": false,
	} {
		if got := IsServiceLabel(label); got != want {
			t.Errorf("IsServiceLabel(%q) = %v", label, got)
		}
	}
}

func TestLegacyNamesLineUp(t *testing.T) {
	if len(LegacyNames) != len(LegacyHomeDirs) || len(LegacyNames) != len(LegacyDisplayNames) {
		t.Fatal("every old name needs its home folder and display name")
	}
	for name, want := range map[string]string{"antbot": "AntBot", ".openhuman": "openHuman", "mirrin": "Mirrin"} {
		if got := LegacyDisplayName(name); got != want {
			t.Errorf("LegacyDisplayName(%q) = %q, want %q", name, got, want)
		}
	}
}

// The names everything else is built from: the command and service label,
// the name people read and the home folder, then the old ones, newest first.
// A machine can skip versions, so an old name is never dropped.
func TestNames(t *testing.T) {
	if Name != "mirrin" || DisplayName != "Mirrin" || HomeDirName != ".mirrin" {
		t.Fatalf("names: %q %q %q", Name, DisplayName, HomeDirName)
	}
	if !slices.Equal(LegacyNames, []string{"antbot", "openhuman"}) ||
		!slices.Equal(LegacyHomeDirs, []string{".antbot", ".openhuman"}) ||
		!slices.Equal(LegacyDisplayNames, []string{"AntBot", "openHuman"}) {
		t.Fatalf("old names: %v %v %v", LegacyNames, LegacyHomeDirs, LegacyDisplayNames)
	}
	if got := EnvAliases("MIRRIN_HOME"); !slices.Equal(got, []string{"MIRRIN_HOME", "ANTBOT_HOME", "OPENHUMAN_HOME"}) {
		t.Fatalf("the home's variables: %v", got)
	}
}
