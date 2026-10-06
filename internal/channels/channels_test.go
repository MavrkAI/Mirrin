package channels

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestBackoffGrowsCapsAndStartsOverAfterAHealthySession(t *testing.T) {
	b := Backoff{Min: 100 * time.Millisecond, Max: 800 * time.Millisecond}
	within := func(d, nominal time.Duration) bool { return d >= nominal*8/10 && d <= nominal*12/10 }
	for i, nominal := range []time.Duration{100, 200, 400, 800, 800} {
		if d := b.Next(); !within(d, nominal*time.Millisecond) {
			t.Fatalf("attempt %d: %v, want about %v", i, d, nominal*time.Millisecond)
		}
	}
	// A session that stayed up a while: the next reconnect is quick again.
	b = Backoff{Min: time.Millisecond, Max: time.Hour}
	for i := 0; i < 20; i++ {
		b.Next()
	}
	start := time.Now()
	if !b.Wait(context.Background(), time.Now().Add(-2*StableAfter)) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("wait after a healthy session took %v", time.Since(start))
	}
	// A cancelled context ends the wait at once.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if (&Backoff{Min: time.Hour}).Wait(ctx, time.Time{}) {
		t.Fatal("wait should give up when ctx ends")
	}
}

func TestFatalAndPlain(t *testing.T) {
	bad := Fatal(errors.New("token rejected"))
	if !IsFatal(bad) || !IsFatal(fmt.Errorf("start: %w", bad)) || IsFatal(errors.New("timeout")) || Fatal(nil) != nil {
		t.Fatal("fatal classification wrong")
	}
	dns := &net.OpError{Op: "dial", Err: &net.DNSError{Name: "api.telegram.org", Err: "no such host"}}
	for err, want := range map[error]string{
		dns: "can't reach api.telegram.org; is this computer online?",
		&net.OpError{Op: "dial", Err: errors.New("connection refused")}: "can't connect to the server; is this computer online?",
		context.DeadlineExceeded: "the server took too long to answer",
		bad:                      "token rejected",
	} {
		if got := Plain(err); got != want {
			t.Errorf("Plain(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestTracker(t *testing.T) {
	var tr Tracker
	if st := tr.Status(); st.State != Connecting || st.Err != "" {
		t.Fatalf("fresh: %+v", st)
	}
	tr.Retrying(errors.New("socket closed"))
	if st := tr.Status(); st.State != Connecting || st.Err != "socket closed" {
		t.Fatalf("retrying: %+v", st)
	}
	tr.Up()
	if st := tr.Status(); st.State != Connected || st.Err != "" || st.Since.IsZero() {
		t.Fatalf("up: %+v", st)
	}
	tr.Down(Fatal(errors.New("intent missing")))
	if st := tr.Status(); st.State != Failed || st.Err != "intent missing" {
		t.Fatalf("down: %+v", st)
	}
}

type fakeTyper struct{ n atomic.Int32 }

func (f *fakeTyper) Typing(context.Context, string) (time.Duration, error) {
	f.n.Add(1)
	return 10 * time.Millisecond, nil
}

func TestKeepTypingUntilTheReplyIsSent(t *testing.T) {
	var f fakeTyper
	stop := KeepTyping(context.Background(), &f, "42")
	defer stop()
	time.Sleep(55 * time.Millisecond)
	if f.n.Load() < 3 {
		t.Fatalf("indicator refreshed %d times", f.n.Load())
	}
	StopTyping(&f, "42")
	time.Sleep(15 * time.Millisecond)
	n := f.n.Load()
	time.Sleep(40 * time.Millisecond)
	if f.n.Load() != n {
		t.Fatal("typing continued after the reply was sent")
	}
}

func TestSplitTextKeepsCharactersWhole(t *testing.T) {
	for _, s := range []string{strings.Repeat("a👍🏽", 500), strings.Repeat("你好", 700), strings.Repeat("line\n", 900)} {
		var joined strings.Builder
		for _, p := range SplitText(s, 400) {
			if len(p) > 400 || !utf8.ValidString(p) {
				t.Fatalf("bad part %q", p)
			}
			joined.WriteString(p)
		}
		if strings.ReplaceAll(joined.String(), "\n", "") != strings.ReplaceAll(s, "\n", "") {
			t.Fatal("text lost in splitting")
		}
	}
}

func TestMedia(t *testing.T) {
	for mime, want := range map[string]string{"audio/ogg": Voice, "image/jpeg": Photo, "video/mp4": Video, "application/pdf": File, "": File} {
		if got := MediaKind(mime); got != want {
			t.Errorf("MediaKind(%q) = %q", mime, got)
		}
	}
	if !strings.Contains(CantOpen(Voice), "voice") || !strings.Contains(CantOpen(Photo), "photos") {
		t.Fatal("replies should name what can't be opened")
	}
	if got := WithAttachment(" what's this? ", Photo); !strings.HasPrefix(got, "what's this?\n[A photo") {
		t.Fatalf("WithAttachment: %q", got)
	}
	if got := Received(Voice); !strings.HasPrefix(got, "[A voice message arrived") {
		t.Fatalf("Received: %q", got)
	}
}

// valueTyper is a Typer on a non-pointer type that can't be a map key.
type valueTyper struct {
	calls *atomic.Int32
	ends  chan string
	_     []string
}

func (v valueTyper) Name() string { return "matrix" }
func (v valueTyper) Typing(context.Context, string) (time.Duration, error) {
	v.calls.Add(1)
	return time.Hour, nil
}
func (v valueTyper) EndTyping(_ context.Context, chatID string) error {
	v.ends <- chatID
	return nil
}

// Any Typer works (a community one on a plain struct must not panic), the
// instance sending the reply stops the one that started it, and a lingering
// indicator is cleared.
func TestTypingIsKeyedByChannelAndCleared(t *testing.T) {
	var calls atomic.Int32
	started := valueTyper{calls: &calls, ends: make(chan string, 2)}
	stop := KeepTyping(context.Background(), started, "!room")
	rebuilt := valueTyper{calls: &calls, ends: started.ends}
	StopTyping(rebuilt, "!room")
	select {
	case chat := <-started.ends:
		if chat != "!room" {
			t.Fatalf("cleared %q", chat)
		}
	case <-time.After(time.Second):
		t.Fatal("the indicator was left showing")
	}
	stop() // already stopped: nothing more to clear
	select {
	case chat := <-started.ends:
		t.Fatalf("cleared twice: %q", chat)
	case <-time.After(50 * time.Millisecond):
	}
}

// A long reply that fails part-way must not be repeated in full elsewhere.
func TestSendPartsRemembersWhatIsLeft(t *testing.T) {
	text := strings.Repeat("a", 10) + "\n" + strings.Repeat("b", 10) + "\n" + strings.Repeat("c", 10)
	var sent []string
	err := SendParts(text, 12, func(part string) error {
		if len(sent) == 1 {
			return errors.New("network down")
		}
		sent = append(sent, part)
		return nil
	})
	if err == nil || len(sent) != 1 {
		t.Fatalf("err %v, sent %v", err, sent)
	}
	if got := Unsent(err, text); got != strings.Repeat("b", 10)+"\n"+strings.Repeat("c", 10) {
		t.Fatalf("unsent %q", got)
	}
	// Nothing went out: all of it is still to send.
	err = SendParts(text, 12, func(string) error { return errors.New("down") })
	if Unsent(err, text) != text || Unsent(errors.New("down"), text) != text {
		t.Fatal("a send that failed outright must be retried in full")
	}
	if !IsFatal(SendParts(text, 12, func(p string) error {
		if p[0] == 'b' {
			return Fatal(errors.New("revoked"))
		}
		return nil
	})) {
		t.Fatal("a Fatal part must stay Fatal")
	}
}
