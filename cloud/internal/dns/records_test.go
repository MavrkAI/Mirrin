package dns

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/sigv4"
)

var (
	v4       = []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("198.51.100.1")}
	v6       = []netip.Addr{netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("2001:db8::2")}
	acmeAcct = "https://acme-v02.api.letsencrypt.org/acme/acct/123456789"
	creds    = sigv4.Creds{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	signedAt = time.Date(2026, 9, 27, 3, 30, 0, 0, time.UTC)
)

func TestHandleCAA(t *testing.T) {
	caa, err := HandleCAA(acmeAcct, "mailto:security@mirrin.app")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`0 issue "letsencrypt.org; accounturi=https://acme-v02.api.letsencrypt.org/acme/acct/123456789; validationmethods=tls-alpn-01"`,
		`0 issuewild ";"`,
		`0 iodef "mailto:security@mirrin.app"`,
	}
	for i, c := range caa {
		if c.String() != want[i] {
			t.Errorf("record %d: %s, want %s", i, c, want[i])
		}
	}
	none, _ := HandleCAA("", "")
	if len(none) != 2 || none[0].String() != `0 issue ";"` || none[1].String() != `0 issuewild ";"` {
		t.Errorf("without an account: %v", none)
	}
	for _, bad := range []string{`https://a/"x`, "https://a/;validationmethods=http-01", `https://a/\x`, "https://a/ b", "https://a/\x01"} {
		if _, err := HandleCAA(bad, ""); err == nil {
			t.Errorf("account %q went into a CAA record", bad)
		}
	}
	if _, err := HandleCAA("", "http://example.com"); err == nil {
		t.Error("an http iodef was accepted")
	}
}

// The ChangeResourceRecordSets body is exactly the golden file: A and AAAA
// for the relays, and the CAA set, upserted.
func TestRoute53UpsertGolden(t *testing.T) {
	r := &Route53{Zone: "mirrin.link", HostedZoneID: "Z0123456789ABCDEFGHIJ"}
	caa, _ := HandleCAA(acmeAcct, "mailto:security@mirrin.app")
	got, err := r.UpsertRequest("ember-otter-42", v4, v6, caa)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/upsert-ember-otter-42.xml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("request body:\n%s\nwant:\n%s", got, want)
	}
	// And it says what it should, read back as XML.
	var req changeRequest
	if err := xml.Unmarshal(got, &req); err != nil {
		t.Fatal(err)
	}
	if req.XMLNS != route53NS || len(req.ChangeBatch.Changes) != 3 {
		t.Fatalf("%+v", req)
	}
	for _, c := range req.ChangeBatch.Changes {
		if c.Action != "UPSERT" || c.Set.Name != "ember-otter-42.mirrin.link." || c.Set.TTL != 300 {
			t.Errorf("%+v", c)
		}
	}
	if v := req.ChangeBatch.Changes[2].Set.Records[0].Value; v != caa[0].String() {
		t.Errorf("CAA value round trip: %s", v)
	}
	for name, bad := range map[string]func() ([]byte, error){
		"bad handle":   func() ([]byte, error) { return r.UpsertRequest("Ember", v4, v6, caa) },
		"no addresses": func() ([]byte, error) { return r.UpsertRequest("ember", nil, nil, caa) },
		"v6 as v4":     func() ([]byte, error) { return r.UpsertRequest("ember", v6, nil, caa) },
		"no CAA":       func() ([]byte, error) { return r.UpsertRequest("ember", v4, v6, nil) },
		"quote in CAA": func() ([]byte, error) { return r.UpsertRequest("ember", v4, v6, []CAA{{Tag: "issue", Value: `a"b`}}) },
		"bad zone id": func() ([]byte, error) {
			return (&Route53{Zone: "mirrin.link", HostedZoneID: "../x"}).UpsertRequest("ember", v4, v6, caa)
		},
	} {
		if _, err := bad(); err == nil {
			t.Errorf("%s: built a request", name)
		}
	}
}

// fakeRoute53 records the calls and checks each one's SigV4 signature by
// signing a copy of it again with internal/sigv4.
type fakeRoute53 struct {
	t     *testing.T
	mu    sync.Mutex
	calls []call
	list  string
	quiet bool // a bad signature is expected
}

type call struct {
	method, path, query, auth string
	body                      []byte
}

func (f *fakeRoute53) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	again, _ := http.NewRequest(r.Method, "http://"+r.Host+r.URL.RequestURI(), bytes.NewReader(body))
	for _, h := range []string{"Content-Type", "X-Amz-Date"} {
		if v := r.Header.Get(h); v != "" {
			again.Header.Set(h, v)
		}
	}
	date, _ := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err := sigv4.Sign(again, creds, "us-east-1", "route53", date, sigv4.PayloadHash(body)); err != nil || again.Header.Get("Authorization") != r.Header.Get("Authorization") {
		f.mu.Lock()
		if !f.quiet {
			f.t.Errorf("%s %s: signature does not verify (%v)", r.Method, r.URL.Path, err)
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<ErrorResponse><Error><Code>SignatureDoesNotMatch</Code><Message>no</Message></Error></ErrorResponse>`)
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), body})
	f.mu.Unlock()
	f.mu.Lock()
	list := f.list
	f.mu.Unlock()
	if r.Method == http.MethodGet {
		io.WriteString(w, list)
		return
	}
	io.WriteString(w, `<ChangeResourceRecordSetsResponse xmlns="https://route53.amazonaws.com/doc/2013-04-01/"><ChangeInfo><Id>/change/C1</Id><Status>PENDING</Status></ChangeInfo></ChangeResourceRecordSetsResponse>`)
}

// snapshot copies the calls so far.
func (f *fakeRoute53) snapshot() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func TestRoute53Calls(t *testing.T) {
	f := &fakeRoute53{t: t}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := &Route53{Zone: "mirrin.link", HostedZoneID: "Z0123456789ABCDEFGHIJ", Creds: creds, Endpoint: srv.URL,
		Now: func() time.Time { return signedAt }}
	caa, _ := HandleCAA(acmeAcct, "mailto:security@mirrin.app")
	if err := r.UpsertHandle(t.Context(), "ember-otter-42", v4, v6, caa); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile("testdata/upsert-ember-otter-42.xml")
	c := f.snapshot()[0]
	if c.method != "POST" || c.path != "/2013-04-01/hostedzone/Z0123456789ABCDEFGHIJ/rrset" || !bytes.Equal(c.body, want) {
		t.Errorf("upsert call: %s %s\n%s", c.method, c.path, c.body)
	}
	if !strings.HasPrefix(c.auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260927/us-east-1/route53/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=") {
		t.Errorf("Authorization %s", c.auth)
	}

	// Delete lists the name's sets and deletes exactly those, leaving a
	// longer name that sorts next to it alone.
	f.mu.Lock()
	f.list = `<?xml version="1.0"?>
<ListResourceRecordSetsResponse xmlns="https://route53.amazonaws.com/doc/2013-04-01/"><ResourceRecordSets>
<ResourceRecordSet><Name>ember-otter-42.mirrin.link.</Name><Type>A</Type><TTL>300</TTL><ResourceRecords><ResourceRecord><Value>192.0.2.1</Value></ResourceRecord></ResourceRecords></ResourceRecordSet>
<ResourceRecordSet><Name>ember-otter-42.mirrin.link.</Name><Type>CAA</Type><TTL>300</TTL><ResourceRecords><ResourceRecord><Value>0 issuewild ";"</Value></ResourceRecord></ResourceRecords></ResourceRecordSet>
<ResourceRecordSet><Name>ember-otter-420.mirrin.link.</Name><Type>A</Type><TTL>300</TTL><ResourceRecords><ResourceRecord><Value>192.0.2.9</Value></ResourceRecord></ResourceRecords></ResourceRecordSet>
</ResourceRecordSets><IsTruncated>false</IsTruncated><MaxItems>10</MaxItems></ListResourceRecordSetsResponse>`
	f.mu.Unlock()
	if err := r.DeleteHandle(t.Context(), "ember-otter-42"); err != nil {
		t.Fatal(err)
	}
	calls := f.snapshot()
	if len(calls) != 3 {
		t.Fatalf("%d calls", len(calls))
	}
	if l := calls[1]; l.method != "GET" || l.query != "maxitems=10&name=ember-otter-42.mirrin.link." {
		t.Errorf("list call: %s ?%s", l.method, l.query)
	}
	var del changeRequest
	if err := xml.Unmarshal(calls[2].body, &del); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ch := range del.ChangeBatch.Changes {
		got = append(got, ch.Action+" "+ch.Set.Name+" "+ch.Set.Type+" "+ch.Set.Records[0].Value)
	}
	if !slices.Equal(got, []string{"DELETE ember-otter-42.mirrin.link. A 192.0.2.1", `DELETE ember-otter-42.mirrin.link. CAA 0 issuewild ";"`}) {
		t.Errorf("delete changes: %q", got)
	}

	// Nothing listed: nothing to delete, and no second call.
	f.mu.Lock()
	f.list = `<ListResourceRecordSetsResponse><ResourceRecordSets></ResourceRecordSets></ListResourceRecordSetsResponse>`
	f.mu.Unlock()
	if err := r.DeleteHandle(t.Context(), "ember-otter-42"); err != nil || len(f.snapshot()) != 4 {
		t.Errorf("empty delete: %v, %d calls", err, len(f.snapshot()))
	}

	// A refusal comes back as an error with Route 53's code.
	bad := &Route53{Zone: "mirrin.link", HostedZoneID: "Z0123456789ABCDEFGHIJ", Creds: sigv4.Creds{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wrong"}, Endpoint: srv.URL}
	f.mu.Lock()
	f.quiet = true
	f.mu.Unlock()
	if err := bad.UpsertHandle(t.Context(), "ember-otter-42", v4, v6, caa); err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Errorf("wrong secret: %v", err)
	}
	if err := (&Route53{Zone: "mirrin.link", HostedZoneID: "Z1", Endpoint: srv.URL}).UpsertHandle(t.Context(), "x-y-z", v4, v6, caa); err == nil {
		t.Error("a call without credentials was sent")
	}
}

func TestFake(t *testing.T) {
	f := NewFake()
	caa, _ := HandleCAA("", "")
	ctx := context.Background()
	if err := f.UpsertHandle(ctx, "a-b-c", v4, v6, caa); err != nil {
		t.Fatal(err)
	}
	if r, ok := f.Records("a-b-c"); !ok || !slices.Equal(r.A, v4) || !slices.Equal(r.CAA, caa) {
		t.Errorf("%+v", r)
	}
	f.SetFail(errors.New("down"))
	if f.DeleteHandle(ctx, "a-b-c") == nil {
		t.Error("the outage did not fail")
	}
	f.SetFail(nil)
	f.DeleteHandle(ctx, "a-b-c")
	if _, ok := f.Records("a-b-c"); ok {
		t.Error("records remain")
	}
}
