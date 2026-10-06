package certwatch

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/MavrkAI/Mirrin/internal/health"
)

const host = "ember-otter-42.mirrin.test"

var checkpoint = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

// keys stands in for this machine's key history and a rogue key.
type keys struct {
	byRole map[string]*ecdsa.PrivateKey
}

func newKeys(t *testing.T) *keys {
	k := &keys{byRole: map[string]*ecdsa.PrivateKey{}}
	for _, r := range []string{"CURRENT", "NEXT", "HISTORY", "OLD", "ROGUE"} {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		k.byRole[r] = key
	}
	return k
}

func spkiSum(k *ecdsa.PrivateKey) []byte {
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	s := sha256.Sum256(der)
	return s[:]
}

func (k *keys) pin(role string) string {
	return base64.RawURLEncoding.EncodeToString(spkiSum(k.byRole[role]))
}

func (k *keys) known() ([]string, time.Time) {
	return []string{k.pin("CURRENT"), k.pin("NEXT"), k.pin("HISTORY")}, checkpoint
}

func (k *keys) cert(t *testing.T, role string, names ...string) []byte {
	ca, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	issuer := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture CA"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuer, &k.byRole[role].PublicKey, ca)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// ctFakes serves Cert Spotter and crt.sh from testdata.
type ctFakes struct {
	spotter, crtsh *httptest.Server
	down           [2]atomic.Bool
	downloads      atomic.Int32
	rogue          atomic.Bool // include the rogue wildcard
	queries        sync.Map
}

func newCTFakes(t *testing.T, k *keys) *ctFakes {
	f := &ctFakes{}
	f.rogue.Store(true)
	spotter, err := os.ReadFile("testdata/certspotter.json")
	if err != nil {
		t.Fatal(err)
	}
	for role := range k.byRole {
		spotter = bytes.ReplaceAll(spotter, []byte("{{"+role+"}}"), []byte(hex.EncodeToString(spkiSum(k.byRole[role]))))
	}
	f.spotter = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if f.down[0].Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != "/v1/issuances" || q.Get("domain") != host || q.Get("match_wildcards") != "true" || !slices.Contains(q["expand"], "dns_names") {
			t.Errorf("Cert Spotter query %s", r.URL)
		}
		if q.Get("after") != "" {
			io.WriteString(w, "[]")
			return
		}
		body := spotter
		if !f.rogue.Load() {
			body = bytes.Replace(body, []byte(`"*.mirrin.test"`), []byte(`"*.elsewhere.test"`), 1)
		}
		w.Write(body)
	}))
	t.Cleanup(f.spotter.Close)
	certs := map[string][]byte{
		"1001": k.cert(t, "CURRENT", host), "1002": k.cert(t, "NEXT", host), "1003": k.cert(t, "HISTORY", host),
		"1004": k.cert(t, "OLD", host), "1005": k.cert(t, "ROGUE", "*.mirrin.test"), "1006": k.cert(t, "ROGUE", "other.mirrin.test"),
	}
	f.crtsh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if f.down[1].Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		if id := q.Get("d"); id != "" {
			f.downloads.Add(1)
			w.Write(certs[id])
			return
		}
		f.queries.Store(q.Get("q"), true)
		if q.Get("output") != "json" {
			t.Errorf("crt.sh query %s", r.URL)
		}
		switch q.Get("q") {
		case host:
			http.ServeFile(w, r, "testdata/crtsh.json")
		case "*.mirrin.test":
			if f.rogue.Load() {
				http.ServeFile(w, r, "testdata/crtsh-wildcard.json")
			} else {
				io.WriteString(w, "[]")
			}
		default:
			t.Errorf("crt.sh asked for %q", q.Get("q"))
			io.WriteString(w, "[]")
		}
	}))
	t.Cleanup(f.crtsh.Close)
	return f
}

func (f *ctFakes) sources() []CTSource {
	return []CTSource{
		&CertSpotter{BaseURL: f.spotter.URL, Client: f.spotter.Client()},
		&CrtSh{BaseURL: f.crtsh.URL, Client: f.crtsh.Client(), NotBefore: func() time.Time { return checkpoint }},
	}
}

func watcher(k *keys, f *ctFakes, got *[]Finding) *Watcher {
	return &Watcher{
		Hosts:     func() []string { return []string{host} },
		Known:     k.known,
		Sources:   f.sources(),
		OnFinding: func(x Finding) { *got = append(*got, x) },
		Now:       func() time.Time { return time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC) },
	}
}

func critical(fs []Finding) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Severity == Critical {
			out = append(out, f)
		}
	}
	return out
}

func TestUnknownWildcardIsExactlyOneCriticalFinding(t *testing.T) {
	k := newKeys(t)
	f := newCTFakes(t, k)
	var got []Finding
	w := watcher(k, f, &got)
	fresh := w.Check(context.Background())
	crit := critical(fresh)
	if len(crit) != 1 || len(critical(got)) != 1 {
		t.Fatalf("critical findings %+v", crit)
	}
	c := crit[0]
	if c.Kind != UnknownIssuance || c.Issuance.SPKI != k.pin("ROGUE") || c.Host != host || !c.NotBefore.Equal(time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("finding %+v", c)
	}
	if !strings.Contains(c.Detail, "never had") {
		t.Fatalf("detail %q", c.Detail)
	}
	if _, ok := f.queries.Load("*.mirrin.test"); !ok {
		t.Fatal("crt.sh wasn't asked for the wildcard")
	}
	// The same rogue certificate on the next poll is not news.
	if again := w.Check(context.Background()); len(again) != 0 {
		t.Fatalf("reported twice: %+v", again)
	}
	if st, _, _ := w.Health(context.Background()); st != health.Fail {
		t.Fatalf("health %v with a rogue certificate", st)
	}
	// Certificates are downloaded once each, and never the old ones.
	n := f.downloads.Load()
	if n != 4 { // current, next, history, rogue wildcard
		t.Fatalf("%d downloads", n)
	}
}

func TestKnownKeysAndOldIssuancesAreQuiet(t *testing.T) {
	k := newKeys(t)
	f := newCTFakes(t, k)
	f.rogue.Store(false)
	var got []Finding
	w := watcher(k, f, &got)
	if fresh := w.Check(context.Background()); len(fresh) != 0 || len(got) != 0 {
		t.Fatalf("findings %+v", fresh)
	}
	if st, detail, _ := w.Health(context.Background()); st != health.OK {
		t.Fatalf("health %v %s", st, detail)
	}
	// The same entries, but the old certificate is after the checkpoint
	// (no restore to explain it): now it is an alarm.
	w.Known = func() ([]string, time.Time) {
		ks, _ := k.known()
		return ks, time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	}
	w.Sources = f.sources()[:1]
	if crit := critical(w.Check(context.Background())); len(crit) != 1 || crit[0].Issuance.SPKI != k.pin("OLD") {
		t.Fatalf("old key after the checkpoint: %+v", crit)
	}
}

func TestOneSourceDownWarnsBothDownNeverPass(t *testing.T) {
	k := newKeys(t)
	f := newCTFakes(t, k)
	f.rogue.Store(false)
	var got []Finding
	w := watcher(k, f, &got)
	f.down[0].Store(true)
	fresh := w.Check(context.Background())
	if len(fresh) != 1 || fresh[0].Kind != SourceDown || fresh[0].Severity != Warning || !strings.Contains(fresh[0].Detail, "Cert Spotter") {
		t.Fatalf("one down: %+v", fresh)
	}
	if st, detail, _ := w.Health(context.Background()); st != health.OK || !strings.Contains(detail, "Cert Spotter didn't answer") {
		t.Fatalf("one down health %v %q", st, detail)
	}
	f.down[1].Store(true)
	w.Check(context.Background())
	if st, _, _ := w.Health(context.Background()); st != health.Warn {
		t.Fatalf("both down health %v", st)
	}
	// Even with a rogue certificate somewhere, silence is never a pass.
	f.rogue.Store(true)
	w.Check(context.Background())
	if st, _, _ := w.Health(context.Background()); st == health.OK {
		t.Fatal("passed with no source")
	}
	if len(critical(got)) != 0 {
		t.Fatal("invented a finding")
	}
}

func TestHealthBeforeFirstPollWarns(t *testing.T) {
	w := &Watcher{Sources: []CTSource{&CrtSh{}}}
	if st, _, _ := w.Health(context.Background()); st != health.Warn {
		t.Fatal(st)
	}
}

func TestCovers(t *testing.T) {
	for _, c := range []struct {
		names []string
		want  bool
	}{
		{[]string{host}, true},
		{[]string{"*.mirrin.test"}, true},
		{[]string{"EMBER-OTTER-42.mirrin.test."}, true},
		{[]string{"*.ember-otter-42.mirrin.test"}, false},
		{[]string{"*.test"}, false},
		{[]string{"other.mirrin.test"}, false},
	} {
		if Covers(c.names, host) != c.want {
			t.Fatalf("%v", c.names)
		}
	}
	if wildcardFor("example.com") != "" {
		t.Fatal("*.com")
	}
}

// fakeDoH answers CAA queries (RFC 8484 POST) from testdata/caa.txt.
func fakeDoH(t *testing.T, fail *atomic.Bool) *httptest.Server {
	t.Helper()
	zone := map[string][]CAA{}
	fh, err := os.Open("testdata/caa.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, " ", 4)
		flags, _ := strconv.Atoi(parts[1])
		v, err := strconv.Unquote(parts[3])
		if err != nil {
			t.Fatal(line, err)
		}
		zone[parts[0]] = append(zone[parts[0]], CAA{Flags: uint8(flags), Tag: parts[2], Value: v})
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail != nil && fail.Load() {
			http.Error(w, "nope", 500)
			return
		}
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/dns-message" {
			t.Errorf("DoH request %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		var p dnsmessage.Parser
		h, err := p.Start(body)
		if err != nil {
			t.Error(err)
			return
		}
		q, err := p.Question()
		if err != nil || q.Type != typeCAA {
			t.Errorf("question %+v %v", q, err)
			return
		}
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, RecursionDesired: true, RecursionAvailable: true})
		b.StartQuestions()
		b.Question(q)
		b.StartAnswers()
		for _, c := range zone[q.Name.String()] {
			b.UnknownResource(dnsmessage.ResourceHeader{Name: q.Name, Type: typeCAA, Class: dnsmessage.ClassINET, TTL: 300}, dnsmessage.UnknownResource{Type: typeCAA, Data: CAAData(c)})
		}
		msg, _ := b.Finish()
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(msg)
	}))
}

func TestLookupCAAParsesFixtures(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	bad := fakeDoH(t, &down)
	defer bad.Close()
	good := fakeDoH(t, nil)
	defer good.Close()
	r := &Resolver{DoH: []string{bad.URL, good.URL}}
	recs, err := r.Lookup(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("%+v", recs)
	}
	byTag := map[string]CAA{}
	for _, c := range recs {
		byTag[c.Tag] = c
		if c.Name != host {
			t.Fatalf("owner %q", c.Name)
		}
	}
	issue := byTag["issue"]
	if issue.Issuer() != "letsencrypt.org" || issue.AccountURI() != "https://acme-v02.api.letsencrypt.org/acme/acct/123456789" || !slices.Equal(issue.ValidationMethods(), []string{"tls-alpn-01"}) {
		t.Fatalf("issue %+v %v", issue, issue.Params())
	}
	if byTag["issuewild"].Issuer() != "" || byTag["iodef"].Value != "mailto:security@mirrin.test" {
		t.Fatalf("issuewild/iodef %+v", byTag)
	}
	if v := CheckCAA(recs, issue.AccountURI()); !v.OK {
		t.Fatalf("our own records: %+v", v)
	}
	if v := CheckCAA(recs, "https://acme-v02.api.letsencrypt.org/acme/acct/1"); v.OK || !v.Critical {
		t.Fatalf("other account: %+v", v)
	}
	// A name with no records of its own inherits the apex's.
	recs, err = r.Lookup(context.Background(), "quiet-fox-7.mirrin.test")
	if err != nil || len(recs) != 2 || recs[0].Name != "mirrin.test" || !CheckCAA(recs, "x").OK {
		t.Fatalf("climb: %+v %v", recs, err)
	}
	hijacked, err := r.Lookup(context.Background(), "hijacked.mirrin.test")
	if err != nil {
		t.Fatal(err)
	}
	if v := CheckCAA(hijacked, issue.AccountURI()); !v.Critical {
		t.Fatalf("hijacked: %+v", v)
	}
	if !hijacked[1].Critical() {
		t.Fatal("issuer-critical flag")
	}
	if v := CheckCAA(nil, "x"); !v.Missing || v.Critical || v.OK {
		t.Fatalf("missing: %+v", v)
	}
	if v := CheckCAA([]CAA{{Tag: "issue", Value: "letsencrypt.org; accounturi=x; validationmethods=http-01"}}, "x"); v.OK || v.Critical {
		t.Fatalf("method change: %+v", v)
	}
	// Every resolver down is an error, never "no records".
	if _, err := (&Resolver{DoH: []string{bad.URL}}).Lookup(context.Background(), host); err == nil {
		t.Fatal("all resolvers down looked like no CAA")
	}
}

func TestWatcherRaisesCAAMismatch(t *testing.T) {
	doh := fakeDoH(t, nil)
	defer doh.Close()
	r := &Resolver{DoH: []string{doh.URL}}
	var got []Finding
	name := host
	w := &Watcher{
		Hosts:      func() []string { return []string{name} },
		Known:      func() ([]string, time.Time) { return nil, checkpoint },
		AccountURI: func() string { return "https://acme-v02.api.letsencrypt.org/acme/acct/123456789" },
		CAA:        r.Lookup,
		OnFinding:  func(f Finding) { got = append(got, f) },
		Now:        func() time.Time { return time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC) },
	}
	if fresh := w.Check(context.Background()); len(fresh) != 0 {
		t.Fatalf("our records: %+v", fresh)
	}
	name = "hijacked.mirrin.test"
	fresh := w.Check(context.Background())
	if len(fresh) != 1 || fresh[0].Kind != CAAMismatch || fresh[0].Severity != Critical || !strings.Contains(fresh[0].Detail, "another ACME account") {
		t.Fatalf("mismatch: %+v", fresh)
	}
	if !fresh[0].NotBefore.Equal(checkpoint) {
		t.Fatalf("window starts %v", fresh[0].NotBefore)
	}
	if again := w.Check(context.Background()); len(again) != 0 {
		t.Fatal("reported twice")
	}
}

func TestPokeTriggersPoll(t *testing.T) {
	var polls atomic.Int32
	w := &Watcher{Hosts: func() []string { polls.Add(1); return nil }, Every: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for polls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	w.Poke()
	for polls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if polls.Load() < 2 {
		t.Fatal("Poke didn't poll")
	}
}

// Regression (review): after the owner cleared the alarm, health stayed
// Fail for as long as the rogue certificate was in CT (90 days or more).
func TestAcknowledgedFindingWarnsInsteadOfFailing(t *testing.T) {
	k := newKeys(t)
	f := newCTFakes(t, k)
	var got []Finding
	w := watcher(k, f, &got)
	cleared := map[string]bool{}
	w.Acknowledged = func(key string) bool { return cleared[key] }
	crit := critical(w.Check(context.Background()))
	if len(crit) != 1 {
		t.Fatalf("critical %+v", crit)
	}
	if st, _, _ := w.Health(context.Background()); st != health.Fail {
		t.Fatalf("health %v before clearing", st)
	}
	cleared[crit[0].Key()] = true
	w.Check(context.Background())
	if st, detail, _ := w.Health(context.Background()); st != health.Warn || !strings.Contains(detail, "dealt with") {
		t.Fatalf("health after clearing: %v %q", st, detail)
	}
}

// Regression (review): the time CAA was last seen correct was kept only in
// memory, so after a restart a mismatch's window started at the checkpoint,
// months back, and the playbook signed out every device used since.
func TestCAAWindowSurvivesRestart(t *testing.T) {
	acct := "https://acme-v02.api.letsencrypt.org/acme/acct/123456789"
	good := []CAA{{Name: host, Tag: "issue", Value: "letsencrypt.org; accounturi=" + acct}}
	bad := []CAA{{Name: host, Tag: "issue", Value: "letsencrypt.org; accounturi=https://acme-v02.api.letsencrypt.org/acme/acct/666"}}
	recs := good
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	state := filepath.Join(t.TempDir(), "certwatch.json")
	mk := func() *Watcher {
		return &Watcher{
			Hosts:      func() []string { return []string{host} },
			Known:      func() ([]string, time.Time) { return nil, checkpoint },
			AccountURI: func() string { return acct },
			CAA:        func(context.Context, string) ([]CAA, error) { return recs, nil },
			Now:        func() time.Time { return now },
			StateFile:  state,
		}
	}
	if fresh := mk().Check(context.Background()); len(fresh) != 0 {
		t.Fatalf("our records: %+v", fresh)
	}
	seen := now
	now, recs = now.Add(6*time.Hour), bad
	fresh := mk().Check(context.Background()) // a new watcher: the daemon restarted
	if len(fresh) != 1 || fresh[0].Kind != CAAMismatch || !fresh[0].NotBefore.Equal(seen) {
		t.Fatalf("after restart: %+v", fresh)
	}
}

// Regression (review): with two accounturi parameters the last one won, so
// a record naming another account first and ours last passed.
func TestCAADuplicateAccountURIIsAMismatch(t *testing.T) {
	acct := "https://acme-v02.api.letsencrypt.org/acme/acct/123456789"
	v := CheckCAA([]CAA{{Name: host, Tag: "issue", Value: "letsencrypt.org; accounturi=https://acme-v02.api.letsencrypt.org/acme/acct/666; accounturi=" + acct}}, acct)
	if v.OK || !v.Critical {
		t.Fatalf("%+v", v)
	}
}
