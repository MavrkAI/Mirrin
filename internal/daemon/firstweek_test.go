package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/skills/google/googletest"
)

// echo is a model that says back the task it was given, without the
// heartbeat's note before it (which names NOTHING_TO_REPORT).
func echo(last string, _ llm.Request) llm.Response {
	_, task, _ := strings.Cut(last, "]\n\n")
	return say(task)
}

// onTourDay makes today the given day of the first-week tour.
func onTourDay(t *testing.T, td *testDaemon, day int) {
	t.Helper()
	ctx := context.Background()
	start := time.Now().Add(-time.Duration(day-1)*24*time.Hour - time.Hour)
	if err := td.store.Set(ctx, "installed_at", start.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	_ = td.store.Unset(ctx, "nudge_done")
}

// The wake-word tip names the persona's own wake word, and waits for voice
// to be set up: Nyra's owner is told "Hey Nyra", never "Hey Mirrin".
func TestTheVoiceTipUsesThePersonasWakeWord(t *testing.T) {
	ctx := context.Background()
	td := newTestDaemon(t, echo)
	// Nyra as the welcome window sets her up: the twin is called Nyra.
	if err := td.UpdateConfig(func(c *config.Config) { c.Persona, c.Name = "nyra", "Nyra" }); err != nil {
		t.Fatal(err)
	}

	onTourDay(t, td, 5)
	td.nudge(ctx)
	if heard, sent := td.llm.heard(), td.ch.messages(); len(heard) != 0 || len(sent) != 0 {
		t.Fatalf("a voice tip before voice is set up: asked %q, sent %q", heard, sent)
	}
	if done, _ := td.store.Get(ctx, "nudge_done"); done != "5" {
		t.Fatalf("the skipped day isn't done: %q", done)
	}

	// Set up: the speech model is on disk.
	model := filepath.Join(t.TempDir(), "ggml-base.en.bin")
	if err := os.WriteFile(model, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := td.UpdateConfig(func(c *config.Config) { c.Channels.Voice.WhisperModel = model }); err != nil {
		t.Fatal(err)
	}
	if !td.connected().Voice {
		t.Fatal("setup: voice isn't set up")
	}
	onTourDay(t, td, 5)
	td.nudge(ctx)
	got := td.ch.next(t)
	if !strings.Contains(got, `"Hey Nyra"`) || strings.Contains(strings.ToLower(got), "hey mirrin") {
		t.Fatalf("the day-5 tip asked for %q", got)
	}
	if !strings.Contains(got, "One message, short, in character.") {
		t.Fatalf("the tip isn't asked for in character: %q", got)
	}
}

// The calendar and inbox tip names what isn't connected yet, and is
// skipped once both are.
func TestTheCalendarTipIsSkippedOnceBothAreConnected(t *testing.T) {
	ctx := context.Background()
	td := newTestDaemon(t, echo)
	if task, ok := td.nudgeTask(3); !ok || !strings.Contains(task, "Not connected yet: their calendar and their email.") {
		t.Fatalf("with nothing connected, day 3 asks %q (%v)", task, ok)
	}

	fake := googletest.NewInProcess()
	td.google.Transport = fake.Transport()
	t.Cleanup(func() { settleGoogle(td) })
	fake.WriteFiles(t, td.google.CredentialsFile, td.google.TokenFile, time.Now().Add(time.Hour))
	off := false
	if err := td.UpdateConfig(func(c *config.Config) {
		c.Skills.Calendar.Enabled, c.Skills.Gmail.Enabled = true, true
		c.UI.Weather = &off // no call to the weather service
	}); err != nil {
		t.Fatal(err)
	}
	td.applyGoogle()
	if c := td.connected(); c != (Connected{Calendar: true, Mail: true}) {
		t.Fatalf("connected %+v", c)
	}
	if sd := td.screenData(ctx); sd.Connected != (Connected{Calendar: true, Mail: true}) {
		t.Fatalf("the screen's connected %+v", sd.Connected)
	}

	onTourDay(t, td, 3)
	td.nudge(ctx)
	if heard, sent := td.llm.heard(), td.ch.messages(); len(heard) != 0 || len(sent) != 0 {
		t.Fatalf("day 3 with both connected: asked %q, sent %q", heard, sent)
	}
	if done, _ := td.store.Get(ctx, "nudge_done"); done != "3" {
		t.Fatalf("the skipped day isn't done: %q", done)
	}
	// One a day: day 4 still comes, and only once.
	onTourDay(t, td, 4)
	td.nudge(ctx)
	td.nudge(ctx)
	if sent := td.ch.messages(); len(sent) != 1 || !strings.Contains(sent[0], "Day 4") {
		t.Fatalf("day 4: %q", sent)
	}
}

// "No more tips" on the screen ends the tour, and the tour ends after day 7
// anyway.
func TestNoMoreTipsEndsTheTour(t *testing.T) {
	ctx := context.Background()
	td := newTestDaemon(t, echo)
	onTourDay(t, td, 8)
	td.nudge(ctx)
	if heard := td.llm.heard(); len(heard) != 0 {
		t.Fatalf("a tip on day 8: %q", heard)
	}

	onTourDay(t, td, 1)
	if err := td.StopTips(ctx); err != nil {
		t.Fatal(err)
	}
	if off, _ := td.store.Get(ctx, "nudges_off"); off != "1" {
		t.Fatalf("nudges_off %q", off)
	}
	td.nudge(ctx)
	if heard, sent := td.llm.heard(), td.ch.messages(); len(heard) != 0 || len(sent) != 0 {
		t.Fatalf("a tip after No more tips: asked %q, sent %q", heard, sent)
	}
}
