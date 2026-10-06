package reach

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/certwatch"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr"
)

// Verify proves the chain from outside, the way a phone on cellular would:
// the name reaches a TLS server holding this machine's key, the certificate
// chains to a public root and isn't about to expire, the twin answers
// behind it, and CAA pins this machine's ACME account.

// CheckResult is one line of a verify report.
type CheckResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Warn   bool   `json:"warn,omitempty"` // passes, but worth fixing
	Detail string `json:"detail"`
}

// Report is what `mirrin reach verify` prints.
type Report struct {
	Host       string        `json:"host"`
	OK         bool          `json:"ok"`
	Served     string        `json:"served_spki,omitempty"`
	Pins       []string      `json:"pins"`
	AccountURI string        `json:"account_uri,omitempty"`
	Checks     []CheckResult `json:"checks"`
}

func (r *Report) add(c CheckResult) {
	r.Checks = append(r.Checks, c)
	if !c.OK {
		r.OK = false
	}
}

// LocalEndpoint is what the CLI knows about this machine's relay reach,
// read from config and data/tls without changing anything.
func LocalEndpoint(c config.Reach, dataDir string) (*Endpoint, error) {
	host, err := CheckRelayConfig(c)
	if err != nil {
		return nil, err
	}
	st, err := tlsmgr.ReadACMEState(filepath.Join(dataDir, "tls"))
	if err != nil {
		return nil, fmt.Errorf("this machine has no HTTPS keys yet; start Mirrin with reach set to relay first (%w)", err)
	}
	pins := []string{st.Current, st.Next}
	return &Endpoint{
		Hostname: host, URL: "https://" + host, RelayURL: c.RelayURL,
		Pins:       func() []string { return pins },
		AccountURI: func() string { return st.AccountURI },
	}, nil
}

// Verify checks e from outside. It returns an error only when it can't
// start; a failed check is in the report.
func Verify(ctx context.Context, e *Endpoint) (Report, error) {
	if e == nil || e.Hostname == "" || e.Pins == nil {
		return Report{}, errors.New("reach: nothing to verify")
	}
	r := Report{Host: e.Hostname, OK: true, Pins: e.Pins()}
	if e.AccountURI != nil {
		r.AccountURI = e.AccountURI()
	}
	if e.Listener != nil {
		if e.Online() {
			r.add(CheckResult{Name: "relay", OK: true, Detail: "connected to " + e.RelayURL})
		} else {
			r.add(CheckResult{Name: "relay", Detail: "not connected to " + e.RelayURL})
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := dialName(ctx, e)
	if err != nil {
		r.add(CheckResult{Name: "certificate", Detail: "couldn't reach " + e.Hostname + ": " + err.Error()})
	} else {
		defer conn.Close()
		checkServed(ctx, e, conn, &r)
	}
	checkCAA(ctx, e, &r)
	return r, nil
}

func dialName(ctx context.Context, e *Endpoint) (*tls.Conn, error) {
	addr := net.JoinHostPort(e.Hostname, "443")
	var raw net.Conn
	var err error
	if e.Dial != nil {
		raw, err = e.Dial(ctx, addr)
	} else {
		raw, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	// The chain is checked below, against the pins first: a wrong key is the
	// finding that matters, and it must be reported as such.
	c := tls.Client(raw, &tls.Config{ServerName: e.Hostname, NextProtos: []string{"http/1.1"}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err := c.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return c, nil
}

func checkServed(ctx context.Context, e *Endpoint, c *tls.Conn, r *Report) {
	cs := c.ConnectionState()
	leaf := cs.PeerCertificates[0]
	r.Served = tlsmgr.SPKIPin(leaf)
	if !slices.Contains(r.Pins, r.Served) {
		r.add(CheckResult{Name: "certificate", Detail: fmt.Sprintf("something else answered as %s: its key (%s) isn't this computer's. The relay may be sending the name elsewhere", e.Hostname, r.Served)})
		return
	}
	r.add(CheckResult{Name: "certificate", OK: true, Detail: "the key is this computer's (" + r.Served + ")"})
	inter := x509.NewCertPool()
	for _, ic := range cs.PeerCertificates[1:] {
		inter.AddCert(ic)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: e.Hostname, Roots: e.Roots, Intermediates: inter}); err != nil {
		r.add(CheckResult{Name: "chain", Detail: "browsers won't trust it: " + err.Error()})
	} else {
		r.add(CheckResult{Name: "chain", OK: true, Detail: "trusted, issued by " + leaf.Issuer.CommonName})
	}
	left := time.Until(leaf.NotAfter)
	switch {
	case left <= 0:
		r.add(CheckResult{Name: "expiry", Detail: "expired " + leaf.NotAfter.Format("2 Jan 2006")})
	case left < tlsmgr.AlarmLeft:
		r.add(CheckResult{Name: "expiry", OK: true, Warn: true, Detail: "expires " + leaf.NotAfter.Format("2 Jan 2006") + " and hasn't renewed yet"})
	default:
		r.add(CheckResult{Name: "expiry", OK: true, Detail: "valid until " + leaf.NotAfter.Format("2 Jan 2006")})
	}
	// The twin itself answers behind the certificate.
	c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "GET /healthz HTTP/1.1\r\nHost: %s\r\nUser-Agent: mirrin-verify\r\nConnection: close\r\n\r\n", e.Hostname)
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		r.add(CheckResult{Name: "twin", Detail: "the twin didn't answer: " + err.Error()})
		return
	}
	io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		r.add(CheckResult{Name: "twin", Detail: "the twin answered " + res.Status})
		return
	}
	r.add(CheckResult{Name: "twin", OK: true, Detail: "the twin answers at https://" + e.Hostname})
}

func checkCAA(ctx context.Context, e *Endpoint, r *Report) {
	if r.AccountURI == "" {
		r.add(CheckResult{Name: "caa", OK: true, Warn: true, Detail: "no ACME account yet, so there's nothing for CAA to pin"})
		return
	}
	lookup := e.CAA
	if lookup == nil {
		lookup = func(ctx context.Context, name string) ([]certwatch.CAA, error) {
			return certwatch.LookupCAA(ctx, name, nil)
		}
	}
	recs, err := lookup(ctx, e.Hostname)
	if err != nil {
		r.add(CheckResult{Name: "caa", OK: true, Warn: true, Detail: "couldn't look CAA up: " + err.Error()})
		return
	}
	v := certwatch.CheckCAA(recs, r.AccountURI)
	switch {
	case v.OK:
		r.add(CheckResult{Name: "caa", OK: true, Detail: "only this machine's ACME account can get certificates for the name"})
	case v.Critical:
		r.add(CheckResult{Name: "caa", Detail: v.Problem})
	default:
		r.add(CheckResult{Name: "caa", OK: true, Warn: true, Detail: v.Problem + " Publish the CAA records `mirrin reach use relay` prints."})
	}
}

// VerifyCommand runs Verify and prints the report, as text or JSON. It
// returns an error when any check failed, so the command exits non-zero.
func VerifyCommand(ctx context.Context, e *Endpoint, w io.Writer, asJSON bool) error {
	r, err := Verify(ctx, e)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(w, "%s\n", r.Host)
		for _, c := range r.Checks {
			mark := "ok  "
			switch {
			case !c.OK:
				mark = "FAIL"
			case c.Warn:
				mark = "warn"
			}
			fmt.Fprintf(w, "  %s  %-11s %s\n", mark, c.Name, c.Detail)
		}
		fmt.Fprintf(w, "  pins: %s\n", strings.Join(r.Pins, " "))
	}
	if !r.OK {
		return fmt.Errorf("%s didn't verify; see the lines marked FAIL", r.Host)
	}
	return nil
}
