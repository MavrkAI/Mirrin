package voice

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// mirrinHellos are Mirrin's hellos (internal/persona/mirrin.yaml).
var mirrinHellos = map[string]string{"morning": "Good morning{, address}.", "afternoon": "Good afternoon{, address}.",
	"evening": "Good evening{, address}.", "late": "Late one{, address}."}

// at is a time of day today, on this computer's clock.
func at(h, m int) time.Time {
	y, mo, d := time.Now().Date()
	return time.Date(y, mo, d, h, m, 0, 0, time.Local)
}

// wordsClips is a channel whose clips hold the words they say.
func wordsClips(t *testing.T) (*Channel, func() int) {
	t.Helper()
	var mu sync.Mutex
	made := 0
	c := &Channel{dataDir: t.TempDir(), cfg: config.Voice{Voice: "bm_george"}}
	c.ackMakerFn = func() ackMaker {
		return ackMaker{"wav", "words", func(_ context.Context, phrase, path string) error {
			mu.Lock()
			made++
			mu.Unlock()
			return os.WriteFile(path, []byte(phrase), 0o644)
		}}
	}
	return c, func() int { mu.Lock(); defer mu.Unlock(); return made }
}

// clipWords is what a clip says ("" for none).
func clipWords(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// "Hey Mirrin" at 7:40 is answered "Good morning, sir."; at 1am, "Late
// one, sir.", never "Good evening" until 5. Each part of the day is its own
// clip, made before it is wanted.
func TestTheHelloIsForThePartOfTheDay(t *testing.T) {
	c, made := wordsClips(t)
	c.SetHellos(mirrinHellos)
	c.SetAddress("sir")
	c.SetName("Akshay")
	for _, w := range []struct {
		h, m int
		want string
	}{{7, 40, "Good morning, sir."}, {13, 0, "Good afternoon, sir."}, {19, 30, "Good evening, sir."}, {1, 0, "Late one, sir."}, {4, 59, "Late one, sir."}} {
		if got := c.helloPhrase(at(w.h, w.m)); got != w.want {
			t.Errorf("%02d:%02d: %q, want %q", w.h, w.m, got, w.want)
		}
	}
	c.prepareAcks(context.Background())
	<-c.acks.done
	if n := made(); n != len(ackPhrases)+4 {
		t.Fatalf("made %d clips, want %d", n, len(ackPhrases)+4)
	}
	morning, late := c.ackClipAt(true, at(7, 40)), c.ackClipAt(true, at(1, 0))
	if clipWords(morning) != "Good morning, sir." || clipWords(late) != "Late one, sir." {
		t.Fatalf("hellos say %q and %q", clipWords(morning), clipWords(late))
	}
	if morning == late {
		t.Fatal("the morning and the late hello share a clip")
	}
	// Not a hello: a brief "on it", never "Good morning" mid-conversation.
	if w := clipWords(c.ackClipAt(false, at(7, 40))); w == "Good morning, sir." {
		t.Fatalf("an acknowledgement said %q", w)
	}
}

// A new name (or form of address) makes a new clip: the old one isn't
// played with the wrong words. Until it is made, the hello is a brief
// "on it".
func TestANewNameMakesANewHello(t *testing.T) {
	c, made := wordsClips(t)
	c.SetHellos(map[string]string{"morning": "Morning, {name}."})
	c.SetAddress("Akshaya")
	c.SetName("Akshaya")
	c.prepareAcks(context.Background())
	<-c.acks.done
	before := made()
	if w := clipWords(c.ackClipAt(true, at(8, 0))); w != "Morning, Akshaya." {
		t.Fatalf("hello %q", w)
	}
	// a part of the day without its own hello has the plain words
	if w := clipWords(c.ackClipAt(true, at(23, 30))); w != "Hello." {
		t.Fatalf("late hello %q", w)
	}
	c.SetName("Aks")
	if w := clipWords(c.ackClipAt(true, at(8, 0))); w == "Morning, Akshaya." {
		t.Fatal("played the old name's hello")
	}
	deadline := time.Now().Add(3 * time.Second)
	for made() == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	var w string
	for time.Now().Before(deadline) {
		if w = clipWords(c.ackClipAt(true, at(8, 0))); w == "Morning, Aks." {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if w != "Morning, Aks." {
		t.Fatalf("the new name's hello is %q", w)
	}
	if ackClipName("v", "r", c.helloPhrase(at(8, 0)), "wav") == ackClipName("v", "r", c.helloPhrase(at(1, 0)), "wav") {
		t.Fatal("the morning and the late hello have one clip name")
	}
}

// The greeting said at start is the persona's, as said to the owner.
func TestStartGreetingIsRendered(t *testing.T) {
	c := New(config.Voice{}, "Mirrin", t.TempDir())
	c.SetPersona("Mirrin", nil, nil, "Good to see you{, address}. What can I take off your plate?")
	c.SetAddress("ma'am")
	if g := c.startGreeting(); g != "Good to see you, ma'am. What can I take off your plate?" {
		t.Fatalf("greeting %q", g)
	}
	c.SetAddress("")
	if g := c.startGreeting(); g != "Good to see you. What can I take off your plate?" {
		t.Fatalf("greeting with no address %q", g)
	}
	c.SetPersona("Mirrin", nil, nil, "")
	if g := c.startGreeting(); g != "Online." {
		t.Fatalf("no greeting: %q", g)
	}
}
