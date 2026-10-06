package voice

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
)

// rig is a voice channel with a scripted wake helper and whisper, recording
// what it plays, says and hands on. Nothing touches a microphone or speaker.
type rig struct {
	t *testing.T
	c *Channel

	mu        sync.Mutex
	heard     map[string]string        // clip → transcript
	hold      map[string]chan struct{} // clip → closed when its transcription may finish
	done      map[string]bool          // clips transcribed
	checked   int                      // wake clips whose check has stored its verdict
	acks      []bool                   // hello?
	chimes    int
	sent      []string // commands to the helper
	problems  []string
	startErr  error
	starts    int
	said      string // file the voice command appends each spoken line to
	turns     chan string
	onTurn    func(text string)
	loopEnded chan error
}

func newRig(t *testing.T, tweak func(*config.Voice)) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{t: t, heard: map[string]string{}, hold: map[string]chan struct{}{}, done: map[string]bool{},
		turns: make(chan string, 16), loopEnded: make(chan error, 1), said: filepath.Join(dir, "said.txt")}
	cfg := config.Voice{WakeWord: "mirrin", FollowupSeconds: 2, MaxSeconds: 5, KokoroDir: dir,
		TTSCommand: `printf '%s\n' "$TEXT" >> "` + r.said + `"`}
	if tweak != nil {
		tweak(&cfg)
	}
	c := New(cfg, "Mirrin", dir)
	c.out = io.Discard
	c.wake = newWakeHelper(dir, "", 0.5, dir, cfg.MaxSeconds, cfg.FollowupSeconds)
	c.wake.startFn = func(context.Context) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.starts++
		return r.startErr
	}
	c.wake.sendFn = func(cmd string) {
		r.mu.Lock()
		r.sent = append(r.sent, cmd)
		r.mu.Unlock()
	}
	c.transcribeFn = func(ctx context.Context, wav string) (string, error) {
		r.mu.Lock()
		h := r.hold[wav]
		r.mu.Unlock()
		if h != nil {
			select {
			case <-h:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.done[wav] = true
		return r.heard[wav], nil
	}
	c.ackFn = func(hello bool) {
		r.mu.Lock()
		r.acks = append(r.acks, hello)
		r.mu.Unlock()
	}
	c.chimeFn = func() {
		r.mu.Lock()
		r.chimes++
		r.mu.Unlock()
	}
	c.OnProblem = func(msg string) {
		r.mu.Lock()
		r.problems = append(r.problems, msg)
		r.mu.Unlock()
	}
	c.ackMakerFn = func() ackMaker { return ackMaker{} }
	c.clipChecked = func() {
		r.mu.Lock()
		r.checked++
		r.mu.Unlock()
	}
	r.c = c
	return r
}

// run starts the wake loop; it stops with the test.
func (r *rig) run() {
	ctx, cancel := context.WithCancel(context.Background())
	r.t.Cleanup(func() {
		cancel()
		select {
		case <-r.loopEnded:
		case <-time.After(5 * time.Second):
			r.t.Error("the wake loop didn't stop")
		}
	})
	go func() {
		r.loopEnded <- r.c.wakeLoop(ctx, func(_ context.Context, in channels.Inbound) {
			if r.onTurn != nil {
				r.onTurn(in.Text)
			}
			r.turns <- in.Text
		})
	}()
	r.eventually("the helper to start", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.starts > 0 })
}

// event sends a helper line, as the current helper.
func (r *rig) event(line string) {
	r.c.wake.mu.Lock()
	gen := r.c.wake.gen
	r.c.wake.mu.Unlock()
	r.c.wake.events <- parseWakeEvent(line, gen)
}

// say scripts what whisper hears in a clip.
func (r *rig) say(clip, text string) {
	r.mu.Lock()
	r.heard[clip] = text
	r.mu.Unlock()
}

func (r *rig) turn() string {
	r.t.Helper()
	select {
	case s := <-r.turns:
		return s
	case <-time.After(5 * time.Second):
		r.t.Fatal("nothing was handed to the twin")
		return ""
	}
}

func (r *rig) noTurn(within time.Duration) {
	r.t.Helper()
	select {
	case s := <-r.turns:
		r.t.Fatalf("handed on %q", s)
	case <-time.After(within):
	}
}

func (r *rig) eventually(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// listens counts the "listen" commands (follow-up windows opened).
func (r *rig) listens() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.sent {
		if s == "listen" {
			n++
		}
	}
	return n
}

func (r *rig) ackCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.acks)
}

func (r *rig) spoken() string {
	b, _ := os.ReadFile(r.said)
	return string(b)
}

// A bare "Yes." in the follow-up window used to be thrown away as a whisper
// hallucination, so the approval the twin had just asked about never
// happened.
func TestFollowUpKeepsABareYesToAQuestion(t *testing.T) {
	r := newRig(t, nil)
	r.onTurn = func(text string) {
		if text == "book the cab" {
			r.c.lastSaid.Store("There's one at six. Shall I book it?")
		}
	}
	r.run()
	r.say("u1", "Hey Mirrin, book the cab")
	r.event("wake 0.91")
	r.event("utterance u1 (wake 1.4s)")
	if got := r.turn(); got != "book the cab" {
		t.Fatalf("first turn %q", got)
	}
	r.eventually("a follow-up window", func() bool { return r.listens() == 1 })
	r.say("u2", "Yes.")
	r.event("utterance u2 (followup 0.9s)")
	if got := r.turn(); got != "Yes." {
		t.Fatalf("the yes was lost: got %q", got)
	}
}

func TestFollowUpKeepsShortAnswersWhileAnApprovalWaits(t *testing.T) {
	for _, answer := range []string{"No.", "Sure.", "Okay.", "Stop."} {
		t.Run(answer, func(t *testing.T) {
			r := newRig(t, nil)
			r.c.Pending = func() bool { return true } // "yes 4" was asked earlier
			r.onTurn = func(string) { r.c.lastSaid.Store("Done. The invite is out.") }
			r.run()
			r.say("u1", "Hey Mirrin, invite Sam")
			r.event("wake 0.9")
			r.event("utterance u1 (wake 1.2s)")
			r.turn()
			r.eventually("a follow-up window", func() bool { return r.listens() == 1 })
			r.say("u2", answer)
			r.event("utterance u2 (followup 0.7s)")
			if got := r.turn(); got != answer {
				t.Fatalf("got %q", got)
			}
		})
	}
}

// With nothing asked and nothing waiting, a lone short word is still most
// likely whisper guessing at noise.
func TestFollowUpStillIgnoresNoiseWhenNothingWasAsked(t *testing.T) {
	r := newRig(t, nil)
	r.onTurn = func(string) { r.c.lastSaid.Store("It's half past two.") }
	r.run()
	r.say("u1", "Hey Mirrin, what's the time")
	r.event("wake 0.9")
	r.event("utterance u1 (wake 1.2s)")
	r.turn()
	r.eventually("a follow-up window", func() bool { return r.listens() == 1 })
	r.say("u2", "Okay.")
	r.event("utterance u2 (followup 0.5s)")
	r.noTurn(300 * time.Millisecond)
}

// The twin used to say "Hello." or "Right." before whisper had confirmed the
// wake phrase, then go silent: a television line that scored on the
// detector got a spoken greeting.
func TestNoWordsUntilTheWakePhraseIsConfirmed(t *testing.T) {
	r := newRig(t, nil)
	r.run()
	r.say("u1", "the Mirrin pilot flew low over the base")
	r.event("wake 0.62")
	r.event("utterance u1 (wake 2.1s)")
	r.eventually("the transcript", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.done["u1"] })
	r.noTurn(200 * time.Millisecond)
	if n := r.ackCount(); n != 0 {
		t.Fatalf("spoke %d acknowledgement(s) for a wake that wasn't confirmed", n)
	}
	r.say("u2", "Hey Mirrin, what's on today?")
	r.event("wake 0.93")
	r.event("utterance u2 (wake 1.8s)")
	if got := r.turn(); got != "what's on today?" {
		t.Fatalf("turn %q", got)
	}
	r.eventually("an acknowledgement for the confirmed wake", func() bool { return r.ackCount() == 1 })
	// The soft tick on the detector is what says "heard something".
	r.eventually("a chime for each wake", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.chimes == 2 })
}

// The wake phrase is confirmed from its own clip while the owner is still
// talking, so the acknowledgement plays as soon as they stop, not after
// the whole utterance is transcribed.
func TestAcknowledgementDoesntWaitForTheWholeTranscript(t *testing.T) {
	r := newRig(t, nil)
	r.run()
	r.say("clip", "Hey Mirrin.")
	r.say("u1", "Hey Mirrin, remind me to call Mum at six")
	release := make(chan struct{})
	r.mu.Lock()
	r.hold["u1"] = release
	r.mu.Unlock()
	r.event("wake 0.95")
	r.event("wakeclip clip")
	r.eventually("the wake clip's verdict", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.checked == 1 })
	r.event("utterance u1 (wake 3.0s)")
	r.eventually("an acknowledgement before the transcript", func() bool { return r.ackCount() == 1 })
	r.mu.Lock()
	transcribed := r.done["u1"]
	r.mu.Unlock()
	if transcribed {
		t.Fatal("the acknowledgement waited for the whole transcript")
	}
	close(release)
	if got := r.turn(); got != "remind me to call Mum at six" {
		t.Fatalf("turn %q", got)
	}
	// The acknowledgement plays on its own goroutine, so it may land just
	// after the turn is handed on; it must land, and only once.
	r.eventually("one acknowledgement", func() bool { return r.ackCount() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if n := r.ackCount(); n != 1 {
		t.Fatalf("%d acknowledgements, want 1", n)
	}
}

// "Hey Mirrin" alone is answered with "Sir?", not an acknowledgement too.
func TestBareWakeIsAskedNotAcknowledged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("records speech with a shell command")
	}
	r := newRig(t, nil)
	r.c.SetAddress("sir") // Mirrin's
	r.run()
	r.say("clip", "Hey Mirrin.")
	r.say("u1", "Hey Mirrin.")
	r.event("wake 0.9")
	r.event("wakeclip clip")
	r.eventually("the wake clip checked", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.done["clip"] })
	r.event("utterance u1 (wake 1.3s bare)")
	r.eventually("Sir?", func() bool { return strings.Contains(r.spoken(), "Sir?") })
	if n := r.ackCount(); n != 0 {
		t.Fatalf("%d acknowledgements on top of Sir?", n)
	}
	r.eventually("a follow-up window", func() bool { return r.listens() == 1 })
	r.say("u2", "What's the weather?")
	r.event("utterance u2 (followup 1.1s)")
	if got := r.turn(); got != "What's the weather?" {
		t.Fatalf("turn %q", got)
	}
}

// "Stop!" said over the twin used to be dropped (too few new words), so it
// kept talking.
func TestStopOverTheTwinStopsIt(t *testing.T) {
	r := newRig(t, nil)
	r.run()
	stopped := make(chan struct{})
	r.c.smu.Lock()
	r.c.speaking = true
	r.c.speakCancel = func() { close(stopped) }
	r.c.smu.Unlock()
	r.c.lastSaid.Store("Here's the long version of the story.")
	r.say("u1", "Stop!")
	r.event("interrupt")
	r.event("utterance u1 (followup 0.6s)")
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("it kept talking")
	}
	r.noTurn(200 * time.Millisecond)
}

// When the helper stops (the microphone went away, Python crashed), it is
// started again; when it can't be, the owner hears about it once and
// listening ends with a reason instead of silently.
func TestHelperThatStopsIsRestartedThenReported(t *testing.T) {
	defer func(d time.Duration) { restartBackoff = d }(restartBackoff)
	restartBackoff = time.Millisecond
	r := newRig(t, nil)
	r.run()
	r.event("err wake helper exited")
	r.eventually("a restart", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.starts == 2 })
	time.Sleep(20 * time.Millisecond)
	r.mu.Lock()
	if len(r.problems) != 0 {
		t.Fatalf("a restart that worked was reported: %q", r.problems)
	}
	r.startErr = errors.New("no input device")
	r.mu.Unlock()
	r.event("err microphone stream ended")
	r.event("err wake helper exited")
	select {
	case err := <-r.loopEnded:
		r.loopEnded <- err // for the cleanup
		if err == nil || !strings.Contains(err.Error(), "wake-word helper stopped") {
			t.Fatalf("the loop ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listening carried on with no helper")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.problems) != 1 || !strings.Contains(r.problems[0], "stopped listening") || !strings.Contains(r.problems[0], "restart me from the menu") {
		t.Fatalf("problems = %q", r.problems)
	}
}

// A blocked microphone gives pure digital silence: the owner is told once,
// and again only after it came back and went away again.
func TestMutedMicrophoneIsToldOnce(t *testing.T) {
	r := newRig(t, nil)
	r.run()
	count := func() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.problems) }
	r.event("mute")
	r.event("mute")
	r.eventually("the report", func() bool { return count() == 1 })
	time.Sleep(20 * time.Millisecond)
	if count() != 1 {
		t.Fatal("told twice")
	}
	r.event("unmute")
	r.event("mute")
	r.eventually("the second report", func() bool { return count() == 2 })
	if !strings.Contains(r.problems[0], "microphone") {
		t.Fatalf("problem %q", r.problems[0])
	}
}

func TestParseWakeEvent(t *testing.T) {
	for line, want := range map[string]wakeEvent{
		"utterance /x/voice-in.wav (wake 1.2s bare)": {kind: "utterance", arg: "/x/voice-in.wav", bare: true},
		"utterance /x/voice-in.wav (followup 0.8s)":  {kind: "utterance", arg: "/x/voice-in.wav", followup: true},
		"silence - (followup)":                       {kind: "silence", arg: "-", followup: true},
		"wakeclip /x/voice-in-wake.wav":              {kind: "wakeclip", arg: "/x/voice-in-wake.wav"},
		"mute":                                       {kind: "mute"},
	} {
		if got := parseWakeEvent(line, 0); got != want {
			t.Errorf("%q: got %+v, want %+v", line, got, want)
		}
	}
}

// A helper that restarts fine and then stops again (the microphone was
// unplugged: sox fails right after "ready") used to use up its restarts
// and then go quiet: no helper, no warning, and listening carried on deaf.
func TestHelperThatKeepsStoppingAfterRestartsIsReported(t *testing.T) {
	defer func(d time.Duration) { restartBackoff = d }(restartBackoff)
	restartBackoff = time.Millisecond
	r := newRig(t, nil)
	r.run()
	starts := func() int { r.mu.Lock(); defer r.mu.Unlock(); return r.starts }
	for i := 1; i <= helperRestarts; i++ {
		r.event("err wake helper exited")
		r.eventually("a restart", func() bool { return starts() == i+1 })
	}
	r.event("err wake helper exited")
	select {
	case err := <-r.loopEnded:
		r.loopEnded <- err // for the cleanup
		if err == nil || !strings.Contains(err.Error(), "wake-word helper stopped") {
			t.Fatalf("the loop ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listening carried on with no helper, and nobody was told")
	}
	if n := starts(); n != helperRestarts+1 {
		t.Fatalf("%d starts, want %d", n, helperRestarts+1)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.problems) != 1 || !strings.Contains(r.problems[0], "stopped listening") {
		t.Fatalf("problems = %q", r.problems)
	}
}

// Once the wake clip confirmed the name, the twin said "Right." and then
// dropped the request when the full transcript spelled the name another
// way ("Hey Mirrins, …"), going silent right after acknowledging.
func TestConfirmedWakeIsNotDroppedAfterTheAcknowledgement(t *testing.T) {
	r := newRig(t, nil)
	r.run()
	r.say("clip", "Hey Mirrin.")
	r.say("u1", "Hey Mirrins, remind me to call Mum at six")
	r.event("wake 0.95")
	r.event("wakeclip clip")
	r.eventually("the wake clip's verdict", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.checked == 1 })
	r.event("utterance u1 (wake 3.0s)")
	if got := r.turn(); got != "remind me to call Mum at six" {
		t.Fatalf("turn %q", got)
	}
	// The acknowledgement plays on its own goroutine, so it may land just
	// after the turn is handed on; it must land, and only once.
	r.eventually("one acknowledgement", func() bool { return r.ackCount() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if n := r.ackCount(); n != 1 {
		t.Fatalf("%d acknowledgements, want 1", n)
	}
}

func TestAfterWakePhrase(t *testing.T) {
	for _, c := range []struct{ text, phrase, want string }{
		{"Hey Mirrins, remind me to call Mum", "hey mirrin", "remind me to call Mum"},
		{"Hay Mirrins remind me", "hey mirrin", "remind me"},
		{"A Mirrin's, what's on today?", "hey mirrin", "what's on today?"},
		{"Hey Mir in, lights off", "hey mirrin", "lights off"},
		{"Hey Mirrins.", "hey mirrin", ""},
		{"Hey Mirrins, Mirrin Street directions", "hey mirrin", "Mirrin Street directions"},
		{"Something else entirely", "hey mirrin", "Something else entirely"},
	} {
		if got := afterWakePhrase(c.text, c.phrase, "mirrin", nil); got != c.want {
			t.Errorf("%q: got %q, want %q", c.text, got, c.want)
		}
	}
}

// "Hold on", "Wait" and "One moment" are the twin's own words often
// enough: heard back from its own speaker they used to cut the reply off.
func TestTwinsOwnWordsDontStopIt(t *testing.T) {
	r := newRig(t, nil)
	r.run()
	stopped := make(chan struct{})
	r.c.smu.Lock()
	r.c.speaking = true
	r.c.speakCancel = func() { close(stopped) }
	r.c.smu.Unlock()
	r.c.lastSaid.Store("Hold on, let me check the calendar for you.")
	r.say("u1", "Hold on.")
	r.event("interrupt")
	r.event("utterance u1 (followup 0.6s)")
	r.eventually("the echo rejected", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, s := range r.sent {
			if s == "reject" {
				return true
			}
		}
		return false
	})
	select {
	case <-stopped:
		t.Fatal("its own words stopped it")
	case <-time.After(100 * time.Millisecond):
	}
	r.noTurn(100 * time.Millisecond)
}

// "Hey Mirrin, stop" while the twin works on what it was asked used to
// wait in line behind that turn, since the handler runs in line. It now
// stops the turn at once (OnStop) and isn't a turn of its own.
func TestSpokenStopCutsTheTurnShort(t *testing.T) {
	r := newRig(t, nil)
	release := make(chan struct{})
	var stops atomic.Int32
	r.c.OnStop = func() {
		if stops.Add(1) == 1 {
			close(release)
		}
	}
	r.onTurn = func(text string) {
		if text == "book the table" {
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
		}
	}
	r.run()
	r.say("u1", "Hey Mirrin, book the table")
	r.event("wake 0.9")
	r.event("utterance u1 (wake 1.2s)")
	r.eventually("the turn to start", func() bool { return r.c.handling.Load() })
	r.say("u2", "Hey Mirrin, stop.")
	r.event("wake 0.95")
	r.event("utterance u2 (wake 0.8s)")
	select {
	case <-release:
	case <-time.After(3 * time.Second):
		t.Fatal("the spoken stop waited behind the turn it was meant to stop")
	}
	if got := r.turn(); got != "book the table" {
		t.Fatalf("turn %q", got)
	}
	r.noTurn(300 * time.Millisecond) // the stop is not a turn of its own
	if n := stops.Load(); n != 1 {
		t.Fatalf("OnStop called %d times", n)
	}
	// With nothing being worked on, "stop" is just what was said.
	r.say("u3", "Hey Mirrin, stop.")
	r.event("wake 0.95")
	r.event("utterance u3 (wake 0.8s)")
	if got := r.turn(); got != "stop." && got != "stop" {
		t.Fatalf("an idle stop: %q", got)
	}
}

// A slow earlier capture cannot delay a new wake or deliver stale work.
func TestWakeSupersedesSlowTranscription(t *testing.T) {
	r := newRig(t, nil)
	release, started := make(chan struct{}), make(chan struct{})
	transcribe := r.c.transcribeFn
	r.c.transcribeFn = func(ctx context.Context, path string) (string, error) {
		if path == "slow" {
			close(started)
			<-release // model ignores cancellation and returns an obsolete result
			return "Hey Mirrin, old request", nil
		}
		return transcribe(ctx, path)
	}
	r.say("new", "Hey Mirrin, new request")
	r.run()
	r.event("wake 0.9")
	r.event("utterance slow (wake 1s)")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("transcription never started")
	}
	r.event("wake 0.95")
	r.event("utterance new (wake 1s)")
	if got := r.turn(); got != "new request" {
		t.Fatalf("got %q", got)
	}
	close(release)
	r.noTurn(100 * time.Millisecond)
}

func TestWakeEventsContinueWithFullCommandQueue(t *testing.T) {
	r := newRig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	commands := make(chan string, 1)
	commands <- "old"
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.c.wakeEvents(ctx, commands, make(chan string, 1), make(chan error, 1))
	}()
	r.say("u", "Hey Mirrin, new request")
	r.event("wake 0.9")
	r.event("utterance u (wake 1s)")
	r.eventually("confirmed transcript ready for delivery", func() bool {
		r.c.tmu.Lock()
		defer r.c.tmu.Unlock()
		return !r.c.pending.transcribed.IsZero()
	})
	r.event("mute")
	r.eventually("microphone event despite a full queue", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.problems) == 1
	})
	cancel()
	<-done
}

func TestConfirmedBargeInStopsWorkAndSaysWhatStopped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("records speech with a shell command")
	}
	for _, interrupt := range []string{"Hey Mirrin.", "Stop!"} {
		t.Run(interrupt, func(t *testing.T) {
			r := newRig(t, nil)
			release := make(chan struct{})
			var stops atomic.Int32
			r.c.OnInterrupt = func(string) func() string {
				if stops.Add(1) == 1 {
					close(release)
				}
				return func() string { return "Stopped checking your calendar." }
			}
			r.onTurn = func(text string) {
				if text == "check my calendar" {
					<-release
				}
			}
			r.run()
			t.Cleanup(func() {
				if stops.CompareAndSwap(0, 1) {
					close(release)
				}
			})
			r.say("first", "Hey Mirrin, check my calendar")
			r.event("wake 0.9")
			r.event("utterance first (wake 1s)")
			r.eventually("running turn", func() bool { return r.c.handling.Load() })
			r.say("interrupt", interrupt)
			if interrupt == "Stop!" {
				r.event("utterance interrupt (followup 1s)")
			} else {
				r.event("wake 0.95")
				r.event("utterance interrupt (wake 1s bare)")
			}
			r.eventually("work cancelled", func() bool { return stops.Load() == 1 })
			r.eventually("spoken stop summary", func() bool {
				return strings.Contains(r.spoken(), "Stopped checking your calendar.")
			})
		})
	}
}

// A detector hint isn't permission to forget an earlier real command.
func TestFalseWakePreservesSlowRealCommand(t *testing.T) {
	for _, event := range []string{"wake 0.6", "interrupt"} {
		t.Run(event, func(t *testing.T) {
			r := newRig(t, nil)
			release, started := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			transcribe := r.c.transcribeFn
			r.c.transcribeFn = func(ctx context.Context, path string) (string, error) {
				if path == "real" {
					close(started)
					select {
					case <-release:
						return "Hey Mirrin, turn off the lights", nil
					case <-ctx.Done():
						return "", ctx.Err()
					}
				}
				return transcribe(ctx, path)
			}
			r.run()
			r.event("wake 0.95")
			r.event("utterance real (wake 2s)")
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("real transcription never started")
			}
			r.say("false", "um")
			r.event(event)
			r.event("utterance false (wake 1s)")
			r.eventually("false wake transcribed without waiting", func() bool {
				r.mu.Lock()
				defer r.mu.Unlock()
				return r.done["false"]
			})
			once.Do(func() { close(release) })
			if got := r.turn(); got != "turn off the lights" {
				t.Fatalf("real command lost: %q", got)
			}
		})
	}
}

func TestNotificationBargeInOnlyStopsPlayback(t *testing.T) {
	r := newRig(t, nil)
	var calls atomic.Int32
	r.c.OnInterrupt = func(string) func() string {
		calls.Add(1)
		return nil
	}
	r.c.smu.Lock()
	r.c.speaking = true
	r.c.speakCancel = func() {}
	r.c.smu.Unlock()
	r.say("u", "Hey Mirrin, what's the time?")
	r.run()
	r.event("wake 0.95")
	r.event("utterance u (wake 1s)")
	if got := r.turn(); got != "what's the time?" {
		t.Fatalf("turn = %q", got)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("notification barge-in called the cancellation hook %d times", got)
	}
}
