package server

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// Acceptance: the benchmark holds 10,000 idle tunnels under 1 GB RSS.
//
//	go test -run '^$' -bench BenchmarkIdleTunnels -benchtime 1x ./internal/relay/server
//
// The relay runs alone in a child process (TestRelayChild) so that its RSS
// is its own; this process plays 10,000 daemons, each with a welcomed
// tunnel and its control stream. MIRRIN_RELAY_BENCH_TUNNELS changes the
// count.
func BenchmarkIdleTunnels(b *testing.B) {
	n := 10000
	if s := os.Getenv("MIRRIN_RELAY_BENCH_TUNNELS"); s != "" {
		n, _ = strconv.Atoi(s)
	}
	dir := b.TempDir()
	pki := newPKI(b)
	cert := pki.leaf(b, controlName)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
	writePEM(b, filepath.Join(dir, "control.crt"), "CERTIFICATE", cert.Certificate[0])
	writePEM(b, filepath.Join(dir, "control.key"), "PRIVATE KEY", keyDER)

	devs := make([]device, n)
	var y strings.Builder
	fmt.Fprintf(&y, "id: r1\ncontrol_hostname: %s\ncert_file: %s\nkey_file: %s\nlisten: 127.0.0.1:0\nhttp_listen: \"\"\nmetrics_listen: \"\"\n",
		controlName, filepath.Join(dir, "control.crt"), filepath.Join(dir, "control.key"))
	y.WriteString("limits: {hello_per_ip_per_min: 0}\nallow:\n")
	for i := range devs {
		devs[i] = benchDevice(i)
		fmt.Fprintf(&y, "  - {hostname: t%d.test, key: %s}\n", i, entitle.EncodeKey(devs[i].pub()))
	}
	if err := os.WriteFile(filepath.Join(dir, "relay.yaml"), []byte(y.String()), 0o600); err != nil {
		b.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRelayChild$")
	cmd.Env = append(os.Environ(), "MIRRIN_RELAY_CHILD="+dir)
	cmd.Stderr = os.Stderr
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		b.Fatal(err)
	}
	defer func() {
		stdin.Close()
		cmd.Wait()
	}()
	sc := bufio.NewScanner(stdout)
	var addr string
	for sc.Scan() {
		if a, ok := strings.CutPrefix(sc.Text(), "ADDR "); ok {
			addr = a
			break
		}
	}
	if addr == "" {
		b.Fatal("the relay did not start")
	}
	go io.Copy(io.Discard, stdout)
	r := &testRelay{addr: addr, pki: pki}

	b.ResetTimer()
	start := time.Now()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		sem  = make(chan struct{}, 32) // under maxPeekingAddr: all dials come from one address
		held = make([]*testTunnel, n)
	)
	for i := range devs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			tt, err := dialTunnel(b, r, signedHello(b, devs[i], ""))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			held[i] = tt
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		b.Fatalf("%d of %d tunnels failed; first: %v", len(errs), n, errs[0])
	}
	b.StopTimer()
	up := time.Since(start)
	time.Sleep(5 * time.Second) // idle: only keepalives from here on
	rss := rssOf(b, cmd.Process.Pid)
	b.ReportMetric(float64(rss)/float64(n), "RSS-bytes/tunnel")
	b.ReportMetric(float64(rss)/(1<<20), "RSS-MiB")
	b.Logf("%d tunnels up in %v; relay RSS %.0f MiB (%.1f KiB per tunnel)", n, up.Round(time.Millisecond), float64(rss)/(1<<20), float64(rss)/float64(n)/1024)
	if n >= 10000 && rss >= 1<<30 {
		b.Fatalf("relay RSS %d bytes for %d idle tunnels, over 1 GiB", rss, n)
	}
	for _, tt := range held {
		tt.sess.Close()
	}
}

// benchDevice is device i, derived so the child can be configured without
// sharing secrets out of band.
func benchDevice(i int) device {
	seed := sha256.Sum256(binary.BigEndian.AppendUint64([]byte("mirrin relay bench "), uint64(i)))
	sk := sha256.Sum256(seed[:])
	return device{key: ed25519.NewKeyFromSeed(seed[:]), statusKey: sk[:]}
}

// TestRelayChild is BenchmarkIdleTunnels' relay process. It runs only with
// MIRRIN_RELAY_CHILD set, and serves until its stdin closes.
func TestRelayChild(t *testing.T) {
	dir := os.Getenv("MIRRIN_RELAY_CHILD")
	if dir == "" {
		t.Skip("the relay process for BenchmarkIdleTunnels")
	}
	cfg, err := LoadConfig(filepath.Join(dir, "relay.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(*cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	fmt.Printf("ADDR %s\n", ln.Addr())
	io.Copy(io.Discard, os.Stdin)
	s.Close()
}

func writePEM(t testing.TB, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// rssOf is a process's resident set size in bytes, from ps (Linux and
// macOS report it in KiB).
func rssOf(t testing.TB, pid int) int64 {
	t.Helper()
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatal(err)
	}
	kib, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return kib << 10
}
