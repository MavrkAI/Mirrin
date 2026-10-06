package tray

import (
	"os"
	"strings"
)

// selfCommand is how a terminal window should run this same mirrin binary
// with args: by absolute path, because the app bundle and ~/.local/bin are
// often not on the shell's PATH.
func selfCommand(args ...string) (exe string, argv []string) {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "mirrin"
	}
	return exe, args
}

// shellLine quotes exe and args for a POSIX shell.
func shellLine(exe string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	for _, a := range append([]string{exe}, args...) {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@%+,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// appleScriptString quotes s as an AppleScript string literal.
func appleScriptString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// terminalScript is the AppleScript that runs line in a new Terminal window.
func terminalScript(line string) string {
	return "tell application \"Terminal\" to do script " + appleScriptString(line)
}

// windowsTerminal is the cmd arguments that open a console running exe with
// argv and keep it open afterwards (/K), so a message mirrin prints before
// it exits stays readable. start's first quoted argument is the window title.
func windowsTerminal(exe string, argv []string) []string {
	return append([]string{"/C", "start", "", "cmd", "/K", exe}, argv...)
}
