package voice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/transcribe"
)

// Hearing is whisper.cpp. whisper-cli loads the model from disk for every
// utterance, which is most of the wait on slower machines. whisper-server
// (built alongside it, and installed by Homebrew) keeps the model loaded, so
// while the twin listens it runs one on a free loopback port and posts each
// utterance to it. The server never makes anyone wait: until it is ready,
// or if anything goes wrong with it, an utterance goes to whisper-cli, and
// the server is tried again a little later. A server that stops answering
// gets a short deadline of its own, so it can never hold an utterance for
// long: whisper-cli hears that one, and the server is rested.
//
// The server has no login, so its routes sit under a random path only this
// channel knows: another program (or a web page probing loopback ports)
// can't swap its model out or keep it busy.

// whisperServer is a whisper-server process kept for the life of a channel.
type whisperServer struct {
	bin, model, lang string
	pidFile          string // where the process id is kept, so a crash never leaves one running
	logFile          string
	startTimeout     time.Duration
	requestTimeout   time.Duration // how long a transcription may take, plus the clip's length
	retryAfter       time.Duration
	path             string // the random prefix of every route (--request-path)

	mu        sync.Mutex
	life      context.Context // the channel's: the server ends with it
	cmd       *exec.Cmd
	base      string        // http://127.0.0.1:port/<path> once ready
	exited    chan struct{} // closed when the process ends
	starting  bool
	downUntil time.Time // after a failure, whisper-cli until then
	lastErr   error
}

// serverDisabled lets a user turn the server off (MIRRIN_WHISPER_SERVER=0),
// exported or saved in secrets.env, which an installed service reads, as
// MIRRIN_WHISPER_URL for voice notes is.
func serverDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(config.Secret("MIRRIN_WHISPER_SERVER"))) {
	case "0", "off", "false", "no":
		return true
	}
	return false
}

// findWhisperServer looks for whisper-server next to the configured
// whisper-cli, then on PATH.
func findWhisperServer(whisperBin string) string {
	name := "whisper-server"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	var dirs []string
	if strings.ContainsAny(whisperBin, `/\`) {
		dirs = append(dirs, filepath.Dir(whisperBin))
	} else if p, err := exec.LookPath(whisperBin); err == nil {
		dirs = append(dirs, filepath.Dir(p))
	}
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("whisper-server"); err == nil {
		return p
	}
	return ""
}

// serverSeq numbers this process's servers, for their pid files.
var serverSeq atomic.Int64

func newWhisperServer(bin, model, lang, dataDir string) *whisperServer {
	return &whisperServer{
		bin: bin, model: model, lang: lang,
		// Named for the process that owns it, so another twin process (the
		// menu bar's, a terminal session's) never ends a server still in use.
		pidFile:        filepath.Join(dataDir, fmt.Sprintf("whisper-server-%d-%d.pid", os.Getpid(), serverSeq.Add(1))),
		logFile:        filepath.Join(dataDir, "whisper-server.log"),
		startTimeout:   60 * time.Second,
		requestTimeout: 10 * time.Second,
		retryAfter:     time.Minute,
		path:           "/" + randomHex(16),
		life:           context.Background(),
	}
}

// randomHex is n random bytes in hex.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// warm starts the server in the background for the life of ctx, so the
// model is loaded before the first utterance; it stops when ctx ends.
func (s *whisperServer) warm(ctx context.Context) {
	s.mu.Lock()
	s.life = ctx
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		s.stop()
	}()
	s.ready()
}

// ready returns the server's address when it is up. Otherwise it starts
// one in the background (unless one is starting, or failed a moment ago)
// and reports false, so this utterance goes to whisper-cli rather than
// waiting on a model load.
func (s *whisperServer) ready() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readyLocked() {
		return s.base, true
	}
	if s.starting || time.Now().Before(s.downUntil) || s.life.Err() != nil {
		return "", false
	}
	s.stopLocked() // a process that died, if any
	s.starting = true
	go s.start(s.life)
	return "", false
}

// running reports whether the server is up and answering.
func (s *whisperServer) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readyLocked()
}

func (s *whisperServer) readyLocked() bool {
	if s.base == "" || s.exited == nil {
		return false
	}
	select {
	case <-s.exited:
		return false
	default:
		return true
	}
}

// start launches the process and waits for it to answer.
func (s *whisperServer) start(ctx context.Context) {
	base, err := s.launch(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starting = false
	if err != nil {
		s.lastErr = err
		s.downUntil = time.Now().Add(s.retryAfter)
		return
	}
	if ctx.Err() != nil {
		s.stopLocked()
		return
	}
	s.base, s.lastErr = base, nil
}

func (s *whisperServer) launch(ctx context.Context) (string, error) {
	reapStale(filepath.Dir(s.pidFile))
	port, err := freePort()
	if err != nil {
		return "", err
	}
	args := []string{"-m", s.model, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--request-path", s.path, "-nt"}
	if s.lang != "" {
		args = append(args, "-l", s.lang)
	}
	// Not CommandContext: stop() ends it, with the channel.
	cmd := exec.Command(s.bin, args...)
	if f, err := os.OpenFile(s.logFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		cmd.Stdout, cmd.Stderr = f, f
		defer f.Close() // the child has its own copy
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	_ = os.WriteFile(s.pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	s.mu.Lock()
	s.cmd, s.exited = cmd, exited
	s.mu.Unlock()
	base := "http://127.0.0.1:" + strconv.Itoa(port) + s.path
	deadline := time.NewTimer(s.startTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-exited:
			_ = os.Remove(s.pidFile)
			return "", fmt.Errorf("whisper-server stopped while loading the model (see %s)", s.logFile)
		case <-deadline.C:
			s.stop()
			return "", errors.New("whisper-server took too long to load the model")
		case <-ctx.Done():
			s.stop()
			return "", ctx.Err()
		case <-tick.C:
		}
		if serverReady(ctx, base) {
			return base, nil
		}
	}
}

// serverReady reports whether the server answers (it says 503 while the
// model is still loading).
func serverReady(ctx context.Context, base string) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode < 500
}

// errNotReady means the server is still loading (or resting after a
// failure): this utterance goes to whisper-cli.
var errNotReady = errors.New("whisper-server is not ready")

// transcribe posts a WAV and returns what the server heard, through
// internal/transcribe (the one way Mirrin talks to whisper.cpp).
func (s *whisperServer) transcribe(ctx context.Context, wav string, o transcribe.Options) (string, error) {
	base, ok := s.ready()
	if !ok {
		return "", errNotReady
	}
	st, err := os.Stat(wav)
	if err != nil {
		return "", err
	}
	// Its own deadline: a server that stopped answering must not hold the
	// utterance (or the wake events behind it) for the caller's whole budget.
	limit := s.requestTimeout + clipLength(int(st.Size()))
	sctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	text, err := transcribe.Post(sctx, base, wav, o)
	var answer *transcribe.AnswerError
	switch {
	case err == nil:
		return text, nil
	case errors.As(err, &answer), ctx.Err() != nil:
		return "", err // it couldn't hear this clip, or the caller gave up: the server did nothing wrong
	case sctx.Err() != nil:
		err = fmt.Errorf("whisper-server didn't answer within %s", limit.Round(time.Second))
	}
	s.failed(err)
	return "", err
}

// clipLength is how long a 16 kHz, 16-bit mono WAV of n bytes plays.
func clipLength(n int) time.Duration {
	return time.Duration(n) * time.Second / 32000
}

// failed stops a server that misbehaved and rests it for a while.
func (s *whisperServer) failed(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	s.lastErr = err
	s.downUntil = time.Now().Add(s.retryAfter)
}

func (s *whisperServer) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

func (s *whisperServer) stopLocked() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		if s.exited != nil {
			<-s.exited
		}
		_ = os.Remove(s.pidFile)
	}
	s.cmd, s.base = nil, ""
}

// reapStale ends the whisper-servers that twin processes which have since
// died (a crash, a kill) left running, so a restart loop never piles up
// loaded models. Each server's id is in whisper-server-<owner>-<n>.pid in
// dir; a server whose owner is alive is left alone.
func reapStale(dir string) {
	files, _ := filepath.Glob(filepath.Join(dir, "whisper-server-*.pid"))
	for _, f := range files {
		var owner, n int
		if _, err := fmt.Sscanf(filepath.Base(f), "whisper-server-%d-%d.pid", &owner, &n); err != nil {
			continue
		}
		if owner == os.Getpid() || alive(owner) {
			continue
		}
		b, err := os.ReadFile(f)
		_ = os.Remove(f)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid <= 1 || runtime.GOOS == "windows" {
			continue
		}
		out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
		if err != nil || !strings.Contains(string(out), "whisper-server") {
			continue // gone, or the id now belongs to something else
		}
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
}

// alive reports whether a process is running (true when it can't tell, so
// nothing is ended by mistake).
func alive(pid int) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	err := exec.Command("ps", "-p", strconv.Itoa(pid)).Run()
	var exit *exec.ExitError
	return err == nil || !errors.As(err, &exit)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// whisperPrompt biases whisper towards the names it will actually hear:
// the wake phrase, then the twin's name when the wake word isn't it.
func whisperPrompt(cfg config.Voice, name string) string {
	wake := titleWords(cfg.WakeWord)
	vocab := []string{"Hey " + wake}
	if name != "" && !strings.EqualFold(name, wake) {
		vocab = append(vocab, name)
	}
	return strings.Join(append(vocab, cfg.Vocabulary...), ", ") + "."
}

// hearingPrompt is whisperPrompt with what's being talked about now: the
// words in play (ContextWords: open tasks, the site on screen) and the
// twin's last words, so "Uniqlo" after it said "Uniqlo" isn't heard as
// "unique law". Whisper reads only the prompt's end, so it's kept short.
func (c *Channel) hearingPrompt() string {
	p := whisperPrompt(c.cfg, c.name)
	var extra []string
	if c.ContextWords != nil {
		extra = c.ContextWords()
	}
	if last, _ := c.lastSaid.Load().(string); strings.TrimSpace(last) != "" {
		if r := []rune(strings.TrimSpace(last)); len(r) > 240 {
			last = string(r[len(r)-240:])
		}
		extra = append(extra, strings.TrimSpace(last))
	}
	if len(extra) == 0 {
		return p
	}
	return p + " " + strings.Join(extra, ". ")
}

// hearing is how the voice channel asks internal/transcribe for text. The
// language follows transcribe.SpokenLanguage, as chat voice notes do
// (daemon/media.go): unset means English.
func hearing(cfg config.Voice, prompt string) transcribe.Options {
	return transcribe.Options{Bin: cfg.WhisperBin, Model: cfg.WhisperModel, Language: transcribe.SpokenLanguage(cfg.Language), Prompt: prompt}
}
