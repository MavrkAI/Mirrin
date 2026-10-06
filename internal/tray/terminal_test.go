package tray

import (
	"reflect"
	"testing"
)

func TestTerminalCommandUsesQuotedAbsolutePath(t *testing.T) {
	cases := []struct {
		exe    string
		args   []string
		line   string
		script string
	}{
		{"/Applications/Mirrin.app/Contents/MacOS/mirrin", []string{"voice"},
			"/Applications/Mirrin.app/Contents/MacOS/mirrin voice",
			`tell application "Terminal" to do script "/Applications/Mirrin.app/Contents/MacOS/mirrin voice"`},
		{"/Users/Jo Smith/.local/bin/mirrin", []string{"voice", "setup"},
			"'/Users/Jo Smith/.local/bin/mirrin' voice setup",
			`tell application "Terminal" to do script "'/Users/Jo Smith/.local/bin/mirrin' voice setup"`},
		{`/tmp/it's "here"/mirrin`, []string{"chat"},
			`'/tmp/it'\''s "here"/mirrin' chat`,
			`tell application "Terminal" to do script "'/tmp/it'\\''s \"here\"/mirrin' chat"`},
	}
	for _, c := range cases {
		line := shellLine(c.exe, c.args)
		if line != c.line {
			t.Errorf("shellLine = %s\n want %s", line, c.line)
		}
		if got := terminalScript(line); got != c.script {
			t.Errorf("terminalScript = %s\n want %s", got, c.script)
		}
	}
	if exe, _ := selfCommand("chat"); exe == "mirrin" || exe == "" {
		t.Fatalf("selfCommand should resolve an absolute path, got %q", exe)
	}
}

func TestWindowsTerminalStaysOpen(t *testing.T) {
	got := windowsTerminal(`C:\Program Files\Mirrin\mirrin.exe`, []string{"voice", "setup"})
	want := []string{"/C", "start", "", "cmd", "/K", `C:\Program Files\Mirrin\mirrin.exe`, "voice", "setup"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}
