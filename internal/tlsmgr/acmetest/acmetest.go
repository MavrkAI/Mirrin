// Package acmetest is an in-process ACME CA for tests (RFC 8555 with
// TLS-ALPN-01 from RFC 8737 and renewal info from RFC 9773). It checks what
// a real CA checks: JWS signatures over ES256 account keys, single-use
// nonces, the request URL in each signature, and the validation itself,
// which dials the name and verifies the acme-tls/1 certificate. Where Let's
// Encrypt validates from several network perspectives, this CA dials once
// per perspective. It never contacts anything but the Dial it is given.
package acmetest

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
)

var idPeACMEIdentifier = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 31}

// Server is the fake CA. Set its exported fields before the first request.
type Server struct {
	// URL is the base URL; the directory is URL + "/dir".
	URL string
	// Roots trusts the certificates this CA issues.
	Roots *x509.CertPool
	// Now is the CA's clock (issuance times). Nil is time.Now.
	Now func() time.Time
	// Lifetime of issued certificates. Zero is 90 days.
	Lifetime time.Duration
	// Perspectives is how many times each validation dials. Zero is 2.
	Perspectives int
	// Dial is the validator's route to <name>:443. Nil dials the name.
	Dial func(ctx context.Context, addr string) (net.Conn, error)
	// Window, if set, turns on renewalInfo and says the suggested window
	// for a certificate.
	Window func(leaf *x509.Certificate) (start, end time.Time)
	// OfferChallenges lists the challenge types offered. Nil is only
	// tls-alpn-01.
	OfferChallenges []string
	// CAA, if set, is checked for every name before a certificate is
	// signed, with the URL of the account asking, as a CA checks CAA
	// accounturi (RFC 8657). An error refuses the order with caa.
	CAA func(name, accountURL string) error

	srv   *httptest.Server
	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey

	mu          sync.Mutex
	nonces      map[string]bool
	accounts    map[string]*account // by URL
	byThumb     map[string]*account
	orders      map[string]*order
	authzs      map[string]*authz
	chals       map[string]*challenge
	certs       map[string][][]byte // by certificate URL
	issued      []*x509.Certificate
	validations []Validation
	seq         int
}

// Validation is one TLS-ALPN-01 attempt the CA made.
type Validation struct {
	Name string
	Err  error
}

type account struct {
	url string
	key *ecdsa.PublicKey
}

type order struct {
	url, acct, status string
	names             []string
	authzs            []string
	cert              string
}

type authz struct {
	url, name, status string
	chals             []string
}

type challenge struct {
	url, typ, token, status, authz string
	acct                           *account
	problem                        string
}

// New starts the CA on an httptest TLS server. Client is the HTTP client
// that trusts it; Close stops it.
func New() *Server {
	s := &Server{nonces: map[string]bool{}, accounts: map[string]*account{}, byThumb: map[string]*account{}, orders: map[string]*order{}, authzs: map[string]*authz{}, chals: map[string]*challenge{}, certs: map[string][][]byte{}}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "acmetest root"},
		NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: time.Now().Add(20 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, SubjectKeyId: []byte("acmetest-root-ski"),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	s.ca, _ = x509.ParseCertificate(der)
	s.caKey = key
	s.Roots = x509.NewCertPool()
	s.Roots.AddCert(s.ca)
	s.srv = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL
	return s
}

// Directory is the directory URL.
func (s *Server) Directory() string { return s.URL + "/dir" }

// Client talks to the CA over its test TLS certificate.
func (s *Server) Client() *http.Client { return s.srv.Client() }

// Close stops the CA.
func (s *Server) Close() { s.srv.Close() }

// Issued is every certificate issued so far, oldest first: the CA's CT log.
func (s *Server) Issued() []*x509.Certificate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.issued)
}

// Validations is every validation attempt, oldest first.
func (s *Server) Validations() []Validation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.validations)
}

// Mint issues a certificate for names and pub with no validation at all, as
// a mis-issuing or compromised CA would. It goes into Issued.
func (s *Server) Mint(pub crypto.PublicKey, names ...string) *x509.Certificate {
	c, err := s.sign(pub, names)
	if err != nil {
		panic(err)
	}
	return c
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) sign(pub crypto.PublicKey, names []string) (*x509.Certificate, error) {
	life := s.Lifetime
	if life <= 0 {
		life = 90 * 24 * time.Hour
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	nb := s.now().Add(-time.Hour) // Let's Encrypt backdates an hour
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: nb, NotAfter: nb.Add(life),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		AuthorityKeyId: s.ca.SubjectKeyId,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, s.ca, pub, s.caKey)
	if err != nil {
		return nil, err
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.issued = append(s.issued, c)
	s.mu.Unlock()
	return c, nil
}

func (s *Server) nextURL(kind string) string {
	s.seq++
	return s.URL + "/" + kind + "/" + strconv.Itoa(s.seq)
}

func (s *Server) nonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	n := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.nonces[n] = true
	s.mu.Unlock()
	return n
}

type problem struct {
	status int
	typ    string
	detail string
}

func (p problem) Error() string { return p.typ + ": " + p.detail }

func bad(typ, detail string) problem {
	return problem{400, "urn:ietf:params:acme:error:" + typ, detail}
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, loc string, v any) {
	w.Header().Set("Replay-Nonce", s.nonce())
	if loc != "" {
		w.Header().Set("Location", loc)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) writeProblem(w http.ResponseWriter, err error) {
	p, ok := err.(problem)
	if !ok {
		p = problem{500, "urn:ietf:params:acme:error:serverInternal", err.Error()}
	}
	w.Header().Set("Replay-Nonce", s.nonce())
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.status)
	json.NewEncoder(w).Encode(map[string]any{"type": p.typ, "detail": p.detail, "status": p.status})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/dir" && r.Method == http.MethodGet:
		d := map[string]any{
			"newNonce": s.URL + "/nonce", "newAccount": s.URL + "/new-acct", "newOrder": s.URL + "/new-order",
			"revokeCert": s.URL + "/revoke", "keyChange": s.URL + "/key-change",
			"meta": map[string]any{"termsOfService": s.URL + "/terms", "caaIdentities": []string{"acmetest.invalid"}},
		}
		if s.Window != nil {
			d["renewalInfo"] = s.URL + "/renewal-info"
		}
		s.writeJSON(w, 200, "", d)
		return
	case r.URL.Path == "/nonce":
		w.Header().Set("Replay-Nonce", s.nonce())
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			w.WriteHeader(200)
		} else {
			w.WriteHeader(204)
		}
		return
	case strings.HasPrefix(r.URL.Path, "/renewal-info/") && r.Method == http.MethodGet:
		s.renewalInfo(w, strings.TrimPrefix(r.URL.Path, "/renewal-info/"))
		return
	case r.Method != http.MethodPost:
		s.writeProblem(w, problem{405, "urn:ietf:params:acme:error:malformed", "POST only"})
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/jose+json" {
		s.writeProblem(w, bad("malformed", "content type "+ct))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		s.writeProblem(w, bad("malformed", err.Error()))
		return
	}
	url := s.URL + r.URL.Path
	req, err := s.verify(body, url, r.URL.Path == "/new-acct")
	if err != nil {
		s.writeProblem(w, err)
		return
	}
	switch path := r.URL.Path; {
	case path == "/new-acct":
		s.newAccount(w, req)
	case strings.HasPrefix(path, "/acct/"):
		s.writeJSON(w, 200, "", map[string]any{"status": "valid"})
	case path == "/new-order":
		s.newOrder(w, req)
	case strings.HasPrefix(path, "/order/"):
		s.getOrder(w, req, url)
	case strings.HasPrefix(path, "/authz/"):
		s.getAuthz(w, url)
	case strings.HasPrefix(path, "/chal/"):
		s.respond(w, r.Context(), req, url)
	case strings.HasPrefix(path, "/finalize/"):
		s.finalize(w, req, strings.Replace(url, "/finalize/", "/order/", 1))
	case strings.HasPrefix(path, "/cert/"):
		s.getCert(w, url)
	default:
		s.writeProblem(w, problem{404, "urn:ietf:params:acme:error:malformed", "no such resource"})
	}
}

type request struct {
	acct    *account
	jwk     *ecdsa.PublicKey
	payload []byte
}

// verify checks a flattened JWS: ES256 only, a nonce this CA issued and
// has not seen back, the URL it was sent to, and either a jwk (new-acct) or
// the kid of a known account.
func (s *Server) verify(body []byte, url string, jwkAllowed bool) (*request, error) {
	var msg struct{ Protected, Payload, Signature string }
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, bad("malformed", "not a JWS")
	}
	ph, err := base64.RawURLEncoding.DecodeString(msg.Protected)
	if err != nil {
		return nil, bad("malformed", "protected header")
	}
	var hdr struct {
		Alg, Nonce, URL, Kid string
		JWK                  *struct{ Kty, Crv, X, Y string }
	}
	if err := json.Unmarshal(ph, &hdr); err != nil {
		return nil, bad("malformed", "protected header")
	}
	if hdr.Alg != "ES256" {
		return nil, bad("badSignatureAlgorithm", hdr.Alg)
	}
	s.mu.Lock()
	fresh := s.nonces[hdr.Nonce]
	delete(s.nonces, hdr.Nonce)
	s.mu.Unlock()
	if !fresh {
		return nil, bad("badNonce", "unknown or reused nonce")
	}
	if hdr.URL != url {
		return nil, problem{401, "urn:ietf:params:acme:error:unauthorized", "url " + hdr.URL + " != " + url}
	}
	req := &request{}
	var pub *ecdsa.PublicKey
	switch {
	case hdr.JWK != nil && hdr.Kid == "" && jwkAllowed:
		if hdr.JWK.Kty != "EC" || hdr.JWK.Crv != "P-256" {
			return nil, bad("badPublicKey", "EC P-256 only")
		}
		x, e1 := base64.RawURLEncoding.DecodeString(hdr.JWK.X)
		y, e2 := base64.RawURLEncoding.DecodeString(hdr.JWK.Y)
		if e1 != nil || e2 != nil {
			return nil, bad("badPublicKey", "coordinates")
		}
		raw := append([]byte{4}, append(pad32(x), pad32(y)...)...)
		k, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
		if err != nil {
			return nil, bad("badPublicKey", err.Error())
		}
		pub, req.jwk = k, k
	case hdr.Kid != "" && hdr.JWK == nil:
		s.mu.Lock()
		a := s.accounts[hdr.Kid]
		s.mu.Unlock()
		if a == nil {
			return nil, problem{400, "urn:ietf:params:acme:error:accountDoesNotExist", "no account " + hdr.Kid}
		}
		pub, req.acct = a.key, a
	default:
		return nil, bad("malformed", "need exactly one of jwk and kid")
	}
	sig, err := base64.RawURLEncoding.DecodeString(msg.Signature)
	if err != nil || len(sig) != 64 {
		return nil, bad("malformed", "signature")
	}
	sum := sha256.Sum256([]byte(msg.Protected + "." + msg.Payload))
	if !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, problem{403, "urn:ietf:params:acme:error:unauthorized", "bad signature"}
	}
	if req.payload, err = base64.RawURLEncoding.DecodeString(msg.Payload); err != nil {
		return nil, bad("malformed", "payload")
	}
	return req, nil
}

func pad32(b []byte) []byte {
	if len(b) >= 32 {
		return b
	}
	return append(make([]byte, 32-len(b)), b...)
}

func (s *Server) newAccount(w http.ResponseWriter, req *request) {
	var p struct {
		TermsOfServiceAgreed bool `json:"termsOfServiceAgreed"`
		OnlyReturnExisting   bool `json:"onlyReturnExisting"`
	}
	json.Unmarshal(req.payload, &p)
	thumb, err := acme.JWKThumbprint(req.jwk)
	if err != nil {
		s.writeProblem(w, err)
		return
	}
	s.mu.Lock()
	a := s.byThumb[thumb]
	s.mu.Unlock()
	if a != nil {
		s.writeJSON(w, 200, a.url, map[string]any{"status": "valid"})
		return
	}
	if p.OnlyReturnExisting {
		s.writeProblem(w, bad("accountDoesNotExist", "no account for this key"))
		return
	}
	if !p.TermsOfServiceAgreed {
		s.writeProblem(w, bad("malformed", "terms of service not agreed"))
		return
	}
	s.mu.Lock()
	a = &account{url: s.nextURL("acct"), key: req.jwk}
	s.accounts[a.url], s.byThumb[thumb] = a, a
	s.mu.Unlock()
	s.writeJSON(w, 201, a.url, map[string]any{"status": "valid"})
}

func (s *Server) newOrder(w http.ResponseWriter, req *request) {
	var p struct {
		Identifiers []struct{ Type, Value string }
	}
	if err := json.Unmarshal(req.payload, &p); err != nil || len(p.Identifiers) == 0 {
		s.writeProblem(w, bad("malformed", "identifiers"))
		return
	}
	offered := s.OfferChallenges
	if offered == nil {
		offered = []string{"tls-alpn-01"}
	}
	s.mu.Lock()
	o := &order{url: s.nextURL("order"), acct: req.acct.url, status: "pending"}
	for _, id := range p.Identifiers {
		if id.Type != "dns" || strings.HasPrefix(id.Value, "*.") {
			s.mu.Unlock()
			s.writeProblem(w, bad("rejectedIdentifier", id.Value))
			return
		}
		z := &authz{url: s.nextURL("authz"), name: strings.ToLower(id.Value), status: "pending"}
		for _, typ := range offered {
			tok := make([]byte, 32)
			rand.Read(tok)
			c := &challenge{url: s.nextURL("chal"), typ: typ, token: base64.RawURLEncoding.EncodeToString(tok), status: "pending", authz: z.url}
			s.chals[c.url] = c
			z.chals = append(z.chals, c.url)
		}
		s.authzs[z.url] = z
		o.names = append(o.names, z.name)
		o.authzs = append(o.authzs, z.url)
	}
	s.orders[o.url] = o
	v := s.orderJSON(o)
	s.mu.Unlock()
	s.writeJSON(w, 201, o.url, v)
}

// orderJSON also moves a pending order to ready once every authorization
// is valid. Callers hold mu.
func (s *Server) orderJSON(o *order) map[string]any {
	if o.status == "pending" {
		ready := true
		for _, u := range o.authzs {
			switch s.authzs[u].status {
			case "valid":
			case "invalid":
				o.status = "invalid"
				ready = false
			default:
				ready = false
			}
		}
		if ready {
			o.status = "ready"
		}
	}
	ids := []map[string]string{}
	for _, n := range o.names {
		ids = append(ids, map[string]string{"type": "dns", "value": n})
	}
	v := map[string]any{"status": o.status, "identifiers": ids, "authorizations": o.authzs, "finalize": strings.Replace(o.url, "/order/", "/finalize/", 1), "expires": s.now().Add(7 * 24 * time.Hour)}
	if o.cert != "" && o.status == "valid" {
		v["certificate"] = o.cert
	}
	return v
}

func (s *Server) getOrder(w http.ResponseWriter, req *request, url string) {
	s.mu.Lock()
	o := s.orders[url]
	if o == nil || o.acct != req.acct.url {
		s.mu.Unlock()
		s.writeProblem(w, problem{404, "urn:ietf:params:acme:error:malformed", "no such order"})
		return
	}
	if o.status == "processing" {
		o.status = "valid" // issued in finalize; seen by the first poll
	}
	v := s.orderJSON(o)
	s.mu.Unlock()
	// Like Pebble, no Location here: RFC 8555 doesn't require one, and a
	// client that reads the order's URL from it loses it.
	s.writeJSON(w, 200, "", v)
}

func (s *Server) authzJSON(z *authz) map[string]any {
	var cs []map[string]any
	for _, u := range z.chals {
		c := s.chals[u]
		m := map[string]any{"type": c.typ, "url": c.url, "token": c.token, "status": c.status}
		if c.problem != "" {
			m["error"] = map[string]any{"type": "urn:ietf:params:acme:error:unauthorized", "detail": c.problem}
		}
		cs = append(cs, m)
	}
	return map[string]any{"status": z.status, "identifier": map[string]string{"type": "dns", "value": z.name}, "challenges": cs, "expires": s.now().Add(7 * 24 * time.Hour)}
}

func (s *Server) getAuthz(w http.ResponseWriter, url string) {
	s.mu.Lock()
	z := s.authzs[url]
	if z == nil {
		s.mu.Unlock()
		s.writeProblem(w, problem{404, "urn:ietf:params:acme:error:malformed", "no such authorization"})
		return
	}
	v := s.authzJSON(z)
	s.mu.Unlock()
	s.writeJSON(w, 200, "", v)
}

// respond validates a challenge the client says is ready. The validation
// finishes before the response, so the client's first poll sees the result.
func (s *Server) respond(w http.ResponseWriter, ctx context.Context, req *request, url string) {
	s.mu.Lock()
	c := s.chals[url]
	if c == nil {
		s.mu.Unlock()
		s.writeProblem(w, problem{404, "urn:ietf:params:acme:error:malformed", "no such challenge"})
		return
	}
	z := s.authzs[c.authz]
	start := c.status == "pending" && string(req.payload) == "{}"
	if start {
		c.status, c.acct = "processing", req.acct
	}
	s.mu.Unlock()
	if start {
		err := s.validate(ctx, c, z.name)
		s.mu.Lock()
		s.validations = append(s.validations, Validation{Name: z.name, Err: err})
		if err != nil {
			c.status, c.problem, z.status = "invalid", err.Error(), "invalid"
		} else {
			c.status, z.status = "valid", "valid"
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	v := map[string]any{"type": c.typ, "url": c.url, "token": c.token, "status": c.status}
	s.mu.Unlock()
	s.writeJSON(w, 200, "", v)
}

func (s *Server) validate(ctx context.Context, c *challenge, name string) error {
	if c.typ != "tls-alpn-01" {
		return fmt.Errorf("challenge type %s isn't validated here", c.typ)
	}
	thumb, err := acme.JWKThumbprint(c.acct.key)
	if err != nil {
		return err
	}
	want := sha256.Sum256([]byte(c.token + "." + thumb))
	n := s.Perspectives
	if n <= 0 {
		n = 2
	}
	for i := 0; i < n; i++ {
		if err := s.dialValidate(ctx, name, want[:]); err != nil {
			return fmt.Errorf("perspective %d: %w", i+1, err)
		}
	}
	return nil
}

func (s *Server) dialValidate(ctx context.Context, name string, want []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addr := net.JoinHostPort(name, "443")
	var conn net.Conn
	var err error
	if s.Dial != nil {
		conn, err = s.Dial(ctx, addr)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	tc := tls.Client(conn, &tls.Config{ServerName: name, NextProtos: []string{acme.ALPNProto}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err := tc.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	cs := tc.ConnectionState()
	if cs.NegotiatedProtocol != acme.ALPNProto {
		return fmt.Errorf("negotiated %q, not acme-tls/1", cs.NegotiatedProtocol)
	}
	leaf := cs.PeerCertificates[0]
	if len(leaf.DNSNames) != 1 || !strings.EqualFold(leaf.DNSNames[0], name) || len(leaf.IPAddresses)+len(leaf.EmailAddresses)+len(leaf.URIs) > 0 {
		return fmt.Errorf("the certificate names %v, not only %s", leaf.DNSNames, name)
	}
	for _, e := range leaf.Extensions {
		if !e.Id.Equal(idPeACMEIdentifier) {
			continue
		}
		if !e.Critical {
			return errors.New("the acmeIdentifier extension isn't critical")
		}
		var got []byte
		if rest, err := asn1.Unmarshal(e.Value, &got); err != nil || len(rest) > 0 {
			return errors.New("the acmeIdentifier extension is malformed")
		}
		if string(got) != string(want) {
			return errors.New("the key authorization doesn't match")
		}
		return nil
	}
	return errors.New("no acmeIdentifier extension")
}

func (s *Server) finalize(w http.ResponseWriter, req *request, orderURL string) {
	var p struct{ CSR string }
	json.Unmarshal(req.payload, &p)
	der, err := base64.RawURLEncoding.DecodeString(p.CSR)
	if err != nil {
		s.writeProblem(w, bad("badCSR", "encoding"))
		return
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil || csr.CheckSignature() != nil {
		s.writeProblem(w, bad("badCSR", "signature"))
		return
	}
	s.mu.Lock()
	o := s.orders[orderURL]
	if o == nil || o.acct != req.acct.url {
		s.mu.Unlock()
		s.writeProblem(w, problem{404, "urn:ietf:params:acme:error:malformed", "no such order"})
		return
	}
	s.orderJSON(o)
	if o.status != "ready" {
		st := o.status
		s.mu.Unlock()
		s.writeProblem(w, problem{403, "urn:ietf:params:acme:error:orderNotReady", "order is " + st})
		return
	}
	names := slices.Clone(o.names)
	s.mu.Unlock()
	got := slices.Clone(csr.DNSNames)
	slices.Sort(got)
	slices.Sort(names)
	if !slices.Equal(got, names) {
		s.writeProblem(w, bad("badCSR", fmt.Sprintf("names %v, order %v", got, names)))
		return
	}
	if s.CAA != nil {
		for _, n := range names {
			if err := s.CAA(n, req.acct.url); err != nil {
				s.writeProblem(w, bad("caa", n+": "+err.Error()))
				return
			}
		}
	}
	c, err := s.sign(csr.PublicKey, csr.DNSNames)
	if err != nil {
		s.writeProblem(w, err)
		return
	}
	// Finalization is asynchronous, as Pebble's is: the answer says
	// "processing", without a Location, and the order turns valid when the
	// client next polls it by the URL newOrder gave.
	s.mu.Lock()
	o.status = "processing"
	o.cert = s.nextURL("cert")
	s.certs[o.cert] = [][]byte{c.Raw, s.ca.Raw}
	v := s.orderJSON(o)
	s.mu.Unlock()
	s.writeJSON(w, 200, "", v)
}

func (s *Server) getCert(w http.ResponseWriter, url string) {
	s.mu.Lock()
	chain := s.certs[url]
	s.mu.Unlock()
	if chain == nil {
		s.writeProblem(w, problem{404, "urn:ietf:params:acme:error:malformed", "no such certificate"})
		return
	}
	w.Header().Set("Replay-Nonce", s.nonce())
	w.Header().Set("Content-Type", "application/pem-certificate-chain")
	for _, der := range chain {
		pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
}

func (s *Server) renewalInfo(w http.ResponseWriter, id string) {
	if s.Window == nil {
		http.NotFound(w, nil)
		return
	}
	for _, c := range s.Issued() {
		serial := c.SerialNumber.Bytes()
		if len(serial) == 0 || serial[0]&0x80 != 0 {
			serial = append([]byte{0}, serial...)
		}
		if base64.RawURLEncoding.EncodeToString(c.AuthorityKeyId)+"."+base64.RawURLEncoding.EncodeToString(serial) != id {
			continue
		}
		start, end := s.Window(c)
		w.Header().Set("Retry-After", "21600")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"suggestedWindow": map[string]time.Time{"start": start, "end": end}})
		return
	}
	w.WriteHeader(http.StatusNotFound)
}
