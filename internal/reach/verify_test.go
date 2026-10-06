package reach

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/certwatch"
	"github.com/MavrkAI/Mirrin/internal/relay"
	"github.com/MavrkAI/Mirrin/internal/relay/server"
	"github.com/MavrkAI/Mirrin/internal/relay/wire"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr/acmetest"
)

// impostor is a TLS server holding the tenant name under another key,
// tunnelled through the relay: what a relay that routes the name elsewhere
// would show.
func startImpostor(t *testing.T, rl *testRelay, key ed25519.PrivateKey, cert tls.Certificate) {
	t.Helper()
	status := make([]byte, wire.StatusKeySize)
	l, err := relay.Listen(context.Background(), relay.ClientConfig{Relays: []relay.RelayRef{{URL: rl.tunnelURL()}}, Key: key, StatusKey: status, RootCAs: rl.roots})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{cert}}))
	t.Cleanup(func() { srv.Close(); l.Close(); <-l.Done() })
	waitFor(t, 10*time.Second, "the impostor's tunnel", func() bool { return l.Status()[0].Online })
}

func TestVerifyFailsWhenRelayRoutesElsewhere(t *testing.T) {
	_, impKey, _ := ed25519.GenerateKey(rand.Reader)
	rl := startTestRelay(t, server.Allow{Hostname: tenant, Key: pubKey(impKey)})
	ca := acmetest.New()
	defer ca.Close()
	tlsKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := ca.Mint(&tlsKey.PublicKey, tenant) // a valid certificate, just not ours
	startImpostor(t, rl, impKey, tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: tlsKey, Leaf: leaf})

	ours := []string{"b3VyLWN1cnJlbnQta2V5LXBpbi1vdXItY3VycmVudC1rZXk", "b3VyLW5leHQta2V5LXBpbi1vdXItbmV4dC1rZXktcGluLXg"}
	e := &Endpoint{Hostname: tenant, Pins: func() []string { return ours }, Roots: ca.Roots, Dial: rl.dial,
		CAA: func(context.Context, string) ([]certwatch.CAA, error) { return nil, nil }}
	var out bytes.Buffer
	err := VerifyCommand(context.Background(), e, &out, false)
	if err == nil {
		t.Fatalf("verify passed against an impostor:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "FAIL  certificate") || !strings.Contains(out.String(), "isn't this computer's") {
		t.Fatalf("output:\n%s", out.String())
	}
	out.Reset()
	if err := VerifyCommand(context.Background(), e, &out, true); err == nil {
		t.Fatal("JSON verify passed")
	}
	var rep Report
	if json.Unmarshal(out.Bytes(), &rep) != nil || rep.OK || rep.Checks[0].Name != "certificate" || rep.Checks[0].OK {
		t.Fatalf("json %s", out.String())
	}
	// Control: with the impostor's pin it would pass, so the key is what
	// failed.
	sum := func() []string { return []string{rep.Served} }
	e.Pins = sum
	out.Reset()
	if err := VerifyCommand(context.Background(), e, &out, false); err != nil {
		t.Fatalf("control failed: %v\n%s", err, out.String())
	}
}
