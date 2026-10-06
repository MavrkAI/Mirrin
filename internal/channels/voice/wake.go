package voice

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed assets/wake_helper.py
var wakeScript []byte

// wakeEvent is one line from the helper.
type wakeEvent struct {
	kind     string // wake | wakeclip | utterance | silence | near | interrupt | mute | unmute | err
	arg      string
	followup bool // produced by a "listen" request rather than the wake word
	bare     bool // a wake utterance with nothing said after the wake phrase
	gen      int  // which run of the helper sent it
}

// wakeHelper runs the on-device wake-word detector.
type wakeHelper struct {
	dir      string
	model    string
	thresh   float64
	talkOver string
	out      string
	maxSec   int
	follow   int

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	gen    int // bumped on every start
	events chan wakeEvent
	// onWake fires immediately on detection (used for barge-in).
	onWake func()

	// Test seams: nil means the real helper process.
	startFn func(ctx context.Context) error
	sendFn  func(cmd string)
}

// parseWakeEvent reads one line of the helper's protocol.
func parseWakeEvent(line string, gen int) wakeEvent {
	kind, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
	ev := wakeEvent{kind: kind, arg: arg, gen: gen}
	if kind == "utterance" || kind == "silence" {
		if path, extra, ok := strings.Cut(arg, " "); ok {
			ev.arg = path
			ev.followup = strings.Contains(extra, "followup")
			ev.bare = strings.Contains(extra, "bare")
		}
	}
	return ev
}

func wakeReady(dir string) bool {
	for _, f := range []string{"wake_helper.py", filepath.Join("venv", pyBin())} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return false
		}
	}
	out, err := exec.Command(filepath.Join(dir, "venv", pyBin()), "-c", "import openwakeword, numpy").CombinedOutput()
	return err == nil && !strings.Contains(string(out), "Error")
}

func newWakeHelper(dir, model string, thresh float64, dataDir string, maxSec, follow int) *wakeHelper {
	model = WakeModelPath(dir, model)
	if thresh <= 0 {
		thresh = 0.5
	}
	return &wakeHelper{dir: dir, model: model, thresh: thresh, out: filepath.Join(dataDir, "voice-in.wav"), maxSec: maxSec, follow: follow, events: make(chan wakeEvent, 8)}
}

func (w *wakeHelper) start(ctx context.Context) error {
	if w.startFn != nil {
		w.mu.Lock()
		w.gen++
		w.mu.Unlock()
		return w.startFn(ctx)
	}
	// The helper ships inside the binary; keep the installed copy current so
	// fixes arrive with an upgrade, not only with `voice setup`.
	if p := filepath.Join(w.dir, "wake_helper.py"); !bytes.Equal(mustRead(p), wakeScript) {
		_ = os.WriteFile(p, wakeScript, 0o644)
	}
	cmd := exec.CommandContext(ctx, filepath.Join(w.dir, "venv", pyBin()), filepath.Join(w.dir, "wake_helper.py"))
	ratio := map[string]string{"off": "0", "low": "2.2", "normal": "1.6", "high": "1.3"}[w.talkOver]
	if ratio == "" {
		ratio = "1.6"
	}
	cmd.Env = append(os.Environ(),
		"TALKOVER_RATIO="+ratio,
		"WAKE_MODEL="+w.model,
		fmt.Sprintf("WAKE_THRESHOLD=%.2f", w.thresh),
		"WAKE_OUT="+w.out,
		"WAKE_CLIP="+strings.TrimSuffix(w.out, ".wav")+"-wake.wav",
		fmt.Sprintf("MAX_SECONDS=%d", w.maxSec),
		fmt.Sprintf("FOLLOWUP_SECONDS=%d", w.follow),
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// Keep the helper's stderr for diagnosis (model warnings, crashes).
	if logf, lerr := os.OpenFile(filepath.Join(filepath.Dir(w.out), "wake-helper.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); lerr == nil {
		cmd.Stderr = logf
		defer logf.Close() // the child has its own copy
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	w.mu.Lock()
	w.gen++
	gen := w.gen
	w.cmd, w.stdin = cmd, stdin
	w.mu.Unlock()
	sc := bufio.NewScanner(stdout)
	ready := make(chan error, 1)
	go func() {
		first := true
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if first {
				first = false
				if line == "ready" {
					ready <- nil
					continue
				}
				ready <- errors.New("wake helper: " + line)
				return
			}
			ev := parseWakeEvent(line, gen)
			if ev.kind == "wake" && w.onWake != nil {
				w.onWake()
			}
			select {
			case w.events <- ev:
			case <-ctx.Done():
				return
			}
		}
		if first {
			ready <- errors.New("wake helper stopped before it was ready (see wake-helper.log)")
		}
		select {
		case w.events <- wakeEvent{kind: "err", arg: "wake helper exited", gen: gen}:
		case <-ctx.Done():
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			_ = cmd.Process.Kill()
		}
		return err
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		return errors.New("wake helper timed out")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// current reports whether an event came from the helper now running.
func (w *wakeHelper) current(ev wakeEvent) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return ev.gen == w.gen
}

func (w *wakeHelper) send(cmd string) {
	if w.sendFn != nil {
		w.sendFn(cmd)
		return
	}
	w.mu.Lock()
	stdin := w.stdin
	w.mu.Unlock()
	if stdin != nil {
		_, _ = io.WriteString(stdin, cmd+"\n")
	}
}

func (w *wakeHelper) stop() {
	w.mu.Lock()
	cmd, stdin := w.cmd, w.stdin
	w.cmd, w.stdin = nil, nil
	w.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

// SetupWake installs openWakeWord into the same environment as Kokoro, and
// the wake-word model this build carries, if any (wakemodel.go).
func SetupWake(ctx context.Context, dir string, w io.Writer) error {
	if err := os.WriteFile(filepath.Join(dir, "wake_helper.py"), wakeScript, 0o644); err != nil {
		return err
	}
	py := filepath.Join(dir, "venv", pyBin())
	if _, err := os.Stat(py); err != nil {
		return errors.New("it needs the Python environment the Kokoro step makes, and that step didn't finish")
	}
	fmt.Fprintln(w, "Installing openWakeWord…")
	var install *exec.Cmd
	if _, err := exec.LookPath("uv"); err == nil {
		install = exec.CommandContext(ctx, "uv", "pip", "install", "--python", py, "-q", "openwakeword")
	} else {
		install = exec.CommandContext(ctx, py, "-m", "pip", "install", "-q", "openwakeword")
	}
	if out, err := install.CombinedOutput(); err != nil {
		return fmt.Errorf("couldn't install openWakeWord; check the internet connection (%v)\n%s", err, out)
	}
	// Download only the shared models every wake-word model runs on
	// (melspectrogram, embedding, VAD): the name matches none of
	// openWakeWord's example models, which are CC BY-NC-SA and unused here.
	dl := exec.CommandContext(ctx, py, "-c", "from openwakeword import utils; utils.download_models(model_names=['feature-models-only'])")
	if out, err := dl.CombinedOutput(); err != nil {
		return fmt.Errorf("couldn't download the wake-word feature models (%v)\n%s", err, out)
	}
	installed, err := installWakeModel(dir)
	if err != nil {
		return err
	}
	// How the name is heard is RunSetup's to say: it knows the wake word
	// and the model.
	if installed {
		fmt.Fprintln(w, "openWakeWord is ready, with the \"Hey Maverick\" model from before the default persona was Mirrin.")
	} else {
		fmt.Fprintln(w, "openWakeWord is ready.")
	}
	return nil
}
