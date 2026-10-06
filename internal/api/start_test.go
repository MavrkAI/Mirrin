package api

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	return port
}

func TestStartServesTheLoopbackListener(t *testing.T) {
	e := newEnv(t)
	addr := "127.0.0.1:" + freePort(t)
	e.s.addr = addr
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.s.Start(ctx) }()
	get := func(path, host string, header map[string]string) (int, string) {
		r, _ := http.NewRequest("GET", "http://"+addr+path, nil)
		if host != "" {
			r.Host = host
		}
		for k, v := range header {
			r.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if code, _ := get("/healthz", "", nil); code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the API never came up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, body := get("/status", "", bearer(master)); code != 200 || !strings.Contains(body, "Mirrin") {
		t.Fatalf("status: %d %s", code, body)
	}
	if code, _ := get("/status", "evil.example:80", bearer(master)); code != http.StatusMisdirectedRequest {
		t.Fatalf("rebound name: %d", code)
	}
	if code, _ := get("/status", "", nil); code != 401 {
		t.Fatalf("no key: %d", code)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func TestStartRefusesAnOutsideAddressUnlessRemote(t *testing.T) {
	e := newEnv(t)
	e.s.addr = "192.0.2.1:7742"
	err := e.s.Start(context.Background())
	if err == nil || err.Error() != "api.listen must be a loopback address unless api.remote is true" {
		t.Fatalf("got %v", err)
	}
}
