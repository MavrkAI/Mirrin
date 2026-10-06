package tasks

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Asked to cancel a task that had already finished, the twin put it to the
// owner anyway; they said yes, and only then heard there was nothing to
// cancel. A finished task is refused before anyone is asked, and an open
// one is asked about by name.
func TestCancellingAFinishedTaskIsNotPutToTheOwner(t *testing.T) {
	m := New(context.Background(), Deps{})
	m.tasks["10011"] = &Task{ID: "10011", Title: "Book Bali flights", Status: Done}
	m.tasks["10030"] = &Task{ID: "10030", Title: "Find a dentist", Status: WaitingUser}
	var cancel tools.Tool
	for _, tl := range m.Tools() {
		if tl.Spec().Name == "cancel_task" {
			cancel = tl
		}
	}
	c, ok := cancel.(tools.Checker)
	if !ok {
		t.Fatal("cancel_task can't refuse before asking")
	}
	call := func(id string) tools.Call {
		in, _ := json.Marshal(map[string]string{"id": id})
		return tools.Call{Input: in}
	}
	if err := c.Check(context.Background(), call("10011")); err == nil || !strings.Contains(err.Error(), "already done") {
		t.Fatalf("finished task: %v", err)
	}
	if err := c.Check(context.Background(), call("10030")); err != nil {
		t.Fatalf("open task: %v", err)
	}
	if got := cancel.(tools.Summarizer).ApprovalSummary(call("10030")); got != `Cancel the task "Find a dentist"` {
		t.Fatalf("summary %q", got)
	}
}

// With every task finished, the twin still told the owner the Bali flight
// task was "waiting on you": its board ended "Present options". The list
// now says first what is actually open.
func TestTheTaskListSaysWhatIsStillOpen(t *testing.T) {
	m := New(context.Background(), Deps{})
	m.tasks["10011"] = &Task{ID: "10011", Title: "Book Bali flights", Status: Done}
	list := func() string {
		for _, tl := range m.Tools() {
			if tl.Spec().Name == "list_tasks" {
				out, _ := tl.Run(context.Background(), tools.Call{})
				return out
			}
		}
		return ""
	}
	if got := list(); !strings.HasPrefix(got, "Nothing is open") {
		t.Fatalf("all done:\n%s", got)
	}
	m.tasks["10030"] = &Task{ID: "10030", Title: "Find a dentist", Status: WaitingUser}
	if got := list(); !strings.HasPrefix(got, `Still open: 10030 "Find a dentist" (waiting on the user).`) {
		t.Fatalf("one open:\n%s", got)
	}
}
