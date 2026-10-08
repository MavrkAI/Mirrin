package voice

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// lockedBuffer is an out log the wake loop can write while a test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// sentCommands is what the wake loop told the helper, in order.
func (r *rig) sentCommands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sent...)
}

func lastListen(cmds []string) string {
	for i := len(cmds) - 1; i >= 0; i-- {
		if strings.HasPrefix(cmds[i], "listen") {
			return cmds[i]
		}
	}
	return ""
}

// The twin ended a reply with "Want me to open the search in the browser
// and grab a screenshot instead?" and the follow-up window closed after
// six seconds, before the owner had answered: the answer was lost. After
// a question the window now stays open at least 12 seconds, and the out
// log says how long it waited when nothing came.
func TestFollowupAfterAQuestionListensLonger(t *testing.T) {
	r := newRig(t, nil) // followup_seconds 2
	out := &lockedBuffer{}
	r.c.out = out
	r.onTurn = func(string) {
		r.c.lastSaid.Store("Nothing came up. Want me to open the search in the browser and grab a screenshot instead?")
	}
	r.run()
	r.say("u1", "Hey Mirrin, find the ferry times")
	r.event("wake 0.9")
	r.event("utterance u1 (wake 1.3s)")
	r.turn()
	r.eventually("a follow-up window", func() bool { return r.listens() == 1 })
	if got := lastListen(r.sentCommands()); got != "listen 12" {
		t.Fatalf("after a question the helper was sent %q, want %q", got, "listen 12")
	}
	r.event("silence - (followup)")
	r.eventually("the window to close", func() bool { return strings.Contains(out.String(), "(follow-up: nothing heard in 12s)") })
}

// After a plain answer the window is the configured length, sent as a
// bare "listen" that the helper has always understood.
func TestFollowupAfterAStatementKeepsTheConfiguredWindow(t *testing.T) {
	r := newRig(t, nil)
	out := &lockedBuffer{}
	r.c.out = out
	r.onTurn = func(string) { r.c.lastSaid.Store("It's half past two.") }
	r.run()
	r.say("u1", "Hey Mirrin, what's the time")
	r.event("wake 0.9")
	r.event("utterance u1 (wake 1.2s)")
	r.turn()
	r.eventually("a follow-up window", func() bool { return r.listens() == 1 })
	if got := lastListen(r.sentCommands()); got != "listen" {
		t.Fatalf("sent %q, want a plain listen", got)
	}
	r.event("silence - (followup)")
	r.eventually("the window to close", func() bool { return strings.Contains(out.String(), "(follow-up: nothing heard in 2s)") })
}

func TestFollowupWindowLengths(t *testing.T) {
	for _, tc := range []struct {
		configured int
		said       string
		pending    bool
		want       int
		cmd        string
	}{
		{8, "Done.", false, 8, "listen"},
		{8, "Shall I book it?", false, 12, "listen 12"},
		{8, "Want me to send it", false, 12, "listen 12"},
		{8, "Done.", true, 12, "listen 12"}, // an approval is waiting
		{20, "Shall I book it?", false, 20, "listen"},
	} {
		c := New(config.Voice{FollowupSeconds: tc.configured, MaxSeconds: 30}, "Mirrin", t.TempDir())
		c.lastSaid.Store(tc.said)
		c.Pending = func() bool { return tc.pending }
		got := c.followupSeconds()
		if got != tc.want || c.listenCommand(got) != tc.cmd {
			t.Errorf("%d after %q (pending %v): %ds %q, want %ds %q", tc.configured, tc.said, tc.pending, got, c.listenCommand(got), tc.want, tc.cmd)
		}
		// The loop's own timeout must outlast the helper's window plus the
		// longest utterance that could start at its last moment.
		if wait := c.followupWait(got); wait <= time.Duration(got+30)*time.Second {
			t.Errorf("waits %v for a %ds window", wait, got)
		}
	}
}

// An install that never set followup_seconds now listens eight seconds.
func TestFollowupDefaultsToEightSeconds(t *testing.T) {
	if c := New(config.Voice{}, "Mirrin", t.TempDir()); c.cfg.FollowupSeconds != 8 {
		t.Fatalf("default follow-up %ds", c.cfg.FollowupSeconds)
	}
	if c := New(config.Voice{FollowupSeconds: 5}, "Mirrin", t.TempDir()); c.cfg.FollowupSeconds != 5 {
		t.Fatalf("an explicit setting changed to %ds", c.cfg.FollowupSeconds)
	}
}

// The wake helper honours "listen 12": it waits twelve seconds of audio for
// speech to start, and a click or breath part way through keeps what is
// left of that window, not of FOLLOWUP_SECONDS.
func TestWakeHelperHonoursTheListenWindow(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		if py, err = exec.LookPath("python"); err != nil {
			t.Skip("no Python")
		}
	}
	dir := t.TempDir()
	stubs := filepath.Join(dir, "stubs")
	for name, body := range map[string]string{
		"numpy.py":                 "int16 = 'int16'\ndef frombuffer(buf, dtype=None):\n    return buf\n",
		"soundfile.py":             "",
		"openwakeword/__init__.py": "",
		"openwakeword/model.py":    "class Model:\n    pass\n",
	} {
		p := filepath.Join(stubs, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "wake_helper.py"), wakeScript, 0o644); err != nil {
		t.Fatal(err)
	}
	script := `
import io, sys, types
sys.path.insert(0, sys.argv[1]); sys.path.insert(0, sys.argv[2])
import os
os.environ.pop("FOLLOWUP_SECONDS", None)
import wake_helper as wh
assert wh.FOLLOWUP_SECONDS == 8, wh.FOLLOWUP_SECONDS

# stdin: "listen 12" names the window; plain "listen" leaves the default.
for cmd, want in (("listen 12", 12.0), ("listen", None), ("listen soon", None), ("listen -3", None)):
    wh.state.update(listen=False, window="stale")
    wh.sys.stdin = io.StringIO(cmd + "\n")
    wh.os = types.SimpleNamespace(_exit=lambda code: None)
    wh.stdin_loop()
    assert wh.state["listen"] and wh.state["window"] == want, (cmd, wh.state)
wh.state.update(listen=False, window=None, speaking=False)

class Mic:
    """Silence, with an optional click (one loud chunk) after some chunks."""
    def __init__(self, click_at=None):
        self.n, self.click_at = 0, click_at
    def read(self):
        self.n += 1
        return b"T" if self.n == self.click_at else b"."

wh.rms = lambda chunk: 0.5 if chunk == b"T" else 0.0
step = wh.CHUNK / wh.RATE

mic = Mic()
assert wh.capture(mic, [], wait_for_speech=True, window=12) == ""
assert abs(mic.n * step - 12) < 0.2, mic.n * step

# A click 6 s in, then quiet: the window still ends at 12 s, not 6+1.3+8.
mic = Mic(click_at=int(6 / step))
assert wh.capture(mic, [], wait_for_speech=True, window=12) == ""
assert abs(mic.n * step - 12) < 0.2, mic.n * step

mic = Mic()
assert wh.capture(mic, [], wait_for_speech=True) == ""
assert abs(mic.n * step - 8) < 0.2, mic.n * step
print("ok")
`
	cmd := exec.Command(py, "-c", script, stubs, dir)
	cmd.Env = append(os.Environ(), "FOLLOWUP_SECONDS=")
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("%v\n%s", err, out)
	}
}
