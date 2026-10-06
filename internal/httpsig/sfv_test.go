package httpsig

import "testing"

func TestParseDictionary(t *testing.T) {
	// Valid inputs and their canonical serialization. The first three are
	// RFC 8941 examples.
	for in, want := range map[string]string{
		`en="Applepie", da=:w4ZibGV0w6ZydGUK:`:          `en="Applepie", da=:w4ZibGV0w6ZydGUK:`,
		`a=?0, b, c; foo=bar`:                           `a=?0, b, c;foo=bar`,
		`a=(1 2), b=3, c=4;aa=bb, d=(5 6);valid`:        `a=(1 2), b=3, c=4;aa=bb, d=(5 6);valid`,
		` sig1=( "@method"  "x" );created=1;keyid="k" `: `sig1=("@method" "x");created=1;keyid="k"`,
		`a=-12,b=*tok/en:x, c="q\"s\\"`:                 `a=-12, b=*tok/en:x, c="q\"s\\"`,
		`a=()`:                                          `a=()`,
		"a=1,\tb=2":                                     `a=1, b=2`,
		`a=?1;x=?0`:                                     `a;x=?0`, // true members are bare keys (RFC 8941 4.1.2)
	} {
		ms, err := parseDictionary(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got := serializeDictionary(ms); got != want {
			t.Errorf("%q serialized as %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{
		`a=1,`, `a=1,,b=2`, `A=1`, `1a=2`, `a="\x"`, `a="é"`, "a=\"\x7f\"", `a="open`,
		`a=:abc:`, `a=:YQ:`, `a=:a-b=:`, `a=:open`, `a=1 b=2`, `a=(1 2`, `a=(1,2)`,
		`a=1234567890123456`, `a=-`, `a=1.5`, `a=?2`, `a=@1`, `a=%"x"`, `a=1;b;b`,
		`a=1, a=2`, `a=1;B=2`, `a=,`, `=1`,
	} {
		if ms, err := parseDictionary(in); err == nil {
			t.Errorf("%q parsed: %+v", in, ms)
		}
	}
}
