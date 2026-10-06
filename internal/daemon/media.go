package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/channels/signal"
	"github.com/MavrkAI/Mirrin/internal/channels/slack"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/transcribe"
)

// What comes with a message from a chat app. A voice note is transcribed on
// this computer and answered as text marked "(voice note)"; a photo is fitted
// to size, kept under the data folder (media/) and shown to the model with
// the message. Anything the twin can't take in is answered honestly, in the
// conversation like any message, instead of being met with silence.

// Limits on what is taken in.
var (
	voiceMaxBytes   int64 = 25 << 20
	voiceMaxSeconds       = 15 * 60
	photoMaxBytes   int64 = 25 << 20 // as downloaded; fitted to channels.PhotoBytes
	mediaKeep             = 30 * 24 * time.Hour
)

// Speech-to-text, as variables so tests can stand in for whisper.
var (
	hearVoice         = transcribe.Transcribe
	canHear           = transcribe.Check
	transcribeOptions = func(cfg config.Config) transcribe.Options {
		vc := cfg.Channels.Voice
		words := append([]string{cfg.Name, cfg.User.Name}, vc.Vocabulary...)
		var prompt []string
		for _, w := range words {
			if w = strings.TrimSpace(w); w != "" {
				prompt = append(prompt, w)
			}
		}
		o := transcribe.Options{
			// A whisper server on its default port is used while it runs;
			// MIRRIN_WHISPER_URL, exported or saved in secrets.env (which an
			// installed service reads), points elsewhere, or "off".
			Server:     config.Secret("MIRRIN_WHISPER_URL"),
			Bin:        vc.WhisperBin,
			Model:      vc.WhisperModel,
			Language:   transcribe.SpokenLanguage(vc.Language), // as the microphone hears it
			TempDir:    filepath.Join(cfg.DataDir, "media", "tmp"),
			MaxSeconds: voiceMaxSeconds,
		}
		if len(prompt) > 0 {
			o.Prompt = strings.Join(prompt, ", ") + "."
		}
		return o
	}
)

// unopened is an attachment the twin couldn't take in: note stands in for
// it in the conversation (and tells the model when there is a caption to
// answer), reply answers a message that was only the attachment.
type unopened struct{ note, reply string }

// takeMedia opens what came with a message so the turn can use it. It
// returns the message as the turn should see it, with the photo on ctx, or
// answered when it has replied itself because there is nothing to answer
// without the attachment.
func (d *Daemon) takeMedia(ctx context.Context, in channels.Inbound) (context.Context, channels.Inbound, bool) {
	kind := in.Media
	if a := in.Attachment; a != nil && kind == channels.File {
		if k := channels.MediaKind(a.Mime); k == channels.Photo || k == channels.Voice {
			kind = k // a picture or a recording sent as a file
		}
	}
	if d.paused.Load() {
		// Nothing is downloaded or transcribed while paused.
		d.cantOpen(ctx, in, channels.Unopened(kind, "you were paused"), d.pausedReply(in.IsOwner))
		return ctx, in, true
	}
	var failed *unopened
	switch {
	case in.Attachment != nil && kind == channels.Voice:
		words, why := d.hear(ctx, in)
		if heardStop(ctx) {
			return ctx, in, true // it said stop, and stopped what it waited behind (voicestop.go)
		}
		if why == nil {
			in.Text, in.Media = channels.VoiceNote(words, in.Text), ""
			return ctx, in, false
		}
		failed = why
	case in.Attachment != nil && kind == channels.Photo:
		rel, why := d.keepPhoto(ctx, in)
		if why == nil {
			in.Text, in.Media = channels.PhotoNote(in.Text), ""
			return agent.WithPhotos(ctx, rel), in, false
		}
		failed = why
	case strings.TrimSpace(in.Text) != "":
		// Something this channel can't hand over, with a caption: the model
		// answers the caption, knowing what it can't see.
		in.Text, in.Media = channels.WithAttachment(in.Text, kind), ""
		return ctx, in, false
	default:
		failed = &unopened{note: channels.Received(kind), reply: channels.CantOpen(kind)}
	}
	if strings.TrimSpace(in.Text) == "" {
		d.cantOpen(ctx, in, failed.note, failed.reply)
		return ctx, in, true
	}
	in.Text, in.Media = strings.TrimSpace(in.Text)+"\n"+failed.note, ""
	return ctx, in, false
}

// forgetConversationPhotos removes the photos a conversation holds (its
// image blocks' media/… files), as /forget clears it, rather than leave them
// for the 30-day prune. A picture another conversation also shows (photos
// are kept by content) stays for that one.
func (d *Daemon) forgetConversationPhotos(ctx context.Context, key string) int {
	h, err := d.store.History(ctx, key, 1<<30)
	if err != nil {
		return 0
	}
	dataDir := d.Config().DataDir
	n := 0
	for _, m := range h {
		for _, b := range m.Blocks {
			rel := path.Clean(b.Path)
			if b.Type != llm.BlockImage || !strings.HasPrefix(rel, "media/") || strings.Contains(rel, "..") {
				continue
			}
			if shared, err := d.store.MentionedElsewhere(ctx, key, rel); err != nil || shared {
				continue
			}
			if os.Remove(filepath.Join(dataDir, filepath.FromSlash(rel))) == nil {
				n++
			}
		}
	}
	return n
}

// photoForTask tells a task that a photo came with the owner's answer when
// the picture itself can't go with it (answer_task from a turn that no
// longer holds it). A photo that answers a task directly goes to the task
// (answerWaitingTask).
const photoForTask = "[A photo came with this reply, but you can't see it here. If it matters, ask them to describe it.]"

// hear transcribes a voice note, or says why it couldn't. One already
// being heard while it waited its turn (voicestop.go) is heard only once.
func (d *Daemon) hear(ctx context.Context, in channels.Inbound) (string, *unopened) {
	if pre, ok := ctx.Value(preheardKey{}).(*preheard); ok {
		select {
		case <-pre.done:
			return pre.words, pre.why
		case <-ctx.Done():
			return "", &unopened{note: channels.Unopened(channels.Voice, "it couldn't be transcribed"), reply: "I couldn't make that voice note out. Could you send it again, or type it?"}
		}
	}
	a, label := in.Attachment, channelLabel(in.Channel)
	tooLong := &unopened{
		note:  channels.Unopened(channels.Voice, "it was too long to transcribe"),
		reply: fmt.Sprintf("That voice note is longer than I can take in (%d minutes). Could you send a shorter one, or type the gist?", voiceMaxSeconds/60),
	}
	if a.Seconds > voiceMaxSeconds {
		return "", tooLong
	}
	cfg := d.Config()
	o := transcribeOptions(cfg)
	// Before downloading: is there anything to transcribe it with?
	if err := canHear(ctx, o); err != nil {
		return "", d.cantHear(in, err)
	}
	data, err := a.Fetch(ctx, voiceMaxBytes)
	if errors.Is(err, channels.ErrTooBig) {
		return "", tooLong
	}
	if err != nil {
		if why := mediaSetupFailure(in, channels.Voice, err); why != nil {
			return "", why
		}
		d.log.Warn("voice note: download", "chat", in.Key(), "err", err)
		return "", &unopened{
			note:  channels.Unopened(channels.Voice, "it couldn't be downloaded"),
			reply: "I couldn't download that voice note from " + label + ". Could you send it again?",
		}
	}
	tctx, cancel := context.WithTimeout(ctx, transcribe.Timeout(a.Seconds))
	defer cancel()
	words, err := hearVoice(tctx, data, a.Mime, o)
	if err != nil {
		d.log.Warn("voice note: transcribe", "chat", in.Key(), "err", err)
		return "", d.cantHear(in, err)
	}
	if strings.TrimSpace(words) == "" {
		return "", &unopened{
			note:  channels.Unopened(channels.Voice, "no words could be made out in it"),
			reply: "I couldn't make out any words in that voice note. Could you try again, or type it?",
		}
	}
	return words, nil
}

// cantHear explains a voice note that couldn't be transcribed. Only the
// owner is told what to install on their computer.
func (d *Daemon) cantHear(in channels.Inbound, err error) *unopened {
	var missing *transcribe.UnavailableError
	switch {
	case errors.As(err, &missing):
		u := &unopened{note: channels.Unopened(channels.Voice, "speech-to-text isn't set up on this computer yet"), reply: channels.CantOpen(channels.Voice)}
		if in.IsOwner {
			u.reply = listenSetup(runtime.GOOS, missing.Missing)
		}
		return u
	case errors.Is(err, transcribe.ErrNoConverter):
		u := &unopened{note: channels.Unopened(channels.Voice, "this computer can't decode the audio yet"), reply: channels.CantOpen(channels.Voice)}
		if in.IsOwner {
			u.reply = "I can't read voice notes yet: the computer I run on needs ffmpeg to open them. " + ffmpegSetup(runtime.GOOS) + " Until then, could you type it for me?"
		}
		return u
	}
	return &unopened{
		note:  channels.Unopened(channels.Voice, "it couldn't be transcribed"),
		reply: "I couldn't make that voice note out. Could you send it again, or type it?",
	}
}

// listenSetup tells the owner how to add what speech-to-text is missing on
// the computer the twin runs on (goos).
func listenSetup(goos, missing string) string {
	const until = " Until then, could you type it for me?"
	if missing == "the whisper model" {
		return "I can't listen to voice notes yet: the speech-to-text model isn't on the computer I run on. To get it, run \"mirrin voice setup\" there." + until
	}
	how := "install whisper.cpp there (github.com/ggml-org/whisper.cpp), then run \"mirrin voice setup\"."
	if goos == "darwin" {
		how = "run \"brew install whisper-cpp\" there, then \"mirrin voice setup\"."
	}
	return "I can't listen to voice notes yet: speech-to-text (whisper.cpp) isn't installed on the computer I run on. To add it, " + how + until
}

// ffmpegSetup says how to install ffmpeg on goos.
func ffmpegSetup(goos string) string {
	switch goos {
	case "darwin":
		return "To add it, run \"brew install ffmpeg\" there."
	case "windows":
		return "To add it, run \"winget install ffmpeg\" there."
	}
	return "To add it, install ffmpeg there (for example \"sudo apt install ffmpeg\")."
}

// keepPhoto fetches a photo, fits it to what models take and keeps it under
// the data folder. It returns the path relative to the data folder, or why
// the photo can't be looked at.
func (d *Daemon) keepPhoto(ctx context.Context, in channels.Inbound) (string, *unopened) {
	cfg := d.Config()
	if p := d.agent.Provider(); !llm.IsUnavailable(p) && !llm.SeesImages(ctx, p) {
		u := &unopened{note: channels.Unopened(channels.Photo, "the model you're running on can't see pictures"), reply: channels.CantOpen(channels.Photo)}
		if in.IsOwner {
			u.reply = fmt.Sprintf("I can't see photos with the model I'm running on (%s). Could you tell me in words what you need? Choose a model that can see pictures from my menu and I'll be able to look.", cfg.LLM.Model)
		}
		return "", u
	}
	tooBig := &unopened{note: channels.Unopened(channels.Photo, "it was too large to open"), reply: "That picture is too large for me to open. Could you send a smaller one?"}
	data, err := in.Attachment.Fetch(ctx, photoMaxBytes)
	if errors.Is(err, channels.ErrTooBig) {
		return "", tooBig
	}
	if err != nil {
		if why := mediaSetupFailure(in, channels.Photo, err); why != nil {
			return "", why
		}
		d.log.Warn("photo: download", "chat", in.Key(), "err", err)
		return "", &unopened{
			note:  channels.Unopened(channels.Photo, "it couldn't be downloaded"),
			reply: "I couldn't download that photo from " + channelLabel(in.Channel) + ". Could you send it again?",
		}
	}
	fit, mime, err := channels.FitImage(data, channels.PhotoEdge, channels.PhotoBytes)
	switch {
	case errors.Is(err, channels.ErrTooBig):
		return "", tooBig
	case err != nil:
		return "", &unopened{
			note:  channels.Unopened(channels.Photo, "it isn't a kind of picture you can open"),
			reply: "I can't open that kind of picture. Could you send it as an ordinary photo (JPEG or PNG)?",
		}
	}
	rel, err := saveMedia(cfg.DataDir, fit, mime)
	if err != nil {
		d.log.Error("photo: save", "err", err)
		return "", &unopened{
			note:  channels.Unopened(channels.Photo, "it couldn't be saved on this computer"),
			reply: "I couldn't save that photo (is the disk full?). Could you tell me in words what you need?",
		}
	}
	return rel, nil
}

// saveMedia writes a picture under dataDir/media/<year-month>/, named by its
// content, readable only by the owner, and returns its path relative to
// dataDir (as history keeps it).
func saveMedia(dataDir string, data []byte, mime string) (string, error) {
	ext := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp"}[mime]
	if ext == "" {
		return "", fmt.Errorf("not a picture: %s", mime)
	}
	sum := sha256.Sum256(data)
	rel := path.Join("media", clock().Format("2006-01"), hex.EncodeToString(sum[:12])+ext)
	full := filepath.Join(dataDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return "", err
	}
	if _, err := os.Stat(full); err == nil {
		// The same picture again: it is kept for as long from now on.
		now := time.Now()
		_ = os.Chtimes(full, now, now)
		return rel, nil
	}
	tmp := full + ".part"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return rel, os.Rename(tmp, full)
}

var (
	pruneMu   sync.Mutex
	prunedAt  = map[string]time.Time{}
	pruneEach = 6 * time.Hour
)

// tidyMedia removes old photos when the twin starts and every few hours
// while it runs, whether or not anything new arrives.
func (d *Daemon) tidyMedia(ctx context.Context) {
	for {
		pruneMedia(d.Config().DataDir, mediaKeep)
		select {
		case <-ctx.Done():
			return
		case <-time.After(pruneEach):
		}
	}
}

// pruneMedia removes kept photos older than keep (at most every few hours),
// and any temporary audio a crash left behind. A conversation that still
// mentions a removed photo tells the model it is gone.
func pruneMedia(dataDir string, keep time.Duration) {
	pruneMu.Lock()
	if time.Since(prunedAt[dataDir]) < pruneEach {
		pruneMu.Unlock()
		return
	}
	prunedAt[dataDir] = time.Now()
	pruneMu.Unlock()
	root := filepath.Join(dataDir, "media")
	months, _ := os.ReadDir(root)
	for _, m := range months {
		dir := filepath.Join(root, m.Name())
		if !m.IsDir() {
			continue
		}
		if m.Name() == "tmp" {
			// Audio a voice note left behind when the twin stopped mid-way.
			tmps, _ := os.ReadDir(dir)
			for _, e := range tmps {
				if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
					_ = os.RemoveAll(filepath.Join(dir, e.Name()))
				}
			}
			continue
		}
		photos, _ := os.ReadDir(dir)
		for _, e := range photos {
			if info, err := e.Info(); err == nil && !e.IsDir() && time.Since(info.ModTime()) > keep {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
		_ = os.Remove(dir) // an emptied month folder; fails, harmlessly, while it holds anything
	}
}

// cantOpen answers a message that was only an attachment the twin couldn't
// take in (a voice note, a photo). It goes the way any reply does: audited,
// answered with the pause notice while paused, and kept in the conversation
// (note stands in for the attachment), so "did you get my voice note?"
// makes sense afterwards.
func (d *Daemon) cantOpen(ctx context.Context, in channels.Inbound, note, reply string) {
	key := in.Key()
	reply = func() string {
		c := d.conv(key)
		c.begin()
		defer d.end(key, c)
		d.store.Audit(ctx, "message.in", key, note)
		if d.paused.Load() {
			paused := d.pausedReply(in.IsOwner)
			c.noteReply(key, paused, in.IsOwner)
			return paused
		}
		for _, m := range []llm.Message{llm.Text(llm.RoleUser, note), llm.Text(llm.RoleAssistant, reply)} {
			if err := d.store.AppendMessage(ctx, key, m); err != nil {
				d.log.Warn("record attachment", "chat", key, "err", err)
			}
		}
		c.noteReply(key, reply, in.IsOwner) // it asks nothing: a later bare yes isn't about an earlier question
		return reply
	}()
	if err := d.Send(ctx, key, reply); err != nil {
		d.log.Error("send", "chat", key, "err", err)
	}
}

// Only the owner sees setup details, and captions keep the same actionable hint.
func mediaSetupFailure(in channels.Inbound, kind string, err error) *unopened {
	var help string
	switch {
	case errors.Is(err, slack.ErrFilePermission):
		help = "Slack needs the files:read permission to open attachments. At https://api.slack.com/apps, choose your app, add files:read under OAuth & Permissions, then reinstall the app."
	case errors.Is(err, signal.ErrAttachmentUpgrade):
		help = "Update signal-cli on the computer running Mirrin, then reconnect Signal to open attachments."
	case errors.Is(err, signal.ErrAttachmentSizeUnknown):
		help = "Signal didn't report this attachment's size. Update signal-cli and reconnect Signal, or type the message for now."
	default:
		return nil
	}
	u := &unopened{note: channels.Unopened(kind, "the channel needs attention"), reply: "I can't open attachments here right now. Could you type the message for now?"}
	if in.IsOwner {
		u.reply = help
		u.note += " " + help
	}
	return u
}
