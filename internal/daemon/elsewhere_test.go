package daemon

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

func TestSharedChatIsOnlyTheOwnersOwn(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return llm.Response{} })
	td.channels["irc"] = &fakeChannel{name: "irc", owner: "owner", out: make(chan string, 1)}
	for key, want := range map[string]bool{
		"telegram:owner":               true,
		"voice:local":                  true,
		"cli:terminal":                 true,
		"screen:local":                 true,
		"api:local":                    true,
		"telegram:family":              false, // a group
		"telegram:555":                 false, // someone else
		"voice:phone":                  false, // a call
		"voice:phone#call-1":           false,
		"telegram:owner#task-12":       false, // a background task
		"telegram:owner#protocol-x":    false, // a scratch run
		"irc:owner":                    false, // anyone could claim to be the owner
		"mail:owner":                   false,
		"whatsapp:owner":               false, // not running
		"portrait:ack":                 false, // not a chat
		"voice":                        false,
		"mattermost:town#task-force-1": false,
	} {
		if got := td.sharedChat(key); got != want {
			t.Errorf("sharedChat(%q) = %v, want %v", key, got, want)
		}
	}
}

// What the owner said out loud reaches their Telegram chat, and never the
// family group.
func TestOwnersVoiceChatReachesTheirTelegramNotTheGroup(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	td := newTestDaemon(t, func(last string, req llm.Request) llm.Response {
		mu.Lock()
		seen[last] = req.SystemVolatile
		mu.Unlock()
		if strings.Contains(last, "flight") {
			return llm.Response{Message: llm.Text(llm.RoleAssistant, "A 9am for 120 pounds or a 2pm for 90."), StopReason: llm.StopEndTurn}
		}
		return llm.Response{Message: llm.Text(llm.RoleAssistant, "Done."), StopReason: llm.StopEndTurn}
	})
	ctx := context.Background()
	if _, err := td.agent.Handle(ctx, "voice:local", "find me a flight to Paris"); err != nil {
		t.Fatal(err)
	}
	if _, err := td.message(ctx, channels.Inbound{Channel: "telegram", ChatID: "family", Sender: "owner", Text: "hi all", IsOwner: true}, agent.Events{}); err != nil {
		t.Fatal(err)
	}
	td.owner(t, "go with the second one")
	mu.Lock()
	defer mu.Unlock()
	var own, group string
	for last, v := range seen {
		switch {
		case strings.Contains(last, "go with the second one"):
			own = v
		case strings.Contains(last, "hi all"):
			group = v
		}
	}
	if !strings.Contains(own, "2pm for 90") || !strings.Contains(own, "out loud") {
		t.Fatalf("the owner's Telegram should see their voice chat:\n%s", own)
	}
	if strings.Contains(own, "hi all") {
		t.Fatalf("the family group reached the owner's elsewhere:\n%s", own)
	}
	if group == "" || strings.Contains(group, "Paris") {
		t.Fatalf("the family group saw the owner's voice chat:\n%s", group)
	}
}
