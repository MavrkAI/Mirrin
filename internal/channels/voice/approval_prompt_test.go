package voice

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

// Exercise the actual speech queue, including split questions and unrelated
// answers before and after the dangerous ask.
func TestDangerousApprovalStreamAsksOnScreen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("records speech with a shell command")
	}
	r := newRig(t, nil)
	id, checks := int64(7), 0
	r.c.ApprovalPrompt = func() (int64, string) {
		checks++
		return id, AskByHand(id, "clearing your downloads")
	}
	stream := r.c.OpenStream(context.Background(), "local")
	stream.Write("You have a meeting at nine. ")
	stream.(interface{ Note(string) }).Note("Checking your downloads.")
	id = 12
	stream.Write("Shall ")
	stream.Write("I clear your downloads? ")
	stream.Write("Say yes to go ahead. ")
	for _, part := range strings.Split("Your next appointment is at noon. ", "") {
		stream.Write(part)
	}
	stream.Close()
	got := r.spoken()
	for _, want := range []string{"meeting at nine", "appointment is at noon", "I need your OK", "send it to your phone"} {
		if !strings.Contains(got, want) {
			t.Errorf("speech missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "Shall") || strings.Contains(got, "Say yes to go ahead") {
		t.Fatalf("spoke the model's approval question: %q", got)
	}
	if checks != 3 { // baseline, first response, response after the tool
		t.Fatalf("%d approval queries; want 3 regardless of delta count", checks)
	}
	// Existing approvals do not replace a later, unrelated question.
	later := r.c.OpenStream(context.Background(), "local")
	later.Write("Would you like tomorrow's calendar?")
	later.Close()
	if !strings.Contains(r.spoken(), "Would you like tomorrow's calendar?") {
		t.Fatalf("existing approval swallowed an unrelated question: %q", r.spoken())
	}
}

func TestDangerousApprovalWithoutAModelQuestionIsStillSpoken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("records speech with a shell command")
	}
	r := newRig(t, nil)
	var id int64
	r.c.ApprovalPrompt = func() (int64, string) {
		return id, AskByHand(id, "running the command")
	}
	stream := r.c.OpenStream(context.Background(), "local")
	stream.(interface{ Note(string) }).Note("Checking the command.")
	id = 9
	stream.Close()
	if got := r.spoken(); !strings.Contains(got, "I need your OK") || !strings.Contains(got, "phone") {
		t.Fatalf("speech = %q", got)
	}
}

// Some gated tools intentionally have no spoken caption. Approval events
// still refresh the cached request, without a database query for each delta.
func TestDangerousApprovalRevisionWithoutCaption(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("records speech with a shell command")
	}
	r := newRig(t, nil)
	var revision uint64
	var id int64
	checks := 0
	r.c.ApprovalRevision = func() uint64 { return revision }
	r.c.ApprovalPrompt = func() (int64, string) {
		checks++
		return id, AskByHand(id, "saving that private note")
	}
	stream := r.c.OpenStream(context.Background(), "local")
	stream.Write("Your calendar is clear. ")
	id, revision = 12, 1
	for _, delta := range strings.Split("Shall I save that note? You have tomorrow free too.", "") {
		stream.Write(delta)
	}
	stream.Close()
	got := r.spoken()
	if strings.Contains(got, "Shall I") || !strings.Contains(got, "I need your OK") ||
		!strings.Contains(got, "tomorrow free too") {
		t.Fatalf("speech = %q", got)
	}
	if checks != 3 {
		t.Fatalf("queried approvals %d times for one change; want 3 including initialization", checks)
	}
}

// A step's caption isn't read out: the owner hears the answer, not each
// step on the way there.
func TestStepCaptionsAreNotSpoken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("records speech with a shell command")
	}
	r := newRig(t, nil)
	stream := r.c.OpenStream(context.Background(), "local")
	stream.(interface{ Note(string) }).Note("Opening jetstar.com.")
	stream.Write("The cheapest one-way is three hundred and eighty-nine dollars.")
	stream.Close()
	got := r.spoken()
	if strings.Contains(got, "Opening") || !strings.Contains(got, "three hundred") {
		t.Fatalf("speech = %q", got)
	}
}

// Out loud, the twin says what it's about to do and then the result; what
// it writes between steps (its working) isn't read to the room.
func TestWorkingBetweenStepsIsNotSpoken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("records speech with a shell command")
	}
	r := newRig(t, nil)
	stream := r.c.OpenStream(context.Background(), "local")
	note := stream.(interface{ Note(string) }).Note
	stream.Write("Sure, adding all seven now. ")
	note("Opening the site.")
	stream.Write("Size M selected, the tooltip got in the way. Let me retry. ")
	note("Clicking Add to cart.")
	stream.Write("Good, modal closed. ")
	note("Opening the cart.")
	stream.Write("All seven are in. That's $349.30. Shall I check out?")
	stream.Close()
	got := r.spoken()
	if !strings.Contains(got, "adding all seven now") || !strings.Contains(got, "All seven are in") || !strings.Contains(got, "check out") {
		t.Fatalf("missing the plan or the result: %q", got)
	}
	if strings.Contains(got, "tooltip") || strings.Contains(got, "modal") {
		t.Fatalf("read its working aloud: %q", got)
	}
}

// A reply that starts with a step says something at once, not a silence.
func TestWorkBeforeWordsGetsANod(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("records speech with a shell command")
	}
	r := newRig(t, nil)
	stream := r.c.OpenStream(context.Background(), "local")
	stream.(interface{ Note(string) }).Note("Opening the site.")
	stream.Write("It's open.")
	stream.Close()
	got := r.spoken()
	nodded := false
	for _, n := range workNods {
		nodded = nodded || strings.HasPrefix(got, n)
	}
	if !nodded || !strings.Contains(got, "It's open.") {
		t.Fatalf("spoken %q", got)
	}
}
