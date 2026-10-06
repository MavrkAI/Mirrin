package voice

import (
	"context"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
)

func TestStripWake(t *testing.T) {
	cases := map[string]string{
		"Mirrin, what's on today?": "what's on today?",
		"Mirrin set a reminder":    "set a reminder",
		"Hey Mirrin. Lights.":      "Lights.",
		"nothing to do with it":    "",
	}
	for in, want := range cases {
		got, ok := stripWake(in, "mirrin")
		if (want == "") == ok || got != want {
			t.Errorf("%q: got %q ok=%v, want %q", in, got, ok, want)
		}
	}
}

func TestSpeakable(t *testing.T) {
	cases := map[string]string{
		"**Mirrin** here — see https://example.com now":                     "Mirrin here, see a link now.",
		"Options:\n1. Jetstar 3:50pm out, A$389\n2. Batik at 7am (AUD 295)": "Options: Jetstar 3:50 pm out, $389. Batik at 7 am ($295).",
		"## Plan\n- check *fares*\n- e.g. [Google Flights](https://g.co) 🙂": "Plan. check fares. for example Google Flights.",
		"MEL→DPS is 17°C & 40% humid":                                       "MEL to DPS is 17 degrees and 40 percent humid.",
		"Pay 349.30 AUD for Uniqlo":                                         "Pay $349.30 for Uniqlo.",
	}
	for in, want := range cases {
		if got := speakable(in); got != want {
			t.Errorf("speakable(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestSentenceBufferListsAndAbbreviations(t *testing.T) {
	var b sentenceBuffer
	var got []string
	for _, d := range []string{"Two options, e.g. these ones:\n1. Jetstar at ", "3:50pm for A$389\n2. Batik ", "at 7am\nShall I book?"} {
		got = append(got, b.Add(d)...)
	}
	got = append(got, b.Flush()...)
	want := []string{"Two options, e.g. these ones:", "1. Jetstar at 3:50pm for A$389.", "2. Batik at 7am.", "Shall I book?"}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q want %q", got, want)
		}
	}
}

func TestSentences(t *testing.T) {
	got := sentences("Yes. Your three o'clock moved to Thursday! I've sent Priya the deck… Shall I book the cab?")
	want := []string{"Yes. Your three o'clock moved to Thursday!", "I've sent Priya the deck…", "Shall I book the cab?"}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q want %q", got, want)
		}
	}
	if s := sentences("   "); s != nil {
		t.Fatalf("blank should be nil, got %q", s)
	}
}

func TestStripWakeHeyMirrin(t *testing.T) {
	got, ok := stripWake("Hey Mirrin, what's on today?", "mirrin")
	if !ok || got != "what's on today?" {
		t.Fatalf("got %q ok=%v", got, ok)
	}
	got, ok = stripWakeLoose("just a follow-up", "mirrin")
	if ok || got != "just a follow-up" {
		t.Fatalf("loose: got %q ok=%v", got, ok)
	}
}

func TestStripWakeMidTranscript(t *testing.T) {
	got, ok := stripWake("1, 2, 3, 4, 5. Hey Mirrin, stop. What day is it?", "mirrin")
	if !ok || got != "stop. What day is it?" {
		t.Fatalf("got %q ok=%v", got, ok)
	}
	if _, ok := stripWake("the Mirrin pilot flew fast", "mirrin"); ok {
		t.Fatal("bare name mid-sentence must not confirm")
	}
	if _, ok := stripWake("she said hey to the mirrin", "mirrin"); ok {
		t.Fatal("'hey' and name apart must not confirm")
	}
}

func TestSentenceBufferStreaming(t *testing.T) {
	var b sentenceBuffer
	var got []string
	for _, d := range []string{"Your 3.5 pm ", "moved to Thursday. I've ", "sent Priya the deck! Shall I book", " the cab?"} {
		got = append(got, b.Add(d)...)
	}
	got = append(got, b.Flush()...)
	want := []string{"Your 3.5 pm moved to Thursday.", "I've sent Priya the deck!", "Shall I book the cab?"}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q want %q", got, want)
		}
	}
}

func TestHallucinatedAnnotations(t *testing.T) {
	for _, x := range []string{"*coughs*", "(laughs)", "[music]", "*sighs* ...", "Thank you."} {
		if !hallucinated(x) {
			t.Errorf("%q should be treated as non-speech", x)
		}
	}
	if hallucinated("what's happening?") {
		t.Error("real speech flagged")
	}
}

func TestSentenceBufferEarlyFirstClause(t *testing.T) {
	var b sentenceBuffer
	var got []string
	for _, w := range strings.Fields("Sorry, say that again — a deadline related to something with the BBC? I don't have anything logged.") {
		got = append(got, b.Add(w+" ")...)
	}
	got = append(got, b.Flush()...)
	if len(got) < 2 || got[0] != "Sorry, say that again —" {
		t.Fatalf("first clause not spoken early: %q", got)
	}
	var b2 sentenceBuffer
	out := b2.Add("One moment.")
	if len(out) != 0 {
		t.Fatalf("no boundary yet: %q", out)
	}
	out = b2.Add("\n")
	if len(out) != 1 || out[0] != "One moment." {
		t.Fatalf("turn boundary should flush: %q", out)
	}
}

func TestStripAck(t *testing.T) {
	if got := stripAck("Let me see. what's on today"); got != "what's on today" {
		t.Fatalf("got %q", got)
	}
	if got := stripAck("set a reminder"); got != "set a reminder" {
		t.Fatalf("got %q", got)
	}
	// The twin's own "Akshaya?" picked up by the microphone goes too.
	if got := stripAck("Akshaya? what's on today", "Akshaya?"); got != "what's on today" {
		t.Fatalf("got %q", got)
	}
}

// A bare wake is answered with the owner's form of address, whatever the
// persona, and never with a "Sir?" nobody chose.
func TestBareWakeValues(t *testing.T) {
	for address, want := range map[string]string{"sir": "Sir?", "Akshaya": "Akshaya?", "ma'am": "Ma'am?", "": "Yes?", "  ": "Yes?", "boss.": "Boss?"} {
		if got := bareWakeFor(address); got != want {
			t.Errorf("bareWakeFor(%q) = %q, want %q", address, got, want)
		}
	}
	c := New(config.Voice{}, "Nyra", t.TempDir())
	if got := c.bareWake(); got != "Yes?" {
		t.Fatalf("before SetAddress: %q", got)
	}
	c.SetAddress("Akshaya")
	if got := c.bareWake(); got != "Akshaya?" {
		t.Fatalf("after SetAddress: %q", got)
	}
	if got := PreviewLine("Akshaya"); !strings.HasPrefix(got, "Good afternoon, Akshaya. Your three") {
		t.Fatalf("preview %q", got)
	}
	if got := PreviewLine(""); !strings.HasPrefix(got, "Good afternoon. Your three") {
		t.Fatalf("preview with no address %q", got)
	}
}

func TestStripWakeLongestAliasWins(t *testing.T) {
	got, ok := stripWakeWith("Hey Mirrin, how are you?", "mirrin", []string{"mirin", "hey mir"})
	if !ok || got != "how are you?" {
		t.Fatalf("got %q ok=%v", got, ok)
	}
	got, ok = stripWakeWith("hey mir, what's up", "mirrin", []string{"hey mir"})
	if !ok || got != "what's up" {
		t.Fatalf("short alias alone: got %q ok=%v", got, ok)
	}
}

func TestSentenceBufferNeverCutsAtComma(t *testing.T) {
	var b sentenceBuffer
	var got []string
	for _, w := range strings.Fields("Doing fine, sir, quiet night with nothing needing attention at all right now, and the board is clear. How about yourself?") {
		got = append(got, b.Add(w+" ")...)
	}
	got = append(got, b.Flush()...)
	if len(got) != 2 || !strings.HasPrefix(got[0], "Doing fine, sir, quiet night") {
		t.Fatalf("comma should not split: %q", got)
	}
}

func TestIsEcho(t *testing.T) {
	c := &Channel{}
	c.lastSaid.Store("One. Two. Three. Four. Five. Six.")
	if !c.isEcho("Four, five, five, five, five.") {
		t.Fatal("own words should be echo")
	}
	if c.isEcho("Actually, what day is it today?") {
		t.Fatal("user's words are not echo")
	}
}

// A blocked microphone gives pure zeros; a quiet room has a noise floor.
// The level comes from sox's stat output.
func TestMaxAmplitudeTellsABlockedMicFromAQuietRoom(t *testing.T) {
	quiet := "Samples read:              8000\nLength (seconds):      0.500000\nMaximum amplitude:     0.003052\nMinimum amplitude:    -0.002930\n"
	blocked := "Samples read:              8000\nMaximum amplitude:     0.000000\nMinimum amplitude:     0.000000\n"
	if a, ok := maxAmplitude(quiet); !ok || a <= 0 {
		t.Fatalf("quiet room: %v %v", a, ok)
	}
	if a, ok := maxAmplitude(blocked); !ok || a != 0 {
		t.Fatalf("blocked: %v %v", a, ok)
	}
	if _, ok := maxAmplitude("sox FAIL"); ok {
		t.Fatal("no stat output read as a level")
	}
}

// A recording starts over three times the room's quiet, between 0.6% and
// 6%: a laptop's own microphone a step away never reached a fixed 2%, and a
// room whose quiet peaks at 3% set off a 2% start on its own noise.
func TestStartLevelFollowsTheRoom(t *testing.T) {
	for floor, want := range map[float64]float64{0.0026: 0.0078, 0.001: 0.006, 0.01: 0.03, 0.032: 0.06} {
		if got := startLevelFor(floor); math.Abs(got-want) > 1e-9 {
			t.Errorf("room %.4f: %.4f, want %.4f", floor, got, want)
		}
	}
	c := &Channel{}
	if got := c.soundLevel(); got != "2.00%" {
		t.Fatalf("unmeasured: %s", got)
	}
	c.startLevel.Store(math.Float64bits(startLevelFor(0.0026)))
	if got := c.soundLevel(); got != "0.78%" {
		t.Fatalf("measured: %s", got)
	}
}

func TestDatesAreSaidAsPeopleSayThem(t *testing.T) {
	cases := map[string]string{
		"Your flight is on oct 2.":                   "Your flight is on October the second.",
		"Booked for Oct. 2nd at 3pm":                 "Booked for October the second at 3 pm.",
		"Fri, Oct 2, 2026 works":                     "Friday October the second, 2026 works.",
		"Due 2 Oct":                                  "Due the second of October.",
		"Thu 23rd of September and Sat 31 Jan":       "Thursday the twenty-third of September and Saturday the thirty-first of January.",
		"May 5 then June 21":                         "May the fifth then June the twenty-first.",
		"you may 2 hours or a 2 octagon by Oct 2026": "you may 2 hours or a 2 octagon by Oct 2026.",
		"I sat 3 dec":                                "I sat the third of December.",
		"Oct 32 is not a day":                        "Oct 32 is not a day.",
	}
	for in, want := range cases {
		if got := speakable(in); got != want {
			t.Errorf("speakable(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestHalfASentenceWaitsForTheRest(t *testing.T) {
	for in, want := range map[string]bool{
		"Open the...": true, "Book it for me and": true, "send it to": true, "Open the…": true,
		"open the page.": false, "Turn it on": false, "Yes": false, "What's it for?": false, "the.": false,
	} {
		if got := unfinished(in); got != want {
			t.Errorf("unfinished(%q) = %v", in, got)
		}
	}
	c := &Channel{out: io.Discard, cfg: config.Voice{FollowupSeconds: 0}}
	var heard []string
	replies := []string{"page", ""}
	c.converse(context.Background(), func(_ context.Context, in channels.Inbound) { heard = append(heard, in.Text) }, "Open the...", func() string {
		r := replies[0]
		replies = replies[1:]
		return r
	})
	if len(heard) != 1 || heard[0] != "Open the page" {
		t.Fatalf("heard %q", heard)
	}
}
