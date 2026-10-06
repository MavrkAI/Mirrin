package dns

import (
	"strings"
	"testing"
)

// Nightly CI runs each of these for 60 s (.github/workflows/fuzz.yml).

// No ACME account that HandleCAA accepts can add a parameter or end the
// quoted value early.
func FuzzHandleCAA(f *testing.F) {
	for _, s := range []string{"https://acme-v02.api.letsencrypt.org/acme/acct/1", `https://a/"`, "https://a/;x=y", "https://a/ b", `\`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, acct string) {
		caa, err := HandleCAA(acct, "")
		if err != nil || acct == "" {
			return
		}
		v := caa[0].Value
		if strings.Count(v, ";") != 2 || strings.ContainsAny(acct, "\"\\; \t\r\n") {
			t.Fatalf("%q made %s", acct, caa[0])
		}
	})
}
