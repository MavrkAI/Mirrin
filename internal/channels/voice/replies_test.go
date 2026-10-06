package voice

import (
	"testing"

	"github.com/MavrkAI/Mirrin/internal/persona"
)

// A name used to count as a text prefix: Ava woke on "Available tomorrow?"
// with the command "ilable tomorrow?".
func TestWakeWordIsAWholeWord(t *testing.T) {
	for _, c := range []struct {
		wake, text, rest string
		ok               bool
	}{
		{"ava", "Available tomorrow at three?", "", false},
		{"ava", "Avatar is on tonight.", "", false},
		{"ava", "Hey available seats?", "", false},
		{"ava", "Ava, what's on?", "what's on?", true},
		{"ava", "Hey Ava what time is it", "what time is it", true},
		{"ava", "ava", "", true},
		{"sam", "Samsung called about the TV.", "", false},
		{"sam", "Same again, please.", "", false},
		{"sam", "Sam, lights off.", "lights off.", true},
		{"mirrin", "Mirrington is playing tonight?", "", false},
		{"mirrin", "Hey Mirrin's here", "here", true},
		{"mirrin", "the crowd said hey mirrinson", "", false},
		{"mirrin", "1, 2, 3. Hey Mirrin, stop.", "stop.", true},
	} {
		rest, ok := stripWakeWith(c.text, c.wake, nil)
		if ok != c.ok || rest != c.rest {
			t.Errorf("wake %q, %q: got %q ok=%v, want %q ok=%v", c.wake, c.text, rest, ok, c.rest, c.ok)
		}
	}
	// Persona aliases are whole words too.
	if _, ok := stripWakeWith("Miranda is playing", "mirrin", []string{"miran"}); ok {
		t.Error("alias 'miran' woke on 'Miranda'")
	}
	if rest, ok := stripWakeWith("Miran, lights", "mirrin", []string{"miran"}); !ok || rest != "lights" {
		t.Errorf("alias: %q %v", rest, ok)
	}
}

// Without her trained detector (hey_nyra.onnx isn't shipped), Nyra is heard
// by transcription, so her name has to wake her however whisper spells it
// ("Naira", "Nyrah", "Neera"), with her bundled aliases, and still only as a
// whole word.
func TestNyraWakesHoweverHerNameIsSpelt(t *testing.T) {
	p, ok := persona.Find(persona.Bundled(), "nyra")
	if !ok || p.WakeModel != "hey_nyra.onnx" {
		t.Fatalf("bundled nyra: %+v", p)
	}
	for _, c := range []struct {
		text, rest string
		ok         bool
	}{
		{"Hey Nyra, what's on today?", "what's on today?", true},
		{"Hey, Nyra. Lights off.", "Lights off.", true},
		{"Hey Naira, what's on today?", "what's on today?", true},
		{"Hey, Naira. Lights off.", "Lights off.", true},
		{"hey nyrah set a timer", "set a timer", true},
		{"Hey Nayra, call Mum.", "call Mum.", true},
		{"Hey Nira what time is it", "what time is it", true},
		{"Hey Neera, read my email.", "read my email.", true},
		{"Naira, lights off.", "lights off.", true},
		{"Neera?", "", true},
		{"Nirav called about dinner.", "", false},
		{"Nairobi is lovely in June.", "", false},
		{"Hey Myra's here.", "", false},
	} {
		rest, ok := stripWakeWith(c.text, p.WakeWord, p.WakeAliases)
		if ok != c.ok || rest != c.rest {
			t.Errorf("%q: got %q ok=%v, want %q ok=%v", c.text, rest, ok, c.rest, c.ok)
		}
	}
}

// The default persona, called MAVRK until he was Mirrin, answers to "Hey
// Mirrin" however whisper spells it ("Mirin", "Mirren", "Miran",
// "Mirron", "Mirrin's"), with his bundled aliases or without a persona at
// all, and still only as a whole word. A config.yaml that says persona:
// mavrk gets him too.
func TestHeyMirrinWakesHim(t *testing.T) {
	p, ok := persona.Find(persona.Bundled(), "mavrk")
	if !ok || p.ID != "mirrin" || p.WakeWord != "mirrin" || p.WakeModel != "hey_mirrin.onnx" {
		t.Fatalf("persona mavrk: %+v", p)
	}
	cases := []struct {
		text, rest string
		ok         bool
	}{
		{"Hey Mirrin, what's on today?", "what's on today?", true},
		{"Hey, Mirrin. Lights off.", "Lights off.", true},
		{"Mirrin, lights off.", "lights off.", true},
		{"OK Mirrin, set a timer", "set a timer", true},
		{"1, 2, 3. Hey Mirrin, stop.", "stop.", true},
		{"Hey Mirin, read my email.", "read my email.", true},
		{"Hey Mirren what time is it", "what time is it", true},
		{"Hey Miran, call Mum.", "call Mum.", true},
		{"Hey, Mirron. Lights off.", "Lights off.", true},
		{"Hey Mirrin's here", "here", true},
		{"Mirrins, are you there?", "are you there?", true},
		{"Mirren?", "", true},
		{"Miranda called about dinner.", "", false},
		{"The mirror is fogged up.", "", false},
		{"Mirinda is on tonight.", "", false},
		{"I put mirin in the stir-fry.", "", false},
		{"the Mirrin pilot flew fast", "", false},
	}
	for _, c := range cases {
		for _, extra := range [][]string{p.WakeAliases, nil} {
			rest, ok := stripWakeWith(c.text, p.WakeWord, extra)
			if ok != c.ok || rest != c.rest {
				t.Errorf("%q (aliases %v): got %q ok=%v, want %q ok=%v", c.text, extra, rest, ok, c.rest, c.ok)
			}
		}
	}
	if got := WakePhrase(p.WakeWord); got != "Hey Mirrin" {
		t.Errorf("WakePhrase = %q", got)
	}
	if got := WakePhrase(""); got != "Hey Mirrin" {
		t.Errorf("the default WakePhrase = %q", got)
	}
}

func TestTitleWords(t *testing.T) {
	for in, want := range map[string]string{"mirrin": "Mirrin", "hey  ava": "Hey Ava", "élodie": "Élodie", "": ""} {
		if got := titleWords(in); got != want {
			t.Errorf("titleWords(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShortAnswers(t *testing.T) {
	names := []string{"Mirrin", "mirrin"}
	for _, s := range []string{"Yes.", "yes", "No.", "Sure.", "Okay.", "OK!", "Yeah.", "Nope.", "Go ahead.", "Yes please.", "Yes, Mirrin.", "Stop.", "Stop, Mirrin!", "Wait.", "Hold on.", "Never mind.", "That's enough.", "yes 12", "Cancel."} {
		if !shortAnswer(s, names...) {
			t.Errorf("%q should be a whole answer", s)
		}
	}
	for _, s := range []string{"", "Thank you.", "you", "Bye.", "Hmm.", "yes, but tomorrow", "stop by the shop", "stopwatch"} {
		if shortAnswer(s, names...) {
			t.Errorf("%q is not a whole answer", s)
		}
	}
	for _, s := range []string{"Stop!", "stop talking", "Shut up.", "Enough, Mirrin."} {
		if !isStop(s, names...) {
			t.Errorf("%q should stop the twin", s)
		}
	}
	if isStop("Yes.", names...) {
		t.Error("yes is not a stop")
	}
}

func TestAsksForAnswer(t *testing.T) {
	for _, s := range []string{"There's one at six. Shall I book it?", "Want me to send it?\n", "Say yes to go ahead.", "Should I call them", "Is that right?\"", "Book it? "} {
		if !asksForAnswer(s) {
			t.Errorf("%q asks for an answer", s)
		}
	}
	for _, s := range []string{"", "It's half past two.", "Done. The invite is out."} {
		if asksForAnswer(s) {
			t.Errorf("%q asks nothing", s)
		}
	}
}

func TestExpectingAnswerFollowsWhatWasSaidAndWhatWaits(t *testing.T) {
	c := &Channel{}
	if c.expectingAnswer() {
		t.Fatal("nothing said, nothing waiting")
	}
	c.lastSaid.Store("Shall I?")
	if !c.expectingAnswer() {
		t.Fatal("a question was asked")
	}
	c.lastSaid.Store("Done.")
	waiting := false
	c.Pending = func() bool { return waiting }
	if c.expectingAnswer() {
		t.Fatal("nothing waits")
	}
	waiting = true
	if !c.expectingAnswer() {
		t.Fatal("an approval waits")
	}
}
