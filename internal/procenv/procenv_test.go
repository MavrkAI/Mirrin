package procenv

import (
	"strings"
	"testing"
)

func TestChildrenGetOnlyTheBasicsAndWhatWasNamed(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret")
	t.Setenv("TELEGRAM_BOT_TOKEN", "123:abc")
	t.Setenv("PARCEL_API_KEY", "parcel-key")
	t.Setenv("JAVA_HOME", "/opt/jdk")
	t.Setenv("DISPLAY", ":0")

	has := func(env []string, kv string) bool {
		for _, e := range env {
			if e == kv {
				return true
			}
		}
		return false
	}
	for _, tc := range []struct {
		name    string
		env     []string
		want    []string
		without []string
	}{
		{"base", Base(), []string{"PATH=/usr/bin:/bin", "JAVA_HOME=/opt/jdk", "DISPLAY=:0"}, []string{"ANTHROPIC_API_KEY=sk-secret", "TELEGRAM_BOT_TOKEN=123:abc", "PARCEL_API_KEY=parcel-key"}},
		{"named", With("PARCEL_API_KEY", "NOT_SET_ANYWHERE"), []string{"PATH=/usr/bin:/bin", "PARCEL_API_KEY=parcel-key"}, []string{"ANTHROPIC_API_KEY=sk-secret", "TELEGRAM_BOT_TOKEN=123:abc"}},
	} {
		for _, w := range tc.want {
			if !has(tc.env, w) {
				t.Errorf("%s: missing %s in %v", tc.name, w, tc.env)
			}
		}
		for _, w := range tc.without {
			if has(tc.env, w) {
				t.Errorf("%s: leaked %s", tc.name, w)
			}
		}
		for _, e := range tc.env {
			if strings.HasPrefix(e, "NOT_SET_ANYWHERE") {
				t.Errorf("%s: unset name produced %q", tc.name, e)
			}
		}
	}
}

func TestExpand(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_x")
	for in, want := range map[string]string{
		"$GITHUB_TOKEN":    "ghp_x",
		"${GITHUB_TOKEN}":  "ghp_x",
		"literal":          "literal",
		"$NOPE_NOT_SET":    "",
		"pre$GITHUB_TOKEN": "pre$GITHUB_TOKEN",
	} {
		if got := Expand(in); got != want {
			t.Errorf("Expand(%q) = %q, want %q", in, got, want)
		}
	}
}
