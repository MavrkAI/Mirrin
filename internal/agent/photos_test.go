package agent

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/approvals"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// seeingProvider is fakeProvider on a model that can look at pictures.
type seeingProvider struct{ *fakeProvider }

func (seeingProvider) SeesImages(context.Context) bool { return true }

func tinyJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 3)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// photoAgent is an agent whose data folder holds the photos named, each a
// small JPEG under media/.
func photoAgent(t *testing.T, p llm.Provider, photos ...string) (*Agent, *memory.Store, string) {
	t.Helper()
	dataDir := t.TempDir()
	for _, rel := range photos {
		full := filepath.Join(dataDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, tinyJPEG(t), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := memory.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := config.Default()
	cfg.DataDir = dataDir
	a := New(cfg, p, store, tools.NewRegistry(), approvals.New(cfg.Autonomy), nil)
	return a, store, dataDir
}

// images lists what the model was shown in a request: "image:<type>" for
// each picture, and the text of each note standing in for one.
func images(req llm.Request) []string {
	var out []string
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			switch {
			case b.Type == llm.BlockImage && len(b.Image) > 0:
				out = append(out, "image:"+b.ImageType)
			case b.Type == llm.BlockImage:
				out = append(out, "unloaded")
			case b.Type == llm.BlockText && strings.Contains(b.Text, "[A photo"):
				out = append(out, b.Text)
			}
		}
	}
	return out
}

// A photo goes to the model with the message it came with, stays in the
// conversation by its path (never its bytes), and is shown again on the
// next turn.
func TestPhotoGoesToTheModelWithItsMessage(t *testing.T) {
	fp := &fakeProvider{script: []llm.Response{text("That's mould, sir."), text("You're welcome.")}}
	a, store, _ := photoAgent(t, seeingProvider{fp}, "media/2026-09/mould.jpg")
	ctx := WithPhotos(context.Background(), "media/2026-09/mould.jpg")
	if _, err := a.Handle(ctx, "whatsapp:me", "(photo) is this mould?"); err != nil {
		t.Fatal(err)
	}
	last := fp.reqs[0].Messages[len(fp.reqs[0].Messages)-1]
	if len(last.Blocks) != 2 || last.Blocks[0].Type != llm.BlockImage || len(last.Blocks[0].Image) == 0 ||
		last.Blocks[0].ImageType != "image/jpeg" || last.Blocks[1].Text != "(photo) is this mould?" {
		t.Fatalf("the model was sent %+v", last.Blocks)
	}
	hist, _ := store.History(context.Background(), "whatsapp:me", 10)
	if b := hist[0].Blocks[0]; b.Type != llm.BlockImage || b.Path != "media/2026-09/mould.jpg" || b.Image != nil {
		t.Fatalf("history keeps %+v", b)
	}
	// The same turn's context doesn't attach it twice.
	if _, err := a.Handle(ctx, "whatsapp:me", "thanks"); err != nil {
		t.Fatal(err)
	}
	if got := images(fp.reqs[1]); len(got) != 1 || got[0] != "image:image/jpeg" {
		t.Fatalf("second turn shows %v", got)
	}
}

// Only the latest few photos are shown; the rest, a photo that is gone and
// every photo on a model that can't see, are described instead, and
// nothing outside the data folder's media is ever read.
func TestPhotosTheModelIsNotShownAreDescribed(t *testing.T) {
	names := []string{"media/a.jpg", "media/b.jpg", "media/c.jpg", "media/d.jpg"}
	fp := &fakeProvider{}
	a, store, dataDir := photoAgent(t, seeingProvider{fp}, names...)
	outside := filepath.Join(filepath.Dir(dataDir), "secret.jpg")
	_ = os.WriteFile(outside, tinyJPEG(t), 0o600)
	ctx := context.Background()
	// Two messages of four pictures each: the photos, then ones that can't be shown.
	for _, album := range [][]string{names, {"media/deleted.jpg", "../secret.jpg", outside, "memory.db"}} {
		m := llm.Message{Role: llm.RoleUser}
		for _, rel := range album {
			m.Blocks = append(m.Blocks, llm.ImageBlock(rel, "image/jpeg"))
		}
		m.Blocks = append(m.Blocks, llm.Block{Type: llm.BlockText, Text: "(photo)"})
		_ = store.AppendMessage(ctx, "telegram:1", m)
		_ = store.AppendMessage(ctx, "telegram:1", llm.Text(llm.RoleAssistant, "Nice."))
	}
	if _, err := a.Handle(ctx, "telegram:1", "which was best?"); err != nil {
		t.Fatal(err)
	}
	got := images(fp.reqs[0])
	want := []string{"image:image/jpeg", "image:image/jpeg", "image:image/jpeg", photoEarlier, photoGone, photoGone, photoGone, photoGone}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("shown %q\nwant  %q", got, want)
	}

	plain := &fakeProvider{}
	a.SetProvider(plain) // a model that can't see
	if _, err := a.Handle(ctx, "telegram:1", "and now?"); err != nil {
		t.Fatal(err)
	}
	for _, s := range images(plain.reqs[0]) {
		if s != photoUnseen {
			t.Fatalf("a model that can't see was shown %q", s)
		}
	}
}

// refusingProvider turns down any request with a picture in it, as a model
// does with one it can't read.
type refusingProvider struct{ seeingProvider }

func (r refusingProvider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.Type == llm.BlockImage && len(b.Image) > 0 {
				r.reqs = append(r.reqs, req)
				return nil, errors.New(`anthropic 400: {"type":"error","error":{"type":"invalid_request_error","message":"messages.0.content.0.image.source.base64: Could not process image"}}`)
			}
		}
	}
	return r.fakeProvider.Complete(ctx, req)
}

// One photo the model can't read must not break every turn after it: it is
// set aside, the owner is told plainly, and the next message is answered.
func TestAPhotoTheModelRefusesIsSetAside(t *testing.T) {
	fp := &fakeProvider{script: []llm.Response{text("Here's what I think.")}}
	a, _, _ := photoAgent(t, refusingProvider{seeingProvider{fp}}, "media/odd.jpg")
	_, err := a.Handle(WithPhotos(context.Background(), "media/odd.jpg"), "telegram:1", "(photo) what's this?")
	if msg := llm.Friendly(err); !strings.Contains(msg, "couldn't open that photo") || !strings.Contains(msg, "Send your message again") {
		t.Fatalf("the owner is told %q", msg)
	}
	out, err := a.Handle(context.Background(), "telegram:1", "what's this?")
	if err != nil || out != "Here's what I think." {
		t.Fatalf("the next message: %q %v", out, err)
	}
	if got := images(fp.reqs[len(fp.reqs)-1]); len(got) != 1 || got[0] != photoRefused {
		t.Fatalf("the refused photo was shown as %v", got)
	}
}

func TestPhotosSurviveTheToolLoop(t *testing.T) {
	fp := &fakeProvider{script: []llm.Response{toolUse("t1", "echo", `{}`), text("Done.")}}
	a, _, _ := photoAgent(t, seeingProvider{fp}, "media/x.png")
	a.Tools().Register(tools.New("echo", "echo", tools.Schema(nil), tools.RiskRead,
		func(context.Context, tools.Call) (string, error) { return "ok", nil }))
	if _, err := a.Handle(WithPhotos(context.Background(), "media/x.png"), "slack:C1", "(photo) read this"); err != nil {
		t.Fatal(err)
	}
	if len(fp.reqs) != 2 || len(images(fp.reqs[1])) != 1 || images(fp.reqs[1])[0] != "image:image/jpeg" {
		t.Fatalf("after a tool call the model was shown %v", images(fp.reqs[len(fp.reqs)-1]))
	}
}

// A photo is shown for a few turns after it came, not for the rest of the
// conversation: each model call would otherwise pay for it again.
func TestAnOldPhotoIsNotSentAgainAndAgain(t *testing.T) {
	fp := &fakeProvider{}
	a, _, _ := photoAgent(t, seeingProvider{fp}, "media/receipt.jpg")
	ctx := context.Background()
	if _, err := a.Handle(WithPhotos(ctx, "media/receipt.jpg"), "telegram:1", "(photo) keep this receipt"); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []string{"what was the total?", "and the date?", "thanks", "what's the weather?", "remind me at 5"} {
		if _, err := a.Handle(ctx, "telegram:1", msg); err != nil {
			t.Fatal(err)
		}
	}
	var shown []string
	for _, req := range fp.reqs {
		shown = append(shown, strings.Join(images(req), ""))
	}
	img := "image:image/jpeg"
	want := []string{img, img, img, photoEarlier, photoEarlier, photoEarlier}
	if strings.Join(shown, "|") != strings.Join(want, "|") {
		t.Fatalf("calls showed %q\nwant %q", shown, want)
	}
}

// A refusal that isn't about the photo leaves it alone: a model that can't
// use tools (its name may say "vision"), or a screenshot a tool returned
// that was too large.
func TestOnlyARefusalOfThePhotoSetsItAside(t *testing.T) {
	a, _, dataDir := photoAgent(t, seeingProvider{&fakeProvider{}}, "media/p.jpg")
	photo := llm.Block{Type: llm.BlockImage, Path: "media/p.jpg", Image: tinyJPEG(t), ImageType: "image/jpeg"}
	shot := llm.Block{Type: llm.BlockToolResult, ToolUseID: "t1", Text: "screenshot", Image: tinyJPEG(t), ImageType: "image/png"}
	withShot := []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{photo, {Type: llm.BlockText, Text: "(photo) book this"}}},
		{Role: llm.RoleUser, Blocks: []llm.Block{shot}}}
	photoOnly := withShot[:1]
	full := filepath.Join(dataDir, "media", "p.jpg")
	for _, tt := range []struct {
		name  string
		msgs  []llm.Message
		err   string
		aside bool
	}{
		{"a vision model without tools", photoOnly, `ollama 400: {"error":"registry.ollama.ai/library/llama3.2-vision:latest does not support tools"}`, false},
		{"the screenshot, by Claude's path", withShot, `anthropic 400: {"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.0.content.1.image.source.base64: image exceeds 5 MB maximum"}}`, false},
		{"a picture, not saying which, with a screenshot in view", withShot, `openai 400: Invalid image.`, false},
		{"the photo, by Claude's path, with a screenshot in view", withShot, `anthropic 400: {"type":"error","error":{"type":"invalid_request_error","message":"messages.0.content.0.image.source.base64: image exceeds 5 MB maximum"}}`, true},
		{"a picture, not saying which, with only the photo", photoOnly, `openai 400: Invalid image.`, true},
	} {
		refusedMu.Lock()
		delete(refused, full)
		refusedMu.Unlock()
		orig := errors.New(tt.err)
		got := a.photosRefused(tt.msgs, orig)
		refusedMu.Lock()
		aside := refused[full]
		refusedMu.Unlock()
		if aside != tt.aside || (got == orig) == tt.aside {
			t.Errorf("%s: set aside %v, error %q", tt.name, aside, llm.Friendly(got))
		}
	}
}
