package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// An approved browser action whose button changed after the owner was asked
// is not run: the approval ends as expired, the audit and the model hear
// "NOT run", and the owner is told nothing was pressed. The page's address
// is the same, so only the browser's own element check can catch it.
func TestBrowserActionRunsOnlyIfItsElementIsUnchanged(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := map[bool]string{false: "unchanged", true: "button changed"}[changed]
		t.Run(name, func(t *testing.T) {
			const input = `{"steps":"[{\"type\":\"click\",\"ref\":12}]"}`
			clicked := 0
			td := newTestDaemon(t, func(last string, req llm.Request) llm.Response {
				switch {
				case last == "save it":
					return call("t1", "browser_act", input)
				case strings.Contains(last, "PENDING_APPROVAL"):
					return say("Shall I press Save draft?")
				case strings.Contains(last, "NOT run"):
					return say("The button changed, so I didn't press anything. Shall I look again?")
				}
				return say("Done.")
			})
			td.pageURL = func() string { return "https://mail.example/drafts" }
			var checked []string
			td.pageCheck = func(_ context.Context, chatKey string, in json.RawMessage) error {
				checked = append(checked, chatKey+" "+string(in))
				if changed {
					return errors.New(`the page changed since this was asked: "Save draft" (element 12) now reads "Delete account", so nothing was clicked or typed. Look at the page again and ask afresh.`)
				}
				return nil
			}
			td.agent.Tools().Register(tools.New("browser_act", "act", tools.Schema(nil), tools.RiskWrite,
				func(context.Context, tools.Call) (string, error) { clicked++; return "clicked", nil }))
			seen := listen(t, td.bus)
			td.owner(t, "save it")
			got := td.owner(t, "yes")

			if (clicked == 1) == changed {
				t.Fatalf("clicked %d times, reply %q", clicked, got)
			}
			if len(checked) != 1 || !strings.Contains(checked[0], `\"ref\":12`) {
				t.Fatalf("the browser was asked to check %v", checked)
			}
			ctx := context.Background()
			ap, _ := td.store.GetApproval(ctx, 1)
			want := map[bool]string{false: "approved", true: "expired"}[changed]
			if ap.Status != want {
				t.Fatalf("#1 is %s, want %s", ap.Status, want)
			}
			var last string
			for _, ev := range seen() {
				if n, ok := ev.Data.(approvalNews); ok {
					last = n.Status
				}
			}
			if last != want {
				t.Fatalf("the screen last heard %q, want %q", last, want)
			}
			if changed && !strings.Contains(got, "didn't press anything") {
				t.Fatalf("reply %q", got)
			}
		})
	}
}

// The same element check holds when the owner answers in their own words and
// the model settles it with resolve_approval (approvals merged with browser):
// a changed button is not pressed, the request ends expired and the model is
// told it was NOT run; an unchanged one is pressed once.
func TestBrowserActionInOwnWordsStillChecksItsElement(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := map[bool]string{false: "unchanged", true: "button changed"}[changed]
		t.Run(name, func(t *testing.T) {
			const input = `{"steps":"[{\"type\":\"click\",\"ref\":12}]"}`
			clicked := 0
			td := newTestDaemon(t, func(last string, req llm.Request) llm.Response {
				switch {
				case last == "save it":
					return call("t1", "browser_act", input)
				case last == "yep, press it":
					return call("r1", "resolve_approval", `{"id":1,"decision":"approve"}`)
				case strings.Contains(last, "PENDING_APPROVAL"):
					return say("Shall I press Save draft?")
				case isToolResult(req):
					return say("Tool: " + last)
				}
				return say("Done.")
			})
			td.pageURL = func() string { return "https://mail.example/drafts" }
			td.pageCheck = func(context.Context, string, json.RawMessage) error {
				if changed {
					return errors.New(`the page changed since this was asked: "Save draft" (element 12) now reads "Delete account", so nothing was clicked or typed`)
				}
				return nil
			}
			td.agent.Tools().Register(tools.New("browser_act", "act", tools.Schema(nil), tools.RiskWrite,
				func(context.Context, tools.Call) (string, error) { clicked++; return "clicked", nil }))
			td.owner(t, "save it")
			got := td.owner(t, "yep, press it")
			ap, _ := td.store.GetApproval(context.Background(), 1)
			switch {
			case changed && (clicked != 0 || ap.Status != "expired" || !strings.Contains(got, "NOT run")):
				t.Fatalf("a changed button: clicked %d, #1 %s, model told %q", clicked, ap.Status, got)
			case !changed && (clicked != 1 || ap.Status != "approved"):
				t.Fatalf("an unchanged button: clicked %d, #1 %s, model told %q", clicked, ap.Status, got)
			}
		})
	}
}
