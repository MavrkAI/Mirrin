// Package transcribe turns a voice note into text on this computer with
// whisper.cpp. A whisper server that is already running is used first (its
// model stays loaded, so it answers fastest), then whisper-cli with the
// model voice setup installed. Audio in any format is converted to 16 kHz
// mono WAV with ffmpeg or sox first. A server that fails is set aside for a
// while and whisper-cli takes over. Nothing leaves the machine, and the
// only files written are temporary ones in TempDir, removed afterwards.
//
// It is Mirrin's one way to whisper.cpp: the voice channel keeps its own
// whisper-server warm for the microphone, but posts to it with Post and
// falls back to CLI, as voice notes from chat apps do.
package transcribe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Options says where the speech-to-text pieces are.
type Options struct {
	// Server is a whisper.cpp server (whisper-server) to use while it runs.
	// Empty tries the one on its default port (DefaultServer); "off" never
	// looks for one.
	Server string
	// Bin is whisper-cli; Model is the ggml model file it loads.
	Bin, Model string
	// Language is the spoken language ("" to detect it, unless the model
	// only knows English).
	Language string
	// Prompt lists names and words to expect, so they are spelled right.
	Prompt string
	// TempDir is where audio is converted (the data folder's, so a voice
	// note never lands anywhere else). It must be set.
	TempDir string
	// FFmpeg and Sox convert audio ("" looks up ffmpeg and sox).
	FFmpeg, Sox string
	// MaxSeconds stops after this much audio (0: all of it). Audio that is
	// already 16 kHz mono WAV is taken whole.
	MaxSeconds int
}

// DefaultServer is where whisper-server listens unless told otherwise.
const DefaultServer = "http://127.0.0.1:8080"

// SpokenLanguage is the language to hear, from channels.voice.language: as
// set, or English when unset, as whisper itself assumes (voice setup on a
// computer set to another language writes "auto"). The microphone and chat
// voice notes both go by it, so one setting means one thing.
func SpokenLanguage(setting string) string {
	if s := strings.TrimSpace(setting); s != "" {
		return s
	}
	return "en"
}

// noteSlot lets one voice note be transcribed at a time: each whisper-cli
// run loads its own copy of the model, and a burst of voice notes (or one
// heard ahead of its turn) must not load several at once. The microphone
// doesn't wait on it: it posts to its own server or runs CLI.
var noteSlot = make(chan struct{}, 1)

// UnavailableError means there is nothing on this computer to transcribe
// with: no whisper server running, and no whisper-cli with a model.
type UnavailableError struct {
	// Missing is what to install, in words ("whisper-cli", "the whisper model").
	Missing string
}

func (e *UnavailableError) Error() string {
	return "speech-to-text isn't set up on this computer (" + e.Missing + " is missing)"
}

// ErrNoConverter means the audio needs converting and nothing installed can
// read it: neither ffmpeg nor sox, or only sox for audio it can't open.
var ErrNoConverter = errors.New("ffmpeg or sox is needed to read this audio")

// Check reports whether a voice note could be transcribed now, and if not,
// what is missing.
func Check(ctx context.Context, o Options) error {
	_, err := pick(ctx, o)
	return err
}

// Transcribe returns the words spoken in audio, whose MIME type is mime
// (audio/ogg for WhatsApp and Telegram voice notes). An empty result means
// no speech was heard.
func Transcribe(ctx context.Context, audio []byte, mime string, o Options) (string, error) {
	if o.TempDir == "" {
		return "", errors.New("transcribe: no folder for temporary audio")
	}
	eng, err := pick(ctx, o)
	if err != nil {
		return "", err
	}
	select {
	case noteSlot <- struct{}{}:
		defer func() { <-noteSlot }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if err := os.MkdirAll(o.TempDir, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(o.TempDir, "voice-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	wav, err := toWAV(ctx, audio, mime, dir, o)
	if err != nil {
		return "", err
	}
	text, err := eng.transcribe(ctx, wav, o)
	if srv, ok := eng.(server); ok && err != nil && ctx.Err() == nil {
		// Something answers on the server's port but can't transcribe (a
		// whisper server that fails, or another program): leave it alone for
		// a while and use whisper-cli, when it is installed. A server that
		// answered it couldn't hear this clip works, as the voice channel
		// also takes it: only this clip goes to whisper-cli.
		var answer *AnswerError
		if !errors.As(err, &answer) {
			setAside(srv.url)
		}
		if cli, cerr := pickCLI(o); cerr == nil {
			text, err = cli.transcribe(ctx, wav, o)
		}
	}
	if err != nil {
		return "", err
	}
	return Clean(text), nil
}

// engine is one way of turning a WAV file into text.
type engine interface {
	transcribe(ctx context.Context, wav string, o Options) (string, error)
}

// pick chooses the engine: a running whisper server, else whisper-cli.
func pick(ctx context.Context, o Options) (engine, error) {
	if url := serverURL(o.Server); url != "" && serverUp(ctx, url) {
		return server{url: url}, nil
	}
	return pickCLI(o)
}

// pickCLI is whisper-cli with its model, or what is missing.
func pickCLI(o Options) (engine, error) {
	bin := o.Bin
	if bin == "" {
		bin = "whisper-cli"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, &UnavailableError{Missing: "whisper-cli"}
	}
	if o.Model == "" {
		return nil, &UnavailableError{Missing: "the whisper model"}
	}
	if st, err := os.Stat(o.Model); err != nil || st.IsDir() {
		return nil, &UnavailableError{Missing: "the whisper model"}
	}
	return cli{bin: path}, nil
}

// CLI runs whisper-cli once on a 16 kHz mono WAV (o.Bin, or whisper-cli on
// the PATH, with o.Model) and returns what it heard. whisper-cli itself says
// when the model is missing; Check and Transcribe look first.
func CLI(ctx context.Context, wav string, o Options) (string, error) {
	text, err := cli{bin: or(o.Bin, "whisper-cli")}.transcribe(ctx, wav, o)
	if err != nil {
		return "", err
	}
	return Clean(text), nil
}

// cli runs whisper-cli once per voice note.
type cli struct{ bin string }

func (c cli) transcribe(ctx context.Context, wav string, o Options) (string, error) {
	args := []string{"-m", o.Model, "-f", wav, "-nt", "-np"}
	lang := o.Language
	if lang == "" && !englishOnly(o.Model) {
		lang = "auto" // whisper-cli assumes English otherwise
	}
	if lang != "" {
		args = append(args, "-l", lang)
	}
	if o.Prompt != "" {
		args = append(args, "--prompt", o.Prompt)
	}
	cmd := exec.CommandContext(ctx, c.bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("whisper-cli: %v: %s", err, lastLine(stderr.String()))
	}
	return string(out), nil
}

// englishOnly reports whether model is one of whisper's English-only models
// ("ggml-small.en.bin").
func englishOnly(model string) bool {
	parts := strings.FieldsFunc(strings.ToLower(filepath.Base(model)), func(r rune) bool { return r == '.' || r == '-' || r == '_' })
	return slices.Contains(parts, "en")
}

// audioFormats are the only containers ffmpeg may open a voice note as, so
// a file that is really a playlist or a list of other files is refused
// rather than followed.
const audioFormats = "ogg,mp3,mov,mp4,m4a,3gp,3g2,mj2,wav,webm,matroska,aac,amr,amrnb,amrwb,flac"

// soxReads are the kinds of audio sox can convert; anything else needs ffmpeg.
var soxReads = []string{".opus", ".wav", ".mp3", ".flac"}

// toWAV writes audio as a 16 kHz mono 16-bit WAV in dir and returns its path.
func toWAV(ctx context.Context, audio []byte, mime, dir string, o Options) (string, error) {
	out := filepath.Join(dir, "voice.wav")
	if isWAV16kMono(audio) {
		return out, os.WriteFile(out, audio, 0o600)
	}
	ext := extension(mime)
	in := filepath.Join(dir, "voice"+ext)
	if err := os.WriteFile(in, audio, 0o600); err != nil {
		return "", err
	}
	ffmpeg, sox := or(o.FFmpeg, "ffmpeg"), or(o.Sox, "sox")
	var cmd *exec.Cmd
	if path, err := exec.LookPath(ffmpeg); err == nil {
		args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
			"-format_whitelist", audioFormats, "-protocol_whitelist", "file", "-i", in}
		if o.MaxSeconds > 0 {
			args = append(args, "-t", fmt.Sprint(o.MaxSeconds))
		}
		cmd = exec.CommandContext(ctx, path, append(args, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", out)...)
	} else if path, err := exec.LookPath(sox); err == nil && slices.Contains(soxReads, ext) {
		args := []string{"-q", in, "-r", "16000", "-c", "1", "-b", "16", out}
		if o.MaxSeconds > 0 {
			args = append(args, "trim", "0", fmt.Sprint(o.MaxSeconds))
		}
		cmd = exec.CommandContext(ctx, path, args...)
	} else {
		return "", ErrNoConverter
	}
	if msg, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("couldn't read the audio: %s", or(lastLine(string(msg)), err.Error()))
	}
	if st, err := os.Stat(out); err != nil || st.Size() <= 44 {
		return "", errors.New("couldn't read the audio: nothing came out of the converter")
	}
	return out, nil
}

// extension names the input file so the converter knows what it holds.
// WhatsApp and Telegram voice notes are Opus in Ogg; sox reads those only
// from a .opus file.
func extension(mime string) string {
	m := strings.ToLower(mime)
	switch {
	case strings.Contains(m, "ogg"), strings.Contains(m, "opus"):
		return ".opus"
	case strings.Contains(m, "mpeg"), strings.Contains(m, "mp3"):
		return ".mp3"
	case strings.Contains(m, "mp4"), strings.Contains(m, "m4a"), strings.Contains(m, "aac"):
		return ".m4a"
	case strings.Contains(m, "wav"):
		return ".wav"
	case strings.Contains(m, "webm"):
		return ".webm"
	case strings.Contains(m, "amr"):
		return ".amr"
	case strings.Contains(m, "flac"):
		return ".flac"
	}
	return ".audio"
}

// isWAV16kMono reports whether audio is already what whisper reads.
func isWAV16kMono(b []byte) bool {
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" || string(b[12:16]) != "fmt " {
		return false
	}
	format := binary.LittleEndian.Uint16(b[20:22])
	channels := binary.LittleEndian.Uint16(b[22:24])
	rate := binary.LittleEndian.Uint32(b[24:28])
	bits := binary.LittleEndian.Uint16(b[34:36])
	return format == 1 && channels == 1 && rate == 16000 && bits == 16
}

// reNonSpeech matches whisper's markers for what isn't speech: [BLANK_AUDIO],
// [Music], (laughs).
var reNonSpeech = regexp.MustCompile(`\[[^\]]*\]|\([^)]*\)`)

// Clean drops non-speech markers and joins whisper's lines into one text.
func Clean(s string) string {
	return strings.Join(strings.Fields(reNonSpeech.ReplaceAllString(s, " ")), " ")
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Timeout is how long transcribing seconds of audio may take: whisper runs
// many times faster than real time, but a first run loads the model.
func Timeout(seconds int) time.Duration {
	if seconds <= 0 {
		seconds = 120
	}
	return time.Minute + time.Duration(seconds)*time.Second
}
