//go:build pebble

package tlsmgr_test

// With -tags pebble, TestIntegrationIssueThroughRelay runs against a real
// Pebble (github.com/letsencrypt/pebble) instead of acmetest. The CI job
// acme-pebble does this with v2.10.1 built natively:
//
//	go install github.com/letsencrypt/pebble/v2/cmd/...@v2.10.1
//	pebble-challtestsrv -dnsserver 127.0.0.1:8053 -defaultIPv6 "" \
//	  -http01 "" -https01 "" -tlsalpn01 "" -doh "" -management 127.0.0.1:8055
//	PEBBLE_VA_NOSLEEP=1 pebble -config test/config/pebble-config.json \
//	  -dnsserver 127.0.0.1:8053   # run in Pebble's module directory
//
// then set:
//
//	MIRRIN_PEBBLE_DIRECTORY  default https://localhost:14000/dir
//	MIRRIN_PEBBLE_CA         Pebble's API certificate (test/certs/pebble.minica.pem)
//	MIRRIN_PEBBLE_TLS_PORT   the port Pebble validates TLS-ALPN-01 on, default 5001
//	MIRRIN_PEBBLE_HOST       the name to request, default twin.mirrin.test
//
// The relay listens on 127.0.0.1 at that port, as a relay would on 443.

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

func init() {
	pebble = func(t *testing.T) (string, *http.Client, string, string) {
		env := func(k, def string) string {
			if v := os.Getenv(k); v != "" {
				return v
			}
			return def
		}
		caFile := os.Getenv("MIRRIN_PEBBLE_CA")
		if caFile == "" {
			t.Fatal("-tags pebble needs MIRRIN_PEBBLE_CA (Pebble's minica certificate)")
		}
		b, err := os.ReadFile(caFile)
		if err != nil {
			t.Fatal(err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			t.Fatal("MIRRIN_PEBBLE_CA holds no certificate")
		}
		client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
		addr := net.JoinHostPort("127.0.0.1", env("MIRRIN_PEBBLE_TLS_PORT", "5001"))
		return env("MIRRIN_PEBBLE_DIRECTORY", "https://localhost:14000/dir"), client, addr, env("MIRRIN_PEBBLE_HOST", "twin.mirrin.test")
	}
}
