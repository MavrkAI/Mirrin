package entitle

import (
	"testing"
	"time"
)

// seedTokens is every official vector token plus a valid entitlement and deny
// list.
func seedTokens(f *testing.F) {
	for _, file := range []string{"paseto-v4-public.json", "paseto-v3.json"} {
		for _, v := range loadVectors(f, file) {
			f.Add(v.Token)
		}
	}
	ent, err := Sign(claims(f), "ent-test-a", devPriv("ent-test-a"))
	if err != nil {
		f.Fatal(err)
	}
	dl, err := SignDenyList(denyList(), "dl-test-a", devPriv("dl-test-a"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(ent)
	f.Add(dl)
	f.Add("v4.public.")
	f.Add("v4.public.AAAA.AAAA")
}

// A token that parses has exactly one encoding: re-encoding what was parsed
// gives back the input byte for byte.
func FuzzParseV4(f *testing.F) {
	seedTokens(f)
	f.Fuzz(func(t *testing.T, tok string) {
		m, sig, foot, err := parseV4(tok, MaxDenyListSize)
		if err != nil {
			return
		}
		again := v4Public + b64.EncodeToString(append(append([]byte{}, m...), sig...))
		if len(foot) > 0 {
			again += "." + b64.EncodeToString(foot)
		}
		if again != tok {
			t.Fatalf("non-canonical token accepted:\n in  %q\n out %q", tok, again)
		}
	})
}

func FuzzVerify(f *testing.F) {
	seedTokens(f)
	f.Fuzz(func(t *testing.T, tok string) {
		c, err := Verify(tok, testEnt, t0.Add(time.Hour))
		if err != nil {
			return
		}
		if len(tok) > MaxTokenSize || c.check() != nil {
			t.Fatalf("accepted a bad token: %q", tok)
		}
	})
}

func FuzzVerifyDenyList(f *testing.F) {
	seedTokens(f)
	f.Fuzz(func(t *testing.T, tok string) {
		d, err := VerifyDenyList(tok, testDL, t0.Add(time.Hour))
		if err != nil {
			return
		}
		if len(tok) > MaxDenyListSize || d.check() != nil {
			t.Fatalf("accepted a bad deny list: %q", tok)
		}
	})
}
