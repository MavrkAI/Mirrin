package voice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// Voice setup downloads about 850 MB. Every file is checked against the
// SHA-256 it is published with, so a truncated or tampered download is
// never used; an interrupted download picks up where it stopped; and
// progress is shown as it goes.

// knownFile is a download's published size and SHA-256.
type knownFile struct {
	size   int64
	sha256 string
}

// knownFiles are the files voice setup downloads, by name.
var knownFiles = map[string]knownFile{
	// huggingface.co/ggerganov/whisper.cpp (git-lfs object ids)
	"ggml-small.en.bin": {487614201, "c6138d6d58ecc8322097e0f987c32f1be8bb0a18532a3f88f734d1bbf9c41e5d"},
	"ggml-small.bin":    {487601967, "1be3a9b2063867b937e64e2ec7483364a79917e157fa98c5d94b5c1fffea987b"},
	"ggml-base.en.bin":  {147964211, "a03779c86df3323075f5e796cb2ce5029f00ec8869eee3fdfb897afe36c6d002"},
	"ggml-base.bin":     {147951465, "60ed5bc3dd14eea856493d334349b405782ddcaf0028d4b5df4088345fba2efe"},
	// github.com/thewh1teagle/kokoro-onnx releases, model-files-v1.0
	"kokoro-v1.0.onnx": {325532387, "7d5df8ecf7d4b1878015a32686053fd0eebe2bc377234608764cc0ef3636a6c5"},
	"voices-v1.0.bin":  {28214398, "bca610b8308e8d99f32e6fe4197e7ec01679264efed0cac9140fe9c29f1fbf7d"},
}

// httpClient fetches downloads; tests point it at a local server.
var httpClient = &http.Client{}

// download fetches url to path, resuming a partial download, and checks it
// against the published SHA-256 when the file is a known one. A file already
// in place is kept if it checks out.
func download(ctx context.Context, url, path string, w io.Writer) error {
	name := filepath.Base(path)
	want, known := knownFiles[name]
	if st, err := os.Stat(path); err == nil {
		if !known {
			if st.Size() > 1<<20 {
				return nil
			}
		} else if st.Size() == want.size {
			if err := verify(path, want); err == nil {
				fmt.Fprintf(w, "%s is already here and checks out.\n", name)
				return nil
			}
			fmt.Fprintf(w, "%s is damaged; downloading it again.\n", name)
			_ = os.Remove(path)
		} else {
			_ = os.Remove(path) // an older or partial copy
		}
	}
	tmp := path + ".part"
	for attempt := 0; attempt < 3; attempt++ {
		err := fetch(ctx, url, tmp, name, want.size, w)
		if err == nil {
			break
		}
		if ctx.Err() != nil || attempt == 2 {
			return fmt.Errorf("download %s: %w (run mirrin voice setup again to pick up where it stopped)", name, err)
		}
		fmt.Fprintf(w, "\n%s: %v; trying again…\n", name, err)
		if !sleepCtx(ctx, time.Duration(attempt+1)*2*time.Second) {
			return ctx.Err()
		}
	}
	if known {
		if err := verify(tmp, want); err != nil {
			_ = os.Remove(tmp) // useless to resume from
			return fmt.Errorf("download %s: %w; run mirrin voice setup again", name, err)
		}
	}
	return os.Rename(tmp, path)
}

// fetch downloads url into tmp, continuing from what tmp already holds.
func fetch(ctx context.Context, url, tmp, name string, size int64, w io.Writer) error {
	var have int64
	if st, err := os.Stat(tmp); err == nil {
		have = st.Size()
	}
	if size > 0 && have == size {
		return nil // complete already; verify decides
	}
	if size > 0 && have > size {
		_ = os.Remove(tmp)
		have = 0
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	case http.StatusOK:
		flags |= os.O_TRUNC // the server sent it whole: start over
		have = 0
	case http.StatusRequestedRangeNotSatisfiable:
		_ = os.Remove(tmp) // what we have doesn't fit what they have
		return errors.New("the partial download didn't match; starting over")
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	total := size
	if total <= 0 && resp.ContentLength > 0 {
		total = have + resp.ContentLength
	}
	f, err := os.OpenFile(tmp, flags, 0o644)
	if err != nil {
		return err
	}
	p := newProgress(w, name, have, total)
	if have > 0 {
		p.resumed = true
	}
	_, err = io.Copy(f, io.TeeReader(resp.Body, p))
	p.done(err == nil)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// verify checks a file's size and SHA-256.
func verify(path string, want knownFile) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if n != want.size || hex.EncodeToString(h.Sum(nil)) != want.sha256 {
		return errors.New("it doesn't match the published checksum")
	}
	return nil
}

// progress prints how a download is going: a line that updates in place on
// a terminal, a line every 10% otherwise (a log, the menu bar's Terminal).
type progress struct {
	w          io.Writer
	name       string
	n, total   int64
	tty        bool
	resumed    bool
	lastPrint  time.Time
	lastTenths int64
	started    time.Time
}

func newProgress(w io.Writer, name string, have, total int64) *progress {
	tty := false
	if f, ok := w.(*os.File); ok {
		tty = term.IsTerminal(int(f.Fd()))
	}
	p := &progress{w: w, name: name, n: have, total: total, tty: tty, started: time.Now(), lastTenths: -1}
	p.print(true)
	return p
}

func (p *progress) Write(b []byte) (int, error) {
	p.n += int64(len(b))
	p.print(false)
	return len(b), nil
}

func mb(n int64) string { return fmt.Sprintf("%d MB", (n+(1<<19))>>20) }

func (p *progress) line() string {
	if p.total <= 0 {
		return fmt.Sprintf("Downloading %s: %s", p.name, mb(p.n))
	}
	return fmt.Sprintf("Downloading %s: %s of %s (%d%%)", p.name, mb(p.n), mb(p.total), p.n*100/p.total)
}

func (p *progress) print(first bool) {
	if first {
		if p.n > 0 {
			fmt.Fprintf(p.w, "Picking up %s where it stopped (%s already here).\n", p.name, mb(p.n))
		}
	}
	if p.tty {
		if first || time.Since(p.lastPrint) >= 200*time.Millisecond {
			p.lastPrint = time.Now()
			fmt.Fprintf(p.w, "\r%s   ", p.line())
		}
		return
	}
	tenths := int64(0)
	if p.total > 0 {
		tenths = p.n * 10 / p.total
	}
	if first || tenths != p.lastTenths {
		p.lastTenths = tenths
		fmt.Fprintln(p.w, p.line())
	}
}

func (p *progress) done(ok bool) {
	if p.tty {
		fmt.Fprintf(p.w, "\r%s   \n", p.line())
	}
	if ok {
		fmt.Fprintf(p.w, "Downloaded %s.\n", p.name)
	}
}

// ---- language ---------------------------------------------------------------

// WhisperModelFor is the whisper model to hear a language with: the
// English-only small model for English (or unknown), else the multilingual one.
func WhisperModelFor(lang string) string {
	if lang == "" || lang == "en" {
		return "small.en"
	}
	return "small"
}

// WhisperModelName is the model name a whisper model file holds ("small.en"
// for ".../ggml-small.en.bin").
func WhisperModelName(path string) string {
	return strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "ggml-"), ".bin")
}

// EnglishOnly reports whether a whisper model file only knows English.
func EnglishOnly(path string) bool { return strings.HasSuffix(WhisperModelName(path), ".en") }

// whisperLanguages are the language codes whisper knows.
var whisperLanguages = map[string]string{
	"en": "English", "zh": "Chinese", "de": "German", "es": "Spanish", "ru": "Russian", "ko": "Korean",
	"fr": "French", "ja": "Japanese", "pt": "Portuguese", "tr": "Turkish", "pl": "Polish", "ca": "Catalan",
	"nl": "Dutch", "ar": "Arabic", "sv": "Swedish", "it": "Italian", "id": "Indonesian", "hi": "Hindi",
	"fi": "Finnish", "vi": "Vietnamese", "he": "Hebrew", "uk": "Ukrainian", "el": "Greek", "ms": "Malay",
	"cs": "Czech", "ro": "Romanian", "da": "Danish", "hu": "Hungarian", "ta": "Tamil", "no": "Norwegian",
	"th": "Thai", "ur": "Urdu", "hr": "Croatian", "bg": "Bulgarian", "lt": "Lithuanian", "la": "Latin",
	"mi": "Maori", "ml": "Malayalam", "cy": "Welsh", "sk": "Slovak", "te": "Telugu", "fa": "Persian",
	"lv": "Latvian", "bn": "Bengali", "sr": "Serbian", "az": "Azerbaijani", "sl": "Slovenian", "kn": "Kannada",
	"et": "Estonian", "mk": "Macedonian", "br": "Breton", "eu": "Basque", "is": "Icelandic", "hy": "Armenian",
	"ne": "Nepali", "mn": "Mongolian", "bs": "Bosnian", "kk": "Kazakh", "sq": "Albanian", "sw": "Swahili",
	"gl": "Galician", "mr": "Marathi", "pa": "Punjabi", "si": "Sinhala", "km": "Khmer", "sn": "Shona",
	"yo": "Yoruba", "so": "Somali", "af": "Afrikaans", "oc": "Occitan", "ka": "Georgian", "be": "Belarusian",
	"tg": "Tajik", "sd": "Sindhi", "gu": "Gujarati", "am": "Amharic", "yi": "Yiddish", "lo": "Lao",
	"uz": "Uzbek", "fo": "Faroese", "ht": "Haitian Creole", "ps": "Pashto", "tk": "Turkmen", "nn": "Nynorsk",
	"mt": "Maltese", "sa": "Sanskrit", "lb": "Luxembourgish", "my": "Myanmar", "bo": "Tibetan", "tl": "Tagalog",
	"mg": "Malagasy", "as": "Assamese", "tt": "Tatar", "haw": "Hawaiian", "ln": "Lingala", "ha": "Hausa",
	"ba": "Bashkir", "jw": "Javanese", "su": "Sundanese", "yue": "Cantonese",
}

// localeAliases map locale language codes to whisper's.
var localeAliases = map[string]string{"nb": "no", "iw": "he", "fil": "tl", "jv": "jw", "in": "id", "ji": "yi"}

// LanguageName is a language code's English name ("fr" → "French").
func LanguageName(code string) string {
	if n, ok := whisperLanguages[code]; ok {
		return n
	}
	return code
}

// langCode reads the language from a locale ("fr_FR.UTF-8", "pt-BR",
// "zh-Hans-CN"), as whisper names it; "" when there is none or whisper
// doesn't know it.
func langCode(locale string) string {
	l := strings.ToLower(strings.TrimSpace(locale))
	if i := strings.IndexAny(l, "._@-"); i >= 0 {
		l = l[:i]
	}
	if a, ok := localeAliases[l]; ok {
		l = a
	}
	if _, ok := whisperLanguages[l]; !ok {
		return "" // "c", "posix", or something whisper can't hear
	}
	return l
}

// SystemLanguage is the language the computer is set to, as a whisper
// language code ("" when it can't tell).
func SystemLanguage() string { return systemLanguage(os.Getenv, osLanguage) }

func systemLanguage(getenv func(string) string, osLang func() string) string {
	// The desktop's own setting first: a service has no LANG, and a Terminal
	// sets it from the region, which isn't always the language spoken.
	if l := langCode(osLang()); l != "" {
		return l
	}
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := getenv(k); v != "" {
			return langCode(v) // the first one set decides, as for any program
		}
	}
	return ""
}

// osLanguage asks the operating system for the user's first language.
func osLanguage() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.CommandContext(ctx, "defaults", "read", "-g", "AppleLanguages").Output()
		if err != nil {
			return ""
		}
		// ( "fr-FR", "en-AU" )
		for _, f := range strings.FieldsFunc(string(out), func(r rune) bool { return strings.ContainsRune("(),\" \n\t", r) }) {
			return f
		}
	case "windows":
		out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", "(Get-UICulture).Name").Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	return ""
}

// ---- the whole setup ---------------------------------------------------------

// Setup installs the offline ears (whisper, in the owner's language), a
// natural offline voice (Kokoro) and the on-device wake word, and points v
// at them. `mirrin voice setup` and the Set up voice page both run it; each
// saves v and hands it to the twin afterwards.

// SetupSteps are the parts of setup, replaceable in tests.
type SetupSteps struct {
	LookPath func(string) (string, error)
	Language func() string
	Whisper  func(ctx context.Context, dir, model string, w io.Writer) (string, error)
	Kokoro   func(ctx context.Context, dir string, w io.Writer) error
	Wake     func(ctx context.Context, dir string, w io.Writer) error
	// OS is the system install hints are for (runtime.GOOS).
	OS string
}

// DefaultSetupSteps are the real downloads and installers.
func DefaultSetupSteps() SetupSteps {
	return SetupSteps{LookPath: exec.LookPath, Language: SystemLanguage, Whisper: DownloadWhisperModel, Kokoro: SetupKokoro, Wake: SetupWake, OS: runtime.GOOS}
}

// SetupStep is one step of setup: while it runs (Running), then what came
// of it.
type SetupStep struct {
	Label   string `json:"label"`
	Running bool   `json:"running,omitempty"`
	OK      bool   `json:"ok"`
	Notes   string `json:"notes,omitempty"`
}

// Setup labels, in the order setup runs them.
const (
	StepHearing = "Hearing"
	StepVoice   = "Voice"
	StepWake    = "Wake word"
	StepTools   = "Tools"
)

// RunSetup runs every step, writing what each does to w and telling
// progress (when set) as each starts and ends. It changes v in place (the
// caller saves it) and returns every step's outcome; its error is only ever
// the context's, when setup was stopped part-way.
func RunSetup(ctx context.Context, v *config.Voice, name, modelsDir string, w io.Writer, s SetupSteps, progress func(SetupStep)) ([]SetupStep, error) {
	if progress == nil {
		progress = func(SetupStep) {}
	}
	var results []SetupStep
	done := func(r SetupStep) {
		results = append(results, r)
		progress(r)
	}

	// The tools it can't install itself.
	whisperBin := v.WhisperBin
	if whisperBin == "" {
		whisperBin = "whisper-cli"
	}
	var missing []string
	for _, bin := range []string{whisperBin, "sox"} {
		if _, err := s.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}

	// Ears. A first setup follows the computer's language; a re-run keeps
	// the hearing that works and only says how to change it (an owner on a
	// German computer may well talk to the twin in English).
	h := ChooseHearing(*v, name, s.Language)
	fmt.Fprintf(w, "Hearing: %s\n", h.Intro)
	progress(SetupStep{Label: StepHearing, Running: true})
	if h.Keep {
		done(SetupStep{Label: StepHearing, OK: true, Notes: fmt.Sprintf("whisper %s, as it was", WhisperModelName(v.WhisperModel))})
	} else {
		path, err := s.Whisper(ctx, modelsDir, h.Model, w)
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
		if err != nil {
			stepFailed(w, err)
			done(SetupStep{Label: StepHearing, Notes: fmt.Sprintf("the %s model didn't download: %s", h.Model, firstLine(err))})
		} else {
			if v.WhisperModel != path && h.Existing {
				fmt.Fprintf(w, "(%s stays where it is; I'm using %s now.)\n", v.WhisperModel, path)
			}
			v.WhisperModel = path
			if v.Language == "" {
				v.Language = h.Language // "auto" on a first setup for a non-English computer
			}
			done(SetupStep{Label: StepHearing, OK: true, Notes: h.Summary})
		}
	}
	if h.Hint != "" {
		fmt.Fprintln(w, h.Hint)
	}

	// A natural offline voice.
	fmt.Fprintln(w, "\nVoice: Kokoro, which speaks offline.")
	progress(SetupStep{Label: StepVoice, Running: true})
	kerr := s.Kokoro(ctx, v.KokoroDir, w)
	if ctx.Err() != nil {
		return results, ctx.Err()
	}
	if kerr != nil {
		stepFailed(w, kerr)
		done(SetupStep{Label: StepVoice, Notes: "Kokoro didn't install (" + firstLine(kerr) + "), so the system voice speaks for now"})
	} else {
		if v.Engine == "" || v.Engine == "system" {
			v.Engine = "auto"
		}
		if !strings.Contains(v.Voice, "_") {
			v.Voice = "bm_george"
		}
		done(SetupStep{Label: StepVoice, OK: true, Notes: "Kokoro, offline"})
	}

	// The wake word, on the device (it shares Kokoro's Python environment).
	phrase := WakePhrase(v.WakeWord)
	fmt.Fprintf(w, "\nWake word: %q, heard on this computer.\n", phrase)
	progress(SetupStep{Label: StepWake, Running: true})
	werr := s.Wake(ctx, v.KokoroDir, w)
	if ctx.Err() != nil {
		return results, ctx.Err()
	}
	// Honest about names no model is trained for, and about a build that
	// comes without one (wakemodel.go).
	transcribed := func() string {
		return fmt.Sprintf("%s, so I listen for %q in what I transcribe, a beat slower (docs/wake-word.md shows how to train your own)", wakeByTranscript(*v), phrase)
	}
	switch {
	case werr != nil && !detectorNeeded(*v):
		// Nothing listens with the detector yet, so this isn't a fault.
		fmt.Fprintf(w, "openWakeWord didn't install (%s). Nothing uses it until there's a wake-word model; run voice setup again if you add one.\n", firstLine(werr))
		done(SetupStep{Label: StepWake, OK: true, Notes: transcribed()})
	case werr != nil:
		stepFailed(w, werr)
		done(SetupStep{Label: StepWake, Notes: fmt.Sprintf("the detector didn't install (%s). Until it does, I listen for %q in what I transcribe, which is slower", firstLine(werr), phrase)})
	case !HasWakeModel(*v):
		done(SetupStep{Label: StepWake, OK: true, Notes: transcribed()})
	default:
		done(SetupStep{Label: StepWake, OK: true, Notes: fmt.Sprintf("on-device detector for %q", phrase)})
	}

	if len(missing) > 0 {
		var hints []string
		for _, m := range missing {
			tool := "sox"
			if strings.Contains(m, "whisper") {
				tool = "whisper"
			}
			hints = append(hints, m+": "+InstallHint(s.OS, tool))
		}
		done(SetupStep{Label: StepTools, Notes: "not found: " + strings.Join(hints, "; ")})
	}
	return results, nil
}

// Hearing is what setup does about whisper.
type Hearing struct {
	Model    string // to download ("small.en", "small")
	Language string // to set when none is: "auto", or ""
	Keep     bool   // leave the model as it is
	Existing bool   // a model was already in place
	Intro    string // the step's first line
	Summary  string // its line in the summary
	Hint     string // how to change it, when the computer's language suggests it
}

// ChooseHearing picks whisper's model and language. A language set by hand
// decides. A re-run over a model that is there keeps it (the English-only
// small model, as setup always gave; a multilingual one chosen by hand is
// left alone) and never forces a language on it. A first setup on a
// computer set to another language gets the multilingual model listening
// for any language, since the owner may speak either.
func ChooseHearing(v config.Voice, name string, systemLanguage func() string) Hearing {
	var h Hearing
	if st, err := os.Stat(v.WhisperModel); err == nil && st.Size() > 0 {
		h.Existing = true
	}
	sys := systemLanguage()
	sysName := LanguageName(sys)
	switch {
	case v.Language != "":
		h.Model = WhisperModelFor(v.Language)
		name := LanguageName(v.Language)
		if v.Language == "auto" {
			name = "any language"
		}
		h.Intro = fmt.Sprintf("%s (set in your config), with whisper's %s model.", name, h.Model)
		h.Summary = fmt.Sprintf("whisper %s (%s)", h.Model, name)
	case h.Existing && !EnglishOnly(v.WhisperModel):
		h.Keep = true
		h.Intro = fmt.Sprintf("keeping whisper's %s model, as it is.", WhisperModelName(v.WhisperModel))
	case h.Existing:
		h.Model = "small.en"
		h.Intro = "English, with whisper's small.en model, as before."
		h.Summary = "whisper small.en (English)"
		if sys != "" && sys != "en" {
			h.Hint = fmt.Sprintf("Your computer is set to %s. To speak %s to %s instead of English, set channels.voice.language: %s in %s and run mirrin voice setup again.", sysName, sysName, name, sys, config.Path())
		}
	case sys != "" && sys != "en":
		h.Model, h.Language = "small", "auto"
		h.Intro = fmt.Sprintf("any language (your computer is set to %s), with whisper's multilingual small model.", sysName)
		h.Summary = "whisper small (any language)"
		h.Hint = fmt.Sprintf("Tip: if you'll always speak %s, set channels.voice.language: %s in %s for better accuracy (or channels.voice.language: en for English only), then run mirrin voice setup again.", sysName, sys, config.Path())
	default:
		h.Model = "small.en"
		h.Intro = "English, with whisper's small.en model."
		h.Summary = "whisper small.en (English)"
	}
	return h
}

// WakePhrase is how to say the wake word: "Hey Mirrin", or the word as
// configured when it already starts with "hey".
func WakePhrase(word string) string {
	word = strings.TrimSpace(word)
	if word == "" {
		word = "mirrin"
	}
	words := strings.Fields(word)
	for i, w := range words {
		r, size := utf8.DecodeRuneInString(w)
		words[i] = string(unicode.ToUpper(r)) + w[size:]
	}
	if !strings.EqualFold(words[0], "hey") {
		words = append([]string{"Hey"}, words...)
	}
	return strings.Join(words, " ")
}

// stepFailed shows a step's whole error where it happened (a pip log, say);
// the summary keeps its first line.
func stepFailed(w io.Writer, err error) {
	fmt.Fprintf(w, "That didn't work: %v\n", strings.TrimSpace(err.Error()))
}

// firstLine is an error's first line, without a trailing full stop.
func firstLine(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, ". ")
}
