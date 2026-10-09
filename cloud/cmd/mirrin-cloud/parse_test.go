package main

import (
	"flag"
	"strings"
	"testing"
)

// A denied key in URL-safe base64 can start with "-": it is still the key,
// not a flag, and "admin deny key -abc --why x" must not fail with "flag
// provided but not defined". Flags still work before, between and after
// the words, and "--" ends them. A misspelt flag is a word the command
// then refuses as it would any stray word.
func TestParseKeepsDashedPositionals(t *testing.T) {
	for _, tc := range []struct {
		args []string
		pos  string
		why  string
	}{
		{[]string{"deny", "key", "-Zm9vYmFy", "--why", "test"}, "deny key -Zm9vYmFy", "test"},
		{[]string{"--why=x", "deny", "-why", "y", "key", "--", "--why", "-k"}, "deny key --why -k", "y"},
		{[]string{"deny", "key", "k", "--bogus"}, "deny key k --bogus", ""},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		why := fs.String("why", "", "")
		pos, err := parse(fs, tc.args)
		if err != nil {
			t.Fatalf("%q: err %v", tc.args, err)
		}
		if got := strings.Join(pos, " "); got != tc.pos || *why != tc.why {
			t.Fatalf("%q: pos %q why %q", tc.args, got, *why)
		}
	}
}
