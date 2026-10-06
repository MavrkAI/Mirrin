package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tasks"
	"github.com/MavrkAI/Mirrin/internal/tools"
	"github.com/MavrkAI/Mirrin/internal/transcribe"
)

// speech stands in for whisper: check is what Check says, and each voice
// note is heard as words (or err).
type speech struct {
	mu    sync.Mutex
	heard []string // "mime:audio" per transcription
}

func stubSpeech(t *testing.T, check error, words string, err error) *speech {
	t.Helper()
	s := &speech{}
	oldHear, oldCan := hearVoice, canHear
	canHear = func(context.Context, transcribe.Options) error { return check }
	hearVoice = func(_ context.Context, audio []byte, mime string, o transcribe.Options) (string, error) {
		s.mu.Lock()
		s.heard = append(s.heard, mime+":"+string(audio))
		s.mu.Unlock()
		if o.TempDir == "" || !strings.HasPrefix(o.TempDir, filepath.Join(os.Getenv("MIRRIN_HOME"), "data")) {
			t.Errorf("voice notes are converted outside the data folder: %q", o.TempDir)
		}
		return words, err
	}
	t.Cleanup(func() { hearVoice, canHear = oldHear, oldCan })
	return s
}

// attached is an attachment whose downloads are counted.
func attached(mime string, seconds int, data []byte, fetches *atomic.Int32) *channels.Attachment {
	return &channels.Attachment{Mime: mime, Size: int64(len(data)), Seconds: seconds,
		Fetch: func(_ context.Context, max int64) ([]byte, error) {
			fetches.Add(1)
			if int64(len(data)) > max {
				return nil, channels.ErrTooBig
			}
			return data, nil
		}}
}

func voiceFrom(owner bool, seconds int, fetches *atomic.Int32) channels.Inbound {
	chat := "owner"
	if !owner {
		chat = "stranger"
	}
	return channels.Inbound{Channel: "telegram", ChatID: chat, Sender: chat, Media: channels.Voice, IsOwner: owner,
		Attachment: attached("audio/ogg", seconds, []byte("OggS voice"), fetches)}
}

// A voice note is transcribed and answered as what was said, marked as a
// voice note in the conversation.
func TestVoiceNotesAreAnsweredAsWhatWasSaid(t *testing.T) {
	td := newTestDaemon(t, func(last string, _ llm.Request) llm.Response {
		if strings.HasPrefix(last, "(voice note) ") {
			return say("I'll remind you at five.")
		}
		return say("?")
	})
	sp := stubSpeech(t, nil, "Remind me to call Mum at five", nil)
	var fetches atomic.Int32
	td.handleQueued(context.Background(), voiceFrom(true, 4, &fetches))
	if got := td.ch.messages(); len(got) != 1 || got[0] != "owner: I'll remind you at five." {
		t.Fatalf("sent %v", got)
	}
	if heard := td.llm.heard(); len(heard) != 1 || heard[0] != "(voice note) Remind me to call Mum at five" {
		t.Fatalf("the model heard %q", heard)
	}
	if len(sp.heard) != 1 || sp.heard[0] != "audio/ogg:OggS voice" {
		t.Fatalf("transcribed %v", sp.heard)
	}
	hist, _ := td.store.History(context.Background(), ownerKey, 10)
	if len(hist) != 2 || hist[0].PlainText() != "(voice note) Remind me to call Mum at five" {
		t.Fatalf("history %+v", hist)
	}
}

// A spoken "yes" is a guess at what was said: it is never an approval. The
// model hears it and can ask for a typed one.
func TestVoiceNoteYesDoesNotApprove(t *testing.T) {
	td := newTestDaemon(t, func(last string, _ llm.Request) llm.Response { return say("Type yes 1 to confirm, sir.") })
	ctx := context.Background()
	id, err := td.store.CreateApproval(ctx, ownerKey, "send", []byte(`{"to":"landlord"}`), "Email the landlord")
	if err != nil {
		t.Fatal(err)
	}
	stubSpeech(t, nil, fmt.Sprintf("Yes %d.", id), nil)
	var fetches atomic.Int32
	td.handleQueued(ctx, voiceFrom(true, 2, &fetches))
	if ap, _ := td.store.GetApproval(ctx, id); ap.Status != "pending" || len(td.ran()) != 0 {
		t.Fatalf("a voice note approved #%d: %s, ran %v", id, ap.Status, td.ran())
	}
	if heard := td.llm.heard(); len(heard) != 1 || !strings.HasPrefix(heard[0], "(voice note) Yes") {
		t.Fatalf("the model heard %q", heard)
	}
}

// Every way a voice note can't be heard gets an honest answer that says
// what to do; install steps are for the owner only, and nothing is
// downloaded that can't be used.
func TestVoiceNotesThatCantBeHeardAreAnsweredHonestly(t *testing.T) {
	for _, tt := range []struct {
		name          string
		check, err    error
		words         string
		owner         bool
		seconds       int
		want, notWant string
		fetched       int32
	}{
		{"whisper missing", &transcribe.UnavailableError{Missing: "whisper-cli"}, nil, "", true, 3, listenSetup(runtime.GOOS, "whisper-cli"), "", 0},
		{"model missing", &transcribe.UnavailableError{Missing: "the whisper model"}, nil, "", true, 3, "run \"mirrin voice setup\"", "whisper-cpp", 0},
		{"whisper missing, stranger", &transcribe.UnavailableError{Missing: "whisper-cli"}, nil, "", false, 3, channels.CantOpen(channels.Voice), "mirrin", 0},
		{"no ffmpeg", nil, transcribe.ErrNoConverter, "", true, 3, "I can't read voice notes yet: the computer I run on needs ffmpeg to open them. " + ffmpegSetup(runtime.GOOS), "play", 1},
		{"too long", nil, nil, "hi", true, 3600, "longer than I can take in (15 minutes)", "", 0},
		{"silence", nil, nil, "  ", true, 3, "couldn't make out any words", "", 1},
		{"whisper failed", nil, errors.New("whisper-cli: exit status 3"), "", true, 3, "couldn't make that voice note out", "exit status", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("the model should not be asked") })
			stubSpeech(t, tt.check, tt.words, tt.err)
			var fetches atomic.Int32
			in := voiceFrom(tt.owner, tt.seconds, &fetches)
			td.handleQueued(context.Background(), in)
			got := td.ch.messages()
			if len(got) != 1 || !strings.Contains(got[0], tt.want) || (tt.notWant != "" && strings.Contains(got[0], tt.notWant)) {
				t.Fatalf("sent %q", got)
			}
			if n := len(td.llm.heard()); n != 0 || fetches.Load() != tt.fetched {
				t.Fatalf("model asked %d times, %d downloads", n, fetches.Load())
			}
			// Kept in the conversation, so "did you get my voice note?" makes sense.
			hist, _ := td.store.History(context.Background(), in.Key(), 10)
			if len(hist) != 2 || !strings.HasPrefix(hist[0].PlainText(), "[A voice message arrived") {
				t.Fatalf("history %+v", hist)
			}
		})
	}
}

// seeingLLM is the test model on one that can look at pictures.
type seeingLLM struct{ *fakeLLM }

func (seeingLLM) SeesImages(context.Context) bool { return true }

// bigPhoto is a JPEG with a long side of 2400 pixels.
func bigPhoto(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2400, 1200))
	for x := 0; x < 2400; x++ {
		img.Set(x, x%1200, color.RGBA{200, 30, 30, 255})
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A photo is shown to the model with its caption, fitted to size and kept
// only in the data folder.
func TestPhotosAreShownToTheModel(t *testing.T) {
	var shown atomic.Value
	td := newTestDaemon(t, func(last string, req llm.Request) llm.Response {
		msgs := req.Messages
		for _, b := range msgs[len(msgs)-1].Blocks {
			if b.Type == llm.BlockImage && len(b.Image) > 0 {
				shown.Store(b.ImageType)
			}
		}
		return say("That's black mould, sir. Bleach won't cut it.")
	})
	td.agent.SetProvider(seeingLLM{td.llm})
	var fetches atomic.Int32
	in := channels.Inbound{Channel: "telegram", ChatID: "owner", Text: "is this mould?", Media: channels.Photo, IsOwner: true,
		Attachment: attached("image/jpeg", 0, bigPhoto(t), &fetches)}
	td.handleQueued(context.Background(), in)
	if got := td.ch.messages(); len(got) != 1 || !strings.Contains(got[0], "black mould") {
		t.Fatalf("sent %v", got)
	}
	if shown.Load() != "image/jpeg" {
		t.Fatal("the model never saw the photo")
	}
	if heard := td.llm.heard(); len(heard) != 1 || heard[0] != "(photo) is this mould?" {
		t.Fatalf("the model heard %q", heard)
	}
	dataDir := td.Config().DataDir
	var kept []string
	_ = filepath.WalkDir(filepath.Join(dataDir, "media"), func(p string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			kept = append(kept, p)
		}
		return nil
	})
	if len(kept) != 1 {
		t.Fatalf("kept %v", kept)
	}
	st, _ := os.Stat(kept[0])
	f, _ := os.Open(kept[0])
	cfg, _, err := image.DecodeConfig(f)
	f.Close()
	if err != nil || max(cfg.Width, cfg.Height) > channels.PhotoEdge || st.Size() > channels.PhotoBytes {
		t.Fatalf("kept photo %dx%d, %d bytes: %v", cfg.Width, cfg.Height, st.Size(), err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("photo readable by others: %v", st.Mode())
	}
}

// With a model that can't see pictures, a photo is answered honestly (and
// a caption is still answered); a file that isn't a picture says so.
func TestPhotosTheModelCantSeeAreAnsweredHonestly(t *testing.T) {
	td := newTestDaemon(t, func(last string, _ llm.Request) llm.Response {
		return say("I can't see it, sir; what does it look like?")
	})
	var fetches atomic.Int32
	photo := func(caption string, data []byte) channels.Inbound {
		return channels.Inbound{Channel: "telegram", ChatID: "owner", Text: caption, Media: channels.Photo, IsOwner: true,
			Attachment: attached("image/jpeg", 0, data, &fetches)}
	}
	ctx := context.Background()
	td.handleQueued(ctx, photo("", bigPhoto(t)))
	if got := td.ch.messages(); len(got) != 1 || !strings.Contains(got[0], "can't see photos with the model I'm running on") {
		t.Fatalf("sent %v", got)
	}
	td.handleQueued(ctx, photo("is this mould?", bigPhoto(t)))
	if heard := td.llm.heard(); len(heard) != 1 || !strings.HasPrefix(heard[0], "is this mould?\n[A photo arrived, but the model you're running on can't see pictures") {
		t.Fatalf("the model heard %q", heard)
	}
	if fetches.Load() != 0 {
		t.Fatal("downloaded a photo that couldn't be shown")
	}
	td.agent.SetProvider(seeingLLM{td.llm})
	td.handleQueued(ctx, photo("", []byte("ftypheic not a jpeg")))
	if got := td.ch.messages(); !strings.Contains(got[len(got)-1], "can't open that kind of picture") {
		t.Fatalf("sent %v", got)
	}
	if _, err := os.Stat(filepath.Join(td.Config().DataDir, "media")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(td.Config().DataDir, "media"))
		if len(entries) != 0 {
			t.Fatalf("kept something: %v", entries)
		}
	}
}

// Old photos are removed; a conversation that mentions one then says it is gone.
func TestOldPhotosArePruned(t *testing.T) {
	dir := t.TempDir()
	rel, err := saveMedia(dir, []byte("\xff\xd8\xff photo"), "image/jpeg")
	if err != nil || !strings.HasPrefix(rel, "media/") || !strings.HasSuffix(rel, ".jpg") {
		t.Fatalf("saved %q: %v", rel, err)
	}
	again, _ := saveMedia(dir, []byte("\xff\xd8\xff photo"), "image/jpeg")
	if again != rel {
		t.Fatal("the same photo is kept once")
	}
	// Audio a crash left behind goes; a voice note being transcribed stays.
	stale, busy := filepath.Join(dir, "media", "tmp", "voice-1"), filepath.Join(dir, "media", "tmp", "voice-2")
	for _, p := range []string{stale, busy} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(stale, old, old)
	pruneMedia(dir, 0)
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); !os.IsNotExist(err) {
		t.Fatalf("old photo still there: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "media"))
	if len(entries) != 1 || entries[0].Name() != "tmp" {
		t.Fatalf("left in media: %v", entries)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale audio left behind")
	}
	if _, err := os.Stat(busy); err != nil {
		t.Fatal("removed a voice note being transcribed")
	}
}

// Install steps name what is actually missing, for the computer the twin
// runs on: "mirrin voice setup" fetches the model but can't install
// whisper.cpp itself.
func TestSetupStepsSayWhatToInstall(t *testing.T) {
	for _, tt := range []struct {
		got, want, notWant string
	}{
		{listenSetup("darwin", "whisper-cli"), `run "brew install whisper-cpp" there, then "mirrin voice setup"`, ""},
		{listenSetup("linux", "whisper-cli"), "install whisper.cpp there (github.com/ggml-org/whisper.cpp)", "brew"},
		{listenSetup("darwin", "the whisper model"), `the speech-to-text model isn't on the computer I run on. To get it, run "mirrin voice setup" there.`, "brew"},
		{ffmpegSetup("darwin"), "brew install ffmpeg", ""},
		{ffmpegSetup("linux"), "sudo apt install ffmpeg", "brew"},
		{ffmpegSetup("windows"), "winget install ffmpeg", "brew"},
	} {
		if !strings.Contains(tt.got, tt.want) || (tt.notWant != "" && strings.Contains(tt.got, tt.notWant)) {
			t.Errorf("%q: want %q", tt.got, tt.want)
		}
	}
}

// oldPhoto keeps a photo under dataDir as if it came days ago, and leaves
// temporary audio behind as a crash would; it returns both paths.
func oldPhoto(t *testing.T, dataDir string, days int) (photo, audio string) {
	t.Helper()
	rel, err := saveMedia(dataDir, []byte("\xff\xd8\xff old photo"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	photo = filepath.Join(dataDir, filepath.FromSlash(rel))
	audio = filepath.Join(dataDir, "media", "tmp", "voice-crashed")
	if err := os.MkdirAll(audio, 0o700); err != nil {
		t.Fatal(err)
	}
	then := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	_ = os.Chtimes(photo, then, then)
	_ = os.Chtimes(audio, then, then)
	return photo, audio
}

func gone(p string) bool {
	_, err := os.Stat(p)
	return os.IsNotExist(err)
}

// Photos go after 30 days even when no new photo comes: an ordinary
// message, or the twin starting, clears them out.
func TestOldPhotosGoEvenWhenNoneArrive(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("Morning, sir.") })
	photo, audio := oldPhoto(t, td.Config().DataDir, 31)
	td.handleQueued(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "owner", Sender: "owner", Text: "morning", IsOwner: true})
	eventually(t, "a plain message to clear old photos", func() bool { return gone(photo) && gone(audio) })

	started := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("?") })
	photo, audio = oldPhoto(t, started.Config().DataDir, 31)
	fresh, err := saveMedia(started.Config().DataDir, []byte("\xff\xd8\xff new photo"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go started.tidyMedia(ctx)
	eventually(t, "the twin starting to clear old photos", func() bool { return gone(photo) && gone(audio) })
	if gone(filepath.Join(started.Config().DataDir, filepath.FromSlash(fresh))) {
		t.Fatal("a new photo was removed")
	}
}

// A picture sent again is kept 30 days from then, not from the first time,
// so a conversation that just mentioned it can still show it.
func TestAPhotoSentAgainIsKeptLonger(t *testing.T) {
	dir := t.TempDir()
	photo, _ := oldPhoto(t, dir, 31)
	if _, err := saveMedia(dir, []byte("\xff\xd8\xff old photo"), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	pruneMedia(dir, mediaKeep)
	if gone(photo) {
		t.Fatal("a photo sent again today was removed as 31 days old")
	}
}

// When the owner answers a task's question with a photo, the task sees the
// photo with their words: the task's run gets it (agent.CopyPhotos), not a
// note that there was one it can't see.
func TestAPhotoAnsweringATaskReachesTheTask(t *testing.T) {
	td := newTestDaemon(t, func(last string, req llm.Request) llm.Response {
		switch {
		case strings.Contains(last, "Background task started"):
			return call("q1", "task_update", `{"ask_user":"Which sofa did you mean?"}`)
		case strings.Contains(last, "Paused for the user's answer"):
			return say("Asked.")
		case strings.Contains(last, "The user replied"):
			if strings.Contains(last, "(photo) this one") && lastHasImage(req) && !strings.Contains(last, photoForTask) {
				return say("Ordered the one in the picture.")
			}
			return say("I can't see the photo; which colour is it?")
		}
		return say("?")
	})
	td.agent.SetProvider(seeingLLM{td.llm})
	if _, err := td.tasks.Start(context.Background(), ownerKey, "Sofa", "order the sofa"); err != nil {
		t.Fatal(err)
	}
	if got := td.ch.next(t); got != "Sofa: Which sofa did you mean?" {
		t.Fatalf("owner told %q", got)
	}
	var fetches atomic.Int32
	td.handleQueued(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "owner", Sender: "owner", Text: "this one",
		Media: channels.Photo, IsOwner: true, Attachment: attached("image/jpeg", 0, bigPhoto(t), &fetches)})
	if got := td.ch.next(t); got != "Thanks. Carrying on with Sofa." {
		t.Fatalf("reply %q", got)
	}
	if got := td.ch.next(t); got != "Sofa: Ordered the one in the picture." {
		t.Fatalf("the task said %q", got)
	}
	if fetches.Load() != 1 {
		t.Fatalf("the photo was fetched %d times, want once", fetches.Load())
	}
}

// lastHasImage reports whether the message a model call answers carries a
// picture.
func lastHasImage(req llm.Request) bool {
	if len(req.Messages) == 0 {
		return false
	}
	for _, b := range req.Messages[len(req.Messages)-1].Blocks {
		if b.Type == llm.BlockImage {
			return true
		}
	}
	return false
}

// MIRRIN_WHISPER_URL works for an installed service too, which gets no
// environment of the owner's: it can be saved with the secrets.
func TestWhisperServerCanBeSetInTheSecretsFile(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	t.Setenv("MIRRIN_WHISPER_URL", "")
	cfg := *config.Default()
	if got := transcribeOptions(cfg).Server; got != "" {
		t.Fatalf("nothing set: %q", got)
	}
	if err := config.SaveSecrets(map[string]string{"MIRRIN_WHISPER_URL": "off"}); err != nil {
		t.Fatal(err)
	}
	if got := transcribeOptions(cfg).Server; got != "off" {
		t.Fatalf("saved: %q", got)
	}
	t.Setenv("MIRRIN_WHISPER_URL", "http://127.0.0.1:9000")
	if got := transcribeOptions(cfg).Server; got != "http://127.0.0.1:9000" {
		t.Fatalf("exported wins: %q", got)
	}
}

// Regression (media merged with approvals' own-words answers): a voice
// note's transcript reached the model as "(voice note) yep, send that one to
// him", and resolve_approval took it as the owner's literal words, so a
// transcript approved a request. A spoken yes is a guess at what was said:
// it never approves, however it is read. A no still goes through.
func TestVoiceNoteCantSettleInItsOwnWords(t *testing.T) {
	ctx := context.Background()
	for _, words := range []string{"yep, send that one to him", "Yes 1."} {
		td := newTestDaemon(t, settler(map[string]string{"(voice note) " + words: `{"id":1,"decision":"approve"}`}))
		td.owner(t, "email the boss")
		stubSpeech(t, nil, words, nil)
		var fetches atomic.Int32
		td.handleQueued(ctx, voiceFrom(true, 2, &fetches))
		if ap, _ := td.store.GetApproval(ctx, 1); ap.Status != "pending" || len(td.ran()) != 0 {
			t.Fatalf("%q: a voice note approved #1: %s, ran %v", words, ap.Status, td.ran())
		}
		if got := td.ch.next(t); !strings.Contains(got, "voice note") || !strings.Contains(got, `"yes 1"`) {
			t.Fatalf("%q: the model was told %q", words, got)
		}
	}
	td := newTestDaemon(t, settler(map[string]string{"(voice note) nope, leave it": `{"id":1,"decision":"deny"}`}))
	td.owner(t, "email the boss")
	stubSpeech(t, nil, "nope, leave it", nil)
	var fetches atomic.Int32
	td.handleQueued(ctx, voiceFrom(true, 2, &fetches))
	if ap, _ := td.store.GetApproval(ctx, 1); ap.Status != "denied" {
		t.Fatalf("a spoken no in its own words: #1 %s; told %q", ap.Status, td.ch.messages())
	}
}

// Regression (media merged with approvals' answers from any chat): a photo
// answering a task that was started in the terminal and asked on the phone
// was shown to the chat model, and the task heard "(photo) the 8pm, this
// table" with no picture. The task that the answer goes to gets the photo.
func TestAPhotoAnsweringATerminalTaskOnThePhone(t *testing.T) {
	td := newTestDaemon(t, func(last string, req llm.Request) llm.Response {
		if strings.Contains(last, "The user replied") && lastHasImage(req) {
			return say("Booked, with the photo: " + last[strings.Index(last, "The user replied"):])
		}
		return dinner(last, req)
	})
	td.agent.SetProvider(seeingLLM{td.llm})
	if _, err := td.tasks.Start(context.Background(), "cli:terminal", "Dinner", "book a table"); err != nil {
		t.Fatal(err)
	}
	if got := td.ch.next(t); got != "Dinner: Shall I book the 7pm?" {
		t.Fatalf("owner told %q", got)
	}
	eventually(t, "the question recorded where it arrived", func() bool {
		h, _ := td.store.History(context.Background(), ownerKey, 5)
		return len(h) > 0 && strings.Contains(h[len(h)-1].PlainText(), "Shall I book the 7pm?")
	})
	var fetches atomic.Int32
	td.handleQueued(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "owner", Sender: "owner", Text: "the 8pm, this table",
		Media: channels.Photo, IsOwner: true, Attachment: attached("image/jpeg", 0, bigPhoto(t), &fetches)})
	if got := td.ch.next(t); got != "Thanks. Carrying on with Dinner." {
		t.Fatalf("reply %q", got)
	}
	if got := td.ch.next(t); !strings.Contains(got, "Booked, with the photo") || !strings.Contains(got, "(photo) the 8pm, this table") || strings.Contains(got, "can't see it here") {
		t.Fatalf("the task heard %q", got)
	}
	if fetches.Load() != 1 {
		t.Fatalf("the photo was fetched %d times, want once", fetches.Load())
	}
}

// A photo whose caption the model passes to a task with answer_task (a chat
// the task's question never reached) is named to the task as one it can't
// see, as a photo answering a task directly is.
func TestAnswerTaskSaysAPhotoCameWithIt(t *testing.T) {
	td := newTestDaemon(t, dinner)
	ctx := context.Background()
	task, err := td.tasks.Start(ctx, ownerKey, "Dinner", "book a table")
	if err != nil {
		t.Fatal(err)
	}
	td.ch.next(t) // the question, on the phone
	eventually(t, "the task to ask", func() bool {
		cur, ok := td.taskByID(task.ID)
		return ok && cur.Status == tasks.WaitingUser
	})
	if err := td.store.AppendMessage(ctx, "screen:local", llm.Text(llm.RoleUser, channels.PhotoNote("this table, at 8"))); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]string{"id": task.ID})
	if _, err := td.answerTaskTool(withArrival(ctx, time.Now().Add(time.Minute)), tools.Call{ChatKey: "screen:local", Input: in}); err != nil {
		t.Fatal(err)
	}
	if got := td.ch.next(t); !strings.Contains(got, "(photo) this table, at 8") || !strings.Contains(got, photoForTask) {
		t.Fatalf("the task heard %q", got)
	}
}

// answer_task from the turn the photo came with passes the photo to the
// task, not a note that there was one.
func TestAnswerTaskCarriesThePhoto(t *testing.T) {
	td := newTestDaemon(t, func(last string, req llm.Request) llm.Response {
		if strings.Contains(last, "The user replied") && lastHasImage(req) && !strings.Contains(last, photoForTask) {
			return say("Booked the table in the photo.")
		}
		return dinner(last, req)
	})
	td.agent.SetProvider(seeingLLM{td.llm})
	ctx := context.Background()
	task, err := td.tasks.Start(ctx, ownerKey, "Dinner", "book a table")
	if err != nil {
		t.Fatal(err)
	}
	td.ch.next(t) // the question, on the phone
	eventually(t, "the task to ask", func() bool {
		cur, ok := td.taskByID(task.ID)
		return ok && cur.Status == tasks.WaitingUser
	})
	if err := td.store.AppendMessage(ctx, "screen:local", llm.Text(llm.RoleUser, channels.PhotoNote("this table, at 8"))); err != nil {
		t.Fatal(err)
	}
	rel, err := saveMedia(td.Config().DataDir, bigPhoto(t), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]string{"id": task.ID})
	turn := agent.WithPhotos(withArrival(ctx, time.Now().Add(time.Minute)), rel)
	if _, err := td.answerTaskTool(turn, tools.Call{ChatKey: "screen:local", Input: in}); err != nil {
		t.Fatal(err)
	}
	if got := td.ch.next(t); got != "Dinner: Booked the table in the photo." {
		t.Fatalf("the task said %q", got)
	}
}

// Chat voice notes and the microphone hear the same language from the same
// setting: unset means English for both, and a set language is used as is.
func TestVoiceNotesHearTheLanguageTheMicrophoneDoes(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := *config.Default()
	cfg.Channels.Voice.Language = ""
	if got := transcribeOptions(cfg).Language; got != transcribe.SpokenLanguage("") || got != "en" {
		t.Fatalf("unset: voice notes hear %q", got)
	}
	cfg.Channels.Voice.Language = "auto"
	if got := transcribeOptions(cfg).Language; got != "auto" {
		t.Fatalf("auto: %q", got)
	}
	cfg.Channels.Voice.Language = "fr"
	if got := transcribeOptions(cfg).Language; got != "fr" {
		t.Fatalf("fr: %q", got)
	}
}

// /forget clears a conversation and the pictures it held: browser
// screenshots named in it (browser), and now the chat photos kept for it
// (media), rather than leaving those for the 30-day prune. A photo another
// conversation also shows stays for that one.
func TestForgetTakesTheConversationsPhotosWithIt(t *testing.T) {
	td := newTestDaemon(t, butler)
	data := td.Config().DataDir
	ctx := context.Background()
	keep := func(rel string) string {
		p := filepath.Join(data, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("jpeg"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mine, shared := keep("media/2026-09/aa11.jpg"), keep("media/2026-09/bb22.jpg")
	photo := func(key, rel string) {
		m := llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "(photo) look"}, {Type: llm.BlockImage, Path: rel}}}
		if err := td.store.AppendMessage(ctx, key, m); err != nil {
			t.Fatal(err)
		}
	}
	photo(ownerKey, "media/2026-09/aa11.jpg")
	photo(ownerKey, "media/2026-09/bb22.jpg")
	photo("screen:local", "media/2026-09/bb22.jpg")
	if got, _ := td.command(ctx, ownerKey, "/forget"); !strings.Contains(got, "Conversation cleared") {
		t.Fatalf("/forget: %q", got)
	}
	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Fatal("the conversation's photo outlived /forget")
	}
	if _, err := os.Stat(shared); err != nil {
		t.Fatal("a photo another conversation still shows was removed")
	}
}
