package voice

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed assets/kokoro_tts.py
var kokoroScript []byte

// kokoro is an offline neural voice (hexgrad/Kokoro-82M via kokoro-onnx). A
// helper Python process keeps the model loaded so each sentence takes well
// under a second.
type kokoro struct {
	dir   string
	voice string
	speed float64
	pitch int    // cents, applied to every clip (sox); 0 for none
	lang  string // the language it speaks ("" for English), sent as LANG_CODE

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
}

// KokoroDir is the default install location.
func KokoroDir(home string) string { return filepath.Join(home, "tts") }

func kokoroReady(dir string) bool {
	for _, f := range []string{"kokoro-v1.0.onnx", "voices-v1.0.bin", "kokoro_tts.py", filepath.Join("venv", pyBin())} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return false
		}
	}
	return true
}

func mustRead(p string) []byte { b, _ := os.ReadFile(p); return b }

func pyBin() string {
	if runtime.GOOS == "windows" {
		return filepath.Join("Scripts", "python.exe")
	}
	return filepath.Join("bin", "python")
}

func newKokoro(dir, voice string, speed float64) *kokoro {
	if voice == "" || !strings.Contains(voice, "_") {
		voice = "bm_george" // British male; also try bm_lewis, bf_emma, am_michael, af_heart
	}
	if speed <= 0 {
		speed = 1.0
	}
	return &kokoro{dir: dir, voice: voice, speed: speed}
}

func (k *kokoro) start() error {
	if k.cmd != nil {
		return nil
	}
	// The helper ships inside the binary; keep the installed copy current so
	// voice improvements arrive with an upgrade, not only with `voice setup`.
	if p := filepath.Join(k.dir, "kokoro_tts.py"); !bytes.Equal(mustRead(p), kokoroScript) {
		_ = os.WriteFile(p, kokoroScript, 0o644)
	}
	cmd := exec.Command(filepath.Join(k.dir, "venv", pyBin()), filepath.Join(k.dir, "kokoro_tts.py"), "--server")
	cmd.Env = kokoroEnv(os.Environ(), k.voice, k.lang)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(stdout)
	ready := make(chan error, 1)
	go func() {
		if sc.Scan() && strings.TrimSpace(sc.Text()) == "ready" {
			ready <- nil
			return
		}
		ready <- errors.New("kokoro helper did not start")
	}()
	select {
	case err := <-ready:
		if err != nil {
			_ = cmd.Process.Kill()
			return err
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		return errors.New("kokoro helper timed out loading the model")
	}
	k.cmd, k.stdin, k.stdout = cmd, stdin, sc
	return nil
}

func (k *kokoro) stop() {
	if k.cmd != nil {
		_ = k.stdin.Close()
		_ = k.cmd.Process.Kill()
		_ = k.cmd.Wait()
		k.cmd = nil
	}
}

// synthesize writes a WAV for text to out.
func (k *kokoro) synthesize(ctx context.Context, text, out string) error {
	return k.synthesizeSpeed(ctx, text, out, k.speed)
}

// synthesizeSpeed is synthesize at an explicit speed (short acknowledgements are brisker).
func (k *kokoro) synthesizeSpeed(ctx context.Context, text, out string, speed float64) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := k.start(); err != nil {
			return err
		}
		req, _ := json.Marshal(map[string]any{"text": text, "out": out, "voice": k.voice, "speed": speed})
		if _, err := k.stdin.Write(append(req, '\n')); err != nil {
			lastErr = err
			k.stop()
			continue
		}
		done := make(chan error, 1)
		go func() {
			if !k.stdout.Scan() {
				done <- errors.New("kokoro helper exited")
				return
			}
			line := strings.TrimSpace(k.stdout.Text())
			if strings.HasPrefix(line, "ok ") {
				done <- nil
			} else {
				done <- errors.New(strings.TrimPrefix(line, "err "))
			}
		}()
		select {
		case err := <-done:
			if err == nil {
				return k.repitch(ctx, out)
			}
			lastErr = err
			if strings.Contains(err.Error(), "exited") {
				k.stop()
				continue
			}
			return err
		case <-ctx.Done():
			k.stop()
			return ctx.Err()
		}
	}
	return lastErr
}

// repitch shifts a written clip by k.pitch cents, in place. Without sox the
// clip keeps its own pitch: the character's voice, a little less so.
func (k *kokoro) repitch(ctx context.Context, path string) error {
	if k.pitch == 0 {
		return nil
	}
	tmp := path + ".pitch.wav"
	if err := exec.CommandContext(ctx, "sox", "-q", path, tmp, "pitch", strconv.Itoa(k.pitch)).Run(); err != nil {
		_ = os.Remove(tmp)
		return nil
	}
	return os.Rename(tmp, path)
}

// ---- setup ---------------------------------------------------------------

// SetupKokoro installs the offline voice into dir: a Python venv with
// kokoro-onnx, the model files and the helper script. It prefers uv, falling
// back to python3 -m venv. Progress goes to w.
func SetupKokoro(ctx context.Context, dir string, w io.Writer) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "kokoro_tts.py"), kokoroScript, 0o644); err != nil {
		return err
	}
	venv := filepath.Join(dir, "venv")
	py := filepath.Join(venv, pyBin())
	if _, err := os.Stat(py); err != nil {
		fmt.Fprintln(w, "Making a Python environment for it…")
		if _, err := exec.LookPath("uv"); err == nil {
			if out, err := exec.CommandContext(ctx, "uv", "venv", "--python", "3.12", venv).CombinedOutput(); err != nil {
				return fmt.Errorf("couldn't make a Python environment with uv (%v)\n%s", err, out)
			}
		} else {
			python := "python3"
			if runtime.GOOS == "windows" {
				python = "python"
			}
			if out, err := exec.CommandContext(ctx, python, "-m", "venv", venv).CombinedOutput(); err != nil {
				return fmt.Errorf("couldn't make a Python environment: install Python 3.10–3.13, or uv (%v)\n%s", err, out)
			}
		}
	}
	fmt.Fprintln(w, "Installing Kokoro (a minute or two)…")
	var install *exec.Cmd
	if _, err := exec.LookPath("uv"); err == nil {
		install = exec.CommandContext(ctx, "uv", "pip", "install", "--python", py, "-q", "kokoro-onnx", "soundfile")
	} else {
		install = exec.CommandContext(ctx, py, "-m", "pip", "install", "-q", "kokoro-onnx", "soundfile")
	}
	if out, err := install.CombinedOutput(); err != nil {
		return fmt.Errorf("couldn't install kokoro-onnx; check the internet connection (%v)\n%s", err, out)
	}
	base := "https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/"
	for _, f := range []string{"kokoro-v1.0.onnx", "voices-v1.0.bin"} {
		if err := download(ctx, base+f, filepath.Join(dir, f), w); err != nil {
			return err
		}
	}
	if out, err := exec.CommandContext(ctx, py, "-c", "import kokoro_onnx, soundfile").CombinedOutput(); err != nil {
		return fmt.Errorf("Kokoro installed but won't load (%v)\n%s", err, out)
	}
	fmt.Fprintln(w, "Kokoro is ready.")
	return nil
}

// DownloadWhisperModel fetches a ggml model (e.g. "small.en") into dir,
// checked against its published checksum (setup.go).
func DownloadWhisperModel(ctx context.Context, dir, name string, w io.Writer) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "ggml-"+name+".bin")
	url := "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-" + name + ".bin"
	return path, download(ctx, url, path, w)
}
