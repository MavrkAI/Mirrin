package voice

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// longAnswer is about 90 spoken words: too many to follow by ear.
const longAnswer = "Your week is busy but manageable. " +
	"On Monday you have the dentist at nine and lunch with Priya at one, then the team review runs until four. " +
	"Tuesday is clear in the morning, but the landlord is coming at two to look at the boiler and you said you'd be in. " +
	"Wednesday has the quarterly planning session all afternoon, which usually overruns, so I'd keep the evening free. " +
	"Thursday and Friday are quiet apart from the gym, and Sam's birthday drinks on Friday at seven at the usual place."

// showRig is a voice channel whose screen records what it was given.
func showRig(t *testing.T, ok bool) (*rig, *[]string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("records speech with a shell command")
	}
	r := newRig(t, nil)
	var shown []string
	r.c.Screen.For = func(chatID string) func(string) bool {
		if chatID != "local" {
			return nil
		}
		return func(text string) bool { shown = append(shown, text); return ok }
	}
	return r, &shown
}

func streamed(r *rig, chatID, text string) {
	s := r.c.OpenStream(context.Background(), chatID)
	for _, w := range strings.SplitAfter(text, " ") {
		s.Write(w)
	}
	s.Close()
}

// A long answer is said as its gist and "It's all on your screen", and the
// screen gets the whole of it.
func TestALongAnswerGoesOnTheScreen(t *testing.T) {
	r, shown := showRig(t, true)
	streamed(r, "local", longAnswer)
	got := r.spoken()
	if !strings.Contains(got, "Your week is busy") || !strings.Contains(got, "It's all on your screen.") {
		t.Fatalf("said %q", got)
	}
	if strings.Contains(got, "birthday drinks") {
		t.Fatalf("the whole answer was read out: %q", got)
	}
	if n := len(strings.Fields(got)); n > gistWords+10 {
		t.Fatalf("the gist ran to %d words: %q", n, got)
	}
	if len(*shown) != 1 || (*shown)[0] != longAnswer {
		t.Fatalf("the screen got %q", *shown)
	}
	if r.c.takeShown() != longAnswer {
		t.Fatal("not kept for \"just read it to me\"")
	}
}

// A short answer is said as it always was, and nothing goes on the screen.
func TestAShortAnswerIsUnchanged(t *testing.T) {
	r, shown := showRig(t, true)
	const short = "It's eighteen degrees and cloudy. Rain later, so take a coat."
	streamed(r, "local", short)
	if got := r.spoken(); !strings.Contains(got, "eighteen degrees") || !strings.Contains(got, "take a coat") || strings.Contains(got, "screen") {
		t.Fatalf("said %q", got)
	}
	if len(*shown) != 0 {
		t.Fatalf("the screen got %q", *shown)
	}
	// A short list of two is still said.
	streamed(r, "local", "Two things today:\n- the dentist at nine\n- lunch with Priya\n")
	if got := r.spoken(); !strings.Contains(got, "the dentist at nine") || !strings.Contains(got, "lunch with Priya") {
		t.Fatalf("said %q", got)
	}
	if len(*shown) != 0 {
		t.Fatalf("the screen got %q", *shown)
	}
}

// A draft is something to look at: the lead-in and the question after it
// are said, the draft itself goes on the screen.
func TestADraftGoesOnTheScreenAndItsQuestionIsAsked(t *testing.T) {
	r, shown := showRig(t, true)
	draft := "Here's a draft for Sam.\n\nSubject: Friday\n\nHi Sam, are we still on for eight?\n\nShall I send it?"
	streamed(r, "local", draft)
	got := r.spoken()
	for _, want := range []string{"Here's a draft for Sam", "It's all on your screen.", "Shall I send it?"} {
		if !strings.Contains(got, want) {
			t.Errorf("didn't say %q: %q", want, got)
		}
	}
	if strings.Contains(got, "still on for eight") {
		t.Fatalf("read the draft out: %q", got)
	}
	if len(*shown) != 1 || !strings.Contains((*shown)[0], "Subject: Friday") {
		t.Fatalf("the screen got %q", *shown)
	}
}

// A list of three or more goes on the screen.
func TestAListGoesOnTheScreen(t *testing.T) {
	r, shown := showRig(t, true)
	streamed(r, "local", "Here's the shopping:\n- milk\n- eggs\n- bread\n- butter\n")
	if got := r.spoken(); strings.Contains(got, "butter") || !strings.Contains(got, "on your screen") {
		t.Fatalf("said %q", got)
	}
	if len(*shown) != 1 {
		t.Fatalf("the screen got %q", *shown)
	}
}

// With no screen to put it on (a call, or "just read it to me"), or when
// the screen can't be opened, every word is said.
func TestWithoutAScreenEverythingIsSaid(t *testing.T) {
	for name, setup := range map[string]func(*testing.T) *rig{
		"no screen": func(t *testing.T) *rig { r, _ := showRig(t, true); r.c.Screen.For = nil; return r },
		"a call":    func(t *testing.T) *rig { r, _ := showRig(t, true); return r },
		"it failed": func(t *testing.T) *rig { r, _ := showRig(t, false); return r },
	} {
		r := setup(t)
		chat := "local"
		if name == "a call" {
			chat = "phone"
		}
		streamed(r, chat, longAnswer)
		if got := r.spoken(); !strings.Contains(got, "birthday drinks") || strings.Contains(got, "screen") {
			t.Errorf("%s: said %q", name, got)
		}
	}
}

// "Just read it to me" reads out the answer on the screen and has long
// answers read out from then on; "put long answers on my screen" undoes it.
// Said with nothing on the screen, it is the twin's to answer.
func TestJustReadItToMe(t *testing.T) {
	r, _ := showRig(t, true)
	var aloud []bool
	r.c.Screen.ReadAloud = func(on bool) { aloud = append(aloud, on) }
	var turns []string
	handler := func(_ context.Context, in channels.Inbound) { turns = append(turns, in.Text) }
	none := func() string { return "" }

	r.c.converse(context.Background(), handler, "Just read it to me.", none)
	if len(turns) != 1 || len(aloud) != 0 {
		t.Fatalf("nothing on the screen: turns %q, aloud %v", turns, aloud)
	}

	streamed(r, "local", longAnswer)
	r.c.converse(context.Background(), handler, "Mirrin, just read it to me please", none)
	if len(turns) != 1 {
		t.Fatalf("went to the twin: %q", turns)
	}
	if len(aloud) != 1 || !aloud[0] {
		t.Fatalf("aloud %v", aloud)
	}
	if got := r.spoken(); !strings.Contains(got, "birthday drinks") || !strings.Contains(got, "read long answers out") {
		t.Fatalf("said %q", got)
	}

	r.c.converse(context.Background(), handler, "Put long answers on my screen.", none)
	if len(turns) != 1 || len(aloud) != 2 || aloud[1] {
		t.Fatalf("turns %q, aloud %v", turns, aloud)
	}
}

func TestForScreen(t *testing.T) {
	for text, want := range map[string]bool{
		"It's eighteen degrees.":                      false,
		longAnswer:                                    true,
		"Two:\n- a\n- b":                              false,
		"Three:\n- a\n- b\n- c":                       true,
		"| Day | Time |\n|---|---|\n| Mon | 9 |":      true,
		"Subject: Hello\n\nHi":                        true,
		"I'll send it at 3. Then we're done, Sam.":    false,
		"Dear Sam,\nThanks for lunch.\nBest, Akshay.": true,
	} {
		if got := ForScreen(text); got != want {
			t.Errorf("ForScreen(%q) = %v", text, got)
		}
	}
}
