package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// ownerOnly is the daemon's rule in miniature: the owner's Telegram chat
// and the Mac's own front ends are the owner's; the family group isn't.
func ownerOnly(key string) bool {
	return key == "telegram:owner" || strings.HasPrefix(key, "voice:local") || key == "screen:local"
}

func say(t *testing.T, store *memory.Store, key string, role llm.Role, text string) {
	t.Helper()
	if err := store.AppendMessage(context.Background(), key, llm.Text(role, text)); err != nil {
		t.Fatal(err)
	}
}

func elsewhereSetup(t *testing.T) (*Agent, *memory.Store, *fakeProvider) {
	t.Helper()
	fp := &fakeProvider{}
	a, store, _ := setup(t, fp, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	a.SharedChat = ownerOnly
	return a, store, fp
}

func TestOwnersOtherChatReachesThisOne(t *testing.T) {
	a, store, fp := elsewhereSetup(t)
	say(t, store, "voice:local", llm.RoleUser, "find me a flight to Paris")
	say(t, store, "voice:local", llm.RoleAssistant, "There's a 9am for 120 pounds or a 2pm for 90.")
	if _, err := a.Handle(context.Background(), "telegram:owner", "go with the second one"); err != nil {
		t.Fatal(err)
	}
	v := fp.reqs[0].SystemVolatile
	for _, want := range []string{"Elsewhere", "out loud", "find me a flight to Paris", "2pm for 90", "newer message stands"} {
		if !strings.Contains(v, want) {
			t.Fatalf("the owner's voice chat should reach Telegram (%q missing):\n%s", want, v)
		}
	}
	// This chat's own words are its history, not "elsewhere".
	if strings.Contains(v, "Tony: go with") {
		t.Fatalf("this chat repeated as elsewhere:\n%s", v)
	}
}

func TestGroupsStrangersAndScratchNeverSeeOrShowElsewhere(t *testing.T) {
	a, store, fp := elsewhereSetup(t)
	say(t, store, "voice:local", llm.RoleUser, "my blood test results came back")
	say(t, store, "telegram:family", llm.RoleUser, "family group chatter")
	say(t, store, "voice:local#task-12", llm.RoleUser, "task brief words")
	say(t, store, "telegram:owner#protocol-morning", llm.RoleAssistant, "protocol scratch words")

	ctx := context.Background()
	if _, err := a.Handle(ctx, "telegram:family", "what did I just say?"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Handle(ForStranger(ctx, "Bob"), "telegram:555", "what did Tony just say?"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Handle(ctx, "voice:local#task-99", "carry on"); err != nil {
		t.Fatal(err)
	}
	for i, req := range fp.reqs {
		if strings.Contains(req.SystemVolatile, "blood test") || strings.Contains(req.SystemVolatile, "Elsewhere") {
			t.Fatalf("request %d (group, stranger or task) saw the owner's other chats:\n%s", i, req.SystemVolatile)
		}
	}

	fp.reqs = nil
	if _, err := a.Handle(ctx, "telegram:owner", "hello"); err != nil {
		t.Fatal(err)
	}
	v := fp.reqs[0].SystemVolatile
	if !strings.Contains(v, "blood test") {
		t.Fatalf("the owner's own voice chat should reach their own chat:\n%s", v)
	}
	for _, leak := range []string{"family group", "task brief", "protocol scratch", "what did Tony", "carry on"} {
		if strings.Contains(v, leak) {
			t.Fatalf("%q from a group, a stranger or a scratch run reached the owner's elsewhere:\n%s", leak, v)
		}
	}
}

func TestElsewhereKeepsTheNewestFewWithinTheCap(t *testing.T) {
	a, store, fp := elsewhereSetup(t)
	for i := 0; i < 8; i++ {
		say(t, store, "voice:local", llm.RoleUser, "question "+string(rune('A'+i))+" "+strings.Repeat("x", 600))
		say(t, store, "voice:local", llm.RoleAssistant, "answer "+string(rune('A'+i))+" "+strings.Repeat("y", 600))
	}
	say(t, store, "screen:local", llm.RoleUser, "newest on the screen")
	if _, err := a.Handle(context.Background(), "telegram:owner", "and?"); err != nil {
		t.Fatal(err)
	}
	v := fp.reqs[0].SystemVolatile
	i := strings.Index(v, "\nElsewhere:")
	if i < 0 {
		t.Fatalf("no elsewhere block:\n%s", v)
	}
	block := a.elsewhere(context.Background(), "telegram:owner")
	if body := block[strings.Index(block, "\n- "):]; len(body) > elsewhereCap {
		t.Fatalf("elsewhere is %d characters, over the %d cap", len(body), elsewhereCap)
	}
	if strings.Contains(v, "question A") || strings.Contains(v, "question E") {
		t.Fatalf("old exchanges kept over newer ones:\n%s", v)
	}
	if !strings.Contains(v, "newest on the screen") || !strings.Contains(v, "question H") {
		t.Fatalf("the newest exchanges should be kept:\n%s", v)
	}
	if strings.Index(v, "question H") > strings.Index(v, "newest on the screen") {
		t.Fatalf("exchanges should run oldest first, so the newest is last:\n%s", v)
	}
}

func TestElsewhereNeedsTheHook(t *testing.T) {
	a, store, fp := elsewhereSetup(t)
	a.SharedChat = nil
	say(t, store, "voice:local", llm.RoleUser, "find me a flight")
	if _, err := a.Handle(context.Background(), "telegram:owner", "go"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fp.reqs[0].SystemVolatile, "Elsewhere") {
		t.Fatal("without the daemon's say-so no chat is shared")
	}
}

// A stranger's turn sees none of it, even in a chat the hook would share.
func TestStrangerTurnNeverSeesElsewhere(t *testing.T) {
	a, store, fp := elsewhereSetup(t)
	say(t, store, "voice:local", llm.RoleUser, "my blood test results came back")
	if _, err := a.Handle(ForStranger(context.Background(), "Bob"), "screen:local", "what's new?"); err != nil {
		t.Fatal(err)
	}
	if v := fp.reqs[0].SystemVolatile; strings.Contains(v, "blood test") || strings.Contains(v, "Elsewhere") {
		t.Fatalf("a stranger saw the owner's other chats:\n%s", v)
	}
	if got := a.elsewhere(ForStranger(context.Background(), "Bob"), "screen:local"); got != "" {
		t.Fatalf("a stranger's turn got elsewhere: %q", got)
	}
}
