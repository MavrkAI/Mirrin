package daemon

import (
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

// Handing the browser back on the screen carries on in the chat that was
// using it, and the owner hears how it's going there without saying "done".
func TestAHandBackCarriesOnInTheChat(t *testing.T) {
	td := newTestDaemon(t, func(last string, _ llm.Request) llm.Response {
		if strings.Contains(last, "clicked Hand back") {
			return llm.Response{Message: llm.Text(llm.RoleAssistant, "Past the check: the cheapest one-way is A$389."), StopReason: llm.StopEndTurn}
		}
		return llm.Response{Message: llm.Text(llm.RoleAssistant, "ok"), StopReason: llm.StopEndTurn}
	})
	td.carryOnAfterHandBack(ownerKey)
	if got := td.ch.next(t); !strings.Contains(got, "A$389") {
		t.Fatalf("sent %q", got)
	}
}
