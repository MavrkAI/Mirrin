// Package voice lets you talk to your twin and hear it answer, out loud and
// hands-free.
//
// Ears: an on-device wake-word detector (openWakeWord, with a model for the
// name) that captures the utterance after the wake word; whisper.cpp then
// transcribes it locally. Without the detector or a model for the name (the
// public build ships none: wakemodel.go), a recorder captures speech on a
// silence gate and the transcript is matched against the wake word instead.
// Mouth: ElevenLabs, Kokoro (offline neural), the OS voice, or any command.
//
// Modes: "push" waits for Enter, then listens. "wake" listens continuously.
// After answering it keeps listening briefly for a follow-up, and saying the
// wake word while it is talking interrupts it (barge-in).
package voice

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/transcribe"
)

// channelsStream aliases the channel stream type for the speech file.
type channelsStream = channels.Stream

// Channel is the microphone and speaker transport.
type Channel struct {
	// listenNow asks the wake loop for a listening window without the wake
	// word (a click on the orb); wakeLive says the loop is running.
	listenNow chan struct{}
	// ContextWords are the names in play right now (open tasks' titles,
	// the site on screen), to help whisper hear them; nil for none.
	ContextWords func() []string
	wakeLive     atomic.Bool

	cfg      config.Voice
	name     string
	dataDir  string
	out      io.Writer
	in       io.Reader
	speakMu  chan struct{}
	eleven   *elevenLabs
	kokoro   *kokoro
	macVoice string
	wake     *wakeHelper

	// Barge-in state: the current utterance's cancel func and player process.
	smu         sync.Mutex
	speakCancel context.CancelFunc
	player      *exec.Cmd
	paused      bool
	speaking    bool

	// OnNoAudio is called with the count of consecutive empty captures, so a
	// host can tell the user the microphone is blocked (typically a permission
	// granted while the process was already running).
	OnNoAudio func(consecutive int)
	// startLevel is the sound level (float64 bits, 0..1) a recording starts
	// at, from the room's measured quiet (micBlocked); zero until measured.
	startLevel atomic.Uint64

	tmu     sync.Mutex
	pending timing          // stages of the utterance now being answered
	told    map[string]bool // trouble already reported (trouble.go)

	acks         *acks
	lastExchange time.Time
	followupOpen atomic.Bool  // a follow-up window is open: unconfirmed speech is the reply
	lastSaid     atomic.Value // the last reply text, to recognise our own echo
	saying       atomic.Value // what the speech now playing was given so far (a reply still streaming)

	// OnState reports presence changes: listening, thinking, speaking, idle.
	OnState func(state string)

	// Pending reports whether an approval is waiting on the owner, so a bare
	// "yes" heard in the follow-up window is taken as the answer to it.
	Pending func() bool

	// ApprovalPrompt identifies the newest dangerous request and its screen instruction.
	ApprovalPrompt func() (int64, string)
	// ApprovalRevision is a cheap generation counter, advanced by approval
	// events even for tools without a spoken caption.
	ApprovalRevision func() uint64

	// OnProblem tells the owner, once, that part of the voice stopped
	// working (a desktop notification, the presence screen).
	OnProblem func(msg string)

	// OnInterrupt stops owner work without waiting for the inline handler,
	// and returns a summary function to call once that handler has finished.
	OnInterrupt func(text string) func() string
	// OnStop is the legacy cancellation hook, used when OnInterrupt is nil.
	OnStop func()

	// Screen puts long answers on the presence screen (spoken.go).
	Screen ScreenHooks
	shown  shownText

	stopMu     sync.Mutex
	stopNotice func() string // guarded by stopMu
	handling   atomic.Bool   // the handler is answering a turn

	// whisper keeps the hearing model loaded between utterances (whisper.go).
	whisper *whisperServer

	// backup speaks when the chosen engine fails (speech.go).
	backupMu     sync.Mutex
	backupKokoro *kokoro
	elevenDown   time.Time // ElevenLabs failed: skip it until then
	kokoroDown   time.Time

	// Test seams: nil means the real thing.
	transcribeFn func(ctx context.Context, wav string) (string, error)
	ackFn        func(hello bool)
	chimeFn      func()
	ackMakerFn   func() ackMaker
	clipChecked  func() // called once a wake clip's check has stored its verdict

	// Persona-driven bits: extra wake aliases, acknowledgement phrases, greeting.
	wakeAliases []string
	ackPhrases  []string
	greeting    string
	hellos      map[string]string // by part of the day (SetHellos)
	spokenName  string
	bare        atomic.Pointer[string] // what a bare wake is answered with (SetAddress)
	address     atomic.Pointer[string] // the owner's form of address (SetAddress)
	ownerName   atomic.Pointer[string] // the owner's first name (SetName)
}

// SetPersona applies a persona's voice traits: the spoken name (how the name is said),
// wake aliases, acknowledgements and greeting.
func (c *Channel) SetPersona(spokenName string, wakeAliases, acks []string, greeting string) {
	c.spokenName, c.wakeAliases, c.ackPhrases, c.greeting = spokenName, wakeAliases, acks, greeting
}

func (c *Channel) setState(state string) {
	if c.OnState != nil {
		c.OnState(state)
	}
}

// timing records one exchange's stages for the "(timing …)" log line.
type timing struct {
	utterance, ack, transcribed, firstDelta, firstAudio, done time.Time
	synth                                                     time.Duration
}

// takeTiming hands the pending utterance timing to the reply that answers it.
func (c *Channel) takeTiming() timing {
	c.tmu.Lock()
	t := c.pending
	c.pending = timing{}
	c.tmu.Unlock()
	return t
}

func (c *Channel) reportTiming(t timing) {
	if t.utterance.IsZero() {
		return
	}
	since := func(a, b time.Time) string {
		if a.IsZero() || b.IsZero() {
			return "?"
		}
		return fmt.Sprintf("%.1fs", b.Sub(a).Seconds())
	}
	fmt.Fprintf(c.out, "(timing: ack %s, transcribe %s, model first word %s, tts %.1fs, first audio %s, total %s)\n",
		since(t.utterance, t.ack), since(t.utterance, t.transcribed), since(t.transcribed, t.firstDelta), t.synth.Seconds(), since(t.transcribed, t.firstAudio), since(t.utterance, t.done))
}

// New builds the channel.
func New(cfg config.Voice, agentName, dataDir string) *Channel {
	if cfg.WakeWord == "" {
		cfg.WakeWord = "mirrin"
	}
	if cfg.Mode == "" {
		cfg.Mode = "push"
	}
	if cfg.WhisperBin == "" {
		cfg.WhisperBin = "whisper-cli"
	}
	if cfg.MaxSeconds <= 0 {
		cfg.MaxSeconds = 30
	}
	if cfg.FollowupSeconds <= 0 {
		cfg.FollowupSeconds = 6
	}
	c := &Channel{cfg: cfg, name: agentName, dataDir: dataDir, out: os.Stdout, in: os.Stdin, speakMu: make(chan struct{}, 1), listenNow: make(chan struct{}, 1)}
	if !serverDisabled() && cfg.WhisperModel != "" {
		if bin := findWhisperServer(cfg.WhisperBin); bin != "" {
			c.whisper = newWhisperServer(bin, cfg.WhisperModel, cfg.Language, dataDir)
		}
	}

	key := ElevenLabsKey(cfg)
	engine := cfg.Engine
	if engine == "" || engine == "auto" {
		switch {
		case cfg.TTSCommand != "":
			engine = "command"
		case key != "":
			engine = "elevenlabs"
		case kokoroReady(cfg.KokoroDir):
			engine = "kokoro"
		default:
			engine = "system"
		}
	}
	lang := speechLanguage(cfg.Language)
	engine, cfg.Voice = speechFor(lang, engine, cfg.Voice, kokoroReady(cfg.KokoroDir))
	c.cfg.Voice = cfg.Voice
	switch engine {
	case "elevenlabs":
		if key == "" {
			fmt.Fprintln(c.out, "voice engine elevenlabs selected but no ELEVENLABS_API_KEY; using system voice")
		} else {
			c.eleven = newElevenLabs(key, cfg.Voice, cfg.ElevenLabsModel)
		}
	case "kokoro":
		if !kokoroReady(cfg.KokoroDir) {
			fmt.Fprintln(c.out, "voice engine kokoro selected but not installed; run `mirrin voice setup`. Using system voice")
		} else {
			c.kokoro = newKokoro(cfg.KokoroDir, cfg.Voice, cfg.Speed)
			c.kokoro.lang, c.kokoro.pitch = lang, cfg.Pitch
		}
	case "command":
		if cfg.TTSCommand == "" {
			fmt.Fprintln(c.out, "voice engine command selected but tts_command is empty; using system voice")
		}
	}
	if c.eleven == nil && c.kokoro == nil && cfg.TTSCommand == "" && runtime.GOOS == "darwin" {
		c.macVoice = bestMacVoice(cfg.Voice)
	}

	wakeEngine := cfg.WakeEngine
	if wakeEngine == "" || wakeEngine == "auto" {
		// The on-device detector only knows names it has a model for; other
		// names, and Mirrin's while there is no model for it (wakemodel.go),
		// fall back to transcribe-and-match automatically.
		if HasWakeModel(cfg) && cfg.RecordCommand == "" && wakeReady(cfg.KokoroDir) {
			wakeEngine = "openwakeword"
		} else {
			wakeEngine = "transcribe"
		}
	}
	if wakeEngine == "openwakeword" {
		switch {
		case !wakeReady(cfg.KokoroDir):
			fmt.Fprintln(c.out, "wake engine openwakeword selected but not installed; run `mirrin voice setup`. Falling back to transcribe-and-match")
		case !HasWakeModel(cfg):
			// Never a stand-in model trained for another name.
			fmt.Fprintf(c.out, "wake engine openwakeword selected, but %s; listening for %q in what I transcribe instead\n", wakeByTranscript(cfg), WakePhrase(cfg.WakeWord))
		default:
			c.wake = newWakeHelper(cfg.KokoroDir, cfg.WakeModel, cfg.WakeThreshold, dataDir, cfg.MaxSeconds, cfg.FollowupSeconds)
			c.wake.talkOver = cfg.TalkOver
			c.wake.threshSpeaking = cfg.WakeThresholdSpeaking
		}
	}
	return c
}

// Engine describes the speech-out backend in use.
// voiceName is the configured synthesis voice (used to key cached clips).
func (c *Channel) voiceName() string {
	switch {
	case c.kokoro != nil:
		return "kokoro-" + c.kokoro.voice
	case c.eleven != nil:
		return "eleven-" + c.cfg.Voice
	default:
		return "system-" + c.cfg.Voice
	}
}

func (c *Channel) Engine() string {
	switch {
	case c.eleven != nil:
		return "ElevenLabs (" + c.eleven.voice + ")"
	case c.kokoro != nil:
		return "Kokoro offline neural voice (" + c.kokoro.voice + ")"
	case c.cfg.TTSCommand != "":
		return "custom command"
	case c.macVoice != "":
		return "macOS say (" + c.macVoice + ")"
	default:
		return "system synthesiser"
	}
}

// WakeEngine describes how the wake word is detected.
func (c *Channel) WakeEngine() string {
	if c.wake != nil {
		return "openWakeWord on-device detector (" + c.wake.model + ")"
	}
	return "transcribe-and-match (\"" + c.cfg.WakeWord + "\")"
}

func (c *Channel) Name() string        { return "voice" }
func (c *Channel) OwnerChatID() string { return "local" }

// Check verifies the speech tools are present.
func (c *Channel) Check() error {
	if _, err := exec.LookPath(c.cfg.WhisperBin); err != nil {
		return fmt.Errorf("%s not found: %s", c.cfg.WhisperBin, installHint("whisper"))
	}
	if c.cfg.WhisperModel == "" {
		return errors.New("voice isn't set up yet: run mirrin voice setup")
	}
	if _, err := os.Stat(c.cfg.WhisperModel); err != nil {
		return fmt.Errorf("the hearing model %s is missing: run mirrin voice setup", c.cfg.WhisperModel)
	}
	if c.cfg.RecordCommand == "" {
		if _, err := exec.LookPath("sox"); err != nil {
			return fmt.Errorf("sox not found (it records the microphone): %s", installHint("sox"))
		}
	}
	return nil
}

// Start runs the listen loop until ctx ends.
func (c *Channel) Start(ctx context.Context, handler channels.Handler) error {
	if err := c.Check(); err != nil {
		return err
	}
	push := c.cfg.Mode != "wake"
	if push {
		// No keyboard at all (running as a service, stdin is /dev/null): always-on
		// wake word is the only option. A pipe still counts as a keyboard.
		if f, ok := c.in.(*os.File); ok && !term.IsTerminal(int(f.Fd())) {
			if st, err := f.Stat(); err == nil && st.Mode()&os.ModeNamedPipe == 0 {
				push = false
				fmt.Fprintf(c.out, "no terminal attached; switching voice to wake mode\n")
			}
		}
	}
	fmt.Fprintf(c.out, "Voice: %s\n", c.Engine())
	if c.eleven == nil && c.kokoro == nil && c.cfg.TTSCommand == "" {
		fmt.Fprintln(c.out, "Tip: run `mirrin voice setup` for a natural offline voice, or set ELEVENLABS_API_KEY.")
	}
	if c.kokoro != nil {
		defer c.kokoro.stop()
	}
	defer c.stopBackup()
	if c.whisper != nil {
		// Load the hearing model now, so the first "Hey …" is answered quickly.
		life, stop := context.WithCancel(ctx)
		defer stop()
		c.whisper.warm(life)
	}
	greeting := c.startGreeting() // acks.go
	if push {
		fmt.Fprintf(c.out, "%s listening. Press Enter, speak, pause. Ctrl-C to stop.\n", c.name)
		c.Speak(ctx, greeting)
		return c.pushLoop(ctx, handler)
	}
	if c.wake != nil {
		fmt.Fprintf(c.out, "%s listening for \"Hey %s\" (%s). Ctrl-C to stop.\n", c.name, titleWords(c.cfg.WakeWord), c.WakeEngine())
		c.Speak(ctx, greeting)
		return c.wakeLoop(ctx, handler)
	}
	fmt.Fprintf(c.out, "%s listening for \"Hey %s\" (%s). Ctrl-C to stop.\n", c.name, titleWords(c.cfg.WakeWord), c.WakeEngine())
	c.Speak(ctx, greeting)
	return c.transcribeLoop(ctx, handler)
}

// converse handles one utterance and then keeps listening for follow-ups.
func (c *Channel) converse(ctx context.Context, handler channels.Handler, text string, followup func() string) {
	for text != "" && ctx.Err() == nil {
		// "Open the..." is half a sentence: hear the rest before acting on it.
		for unfinished(text) {
			fmt.Fprintf(c.out, "(heard %q, waiting for the rest…)\n", text)
			more := followup()
			if more == "" {
				break
			}
			text = strings.TrimRight(text, " .…,") + " " + more
		}
		fmt.Fprintf(c.out, "You: %s\n", text)
		c.handling.Store(true)
		if !c.screenCommand(ctx, text) { // spoken.go: "just read it to me"
			handler(ctx, channels.Inbound{Channel: "voice", ChatID: "local", Sender: "owner", Text: text, IsOwner: true})
		}
		c.stopMu.Lock()
		c.handling.Store(false)
		notice := c.stopNotice
		c.stopNotice = nil
		c.stopMu.Unlock()
		if notice != nil {
			_ = c.Send(ctx, "local", notice())
		}
		if c.cfg.FollowupSeconds <= 0 {
			return
		}
		fmt.Fprintf(c.out, "(listening for a follow-up…)\n")
		text = followup()
	}
}

// danglers are words a sentence can't end on; heard last, the speaker
// paused mid-thought ("Open the", "Book it for me and").
var danglers = map[string]bool{"the": true, "a": true, "an": true, "and": true, "or": true, "my": true, "your": true, "to": true, "of": true, "with": true}

// unfinished reports whether a transcript stops mid-sentence: whisper
// writes a trailing "..." when speech trails off, or it ends on a dangler.
func unfinished(text string) bool {
	t := strings.TrimSpace(text)
	if strings.HasSuffix(t, "...") || strings.HasSuffix(t, "…") {
		return true
	}
	f := strings.Fields(strings.ToLower(strings.TrimRight(t, " .,;:!?")))
	return len(f) > 0 && !strings.ContainsAny(t[len(t)-1:], ".!?") && danglers[f[len(f)-1]]
}

// pushLoop: Enter to talk.
func (c *Channel) pushLoop(ctx context.Context, handler channels.Handler) error {
	enter := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(c.in)
		for sc.Scan() {
			select {
			case enter <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
		close(enter)
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-enter:
			if !ok {
				return io.EOF
			}
		}
		text, err := c.listen(ctx, c.cfg.MaxSeconds)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Fprintf(c.out, "(listen: %v)\n", err)
			continue
		}
		c.converse(ctx, handler, text, func() string {
			t, err := c.listen(ctx, c.cfg.FollowupSeconds)
			if err != nil {
				return ""
			}
			t, _ = stripWakeLoose(t, c.cfg.WakeWord)
			return t
		})
	}
}

// isEcho reports whether a transcript is mostly words from the reply being
// spoken (the microphone heard the speaker, not the user).
func (c *Channel) isEcho(text string) bool {
	last, _ := c.lastSaid.Load().(string)
	if now, _ := c.saying.Load().(string); now != "" {
		last += " " + now // a reply still streaming isn't in lastSaid yet
	}
	if strings.TrimSpace(last) == "" {
		return false
	}
	norm := func(s string) []string {
		s = strings.ToLower(s)
		s = regexp.MustCompile(`[^a-z0-9' ]+`).ReplaceAllString(s, " ")
		return strings.Fields(s)
	}
	said := map[string]bool{}
	for _, w := range norm(last) {
		said[w] = true
	}
	words := norm(text)
	if len(words) == 0 {
		return false
	}
	hit := 0
	for _, w := range words {
		if said[w] {
			hit++
		}
	}
	return float64(hit)/float64(len(words)) >= 0.7
}

// novelWords counts words in text that were not in the reply being spoken.
func (c *Channel) novelWords(text string) int {
	last, _ := c.lastSaid.Load().(string)
	clean := func(s string) []string {
		return strings.Fields(regexp.MustCompile(`[^a-z0-9' ]+`).ReplaceAllString(strings.ToLower(s), " "))
	}
	said := map[string]bool{}
	for _, w := range clean(last) {
		said[w] = true
	}
	n := 0
	for _, w := range clean(text) {
		if !said[w] && len(w) > 1 {
			n++
		}
	}
	return n
}

// repetitive catches whisper's stutter on garbled audio ("19-19-19", "the ones, the ones").
func repetitive(text string) bool {
	words := strings.Fields(strings.ToLower(regexp.MustCompile(`[^a-z0-9' ]+`).ReplaceAllString(strings.ToLower(text), " ")))
	if len(words) < 4 {
		return false
	}
	uniq := map[string]bool{}
	for _, w := range words {
		uniq[w] = true
	}
	return float64(len(uniq))/float64(len(words)) <= 0.5
}

// hallucinated reports whisper's classic outputs on silence or noise.
func hallucinated(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	// non-speech annotations: *coughs*, *sighs*, (laughs), [music]
	t = strings.TrimSpace(reAnnotation.ReplaceAllString(t, ""))
	t = strings.Trim(t, ".,!? ")
	switch t {
	case "", "you", "thank you", "thanks", "sir", "mirrin", "hey mirrin", "bye", "okay", "ok", "hmm", "um", "uh":
		return true
	}
	return len(strings.Fields(t)) < 2 && len(t) < 6
}

func (c *Channel) isSpeaking() bool {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.speaking
}

// chime plays a short soft tone so the user knows the wake word was heard.
// channels.voice.chime_sound can point at any audio file (e.g. /System/Library/Sounds/Tink.aiff).
func (c *Channel) chime() {
	if c.chimeFn != nil {
		c.chimeFn()
		return
	}
	path := c.cfg.ChimeSound
	if path == "" {
		path = filepath.Join(c.dataDir, "chime-v2.wav")
		if _, err := os.Stat(path); err != nil {
			if err := writeChime(path); err != nil {
				return
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = playQuiet(ctx, path)
}

// writeChime synthesises a soft 140 ms rising two-note "blip" as 16-bit mono WAV.
func writeChime(path string) error {
	const rate = 22050
	n := rate * 140 / 1000
	samples := make([]byte, 44+n*2)
	copy(samples[0:], []byte("RIFF"))
	binary.LittleEndian.PutUint32(samples[4:], uint32(36+n*2))
	copy(samples[8:], []byte("WAVEfmt "))
	binary.LittleEndian.PutUint32(samples[16:], 16)
	binary.LittleEndian.PutUint16(samples[20:], 1)
	binary.LittleEndian.PutUint16(samples[22:], 1)
	binary.LittleEndian.PutUint32(samples[24:], rate)
	binary.LittleEndian.PutUint32(samples[28:], rate*2)
	binary.LittleEndian.PutUint16(samples[32:], 2)
	binary.LittleEndian.PutUint16(samples[34:], 16)
	copy(samples[36:], []byte("data"))
	binary.LittleEndian.PutUint32(samples[40:], uint32(n*2))
	for i := 0; i < n; i++ {
		t := float64(i) / rate
		// C5 then G5, each with a quick attack and a long, soft decay; a second harmonic for warmth.
		freq := 523.25
		local := i
		if i > n/2 {
			freq = 783.99
			local = i - n/2
		}
		half := float64(n / 2)
		env := math.Exp(-4.5*float64(local)/half) * math.Min(1, float64(local)/150)
		v := 0.16 * env * (math.Sin(2*math.Pi*freq*t) + 0.35*math.Sin(2*math.Pi*2*freq*t))
		binary.LittleEndian.PutUint16(samples[44+i*2:], uint16(int16(v*32767)))
	}
	return os.WriteFile(path, samples, 0o644)
}

// pausePlayback freezes the player on a wake event; resume or stop follows
// once the transcript is in.
func (c *Channel) pausePlayback() {
	c.smu.Lock()
	defer c.smu.Unlock()
	if c.player != nil && c.player.Process != nil && !c.paused {
		if err := pauseProcess(c.player.Process); err == nil {
			c.paused = true
			fmt.Fprintln(c.out, "(paused)")
		}
	}
}

func (c *Channel) resumePlayback() {
	c.smu.Lock()
	defer c.smu.Unlock()
	if c.player != nil && c.player.Process != nil && c.paused {
		_ = resumeProcess(c.player.Process)
		c.paused = false
		fmt.Fprintln(c.out, "(resumed)")
	}
}

// stopPlayback cancels the current utterance (confirmed barge-in).
func (c *Channel) stopPlayback() {
	c.smu.Lock()
	cancel := c.speakCancel
	player, paused := c.player, c.paused
	c.paused = false
	c.smu.Unlock()
	if cancel != nil {
		cancel()
		if player != nil && player.Process != nil {
			if paused {
				_ = resumeProcess(player.Process)
			}
			_ = player.Process.Kill()
		}
		fmt.Fprintln(c.out, "(interrupted)")
	}
}

// transcribeLoop: fallback wake mode without the detector.
func (c *Channel) transcribeLoop(ctx context.Context, handler channels.Handler) error {
	failures, quiet, blocked := 0, 0, 0
	c.micBlocked(ctx) // the room's level, before the first recording
	measured := time.Now()
	for ctx.Err() == nil {
		c.waitTillQuiet(ctx)
		if time.Since(measured) > time.Minute {
			// A noisy room never has a quiet round to measure in; look again
			// anyway, so the start level follows the room.
			c.micBlocked(ctx)
			measured = time.Now()
		}
		text, err := c.listen(ctx, c.cfg.MaxSeconds)
		if errors.Is(err, errNoAudio) {
			// A quiet room: listen again at once, so a wake word said now
			// isn't lost in a pause. Every few quiet rounds, check the
			// microphone isn't simply blocked (pure zeros), and only then warn.
			quiet++
			if quiet%4 == 0 && c.OnNoAudio != nil {
				if c.micBlocked(ctx) {
					blocked++
					c.OnNoAudio(blocked + 2) // the host warns on its third
				} else {
					blocked = 0
				}
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			failures++
			fmt.Fprintf(c.out, "(listen: %v)\n", err)
			time.Sleep(time.Duration(min(failures, 10)) * time.Second)
			continue
		}
		failures, quiet, blocked = 0, 0, 0
		t, ok := stripWakeWith(text, c.cfg.WakeWord, c.wakeAliases)
		if !ok {
			continue
		}
		if c.isEcho(t) {
			// "Hey Nyra" said over its own reply: the rest is the twin's
			// words, heard back. The name alone was meant.
			fmt.Fprintf(c.out, "(heard my own words after the name: %q)\n", t)
			t = ""
		}
		followup := func() string {
			c.waitTillQuiet(ctx)
			t, err := c.listen(ctx, c.cfg.FollowupSeconds)
			if err != nil {
				return ""
			}
			if c.isEcho(t) {
				fmt.Fprintf(c.out, "(ignored, that was me: %q)\n", t)
				return ""
			}
			t, _ = stripWakeLoose(t, c.cfg.WakeWord)
			return t
		}
		if strings.TrimSpace(t) == "" {
			c.Speak(ctx, c.bareWake()) // "Sir?": the name alone
			if t = followup(); t == "" {
				continue
			}
		}
		c.converse(ctx, handler, t, followup)
	}
	return nil
}

// waitTillQuiet holds a recording back while the twin is talking, and a
// moment after, so it doesn't hear itself as the owner.
func (c *Channel) waitTillQuiet(ctx context.Context) {
	spoke := false
	for c.isSpeaking() && ctx.Err() == nil {
		spoke = true
		sleepCtx(ctx, 100*time.Millisecond)
	}
	if spoke {
		sleepCtx(ctx, 400*time.Millisecond)
	}
}

var reWake = regexp.MustCompile(`^[\s,.!?]*`)

// stripWake returns the utterance after the wake word, and whether it was present.
// The bare name only counts at the start of the text; the "hey <name>" forms count
// anywhere, because a capture taken while the twin is talking begins with its own words.
func stripWake(text, wake string) (string, bool) {
	return stripWakeWith(text, wake, nil)
}

// stripWakeWith is stripWake with extra aliases (from the persona). A name
// only counts as a whole word: "Ava" wakes on "Ava, lights", never on
// "Available tomorrow?" or "Avatar is on".
func stripWakeWith(text, wake string, extra []string) (string, bool) {
	text = strings.TrimSpace(text)
	lt := strings.ToLower(text)
	if len(lt) != len(text) {
		text = lt // lowercasing changed byte lengths (rare scripts): keep offsets valid
	}
	all := append(aliases(wake), strings.ToLower(strings.TrimSpace(wake)))
	for _, e := range extra {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" {
			// "hey, " too, as for the wake word: whisper often writes "Hey, Naira."
			all = append(all, "hey "+e, "hey, "+e, e)
		}
	}
	// Longest first, so "mirrin" can never win over "hey mirrin".
	sort.SliceStable(all, func(i, j int) bool { return len(all[i]) > len(all[j]) })
	for _, w := range all {
		if w != "" && strings.HasPrefix(lt, w) && wordEndsAt(lt, len(w)) {
			rest := strings.TrimSpace(reWake.ReplaceAllString(text[len(w):], ""))
			return rest, true
		}
	}
	best := -1
	bestLen := 0
	for _, w := range all {
		if !strings.HasPrefix(w, "hey ") && !strings.HasPrefix(w, "ok") && !strings.HasPrefix(w, "hi ") {
			continue
		}
		if i := lastWord(lt, w); i > best {
			best, bestLen = i, len(w)
		}
	}
	if best >= 0 {
		rest := strings.TrimSpace(reWake.ReplaceAllString(text[best+bestLen:], ""))
		return rest, true
	}
	return "", false
}

// wordEndsAt reports whether a word ends at byte i of s: the end of the
// text, or anything but a letter or digit next.
func wordEndsAt(s string, i int) bool {
	if i >= len(s) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// wordStartsAt reports whether a word starts at byte i of s.
func wordStartsAt(s string, i int) bool {
	if i <= 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// lastWord finds the last whole-word occurrence of w in s, or -1.
func lastWord(s, w string) int {
	end := len(s)
	for end > 0 {
		i := strings.LastIndex(s[:end], w)
		if i < 0 {
			return -1
		}
		if wordStartsAt(s, i) && wordEndsAt(s, i+len(w)) {
			return i
		}
		end = i + len(w) - 1
	}
	return -1
}

// titleWords capitalises the first letter of each word ("hey mirrin" →
// "Hey Mirrin").
func titleWords(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		r, size := utf8.DecodeRuneInString(w)
		words[i] = string(unicode.ToUpper(r)) + w[size:]
	}
	return strings.Join(words, " ")
}

// stripWakeLoose removes a leading wake word if present, else returns the text.
func stripWakeLoose(text, wake string) (string, bool) {
	if t, ok := stripWake(text, wake); ok {
		return t, true
	}
	return strings.TrimSpace(text), false
}

// aliases returns common phrasings and misrecognitions of the wake word (longest first).
func aliases(wake string) []string {
	if strings.EqualFold(strings.TrimSpace(wake), "mirrin") {
		// "Hey Mirrin" as whisper tends to write it (the persona's
		// wake_aliases say the same, for a channel without a persona)
		out := []string{"hey mirrin", "hey, mirrin", "hey mirrin's", "ok mirrin", "okay mirrin", "hi mirrin"}
		for _, w := range []string{"mirin", "mirren", "miran", "mirron", "mirrins"} {
			out = append(out, "hey "+w, "hey, "+w, w)
		}
		return out
	}
	return []string{"hey " + wake, "hey, " + wake, "ok " + wake, "okay " + wake, "hi " + wake}
}

// Listen records one utterance and transcribes it.
func (c *Channel) Listen(ctx context.Context) (string, error) {
	return c.listen(ctx, c.cfg.MaxSeconds)
}

func (c *Channel) listen(ctx context.Context, maxSeconds int) (string, error) {
	wav := filepath.Join(c.dataDir, "voice-in.wav")
	if err := c.record(ctx, wav, maxSeconds); err != nil {
		return "", err
	}
	return c.Transcribe(ctx, wav)
}

func (c *Channel) record(ctx context.Context, wav string, maxSeconds int) error {
	_ = os.Remove(wav)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(maxSeconds+10)*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if c.cfg.RecordCommand != "" {
		cmd = shell(ctx, c.cfg.RecordCommand)
		cmd.Env = append(os.Environ(), "OUT="+wav, "MAX_SECONDS="+fmt.Sprint(maxSeconds))
	} else {
		// 16 kHz mono 16-bit, start on sound, stop after 1.2 s of silence, cap the length.
		cmd = exec.CommandContext(ctx, "sox", "-q", "-d", "-r", "16000", "-c", "1", "-b", "16", wav,
			"trim", "0", fmt.Sprint(maxSeconds),
			"silence", "1", "0.2", c.soundLevel(), "1", "1.2", c.soundLevel())
	}
	out, err := cmd.CombinedOutput()
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("record: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if st, err := os.Stat(wav); err != nil || st.Size() < 1000 {
		return errNoAudio
	}
	return nil
}

// errNoAudio is a recording that heard nothing: in a quiet room that is
// normal (sox waits for sound and gives up); only a microphone that gives
// pure zeros is blocked (micBlocked tells them apart).
var errNoAudio = errors.New("no audio captured")

// micBlocked records half a second and reports whether every sample was
// zero, as macOS gives an app it hasn't allowed the microphone. A quiet
// room still has a noise floor, and that sets the level a recording
// starts at (soundLevel). Without sox it can't tell, and says no.
func (c *Channel) micBlocked(ctx context.Context) bool {
	if c.cfg.RecordCommand != "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sox", "-q", "-d", "-r", "16000", "-c", "1", "-n", "trim", "0", "0.5", "stat").CombinedOutput()
	if err != nil {
		return false
	}
	amp, ok := maxAmplitude(string(out))
	if !ok {
		return false
	}
	if amp > 0 {
		c.startLevel.Store(math.Float64bits(startLevelFor(amp)))
	}
	fmt.Fprintf(c.out, "(listen: room level %.2f%%, recording starts over %s)\n", amp*100, c.soundLevel())
	return amp == 0
}

// startLevelFor is the level a recording starts at, for a room whose
// quiet peaks at floor: three times that, between 0.6% and 6%. A fixed 2%
// was too loud for a laptop's own microphone a step away, so a spoken wake
// word never started a recording; capped at 2%, a room whose quiet peaks
// at 3% started a recording on its own noise, over and over.
func startLevelFor(floor float64) float64 {
	return min(max(floor*3, 0.006), 0.06)
}

// soundLevel is the start level as sox wants it ("0.80%"); 2% until the
// room has been measured.
func (c *Channel) soundLevel() string {
	l := math.Float64frombits(c.startLevel.Load())
	if l == 0 {
		l = 0.02
	}
	return strconv.FormatFloat(l*100, 'f', 2, 64) + "%"
}

// maxAmplitude reads "Maximum amplitude:" from sox's stat output.
func maxAmplitude(stat string) (float64, bool) {
	for _, line := range strings.Split(stat, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "Maximum amplitude" {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			return f, err == nil
		}
	}
	return 0, false
}

// Transcribe runs whisper.cpp on a 16 kHz WAV: through the loaded
// whisper-server when it is up, else a whisper-cli run.
func (c *Channel) Transcribe(ctx context.Context, wav string) (string, error) {
	if c.transcribeFn != nil {
		return c.transcribeFn(ctx, wav)
	}
	o := hearing(c.cfg, c.hearingPrompt())
	if s := c.whisper; s != nil {
		text, err := s.transcribe(ctx, wav, o) // with a short deadline of its own
		switch {
		case err == nil:
			c.recovered(troubleEars)
			return transcribe.Clean(text), nil
		case ctx.Err() != nil:
			return "", ctx.Err()
		case !errors.Is(err, errNotReady):
			fmt.Fprintf(c.out, "(whisper server: %v; using whisper-cli for now)\n", err)
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	text, err := transcribe.CLI(cctx, wav, o)
	if err != nil && ctx.Err() == nil {
		c.problem(troubleEars, fmt.Sprintf("I can't make out speech right now: whisper failed (%s). Try mirrin doctor, or run mirrin voice setup again.", brief(err)))
		return "", err
	}
	if err == nil {
		c.recovered(troubleEars)
	}
	return text, err
}

// Send prints and speaks a reply. Said unprompted (a task's update, not a
// reply in a conversation, which listens afterwards anyway) and asking
// something ("Shall I go to checkout?"), it then listens for the answer,
// as if the owner had said the wake word.
func (c *Channel) Send(ctx context.Context, _ string, text string) error {
	fmt.Fprintf(c.out, "%s: %s\n", c.name, text)
	c.lastSaid.Store(text)
	unprompted := !c.handling.Load()
	c.Speak(ctx, text)
	if unprompted && ctx.Err() == nil && asksForAnswer(text) && !c.handling.Load() {
		c.ListenNow()
	}
	return nil
}

// Interrupt stops whatever the twin is saying (barge-in).
func (c *Channel) Interrupt() { c.stopPlayback() }

var reAnnotation = regexp.MustCompile(`\*[^*]*\*|\[[^\]]*\]|\([^)]*\)`)

var reSentence = regexp.MustCompile(`([.!?…]+)(\s+|$)`)

// sentences splits text so speech can start on the first sentence while the
// rest is still being synthesised.
func sentences(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	idx := reSentence.FindAllStringIndex(s, -1)
	var out []string
	last := 0
	for _, m := range idx {
		part := strings.TrimSpace(s[last:m[1]])
		if part != "" {
			out = append(out, part)
		}
		last = m[1]
	}
	if rest := strings.TrimSpace(s[last:]); rest != "" {
		out = append(out, rest)
	}
	// Merge very short fragments ("Yes." "Sir.") so the voice doesn't stutter.
	var merged []string
	for _, p := range out {
		if n := len(merged); n > 0 && len(merged[n-1]) < 25 {
			merged[n-1] += " " + p
			continue
		}
		merged = append(merged, p)
	}
	return merged
}

// Speak voices text through the configured synthesiser, sentence by sentence,
// synthesising the next sentence while the current one plays. Interruptible.
func (c *Channel) Speak(ctx context.Context, text string) {
	spoken := speakable(text)
	if spoken == "" {
		return
	}
	q := c.newSpeechQueue(ctx)
	for _, p := range sentences(spoken) {
		q.Push(p)
	}
	q.Finish()
}

// speakSystem uses the OS synthesiser or a custom command.
func (c *Channel) speakSystem(ctx context.Context, spoken string) {
	var cmd *exec.Cmd
	switch {
	case c.cfg.TTSCommand != "":
		cmd = shell(ctx, c.cfg.TTSCommand)
		cmd.Env = append(os.Environ(), "TEXT="+spoken, "VOICE="+c.cfg.Voice)
		cmd.Stdin = strings.NewReader(spoken)
	case runtime.GOOS == "darwin":
		args := []string{}
		if v := c.macVoice; v != "" {
			args = append(args, "-v", v)
		} else if c.cfg.Voice != "" && !strings.Contains(c.cfg.Voice, "_") {
			args = append(args, "-v", c.cfg.Voice)
		}
		if c.cfg.Rate > 0 {
			args = append(args, "-r", fmt.Sprint(c.cfg.Rate))
		}
		cmd = exec.CommandContext(ctx, "say", append(args, spoken)...)
	case runtime.GOOS == "windows":
		ps := `Add-Type -AssemblyName System.Speech; $s = New-Object System.Speech.Synthesis.SpeechSynthesizer; $s.Speak([Console]::In.ReadToEnd())`
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", ps)
		cmd.Stdin = strings.NewReader(spoken)
	default:
		if _, err := exec.LookPath("spd-say"); err == nil {
			cmd = exec.CommandContext(ctx, "spd-say", "-w", spoken)
		} else {
			cmd = exec.CommandContext(ctx, "espeak", spoken)
		}
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(c.out, "(speak: %v)\n", err)
		c.problem(troubleSpeaker, fmt.Sprintf("I can't speak out loud right now (%s). You'll still see my replies on screen; check your speakers or run mirrin doctor.", brief(err)))
		return
	}
	// A wake word pauses it while it speaks; once it's done, the player it
	// stood in for (the stream with the rest of the reply) is what a wake
	// word pauses again.
	c.smu.Lock()
	prev := c.player
	c.player = cmd
	c.smu.Unlock()
	defer func() {
		c.smu.Lock()
		if c.player == cmd {
			c.player = prev
		}
		c.smu.Unlock()
	}()
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		fmt.Fprintf(c.out, "(speak: %v)\n", err)
		c.problem(troubleSpeaker, fmt.Sprintf("I can't speak out loud right now (%s). You'll still see my replies on screen; check your speakers or run mirrin doctor.", brief(err)))
		return
	}
	if ctx.Err() == nil {
		c.recovered(troubleSpeaker)
	}
}

// speakable rewrites text for the ear: the agent's name is pronounced,
// markdown and URLs are dropped.
var (
	reURL        = regexp.MustCompile(`https?://\S+|\bwww\.\S+`)
	reMDLink     = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	reListMark   = regexp.MustCompile(`(?m)^[ \t]*(?:[-*•▪◦]|\d{1,2}[.)])[ \t]+`)
	reHeading    = regexp.MustCompile(`(?m)^[ \t]*#{1,6}[ \t]*`)
	reEmphasis   = regexp.MustCompile(`(\*\*|__|\*|_)([^*_\n]+?)(\*\*|__|\*|_)`)
	reCurrency   = regexp.MustCompile(`\b(?:A|AU|US|NZ|CA)\$`)
	reCurrCode   = regexp.MustCompile(`\b(?:AUD|USD|NZD|CAD)[ \t]?(\d)`)
	reCurrAfter  = regexp.MustCompile(`\b(\d[\d,]*(?:\.\d{2})?)[ \t]?(?:AUD|USD|NZD|CAD)\b`)
	reTimeAMPM   = regexp.MustCompile(`(?i)\b(\d{1,2}(?::\d{2})?)(am|pm)\b`)
	reEmoji      = regexp.MustCompile(`[\x{1F000}-\x{1FAFF}\x{2600}-\x{27BF}\x{2B00}-\x{2BFF}\x{FE0F}\x{200D}]`)
	reSpaces     = regexp.MustCompile(`[ \t]+`)
	reSpaceComma = regexp.MustCompile(`[ \t]+([,.;:!?])`)
	reAbbrevs    = strings.NewReplacer("e.g.", "for example", "i.e.", "that is", "etc.", "and so on", "vs.", "versus", " vs ", " versus ", "approx.", "about", "w/", "with ")
	reSymbols    = strings.NewReplacer("→", " to ", "←", " from ", "&", " and ", "°C", " degrees", "°", " degrees", "≈", "about ", "%", " percent", "…", "...")
)

// speakable turns model output into something a voice can read aloud:
// markdown, links, list markers, symbols and currency become words or go,
// and each line ends as a sentence so the voice pauses where the eye would.
func speakable(s string) string {
	s = speakableName(s)
	s = reEmoji.ReplaceAllString(s, "")
	s = reMDLink.ReplaceAllString(s, "$1")
	s = reURL.ReplaceAllString(s, "a link")
	s = reHeading.ReplaceAllString(s, "")
	s = reListMark.ReplaceAllString(s, "")
	s = strings.NewReplacer("**", "", "__", "", "`", "").Replace(s)
	s = reEmphasis.ReplaceAllString(s, "$2")
	s = reCurrency.ReplaceAllString(s, "$$")
	s = reCurrCode.ReplaceAllString(s, "$$$1")
	s = reCurrAfter.ReplaceAllString(s, "$$$1") // "349.30 AUD" is said "$349.30"
	s = reTimeAMPM.ReplaceAllString(s, "$1 $2")
	s = speakDates(s)
	s = reAbbrevs.Replace(s)
	s = reSymbols.Replace(s)
	s = regexp.MustCompile(`[ \t]*—[ \t]*`).ReplaceAllString(s, ", ")
	s = strings.ReplaceAll(s, "–", " to ")
	// Each non-empty line is a sentence of its own.
	var lines []string
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(reSpaces.ReplaceAllString(ln, " "))
		if ln == "" {
			continue
		}
		if !strings.ContainsAny(ln[len(ln)-1:], ".!?:;,") {
			ln += "."
		}
		lines = append(lines, ln)
	}
	s = strings.Join(lines, " ")
	s = reSpaceComma.ReplaceAllString(s, "$1")
	s = strings.ReplaceAll(s, ",,", ",")
	return strings.TrimSpace(reSpaces.ReplaceAllString(s, " "))
}

// speakableName is set by the channel to swap its name for its pronunciation.
var speakableName = func(s string) string { return s }

func shell(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", command)
	}
	return exec.CommandContext(ctx, "/bin/sh", "-c", command)
}
