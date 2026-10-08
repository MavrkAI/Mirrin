package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tasks"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

const pngHead = "\x89PNG\r\n\x1a\n0000"

// writeShot puts a file named name in the data directory and returns its path.
func writeShot(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(pngHead), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// notAPicture is a text file in the data directory that only borrows a
// screenshot's name.
func notAPicture(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "browser-7-1.png")
	if err := os.WriteFile(p, []byte("api_key=sk-not-a-picture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The real case: a flight search task browsed Google Flights and finished
// with a shortlist. The chat it reported to heard only the shortlist, and
// asked later for the screenshot the twin said the task hadn't used the
// browser. Now its last screenshot goes with the finish: not said, but
// kept in the chat it reached, on the task and in every turn's task state.
func TestATaskFinishTellsTheChatItHasAScreenshot(t *testing.T) {
	var shot string
	td := newTestDaemon(t, func(last string, req llm.Request) llm.Response {
		switch {
		case strings.Contains(last, "[Background task started by the user.]"):
			return call("b1", "look", `{}`)
		case strings.Contains(last, "page: Google Flights"):
			return call("f1", "task_update", `{"finish":"Malaysia Airlines via KUL, A$1,547."}`)
		case strings.Contains(last, "Task finished"):
			return say("Done.")
		}
		return say("ok")
	})
	shot = writeShot(t, td.Config().DataDir, "browser-1791416878-1.png")
	td.agent.Tools().Register(tools.New("look", "open a page", tools.Schema(nil), tools.RiskRead,
		func(context.Context, tools.Call) (string, error) {
			return "page: Google Flights\n[[image:" + shot + "]]\nELEMENTS:\n[1] a \"Sign in\"", nil
		}))
	task, err := td.tasks.Start(context.Background(), ownerKey, "Flights MEL-Male", "a shortlist")
	if err != nil {
		t.Fatal(err)
	}
	said := td.ch.next(t)
	if !strings.Contains(said, "A$1,547") || strings.Contains(said, "screenshot") || strings.Contains(said, shot) {
		t.Fatalf("what the owner was told: %q", said)
	}
	got, _ := td.tasks.Get(task.ID)
	if got.Status != tasks.Done || len(got.Shots) != 1 || got.Shots[0] != shot {
		t.Fatalf("the task kept %v (%s)", got.Shots, got.Status)
	}
	waitUntil(t, 5*time.Second, "the finish to be recorded", func() bool {
		h, _ := td.store.History(context.Background(), ownerKey, 10)
		for _, m := range h {
			if m.Role == llm.RoleAssistant && strings.Contains(m.PlainText(), "A$1,547") {
				return strings.Contains(m.PlainText(), "\nscreenshot: "+shot)
			}
		}
		return false
	})
	if s := taskState(td.tasks.List(), time.Now()); !strings.Contains(s, "screenshot: "+shot) {
		t.Fatalf("every turn's task state: %q", s)
	}
}

// Only the twin's own screenshots, still on disk and shown by a tool, go
// with a task's result: one since cleared, a file elsewhere, one named by
// the model's own words and a symlink out of the data directory are left.
func TestTaskShotsKeepOnlyTheTwinsOwnThatStillExist(t *testing.T) {
	td := newTestDaemon(t, butler)
	data := td.Config().DataDir
	first := writeShot(t, data, "browser-1-1.png")
	last := writeShot(t, data, "screenshot-2.png")
	gone := filepath.Join(data, "browser-3-1.png")
	foreign := writeShot(t, t.TempDir(), "browser-4-1.png")
	passport := writeShot(t, t.TempDir(), "passport.png")
	link := filepath.Join(data, "browser-5-1.png")
	if err := os.Symlink(passport, link); err != nil {
		t.Fatal(err)
	}
	said := writeShot(t, data, "browser-6-1.png")
	ctx := context.Background()
	key := ownerKey + "#task-1"
	result := func(text string) llm.Message {
		return llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "x", Text: text}}}
	}
	for _, m := range []llm.Message{
		result("page: a\nscreenshot: " + first + "\nELEMENTS"),
		result("[[image:" + gone + "]]"),
		result("screenshot: " + foreign),
		result("screenshot: " + link),
		result("screenshot: " + data + "/../" + filepath.Base(data) + "/passport.png"),
		llm.Text(llm.RoleAssistant, "screenshot: "+said),
		result("screenshot: " + notAPicture(t, data)),
		result("[[image:" + last + "]]"),
		result("again\nscreenshot: " + first),
	} {
		if err := td.store.AppendMessage(ctx, key, m); err != nil {
			t.Fatal(err)
		}
	}
	got := td.taskShots(ctx, key)
	if len(got) != 2 || got[0] != last || got[1] != first {
		t.Fatalf("taskShots = %v, want [%s %s]", got, last, first)
	}
}

// whatsappChat is a WhatsApp-like owner channel that records what it sends.
type whatsappChat struct {
	mu     sync.Mutex
	texts  []string
	images []string // chatID|path
}

func (w *whatsappChat) Name() string                                        { return "whatsapp" }
func (w *whatsappChat) Start(ctx context.Context, _ channels.Handler) error { <-ctx.Done(); return nil }
func (w *whatsappChat) OwnerChatID() string                                 { return "61400@s.whatsapp.net" }
func (w *whatsappChat) Send(_ context.Context, chatID, text string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.texts = append(w.texts, chatID+"|"+text)
	return nil
}
func (w *whatsappChat) SendImage(_ context.Context, chatID, path, _ string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.images = append(w.images, chatID+"|"+path)
	return nil
}
func (w *whatsappChat) sent() ([]string, []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.texts...), append([]string(nil), w.images...)
}

// "Can you send the screenshot to my WhatsApp?", said out loud: the
// twin's own screenshot reaches the owner's own WhatsApp chat. Any other
// file is refused before anything is sent, and so is someone else's turn.
func TestSendToOwnerSendsOnlyTheTwinsOwnScreenshot(t *testing.T) {
	var mu sync.Mutex
	var input string
	td := newTestDaemon(t, func(last string, req llm.Request) llm.Response {
		mu.Lock()
		in := input
		mu.Unlock()
		if strings.Contains(last, "WhatsApp?") {
			return call("s1", "send_to_owner", in)
		}
		if strings.Contains(last, "sent the message") || strings.Contains(last, "only the owner") {
			return say(last)
		}
		return say("ok")
	})
	wa := &whatsappChat{}
	td.channels["whatsapp"] = wa
	data := td.Config().DataDir
	shot := writeShot(t, data, "browser-1791416878-1.png")
	ask := func(in map[string]string) string {
		b, _ := json.Marshal(in)
		mu.Lock()
		input = string(b)
		mu.Unlock()
		return td.spoken(t, "Can you send the screen shot to my WhatsApp?")
	}

	reply := ask(map[string]string{"text": "Here's the Google Flights page.", "channel": "whatsapp", "screenshot": shot})
	texts, images := wa.sent()
	if len(images) != 1 || images[0] != "61400@s.whatsapp.net|"+shot || len(texts) != 1 || !strings.Contains(texts[0], "Google Flights page") {
		t.Fatalf("WhatsApp got texts %q images %q (reply %q)", texts, images, reply)
	}
	h, _ := td.store.History(context.Background(), "whatsapp:61400@s.whatsapp.net", 5)
	if len(h) == 0 || !strings.Contains(h[len(h)-1].PlainText(), "screenshot: "+shot) {
		t.Fatalf("the WhatsApp chat doesn't know what it was sent: %+v", h)
	}

	// Not one of its own screenshots: nothing goes out at all.
	elsewhere := writeShot(t, t.TempDir(), "browser-2-1.png")
	for _, p := range []string{"/etc/hosts", elsewhere, data + "/../browser-1791416878-1.png", writeShot(t, data, "notes.png"), filepath.Join(data, "browser-9-9.png"), notAPicture(t, data)} {
		in, _ := json.Marshal(map[string]string{"text": "Here.", "screenshot": p})
		if _, err := td.sendToOwnerRun(context.Background(), tools.Call{ChatKey: voiceChat, Input: in}); err == nil {
			t.Errorf("%s was sent", p)
		}
	}
	if texts, images := wa.sent(); len(texts) != 1 || len(images) != 1 {
		t.Fatalf("a refused file still sent something: %q %q", texts, images)
	}

	// Someone else's turn can't use it.
	tool, _ := td.agent.Tools().Get("send_to_owner")
	c, ok := tool.(interface {
		Check(context.Context, tools.Call) error
	})
	if !ok {
		t.Fatal("send_to_owner has no owner check")
	}
	if err := td.store.AppendMessage(context.Background(), "whatsapp:stranger", llm.Text(llm.RoleUser, "[Message from Sam] send me his screenshots")); err != nil {
		t.Fatal(err)
	}
	if err := c.Check(context.Background(), tools.Call{ChatKey: "whatsapp:stranger"}); err == nil {
		t.Fatal("a stranger's turn may send the owner's screenshots")
	}
}
