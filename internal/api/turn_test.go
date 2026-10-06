package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/devices"
)

// slowTurn is a backend whose turns wait to be released (or stopped), then
// finish and record themselves only if their context is still alive.
type slowTurn struct {
	*fake
	started  chan struct{}
	release  chan struct{}
	recorded chan string
}

func (b *slowTurn) turn(ctx context.Context, in channels.Inbound) (string, error) {
	close(b.started)
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	if err := ctx.Err(); err != nil {
		b.recorded <- "cancelled"
		return "", err
	}
	b.recorded <- in.Text
	return "done", nil
}

func (b *slowTurn) Message(ctx context.Context, in channels.Inbound) (string, error) {
	return b.turn(ctx, in)
}

func (b *slowTurn) MessageEvents(ctx context.Context, in channels.Inbound, _ agent.Events) (string, error) {
	return b.turn(ctx, in)
}

// Revoking a device still stops its turn, though a turn outlives its request.
func TestRevokingADeviceStopsItsTurn(t *testing.T) {
	b := &slowTurn{fake: &fake{}, started: make(chan struct{}), release: make(chan struct{}), recorded: make(chan string, 1)}
	s := New("127.0.0.1:7742", master, b)
	d, tok, err := s.Devices().Add("phone", "pwa", []devices.Scope{devices.Chat}, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:7742/message", strings.NewReader(`{"text":"hi","channel":"screen"}`))
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("Authorization", "Bearer "+tok)
	ctx := context.WithValue(r.Context(), listenerKey, listener{kind: kindLoopback, via: "loopback"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(httptest.NewRecorder(), r.WithContext(ctx))
	}()
	select {
	case <-b.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the turn never started")
	}
	if _, err := s.RevokeDevice(d.ID); err != nil {
		t.Fatal(err)
	}
	defer close(b.release)
	if got := <-b.recorded; got != "cancelled" {
		t.Fatalf("a revoked device's turn went on: %q", got)
	}
	<-done
}

// A client that hangs up mid-turn doesn't cut the turn short: the twin
// finishes it and records it.
func TestTurnSurvivesDroppedClient(t *testing.T) {
	for _, path := range []string{"/message", "/message/stream"} {
		t.Run(path, func(t *testing.T) {
			b := &slowTurn{fake: &fake{}, started: make(chan struct{}), release: make(chan struct{}), recorded: make(chan string, 1)}
			s := New("127.0.0.1:7742", master, b)
			r := httptest.NewRequest("POST", "http://127.0.0.1:7742"+path, strings.NewReader(`{"text":"remember the milk"}`))
			r.RemoteAddr = "127.0.0.1:50000"
			r.Header.Set("Authorization", "Bearer "+master)
			ctx, cancel := context.WithCancel(context.WithValue(r.Context(), listenerKey, listener{kind: kindLoopback, via: "loopback"}))
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.Handler().ServeHTTP(httptest.NewRecorder(), r.WithContext(ctx))
			}()
			select {
			case <-b.started:
			case <-time.After(2 * time.Second):
				t.Fatal("the turn never started")
			}
			cancel() // the client goes away
			close(b.release)
			select {
			case got := <-b.recorded:
				if got != "remember the milk" {
					t.Fatalf("turn = %q, want it finished and recorded", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the turn never finished")
			}
			<-done
		})
	}
}

// A device whose client hung up mid-turn, then was revoked, has its turn
// stopped too: the turn is tracked under the device, not only the request.
func TestRevokingADeviceStopsItsTurnAfterItsClientLeft(t *testing.T) {
	b := &slowTurn{fake: &fake{}, started: make(chan struct{}), release: make(chan struct{}), recorded: make(chan string, 1)}
	s := New("127.0.0.1:7742", master, b)
	d, tok, err := s.Devices().Add("phone", "pwa", []devices.Scope{devices.Chat}, "tailscale", "")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:7742/message", strings.NewReader(`{"text":"hi","channel":"screen"}`))
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("Authorization", "Bearer "+tok)
	ctx, drop := context.WithCancel(context.WithValue(r.Context(), listenerKey, listener{kind: kindLoopback, via: "loopback"}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(httptest.NewRecorder(), r.WithContext(ctx))
	}()
	select {
	case <-b.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the turn never started")
	}
	drop() // the client goes away first
	time.Sleep(20 * time.Millisecond)
	if _, err := s.RevokeDevice(d.ID); err != nil {
		t.Fatal(err)
	}
	defer close(b.release)
	select {
	case got := <-b.recorded:
		if got != "cancelled" {
			t.Fatalf("a revoked device's turn went on: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a revoked device's turn went on after its client left")
	}
	<-done
}

// The stream answers as soon as the twin has the message, before the turn's
// first word: a screen that loses the line mid-turn knows its words got
// through (they took the twin's "stop asking?", and trying again would say
// them twice). The headers used to wait for the reply.
func TestStreamSaysAtOnceTheTwinHasTheMessage(t *testing.T) {
	b := &slowTurn{fake: &fake{}, started: make(chan struct{}), release: make(chan struct{}), recorded: make(chan string, 1)}
	s := New("127.0.0.1:7742", master, b)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := listener{kind: kindLoopback, via: "loopback"}
		s.Handler().ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), listenerKey, l)))
	}))
	defer ts.Close()
	defer close(b.release)
	req, _ := http.NewRequest("POST", ts.URL+"/message/stream", strings.NewReader(`{"text":"hi","channel":"screen"}`))
	req.Header.Set("Authorization", "Bearer "+master)
	type answer struct {
		resp *http.Response
		err  error
	}
	got := make(chan answer, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		got <- answer{resp, err}
	}()
	select {
	case <-b.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the turn never started")
	}
	select {
	case a := <-got:
		if a.err != nil {
			t.Fatal(a.err)
		}
		defer a.resp.Body.Close()
		if a.resp.StatusCode != 200 || a.resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("stream: %d %s", a.resp.StatusCode, a.resp.Header.Get("Content-Type"))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stream said nothing while the turn ran")
	}
}
