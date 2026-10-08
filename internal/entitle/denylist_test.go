package entitle

import (
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func denyList() DenyList {
	return DenyList{
		Seq:     412,
		Iat:     t0,
		Handles: []Entry{{Value: "phish-bank-01", Why: "abuse"}},
		Keys:    []Entry{{Value: EncodeKey(devPriv("old-device").Public().(ed25519.PublicKey)), Why: "recover"}},
	}
}

func TestDenyListRoundTrip(t *testing.T) {
	d := denyList()
	tok, err := SignDenyList(d, "dl-test-a", devPriv("dl-test-a"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyDenyList(tok, testDL, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, d) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, d)
	}
	m, _, _, _ := parseV4(tok, MaxDenyListSize)
	want := `{"seq":412,"iat":"2026-09-27T12:00:00Z","handles":[{"h":"phish-bank-01","why":"abuse"}],"keys":[{"k":"` + d.Keys[0].Value + `","why":"recover"}]}`
	if string(m) != want {
		t.Fatalf("wire format:\n got %s\nwant %s", m, want)
	}
	if !got.DeniesHandle("phish-bank-01") || got.DeniesHandle("ember-otter-42") {
		t.Fatal("DeniesHandle")
	}
	if !got.DeniesKey(devPriv("old-device").Public().(ed25519.PublicKey)) || got.DeniesKey(devPriv("new-device").Public().(ed25519.PublicKey)) {
		t.Fatal("DeniesKey")
	}
}

func TestDenyListRejects(t *testing.T) {
	priv := devPriv("dl-test-a")
	d := denyList()
	d.Iat = t0.Add(time.Hour)
	future, _ := SignDenyList(d, "dl-test-a", priv)
	_, err := VerifyDenyList(future, testDL, t0)
	wantErr(t, "deny list from the future", err, ErrNotYetValid, "deny list iat is in the future")
	if _, err := VerifyDenyList(future, testDL, t0.Add(55*time.Minute)); err != nil {
		t.Errorf("within leeway: %v", err)
	}
	for name, c := range map[string]struct {
		d   DenyList
		msg string
	}{
		"seq zero":  {DenyList{Seq: 0, Iat: t0}, "seq starts at 1"},
		"no iat":    {DenyList{Seq: 1}, "iat is required"},
		"empty":     {DenyList{Seq: 1, Iat: t0, Handles: []Entry{{}}}, "empty handle"},
		"bad key":   {DenyList{Seq: 1, Iat: t0, Keys: []Entry{{Value: "not-a-key"}}}, "entry is not a public key"},
		"short key": {DenyList{Seq: 1, Iat: t0, Keys: []Entry{{Value: EncodeKey(make([]byte, 31))}}}, "entry is not a public key"},
	} {
		_, err := SignDenyList(c.d, "dl-test-a", priv)
		wantErr(t, "sign "+name, err, ErrMalformed, c.msg)
		// And the same list signed by hand is refused on reading.
		w := wireDenyList{Seq: c.d.Seq, Iat: formatTime(c.d.Iat), Handles: []wireHandle{}, Keys: []wireKey{}}
		for _, e := range c.d.Handles {
			w.Handles = append(w.Handles, wireHandle{H: e.Value})
		}
		for _, e := range c.d.Keys {
			w.Keys = append(w.Keys, wireKey{K: e.Value})
		}
		if c.d.Iat.IsZero() {
			w.Iat = "0001-01-01T00:00:00Z"
		}
		b, _ := json.Marshal(w)
		tok, err := seal(b, denyListPrefix, "dl-test-a", priv, MaxDenyListSize)
		if err != nil {
			t.Fatal(err)
		}
		_, err = VerifyDenyList(tok, testDL, t0)
		wantErr(t, "verify "+name, err, ErrMalformed, c.msg)
	}
	for name, c := range map[string]struct{ payload, msg string }{
		"unknown member":  {`{"seq":1,"iat":"2026-09-27T12:00:00Z","handles":[],"keys":[],"all":true}`, `unknown object member name "all"`},
		"entry member":    {`{"seq":1,"iat":"2026-09-27T12:00:00Z","handles":[{"h":"x","why":"","k":"y"}],"keys":[]}`, `unknown object member name "k"`},
		"duplicate seq":   {`{"seq":1,"seq":9,"iat":"2026-09-27T12:00:00Z","handles":[],"keys":[]}`, `duplicate object member name "seq"`},
		"negative seq":    {`{"seq":-1,"iat":"2026-09-27T12:00:00Z","handles":[],"keys":[]}`, "seq starts at 1"},
		"string seq":      {`{"seq":"1","iat":"2026-09-27T12:00:00Z","handles":[],"keys":[]}`, `within "/seq"`},
		"invalid utf-8":   {"{\"seq\":1,\"iat\":\"2026-09-27T12:00:00Z\",\"handles\":[{\"h\":\"\xff\",\"why\":\"\"}],\"keys\":[]}", "invalid UTF-8"},
		"not an object":   {`"deny everything"`, "unmarshal JSON string"}, // json/v2 says "cannot" or "unable to"
		"entitlement too": {`{"seq":1,"iat":"2026-09-27T12:00:00Z","handles":[],"keys":[],"aud":"mirrin"}`, `unknown object member name "aud"`},
		"iat lowercase z": {`{"seq":1,"iat":"2026-09-27T12:00:00z","handles":[],"keys":[]}`, "iat is not an RFC 3339 time"},
	} {
		tok, err := seal([]byte(c.payload), denyListPrefix, "dl-test-a", priv, MaxDenyListSize)
		if err != nil {
			t.Fatal(err)
		}
		_, err = VerifyDenyList(tok, testDL, t0)
		wantErr(t, name, err, ErrMalformed, c.msg)
	}
}

// Deny lists may outgrow an entitlement's 8 KiB, up to their own limit.
func TestDenyListSize(t *testing.T) {
	priv := devPriv("dl-test-a")
	d := DenyList{Seq: 7, Iat: t0}
	for i := 0; len(d.Keys) < 500; i++ {
		d.Keys = append(d.Keys, Entry{Value: EncodeKey(devPriv(strings.Repeat("k", i+1)).Public().(ed25519.PublicKey)), Why: "recover"})
	}
	tok, err := SignDenyList(d, "dl-test-a", priv)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) <= MaxTokenSize {
		t.Fatalf("setup: %d bytes", len(tok))
	}
	if got, err := VerifyDenyList(tok, testDL, t0); err != nil || len(got.Keys) != 500 {
		t.Fatalf("%d keys, %v", len(got.Keys), err)
	}
	if _, err := VerifyDenyList(strings.Repeat("A", MaxDenyListSize+1), testDL, t0); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize: %v", err)
	}
	for len(d.Keys) < 4000 {
		d.Keys = append(d.Keys, d.Keys[0])
	}
	if _, err := SignDenyList(d, "dl-test-a", priv); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("SignDenyList over the limit: %v", err)
	}
}
